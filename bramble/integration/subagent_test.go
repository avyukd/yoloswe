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

	"github.com/bazelment/yoloswe/bramble/session"
)

// stubModel routes gpt-* model IDs to the scripted codex stand-in.
const stubModel = "gpt-5.5"

// stubCursorModel routes composer-* IDs to the hookless cursor stand-in.
const stubCursorModel = "composer-3"

// reportMarker is the prefix of every report bramble generates for a parent.
const reportMarker = "[bramble] subagent"

// deliveredReportMarker counts reports the recipient actually answered, not
// merely text that was pasted or echoed in its pane.
const deliveredReportMarker = "STUB-REPLY " + reportMarker

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

// TestSubagentReportsToParentOnItsOwn pins bramble-generated reports: non-Claude
// backends cannot be reliably instructed to call back through this wrapper, so
// bramble reports from the session's own state.
func TestSubagentReportsToParentOnItsOwn(t *testing.T) {
	h := newHarness(t, true)

	parent := h.spawn("builder", stubModel, "", "PARENT-BOOT")
	h.awaitStatus(parent, "idle")

	child := h.spawn("codetalk", stubModel, string(parent), "CHILD-ANSWERS-THIS")
	dumpPanesOnFailure(t, h, parent, child)

	h.awaitPane(parent, reportMarker, "parent was never told its subagent finished")

	report := h.pane(parent)
	assert.Contains(t, report, string(child), "the report should name the child")
	assert.Contains(t, report, "result:", "the report should point at the child's output")
		// Pointer, not payload: do not paste the child's transcript into the parent.
	assert.NotContains(t, report, "STUB-REPLY CHILD-ANSWERS-THIS",
		"the report should carry a path, not the child's transcript")
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

// TestQueuedDeliveryWaitsForIdle is the reason --queue exists. Typing into a
// live turn lands the text in the recipient's *next* prompt, stripped of the
// context that made it make sense.
func TestQueuedDeliveryWaitsForIdle(t *testing.T) {
	h := newHarness(t, true)

	target := h.spawn("builder", stubModel, "", "FIRST-TURN")
	h.awaitStatus(target, "idle")
	dumpPanesOnFailure(t, h, target)

		// SLOW-TURN keeps the stub busy only briefly, so this deliberately races
		// the idle transition; queued text must not be written mid-turn.
	_, err := h.send("", target, "SLOW-TURN", false)
	require.NoError(t, err)

	result, err := h.send("", target, "QUEUED-BEHIND", true)
	require.NoError(t, err)

	if result.Queued {
		assert.Equal(t, 1, h.queuedFor(target), "a queued delivery should be persisted")
		h.awaitPane(target, "STUB-REPLY QUEUED-BEHIND", "the queued message never landed")
	} else {
		h.awaitPane(target, "STUB-REPLY QUEUED-BEHIND", "an immediate delivery never landed")
	}

	require.Eventually(t, func() bool { return h.deliveryQueueLen() == 0 },
		settleTimeout, pollInterval, "the queue should drain once delivered")
}

// TestQueuedDeliveryToTerminalSessionIsRefused stops a caller from queueing a
// message nothing will ever deliver.
func TestQueuedDeliveryToTerminalSessionIsRefused(t *testing.T) {
	h := newHarness(t, true)

	target := h.spawn("builder", stubModel, "", "BOOT")
	h.awaitStatus(target, "idle")

	_, err := h.tmux("kill-window", "-t", h.tmuxTargetOf(target))
	require.NoError(t, err)
	h.awaitStatus(target, "completed", "failed", "stopped")

	_, err = h.send("", target, "TOO-LATE", true)
	require.Error(t, err, "a terminal session must refuse mail")
	assert.Equal(t, 0, h.deliveryQueueLen(), "nothing should be queued for a dead session")
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

	_, err = h.send("", target, "THROUGH-COPY-MODE", true)
	require.NoError(t, err)

	h.awaitPane(target, "STUB-REPLY THROUGH-COPY-MODE",
		"delivery to a pane in copy mode was swallowed")
}

// TestTwoWayConversationKeepsReporting pins the state transition after queued
// delivery: a child must leave "idle" when bramble types into it, or the notify
// ending that turn is dropped and the conversation goes silent.
// tmux status comes from outside the session, so bramble has to create the
// "running" transition for turns it submits itself.
func TestTwoWayConversationKeepsReporting(t *testing.T) {
	h := newHarness(t, true)

	parent := h.spawn("builder", stubModel, "", "PARENT-BOOT")
	h.awaitStatus(parent, "idle")

	child := h.spawn("codetalk", stubModel, string(parent), "ROUND-ONE")
	dumpPanesOnFailure(t, h, parent, child)

	h.awaitPane(child, "STUB-REPLY ROUND-ONE", "the child never answered round one")
	require.Eventually(t, func() bool {
		return h.countInPane(parent, deliveredReportMarker) >= 1
	}, settleTimeout, pollInterval, "no report for round one")

	// Submitting must move the child off "idle".
	_, err := h.send(parent, child, "ROUND-TWO", true)
	require.NoError(t, err)
	h.awaitPane(child, "STUB-REPLY ROUND-TWO", "the parent's reply never reached the child")

	h.awaitPaneCond(parent, func() bool {
		return h.countInPane(parent, deliveredReportMarker) >= 2
	}, "round two was never reported — the conversation went silent after one exchange")
}

// TestSubagentIsReportedOnceNotOnEveryStateChange keeps a finished subagent
// from reporting the same turn on every later state change.
//
// The completed-vs-failed terminal rules are pinned deterministically by the
// session delivery unit tests.
// Completed after an idle report is silent; failure is never silent.
func TestSubagentIsReportedOnceNotOnEveryStateChange(t *testing.T) {
	h := newHarness(t, true)

	parent := h.spawn("builder", stubModel, "", "PARENT-BOOT")
	h.awaitStatus(parent, "idle")

	child := h.spawn("codetalk", stubModel, string(parent), "ONE-AND-DONE")
	dumpPanesOnFailure(t, h, parent, child)

	h.awaitPane(parent, reportMarker, "no report at all")
	h.awaitStatus(child, "idle")

	// Give the watcher room to fire again on any later transition.
	require.Never(t, func() bool {
		return h.countInPane(parent, deliveredReportMarker) > 1
	}, 8*time.Second, pollInterval, "the parent was told more than once about one turn")
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

	// Queued delivery exercises the courier path that needs the child to go idle.
	h.awaitStatus(child, "idle")
	_, err := h.send(parent, child, "AFTER-RESTART", true)
	require.NoError(t, err)
	h.awaitPane(child, "CURSOR-STUB-REPLY AFTER-RESTART", "the message never reached the re-adopted child")

	// The re-adopted loop must see the pane go quiet again.
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
			h.awaitPaneCond(parent, func() bool {
				return h.countInPane(parent, reportMarker) >= 1
			}, "the parent was never told about its %s subagent", backend.provider)

			// The report path must be usable; "result:" alone can point at a
			// file that failed pane capture never wrote.
			resultPath, ok := reportedResultPath(h.pane(parent))
			require.Truef(t, ok, "the report carried no result path\n--- parent pane ---\n%s", h.pane(parent))
			body, err := os.ReadFile(resultPath)
			require.NoErrorf(t, err, "the reported result file is not readable: %s", resultPath)
			assert.Containsf(t, string(body), "ARTICHOKE",
				"the %s subagent's answer is missing from its result file %s", backend.provider, resultPath)

			// Delivery must move the child off idle or round two produces no
			// state change and the conversation goes quiet.
			result, err := h.send(parent, child,
				"R2: reply with exactly one line and nothing else: R2 CONFIRMED", true)
			if err != nil {
				// A modal in the recipient's pane blocks delivery, which is
				// correctly an error rather than an Enter into a menu. Answer it
				// and try once more before giving up.
				h.answerStartupDialogs(child, h.pane(child))
				result, err = h.send(parent, child,
					"R2: reply with exactly one line and nothing else: R2 CONFIRMED", true)
				require.NoErrorf(t, err, "could not deliver to the %s subagent", backend.provider)
			}
			require.False(t, result.Queued, "the child was idle, so this should have been written at once")

			h.awaitPaneClearingDialogs(child, "R2 CONFIRMED", "the subagent never answered round two")
			h.awaitPaneCond(parent, func() bool {
				return h.countInPane(parent, reportMarker) >= 2
			}, "round two was never reported for %s — the conversation went quiet after one exchange", backend.provider)
		})
	}
}

