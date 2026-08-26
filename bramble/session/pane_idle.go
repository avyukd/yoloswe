package session

import (
	"regexp"
	"strings"
)

// paneIdleProbe tells, from tmux pane text, whether an agent CLI is waiting for
// input. It covers hookless providers and providers whose hook can report idle
// before the pane is actually ready.
//
// Reading the pane is treated as a weak signal: the probe must positively
// recognize provider chrome before judging, and otherwise reports unknown.
type paneIdleProbe struct {
	// judge replaces substring matching for CLIs whose chrome needs structure.
	judge func(lines []string) (working, known bool)
	// promptMarkers identify the composer line itself. Until one appears the
	// pane says nothing useful — the CLI may still be booting.
	promptMarkers []string
	// workingOnPrompt means a turn is in flight when found on the composer line.
	// This catches Cursor's "ctrl+c to stop" without relying on a fixed footer.
	workingOnPrompt []string
	// workingInFooter means a turn is in flight within paneIdleTailLines of the
	// bottom; the tail bound keeps quoted scrollback markers out of reach.
	workingInFooter []string
	// confirmations overrides paneIdleConfirmations for noisy chrome.
	confirmations int
	// correctsPrematureIdle keeps polling an idle session whose hook can fire
	// before the prompt is ready, so a live turn can be restored to running.
	correctsPrematureIdle bool
}

// paneIdleProbes lists only providers whose chrome is understood. A wrong idle
// verdict releases queued messages into a live turn, so unknown providers are
// not guessed from the pane.
var paneIdleProbes = map[string]paneIdleProbe{
	// "Add a follow-up" is not idle; Cursor shows it while working too.
	ProviderCursor: {
		promptMarkers:   []string{"Add a follow-up"},
		workingOnPrompt: []string{"ctrl+c to stop"},
	},
	// Codex reports turn ends through a notify hook, but that hook can fire
	// before the pane shows idle — while "Working (… • esc to interrupt)" is
	// still on screen. The probe corrects that premature idle back to running.
	ProviderCodex: {
		promptMarkers:         []string{"Ask Codex to do anything"},
		workingInFooter:       []string{"esc to interrupt"},
		correctsPrematureIdle: true,
	},
	// Claude normally reports idle through its Stop hook; this is the fallback
	// when that hook never arrives. Its chrome needs claudePaneJudge because "no
	// spinner" is not enough to call the pane idle. correctsPrematureIdle stays
	// off because the hook is not premature.
	ProviderClaude: {
		judge: claudePaneJudge,
		// Five, not two. Claude's working chrome is often absent from any given
		// frame, so agreement has to be sustained before it means anything.
		confirmations: 5,
	},
}

// claudeCompletionPastRe matches claude's finished-turn line, e.g.
// "✻ Baked for 3m 48s". Match the verb as non-space rather than \w because
// Go's \w is ASCII-only and claude's verbs need not be.
var claudeCompletionPastRe = regexp.MustCompile(`^[✻✢✽✹]\s+\S+ for\s+\d`)

// isClaudeSeparator matches either claude rule, including the input rule whose
// box drawing is split by the mode indicator.
func isClaudeSeparator(trimmed string) bool {
	return separatorRe.MatchString(trimmed) ||
		(strings.HasPrefix(trimmed, "─") && strings.Contains(trimmed, "▪"))
}

// claudeComposerIdx locates claude-code's live composer by position: the first
// non-empty line above the lowest status separator, with the next separator
// above it bounding content when present.
//
// Do not scan by glyph alone. Claude echoes submitted prompts with the same `❯`,
// so position relative to the status separator is the only anchor that separates
// the live composer from transcript scrollback.
//
// contentEndIdx is -1 when the upper rule is missing, often because a multi-line
// composer absorbed it. Callers must treat that as "content region unknown",
// not "no content".
func statusSepIdx(lines []string) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if isClaudeSeparator(strings.TrimSpace(lines[i])) {
			return i
		}
	}
	return -1
}

