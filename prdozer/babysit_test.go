package prdozer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/wt"
)

// busyProbeBlob is a heavily loaded box, so scoring prefers the other one.
const busyProbeBlob = `__NPROC__
8
__LOAD__
14.0 13.0 12.0 9/100 1
__DF__
Filesystem     1024-blocks      Used Available Capacity Mounted on
/dev/root        506771172 345397612 161357176      69% /
__TMUX__
90
__LEASES__
0
__PRDOZER__
/usr/bin/prdozer
__END__
`

// noPrdozerProbeBlob is a reachable box missing the binary.
const noPrdozerProbeBlob = `__NPROC__
8
__LOAD__
0.1 0.1 0.1 1/100 1
__DF__
Filesystem     1024-blocks      Used Available Capacity Mounted on
/dev/root        506771172 345397612 161357176      69% /
__TMUX__
1
__LEASES__
0
__PRDOZER__
MISSING
__END__
`

// writeFleetFixture creates a two-box fleet registry under home.
func writeFleetFixture(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, "magent", "fleet")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	write := func(name, hostname, dns string) {
		body := fmt.Sprintf(`{"cloud":"aws","cron_style":"legacy","hostname":%q,`+
			`"public_dns":%q,"registered":"2026-07-26","roles":"","ssh_user":"ubuntu","sync_offset":""}`,
			hostname, dns)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	write("self.json", "self-box", "self.example")
	write("other.json", "other-box", "other.example")
}

// fakeGit is a no-op GitRunner for tests that never reach real git.
type fakeGit struct{}

func (fakeGit) Run(_ context.Context, _ []string, _ string) (*wt.CmdResult, error) {
	return &wt.CmdResult{}, nil
}

func TestPlanDispatch_SelfHostRunsInProcess(t *testing.T) {
	// Never SSH to yourself: if the best box is the one you are on, run
	// in-process.
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFleetFixture(t, home)

	ssh := &fakeSSH{out: map[string]string{
		"ubuntu@self.example":  awsProbeBlob,
		"ubuntu@other.example": busyProbeBlob,
	}}
	plan, err := PlanDispatch(context.Background(), ssh, DispatchOptions{
		OwnerRepo: "o/r",
		PRNumber:  1,
		Probe:     ProbeOptions{SelfDNS: "self.example"},
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, "self-box", plan.Chosen.Host, "the idle box wins")
	assert.True(t, plan.RanLocal, "a self-host choice must run in-process, not over SSH")
}

func TestPlanDispatch_ProducesRunnableCommandAndScores(t *testing.T) {
	// --dry-run shares this decision path with the real dispatch: a dry run
	// that computes a different answer than the real thing is worse than none.
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFleetFixture(t, home)

	ssh := &fakeSSH{out: map[string]string{
		"ubuntu@self.example":  busyProbeBlob,
		"ubuntu@other.example": awsProbeBlob,
	}}
	plan, err := PlanDispatch(context.Background(), ssh, DispatchOptions{
		OwnerRepo:    "sycamore-labs/kernel",
		PRNumber:     8123,
		RegistryPath: "~/magent/prdozer/registry.yaml",
		Probe:        ProbeOptions{SelfDNS: "self.example"},
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, "other-box", plan.Chosen.Host)
	assert.False(t, plan.RanLocal)
	assert.Len(t, plan.Scores, 2, "the full score table must be available to print")

	assert.Contains(t, plan.Command, "ssh ")
	assert.Contains(t, plan.Command, "ubuntu@other.example")
	assert.Contains(t, plan.Command, "babysit-local")
	assert.Contains(t, plan.Command, "8123")
	assert.NotContains(t, plan.Command, "flock",
		"the worker takes the lease itself; flock(1) around tmux does not hold")
}

func TestPlanDispatch_PinnedHostIsHonoured(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFleetFixture(t, home)

	ssh := &fakeSSH{out: map[string]string{
		"ubuntu@self.example":  awsProbeBlob,
		"ubuntu@other.example": busyProbeBlob,
	}}
	// "other-box" is the busier machine, so only an explicit pin selects it.
	plan, err := PlanDispatch(context.Background(), ssh, DispatchOptions{
		OwnerRepo: "o/r", PRNumber: 1, Host: "other-box",
		Probe: ProbeOptions{SelfDNS: "self.example"},
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, "other-box", plan.Chosen.Host)
}

func TestPlanDispatch_UnknownPinnedHostErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFleetFixture(t, home)

	_, err := PlanDispatch(context.Background(), &fakeSSH{}, DispatchOptions{
		OwnerRepo: "o/r", PRNumber: 1, Host: "no-such-box",
	}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in the fleet registry")
}

func TestPlanDispatch_NoEligibleHostStillReturnsScores(t *testing.T) {
	// "No eligible host" is only actionable if the caller can print why each
	// candidate was rejected.
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFleetFixture(t, home)

	ssh := &fakeSSH{out: map[string]string{
		"ubuntu@self.example":  noPrdozerProbeBlob,
		"ubuntu@other.example": noPrdozerProbeBlob,
	}}
	plan, err := PlanDispatch(context.Background(), ssh, DispatchOptions{OwnerRepo: "o/r", PRNumber: 1}, nil)
	require.Error(t, err)
	assert.Len(t, plan.Scores, 2, "the score table must survive the error for printing")
	assert.Contains(t, err.Error(), "PATH")
}

func TestBabysitter_RefusesUnusableRepo(t *testing.T) {
	t.Parallel()
	b := NewBabysitter(newFakeGH(), &fakeGit{}, nil, nil, BabysitOptions{
		OwnerRepo: "sycamore-labs/sycaweave",
		PRNumber:  1,
		// No worktree_root: the repo has no local clone.
		Entry: RepoEntry{Flow: "pr-polish"},
	})
	state, err := b.Run(context.Background())
	require.Error(t, err)
	assert.Equal(t, TerminalFailed, state)
	assert.Contains(t, err.Error(), "worktree_root")
}

func TestBabysitter_RefusesWhenLeaseHeld(t *testing.T) {
	// A duplicate dispatch must not produce a second babysitter on one PR.
	home := t.TempDir()
	t.Setenv("HOME", home)

	held, err := AcquireLease("o/r", 42)
	require.NoError(t, err)
	t.Cleanup(func() { _ = held.Release() })

	// A USABLE entry, because Run checks the layout before it ever reaches the
	// lease: a bare temp dir fails that first check, and the test would then
	// pass on the wrong error while the lease went unexercised.
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "main", ".git"), 0o755))

	b := NewBabysitter(newFakeGH(), &fakeGit{}, nil, nil, BabysitOptions{
		OwnerRepo: "o/r",
		PRNumber:  42,
		Entry: RepoEntry{
			WorktreeRoot: root, Layout: LayoutWT, BaseBranch: "main", Flow: "pr-polish",
		},
	})
	_, err = b.Run(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrLeaseHeld)
}

