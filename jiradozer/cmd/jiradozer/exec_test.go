package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/cliapp"
	"github.com/bazelment/yoloswe/jiradozer"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
	"github.com/bazelment/yoloswe/wt"
)

func execTestApp(t *testing.T) *cliapp.App {
	return &cliapp.App{Logger: testMainLogger(t)}
}

// Running under an orchestrator would silence every failure on the fleet: the
// variable tells a child to suppress its own report because a parent will
// speak for it, and exec has no parent.
func TestExecRefusesToRunUnderAnOrchestrator(t *testing.T) {
	t.Setenv(jiradozer.OrchestratedEnvVar, "1")

	err := runExec(context.Background(), execTestApp(t), execArgs{issueID: "INF-1", repo: "kernel"})
	require.Error(t, err)
	require.Contains(t, err.Error(), jiradozer.OrchestratedEnvVar)
}

func TestExecRequiresExactlyOneTaskSource(t *testing.T) {
	t.Setenv(jiradozer.OrchestratedEnvVar, "")

	err := runExec(context.Background(), execTestApp(t), execArgs{repo: "kernel"})
	require.ErrorContains(t, err, "exactly one of --issue or --description")

	err = runExec(context.Background(), execTestApp(t), execArgs{
		repo: "kernel", issueID: "INF-1", description: "do a thing"})
	require.ErrorContains(t, err, "exactly one of --issue or --description")
}

// --repo cannot be inferred: a dispatched worker starts under tmux with cwd
// $HOME, which is neither a wt worktree nor a git repo. Failing here with a
// clear message beats failing later inside wt.
func TestResolveExecWTManagerRequiresRepo(t *testing.T) {
	_, err := resolveExecWTManager("")
	require.ErrorContains(t, err, "--repo is required")

	t.Setenv("WT_ROOT", t.TempDir())
	mgr, err := resolveExecWTManager("kernel")
	require.NoError(t, err)
	require.NotNil(t, mgr)
}

// The lock label is the ONLY cross-host claim. A flock lease is per-box, so
// without this check two machines happily take the same issue.
func TestCheckNotClaimedBlocksAForeignClaim(t *testing.T) {
	claimed := &tracker.Issue{
		Identifier: "INF-1",
		Labels:     []string{"ming-work", jiradozer.LockLabel},
	}

	x := &execRun{}
	err := x.checkNotClaimed(claimed)
	require.Error(t, err)
	require.Contains(t, err.Error(), jiradozer.LockLabel)
	require.Contains(t, err.Error(), "--force", "the message must say how to override a stale claim")

	// --force is the documented escape hatch for a claim left by a dead run.
	forced := &execRun{args: execArgs{force: true}}
	require.NoError(t, forced.checkNotClaimed(claimed))

	unclaimed := &execRun{}
	require.NoError(t, unclaimed.checkNotClaimed(&tracker.Issue{
		Identifier: "INF-2", Labels: []string{"ming-work"}}))
}

// A lease is held INSIDE the worker for its whole lifetime, so a second worker
// on the same box cannot take the same task.
func TestLeaseExcludesASecondWorkerOnThisBox(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	first, err := jiradozer.AcquireLease("INF-1")
	require.NoError(t, err)

	_, err = jiradozer.AcquireLease("INF-1")
	require.ErrorIs(t, err, jiradozer.ErrLeaseHeld)

	// A different task is unaffected.
	other, err := jiradozer.AcquireLease("INF-2")
	require.NoError(t, err)
	require.NoError(t, other.Release())

	require.NoError(t, first.Release())

	// Release leaves the lock FILE behind on purpose — removing it races a
	// process about to flock it. So the file existing must not block a retake;
	// only a held lock does. Counting files instead of held locks once made
	// every host exclude itself permanently.
	retaken, err := jiradozer.AcquireLease("INF-1")
	require.NoError(t, err, "a leftover lock file must not look like a held lease")
	require.NoError(t, retaken.Release())
}

// gc keys reclamation SOLELY on the recorded PR URL, and exec always keeps its
// worktree. A run that finishes without recording one therefore leaks its
// worktree permanently — the exact leak `jiradozer gc` exists to prevent.
func TestFinishRecordsThePRSoGCCanReclaimTheWorktree(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	x := newFinishTestRun(t)
	x.lookupPR = func(_ context.Context, branch, dir string) (*wt.PRInfo, error) {
		require.Equal(t, "jiradozer/INF-1", branch, "the PR is looked up by this run's branch")
		require.Equal(t, x.cfg.WorkDir, dir, "the lookup must run inside the worktree")
		return &wt.PRInfo{URL: "https://github.com/o/r/pull/42", Number: 42}, nil
	}

	x.finish(nil)

	m := x.rl.Meta()
	assert.Equal(t, "https://github.com/o/r/pull/42", m.PRURL)
	assert.Equal(t, 42, m.PRNumber)
	assert.True(t, m.WorktreeKept)
	assert.Equal(t, jiradozer.RunStateDone, m.State)

	// The record on disk is what a sweeper on this box actually reads.
	onDisk, err := jiradozer.LoadRunMeta(x.rl.Dir())
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/o/r/pull/42", onDisk.PRURL)
}

// A run that failed before create_pr legitimately has no PR. That must settle
// the run-log normally rather than abort the exit path.
func TestFinishToleratesARunWithNoPR(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	x := newFinishTestRun(t)
	x.lookupPR = func(context.Context, string, string) (*wt.PRInfo, error) {
		return nil, errors.New("no pull requests found for branch")
	}

	x.finish(errors.New("build: agent exited 1"))

	m := x.rl.Meta()
	assert.Empty(t, m.PRURL)
	assert.Equal(t, jiradozer.RunStateFailed, m.State)
	assert.Contains(t, m.Error, "agent exited 1")
	assert.True(t, m.WorktreeKept, "a failed run keeps its worktree; the work exists nowhere else")
}

