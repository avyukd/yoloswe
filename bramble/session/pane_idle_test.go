package session

import (
	"strings"
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

// TestEveryTmuxProviderHasAProbe. Claude's is a fallback, not a second opinion:
// its hook is authoritative when it arrives, but when it does not — the socket
// moved, the window outlived its TUI — nothing else could ever mark the session
// idle, so its parent's mail was undeliverable forever. The probe only ever
// adds an idle that would otherwise never come; it cannot contradict a hook,
// because a hook that fired already moved the session on.
func TestEveryTmuxProviderHasAProbe(t *testing.T) {
	t.Parallel()

	for _, provider := range []string{ProviderClaude, ProviderCodex, ProviderCursor} {
		assert.True(t, providerHasIdleProbe(provider), "provider %q", provider)
		assert.NotNil(t, newPaneIdleTracker(provider), "provider %q", provider)
	}

	// Only codex's hook fires early enough to need correcting.
	assert.True(t, newPaneIdleTracker(ProviderCodex).correctsPrematureIdle())
	assert.False(t, newPaneIdleTracker(ProviderClaude).correctsPrematureIdle(),
		"claude's hook is not premature; re-reading every idle claude pane buys nothing")
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
	pane := codexPane(true, "  still going")

	// Confirmed, like the idle direction: one frame is not a state change.
	require.Equal(t, paneIdleActionNone, decidePaneIdlePoll(tr, StatusIdle, pane),
		"one observation is not enough to resurrect a session")
	assert.Equal(t, paneIdleActionMarkRunning, decidePaneIdlePoll(tr, StatusIdle, pane),
		"two in a row means the turn really is still running")
}

// TestStrayWorkingFrameDoesNotResurrect is why the correction is confirmed. It
// used to fire on a single frame while going idle needed two, and because every
// resurrection re-arms idle reporting, a pane flapping around the marker sent
// the parent one report per flap.
func TestStrayWorkingFrameDoesNotResurrect(t *testing.T) {
	t.Parallel()

	tr := newPaneIdleTracker(ProviderCodex)
	require.Equal(t, paneIdleActionNone,
		decidePaneIdlePoll(tr, StatusIdle, codexPane(true, "  a half-painted frame")))
	// The next frame shows it really was idle, so the streak dies with it.
	require.Equal(t, paneIdleActionNone,
		decidePaneIdlePoll(tr, StatusIdle, codexPane(false, "  all done")))
	assert.Equal(t, paneIdleActionNone,
		decidePaneIdlePoll(tr, StatusIdle, codexPane(true, "  another stray")),
		"the count restarted, so a lone frame still cannot resurrect")
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
	assert.NotNil(t, m.newPaneIdleTrackerForModel("sonnet", ""),
		"claude needs the fallback too: a window whose hook cannot reach bramble has no other way to be seen idle")
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
	assert.NotNil(t, m.newPaneIdleTrackerForModel(thirdPartyModel, ProviderClaude),
		"an explicit backend names the provider for claude too")
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

// claudePane renders claude-code's pane as it really appears, measured against
// 19 live panes on 2026-08-25:
//
//	<transcript>
//	<state>            <- the line that decides idle vs working
//	────────────────   <- content boundary
//	❯ <composer>
//	────────────────   <- status separator
//	<info line>
//	<permissions line>
//
// Both rules are plain runs of ─. The "─ ▪▪▪ ─" mode marker that an earlier
// version of this helper emitted appeared in ZERO of those panes; requiring it
// made the judge bail on every real session, so it is not written here.
//
// The composer is always drawn, because claude always draws it, whether or not
// a turn is running — that is the whole reason the composer cannot be used to
// judge idleness.
//
// state is the nearest content line. It is never empty in production:
// CaptureTmuxPane drops blank lines and a session that has run a turn always
// has a transcript, so a pane whose content region is empty is not a state the
// probe can encounter. Tests that want "idle" pass a completion line.
func claudePane(state string, transcript ...string) []string {
	return claudePaneComposer("❯ ", state, transcript...)
}

// claudePaneComposer is claudePane with the composer line spelled out, for the
// cases that turn on what the composer itself holds.
func claudePaneComposer(composer, state string, transcript ...string) []string {
	lines := append([]string{}, transcript...)
	if state != "" {
		lines = append(lines, state)
	}
	return append(lines,
		"────────────────────────────────────────────",
		composer,
		"────────────────────────────────────────────",
		"  ~/wt/branch  main  Opus 4.6  ctx:43%  tokens:20k",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
	)
}

// TestClaudePaneJudge covers each shape the parser can see. The last group is
// the one that matters: claude's spinner is sub-second and was never caught in
// 400+ samples of live monitoring, so a frame with no marker at all is the
// normal appearance of a *working* session. Reading it as idle would release
// queued mail into a running turn.
func TestClaudePaneJudge(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		lines   []string
		working bool
		known   bool
	}{
		// Positive idle markers. The composer is present in every one of these
		// — it is present in the working cases too, which is the point: the
		// composer says nothing about whether a turn is running.
		{"turn just finished", claudePane("✻ Worked for 36m 36s"), false, true},
		{"completion with a non-ASCII verb", claudePane("✻ Sautéed for 6m 16s"), false, true},
		{"completion with a trailing clause", claudePane("✻ Baked for 3m 48s · 1 shell still running"), false, true},
		{"a draft in the composer is not a turn in flight", claudePaneComposer("❯ half a thought", "✻ Worked for 12s"), false, true},
		{"a completion pushed up by a recap is still found", claudePane("recap tail (disable recaps in /config)", "✻ Cooked for 2m 44s", "※ recap: you asked me to…"), false, true},

		// Positive working markers.
		{"spinner", claudePane("* Frosting… (2m 30s)"), true, true},
		{"braille spinner", claudePane("⠋ Thinking…"), true, true},
		{"tool line", claudePane("● Bash(git status)"), true, true},

		// Ambiguous: agent output, no marker either way. Must be unknown.
		{"agent prose mid-turn", claudePane("Let me check the delivery path."), false, false},
		{"wrapped output", claudePane("  ...and that is why it failed."), false, false},

		// Not claude's pane at all.
		{"no separator", []string{"$ ", "some shell"}, false, false},
		{"empty pane", []string{}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			working, known := paneShowsWorking(ProviderClaude, tc.lines)
			assert.Equal(t, tc.known, known, "known")
			if tc.known {
				assert.Equal(t, tc.working, working, "working")
			}
		})
	}
}

