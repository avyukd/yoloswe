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
		FromBuildTime:    time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC),
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
	path := filepath.Join(t.TempDir(), "state.json")
	require.NoError(t, WriteRestartState(path, RestartState{ActiveRepo: "yoloswe"}))
	t.Setenv(RestartStateEnvVar, path)

	got := LoadRestartStateFromEnv()
	require.NotNil(t, got)
	assert.Equal(t, "yoloswe", got.ActiveRepo)

	// Both the env var and the file must be gone. The var is inherited by every
	// child we spawn, and a later restart in this same process would otherwise
	// re-apply this stale snapshot over wherever the user had navigated to.
	assert.Empty(t, os.Getenv(RestartStateEnvVar))
	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err), "state file should be removed after being consumed")
}

func TestLoadRestartStateFromEnvDegradesToColdStart(t *testing.T) {
	corrupt := filepath.Join(t.TempDir(), "corrupt.json")
	require.NoError(t, os.WriteFile(corrupt, []byte("{not json"), 0o600))

	tests := []struct {
		name string
		path string
	}{
		{"unset", ""},
		{"missing file", filepath.Join(t.TempDir(), "absent.json")},
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
