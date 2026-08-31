package session

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// collectStateChanges subscribes to a manager and returns a snapshot of the
// state changes seen so far, plus an unsubscribe.
func collectStateChanges(m *Manager) (func() []SessionStateChangeEvent, func()) {
	var mu sync.Mutex
	var events []SessionStateChangeEvent
	unsub := m.SubscribeStateChanges(func(evt SessionStateChangeEvent) {
		mu.Lock()
		events = append(events, evt)
		mu.Unlock()
	})
	return func() []SessionStateChangeEvent {
		mu.Lock()
		defer mu.Unlock()
		return append([]SessionStateChangeEvent(nil), events...)
	}, unsub
}

// tmuxManager builds a tmux-mode manager over store (nil for none) that closes
// with the test.
func tmuxManager(t *testing.T, store *Store) *Manager {
	t.Helper()
	m := NewManagerWithConfig(ManagerConfig{
		RepoName:    "test-repo",
		Store:       store,
		SessionMode: SessionModeTmux,
	})
	t.Cleanup(m.Close)
	return m
}

// reapedSession builds a session in the state a monitor loop leaves behind when
// it finds the tmux window gone: driven to a terminal status, then dropped from
// m.sessions. That pair is what makes list-sessions report zero while a stale
// goroutine still holds the pointer. It returns the stale pointer and a snapshot
// of the state changes emitted after the reap.
func reapedSession(t *testing.T, m *Manager, id SessionID) (*Session, func() []SessionStateChangeEvent) {
	t.Helper()
	s := &Session{
		ID:           id,
		Type:         SessionTypeBuilder,
		Status:       StatusRunning,
		RepoName:     "test-repo",
		WorktreeName: "feature",
		RunnerType:   RunnerTypeTmux,
		Progress:     &SessionProgress{},
		CreatedAt:    time.Now(),
	}
	m.AddSession(s)
	m.updateSessionStatus(s, StatusCompleted)
	m.mu.Lock()
	delete(m.sessions, s.ID)
	m.mu.Unlock()
	require.True(t, s.ToInfo().Status.IsTerminal(), "reap did not leave a terminal status")

	snapshot, unsub := collectStateChanges(m)
	t.Cleanup(unsub)
	return s, snapshot
}

// TestReapedSessionCannotBeRevivedToRunning is the core regression for issue
// #331.
//
// monitorTrackedTmuxWindow's 15-second capture ticker and its 2-second liveness
// ticker share one *Session. A capture that lands after the liveness tick has
// reaped the session calls tryUpdateSessionStatus(idle→running) on that stale
// pointer, and neither it nor updateSessionStatus consulted anything but the
// session's own status. The revive re-arms the session for a later idle, which
// Courier.Watch reports to the parent as a subagent that is "is idle" — for a
// session list-sessions no longer knows about.
func TestReapedSessionCannotBeRevivedToRunning(t *testing.T) {
	t.Parallel()

	m := tmuxManager(t, nil)
	s, snapshot := reapedSession(t, m, "reaped-revive")

	// The stale capture goroutine sees a working pane and tries to revive.
	revived := m.tryUpdateSessionStatus(s, StatusCompleted, StatusRunning)

	assert.False(t, revived, "a reaped session was moved back to running")
	assert.Empty(t, snapshot(), "a reaped session emitted a state change")
	assert.True(t, s.ToInfo().Status.IsTerminal(),
		"reaped session status is %q, want terminal", s.ToInfo().Status)
}

// TestReapedSessionCannotBeMarkedIdle covers the other emitter on the same
// pointer: updateSessionStatus takes no from-status, so nothing but this guard
// stops a late pane poll from writing idle straight over a terminal status.
func TestReapedSessionCannotBeMarkedIdle(t *testing.T) {
	t.Parallel()

	m := tmuxManager(t, nil)
	s, snapshot := reapedSession(t, m, "reaped-idle")

	m.updateSessionStatus(s, StatusIdle)

	assert.Empty(t, snapshot(), "a reaped session emitted a stale idle transition")
	assert.True(t, s.ToInfo().Status.IsTerminal(),
		"reaped session status is %q, want terminal", s.ToInfo().Status)
}

