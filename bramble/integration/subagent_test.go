//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/ipc"
	"github.com/bazelment/yoloswe/bramble/session"
)

// stubModel routes gpt-* model IDs to the scripted codex stand-in.
const stubModel = "gpt-5.5"

// stubCursorModel routes composer-* IDs to the hookless cursor stand-in.
const stubCursorModel = "composer-3"

// nudgeMarker is the whole hint bramble now writes into a parent's pane. It
// names no child and carries no result on purpose — the run directory and git
// are the record, so a hint cannot go stale and a duplicate is harmless.
const nudgeMarker = "[bramble] subagent activity"

// answeredNudgeMarker counts hints the recipient actually answered, not merely
// text that was pasted or echoed in its pane.
const answeredNudgeMarker = "STUB-REPLY " + nudgeMarker

// TestSubagentLineageIsRecorded pins the link a subagent's whole return path
// hangs on: without a recorded parent nothing knows where to report.
func TestSubagentLineageIsRecorded(t *testing.T) {
	h := newHarness(t, true)

	parent := h.spawn("builder", stubModel, "", "PARENT-BOOT")
	h.awaitStatus(parent, "idle")

	child := h.spawn("codetalk", stubModel, string(parent), "CHILD-BOOT")
	dumpPanesOnFailure(t, h, parent, child)

	var found bool
	for _, s := range h.sessions() {
		if s.ID != string(child) {
			continue
		}
		found = true
		assert.Equal(t, string(parent), s.ParentSessionID)
		assert.Equal(t, stubModel, s.Model)
	}
	require.True(t, found, "child session missing from list-sessions")

	// A subagent with no branch of its own works on its parent's tree.
	for _, s := range h.sessions() {
		if s.ID == string(child) {
			assert.Equal(t, "main", s.WorktreeName)
		}
	}
}

// TestTopLevelSessionHasNoParent guards the other direction: an ordinary
// session must not acquire a parent it never asked for and start mailing it.
func TestTopLevelSessionHasNoParent(t *testing.T) {
	h := newHarness(t, true)

	solo := h.spawn("builder", stubModel, "", "SOLO-BOOT")
	h.awaitStatus(solo, "idle")

	for _, s := range h.sessions() {
		if s.ID == string(solo) {
			assert.Empty(t, s.ParentSessionID)
		}
	}
}

// TestSubagentNudgesItsParentWithoutState pins what a parent actually receives:
// a hint that something happened, carrying nothing that could later be wrong.
//
// The old report named the child, its status and a result path. That made a
// replayed report indistinguishable from a real "lane died holding uncommitted
// work" (issue #330), and pointed at a 2000-line pane dump that opened with the
// CLI's splash screen (issue #331). The run directory and git hold the truth.
func TestSubagentNudgesItsParentWithoutState(t *testing.T) {
	h := newHarness(t, true)

	parent := h.spawn("builder", stubModel, "", "PARENT-BOOT")
	h.awaitStatus(parent, "idle")

	child := h.spawn("codetalk", stubModel, string(parent), "CHILD-ANSWERS-THIS")
	dumpPanesOnFailure(t, h, parent, child)

	h.awaitPane(parent, nudgeMarker, "parent was never hinted that its subagent finished")

	hint := h.pane(parent)
	assert.NotContains(t, hint, string(child), "a hint names no session")
	assert.NotContains(t, hint, "result:", "a hint points at no file")
	assert.NotContains(t, hint, "STUB-REPLY CHILD-ANSWERS-THIS",
		"a hint never carries the child's transcript")
}

// TestSubagentNotifyHookMarksItIdle covers the hook that moves codex sessions
// off "running" so queues drain and parents hear before window exit.
func TestSubagentNotifyHookMarksItIdle(t *testing.T) {
	h := newHarness(t, true)

	child := h.spawn("codetalk", stubModel, "", "ANSWER-ME")
	dumpPanesOnFailure(t, h, child)

	h.awaitPane(child, "STUB-REPLY ANSWER-ME", "the agent never answered")
	h.awaitStatus(child, "idle")
}

