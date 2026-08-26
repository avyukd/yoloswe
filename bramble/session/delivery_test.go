package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTarget is a DeliveryTarget backed by a map, so courier behaviour can be
// driven through every status and runner type without live managers or tmux.
type fakeTarget struct { //nolint:govet // fieldalignment: readability over packing
	mu         sync.Mutex
	sessions   map[SessionID]SessionInfo
	followUps  []string
	tmuxTarget string
	followErr  error
	captured   map[SessionID][]string
	captureErr error
	// captureDelay stands in for how long a real pane capture takes. It is what
	// makes the courier's event handling slow enough to test what happens to the
	// events arriving behind it.
	captureDelay  time.Duration
	captureCount  int
	markedRunning []SessionID
}

func newFakeTarget() *fakeTarget {
	return &fakeTarget{
		sessions:   make(map[SessionID]SessionInfo),
		captured:   make(map[SessionID][]string),
		tmuxTarget: "@7",
	}
}

func (f *fakeTarget) set(id SessionID, status SessionStatus, runner string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prev := f.sessions[id]
	prev.ID = id
	prev.Status = status
	prev.RunnerType = runner
	f.sessions[id] = prev
}

// setChild registers a session as a subagent of parent, keeping whatever
// result paths a test has already attached.
func (f *fakeTarget) setChild(id, parent SessionID, status SessionStatus, runner string) {
	f.set(id, status, runner)
	f.mu.Lock()
	defer f.mu.Unlock()
	info := f.sessions[id]
	info.ParentSessionID = parent
	f.sessions[id] = info
}

// annotate attaches result metadata to an existing session.
func (f *fakeTarget) annotate(id SessionID, fn func(*SessionInfo)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info := f.sessions[id]
	fn(&info)
	f.sessions[id] = info
}

// setBackend records which agent CLI backs a session. Paste verification is
// provider-specific — only a backend known to drop pastes is re-pasted — so a
// test that exercises the strict path has to say which one it means.
func (f *fakeTarget) setBackend(id SessionID, backend, model string) {
	f.annotate(id, func(i *SessionInfo) {
		i.Backend = backend
		i.Model = model
	})
}

func (f *fakeTarget) SessionInfo(id SessionID) (SessionInfo, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, ok := f.sessions[id]
	return info, ok
}

func (f *fakeTarget) SendFollowUp(id SessionID, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.followErr != nil {
		return f.followErr
	}
	f.followUps = append(f.followUps, message)
	return nil
}

func (f *fakeTarget) ResolveTmuxTarget(id SessionID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tmuxTarget == "" {
		return "", fmt.Errorf("session %s is not a tmux session", id)
	}
	return f.tmuxTarget, nil
}

func (f *fakeTarget) CapturePaneText(id SessionID, _ int) ([]string, error) {
	f.mu.Lock()
	f.captureCount++
	delay := f.captureDelay
	err := f.captureErr
	lines := f.captured[id]
	f.mu.Unlock()
	if delay > 0 {
		// Outside the lock: this stands in for a real pane capture, which
		// shells out to tmux and holds nothing of the courier's.
		time.Sleep(delay)
	}
	if err != nil {
		return nil, err
	}
	return lines, nil
}

// appendPane mirrors text into every session's pane buffer. The courier only
// ever reads back the pane it just wrote to, so a shared buffer is enough and
// keeps the fake from needing to know which session a paste targeted.
func (f *fakeTarget) captures() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.captureCount
}

// setPane REPLACES every session's pane buffer, for tests that need the pane to
// stop saying what it said before — appendPane only ever adds, so an earlier
// working marker would still be found in the tail.
func (f *fakeTarget) setPane(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id := range f.sessions {
		f.captured[id] = strings.Split(text, "\n")
	}
}

func (f *fakeTarget) appendPane(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id := range f.sessions {
		f.captured[id] = append(f.captured[id], strings.Split(text, "\n")...)
	}
}

func (f *fakeTarget) MarkRunning(id SessionID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info := f.sessions[id]
	if info.Status == StatusIdle {
		info.Status = StatusRunning
		f.sessions[id] = info
	}
	f.markedRunning = append(f.markedRunning, id)
}

// mustInfo returns a registered session, for tests that hand a SessionInfo
// straight to a courier method.
func (f *fakeTarget) mustInfo(id SessionID) SessionInfo {
	info, _ := f.SessionInfo(id)
	return info
}

func (f *fakeTarget) sentFollowUps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.followUps...)
}

// fakePanes records tmux writes in order so a test can assert that a paste was
// followed by the Enter that submits it.
type fakePanes struct { //nolint:govet // fieldalignment: readability over packing
	mu       sync.Mutex
	writes   []string
	pasteErr error
	// echo, when set, mirrors a pasted line into the pane the courier reads
	// back, standing in for a TUI that accepted the paste.
	echo func(string)
	// onSubmit, when set, stands in for a TUI accepting Enter: the composer
	// clears and the text moves up into the transcript.
	onSubmit func()
	// enterErr, when set, stands in for a tmux send-keys that failed: the text
	// is in the composer and no turn was started.
	enterErr error
}

func (p *fakePanes) Paste(_ context.Context, target, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pasteErr != nil {
		return p.pasteErr
	}
	p.writes = append(p.writes, "paste("+target+"): "+text)
	if p.echo != nil {
		p.echo(text)
	}
	return nil
}

func (p *fakePanes) SendEnter(_ context.Context, target string) error {
	p.mu.Lock()
	onSubmit := p.onSubmit
	enterErr := p.enterErr
	p.writes = append(p.writes, "enter("+target+")")
	p.mu.Unlock()
	if enterErr != nil {
		return enterErr
	}
	if onSubmit != nil {
		onSubmit()
	}
	return nil
}

// pasteCount reports how many pastes reached the pane. Counting the writes
// rather than the echoes: a test whose echo is unset still needs to know
// whether the courier pasted once or twice.
func (p *fakePanes) pasteCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, w := range p.writes {
		if strings.HasPrefix(w, "paste(") {
			n++
		}
	}
	return n
}

func (p *fakePanes) recorded() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.writes...)
}

// echoPanes stands in for a CLI that echoes a pasted message into its composer.
// It writes real claude chrome because pasteConfirmed reads only a locatable
// composer, not a bare line.
func echoPanes(target *fakeTarget) *fakePanes {
	p := &fakePanes{}
	p.echo = func(text string) { target.appendPane(claudeComposerPane(text)) }
	// Submitting scrolls the composer contents up into the transcript, leaving
	// a fresh empty composer behind.
	p.onSubmit = func() { target.appendPane(claudeComposerPane("")) }
	return p
}

// claudeComposerPane renders a composer holding body as claude draws it.
func claudeComposerPane(body string) string {
	return strings.Join([]string{
		"────────────────────────────────────────────",
		"❯ " + body,
		"────────────────────────────────────────────",
		"  ~/wt/branch  main  Opus 4.6  ctx:43%  tokens:20k",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
	}, "\n")
}

// testCourierResultDir names a result dir inside this test's temp dir without
// creating it. Deliberately not pre-created: the courier's own MkdirAll must be
// what brings it into existence, or TestResultArtifactsAreNotWorldReadable
// would be asserting on the mode this helper chose rather than the one the
// production code does.
func testCourierResultDir(root string) string {
	return filepath.Join(root, "results")
}

func testCourierConfig(t *testing.T) CourierConfig {
	t.Helper()
	root := t.TempDir()
	return CourierConfig{
		DeliveryDir: filepath.Join(root, "deliveries"),
		ResultDir:   testCourierResultDir(root),
	}
}

func testCourierConfigDeliveryDir(t *testing.T, deliveryDir string) CourierConfig {
	t.Helper()
	return CourierConfig{
		DeliveryDir: deliveryDir,
		ResultDir:   testCourierResultDir(t.TempDir()),
	}
}

func newTestCourier(t *testing.T) (*Courier, *fakeTarget, *fakePanes) {
	t.Helper()
	target := newFakeTarget()
	// By default the fake TUI accepts what is pasted, so paste verification
	// passes. Tests that care about a dropped paste clear echo.
	panes := echoPanes(target)
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)
	return c, target, panes
}

// reportFixture builds the setup every subagent-report test shares: a courier,
// a running parent, and a child of that parent in the given status.
func reportFixture(t *testing.T, childStatus SessionStatus) (*Courier, *fakeTarget, SessionID, SessionID) {
	t.Helper()
	parentID, childID := ids(t)
	c, target, _ := newTestCourier(t)
	target.set(parentID, StatusRunning, RunnerTypeTmux)
	target.setChild(childID, parentID, childStatus, RunnerTypeTmux)
	return c, target, parentID, childID
}

// reportNow reads the child's current state and runs one report pass over it,
// the way the state-change watcher does.
func reportNow(c *Courier, target *fakeTarget, childID SessionID) {
	child, _ := target.SessionInfo(childID)
	c.reportToParent(context.Background(), child)
}

func reportResultPath(t *testing.T, report string) string {
	t.Helper()
	for _, line := range strings.Split(report, "\n") {
		path, ok := strings.CutPrefix(line, "result: ")
		if ok {
			return path
		}
	}
	require.FailNow(t, "report did not include a result path", report)
	return ""
}

// ids returns session IDs unique to this test so parallel runs get readable,
// collision-free fixture names in logs and assertions.
func ids(t *testing.T) (parent, child SessionID) {
	t.Helper()
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, t.Name())
	return SessionID("parent-" + safe), SessionID("child-" + safe)
}

// TestSendToIdleTUISessionUsesFollowUp pins that a TUI-mode recipient is
// reached through the turn loop, not through tmux — it has no pane to type in.
func TestSendToIdleTUISessionUsesFollowUp(t *testing.T) {
	t.Parallel()
	c, target, panes := newTestCourier(t)
	target.set("s1", StatusIdle, RunnerTypeTUI)

	queued, err := c.Send(context.Background(), "", "s1", "hello", true)
	require.NoError(t, err)
	assert.False(t, queued, "an idle session should be written immediately")
	assert.Equal(t, []string{"hello"}, target.sentFollowUps())
	assert.Empty(t, panes.recorded(), "TUI sessions must not be driven through tmux")
}

// TestSendToIdleTmuxSessionPastesAndSubmits covers the other half of the switch
// that is the courier's reason to exist.
func TestSendToIdleTmuxSessionPastesAndSubmits(t *testing.T) {
	t.Parallel()
	c, target, panes := newTestCourier(t)
	target.set("s1", StatusIdle, RunnerTypeTmux)

	queued, err := c.Send(context.Background(), "", "s1", "hello", true)
	require.NoError(t, err)
	assert.False(t, queued)
	assert.Equal(t, []string{"paste(@7): hello", "enter(@7)"}, panes.recorded())
	assert.Empty(t, target.sentFollowUps())
}

// TestSendWithoutSubmitDoesNotPressEnter keeps the draft case working: text is
// staged in the pane for a human to review rather than submitted.
func TestSendWithoutSubmitDoesNotPressEnter(t *testing.T) {
	t.Parallel()
	c, target, panes := newTestCourier(t)
	target.set("s1", StatusIdle, RunnerTypeTmux)

	_, err := c.Send(context.Background(), "", "s1", "draft", false)
	require.NoError(t, err)
	assert.Equal(t, []string{"paste(@7): draft"}, panes.recorded())
}

// TestSendToBusySessionQueuesInsteadOfInterrupting is the central case. Today's
// send-input would type into a running turn and land out of context; the whole
// queue exists so that cannot happen.
func TestSendToBusySessionQueuesInsteadOfInterrupting(t *testing.T) {
	t.Parallel()
	c, target, panes := newTestCourier(t)
	target.set("s1", StatusRunning, RunnerTypeTmux)

	queued, err := c.Send(context.Background(), "", "s1", "hello", true)
	require.NoError(t, err)
	assert.True(t, queued)
	assert.Empty(t, panes.recorded(), "nothing may be written while the recipient is mid-turn")
	assert.Empty(t, target.sentFollowUps())

	require.Len(t, c.Pending("s1"), 1)
	assert.Equal(t, "hello", c.Pending("s1")[0].Text)
}

// TestQueuedDeliveryLandsOnIdleTransition completes the story: the message is
// held, then written the moment the recipient is actually ready for it.
func TestQueuedDeliveryLandsOnIdleTransition(t *testing.T) {
	t.Parallel()
	c, target, panes := newTestCourier(t)
	target.set("s1", StatusRunning, RunnerTypeTmux)

	queued, err := c.Send(context.Background(), "", "s1", "hello", true)
	require.NoError(t, err)
	require.True(t, queued)

	target.set("s1", StatusIdle, RunnerTypeTmux)
	c.Drain(context.Background(), "s1")

	assert.Equal(t, []string{"paste(@7): hello", "enter(@7)"}, panes.recorded())
	assert.Empty(t, c.Pending("s1"), "a written delivery must not be written twice")
}

// TestDrainWritesOneDeliveryPerIdleTransition pins the pacing rule. Writing a
// message starts the recipient's next turn, so a second write in the same drain
// would land mid-turn — the very thing being prevented.
func TestDrainWritesOneDeliveryPerIdleTransition(t *testing.T) {
	t.Parallel()
	c, target, panes := newTestCourier(t)
	target.set("s1", StatusRunning, RunnerTypeTmux)

	for _, msg := range []string{"first", "second", "third"} {
		_, err := c.Send(context.Background(), "", "s1", msg, true)
		require.NoError(t, err)
	}
	require.Len(t, c.Pending("s1"), 3)

	target.set("s1", StatusIdle, RunnerTypeTmux)
	c.Drain(context.Background(), "s1")

	assert.Equal(t, []string{"paste(@7): first", "enter(@7)"}, panes.recorded())
	pending := c.Pending("s1")
	require.Len(t, pending, 2)
	assert.Equal(t, "second", pending[0].Text, "FIFO order must survive a partial drain")
	assert.Equal(t, "third", pending[1].Text)

	// Writing the first delivery started a turn, so the session is no longer
	// idle and a second drain right now is correctly a no-op.
	info, _ := target.SessionInfo("s1")
	require.Equal(t, StatusRunning, info.Status, "a submitted delivery should start a turn")
	c.Drain(context.Background(), "s1")
	assert.Len(t, panes.recorded(), 2, "nothing more may be written mid-turn")

	// That turn ends, and the next delivery goes out.
	target.set("s1", StatusIdle, RunnerTypeTmux)
	c.Drain(context.Background(), "s1")
	assert.Equal(t, "paste(@7): second", panes.recorded()[2])
	assert.Len(t, c.Pending("s1"), 1)
}

