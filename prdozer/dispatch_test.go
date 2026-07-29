package prdozer

import (
	"context"
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

func TestDispatchRequest_RemoteCommand(t *testing.T) {
	t.Parallel()
	req := DispatchRequest{
		Host:         HostHealth{Host: "box", PublicDNS: "box.example", SSHUser: "ubuntu", HasPrdozer: true},
		OwnerRepo:    "sycamore-labs/kernel",
		PRNumber:     8123,
		RegistryPath: "~/magent/prdozer/registry.yaml",
	}
	cmd := req.RemoteCommand()
	assert.Contains(t, cmd, "tmux new-session -d")
	assert.Contains(t, cmd, "babysit/kernel#8123", "the session must be attachable by a recognisable name")
	assert.Contains(t, cmd, "babysit-local")
	assert.Contains(t, cmd, "--pr 8123")
	assert.Contains(t, cmd, "sycamore-labs/kernel")

	// flock(1) must NOT appear: it looks like mutual exclusion but drops the
	// lock the moment tmux daemonizes. The worker takes the lease itself.
	assert.NotContains(t, cmd, "flock",
		"flock(1) around tmux does not hold the lock; the worker must acquire it internally")
}

func TestDispatchRequest_SSHCommandIsPrintableForDryRun(t *testing.T) {
	t.Parallel()
	// --dry-run prints this verbatim; it is the primary debugging surface for
	// dispatch, so it must be a command a human can paste and run.
	req := DispatchRequest{
		Host:      HostHealth{Host: "box", PublicDNS: "box.example", SSHUser: "ming", HasPrdozer: true},
		OwnerRepo: "bazelment/yoloswe",
		PRNumber:  42,
	}
	got := req.SSHCommand()
	assert.Contains(t, got, "ssh -o BatchMode=yes -o ConnectTimeout=8 -o StrictHostKeyChecking=accept-new")
	assert.Contains(t, got, "ming@box.example")
	assert.Contains(t, got, "babysit-local")
}

func TestDispatchRequest_UsesResolvedBinaryPath(t *testing.T) {
	t.Parallel()
	// A non-interactive SSH shell's PATH omits ~/bin (verified against the
	// real fleet), so a bare "prdozer" produces a tmux session that dies with
	// "command not found" — indistinguishable from a silent no-op.
	req := DispatchRequest{
		Host: HostHealth{
			PublicDNS:   "b.example",
			HasPrdozer:  true,
			PrdozerPath: "/home/ming/bin/prdozer",
		},
		OwnerRepo: "o/r",
		PRNumber:  1,
	}
	assert.Contains(t, req.RemoteCommand(), "/home/ming/bin/prdozer babysit-local")

	// With no resolved path, fall back to the bare name rather than emitting
	// an empty command.
	bare := DispatchRequest{
		Host:      HostHealth{PublicDNS: "b.example", HasPrdozer: true},
		OwnerRepo: "o/r",
		PRNumber:  1,
	}
	assert.Contains(t, bare.RemoteCommand(), "prdozer babysit-local")
}

func TestDispatchRequest_KeepWorktreeFlagPropagates(t *testing.T) {
	t.Parallel()
	req := DispatchRequest{
		Host:         HostHealth{PublicDNS: "b.example", HasPrdozer: true},
		OwnerRepo:    "o/r",
		PRNumber:     1,
		KeepWorktree: true,
	}
	assert.Contains(t, req.RemoteCommand(), "--keep-worktree")
}

func TestShellQuote_EscapesSingleQuotes(t *testing.T) {
	t.Parallel()
	// A branch or repo name containing a quote must not break out of the
	// remote command.
	got := shellQuote(`it's`)
	assert.Equal(t, `'it'\''s'`, got)
}

func TestDispatch_RefusesHostWithoutPrdozer(t *testing.T) {
	t.Parallel()
	// Dispatching to a box lacking the binary creates a tmux session that dies
	// instantly — indistinguishable from a silent no-op.
	ssh := &fakeSSH{out: map[string]string{}}
	err := Dispatch(context.Background(), ssh, DispatchRequest{
		Host:      HostHealth{Host: "bare", PublicDNS: "bare.example", HasPrdozer: false},
		OwnerRepo: "o/r",
		PRNumber:  1,
	}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PATH")
	assert.Empty(t, ssh.seen, "nothing should have been dispatched")
}

func TestDispatch_SendsCommandToChosenHost(t *testing.T) {
	t.Parallel()
	ssh := &fakeSSH{out: map[string]string{"ming@box.example": ""}}
	err := Dispatch(context.Background(), ssh, DispatchRequest{
		Host:      HostHealth{Host: "box", PublicDNS: "box.example", SSHUser: "ming", HasPrdozer: true},
		OwnerRepo: "o/r",
		PRNumber:  5,
	}, nil)
	require.NoError(t, err)
	require.Len(t, ssh.seen, 1)
	assert.Equal(t, "ming@box.example", ssh.seen[0])
}

func TestDispatch_SurfacesRemoteFailure(t *testing.T) {
	t.Parallel()
	ssh := &fakeSSH{errs: map[string]error{"ubuntu@box.example": fmt.Errorf("permission denied")}}
	err := Dispatch(context.Background(), ssh, DispatchRequest{
		Host:      HostHealth{Host: "box", PublicDNS: "box.example", SSHUser: "ubuntu", HasPrdozer: true},
		OwnerRepo: "o/r",
		PRNumber:  5,
	}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission denied")
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
func TestDispatchRequest_RemoteCommandPutsUserBinOnPath(t *testing.T) {
	t.Parallel()
	req := DispatchRequest{
		Host:      HostHealth{Host: "box", PublicDNS: "box.example", SSHUser: "ming", HasPrdozer: true},
		OwnerRepo: "sycamore-labs/kernel",
		PRNumber:  8227,
	}
	req.Host.PrdozerPath = "/home/ming/bin/prdozer"
	cmd := req.RemoteCommand()
	assert.Contains(t, cmd, "tmux new-session -d -e",
		"PATH goes through tmux -e, not a nested sh -c wrapper that needs three levels of quoting")
	assert.Contains(t, cmd, "/home/ming/.local/bin",
		"the agent CLI lives in ~/.local/bin and is resolved from PATH")
	assert.Contains(t, cmd, "/home/ming/bin")
	assert.Contains(t, cmd, "/usr/bin",
		"tmux -e replaces PATH wholesale, so the base entries must be carried")
	assert.NotContains(t, cmd, "$HOME",
		"tmux -e does no shell expansion; the value must be literal")
}

func TestDispatchRequest_RemotePathEnvUnresolvableIsOmitted(t *testing.T) {
	t.Parallel()
	// A bare "prdozer" (probe could not resolve an absolute path) gives no
	// basis for deriving the home root. Leave PATH untouched rather than
	// setting a wrong one.
	req := DispatchRequest{
		Host:      HostHealth{Host: "box", PublicDNS: "box.example", SSHUser: "ming"},
		OwnerRepo: "o/r",
		PRNumber:  1,
	}
	assert.Empty(t, req.remotePathEnv())
	assert.NotContains(t, req.RemoteCommand(), "-e ", "no -e flag when PATH cannot be derived")
}