// TestQueuedSendIsRefused pins the removal of --queue end to end.
//
// Refused rather than downgraded to an immediate paste: a caller that asked to
// wait for an idle recipient must not silently get a mid-turn interrupt. The
// swarm skill's own examples now use a plain send for exactly this reason.
func TestQueuedSendIsRefused(t *testing.T) {
	h := newHarness(t, true)

	target := h.spawn("builder", stubModel, "", "FIRST-TURN")
	h.awaitStatus(target, "idle")
	dumpPanesOnFailure(t, h, target)

	_, err := h.send("", target, "QUEUE-ME", true)
	require.Error(t, err, "--queue must be refused, not honoured")
	assert.Contains(t, err.Error(), "removed")

	assert.Equal(t, 0, h.deliveryQueueLen(), "a refused send writes nothing to disk")
	h.neverDuring(target, 3*time.Second, func() bool {
		return strings.Contains(h.pane(target), "QUEUE-ME")
	}, "a refused send must not fall through to typing into the pane")
}

// TestSendToTerminalSessionIsRefused stops a caller from addressing a session
// nothing will ever read.
func TestSendToTerminalSessionIsRefused(t *testing.T) {
	h := newHarness(t, true)

	target := h.spawn("builder", stubModel, "", "BOOT")
	h.awaitStatus(target, "idle")

	_, err := h.tmux("kill-window", "-t", h.tmuxTargetOf(target))
	require.NoError(t, err)
	h.awaitStatus(target, "completed", "failed", "stopped")

	_, err = h.send("", target, "TOO-LATE", false)
	require.Error(t, err, "a terminal session has no pane to write to")
	assert.Equal(t, 0, h.deliveryQueueLen(), "nothing is ever queued for a dead session")
}

// TestDeliveryReachesPaneInCopyMode pins a silent tmux failure: copy mode
// consumes Enter while tmux reports success, so bramble must leave copy mode
// before writing.
// Otherwise the message can sit in the composer forever without bramble seeing
// an error.
func TestDeliveryReachesPaneInCopyMode(t *testing.T) {
	h := newHarness(t, true)

	target := h.spawn("builder", stubModel, "", "BOOT")
	h.awaitStatus(target, "idle")
	h.awaitPane(target, "STUB-REPLY BOOT", "the agent never answered its opening prompt")
	dumpPanesOnFailure(t, h, target)

	tmuxTarget := h.tmuxTargetOf(target)
	_, err := h.tmux("copy-mode", "-t", tmuxTarget)
	require.NoError(t, err)
	inMode, err := h.tmux("display-message", "-p", "-t", tmuxTarget, "#{pane_in_mode}")
	require.NoError(t, err)
	require.Equal(t, "1", strings.TrimSpace(inMode), "pane should be in copy mode")

	_, err = h.send("", target, "THROUGH-COPY-MODE", false)
	require.NoError(t, err)

	h.awaitPane(target, "STUB-REPLY THROUGH-COPY-MODE",
		"delivery to a pane in copy mode was swallowed")
}

// TestTwoWayConversationKeepsHinting pins the state transition after a send: a
// child must leave "idle" when bramble types into it, or the notify ending that
// turn is dropped and the conversation goes silent.
// tmux status comes from outside the session, so bramble has to create the
// "running" transition for turns it submits itself.
func TestTwoWayConversationKeepsHinting(t *testing.T) {
	h := newHarness(t, true)

	parent := h.spawn("builder", stubModel, "", "PARENT-BOOT")
	h.awaitStatus(parent, "idle")

	child := h.spawn("codetalk", stubModel, string(parent), "ROUND-ONE")
	dumpPanesOnFailure(t, h, parent, child)

	h.awaitPane(child, "STUB-REPLY ROUND-ONE", "the child never answered round one")
	h.awaitPaneCond(parent, func() bool {
		return h.countInPane(parent, answeredNudgeMarker) >= 1
	}, "no hint for round one")

	// Submitting must move the child off "idle", or its next turn ends with a
	// notify that lands on a session already marked idle and is dropped.
	_, err := h.send(parent, child, "ROUND-TWO", false)
	require.NoError(t, err)
	h.awaitPane(child, "STUB-REPLY ROUND-TWO", "the parent's reply never reached the child")

	h.awaitPaneCond(parent, func() bool {
		return h.countInPane(parent, answeredNudgeMarker) >= 2
	}, "round two was never hinted — the conversation went silent after one exchange")
}

