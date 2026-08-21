package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// RestartStateEnvVar names the file holding the state handed from a bramble
// process to the image that replaces it via an in-place exec restart.
const RestartStateEnvVar = "BRAMBLE_RESTART_STATE"

// restartStateVersion guards the on-disk shape. A state file written by a
// different version is ignored rather than half-applied: restart state is
// disposable, and starting fresh beats restoring nonsense.
const restartStateVersion = 1

// RestartState is the slice of a running TUI that has no other way back.
//
// It is deliberately small. Sessions, their tmux windows, and their output all
// survive on their own — the session store plus Manager.ReconcileTmuxSessions
// and session.ReposWithLiveTmuxSessions re-adopt them at startup. What those
// cannot reconstruct is which repo the user was looking at (repos with no live
// tmux session are invisible to that scan) and where the cursor was.
type RestartState struct {
	// FromBuildTime is the mtime of the binary being replaced, used only to
	// tell the user which build they left behind.
	FromBuildTime time.Time `json:"from_build_time,omitzero"`
	// ActiveRepo is the repo the TUI was showing. It outranks --repo and cwd
	// detection on the next start.
	ActiveRepo string `json:"active_repo"`
	// SelectedWorktree is the worktree name selected in the dropdown.
	SelectedWorktree string `json:"selected_worktree,omitempty"`
	// ViewingSessionID is the session whose output was on screen.
	ViewingSessionID string `json:"viewing_session_id,omitempty"`
	// OpenedRepos is every repo open in the TUI, including ActiveRepo.
	OpenedRepos []string `json:"opened_repos,omitempty"`
	// Version is restartStateVersion at write time.
	Version int `json:"version"`
}

// DefaultRestartStateDir returns ~/.bramble/restart, alongside the session
// store.
func DefaultRestartStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(home, ".bramble", "restart"), nil
}

// RestartStatePath returns the state file path for this process. It is keyed by
// pid so that concurrent bramble processes restarting at once cannot clobber
// each other's handoff.
func RestartStatePath() (string, error) {
	dir, err := DefaultRestartStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("%d.json", os.Getpid())), nil
}

// WriteRestartState writes state to path via temp file + rename, so a reader
// (the next process image) never observes a partial file.
func WriteRestartState(path string, state RestartState) error {
	state.Version = restartStateVersion
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create restart state dir: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal restart state: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create restart state temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write restart state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close restart state: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename restart state: %w", err)
	}
	return nil
}

// LoadRestartState reads a state file written by WriteRestartState.
func LoadRestartState(path string) (*RestartState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read restart state: %w", err)
	}
	var state RestartState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse restart state: %w", err)
	}
	if state.Version != restartStateVersion {
		return nil, fmt.Errorf("unsupported restart state version %d (want %d)", state.Version, restartStateVersion)
	}
	return &state, nil
}

// LoadRestartStateFromEnv consumes the handoff named by $BRAMBLE_RESTART_STATE:
// it unsets the variable, loads the file, and deletes it. Returns nil when
// there is no handoff or it cannot be read — a failed restore degrades to a
// normal cold start, which is always safe.
//
// Unsetting is not optional. The variable is inherited by every child this
// process spawns, and a second restart later in this process's life would
// otherwise re-apply a stale snapshot on top of wherever the user had navigated
// to since.
func LoadRestartStateFromEnv() *RestartState {
	path := os.Getenv(RestartStateEnvVar)
	if path == "" {
		return nil
	}
	os.Unsetenv(RestartStateEnvVar)
	// Remove regardless of whether the parse succeeds: a file we cannot use is
	// a file nobody should retry.
	defer os.Remove(path)
	state, err := LoadRestartState(path)
	if err != nil {
		return nil
	}
	return state
}
