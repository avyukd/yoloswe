package prdozer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/wt"
)

// gitRepoFixture is a real on-disk origin + bare-clone-with-worktrees layout.
// The unpushed-commit safety condition is the one thing in this change that
// must not be verified against a mock: it exists to prevent real data loss, so
// it is tested against real git.
type gitRepoFixture struct {
	t      *testing.T
	git    wt.GitRunner
	origin string // a bare "remote"
	root   string // worktree_root containing .bare
	branch string
}

func newGitRepoFixture(t *testing.T) *gitRepoFixture {
	t.Helper()
	base := t.TempDir()
	git := &wt.DefaultGitRunner{}
	ctx := context.Background()

	run := func(dir string, args ...string) {
		t.Helper()
		res, err := git.Run(ctx, args, dir)
		require.NoError(t, err, "git %v in %s: %s", args, dir, stderrOf(res))
	}

	// A seed repo we push from, and a bare origin to push to.
	origin := filepath.Join(base, "origin.git")
	run(base, "init", "--bare", "--initial-branch=main", origin)

	seed := filepath.Join(base, "seed")
	run(base, "init", "--initial-branch=main", seed)
	run(seed, "config", "user.email", "test@example.com")
	run(seed, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o600))
	run(seed, "add", ".")
	run(seed, "commit", "-m", "initial")
	run(seed, "remote", "add", "origin", origin)
	run(seed, "push", "-u", "origin", "main")
	// A feature branch standing in for the PR head.
	run(seed, "checkout", "-b", "feature/x")
	require.NoError(t, os.WriteFile(filepath.Join(seed, "feature.txt"), []byte("work\n"), 0o600))
	run(seed, "add", ".")
	run(seed, "commit", "-m", "feature work")
	run(seed, "push", "-u", "origin", "feature/x")

	// The worktree_root: a .bare clone with sibling worktrees, matching the
	// layout prdozer targets.
	root := filepath.Join(base, "wtroot")
	require.NoError(t, os.MkdirAll(root, 0o755))
	run(root, "clone", "--bare", origin, filepath.Join(root, ".bare"))
	run(filepath.Join(root, ".bare"), "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	run(filepath.Join(root, ".bare"), "fetch", "origin")

	return &gitRepoFixture{t: t, git: git, origin: origin, root: filepath.Join(root, ".bare"), branch: "feature/x"}
}

func (f *gitRepoFixture) entry() RepoEntry {
	return RepoEntry{WorktreeRoot: f.root, Layout: LayoutWT, BaseBranch: "main", Flow: "pr-polish"}
}

func (f *gitRepoFixture) pr(number int) DiscoveredPR {
	return DiscoveredPR{
		Number:      number,
		HeadRefName: f.branch,
		BaseRefName: "main",
		URL:         "https://github.com/o/r/pull/1",
	}
}

func (f *gitRepoFixture) mustRun(dir string, args ...string) {
	f.t.Helper()
	res, err := f.git.Run(context.Background(), args, dir)
	require.NoError(f.t, err, "git %v: %s", args, stderrOf(res))
}

func TestPrepareWorktree_ConcurrentRunsGetDistinctPaths(t *testing.T) {
	t.Parallel()
	// Two babysit runs on the SAME PR must never share a directory — that is
	// the whole reason each run gets its own run ID.
	f := newGitRepoFixture(t)
	ctx := context.Background()

	rc1, err := PrepareWorktree(ctx, f.git, f.entry(), f.pr(42), "aaa111", nil)
	require.NoError(t, err)
	rc2, err := PrepareWorktree(ctx, f.git, f.entry(), f.pr(42), "bbb222", nil)
	require.NoError(t, err)

	assert.NotEqual(t, rc1.WorktreePath, rc2.WorktreePath)
	assert.DirExists(t, rc1.WorktreePath)
	assert.DirExists(t, rc2.WorktreePath)
	// Both live under the .babysit namespace so the sweeper can find them.
	assert.Contains(t, rc1.WorktreePath, BabysitNamespace)
	assert.Contains(t, rc2.WorktreePath, BabysitNamespace)
	// And both actually contain the branch's content.
	assert.FileExists(t, filepath.Join(rc1.WorktreePath, "feature.txt"))
}