func claudeComposerIdx(lines []string) (composerIdx, contentEndIdx int) {
	sepIdx := statusSepIdx(lines)
	if sepIdx < 0 {
		return -1, -1
	}
	// Walk up from the status separator to the upper rule. The composer may wrap,
	// so its line is the first line in that block, not the nearest one.
	composerIdx = -1
	seen := 0
	sawUpperRule := false
	for i := sepIdx - 1; i >= 0 && seen < claudeComposerMaxLines; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		if isClaudeSeparator(trimmed) {
			if composerIdx < 0 {
				// Two rules with nothing between them: no composer drawn.
				return -1, i
			}
			sawUpperRule = true
			break
		}
		seen++
		composerIdx = i
	}
	if composerIdx < 0 {
		return -1, -1
	}
	if !sawUpperRule {
		// Without the upper rule, the topmost reached line is not a proven
		// composer. Fail closed as unfound; trusting it can either deliver into an
		// oversized draft or latch onto a transcript prompt that never clears.
		return -1, -1
	}
	contentEndIdx = -1
	for i := composerIdx - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		if isClaudeSeparator(trimmed) {
			contentEndIdx = i
		}
		break
	}
	return composerIdx, contentEndIdx
}

// claudePaneJudge reads claude-code's pane to decide whether a turn is in flight.
// It does not use ParseClaudeStatusBar's IsIdle: the live composer is visible in
// both states, so that parser can call a working pane idle and release queued
// mail into it.
//
// Claude's reliable signal is the nearest sparkle line in the bounded content
// tail: gerund/ellipsis means working, past tense plus "for <duration>" means
// done. A `●` tool line counts as work only when no completion sparkle sits
// below it.
//
// Ambiguity reports known=false. "No marker" must never mean idle; the spinner
// can be absent from a single frame, and observe resets the streak on unknowns.
func claudePaneJudge(lines []string) (working, known bool) {
	composerIdx, contentEndIdx := claudeComposerIdx(lines)
	if composerIdx < 0 {
		return false, false // no composer: not claude's prompt, or still booting
	}
	if contentEndIdx < 0 {
		// No rule above the composer: a multi-line composer absorbed it, so the
		// content region cannot be delimited and a transcript line would be
		// indistinguishable from live chrome. Refuse to guess.
		return false, false
	}

	// Stop at this turn's submitted prompt. Completion lines above it belong to
	// previous turns and would otherwise make a newly started live turn look idle.
	seen := 0
	for i := contentEndIdx - 1; i >= 0 && seen < claudePaneContentTailLines; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, claudePromptGlyph) {
			// This turn's own submitted prompt. Nothing above it can speak for
			// the turn now running, and this turn has produced no verdict yet.
			return false, false
		}
		seen++
		if working, known := claudeLineVerdict(line); known {
			return working, true
		}
	}

	// Nothing decisive in the tail. Never guess idle here.
	return false, false
}

// claudeComposerMaxLines bounds the composer walk so a missing upper rule cannot
// turn arbitrary transcript into a composer. It is sized against pasteVerifyLines
// rather than a typical composer: wrapped deliveries and long drafts can be
// ordinary, and too small a bound manufactures the fail-closed paths in
// pasteEvidenceObscured and composerDraftText.
const claudeComposerMaxLines = pasteVerifyLines - 6

// claudePaneContentTailLines finds nearby sparkle lines while staying below
// quoted transcript history.
const claudePaneContentTailLines = 4

// claudeLineVerdict reports what one content line says about the turn, and
// whether it says anything at all.
func claudeLineVerdict(line string) (working, known bool) {
	if spinnerRe.MatchString(line) {
		return true, true
	}
	if completionRe.MatchString(line) {
		// Same glyph class, opposite meanings — the tense decides.
		if claudeCompletionPastRe.MatchString(line) {
			return false, true // "✻ Baked for 3m 48s": the turn is over
		}
		if strings.Contains(line, "…") {
			return true, true // "✽ Baking… (1m 55s)": still running
		}
		return false, false // neither shape: say nothing
	}
	if strings.HasPrefix(line, "●") {
		// A tool line means work only when nothing below it has already
		// reported the turn finished; the caller reaches this first only when
		// that is the case.
		return true, true
	}
	return false, false
}

