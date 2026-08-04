package prdozer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/notify"
)

func sampleMeta() RunMeta {
	return RunMeta{
		Repo:         "sycamore-labs/kernel",
		PRNumber:     8123,
		RunID:        "a3f9",
		Host:         "ming-devbox2",
		Branch:       "feature/x",
		PRURL:        "https://github.com/sycamore-labs/kernel/pull/8123",
		TmuxSession:  "babysit/kernel#8123",
		LogDir:       "/home/ming/.prdozer/runs/sycamore-labs-kernel-8123-a3f9",
		MergePolicy:  MergePolicyQueue,
		PolishRounds: 3,
		MergeAttempt: 2,
	}
}

func TestRunReport_CarriesEverythingNeededToAct(t *testing.T) {
	t.Parallel()
	// The message must let a human act WITHOUT opening a log first.
	got := RunReport{
		Meta:    sampleMeta(),
		State:   TerminalMerged,
		Elapsed: 42 * time.Minute,
	}.Message().Render()

	for _, want := range []string{
		"sycamore-labs/kernel#8123",
		"https://github.com/sycamore-labs/kernel/pull/8123",
		"ming-devbox2",
		"Polish rounds: 3",
		"Merge attempts: 2",
		"Elapsed: 42m0s",
		`tmux attach -t "babysit/kernel#8123"`,
		"/home/ming/.prdozer/runs/sycamore-labs-kernel-8123-a3f9",
	} {
		assert.Contains(t, got, want)
	}
}

func TestRunReport_LogDirSurvivesGCAndIsAlwaysReported(t *testing.T) {
	t.Parallel()
	// The worktree is removed on terminal state; the log dir is the durable
	// pointer, so it must always be present.
	for _, state := range []TerminalState{TerminalMerged, TerminalClosed, TerminalNeedsHuman, TerminalFailed} {
		got := RunReport{Meta: sampleMeta(), State: state}.Message().Render()
		assert.Contains(t, got, "Logs: /home/ming/.prdozer/runs/", "state %s must report the log dir", state)
	}
}

func TestRunReport_StatesRenderDistinctly(t *testing.T) {
	t.Parallel()
	cases := map[TerminalState]string{
		TerminalMerged:     "merged",
		TerminalClosed:     "PR closed",
		TerminalNeedsHuman: "needs a human",
		TerminalFailed:     "failed",
	}
	for state, want := range cases {
		got := RunReport{Meta: sampleMeta(), State: state}.Message().Render()
		assert.Contains(t, got, want, "state %s", state)
	}
}

func TestRunReport_KeptWorktreeIsSurfaced(t *testing.T) {
	t.Parallel()
	// GC skipped for unpushed commits must say so AND give the path — the disk
	// cost and the location can never be silent.
	got := RunReport{
		Meta:             sampleMeta(),
		State:            TerminalNeedsHuman,
		WorktreeKeptPath: "/home/ming/worktrees/kernel/.babysit/8123-a3f9",
	}.Message().Render()
	assert.Contains(t, got, "Worktree KEPT")
	assert.Contains(t, got, "/home/ming/worktrees/kernel/.babysit/8123-a3f9")
}

func TestCooldownWarning_IsVisibleAndNonTerminal(t *testing.T) {
	t.Parallel()
	// Unbounded retry is only acceptable if it stays visible: the loop keeps
	// trying, so it must keep telling you.
	until := time.Date(2026, 7, 29, 3, 0, 0, 0, time.UTC)
	report := CooldownWarning(sampleMeta(), until, "Pull request is in an unmergeable state")
	assert.True(t, report.Warning)

	got := report.Message().Render()
	assert.Contains(t, got, "⚠️")
	assert.Contains(t, got, "backed off", "a cooldown is not a terminal outcome")
	assert.Contains(t, got, "unmergeable state", "the real error must be included")
	assert.Contains(t, got, "2026-07-29T03:00:00Z", "say when it will retry")
	assert.Contains(t, got, "Merge attempts: 2", "say how many attempts have happened")
}

// The cooldown is tripped by ConsecutiveFailures, which a stalled polish round
// increments just as a rejected merge does — but only the merge path writes
// LastMergeError. Reporting that empty string verbatim gave the operator a
// warning that named no cause at all.
func TestCooldownWarning_NonMergeCauseIsStillNamed(t *testing.T) {
	t.Parallel()
	got := CooldownWarning(sampleMeta(), time.Date(2026, 7, 29, 3, 0, 0, 0, time.UTC), "").
		Message().Render()
	assert.NotContains(t, got, "Repeated merge failures",
		"a polish stall is not a merge failure; the wording must not assert one")
	assert.Contains(t, got, "not recorded",
		"an empty cause must say so rather than render as a blank line")
	assert.Contains(t, got, "run log", "and point at where the cause can be found")
}