// TestDrainWhileBusyIsANoOp guards the queue against a spurious state change.
func TestDrainWhileBusyIsANoOp(t *testing.T) {
	t.Parallel()
	c, target, panes := newTestCourier(t)
	target.set("s1", StatusRunning, RunnerTypeTmux)
	_, err := c.Send(context.Background(), "", "s1", "hello", true)
	require.NoError(t, err)

	c.Drain(context.Background(), "s1")

	assert.Empty(t, panes.recorded())
	assert.Len(t, c.Pending("s1"), 1)
}

// TestFailedWriteKeepsDeliveryQueued makes a transient tmux error a retry
// rather than a silent drop.
func TestFailedWriteKeepsDeliveryQueued(t *testing.T) {
	t.Parallel()
	c, target, panes := newTestCourier(t)
	target.set("s1", StatusRunning, RunnerTypeTmux)
	_, err := c.Send(context.Background(), "", "s1", "hello", true)
	require.NoError(t, err)

	panes.pasteErr = errors.New("tmux exploded")
	target.set("s1", StatusIdle, RunnerTypeTmux)
	c.Drain(context.Background(), "s1")
	require.Len(t, c.Pending("s1"), 1, "a failed write must not consume the delivery")

	panes.pasteErr = nil
	c.Drain(context.Background(), "s1")
	assert.Equal(t, []string{"paste(@7): hello", "enter(@7)"}, panes.recorded())
	assert.Empty(t, c.Pending("s1"))
}

// TestSendToTerminalSessionIsRefused stops a caller from queueing a message
// that nothing will ever deliver.
func TestSendToTerminalSessionIsRefused(t *testing.T) {
	t.Parallel()
	for _, status := range []SessionStatus{StatusCompleted, StatusFailed, StatusStopped} {
		c, target, _ := newTestCourier(t)
		target.set("s1", status, RunnerTypeTmux)

		_, err := c.Send(context.Background(), "", "s1", "hello", true)
		require.Error(t, err, "status %s should be refused", status)
		assert.Contains(t, err.Error(), string(status))
		assert.Empty(t, c.Pending("s1"))
	}
}

// TestSendToUnknownSessionErrors keeps a typo'd ID from silently creating a
// queue nobody reads.
func TestSendToUnknownSessionErrors(t *testing.T) {
	t.Parallel()
	c, _, _ := newTestCourier(t)

	_, err := c.Send(context.Background(), "", "ghost", "hello", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	assert.Empty(t, c.Pending("ghost"))
}

// TestDrainDiscardsQueueForTerminalSession stops the on-disk queue leaking when
// a recipient dies with mail still waiting.
//
// It asserts on EVERY per-recipient map, not just the queue. discard is the
// only lifecycle cleanup point the courier has, so a map it forgets is a map
// that never empties: one entry per session that ever held a delivery, kept for
// the process lifetime, and inherited by any session ID reused after a terminal
// transition.
func TestDrainDiscardsQueueForTerminalSession(t *testing.T) {
	t.Parallel()
	c, target, _ := newTestCourier(t)
	target.set("s1", StatusRunning, RunnerTypeTmux)
	_, err := c.Send(context.Background(), "", "s1", "hello", true)
	require.NoError(t, err)

	// Populate the state a held delivery leaves behind, so the assertions below
	// are about release and not about maps that were empty all along.
	c.noteStaged("s1", "hello")
	c.noteDraftHold("s1", "a half-typed line")
	require.True(t, c.noteBlockedReport("s1", "a half-typed line"))
	c.notePaneHold("s1", []string{"some pane content"})
	c.mu.Lock()
	for name, populated := range map[string]bool{
		"staged":          len(c.staged) == 1,
		"heldForDraft":    len(c.heldForDraft) == 1,
		"reportedBlocked": len(c.reportedBlocked) == 1,
		"heldForPane":     len(c.heldForPane) == 1,
	} {
		require.True(t, populated, "%s must hold an entry before discard for this test to prove anything", name)
	}
	c.mu.Unlock()

	target.set("s1", StatusFailed, RunnerTypeTmux)
	c.Drain(context.Background(), "s1")

	assert.Empty(t, c.Pending("s1"))
	c.mu.Lock()
	assert.Empty(t, c.pending, "pending")
	assert.Empty(t, c.staged, "staged")
	assert.Empty(t, c.heldForDraft, "heldForDraft")
	assert.Empty(t, c.reportedBlocked, "reportedBlocked")
	assert.Empty(t, c.heldForPane, "heldForPane")
	c.mu.Unlock()

	// And again with NO queue, which is the ordinary case: Drain removes
	// pending itself once the last message lands, so a recipient whose mail all
	// delivered and then went terminal reaches discard with nothing queued.
	// Gating the release on a queue made the cleanup miss exactly that session.
	c.noteStaged("s1", "hello")
	c.noteDraftHold("s1", "a half-typed line")
	require.True(t, c.noteBlockedReport("s1", "a half-typed line"))
	c.notePaneHold("s1", []string{"some pane content"})
	c.mu.Lock()
	require.Empty(t, c.pending, "this half of the test is only meaningful with no queue")
	c.mu.Unlock()

	c.discard("s1")

	c.mu.Lock()
	defer c.mu.Unlock()
	assert.Empty(t, c.staged, "staged, with no queue to gate on")
	assert.Empty(t, c.heldForDraft, "heldForDraft, with no queue to gate on")
	assert.Empty(t, c.reportedBlocked, "reportedBlocked, with no queue to gate on")
	assert.Empty(t, c.heldForPane, "heldForPane, with no queue to gate on")
}

// TestQueueSurvivesReload is why the queue is on disk: a bramble restart must
// not lose a subagent's report.
func TestQueueSurvivesReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := newFakeTarget()
	target.set("s1", StatusRunning, RunnerTypeTmux)

	cfg := testCourierConfigDeliveryDir(t, dir)
	c1, err := NewCourier(target, echoPanes(target), cfg)
	require.NoError(t, err)
	for _, msg := range []string{"first", "second"} {
		_, err := c1.Send(context.Background(), "", "s1", msg, true)
		require.NoError(t, err)
	}

	panes := echoPanes(target)
	c2, err := NewCourier(target, panes, cfg)
	require.NoError(t, err)

	pending := c2.Pending("s1")
	require.Len(t, pending, 2)
	assert.Equal(t, "first", pending[0].Text)
	assert.Equal(t, "second", pending[1].Text)

	target.set("s1", StatusIdle, RunnerTypeTmux)
	c2.Drain(context.Background(), "s1")
	assert.Equal(t, []string{"paste(@7): first", "enter(@7)"}, panes.recorded())
}

// TestEmptyQueueLeavesNoFile keeps the delivery directory from filling with
// empty stubs, one per session that ever received a message.
func TestEmptyQueueLeavesNoFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := newFakeTarget()
	target.set("s1", StatusRunning, RunnerTypeTmux)

	c, err := NewCourier(target, echoPanes(target), testCourierConfigDeliveryDir(t, dir))
	require.NoError(t, err)
	_, err = c.Send(context.Background(), "", "s1", "hello", true)
	require.NoError(t, err)

	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	require.NoError(t, err)
	require.Len(t, files, 1)

	target.set("s1", StatusIdle, RunnerTypeTmux)
	c.Drain(context.Background(), "s1")

	files, err = filepath.Glob(filepath.Join(dir, "*.json"))
	require.NoError(t, err)
	assert.Empty(t, files)
}

// TestQueueFileNameIsSanitized keeps a hand-passed session ID from escaping the
// delivery directory. Generated IDs are tame, but the ID reaches this code
// straight off a socket.
func TestQueueFileNameIsSanitized(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := newFakeTarget()
	target.set("../../escape", StatusRunning, RunnerTypeTmux)

	c, err := NewCourier(target, &fakePanes{}, testCourierConfigDeliveryDir(t, dir))
	require.NoError(t, err)
	_, err = c.Send(context.Background(), "", "../../escape", "hello", true)
	require.NoError(t, err)

	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	require.NoError(t, err)
	require.Len(t, files, 1, "the queue must stay inside the delivery dir")
	assert.Equal(t, "______escape.json", filepath.Base(files[0]))

	// The path the unsanitized ID would have produced must not exist.
	escaped := filepath.Join(dir, "../../escape.json")
	_, statErr := os.Stat(escaped)
	assert.True(t, os.IsNotExist(statErr), "queue escaped to %s", escaped)
}

// TestPendingReturnsACopy stops a caller mutating the courier's queue.
func TestPendingReturnsACopy(t *testing.T) {
	t.Parallel()
	c, target, _ := newTestCourier(t)
	target.set("s1", StatusRunning, RunnerTypeTmux)
	_, err := c.Send(context.Background(), "", "s1", "hello", true)
	require.NoError(t, err)

	got := c.Pending("s1")
	got[0].Text = "tampered"
	assert.Equal(t, "hello", c.Pending("s1")[0].Text)
}

// TestSendToPendingSessionQueues covers a session spawned but not yet running:
// it has no runner type, so there is nowhere to write yet.
func TestSendToPendingSessionQueues(t *testing.T) {
	t.Parallel()
	c, target, panes := newTestCourier(t)
	target.set("s1", StatusPending, "")

	queued, err := c.Send(context.Background(), "", "s1", "hello", true)
	require.NoError(t, err)
	assert.True(t, queued)
	assert.Empty(t, panes.recorded())
}

// TestWatchDrainsOnIdle exercises the real wiring: a live Manager's state
// changes drive the courier with no polling anywhere.
func TestWatchDrainsOnIdle(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	panes := echoPanes(target)
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	mgr := NewManagerWithConfig(ManagerConfig{RepoName: "repo"})
	defer mgr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	unsub := c.Watch(ctx, mgr)
	defer unsub()

	target.set("s1", StatusRunning, RunnerTypeTmux)
	_, err = c.Send(ctx, "", "s1", "hello", true)
	require.NoError(t, err)

	// Flip the fake to idle first, then announce the transition, mirroring the
	// order the Manager itself uses.
	target.set("s1", StatusIdle, RunnerTypeTmux)
	mgr.emitSessionStateChange(SessionStateChangeEvent{
		SessionID: "s1", OldStatus: StatusRunning, NewStatus: StatusIdle,
	})

	require.Eventually(t, func() bool { return len(c.Pending("s1")) == 0 },
		5*time.Second, 10*time.Millisecond,
		"queued delivery was not written after the idle transition")
	assert.Equal(t, []string{"paste(@7): hello", "enter(@7)"}, panes.recorded())
}

// TestWatchDiscardsOnTerminal pins the cleanup half of the watcher.
func TestWatchDiscardsOnTerminal(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	c, err := NewCourier(target, echoPanes(target), testCourierConfig(t))
	require.NoError(t, err)

	mgr := NewManagerWithConfig(ManagerConfig{RepoName: "repo"})
	defer mgr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	unsub := c.Watch(ctx, mgr)
	defer unsub()

	target.set("s1", StatusRunning, RunnerTypeTmux)
	_, err = c.Send(ctx, "", "s1", "hello", true)
	require.NoError(t, err)
	require.Len(t, c.Pending("s1"), 1)

	mgr.emitSessionStateChange(SessionStateChangeEvent{
		SessionID: "s1", OldStatus: StatusRunning, NewStatus: StatusFailed,
	})

	require.Eventually(t, func() bool {
		return len(c.Pending("s1")) == 0
	}, 5*time.Second, 10*time.Millisecond, "queue for a dead session should be reclaimed")
}

// TestNewCourierIgnoresJunkFiles keeps a stray file in the delivery directory
// from failing bramble's startup.
func TestNewCourierIgnoresJunkFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{{{"), 0o644))

	c, err := NewCourier(newFakeTarget(), &fakePanes{}, testCourierConfigDeliveryDir(t, dir))
	require.NoError(t, err)
	assert.Empty(t, c.Pending("s1"))
}

// --- subagent auto-reporting -------------------------------------------------

// TestChildIdleReportsToParent is the codex case. A non-Claude child cannot be
// reliably told to call back, so bramble reports on its behalf — otherwise a
// parent that spawned a codex subagent would wait forever.
func TestChildIdleReportsToParent(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)
	target.annotate(childID, func(i *SessionInfo) {
		i.Type = SessionTypeCodeTalk
		i.Model = "gpt-5.4-mini"
		i.ResearchFilePath = "/tmp/some-plan/child.md"
		i.Progress = SessionProgressSnapshot{TurnCount: 2, TotalCostUSD: 0.25}
	})

	reportNow(c, target, childID)

	pending := c.Pending(parentID)
	require.Len(t, pending, 1, "the parent should have been told its child is done")
	assert.Equal(t, SessionID(childID), pending[0].From)
	assert.Contains(t, pending[0].Text, "subagent child")
	assert.Contains(t, pending[0].Text, "gpt-5.4-mini")
	assert.Contains(t, pending[0].Text, "result: /tmp/some-plan/child.md")
}

// TestReportIsSentOnlyOncePerStatus keeps a chatty session from nagging its
// parent with the same news after every state change.
func TestReportIsSentOnlyOncePerStatus(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)

	reportNow(c, target, childID)
	reportNow(c, target, childID)
	reportNow(c, target, childID)

	assert.Len(t, c.Pending(parentID), 1)
}

// TestCompletedAfterIdleIsSilent pins the quiet rule: a tmux window closing
// after the result was already reported carries no new information.
func TestCompletedAfterIdleIsSilent(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)

	reportNow(c, target, childID)

	target.setChild(childID, parentID, StatusCompleted, RunnerTypeTmux)
	reportNow(c, target, childID)

	assert.Len(t, c.Pending(parentID), 1, "completion after a report adds nothing")
}

// TestFailureIsReportedEvenAfterAnIdleReport is the exception to that rule: a
// failure changes what the parent should do next.
func TestFailureIsReportedEvenAfterAnIdleReport(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)

	reportNow(c, target, childID)

	target.setChild(childID, parentID, StatusFailed, RunnerTypeTmux)
	target.annotate(childID, func(i *SessionInfo) { i.ErrorMsg = "context window exhausted" })
	reportNow(c, target, childID)

	pending := c.Pending(parentID)
	require.Len(t, pending, 2)
	assert.Contains(t, pending[1].Text, "context window exhausted")
}

// TestCompletedWithoutPriorReportIsAnnounced covers a child that dies before
// ever going idle — the parent still needs to hear about it.
func TestCompletedWithoutPriorReportIsAnnounced(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusCompleted)

	reportNow(c, target, childID)

	require.Len(t, c.Pending(parentID), 1)
}