// TestClaudeAmbiguousFrameResetsTheStreak is caveat 3 turned into a test. A
// working claude session usually shows no marker at all, so those frames must
// not accumulate toward idle — one of them mid-streak sends the count back to
// zero.
func TestClaudeAmbiguousFrameResetsTheStreak(t *testing.T) {
	t.Parallel()

	tr := newPaneIdleTracker(ProviderClaude)
	need := tr.confirmationsNeeded()
	require.Greater(t, need, paneIdleConfirmations,
		"claude needs more agreement than a provider whose working chrome is always on screen")

	// One short of firing...
	for i := 0; i < need-1; i++ {
		require.False(t, tr.observe(claudePane("✻ Worked for 36m 36s")), "observation %d", i+1)
	}
	// ...then a frame of plain agent output, which says nothing.
	require.False(t, tr.observe(claudePane("still working on it")))

	// The count restarted, so the very next idle frame must not fire.
	assert.False(t, tr.observe(claudePane("✻ Worked for 36m 36s")), "the streak restarted")
	for i := 0; i < need-2; i++ {
		assert.False(t, tr.observe(claudePane("✻ Worked for 36m 36s")))
	}
	assert.True(t, tr.observe(claudePane("✻ Worked for 36m 36s")), "a full fresh streak fires")
}

