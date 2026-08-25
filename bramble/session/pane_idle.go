package session

import (
	"regexp"
	"strings"
)

// paneIdleProbe tells, from a tmux pane's text, whether an agent CLI is waiting
// for input.
//
// It exists for backends with no turn-completion hook and for hook-correction
// cases. Claude gets a Stop hook through --settings and Codex a notify program
// through -c notify=[...]. Cursor has neither: no --notify flag, and its plugin
// `stop` hook does not fire from the CLI in either interactive or --print mode
// (checked against cursor-agent 2026.08.11). Without some signal, bramble only
// ever learns such a session finished when its window dies — so nothing drains
// its queued mail and its parent is never told it is done.
//
// Codex also uses a pane probe, but only to correct premature notify-hook
// idles — its hook can fire before the pane shows idle.
//
// Reading the pane is a poor second to a hook and is treated as such: the probe
// must positively recognize the CLI's chrome before it will judge anything, and
// it reports "unknown" rather than guessing.
type paneIdleProbe struct {
	// judge, when set, replaces the substring matching below entirely. An
	// escape hatch for a CLI whose chrome cannot be expressed as "this string
	// on that line" — claude's needs anchored regexes over multi-byte glyphs
	// and a real notion of "the pane is ambiguous right now".
	judge func(lines []string) (working, known bool)
	// promptMarkers identify the composer line itself. Until one appears the
	// pane says nothing useful — the CLI may still be booting.
	promptMarkers []string
	// workingOnPrompt mean a turn is in flight when found on the composer line.
	// Cursor's "ctrl+c to stop" lives here; a fixed window of trailing lines
	// would sometimes miss it when the footer grows (plan mode adds a mode line)
	// and read a working session as idle — releasing queued mail into a live
	// turn.
	workingOnPrompt []string
	// workingInFooter mean a turn is in flight when found on a non-composer line
	// within paneIdleTailLines of the bottom. Codex shows "Working (… • esc to
	// interrupt)" on its own line above the composer; the tail bound keeps
	// scrollback that quotes these markers out of reach.
	workingInFooter []string
	// confirmations is how many consecutive idle observations this provider
	// needs before it is called idle. Zero means paneIdleConfirmations. Raised
	// for a CLI whose working chrome is often missing from a given frame, where
	// agreement has to be sustained before it means anything.
	confirmations int
	// correctsPrematureIdle marks a provider whose completion hook can fire
	// before its turn is really over, so the pane is worth reading even after
	// the session is already idle — the only way such a session ever gets back
	// to running. Off by default: for a hookless provider an idle session has
	// nothing to correct, and polling it would buy tmux I/O for every idle
	// session on the host and nothing else.
	correctsPrematureIdle bool
}

// paneIdleProbes holds a probe per provider that lacks a completion hook or
// needs pane evidence to correct a premature hook result.
//
// Deliberately not a fallback for every provider: a wrong "idle" is worse than
// no signal, because it releases queued messages into a live turn. A provider
// is listed only once its chrome has been checked against the real CLI.
var paneIdleProbes = map[string]paneIdleProbe{
	// cursor-agent's composer footer carries "ctrl+c to stop" for exactly as
	// long as a turn is running. "Add a follow-up" is NOT an idle marker — it
	// is shown while working too, which is the trap this table exists to
	// record.
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
	// Claude reports its own turn ends through a Stop hook, so this probe is
	// not how it normally goes idle — it is the fallback for a session whose
	// hook never arrived, which used to leave the session running forever with
	// its parent's mail undeliverable.
	//
	// Its chrome cannot be matched by substring: the markers are multi-byte
	// glyphs that must be anchored to the start of a line, and "no spinner" is
	// emphatically not "idle" (see claudePaneJudge). Hence the judge.
	//
	// correctsPrematureIdle stays off: the hook is not premature, and
	// captureRecentOutput already revives an idle claude session from the same
	// parser every 15s. Turning it on would re-read every idle claude pane on
	// the host every couple of seconds for nothing.
	ProviderClaude: {
		judge: claudePaneJudge,
		// Five, not two. Claude's working chrome is often absent from any given
		// frame, so agreement has to be sustained before it means anything.
		confirmations: 5,
	},
}

