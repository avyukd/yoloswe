package session

import (
	"context"
	"fmt"
	"strings"
)

// subagentReportPrefix marks a courier-generated message so a reader can tell
// bramble's own reporting apart from a peer's prose.
const subagentReportPrefix = "[bramble]"

// shouldReport decides whether a child reaching status is worth telling its
// parent about, and records the decision so the same news is not sent twice.
//
// The rule is deliberately quiet. A subagent typically goes idle once, when its
// one-shot prompt is answered, and that is the report the parent is waiting
// for. A later completed/stopped — a tmux window closing, say — carries no new
// information, so it is only reported when nothing has been reported yet.
// A failure is always worth knowing, even after a successful report, because it
// changes what the parent should do next.
func (c *Courier) shouldReport(child SessionID, status SessionStatus) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	seen := c.reportedForLocked(child)
	if seen[status] {
		return false
	}

	switch status {
	case StatusFailed:
		// Always worth a word.
	case StatusIdle:
		// The normal "here is your result" moment.
	case StatusCompleted, StatusStopped:
		// Only if the parent has heard nothing at all so far. Every entry that
		// is present is true — resetIdleReport deletes rather than clears — so
		// a non-empty set means something has been reported.
		if len(seen) > 0 {
			return false
		}
	default:
		return false
	}

	seen[status] = true
	return true
}

// reportedForLocked returns the child's reporting history, creating it on first
// use. The caller must hold c.mu.
func (c *Courier) reportedForLocked(child SessionID) map[SessionStatus]bool {
	seen := c.reported[child]
	if seen == nil {
		seen = make(map[SessionStatus]bool)
		c.reported[child] = seen
	}
	return seen
}

// unmarkReported undoes a shouldReport reservation whose delivery then failed,
// so the next eligible transition tries again.
//
// shouldReport has to reserve the status up front — two transitions for one
// child would otherwise both pass the check and both report — but a reservation
// that is never released turns one transient tmux error into a report the
// parent never gets and nothing ever retries.
func (c *Courier) unmarkReported(child SessionID, status SessionStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if seen := c.reported[child]; seen != nil {
		delete(seen, status)
	}
}

// noteChildSpoke records that a child has messaged its parent directly, so the
// courier does not then repeat itself with a generated report. A subagent that
// writes its own summary always says it better than this file can.
func (c *Courier) noteChildSpoke(child SessionID) {
	if child == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := c.reportedForLocked(child)
	// Mark the states a self-report stands in for. A later failure still
	// reports, because the child's own message predates it.
	seen[StatusIdle] = true
	seen[StatusCompleted] = true
	seen[StatusStopped] = true
}

// resetIdleReport re-arms idle reporting for a session that has just been sent
// a message.
//
// Reporting is per-turn, not per-lifetime. A subagent goes idle once when its
// opening prompt is answered and is reported then; but if its parent replies,
// the child starts another turn and the answer to *that* is news the parent is
// waiting on just as much. Without this, a two-way conversation goes silent
// after the first exchange and the parent is left polling — the thing the
// queue exists to avoid.
//
// Only a recipient that can still take a turn earns that. write guards the
// delivery — it re-arms only when the write succeeded — and this guards the
// recipient, for the paths that reach here without one: Watch re-arms on a
// StatusRunning transition, and a session can be reaped between the write and
// the reset. Re-arming a session that will never run again leaves the door open
// for its next stale idle to reach the parent as a fresh answer (#330, #331).
func (c *Courier) resetIdleReport(to SessionID) {
	if !c.isLive(to) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if seen := c.reported[to]; seen != nil {
		delete(seen, StatusIdle)
	}
}

// isLive reports whether a session can still start another turn.
//
// Absence is the load-bearing half. A session reaped with tmux kill-window —
// the teardown subagent-swarm's docs recommend, because a wedged session cannot
// process /exit — may never emit a terminal transition, so it disappears from
// the registry without ever running forgetChild. "Gone" is the only signal that
// case leaves behind.
func (c *Courier) isLive(id SessionID) bool {
	info, ok := c.target.SessionInfo(id)
	return ok && !info.Status.IsTerminal()
}

// forgetChild drops a finished child's reporting history so the map does not
// grow for the life of the process.
func (c *Courier) forgetChild(child SessionID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.reported, child)
}

