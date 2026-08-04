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

type fakeRemover struct {
	removed []string
	// forced records the force flag of each removal, so a test can assert the
	// operator's --force actually reaches git rather than stopping at gc's own
	// dirty-worktree check.
	forced []bool
}

func (r *fakeRemover) RemoveWorktree(_ context.Context, nameOrBranch string, _, force bool) error {
	r.removed = append(r.removed, nameOrBranch)
	r.forced = append(r.forced, force)
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
		Git: fakeGit{dirty: dirty},
		PR:  pr,
		// Keyed the way gc looks it up: worktree root AND repo. The fixtures
		// leave WTRoot empty, which is what a pre-WTRoot record looks like.
		Removers:  map[string]WorktreeRemover{RunMeta{Repo: "kernel"}.RemoverKey(): rm},
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

	// --force is the deliberate override, and it has to reach git. Waving the
	// dirty check through while still calling an unforced `git worktree remove`
	// would leave --force advertising a reclaim it can never perform.
	res, err = RunGC(context.Background(), deps, GCOptions{Apply: true, Force: true}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, res.Removed)
	require.Equal(t, []bool{true}, rm.forced, "--force must be forwarded to the removal itself")
}

// The unforced path must stay unforced: a clean reclaim that silently forced
// would delete work the dirty check exists to protect.
func TestGCDoesNotForceARoutineReclaim(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gcFixture(t, RunMeta{
		RunID: "r1", IssueIdentifier: "INF-1", Repo: "kernel", Branch: "feature/INF-1",
		State: RunStateDone, PRURL: "https://github.com/o/r/pull/1",
	})

	rm := &fakeRemover{}
	deps := gcDeps(fakePRChecker{merged: map[string]bool{"https://github.com/o/r/pull/1": true}}, rm, nil)

	_, err := RunGC(context.Background(), deps, GCOptions{Apply: true}, nil)
	require.NoError(t, err)
	require.Equal(t, []bool{false}, rm.forced)
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

type fakePRResolver struct {
	byBranch map[string]string
	err      error
	calls    int
}

func (f *fakePRResolver) ResolveForBranch(_ context.Context, branch, _ string) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return f.byBranch[branch], nil
}

// A run records its PR at shutdown, which is exactly when a transient gh
// failure is most likely — and nothing ever revisits a terminal run's meta. So
// without recovery here, one bad minute strands a worktree on disk forever.
func TestGCRecoversAPRTheRunFailedToRecord(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gcFixture(t, RunMeta{
		RunID: "r1", IssueIdentifier: "INF-1", Repo: "kernel", Branch: "feature/INF-1",
		State: RunStateDone, // no PRURL: the shutdown lookup failed
	})

	rm := &fakeRemover{}
	resolver := &fakePRResolver{byBranch: map[string]string{
		"feature/INF-1": "https://github.com/o/r/pull/1",
	}}
	deps := gcDeps(fakePRChecker{merged: map[string]bool{"https://github.com/o/r/pull/1": true}}, rm, nil)
	deps.PRByBranch = resolver

	res, err := RunGC(context.Background(), deps, GCOptions{Apply: true}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, res.Removed)
	require.Equal(t, []string{"feature/INF-1"}, rm.removed)
	require.Equal(t, 1, resolver.calls)
}

// Recovery must not become a way to guess. A branch with no PR, and a resolver
// that itself fails, both leave the worktree exactly where it is.
func TestGCRecoveryStillFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		resolver *fakePRResolver
		name     string
	}{
		{name: "branch has no PR", resolver: &fakePRResolver{byBranch: map[string]string{}}},
		{name: "resolver itself fails", resolver: &fakePRResolver{err: errors.New("gh: network unreachable")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			gcFixture(t, RunMeta{
				RunID: "r1", IssueIdentifier: "INF-1", Repo: "kernel", Branch: "feature/INF-1",
				State: RunStateDone,
			})

			rm := &fakeRemover{}
			deps := gcDeps(fakePRChecker{}, rm, nil)
			deps.PRByBranch = tc.resolver

			res, err := RunGC(context.Background(), deps, GCOptions{Apply: true}, nil)
			require.NoError(t, err)
			require.Empty(t, rm.removed)
			requireReason(t, res, "worktree", "no PR recorded")
		})
	}
}

// A recorded PR is authoritative: recovery is for a MISSING one, and re-asking
// would cost a gh round trip per candidate on every sweep.
func TestGCDoesNotReResolveARecordedPR(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gcFixture(t, RunMeta{
		RunID: "r1", IssueIdentifier: "INF-1", Repo: "kernel", Branch: "feature/INF-1",
		State: RunStateDone, PRURL: "https://github.com/o/r/pull/1",
	})

	rm := &fakeRemover{}
	resolver := &fakePRResolver{byBranch: map[string]string{"feature/INF-1": "https://github.com/o/r/pull/9"}}
	deps := gcDeps(fakePRChecker{merged: map[string]bool{"https://github.com/o/r/pull/1": true}}, rm, nil)
	deps.PRByBranch = resolver

	res, err := RunGC(context.Background(), deps, GCOptions{Apply: true}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, res.Removed)
	require.Zero(t, resolver.calls, "the recorded PR must be used as-is")
}