// TestChildSelfReportSuppressesGeneratedReport keeps bramble from talking over
// a subagent that wrote its own, better summary.
func TestChildSelfReportSuppressesGeneratedReport(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusRunning)

	// The child speaks for itself while still mid-turn.
	_, err := c.Send(context.Background(), childID, parentID, "done: see /tmp/mine.md", true)
	require.NoError(t, err)

	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)
	reportNow(c, target, childID)

	pending := c.Pending(parentID)
	require.Len(t, pending, 1, "bramble should not repeat what the child already said")
	assert.Equal(t, "done: see /tmp/mine.md", pending[0].Text)
}

// TestUnrelatedSenderDoesNotSuppressReport guards the suppression from firing
// on a message between two sessions that are not parent and child.
func TestUnrelatedSenderDoesNotSuppressReport(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusRunning)
	target.set("stranger", StatusRunning, RunnerTypeTmux)

	_, err := c.Send(context.Background(), "stranger", parentID, "fyi", true)
	require.NoError(t, err)

	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)
	reportNow(c, target, childID)

	assert.Len(t, c.Pending(parentID), 2)
}

// TestTopLevelSessionReportsToNobody keeps ordinary sessions from generating
// mail for a parent they do not have.
func TestTopLevelSessionReportsToNobody(t *testing.T) {
	t.Parallel()
	c, target, _ := newTestCourier(t)
	target.set("solo", StatusIdle, RunnerTypeTmux)

	info, _ := target.SessionInfo("solo")
	c.reportToParent(context.Background(), info)

	assert.Empty(t, c.Pending("solo"))
	assert.Empty(t, c.Pending(""))
}

// TestReportPrefersPlanOverTranscript: a planner subagent was asked to produce
// a plan, so that path is the answer — the transcript is just what it said.
func TestReportPrefersPlanOverTranscript(t *testing.T) {
	t.Parallel()
	parentID, childID := ids(t)
	info := SessionInfo{
		ID: childID, Type: SessionTypePlanner, Status: StatusIdle,
		ParentSessionID:  parentID,
		PlanFilePath:     "/plans/x.md",
		ResearchFilePath: "/tmp/x.md",
	}
	text := formatSubagentReport(info, info.PlanFilePath)
	assert.Contains(t, text, "plan: /plans/x.md")
	assert.NotContains(t, text, "/tmp/x.md")
}

// TestReportToIdleParentIsWrittenImmediately checks the report takes the same
// delivery path as any other message rather than always queueing.
func TestReportToIdleParentIsWrittenImmediately(t *testing.T) {
	t.Parallel()
	parentID, childID := ids(t)
	c, target, panes := newTestCourier(t)
	target.set(parentID, StatusIdle, RunnerTypeTmux)
	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)

	reportNow(c, target, childID)

	require.Len(t, panes.recorded(), 2)
	assert.Contains(t, panes.recorded()[0], "subagent child")
	assert.Empty(t, c.Pending(parentID))
}

// TestFailedReportIsQueuedNotDropped is the dedup-before-delivery case:
// shouldReport reserves the status before the write is attempted, so a failed
// write must either release the reservation or make the message durable. It
// makes it durable — the queue plus its timed retry outlive a transient tmux
// error, and the reservation is what stops the retry arriving twice.
func TestFailedReportIsQueuedNotDropped(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)
	// An idle parent is written to directly, so a paste failure exercises the
	// direct path rather than the queued one.
	target.set(parentID, StatusIdle, RunnerTypeTmux)
	panes := &fakePanes{pasteErr: errors.New("tmux exploded")}
	c.panes = panes

	reportNow(c, target, childID)
	require.Empty(t, panes.recorded(), "a failed paste records no write")
	require.Len(t, c.Pending(parentID), 1,
		"a report whose write failed must be queued: an already-idle parent makes no further transition to retry on")

	c.mu.Lock()
	armed, seen := c.retryArmed, c.reported[childID][StatusIdle]
	c.mu.Unlock()
	assert.True(t, armed, "nothing would ever write the queued report")
	assert.True(t, seen, "the queued report still stands; reporting it again would deliver it twice")
}

// TestUnqueueableReportIsRetriedOnTheNextTransition is the remaining way a
// report can be lost outright: the write failed *and* the queue could not take
// it, so nothing holds the message. The reservation must be released or the
// parent never hears about this child again.
func TestUnqueueableReportIsRetriedOnTheNextTransition(t *testing.T) {
	t.Parallel()
	queueDir := t.TempDir()
	target := newFakeTarget()
	c, err := NewCourier(target, &fakePanes{pasteErr: errors.New("tmux exploded")},
		testCourierConfigDeliveryDir(t, queueDir))
	require.NoError(t, err)
	parentID, childID := ids(t)
	target.set(parentID, StatusIdle, RunnerTypeTmux)
	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)

	// A queue directory that cannot be written to is the failure enqueue
	// reports; 0500 leaves it readable so NewCourier's load still works.
	require.NoError(t, os.Chmod(queueDir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(queueDir, 0o700) })

	reportNow(c, target, childID)
	require.Empty(t, c.Pending(parentID), "the queue rejected the delivery, so nothing is holding it")
	c.mu.Lock()
	seen := len(c.reported[childID])
	c.mu.Unlock()
	require.Zero(t, seen, "a report nothing holds must leave no state claiming the parent was told")

	// The same child goes idle again; the report must be attempted afresh.
	require.NoError(t, os.Chmod(queueDir, 0o700))
	working := echoPanes(target)
	c.panes = working
	reportNow(c, target, childID)

	assert.Contains(t, strings.Join(working.recorded(), "\n"), "subagent "+string(childID),
		"the report was never retried after the first attempt failed")
}

// TestGeneratedReportDoesNotCountAsTheChildSpeaking keeps the courier's own
// report from registering as a self-report. If it did it would mark idle,
// completed and stopped in one go — see noteChildSpoke — and every later state
// of this child would be silently swallowed as already told.
func TestGeneratedReportDoesNotCountAsTheChildSpeaking(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)
	target.set(parentID, StatusIdle, RunnerTypeTmux)

	reportNow(c, target, childID)
	require.Empty(t, c.Pending(parentID), "an idle parent takes the report directly")

	c.mu.Lock()
	seen := map[SessionStatus]bool{}
	for k, v := range c.reported[childID] {
		seen[k] = v
	}
	c.mu.Unlock()
	assert.Equal(t, map[SessionStatus]bool{StatusIdle: true}, seen,
		"the courier's own report stands only for the status it reported")
}

// TestReportToDeadParentIsDropped covers the child outliving its parent: there
// is nowhere to report, and that must not be an error or a leaked queue.
func TestReportToDeadParentIsDropped(t *testing.T) {
	t.Parallel()
	parentID, childID := ids(t)
	c, target, _ := newTestCourier(t)
	target.set(parentID, StatusCompleted, RunnerTypeTmux)
	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)

	reportNow(c, target, childID)

	assert.Empty(t, c.Pending(parentID))
}

// TestQueueThatCannotPersistIsNotReportedAsQueued pins the meaning of
// "queued": the caller is told the message survives a restart, so a queue that
// could not be written must be an error rather than a promise this process
// cannot keep.
func TestQueueThatCannotPersistIsNotReportedAsQueued(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	dir := t.TempDir()
	c, err := NewCourier(target, &fakePanes{}, testCourierConfigDeliveryDir(t, dir))
	require.NoError(t, err)
	target.set("s1", StatusRunning, RunnerTypeTmux)

	// Make the queue file unwritable by putting a directory where it goes.
	qp, err := c.queuePath("s1")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(qp, 0o700))

	queued, err := c.Send(context.Background(), "", "s1", "hello", true)
	require.Error(t, err, "an unpersistable queue must not be reported as queued")
	assert.False(t, queued)
	assert.Empty(t, c.Pending("s1"), "the failed delivery must not linger in memory either")
}

// TestConcurrentSendsToAnIdleRecipientQueueTheLoser pins the write claim on the
// direct path: two reports landing on one idle parent must not both be typed
// into the same turn, which is the interruption the queue exists to prevent.
func TestConcurrentSendsToAnIdleRecipientQueueTheLoser(t *testing.T) {
	t.Parallel()
	c, target, panes := newTestCourier(t)
	target.set("s1", StatusIdle, RunnerTypeTmux)

	// Hold the first write inside its submit, which the fake runs outside its
	// own lock, then send again while it is provably still in flight.
	entered := make(chan struct{})
	release := make(chan struct{})
	panes.onSubmit = func() {
		close(entered)
		<-release
		target.appendPane("> ")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.Send(context.Background(), "", "s1", "first", true)
	}()
	<-entered

	queued, err := c.Send(context.Background(), "", "s1", "second", true)
	require.NoError(t, err)
	assert.True(t, queued, "a send racing a write in flight must queue, not join the turn")

	close(release)
	<-done

	for _, w := range panes.recorded() {
		assert.NotContains(t, w, "second", "the second message was written into a live turn")
	}
}

// TestReAdoptionDoesNotReReportOnEveryRestart guards the synthetic same-status
// event reconciliation emits so restored mail can drain. It is not a
// transition, and the dedup map lives only in memory, so reporting on it would
// hand the parent the same report again after every restart.
func TestReAdoptionDoesNotReReportOnEveryRestart(t *testing.T) {
	t.Parallel()
	parentID, childID := ids(t)
	target := newFakeTarget()
	c, err := NewCourier(target, echoPanes(target), testCourierConfig(t))
	require.NoError(t, err)
	target.set(parentID, StatusRunning, RunnerTypeTmux)
	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)

	mgr := NewManagerWithConfig(ManagerConfig{RepoName: "repo"})
	defer mgr.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	unsub := c.Watch(ctx, mgr)
	defer unsub()

	child, _ := target.SessionInfo(childID)
	mgr.emitSessionStateChange(SessionStateChangeEvent{
		Info: child, SessionID: childID,
		OldStatus: StatusIdle, NewStatus: StatusIdle,
	})

	require.Never(t, func() bool { return len(c.Pending(parentID)) > 0 },
		500*time.Millisecond, 20*time.Millisecond,
		"a re-adoption emit is not a transition and must not re-report")
}

// TestFailedWriteToAnIdleRecipientIsRetried covers the case that produces no
// further transition to ride: the recipient was already idle when the drain
// ran, so waiting for the next running->idle edge means waiting forever.
func TestFailedWriteToAnIdleRecipientIsRetried(t *testing.T) {
	t.Parallel()
	c, target, _ := newTestCourier(t)
	target.set("s1", StatusRunning, RunnerTypeTmux)
	_, err := c.Send(context.Background(), "", "s1", "report me", true)
	require.NoError(t, err)

	// The write fails while the recipient sits idle and stays idle.
	failing := &fakePanes{pasteErr: errors.New("tmux exploded")}
	c.panes = failing
	target.set("s1", StatusIdle, RunnerTypeTmux)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Drain(ctx, "s1")
	require.Len(t, c.Pending("s1"), 1, "a failed write keeps the delivery queued")

	// A retry must be armed rather than left to a transition that never comes.
	c.mu.Lock()
	armed := c.retryArmed
	c.mu.Unlock()
	assert.True(t, armed, "no retry was scheduled for a recipient that never leaves idle")
}

// TestFailedDirectWriteIsQueuedAndRetried is the same hole as
// TestFailedWriteToAnIdleRecipientIsRetried on the other write path. Send takes
// the direct branch when the recipient is already idle, and that is exactly the
// recipient that produces no further transition: returning the error and
// dropping the text loses a subagent report for good, because a child in a
// terminal state never reports again.
func TestFailedDirectWriteIsQueuedAndRetried(t *testing.T) {
	t.Parallel()
	c, target, _ := newTestCourier(t)
	c.panes = &fakePanes{pasteErr: errors.New("tmux exploded")}
	target.set("s1", StatusIdle, RunnerTypeTmux)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queued, err := c.Send(ctx, "", "s1", "report me", true)
	require.NoError(t, err, "a failed write must not lose the message")
	assert.True(t, queued, "the caller must be told the message is waiting, not written")
	require.Len(t, c.Pending("s1"), 1, "a failed direct write must leave the delivery queued")

	c.mu.Lock()
	armed := c.retryArmed
	c.mu.Unlock()
	assert.True(t, armed, "no retry was scheduled for a recipient that never leaves idle")
}

// TestNoReportIsLostWhenManySubagentsFinishAtOnce pins the path a completion
// takes from the manager to the courier. The courier's handling of one event is
// slow — a report captures a pane and writes a file — so the emitter runs far
// ahead of it, which is what a bounded buffer between them would drop. Fan-out
// is the case this feature exists for, and a dropped completion is the one event
// that never comes again: the parent would wait forever for a subagent that
// already finished. See SubscribeStateChanges for why there is no such buffer.
func TestNoReportIsLostWhenManySubagentsFinishAtOnce(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	c, err := NewCourier(target, echoPanes(target), testCourierConfig(t))
	require.NoError(t, err)

	mgr := NewManagerWithConfig(ManagerConfig{RepoName: "repo"})
	defer mgr.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.Watch(ctx, mgr)()

	// Busy, so every report queues rather than racing a write.
	parentID, _ := ids(t)
	target.set(parentID, StatusRunning, RunnerTypeTmux)

	// Slow enough that the emitter runs far ahead of the handler, which is what
	// lost events with a bounded channel.
	target.mu.Lock()
	target.captureDelay = 2 * time.Millisecond
	target.mu.Unlock()

	const children = 150
	for i := range children {
		childID := SessionID(fmt.Sprintf("%s-child-%03d", parentID, i))
		target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)
		target.mu.Lock()
		target.captured[childID] = []string{"answer from " + string(childID)}
		target.mu.Unlock()
		mgr.emitSessionStateChange(SessionStateChangeEvent{
			Info:      target.mustInfo(childID),
			SessionID: childID,
			OldStatus: StatusRunning,
			NewStatus: StatusIdle,
		})
	}

	require.Eventually(t, func() bool { return len(c.Pending(parentID)) == children },
		30*time.Second, 20*time.Millisecond,
		"only %d of %d subagents were reported: events were dropped on the way to the courier",
		len(c.Pending(parentID)), children)
}

// TestEventPumpKeepsEventsQueuedBeforeItsWorkerStarts covers the ordering the
// pump promises. push runs on the goroutine making the status transition and
// may well run before the worker is scheduled at all, so the queue — not a
// wakeup — is what carries those events, and it must carry them in order.
func TestEventPumpKeepsEventsQueuedBeforeItsWorkerStarts(t *testing.T) {
	t.Parallel()
	p := newEventPump()

	const events = 5
	for i := range events {
		p.push(SessionStateChangeEvent{SessionID: SessionID(fmt.Sprintf("s%d", i))})
	}

	seen := make(chan SessionID, events)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.run(ctx, func(evt SessionStateChangeEvent) { seen <- evt.SessionID })
	t.Cleanup(p.close)

	for i := range events {
		select {
		case got := <-seen:
			require.Equal(t, SessionID(fmt.Sprintf("s%d", i)), got,
				"events queued before the worker started must arrive in order")
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d events queued before the worker started arrived", i, events)
		}
	}
}

