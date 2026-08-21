package app

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
)

// isQuit reports whether a command resolves to tea.Quit. A restart leaves the
// TUI the same way a quit does; what distinguishes them is restartRequested on
// the final model, which is what main() reads.
func isQuit(t *testing.T, cmd tea.Cmd) bool {
	t.Helper()
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// addRunningSession injects a running session directly, avoiding the race where
// the runSession goroutine fails before m.sessions is populated.
func addRunningSession(m *Model, id session.SessionID) {
	m.sessionManager.AddSession(&session.Session{
		ID: id, Status: session.StatusRunning,
		WorktreePath: "/tmp/wt/main", Type: session.SessionTypePlanner,
	})
	m.sessions = m.sessionManager.GetAllSessions()
}

func mainWorktree() []wt.Worktree {
	return []wt.Worktree{{Branch: "main", Path: "/tmp/wt/main"}}
}

func TestCtrlR_TmuxSessions_RestartsWithoutConfirming(t *testing.T) {
	// Tmux-mode sessions are children of the tmux server, so an exec restart
	// does not disturb them — there is nothing to warn about.
	m := setupModel(t, session.SessionModeTmux, mainWorktree(), "test-repo")
	addRunningSession(&m, "tmux-session")

	newModel, cmd := m.handleKeyPress(ctrlKey('r'))
	m2 := newModel.(Model)

	assert.True(t, m2.RestartRequested())
	assert.False(t, m2.confirmRestart)
	assert.True(t, isQuit(t, cmd))
}

func TestCtrlR_NoSessions_RestartsImmediately(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, mainWorktree(), "test-repo")

	newModel, cmd := m.handleKeyPress(ctrlKey('r'))
	m2 := newModel.(Model)

	assert.True(t, m2.RestartRequested())
	assert.True(t, isQuit(t, cmd))
}

func TestCtrlR_InProcessSessions_ConfirmsFirst(t *testing.T) {
	// TUI-mode sessions are our own children and cannot survive the exec, so
	// the user gets a chance to back out before anything is torn down.
	m := setupModel(t, session.SessionModeTUI, mainWorktree(), "test-repo")
	addRunningSession(&m, "in-process-session")

	newModel, cmd := m.handleKeyPress(ctrlKey('r'))
	m2 := newModel.(Model)

	assert.True(t, m2.confirmRestart)
	assert.False(t, m2.RestartRequested(), "must not commit to a restart before the user confirms")
	require.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "in-process session")
	assert.False(t, isQuit(t, cmd))
}

func TestRestartConfirm_AcceptedKeys_Restart(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{ctrlKey('r'), keyPress('y')} {
		t.Run(key.String(), func(t *testing.T) {
			m := setupModel(t, session.SessionModeTUI, mainWorktree(), "test-repo")
			addRunningSession(&m, "in-process-session")

			newModel, _ := m.handleKeyPress(ctrlKey('r'))
			m2 := newModel.(Model)
			require.True(t, m2.confirmRestart)

			newModel2, cmd := m2.handleKeyPress(key)
			m3 := newModel2.(Model)

			assert.False(t, m3.confirmRestart)
			assert.True(t, m3.RestartRequested())
			assert.True(t, isQuit(t, cmd))
		})
	}
}

func TestRestartConfirm_OtherKey_Cancels(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, mainWorktree(), "test-repo")
	addRunningSession(&m, "in-process-session")

	newModel, _ := m.handleKeyPress(ctrlKey('r'))
	m2 := newModel.(Model)
	require.True(t, m2.confirmRestart)

	newModel2, cmd := m2.handleKeyPress(keyPress('x'))
	m3 := newModel2.(Model)

	assert.False(t, m3.confirmRestart)
	assert.False(t, m3.RestartRequested())
	assert.False(t, isQuit(t, cmd))
	assert.Contains(t, m3.toasts.toasts[len(m3.toasts.toasts)-1].Message, "Restart cancelled")
}

