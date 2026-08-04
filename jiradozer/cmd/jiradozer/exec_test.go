package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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

// gc's liveness guard reads the lease name off the run-log. If exec does not
// write it, the guard asks about the wrong lock for every --description run and
// answers "not held" about a live worker.
func TestStartRunLogRecordsTheLeaseItHolds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	x := &execRun{
		app:    execTestApp(t),
		logger: testMainLogger(t),
		cfg:    &jiradozer.Config{WorkDir: t.TempDir(), BaseBranch: "main"},
		args:   execArgs{description: "tidy the helm chart", repo: "kernel"},
		runID:  "r1",
		branch: "jiradozer/r1",
	}
	require.NoError(t, x.startRunLog())

	want := leaseTarget(x.args)
	require.NotEmpty(t, want)
	assert.Equal(t, want, x.rl.Meta().LeaseTarget)
	assert.Equal(t, want, x.rl.Meta().LeaseKey(),
		"before a local issue exists the lease name is the only name this run has")

	onDisk, err := jiradozer.LoadRunMeta(x.rl.Dir())
	require.NoError(t, err)
	assert.Equal(t, want, onDisk.LeaseTarget, "gc reads meta.json, not this process's memory")
}

// The run-log IS gc's ownership namespace: these are ordinary wt worktrees
// sitting beside human-owned ones, so a worktree no run-log claims is never a
// candidate. A worktree that outlives a failed startRunLog is therefore
// orphaned permanently — and nothing has run in it yet, so tearing it down
// loses nothing.
func TestAWorktreeIsNotLeftBehindWhenTheRunLogCannotBeCreated(t *testing.T) {
	var removed []string
	x := &execRun{
		logger: testMainLogger(t),
		cfg:    &jiradozer.Config{WorkDir: t.TempDir()},
		branch: "jiradozer/INF-1",
		removeWorktree: func(_ context.Context, branch string) error {
			removed = append(removed, branch)
			return nil
		},
	}

	x.discardUnclaimedWorktree(context.Background())

	assert.Equal(t, []string{"jiradozer/INF-1"}, removed,
		"the checkout must be removed by branch, through the worktree manager")
}

// If the teardown itself fails the directory is now invisible to gc and only a
// human can find it, so the path must survive into the log rather than be
// swallowed.
func TestAFailedTeardownIsReportedNotSwallowed(t *testing.T) {
	var logged bytes.Buffer
	x := &execRun{
		logger: slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelError})),
		cfg:    &jiradozer.Config{WorkDir: "/roots/kernel/jiradozer-INF-1"},
		branch: "jiradozer/INF-1",
		removeWorktree: func(context.Context, string) error {
			return errors.New("worktree is locked")
		},
	}

	x.discardUnclaimedWorktree(context.Background())

	out := logged.String()
	assert.Contains(t, out, "/roots/kernel/jiradozer-INF-1", "a human needs the path to find it")
	assert.Contains(t, out, "worktree is locked")
}

// recordingGit captures the git command lines a wt.Manager issues, so a test
// can assert on the flags that actually reach git rather than on a stand-in
// remover that can never refuse.
type recordingGit struct{ cmds [][]string }

func (g *recordingGit) Run(_ context.Context, args []string, _ string) (*wt.CmdResult, error) {
	g.cmds = append(g.cmds, args)
	if len(args) > 1 && args[0] == "branch" && args[1] == "--show-current" {
		return &wt.CmdResult{Stdout: "jiradozer/INF-1\n"}, nil
	}
	return &wt.CmdResult{}, nil
}

// The teardown must pass --force. wt.New runs post-create hooks in the fresh
// checkout, and hooks routinely leave untracked build output; an unforced
// `git worktree remove` refuses on exactly that. The refusal would strand a
// worktree no run-log claims, which is the one thing gc can never find. A test
// against an injected remover cannot see this — only the real flags can.
func TestDiscardingAnUnclaimedWorktreeForcesPastHookOutput(t *testing.T) {
	root := t.TempDir()
	repo := "kernel"
	branch := "jiradozer/INF-1"
	require.NoError(t, os.MkdirAll(filepath.Join(root, repo, branch), 0o755))

	git := &recordingGit{}
	mgr := wt.NewManager(root, repo, wt.WithGitRunner(git), wt.WithOutput(wt.NewOutput(io.Discard, false)))

	require.NoError(t, discardRemover(mgr)(context.Background(), branch))

	var removeCmd []string
	for _, c := range git.cmds {
		if len(c) > 1 && c[0] == "worktree" && c[1] == "remove" {
			removeCmd = c
			break
		}
	}
	require.NotNil(t, removeCmd, "the teardown must issue a worktree remove")
	assert.Contains(t, removeCmd, "--force",
		"an unforced removal refuses on post-create hook output and leaks the worktree")
}

// Nothing to discard must be a no-op, not a removal against an empty branch
// name — wt would resolve that to some other worktree.
func TestDiscardUnclaimedWorktreeIsANoOpBeforeAWorktreeExists(t *testing.T) {
	called := false
	x := &execRun{
		logger:         testMainLogger(t),
		cfg:            &jiradozer.Config{},
		removeWorktree: func(context.Context, string) error { called = true; return nil },
	}

	x.discardUnclaimedWorktree(context.Background())
	x.branch = "jiradozer/INF-1" // branch known, but no worktree created yet
	x.discardUnclaimedWorktree(context.Background())

	assert.False(t, called, "nothing may be removed before a worktree exists")
}