func TestPrepareWorktree_RejectsForkPR(t *testing.T) {
	t.Parallel()
	f := newGitRepoFixture(t)
	pr := f.pr(42)
	pr.IsCrossRepository = true
	pr.HeadRepoOwner = "outsider"

	_, err := PrepareWorktree(context.Background(), f.git, f.entry(), pr, "aaa111", nil)
	require.Error(t, err, "fork PRs must fail clearly rather than guess at remote setup")
	assert.Contains(t, err.Error(), "fork")
}

func TestCleanup_RemovesWorktreeButNotLogDir(t *testing.T) {
	t.Parallel()
	f := newGitRepoFixture(t)
	ctx := context.Background()
	rc, err := PrepareWorktree(ctx, f.git, f.entry(), f.pr(42), "ccc333", nil)
	require.NoError(t, err)

	// Stand in for the run log dir, which lives outside the worktree.
	logDir := t.TempDir()
	rc.LogDir = logDir
	require.NoError(t, os.WriteFile(filepath.Join(logDir, "events.jsonl"), []byte("{}\n"), 0o600))

	require.NoError(t, rc.Cleanup(ctx, f.git, false, nil))
	assert.NoDirExists(t, rc.WorktreePath, "clean worktree should be removed")
	assert.False(t, rc.Kept)
	assert.FileExists(t, filepath.Join(logDir, "events.jsonl"),
		"logs live outside the worktree and must survive GC")
}

func TestCleanup_KeepsWorktreeWithUncommittedChanges(t *testing.T) {
	t.Parallel()
	// A dirty working tree cannot be pushed, so it must be kept and reported.
	// The edit is to a TRACKED file: untracked files are build artifacts and
	// deliberately do not count (see
	// TestWorktreeHasUnpushedWork_IgnoresUntrackedBuildArtifacts).
	f := newGitRepoFixture(t)
	ctx := context.Background()
	rc, err := PrepareWorktree(ctx, f.git, f.entry(), f.pr(42), "ddd444", nil)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(rc.WorktreePath, "feature.txt"), []byte("edited\n"), 0o600))

	require.NoError(t, rc.Cleanup(ctx, f.git, false, nil))
	assert.DirExists(t, rc.WorktreePath, "a dirty worktree must never be silently discarded")
	assert.True(t, rc.Kept)
	assert.NotEmpty(t, rc.KeptReason, "the reason must be reportable in the notification")
}

func TestCleanup_PushesUnpushedCommitsThenRemoves(t *testing.T) {
	t.Parallel()
	// A committed-but-unpushed change is the case where "the PR is the durable
	// state" is not yet true. Cleanup must push first, then remove.
	f := newGitRepoFixture(t)
	ctx := context.Background()
	rc, err := PrepareWorktree(ctx, f.git, f.entry(), f.pr(42), "eee555", nil)
	require.NoError(t, err)

	f.mustRun(rc.WorktreePath, "config", "user.email", "test@example.com")
	f.mustRun(rc.WorktreePath, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(rc.WorktreePath, "new.txt"), []byte("committed\n"), 0o600))
	f.mustRun(rc.WorktreePath, "add", ".")
	f.mustRun(rc.WorktreePath, "commit", "-m", "agent work")

	unclean, _, err := WorktreeHasUnpushedWork(ctx, f.git, rc.WorktreePath, rc.Branch)
	require.NoError(t, err)
	require.True(t, unclean, "precondition: the commit is local-only")

	require.NoError(t, rc.Cleanup(ctx, f.git, false, nil))
	assert.False(t, rc.Kept, "after a successful push the worktree is safe to remove")
	assert.NoDirExists(t, rc.WorktreePath)

	// The commit must actually be on the origin now, not merely gone.
	res, err := f.git.Run(ctx, []string{"log", "--oneline", "feature/x"}, f.origin)
	require.NoError(t, err)
	assert.Contains(t, res.Stdout, "agent work", "the commit must survive on the remote")
}

