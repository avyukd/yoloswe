package session

import "strings"

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

// claudePaneJudge reads claude-code's pane through the parser that already
// backs the status line, rather than a second set of markers that could drift
// away from it.
//
// The critical case is the third one. A capture taken mid-turn frequently shows
// neither a spinner nor a prompt — just agent output — because the spinner is
// sub-second and was never caught once in 400+ samples of live monitoring (see
// .claude/memory/tmux-capture-learnings.md, caveat 3). Read as idle, that would
// release queued mail into a running turn, which is exactly what this whole
// table exists to prevent. So ambiguity reports known=false, and observe()
// resets the streak on it.
func claudePaneJudge(lines []string) (working, known bool) {
	ps := ParseClaudeStatusBar(lines)
	if ps == nil {
		return false, false // no separator: not claude's prompt, or still booting
	}
	switch {
	case ps.IsWorking:
		return true, true
	case ps.IsIdle:
		return false, true
	default:
		// Agent output above the separator, and no positive marker either way.
		return false, false
	}
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
	streak   int
	// epoch is the session turn the current streak was observed during, so
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
	p.streak = 0
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
		p.streak = 0
		return false
	}
	p.streak++
	return p.streak == p.confirmationsNeeded()
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
		p.streak = 0
		return false
	}
	p.streak++
	return p.streak == p.confirmationsNeeded()
}

// reset forgets the current streak, so a session that went idle and was then
// given more work must be observed idle afresh.
func (p *paneIdleTracker) reset() {
	if p != nil {
		p.streak = 0
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

var pasteEvidenceProbes = map[string]pasteEvidence{
	ProviderCursor: {chipMarkers: []string{"[Pasted text"}},
	ProviderCodex:  {required: true},
}

// pasteVerifyRequired reports whether a paste must be confirmed before Enter.
func pasteVerifyRequired(provider string) bool {
	return pasteEvidenceProbes[provider].required
}

// pasteConfirmed reports whether a captured pane shows the paste arrived,
// either by echoing the text or by rendering a chip in its place.
func pasteConfirmed(provider string, lines []string, probe string) bool {
	if probe == "" {
		return true // nothing distinctive to look for
	}
	chips := pasteEvidenceProbes[provider].chipMarkers
	for _, line := range lines {
		if strings.Contains(line, probe) {
			return true
		}
		if len(chips) > 0 && containsAny(line, chips) {
			return true
		}
	}
	return false
}

// claudePromptGlyph is the composer prompt in claude-code's TUI, U+276F.
const claudePromptGlyph = "❯"

// composerDraft reports whether the user has typed something into the composer
// that has not been submitted yet.
//
// Delivering into a non-empty composer appends to whatever the human was
// writing and then presses Enter, submitting their half-finished sentence
// wearing someone else's text. The idle probe cannot catch this: a composer
// holding a draft is still a composer, so the session reads as maximally idle
// at exactly the moment it is least safe to write to.
//
// known is false for a provider whose composer cannot be read, and callers must
// treat that as "no draft" — refusing to deliver into every pane we cannot
// parse would strand mail rather than protect it.
//
// Only claude is judged today. cursor and codex render *placeholder* text in an
// empty composer ("Add a follow-up", "Ask Codex to do anything") which
// disappears as soon as the user types, so a typed draft is indistinguishable
// from a CLI that has not finished booting. Reading those needs a positive
// anchor on the composer glyph, validated against a live capture first.
func composerDraft(provider string, lines []string) (draft, known bool) {
	if provider != ProviderClaude {
		return false, false
	}
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, claudePromptGlyph) {
			continue
		}
		// The glyph is separated from the text by a non-breaking space
		// (U+00A0), not an ordinary one -- see the fixtures in
		// TestEmptyClaudeComposerIsNotADraft. The TrimSpace above is what
		// handles it: it is Unicode-aware, so an empty composer is already bare
		// by the time the prefix comes off. Trimming again here would be
		// redundant, but an ASCII-only cutset anywhere in this path would leave
		// " " behind and read every empty composer as a draft, holding back
		// all mail on the host.
		return strings.TrimPrefix(line, claudePromptGlyph) != "", true
	}
	return false, false
}