// TestLiveQueuedDeliveryWaitsForALiveTurn pins deferred delivery against real
// CLIs. A false idle, especially from cursor pane polling, would release queued
// text straight into the running turn.
// Other live assertions send to idle children; this one holds a real child
// mid-turn so the deferred path is exercised against real TUI chrome.
func TestLiveQueuedDeliveryWaitsForALiveTurn(t *testing.T) {
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
			h.awaitWorking(child, "sleep")

			result, err := h.send(parent, child, "QUEUED-MID-TURN: acknowledge with QUEUE-ACK", true)
			require.NoErrorf(t, err, "could not queue for the %s subagent", backend.provider)
			require.Truef(t, result.Queued,
				"a message sent to a %s subagent mid-turn should have been held, not written", backend.provider)
			assert.Equal(t, 1, h.queuedFor(child), "the held message should be persisted")

			// While the turn runs, the session must stay running and untouched.
			watchFor := (longTurnSeconds - 6) * int(time.Second)
			h.neverDuring(child, time.Duration(watchFor), func() bool {
				return h.status(child) == "idle" || strings.Contains(h.pane(child), "QUEUED-MID-TURN")
			}, "the %s subagent was treated as idle mid-turn, or the queued message was typed into a live turn",
				backend.provider)

			h.awaitPaneClearingDialogs(child, "LONG-DONE", "the subagent never finished its long turn")
			h.awaitPaneClearingDialogs(child, "QUEUED-MID-TURN", "the held message never landed after the turn ended")
			// Check the child's queue, not the whole spool; the live parent may
			// still be consuming its own report.
			require.Eventually(t, func() bool { return h.queuedFor(child) == 0 },
				settleTimeout, pollInterval, "the queue should drain once delivered")
		})
	}
}