func TestCleanup_PushRefusesToClobberDivergedRemote(t *testing.T) {
	t.Parallel()
	// The lease must still bite. If someone else pushed to the branch while
	// this run was working, the cleanup push must be REFUSED (not force
	// through), and the worktree kept so the local commit isn't lost.
	f := newGitRepoFixture(t)
	ctx := context.Background()
	rc, err := PrepareWorktree(ctx, f.git, f.entry(), f.pr(42), "lease1", nil)
	require.NoError(t, err)

	// Our run commits locally.
	f.mustRun(rc.WorktreePath, "config", "user.email", "test@example.com")
	f.mustRun(rc.WorktreePath, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(rc.WorktreePath, "ours.txt"), []byte("ours\n"), 0o600))
	f.mustRun(rc.WorktreePath, "add", ".")
	f.mustRun(rc.WorktreePath, "commit", "-m", "our work")

	// Meanwhile, someone else advances the same branch on the remote.
	other := t.TempDir()
	f.mustRun("", "clone", f.origin, other)
	f.mustRun(other, "config", "user.email", "other@example.com")
	f.mustRun(other, "config", "user.name", "Other")
	f.mustRun(other, "checkout", "feature/x")
	require.NoError(t, os.WriteFile(filepath.Join(other, "theirs.txt"), []byte("theirs\n"), 0o600))
	f.mustRun(other, "add", ".")
	f.mustRun(other, "commit", "-m", "their work")
	f.mustRun(other, "push", "origin", "feature/x")

	require.NoError(t, rc.Cleanup(ctx, f.git, false, nil))
	assert.True(t, rc.Kept, "a refused push must keep the worktree, not discard the commit")
	assert.DirExists(t, rc.WorktreePath)

	// Their commit must still be the remote tip — ours must not have clobbered it.
	res, err := f.git.Run(ctx, []string{"log", "--oneline", "feature/x"}, f.origin)
	require.NoError(t, err)
	assert.Contains(t, res.Stdout, "their work", "concurrent work must survive")
	assert.NotContains(t, res.Stdout, "our work", "the lease must have refused the push")
}

func TestCleanup_PushLeaseIsPinnedAtCheckoutNotPushTime(t *testing.T) {
	t.Parallel()
	// REGRESSION: the lease baseline must be captured when the worktree is
	// created, not read at push time. The polish agent's job includes rebasing
	// onto a moved base, so it runs `git fetch` — which advances
	// refs/remotes/origin/<branch>. A lease read after that always matches the
	// current remote, turning --force-with-lease into a silent no-op that
	// destroys concurrent work.
	f := newGitRepoFixture(t)
	ctx := context.Background()
	rc, err := PrepareWorktree(ctx, f.git, f.entry(), f.pr(42), "pin111", nil)
	require.NoError(t, err)
	require.NotEmpty(t, rc.BaseRemoteSHA, "the lease baseline must be pinned at checkout")

	// Our run commits.
	f.mustRun(rc.WorktreePath, "config", "user.email", "test@example.com")
	f.mustRun(rc.WorktreePath, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(rc.WorktreePath, "ours.txt"), []byte("ours\n"), 0o600))
	f.mustRun(rc.WorktreePath, "add", ".")
	f.mustRun(rc.WorktreePath, "commit", "-m", "our work")

	// Someone else pushes to the same branch.
	other := t.TempDir()
	f.mustRun("", "clone", f.origin, other)
	f.mustRun(other, "config", "user.email", "other@example.com")
	f.mustRun(other, "config", "user.name", "Other")
	f.mustRun(other, "checkout", "feature/x")
	require.NoError(t, os.WriteFile(filepath.Join(other, "theirs.txt"), []byte("theirs\n"), 0o600))
	f.mustRun(other, "add", ".")
	f.mustRun(other, "commit", "-m", "their work")
	f.mustRun(other, "push", "origin", "feature/x")

	// The agent fetches, as it does when rebasing. This advances the local
	// remote-tracking ref — the exact condition that defeated the old code.
	f.mustRun(rc.WorktreePath, "fetch", "origin")

	require.NoError(t, rc.Cleanup(ctx, f.git, false, nil))
	assert.True(t, rc.Kept, "the pinned lease must still refuse the push after a fetch")

	res, err := f.git.Run(ctx, []string{"log", "--oneline", "feature/x"}, f.origin)
	require.NoError(t, err)
	assert.Contains(t, res.Stdout, "their work", "concurrent work must survive an intervening fetch")
	assert.NotContains(t, res.Stdout, "our work", "the lease must have refused the force-push")
}