// A cancellation is a stop, not a failure: recording it as failed would make
// every Ctrl-C look like a broken run to a fleet-wide listing.
func TestFinishRecordsACancellationAsCancelled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	x := newFinishTestRun(t)
	x.finish(fmt.Errorf("plan: %w", context.Canceled))

	assert.Equal(t, jiradozer.RunStateCancelled, x.rl.Meta().State)
}

func newFinishTestRun(t *testing.T) *execRun {
	t.Helper()
	runID, err := jiradozer.NewRunID()
	require.NoError(t, err)
	rl, err := jiradozer.NewRunLog(jiradozer.RunMeta{
		RunID:           runID,
		IssueIdentifier: "INF-1",
		Repo:            "kernel",
		Branch:          "jiradozer/INF-1",
		WorktreePath:    t.TempDir(),
		State:           jiradozer.RunStateRunning,
	})
	require.NoError(t, err)
	return &execRun{
		app:    execTestApp(t),
		logger: testMainLogger(t),
		cfg:    &jiradozer.Config{WorkDir: t.TempDir()},
		rl:     rl,
		runID:  runID,
		branch: "jiradozer/INF-1",
		lookupPR: func(context.Context, string, string) (*wt.PRInfo, error) {
			return nil, errors.New("no pull requests found for branch")
		},
	}
}

// Phase is what makes a dispatched worker's progress readable from another box:
// `jiradozer fleet runs` reads meta.json over ssh and can see neither this
// host's log nor its terminal.
func TestRecordPhaseMirrorsWorkflowStepsIntoTheRunLog(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	x := newFinishTestRun(t)
	require.Empty(t, x.rl.Meta().Phase)

	x.recordPhase(jiradozer.StepBuilding)

	// The step's own name, not its numeric value: a WorkflowStep is an int, so
	// a plain conversion would write an unprintable rune into the listing.
	assert.Equal(t, jiradozer.StepBuilding.String(), x.rl.Meta().Phase)
	assert.NotEmpty(t, x.rl.Meta().Phase)

	onDisk, err := jiradozer.LoadRunMeta(x.rl.Dir())
	require.NoError(t, err)
	assert.Equal(t, jiradozer.StepBuilding.String(), onDisk.Phase,
		"a remote reader sees meta.json, never this process's memory")
}

// dispatch refuses a duplicate by asking which box holds this task's lease —
// a check that runs BEFORE the worker exists. It can therefore only work if
// both sides derive the same name, which is why this is one function.
func TestLeaseTargetIsDerivedIdenticallyForDispatchAndExec(t *testing.T) {
	assert.Equal(t, "INF-1234", leaseTarget(execArgs{issueID: "INF-1234", taskID: "t-7"}),
		"an issue identifier wins: it is the cross-host claim")
	assert.Equal(t, "t-7", leaseTarget(execArgs{taskID: "t-7", description: "tidy things"}))
	assert.Equal(t, "", leaseTarget(execArgs{}))
}

// A --description run with no --task-id used to lease a per-run random name, so
// `dispatch` had nothing to look for and two concurrent dispatches of the same
// task both proceeded — duplicate worktrees, duplicate PRs.
func TestLeaseTargetIsStableForADescriptionRun(t *testing.T) {
	x := execArgs{description: "tidy the helm chart"}
	first := leaseTarget(x)
	assert.NotEmpty(t, first)
	assert.Equal(t, first, leaseTarget(x), "the same task must lease the same name every time")
	assert.Equal(t, first, leaseTarget(execArgs{description: "  tidy the helm chart\n"}),
		"incidental whitespace must not make a task look new")
	assert.NotEqual(t, first, leaseTarget(execArgs{description: "tidy the ingress"}))

	// It becomes a lock FILENAME, so free-form text must not survive verbatim.
	assert.NotContains(t, leaseTarget(execArgs{description: "fix /etc/hosts\nand more"}), "/")
	assert.NotContains(t, leaseTarget(execArgs{description: "fix /etc/hosts\nand more"}), "\n")
}

// An alert with no target names nothing an on-call human can act on.
func TestReportTargetIsNeverAnonymous(t *testing.T) {
	assert.Equal(t, "INF-9", (&execRun{args: execArgs{issueID: "INF-9"}}).reportTarget(),
		"a failure before the issue fetch must still name the issue")
	assert.Equal(t, "INF-9", (&execRun{
		issue: &tracker.Issue{Identifier: "INF-9"},
		args:  execArgs{issueID: "inf-9"},
	}).reportTarget(), "the resolved identifier wins once it is known")
	assert.Equal(t, "t-7", (&execRun{args: execArgs{taskID: "t-7"}}).reportTarget())
	assert.Equal(t, "tidy the helm chart",
		(&execRun{args: execArgs{description: "tidy the helm chart"}}).reportTarget())
}

// A GitHub-style identifier must not turn a branch into nested path segments.
func TestSanitizeBranchLeaf(t *testing.T) {
	require.Equal(t, "acme-app-42", sanitizeBranchLeaf("acme/app#42"))
	require.Equal(t, "INF-1234", sanitizeBranchLeaf("INF-1234"))
	require.Equal(t, "a-b", sanitizeBranchLeaf("a b"))
}