// TestFinishedSubagentDoesNotHintForever keeps a finished subagent from hinting
// again on every later state change — the shape of issue #330, where reaped
// children replayed for hours.
//
// A hint carries no state, so a stray extra one is harmless rather than
// ambiguous; this bounds it anyway, because a pane full of hints is still noise.
func TestFinishedSubagentDoesNotHintForever(t *testing.T) {
	h := newHarness(t, true)

	parent := h.spawn("builder", stubModel, "", "PARENT-BOOT")
	h.awaitStatus(parent, "idle")

	child := h.spawn("codetalk", stubModel, string(parent), "ONE-AND-DONE")
	dumpPanesOnFailure(t, h, parent, child)

	h.awaitPane(parent, nudgeMarker, "no hint at all")
	h.awaitStatus(child, "idle")

	// Reap it the way the swarm skill documents, then give the watcher room to
	// fire on the terminal transition that follows.
	_, err := h.tmux("kill-window", "-t", h.tmuxTargetOf(child))
	require.NoError(t, err)

	// Answered hints, not raw occurrences — see the note in
	// TestConcurrentSubagentsCoalesceIntoOneNudge. The reaped child may produce
	// one more as it goes terminal; what must not happen is the unbounded
	// replay of issue #330.
	before := h.countInPane(parent, answeredNudgeMarker)
	h.neverDuring(parent, 8*time.Second, func() bool {
		return h.countInPane(parent, answeredNudgeMarker) > before+1
	}, "a reaped subagent kept hinting after it was gone")
	assert.Equal(t, 0, h.deliveryQueueLen(), "and nothing was left on disk to replay")
}

// TestReadoptedCursorSubagentIsStillSeenToFinish pins the restart monitor loop.
// Cursor has no completion hook, so a re-adopted cursor subagent must be polled
// from its pane or it stays "running" forever and drains no queued mail.
//
// The second turn is the proof because only the re-adopted loop can observe it.
func TestReadoptedCursorSubagentIsStillSeenToFinish(t *testing.T) {
	h := newHarness(t, true)

	parent := h.spawn("builder", stubModel, "", "PARENT-BOOT")
	h.awaitStatus(parent, "idle")

	child := h.spawn("codetalk", stubCursorModel, string(parent), "BEFORE-RESTART")
	dumpPanesOnFailure(t, h, parent, child)

	h.awaitPane(child, "CURSOR-STUB-REPLY BEFORE-RESTART", "the cursor stand-in never answered")
	h.awaitStatus(child, "idle")

	h.restart()

	h.awaitStatus(child, "idle")
	_, err := h.send(parent, child, "AFTER-RESTART", false)
	require.NoError(t, err)
	h.awaitPane(child, "CURSOR-STUB-REPLY AFTER-RESTART", "the message never reached the re-adopted child")

	// A submitted send marks the recipient running, so this is a real
	// running→idle transition rather than a status that never moved. Only the
	// re-adopted loop's pane polling can produce it for cursor, which has no
	// completion hook.
	h.awaitStatus(child, "running")
	h.awaitPaneCond(child, func() bool {
		return h.status(child) == "idle"
	}, "the re-adopted cursor session was never seen to finish its turn — nothing polls its pane")
}

// --- live backends -----------------------------------------------------------

