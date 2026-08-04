package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/cliapp"
	"github.com/bazelment/yoloswe/jiradozer"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
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

// A GitHub-style identifier must not turn a branch into nested path segments.
func TestSanitizeBranchLeaf(t *testing.T) {
	require.Equal(t, "acme-app-42", sanitizeBranchLeaf("acme/app#42"))
	require.Equal(t, "INF-1234", sanitizeBranchLeaf("INF-1234"))
	require.Equal(t, "a-b", sanitizeBranchLeaf("a b"))
}
