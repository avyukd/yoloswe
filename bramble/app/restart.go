package app

import (
	"fmt"
	"slices"

	tea "charm.land/bubbletea/v2"

	"github.com/bazelment/yoloswe/bramble/selfexec"
	"github.com/bazelment/yoloswe/bramble/session"
)

// RestartRequestedMsg asks the TUI to restart into the binary now on disk. It
// is exported because the request can arrive from outside the Bubble Tea event
// loop — a SIGUSR2 handler or an IPC request — via Program.Send.
type RestartRequestedMsg struct {
	// Force skips the "these sessions will be lost" confirmation. Only the
	// `bramble restart --force` path sets it; a keypress never does, because a
	// user at the keyboard can answer the prompt.
	Force bool
}

// RestartRequested reports whether the TUI exited in order to be replaced by a
// new binary rather than to quit. main() reads this off the final model.
func (m Model) RestartRequested() bool { return m.restartRequested }

// RestartSnapshot captures the view state that an exec restart cannot otherwise
// recover. Everything else — sessions, their tmux windows, their history —
// comes back through the session store and tmux reconciliation.
func (m Model) RestartSnapshot() RestartState {
	state := RestartState{
		ActiveRepo:       m.repoName,
		ViewingSessionID: string(m.viewingSessionID),
		OpenedRepos:      slices.Clone(m.openedRepos),
	}
	if item := m.worktreeDropdown.SelectedItem(); item != nil {
		state.SelectedWorktree = item.ID
	}
	return state
}

// DisableTmuxExitOnQuitAll clears TmuxExitOnQuit on every open repo's manager.
// Close() reads that flag as "the user is finished, tear the windows down",
// which is the opposite of what a restart wants: those windows are precisely
// what the replacement process re-adopts.
func (m Model) DisableTmuxExitOnQuitAll() {
	for _, rc := range m.repos {
		if rc.sessionManager != nil {
			rc.sessionManager.DisableTmuxExitOnQuit()
		}
	}
}

// requestRestart runs the pre-flight for an in-place restart: refuse outright if
// the binary on disk could not be exec'd, ask first if live work would be lost,
// and otherwise quit with restartRequested set so main() execs instead of
// returning.
//
// Verifying before tea.Quit is deliberate: a restart that fails after the TUI
// has torn itself down leaves the user staring at a shell.
func (m Model) requestRestart(force bool) (Model, tea.Cmd) {
	if err := selfexec.Verify(); err != nil {
		cmd := m.addToast(fmt.Sprintf("Cannot restart: %v", err), ToastError)
		return m, cmd
	}

	// Tmux-mode sessions are children of the tmux server and outlive the exec.
	// TUI-mode sessions are our own children, spawned with Pdeathsig, and die
	// with the process image — those are the ones worth warning about.
	if lost := m.countActiveSessions(true); !force && lost > 0 {
		model, cmd := m.showConfirm(
			fmt.Sprintf("%d in-process session(s) will be lost by restarting. Restart anyway?", lost),
			[]ConfirmOption{{Key: "y", Label: "restart"}},
			func(string) tea.Cmd { return func() tea.Msg { return RestartRequestedMsg{Force: true} } },
		)
		return model.(Model), cmd
	}

	m.restartRequested = true
	return m, tea.Quit
}

// ApplyRestartState restores the view state handed over by the previous process
// image. A nil state (normal cold start) is a no-op.
//
// The worktree selection has to cope with both startup paths. main() usually
// pre-loads worktrees before building the model, and Init() then skips the
// async refresh entirely — so the selection must be applied right here.
// Deferring to pendingWorktreeSelect is only correct when worktrees have yet to
// load, because that field is drained solely by the worktreesMsg handler.
func (m *Model) ApplyRestartState(state *RestartState) {
	if state == nil {
		return
	}
	if state.SelectedWorktree != "" {
		switch {
		case m.worktreeDropdown.SelectByID(state.SelectedWorktree):
			m.updateSessionDropdown()
		case !m.worktreesLoaded:
			m.pendingWorktreeSelect = state.SelectedWorktree
		}
	}
	if state.ViewingSessionID != "" {
		m.viewingSessionID = session.SessionID(state.ViewingSessionID)
	}
}