// claudeCompletionPastRe matches claude's finished-turn line: a sparkle glyph,
// a verb, and " for <duration>" ("✻ Baked for 3m 48s", "✻ Sautéed for 6m 16s",
// "✻ Crunched for 46s · 1 shell still running").
//
// The verb is matched as "any run of non-space characters", NOT as \w. Go's \w
// is ASCII-only, and claude's verb list is not: "Sautéed" failed to match, so
// three live idle sessions in the 2026-08-25 survey came back ambiguous and
// would never have drained their mail. Enumerating verbs has the same defect
// one level up — the list is claude's to change — so what is matched is the
// structural part: the " for <duration>" suffix, which is what actually
// distinguishes a finished turn from the "Baking…" gerund.
var claudeCompletionPastRe = regexp.MustCompile(`^[✻✢✽✹]\s+\S+ for\s+\d`)

// isClaudeSeparator matches either of claude's two horizontal rules.
//
// separatorRe alone is not enough: it requires ten unbroken box-drawing
// characters, and the *input* separator carries the `▪▪▪` mode indicator that
// breaks the run ("───────── ▪▪▪ ─"). Matching only the status separator is
// how the input separator went unfound, which left the content region
// undelimited.
func isClaudeSeparator(trimmed string) bool {
	return separatorRe.MatchString(trimmed) ||
		(strings.HasPrefix(trimmed, "─") && strings.Contains(trimmed, "▪"))
}

