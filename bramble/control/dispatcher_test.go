package control

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/bramble/tmuxctl"
)

// fakeRegistry is a hand fake of the Registry interface for dispatcher tests.
type fakeRegistry struct {
	targets map[string]string // sessionID -> tmux target
	// onResolve, when set, runs inside ResolveTmuxTarget. It lets a test park a
	// request inside a live handler so Close's drain has something to wait on.
	onResolve  func()
	resolveErr error
	captureErr error
	stopErr    error
	sessions   []session.SessionInfo
	captured   []string
	stopped    []string
	setRunning []string
	setIdle    []string
}

func (f *fakeRegistry) GetAllSessions() []session.SessionInfo { return f.sessions }

func (f *fakeRegistry) ResolveTmuxTarget(id session.SessionID) (string, error) {
	if f.onResolve != nil {
		f.onResolve()
	}
	if f.resolveErr != nil {
		return "", f.resolveErr
	}
	t, ok := f.targets[string(id)]
	if !ok {
		return "", fmt.Errorf("session not found: %s", id)
	}
	return t, nil
}

func (f *fakeRegistry) CapturePaneText(id session.SessionID, _ int) ([]string, error) {
	if f.captureErr != nil {
		return nil, f.captureErr
	}
	return f.captured, nil
}

func (f *fakeRegistry) SetSessionRunning(id session.SessionID) {
	f.setRunning = append(f.setRunning, string(id))
}

func (f *fakeRegistry) SetSessionIdle(id session.SessionID) {
	f.setIdle = append(f.setIdle, string(id))
}

func (f *fakeRegistry) StopSession(id session.SessionID) error {
	if f.stopErr != nil {
		return f.stopErr
	}
	f.stopped = append(f.stopped, string(id))
	return nil
}

func newDispatcher(reg *fakeRegistry) (*Dispatcher, *tmuxctl.FakeController) {
	ctl := tmuxctl.NewFake()
	return NewDispatcher(reg, ctl), ctl
}

func req(t *testing.T, typ MsgType, payload any) *Msg {
	t.Helper()
	m, err := NewRequest(typ, "rid-1", payload)
	require.NoError(t, err)
	return m
}

func TestSessionSendInputResolvesTargetAndSubmits(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{targets: map[string]string{"s1": "@7"}}
	d, ctl := newDispatcher(reg)

	resp := d.Handle(context.Background(), req(t, TypeSessionSendInput,
		SendInputReq{SessionID: "s1", Text: "hello", Submit: true}))

	var ok OKResult
	require.NoError(t, resp.DecodeResponse(&ok))
	assert.True(t, ok.OK)

	pastes := ctl.CallsFor("Paste")
	require.Len(t, pastes, 1)
	assert.Equal(t, "@7", pastes[0].Target)
	assert.Equal(t, "hello", pastes[0].Text)
	// Submit=true → exactly one Enter.
	enters := ctl.CallsFor("SendSpecial")
	require.Len(t, enters, 1)
	assert.Equal(t, tmuxctl.KeyEnter, enters[0].Special)
}

func TestSessionSendInputNoSubmitSendsNoEnter(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{targets: map[string]string{"s1": "@7"}}
	d, ctl := newDispatcher(reg)

	resp := d.Handle(context.Background(), req(t, TypeSessionSendInput,
		SendInputReq{SessionID: "s1", Text: "draft", Submit: false}))

	require.NoError(t, resp.DecodeResponse(nil))
	assert.Len(t, ctl.CallsFor("Paste"), 1)
	assert.Empty(t, ctl.CallsFor("SendSpecial"), "no Enter when Submit is false")
}

func TestSessionSendInputUnknownSessionErrors(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{targets: map[string]string{}}
	d, ctl := newDispatcher(reg)

	resp := d.Handle(context.Background(), req(t, TypeSessionSendInput,
		SendInputReq{SessionID: "ghost", Text: "x", Submit: true}))

	err := resp.DecodeResponse(nil)
	require.Error(t, err)
	var re *RemoteError
	assert.ErrorAs(t, err, &re)
	// Must not touch tmux when resolution fails.
	assert.Empty(t, ctl.Calls)
}