// TestReapedSessionStillSettles keeps the guard from swallowing the news a
// parent actually needs. A stop landing after a natural completion must still
// record and emit, or suppressing a stale idle would cost a real terminal event.
func TestReapedSessionStillSettles(t *testing.T) {
	t.Parallel()

	m := tmuxManager(t, nil)
	s, snapshot := reapedSession(t, m, "reaped-settle")

	m.updateSessionStatus(s, StatusStopped)

	events := snapshot()
	require.Len(t, events, 1, "terminal→terminal transition was suppressed")
	assert.Equal(t, StatusStopped, events[0].NewStatus)
}

// TestLiveSessionStillTransitions pins that the guard is scoped to terminal
// sessions: an ordinary running→idle must still emit.
func TestLiveSessionStillTransitions(t *testing.T) {
	t.Parallel()

	m := tmuxManager(t, nil)

	s := &Session{
		ID:         "live-1",
		Type:       SessionTypeBuilder,
		Status:     StatusRunning,
		RepoName:   "test-repo",
		RunnerType: RunnerTypeTmux,
		Progress:   &SessionProgress{},
		CreatedAt:  time.Now(),
	}
	m.AddSession(s)

	snapshot, unsub := collectStateChanges(m)
	defer unsub()

	m.SetSessionIdle(s.ID)

	events := snapshot()
	require.Len(t, events, 1)
	assert.Equal(t, StatusIdle, events[0].NewStatus)
}

// storedTmuxSessionWithWindowID files one non-terminal tmux session at
// worktreePath under the given tmux window ID, and returns a manager over that
// store plus a snapshot of what reconciling emits.
func storedTmuxSessionWithWindowID(t *testing.T, id SessionID, worktreePath, windowID string) (*Manager, *Store, func() []SessionStateChangeEvent) {
	t.Helper()
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.SaveSession(&StoredSession{
		ID:             id,
		Type:           SessionTypeBuilder,
		Status:         StatusIdle,
		RepoName:       "test-repo",
		WorktreePath:   worktreePath,
		WorktreeName:   "feature",
		TmuxWindowName: "test-repo/feature:0",
		TmuxWindowID:   windowID,
		RunnerType:     RunnerTypeTmux,
		CreatedAt:      time.Now(),
	}))

	m := tmuxManager(t, store)
	snapshot, unsub := collectStateChanges(m)
	t.Cleanup(unsub)
	return m, store, snapshot
}

// storedTmuxSession files one non-terminal tmux session at worktreePath whose
// record carries no window ID — the legacy shape, and the only one where a live
// window is not proof the *stored* session is the one answering, because
// GenerateTmuxWindowName has by then handed its "repo/worktree:N" on.
func storedTmuxSession(t *testing.T, id SessionID, worktreePath string) (*Manager, *Store, func() []SessionStateChangeEvent) {
	t.Helper()
	return storedTmuxSessionWithWindowID(t, id, worktreePath, "")
}

// windowIsAlive stands in for tmuxWindowAlive answering yes — a window with the
// stored name exists, whether or not it belongs to the stored session.
func windowIsAlive(string, string) bool { return true }

// TestReconcileReapsSessionWhoseWorktreeIsGone drives the reconcile pass that
// runs on every start, with a live window answering the stored name.
//
// Deciding on the window alone re-adopts the session: into m.sessions, with a
// monitor goroutine free to emit fresh idle transitions that reach the parent as
// subagent reports for a session list-sessions no longer knows about. The
// courier's reported-set does not survive a restart, so the parent hears it
// again every time. The missing worktree is what says the session is gone.
func TestReconcileReapsSessionWhoseWorktreeIsGone(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "removed-worktree")
	m, store, snapshot := storedTmuxSession(t, "stale-1", missing)
	require.NoError(t, m.reconcileTmuxSessions(windowIsAlive))

	_, adopted := m.GetSession("stale-1")
	assert.False(t, adopted, "session with a removed worktree was adopted as live")

	reloaded, err := store.LoadSession("test-repo", "feature", "stale-1")
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, reloaded.Status)

	events := snapshot()
	require.Len(t, events, 1, "the reap owes the parent exactly one transition")
	assert.Equal(t, StatusCompleted, events[0].NewStatus)
}