// TestClaudeNeedsAFullStreakToGoIdle: a single idle-looking frame is not
// enough, which is what keeps a half-painted repaint from releasing mail.
func TestClaudeNeedsAFullStreakToGoIdle(t *testing.T) {
	t.Parallel()

	tr := newPaneIdleTracker(ProviderClaude)
	need := tr.confirmationsNeeded()

	for i := 0; i < need-1; i++ {
		assert.False(t, tr.observe(claudePane("✻ Worked for 36m 36s")), "observation %d of %d", i+1, need)
	}
	assert.True(t, tr.observe(claudePane("✻ Worked for 36m 36s")), "the %dth consecutive idle frame fires", need)
}

// TestClaudeWorkingFrameIsNeverIdle: the direct statement of the bug this probe
// must not introduce.
func TestClaudeWorkingFrameIsNeverIdle(t *testing.T) {
	t.Parallel()

	for _, working := range []string{"* Frosting… (2m 30s)", "● Bash(git status)", "⠹ Thinking…"} {
		idle, known := paneShowsIdle(ProviderClaude, claudePane(working))
		require.True(t, known, "%q", working)
		assert.False(t, idle, "a running turn must never read as idle: %q", working)
	}
}

// TestWorkingClaudePaneIsNeverReadAsIdle is the regression this whole judge
// exists for, built from the repo's own fixture rather than a synthesized one:
// bramble/session/tmux_test.go:739-761 pins a pane with `✢ Fluttering… (4m 16s)`
// in flight whose ParseClaudeStatusBar result is IsIdle:true, because that
// parser stops at the first `❯` above the status separator and the composer is
// always on screen.
//
// Judging claude from that would call a working session idle on essentially
// every frame: five agreeing polls is ~10s, after which the parent is told the
// turn finished and Drain releases queued mail straight into it.
func TestWorkingClaudePaneIsNeverReadAsIdle(t *testing.T) {
	t.Parallel()

	// Verbatim from tmux_test.go's "working with completion indicator" case.
	live := []string{
		"● Bash(pytest tests/)",
		"  ⎿  Running…",
		"✢ Fluttering… (4m 16s)",
		"",
		"───────── ▪▪▪ ─",
		"❯ ",
		"───────────────────────────────────────",
		"  ~/project  main  Opus 4.6  ctx:19%  tokens:67k",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
		"",
	}

	require.True(t, ParseClaudeStatusBar(live).IsIdle,
		"precondition: the status-bar parser still reads this working pane as idle")

	working, known := claudePaneJudge(live)
	assert.True(t, known, "a pane with a tool line in flight is readable")
	assert.True(t, working, "a working claude pane must never be judged idle")

	// And the tracker must never fire idle on it, however many frames agree.
	tr := &paneIdleTracker{provider: ProviderClaude}
	for i := 0; i < 3*tr.confirmationsNeeded(); i++ {
		require.False(t, tr.observe(live), "frame %d released mail into a live turn", i+1)
	}
}

// TestStaleTranscriptPromptIsNotADraft: claude renders submitted prompts with
// the same `❯` glyph as the composer. A bottom-up scan for the lowest glyph
// latches onto one of those whenever the live composer is not the lowest match,
// and then reports a draft that never clears — holding every delivery on a 30s
// retry forever.
func TestStaleTranscriptPromptIsNotADraft(t *testing.T) {
	t.Parallel()

	// A dialog occupying the bottom rows, so the live (empty) composer is no
	// longer the lowest `❯` in the capture — a submitted transcript prompt
	// re-drawn below it is. An unanchored bottom-up scan takes that one.
	pane := append(claudePaneComposer("❯ ", ""),
		"❯ an earlier prompt the user already sent",
		"  [press esc to dismiss]",
	)

	draft, known := composerDraft(ProviderClaude, pane)
	require.True(t, known, "the composer is readable")
	assert.False(t, draft,
		"an empty composer is not a draft just because a redrawn transcript prompt sits below it")
}