func TestSessionSendInputNonTmuxSessionErrorsCleanly(t *testing.T) {
	t.Parallel()
	// Simulate the runner-type guard rejecting a TUI session.
	reg := &fakeRegistry{resolveErr: fmt.Errorf("session \"s1\" is not a tmux session (runner type: tui)")}
	d, ctl := newDispatcher(reg)

	resp := d.Handle(context.Background(), req(t, TypeSessionSendKey,
		SendKeyReq{SessionID: "s1", Key: tmuxctl.KeyCtrlC}))

	err := resp.DecodeResponse(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a tmux session")
	assert.Empty(t, ctl.Calls)
}

func TestSessionListProjectsSummaries(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{sessions: []session.SessionInfo{
		{ID: "s1", Type: "builder", Status: "running", WorktreeName: "wt", Model: "opus", RunnerType: "tmux", TmuxWindowID: "@3"},
		{ID: "s2", Type: "planner", Status: "idle", RunnerType: "tmux-tracked", TmuxWindowName: "repo/wt:0"},
	}}
	d, _ := newDispatcher(reg)

	resp := d.Handle(context.Background(), req(t, TypeSessionList, nil))
	var res SessionListResult
	require.NoError(t, resp.DecodeResponse(&res))
	require.Len(t, res.Sessions, 2)
	assert.Equal(t, "@3", res.Sessions[0].TmuxTarget)
	// Falls back to window name when ID is empty.
	assert.Equal(t, "repo/wt:0", res.Sessions[1].TmuxTarget)
}

func TestSessionCaptureUsesRegistryGuard(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{captured: []string{"line1", "line2"}}
	d, _ := newDispatcher(reg)

	resp := d.Handle(context.Background(), req(t, TypeSessionCapture,
		CaptureReq{SessionID: "s1", Lines: 20}))
	var res CaptureResult
	require.NoError(t, resp.DecodeResponse(&res))
	assert.Equal(t, []string{"line1", "line2"}, res.Lines)
}

func TestPaneSendInputUsesRawTarget(t *testing.T) {
	t.Parallel()
	// Raw-pane path bypasses the registry and writes straight to the target.
	reg := &fakeRegistry{}
	d, ctl := newDispatcher(reg)

	resp := d.Handle(context.Background(), req(t, TypePaneSendInput,
		SendInputReq{Target: "%9", Text: "raw", Submit: false}))

	require.NoError(t, resp.DecodeResponse(nil))
	pastes := ctl.CallsFor("Paste")
	require.Len(t, pastes, 1)
	assert.Equal(t, "%9", pastes[0].Target)
}

func TestPaneSendInputMissingTargetErrors(t *testing.T) {
	t.Parallel()
	d, ctl := newDispatcher(&fakeRegistry{})

	resp := d.Handle(context.Background(), req(t, TypePaneSendInput,
		SendInputReq{Text: "x"}))

	require.Error(t, resp.DecodeResponse(nil))
	assert.Empty(t, ctl.Calls)
}

func TestSessionStop(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{}
	d, _ := newDispatcher(reg)

	resp := d.Handle(context.Background(), req(t, TypeSessionStop, SessionRef{SessionID: "s1"}))
	require.NoError(t, resp.DecodeResponse(nil))
	assert.Equal(t, []string{"s1"}, reg.stopped)
}

func TestRawListWindows(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{}
	d, ctl := newDispatcher(reg)
	ctl.Windows = []tmuxctl.TmuxWindow{{ID: "@1", Name: "w"}}

	resp := d.Handle(context.Background(), req(t, TypeTmuxListWindows, TargetRef{Target: ""}))
	var res []tmuxctl.TmuxWindow
	require.NoError(t, resp.DecodeResponse(&res))
	require.Len(t, res, 1)
	assert.Equal(t, "@1", res[0].ID)
}

func TestUnsupportedTypeErrors(t *testing.T) {
	t.Parallel()
	d, _ := newDispatcher(&fakeRegistry{})
	resp := d.Handle(context.Background(), &Msg{Type: MsgType("bogus"), ID: "x"})
	err := resp.DecodeResponse(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported request type")
}

// compile-time: the real registry satisfies the narrow Registry interface.
var _ Registry = (*session.SessionRegistry)(nil)

// TestSendInputWithoutQueueTypesIntoThePane is the compatibility guard: the
// deliberate-interrupt path is what send-input has always done for a caller who
// wants the recipient to see the text now, and it is untouched.
func TestSendInputWithoutQueueTypesIntoThePane(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{targets: map[string]string{"s1": "@7"}}
	d, ctl := newDispatcher(reg)

	resp := d.Handle(context.Background(), req(t, TypeSessionSendInput,
		SendInputReq{SessionID: "s1", Text: "hello", Submit: true}))

	var result SendInputResult
	require.NoError(t, resp.DecodeResponse(&result))
	assert.True(t, result.OK)
	assert.Len(t, ctl.CallsFor("Paste"), 1)
	assert.Len(t, ctl.CallsFor("SendSpecial"), 1, "Submit presses Enter")
}

// TestSendInputQueueIsRefused pins the removal of queued delivery.
//
// It is refused rather than downgraded to an immediate paste on purpose: a
// caller that asked to wait for an idle recipient must not silently get a
// mid-turn interrupt instead. Queued delivery held a message until the pane
// looked ready, which meant guessing readiness from screen chrome and keeping
// undeliverable mail on disk; both halves misfired, so completion is now read
// from the run directory rather than pushed.
func TestSendInputQueueIsRefused(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{targets: map[string]string{"s1": "@7"}}
	d, ctl := newDispatcher(reg)

	resp := d.Handle(context.Background(), req(t, TypeSessionSendInput,
		SendInputReq{SessionID: "s1", From: "s0", Text: "hello", Submit: true, Queue: true}))

	err := resp.DecodeResponse(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "removed")
	assert.Empty(t, ctl.CallsFor("Paste"),
		"refusing must not fall back to interrupting the recipient")
	assert.Empty(t, ctl.CallsFor("SendSpecial"))
}

// TestSendInputLeavesCopyModeFirst pins a silent tmux failure. A pane someone
// scrolled back in swallows the Enter that would submit the text, so the message
// lands in the composer and sits there — reported OK by every measure the caller
// can see, and never read by the agent.
//
// This was previously covered only through the queued path, whose pane writer
// exits copy mode on its own. With --queue gone, the direct write is the only
// path left and has to do it itself.
func TestSendInputLeavesCopyModeFirst(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{targets: map[string]string{"s1": "@7"}}
	d, ctl := newDispatcher(reg)

	resp := d.Handle(context.Background(), req(t, TypeSessionSendInput,
		SendInputReq{SessionID: "s1", Text: "hello", Submit: true}))

	var result SendInputResult
	require.NoError(t, resp.DecodeResponse(&result))
	require.Len(t, ctl.CallsFor("ExitCopyMode"), 1, "the pane must be taken out of copy mode")
	require.Len(t, ctl.CallsFor("Paste"), 1)
}

// TestSubmittedSendMarksTheSessionRunning pins the status half of a write.
//
// A tmux session's status only ever moves one way from outside: the agent's
// hook reports idle, and nothing reports the opposite. Whoever typed the prompt
// is the only party that knows a turn started. This used to be the courier's
// job on the --queue path; with --queue refused, the direct write is the only
// write left, so it has to do it.
//
// Leaving it unset is not cosmetic. list-sessions is the delivery path this
// transport now relies on, so an idle-looking session is read as a finished
// lane; and the turn's real completion notify is then dropped by the
// StatusRunning guard in SetSessionIdle, so the next turn produces no state
// change either and the conversation goes quiet after one exchange.
func TestSubmittedSendMarksTheSessionRunning(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{targets: map[string]string{"s1": "@7"}}
	d, _ := newDispatcher(reg)

	resp := d.Handle(context.Background(), req(t, TypeSessionSendInput,
		SendInputReq{SessionID: "s1", Text: "hello", Submit: true}))

	var result SendInputResult
	require.NoError(t, resp.DecodeResponse(&result))
	require.Equal(t, []string{"s1"}, reg.setRunning,
		"a submitted prompt started a turn and nothing else reports that")
}

// Staged text is not a turn: without Enter the agent never sees it, so moving
// the session to running would make an idle session look busy forever.
func TestUnsubmittedSendDoesNotMarkRunning(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{targets: map[string]string{"s1": "@7"}}
	d, _ := newDispatcher(reg)

	d.Handle(context.Background(), req(t, TypeSessionSendInput,
		SendInputReq{SessionID: "s1", Text: "hello", Submit: false}))

	require.Empty(t, reg.setRunning, "no Enter, no turn")
}

// A raw --target names a pane, not a session, so there is no status to move.
func TestRawPaneSendMarksNothingRunning(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{targets: map[string]string{"s1": "@7"}}
	d, _ := newDispatcher(reg)

	d.Handle(context.Background(), req(t, TypePaneSendInput,
		SendInputReq{Target: "@9", Text: "hello", Submit: true}))

	require.Empty(t, reg.setRunning, "a raw pane target has no session status")
}

// send-key is the other half of the documented two-step: stage text with an
// unsubmitted send, then press Enter. That Enter is what starts the turn, so it
// carries the same obligation as a submitted send — the defect this mirrors was
// found only because send_input was fixed and its sibling was not.
func TestSendKeyEnterMarksTheSessionRunning(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{targets: map[string]string{"s1": "@7"}}
	d, _ := newDispatcher(reg)

	d.Handle(context.Background(), req(t, TypeSessionSendKey,
		SendKeyReq{SessionID: "s1", Key: tmuxctl.KeyEnter}))

	require.Equal(t, []string{"s1"}, reg.setRunning,
		"Enter submits the composer, which starts a turn")
}

// Only Enter. Interrupting or dismissing does not start a turn, and marking one
// would leave an idle session looking busy with nothing to end it.
func TestNonSubmittingKeysMarkNothingRunning(t *testing.T) {
	t.Parallel()
	for _, key := range []tmuxctl.SpecialKey{tmuxctl.KeyEscape, tmuxctl.KeyCtrlC} {
		t.Run(string(key), func(t *testing.T) {
			t.Parallel()
			reg := &fakeRegistry{targets: map[string]string{"s1": "@7"}}
			d, _ := newDispatcher(reg)

			d.Handle(context.Background(), req(t, TypeSessionSendKey,
				SendKeyReq{SessionID: "s1", Key: key}))

			require.Empty(t, reg.setRunning, "%s does not start a turn", key)
		})
	}
}

// A submit that failed started no turn, so the marking must be rolled back or
// the session reads busy forever with nothing alive to end it.
func TestAFailedSubmitRollsTheTurnBack(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{targets: map[string]string{"s1": "@7"}}
	d, ctl := newDispatcher(reg)
	ctl.SendSpecialErr = fmt.Errorf("send-keys failed")

	d.Handle(context.Background(), req(t, TypeSessionSendInput,
		SendInputReq{SessionID: "s1", Text: "hello", Submit: true}))

	require.Equal(t, []string{"s1"}, reg.setRunning, "marked before the Enter")
	require.Equal(t, []string{"s1"}, reg.setIdle, "and put back when the Enter failed")
}

// A raw --target names a pane, which has no session status to move.
func TestRawPaneEnterMarksNothingRunning(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{targets: map[string]string{"s1": "@7"}}
	d, _ := newDispatcher(reg)

	d.Handle(context.Background(), req(t, TypePaneSendKey,
		SendKeyReq{Target: "@9", Key: tmuxctl.KeyEnter}))

	require.Empty(t, reg.setRunning, "a raw pane target has no session status")
}
