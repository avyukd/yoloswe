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

// TestSocketPathDerivation pins that paths are knowable before their servers
// start. Publication and bind equality are covered by the TestPublishSockPaths*
// and TestStartIPCServerBindsThePublishedPath cases.
// If derivation starts depending on a running server, the startup ordering
// window silently reopens.
func TestSocketPathDerivation(t *testing.T) {
	ipc := ipcSocketPath()
	control := controlSocketPath()

	if ipc == "" || control == "" {
		t.Fatalf("socket paths must be non-empty before any server starts: ipc=%q control=%q", ipc, control)
	}
	if ipc == control {
		t.Errorf("IPC and control servers must not share a socket path, both = %q", ipc)
	}

	// Repeated calls must agree with the value later published to the manager.
	if got := ipcSocketPath(); got != ipc {
		t.Errorf("ipcSocketPath() not deterministic: %q then %q", ipc, got)
	}
	if got := controlSocketPath(); got != control {
		t.Errorf("controlSocketPath() not deterministic: %q then %q", control, got)
	}

	// Sessions freeze the callback path in tmux, so the stable path must survive
	// a restart under a new pid. Concurrent collisions are handled at bind time.
	// A pid-scoped address stranded every existing window after restart.
	pid := strconv.Itoa(os.Getpid())
	for _, p := range []string{ipc, control} {
		if strings.Contains(filepath.Base(p), pid) {
			t.Errorf("socket path %q is pid-scoped; it must survive a restart under a new pid", p)
		}
	}
}

// TestPidScopedFallbackIsDistinct covers the path a second bramble takes when
// the stable one is already served. The fallback must stay per-process and
// per-socket.
// Otherwise two live brambles fight over the same address.
func TestPidScopedFallbackIsDistinct(t *testing.T) {
	ipcFallback := pidScopedSocketPath(ipcSockBase)
	controlFallback := pidScopedSocketPath(controlSockBase)

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

// TestSocketPathsPreferRuntimeDir pins $XDG_RUNTIME_DIR as the preferred
// user-private socket root.
// That avoids world-writable directories where a symlink can be swapped in.
func TestSocketPathsPreferRuntimeDir(t *testing.T) {
	dir := t.TempDir()
	// Match real /run/user/$UID permissions; socketDirPrivate verifies them.
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

// TestSocketPathsFallBackUnderTempDir pins the fallback root: with no
// XDG_RUNTIME_DIR, sockets live in a private per-user directory under os.TempDir,
// never directly in the shared temp directory.
// A stable name directly in shared temp is predictable enough for another local
// user to pre-bind it or force pid-scoped fallback.
// The privacy comes from the directory, not from the filename.
func TestSocketPathsFallBackUnderTempDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", t.TempDir())
	// Refusal without a private directory is covered separately.

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

// TestPublishSockPathsWritesManagerAndSharedConfig pins both publication
// destinations: live manager and shared config for repos opened later with Alt-R.
// Updating only one silently drops sockets for secondary repo sessions.
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

// TestPublishSockPathsClearsBothOnEmpty pins failed-bind cleanup: an empty path
// must clear the shared config too, or Alt-R sessions export a dead socket.
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
	if got := m.IPCSockPath(); got != "/run/bramble.sock" {
		t.Errorf("IPC path = %q, want it untouched by a control-socket clear", got)
	}
}

// TestStartIPCServerBindsThePublishedPath pins the startup ordering: the
// manager advertises the same path startIPCServer later binds.
// startIPCServer takes that path as a parameter rather than recomputing it.
func TestStartIPCServerBindsThePublishedPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)

	published := ipcSocketPath()
	registry := session.NewSessionRegistry()

	srv, err := startIPCServer(registry, published, dir, "repo")
	if err != nil {
		t.Fatalf("startIPCServer failed to bind %q: %v", published, err)
	}
	defer srv.Close()

	if got := srv.SocketPath(); got != published {
		t.Errorf("server bound %q but the manager was told %q — sessions would hold a dead socket", got, published)
	}
	if _, err := os.Stat(published); err != nil {
		t.Errorf("nothing listening at the published path %q: %v", published, err)
	}
}

// TestSocketDirIsPrivateWithoutXDGRuntimeDir pins the security property the
// stable socket name depends on: predictable names are safe only inside a
// directory this user alone can traverse.
// A shared directory lets another local user receive frozen-window traffic or
// push every bramble back to the stranding-prone pid fallback.
// The uid in the filename avoids accidental collisions, not deliberate ones.
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

	// Both sockets land inside it.
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

// TestNoSocketIsPublishedWithoutAPrivateDirectory pins fail-closed behavior:
// without a private directory, both stable and pid-scoped socket paths are
// exposed enough that bramble must publish neither.
// Pids are observable, and abandoned pid-scoped paths can be pre-bound before a
// restart by another local user.
// Running without IPC and control is safer than serving them over a path anyone
// can take.
func TestNoSocketIsPublishedWithoutAPrivateDirectory(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", tmp)

	// A squatted directory with open permissions.
	squat := filepath.Join(tmp, fmt.Sprintf("bramble-%d", os.Getuid()))
	require.NoError(t, os.MkdirAll(squat, 0o777))
	require.NoError(t, os.Chmod(squat, 0o777))

	_, private := socketDirPrivate()
	require.False(t, private, "precondition: the directory must be judged unprivate")

	assert.Empty(t, ipcSocketPath(), "no IPC socket may be published")
	assert.Empty(t, controlSocketPath(), "no control socket may be published")
	assert.Empty(t, pidScopedSocketPath(ipcSockBase),
		"the collision fallback derives its own path and must refuse too")
}

// TestUnprivateRuntimeDirIsNotTrusted pins that XDG_RUNTIME_DIR is still just an
// environment variable; world-writable values must be rejected.
// Trusting one would publish stable, predictable sockets into a directory anyone
// can write to.
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

// TestSymlinkedSocketDirIsNotPrivate pins the symlink/TOCTOU hazard: privacy
// checks must judge the directory entry bramble will use, not a private target
// chosen through a link.
// os.Stat follows the link, so target-only checks would bless an attacker-chosen
// socket location.
// MkdirAll would succeed on the link before those target checks ran.
func TestSymlinkedSocketDirIsNotPrivate(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// Private by every other measure; reached through a link it is still refused.
	target := filepath.Join(root, "attacker-chosen")
	require.NoError(t, os.Mkdir(target, 0o700))
	require.True(t, isPrivateDir(target), "the target itself is private; only the link is the problem")

	link := filepath.Join(root, "bramble-link")
	require.NoError(t, os.Symlink(target, link))

	assert.False(t, isPrivateDir(link),
		"a symlink is never a private directory we created, however private its target")
}
