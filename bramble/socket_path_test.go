package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

	// Neither is pid-scoped. A session's callback address is frozen into its
	// tmux window at creation and can never be updated, so a path that moved
	// with the pid stranded every window whenever bramble came back under a new
	// one. Collision between concurrent brambles is handled at bind time
	// instead — see pidScopedSocketPath and TestPidScopedFallbackIsDistinct.
	pid := strconv.Itoa(os.Getpid())
	for _, p := range []string{ipc, control} {
		if strings.Contains(filepath.Base(p), pid) {
			t.Errorf("socket path %q is pid-scoped; it must survive a restart under a new pid", p)
		}
	}
}

// TestPidScopedFallbackIsDistinct covers the path a second bramble takes when
// the stable one is already served. It must differ from the stable name and
// from the other socket's fallback, or two live brambles would fight over one
// address — the collision the pid used to prevent by construction.
func TestPidScopedFallbackIsDistinct(t *testing.T) {
	ipcFallback := pidScopedSocketPath(userSockName(ipcSockBase))
	controlFallback := pidScopedSocketPath(userSockName(controlSockBase))

	if ipcFallback == ipcSocketPath() || controlFallback == controlSocketPath() {
		t.Errorf("fallback must differ from the stable path: ipc=%q control=%q", ipcFallback, controlFallback)
	}
	if ipcFallback == controlFallback {
		t.Errorf("the two fallbacks collide, both = %q", ipcFallback)
	}

	pid := strconv.Itoa(os.Getpid())
	for _, p := range []string{ipcFallback, controlFallback} {
		if !strings.Contains(filepath.Base(p), pid) {
			t.Errorf("fallback %q is not pid-scoped, so two brambles would collide", p)
		}
		if !strings.HasSuffix(p, ".sock") {
			t.Errorf("fallback %q lost its .sock suffix", p)
		}
	}
}

// The sockets belong in $XDG_RUNTIME_DIR when it is set: a user-private tmpfs,
// rather than a world-writable directory where a symlink could be swapped in.
func TestSocketPathsPreferRuntimeDir(t *testing.T) {
	dir := t.TempDir()
	// A real XDG_RUNTIME_DIR (/run/user/$UID) is 0700; t.TempDir() is 0775, and
	// socketDirPrivate now verifies the directory rather than trusting the
	// variable, so the fixture has to match reality.
	require.NoError(t, os.Chmod(dir, 0o700))
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

// TestSocketPathsFallBackUnderTempDir: with no XDG_RUNTIME_DIR the sockets
// live in a private per-user directory *under* os.TempDir(), not in os.TempDir()
// itself.
//
// This test used to assert the opposite — that the socket's parent WAS
// os.TempDir(). That is the shared, world-writable directory, and since the
// socket name is stable it is also predictable, so any local user could bind it
// first and either receive traffic from windows holding the frozen path or push
// every bramble onto a pid-scoped fallback. The privacy now comes from the
// directory; see TestSocketDirIsPrivateWithoutXDGRuntimeDir.
func TestSocketPathsFallBackUnderTempDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", t.TempDir())

	for name, got := range map[string]string{
		"ipc":     ipcSocketPath(),
		"control": controlSocketPath(),
	} {
		if !filepath.IsAbs(got) {
			t.Errorf("%s socket path %q is not absolute", name, got)
		}
		dir := filepath.Dir(got)
		if dir == os.TempDir() {
			t.Errorf("%s socket %q sits directly in the shared temp dir", name, got)
		}
		if filepath.Dir(dir) != os.TempDir() {
			t.Errorf("%s socket %q is not under %q", name, got, os.TempDir())
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

// TestSocketDirIsPrivateWithoutXDGRuntimeDir pins the property the stable
// socket name depends on for its safety.
//
// The name is stable, so it is predictable by construction. In a shared /tmp
// that is enough for any local user to pre-bind it and receive the IPC and
// control traffic of tmux windows that have the path frozen in their
// environment, or to force every bramble on the host onto a pid-scoped fallback
// — which silently restores the stranding bug the stable path exists to fix.
// Putting the uid in the filename prevents an accidental collision but not a
// deliberate one; only a directory this user alone can traverse does that.
func TestSocketDirIsPrivateWithoutXDGRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", t.TempDir())

	dir := socketDir()
	require.NotEqual(t, os.TempDir(), dir,
		"the fallback must not be the shared temp dir itself")

	info, err := os.Stat(dir)
	require.NoError(t, err, "the private socket directory must exist")
	require.True(t, info.IsDir())
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(),
		"a group- or world-accessible directory is not private")
	assert.True(t, ownedByCurrentUser(info), "the directory must be owned by this user")

	// Both sockets land inside it, so neither is directly reachable.
	assert.Equal(t, dir, filepath.Dir(ipcSocketPath()))
	assert.Equal(t, dir, filepath.Dir(controlSocketPath()))
}

// TestSocketDirRefusesADirectoryItDoesNotOwn: MkdirAll leaves an existing
// directory's mode alone, so a squatter who pre-creates the name with open
// permissions would otherwise be trusted silently.
func TestSocketDirRefusesAWorldWritableDirectory(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", tmp)

	squat := filepath.Join(tmp, fmt.Sprintf("bramble-%d", os.Getuid()))
	require.NoError(t, os.MkdirAll(squat, 0o777))
	require.NoError(t, os.Chmod(squat, 0o777))

	assert.Equal(t, os.TempDir(), socketDir(),
		"a directory with open permissions must be refused, not used")
}

// TestStableNameIsNotUsedInAnUnprivateDirectory: a stable socket name is
// predictable by construction, so publishing one in a shared directory lets any
// local user bind it first and receive the IPC and control traffic of windows
// that have the path frozen in their environment.
//
// When the private directory cannot be established, the earlier code still
// published the stable name into the shared temp dir — the exact exposure the
// privacy work was meant to remove. It must degrade to a pid-scoped name
// instead: that costs restart survival on such a host, which is a liveness
// cost, not a confidentiality one.
func TestStableNameIsNotUsedInAnUnprivateDirectory(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", tmp)

	// A squatted directory with open permissions: private-directory setup fails.
	squat := filepath.Join(tmp, fmt.Sprintf("bramble-%d", os.Getuid()))
	require.NoError(t, os.MkdirAll(squat, 0o777))
	require.NoError(t, os.Chmod(squat, 0o777))

	_, private := socketDirPrivate()
	require.False(t, private, "precondition: the directory must be judged unprivate")

	pid := strconv.Itoa(os.Getpid())
	for name, got := range map[string]string{
		"ipc":     ipcSocketPath(),
		"control": controlSocketPath(),
	} {
		assert.Contains(t, filepath.Base(got), pid,
			"%s socket %q must be pid-scoped when no private directory is available", name, got)
	}
}

// TestUnprivateRuntimeDirIsNotTrusted: XDG_RUNTIME_DIR is an environment
// variable, so it can be inherited from another user's session or point
// somewhere world-writable. Trusting it would publish a stable, predictable
// socket name into a directory anyone can write to — the exposure the private
// directory exists to close.
func TestUnprivateRuntimeDirIsNotTrusted(t *testing.T) {
	shared := t.TempDir()
	require.NoError(t, os.Chmod(shared, 0o777))
	t.Setenv("XDG_RUNTIME_DIR", shared)
	t.Setenv("TMPDIR", t.TempDir())

	dir, private := socketDirPrivate()
	assert.NotEqual(t, shared, dir, "a world-writable runtime dir must not be used")
	assert.True(t, private, "the private fallback directory should still be available")
	assert.True(t, isPrivateDir(dir))
}
