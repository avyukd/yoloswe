package jiradozer

import (
	"context"
	"errors"
	"io/fs"
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
// A caller that needs the path to have a particular shape — the <root>/<repo>/
// <branch> layout a pre-wt_root record's root is read back off — sets
// WorktreePath itself and gets that directory created instead.
func gcFixture(t *testing.T, m RunMeta) (RunMeta, string) {
	t.Helper()
	wtPath := m.WorktreePath
	if wtPath == "" {
		wtPath = filepath.Join(t.TempDir(), "worktree")
	}
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
		LeaseHeld: func(string) (bool, error) { return false, nil },
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
	deps.LeaseHeld = func(string) (bool, error) { return true, nil }

	res, err := RunGC(context.Background(), deps, GCOptions{Apply: true}, nil)
	require.NoError(t, err)
	require.Empty(t, rm.removed)
	requireReason(t, res, "worktree", "holds this task's lease")
}

// Same invariant as the stat gates: a lease gc could not read is not a lease
// nobody holds, and "nobody holds it" is what clears a checkout for deletion.
func TestGCWillNotReclaimOnALeaseItCouldNotRead(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gcFixture(t, RunMeta{
		RunID: "r1", IssueIdentifier: "INF-1", Repo: "kernel", Branch: "feature/INF-1",
		State: RunStateDone, PRURL: "https://github.com/o/r/pull/1",
	})

	rm := &fakeRemover{}
	deps := gcDeps(fakePRChecker{merged: map[string]bool{"https://github.com/o/r/pull/1": true}}, rm, nil)
	deps.LeaseHeld = func(string) (bool, error) {
		return false, errors.New("permission denied")
	}

	res, err := RunGC(context.Background(), deps, GCOptions{Apply: true}, nil)
	require.NoError(t, err, "one unreadable lease must not abort the whole sweep")
	require.Empty(t, rm.removed)
	requireReason(t, res, "worktree", "could not verify the task's lease")
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

// unverifiableFixture seeds a run whose recorded worktree path os.Stat cannot
// answer for: its parent is a regular file, so the kernel returns ENOTDIR.
//
// That stands in for the EACCES/EIO a real sweep hits on a bad mount, and
// unlike a chmod-0 trick it behaves identically when the tests run as root.
// The path is taken literally — no directory is created — which is why this
// cannot go through gcFixture.
func unverifiableFixture(t *testing.T, m RunMeta) RunMeta {
	t.Helper()
	notADir := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(notADir, []byte("x"), 0o600))
	m.WorktreePath = filepath.Join(notADir, "worktree")

	_, err := os.Stat(m.WorktreePath)
	require.Error(t, err, "the fixture must make stat fail")
	require.False(t, errors.Is(err, fs.ErrNotExist),
		"and fail with something other than ENOENT, or it proves nothing")

	rl, err := NewRunLog(m)
	require.NoError(t, err)
	if m.State.IsTerminal() {
		require.NoError(t, rl.Finish(m.State, "", nil))
	}
	return rl.Meta()
}

// A stat that failed is not a stat that said "gone". gc must not walk into the
// reclaim path on a directory it has not established is there.
func TestGCWillNotReclaimAWorktreeItCannotSee(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	unverifiableFixture(t, RunMeta{
		RunID: "r1", IssueIdentifier: "INF-1", Repo: "kernel", Branch: "feature/INF-1",
		State: RunStateDone, PRURL: "https://github.com/o/r/pull/1",
	})

	rm := &fakeRemover{}
	res, err := RunGC(context.Background(),
		gcDeps(fakePRChecker{merged: map[string]bool{"https://github.com/o/r/pull/1": true}}, rm, nil),
		GCOptions{Apply: true}, nil)
	require.NoError(t, err)
	require.Empty(t, rm.removed, "an unverifiable path must not reach the remover")
	require.Zero(t, res.Removed)
	requireReason(t, res, "worktree", "could not verify the worktree path")
}

// The run-log is the worktree's only ownership record, so expiring it on an
// inconclusive stat orphans the directory permanently — no later sweep can
// rediscover it. Keep the record until the filesystem gives a real answer.
func TestGCKeepsARunLogItCannotProveIsOrphaned(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := unverifiableFixture(t, RunMeta{
		RunID: "r1", IssueIdentifier: "INF-1", Repo: "kernel", Branch: "feature/INF-1",
		State: RunStateDone,
	})
	require.NotEmpty(t, m.LogDir)

	res, err := RunGC(context.Background(), gcDeps(fakePRChecker{}, &fakeRemover{}, nil),
		GCOptions{Apply: true, RunLogTTL: time.Nanosecond}, nil)
	require.NoError(t, err)
	require.DirExists(t, m.LogDir, "a run-log must outlive a stat it could not complete")
	requireReason(t, res, "runlog", "could not verify the worktree path")
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
// that itself fails, both leave the worktree exactly where it is — but they say
// so differently, because one is a permanent state and the other is worth a
// retry, and an operator reading the preview has to be able to tell which.
func TestGCRecoveryStillFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		resolver *fakePRResolver
		name     string
		reason   string
	}{
		{
			name:     "branch has no PR",
			resolver: &fakePRResolver{byBranch: map[string]string{}},
			reason:   "no PR recorded",
		},
		{
			name:     "resolver itself fails",
			resolver: &fakePRResolver{err: errors.New("gh: network unreachable")},
			reason:   "could not look up a PR for this branch: gh: network unreachable",
		},
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
			requireReason(t, res, "worktree", tc.reason)
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
	deps.LeaseHeld = func(target string) (bool, error) {
		asked = append(asked, target)
		return target == "adhoc-deadbeefcafe", nil
	}

	res, err := RunGC(context.Background(), deps, GCOptions{Apply: true}, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"adhoc-deadbeefcafe"}, asked)
	require.Empty(t, rm.removed, "a live worker's worktree must survive the sweep")
	requireReason(t, res, "worktree", "holds this task's lease")
}