// TestEventPumpDrainsWhatItHoldsOnClose is the contract with teeth. An event
// dropped at close is a completion that never became a queued Delivery, so
// unlike a delivery it is not recoverable from anything on disk — the parent
// simply never hears.
func TestEventPumpDrainsWhatItHoldsOnClose(t *testing.T) {
	t.Parallel()
	p := newEventPump()

	// Block the worker on its first event so the rest are still queued when the
	// pump is closed underneath it.
	release := make(chan struct{})
	var handled sync.WaitGroup
	handled.Add(4)
	first := true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.run(ctx, func(SessionStateChangeEvent) {
		if first {
			first = false
			<-release
		}
		handled.Done()
	})

	for i := range 4 {
		p.push(SessionStateChangeEvent{SessionID: SessionID(fmt.Sprintf("s%d", i))})
	}
	p.close()
	close(release)

	done := make(chan struct{})
	go func() { handled.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the pump discarded events it had already accepted")
	}
}

// TestStaleQueueIsReclaimedOnLoad bounds the cost of never discarding on an
// unknown recipient: a session that vanished without a terminal transition
// would otherwise keep its queue file forever.
func TestStaleQueueIsReclaimedOnLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	old := []Delivery{{
		ID: "1", To: "gone", Text: "nobody is coming for this", Submit: true,
		CreatedAt: time.Now().Add(-maxDeliveryAge - time.Hour),
	}}
	data, err := json.MarshalIndent(old, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gone.json"), data, 0o600))

	c, err := NewCourier(newFakeTarget(), &fakePanes{}, testCourierConfigDeliveryDir(t, dir))
	require.NoError(t, err)

	assert.Empty(t, c.Pending("gone"), "a delivery past its age should not be reloaded")
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	require.NoError(t, err)
	assert.Empty(t, files, "the emptied queue file should be reclaimed")
}

// TestUnknownRecipientKeepsItsQueue is the failure mode the startup sweep
// introduced: DrainIdle runs over every persisted recipient, including ones
// whose repo has not been opened and ones reconciliation has not re-adopted
// yet. Treating "I cannot see it" as "it is gone" deleted the queue on every
// restart — exactly what persisting it was for.
func TestUnknownRecipientKeepsItsQueue(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := newFakeTarget()
	target.set("s1", StatusRunning, RunnerTypeTmux)

	cfg := testCourierConfigDeliveryDir(t, dir)
	c, err := NewCourier(target, echoPanes(target), cfg)
	require.NoError(t, err)
	_, err = c.Send(context.Background(), "", "s1", "held for a repo that is not open yet", true)
	require.NoError(t, err)

	// A courier that cannot see the recipient at all — a not-yet-registered
	// manager, or a sweep that beat reconciliation to it.
	blind, err := NewCourier(newFakeTarget(), &fakePanes{}, cfg)
	require.NoError(t, err)
	require.Len(t, blind.Pending("s1"), 1)

	blind.DrainIdle(context.Background())

	assert.Len(t, blind.Pending("s1"), 1, "an unseen recipient's queue must survive the sweep")
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	require.NoError(t, err)
	assert.Len(t, files, 1, "the queue file must still be on disk")
}

// TestConcurrentDrainsDeliverOnlyOnce pins the claim: Watch and the startup
// sweep can reach one recipient at the same moment, and Drain does not hold the
// lock across the write.
func TestConcurrentDrainsDeliverOnlyOnce(t *testing.T) {
	t.Parallel()
	c, target, panes := newTestCourier(t)
	target.set("s1", StatusRunning, RunnerTypeTmux)
	_, err := c.Send(context.Background(), "", "s1", "deliver me once", true)
	require.NoError(t, err)
	target.set("s1", StatusIdle, RunnerTypeTmux)

	// Deterministic, not a stress loop: hold the first drain inside its write
	// (onSubmit runs outside the fake's lock) and call Drain again while it is
	// provably still in flight. Without a claim the second drain reads the same
	// head — the dequeue only happens after write returns — and pastes it too.
	entered := make(chan struct{})
	release := make(chan struct{})
	panes.onSubmit = func() {
		close(entered)
		<-release
		target.appendPane("> ")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Drain(context.Background(), "s1")
	}()
	<-entered
	c.Drain(context.Background(), "s1")
	close(release)
	<-done

	pastes := 0
	for _, w := range panes.recorded() {
		if strings.Contains(w, "deliver me once") {
			pastes++
		}
	}
	assert.Equal(t, 1, pastes, "concurrent drains delivered the same message more than once")
}

// TestDrainIdleDeliversAfterAReload covers the restart gap: Watch only reacts
// to idle transitions, and a recipient that is already idle when bramble comes
// back makes no transition to react to.
func TestDrainIdleDeliversAfterAReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := newFakeTarget()
	target.set("s1", StatusRunning, RunnerTypeTmux)

	cfg := testCourierConfigDeliveryDir(t, dir)
	first, err := NewCourier(target, echoPanes(target), cfg)
	require.NoError(t, err)
	queued, err := first.Send(context.Background(), "", "s1", "held over a restart", true)
	require.NoError(t, err)
	require.True(t, queued)

	// A fresh courier over the same directory is what a restart looks like.
	reloaded, err := NewCourier(target, echoPanes(target), cfg)
	require.NoError(t, err)
	require.Len(t, reloaded.Pending("s1"), 1, "the queue should have been reloaded")

	// The recipient is already idle and will never transition again.
	target.set("s1", StatusIdle, RunnerTypeTmux)
	reloaded.DrainIdle(context.Background())

	assert.Empty(t, reloaded.Pending("s1"), "a queue reloaded for an already-idle session was never delivered")
}

// TestResultArtifactsAreNotWorldReadable pins the privacy of what a subagent
// leaves behind: a captured pane is the child's whole transcript, so neither it
// nor the directory holding it may be readable by another local user.
//
// The assertion is about the mode a *fresh* directory is created with, since
// os.MkdirAll leaves an existing one alone — a dir some earlier test had made
// would let this pass with the fix reverted. An injected ResultDir gets that
// without t.Setenv("HOME"), which would forbid t.Parallel: testCourierResultDir
// names a path inside this test's temp dir and deliberately does not create it,
// so the courier's own MkdirAll is what this measures.
func TestResultArtifactsAreNotWorldReadable(t *testing.T) {
	t.Parallel()
	c, target, _, childID := reportFixture(t, StatusIdle)
	target.annotate(childID, func(i *SessionInfo) { i.RunnerType = RunnerTypeTmux })
	target.captured[childID] = []string{"secret: hunter2"}

	path := c.resultPathFor(target.mustInfo(childID))
	require.NotEmpty(t, path, "the pane capture should have produced a result file")

	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "a transcript must not be readable by other users")

	di, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), di.Mode().Perm(), "the result dir must not be traversable by other users")
}

// TestResultFilesLiveUnderThePrivateHome is the other half of the privacy
// invariant, and it belongs here rather than on a courier: mode bits on a path
// in a shared temp dir prove nothing, because another local user can own the
// directory those bits sit in. Only $HOME is not writable by anyone else.
//
// Asserted against DefaultResultDir, which is the one place that decides the
// location, rather than against a configured courier — a test that injects its
// own dir necessarily cannot check this, and README and the design docs quote
// ~/.bramble/research/<id>.md to users.
func TestResultFilesLiveUnderThePrivateHome(t *testing.T) {
	t.Parallel()

	dir, err := DefaultResultDir()
	require.NoError(t, err)

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".bramble", resultDirName), dir,
		"result files must land under the user's private ~/.bramble")
}

// TestCompletedChildIsReportedAfterTheManagerDropsIt is the window-close path
// Gemini and Agy depend on, and the one the tmux monitor races: it emits
// StatusCompleted and immediately deletes the session, so a subscriber that
// looks the child up by ID finds nothing and reports nothing.
func TestCompletedChildIsReportedAfterTheManagerDropsIt(t *testing.T) {
	t.Parallel()
	parentID, childID := ids(t)
	target := newFakeTarget()
	c, err := NewCourier(target, echoPanes(target), testCourierConfig(t))
	require.NoError(t, err)
	target.set(parentID, StatusRunning, RunnerTypeTmux)

	mgr := NewManagerWithConfig(ManagerConfig{RepoName: "repo"})
	defer mgr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	unsub := c.Watch(ctx, mgr)
	defer unsub()

	// The child is gone from the target by the time the event is handled —
	// exactly what deleting it from m.sessions produces.
	child := SessionInfo{
		ID:              childID,
		ParentSessionID: parentID,
		Status:          StatusCompleted,
		Type:            SessionTypeCodeTalk,
		RunnerType:      RunnerTypeTmux,
	}
	mgr.emitSessionStateChange(SessionStateChangeEvent{
		Info:      child,
		SessionID: childID,
		OldStatus: StatusRunning,
		NewStatus: StatusCompleted,
	})

	require.Eventually(t, func() bool { return len(c.Pending(parentID)) == 1 },
		5*time.Second, 10*time.Millisecond,
		"a child the manager already dropped was never reported to its parent")
	assert.Contains(t, c.Pending(parentID)[0].Text, "is completed")
}

// TestWatchReportsChildCompletion is the end-to-end wiring: a real Manager
// state change produces a report with no polling.
func TestWatchReportsChildCompletion(t *testing.T) {
	t.Parallel()
	parentID, childID := ids(t)
	target := newFakeTarget()
	c, err := NewCourier(target, echoPanes(target), testCourierConfig(t))
	require.NoError(t, err)

	mgr := NewManagerWithConfig(ManagerConfig{RepoName: "repo"})
	defer mgr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	unsub := c.Watch(ctx, mgr)
	defer unsub()

	target.set(parentID, StatusRunning, RunnerTypeTmux)
	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)
	target.annotate(childID, func(i *SessionInfo) { i.ResearchFilePath = "/tmp/child.md" })

	mgr.emitSessionStateChange(SessionStateChangeEvent{
		SessionID: childID, OldStatus: StatusRunning, NewStatus: StatusIdle,
	})

	require.Eventually(t, func() bool {
		return len(c.Pending(parentID)) == 1
	}, 5*time.Second, 10*time.Millisecond, "parent was never told its subagent finished")
	assert.Contains(t, c.Pending(parentID)[0].Text, "/tmp/child.md")
}

// TestTmuxChildResultComesFromPaneCapture is the codex-in-tmux path. That mode
// never runs the TUI turn loop, so bramble holds no transcript — without the
// capture the parent would be told "your subagent finished" and handed nothing.
func TestTmuxChildResultComesFromPaneCapture(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)
	target.mu.Lock()
	target.captured[childID] = []string{"codex here", "the answer is 42"}
	target.mu.Unlock()

	reportNow(c, target, childID)

	pending := c.Pending(parentID)
	require.Len(t, pending, 1)

	path := reportResultPath(t, pending[0].Text)

	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "codex here\nthe answer is 42\n", string(body))
}

// TestCaptureFailureStillReports keeps a dead pane from swallowing the report:
// the parent needs to know the child finished even without a result file.
func TestCaptureFailureStillReports(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusFailed)
	target.mu.Lock()
	target.captureErr = errors.New("window is gone")
	target.mu.Unlock()

	reportNow(c, target, childID)

	pending := c.Pending(parentID)
	require.Len(t, pending, 1)
	assert.Contains(t, pending[0].Text, "subagent child")
	assert.NotContains(t, pending[0].Text, "result:")
}

// TestTUIChildDoesNotCapturePane guards against reaching for tmux on a session
// that has no pane; its transcript is the result.
func TestTUIChildDoesNotCapturePane(t *testing.T) {
	t.Parallel()
	parentID, childID := ids(t)
	c, target, _ := newTestCourier(t)
	target.set(parentID, StatusRunning, RunnerTypeTmux)
	target.setChild(childID, parentID, StatusIdle, RunnerTypeTUI)
	target.annotate(childID, func(i *SessionInfo) { i.ResearchFilePath = "/tmp/transcript.md" })

	reportNow(c, target, childID)

	require.Len(t, c.Pending(parentID), 1)
	assert.Contains(t, c.Pending(parentID)[0].Text, "result: /tmp/transcript.md")
}

// TestRunningTransitionRearmsIdleReporting is the path a prematurely-reported
// codex subagent takes: it announces idle, is reported, keeps working without
// the courier writing to it, then goes idle again — and the parent must hear
// about that second idle.
func TestRunningTransitionRearmsIdleReporting(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusRunning)

	mgr := NewManagerWithConfig(ManagerConfig{RepoName: "repo"})
	defer mgr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	unsub := c.Watch(ctx, mgr)
	defer unsub()

	// Round 1: premature idle is reported.
	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)
	mgr.emitSessionStateChange(SessionStateChangeEvent{
		SessionID: childID, OldStatus: StatusRunning, NewStatus: StatusIdle,
	})
	require.Eventually(t, func() bool { return len(c.Pending(parentID)) == 1 },
		5*time.Second, 10*time.Millisecond,
		"first idle was never reported")

	// The child keeps working — no courier write, just a status transition.
	target.setChild(childID, parentID, StatusRunning, RunnerTypeTmux)
	mgr.emitSessionStateChange(SessionStateChangeEvent{
		SessionID: childID, OldStatus: StatusIdle, NewStatus: StatusRunning,
	})

	// Round 2: genuine completion must be reported too.
	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)
	mgr.emitSessionStateChange(SessionStateChangeEvent{
		SessionID: childID, OldStatus: StatusRunning, NewStatus: StatusIdle,
	})
	require.Eventually(t, func() bool { return len(c.Pending(parentID)) == 2 },
		5*time.Second, 10*time.Millisecond,
		"second idle after running without courier write was never reported")
}

// TestSyntheticSameStatusDoesNotRearmReporting pins that re-adoption's
// same-status events neither re-report nor re-arm idle dedup.
func TestSyntheticSameStatusDoesNotRearmReporting(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)

	mgr := NewManagerWithConfig(ManagerConfig{RepoName: "repo"})
	defer mgr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	unsub := c.Watch(ctx, mgr)
	defer unsub()

	mgr.emitSessionStateChange(SessionStateChangeEvent{
		SessionID: childID, OldStatus: StatusRunning, NewStatus: StatusIdle,
	})
	require.Eventually(t, func() bool { return len(c.Pending(parentID)) == 1 },
		5*time.Second, 10*time.Millisecond)

	// Synthetic same-status idle (re-adoption): must not report again.
	mgr.emitSessionStateChange(SessionStateChangeEvent{
		SessionID: childID, OldStatus: StatusIdle, NewStatus: StatusIdle,
	})
	require.Never(t, func() bool { return len(c.Pending(parentID)) > 1 },
		500*time.Millisecond, 10*time.Millisecond,
		"synthetic idle must not re-report")

	// Synthetic same-status running: must not re-arm (child stays idle after).
	mgr.emitSessionStateChange(SessionStateChangeEvent{
		SessionID: childID, OldStatus: StatusRunning, NewStatus: StatusRunning,
	})
	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)
	mgr.emitSessionStateChange(SessionStateChangeEvent{
		SessionID: childID, OldStatus: StatusRunning, NewStatus: StatusIdle,
	})
	require.Never(t, func() bool { return len(c.Pending(parentID)) > 1 },
		500*time.Millisecond, 10*time.Millisecond,
		"synthetic running must not re-arm reporting")
}

