package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// forget removes a session from the fake registry, standing in for the manager
// dropping a session whose tmux window was killed out from under it. This is
// the state issues #330/#331 describe: the session is absent from
// `bramble list-sessions` while stale events for its ID are still in flight.
func (f *fakeTarget) forget(id SessionID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, id)
}

// reportedIdle reports whether the courier still believes the parent has been
// told about this child's idle.
func reportedIdle(c *Courier, child SessionID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reported[child][StatusIdle]
}

// TestReapedChildIsNotReportedAgainAfterAFailedWrite is the mechanism issues
// #330 and #331 describe, reduced to its parts.
//
// A child is reported idle. Its tmux window is then killed — the teardown
// subagent-swarm's own docs recommend, because a wedged session cannot process
// /exit — so the manager never sees a terminal transition and the child's entry
// stays in c.reported. The orchestrator then sends the child a message; the
// write fails because the window is gone, but write's deferred resetIdleReport
// fires anyway and re-arms idle dedup. A later stale idle event for the same ID
// then passes shouldReport and the parent is told a second time about a lane it
// already merged.
func TestReapedChildIsNotReportedAgainAfterAFailedWrite(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)

	reportNow(c, target, childID)
	require.Len(t, c.Pending(parentID), 1, "the first idle report is the real one")
	require.True(t, reportedIdle(c, childID))

	// The window is killed. The manager saw no transition, so the child is
	// still recorded idle; only tmux knows it is gone.
	target.mu.Lock()
	target.tmuxTarget = ""
	target.mu.Unlock()

	// The orchestrator replies to the child. The write cannot land.
	_, err := c.Send(context.Background(), parentID, childID, "any update?", true)
	_ = err // queued or refused; either way the child never reads it.

	assert.True(t, reportedIdle(c, childID),
		"a write that never reached a reaped child must not re-arm its idle report")

	// A stale idle event for the same ID arrives later, as observed in #331.
	reportNow(c, target, childID)
	assert.Len(t, c.Pending(parentID), 1,
		"a child reaped without a terminal transition must not be reported idle twice")
}

// TestResetIdleReportIgnoresAGoneSession covers the recipient that has vanished
// from the registry entirely: nothing will ever start a turn for it, so there
// is no next answer for the parent to wait on and no reason to re-arm.
func TestResetIdleReportIgnoresAGoneSession(t *testing.T) {
	t.Parallel()
	c, target, _, childID := reportFixture(t, StatusIdle)
	reportNow(c, target, childID)
	require.True(t, reportedIdle(c, childID))

	target.forget(childID)
	c.resetIdleReport(childID)

	assert.True(t, reportedIdle(c, childID),
		"a session absent from the registry must not have its idle report re-armed")
}

// TestResetIdleReportIgnoresATerminalSession is the same guard for a session
// the manager did mark terminal. It can never take another turn, so re-arming
// only leaves the door open for a late duplicate.
func TestResetIdleReportIgnoresATerminalSession(t *testing.T) {
	t.Parallel()
	c, target, _, childID := reportFixture(t, StatusIdle)
	reportNow(c, target, childID)
	require.True(t, reportedIdle(c, childID))

	target.set(childID, StatusStopped, RunnerTypeTmux)
	c.resetIdleReport(childID)

	assert.True(t, reportedIdle(c, childID),
		"a terminal session must not have its idle report re-armed")
}

// TestReapedChildDoesNotReportAgainAfterAResetRace is the interleaving in #330
// stated end to end: reset happens (however it was reached) and a stale idle
// event follows. The child is gone from the registry by then, which is the fact
// that must stop the second report.
func TestReapedChildDoesNotReportAgainAfterAResetRace(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)
	reportNow(c, target, childID)
	require.Len(t, c.Pending(parentID), 1)

	// The child is reaped with tmux kill-window: no terminal transition, and
	// the manager eventually drops it.
	stale := target.mustInfo(childID)
	target.forget(childID)
	c.resetIdleReport(childID)

	// A stale event snapshot for the reaped child, as Watch would pass it.
	c.reportToParent(context.Background(), stale)

	assert.Len(t, c.Pending(parentID), 1,
		"a child that is gone from the registry must not produce a second report")
}

// TestStaleIdleForAGoneChildIsNotReportedAfterARestart is the case dedup state
// alone cannot catch. c.reported lives only in memory, so a bramble restart
// starts with an empty map; a stale idle event for a child reaped before the
// restart then finds nothing saying the parent was already told, passes
// shouldReport, and is delivered. #331 reports exactly this — notifications for
// session IDs absent from `bramble list-sessions`, pointing at result files
// written the previous day.
//
// The child being gone from the registry is the only evidence left at that
// point, so it has to be what stops the report.
func TestStaleIdleForAGoneChildIsNotReportedAfterARestart(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)

	// A fresh process: nothing has been reported for this child, and the child
	// itself was reaped before it started.
	stale := target.mustInfo(childID)
	target.forget(childID)
	require.False(t, reportedIdle(c, childID), "the dedup map starts empty after a restart")

	c.reportToParent(context.Background(), stale)

	assert.Empty(t, c.Pending(parentID),
		"an idle event for a child absent from the registry is stale and must not be reported")
}

// TestTerminalReportForAGoneChildStillLands keeps the guard above from
// swallowing a child's real last word. The manager deletes a completed tmux
// session before the watcher callback runs, so "completed" for a session that
// is already gone is the normal shape of a final report, not a stale one.
func TestTerminalReportForAGoneChildStillLands(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusCompleted)

	final := target.mustInfo(childID)
	target.forget(childID)

	c.reportToParent(context.Background(), final)

	require.Len(t, c.Pending(parentID), 1,
		"a child the manager already dropped was never reported to its parent")
	assert.Contains(t, c.Pending(parentID)[0].Text, "is completed")
}

// TestPaneCaptureIsLabelledDistinctly is #331's third complaint: a tmux-mode
// child has no agent artifact, so resultPathFor falls back to dumping 2000
// lines of pane into the same ~/.bramble/research/<id>.md that a real research
// file uses. A consumer reading it for "what the lane concluded" gets a Codex
// TUI frame and its own pasted brief back. The label has to say which it is.
func TestPaneCaptureIsLabelledDistinctly(t *testing.T) {
	t.Parallel()
	c, target, _, childID := reportFixture(t, StatusIdle)
	target.captured[childID] = []string{"╭──────────╮", "│ >_ OpenAI Codex │", "the brief that was pasted in"}

	child := target.mustInfo(childID)
	path := c.resultPathFor(child)
	require.NotEmpty(t, path, "a tmux child's pane capture is its only record")

	text := formatSubagentReport(child, path)
	assert.Contains(t, text, "pane-capture: "+path,
		"a pane dump must not be presented as the child's result")
	assert.NotContains(t, text, "result: ",
		"result: is reserved for a real agent artifact")
}

// TestResearchFileIsStillLabelledResult keeps the new label from swallowing the
// case it must stay distinct from: a real research artifact the child wrote.
func TestResearchFileIsStillLabelledResult(t *testing.T) {
	t.Parallel()
	_, childID := ids(t)
	child := SessionInfo{
		ID: childID, Type: SessionTypeBuilder, Status: StatusIdle,
		ParentSessionID:  "parent",
		ResearchFilePath: "/research/x.md",
	}
	text := formatSubagentReport(child, child.ResearchFilePath)
	assert.Contains(t, text, "result: /research/x.md")
	assert.NotContains(t, text, "pane-capture")
}
