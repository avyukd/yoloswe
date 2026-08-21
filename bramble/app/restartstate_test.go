package app

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRestartStateRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "state.json")
	want := RestartState{
		ActiveRepo:       "yoloswe",
		SelectedWorktree: "feat/in-place-start",
		ViewingSessionID: "sess-123",
		OpenedRepos:      []string{"yoloswe", "other"},
	}
	require.NoError(t, WriteRestartState(path, want))

	got, err := LoadRestartState(path)
	require.NoError(t, err)
	want.Version = restartStateVersion
	assert.Equal(t, want, *got)
}

func TestWriteRestartStateStampsVersion(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	require.NoError(t, WriteRestartState(path, RestartState{ActiveRepo: "r"}))

	got, err := LoadRestartState(path)
	require.NoError(t, err)
	assert.Equal(t, restartStateVersion, got.Version)
}

func TestLoadRestartStateRejectsForeignVersion(t *testing.T) {
	t.Parallel()

	// A state file from a different bramble is ignored rather than half
	// applied: restoring a stale view is worse than starting fresh.
	path := filepath.Join(t.TempDir(), "state.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"version":999,"active_repo":"r"}`), 0o600))

	_, err := LoadRestartState(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported restart state version")
}

func TestLoadRestartStateFromEnvConsumesTheHandoff(t *testing.T) {
	// Not parallel: pins $HOME, which is what makes the handoff path an owned
	// one. Production only ever points the env var at RestartStatePath().
	t.Setenv("HOME", t.TempDir())
	path, err := RestartStatePath()
	require.NoError(t, err)
	require.NoError(t, WriteRestartState(path, RestartState{ActiveRepo: "yoloswe"}))
	t.Setenv(RestartStateEnvVar, path)

	got := LoadRestartStateFromEnv()
	require.NotNil(t, got)
	assert.Equal(t, "yoloswe", got.ActiveRepo)

	// Both the env var and the file must be gone. The var is inherited by every
	// child we spawn, and a later restart in this same process would otherwise
	// re-apply this stale snapshot over wherever the user had navigated to.
	assert.Empty(t, os.Getenv(RestartStateEnvVar))
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err), "state file should be removed after being consumed")
}

func TestLoadRestartStateFromEnvDegradesToColdStart(t *testing.T) {
	// Not parallel: pins $HOME so the owned-path check has a restart dir.
	home := t.TempDir()
	t.Setenv("HOME", home)
	restartDir := filepath.Join(home, ".bramble", "restart")
	require.NoError(t, os.MkdirAll(restartDir, 0o700))

	corrupt := filepath.Join(restartDir, "424242.json")
	require.NoError(t, os.WriteFile(corrupt, []byte("{not json"), 0o600))

	tests := []struct {
		name string
		path string
	}{
		{"unset", ""},
		{"missing file", filepath.Join(restartDir, "999999.json")},
		{"corrupt file", corrupt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.path == "" {
				t.Setenv(RestartStateEnvVar, "")
			} else {
				t.Setenv(RestartStateEnvVar, tt.path)
			}
			assert.Nil(t, LoadRestartStateFromEnv())
		})
	}

	// A file we could not parse is a file nobody should retry.
	_, err := os.Stat(corrupt)
	assert.True(t, os.IsNotExist(err), "unusable state file should still be removed")
}

func TestLoadRestartStateFromEnvIgnoresUnownedPaths(t *testing.T) {
	// $BRAMBLE_RESTART_STATE is an inherited environment variable, so a value
	// bramble did not write can reach this code — from a shell profile, a stale
	// export, or a caller pointing it somewhere on purpose. The deletion below
	// is unconditional and silent, so anything that is not recognisably our own
	// handoff must be left completely alone rather than read and unlinked.
	home := t.TempDir()
	t.Setenv("HOME", home)
	restartDir := filepath.Join(home, ".bramble", "restart")
	require.NoError(t, os.MkdirAll(restartDir, 0o700))

	outside := filepath.Join(t.TempDir(), "precious.json")
	nested := filepath.Join(restartDir, "sub")
	require.NoError(t, os.MkdirAll(nested, 0o700))

	tests := []struct {
		name string
		path string
	}{
		{"outside the restart dir", outside},
		{"right dir, foreign name", filepath.Join(restartDir, "settings.json")},
		{"right dir, not a json handoff", filepath.Join(restartDir, "12345.txt")},
		{"below the restart dir", filepath.Join(nested, "12345.json")},
		{"traversal back out", filepath.Join(restartDir, "..", "settings.json")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(tt.path, []byte(`{"version":1}`), 0o600))
			t.Setenv(RestartStateEnvVar, tt.path)

			assert.Nil(t, LoadRestartStateFromEnv(), "an unowned path is not a handoff")
			assert.FileExists(t, tt.path, "an unowned path must never be unlinked")
		})
	}
}

func TestPruneStaleRestartStatesLeavesForeignFiles(t *testing.T) {
	t.Parallel()

	// The sweep walks a whole directory, so it needs the same ownership test:
	// an old file that bramble did not write is not an abandoned handoff.
	dir := t.TempDir()
	old := time.Now().Add(-2 * staleRestartStateAge)

	foreign := []string{"settings.json", "notes.txt", "abc.json", "12345.json.bak"}
	for _, name := range foreign {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte("keep me"), 0o600))
		require.NoError(t, os.Chtimes(p, old, old))
	}
	ours := filepath.Join(dir, "999999.json")
	require.NoError(t, os.WriteFile(ours, []byte(`{"version":1}`), 0o600))
	require.NoError(t, os.Chtimes(ours, old, old))

	pruneStaleRestartStates(dir)

	for _, name := range foreign {
		assert.FileExists(t, filepath.Join(dir, name), "%s is not ours to delete", name)
	}
	_, err := os.Stat(ours)
	assert.True(t, os.IsNotExist(err), "an abandoned handoff should still be swept")
}

func TestRestartStatePathIsPidScoped(t *testing.T) {
	// Not parallel: pins $HOME, which the bazel sandbox does not set.
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Two bramble processes restarting at the same moment must not clobber each
	// other's handoff, which is what the pid in the name buys.
	path, err := RestartStatePath()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".bramble", "restart", fmt.Sprintf("%d.json", os.Getpid())), path)

	again, err := RestartStatePath()
	require.NoError(t, err)
	assert.Equal(t, path, again)
}

func TestWriteRestartStateSweepsAbandonedHandoffs(t *testing.T) {
	t.Parallel()

	// A handoff whose replacement image never started would otherwise sit in
	// ~/.bramble/restart forever; nothing else ever reads that directory.
	dir := t.TempDir()
	stale := filepath.Join(dir, "999999.json")
	require.NoError(t, os.WriteFile(stale, []byte(`{"version":1}`), 0o600))
	old := time.Now().Add(-2 * staleRestartStateAge)
	require.NoError(t, os.Chtimes(stale, old, old))

	fresh := filepath.Join(dir, "12345.json")
	require.NoError(t, os.WriteFile(fresh, []byte(`{"version":1}`), 0o600))

	path := filepath.Join(dir, "state.json")
	require.NoError(t, WriteRestartState(path, RestartState{ActiveRepo: "r"}))

	_, err := os.Stat(stale)
	assert.True(t, os.IsNotExist(err), "abandoned handoff should be swept")
	assert.FileExists(t, fresh, "a recent handoff belongs to a live restart")
	assert.FileExists(t, path)
}