// TestFollowUpToChildRearmsReporting is what makes a conversation possible
// rather than a single exchange. The child's first idle is reported; then the
// parent replies, and the answer to *that* must be reported too, or the parent
// is left polling a child it just spoke to.
func TestFollowUpToChildRearmsReporting(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)

	// Round 1: the child finishes and is reported.
	reportNow(c, target, childID)
	require.Len(t, c.Pending(parentID), 1)

	// The parent replies. The child is idle, so this is written straight away.
	_, err := c.Send(context.Background(), parentID, childID, "round two please", true)
	require.NoError(t, err)

	// Round 2: the child finishes again and must be reported again.
	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)
	reportNow(c, target, childID)

	assert.Len(t, c.Pending(parentID), 2, "the second answer was never reported")
}

// TestUnansweredChildIsStillReportedOnlyOnce keeps the re-arming narrow: with
// no new message, repeated idle transitions stay quiet.
func TestUnansweredChildIsStillReportedOnlyOnce(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)

	child, _ := target.SessionInfo(childID)
	for i := 0; i < 3; i++ {
		c.reportToParent(context.Background(), child)
	}
	assert.Len(t, c.Pending(parentID), 1)
}

// TestQueuedFollowUpRearmsReportingWhenDelivered covers the same rule on the
// deferred path: the re-arm must happen when the message is actually written,
// not when it was queued.
func TestQueuedFollowUpRearmsReportingWhenDelivered(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)

	reportNow(c, target, childID)
	require.Len(t, c.Pending(parentID), 1)

	// The child is busy again, so the parent's reply is held.
	target.setChild(childID, parentID, StatusRunning, RunnerTypeTmux)
	queued, err := c.Send(context.Background(), parentID, childID, "round two", true)
	require.NoError(t, err)
	require.True(t, queued)

	// Still one report: nothing has been delivered to the child yet.
	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)
	reportNow(c, target, childID)
	require.Len(t, c.Pending(parentID), 1, "queueing alone must not re-arm reporting")

	// Now it lands, and the following turn is reportable again.
	c.Drain(context.Background(), childID)
	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)
	reportNow(c, target, childID)
	assert.Len(t, c.Pending(parentID), 2)
}

// TestHeldWriteDoesNotRearmReporting covers the case the assertion in
// TestQueuedFollowUpRearmsReportingWhenDelivered is worded for but cannot reach.
// That test queues by making the recipient StatusRunning, so deliver skips the
// write branch and write is never entered. A HELD write is different: the
// recipient is idle, write runs, captures the pane, finds a draft, and returns
// errComposerBusy having written nothing.
//
// The re-arm must follow what was written, not what was attempted. Nothing was
// written and no turn started, so the child's next idle is the same turn the
// parent already has -- re-arming there delivers the parent a duplicate report,
// and for a tmux child costs another full pane capture and another result file.
// This is steady state, not a rare failure: errComposerBusy repeats every
// retryDelay for as long as the user's draft sits there.
func TestHeldWriteDoesNotRearmReporting(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)
	target.setBackend(childID, ProviderClaude, "claude-opus-5")

	reportNow(c, target, childID)
	require.Len(t, c.Pending(parentID), 1, "precondition: the child's first idle is reported")

	// A user is mid-sentence in the child's composer, so the reply is held.
	target.appendPane("❯ file the dev deprovisioning bug")
	queued, err := c.Send(context.Background(), parentID, childID, "round two", true)
	require.NoError(t, err)
	require.True(t, queued, "precondition: a held delivery stays queued")

	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)
	reportNow(c, target, childID)
	assert.Len(t, c.Pending(parentID), 1,
		"a held write started no turn, so it must not re-arm idle reporting")

	// Holding is not dropping: once the composer clears, the delivery lands and
	// the turn it starts IS reportable again.
	target.setPane("❯ ")
	c.Drain(context.Background(), childID)
	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)
	reportNow(c, target, childID)
	assert.Len(t, c.Pending(parentID), 2,
		"once the write actually lands, the turn it starts is news again")
}

// TestSubmittedWriteMarksSessionRunning pins the fix for a bug that silently
// ended every two-way conversation after one round.
//
// A tmux session's status comes entirely from outside: its agent's notify hook
// reports idleness and nothing reports the opposite. So a session bramble typed
// into stayed "idle" for the whole turn, and the notify that ended that turn hit
// SetSessionIdle's StatusRunning guard and was dropped — no state change, no
// drain, no report. The parent simply never heard back a second time.
func TestSubmittedWriteMarksSessionRunning(t *testing.T) {
	t.Parallel()
	c, target, _ := newTestCourier(t)
	target.set("s1", StatusIdle, RunnerTypeTmux)

	_, err := c.Send(context.Background(), "", "s1", "do the thing", true)
	require.NoError(t, err)

	assert.Equal(t, []SessionID{"s1"}, target.markedRunning)
	info, _ := target.SessionInfo("s1")
	assert.Equal(t, StatusRunning, info.Status)
}

// TestUnsubmittedWriteDoesNotMarkRunning: staging a draft in a pane starts no
// turn, so claiming one would strand the session in "running".
func TestUnsubmittedWriteDoesNotMarkRunning(t *testing.T) {
	t.Parallel()
	c, target, _ := newTestCourier(t)
	target.set("s1", StatusIdle, RunnerTypeTmux)

	_, err := c.Send(context.Background(), "", "s1", "draft", false)
	require.NoError(t, err)

	assert.Empty(t, target.markedRunning)
	info, _ := target.SessionInfo("s1")
	assert.Equal(t, StatusIdle, info.Status)
}

// TestTUIWriteDoesNotMarkRunning: the TUI turn loop sets StatusRunning itself
// when it picks the follow-up off the channel.
func TestTUIWriteDoesNotMarkRunning(t *testing.T) {
	t.Parallel()
	c, target, _ := newTestCourier(t)
	target.set("s1", StatusIdle, RunnerTypeTUI)

	_, err := c.Send(context.Background(), "", "s1", "hello", true)
	require.NoError(t, err)

	assert.Empty(t, target.markedRunning)
}

// TestTwoWayConversationKeepsReporting is the whole feature in one test: a
// child reports, its parent replies, and the answer to that reply is reported
// too. Both fixes are load-bearing here — re-arming the idle report, and
// marking the session running so the turn boundary exists at all.
func TestTwoWayConversationKeepsReporting(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)

	// Round 1: the child answers its opening prompt.
	reportNow(c, target, childID)
	require.Len(t, c.Pending(parentID), 1)

	// The parent replies; the child starts a turn.
	_, err := c.Send(context.Background(), parentID, childID, "round two", true)
	require.NoError(t, err)
	info, _ := target.SessionInfo(childID)
	require.Equal(t, StatusRunning, info.Status)

	// Round 2 ends, and the parent hears about it.
	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)
	reportNow(c, target, childID)

	assert.Len(t, c.Pending(parentID), 2, "the second round was never reported")
}

// TestDroppedPasteIsRetried covers the gap between an agent announcing it is
// idle and its TUI being ready for input. tmux reports success for a paste the
// TUI drops, so without a read-back the message vanishes silently — and the
// session is then marked running for a turn that never began.
func TestDroppedPasteIsRetried(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	// Codex is the provider whose TUI drops pastes, and the only one the
	// courier re-pastes for.
	target.setBackend("s1", ProviderCodex, "gpt-5.4-mini")

	panes := echoPanes(target)
	var pastes int
	panes.echo = func(text string) {
		// The first paste is swallowed, as codex does mid-finalize.
		pastes++
		if pastes > 1 {
			target.appendPane(text)
		}
	}
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	_, err = c.Send(context.Background(), "", "s1", "the real message", true)
	require.NoError(t, err)

	assert.Equal(t, 2, pastes, "a dropped paste should be retried once")
	assert.Contains(t, panes.recorded(), "enter(@7)", "the retry must still be submitted")
	assert.Equal(t, []SessionID{"s1"}, target.markedRunning)
}

// TestPersistentlyDroppedPasteKeepsDeliveryQueued: if the prompt never takes
// the text, the message must stay queued for the next idle rather than being
// reported as delivered — and the session must not be marked running for a
// turn that never started.
func TestPersistentlyDroppedPasteKeepsDeliveryQueued(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusRunning, RunnerTypeTmux)
	target.setBackend("s1", ProviderCodex, "gpt-5.4-mini")

	panes := &fakePanes{} // echo unset: every paste is swallowed
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	_, err = c.Send(context.Background(), "", "s1", "never lands", true)
	require.NoError(t, err)
	require.Len(t, c.Pending("s1"), 1)

	target.set("s1", StatusIdle, RunnerTypeTmux)
	c.Drain(context.Background(), "s1")

	assert.Len(t, c.Pending("s1"), 1, "an undelivered message must stay queued")
	assert.NotContains(t, panes.recorded(), "enter(@7)", "nothing may be submitted")
	assert.Empty(t, target.markedRunning, "no turn started, so none may be claimed")
}

// TestConcurrentSendsToOneRecipientAllPersist covers several subagents
// finishing at once and reporting to the same parent — the normal shape of a
// fan-out, and the only place the queue takes concurrent writes.
//
// In memory this was always safe. On disk it was not: each enqueue took a
// snapshot under the lock and then wrote it *outside* the lock, so a goroutine
// that snapshotted first could write last and put back a queue missing
// everything appended in between. The messages were delivered normally, so the
// loss only surfaced after a restart — the one case the on-disk queue exists
// for.
func TestConcurrentSendsToOneRecipientAllPersist(t *testing.T) {
	t.Parallel()

	const senders = 12
	dir := t.TempDir()
	target := newFakeTarget()
	target.set("parent", StatusRunning, RunnerTypeTmux)

	cfg := testCourierConfigDeliveryDir(t, dir)
	c, err := NewCourier(target, echoPanes(target), cfg)
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < senders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := c.Send(context.Background(), "", "parent", fmt.Sprintf("report-%02d", i), true)
			assert.NoError(t, err)
		}(i)
	}
	wg.Wait()

	require.Len(t, c.Pending("parent"), senders, "in-memory queue lost a report")

	// Reload from disk: this is what a restarted bramble would see.
	reloaded, err := NewCourier(target, echoPanes(target), cfg)
	require.NoError(t, err)
	assert.Lenf(t, reloaded.Pending("parent"), senders,
		"the persisted queue lost reports; a restart would drop them")
}

// TestConcurrentDrainAndSendKeepsQueueConsistent covers the other overlap: a
// parent going idle and draining while more subagents are still reporting to
// it.
func TestConcurrentDrainAndSendKeepsQueueConsistent(t *testing.T) {
	t.Parallel()

	const senders = 12
	dir := t.TempDir()
	target := newFakeTarget()
	target.set("parent", StatusIdle, RunnerTypeTmux)

	cfg := testCourierConfigDeliveryDir(t, dir)
	c, err := NewCourier(target, echoPanes(target), cfg)
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < senders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := c.Send(context.Background(), "", "parent", fmt.Sprintf("report-%02d", i), true)
			assert.NoError(t, err)
		}(i)
	}
	for i := 0; i < senders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			target.set("parent", StatusIdle, RunnerTypeTmux)
			c.Drain(context.Background(), "parent")
		}()
	}
	wg.Wait()

	// Whatever is still queued in memory must match what is on disk: a stale
	// write would leave a restart with a different queue than this process has.
	inMemory := c.Pending("parent")
	reloaded, err := NewCourier(target, echoPanes(target), cfg)
	require.NoError(t, err)
	assert.Lenf(t, reloaded.Pending("parent"), len(inMemory),
		"the persisted queue disagrees with the live one")
}

// TestCursorDeliveryPastesOnceAndSubmits: cursor-agent collapses a bracketed
// paste into a "[Pasted text #N]" chip and never echoes the characters, so
// looking for the text itself can never succeed. Before this was provider-aware
// every cursor delivery pasted twice and then refused to submit.
//
// What fixes cursor is that it is not verified at all — Courier.write checks
// pasteVerifyRequired before it probes, and cursor's entry is required:false —
// NOT that a chip is accepted as proof. Measured: deleting cursor's chipMarkers
// leaves the end-to-end half of this test green and fails only the direct
// pasteConfirmed asserts below, which exercise a branch production never
// reaches for cursor. Those asserts are kept as the pin on the table entry
// itself, which is inert until cursor becomes required:true; the regression
// signal for the shipped cursor fix is TestUnverifiablePasteStillSubmits.
func TestCursorDeliveryPastesOnceAndSubmits(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderCursor, "cursor-composer-2")

	panes := &fakePanes{}
	var pastes int
	panes.echo = func(string) {
		pastes++
		// What cursor actually shows: a chip standing in for the content.
		target.appendPane("[Pasted text #1 +12 lines]")
	}
	panes.onSubmit = func() { target.appendPane("> ") }

	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	_, err = c.Send(context.Background(), "", "s1", "the report body", true)
	require.NoError(t, err)

	assert.Equal(t, 1, pastes, "a chip is proof enough; the text must not be pasted twice")
	assert.Contains(t, panes.recorded(), "enter(@7)", "the delivery must be submitted")
	assert.Equal(t, []SessionID{"s1"}, target.markedRunning)
	assert.Empty(t, c.Pending("s1"), "a delivered message must leave the queue")

	// Below this line the subject changes: the delivery above succeeded because
	// cursor is not verified, and these two asserts pin the table entry rather
	// than the delivery. They are the only thing that would notice cursor's
	// chipMarkers being dropped, and they matter for the day cursor becomes
	// required:true — at which point scanning for the pasted characters could
	// never succeed and only the chip would confirm.
	assert.True(t, pasteConfirmed(ProviderCursor,
		[]string{"[Pasted text #1 +12 lines]"}, "the report body", pasteProbe("the report body")),
		"a chip stands in for the pasted text")
	assert.False(t, pasteConfirmed(ProviderCodex,
		[]string{"[Pasted text #1 +12 lines]"}, "the report body", pasteProbe("the report body")),
		"a chip is cursor's chrome; codex must not accept it as evidence")
}

