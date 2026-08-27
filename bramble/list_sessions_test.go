package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/ipc"
	"github.com/bazelment/yoloswe/bramble/session"
)

// listSessionsRegistry builds a registry holding exactly the given sessions, so
// a test can pin what handleListSessions projects onto the wire.
func listSessionsRegistry(t *testing.T, sessions ...*session.Session) *session.SessionRegistry {
	t.Helper()

	mgr := session.NewManagerWithConfig(session.ManagerConfig{RepoName: "repo-a"})
	t.Cleanup(mgr.Close)
	for _, s := range sessions {
		mgr.AddSession(s)
	}

	registry := session.NewSessionRegistry()
	registry.Register(mgr)
	return registry
}

// TestListSessionsCarriesTheTmuxWindowID is the whole point of the field. A
// caller that wants to act on one session's tmux window — kill it, capture it,
// select it — has nothing else to address it by: the CLI used to return only
// id/type/status/worktree_name/prompt/model/backend/parent_session_id, which
// forced external tooling into matching on window names or indexes. Both are
// unstable: bramble renames a window to "!"+name when it wants attention
// (session.TmuxNotifyPrefix), and tmux reuses indexes as windows come and go.
// A /subagent-swarm reaper that matched on names for exactly this reason killed
// 18 healthy sessions on 2026-08-27.
func TestListSessionsCarriesTheTmuxWindowID(t *testing.T) {
	t.Parallel()

	registry := listSessionsRegistry(t, &session.Session{
		ID:             "sess-tmux",
		Type:           session.SessionTypeBuilder,
		Status:         session.StatusRunning,
		WorktreeName:   "feature-x",
		TmuxWindowName: "yoloswe/feature-x:1",
		TmuxWindowID:   "@42",
		RunnerType:     "tmux",
	})

	result := handleListSessions(registry)

	require.Len(t, result.Sessions, 1)
	assert.Equal(t, "@42", result.Sessions[0].TmuxTarget,
		"the window ID must reach the caller, or nothing outside bramble can address this session's window")
}

// TestListSessionsDoesNotFallBackToTheWindowName pins the deliberate difference
// from the control socket's projection (control.Dispatcher.sessionList), which
// falls back to TmuxWindowName when the ID is empty. The IPC surface stays
// ID-only on purpose: a name is not a stable target — bramble's own
// NotifyTmuxWindow renames the window out from under any caller holding one,
// and session.TmuxNotifyPrefix means the name a caller sees may already be
// decorated. Handing a caller a name it could pass to `tmux kill-window`
// reintroduces exactly the ambiguity this field exists to remove. A session
// with no captured window ID reports no target, and the caller must handle the
// empty string rather than be silently given something weaker.
func TestListSessionsDoesNotFallBackToTheWindowName(t *testing.T) {
	t.Parallel()

	registry := listSessionsRegistry(t, &session.Session{
		ID:             "sess-name-only",
		Type:           session.SessionTypeBuilder,
		Status:         session.StatusRunning,
		WorktreeName:   "feature-y",
		TmuxWindowName: "yoloswe/feature-y:2",
		RunnerType:     "tmux",
	})

	result := handleListSessions(registry)

	require.Len(t, result.Sessions, 1)
	assert.Empty(t, result.Sessions[0].TmuxTarget,
		"a window name must never be served as a tmux target: renaming invalidates it")
}

// TestListSessionsReportsNoTargetForASessionThatNeverGotAWindow covers the
// session that failed before a window existed. Fabricating a target here is
// worse than reporting none: a caller reaping dead sessions would be handed a
// string that resolves to whatever tmux currently has at that address.
func TestListSessionsReportsNoTargetForASessionThatNeverGotAWindow(t *testing.T) {
	t.Parallel()

	registry := listSessionsRegistry(t, &session.Session{
		ID:           "sess-failed",
		Type:         session.SessionTypeBuilder,
		Status:       session.StatusFailed,
		WorktreeName: "feature-z",
		RunnerType:   "tui",
	})

	result := handleListSessions(registry)

	require.Len(t, result.Sessions, 1)
	assert.Equal(t, string(session.StatusFailed), result.Sessions[0].Status)
	assert.Empty(t, result.Sessions[0].TmuxTarget,
		"a session that never had a window must not be given a target")
}

// TestListSessionsKeepsEachSessionsOwnTarget guards the projection loop itself:
// the mapping is per-session, so a caller can tell two live windows apart. A
// loop that leaked one session's target onto another would be worse than no
// field at all — it would aim a kill at the wrong window.
func TestListSessionsKeepsEachSessionsOwnTarget(t *testing.T) {
	t.Parallel()

	registry := listSessionsRegistry(t,
		&session.Session{ID: "sess-a", Status: session.StatusRunning, TmuxWindowID: "@7", RunnerType: "tmux"},
		&session.Session{ID: "sess-b", Status: session.StatusRunning, TmuxWindowID: "@9", RunnerType: "tmux"},
		&session.Session{ID: "sess-c", Status: session.StatusRunning, RunnerType: "tui"},
	)

	result := handleListSessions(registry)

	require.Len(t, result.Sessions, 3)
	targets := make(map[string]string, len(result.Sessions))
	for _, s := range result.Sessions {
		targets[s.ID] = s.TmuxTarget
	}
	assert.Equal(t, map[string]string{"sess-a": "@7", "sess-b": "@9", "sess-c": ""}, targets)
}

// TestListSessionsOverTheSocketCarriesTmuxTarget is the end-to-end check the
// unit tests above cannot make: it runs the real IPC server on a scratch
// socket and reads the field back through the same JSON round-trip
// `bramble list-sessions` uses. The field is only useful if it survives the
// wire — a struct field the encoder drops would still pass every test above.
func TestListSessionsOverTheSocketCarriesTmuxTarget(t *testing.T) {
	t.Parallel()

	registry := listSessionsRegistry(t,
		&session.Session{ID: "sess-live", Status: session.StatusRunning, TmuxWindowID: "@42", RunnerType: "tmux"},
		&session.Session{ID: "sess-windowless", Status: session.StatusFailed, RunnerType: "tui"},
	)

	// A socket under the test's own dir, so parallel tests never collide.
	sockPath := filepath.Join(t.TempDir(), "bramble-test.sock")
	srv, err := startIPCServer(registry, sockPath, t.TempDir(), "repo-a")
	require.NoError(t, err)
	// startIPCServer already binds; Serve is the accept loop.
	defer func() { _ = srv.Close() }()
	go srv.Serve()

	resp, err := ipc.NewClient(sockPath).Send(&ipc.Request{
		Type: ipc.RequestListSessions,
		ID:   "e2e-list-sessions",
	})
	require.NoError(t, err)
	require.True(t, resp.OK, "server error: %s", resp.Error)

	// Re-marshal through the typed struct exactly as the CLI does, so this
	// asserts on what a caller actually decodes.
	raw, err := json.Marshal(resp.Result)
	require.NoError(t, err)
	var list ipc.ListSessionsResult
	require.NoError(t, json.Unmarshal(raw, &list))

	targets := make(map[string]string, len(list.Sessions))
	for i := range list.Sessions {
		targets[list.Sessions[i].ID] = list.Sessions[i].TmuxTarget
	}
	assert.Equal(t, map[string]string{"sess-live": "@42", "sess-windowless": ""}, targets)

	// And the key is absent, not empty, for the windowless session — the
	// distinction a reaper depends on.
	assert.NotContains(t, string(raw), `"tmux_target":""`)
	assert.Contains(t, string(raw), `"tmux_target":"@42"`)
}