// paneIdleConfirmations is how many consecutive polls must agree before a
// session is called idle. Two, so a single half-painted frame cannot release
// queued mail into a turn that is still running.
const paneIdleConfirmations = 2

// paneIdleTailLines bounds how far up from the bottom the composer line is
// looked for. Generous enough for a footer that grows a mode line or two, tight
// enough that the transcript above — which can quote these very markers back —
// is out of reach.
const paneIdleTailLines = 6

// providerHasIdleProbe reports whether a provider's idleness can be read off
// its pane.
func providerHasIdleProbe(provider string) bool {
	_, ok := paneIdleProbes[provider]
	return ok
}

// paneShowsWorking judges whether a captured pane shows a turn in flight.
// known is false when the pane does not yet look like the CLI's prompt.
func paneShowsWorking(provider string, lines []string) (working, known bool) {
	probe, ok := paneIdleProbes[provider]
	if !ok {
		return false, false
	}
	if probe.judge != nil {
		return probe.judge(lines)
	}

	prompt, ok := findPromptLine(lines, probe.promptMarkers)
	if !ok {
		return false, false
	}
	if len(probe.workingOnPrompt) > 0 && containsAny(prompt, probe.workingOnPrompt) {
		return true, true
	}
	if len(probe.workingInFooter) > 0 && footerShowsWorking(lines, probe.promptMarkers, probe.workingInFooter) {
		return true, true
	}
	return false, true
}

// paneShowsIdle judges a captured pane. known is false when the pane does not
// yet look like the CLI's prompt, in which case idle is meaningless.
func paneShowsIdle(provider string, lines []string) (idle, known bool) {
	working, known := paneShowsWorking(provider, lines)
	if !known {
		return false, false
	}
	return !working, true
}

// footerShowsWorking reports whether a working marker appears on a non-composer
// line within the tail window.
func footerShowsWorking(lines []string, promptMarkers, workingMarkers []string) bool {
	working := false
	forEachPaneTailLine(lines, func(line string) bool {
		if containsAny(line, promptMarkers) {
			return false
		}
		if containsAny(line, workingMarkers) {
			working = true
			return true
		}
		return false
	})
	return working
}

func forEachPaneTailLine(lines []string, visit func(string) bool) {
	seen := 0
	for i := len(lines) - 1; i >= 0 && seen < paneIdleTailLines; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		seen++
		if visit(lines[i]) {
			return
		}
	}
}

