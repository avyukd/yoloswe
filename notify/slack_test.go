package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// slackStub stands in for the Slack Web API, recording what was posted.
type slackStub struct {
	srv        *httptest.Server
	postedText atomic.Value // string
	postedChan atomic.Value // string
	usersCalls atomic.Int32
	openCalls  atomic.Int32
}

func newSlackStub(t *testing.T) *slackStub {
	t.Helper()
	s := &slackStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/users.list", func(w http.ResponseWriter, _ *http.Request) {
		s.usersCalls.Add(1)
		writeJSON(w, `{"ok":true,"members":[
			{"id":"U111","name":"someoneelse","profile":{"display_name":"Other","real_name":"Other Person"}},
			{"id":"U999","name":"ming","profile":{"display_name":"Ming","real_name":"Ming Zhao"}}]}`)
	})
	mux.HandleFunc("/conversations.open", func(w http.ResponseWriter, _ *http.Request) {
		s.openCalls.Add(1)
		writeJSON(w, `{"ok":true,"channel":{"id":"D123"}}`)
	})
	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Channel string `json:"channel"`
			Text    string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.postedChan.Store(body.Channel)
		s.postedText.Store(body.Text)
		// The real API returns "channel" as a STRING here, unlike
		// conversations.open which returns an object. Mirror that exactly:
		// a stub that omits it hid a decode failure that only appeared
		// against production Slack.
		writeJSON(w, fmt.Sprintf(`{"ok":true,"channel":%q,"ts":"1785299143.574789"}`, body.Channel))
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)

	prev := SlackAPIURL
	SlackAPIURL = s.srv.URL
	t.Cleanup(func() { SlackAPIURL = prev })
	return s
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, body)
}

func (s *slackStub) text() string {
	v, _ := s.postedText.Load().(string)
	return v
}

func (s *slackStub) channel() string {
	v, _ := s.postedChan.Load().(string)
	return v
}

func TestSlackAPINotifier_EmptyTokenOrTargetIsDisabled(t *testing.T) {
	n := &SlackAPINotifier{Target: "@ming"}
	if err := n.Notify(context.Background(), Message{Title: "x"}); err != nil {
		t.Fatalf("empty token should disable the sink silently, got %v", err)
	}
	n = &SlackAPINotifier{Token: "xoxp-1"}
	if err := n.Notify(context.Background(), Message{Title: "x"}); err != nil {
		t.Fatalf("empty target should disable the sink silently, got %v", err)
	}
}

