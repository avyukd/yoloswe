package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/jiradozer"
)

// removerRoot reports the worktree root the manager behind a remover was built
// on. RepoDir() is <root>/<repo>, which is the only observable that says which
// tree gc will actually go looking in.
func removerRoot(t *testing.T, r jiradozer.WorktreeRemover) string {
	t.Helper()
	a, ok := r.(*wtAdapter)
	require.True(t, ok, "removersForRuns should build wt-backed removers")
	return filepath.Dir(a.mgr.RepoDir())
}

// The failure this guards: WT_ROOT moves, and every run recorded before wt_root
// existed gets a manager pointed at the NEW tree. Those worktrees are not there,
// so gc reports nothing to reclaim and they leak forever. The recorded worktree
// path still names the old root, so nothing about them was ever unknowable.
func TestRemoversUseTheRootARunWasActuallyCreatedUnder(t *testing.T) {
	t.Setenv("WT_ROOT", "/roots/new")

	runs := []jiradozer.RunMeta{
		// Pre-wt_root record: only the worktree path says where it lives.
		{RunID: "r1", Repo: "kernel", Branch: "feature/INF-1",
			WorktreePath: "/roots/old/kernel/feature/INF-1"},
		// Modern record: wt_root is authoritative.
		{RunID: "r2", Repo: "kernel", Branch: "feature/INF-2", WTRoot: "/roots/pinned",
			WorktreePath: "/roots/pinned/kernel/feature/INF-2"},
		// Says nothing either way — the ambient root is the best guess left.
		{RunID: "r3", Repo: "kernel", Branch: "feature/INF-3"},
	}

	removers, err := removersForRuns(runs)
	require.NoError(t, err)

	// Every run gets its own bucket. r1 and r3 both have an empty wt_root, so a
	// key built from that raw field would collapse them together and hand
	// whichever was seen first its manager to the other — the same wrong-tree
	// lookup, arriving from the other direction.
	require.Len(t, removers, 3, "three distinct roots, three managers")

	for _, tc := range []struct {
		name string
		run  jiradozer.RunMeta
		want string
	}{
		{"root read back off the recorded path", runs[0], "/roots/old"},
		{"root recorded outright", runs[1], "/roots/pinned"},
		{"no root to be had; ambient is the last resort", runs[2], "/roots/new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := removers[tc.run.RemoverKey()]
			require.NotNil(t, r, "the lookup key must match the key it was built under")
			require.Equal(t, tc.want, removerRoot(t, r))
		})
	}
}

// With nothing to derive from, the current root is the only answer available —
// and it must still be given, or the run has no remover at all and gc reports
// "no worktree manager" instead of sweeping.
func TestRemoversFallBackToTheCurrentRootWhenARecordNamesNone(t *testing.T) {
	t.Setenv("WT_ROOT", "/roots/new")

	removers, err := removersForRuns([]jiradozer.RunMeta{
		{RunID: "r1", Repo: "kernel", Branch: "feature/INF-1"},
	})
	require.NoError(t, err)

	r := removers[jiradozer.RunMeta{Repo: "kernel"}.RemoverKey()]
	require.NotNil(t, r)
	require.Equal(t, "/roots/new", removerRoot(t, r))
}

// leaseHeld is the last gate before an irreversible removal, and it used to
// answer "not held" for every OpenFile failure — so an EACCES or EIO on the
// lease directory read as clearance to delete a live worker's checkout.
func TestAnUnreadableLeaseIsNotAFreeLease(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Make the lease directory a regular file, so opening anything beneath it
	// fails with ENOTDIR rather than ENOENT. Deterministic, and unlike a chmod-0
	// fixture it still fails when the tests run as root.
	leaseDir := jiradozer.ExpandHome(jiradozer.LeaseDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(leaseDir), 0o755))
	require.NoError(t, os.WriteFile(leaseDir, []byte("x"), 0o600))

	held, err := leaseHeld("INF-1")
	require.Error(t, err, "an unopenable lease path must report that it could not tell")
	require.False(t, errors.Is(err, fs.ErrNotExist), "and not as a missing file")
	require.False(t, held)
}

// The ordinary case still has to work: no lease file means nothing has ever run
// this target here. Release leaves the file behind deliberately, so absence —
// not presence — is the only safe reading of ENOENT.
func TestAMissingLeaseFileIsAnAnswer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	held, err := leaseHeld("INF-1")
	require.NoError(t, err)
	require.False(t, held)
}