// findPromptLine returns the composer line, searched upwards from the bottom of
// the pane so the most recent one wins.
func findPromptLine(lines, markers []string) (string, bool) {
	var prompt string
	var found bool
	forEachPaneTailLine(lines, func(line string) bool {
		if containsAny(line, markers) {
			prompt = line
			found = true
			return true
		}
		return false
	})
	// A found flag rather than prompt != "": a matched line is by definition
	// non-empty today, but making emptiness stand in for "no match" is the kind
	// of sentinel that silently becomes wrong when a marker changes.
	return prompt, found
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// paneIdleTracker turns a stream of pane judgements into one idle transition.
type paneIdleTracker struct {
	provider string
	// Idle and working streaks are separate. observe fires only on equality,
	// so a shared counter that just fired idle would increment past the
	// working threshold and never correct a premature idle.
	idleStreak    int
	workingStreak int
	// epoch is the session turn the current streaks were observed during, so
	// observations cannot be carried across a turn boundary. See forTurn.
	epoch uint64
}

// correctsPrematureIdle reports whether this provider's pane is worth reading
// while the session is already idle.
func (p *paneIdleTracker) correctsPrematureIdle() bool {
	if p == nil {
		return false
	}
	return paneIdleProbes[p.provider].correctsPrematureIdle
}

// newPaneIdleTracker returns a tracker for a provider that needs pane evidence,
// or nil when its hook is sufficient on its own.
func newPaneIdleTracker(provider string) *paneIdleTracker {
	if !providerHasIdleProbe(provider) {
		return nil
	}
	return &paneIdleTracker{provider: provider}
}

// forTurn re-arms the tracker on a new turn. The pane may not repaint between a
// delivery and the next poll, so observations must not carry across the turn
// boundary.
func (p *paneIdleTracker) forTurn(epoch uint64) {
	if p == nil || p.epoch == epoch {
		return
	}
	p.epoch = epoch
	p.idleStreak = 0
	p.workingStreak = 0
}

// observe feeds one capture in and reports whether the session should now be
// marked idle. It fires once per run of idle observations: the caller marks the
// session idle, and the next turn re-arms it through forTurn.
func (p *paneIdleTracker) observe(lines []string) bool {
	if p == nil {
		return false
	}
	idle, known := paneShowsIdle(p.provider, lines)
	if !known || !idle {
		p.idleStreak = 0
		return false
	}
	p.idleStreak++
	return p.idleStreak == p.confirmationsNeeded()
}

// confirmationsNeeded is per-probe because false idle releases queued mail into
// a live turn, while false working costs only polling latency.
func (p *paneIdleTracker) confirmationsNeeded() int {
	if n := paneIdleProbes[p.provider].confirmations; n > 0 {
		return n
	}
	return paneIdleConfirmations
}

// observeWorking is the symmetric correction for a session already marked idle.
// It also requires consecutive frames so one half-painted frame cannot flap state.
func (p *paneIdleTracker) observeWorking(lines []string) bool {
	if p == nil {
		return false
	}
	working, known := paneShowsWorking(p.provider, lines)
	if !known || !working {
		p.workingStreak = 0
		return false
	}
	p.workingStreak++
	return p.workingStreak == p.confirmationsNeeded()
}

// reset forgets both streaks, so a session that went idle and was then given
// more work must be observed afresh in either direction.
func (p *paneIdleTracker) reset() {
	if p != nil {
		p.idleStreak = 0
		p.workingStreak = 0
	}
}

// paneIdleAction is what pollPaneIdle would do for one capture, before any
// tmux I/O. Tests drive this directly with literal pane fixtures.
type paneIdleAction int

const (
	paneIdleActionNone paneIdleAction = iota
	paneIdleActionMarkIdle
	paneIdleActionMarkRunning
)

func decidePaneIdlePoll(tracker *paneIdleTracker, status SessionStatus, lines []string) paneIdleAction {
	if tracker == nil {
		return paneIdleActionNone
	}
	if status.IsTerminal() {
		tracker.reset()
		return paneIdleActionNone
	}
	if status == StatusIdle {
		// Confirm working too; a single stray frame must not flap parent reports.
		if tracker.observeWorking(lines) {
			tracker.reset()
			return paneIdleActionMarkRunning
		}
		return paneIdleActionNone
	}
	if status != StatusRunning {
		tracker.reset()
		return paneIdleActionNone
	}
	if tracker.observe(lines) {
		return paneIdleActionMarkIdle
	}
	return paneIdleActionNone
}

// paneIdleCaptureLines is sized against the walks it feeds, not a typical
// footer. claudePaneJudge must find the composer, its upper rule, and
// claudePaneContentTailLines above that; too shallow a capture makes the judge
// unknown forever. Keep it equal to pasteVerifyLines because
// claudeComposerMaxLines is sized against the same capture depth.
const paneIdleCaptureLines = pasteVerifyLines

// pasteEvidence describes how a provider's pane shows that a paste arrived.
// This is separate from paneIdleProbe: a CLI can expose turn state but still not
// echo pasted text.
type pasteEvidence struct {
	// chipMarkers are what a CLI renders *instead of* the pasted text.
	// cursor-agent collapses a bracketed paste to "[Pasted text #N]", so the
	// characters never appear in the pane and looking for them always fails.
	chipMarkers []string
	// required means Enter waits for positive paste confirmation. False is
	// deliberate for providers whose paste evidence is silence rather than a
	// reliable negative; re-pasting on silence duplicates the message.
	required bool
}

// Non-required entries are not consulted in production, but they document chip
// evidence and keep pasteConfirmed covered for providers that do not echo text.
var pasteEvidenceProbes = map[string]pasteEvidence{
	ProviderCursor: {chipMarkers: []string{"[Pasted text"}},
	ProviderCodex:  {required: true},
	// Claude's composer is readable, so dropped pastes must be caught before
	// MarkRunning records a turn that never started. Keep the chip marker:
	// a chip still proves arrival, while a false negative re-pastes and
	// re-queues.
	ProviderClaude: {required: true, chipMarkers: []string{"[Pasted text"}},
}

// pasteVerifyRequired reports whether a paste must be confirmed before Enter.
func pasteVerifyRequired(provider string) bool {
	return pasteEvidenceProbes[provider].required
}

// pasteEvidenceObscured reports the one readable-but-silent shape: provider
// chrome is present, but the composer could not be located within it. Other
// unreadable captures remain negative because an empty or half-painted pane is
// what a dropped paste looks like.
func pasteEvidenceObscured(provider string, lines []string) bool {
	if !composerReadable(provider) {
		return false
	}
	if composerIdx, _ := claudeComposerIdx(lines); composerIdx >= 0 {
		return false
	}
	return searchedForComposer(lines)
}

// pasteConfirmed reports whether the pane shows the paste arrived, either as
// text or as a chip.
//
// If the composer is located, only that line may answer. A full-pane scan can
// match an older echoed prompt and then press Enter on an empty composer.
//
// If no composer can be scoped to, only the pane tail is scanned. The bound keeps
// transcript history out, while pasteConfirmTailLines is still large enough for
// wrapped deliveries because it is sized against pasteVerifyLines. The probe,
// not this depth, is what distinguishes neighboring deliveries.
//
// A residual remains: two first lines identical across the probe window still
// collide when the provider has no readable composer. Closing that needs pre-
// and post-paste evidence from the delivery path; the test documents the gap.
// The two arms are handed different anchors on purpose: a located composer shows
// the head of the first line, while the tail scan reads transcript rows that
// echo it whole. See confirmsComposer and pasteProbe.
func pasteConfirmed(provider string, lines []string, first, probe string) bool {
	if probe == "" {
		return true // nothing distinctive to look for
	}
	chips := pasteEvidenceProbes[provider].chipMarkers
	return scanForPaste(provider, lines,
		func(line string) bool { return confirmsComposer(line, first, chips) },
		func(line string) bool { return strings.Contains(line, probe) || containsAny(line, chips) })
}

// scanForPaste scopes both paste readers the same way: a located composer when
// readable, otherwise a bounded tail. The excluded region is transcript history
// where an older delivery can match the same probe.
func scanForPaste(provider string, lines []string, onComposer, onTail func(string) bool) bool {
	if composerReadable(provider) {
		if composerIdx, _ := claudeComposerIdx(lines); composerIdx >= 0 {
			return onComposer(lines[composerIdx])
		}
		// An unlocatable readable composer is silence, not a negative. Do not
		// fall back to transcript scanning, and let Courier.pasteVerdict avoid
		// the infinite re-paste loop on silence.
		return false
	}
	// No readable composer, so use the pane tail, never the whole capture.
	// Transcript echoes above it can falsely confirm a dropped paste. This uses
	// pasteConfirmTailLines, not paneIdleTailLines: the idle probe looks for
	// footer chrome, while paste confirmation must reach the first line of a
	// wrapped composer.
	seen := 0
	for i := len(lines) - 1; i >= 0 && seen < pasteConfirmTailLines; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		seen++
		if onTail(lines[i]) {
			return true
		}
	}
	return false
}