// reportToParent tells a child's parent that the child reached status.
//
// This is what makes a non-Claude backend usable as a subagent. Codex has no
// system prompt, no MCP and no tool restrictions in this wrapper, so a codex
// child cannot be reliably instructed to call back — the Reporting section in
// its prompt is a suggestion it may ignore. Generating the report from the
// session's own state means the parent hears back regardless of which CLI ran,
// and regardless of whether the child cooperated.
func (c *Courier) reportToParent(ctx context.Context, child SessionInfo) {
	if child.ParentSessionID == "" {
		return
	}
	// Check the parent can still take a message before composing one:
	// resultPathFor captures two thousand lines of pane and writes them to a
	// file, all of it discarded if the parent has already gone.
	parent, ok := c.target.SessionInfo(child.ParentSessionID)
	if !ok || parent.Status.IsTerminal() {
		return
	}
	// A child the registry no longer knows has been reaped, and a non-terminal
	// status for it is stale by definition: nothing is running, so there is no
	// turn whose answer this could be. This is the guard that survives a
	// restart, where the in-memory dedup map is empty and absence is the only
	// evidence left that the "is idle" is hours stale (#330, #331).
	//
	// A terminal status for an absent child is the opposite and must survive:
	// the manager deletes a completed tmux session before this callback runs,
	// so "completed" for a session already gone is a normal last report.
	//
	// Only the report is dropped, never the child's history. One monitor tick
	// can queue a non-terminal and a terminal event for the same reaped child,
	// and both drain after the delete; forgetting here would let the terminal
	// one past shouldReport's "has the parent heard anything at all" gate and
	// deliver the duplicate this guard exists to stop. So a child reaped
	// without a terminal transition keeps its entry for the process lifetime —
	// a bounded leak of a small map, which is the cheaper half of that trade.
	if _, known := c.target.SessionInfo(child.ID); !known && !child.Status.IsTerminal() {
		return
	}
	if !c.shouldReport(child.ID, child.Status) {
		return
	}
	resultPath := c.resultPathFor(child)
	// deliver, not Send: this report is composed by the courier, so it must not
	// register as the child having spoken for itself — that would suppress the
	// reports the courier still owes for the child's remaining states.
	if _, err := c.deliver(ctx, child.ID, child.ParentSessionID, formatSubagentReport(child, resultPath), true); err != nil {
		logDeliveryWarn("failed to report subagent completion", child.ParentSessionID, err)
		c.unmarkReported(child.ID, child.Status)
	}
}

// tmuxCaptureLines is how much scrollback a tmux subagent's result file keeps.
// Generous: it is the only record of what that session did, and a pane that
// scrolled past this is lost either way.
const tmuxCaptureLines = 2000

// resultPathFor picks the file a parent should read for a child's output.
//
// A plan is the most specific artifact, then a transcript. A tmux-mode child
// has neither: that mode returns from runSession as soon as the window is up,
// so no turn loop ever records what the session said. Its pane is captured to a
// file here so the parent still gets a path instead of being told to go and
// look at a window that will scroll away.
func (c *Courier) resultPathFor(child SessionInfo) string {
	if child.PlanFilePath != "" {
		return child.PlanFilePath
	}
	if child.ResearchFilePath != "" {
		return child.ResearchFilePath
	}
	if !isTmuxRunner(child.RunnerType) {
		return ""
	}

	lines, err := c.target.CapturePaneText(child.ID, tmuxCaptureLines)
	if err != nil {
		// The session-keyed capture misses once the manager has dropped a
		// completed session, which is precisely when this runs. The window
		// itself often outlives it — tmux keeps it open under remain-on-exit —
		// so fall back to the target recorded on the event's own snapshot.
		if target := child.TmuxTarget(); target != "" {
			lines, err = CaptureTmuxPane(target, tmuxCaptureLines)
		}
	}
	if err != nil || len(lines) == 0 {
		// Not worth failing the report over — the parent still learns the
		// child finished, just without a pointer to read.
		return ""
	}
	path, err := ResultFilePath(c.resultDir, child.ID)
	if err != nil {
		return ""
	}
	body := strings.Join(lines, "\n") + "\n"
	// writeFileAtomic, not os.WriteFile: 0600 because a captured pane can hold
	// anything the subagent printed, and the create-and-rename replaces a
	// symlink someone planted at this predictable path instead of writing
	// through it.
	if err := writeFileAtomic(path, []byte(body), 0o600); err != nil {
		logDeliveryWarn("failed to write captured pane", child.ID, err)
		return ""
	}
	return path
}

// formatSubagentReport renders the message a parent receives.
//
// It is a pointer, not a payload: a one-line headline plus the path to the
// child's full output. Pasting a transcript into a parent's prompt would burn
// its context on text it may not need, and a pane scrolls away while a file
// does not — the same reason the delegator reads a child's research file
// instead of capturing its screen.
func formatSubagentReport(child SessionInfo, resultPath string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s subagent %s (%s", subagentReportPrefix, child.ID, child.Type)
	if child.Model != "" {
		fmt.Fprintf(&b, ", %s", child.Model)
	}
	fmt.Fprintf(&b, ") is %s", child.Status)
	if child.Progress.TurnCount > 0 {
		fmt.Fprintf(&b, " — %d turn(s)", child.Progress.TurnCount)
		if child.Progress.TotalCostUSD > 0 {
			fmt.Fprintf(&b, ", $%.4f", child.Progress.TotalCostUSD)
		}
	}

	if child.ErrorMsg != "" {
		fmt.Fprintf(&b, "\nerror: %s", child.ErrorMsg)
	}
	if resultPath != "" {
		fmt.Fprintf(&b, "\n%s: %s", resultLabel(child, resultPath), resultPath)
	}

	return b.String()
}

// resultLabel names what the reported path actually is.
//
// The distinction matters to whoever reads the file. A plan or a research file
// is something the agent wrote; a pane capture is 2000 lines of terminal
// scrollback, which for a tmux child that never answered opens with the CLI's
// startup frame followed by the brief that was pasted in — so a consumer
// reading it for "what the lane concluded" gets its own prompt back (#331).
// Both land in the same directory under the same name, so the label is the only
// thing that can tell them apart.
func resultLabel(child SessionInfo, resultPath string) string {
	if resultPath == "" {
		// A child with no plan and no research file — the normal shape of a
		// tmux child — has "" in both fields, so an empty path would otherwise
		// match the first case and be labelled a plan.
		return ""
	}
	switch resultPath {
	case child.PlanFilePath:
		return "plan"
	case child.ResearchFilePath:
		return "result"
	default:
		return "pane-capture"
	}
}