// TestPaneIdleAndWorkingStreaksAreIndependent: observe() fires by equality and
// does not reset on firing, so a shared counter left sitting at the target
// makes observeWorking's equality permanently false. Codex — the only
// correctsPrematureIdle provider — would then stay wedged idle for the rest of
// the turn, which is the wedge the correction exists to undo.
func TestPaneIdleAndWorkingStreaksAreIndependent(t *testing.T) {
	t.Parallel()

	tr := &paneIdleTracker{provider: ProviderCodex}
	need := tr.confirmationsNeeded()

	// Drive a pane-observed idle to firing point.
	for i := 1; i < need; i++ {
		require.False(t, tr.observe(codexPane(false)), "idle frame %d", i)
	}
	require.True(t, tr.observe(codexPane(false)), "the idle streak fires")

	// The session is now idle. Working frames must still be able to resurrect
	// it — this is exactly the state the shared counter made unreachable.
	for i := 1; i < need; i++ {
		require.False(t, tr.observeWorking(codexPane(true)), "working frame %d", i)
	}
	assert.True(t, tr.observeWorking(codexPane(true)),
		"a premature idle must still be correctable after a pane-driven idle")
}

// TestIdleClaudeSessionIsActuallyReachable is the inverse of
// TestWorkingClaudePaneIsNeverReadAsIdle, and the reason deliverable 3(b)
// exists: a claude window whose Stop hook can no longer reach bramble is
// rescued only if the probe can reach an *idle* verdict on a real pane.
//
// An earlier judge demanded a positive marker on the topmost content line. For
// any session that has done work — the only kind that can be stranded — that
// line is the tail of its own last answer, so every real pane came back
// ambiguous, observe() reset the streak forever, and the fallback never fired
// while its tests read as if it worked.
//
// The layouts below are taken verbatim from live panes captured 2026-08-25.
func TestIdleClaudeSessionIsActuallyReachable(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		lines []string
	}{
		{"answer tail above the completion line", claudePane(
			"✻ Cogitated for 1m 27s",
			"but if you ever see a dangling ~/.agents/skills on a new box, that means the",
			"Committed as 5aeae0f. I did not push — you'd asked to push last turn for that",
		)},
		{"completion pushed up by a recap block", claudePane(
			"estimate's weakest input. (disable recaps in /config)",
			"✻ Cooked for 2m 44s",
			"※ recap: You asked me to cost the per-tenant-org pattern on code.storage",
		)},
		{"non-ASCII verb", claudePane(
			"✻ Sautéed for 6m 10s",
			"services/python/{api-gateway-service,tenant-user-agent}",
			"apps).",
		)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			working, known := claudePaneJudge(tc.lines)
			require.True(t, known, "a real idle claude pane must produce a verdict")
			assert.False(t, working, "a finished turn is not work in flight")

			// And the tracker must actually reach the idle decision.
			tr := &paneIdleTracker{provider: ProviderClaude}
			need := tr.confirmationsNeeded()
			for i := 1; i < need; i++ {
				require.False(t, tr.observe(tc.lines), "frame %d", i)
			}
			assert.True(t, tr.observe(tc.lines),
				"%d agreeing frames must mark a stranded session idle", need)
		})
	}
}

// TestWrappedComposerStillReadsAsADraft: a queued delivery is long, so the
// composer wraps onto a second line. Taking the nearest line above the status
// separator reads that continuation as the composer, fails the `❯` check, and
// reports unknown — which means deliver, pasting straight into the draft.
// Captured live: window 6 of the 2026-08-25 survey held exactly this.
func TestWrappedComposerStillReadsAsADraft(t *testing.T) {
	t.Parallel()

	// A human draft long enough to wrap. Live window 6 of the 2026-08-25 survey
	// showed the same shape holding one of bramble's own staged deliveries;
	// that text is deliberately NOT treated as a draft (see
	// TestBrambleOwnStagedDeliveryIsOverwritten), so the wrapping itself is what this
	// case pins.
	pane := []string{
		"● Bash(git status)",
		"────────────────────────────────────────────",
		"❯ file the dev deprovisioning bug and then check whether the staging",
		"tenant still has the old role binding attached to it",
		"────────────────────────────────────────────",
		"  ~/wt/branch  main  Opus 4.6  ctx:43%  tokens:20k",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
	}

	draft, known := composerDraft(ProviderClaude, pane)
	require.True(t, known, "a wrapped composer is still a readable composer")
	assert.True(t, draft, "a wrapped draft must hold the delivery, not invite one")
}