func TestWatcherConfig_AutoMergeOnButPolicyDecides(t *testing.T) {
	t.Parallel()
	// AutoMerge is enabled so the loop can REACH a merge; the policy is what
	// decides whether anything lands. With "notify" nothing merges.
	b := NewBabysitter(nil, nil, nil, nil, BabysitOptions{
		PRNumber: 7,
		Entry: RepoEntry{
			MergePolicy: MergePolicyNotify,
			BaseBranch:  "main",
			Model:       "opus",
		},
	})
	cfg := b.watcherConfig(&RunContext{WorktreePath: "/tmp/wt"})
	assert.True(t, cfg.Polish.AutoMerge)
	assert.Equal(t, MergePolicyNotify, cfg.Polish.MergePolicy)
	assert.Equal(t, "/tmp/wt", cfg.WorkDir)
	assert.Equal(t, "opus", cfg.Agent.Model)
	assert.Equal(t, []int{7}, cfg.Source.PRs)
}

func TestTerminalFor(t *testing.T) {
	t.Parallel()
	pr := DiscoveredPR{Number: 42}
	cases := []struct {
		action   LastAction
		want     TerminalState
		terminal bool
	}{
		{LastActionMerged, TerminalMerged, true},
		{LastActionClosed, TerminalClosed, true},
		{LastActionNeedsHuman, TerminalNeedsHuman, true},
		// Non-terminal: the loop keeps going.
		{LastActionPolished, "", false},
		{LastActionReworked, "", false},
		{LastActionArmed, "", false},
		{LastActionIdle, "", false},
		{LastActionFailed, "", false},
	}
	for _, tc := range cases {
		t.Run(string(tc.action), func(t *testing.T) {
			t.Parallel()
			got, _, done := terminalFor(TickResult{Action: tc.action}, pr)
			assert.Equal(t, tc.terminal, done)
			if tc.terminal {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

// A divergence stop and an approval block are both LastActionNeedsHuman but
// call for opposite responses, so the message must tell them apart — and must
// quote the streak the guard tripped on, not the run's cumulative round count.
func TestTerminalFor_DivergedSaysWhy(t *testing.T) {
	t.Parallel()
	pr := DiscoveredPR{Number: 42}

	state, msg, done := terminalFor(TickResult{
		Action: LastActionNeedsHuman, Diverged: true,
		RoundsSinceImprovement: 3, PolishRounds: 17,
	}, pr)
	require.True(t, done)
	assert.Equal(t, TerminalNeedsHuman, state)
	assert.Contains(t, msg, "stopped improving")
	assert.Contains(t, msg, "3 polish rounds produced no better result",
		"must report the flat streak, not the 17 rounds the run did in total")
	assert.Contains(t, msg, "17 rounds total")

	_, plain, done := terminalFor(TickResult{Action: LastActionNeedsHuman}, pr)
	require.True(t, done)
	assert.NotContains(t, plain, "stopped improving",
		"a PR blocked on an approval must not be reported as diverging")
}

func TestTerminalFor_ArmedIsNotTerminal(t *testing.T) {
	t.Parallel()
	// --auto only ARMS the merge queue; the PR has not landed. Treating this
	// as terminal would report "merged" for a PR still sitting in the queue.
	_, _, done := terminalFor(TickResult{Action: LastActionArmed}, DiscoveredPR{Number: 1})
	assert.False(t, done, "an armed auto-merge must keep polling until .merged is true")
}
