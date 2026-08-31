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

// TestResetIdleReportIgnoresADeadSession covers the two ways a recipient can
// be past taking another turn. Neither will ever start one, so there is no next
// answer for the parent to wait on and re-arming only leaves the door open for
// a late duplicate.
//
// Both halves are load-bearing, and both are reachable in production. A reaped
// window leaves only absence behind. The terminal half is the same single-tick
// race: Watch re-arms on a Running transition, and by then the monitor may have
// already marked the session Completed (manager.go:2177) without yet having
// deleted it (:2186), so isLive sees a present-but-terminal session.
func TestResetIdleReportIgnoresADeadSession(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		kill func(*fakeTarget, SessionID)
		name string
	}{
		{(*fakeTarget).forget, "gone from the registry"},
		{func(f *fakeTarget, id SessionID) {
			f.set(id, StatusStopped, RunnerTypeTmux)
		}, "marked terminal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, target, _, childID := reportFixture(t, StatusIdle)
			reportNow(c, target, childID)
			require.True(t, reportedIdle(c, childID))

			tc.kill(target, childID)
			c.resetIdleReport(childID)

			assert.True(t, reportedIdle(c, childID),
				"a session that cannot take another turn must not have its idle report re-armed")
		})
	}
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

// TestAStaleEventDoesNotUnsuppressALaterTerminalReport is the pair of events one
// monitor tick produces for a reaped child, and the case no single-event test
// can see.
//
// pollPaneIdle can emit Idle→Running for a provider that corrects a premature
// idle, and two lines later the dead pane drives Completed and deletes the
// session; both drain from the async event pump after the delete. So
// reportToParent sees a non-terminal event and then a terminal one, both for a
// child the registry no longer knows.
//
// The stale Running must be dropped — but dropping it must not also drop the
// record of the idle report the parent already received, or the Completed
// behind it passes shouldReport's "has the parent heard anything at all" gate
// and the parent is told twice about a lane it already merged (#330).
func TestAStaleEventDoesNotUnsuppressALaterTerminalReport(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)

	reportNow(c, target, childID)
	require.Len(t, c.Pending(parentID), 1, "the first idle report is the real one")

	// One tick: the window dies, and the queued Running and Completed events
	// for it are both drained afterwards.
	stale := target.mustInfo(childID)
	target.forget(childID)

	running := stale
	running.Status = StatusRunning
	c.reportToParent(context.Background(), running)

	final := stale
	final.Status = StatusCompleted
	c.reportToParent(context.Background(), final)

	assert.Len(t, c.Pending(parentID), 1,
		"a stale event must not re-open a child the parent has already heard from")
}

// TestResultLabelHasNoLabelForNoPath closes a trap for the next caller. The
// switch compares the path against the child's own fields, and a tmux child has
// neither set — so "" matches PlanFilePath and an absent artifact would be
// announced as a plan. formatSubagentReport guards on a non-empty path today,
// which is the only reason this has never shipped.
func TestResultLabelHasNoLabelForNoPath(t *testing.T) {
	t.Parallel()
	_, childID := ids(t)
	child := SessionInfo{ID: childID, Type: SessionTypeBuilder, Status: StatusIdle}
	assert.Empty(t, resultLabel(child, ""), "no path is not a plan")
}
