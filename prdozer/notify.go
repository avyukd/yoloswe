package prdozer

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/notify"
)

// NotifyConfig configures where babysit outcomes are reported.
type NotifyConfig struct {
	// SlackWebhook is either a URL or a "$ENV_VAR" reference. A webhook is the
	// better default for an unattended process: no token handling. Empty
	// disables notification entirely.
	SlackWebhook string `yaml:"slack_webhook"`
	// Target names the DM recipient (e.g. "@ming") for message context.
	Target string `yaml:"target"`
}

// DefaultSlackWebhookEnv is the env var consulted when no webhook is
// configured explicitly.
const DefaultSlackWebhookEnv = "$PRDOZER_SLACK_WEBHOOK"

// Notifier builds the sinks for a babysit run. A nil result means notification
// is disabled, which is a normal unconfigured state and not an error.
func (c NotifyConfig) Notifier() notify.Notifier {
	raw := c.SlackWebhook
	if strings.TrimSpace(raw) == "" {
		raw = DefaultSlackWebhookEnv
	}
	url := notify.ResolveWebhookURL(raw)
	if url == "" {
		return nil
	}
	return notify.SlackWebhookNotifier{WebhookURL: url}
}

// RunReport is everything a human needs to act on a finished (or stalled)
// babysit run without opening a log first.
type RunReport struct {
	State            TerminalState
	Detail           string
	WorktreeKeptPath string
	Meta             RunMeta
	Elapsed          time.Duration
	Warning          bool
}

// Message renders the report.
func (r RunReport) Message() notify.Message {
	icon, verb := stateIconVerb(r.State, r.Warning)
	pr := fmt.Sprintf("%s#%d", r.Meta.Repo, r.Meta.PRNumber)

	msg := notify.Message{
		Title: fmt.Sprintf("%s prdozer babysit %s for %s", icon, verb, pr),
		Body:  r.Detail,
	}
	msg = msg.WithField("PR", r.Meta.PRURL)
	msg = msg.WithField("Host", r.Meta.Host)
	msg = msg.WithField("Branch", r.Meta.Branch)
	msg = msg.WithField("Merge policy", string(r.Meta.MergePolicy))
	if r.Meta.PolishRounds > 0 {
		msg = msg.WithField("Polish rounds", fmt.Sprintf("%d", r.Meta.PolishRounds))
	}
	if r.Meta.MergeAttempt > 0 {
		msg = msg.WithField("Merge attempts", fmt.Sprintf("%d", r.Meta.MergeAttempt))
	}
	if r.Elapsed > 0 {
		msg = msg.WithField("Elapsed", r.Elapsed.Round(time.Second).String())
	}
	// The tmux session is how you attach to a run that is still going.
	msg = msg.WithField("Attach", tmuxAttachHint(r.Meta.TmuxSession))
	// The log dir OUTLIVES the GC'd worktree, so it is the durable pointer.
	msg = msg.WithField("Logs", r.Meta.LogDir)
	if r.WorktreeKeptPath != "" {
		msg = msg.WithField("Worktree KEPT (unpushed work)", r.WorktreeKeptPath)
	}
	return msg
}

func stateIconVerb(state TerminalState, warning bool) (icon, verb string) {
	if warning {
		return "⚠️", "backed off"
	}
	switch state {
	case TerminalMerged:
		return "✅", "merged"
	case TerminalClosed:
		return "🚪", "stopped (PR closed)"
	case TerminalNeedsHuman:
		return "🙋", "needs a human"
	case TerminalFailed:
		return "🚨", "failed"
	default:
		return "ℹ️", string(state)
	}
}

func tmuxAttachHint(session string) string {
	if session == "" {
		return ""
	}
	return fmt.Sprintf("tmux attach -t %q", session)
}

// Report sends the run report to the configured sink. It never returns an
// error: a notification failure must not change the run's outcome.
func Report(ctx context.Context, logger *slog.Logger, n notify.Notifier, report RunReport) {
	if n == nil {
		return
	}
	notify.Send(ctx, logger, report.Message(), n)
}

// CooldownWarning builds the non-terminal alert emitted each time repeated
// merge rework trips the backoff cooldown.
//
// Unbounded retry is only an acceptable design if it stays VISIBLE: the loop
// will keep trying, so it must keep telling you.
func CooldownWarning(meta RunMeta, until time.Time, lastErr string) RunReport {
	return RunReport{
		Meta:    meta,
		State:   TerminalRunning,
		Warning: true,
		Detail: fmt.Sprintf(
			"Repeated merge failures tripped the backoff cooldown; retrying after %s.\nLast merge error: %s",
			until.Format(time.RFC3339), lastErr),
	}
}