// claudeComposerIdx locates claude-code's live composer and the boundary of the
// content region above it.
//
// The layout, measured against 14 live claude panes on 2026-08-25, is:
//
//	<agent content>
//	────────────────  <- content boundary
//	❯ <composer>
//	────────────────  <- status separator
//	<cwd/branch/model status line>
//	<permissions line>
//
// Both rules are plain box-drawing runs. An earlier version of this function
// looked for a "─ ▪▪▪ ─" mode marker on the upper rule, taken from a fixture in
// tmux_test.go; that marker appeared in ZERO of the 14 live panes, so requiring
// it made the judge bail on every real session. The composer is therefore
// identified by position — the first non-empty line above the status separator
// — and the content boundary is the next rule above it, if any.
//
// Position, not glyph, is what makes this safe: claude renders every submitted
// transcript prompt with the same `❯`, so scanning for the lowest match can
// latch onto scrollback (window 26 of that survey had two `❯` lines, one of
// them a submitted `/clear`).
//
// contentEndIdx is -1 when no rule sits above the composer, which happens when
// a multi-line composer absorbs it. Callers must treat that as "content region
// unknown" rather than "no content".
func claudeComposerIdx(lines []string) (composerIdx, contentEndIdx int) {
	statusSepIdx := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if isClaudeSeparator(strings.TrimSpace(lines[i])) {
			statusSepIdx = i
			break
		}
	}
	if statusSepIdx < 0 {
		return -1, -1
	}
	// Walk up from the status separator to the rule above it. Everything
	// between the two rules is the composer, which may wrap onto several lines
	// — a long queued delivery does exactly that — so the composer line is the
	// FIRST of them, not the last. Taking the nearest line instead reads a
	// wrapped continuation as the composer, fails the `❯` prefix check, and
	// reports "unknown", which means deliver: a paste straight into the user's
	// draft, the harm this is here to prevent.
	composerIdx = -1
	seen := 0
	sawUpperRule := false
	for i := statusSepIdx - 1; i >= 0 && seen < claudeComposerMaxLines; i-- {
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
		// The walk ran out of lines without meeting the rule above the
		// composer, so the region is not bounded above and composerIdx is just
		// the topmost line reached — arbitrary transcript, not a located
		// composer. Claude Code runs on the alternate screen, where
		// capture-pane returns only the visible rows however deep -S goes, so a
		// composer taller than the window genuinely produces this.
		//
		// Report it unfound. Trusting it either delivers into an oversized
		// draft (the line lacks the glyph, so judgeComposerLine says "unknown",
		// which means deliver) or latches onto a submitted transcript prompt
		// and reports a draft that never clears.
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

// claudePaneJudge reads claude-code's pane to decide whether a turn is in
// flight.
//
// It deliberately does NOT use ParseClaudeStatusBar's IsIdle. That parser scans
// up from the status separator and stops at the first `❯` it meets — which is
// the composer, and the composer is on screen whether or not a turn is running.
// The repo's own fixture pins the consequence: tmux_test.go's "working with
// completion indicator" case has `✢ Fluttering… (4m 16s)` in flight and parses
// IsIdle:true. Judging claude that way calls a working session idle on
// essentially every frame and releases its queued mail into a live turn.
//
// What actually separates the two states was measured across 19 live claude
// panes on 2026-08-25: claude's *sparkle line*, and specifically its tense.
//
//	working   ✽ Baking… (1m 55s · ↓ 8.1k tokens)      gerund + ellipsis
//	          · Brewing… (25s · ↓ 1.5k tokens)
//	idle      ✻ Cogitated for 1m 27s                  past tense + "for <dur>"
//	          ✻ Baked for 3m 48s · 1 shell still running
//
// Everything else in the content region — prose, a `⎿` tip, a `※ recap`, a
// table — appears in both states and says nothing. That is why this reads the
// nearest *sparkle* line rather than the nearest line: an earlier version
// demanded a marker on the topmost content line, and since an idle session's
// last line is usually the tail of its own answer, every real idle pane came
// back ambiguous and the fallback for a stranded window never fired.
//
// A `●` tool line still counts as work, but only when no sparkle line sits
// below it: a finished turn leaves its tool output on screen above the
// completion line.
//
// Ambiguity — no sparkle line at all within the bounded tail — reports
// known=false, and observe() resets the streak on it. That is load-bearing: the
// spinner is sub-second and was never caught once in 400+ samples of live
// monitoring (.claude/memory/tmux-capture-learnings.md, caveat 3), so "no
// marker" must never be read as idleness on its own.
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

	// Walk a bounded tail of the content region upward and take the verdict of
	// the first line that carries one — but stop at a submitted prompt.
	//
	// Claude echoes every submitted prompt into the transcript with the same
	// `❯` glyph (see claudeComposerIdx's doc), so such a line is the boundary
	// of the current turn: everything above it belongs to a previous one. A
	// completion line persists and is pushed up by later output, so without
	// this stop the seconds right after bramble writes a delivery read the
	// PREVIOUS turn's "✻ Worked for …" as this turn's verdict. The spinner is
	// usually absent from any given frame (caveat 3), forTurn resets the streak
	// at exactly that boundary, and five confirmations is ~10s — comfortably
	// inside the window before the new turn has produced four content lines.
	// The result would be SetSessionIdle on a live turn, which releases queued
	// mail into it and reports a spurious idle to the parent: precisely the two
	// harms this probe exists to prevent.
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

// claudeComposerMaxLines bounds the composer walk. Without a bound, a capture
// with no upper rule walks to the top and calls arbitrary transcript the
// composer.
//
// Sized against the capture, not against a typical composer. The walk is what
// separates "a composer taller than this" from "not a composer at all", and a
// composer genuinely does grow past a handful of lines — a wrapped 500-char
// delivery, or a long human draft, does it routinely. At 6 those ordinary panes
// came back unfound, and every consumer of unfound then failed in the unsafe
// direction: the paste check re-pasted in a loop, and the draft check delivered
// into the draft. Both are now fail-safe on their own (see pasteEvidenceObscured and
// composerDraftText), but the bound should not be manufacturing the case in the
// first place, so it is the capture depth the walk actually gets — deliveries
// capture pasteVerifyLines — less the few rows of chrome below the composer.
const claudeComposerMaxLines = pasteVerifyLines - 6

// claudePaneContentTailLines bounds how far up the content region is read.
// Deep enough that a sparkle line pushed up by a recap or a tip is still found
// — the widest gap measured live was two lines — shallow enough that a sparkle
// line quoted in the transcript is out of reach.
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
	// idleStreak and workingStreak count the two directions separately.
	//
	// One shared counter cannot serve both. observe() fires by equality
	// (streak == confirmationsNeeded) and does not reset on firing, so after a
	// pane-driven idle the counter sits exactly at the target; every later
	// working frame then increments past it and observeWorking's equality can
	// never be true again. Codex — the only correctsPrematureIdle provider —
	// would stay wedged idle for the rest of the turn, which is the very wedge
	// the correction exists to undo.
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

// forTurn re-arms the tracker when the session has been started on a new turn.
//
// The monitor cannot see that boundary in the pane. A delivery is written while
// the recipient is idle and marks it running again between two polls, so
// without this the poll after the write extends the streak the poll before it
// began — a frame the CLI has not repainted yet would then be counted towards
// calling the new turn idle. It is also what re-arms a tracker whose streak has
// already fired: a turn short enough that no poll catches its working chrome
// would otherwise leave the streak counting past the confirmation count
// forever, and the session would never be seen to go idle again.
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

// confirmationsNeeded is how many consecutive idle observations this provider
// needs. Per-probe because the cost of being wrong is not symmetric: calling a
// working session idle releases queued mail into a live turn, while calling an
// idle one working costs only a poll of latency.
func (p *paneIdleTracker) confirmationsNeeded() int {
	if n := paneIdleProbes[p.provider].confirmations; n > 0 {
		return n
	}
	return paneIdleConfirmations
}

// observeWorking feeds one capture in and reports whether a session already
// marked idle should be put back to running. Symmetric with observe: the same
// number of consecutive agreeing frames, for the same reason — one half-painted
// frame must not drive a state change on its own.
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
		// Confirmed, like the idle direction. A single stray frame showing
		// working chrome used to resurrect the session on the spot, while
		// getting back to idle needed two — and every resurrection re-arms idle
		// reporting, so a flapping pane sent the parent one report per flap.
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

// paneIdleCaptureLines is how much scrollback the monitor pulls per poll. Small
// on purpose: only the footer is read, and this runs every couple of seconds
// for every session with a hookless or hook-correcting probe.
const paneIdleCaptureLines = 12

// pasteEvidence describes how a provider's pane shows that a paste arrived.
//
// Separate from paneIdleProbe because the questions are different: that table
// asks "is a turn in flight", this one asks "did my text reach the composer".
// A CLI can be perfectly readable for one and opaque for the other — cursor
// announces its turns clearly but never echoes a paste.
type pasteEvidence struct {
	// chipMarkers are what a CLI renders *instead of* the pasted text.
	// cursor-agent collapses a bracketed paste to "[Pasted text #N]", so the
	// characters never appear in the pane and looking for them always fails.
	chipMarkers []string
	// required says a paste must be positively confirmed before Enter is sent.
	// True only for codex, whose TUI drops a paste that arrives while the
	// previous turn is finalizing — the case this verification exists for.
	//
	// False everywhere else on purpose. tmux reports whether paste-buffer
	// succeeded, so for a provider whose chrome we cannot read an empty pane
	// scrape is silence, not a negative, and re-pasting on silence puts the
	// message in the composer twice.
	required bool
}

// Only an entry with required:true is consulted in production: the courier
// checks pasteVerifyRequired before it probes, so a non-required provider's
// markers are never read. Cursor's entry is kept anyway — it records the fact
// that made this table necessary (cursor renders a chip, so scanning for the
// pasted characters can never succeed), and it is what pasteConfirmed is tested
// against. If cursor ever needs required:true, the markers are already right.
var pasteEvidenceProbes = map[string]pasteEvidence{
	ProviderCursor: {chipMarkers: []string{"[Pasted text"}},
	ProviderCodex:  {required: true},
	// Claude echoes a pasted message into its composer verbatim — measured
	// across 13 live panes on 2026-08-25, every one showed the text itself and
	// not one showed a "[Pasted text …]" chip, including a pane holding a real
	// bramble delivery. So pasteConfirmed's prefix scan works here, and the
	// false-negative risk that makes verification unsafe for an unreadable CLI
	// does not apply.
	//
	// It has to be required, because this is the same composer composerDraftText
	// reads: the rest of this change rests on claude's composer being legible,
	// so declining to check it would mean a paste claude's TUI dropped goes
	// unnoticed — deliver() writes directly rather than queueing, so the
	// message is lost and MarkRunning wedges the session on a turn that never
	// started.
	//
	// The chip marker is carried anyway. The measurement above is of the
	// deliveries seen, not of every delivery possible: formatSubagentReport
	// emits two or three lines once ErrorMsg or a result path is set, and a
	// large enough paste collapses to a chip in claude too. Accepting a chip
	// cannot produce a false positive that matters — a chip in the pane means
	// the paste reached the composer, which is exactly what is being asked —
	// while a false NEGATIVE here re-pastes and re-queues, which is the loop
	// section 1 of this PR exists to remove.
	ProviderClaude: {required: true, chipMarkers: []string{"[Pasted text"}},
}

// pasteVerifyRequired reports whether a paste must be confirmed before Enter.
func pasteVerifyRequired(provider string) bool {
	return pasteEvidenceProbes[provider].required
}

// pasteEvidenceObscured reports the one shape in which a capture says nothing
// about a paste rather than denying it: this provider's chrome is on screen —
// so the pane is painted and the CLI is up — yet the composer could not be
// located within it, which is what a composer taller than the walk looks like.
//
// Every other unreadable capture stays a negative. An empty or half-painted
// pane is exactly what a dropped paste looks like, and calling that silence
// would press Enter on an empty prompt: a turn starts with no message, the
// delivery is dropped as sent, and MarkRunning wedges the session.
func pasteEvidenceObscured(provider string, lines []string) bool {
	if !composerReadable(provider) {
		return false
	}
	if composerIdx, _ := claudeComposerIdx(lines); composerIdx >= 0 {
		return false
	}
	return searchedForComposer(lines)
}

// pasteConfirmed reports whether a captured pane shows the paste arrived,
// either by echoing the text or by rendering a chip in its place.
//
// Where the composer can be located, ONLY the composer is read. A capture is 40
// lines deep and an agent echoes every submitted prompt into its transcript, so
// a scan of the whole pane is confirmed by a previous delivery of the same text
// — and for a provider whose verdict actually gates Enter (claude, codex) that
// means pressing Enter on an empty composer: the message is lost and
// MarkRunning wedges the session on a turn that never started, which is the
// failure this check exists to prevent.
//
// Where it cannot be located, the whole capture is scanned as before. That is
// the weaker test, but for a CLI whose chrome bramble cannot read it is the
// only one available, and those providers are not required:true.
func pasteConfirmed(provider string, lines []string, probe string) bool {
	if probe == "" {
		return true // nothing distinctive to look for
	}
	chips := pasteEvidenceProbes[provider].chipMarkers
	confirms := func(line string) bool {
		return strings.Contains(line, probe) || containsAny(line, chips)
	}
	if composerReadable(provider) {
		if composerIdx, _ := claudeComposerIdx(lines); composerIdx >= 0 {
			return confirms(lines[composerIdx])
		}
		// The composer could not be located, so there is nothing to read. Do
		// not fall back to the whole-pane scan here: this provider's verdict
		// gates Enter, and a transcript match would submit an empty composer.
		//
		// Nor is this a negative. Callers must treat an unlocatable composer as
		// unreadable rather than as "the paste is absent" — see
		// Courier.pasteVerdict, which stops re-pasting once the pane has stopped
		// being legible. Answering false here and letting a caller read it as a
		// negative is what re-pasted a message on every retry forever.
		return false
	}
	for _, line := range lines {
		if confirms(line) {
			return true
		}
	}
	return false
}

// claudePromptGlyph is the composer prompt in claude-code's TUI, U+276F.
const claudePromptGlyph = "❯"

// composerReadable reports whether bramble can read this provider's composer.
//
// Only claude: its `❯` is a real prompt glyph followed by U+00A0, so an empty
// composer is distinguishable from one holding a draft. Cursor and codex render
// placeholder text that disappears once the user types, which makes a draft
// indistinguishable from a CLI still booting.
//
// Kept beside the probe tables because it answers the same question they do,
// and callers use it to skip work rather than to interpret a capture.
func composerReadable(provider string) bool {
	return provider == ProviderClaude
}

// composerDraft reports whether claude's composer holds text the user has
// typed but not yet submitted.
//
// A thin wrapper over composerDraftText, which is what production calls. It
// exists so the draft-detection tests read as the question they are asking; the
// body lives in one place because both copies carried the same non-obvious
// safety rules (the located-but-unreadable composer that reports a hold, and
// the bounded tail fallback), and two copies of a safety rule is one too many.
func composerDraft(provider string, lines []string) (draft, known bool) {
	_, draft, known = composerDraftText(provider, lines)
	return draft, known
}

// composerDraftText is composerDraft plus the draft's text, for callers that
// must tell one draft from another — a hold restarts when the text changes,
// since a changing draft means somebody is still typing.
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
		// The walk failed, but position still says where the composer must be:
		// claude draws it immediately above the status rule. Read that line
		// directly. When it carries the glyph the composer IS legible — the
		// walk only failed to bound the region above — and its verdict is the
		// real one, which is what keeps an empty composer under an unbounded
		// region from holding mail forever.
		if line, ok := lineAboveStatusRule(lines); ok {
			if draft, known := judgeComposerLine(line); known {
				return strings.TrimSpace(line), draft, known
			}
		}
		// No glyph there either: something is in the composer region this
		// parser cannot read, and the commonest reason is a composer holding
		// many lines — precisely a long draft, the case most worth protecting.
		//
		// The tail scan that used to run here read the last paneIdleTailLines
		// rows, three of which are chrome (status rule, status line,
		// permissions line) on a real pane. A composer taller than the
		// remainder never showed its `❯` line, so the scan reported
		// known=false — which means deliver — straight into that draft.
		//
		// Report a hold instead. The cost of being wrong is bounded and
		// visible: a delivery waits out composerHoldGrace and is then written
		// anyway with a logged warning. The cost of the other error is a
		// human's half-written sentence submitted with a message stapled to
		// it, which is unrecoverable.
		return "", true, true
	}
	// No status rule anywhere in the capture, so this is not the bounded-walk
	// failure above — it is a capture with no claude chrome to walk at all: a
	// bare fixture, a pane mid-redraw, a CLI still booting. The tail scan is
	// the only reader left, and it is sound here precisely because there is no
	// chrome below the composer competing for those rows.
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
// rule — where claude always draws its live composer, whether or not the rule
// bounding the region above it survived the capture.
//
// Position is the whole point: claude renders every submitted transcript prompt
// with the same `❯`, so a scan that takes the nearest glyph latches onto
// scrollback and reports a draft that never clears.
func lineAboveStatusRule(lines []string) (string, bool) {
	statusSepIdx := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if isClaudeSeparator(strings.TrimSpace(lines[i])) {
			statusSepIdx = i
			break
		}
	}
	if statusSepIdx < 0 {
		return "", false
	}
	for i := statusSepIdx - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		return lines[i], true
	}
	return "", false
}

