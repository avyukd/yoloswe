package codex

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// drainEvents collects everything currently queued on the client's event
// channel without blocking.
func drainEvents(c *Client) []Event {
	var events []Event
	for {
		select {
		case ev := <-c.events:
			events = append(events, ev)
		default:
			return events
		}
	}
}

func newNotificationTestClient() *Client {
	return &Client{events: make(chan Event, 16)}
}

// malformedNotification builds a frame that fails the JSONRPCNotification
// decode for the given method.
//
// Note Params is json.RawMessage, so ANY valid JSON shape under "params"
// decodes fine — the tolerance branch is reachable only for structurally broken
// JSON. A truncated line is the production-realistic case (it is what a killed
// or mid-flush writer emits), and it still carries an intact method. That is
// what makes the method a trustworthy discriminator here: it comes from the
// outer id/method decode in handleMessage, not from this failed one.
func malformedNotification(method string) []byte {
	return []byte(`{"jsonrpc":"2.0","method":"` + method + `","params":{"threadId":"t1"`)
}

// A malformed NON-terminal notification must be skipped rather than aborting the
// run. In the reviewer bridge any ErrorEvent is a terminal failure, so emitting
// one here discards an otherwise-healthy review over a single drifted progress
// frame — the failure mode that cost 102 cursor reviews before #266.
func TestHandleNotification_MalformedNonTerminalIsSkipped(t *testing.T) {
	for _, method := range []string{
		NotifyItemStarted,
		NotifyItemCompleted,
		NotifyAgentMessageDelta,
		NotifyCodexEventTokenCount,
		NotifyTurnStarted,
		NotifyCodexEventExecOutput,
		NotifyCodexEventTaskStarted,
		NotifyCodexEventReasoningDelta,
	} {
		t.Run(method, func(t *testing.T) {
			c := newNotificationTestClient()
			c.handleNotification(malformedNotification(method), method)

			assert.Empty(t, drainEvents(c),
				"a malformed non-terminal notification must not emit an ErrorEvent")
		})
	}
}

// The terminal notifications keep failing loud: losing one leaves the caller
// with no completion signal, so a silent skip would hang the review until EOF.
func TestHandleNotification_MalformedTerminalIsFatal(t *testing.T) {
	for _, method := range []string{
		NotifyTurnCompleted,
		NotifyCodexEventTaskComplete,
		NotifyCodexEventError,
	} {
		t.Run(method, func(t *testing.T) {
			c := newNotificationTestClient()
			c.handleNotification(malformedNotification(method), method)

			events := drainEvents(c)
			require.Len(t, events, 1, "a malformed terminal notification must be fatal")

			errEvt, ok := events[0].(ErrorEvent)
			require.True(t, ok)
			assert.Equal(t, "parse_notification", errEvt.Context)
		})
	}
}

// A well-formed notification is unaffected by the tolerance change.
func TestHandleNotification_WellFormedStillDispatches(t *testing.T) {
	c := newNotificationTestClient()
	c.handleNotification(
		[]byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"t1","turn":{"id":"turn-1"}}}`),
		NotifyTurnStarted)

	// No error event: the frame parsed and dispatched normally.
	for _, ev := range drainEvents(c) {
		_, isErr := ev.(ErrorEvent)
		assert.False(t, isErr, "a well-formed notification must not produce an ErrorEvent")
	}
}

// handleMessage is the one parse site that stays fatal unconditionally: it
// decodes only id/method, so its error arm means no method is recoverable and a
// terminal frame cannot be distinguished from droppable traffic.
func TestHandleMessage_UnrecoverableLineIsFatal(t *testing.T) {
	c := newNotificationTestClient()
	c.handleMessage([]byte(`{not json at all`))

	events := drainEvents(c)
	require.Len(t, events, 1)

	errEvt, ok := events[0].(ErrorEvent)
	require.True(t, ok)
	assert.Equal(t, "parse_message", errEvt.Context)
}

func TestIsTerminalNotification(t *testing.T) {
	terminal := []string{NotifyTurnCompleted, NotifyCodexEventTaskComplete, NotifyCodexEventError}
	for _, m := range terminal {
		assert.True(t, isTerminalNotification(m), "%s must be terminal", m)
	}

	nonTerminal := []string{
		NotifyItemStarted, NotifyItemCompleted, NotifyAgentMessageDelta,
		NotifyTurnStarted, NotifyThreadStarted, NotifyCodexEventTokenCount,
		NotifyCodexEventExecBegin, NotifyCodexEventExecEnd, "some/future/method",
	}
	for _, m := range nonTerminal {
		assert.False(t, isTerminalNotification(m), "%s must not be terminal", m)
	}
}

// The raw offending frame must reach the log through ProtocolLine — without it
// the record names a Go struct field but never what the backend actually sent.
func TestProtocolError_ProtocolLine(t *testing.T) {
	err := &ProtocolError{Message: "failed to parse message", Line: `{"type":"x"}`}
	assert.Equal(t, `{"type":"x"}`, err.ProtocolLine())
	assert.NotContains(t, err.Error(), `{"type":"x"}`,
		"the raw line must stay out of Error() and be logged as its own field")
}