func TestSlackAPINotifier_ResolvesUserToDMChannel(t *testing.T) {
	s := newSlackStub(t)
	n := &SlackAPINotifier{Token: "xoxp-test", Target: "@ming"}
	if err := n.Notify(context.Background(), Message{Title: "babysit merged"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if got := s.channel(); got != "D123" {
		t.Errorf("expected the resolved DM channel, got %q", got)
	}
	if got := s.text(); !strings.Contains(got, "babysit merged") {
		t.Errorf("rendered text not delivered, got %q", got)
	}
}

// Resolution costs two extra API calls, so it must happen once per target.
func TestSlackAPINotifier_CachesResolvedChannel(t *testing.T) {
	s := newSlackStub(t)
	n := &SlackAPINotifier{Token: "xoxp-test", Target: "@ming"}
	for range 3 {
		if err := n.Notify(context.Background(), Message{Title: "x"}); err != nil {
			t.Fatalf("Notify: %v", err)
		}
	}
	if got := s.usersCalls.Load(); got != 1 {
		t.Errorf("users.list should be called once, got %d", got)
	}
	if got := s.openCalls.Load(); got != 1 {
		t.Errorf("conversations.open should be called once, got %d", got)
	}
}

func TestSlackAPINotifier_ChannelTargetsSkipResolution(t *testing.T) {
	s := newSlackStub(t)
	for _, target := range []string{"#engg", "C0123456"} {
		n := &SlackAPINotifier{Token: "xoxp-test", Target: target}
		if err := n.Notify(context.Background(), Message{Title: "x"}); err != nil {
			t.Fatalf("Notify(%s): %v", target, err)
		}
		if got := s.channel(); got != target {
			t.Errorf("target %q should pass through, got %q", target, got)
		}
	}
	if got := s.usersCalls.Load(); got != 0 {
		t.Errorf("a channel target must not trigger user lookup, got %d calls", got)
	}
}

// Slack reports failures in the body with HTTP 200, so the status code alone
// would be a false success signal.
func TestSlackAPINotifier_BodyErrorWithHTTP200IsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"ok":false,"error":"missing_scope"}`)
	}))
	defer srv.Close()
	prev := SlackAPIURL
	SlackAPIURL = srv.URL
	defer func() { SlackAPIURL = prev }()

	n := &SlackAPINotifier{Token: "xoxp-test", Target: "C0123456"}
	err := n.Notify(context.Background(), Message{Title: "x"})
	if err == nil {
		t.Fatal("expected an error when the body reports ok:false")
	}
	if !strings.Contains(err.Error(), "missing_scope") {
		t.Errorf("error should name the Slack code, got %v", err)
	}
	if !strings.Contains(err.Error(), "chat:write") {
		t.Errorf("error should carry an actionable hint, got %v", err)
	}
}

func TestSlackAPINotifier_UnknownUserIsAnError(t *testing.T) {
	newSlackStub(t)
	n := &SlackAPINotifier{Token: "xoxp-test", Target: "@nobody"}
	err := n.Notify(context.Background(), Message{Title: "x"})
	if err == nil || !strings.Contains(err.Error(), "no user matching") {
		t.Fatalf("expected a clear unknown-user error, got %v", err)
	}
}

func TestResolveSlackToken_PrefersEnvThenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("SLACK_BOT_TOKEN=xoxb-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(SlackTokenEnvVar, "xoxp-from-env")
	if got := ResolveSlackToken(path); got != "xoxp-from-env" {
		t.Errorf("environment must win, got %q", got)
	}
	t.Setenv(SlackTokenEnvVar, "")
	if got := ResolveSlackToken(path); got != "xoxb-from-file" {
		t.Errorf("file is the fallback, got %q", got)
	}
}

// A dispatched worker inherits almost no environment, so the file fallback is
// what makes the sink work without any environment plumbing.
func TestResolveSlackToken_PrefersUserTokenAndHandlesQuoting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	body := "# comment\nexport SLACK_BOT_TOKEN=\"xoxb-bot\"\nSLACK_BOT_TOKEN='xoxp-user'\nOTHER=x\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(SlackTokenEnvVar, "")
	if got := ResolveSlackToken(path); got != "xoxp-user" {
		t.Errorf("a user token must win over a bot token, got %q", got)
	}
}

func TestResolveSlackToken_MissingFileIsEmpty(t *testing.T) {
	t.Setenv(SlackTokenEnvVar, "")
	if got := ResolveSlackToken(filepath.Join(t.TempDir(), "absent")); got != "" {
		t.Errorf("a missing file must yield no token, got %q", got)
	}
}

// Slack returns "channel" as an object from conversations.open but as a bare
// string from chat.postMessage. One struct field cannot decode both.
func TestSlackResponse_ChannelIDAcceptsBothShapes(t *testing.T) {
	t.Parallel()
	var obj slackResponse
	if err := json.Unmarshal([]byte(`{"ok":true,"channel":{"id":"D123"}}`), &obj); err != nil {
		t.Fatalf("object form: %v", err)
	}
	if got := obj.channelID(); got != "D123" {
		t.Errorf("object form: got %q", got)
	}

	var str slackResponse
	if err := json.Unmarshal([]byte(`{"ok":true,"channel":"D456"}`), &str); err != nil {
		t.Fatalf("string form: %v", err)
	}
	if got := str.channelID(); got != "D456" {
		t.Errorf("string form: got %q", got)
	}

	var absent slackResponse
	if err := json.Unmarshal([]byte(`{"ok":true}`), &absent); err != nil {
		t.Fatalf("absent form: %v", err)
	}
	if got := absent.channelID(); got != "" {
		t.Errorf("absent form should be empty, got %q", got)
	}
}
