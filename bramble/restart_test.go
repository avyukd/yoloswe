package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/app"
)

func TestMergeResumeRepos(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		activeRepo string
		live       []string
		extra      []string
		want       []string
	}{
		{
			name:       "no handoff leaves the live scan untouched",
			activeRepo: "a",
			live:       []string{"b", "c"},
			want:       []string{"b", "c"},
		},
		{
			// The live scan only finds repos with a live tmux window, so a repo
			// the user had open but left idle arrives only via the handoff.
			name:       "handoff contributes idle repos",
			activeRepo: "a",
			live:       []string{"b"},
			extra:      []string{"c"},
			want:       []string{"b", "c"},
		},
		{
			// Live repos come first so they are re-adopted before idle ones.
			name:       "duplicates keep the live ordering",
			activeRepo: "a",
			live:       []string{"b", "c"},
			extra:      []string{"c", "b", "d"},
			want:       []string{"b", "c", "d"},
		},
		{
			// The active repo's manager is constructed directly and reconciles
			// itself; listing it here would open it a second time.
			name:       "active repo is excluded from both sources",
			activeRepo: "a",
			live:       []string{"a", "b"},
			extra:      []string{"a", "c"},
			want:       []string{"b", "c"},
		},
		{
			name:       "empty names are dropped",
			activeRepo: "a",
			extra:      []string{"", "b", ""},
			want:       []string{"b"},
		},
		{
			name:       "nothing to resume yields nil",
			activeRepo: "a",
			live:       []string{"a"},
			extra:      []string{"a"},
			want:       nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var restored *app.RestartState
			if tt.extra != nil {
				restored = &app.RestartState{OpenedRepos: tt.extra}
			}
			assert.Equal(t, tt.want, mergeResumeRepos(tt.live, restored, tt.activeRepo))
		})
	}
}

func TestRestartRequesterRefusesWhenNoTUIIsRunning(t *testing.T) {
	t.Parallel()

	// The IPC server binds long before tea.NewProgram exists and keeps serving
	// until after the program exits, so a request can arrive outside the window
	// where anything could act on it. That must be an honest error, not a
	// silently dropped request.
	var r restartRequester
	err := r.request(app.RestartRequestedMsg{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no bramble TUI is running")
}

func TestRestartRequesterDeliversAndClears(t *testing.T) {
	t.Parallel()

	var r restartRequester
	got := make(chan app.RestartRequestedMsg, 1)
	r.set(func(msg app.RestartRequestedMsg) { got <- msg })

	require.NoError(t, r.request(app.RestartRequestedMsg{Force: true}))
	select {
	case msg := <-got:
		assert.True(t, msg.Force)
	default:
		t.Fatal("expected the request to be delivered")
	}

	r.set(nil)
	assert.Error(t, r.request(app.RestartRequestedMsg{}))
}

// TestSocketPathsSurviveAnExecRestart pins the property the whole in-place
// restart depends on.
//
// Both socket paths are a pure function of the pid, and syscall.Exec preserves
// the pid — so the replacement image recomputes exactly the paths that are
// already baked into every live tmux window's environment, and re-binds them.
// If either helper ever mixed in something that changes across an exec (a start
// timestamp, a random suffix, a boot id), every already-running session would
// go permanently mute on the notify and control paths, silently.
func TestSocketPathsSurviveAnExecRestart(t *testing.T) {
	t.Parallel()

	pid := os.Getpid()
	assert.Contains(t, ipcSocketPath(), fmt.Sprintf("bramble-%d.sock", pid))
	assert.Contains(t, controlSocketPath(), fmt.Sprintf("bramble-control-%d.sock", pid))

	// Recomputing after the process image is replaced must yield the same
	// answer; nothing but the pid may feed into these.
	assert.Equal(t, ipcSocketPath(), ipcSocketPath())
	assert.Equal(t, controlSocketPath(), controlSocketPath())
}

// runTUI stops the signal watcher the moment tea's Run returns and also defers
// it, so the ordinary path calls stop twice. That belt-and-braces pair is what
// keeps a restart request from being accepted during post-run cleanup and
// silently discarded, so stop has to tolerate it — a second close(done) would
// panic and take the process down on every clean exit.
func TestWatchRestartSignalStopIsIdempotent(t *testing.T) {
	t.Parallel()

	stop := watchRestartSignal(func() {})
	stop()
	assert.NotPanics(t, stop, "runTUI calls stop twice on every clean exit")
	assert.NotPanics(t, stop)
}
