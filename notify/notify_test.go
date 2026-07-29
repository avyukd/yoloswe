package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestMessage_Render(t *testing.T) {
	t.Parallel()
	msg := Message{Title: "🚨 babysit failed", Body: "PR could not land."}
	msg = msg.WithField("PR", "https://github.com/o/r/pull/42")
	msg = msg.WithField("Host", "ming-devbox2")
	msg = msg.WithField("Empty", "   ")
	msg.Code = []string{"line one", "line two"}

	got := msg.Render()
	if !strings.HasPrefix(got, "🚨 babysit failed\n") {
		t.Errorf("title must lead the message, got %q", got)
	}
	for _, want := range []string{"PR: https://github.com/o/r/pull/42", "Host: ming-devbox2", "```\nline one\nline two\n```"} {
		if !strings.Contains(got, want) {
			t.Errorf("Render() missing %q\ngot:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Empty:") {
		t.Error("an empty field value must not render a blank row")
	}
}

func TestMessage_FieldOrderIsPreserved(t *testing.T) {
	t.Parallel()
	// Fields are a slice, not a map, precisely so the reading order is stable.
	msg := Message{Title: "t"}
	for _, k := range []string{"first", "second", "third"} {
		msg = msg.WithField(k, "v")
	}
	got := msg.Render()
	if i, j := strings.Index(got, "first"), strings.Index(got, "second"); i > j {
		t.Error("field order must be preserved")
	}
	if i, j := strings.Index(got, "second"), strings.Index(got, "third"); i > j {
		t.Error("field order must be preserved")
	}
}

func TestSlackWebhookNotifier_EmptyURLIsDisabled(t *testing.T) {
	t.Parallel()
	// An unconfigured deployment must not turn every run into a failure.
	n := SlackWebhookNotifier{}
	if err := n.Notify(context.Background(), Message{Title: "x"}); err != nil {
		t.Errorf("empty webhook must be a silent no-op, got %v", err)
	}
}

func TestSlackWebhookNotifier_PostsRenderedText(t *testing.T) {
	t.Parallel()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		got = payload["text"]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := SlackWebhookNotifier{WebhookURL: srv.URL, Client: srv.Client()}
	msg := Message{Title: "babysit merged"}.WithField("PR", "#42")
	if err := n.Notify(context.Background(), msg); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if !strings.Contains(got, "babysit merged") || !strings.Contains(got, "PR: #42") {
		t.Errorf("posted text missing content: %q", got)
	}
}

func TestSlackWebhookNotifier_NonOKStatusIsAnError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	n := SlackWebhookNotifier{WebhookURL: srv.URL, Client: srv.Client()}
	if err := n.Notify(context.Background(), Message{Title: "x"}); err == nil {
		t.Error("a 500 from the webhook must surface as an error")
	}
}

func TestResolveWebhookURL(t *testing.T) {
	t.Setenv("PRDOZER_TEST_WEBHOOK", "https://hooks.example/abc")
	cases := []struct{ in, want string }{
		{"$PRDOZER_TEST_WEBHOOK", "https://hooks.example/abc"},
		{"  $PRDOZER_TEST_WEBHOOK  ", "https://hooks.example/abc"},
		{"$PRDOZER_UNSET_VAR", ""},
		{"https://hooks.example/plain", "https://hooks.example/plain"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := ResolveWebhookURL(tc.in); got != tc.want {
			t.Errorf("ResolveWebhookURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// captureNotifier records what it receives.
type captureNotifier struct {
	err  error
	msgs []Message
	mu   sync.Mutex
}

func (c *captureNotifier) Notify(_ context.Context, msg Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, msg)
	return c.err
}

func (c *captureNotifier) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.msgs)
}

func TestSend_DeliversToEverySink(t *testing.T) {
	t.Parallel()
	a, b := &captureNotifier{}, &captureNotifier{}
	Send(context.Background(), discardLogger(), Message{Title: "hello"}, a, b, nil)
	if a.count() != 1 || b.count() != 1 {
		t.Errorf("every sink must receive the message, got a=%d b=%d", a.count(), b.count())
	}
}

func TestSend_StillSendsAfterContextCancel(t *testing.T) {
	t.Parallel()
	// The moment you MOST need an alert is when the run was cancelled, so the
	// context is detached with WithoutCancel.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sink := &captureNotifier{}
	Send(ctx, discardLogger(), Message{Title: "run cancelled"}, sink)
	if sink.count() != 1 {
		t.Error("a cancelled run context must not suppress the alert")
	}
}

// hangingNotifier blocks until its context expires, standing in for a wedged
// sink.
type hangingNotifier struct{ observed chan time.Duration }

func (h hangingNotifier) Notify(ctx context.Context, _ Message) error {
	start := time.Now()
	<-ctx.Done()
	h.observed <- time.Since(start)
	return ctx.Err()
}

func TestSend_HungSinkDoesNotStarveOthers(t *testing.T) {
	// Losing one sink must not lose the rest: each gets its own deadline.
	old := SinkTimeout
	SinkTimeout = 100 * time.Millisecond
	defer func() { SinkTimeout = old }()

	hung := hangingNotifier{observed: make(chan time.Duration, 1)}
	good := &captureNotifier{}
	Send(context.Background(), discardLogger(), Message{Title: "x"}, hung, good)

	select {
	case d := <-hung.observed:
		if d > time.Second {
			t.Errorf("the hung sink was not bounded by SinkTimeout, took %s", d)
		}
	default:
		t.Fatal("the hung sink should have been cancelled")
	}
	if good.count() != 1 {
		t.Error("a sink after a hung one must still receive the message")
	}
}

func TestSend_FailingSinkIsLoggedNotFatal(t *testing.T) {
	t.Parallel()
	// A notification failure must never mask the outcome being reported.
	failing := &captureNotifier{err: fmt.Errorf("slack is down")}
	good := &captureNotifier{}
	Send(context.Background(), discardLogger(), Message{Title: "x"}, failing, good)
	if good.count() != 1 {
		t.Error("a failing sink must not prevent later sinks")
	}
}
