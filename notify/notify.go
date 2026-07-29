// Package notify delivers operational alerts to external sinks.
//
// It is deliberately tool-agnostic: jiradozer reports run failures through it,
// prdozer reports babysit outcomes, and neither depends on the other's domain
// types. Anything tool-specific (which workflow step failed, which tracker
// issue to comment on) stays in the calling tool.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Message is one alert. Title and Body carry the human-facing text; Fields are
// rendered as "key: value" lines so a reader can act without opening a log.
type Message struct {
	// Title is the one-line summary, e.g. "🚨 jiradozer run failed for INF-703".
	Title string
	// Body is optional prose shown under the title.
	Body string
	// Fields are ordered key/value details. A slice, not a map, because the
	// order is meaningful in the rendered message.
	Fields []Field
	// Code is an optional preformatted block (a log tail, an error dump),
	// rendered fenced.
	Code []string
}

// Field is one labelled detail line.
type Field struct {
	Key   string
	Value string
}

// WithField appends a detail line, skipping empty values so an unset field
// never renders as a blank row.
func (m Message) WithField(key, value string) Message {
	if strings.TrimSpace(value) == "" {
		return m
	}
	m.Fields = append(m.Fields, Field{Key: key, Value: value})
	return m
}

// Render produces the plain-text form sent to sinks.
func (m Message) Render() string {
	var b strings.Builder
	b.WriteString(m.Title)
	b.WriteString("\n")
	if m.Body != "" {
		b.WriteString(m.Body)
		if !strings.HasSuffix(m.Body, "\n") {
			b.WriteString("\n")
		}
	}
	for _, f := range m.Fields {
		fmt.Fprintf(&b, "%s: %s\n", f.Key, f.Value)
	}
	if len(m.Code) > 0 {
		b.WriteString("```\n")
		for _, line := range m.Code {
			b.WriteString(line)
			b.WriteByte('\n')
		}
		b.WriteString("```\n")
	}
	return b.String()
}

// Notifier delivers a message to an external destination. Implementations must
// be safe to call with a context deadline and must not panic on partial
// configuration.
type Notifier interface {
	Notify(ctx context.Context, msg Message) error
}

// SlackWebhookNotifier posts messages to a Slack incoming webhook. It is the
// better default for an unattended process: no token handling, and nothing to
// refresh.
type SlackWebhookNotifier struct {
	Client     *http.Client
	WebhookURL string
}

// Notify posts the rendered message. An empty WebhookURL disables the sink
// silently, so an unconfigured deployment is not a run failure.
func (n SlackWebhookNotifier) Notify(ctx context.Context, msg Message) error {
	if n.WebhookURL == "" {
		return nil
	}
	client := n.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	payload, err := json.Marshal(map[string]string{"text": msg.Render()})
	if err != nil {
		return fmt.Errorf("marshal slack payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.WebhookURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build slack request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post to slack: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("slack webhook returned status %d", resp.StatusCode)
	}
	return nil
}

// CommandNotifier delivers a message by executing an external program, with the
// rendered text passed on stdin and the recipient as an argument.
//
// It exists because a Slack incoming webhook has to be created by hand in the
// Slack UI, whereas a DM helper backed by an already-provisioned user token
// works with no setup at all. The trade-off versus SlackWebhookNotifier is that
// this sink depends on an interpreter and a token file being present, so it is
// the fallback rather than the default.
//
// Path is the program and Args are its arguments. Two placeholders are
// substituted in each arg: "{{recipient}}" becomes Recipient and "{{message}}"
// becomes the rendered message text. The rendered text is also always written
// to stdin, so a helper can consume it either way.
type CommandNotifier struct {
	Path      string
	Recipient string
	Args      []string
	Timeout   time.Duration
}

// Notify runs the command. An empty Path disables the sink silently, matching
// SlackWebhookNotifier's behaviour so an unconfigured deployment is not a run
// failure.
func (n CommandNotifier) Notify(ctx context.Context, msg Message) error {
	if strings.TrimSpace(n.Path) == "" {
		return nil
	}
	timeout := n.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	rendered := msg.Render()
	args := make([]string, 0, len(n.Args))
	for _, a := range n.Args {
		a = strings.ReplaceAll(a, "{{recipient}}", n.Recipient)
		a = strings.ReplaceAll(a, "{{message}}", rendered)
		args = append(args, a)
	}

	cmd := exec.CommandContext(ctx, n.Path, args...)
	cmd.Stdin = strings.NewReader(rendered)
	// Capture combined output into our own buffer rather than via
	// CombinedOutput: that helper waits for the output pipes to close, and a
	// killed process that spawned children holding those pipes keeps them open
	// well past the deadline. Writing into a shared buffer lets Wait return as
	// soon as the process itself is reaped.
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	// WaitDelay bounds how long Wait blocks on inherited pipes after the
	// context is cancelled, so an enforced timeout is actually enforced.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	trimmed := strings.TrimSpace(buf.String())
	if err != nil {
		if trimmed != "" {
			return fmt.Errorf("notify command %s: %w: %s", n.Path, err, truncateOutput(trimmed))
		}
		return fmt.Errorf("notify command %s: %w", n.Path, err)
	}
	if strings.HasPrefix(trimmed, "ERROR:") {
		return fmt.Errorf("notify command %s reported: %s", n.Path, truncateOutput(trimmed))
	}
	return nil
}

// truncateOutput bounds command output folded into an error, so a helper that
// dumps a stack trace cannot swamp the log line that reports it.
func truncateOutput(s string) string {
	const maxLen = 400
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}

// ResolveWebhookURL expands a "$ENV_VAR" reference to its value, so a config
// file can name the variable holding the secret instead of embedding it. A
// plain URL is returned unchanged; an unset variable yields "", which disables
// the sink.
func ResolveWebhookURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if after, ok := strings.CutPrefix(raw, "$"); ok {
		return strings.TrimSpace(os.Getenv(after))
	}
	return raw
}

// SinkTimeout bounds each sink independently. It is a var so tests can shrink
// it.
var SinkTimeout = 15 * time.Second

// Send fans a message out to every configured sink.
//
// It is best-effort and never returns an error: a notification failure must
// not mask the outcome being reported, or change the caller's exit path.
//
// Two properties are load-bearing and must be preserved:
//
//   - The context is detached with context.WithoutCancel, so an alert still
//     sends after a cancelled run. The moment you most need the alert is
//     exactly when the run was cancelled.
//   - Each sink gets its OWN deadline derived from that detached parent, so a
//     hung sink cannot starve the others.
func Send(ctx context.Context, logger *slog.Logger, msg Message, sinks ...Notifier) {
	if logger == nil {
		logger = slog.Default()
	}
	parent := context.WithoutCancel(ctx)
	for _, sink := range sinks {
		if sink == nil {
			continue
		}
		sinkCtx, cancel := context.WithTimeout(parent, SinkTimeout)
		err := sink.Notify(sinkCtx, msg)
		cancel()
		if err != nil {
			logger.Warn("failed to send notification", "error", err)
			continue
		}
		logger.Info("sent notification", "title", msg.Title)
	}
}