// TestUnverifiablePasteStillSubmits is the regression test for the reported
// bug. tmux reported the paste succeeded; a pane we cannot read back is
// silence, not a negative. Re-pasting on silence is what put the message in
// the composer twice and then never submitted it.
func TestUnverifiablePasteStillSubmits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		backend string
		model   string
	}{
		{"cursor renders nothing we can match", ProviderCursor, "cursor-composer-2"},
		{"unresolvable model has no known chrome", "", "not-a-real-model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			target := newFakeTarget()
			target.set("s1", StatusIdle, RunnerTypeTmux)
			if tc.backend != "" || tc.model != "" {
				target.setBackend("s1", tc.backend, tc.model)
			}

			panes := &fakePanes{} // echo unset: the pane never shows the text
			c, err := NewCourier(target, panes, testCourierConfig(t))
			require.NoError(t, err)

			_, err = c.Send(context.Background(), "", "s1", "never echoed", true)
			require.NoError(t, err)

			assert.Equal(t, 1, panes.pasteCount(), "one paste only — never double-paste on silence")
			assert.Contains(t, panes.recorded(), "enter(@7)", "the message must still be submitted")
			assert.Empty(t, c.Pending("s1"), "the delivery must not stay queued")
		})
	}
}

// TestDeliveryHeldWhileComposerHasDraft: a paste appends to whatever the user
// is typing, and the Enter that follows submits their half-written sentence
// wearing the delivered text. The idle probe cannot catch this on its own —
// a composer holding a draft still reads as idle.
func TestDeliveryHeldWhileComposerHasDraft(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderClaude, "claude-opus-5")
	// A real claude composer mid-sentence. The glyph is followed by U+00A0.
	target.appendPane("❯ file the dev deprovisioning bug")

	panes := &fakePanes{}
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	_, err = c.Send(context.Background(), "", "s1", "a report from a subagent", true)
	require.NoError(t, err)

	assert.Zero(t, panes.pasteCount(), "nothing may be pasted into a draft")
	assert.NotContains(t, panes.recorded(), "enter(@7)", "the user's draft must not be submitted")
	assert.Len(t, c.Pending("s1"), 1, "the message stays queued for the next transition")
	assert.Empty(t, target.markedRunning, "no turn started")
}

// TestDeliveryProceedsOnceDraftIsCleared: holding is not dropping. Once the
// composer is empty the queued message goes out on the next drain.
func TestDeliveryProceedsOnceDraftIsCleared(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderClaude, "claude-opus-5")
	target.appendPane("❯ half a thought")

	panes := echoPanes(target)
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	_, err = c.Send(context.Background(), "", "s1", "queued while typing", true)
	require.NoError(t, err)
	require.Len(t, c.Pending("s1"), 1, "held while the draft was there")

	// The user submits or clears the line: the composer is empty again.
	target.appendPane("❯ ")
	c.Drain(context.Background(), "s1")

	assert.Contains(t, panes.recorded(), "enter(@7)", "delivery resumes once the composer is clear")
	assert.Empty(t, c.Pending("s1"), "the queue drains")
}

// TestComposerBodyIsUnicodeAwareAtEveryReader pins the byte that decides
// whether any mail is ever delivered to a claude session, at the one helper all
// three composer readers share. The prompt glyph is followed by a non-breaking
// space (U+00A0), so trimming only ordinary spaces leaves " " behind.
//
// Testing composerBody rather than each caller is the point: the same regression
// breaks the three readers in three different directions — judgeComposerLine
// reports a draft on every empty composer and holds back every delivery on the
// host, composerHoldsThisDelivery stops recognizing bramble's own staged text,
// and confirmsComposer stops confirming a landed paste and drives write() into a
// second paste plus a re-queue. Before the extraction each reader re-derived the
// body inline and only this file's caller was covered, so an ASCII cutset in
// either of the other two passed the whole suite.
func TestComposerBodyIsUnicodeAwareAtEveryReader(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		"❯ ", // real capture: glyph + NBSP
		"❯ ", // glyph + ordinary space
		"❯",  // bare glyph
	} {
		body, hasGlyph := composerBody(line)
		assert.True(t, hasGlyph, "the glyph is present: %q", line)
		assert.Equal(t, "", body, "an empty composer has an empty body: %q", line)
	}
	for _, line := range []string{
		"❯ real text",
		"❯ real text",
	} {
		body, hasGlyph := composerBody(line)
		assert.True(t, hasGlyph, "%q", line)
		assert.Equal(t, "real text", body, "the separator is not part of the body: %q", line)
	}
	// No glyph is not a composer at all; judgeComposerLine turns this into
	// known=false rather than into a verdict.
	if _, hasGlyph := composerBody("  Add a follow-up"); hasGlyph {
		t.Fatal("a line without the glyph must not report one")
	}
}

// TestEmptyClaudeComposerIsNotADraft keeps the caller-level assertion beside the
// helper-level one: composerBody is only correct if the verdict it feeds is.
func TestEmptyClaudeComposerIsNotADraft(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		"❯ ", // real capture: glyph + NBSP
		"❯ ", // glyph + ordinary space
		"❯",  // bare glyph
	} {
		draft, known := composerDraft(ProviderClaude, []string{line})
		assert.True(t, known, "a claude composer is readable: %q", line)
		assert.False(t, draft, "an empty composer is not a draft: %q", line)
	}

	// Both spellings of the separator, so neither trim can regress to an
	// ASCII-only cutset without a test noticing.
	for _, line := range []string{
		"❯ real text", // ordinary space
		"❯ real text", // NBSP, as the real TUI writes it
	} {
		draft, known := composerDraft(ProviderClaude, []string{line})
		assert.True(t, known, "%q", line)
		assert.True(t, draft, "typed text is a draft: %q", line)
	}
}

// TestComposerDraftUnknownForUnreadableProviders: cursor and codex render
// placeholder text that disappears the moment the user types, so a draft is
// indistinguishable from a CLI that has not finished booting. Unknown must mean
// "deliver anyway" — refusing to write to every pane we cannot parse would
// strand mail rather than protect it.
func TestComposerDraftUnknownForUnreadableProviders(t *testing.T) {
	t.Parallel()
	for _, provider := range []string{ProviderCursor, ProviderCodex, ""} {
		_, known := composerDraft(provider, []string{"  → Add a follow-up"})
		assert.False(t, known, "provider %q composer is not judged", provider)
	}
}

// TestDraftHoldIsBoundedByElapsedTime pins elapsed time on an unchanged draft.
// Counting write attempts measures how often bramble looked at the pane, not
// how long the draft sat there.
func TestDraftHoldIsBoundedByElapsedTime(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderClaude, "claude-opus-5")
	target.appendPane("❯ a half-typed line whose author has gone home")

	panes := echoPanes(target)
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	now := time.Now()
	c.now = func() time.Time { return now }

	_, err = c.Send(context.Background(), "", "s1", "a report from a subagent", true)
	require.NoError(t, err)
	require.Len(t, c.Pending("s1"), 1, "the first attempt holds")
	require.Zero(t, panes.pasteCount(), "nothing is pasted while the hold stands")

	// Any number of attempts inside the grace period must keep holding: the
	// bound is time, not attempts.
	for i := 0; i < 50; i++ {
		c.Drain(context.Background(), "s1")
	}
	require.Zero(t, panes.pasteCount(), "50 attempts inside the grace period must not release")
	require.Len(t, c.Pending("s1"), 1)

	// Past the grace period the delivery still holds. tmux paste-buffer appends,
	// so pressing Enter would submit the report joined to the human's draft.
	// maxDeliveryAge prunes only at process start and deletes rather than
	// delivers, so it is not a running-process backstop. A paste chip cannot
	// answer whose paste it is; see TestAnUnownedChipIsNeitherPastedOverNorSubmitted.
	now = now.Add(composerHoldGrace + time.Second)
	c.Drain(context.Background(), "s1")

	assert.Zero(t, panes.pasteCount(),
		"a human's draft is never pasted over, however long it has sat there")
	assert.Len(t, c.Pending("s1"), 1, "the delivery stays queued and keeps retrying")
}

// TestActivelyEditedDraftHoldsIndefinitely: a changing draft means somebody is
// at the keyboard, and a drafter who is present should be waited for, not
// raced. This is the case a call-counting bound got wrong — a wrapped draft's
// continuation lines are not chrome, so every keystroke flips contentChanged,
// revives the session, and would have burned one attempt from the budget.
func TestActivelyEditedDraftHoldsIndefinitely(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderClaude, "claude-opus-5")
	target.appendPane("❯ file the")

	panes := echoPanes(target)
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	now := time.Now()
	c.now = func() time.Time { return now }

	_, err = c.Send(context.Background(), "", "s1", "a report from a subagent", true)
	require.NoError(t, err)
	require.Len(t, c.Pending("s1"), 1)

	// Someone keeps typing, well past the grace period in total.
	for _, draft := range []string{
		"❯ file the dev",
		"❯ file the dev deprovisioning",
		"❯ file the dev deprovisioning bug",
	} {
		now = now.Add(composerHoldGrace)
		target.appendPane(draft)
		c.Drain(context.Background(), "s1")
	}

	assert.Zero(t, panes.pasteCount(),
		"a draft being actively edited must never be delivered over")
	assert.Len(t, c.Pending("s1"), 1, "the delivery stays queued while the user types")
}

// TestDraftHoldResetsWhenTheComposerClears: the grace applies to a single
// uninterrupted draft, not to a session's lifetime.
func TestDraftHoldResetsWhenTheComposerClears(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderClaude, "claude-opus-5")
	target.appendPane("❯ typing")

	panes := echoPanes(target)
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	_, err = c.Send(context.Background(), "", "s1", "first", true)
	require.NoError(t, err)
	require.Len(t, c.Pending("s1"), 1)

	// The user finishes the line; the delivery lands normally.
	target.appendPane("❯ ")
	c.Drain(context.Background(), "s1")
	require.Empty(t, c.Pending("s1"), "the delivery went out on a clear composer")

	// A fresh draft is held again, with the full grace period.
	target.appendPane("❯ typing again")
	_, err = c.Send(context.Background(), "", "s1", "second", true)
	require.NoError(t, err)
	assert.Len(t, c.Pending("s1"), 1, "a new draft is held, not delivered over")
}

// TestBrambleOwnStagedDeliveryIsOverwritten: a composer holding a message
// bramble itself staged is not a human draft. Nothing but a keypress clears a
// composer, so holding for it waits for someone who is not coming and every
// later delivery queues behind it forever.
//
// Ownership is decided by matching what is actually queued for the recipient,
// not by the "[bramble]" prefix — a user can type that themselves, and their
// draft must still be protected.
func TestBrambleOwnStagedDeliveryIsOverwritten(t *testing.T) {
	t.Parallel()

	t.Run("our own staged delivery does not hold the queue", func(t *testing.T) {
		t.Parallel()
		target := newFakeTarget()
		target.set("s1", StatusIdle, RunnerTypeTmux)
		target.setBackend("s1", ProviderClaude, "claude-opus-5")

		panes := echoPanes(target)
		c, err := NewCourier(target, panes, testCourierConfig(t))
		require.NoError(t, err)

		staged := subagentReportPrefix + " subagent child-1 (planner, opus) is idle"
		// The message is queued and its text is sitting in the composer, as it
		// would be after a --queue send that never pressed Enter.
		_, err = c.Send(context.Background(), "", "s1", staged, true)
		require.NoError(t, err)
		target.appendPane("❯ " + staged)
		c.Drain(context.Background(), "s1")

		assert.Empty(t, c.Pending("s1"), "bramble's own staged text must not wedge its queue")
	})

	t.Run("a user draft wearing the prefix is still protected", func(t *testing.T) {
		t.Parallel()
		target := newFakeTarget()
		target.set("s1", StatusIdle, RunnerTypeTmux)
		target.setBackend("s1", ProviderClaude, "claude-opus-5")
		// Someone typed the prefix themselves; nothing matching it is queued.
		target.appendPane("❯ " + subagentReportPrefix + " I was about to ask about")

		panes := echoPanes(target)
		c, err := NewCourier(target, panes, testCourierConfig(t))
		require.NoError(t, err)

		_, err = c.Send(context.Background(), "", "s1", "a report from a subagent", true)
		require.NoError(t, err)

		assert.Zero(t, panes.pasteCount(),
			"a user draft must be protected even when it wears bramble's prefix")
		assert.Len(t, c.Pending("s1"), 1, "the delivery stays queued")
	})
}

// TestOwnDeliveryOverwriteIsNotLoggedAsAGraceExpiry: the grace-period warning
// says a human's unchanged draft was typed over after composerHoldGrace. When
// bramble overwrites its own staged text, none of that happened — no draft was
// held and no grace elapsed — so borrowing that line tells an operator the
// opposite of the truth, inverting the distinct diagnostic this PR added.
func TestOwnDeliveryOverwriteIsNotLoggedAsAGraceExpiry(t *testing.T) {
	// Not parallel: it captures the shared standard logger.
	var logs bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })

	target := newFakeTarget()
	// Busy, so the first Send only queues: the delivery must still be pending
	// when the staged text is already sitting in the composer, which is the
	// state the own-delivery branch exists for.
	target.set("s1", StatusRunning, RunnerTypeTmux)
	target.setBackend("s1", ProviderClaude, "claude-opus-5")

	panes := &fakePanes{}
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	staged := subagentReportPrefix + " subagent child-1 (planner, opus) is idle"
	queued, err := c.Send(context.Background(), "", "s1", staged, true)
	require.NoError(t, err)
	require.True(t, queued, "precondition: the delivery is queued, not written")

	// The same text is in the composer — a --queue send that never pressed
	// Enter — and the session is now idle, so the drain attempts a write.
	target.appendPane("❯ " + staged)
	target.set("s1", StatusIdle, RunnerTypeTmux)
	c.Drain(context.Background(), "s1")

	// The delivery goes out without a second paste — the text is already in the
	// composer, and tmux paste-buffer appends (see
	// TestStagedDeliveryIsSubmittedNotRePasted). What matters here is that it
	// was not held.
	require.Contains(t, panes.recorded(), "enter(@7)",
		"precondition: bramble's own staged text must be submitted, not held")
	require.Empty(t, c.Pending("s1"), "precondition: and must leave the queue")
	assert.NotContains(t, logs.String(), "past the grace period",
		"overwriting bramble's own text must not claim a human draft was typed over")
}

