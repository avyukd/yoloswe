package main

import (
	"fmt"
	"sync"

	"github.com/bazelment/yoloswe/bramble/app"
)

// restartRequester routes an out-of-band restart request (SIGUSR2, IPC) into
// the Bubble Tea event loop.
//
// It exists because the IPC server binds and starts serving well before
// tea.NewProgram exists, so the handler cannot close over the program directly.
// It closes over this holder instead, which is filled in once the program is
// created and emptied again when it exits. A request that lands outside that
// window is refused honestly rather than dropped.
type restartRequester struct {
	send func(app.RestartRequestedMsg)
	mu   sync.Mutex
}

// set installs the delivery func, or clears it when send is nil.
func (r *restartRequester) set(send func(app.RestartRequestedMsg)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.send = send
}

// request delivers a restart request to the TUI. It reports an error when no
// TUI is accepting messages — before the program starts, or after it exits.
func (r *restartRequester) request(msg app.RestartRequestedMsg) error {
	r.mu.Lock()
	send := r.send
	r.mu.Unlock()
	if send == nil {
		return fmt.Errorf("no bramble TUI is running to restart")
	}
	send(msg)
	return nil
}

// mergeResumeRepos appends the repos a restart handoff had open onto the ones
// found by the live tmux scan, dropping duplicates and activeRepo.
//
// activeRepo is excluded because its manager is constructed directly and
// reconciles itself; re-listing it here would have the TUI open it a second
// time. Order is preserved so repos with live tmux windows are re-adopted first.
func mergeResumeRepos(live []string, restored *app.RestartState, activeRepo string) []string {
	extra := restored.Repos()
	seen := make(map[string]bool, len(live)+len(extra))
	seen[activeRepo] = true
	merged := make([]string, 0, len(live)+len(extra))
	for _, repos := range [][]string{live, extra} {
		for _, repo := range repos {
			if repo == "" || seen[repo] {
				continue
			}
			seen[repo] = true
			merged = append(merged, repo)
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}
