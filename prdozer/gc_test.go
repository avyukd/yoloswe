package prdozer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newGCWorktree creates a .babysit worktree under root, aged by backdating its
// mtime, and returns its path.
func newGCWorktree(t *testing.T, root, name string, age time.Duration) string {
	t.Helper()
	path := filepath.Join(root, BabysitNamespace, name)
	require.NoError(t, os.MkdirAll(path, 0o755))
	when := time.Now().Add(-age)
	require.NoError(t, os.Chtimes(path, when, when))
	return path
}

// A dry run removes nothing, so labelling every row from the dryRun flag made
// kept candidates read as pending deletions — the table said "would-remove"
// beside "younger than TTL" while the summary said 0 would be removed.
func TestRunGC_DryRunLabelsOnlyEligibleCandidates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repoRoot := filepath.Join(home, "worktrees", "yoloswe")

	young := newGCWorktree(t, repoRoot, "284-young", time.Hour)
	old := newGCWorktree(t, repoRoot, "285-old", 96*time.Hour)

	res, err := RunGC(context.Background(), fakeGit{}, GCOptions{
		WorktreeRoots: []string{repoRoot},
		DryRun:        true,
		Force:         true, // isolate the TTL decision from the unpushed-work check
	}, nil)
	require.NoError(t, err)

	byPath := make(map[string]GCCandidate, len(res.Candidates))
	for _, c := range res.Candidates {
		byPath[c.Path] = c
	}

	require.Contains(t, byPath, young)
	assert.False(t, byPath[young].Eligible, "a worktree younger than the TTL must not be eligible")
	assert.Contains(t, byPath[young].Reason, "younger than TTL")

	require.Contains(t, byPath, old)
	assert.True(t, byPath[old].Eligible, "an expired worktree is eligible even in a dry run")

	assert.Equal(t, 1, res.Eligible, "exactly one candidate qualifies")
	assert.Equal(t, 0, res.Removed, "a dry run must remove nothing")
	assert.False(t, byPath[old].Removed, "a dry run must not mark anything removed")
}

// Rooted under PlainWorktreeRoot deliberately: the plain layout removes the
// directory itself, so a mock git can still prove the on-disk effect. Under the
// wt layout the removal IS `git worktree remove`, which only real git performs —
// TestGC_ReapsOnlyBabysitOrphans covers that against a real repository.
func TestRunGC_RemovesExpiredWorktree(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repoRoot := filepath.Join(ExpandHome(PlainWorktreeRoot), "yoloswe")
	old := newGCWorktree(t, repoRoot, "285-old", 96*time.Hour)

	res, err := RunGC(context.Background(), fakeGit{}, GCOptions{
		WorktreeRoots: []string{repoRoot},
		Force:         true,
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Removed)
	assert.NoDirExists(t, old, "an expired worktree must actually be gone")
}

// A real worktree sitting beside the namespace must never be a candidate:
// that separation is the entire reason ephemeral runs live under .babysit.
func TestRunGC_NeverTouchesWorktreesOutsideTheNamespace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repoRoot := filepath.Join(home, "worktrees", "yoloswe")

	real := filepath.Join(repoRoot, "feature", "something-important")
	require.NoError(t, os.MkdirAll(real, 0o755))
	when := time.Now().Add(-200 * 24 * time.Hour) // far past every TTL
	require.NoError(t, os.Chtimes(real, when, when))

	res, err := RunGC(context.Background(), fakeGit{}, GCOptions{
		WorktreeRoots: []string{repoRoot},
		Force:         true,
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Removed)
	assert.DirExists(t, real, "a human-owned worktree must survive any sweep")
	for _, c := range res.Candidates {
		assert.NotEqual(t, real, c.Path, "it must not even be considered")
	}
}

// Every considered path belongs in the table, including skips: a row that
// vanishes reads as "nothing there" rather than "deliberately kept".
func TestRunGC_YoungRunLogIsReportedNotSilentlyDropped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	runDir := filepath.Join(home, ".prdozer", "runs", "bazelment-yoloswe-284-abc123")
	require.NoError(t, os.MkdirAll(runDir, 0o755))

	res, err := RunGC(context.Background(), fakeGit{}, GCOptions{}, nil)
	require.NoError(t, err)

	var found bool
	for _, c := range res.Candidates {
		if c.Path == runDir {
			found = true
			assert.False(t, c.Eligible)
			assert.Contains(t, c.Reason, "younger than run-log TTL")
		}
	}
	assert.True(t, found, "a skipped run log must still appear in the candidate table")
}