func TestRestartRequestedMsg_TakesTheSamePath(t *testing.T) {
	// SIGUSR2 and `bramble restart` arrive as a message rather than a keypress,
	// but must land in the same pre-flight — including the confirmation.
	m := setupModel(t, session.SessionModeTUI, mainWorktree(), "test-repo")
	addRunningSession(&m, "in-process-session")

	newModel, _ := m.Update(RestartRequestedMsg{Source: "signal"})
	m2 := newModel.(Model)

	assert.True(t, m2.confirmRestart)
	assert.False(t, m2.RestartRequested())
}

func TestRestartRequestedMsg_ForceSkipsConfirmation(t *testing.T) {
	// `bramble restart --force` has no way to answer a prompt, so it opts out.
	m := setupModel(t, session.SessionModeTUI, mainWorktree(), "test-repo")
	addRunningSession(&m, "in-process-session")

	newModel, cmd := m.Update(RestartRequestedMsg{Source: "ipc", Force: true})
	m2 := newModel.(Model)

	assert.False(t, m2.confirmRestart)
	assert.True(t, m2.RestartRequested())
	assert.True(t, isQuit(t, cmd))
}

func TestRestartSnapshot_CapturesViewState(t *testing.T) {
	m := setupModel(t, session.SessionModeTmux, mainWorktree(), "test-repo")
	m.worktreeDropdown.SelectByID("main")
	m.viewingSessionID = "some-session"

	state := m.RestartSnapshot()

	assert.Equal(t, "test-repo", state.ActiveRepo)
	assert.Equal(t, "main", state.SelectedWorktree)
	assert.Equal(t, "some-session", state.ViewingSessionID)
	assert.Contains(t, state.OpenedRepos, "test-repo")
}

// TestApplyRestartState_PreloadedWorktrees covers the path main() actually
// takes: worktrees are loaded synchronously before the model is built, so
// Init() skips refreshWorktrees and no worktreesMsg is ever emitted. Deferring
// the selection to pendingWorktreeSelect here would strand it forever — that
// field is only drained by the worktreesMsg handler.
func TestApplyRestartState_PreloadedWorktrees(t *testing.T) {
	worktrees := []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
		{Branch: "feature-branch", Path: "/tmp/wt/feature-branch"},
	}
	m := setupModel(t, session.SessionModeTmux, worktrees, "test-repo")
	require.Equal(t, "main", m.worktreeDropdown.SelectedItem().ID, "expected the default auto-selection")

	m.ApplyRestartState(&RestartState{
		ActiveRepo:       "test-repo",
		SelectedWorktree: "feature-branch",
		ViewingSessionID: "restored-session",
	})

	assert.Equal(t, "feature-branch", m.worktreeDropdown.SelectedItem().ID)
	assert.Empty(t, m.pendingWorktreeSelect, "no need to defer a selection that already applied")
	assert.Equal(t, session.SessionID("restored-session"), m.viewingSessionID)
}

// TestApplyRestartState_WorktreesNotYetLoaded covers the other startup path,
// where the dropdown is still empty and the selection has to wait for the
// async refresh.
func TestApplyRestartState_WorktreesNotYetLoaded(t *testing.T) {
	m := setupModel(t, session.SessionModeTmux, nil, "test-repo")

	m.ApplyRestartState(&RestartState{
		ActiveRepo:       "test-repo",
		SelectedWorktree: "feature-branch",
		ViewingSessionID: "restored-session",
	})
	assert.Equal(t, "feature-branch", m.pendingWorktreeSelect)

	// The worktreesMsg handler drains it once the dropdown has items.
	worktrees := []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
		{Branch: "feature-branch", Path: "/tmp/wt/feature-branch"},
	}
	newModel, _ := m.Update(worktreesMsg{repoName: "test-repo", worktrees: worktrees})
	m2 := newModel.(Model)

	assert.Equal(t, "feature-branch", m2.worktreeDropdown.SelectedItem().ID)
	assert.Empty(t, m2.pendingWorktreeSelect)
}

func TestApplyRestartState_NilIsNoOp(t *testing.T) {
	m := setupModel(t, session.SessionModeTmux, mainWorktree(), "test-repo")
	m.viewingSessionID = "unchanged"

	m.ApplyRestartState(nil)

	assert.Empty(t, m.pendingWorktreeSelect)
	assert.Equal(t, session.SessionID("unchanged"), m.viewingSessionID)
}
