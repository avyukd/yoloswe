package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// codexPane renders codex's footer chrome. working=true adds the status line
// codex shows above the composer while a turn runs.
func codexPane(working bool, transcript ...string) []string {
	lines := append([]string{}, transcript...)
	if working {
		lines = append(lines, "  ◦ Working (2m 29s • esc to interrupt)")
	}
	return append(lines,
		"  › Ask Codex to do anything",
		"  gpt-5.4 · /tmp/wt",
	)
}

// cursorPane renders the footer cursor-agent keeps at the bottom of its pane.
// working=true adds the hint it shows for exactly as long as a turn runs.
func cursorPane(working bool, transcript ...string) []string {
	return cursorPaneMode(working, false, transcript...)
}

// cursorPaneMode renders the footer with or without the extra mode line cursor
// shows in plan mode, which is what a codetalk subagent runs in.
func cursorPaneMode(working, planMode bool, transcript ...string) []string {
	lines := append([]string{}, transcript...)
	prompt := "  → Add a follow-up"
	if working {
		prompt += "                    ctrl+c to stop"
	}
	lines = append(lines, prompt)
	if planMode {
		lines = append(lines, "  Plan (shift+tab to cycle)")
	}
	return append(lines,
		"  Composer 2.5 · 7.6%                    Run Everything",
		"  /tmp/wt · master",
	)
}

// TestCursorPaneWorkingIsNotIdle guards the trap this probe exists to record.
//
// "Add a follow-up" is present the whole time, working or not — reading it as
// an idle marker would release queued mail into a live turn, which is exactly
// what the queue is for. Only "ctrl+c to stop" distinguishes the two.
func TestCursorPaneWorkingIsNotIdle(t *testing.T) {
	t.Parallel()

	idle, known := paneShowsIdle(ProviderCursor, cursorPane(true, "  1", "  2"))
	require.True(t, known, "the footer is present, so the pane is readable")
	assert.False(t, idle, "a running turn must never read as idle")
}

func TestCursorPaneIdleIsIdle(t *testing.T) {
	t.Parallel()

	idle, known := paneShowsIdle(ProviderCursor, cursorPane(false, "  30"))
	require.True(t, known)
	assert.True(t, idle)
}

// TestUnpaintedPaneIsUnknown keeps a still-booting CLI from being called idle
// before it has shown a prompt at all.
func TestUnpaintedPaneIsUnknown(t *testing.T) {
	t.Parallel()

	_, known := paneShowsIdle(ProviderCursor, []string{"", "loading...", ""})
	assert.False(t, known, "a pane with no recognizable chrome tells us nothing")
}

// TestProvidersWithHooksHaveNoProbe: claude reports its own turn ends, and a
// second, weaker signal could only contradict it. Codex has a hook too, but its
// probe corrects premature hook idles — see pane_idle.go.
func TestProvidersWithHooksHaveNoProbe(t *testing.T) {
	t.Parallel()

	assert.False(t, providerHasIdleProbe(ProviderClaude))
	assert.Nil(t, newPaneIdleTracker(ProviderClaude))

	assert.True(t, providerHasIdleProbe(ProviderCodex))
	assert.NotNil(t, newPaneIdleTracker(ProviderCodex))
	assert.True(t, providerHasIdleProbe(ProviderCursor))
	assert.NotNil(t, newPaneIdleTracker(ProviderCursor))
}

// TestTrackerNeedsConsecutiveObservations stops one half-painted frame from
// releasing queued mail.
func TestTrackerNeedsConsecutiveObservations(t *testing.T) {
	t.Parallel()

	tr := newPaneIdleTracker(ProviderCursor)
	assert.False(t, tr.observe(cursorPane(false)), "one observation is not enough")
	assert.True(t, tr.observe(cursorPane(false)), "two in a row means idle")
}

// TestTrackerStreakResetsWhenWorkResumes: a flicker back to working must
// restart the count, not carry it.
func TestTrackerStreakResetsWhenWorkResumes(t *testing.T) {
	t.Parallel()

	tr := newPaneIdleTracker(ProviderCursor)
	require.False(t, tr.observe(cursorPane(false)))
	require.False(t, tr.observe(cursorPane(true)), "still working")
	assert.False(t, tr.observe(cursorPane(false)), "the streak restarted")
	assert.True(t, tr.observe(cursorPane(false)))
}

// TestTrackerFiresOnceUntilReset pins the transition being edge-triggered: the
// monitor marks the session idle once, and only new work re-arms it.
func TestTrackerFiresOnceUntilReset(t *testing.T) {
	t.Parallel()

	tr := newPaneIdleTracker(ProviderCursor)
	require.False(t, tr.observe(cursorPane(false)))
	require.True(t, tr.observe(cursorPane(false)))
	assert.False(t, tr.observe(cursorPane(false)), "no repeat while it stays idle")

	// A delivered message starts a turn; the monitor resets the tracker.
	tr.reset()
	require.False(t, tr.observe(cursorPane(false)))
	assert.True(t, tr.observe(cursorPane(false)), "a fresh idle is reported again")
}

