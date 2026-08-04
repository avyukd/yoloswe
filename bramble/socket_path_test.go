package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/bramble/session"
)

// Covers path derivation only — NOT the startup ordering that depends on it.
// See TestPublishSockPaths* for the publication behavior and
// TestStartIPCServerBindsThePublishedPath for the publish/bind equality.
//
// The ordering fix in runTUI is only possible because a path is knowable before
// its server exists: if either helper grew a dependency on a started server, the
// startup window would silently reopen. That is the property pinned here.
func TestSocketPathDerivation(t *testing.T) {
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

// publishSockPaths must write BOTH destinations: the live manager (whose
// sessions read it via newTmuxRunner) and the shared config template that
// openRepo copies for repos opened later with Alt-R. Updating only one is how a
// secondary repo's sessions silently lose a socket.
func TestPublishSockPathsWritesManagerAndSharedConfig(t *testing.T) {
	t.Parallel()

	m := session.NewManager()
	shared := session.ManagerConfig{}

	const (
		ipcPath     = "/run/user/1000/bramble-42.sock"
		controlPath = "/run/user/1000/bramble-control-42.sock"
	)
	publishSockPaths(m, &shared, ipcPath, controlPath)

	if got := m.IPCSockPath(); got != ipcPath {
		t.Errorf("manager IPC path = %q, want %q", got, ipcPath)
	}
	if got := m.ControlSockPath(); got != controlPath {
		t.Errorf("manager control path = %q, want %q", got, controlPath)
	}
	if shared.IPCSockPath != ipcPath {
		t.Errorf("shared config IPC path = %q, want %q — sessions in repos opened via Alt-R would lose it", shared.IPCSockPath, ipcPath)
	}
	if shared.ControlSockPath != controlPath {
		t.Errorf("shared config control path = %q, want %q — sessions in repos opened via Alt-R would lose it", shared.ControlSockPath, controlPath)
	}
}

// When the control server fails to bind, runTUI publishes an empty control path
// rather than the path nothing is listening on. Clearing must reach the shared
// config too, or Alt-R sessions would export a dead socket.
func TestPublishSockPathsClearsBothOnEmpty(t *testing.T) {
	t.Parallel()

	m := session.NewManager()
	shared := session.ManagerConfig{}

	publishSockPaths(m, &shared, "/run/bramble.sock", "/run/bramble-control.sock")
	publishSockPaths(m, &shared, "/run/bramble.sock", "")

	if got := m.ControlSockPath(); got != "" {
		t.Errorf("manager control path = %q, want empty after a failed control bind", got)
	}
	if shared.ControlSockPath != "" {
		t.Errorf("shared config control path = %q, want empty after a failed control bind", shared.ControlSockPath)
	}
	// Clearing one socket must not disturb the other.
	if got := m.IPCSockPath(); got != "/run/bramble.sock" {
		t.Errorf("IPC path = %q, want it untouched by a control-socket clear", got)
	}
}

// The whole ordering fix rests on the manager advertising the path the IPC
// server actually binds. startIPCServer takes that path as a parameter rather
// than recomputing it; this pins that it binds what it was given, so the value
// published to the manager beforehand cannot be stale.
func TestStartIPCServerBindsThePublishedPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)

	published := ipcSocketPath()
	registry := session.NewSessionRegistry()

	srv := startIPCServer(registry, published, dir, "repo")
	if srv == nil {
		t.Fatal("startIPCServer returned nil; expected a bound server")
	}
	defer srv.Close()

	if got := srv.SocketPath(); got != published {
		t.Errorf("server bound %q but the manager was told %q — sessions would hold a dead socket", got, published)
	}
	if _, err := os.Stat(published); err != nil {
		t.Errorf("nothing listening at the published path %q: %v", published, err)
	}
}