// concurrentSubagents gives reports room to overlap without making the suite slow.
const concurrentSubagents = 3

// TestConcurrentSubagentsAllReport covers a fan-out: one parent, several
// subagents working at once, all reporting to the same place.
//
// Every child's completion races the others into one recipient's queue. The
// parent is deliberately left mid-turn so reports must queue, where drops are
// silent rather than crashes.
// A dropped report leaves the parent waiting for a child that already finished.
func TestConcurrentSubagentsAllReport(t *testing.T) {
	h := newHarness(t, true)

	parent := h.spawn("builder", stubModel, "", "PARENT-BOOT")
	h.awaitStatus(parent, "idle")

	_, err := h.send("", parent, "PARENT-BUSY", true)
	require.NoError(t, err)

	children := make([]session.SessionID, 0, concurrentSubagents)
	for i := 0; i < concurrentSubagents; i++ {
		child := h.spawn("codetalk", stubModel, string(parent), fmt.Sprintf("CHILD-%d-WORK", i))
		children = append(children, child)
	}
	dumpPanesOnFailure(t, h, append(children, parent)...)

	for _, child := range children {
		h.awaitStatus(child, "idle")
	}

	// Count as well as presence, because duplicated reports are also wrong.
	for _, child := range children {
		want := deliveredReportMarker + " " + string(child)
		h.awaitPaneCond(parent, func() bool { return h.countInPane(parent, want) >= 1 },
			"the parent was never told about subagent %s", child)
	}
	for _, child := range children {
		assert.Equalf(t, 1, h.countInPane(parent, deliveredReportMarker+" "+string(child)),
			"subagent %s was reported more than once", child)
	}

	require.Eventually(t, func() bool { return h.deliveryQueueLen() == 0 },
		settleTimeout, pollInterval, "every queued report should drain")
}