// TestOversizedComposerIsHeldNotDelivered: claude runs on the alternate screen,
// where capture-pane returns only the visible rows however deep -S goes, so a
// composer taller than the window leaves no rule above it in the capture.
//
// The composer walk used to run to the top of the capture in that case and
// call arbitrary transcript the composer. Two ways that hurts, both pinned
// here: a line without the glyph reported "unknown", which means deliver —
// straight into the oversized draft — and a submitted transcript prompt, which
// claude draws with the same glyph, reported a draft that never cleared.
func TestOversizedComposerIsHeldNotDelivered(t *testing.T) {
	t.Parallel()

	t.Run("a draft wrapping past the top of the capture is held", func(t *testing.T) {
		t.Parallel()
		// The composer's first line is still visible at the very top of the
		// capture, but the rule above it has scrolled off.
		pane := []string{
			"❯ the beginning of a very long draft that fills the window",
			"and the rest of my long draft continues here",
			"more of the draft, still no rule above it",
			"────────────────────────────────────────────",
			"  ~/wt/branch  main  Opus 4.6  ctx:43%  tokens:20k",
			"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
		}
		draft, known := composerDraft(ProviderClaude, pane)
		require.True(t, known, "an unreadable composer must not report unknown — that means deliver")
		assert.True(t, draft, "an oversized draft must hold the delivery")
	})

	t.Run("a stale prompt with no rule above it does not wedge the queue", func(t *testing.T) {
		t.Parallel()
		pane := []string{
			"❯ an earlier submitted prompt",
			"● some tool output",
			"❯ ",
			"────────────────────────────────────────────",
			"  ~/wt/branch  main  Opus 4.6  ctx:43%  tokens:20k",
			"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
		}
		draft, known := composerDraft(ProviderClaude, pane)
		require.True(t, known)
		assert.False(t, draft, "the live composer is empty; a transcript prompt must not hold mail forever")
	})
}

// TestComposerLayerDoesNotJudgeOwnership: whether staged text belongs to
// bramble is not decidable from the pane. The "[bramble]" prefix is
// user-controllable, so this layer reports any non-empty composer as a draft
// and leaves ownership to the courier, which knows what it queued.
// See TestBrambleOwnStagedDeliveryIsOverwritten.
func TestComposerLayerDoesNotJudgeOwnership(t *testing.T) {
	t.Parallel()

	for _, body := range []string{
		subagentReportPrefix + " subagent forge-planner-0da3be25 (planner, opus) is idle",
		"file the dev deprovisioning bug",
	} {
		draft, known := composerDraft(ProviderClaude, claudePaneComposer("❯ "+body, "✻ Worked for 12s"))
		require.True(t, known)
		assert.True(t, draft, "any non-empty composer is a draft at this layer: %q", body)
	}
}

