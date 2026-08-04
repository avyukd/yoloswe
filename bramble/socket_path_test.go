package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// runTUI publishes both socket paths to the session manager before either
// server starts listening, because the IPC server serves RequestNewSession the
// moment it binds and that handler runs all the way to newTmuxRunner, which
// snapshots the control socket into the window environment for the session's
// lifetime. That ordering is only possible because both paths derive from the
// pid alone. These cases pin that property: if either helper grew a dependency
// on a started server, the paths would stop being knowable up front and the
// startup window would silently reopen.
func TestSocketPathsAreKnowableBeforeListening(t *testing.T) {
	ipc := ipcSocketPath()
	control := controlSocketPath()

	if ipc == "" || control == "" {
		t.Fatalf("socket paths must be non-empty before any server starts: ipc=%q control=%q", ipc, control)
	}
	if ipc == control {
		t.Errorf("IPC and control servers must not share a socket path, both = %q", ipc)
	}

	// Deterministic: repeated calls agree, so the value published to the manager
	// is the same one the server later binds.
	if got := ipcSocketPath(); got != ipc {
		t.Errorf("ipcSocketPath() not deterministic: %q then %q", ipc, got)
	}
	if got := controlSocketPath(); got != control {
		t.Errorf("controlSocketPath() not deterministic: %q then %q", control, got)
	}

	// Both are pid-scoped, so concurrent bramble processes do not collide.
	pid := strconv.Itoa(os.Getpid())
	for _, p := range []string{ipc, control} {
		if !strings.Contains(filepath.Base(p), pid) {
			t.Errorf("socket path %q does not contain pid %s; concurrent bramble processes would collide", p, pid)
		}
	}
}

// The sockets belong in $XDG_RUNTIME_DIR when it is set: a user-private tmpfs,
// rather than a world-writable directory where a symlink could be swapped in.
func TestSocketPathsPreferRuntimeDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)

	for name, got := range map[string]string{
		"ipc":     ipcSocketPath(),
		"control": controlSocketPath(),
	} {
		if filepath.Dir(got) != dir {
			t.Errorf("%s socket %q is not under $XDG_RUNTIME_DIR %q", name, got, dir)
		}
	}
}

// With no runtime dir configured, fall back to the temp dir rather than
// producing a path relative to the cwd.
func TestSocketPathsFallBackToTempDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")

	for name, got := range map[string]string{
		"ipc":     ipcSocketPath(),
		"control": controlSocketPath(),
	} {
		if !filepath.IsAbs(got) {
			t.Errorf("%s socket path %q is not absolute", name, got)
		}
		if filepath.Dir(got) != os.TempDir() {
			t.Errorf("%s socket %q did not fall back to %q", name, got, os.TempDir())
		}
	}
}