// pasteConfirmTailLines bounds pasteConfirmed's fallback. Like
// claudeComposerMaxLines, it is sized against pasteVerifyLines because the target
// is the top of a composer holding this delivery, and the composer grows with
// message length. It still leaves enough of the capture unread to keep transcript
// echoes from confirming a paste that never arrived.
const pasteConfirmTailLines = pasteVerifyLines - 8

// confirmsComposer checks a located composer line against the delivery's whole
// first line, not against pasteProbe. The two anchor at opposite ends: a
// composer wraps, so claudeComposerIdx returns its topmost row and tmux truncates
// that row at the pane width, leaving the HEAD of the line visible — while
// pasteProbe deliberately returns the TAIL. Matching a head-truncated row against
// a tail probe fails for every delivery whose first line exceeds the pane width,
// and for claude that false negative is not cosmetic: pasteVerifyRequired is
// true, so write's `default` branch pastes a second copy and re-queues, which is
// the duplicate-paste loop this package exists to prevent.
//
// The comparison is the same one-way rule composerHoldsThisDelivery uses: the
// visible body must be a prefix of what we sent. Narrow panes then read as
// confirmation rather than as a dropped paste.
func confirmsComposer(line, first string, chips []string) bool {
	if containsAny(line, chips) {
		return true
	}
	body := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), claudePromptGlyph))
	// An empty composer confirms nothing: that is what a dropped paste looks
	// like, and every string has the empty prefix.
	if body == "" {
		return false
	}
	return strings.HasPrefix(first, body)
}