func TestNotifyConfig_EmptyWebhookDisablesNotification(t *testing.T) {
	// An unconfigured deployment is a normal state, not an error.
	t.Setenv("PRDOZER_SLACK_WEBHOOK", "")
	assert.Nil(t, NotifyConfig{}.Notifier(),
		"no webhook configured and no env var set means notification is off")
}

func TestNotifyConfig_FallsBackToEnvVar(t *testing.T) {
	t.Setenv("PRDOZER_SLACK_WEBHOOK", "https://hooks.example/from-env")
	require.NotNil(t, NotifyConfig{}.Notifier(),
		"the env var is the zero-config path")
}

func TestNotifyConfig_ExpandsExplicitEnvReference(t *testing.T) {
	t.Setenv("MY_CUSTOM_HOOK", "https://hooks.example/custom")
	// A config file names the variable rather than embedding the secret.
	require.NotNil(t, NotifyConfig{SlackWebhook: "$MY_CUSTOM_HOOK"}.Notifier())
	// An unset variable disables rather than posting to a literal "$VAR".
	assert.Nil(t, NotifyConfig{SlackWebhook: "$DEFINITELY_UNSET_VAR_XYZ"}.Notifier())
}

func TestNotifyConfig_FallsBackToDMCommandWhenNoWebhook(t *testing.T) {
	t.Setenv("PRDOZER_SLACK_WEBHOOK", "")
	n := NotifyConfig{
		Target:        "@ming",
		DMCommand:     "/bin/echo",
		DMCommandArgs: []string{"--to", "{{recipient}}"},
	}.Notifier()
	require.NotNil(t, n, "a DM command is the zero-setup notification path")
	cmd, ok := n.(notify.CommandNotifier)
	require.True(t, ok, "expected a CommandNotifier, got %T", n)
	assert.Equal(t, "@ming", cmd.Recipient)
}

func TestNotifyConfig_WebhookWinsOverDMCommand(t *testing.T) {
	t.Setenv("PRDOZER_SLACK_WEBHOOK", "https://hooks.example/from-env")
	n := NotifyConfig{DMCommand: "/bin/echo"}.Notifier()
	require.NotNil(t, n)
	// The webhook has no interpreter or token-file dependency, so it is the
	// more robust sink for unattended runs and must take precedence.
	assert.IsType(t, notify.SlackWebhookNotifier{}, n)
}

func TestNotifyConfig_DMCommandExpandsHome(t *testing.T) {
	t.Setenv("PRDOZER_SLACK_WEBHOOK", "")
	// The test sandbox may not set HOME, and os.UserHomeDir then fails; pin it
	// so this asserts the expansion behaviour rather than the environment.
	t.Setenv("HOME", "/home/testuser")
	n := NotifyConfig{DMCommand: "~/bin/send"}.Notifier()
	require.NotNil(t, n)
	cmd, ok := n.(notify.CommandNotifier)
	require.True(t, ok)
	assert.Equal(t, "/home/testuser/bin/send", cmd.Path,
		"a config path must be expanded to something exec can run")
}

// The registry is shared across the fleet, where the home directory differs
// per box (the Azure devbox runs as "ming", the AWS boxes as "ubuntu"). A
// hardcoded /home/<user> path in an arg silently breaks on the other boxes —
// which is exactly how the first dispatched run failed to notify.
func TestNotifyConfig_DMCommandArgsExpandHome(t *testing.T) {
	t.Setenv("PRDOZER_SLACK_WEBHOOK", "")
	t.Setenv("HOME", "/home/testuser")
	n := NotifyConfig{
		DMCommand:     "/usr/bin/python3",
		DMCommandArgs: []string{"~/.claude/skills/slack-message/slack_send.py", "--to", "{{recipient}}"},
	}.Notifier()
	require.NotNil(t, n)
	cmd, ok := n.(notify.CommandNotifier)
	require.True(t, ok)
	assert.Equal(t, "/home/testuser/.claude/skills/slack-message/slack_send.py", cmd.Args[0])
	assert.Equal(t, "{{recipient}}", cmd.Args[2], "placeholders must survive expansion")
}

func TestReport_NilNotifierIsSafe(t *testing.T) {
	t.Parallel()
	// Report must be callable unconditionally at every terminal state.
	Report(context.Background(), nil, nil, RunReport{Meta: sampleMeta(), State: TerminalMerged})
}

func TestRunReport_OmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()
	// A fresh run with no rounds yet must not render blank "0" rows.
	meta := sampleMeta()
	meta.PolishRounds = 0
	meta.MergeAttempt = 0
	meta.TmuxSession = ""
	got := RunReport{Meta: meta, State: TerminalFailed}.Message().Render()
	assert.NotContains(t, got, "Polish rounds:")
	assert.NotContains(t, got, "Merge attempts:")
	assert.NotContains(t, got, "Attach:")
	// But the essentials are still there.
	assert.Contains(t, got, "Logs:")
	assert.True(t, strings.HasPrefix(got, "🚨"), "a failure still leads with the failure icon")
}
