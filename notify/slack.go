package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SlackAPIURL is the Slack Web API base. It is a var so tests can point the
// client at an httptest server.
var SlackAPIURL = "https://slack.com/api"

// SlackAPINotifier posts through the Slack Web API using a bot or user token.
//
// It exists because the alternative sinks each carry a setup cost this one
// does not: an incoming webhook has to be created by hand in the Slack UI, and
// shelling out to a helper script depends on an interpreter and an absolute
// path that differs per box. A token already provisioned in ~/.env plus a
// plain HTTPS POST needs neither.
//
// Target may be a channel ID, "#channel", or "@user"; "@user" is resolved to a
// DM channel, which costs two extra API calls the first time and is then
// cached for the process lifetime.
type SlackAPINotifier struct {
	Client *http.Client
	Token  string
	Target string

	mu       sync.Mutex
	resolved map[string]string
}

// Notify delivers the rendered message. An empty Token or Target disables the
// sink silently, matching the other notifiers so an unconfigured deployment is
// not a run failure.
func (n *SlackAPINotifier) Notify(ctx context.Context, msg Message) error {
	if strings.TrimSpace(n.Token) == "" || strings.TrimSpace(n.Target) == "" {
		return nil
	}
	channel, err := n.resolveChannel(ctx, n.Target)
	if err != nil {
		return err
	}
	_, err = n.call(ctx, "chat.postMessage", map[string]any{
		"channel": channel,
		"text":    msg.Render(),
	})
	return err
}

// resolveChannel maps "@user" to a DM channel ID. Channel IDs and "#channel"
// names pass through untouched, since the API accepts both.
func (n *SlackAPINotifier) resolveChannel(ctx context.Context, target string) (string, error) {
	if !strings.HasPrefix(target, "@") {
		return target, nil
	}
	n.mu.Lock()
	if id, ok := n.resolved[target]; ok {
		n.mu.Unlock()
		return id, nil
	}
	n.mu.Unlock()

	username := strings.ToLower(strings.TrimPrefix(target, "@"))
	users, err := n.call(ctx, "users.list", map[string]any{"limit": 200})
	if err != nil {
		return "", err
	}
	var userID string
	for _, m := range users.Members {
		for _, name := range []string{m.Name, m.Profile.DisplayName, m.Profile.RealName} {
			if name != "" && strings.EqualFold(name, username) {
				userID = m.ID
				break
			}
		}
		if userID != "" {
			break
		}
	}
	if userID == "" {
		return "", fmt.Errorf("slack: no user matching %q", target)
	}

	conv, err := n.call(ctx, "conversations.open", map[string]any{"users": userID})
	if err != nil {
		return "", err
	}
	channelID := conv.channelID()
	if channelID == "" {
		return "", fmt.Errorf("slack: conversations.open returned no channel for %q", target)
	}

	n.mu.Lock()
	if n.resolved == nil {
		n.resolved = make(map[string]string)
	}
	n.resolved[target] = channelID
	n.mu.Unlock()
	return channelID, nil
}

// slackResponse covers the fields used across the endpoints called here.
//
// Channel is deliberately json.RawMessage: conversations.open returns an
// OBJECT ({"channel":{"id":"D123"}}) while chat.postMessage returns a STRING
// ({"channel":"D123"}). Decoding both into one struct field fails on the
// string form, so the shape is resolved per call site instead.
type slackResponse struct {
	OK      bool            `json:"ok"`
	Error   string          `json:"error"`
	Channel json.RawMessage `json:"channel"`
	Members []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Profile struct {
			DisplayName string `json:"display_name"`
			RealName    string `json:"real_name"`
		} `json:"profile"`
	} `json:"members"`
}

// channelID extracts the channel id from either shape Slack returns.
func (r *slackResponse) channelID() string {
	if len(r.Channel) == 0 {
		return ""
	}
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(r.Channel, &obj); err == nil && obj.ID != "" {
		return obj.ID
	}
	var s string
	if err := json.Unmarshal(r.Channel, &s); err == nil {
		return s
	}
	return ""
}

func (n *SlackAPINotifier) call(ctx context.Context, method string, payload map[string]any) (*slackResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("slack %s: marshal payload: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, SlackAPIURL+"/"+method, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("slack %s: build request: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+n.Token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	client := n.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("slack %s: %w", method, err)
	}
	defer resp.Body.Close()

	var out slackResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("slack %s: decode response (HTTP %d): %w", method, resp.StatusCode, err)
	}
	// Slack signals failure in the body with HTTP 200, so the status code alone
	// is not a success signal.
	if !out.OK {
		return nil, fmt.Errorf("slack %s: %s%s", method, out.Error, slackHint(out.Error))
	}
	return &out, nil
}

// slackHint appends the actionable part of a Slack error code, so a failure in
// an unattended run says what to do rather than only what broke.
func slackHint(code string) string {
	switch code {
	case "invalid_auth", "token_revoked":
		return " (token is invalid or expired)"
	case "missing_scope":
		return " (token lacks a required scope: chat:write, and users:read for @user targets)"
	case "channel_not_found":
		return " (check the channel name or ID)"
	case "not_in_channel":
		return " (invite the app to the channel first)"
	}
	return ""
}

// SlackTokenEnvVar is the environment variable and .env key holding the token.
const SlackTokenEnvVar = "SLACK_BOT_TOKEN"

// ResolveSlackToken finds a Slack token, preferring the environment and then
// falling back to dotenv files.
//
// The file fallback matters for dispatched runs: a worker started over SSH
// under tmux inherits almost no environment, so a token that exists only as a
// shell export is not visible to it. Reading the file directly makes the sink
// work without any environment plumbing.
//
// A user token (xoxp-) is preferred over a bot token (xoxb-) when a file
// defines both, so messages come from the operator rather than an app.
func ResolveSlackToken(paths ...string) string {
	if tok := strings.TrimSpace(os.Getenv(SlackTokenEnvVar)); tok != "" {
		return tok
	}
	if len(paths) == 0 {
		if home, err := os.UserHomeDir(); err == nil {
			paths = []string{filepath.Join(home, ".env")}
		}
	}
	for _, p := range paths {
		if tok := tokenFromDotenv(p); tok != "" {
			return tok
		}
	}
	return ""
}

func tokenFromDotenv(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var found []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != SlackTokenEnvVar {
			continue
		}
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		if val != "" {
			found = append(found, val)
		}
	}
	for _, tok := range found {
		if strings.HasPrefix(tok, "xoxp-") {
			return tok
		}
	}
	if len(found) > 0 {
		return found[len(found)-1]
	}
	return ""
}