// TestLiveSubagentTwoWay drives real agent CLIs because hook delivery, pane
// idleness, paste timing, and TUI copy-mode failures are only visible in a real
// pane. It skips per backend only when that backend is unavailable.
func TestLiveSubagentTwoWay(t *testing.T) {
	for _, backend := range liveBackends {
		t.Run(backend.provider, func(t *testing.T) {
			model := backend.require(t)
			h := newHarness(t, false)

			parentModel := liveBackends[0].require(t)
			parent := h.spawn("builder", parentModel, "",
				"You are the PARENT in an automated test. Say exactly: PARENT READY. Then wait. Do not read or edit files.")
			h.awaitReady(parent)

			child := h.spawn("codetalk", model, string(parent),
				"Reply with exactly one line and nothing else: R1 ARTICHOKE. Do not read files. Do not run commands.")
			dumpPanesOnFailure(t, h, parent, child)

			// Reaching idle is backend-specific: hooks for claude/codex, pane
			// polling for cursor.
			h.awaitPaneClearingDialogs(child, "ARTICHOKE", "the subagent never answered round one")
			h.awaitStatus(child, "idle")
			// Deliberately NOT asserted here: that a hint reached the parent, or
			// that bramble wrote a result file.
			//
			// The hint is dropped whenever the parent is not idle at that
			// instant, and a live child often finishes its first turn while the
			// parent is still settling. Requiring it would be requiring the
			// guarantee this branch removed.
			//
			// And a tmux-mode child has no result file: writeResearchFile runs
			// in the TUI turn loop, which tmux sessions never enter. The old
			// courier papered over that by capturing 2000 lines of pane, which
			// is the artifact issue #331 objects to — it opened with the CLI
			// splash screen and the prompt echoed back. A tmux lane reports by
			// writing the literal path its brief names, which is what
			// subagent-swarm briefs every lane to do.
			//
			// What must hold is that the child's turn is observable at all,
			// which awaitStatus above just proved: that is what a polling
			// orchestrator reads.

			// A write must move the child off idle, or round two produces no
			// state change and the conversation goes quiet.
			_, err := h.send(parent, child,
				"R2: reply with exactly one line and nothing else: R2 CONFIRMED", false)
			if err != nil {
				// A modal in the recipient's pane blocks the write, which is
				// correctly an error rather than an Enter into a menu. Answer it
				// and try once more before giving up.
				h.answerStartupDialogs(child, h.pane(child))
				_, err = h.send(parent, child,
					"R2: reply with exactly one line and nothing else: R2 CONFIRMED", false)
				require.NoErrorf(t, err, "could not write to the %s subagent", backend.provider)
			}

			h.awaitPaneClearingDialogs(child, "R2 CONFIRMED", "the subagent never answered round two")

			// Round two must also be observable. This is the real two-way bug:
			// a child that never leaves idle ends its second turn with a notify
			// that lands on a session already marked idle, so the turn goes
			// unrecorded and a polling parent sees nothing new.
			h.awaitStatus(child, "idle")
		})
	}
}

// TestLiveBusyChildIsNeverWrittenInto is the live counterpart to dropping
// deferred delivery. Nothing is held for a busy session any more, so the
// property that matters is the one that protects a running turn: bramble must
// never treat it as idle or accumulate anything behind it.
//
// Other live assertions send to idle children; this one holds a real child
// mid-turn so the rule is exercised against real TUI chrome, where a false idle
// — especially from cursor pane polling — would corrupt the turn.
func TestLiveBusyChildIsNeverWrittenInto(t *testing.T) {
	for _, backend := range liveBackends {
		t.Run(backend.provider, func(t *testing.T) {
			model := backend.require(t)
			h := newHarness(t, false)

			parent := h.spawn("builder", liveBackends[0].require(t), "",
				"You are the PARENT in an automated test. Say exactly: PARENT READY. Then wait.")
			h.awaitReady(parent)

			// Builder can run the sleep command that keeps the child mid-turn.
			child := h.spawn("builder", model, string(parent), longTurnPrompt("LONG-DONE"))
			dumpPanesOnFailure(t, h, parent, child)
			if !h.reachedWorking(child, "sleep", 60*time.Second) {
				// The setup did not happen, so there is no live turn to protect
				// and the assertions below would pass without proving anything.
				t.Skipf("the %s child never started its long turn; nothing to hold mail against",
					backend.provider)
			}

			watchFor := (longTurnSeconds - 6) * int(time.Second)
			h.neverDuring(child, time.Duration(watchFor), func() bool {
				return h.status(child) == "idle" || h.deliveryQueueLen() > 0
			}, "the %s subagent was treated as idle mid-turn, or something was queued for it",
				backend.provider)

			h.awaitPaneClearingDialogs(child, "LONG-DONE", "the subagent never finished its long turn")

			// Once it is genuinely idle, a write lands — proving the rule above
			// was about the live turn, not a permanently unreachable pane.
			h.awaitStatus(child, "idle")
			_, err := h.send(parent, child, "AFTER-TURN: acknowledge with TURN-ACK", false)
			require.NoErrorf(t, err, "could not write to the idle %s subagent", backend.provider)
			h.awaitPaneClearingDialogs(child, "AFTER-TURN", "the message never landed after the turn ended")
			assert.Equal(t, 0, h.deliveryQueueLen(), "nothing is ever persisted")
		})
	}
}