// TestReconcileAdoptsSessionWhoseWorktreeSurvives keeps the worktree gate from
// reaping healthy sessions: a live window over a live worktree is re-adopted,
// and re-announced at its stored status so the courier learns it is reachable.
func TestReconcileAdoptsSessionWhoseWorktreeSurvives(t *testing.T) {
	t.Parallel()

	m, _, snapshot := storedTmuxSession(t, "live-wt", t.TempDir())
	require.NoError(t, m.reconcileTmuxSessions(windowIsAlive))

	_, adopted := m.GetSession("live-wt")
	assert.True(t, adopted, "a live session was not re-adopted")

	events := snapshot()
	require.Len(t, events, 1)
	assert.Equal(t, StatusIdle, events[0].NewStatus)
}

// TestReconcileReapsSessionWhoseWindowIsGone keeps a dead window decisive on its
// own, worktree or no worktree.
func TestReconcileReapsSessionWhoseWindowIsGone(t *testing.T) {
	t.Parallel()

	m, store, _ := storedTmuxSession(t, "dead-window", t.TempDir())
	require.NoError(t, m.reconcileTmuxSessions(func(string, string) bool { return false }))

	_, adopted := m.GetSession("dead-window")
	assert.False(t, adopted, "a session whose window is gone was adopted")

	reloaded, err := store.LoadSession("test-repo", "feature", "dead-window")
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, reloaded.Status)
}

// TestReconcileWithoutWorktreePathDefersToWindow keeps an unrecorded worktree
// path from being read as evidence the session is gone.
func TestReconcileWithoutWorktreePathDefersToWindow(t *testing.T) {
	t.Parallel()

	m, _, _ := storedTmuxSession(t, "no-path", "")
	require.NoError(t, m.reconcileTmuxSessions(windowIsAlive))

	_, adopted := m.GetSession("no-path")
	assert.True(t, adopted, "a session with no recorded worktree was reaped on that alone")
}

// TestReconcileKeepsIDCarryingSessionWithUnstattableWorktree is the regression
// for the widened worktree gate.
//
// tmux window IDs are never recycled within a server lifetime, so a record that
// carries one is answered by its own window or by nothing — the name-recycling
// confusion the worktree gate exists for cannot reach it. Letting the gate reap
// it anyway turns any os.Stat failure into a verdict, and a failure is not the
// same fact as an absence: here the worktree is present and the agent is working
// in it, but a parent directory bramble cannot traverse makes it unstattable.
//
// Reaping is unrecoverable. It writes StatusCompleted to the store without
// killing the window, so every later reconcile skips the record as terminal and
// the agent keeps running in a session bramble has permanently disowned.
func TestReconcileKeepsIDCarryingSessionWithUnstattableWorktree(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root traverses a 0000 directory, so the stat cannot be made to fail")
	}

	// A live worktree under a parent bramble cannot traverse.
	parent := filepath.Join(t.TempDir(), "locked")
	worktree := filepath.Join(parent, "feature")
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	require.NoError(t, os.Chmod(parent, 0o000))
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	_, err := os.Stat(worktree)
	require.Error(t, err, "worktree is still stattable, so this test proves nothing")
	require.False(t, os.IsNotExist(err), "want a traversal failure, not an absence")

	m, store, snapshot := storedTmuxSessionWithWindowID(t, "id-live", worktree, "@7")
	require.NoError(t, m.reconcileTmuxSessions(windowIsAlive))

	_, adopted := m.GetSession("id-live")
	assert.True(t, adopted, "a live ID-matched session was reaped on a failed stat")

	reloaded, err := store.LoadSession("test-repo", "feature", "id-live")
	require.NoError(t, err)
	assert.Equal(t, StatusIdle, reloaded.Status,
		"a live session was written to the store as terminal, which no later reconcile undoes")

	events := snapshot()
	require.Len(t, events, 1, "re-adoption owes the courier exactly one same-status event")
	assert.Equal(t, StatusIdle, events[0].NewStatus)
}

// TestReconcileReapsIDCarryingSessionWhoseWindowIsGone keeps the dead-window
// verdict decisive for ID-carrying records too: scoping the worktree gate to
// ID-less records must not make an ID a shield.
func TestReconcileReapsIDCarryingSessionWhoseWindowIsGone(t *testing.T) {
	t.Parallel()

	m, store, _ := storedTmuxSessionWithWindowID(t, "id-dead", t.TempDir(), "@9")
	require.NoError(t, m.reconcileTmuxSessions(func(string, string) bool { return false }))

	_, adopted := m.GetSession("id-dead")
	assert.False(t, adopted, "a session whose window is gone was adopted")

	reloaded, err := store.LoadSession("test-repo", "feature", "id-dead")
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, reloaded.Status)
}