// The liveness guard must ask about the lock the worker actually holds. A
// --description run leases a name derived from its description, then acquires a
// local-tracker identifier that Target() reports instead — so asking by Target
// answers "not held" about a live worker, which reads as permission to delete
// its directory.
func TestGCAsksAboutTheLockAWorkerActuallyHolds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gcFixture(t, RunMeta{
		RunID: "r1", Repo: "kernel", Branch: "jiradozer/r1",
		IssueIdentifier: "LOCAL-1", LeaseTarget: "adhoc-deadbeefcafe",
		State: RunStateDone, PRURL: "https://github.com/o/r/pull/1",
	})

	var asked []string
	rm := &fakeRemover{}
	deps := gcDeps(fakePRChecker{merged: map[string]bool{"https://github.com/o/r/pull/1": true}}, rm, nil)
	deps.LeaseHeld = func(target string) bool {
		asked = append(asked, target)
		return target == "adhoc-deadbeefcafe"
	}

	res, err := RunGC(context.Background(), deps, GCOptions{Apply: true}, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"adhoc-deadbeefcafe"}, asked)
	require.Empty(t, rm.removed, "a live worker's worktree must survive the sweep")
	requireReason(t, res, "worktree", "holds this task's lease")
}

// Records written before LeaseTarget existed still have to be swept, and for
// issue and task runs the two names did coincide.
func TestLeaseKeyFallsBackToTargetForOlderRecords(t *testing.T) {
	require.Equal(t, "adhoc-abc", RunMeta{IssueIdentifier: "LOCAL-1", LeaseTarget: "adhoc-abc"}.LeaseKey())
	require.Equal(t, "INF-1", RunMeta{IssueIdentifier: "INF-1"}.LeaseKey())
	require.Equal(t, "t-7", RunMeta{TaskID: "t-7"}.LeaseKey())
	require.Equal(t, "r1", RunMeta{RunID: "r1"}.LeaseKey())
}

// Every name an operator is ever shown has to find the run again — dispatch
// prints the lease name, the tracker comment prints the identifier.
func TestMatchesAcceptsEveryNameARunIsKnownBy(t *testing.T) {
	m := RunMeta{IssueIdentifier: "LOCAL-1", TaskID: "t-7", LeaseTarget: "adhoc-abc"}
	require.True(t, m.Matches("LOCAL-1"))
	require.True(t, m.Matches("t-7"))
	require.True(t, m.Matches("adhoc-abc"))
	require.False(t, m.Matches("INF-999"))
	require.False(t, m.Matches(""), "an empty filter must not match everything by accident")
}

// Each run records the worktree root it was created under, and those can differ
// — WT_ROOT changes, or older runs were made elsewhere. Keying a remover by
// repo alone hands a run to a manager pointed at a directory its worktree never
// occupied, so gc quietly reclaims nothing.
func TestGCPicksTheRemoverForTheRootTheRunWasCreatedUnder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gcFixture(t, RunMeta{
		RunID: "r1", IssueIdentifier: "INF-1", Repo: "kernel", Branch: "feature/INF-1",
		WTRoot: "/roots/old", State: RunStateDone, PRURL: "https://github.com/o/r/pull/1",
	})

	right, wrong := &fakeRemover{}, &fakeRemover{}
	deps := gcDeps(fakePRChecker{merged: map[string]bool{"https://github.com/o/r/pull/1": true}}, wrong, nil)
	deps.Removers = map[string]WorktreeRemover{
		RunMeta{Repo: "kernel", WTRoot: "/roots/old"}.RemoverKey(): right,
		RunMeta{Repo: "kernel", WTRoot: "/roots/new"}.RemoverKey(): wrong,
	}

	res, err := RunGC(context.Background(), deps, GCOptions{Apply: true}, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"feature/INF-1"}, right.removed)
	require.Empty(t, wrong.removed, "a manager on the wrong root must never be handed the run")
	require.Equal(t, 1, res.Removed)
}

// A run whose root has no manager must be reported, not silently counted as
// swept — the reason is the only thing that explains why a mergeable worktree
// is still on disk.
func TestGCSaysWhichRootHasNoRemover(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gcFixture(t, RunMeta{
		RunID: "r1", IssueIdentifier: "INF-1", Repo: "kernel", Branch: "feature/INF-1",
		WTRoot: "/roots/gone", State: RunStateDone, PRURL: "https://github.com/o/r/pull/1",
	})

	rm := &fakeRemover{}
	deps := gcDeps(fakePRChecker{merged: map[string]bool{"https://github.com/o/r/pull/1": true}}, rm, nil)

	res, err := RunGC(context.Background(), deps, GCOptions{Apply: true}, nil)
	require.NoError(t, err)
	require.Empty(t, rm.removed)
	require.Zero(t, res.Removed)
	requireReason(t, res, "worktree", "/roots/gone")
}