// concurrentSubagents gives finishes room to overlap without making the suite slow.
const concurrentSubagents = 3

// TestConcurrentSubagentsCoalesceIntoOneNudge covers a fan-out the way the
// swarm skill actually runs it: one parent, several lanes finishing at once.
//
// The old queue delivered one report per child, in order, one per idle
// transition — which is how a parent ended up holding 25 reports spanning 4.5
// hours. A hint says only "something happened", so a wave is bounded, and the
// parent then polls the run directory for what actually changed.
func TestConcurrentSubagentsCoalesceIntoOneNudge(t *testing.T) {
	h := newHarness(t, true)

	parent := h.spawn("builder", stubModel, "", "PARENT-BOOT")
	h.awaitStatus(parent, "idle")

	children := make([]session.SessionID, 0, concurrentSubagents)
	for i := 0; i < concurrentSubagents; i++ {
		child := h.spawn("codetalk", stubModel, string(parent), fmt.Sprintf("CHILD-%d-WORK", i))
		children = append(children, child)
	}
	dumpPanesOnFailure(t, h, append(children, parent)...)

	for _, child := range children {
		h.awaitStatus(child, "idle")
	}

	h.awaitPaneCond(parent, func() bool { return h.countInPane(parent, answeredNudgeMarker) >= 1 },
		"a fan-out of %d subagents produced no hint at all", len(children))

	// Count answered hints, not raw occurrences: a delivered hint appears three
	// times in the pane (typed, echoed by the CLI, then quoted in the reply), so
	// matching the bare marker would treat one delivery as three.
	//
	// Coalescing bounds this below one per child. It cannot be exactly one: each
	// hint starts a turn, and the parent going idle again is itself a state
	// change the next finishing child can ride.
	h.neverDuring(parent, 8*time.Second, func() bool {
		return h.countInPane(parent, answeredNudgeMarker) > concurrentSubagents
	}, "the parent's pane was flooded: more hints than subagents")

	assert.Equal(t, 0, h.deliveryQueueLen(), "a hint is never written to disk")
}

// TestABusyParentAccumulatesNothing is the failure that wedged three real
// sessions for hours: the courier held mail for a parent it could not safely
// write to, retried every 30s, and never delivered.
//
// A hint has no such obligation. If the parent is mid-turn there is nothing to
// hold, because the run directory already has the result.
func TestABusyParentAccumulatesNothing(t *testing.T) {
	h := newHarness(t, true)

	parent := h.spawn("builder", stubModel, "", "PARENT-BOOT")
	h.awaitStatus(parent, "idle")

	// STUB-SLEEP holds the parent inside a turn. A submitted send marks the
	// recipient running, so the parent is genuinely busy for the assertions
	// below rather than merely looking busy in the pane. What matters here is
	// that nothing accumulates while it is.
	_, err := h.send("", parent, "STUB-SLEEP 8", false)
	require.NoError(t, err)

	children := make([]session.SessionID, 0, concurrentSubagents)
	for i := 0; i < concurrentSubagents; i++ {
		children = append(children, h.spawn("codetalk", stubModel, string(parent), fmt.Sprintf("CHILD-%d-WORK", i)))
	}
	dumpPanesOnFailure(t, h, append(children, parent)...)
	for _, child := range children {
		h.awaitStatus(child, "idle")
	}

	// This is the property whose absence produced the 25-message queue.
	assert.Equal(t, 0, h.deliveryQueueLen(),
		"a busy parent must accumulate no queue: the run directory is the record")

	// And the children stay observable as finished, which is what the
	// orchestrator's poll actually reads — the delivery path that replaces the
	// queue.
	for _, child := range children {
		assert.Equal(t, "idle", h.status(child),
			"the child must be observable as finished through list-sessions")
	}
}