func TestPushBranch_RefusesWithoutPinnedBaseline(t *testing.T) {
	t.Parallel()
	// Fail closed: with no trustworthy baseline the only options are "don't
	// push" and "force blindly", and blind force is the data loss this guards.
	f := newGitRepoFixture(t)
	err := pushBranch(context.Background(), f.git, f.root, "feature/x", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to force-push")
}

func TestWorktreeHasUnpushedWork_IgnoresUntrackedBuildArtifacts(t *testing.T) {
	t.Parallel()
	// Untracked files in a build tree are artifacts (bazel-*, node_modules),
	// not work. Counting them would mark every post-build worktree permanently
	// unclean, so neither Cleanup nor the sweeper could ever reclaim one and
	// the GC would free nothing at all on a Bazel repo.
	f := newGitRepoFixture(t)
	ctx := context.Background()
	rc, err := PrepareWorktree(ctx, f.git, f.entry(), f.pr(42), "arti11", nil)
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(rc.WorktreePath, "node_modules"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rc.WorktreePath, "node_modules", "x.js"), []byte("//\n"), 0o600))

	unclean, reason, err := WorktreeHasUnpushedWork(ctx, f.git, rc.WorktreePath, rc.Branch)
	require.NoError(t, err)
	assert.False(t, unclean, "build artifacts must not block reclamation, got: %s", reason)

	// But a modification to a TRACKED file is real work and still counts.
	require.NoError(t, os.WriteFile(filepath.Join(rc.WorktreePath, "feature.txt"), []byte("edited\n"), 0o600))
	unclean, _, err = WorktreeHasUnpushedWork(ctx, f.git, rc.WorktreePath, rc.Branch)
	require.NoError(t, err)
	assert.True(t, unclean, "an uncommitted edit to a tracked file is work")
}

func TestCleanup_ForceSkipsSafetyCheck(t *testing.T) {
	t.Parallel()
	f := newGitRepoFixture(t)
	ctx := context.Background()
	rc, err := PrepareWorktree(ctx, f.git, f.entry(), f.pr(42), "fff666", nil)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(rc.WorktreePath, "feature.txt"), []byte("edited\n"), 0o600))

	require.NoError(t, rc.Cleanup(ctx, f.git, true, nil))
	assert.NoDirExists(t, rc.WorktreePath, "force must remove even a dirty worktree")
}

func TestCleanup_IsIdempotent(t *testing.T) {
	t.Parallel()
	f := newGitRepoFixture(t)
	ctx := context.Background()
	rc, err := PrepareWorktree(ctx, f.git, f.entry(), f.pr(42), "999aaa", nil)
	require.NoError(t, err)
	require.NoError(t, rc.Cleanup(ctx, f.git, false, nil))
	// A deferred Cleanup after an explicit one must not error.
	require.NoError(t, rc.Cleanup(ctx, f.git, false, nil))
}

// NOTE: the GC tests below are not t.Parallel() — they use t.Setenv to isolate
// HOME (so RunsRoot points at a temp dir), which Go forbids combining with
// parallel execution.