// TestLocatedButUnreadableComposerHolds: the composer region is bounded by two
// rules — so it IS located — but its first line does not carry the glyph, which
// happens when claude decorates the composer or a repaint lands mid-frame.
//
// Something is in there that this parser cannot read, and "unknown" means
// deliver. The safe verdict is to hold. This case is only reachable when the
// upper rule is present: with no rule the composer is reported unfound and the
// bounded tail fallback runs instead, which is what
// TestOversizedComposerIsHeldNotDelivered covers.
func TestLocatedButUnreadableComposerHolds(t *testing.T) {
	t.Parallel()

	pane := []string{
		"✻ Worked for 12s",
		"────────────────────────────────────────────",
		"⏎ some decorated composer shape we do not parse",
		"────────────────────────────────────────────",
		"  ~/wt/branch  main  Opus 4.6  ctx:43%  tokens:20k",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
	}

	// Precondition: the composer really is located, so this is the branch under
	// test and not the tail fallback.
	composerIdx, contentEnd := claudeComposerIdx(pane)
	require.GreaterOrEqual(t, composerIdx, 0, "the composer region must be located")
	require.GreaterOrEqual(t, contentEnd, 0, "bounded above by a rule")

	draft, known := composerDraft(ProviderClaude, pane)
	require.True(t, known, "an unreadable composer must not report unknown — that means deliver")
	assert.True(t, draft, "hold when something unparseable occupies the composer")
}

// TestStaleCompletionLineIsNotThisTurnsVerdict: a completion line persists in
// claude's transcript and is pushed up by later output, so in the seconds right
// after bramble writes a delivery the content region holds this turn's echoed
// prompt with the PREVIOUS turn's "✻ Worked for …" just above it.
//
// Reading that as the current verdict marks a live turn idle: Drain then
// releases the next queued delivery into it and the parent is told the child
// finished — the two harms this probe exists to prevent. The spinner is usually
// absent from any given frame (caveat 3) and forTurn resets the streak at
// exactly this boundary, so all five confirmations (~10s) fit inside the window.
//
// A submitted prompt is the boundary: claude echoes every one with the same
// glyph, so nothing above it speaks for the turn now running.
func TestStaleCompletionLineIsNotThisTurnsVerdict(t *testing.T) {
	t.Parallel()

	justSubmitted := []string{
		"  Worktree is ready for your next task.",
		"✻ Worked for 36m 36s",                                    // the PREVIOUS turn's completion
		"❯ " + subagentReportPrefix + " subagent child-1 is idle", // THIS turn's prompt
		"────────────────────────────────────────────",
		"❯ ",
		"────────────────────────────────────────────",
		"  ~/wt/branch  main  Opus 4.6  ctx:43%  tokens:20k",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
	}

	working, known := claudePaneJudge(justSubmitted)
	assert.False(t, known,
		"a turn that has produced no output yet has no verdict; the line above its prompt is a previous turn's")
	assert.False(t, working)

	// And the tracker must never reach an idle decision on it, however many
	// frames agree.
	tr := &paneIdleTracker{provider: ProviderClaude}
	for i := 0; i < 3*tr.confirmationsNeeded(); i++ {
		require.False(t, tr.observe(justSubmitted),
			"frame %d released queued mail into a live turn", i+1)
	}

	// Once the turn genuinely ends, its own completion line sits below the
	// prompt and the verdict is reachable again.
	finished := []string{
		"❯ " + subagentReportPrefix + " subagent child-1 is idle",
		"● Read(delivery.go)",
		"✻ Worked for 12s",
		"────────────────────────────────────────────",
		"❯ ",
		"────────────────────────────────────────────",
		"  ~/wt/branch  main  Opus 4.6  ctx:43%  tokens:20k",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
	}
	working, known = claudePaneJudge(finished)
	require.True(t, known, "a completed turn below its own prompt is readable")
	assert.False(t, working, "and reads as idle")
}