// TestSubagentOnItsOwnWorktreeIsIsolated pins branch worktree isolation and the
// return path across it. Isolation must be asserted against git, not only the
// path bramble reports.
func TestSubagentOnItsOwnWorktreeIsIsolated(t *testing.T) {
	h := newHarness(t, true)

	parent := h.spawn("builder", stubModel, "", "PARENT-BOOT")
	h.awaitStatus(parent, "idle")

	const branch = "sub/isolated"
	child, worktree := h.spawnOnNewWorktree("builder", stubModel, string(parent), branch, "main", "CHILD-ON-OWN-TREE")
	dumpPanesOnFailure(t, h, parent, child)

	assert.NotEqualf(t, h.worktreePath, worktree,
		"the subagent was put on its parent's tree instead of a new one")
	require.DirExists(t, worktree)

	// Ask git; path existence does not prove the branch or base.
	assert.Equal(t, branch, h.gitIn(worktree, "branch", "--show-current"),
		"the new worktree is not on the requested branch")
	assert.Equal(t,
		h.gitIn(h.worktreePath, "rev-parse", "main"),
		h.gitIn(worktree, "rev-parse", "HEAD"),
		"the new branch is not based on main")

	require.NoError(t, os.WriteFile(filepath.Join(worktree, "child-only.txt"), []byte("x\n"), 0o644))
	assert.NoFileExists(t, filepath.Join(h.worktreePath, "child-only.txt"),
		"a file written on the subagent's tree showed up on its parent's")

	var found bool
	for _, s := range h.sessions() {
		if s.ID != string(child) {
			continue
		}
		found = true
		assert.Equal(t, string(parent), s.ParentSessionID)
		assert.Equal(t, filepath.Base(branch), s.WorktreeName)
	}
	require.True(t, found, "the subagent is missing from list-sessions")

	h.awaitPane(parent, nudgeMarker, "a subagent on its own worktree never hinted to its parent")
	_, err := h.send(parent, child, "CROSS-TREE-FOLLOWUP", false)
	require.NoError(t, err)
	h.awaitPane(child, "STUB-REPLY CROSS-TREE-FOLLOWUP",
		"a message to a subagent on another worktree never landed")
}

// TestSubagentWorktreeIsReusedNotDuplicated: a parent that respawns a subagent
// on the same branch — after a crash, or for a second attempt — should land on
// the existing tree rather than failing.
func TestSubagentWorktreeIsReusedNotDuplicated(t *testing.T) {
	h := newHarness(t, true)

	parent := h.spawn("builder", stubModel, "", "PARENT-BOOT")
	h.awaitStatus(parent, "idle")

	const branch = "sub/reused"
	first, firstPath := h.spawnOnNewWorktree("builder", stubModel, string(parent), branch, "main", "FIRST-ATTEMPT")
	h.awaitStatus(first, "idle")

	second, secondPath := h.spawnOnNewWorktree("builder", stubModel, string(parent), branch, "main", "SECOND-ATTEMPT")
	dumpPanesOnFailure(t, h, parent, first, second)

	assert.Equal(t, firstPath, secondPath,
		"respawning on the same branch should reuse the worktree, not make another")
	assert.NotEqual(t, first, second, "each spawn is still its own session")
	h.awaitPane(second, "STUB-REPLY SECOND-ATTEMPT", "the second subagent never ran")
}

