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
	// better default for an unattended process: no token handling. Empty falls
	// back to DMCommand.
	SlackWebhook string `yaml:"slack_webhook"`
	// Target names the DM recipient (e.g. "@ming") for message context, and is
	// substituted for "{{recipient}}" in DMCommandArgs.
	Target string `yaml:"target"`
	// SlackToken is the Web API token, either a literal or a "$ENV_VAR"
	// reference. When empty, the token is discovered from $SLACK_BOT_TOKEN or
	// ~/.env, which is what makes DM notification work with no setup at all.
	SlackToken string `yaml:"slack_token"`
	// DMCommand is an optional external program that delivers a message. The
	// native API sink covers this case without an interpreter or an absolute
	// path, so this exists only as an escape hatch for a bespoke transport.
	DMCommand string `yaml:"dm_command"`
	// DMCommandArgs are the arguments to DMCommand. "{{recipient}}" and
	// "{{message}}" are substituted per notify.CommandNotifier.
	DMCommandArgs []string `yaml:"dm_command_args"`
}

// DefaultSlackWebhookEnv is the env var consulted when no webhook is
// configured explicitly.
const DefaultSlackWebhookEnv = "$PRDOZER_SLACK_WEBHOOK"

// WithTarget returns a copy addressed to recipient. An empty recipient leaves
// the configured target alone, so a repo that names no target inherits the
// fleet-wide one rather than losing its destination.
func (c NotifyConfig) WithTarget(recipient string) NotifyConfig {
	if strings.TrimSpace(recipient) != "" {
		c.Target = recipient
	}
	return c
}

// Notifier builds the sinks for a babysit run. A nil result means notification
// is disabled, which is a normal unconfigured state and not an error.
//
// A configured webhook wins: it has no interpreter or token-file dependency and
// so is the more robust sink for unattended runs. The DM command is the
// fallback that makes an unconfigured deployment still notify.
func (c NotifyConfig) Notifier() notify.Notifier {
	raw := c.SlackWebhook
	if strings.TrimSpace(raw) == "" {
		raw = DefaultSlackWebhookEnv
	}
	if url := notify.ResolveWebhookURL(raw); url != "" {
		return notify.SlackWebhookNotifier{WebhookURL: url}
	}
	// Native API next: it needs no interpreter, no absolute script path that
	// differs per box, and no webhook created by hand in the Slack UI.
	if token := notify.ResolveEnvRef(c.SlackToken); token != "" {
		return &notify.SlackAPINotifier{Token: token, Target: c.Target}
	}
	if token := notify.ResolveSlackToken(); token != "" && strings.TrimSpace(c.Target) != "" {
		return &notify.SlackAPINotifier{Token: token, Target: c.Target}
	}
	if cmd := strings.TrimSpace(c.DMCommand); cmd != "" {
		// Expand "~" in args too, not just the program path: the registry is
		// shared across the fleet, where the home directory differs per box
		// (the Azure devbox runs as "ming", the AWS boxes as "ubuntu"). A
		// hardcoded /home/<user> path silently breaks on the other boxes.
		args := make([]string, 0, len(c.DMCommandArgs))
		for _, a := range c.DMCommandArgs {
			args = append(args, ExpandHome(a))
		}
		return notify.CommandNotifier{
			Path:      ExpandHome(cmd),
			Recipient: c.Target,
			Args:      args,
		}
	}
	return nil
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
// failures trip the backoff cooldown.
//
// Unbounded retry is only an acceptable design if it stays VISIBLE: the loop
// will keep trying, so it must keep telling you.
//
// Deliberately NOT merge-specific. The cooldown is driven by
// ConsecutiveFailures, which a stalled polish round increments just as a
// rejected merge does — so wording this as "repeated merge failures" and
// quoting LastMergeError misreports every non-merge cause, and on a polish-only
// stall LastMergeError is empty, naming no cause at all.
func CooldownWarning(meta RunMeta, until time.Time, lastErr string) RunReport {
	cause := lastErr
	if cause == "" {
		cause = "not recorded (the cooldown was not tripped by a merge attempt; see the run log for the failing step)"
	}
	return RunReport{
		Meta:    meta,
		State:   TerminalRunning,
		Warning: true,
		Detail: fmt.Sprintf(
			"Repeated failures tripped the backoff cooldown; retrying after %s.\nLast error: %s",
			until.Format(time.RFC3339), cause),
	}
}