// TestClaudeAcceptsAPasteChip: claude is required:true, so a paste it cannot
// confirm is re-pasted and then re-queued — the loop section 1 of this PR
// exists to remove. The live measurement behind required:true covered the
// deliveries seen, not every delivery possible: a subagent report grows to two
// or three lines once an error or a result path is set, and a large enough
// paste collapses to a chip.
//
// Accepting a chip cannot produce a false positive that matters: a chip in the
// pane means the paste reached the composer, which is exactly what is asked.
func TestClaudeAcceptsAPasteChip(t *testing.T) {
	t.Parallel()

	require.True(t, pasteVerifyRequired(ProviderClaude), "precondition")

	// A real pane: pasteConfirmed reads the composer, so the chrome has to be
	// there for the composer to be located.
	assert.True(t, pasteConfirmed(ProviderClaude,
		claudePaneComposer("❯ [Pasted text #1 +42 lines]", "✻ Worked for 12s"),
		"a report from a subagent"),
		"a collapsed paste is still a paste that arrived")

	// An empty composer is not confirmation.
	assert.False(t, pasteConfirmed(ProviderClaude,
		claudePaneComposer("❯ ", "✻ Worked for 12s"), "a report from a subagent"))

	// Nor is the same text sitting in the transcript above an empty composer:
	// an agent echoes every submitted prompt, so a previous delivery would
	// otherwise confirm a paste that was dropped, and Enter would submit an
	// empty composer.
	assert.False(t, pasteConfirmed(ProviderClaude,
		claudePaneComposer("❯ ", "❯ a report from a subagent", "● I read it."),
		"a report from a subagent"),
		"transcript history must not confirm a paste that never arrived")

	// A composer too narrow to show the whole probe still confirms. The capture
	// stops at the pane width and the composer wraps, so a delivery that DID
	// arrive can show fewer than pasteProbeLen bytes of itself.
	// composerHoldsThisDelivery already compares prefix-wise for this reason;
	// requiring full containment here made a narrow pane a real negative, and
	// because the composer was located the caller reads that as readable and
	// re-pastes — appending a second copy, which is the loop this check exists
	// to end.
	long := strings.Repeat("x", pasteProbeLen*2)
	require.Greater(t, len(pasteProbe(long)), 8, "precondition: the probe must be longer than the truncation below")
	assert.True(t, pasteConfirmed(ProviderClaude,
		claudePaneComposer("❯ "+pasteProbe(long)[:8], "✻ Worked for 12s"), long),
		"a composer showing a truncation of the paste confirms it; the pane is simply narrow")

	// But a truncation is only evidence in ONE direction. A composer holding
	// something that merely happens to sit under the probe's length, and is not
	// a prefix of it, is somebody else's line and confirms nothing.
	assert.False(t, pasteConfirmed(ProviderClaude,
		claudePaneComposer("❯ zzzz", "✻ Worked for 12s"), long),
		"an unrelated short line is not a truncation of our paste")
}

// TestCodexTranscriptDoesNotConfirmAPaste is the codex half of the rule the
// claude case already pins: what is on screen near the composer is now, what is
// scrolled above it is history.
//
// codex is required:true and never composer-readable, so it always takes
// pasteConfirmed's fallback — the branch whose own doc says confirming a
// required:true provider off the transcript means "pressing Enter on an empty
// composer: the message is lost and MarkRunning wedges the session on a turn
// that never started".
//
// The collision is not hypothetical. pasteProbe takes pasteProbeLen bytes of
// the first line; a subagent report opens with a 19-byte constant prefix and
// the remaining bytes are the worktree name every sibling shares, so two
// different reports to one parent produce the SAME probe. Once report #1 is
// submitted and echoed, report #2's dropped paste would be confirmed off that
// echo — exactly the drop required:true exists to catch.
func TestCodexTranscriptDoesNotConfirmAPaste(t *testing.T) {
	t.Parallel()

	require.True(t, pasteVerifyRequired(ProviderCodex), "precondition: codex's verdict gates Enter")
	require.False(t, composerReadable(ProviderCodex), "precondition: codex always takes the fallback")

	report := "[bramble] subagent wt-builder-a1b2c3d4 (builder) is completed"
	probe := pasteProbe(report)

	// A deep pane: the previous delivery is echoed far above the composer, with
	// enough intervening rows to put it out of the tail's reach — which is what
	// a real transcript looks like after the agent has answered it.
	deep := []string{"  › " + report, "  • I read the report and acted on it."}
	for i := 0; i < 20; i++ {
		deep = append(deep, "  • still working through it")
	}
	assert.False(t, pasteConfirmed(ProviderCodex, codexPane(false, deep...), probe),
		"a delivery echoed into the transcript must not confirm the NEXT one, which may have been dropped")

	// The paste that actually just arrived sits at the bottom, where the tail
	// reaches it. Bounding the scan must not cost the real confirmation.
	assert.True(t, pasteConfirmed(ProviderCodex,
		codexPane(false, "  • an earlier turn", "  › "+report), probe),
		"a paste sitting at the bottom of the pane is what the check is for")
}

