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
// A truncated line is the production-realistic corruption (what a killed or
// mid-flush writer emits). It fails the OUTER decode in handleMessage — Go's
// json validates the whole input — which is why every test here drives
// handleMessage rather than handleNotification. The method survives the cut and
// is recovered by byte scan.
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
			// Through handleMessage — the REAL entry point. Driving
			// handleNotification directly would bypass the outer decode that
			// actually rejects a truncated frame, and pass against code that
			// aborts in production.
			c.handleMessage(malformedNotification(method))

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
			c.handleMessage(malformedNotification(method))

			events := drainEvents(c)
			require.Len(t, events, 1, "a malformed terminal notification must be fatal")

			errEvt, ok := events[0].(ErrorEvent)
			require.True(t, ok)
			assert.Equal(t, "parse_message", errEvt.Context)
		})
	}
}

// A well-formed notification still DISPATCHES. Asserting only "no ErrorEvent"
// would pass even if the whole dispatch switch were deleted, so this asserts the
// observable effect: the turn/started event reaches the channel.
func TestHandleNotification_WellFormedStillDispatches(t *testing.T) {
	c := newNotificationTestClient()
	c.handleMessage(
		[]byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"t1","turn":{"id":"turn-1"}}}`))

	events := drainEvents(c)
	var sawTurnStarted bool
	for _, ev := range events {
		if _, isErr := ev.(ErrorEvent); isErr {
			t.Fatalf("a well-formed notification must not produce an ErrorEvent: %#v", ev)
		}
		if _, ok := ev.(TurnStartedEvent); ok {
			sawTurnStarted = true
		}
	}
	require.True(t, sawTurnStarted,
		"turn/started must dispatch a TurnStartedEvent, not merely avoid an error")
}

// recoverMethod is what makes the skip decision possible on a line Go's json
// rejects. It must recover a real method and refuse anything it cannot trust —
// a forged/escaped occurrence must not be able to downgrade a terminal frame.
func TestRecoverMethod(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
		ok   bool
	}{
		{"truncated frame", `{"jsonrpc":"2.0","method":"item/started","params":{"a"`, "item/started", true},
		{"whitespace around colon", `{"method" : "turn/completed","params":{`, "turn/completed", true},
		{"no method key", `{"jsonrpc":"2.0","params":{"a":1`, "", false},
		{"method value truncated mid-string", `{"method":"item/star`, "", false},
		{"method key present but non-string value", `{"method":123,"params":{`, "", false},
		{"escaped method inside a string is not recovered", `{"payload":"\"method\":\"item/started\"","x":1`, "", false},
		// A nested "method" is not THIS frame's method. Trusting one would
		// silently drop a frame that should have been fatal.
		{"nested method inside params is not recovered", `{"jsonrpc":"2.0","params":{"method":"item/started","turn":{"id":"t1"`, "", false},
		{"nested method inside a truncated result is not recovered", `{"jsonrpc":"2.0","result":{"method":"item/started","data":{`, "", false},
		// A top-level id marks a response/request: dropping one would leave
		// sendRequestAndWait blocked until context cancellation.
		{"top-level id refuses recovery", `{"jsonrpc":"2.0","id":7,"result":{"method":"item/started"`, "", false},
		{"top-level id before method still refuses", `{"id":3,"method":"item/started","params":{`, "", false},
		// ACCEPTED LIMITATION: the scan stops at the first unterminated string,
		// so an "id" after the truncation point is never seen and this frame is
		// skipped. Latent, not live — codex dispatches no inbound requests, and
		// a response with no top-level method falls through to fatal anyway.
		// Pinned so the docstring's narrowed claim stays checkable.
		{"id after the truncation point is NOT seen (documented limit)",
			`{"jsonrpc":"2.0","method":"item/started","params":{"a":"cut`, "item/started", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := recoverMethod([]byte(tt.line))
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

// A corrupt frame whose method sits nested must stay FATAL end-to-end: the skip
// decision is what makes a wrong recovery dangerous.
func TestHandleMessage_NestedMethodStaysFatal(t *testing.T) {
	for _, name := range []string{
		`{"jsonrpc":"2.0","params":{"method":"item/started","turn":{"id":"t1"`,
		`{"jsonrpc":"2.0","id":9,"result":{"method":"item/started","x":{`,
	} {
		c := newNotificationTestClient()
		c.handleMessage([]byte(name))

		events := drainEvents(c)
		require.Len(t, events, 1, "a frame with no trustworthy top-level method must be fatal: %s", name)
		errEvt, ok := events[0].(ErrorEvent)
		require.True(t, ok)
		assert.Equal(t, "parse_message", errEvt.Context)
	}
}
