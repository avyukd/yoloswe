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

func TestListSessionsProjectsTmuxWindowIDs(t *testing.T) {
	t.Parallel()

	registry := listSessionsRegistry(t,
		&session.Session{ID: "with-id", TmuxWindowName: "repo/feature:1", TmuxWindowID: "@7"},
		&session.Session{ID: "name-only", TmuxWindowName: "repo/feature:2"},
		&session.Session{ID: "windowless"},
	)

	result := handleListSessions(registry)

	require.Len(t, result.Sessions, 3)
	targets := make(map[string]string, len(result.Sessions))
	for _, s := range result.Sessions {
		targets[s.ID] = s.TmuxTarget
	}
	assert.Equal(t, map[string]string{"with-id": "@7", "name-only": "", "windowless": ""}, targets)
}

func TestListSessionsOverTheSocketCarriesTmuxTarget(t *testing.T) {
	t.Parallel()

	registry := listSessionsRegistry(t,
		&session.Session{ID: "sess-live", Status: session.StatusRunning, TmuxWindowID: "@42", RunnerType: "tmux"},
		&session.Session{ID: "sess-windowless", Status: session.StatusFailed, RunnerType: "tui"},
	)

	sockPath := filepath.Join(t.TempDir(), "bramble-test.sock")
	srv, err := startIPCServer(registry, sockPath, t.TempDir(), "repo-a")
	require.NoError(t, err)
	defer func() { _ = srv.Close() }()
	go srv.Serve()

	resp, err := ipc.NewClient(sockPath).Send(&ipc.Request{
		Type: ipc.RequestListSessions,
		ID:   "e2e-list-sessions",
	})
	require.NoError(t, err)
	require.True(t, resp.OK, "server error: %s", resp.Error)

	raw, err := json.Marshal(resp.Result)
	require.NoError(t, err)
	var list ipc.ListSessionsResult
	require.NoError(t, json.Unmarshal(raw, &list))

	targets := make(map[string]string, len(list.Sessions))
	for i := range list.Sessions {
		targets[list.Sessions[i].ID] = list.Sessions[i].TmuxTarget
	}
	assert.Equal(t, map[string]string{"sess-live": "@42", "sess-windowless": ""}, targets)

	assert.NotContains(t, string(raw), `"tmux_target":""`)
	assert.Contains(t, string(raw), `"tmux_target":"@42"`)
}