// TestCursorDeliveryIsPastedOnceAndSubmitted covers the hookless backend end to
// end: one paste, submitted, queue drained.
//
// A shell stand-in cannot reproduce cursor-agent's paste chip, because the
// terminal echoes the delivered text. The chip and unverifiable-paste branches
// are pinned in bramble/session; this tmux test pins exactly one paste and one
// Enter reaching a cursor-shaped session.
// A decorative chip in the stand-in would not prove provenance; it looks the
// same no matter who printed it.
func TestCursorDeliveryIsPastedOnceAndSubmitted(t *testing.T) {
	h := newHarness(t, true)

	target := h.spawn("builder", stubCursorModel, "", "BOOT")
	h.awaitStatus(target, "idle")
	dumpPanesOnFailure(t, h, target)

	_, err := h.send("", target, "CURSOR-DELIVERY", false)
	require.NoError(t, err)

	h.awaitPane(target, "CURSOR-STUB-REPLY CURSOR-DELIVERY",
		"the message was never submitted to the cursor session")

	// cursor collapses a bracketed paste to a "[Pasted text #N]" chip rather
	// than echoing it, which the old verifier read as "not landed" and re-pasted
	// every 30s forever. Nothing verifies a paste any more, so this pins the
	// outcome that regression produced: one paste, one turn.
	assert.Equal(t, 1, h.countInPane(target, "CURSOR-STUB-REPLY CURSOR-DELIVERY"),
		"the message must be written once, not re-pasted and run twice")

	assert.Equal(t, 0, h.deliveryQueueLen(), "nothing is ever persisted")
}

// TestExplicitSendIntoADraftIsAnInterrupt documents a deliberate limit, and the
// boundary between the two write paths.
//
// An explicit send is an interrupt: it overrides the composer on purpose, which
// is what makes it the right tool for a deliberate nudge. Only bramble's own
// hint yields to a draft, and only where the composer can be read — claude's
// prompt glyph is validated against captured pane bytes, while codex and cursor
// render placeholder text that vanishes when a user types, making a draft
// indistinguishable from a booting CLI.
//
// This pins the codex-shaped outcome so a future draft check for those backends
// breaks this test rather than passing against the old behaviour.
func TestExplicitSendIntoADraftIsAnInterrupt(t *testing.T) {
	h := newHarness(t, true)

	target := h.spawn("builder", stubModel, "", "BOOT")
	h.awaitStatus(target, "idle")
	h.awaitPane(target, "STUB-REPLY BOOT", "the agent never answered its opening prompt")
	dumpPanesOnFailure(t, h, target)

	tmuxTarget := h.tmuxTargetOf(target)
	_, err := h.tmux("send-keys", "-t", tmuxTarget, "HALF-TYPED-THOUGHT")
	require.NoError(t, err)
	h.awaitPane(target, "HALF-TYPED-THOUGHT", "the draft never appeared in the composer")

	_, err = h.send("", target, "ARRIVES-MID-SENTENCE", false)
	require.NoError(t, err)

	// The exact concatenation an interrupt produces on a composer that cannot
	// be read for a draft.
	h.awaitPane(target, "STUB-REPLY HALF-TYPED-THOUGHTARRIVES-MID-SENTENCE",
		"expected the known-unprotected path: a codex-shaped composer cannot be read for a draft")

	assert.Equal(t, 0, h.deliveryQueueLen(), "an interrupt is never queued")
}

// TestSessionsStillReachBrambleAfterARestart pins the stranded-parent bug:
// session callback addresses are frozen into tmux windows and agent hooks at
// startup, so a pid-scoped socket leaves pre-restart windows reporting to a dead
// address and stuck "running".
// The hook used --silent, so the failure was swallowed; Drain then refused the
// parent's mail because the child never returned to idle.
func TestSessionsStillReachBrambleAfterARestart(t *testing.T) {
	h := newHarness(t, true)

	parent := h.spawn("builder", stubModel, "", "PARENT-BOOT")
	h.awaitStatus(parent, "idle")
	dumpPanesOnFailure(t, h, parent)

	sockBefore := h.ipcSock

	h.restart()

	assert.Equal(t, sockBefore, h.ipcSock,
		"the restarted bramble must bind the same path its live windows still point at")

	// A pre-restart session can update status only if its frozen socket resolves.
	h.awaitStatus(parent, "idle")
	_, err := h.send("", parent, "AFTER-RESTART", false)
	require.NoError(t, err)
	h.awaitPane(parent, "STUB-REPLY AFTER-RESTART", "a pre-restart session was unreachable afterwards")

	// A plain check, not Eventually: nothing writes to the retired spool any
	// more, so there is no drain to wait for. It stays as a regression fence
	// against reintroducing persistence.
	require.Zero(t, h.deliveryQueueLen(), "the retired spool must stay unused")
}