// TestTallComposerIsHeldNotDelivered: a composer taller than the walk's bound
// used to report unfound, and both consumers of unfound then failed in the
// unsafe direction. This pins the draft half: an ordinary long draft — a
// wrapped delivery or a long human line — must hold, not deliver.
//
// The bound was 6, which a 500-character wrapped message clears routinely.
func TestTallComposerIsHeldNotDelivered(t *testing.T) {
	t.Parallel()

	pane := []string{"────────────────────────────────────────────"}
	for i := 0; i < 12; i++ {
		pane = append(pane, "and the draft continues onto another line")
	}
	pane = append([]string{pane[0]}, pane[1:]...)
	// Put the glyph on the first composer line, as claude draws it.
	pane[1] = "❯ the beginning of a draft that wraps well past six lines"
	pane = append(pane,
		"────────────────────────────────────────────",
		"  ~/wt/branch  main  Opus 4.6  ctx:43%  tokens:20k",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
	)

	draft, known := composerDraft(ProviderClaude, pane)
	require.True(t, known, "unknown means deliver, straight into the draft")
	assert.True(t, draft, "a composer taller than six lines is still a draft")
}

// TestObscuredPasteEvidenceIsNotANegative pins the distinction that ends the
// re-paste loop: a capture showing claude's chrome but no locatable composer
// says NOTHING about whether the paste arrived, while an empty or unpainted
// pane is a real negative.
//
// Conflating them re-pasted on silence, appending a second copy of the message
// every retry and submitting none of them.
func TestObscuredPasteEvidenceIsNotANegative(t *testing.T) {
	t.Parallel()

	// Chrome on screen, composer region unbounded above: obscured.
	obscured := []string{
		"a composer line with no rule above it",
		"────────────────────────────────────────────",
		"  ~/wt/branch  main  Opus 4.6  ctx:43%  tokens:20k",
	}
	assert.True(t, pasteEvidenceObscured(ProviderClaude, obscured),
		"chrome present but composer unfound is silence, not a negative")

	// Nothing painted at all: a real negative, or a dropped paste would be
	// submitted on an empty prompt.
	assert.False(t, pasteEvidenceObscured(ProviderClaude, nil),
		"an unpainted pane is exactly what a dropped paste looks like")
	assert.False(t, pasteEvidenceObscured(ProviderClaude, []string{"nothing here"}),
		"a pane with no claude chrome is a negative")

	// A locatable composer is readable, so its verdict is real either way.
	assert.False(t, pasteEvidenceObscured(ProviderClaude, claudePaneComposer("❯ ", "✻ Worked for 12s")),
		"a located composer yields a real verdict")

	// Providers whose composer bramble does not read never reach this path.
	assert.False(t, pasteEvidenceObscured(ProviderCursor, nil), "cursor's evidence is never obscured")
}

// TestComposerBoundFollowsTheCapture: the walk's bound must be sized against
// the capture it runs over, not against a guess at composer height. At 6 it
// manufactured the unfound case for ordinary panes.
func TestComposerBoundFollowsTheCapture(t *testing.T) {
	t.Parallel()
	assert.Greater(t, claudeComposerMaxLines, paneIdleTailLines,
		"the walk must reach further than the tail scan it replaced")
	assert.Less(t, claudeComposerMaxLines, pasteVerifyLines,
		"but never past the capture, or it walks into transcript")
}
