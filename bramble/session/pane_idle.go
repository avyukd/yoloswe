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
	forEachPaneTailLine(lines, func(line string) bool {
		if containsAny(line, markers) {
			prompt = line
			return true
		}
		return false
	})
	if prompt != "" {
		return prompt, true
	}
	return "", false
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

// observe feeds one capture in and reports whether the session should now be
// marked idle. It fires once per run of idle observations: the caller marks the
// session idle, and anything that starts a new turn (a delivered message, which
// marks it running again) re-arms it.
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
	return p.streak == paneIdleConfirmations
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
		working, known := paneShowsWorking(tracker.provider, lines)
		if known && working {
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
