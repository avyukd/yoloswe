package session

import (
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

// reapedSession builds a session in the state a monitor loop leaves behind when
// it finds the tmux window gone: driven to a terminal status, then dropped from
// m.sessions. That pair is what makes list-sessions report zero while a stale
// goroutine still holds the pointer.
func reapedSession(t *testing.T, m *Manager, id SessionID) *Session {
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
	return s
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

	m := NewManagerWithConfig(ManagerConfig{
		RepoName:    "test-repo",
		SessionMode: SessionModeTmux,
	})
	defer m.Close()

	s := reapedSession(t, m, "reaped-revive")

	snapshot, unsub := collectStateChanges(m)
	defer unsub()

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

	m := NewManagerWithConfig(ManagerConfig{
		RepoName:    "test-repo",
		SessionMode: SessionModeTmux,
	})
	defer m.Close()

	s := reapedSession(t, m, "reaped-idle")

	snapshot, unsub := collectStateChanges(m)
	defer unsub()

	m.updateSessionStatus(s, StatusIdle)

	for _, evt := range snapshot() {
		assert.NotEqual(t, StatusIdle, evt.NewStatus,
			"reaped session %s emitted a stale idle transition", evt.SessionID)
	}
	assert.True(t, s.ToInfo().Status.IsTerminal(),
		"reaped session status is %q, want terminal", s.ToInfo().Status)
}

// TestReapedSessionStillSettles keeps the guard from swallowing the news a
// parent actually needs. A stop landing after a natural completion must still
// record and emit, or suppressing a stale idle would cost a real terminal event.
func TestReapedSessionStillSettles(t *testing.T) {
	t.Parallel()

	m := NewManagerWithConfig(ManagerConfig{
		RepoName:    "test-repo",
		SessionMode: SessionModeTmux,
	})
	defer m.Close()

	s := reapedSession(t, m, "reaped-settle")

	snapshot, unsub := collectStateChanges(m)
	defer unsub()

	m.updateSessionStatus(s, StatusStopped)

	events := snapshot()
	require.Len(t, events, 1, "terminal→terminal transition was suppressed")
	assert.Equal(t, StatusStopped, events[0].NewStatus)
}

// TestLiveSessionStillTransitions pins that the guard is scoped to terminal
// sessions: an ordinary running→idle must still emit.
func TestLiveSessionStillTransitions(t *testing.T) {
	t.Parallel()

	m := NewManagerWithConfig(ManagerConfig{
		RepoName:    "test-repo",
		SessionMode: SessionModeTmux,
	})
	defer m.Close()

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

// TestReconcileSkipsSessionWhoseWorktreeIsGone pins the decision that keeps a
// reaped session from being re-adopted on the next start.
//
// Window names are recycled — GenerateTmuxWindowName hands out the lowest free
// "repo/worktree:N" — so a killed session's recorded name is answered by the
// next session on that worktree. A stored record that kept only a name then
// looks alive, gets adopted into m.sessions with a monitor goroutine, and emits
// fresh idle transitions. The courier's reported-set does not survive a
// restart, so the parent hears about it again every time.
func TestReconcileSkipsSessionWhoseWorktreeIsGone(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "removed-worktree")
	stored := &StoredSession{
		ID:             "stale-1",
		Status:         StatusIdle,
		WorktreePath:   missing,
		TmuxWindowName: "test-repo/feature:0",
		RunnerType:     RunnerTypeTmux,
	}

	assert.Equal(t, reconcileReap, decideReconcile(stored, true),
		"session with a removed worktree was adopted as live")
}

// TestReconcileAdoptsLiveSession keeps the worktree gate from reaping healthy
// sessions, and keeps a dead window decisive on its own.
func TestReconcileAdoptsLiveSession(t *testing.T) {
	t.Parallel()

	live := t.TempDir()
	stored := &StoredSession{
		ID:             "live-1",
		Status:         StatusIdle,
		WorktreePath:   live,
		TmuxWindowName: "test-repo/feature:0",
		RunnerType:     RunnerTypeTmux,
	}

	assert.Equal(t, reconcileAdopt, decideReconcile(stored, true))
	assert.Equal(t, reconcileReap, decideReconcile(stored, false),
		"a dead window must be reaped even when the worktree survives")
}

// TestReconcileWithoutWorktreePathDefersToWindow keeps an unrecorded worktree
// path from being read as evidence the session is gone.
func TestReconcileWithoutWorktreePathDefersToWindow(t *testing.T) {
	t.Parallel()

	stored := &StoredSession{
		ID:             "no-path",
		Status:         StatusIdle,
		TmuxWindowName: "test-repo/feature:0",
		RunnerType:     RunnerTypeTmux,
	}

	assert.Equal(t, reconcileAdopt, decideReconcile(stored, true))
	assert.Equal(t, reconcileReap, decideReconcile(stored, false))
}