// Records written before LeaseTarget existed still have to be swept. The
// fallbacks may only reconstruct a name that was actually taken — a plausible
// one is worse than none, because "no such lock" reads as "nobody is working
// here" and clears a checkout for deletion.
func TestLeaseKeyOnlyAnswersWhenItCanNameTheRealLock(t *testing.T) {
	// Recorded wins outright.
	require.Equal(t, "adhoc-abc", RunMeta{IssueIdentifier: "LOCAL-1", LeaseTarget: "adhoc-abc"}.LeaseKey())
	// An issue or task run leased its own identifier, so the old records match.
	require.Equal(t, "INF-1", RunMeta{IssueIdentifier: "INF-1"}.LeaseKey())
	require.Equal(t, "t-7", RunMeta{TaskID: "t-7"}.LeaseKey())
	// A --description run leased a hash of the description. LOCAL-1 is the
	// local tracker's issue, not a lock name, and the run id never was one
	// either — both must come back empty rather than confidently wrong.
	require.Empty(t, RunMeta{RunID: "r1", Description: "tidy the chart"}.LeaseKey())
	require.Empty(t, RunMeta{RunID: "r1", Description: "tidy the chart", IssueIdentifier: "LOCAL-1"}.LeaseKey())
	require.Empty(t, RunMeta{RunID: "r1"}.LeaseKey())
	// Except when the dispatcher named the task: then the task id is the lock,
	// exactly as leaseTarget derives it.
	require.Equal(t, "t-7", RunMeta{RunID: "r1", Description: "tidy the chart", TaskID: "t-7"}.LeaseKey())
}