// TestProbeReadsOnlyTheFooter keeps the agent from talking its own session into
// a state change by quoting the marker back.
func TestProbeReadsOnlyTheFooter(t *testing.T) {
	t.Parallel()

	transcript := make([]string, 0, 20)
	for i := 0; i < 12; i++ {
		transcript = append(transcript, "  the hint is: ctrl+c to stop")
	}
	idle, known := paneShowsIdle(ProviderCursor, cursorPane(false, transcript...))
	require.True(t, known)
	assert.True(t, idle, "an old line in the transcript must not read as working")
}

// TestNilTrackerIsInert: providers with hooks get a nil tracker, and the
// monitor calls straight through it.
func TestNilTrackerIsInert(t *testing.T) {
	t.Parallel()

	var tr *paneIdleTracker
	assert.False(t, tr.observe(cursorPane(false)))
	assert.NotPanics(t, func() { tr.reset() })
}

// TestCursorPlanModeWorkingIsNotIdle is the case that a fixed trailing-lines
// window got wrong. A codetalk subagent runs cursor in plan mode, which adds a
// mode line to the footer and pushes "ctrl+c to stop" further from the bottom.
// Reading a window of trailing lines would miss it and call a running turn
// idle — releasing queued mail into it. The hint is looked for on the composer
// line itself, so footer height does not matter.
func TestCursorPlanModeWorkingIsNotIdle(t *testing.T) {
	t.Parallel()

	idle, known := paneShowsIdle(ProviderCursor, cursorPaneMode(true, true, "  thinking"))
	require.True(t, known, "the composer line is present")
	assert.False(t, idle, "a running turn in plan mode must not read as idle")
}

func TestCursorPlanModeIdleIsIdle(t *testing.T) {
	t.Parallel()

	idle, known := paneShowsIdle(ProviderCursor, cursorPaneMode(false, true, "  done"))
	require.True(t, known)
	assert.True(t, idle)
}

func TestCodexPaneWorkingIsNotIdle(t *testing.T) {
	t.Parallel()

	idle, known := paneShowsIdle(ProviderCodex, codexPane(true, "  running a subagent review"))
	require.True(t, known, "the composer line is present")
	assert.False(t, idle, "a running turn must never read as idle")
}

func TestCodexPaneIdleIsIdle(t *testing.T) {
	t.Parallel()

	idle, known := paneShowsIdle(ProviderCodex, codexPane(false, "  done"))
	require.True(t, known)
	assert.True(t, idle)
}

// TestCodexFooterWorkingMarkerNotOnComposer: the working hint is on its own
// line above the composer, not on "Ask Codex to do anything".
func TestCodexFooterWorkingMarkerNotOnComposer(t *testing.T) {
	t.Parallel()

	working, known := paneShowsWorking(ProviderCodex, codexPane(true))
	require.True(t, known)
	assert.True(t, working)

	probe := paneIdleProbes[ProviderCodex]
	prompt, ok := findPromptLine(codexPane(true), probe.promptMarkers)
	require.True(t, ok)
	assert.False(t, containsAny(prompt, probe.workingInFooter))
}

// TestCodexTranscriptDoesNotReadAsWorking keeps scrollback that quotes the
// working line from resurrecting an idle session.
func TestCodexTranscriptDoesNotReadAsWorking(t *testing.T) {
	t.Parallel()

	transcript := []string{"  old: Working (8s • esc to interrupt)"}
	for i := 0; i < 12; i++ {
		transcript = append(transcript, "  line of output")
	}
	working, known := paneShowsWorking(ProviderCodex, codexPane(false, transcript...))
	require.True(t, known)
	assert.False(t, working, "a quoted line in scrollback must not read as working")
}

func TestCodexPrematureIdleReturnsToRunning(t *testing.T) {
	t.Parallel()

	tr := newPaneIdleTracker(ProviderCodex)
	action := decidePaneIdlePoll(tr, StatusIdle, codexPane(true, "  still going"))
	assert.Equal(t, paneIdleActionMarkRunning, action)
}

func TestCodexGenuinelyIdleStaysIdle(t *testing.T) {
	t.Parallel()

	tr := newPaneIdleTracker(ProviderCodex)
	action := decidePaneIdlePoll(tr, StatusIdle, codexPane(false, "  all done"))
	assert.Equal(t, paneIdleActionNone, action)
}

func TestTerminalSessionNeverResurrectedByProbe(t *testing.T) {
	t.Parallel()

	for _, status := range []SessionStatus{StatusCompleted, StatusFailed, StatusStopped} {
		tr := newPaneIdleTracker(ProviderCodex)
		action := decidePaneIdlePoll(tr, status, codexPane(true))
		assert.Equal(t, paneIdleActionNone, action, "status %s", status)
	}
}

