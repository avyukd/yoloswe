package jiradozer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/wt"
)

type fakePRChecker struct {
	merged map[string]bool
	err    error
}

func (f fakePRChecker) Merged(_ context.Context, prURL string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.merged[prURL], nil
}

type fakeRemover struct{ removed []string }

func (r *fakeRemover) RemoveWorktree(_ context.Context, nameOrBranch string, _ bool) error {
	r.removed = append(r.removed, nameOrBranch)
	return nil
}

type fakeGit struct{ dirty map[string]bool }

func (g fakeGit) Run(_ context.Context, _ []string, dir string) (*wt.CmdResult, error) {
	if g.dirty[dir] {
		return &wt.CmdResult{Stdout: " M some/file.go\n"}, nil
	}
	return &wt.CmdResult{Stdout: ""}, nil
}

// gcFixture seeds one run with a real directory standing in for its worktree.
func gcFixture(t *testing.T, m RunMeta) (RunMeta, string) {
	t.Helper()
	wtPath := filepath.Join(t.TempDir(), "worktree")
	require.NoError(t, os.MkdirAll(wtPath, 0o755))
	m.WorktreePath = wtPath
	rl, err := NewRunLog(m)
	require.NoError(t, err)
	if m.State.IsTerminal() {
		require.NoError(t, rl.Finish(m.State, "", nil))
	}
	return rl.Meta(), wtPath
}

func gcDeps(pr PRChecker, rm *fakeRemover, dirty map[string]bool) GCDeps {
	return GCDeps{
		Git:       fakeGit{dirty: dirty},
		PR:        pr,
		Removers:  map[string]WorktreeRemover{"kernel": rm},
		LeaseHeld: func(string) bool { return false },
	}
}

// The load-bearing rule: a run finishing successfully means "a PR is open",
// never "it merged". Reclaiming on terminal state would delete the branch's
// only checkout while the PR was still awaiting review.
func TestGCDoesNotReclaimOnTerminalStateAlone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, wtPath := gcFixture(t, RunMeta{
		RunID: "r1", IssueIdentifier: "INF-1", Repo: "kernel", Branch: "feature/INF-1",
		State: RunStateDone, PRURL: "https://github.com/o/r/pull/1",
	})

	rm := &fakeRemover{}
	res, err := RunGC(context.Background(),
		gcDeps(fakePRChecker{merged: map[string]bool{}}, rm, nil),
		GCOptions{Apply: true}, nil)
	require.NoError(t, err)

	require.Empty(t, rm.removed, "an unmerged PR's worktree must survive a done run")
	require.Zero(t, res.Removed)
	requireReason(t, res, "worktree", "PR has not merged")
	require.DirExists(t, wtPath)
}

func TestGCReclaimsOnceThePRMerges(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gcFixture(t, RunMeta{
		RunID: "r1", IssueIdentifier: "INF-1", Repo: "kernel", Branch: "feature/INF-1",
		State: RunStateDone, PRURL: "https://github.com/o/r/pull/1",
	})

	rm := &fakeRemover{}
	deps := gcDeps(fakePRChecker{merged: map[string]bool{"https://github.com/o/r/pull/1": true}}, rm, nil)

	// Preview first: eligible, but nothing touched.
	res, err := RunGC(context.Background(), deps, GCOptions{}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, res.Eligible)
	require.Zero(t, res.Removed, "a preview must never remove")
	require.Empty(t, rm.removed)

	res, err = RunGC(context.Background(), deps, GCOptions{Apply: true}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, res.Removed)
	require.Equal(t, []string{"feature/INF-1"}, rm.removed,
		"removal must go through the worktree manager, not rm -rf")
}

// An unreachable GitHub is not evidence that a PR landed.
func TestGCFailsClosedWhenPRStateIsUnknown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gcFixture(t, RunMeta{
		RunID: "r1", IssueIdentifier: "INF-1", Repo: "kernel", Branch: "feature/INF-1",
		State: RunStateDone, PRURL: "https://github.com/o/r/pull/1",
	})

	rm := &fakeRemover{}
	res, err := RunGC(context.Background(),
		gcDeps(fakePRChecker{err: errors.New("gh: network unreachable")}, rm, nil),
		GCOptions{Apply: true}, nil)
	require.NoError(t, err)
	require.Empty(t, rm.removed)
	requireReason(t, res, "worktree", "could not determine PR state")
}

// A live worker must never lose its directory, whatever its meta says.
func TestGCSkipsAWorktreeWhoseLeaseIsHeld(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gcFixture(t, RunMeta{
		RunID: "r1", IssueIdentifier: "INF-1", Repo: "kernel", Branch: "feature/INF-1",
		State: RunStateDone, PRURL: "https://github.com/o/r/pull/1",
	})

	rm := &fakeRemover{}
	deps := gcDeps(fakePRChecker{merged: map[string]bool{"https://github.com/o/r/pull/1": true}}, rm, nil)
	deps.LeaseHeld = func(string) bool { return true }

	res, err := RunGC(context.Background(), deps, GCOptions{Apply: true}, nil)
	require.NoError(t, err)
	require.Empty(t, rm.removed)
	requireReason(t, res, "worktree", "holds this task's lease")
}