// TestSwarmLaneProtocol walks the exact contract the subagent-swarm skill
// depends on, in the order the skill performs it. Each step is something the
// skill's own scripts read, and each has broken in production at least once.
//
// The skill polls; it does not wait to be told. So the assertions here are about
// what a poller can observe — list-sessions, the worktree, the run directory —
// never about a message arriving. A hint may or may not land, and the protocol
// must close either way.
func TestSwarmLaneProtocol(t *testing.T) {
	h := newHarness(t, true)

	// 1. Preflight: `bramble list-sessions | rg "$SELF"`. The skill fails the
	//    run here if the orchestrator cannot see itself.
	orchestrator := h.spawn("planner", stubModel, "", "ORCHESTRATOR-BOOT")
	h.awaitStatus(orchestrator, "idle")
	require.Contains(t, sessionIDs(h.sessions()), orchestrator,
		"the orchestrator cannot find itself in list-sessions; the skill's preflight fails here")

	// The run directory the skill creates and treats as the record.
	run := filepath.Join(t.TempDir(), "subagent-swarm", "run-1")
	require.NoError(t, os.MkdirAll(run, 0o755))

	// 2. Spawn a lane on its own branch worktree with --parent, as
	//    `new-session -r ... --create-worktree -b "$BRANCH" --parent "$SELF"`.
	lane, worktree := h.spawnOnNewWorktree("builder", stubModel, string(orchestrator),
		"swarm-lane-1", "main", "LANE-WORK")
	dumpPanesOnFailure(t, h, orchestrator, lane)
	h.awaitStatus(lane, "idle")

	// 3. Lineage must be visible to the poller, or the skill cannot tell its own
	//    lanes from unrelated sessions: `list-sessions --parent "$SELF"`.
	var summary ipc.SessionSummary
	for _, s := range h.sessions() {
		if session.SessionID(s.ID) == lane {
			summary = s
		}
	}
	require.Equal(t, string(orchestrator), summary.ParentSessionID,
		"the lane is orphaned; the skill's --parent contract is broken")

	// 4. A stable window id, which watch_lanes.sh's liveness check reads. Empty
	//    means the watcher can never notice a dead lane.
	require.NotEmpty(t, summary.TmuxTarget,
		"no tmux target for the lane; watch_lanes.sh cannot check whether it is alive")

	// 5. The lane commits, then writes .done last — the ordering pr-lane.md
	//    requires so a .done is never seen ahead of the work it claims.
	h.gitIn(worktree, "commit", "--allow-empty", "-m", "lane work")
	require.NoError(t, os.WriteFile(filepath.Join(run, "lane-1.swe.done"), []byte("done\n"), 0o644))

	// 6. The orchestrator verifies the claim against git rather than believing
	//    it — what watch_lanes.sh prints as commits=N, and why an EMPTY BRANCH
	//    is refused rather than merged.
	require.NotEmpty(t, strings.TrimSpace(h.gitIn(worktree, "log", "--oneline", "main..HEAD")),
		"the .done claims work git cannot confirm; this is the EMPTY BRANCH case")

	// 7. Reap with kill-window, which the skill documents because a wedged
	//    session cannot process /exit.
	_, err := h.tmux("kill-window", "-t", h.tmuxTargetOf(lane))
	require.NoError(t, err)

	// 8. Nothing may accumulate for the orchestrator afterwards. This is issue
	//    #330 exactly: reaped lanes replayed for hours because their
	//    undeliverable reports stayed on disk.
	h.neverDuring(orchestrator, 6*time.Second, func() bool {
		return h.deliveryQueueLen() > 0
	}, "a reaped lane left queued state behind; this is what replayed for hours")

	// 9. And the orchestrator is still healthy enough to run the next tick.
	assert.Equal(t, "idle", h.status(orchestrator))
}

// sessionIDs is the projection the skill's preflight greps for.
func sessionIDs(summaries []ipc.SessionSummary) []session.SessionID {
	out := make([]session.SessionID, 0, len(summaries))
	for _, s := range summaries {
		out = append(out, session.SessionID(s.ID))
	}
	return out
}