// TestConcurrentSubagentsQueueDurablyWhileParentIsBusy pins durable fan-out:
// reports arriving while a parent is mid-turn must live on disk, because a
// restart otherwise drops reports that only existed in memory.
//
// The parent is held busy so the queue has concurrent contents, not one report
// at a time.
func TestConcurrentSubagentsQueueDurablyWhileParentIsBusy(t *testing.T) {
	h := newHarness(t, true)

	parent := h.spawn("builder", stubModel, "", "PARENT-BOOT")
	h.awaitStatus(parent, "idle")

	_, err := h.send("", parent, "STUB-SLEEP 8", true)
	require.NoError(t, err)
	h.awaitStatus(parent, "running")

	children := make([]session.SessionID, 0, concurrentSubagents)
	for i := 0; i < concurrentSubagents; i++ {
		children = append(children, h.spawn("codetalk", stubModel, string(parent), fmt.Sprintf("CHILD-%d-WORK", i)))
	}
	dumpPanesOnFailure(t, h, append(children, parent)...)
	for _, child := range children {
		h.awaitStatus(child, "idle")
	}

	// While the parent is busy, every report must be on disk.
	queued := h.queuedTextFor(parent)
	require.NotEmptyf(t, queued, "no report was queued while the parent was busy\n--- parent pane ---\n%s", h.pane(parent))
	for _, child := range children {
		assert.Containsf(t, queued, string(child),
			"the persisted queue is missing %s; a restart here would drop it", child)
	}

	for _, child := range children {
		want := deliveredReportMarker + " " + string(child)
		h.awaitPaneCond(parent, func() bool { return h.countInPane(parent, want) >= 1 },
			"subagent %s was queued but never delivered", child)
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

	h.awaitPane(parent, reportMarker, "a subagent on its own worktree never reported to its parent")
	_, err := h.send(parent, child, "CROSS-TREE-FOLLOWUP", true)
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

	_, err := h.send("", target, "CURSOR-DELIVERY", true)
	require.NoError(t, err)

	h.awaitPane(target, "CURSOR-STUB-REPLY CURSOR-DELIVERY",
		"the delivery was never submitted to the cursor session")

	// A re-paste would drive a second turn.
	assert.Equal(t, 1, h.countInPane(target, "CURSOR-STUB-REPLY CURSOR-DELIVERY"),
		"the message must be delivered once, not re-pasted and run twice")

	require.Eventually(t, func() bool { return h.deliveryQueueLen() == 0 },
		settleTimeout, pollInterval, "a delivered message must leave the queue")
}

// TestDeliveryIntoADraftIsHeldOnlyWhereTheComposerCanBeRead documents a
// deliberate limit: delivery is held only when the composer has a validated
// prompt glyph that makes drafts distinguishable from startup placeholders.
//
// Codex and cursor are not protected today. Guessing on unreadable composers
// would hold mail forever on panes bramble failed to parse, so this test pins
// the known unprotected outcome until a live-capture-backed draft check exists.
// Claude is protected separately by tests that validate its prompt glyph against
// captured pane bytes.
func TestDeliveryIntoADraftIsHeldOnlyWhereTheComposerCanBeRead(t *testing.T) {
	h := newHarness(t, true)

	target := h.spawn("builder", stubModel, "", "BOOT")
	h.awaitStatus(target, "idle")
	h.awaitPane(target, "STUB-REPLY BOOT", "the agent never answered its opening prompt")
	dumpPanesOnFailure(t, h, target)

	tmuxTarget := h.tmuxTargetOf(target)
	_, err := h.tmux("send-keys", "-t", tmuxTarget, "HALF-TYPED-THOUGHT")
	require.NoError(t, err)
	h.awaitPane(target, "HALF-TYPED-THOUGHT", "the draft never appeared in the composer")

	_, err = h.send("", target, "ARRIVES-MID-SENTENCE", true)
	require.NoError(t, err)

	// Assert the exact unprotected concatenation so a future codex draft check
	// breaks this test instead of passing against the old behavior.
	h.awaitPane(target, "STUB-REPLY HALF-TYPED-THOUGHTARRIVES-MID-SENTENCE",
		"expected the known-unprotected path: a codex-shaped composer cannot be read for a draft")

	require.Eventually(t, func() bool { return h.deliveryQueueLen() == 0 },
		settleTimeout, pollInterval, "the delivery still leaves the queue")
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
	_, err := h.send("", parent, "AFTER-RESTART", true)
	require.NoError(t, err)
	h.awaitPane(parent, "STUB-REPLY AFTER-RESTART", "a pre-restart session was unreachable afterwards")

	require.Eventually(t, func() bool { return h.deliveryQueueLen() == 0 },
		settleTimeout, pollInterval, "the queue must drain, which needs the session to be seen going idle")
}