// A run that died without settling may hold the only copy of its work.
func TestGCLeavesADeadRunForAHuman(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gcFixture(t, RunMeta{
		RunID: "r1", IssueIdentifier: "INF-1", Repo: "kernel", Branch: "feature/INF-1",
		State: RunStateRunning, PRURL: "https://github.com/o/r/pull/1",
	})

	rm := &fakeRemover{}
	res, err := RunGC(context.Background(),
		gcDeps(fakePRChecker{merged: map[string]bool{"https://github.com/o/r/pull/1": true}}, rm, nil),
		GCOptions{Apply: true}, nil)
	require.NoError(t, err)
	require.Empty(t, rm.removed)
	requireReason(t, res, "worktree", "inspect before reclaiming")
}

// A squash carries every commit, but nothing preserves an uncommitted edit.
func TestGCRefusesAWorktreeWithUncommittedWork(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, wtPath := gcFixture(t, RunMeta{
		RunID: "r1", IssueIdentifier: "INF-1", Repo: "kernel", Branch: "feature/INF-1",
		State: RunStateDone, PRURL: "https://github.com/o/r/pull/1",
	})

	rm := &fakeRemover{}
	merged := fakePRChecker{merged: map[string]bool{"https://github.com/o/r/pull/1": true}}
	deps := gcDeps(merged, rm, map[string]bool{wtPath: true})

	res, err := RunGC(context.Background(), deps, GCOptions{Apply: true}, nil)
	require.NoError(t, err)
	require.Empty(t, rm.removed)
	requireReason(t, res, "worktree", "uncommitted changes")

	// --force is the deliberate override.
	res, err = RunGC(context.Background(), deps, GCOptions{Apply: true, Force: true}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, res.Removed)
}

func TestGCWithoutARecordedPRWillNotGuess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gcFixture(t, RunMeta{
		RunID: "r1", IssueIdentifier: "INF-1", Repo: "kernel", Branch: "feature/INF-1",
		State: RunStateDone,
	})

	rm := &fakeRemover{}
	res, err := RunGC(context.Background(), gcDeps(fakePRChecker{}, rm, nil), GCOptions{Apply: true}, nil)
	require.NoError(t, err)
	require.Empty(t, rm.removed)
	requireReason(t, res, "worktree", "no PR recorded")
}

// The run-log is the only thing marking a directory as jiradozer's, so it must
// outlive the worktree or the worktree is orphaned forever.
func TestGCKeepsARunLogWhileItsWorktreeExists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, _ := gcFixture(t, RunMeta{
		RunID: "r1", IssueIdentifier: "INF-1", Repo: "kernel", Branch: "feature/INF-1",
		State: RunStateDone,
	})
	require.NotEmpty(t, m.LogDir)

	res, err := RunGC(context.Background(), gcDeps(fakePRChecker{}, &fakeRemover{}, nil),
		GCOptions{Apply: true, RunLogTTL: time.Nanosecond}, nil)
	require.NoError(t, err)
	require.DirExists(t, m.LogDir)
	requireReason(t, res, "runlog", "only ownership record")
}

func TestGCExpiresARunLogOnceItsWorktreeIsGone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, wtPath := gcFixture(t, RunMeta{
		RunID: "r1", IssueIdentifier: "INF-1", Repo: "kernel", Branch: "feature/INF-1",
		State: RunStateDone,
	})
	require.NoError(t, os.RemoveAll(wtPath))

	res, err := RunGC(context.Background(), gcDeps(fakePRChecker{}, &fakeRemover{}, nil),
		GCOptions{Apply: true, RunLogTTL: time.Nanosecond}, nil)
	require.NoError(t, err)
	require.NoDirExists(t, m.LogDir)
	require.GreaterOrEqual(t, res.Removed, 1)
}

// A worktree with no run-log is somebody's, not jiradozer's.
func TestGCNeverConsidersAnUnclaimedWorktree(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	humanWorktree := filepath.Join(t.TempDir(), "my-important-work")
	require.NoError(t, os.MkdirAll(humanWorktree, 0o755))

	res, err := RunGC(context.Background(), gcDeps(fakePRChecker{}, &fakeRemover{}, nil),
		GCOptions{Apply: true}, nil)
	require.NoError(t, err)
	require.Empty(t, res.Candidates, "nothing unclaimed may even be considered")
	require.DirExists(t, humanWorktree)
}

func requireReason(t *testing.T, res GCResult, kind, substr string) {
	t.Helper()
	for i := range res.Candidates {
		c := &res.Candidates[i]
		if c.Kind == kind && strings.Contains(c.Reason, substr) {
			return
		}
	}
	t.Fatalf("no %s candidate with reason containing %q; got %+v", kind, substr, res.Candidates)
}