// TestClaudePasteIsVerified: claude's composer echoes a pasted message
// verbatim — measured across 13 live panes, none of which rendered a
// "[Pasted text …]" chip — so paste evidence is readable for it and must be
// required.
//
// This is the same composer composerDraftText reads: the draft protection in
// this change rests on claude's composer being legible, so declining to check
// it would be the two tables contradicting each other. A paste claude's TUI
// dropped would otherwise go unnoticed, because deliver() writes directly
// rather than queueing — the message is lost and MarkRunning wedges the session
// on a turn that never started.
func TestClaudePasteIsVerified(t *testing.T) {
	t.Parallel()

	require.True(t, pasteVerifyRequired(ProviderClaude),
		"claude's composer is readable, so its paste must be confirmed before Enter")

	t.Run("an echoed paste is submitted once", func(t *testing.T) {
		t.Parallel()
		target := newFakeTarget()
		target.set("s1", StatusIdle, RunnerTypeTmux)
		target.setBackend("s1", ProviderClaude, "claude-opus-5")

		panes := echoPanes(target) // claude echoes, as the live panes show
		c, err := NewCourier(target, panes, testCourierConfig(t))
		require.NoError(t, err)

		_, err = c.Send(context.Background(), "", "s1", "a report from a subagent", true)
		require.NoError(t, err)

		assert.Equal(t, 1, panes.pasteCount(), "a confirmed paste is never repeated")
		assert.Contains(t, panes.recorded(), "enter(@7)", "and is submitted")
		assert.Empty(t, c.Pending("s1"))
	})

	t.Run("a paste that never lands is queued, not submitted", func(t *testing.T) {
		t.Parallel()
		target := newFakeTarget()
		target.set("s1", StatusIdle, RunnerTypeTmux)
		target.setBackend("s1", ProviderClaude, "claude-opus-5")

		panes := &fakePanes{} // the pane never shows the text: the TUI dropped it
		c, err := NewCourier(target, panes, testCourierConfig(t))
		require.NoError(t, err)

		_, err = c.Send(context.Background(), "", "s1", "a report from a subagent", true)
		require.NoError(t, err)

		assert.NotContains(t, panes.recorded(), "enter(@7)",
			"never press Enter on a paste that did not arrive — that starts a turn with no prompt")
		assert.Len(t, c.Pending("s1"), 1, "the delivery stays queued for the next transition")
	})
}

// TestUnreadableComposerCostsNoPaneCapture: composerDraftText returns unknown
// for every provider but claude, so capturing the pane first made each codex,
// cursor and unresolved-model delivery pay a tmux round-trip for an answer that
// is thrown away — the same waste the pasteVerifyRequired ordering avoids, and
// it widens the same paste-to-Enter window.
func TestUnreadableComposerCostsNoPaneCapture(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderCursor, "cursor-composer-2")

	panes := echoPanes(target)
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	_, err = c.Send(context.Background(), "", "s1", "a report from a subagent", true)
	require.NoError(t, err)

	// Cursor's composer cannot be read and its paste evidence is not required,
	// so neither of those checks may reach tmux.
	require.False(t, composerReadable(ProviderCursor), "precondition")
	require.False(t, pasteVerifyRequired(ProviderCursor), "precondition")
	// One capture total, however many questions are asked of it. Cursor does
	// have an idle probe — the only thing standing between a delivery and a
	// live cursor turn, since cursor reports no turn ends at all and
	// info.Status can say idle for a session that is running — so the pane is
	// read once and every reader shares that frame. Two captures would both
	// cost a second round-trip and let the readers disagree about the pane.
	require.True(t, providerHasIdleProbe(ProviderCursor), "precondition")
	assert.Equal(t, 1, target.captures(),
		"the pane is captured once and shared; the discarded checks add nothing")
}

// TestNothingReadablePaysNoPaneCapture: a provider with neither an idle probe
// nor a readable composer has no question worth asking of its pane, so it must
// not pay a tmux round-trip for two verdicts that are both discarded.
func TestNothingReadablePaysNoPaneCapture(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", "", "some-unknown-model")

	panes := echoPanes(target)
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	_, err = c.Send(context.Background(), "", "s1", "a report from a subagent", true)
	require.NoError(t, err)

	require.False(t, providerHasIdleProbe(""), "precondition")
	require.False(t, composerReadable(""), "precondition")
	assert.Zero(t, target.captures(),
		"no pane capture may be made when nothing can read the pane")
	assert.Contains(t, panes.recorded(), "enter(@7)", "and the delivery still goes out")
}

// TestStagedDeliveryIsSubmittedNotRePasted: tmux paste-buffer APPENDS into the
// composer — PaneWriter.Paste is set-buffer + paste-buffer with no clearing
// step. So when an earlier attempt already pasted this message and then failed
// before pressing Enter, pasting again would leave two copies and submit both
// as one prompt: the double-paste symptom this change set out to remove,
// re-created on the failure path.
//
// The state is reachable through failure paths this PR itself creates — a
// SendEnter error requeues with the text still staged, and claude's now-required
// paste verification requeues after a re-paste.
func TestStagedDeliveryIsSubmittedNotRePasted(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderClaude, "claude-opus-5")

	staged := subagentReportPrefix + " subagent child-1 (planner, opus) is idle"
	// The message is already in the composer from a previous attempt.
	target.appendPane("❯ " + staged)

	panes := echoPanes(target)
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	_, err = c.Send(context.Background(), "", "s1", staged, true)
	require.NoError(t, err)

	assert.Zero(t, panes.pasteCount(),
		"the message is already staged; pasting again would submit it twice in one prompt")
	assert.Contains(t, panes.recorded(), "enter(@7)", "it must still be submitted")
	assert.Empty(t, c.Pending("s1"), "and leave the queue")
}

// TestADifferentStagedMessageStillHolds: a composer holding a DIFFERENT bramble
// message is still a composer that must not be pasted into — appending would
// submit both messages as a single prompt. Only the message about to be written
// may skip the paste.
func TestADifferentStagedMessageStillHolds(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderClaude, "claude-opus-5")
	target.appendPane("❯ " + subagentReportPrefix + " subagent OTHER-child is idle")

	panes := echoPanes(target)
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	_, err = c.Send(context.Background(), "", "s1",
		subagentReportPrefix+" subagent child-1 is idle", true)
	require.NoError(t, err)

	assert.Zero(t, panes.pasteCount(),
		"a different staged message must not be appended to")
	assert.Len(t, c.Pending("s1"), 1, "the delivery is held for the next transition")
}

// TestStagedDeliveryIsNotAppendedByPasteVerification pins that alreadyStaged
// covers both the first paste and the paste-verification retry. Otherwise
// <staged copy><second copy> is submitted as one prompt.
//
// The two predicates disagree most cleanly when the pane shows less of the
// message than pasteProbeLen: composerHoldsThisDelivery matches on a prefix in
// either direction, while the probe wants a fixed 24 characters.
func TestStagedDeliveryIsNotAppendedByPasteVerification(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderClaude, "claude-opus-5")

	// A short prefix of the message, as a narrow pane would show it.
	target.appendPane(claudeComposerPane(subagentReportPrefix + " sub"))

	panes := &fakePanes{} // no echo: the pane keeps showing only that prefix
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	full := subagentReportPrefix + " subagent child-1 (planner, opus) is idle"
	_, err = c.Send(context.Background(), "", "s1", full, true)
	require.NoError(t, err)

	assert.Zero(t, panes.pasteCount(),
		"verification must not append a second copy of a message already staged")
}

// TestPlainQueuedMessageIsNotDuplicated: a queued CLI message carries no
// "[bramble]" prefix, so requiring one classified it as a human draft — the
// retry after a failed SendEnter then pasted it again and submitted a
// duplicate. Ownership is decided by matching the queued text, not a prefix.
func TestPlainQueuedMessageIsNotDuplicated(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderClaude, "claude-opus-5")
	target.appendPane(claudeComposerPane("hello"))

	panes := &fakePanes{}
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	_, err = c.Send(context.Background(), "", "s1", "hello", true)
	require.NoError(t, err)

	assert.Zero(t, panes.pasteCount(),
		"a plain queued message already in the composer must not be pasted again")
	// Both halves matter: requiring the "[bramble]" prefix also leaves
	// pasteCount at zero, but by classifying the message as a HUMAN draft and
	// holding it — so the queue never drains. Assert it was actually delivered.
	assert.Contains(t, panes.recorded(), "enter(@7)", "it must be submitted, not held")
	assert.Empty(t, c.Pending("s1"), "and must leave the queue")
}

// TestUserPasteIsNotMistakenForOurOwnStagedDelivery pins the ownership rule
// that decides whether a non-empty composer gets Enter pressed on it.
//
// A "[Pasted text #N]" chip is what claude renders for ANY paste, including one
// the USER made and has not yet submitted. Accepting a chip as proof that
// bramble staged the delivery meant that user's block was submitted for them —
// the exact harm the draft hold exists to prevent — while the delivery itself
// was never written yet was dropped from the queue as though it had been.
//
// The only thing that can answer "did bramble put this here" is bramble's own
// record of having pasted it, which this courier has not.
func TestUserPasteIsNotMistakenForOurOwnStagedDelivery(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderClaude, "claude-opus-5")
	// The user pasted something and has not hit Enter.
	target.appendPane(claudeComposerPane("[Pasted text #3 +45 lines]"))

	panes := &fakePanes{}
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	_, err = c.Send(context.Background(), "", "s1", "a report from a subagent", true)
	require.NoError(t, err)

	assert.NotContains(t, panes.recorded(), "enter(@7)",
		"a chip is not proof the composer is ours; submitting it sends the user's paste")
	assert.Len(t, c.Pending("s1"), 1,
		"and the delivery must stay queued rather than be dropped as delivered")
}

// TestShortDraftPrefixingTheMessageIsNotOurs is the same rule in miniature: a
// one-sided prefix match let a short typed line that happens to begin the
// message pass as bramble's own staged text.
func TestShortDraftPrefixingTheMessageIsNotOurs(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderClaude, "claude-opus-5")
	target.appendPane(claudeComposerPane("a report"))

	panes := &fakePanes{}
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	_, err = c.Send(context.Background(), "", "s1", "a report from a subagent", true)
	require.NoError(t, err)

	assert.NotContains(t, panes.recorded(), "enter(@7)",
		"a typed line that merely prefixes the message must not be submitted")
	assert.Len(t, c.Pending("s1"), 1, "the delivery stays queued")
}

// TestPaneShowingAWorkingTurnHoldsTheDelivery covers the loop seen in
// production: bramble's bookkeeping said idle (codex's notify hook fires ahead
// of its prompt being ready), so a delivery was pasted into a live turn, the
// TUI discarded it, verification failed, and the pair repeated every retryDelay
// for as long as the turn ran — one "paste did not reach ...'s prompt" warning
// per retry, per stuck session.
//
// The recipient's own pane is the authority on whether its turn is over.
func TestPaneShowingAWorkingTurnHoldsTheDelivery(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux) // bookkeeping says idle...
	target.setBackend("s1", ProviderCodex, "gpt-5.6-terra")
	// ...but codex's own pane says otherwise.
	target.appendPane(strings.Join([]string{
		"• Ran 3 commands · ctrl + t to view transcript",
		"◦ Working (28s • esc to interrupt)",
		"",
		"› Ask Codex to do anything",
	}, "\n"))

	panes := &fakePanes{}
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	_, err = c.Send(context.Background(), "", "s1", "a report from a subagent", true)
	require.NoError(t, err)

	assert.Zero(t, panes.pasteCount(),
		"nothing may be pasted into a pane whose CLI says a turn is in flight")
	assert.Len(t, c.Pending("s1"), 1, "the delivery rides the next idle transition")
}

// TestUnknownPaneStillDelivers is the other half of that rule. Only a POSITIVE
// working verdict holds; refusing to deliver into every pane bramble cannot
// read would strand mail, which is the failure class this PR exists to close.
func TestUnknownPaneStillDelivers(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderCodex, "gpt-5.6-terra")
	// No codex chrome at all: the pane says nothing either way.
	target.appendPane("some unrelated output")

	panes := echoPanes(target)
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	_, err = c.Send(context.Background(), "", "s1", "a report from a subagent", true)
	require.NoError(t, err)

	assert.Contains(t, panes.recorded(), "enter(@7)", "an unreadable pane must not wedge the queue")
	assert.Empty(t, c.Pending("s1"))
}

// TestStagedRecordDoesNotVouchForAChangedComposer: the staged record proves
// bramble PASTED the text, never that the composer still HOLDS it. A composer
// is editable between a failed attempt and its retry, so a user who clears it
// and pastes their own block inside the retry window must not have Enter
// pressed on their paste.
//
// A chip was the case that made this concrete: it is the rendering of *a*
// paste, so accepting one on the strength of the record alone submitted
// whatever the user had put there and dropped the delivery as delivered.
func TestStagedRecordDoesNotVouchForAChangedComposer(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderClaude, "claude-opus-5")

	text := "a report from a subagent"
	panes := &fakePanes{}
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	// An earlier attempt pasted the text and could not submit it.
	c.noteStaged("s1", text)
	// The user has since replaced the composer contents with their own paste.
	target.appendPane(claudeComposerPane("[Pasted text #7 +90 lines]"))

	_, err = c.Send(context.Background(), "", "s1", text, true)
	require.NoError(t, err)

	assert.NotContains(t, panes.recorded(), "enter(@7)",
		"the record must not authorize submitting text the composer no longer holds")
	assert.Len(t, c.Pending("s1"), 1, "and the delivery stays queued")
}

// TestPaneHoldIsBoundedByElapsedTime: the working verdict is one frame's, and a
// frame can be wrong in a way that never corrects itself. claudeLineVerdict
// reports work for a `●` tool line with no sparkle below it — what an
// interrupted turn leaves on screen — and spinnerRe matches any line opening
// "* " or "· ", so echoed content can read as a spinner. A pane that is static
// because the session is idle never changes its mind.
//
// Unbounded, that is the "parent's mail never drains" failure this PR exists to
// close, re-entered through a different door. Bounded, it costs latency.
func TestPaneHoldIsBoundedByElapsedTime(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderCodex, "gpt-5.6-terra")
	// A pane stuck reporting work, which never changes because nothing is
	// running to change it.
	target.appendPane(strings.Join([]string{
		"◦ Working (28s • esc to interrupt)",
		"",
		"› Ask Codex to do anything",
	}, "\n"))

	panes := echoPanes(target)
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	now := time.Now()
	c.now = func() time.Time { return now }

	_, err = c.Send(context.Background(), "", "s1", "a report from a subagent", true)
	require.NoError(t, err)
	require.Len(t, c.Pending("s1"), 1, "precondition: the first verdict holds the delivery")
	require.Zero(t, panes.pasteCount(), "precondition: nothing was written")

	// Still inside the grace period: still held.
	now = now.Add(paneHoldGrace - time.Second)
	c.Drain(context.Background(), "s1")
	assert.Len(t, c.Pending("s1"), 1, "a hold inside the grace period stands")

	// Past it: the verdict has been unchanged too long to be a real turn.
	now = now.Add(2 * time.Second)
	c.Drain(context.Background(), "s1")
	assert.Empty(t, c.Pending("s1"),
		"a pane stuck on working must not strand mail for the life of the process")
}