func TestGC_ReapsOnlyBabysitOrphans(t *testing.T) {
	// The sweeper must never touch a real worktree sitting beside the
	// ephemeral ones. This is the guarantee the .babysit namespace buys.
	f := newGitRepoFixture(t)
	ctx := context.Background()
	t.Setenv("HOME", t.TempDir())

	rc, err := PrepareWorktree(ctx, f.git, f.entry(), f.pr(42), "old111", nil)
	require.NoError(t, err)

	// A real, human-owned worktree in the ordinary location.
	realWT := filepath.Join(filepath.Dir(f.root), "my-real-feature")
	f.mustRun(f.root, "worktree", "add", "-b", "my-real-feature", realWT, "main")

	// Age the ephemeral worktree past the TTL.
	old := time.Now().Add(-10 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(rc.WorktreePath, old, old))

	res, err := RunGC(ctx, f.git, GCOptions{
		WorktreeRoots: []string{f.root},
		WorktreeTTL:   3 * 24 * time.Hour,
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Removed)
	assert.NoDirExists(t, rc.WorktreePath)
	assert.DirExists(t, realWT, "a real worktree must never be a GC candidate")
}

func TestBranchOfWorktree_ResolvesDetachedHead(t *testing.T) {
	t.Parallel()
	// Babysit worktrees run detached, so `rev-parse --abbrev-ref HEAD` returns
	// the literal "HEAD". If branchOfWorktree returned that, the GC's
	// unpushed-work check would compare against the nonexistent "origin/HEAD",
	// error, fail closed, and silently disable the entire sweeper.
	f := newGitRepoFixture(t)
	ctx := context.Background()
	rc, err := PrepareWorktree(ctx, f.git, f.entry(), f.pr(42), "det111", nil)
	require.NoError(t, err)

	got := branchOfWorktree(ctx, f.git, rc.WorktreePath)
	assert.Equal(t, "feature/x", got, "must resolve the real branch, not %q", "HEAD")

	// And the resolved name must actually work for the safety check.
	unclean, _, err := WorktreeHasUnpushedWork(ctx, f.git, rc.WorktreePath, got)
	require.NoError(t, err, "the resolved branch must be comparable against origin")
	assert.False(t, unclean, "a fresh worktree matches its remote branch")
}

func TestGC_DryRunRemovesNothing(t *testing.T) {
	f := newGitRepoFixture(t)
	ctx := context.Background()
	t.Setenv("HOME", t.TempDir())

	rc, err := PrepareWorktree(ctx, f.git, f.entry(), f.pr(42), "dry111", nil)
	require.NoError(t, err)
	old := time.Now().Add(-10 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(rc.WorktreePath, old, old))

	res, err := RunGC(ctx, f.git, GCOptions{
		WorktreeRoots: []string{f.root},
		WorktreeTTL:   3 * 24 * time.Hour,
		DryRun:        true,
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Eligible, "dry run reports what it WOULD remove")
	assert.Zero(t, res.Removed, "Removed counts only actual removals, so a dry run never inflates it")
	assert.DirExists(t, rc.WorktreePath, "dry run must not remove anything")
	require.NotEmpty(t, res.Candidates)
	assert.False(t, res.Candidates[0].Removed)
	assert.NotEmpty(t, res.Candidates[0].Reason, "every candidate must explain itself")
}

func TestGC_SkipsYoungAndUnpushedWorktrees(t *testing.T) {
	f := newGitRepoFixture(t)
	ctx := context.Background()
	t.Setenv("HOME", t.TempDir())

	// Young: under TTL.
	young, err := PrepareWorktree(ctx, f.git, f.entry(), f.pr(42), "young1", nil)
	require.NoError(t, err)

	// Old but holding uncommitted work on a TRACKED file: must be kept despite
	// disk pressure. (Untracked files are build artifacts and don't count.)
	dirty, err := PrepareWorktree(ctx, f.git, f.entry(), f.pr(43), "dirty1", nil)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dirty.WorktreePath, "feature.txt"), []byte("edited\n"), 0o600))
	old := time.Now().Add(-10 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(dirty.WorktreePath, old, old))

	res, err := RunGC(ctx, f.git, GCOptions{
		WorktreeRoots: []string{f.root},
		WorktreeTTL:   3 * 24 * time.Hour,
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Removed)
	assert.DirExists(t, young.WorktreePath)
	assert.DirExists(t, dirty.WorktreePath, "unpushed work outranks disk pressure")

	var sawUnpushedReason bool
	for _, c := range res.Candidates {
		if c.Path == dirty.WorktreePath {
			sawUnpushedReason = true
			assert.Contains(t, c.Reason, "local-only work")
		}
	}
	assert.True(t, sawUnpushedReason, "the skip reason must be reported")
}