// searchedForComposer reports whether the capture is one where claude's
// composer should have been findable — i.e. it shows the status separator the
// walk starts from. Without that rule the pane is not claude chrome at all
// (a booting CLI, a cleared screen, a capture that raced a redraw), and
// claiming a draft there would hold mail against a pane that has no composer.
func searchedForComposer(lines []string) bool {
	for i := len(lines) - 1; i >= 0; i-- {
		if isClaudeSeparator(strings.TrimSpace(lines[i])) {
			return true
		}
	}
	return false
}

// judgeComposerLine reports whether one composer line holds a draft.
//
// The glyph is separated from the text by a non-breaking space (U+00A0), not an
// ordinary one -- see the fixtures in TestEmptyClaudeComposerIsNotADraft.
// TrimSpace is what handles it: it is Unicode-aware, so an empty composer is
// already bare by the time the prefix comes off. An ASCII-only cutset anywhere
// in this path would leave "\u00a0" behind and read every empty composer as a
// draft, holding back all mail on the host.
func judgeComposerLine(line string) (draft, known bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, claudePromptGlyph) {
		return false, false
	}
	body := strings.TrimSpace(strings.TrimPrefix(trimmed, claudePromptGlyph))
	if body == "" {
		return false, true
	}
	// Whether the text is bramble's own staged delivery is deliberately NOT
	// decided here. The prefix alone is user-controllable — anyone can type
	// "[bramble] ..." into their composer — so only the courier, which knows
	// what it actually queued for this recipient, can tell its own message from
	// a draft that merely looks like one. See composerHoldsThisDelivery.
	return true, true
}