// TestPaneHoldResetsWhenTheTurnEnds: the grace period covers one uninterrupted
// run of working verdicts, not the session's lifetime. A turn that ends and a
// later one that starts must each get the full budget, or a busy session would
// accumulate its way to the bound and deliver into a live turn.
func TestPaneHoldResetsWhenTheTurnEnds(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderCodex, "gpt-5.6-terra")
	working := "◦ Working (28s • esc to interrupt)\n\n› Ask Codex to do anything"
	target.appendPane(working)

	panes := echoPanes(target)
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)
	now := time.Now()
	c.now = func() time.Time { return now }

	_, err = c.Send(context.Background(), "", "s1", "a report from a subagent", true)
	require.NoError(t, err)
	require.Len(t, c.Pending("s1"), 1, "precondition: held")

	// The turn ends, the delivery goes out, and a NEW turn starts.
	now = now.Add(paneHoldGrace - time.Second)
	target.setPane("› Ask Codex to do anything")
	target.set("s1", StatusIdle, RunnerTypeTmux)
	c.Drain(context.Background(), "s1")
	require.Empty(t, c.Pending("s1"), "precondition: an idle pane delivers")

	target.setPane(working)
	target.set("s1", StatusIdle, RunnerTypeTmux)
	_, err = c.Send(context.Background(), "", "s1", "a second report", true)
	require.NoError(t, err)
	require.Len(t, c.Pending("s1"), 1, "precondition: the new turn holds")

	// Only a second past the FIRST hold's start. If the clock had not been
	// reset by the intervening idle pane, this would deliver into a live turn.
	now = now.Add(2 * time.Second)
	c.Drain(context.Background(), "s1")
	assert.Len(t, c.Pending("s1"), 1,
		"each uninterrupted run of working verdicts gets the full grace period")
}

// TestALongRunningTurnIsNeverReleasedByTheGrace: the pane hold's bound is on
// STALENESS, not on turn length. A real turn repaints — its elapsed timer moves
// every second — so however long it runs it keeps restarting the clock and is
// never released. Only a pane that has stopped changing expires, which is the
// shape of a false positive rather than of a long turn.
//
// Without that distinction the grace period would eventually deliver into a
// live turn, which is the harm the gate exists to prevent.
func TestALongRunningTurnIsNeverReleasedByTheGrace(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderCodex, "gpt-5.6-terra")
	working := func(elapsed int) string {
		return fmt.Sprintf("◦ Working (%ds • esc to interrupt)\n\n› Ask Codex to do anything", elapsed)
	}
	target.setPane(working(1))

	panes := echoPanes(target)
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)
	now := time.Now()
	c.now = func() time.Time { return now }

	_, err = c.Send(context.Background(), "", "s1", "a report from a subagent", true)
	require.NoError(t, err)
	require.Len(t, c.Pending("s1"), 1, "precondition: the turn holds the delivery")

	// Run far past the grace period, repainting as a real turn does.
	for i := 0; i < 10; i++ {
		now = now.Add(paneHoldGrace / 2)
		target.setPane(working(i + 2))
		target.set("s1", StatusIdle, RunnerTypeTmux)
		c.Drain(context.Background(), "s1")
	}

	assert.Len(t, c.Pending("s1"), 1,
		"a turn that keeps repainting is still working, however long it runs")
	assert.Zero(t, panes.pasteCount(), "and nothing may be written into it")
}

// TestTextTypedOntoOurStagedLineIsNotSubmitted: the record widens an exact
// match to a prefix match in ONE direction only. A composer showing less than
// we pasted is a capture truncated at the pane width, which is consistent with
// our paste and nothing else. A composer showing our line AND MORE is our text
// with something typed onto the end of it — submitting that sends the user's
// edit while the delivery is dropped as delivered.
func TestTextTypedOntoOurStagedLineIsNotSubmitted(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderClaude, "claude-opus-5")

	text := "a report from a subagent"
	panes := &fakePanes{}
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	// An earlier attempt pasted the text and could not submit it; the user has
	// since typed onto the end of the line.
	c.noteStaged("s1", text)
	target.setPane(claudeComposerPane(text + " and my own question"))

	_, err = c.Send(context.Background(), "", "s1", text, true)
	require.NoError(t, err)

	assert.NotContains(t, panes.recorded(), "enter(@7)",
		"our line with the user's text appended is their draft, not our staged delivery")
	assert.Len(t, c.Pending("s1"), 1, "the delivery stays queued")
}

// TestTruncatedCaptureOfOurStagedLineStillCounts is the direction that IS safe,
// pinned so the fix above cannot be tightened into never matching at all: a
// capture stops at the pane width, so a composer showing a prefix of what we
// pasted is that paste seen through a narrow pane.
func TestTruncatedCaptureOfOurStagedLineStillCounts(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderClaude, "claude-opus-5")

	text := "a report from a subagent that runs past the width of this pane"
	panes := &fakePanes{}
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	c.noteStaged("s1", text)
	target.setPane(claudeComposerPane("a report from a subagent that runs"))

	_, err = c.Send(context.Background(), "", "s1", text, true)
	require.NoError(t, err)

	assert.Zero(t, panes.pasteCount(),
		"the text is already staged; pasting again would submit it twice in one prompt")
	assert.Contains(t, panes.recorded(), "enter(@7)", "it must still be submitted")
	assert.Empty(t, c.Pending("s1"))
}

// TestAChippedPasteLeavesNoStagedRecord: the staged record is only worth
// keeping if a retry could RECOGNIZE the text in the composer, and a chipped
// paste can never be matched by any text comparison — claude collapses a large
// enough paste to "[Pasted text #N]", and a chip is what ANY paste looks like.
//
// Kept anyway, such a record would vouch for whatever the chip turned out to
// be. Dropped, the retry reads an unidentified draft and holds, which costs
// composerHoldGrace instead of pasting the message on top of its own chip and
// submitting both in one prompt — the double-paste symptom section 1 removes.
func TestAChippedPasteLeavesNoStagedRecord(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderClaude, "claude-opus-5")

	text := "a report from a subagent"
	panes := &fakePanes{}
	// The CLI chips the paste rather than echoing it, and Enter then fails.
	panes.echo = func(string) { target.setPane(claudeComposerPane("[Pasted text #1 +80 lines]")) }
	panes.enterErr = errors.New("send-keys failed")

	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	_, err = c.Send(context.Background(), "", "s1", text, true)
	require.NoError(t, err, "Send queues; the write failure surfaces as a retained queue")
	require.Contains(t, panes.recorded(), "enter(@7)", "precondition: Enter was attempted")
	require.Len(t, c.Pending("s1"), 1, "precondition: the failed Enter left the delivery queued")

	assert.Empty(t, c.stagedText("s1"),
		"a record no later comparison could use must not survive the attempt that made it")
}

// TestAnUnownedChipIsNeitherPastedOverNorSubmitted: a "[Pasted text #N]" chip is
// what claude renders for ANY paste, so it cannot tell bramble's own unsent
// paste from a block the user pasted and has not submitted.
//
// An earlier version of this branch submitted it, reasoning that "the only
// paste in play here is one of ours". That is the chip-as-provenance reasoning
// already removed from composerHoldsThisDelivery, and it is doubly unavailable
// here: a chipped paste deliberately leaves no staged record (see
// TestAChippedPasteLeavesNoStagedRecord), so by construction bramble has no way
// to know whose chip it is. Pressing Enter on it would submit the user's paste
// and drop this delivery as though it had been sent.
//
// Neither pasted over nor submitted: held, and reported.
func TestAnUnownedChipIsNeitherPastedOverNorSubmitted(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderClaude, "claude-opus-5")
	// A chip this courier has never pasted anything to account for.
	target.setPane(claudeComposerPane("[Pasted text #1 +80 lines]"))

	panes := &fakePanes{}
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)
	now := time.Now()
	c.now = func() time.Time { return now }

	_, err = c.Send(context.Background(), "", "s1", "a report from a subagent", true)
	require.NoError(t, err)
	require.Zero(t, panes.pasteCount(), "precondition: an unidentified composer holds at first")

	now = now.Add(composerHoldGrace + time.Second)
	c.Drain(context.Background(), "s1")

	assert.Zero(t, panes.pasteCount(), "pasting would append to whatever the chip represents")
	assert.NotContains(t, panes.recorded(), "enter(@7)",
		"submitting would send a paste bramble cannot show is its own")
	assert.Len(t, c.Pending("s1"), 1, "the delivery stays queued rather than being dropped as sent")
}

// TestOneStandingBlockIsReportedOnce: the grace branch is reached on every
// retry for as long as the composer holds the same thing, so warning there
// unconditionally is one line every retryDelay for the life of the block — the
// log-flood shape errPaneBusy is logged at Debug to avoid. One standing
// condition is one report.
func TestOneStandingBlockIsReportedOnce(t *testing.T) {
	t.Parallel()
	c := &Courier{
		reportedBlocked: map[SessionID]string{},
		heldForDraft:    map[SessionID]draftHold{},
	}

	assert.True(t, c.noteBlockedReport("s1", "❯ a half-typed line"), "the first block is reported")
	for i := 0; i < 20; i++ {
		assert.False(t, c.noteBlockedReport("s1", "❯ a half-typed line"),
			"the same block must not be reported again on every retry")
	}
	assert.True(t, c.noteBlockedReport("s1", "❯ a different half-typed line"),
		"a new blocking draft is a new situation and is reported")

	// A block that ENDED and came back is a new standing condition, not the
	// same one repeating. The record is released where the block ends —
	// clearDraftHold, on the path that goes on to paste — so without that
	// release "once per block" silently means "once per session per text for
	// the life of the process", and the second standing block goes unreported.
	//
	// clearDraftHold is called directly because that IS the end-of-block
	// signal: write calls it on the path that proceeds to paste, once the
	// composer no longer holds a draft.
	c.clearDraftHold("s1")
	assert.True(t, c.noteBlockedReport("s1", "❯ a different half-typed line"),
		"a block that cleared and returned is a new standing condition and is reported again")
}

// TestAChipBesideTypedTextIsStillAHumanDraft: a chip with text beside it is
// unambiguously a person's composer — a paste happened AND somebody typed. It
// holds for the same reason a bare chip does, and this pins the clearer case so
// no future attempt to read provenance off a chip can start from the easy end.
func TestAChipBesideTypedTextIsStillAHumanDraft(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderClaude, "claude-opus-5")
	target.setPane(claudeComposerPane("[Pasted text #1 +80 lines] and my own question"))

	panes := &fakePanes{}
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)
	now := time.Now()
	c.now = func() time.Time { return now }

	_, err = c.Send(context.Background(), "", "s1", "a report from a subagent", true)
	require.NoError(t, err)
	now = now.Add(composerHoldGrace + time.Second)
	c.Drain(context.Background(), "s1")

	assert.Zero(t, panes.pasteCount(),
		"a chip beside typed text has an author at the keyboard; never paste over it")
	assert.Len(t, c.Pending("s1"), 1, "the delivery stays queued")
}

// TestUserTypingDuringPasteVerificationIsNotPastedInto covers the window the
// draft check cannot: a person starts typing AFTER composerDraftText read an
// empty composer and BEFORE pasteVerdict returns. The composer then holds
// "their draft" + "our text", which confirmsComposer's one-way prefix rule
// correctly refuses to accept as our paste.
//
// Refusing is only half the answer. That refusal is a readable negative, and a
// readable negative used to mean "the TUI dropped it, paste again" — so the
// repair for a dropped paste ran on a pane where nothing was dropped, appending
// a second copy to a person's unsent line. Enter would then submit their
// sentence wearing both copies.
//
// The rule this pins: a re-paste is the repair for an EMPTY composer only. A
// composer holding text bramble cannot own is interference, and interference
// fails closed the same way a draft found up front does — provenance dropped,
// delivery re-queued.
//
// The pane is mutated from the echo hook because that is the only seam that
// runs between the two captures; setting it up front would be caught by the
// draft check instead and would test nothing new.
func TestUserTypingDuringPasteVerificationIsNotPastedInto(t *testing.T) {
	t.Parallel()
	const delivery = "a report from a subagent that ran for a while"
	const draft = "what were we doing with the "

	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderClaude, "claude-opus-5")
	// The composer is empty when the draft check reads it, so the write path
	// proceeds to paste. This is the precondition, not the subject.
	target.appendPane(claudeComposerPane(""))

	panes := &fakePanes{}
	panes.echo = func(text string) {
		// The human was mid-word when our paste landed; tmux paste-buffer
		// appends, so their line now carries our text too.
		target.appendPane(claudeComposerPane(draft + text))
	}
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	_, err = c.Send(context.Background(), "", "s1", delivery, true)
	require.NoError(t, err)

	assert.Equal(t, 1, panes.pasteCount(),
		"a composer holding someone else's draft is not a dropped paste: pasting again appends a second copy to their unsent line")
	assert.NotContains(t, panes.recorded(), "enter(@7)",
		"and Enter would submit their half-typed sentence with our delivery riding on it")
	assert.Len(t, c.Pending("s1"), 1,
		"the delivery is held, not lost, and lands once the composer clears")
	assert.Empty(t, c.stagedText("s1"),
		"a composer bramble does not own cannot be its staged record")
}

// TestDroppedPasteIntoAnEmptyComposerIsStillRetried is the other half of the
// rule above, kept adjacent so neither can be tightened without the other being
// read. An empty composer after a paste IS a dropped paste, and the retry that
// repairs it must survive the interference check.
func TestDroppedPasteIntoAnEmptyComposerIsStillRetried(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)
	target.setBackend("s1", ProviderClaude, "claude-opus-5")
	target.appendPane(claudeComposerPane(""))

	// The pane never shows the text: the TUI dropped every paste.
	panes := &fakePanes{}
	c, err := NewCourier(target, panes, testCourierConfig(t))
	require.NoError(t, err)

	_, err = c.Send(context.Background(), "", "s1", "a report from a subagent", true)
	require.NoError(t, err)

	assert.Equal(t, 2, panes.pasteCount(),
		"an empty composer is a real dropped paste; the one retry must not be suppressed as interference")
	assert.NotContains(t, panes.recorded(), "enter(@7)")
	assert.Len(t, c.Pending("s1"), 1)
}