// The fallback used to be Target(), which for a --description run is a
// local-tracker id — a lock name that never existed. gc would ask about it,
// hear "not held", and go on to delete the directory of a worker that was still
// running. A record that cannot name its lock must stop the sweep instead.
func TestGCWillNotReclaimARecordThatCannotNameItsLease(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, wtPath := gcFixture(t, RunMeta{
		RunID: "r1", Repo: "kernel", Branch: "jiradozer/r1",
		// A pre-LeaseTarget --description record: local issue, no lease name.
		IssueIdentifier: "LOCAL-1", Description: "tidy the helm chart",
		State: RunStateDone, PRURL: "https://github.com/o/r/pull/1",
	})

	var asked []string
	rm := &fakeRemover{}
	deps := gcDeps(fakePRChecker{merged: map[string]bool{"https://github.com/o/r/pull/1": true}}, rm, nil)
	deps.LeaseHeld = func(target string) (bool, error) {
		asked = append(asked, target)
		return false, nil
	}

	res, err := RunGC(context.Background(), deps, GCOptions{Apply: true}, nil)
	require.NoError(t, err)
	require.Empty(t, asked, "a name no lock ever had must not be asked about at all")
	require.Empty(t, rm.removed)
	require.DirExists(t, wtPath)
	requireReason(t, res, "worktree", "does not record which lease it holds")
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

// The case above, for the records that have no wt_root to key on — the ones
// EffectiveWTRoot exists for. Their root is readable off their worktree path,
// so they must NOT share a bucket with records that genuinely cannot say and
// fall back to the ambient root. Keying on the raw field collapsed both into
// one, and whichever was seen first handed its manager to the other.
func TestGCRoutesAPreWTRootRecordToTheRootItsPathNames(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldRoot := filepath.Join(t.TempDir(), "roots", "old")

	// Recorded before wt_root existed, but its path still names the old root.
	derivable, _ := gcFixture(t, RunMeta{
		RunID: "r1", IssueIdentifier: "INF-1", Repo: "kernel", Branch: "feature/INF-1",
		WorktreePath: filepath.Join(oldRoot, "kernel", "feature/INF-1"),
		State:        RunStateDone, PRURL: "https://github.com/o/r/pull/1",
	})
	// Also pre-wt_root, but its path is not in the layout, so nothing can be
	// read back off it: the ambient root is all this one has.
	ambient, _ := gcFixture(t, RunMeta{
		RunID: "r2", IssueIdentifier: "INF-2", Repo: "kernel", Branch: "feature/INF-2",
		State: RunStateDone, PRURL: "https://github.com/o/r/pull/2",
	})
	require.Equal(t, oldRoot, derivable.EffectiveWTRoot())
	require.Empty(t, ambient.EffectiveWTRoot())

	atOldRoot, atAmbientRoot := &fakeRemover{}, &fakeRemover{}
	deps := gcDeps(fakePRChecker{merged: map[string]bool{
		"https://github.com/o/r/pull/1": true,
		"https://github.com/o/r/pull/2": true,
	}}, atAmbientRoot, nil)
	deps.Removers = map[string]WorktreeRemover{
		derivable.RemoverKey(): atOldRoot,
		ambient.RemoverKey():   atAmbientRoot,
	}
	require.NotEqual(t, derivable.RemoverKey(), ambient.RemoverKey(),
		"a derivable root and an unknowable one must not share a bucket")

	res, err := RunGC(context.Background(), deps, GCOptions{Apply: true}, nil)
	require.NoError(t, err)
	require.Equal(t, 2, res.Removed)
	require.Equal(t, []string{"feature/INF-1"}, atOldRoot.removed)
	require.Equal(t, []string{"feature/INF-2"}, atAmbientRoot.removed)
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

// recordingGH captures the argv gc hands to gh.
type recordingGH struct {
	err    error
	stdout string
	args   []string
}

func (g *recordingGH) Run(_ context.Context, args []string, _ string) (*wt.CmdResult, error) {
	g.args = args
	return &wt.CmdResult{Stdout: g.stdout}, g.err
}

// The real GHPRChecker was never exercised — every gc test used a fake
// PRChecker — so it shipped calling `gh pr view --json merged`, which gh
// rejects with "Unknown JSON field: merged" on EVERY call. gc then failed
// closed on every worktree with a plausible "could not determine PR state",
// which is indistinguishable from a sweeper being careful. It never reclaimed
// anything.
//
// This asserts the argv, which is the only part a fake can get wrong for free.
//
// Whole-argv equality, not Contains: ".merged" is a substring of ".mergedAt",
// so a containment check passes a regression to `--jq .mergedAt` — which REST
// spells merged_at, so it returns null, Merged errors, and gc goes back to
// failing closed on every worktree. The one selector that answers the question
// has to be the one asserted.
func TestGHPRCheckerAsksTheAPIForMerged(t *testing.T) {
	gh := &recordingGH{stdout: "true\n"}
	merged, err := GHPRChecker{GH: gh}.Merged(context.Background(),
		"https://github.com/bazelment/yoloswe/pull/302")
	require.NoError(t, err)
	require.True(t, merged)

	require.Equal(t,
		[]string{"api", "repos/bazelment/yoloswe/pulls/302", "--jq", ".merged"},
		gh.args,
		"`gh pr view --json merged` is not a valid query, and state/mergeStateStatus/"+
			"mergeCommit/mergedAt do not answer 'did this land'")
}

func TestGHPRCheckerReadsFalseAndRejectsNonsense(t *testing.T) {
	gh := &recordingGH{stdout: "false\n"}
	merged, err := GHPRChecker{GH: gh}.Merged(context.Background(),
		"https://github.com/o/r/pull/1")
	require.NoError(t, err)
	require.False(t, merged)

	// A reply that is not true/false is not an answer. Reporting it as "not
	// merged" would be the safe direction but would hide a broken query
	// forever — which is exactly how the pr-view bug survived.
	bad := &recordingGH{stdout: `{"merged":true}`}
	_, err = GHPRChecker{GH: bad}.Merged(context.Background(), "https://github.com/o/r/pull/1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unexpected")
}

func TestParsePRURL(t *testing.T) {
	o, r, n, err := parsePRURL("https://github.com/bazelment/yoloswe/pull/302")
	require.NoError(t, err)
	require.Equal(t, "bazelment", o)
	require.Equal(t, "yoloswe", r)
	require.Equal(t, 302, n)

	_, _, _, err = parsePRURL("not a url")
	require.Error(t, err)
}

// The parse is what picks the repository the merged check asks about, and its
// answer authorises deleting a worktree. A URL it should not claim has to fail
// loudly — answering with the wrong repo's .merged is how a worktree whose work
// never landed gets swept.
func TestParsePRURLRejectsURLsItDoesNotOwn(t *testing.T) {
	for _, bad := range []string{
		// A lookalike host contains "github.com/o/r/pull/1" as a substring; an
		// unanchored pattern would answer it with github.com's o/r.
		"https://notgithub.com/o/r/pull/1",
		"https://evil.example/github.com/o/r/pull/1",
		// Enterprise hosts have no correct answer here — nothing plumbs a host
		// through to `gh` — so they are rejected, not rewritten to github.com.
		"https://ghe.example.com/o/r/pull/1",
		// Not a PR number.
		"https://github.com/o/r/pull/12x",
		"https://github.com/o/r/pull/",
		// Not a PR.
		"https://github.com/o/r/issues/1",
	} {
		_, _, _, err := parsePRURL(bad)
		require.Error(t, err, "parsePRURL(%q) must not claim this URL", bad)
	}
}

// The tails gh and the browser append are still the same PR.
func TestParsePRURLAcceptsTrailingPath(t *testing.T) {
	for _, u := range []string{
		"https://github.com/bazelment/yoloswe/pull/302",
		"https://github.com/bazelment/yoloswe/pull/302/files",
		"https://github.com/bazelment/yoloswe/pull/302#discussion_r1",
		"http://github.com/bazelment/yoloswe/pull/302",
		"github.com/bazelment/yoloswe/pull/302",
	} {
		o, r, n, err := parsePRURL(u)
		require.NoError(t, err, u)
		require.Equal(t, "bazelment", o, u)
		require.Equal(t, "yoloswe", r, u)
		require.Equal(t, 302, n, u)
	}
}