// TestOnlyHookCorrectingProvidersArePolledWhileIdle pins which providers the
// monitor reads a pane for after a session is already idle. That poll exists
// solely to undo a hook that fired early, so a provider without one buys tmux
// I/O for every idle session on the host and learns nothing. Cursor has no hook
// at all — an idle cursor session has nothing to correct.
func TestOnlyHookCorrectingProvidersArePolledWhileIdle(t *testing.T) {
	t.Parallel()

	assert.True(t, newPaneIdleTracker(ProviderCodex).correctsPrematureIdle(),
		"codex's notify hook fires early; its pane must still be read when idle")
	assert.False(t, newPaneIdleTracker(ProviderCursor).correctsPrematureIdle(),
		"cursor has no hook to correct; polling it while idle is pure cost")
	assert.False(t, newPaneIdleTracker(ProviderClaude).correctsPrematureIdle(),
		"a provider with no probe at all has no tracker to ask")
}

// TestPaneIdleTrackerComesFromTheStoredModel pins the input the re-adopt path
// has to work from. monitorTrackedTmuxWindow never sees a resolved agent model
// — only the model string the session was persisted with — so if that string
// does not yield a provider, a cursor session that survives a bramble restart
// gets no idle signal at all and its parent is never told it finished.
func TestPaneIdleTrackerComesFromTheStoredModel(t *testing.T) {
	t.Parallel()

	m := NewManagerWithConfig(ManagerConfig{RepoName: "repo"})
	defer m.Close()

	assert.NotNil(t, m.newPaneIdleTrackerForModel("composer-3", ""),
		"a stored cursor model must still produce a pane-idle tracker")
	assert.Nil(t, m.newPaneIdleTrackerForModel("sonnet", ""),
		"claude reports its own turn ends; a second signal could only contradict it")
	assert.Nil(t, m.newPaneIdleTrackerForModel("not-a-model", ""),
		"an unresolvable model is not grounds for guessing at a pane's chrome")
}

// TestPaneIdleTrackerUsesTheSessionBackend covers the case the two features
// only create together: a session started with an explicit --backend carries a
// third-party model id the curated registry has never heard of, so resolving on
// the model alone yields nothing. Without the backend the re-adopt path would
// hand back a nil tracker for a hookless backend — the exact silent
// never-seen-to-finish failure the tracker exists to prevent, reachable only
// once per-session endpoints made unrecognized model ids legal.
func TestPaneIdleTrackerUsesTheSessionBackend(t *testing.T) {
	t.Parallel()

	m := NewManagerWithConfig(ManagerConfig{RepoName: "repo"})
	defer m.Close()

	const thirdPartyModel = "stealth/ox-alpha"

	assert.Nil(t, m.newPaneIdleTrackerForModel(thirdPartyModel, ""),
		"precondition: the model alone does not resolve, which is why the backend has to travel with it")
	assert.NotNil(t, m.newPaneIdleTrackerForModel(thirdPartyModel, ProviderCursor),
		"an explicit backend names the provider the model cannot")
	assert.Nil(t, m.newPaneIdleTrackerForModel(thirdPartyModel, ProviderClaude),
		"a backend that reports its own turn ends still gets no pane probe")
}

// TestTrackerDoesNotCarryObservationsAcrossATurn is the boundary the monitor
// cannot see in the pane. A delivery is written while the recipient is idle and
// marks it running again between two polls, so an idle frame observed before
// the write must not count towards calling the turn that write started idle —
// the CLI has not necessarily repainted yet.
func TestTrackerDoesNotCarryObservationsAcrossATurn(t *testing.T) {
	t.Parallel()

	tr := newPaneIdleTracker(ProviderCursor)
	tr.forTurn(1)
	require.False(t, tr.observe(cursorPane(false)), "one observation is never enough")

	// A message is delivered; the session is marked running again.
	tr.forTurn(2)
	assert.False(t, tr.observe(cursorPane(false)),
		"the frame before the delivery must not be counted towards the new turn")
	assert.True(t, tr.observe(cursorPane(false)), "two fresh observations agree")
}

// TestTrackerRearmsForEveryTurn keeps a turn too short to be caught working
// from latching the session as never-idle-again: the streak counts past the
// confirmation count, and only a new turn brings it back.
func TestTrackerRearmsForEveryTurn(t *testing.T) {
	t.Parallel()

	tr := newPaneIdleTracker(ProviderCursor)
	tr.forTurn(1)
	require.False(t, tr.observe(cursorPane(false)))
	require.True(t, tr.observe(cursorPane(false)), "the first turn is seen to end")
	require.False(t, tr.observe(cursorPane(false)), "it fires once per run of observations")

	// A second turn runs and finishes between polls, so no working frame is ever
	// captured — the only signal that it happened is the turn bump.
	tr.forTurn(2)
	assert.False(t, tr.observe(cursorPane(false)))
	assert.True(t, tr.observe(cursorPane(false)), "the second turn was never seen to end")
}
