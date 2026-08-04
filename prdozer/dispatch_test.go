package prdozer

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcquireLease_ExcludesSecondBabysitter(t *testing.T) {
	// This is THE mutual exclusion. It is held inside the worker process
	// because wrapping tmux in flock(1) does not keep the lock (tmux
	// daemonizes and flock exits) — verified experimentally.
	t.Setenv("HOME", t.TempDir())

	first, err := AcquireLease("sycamore-labs/kernel", 8123)
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.FileExists(t, first.Path())

	_, err = AcquireLease("sycamore-labs/kernel", 8123)
	require.Error(t, err, "a second babysitter on the same PR must be refused")
	assert.True(t, errors.Is(err, ErrLeaseHeld), "callers dispatch elsewhere on ErrLeaseHeld")

	// A DIFFERENT PR is independently lockable.
	other, err := AcquireLease("sycamore-labs/kernel", 9999)
	require.NoError(t, err)
	require.NoError(t, other.Release())

	// After release the PR can be picked up again.
	require.NoError(t, first.Release())
	again, err := AcquireLease("sycamore-labs/kernel", 8123)
	require.NoError(t, err, "a released lease must be re-acquirable")
	require.NoError(t, again.Release())
}

func TestLeasePath_SlugIsNotAPathSeparator(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := LeasePath("sycamore-labs/kernel", 8123)
	assert.Contains(t, path, "sycamore-labs-kernel-8123.lock")
	assert.NotContains(t, strings.TrimPrefix(path, ExpandHome(LeaseDir)), "/kernel",
		"the repo slug must not create a nested directory")
}

func TestLease_ReleaseIsIdempotent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	l, err := AcquireLease("o/r", 1)
	require.NoError(t, err)
	require.NoError(t, l.Release())
	require.NoError(t, l.Release(), "a deferred Release after an explicit one must not error")
	var nilLease *Lease
	require.NoError(t, nilLease.Release())
}

func TestTmuxSessionName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "babysit/kernel#8123", TmuxSessionName("sycamore-labs/kernel", 8123))
	assert.Equal(t, "babysit/yoloswe#7", TmuxSessionName("bazelment/yoloswe", 7))
}

func TestDispatchRequest_KeepWorktreeFlagPropagates(t *testing.T) {
	t.Parallel()
	req := DispatchRequest{
		Host:         HostHealth{PublicDNS: "b.example", HasBinary: true},
		OwnerRepo:    "o/r",
		PRNumber:     1,
		KeepWorktree: true,
	}
	assert.Contains(t, req.RemoteCommand(), "--keep-worktree")
}

func TestAcquireLease_SurvivesProcessScopedUse(t *testing.T) {
	// Regression guard for the design decision: the lock must be tied to the
	// holding process's open file descriptor, so it is released when that
	// process exits and NOT before.
	home := t.TempDir()
	t.Setenv("HOME", home)
	l, err := AcquireLease("o/r", 77)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Release() })

	// The lease file records the holder for a human reading the directory.
	data, err := os.ReadFile(l.Path())
	require.NoError(t, err)
	assert.Contains(t, string(data), fmt.Sprintf("pid=%d", os.Getpid()))
}

// prdozer resolves the agent CLI from PATH and spawns it as a child. `claude`
// lives in ~/.local/bin, which a non-interactive SSH shell does not include —
// so without an explicit PATH the worker starts, detects work correctly, then
// every polish round dies with `exec: "claude": executable file not found`.
// Observed on kernel#8227.