// claudePromptGlyph is the composer prompt in claude-code's TUI, U+276F.
const claudePromptGlyph = "❯"

// composerReadable reports whether bramble can distinguish an empty composer
// from a draft. Only claude has a stable prompt glyph for that.
func composerReadable(provider string) bool {
	return provider == ProviderClaude
}

// composerDraftText reports whether claude's composer holds a draft and returns
// its text so a changed draft can restart the hold.
func composerDraftText(provider string, lines []string) (text string, draft, known bool) {
	if provider != ProviderClaude {
		return "", false, false
	}
	if composerIdx, _ := claudeComposerIdx(lines); composerIdx >= 0 {
		line := strings.TrimSpace(lines[composerIdx])
		draft, known = judgeComposerLine(line)
		if !known {
			return line, true, true
		}
		return line, draft, known
	}
	// The composer could not be located by the bounded walk.
	if searchedForComposer(lines) {
		// Position still says where the composer is. If that line has the glyph,
		// it is legible even though the upper region was not bounded.
		if line, ok := lineAboveStatusRule(lines); ok {
			if draft, known := judgeComposerLine(line); known {
				return strings.TrimSpace(line), draft, known
			}
		}
		// The composer region exists but cannot be read, often because a long draft
		// filled it. Fail closed as a hold: the bounded cost is composerHoldGrace,
		// while delivering here can submit a human draft with this message appended.
		return "", true, true
	}
	// No status rule means no claude chrome to scope; the tail scan is the only
	// reader left, and there is no lower chrome competing for those rows.
	forEachPaneTailLine(lines, func(line string) bool {
		if !strings.HasPrefix(strings.TrimSpace(line), claudePromptGlyph) {
			return false
		}
		text = strings.TrimSpace(line)
		draft, known = judgeComposerLine(line)
		return true
	})
	return text, draft, known
}

// lineAboveStatusRule returns the first non-empty line above the lowest status
// rule, where claude draws its live composer even if the upper rule is missing.
func lineAboveStatusRule(lines []string) (string, bool) {
	sepIdx := statusSepIdx(lines)
	if sepIdx < 0 {
		return "", false
	}
	for i := sepIdx - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		return lines[i], true
	}
	return "", false
}

// searchedForComposer reports whether claude's status separator is present. If
// not, claiming a draft would hold mail against a pane with no proven composer.
func searchedForComposer(lines []string) bool {
	return statusSepIdx(lines) >= 0
}

// judgeComposerLine reports whether one composer line holds a draft. TrimSpace
// is required because claude separates `❯` from the body with U+00A0; an
// ASCII-only trim would read every empty composer as a draft.
func judgeComposerLine(line string) (draft, known bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, claudePromptGlyph) {
		return false, false
	}
	body := strings.TrimSpace(strings.TrimPrefix(trimmed, claudePromptGlyph))
	if body == "" {
		return false, true
	}
	// Do not decide bramble provenance here; the prefix is user-controllable.
	// Only the courier knows what it staged for this recipient.
	return true, true
}
