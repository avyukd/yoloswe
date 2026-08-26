package control

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/tmuxctl"
)

// pipeConns returns two control.Conns connected via an in-memory net.Pipe.
func pipeConns() (Conn, Conn) {
	a, b := net.Pipe()
	return NewJSONConn(a), NewJSONConn(b)
}

// TestUnixServerRoundTrip drives a real control.UnixServer over a Unix socket
// (under t.TempDir, no static path) and asserts a send-input request reaches the
// dispatcher and the fake controller. Exercises the local transport end-to-end.
func TestUnixServerRoundTrip(t *testing.T) {
	t.Parallel()

	reg := &fakeRegistry{targets: map[string]string{"s1": "@4"}}
	ctl := tmuxctl.NewFake()
	disp := NewDispatcher(reg, ctl)

	sock := filepath.Join(t.TempDir(), "control.sock")
	srv := NewUnixServer(sock, disp)
	require.NoError(t, srv.Start())
	t.Cleanup(func() { _ = srv.Close() })

	req, err := NewRequest(TypeSessionSendInput, "r1",
		SendInputReq{SessionID: "s1", Text: "hello", Submit: true})
	require.NoError(t, err)

	resp, err := Request(context.Background(), sock, req)
	require.NoError(t, err)
	require.Equal(t, "r1", resp.ID, "response echoes the request ID")

	var ok OKResult
	require.NoError(t, resp.DecodeResponse(&ok))
	assert.True(t, ok.OK)

	pastes := ctl.CallsFor("Paste")
	require.Len(t, pastes, 1)
	assert.Equal(t, "@4", pastes[0].Target)
	assert.Equal(t, "hello", pastes[0].Text)
}

// TestUnixServerErrorRoundTrip verifies a dispatcher error is carried back as a
// RemoteError over the transport.
func TestUnixServerErrorRoundTrip(t *testing.T) {
	t.Parallel()

	reg := &fakeRegistry{targets: map[string]string{}} // s1 not present
	srv := NewUnixServer(filepath.Join(t.TempDir(), "c.sock"), NewDispatcher(reg, tmuxctl.NewFake()))
	require.NoError(t, srv.Start())
	t.Cleanup(func() { _ = srv.Close() })

	req, err := NewRequest(TypeSessionSendKey, "r2",
		SendKeyReq{SessionID: "s1", Key: tmuxctl.KeyCtrlC})
	require.NoError(t, err)

	resp, err := Request(context.Background(), srv.SocketPath(), req)
	require.NoError(t, err)

	derr := resp.DecodeResponse(nil)
	require.Error(t, derr)
	var re *RemoteError
	assert.ErrorAs(t, derr, &re)
}

// TestJSONConnFraming round-trips multiple messages over an in-memory pipe to
// confirm newline framing handles back-to-back messages.
func TestJSONConnFraming(t *testing.T) {
	t.Parallel()

	c1, c2 := pipeConns()
	defer c1.Close()
	defer c2.Close()

	go func() {
		_ = c1.WriteMsg(&Msg{Type: TypeSessionList, ID: "a"})
		_ = c1.WriteMsg(&Msg{Type: TypePaneKill, ID: "b"})
	}()

	m1, err := c2.ReadMsg()
	require.NoError(t, err)
	assert.Equal(t, "a", m1.ID)
	m2, err := c2.ReadMsg()
	require.NoError(t, err)
	assert.Equal(t, "b", m2.ID)
}

// newTestServer builds a control server on a fresh socket path.
func newTestServer(t *testing.T, sock string) *UnixServer {
	t.Helper()
	reg := &fakeRegistry{targets: map[string]string{"s1": "@4"}}
	return NewUnixServer(sock, NewDispatcher(reg, tmuxctl.NewFake()))
}

// TestControlLiveSocketIsNotStolen: the control socket carries the same
// stability contract as the IPC one — its path is frozen into every tmux window
// at creation and can never be updated — so a second bramble must refuse a path
// a live peer is serving rather than unlink it.
//
// The sentinel matters beyond the behaviour: main.go branches on
// errors.Is(err, control.ErrSocketInUse) to decide whether to take the
// pid-scoped fallback. If Start ever reported this as a plain listen error,
// bramble would silently run with no control socket instead of falling back.
func TestControlLiveSocketIsNotStolen(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(t.TempDir(), "live.sock")

	winner := newTestServer(t, sock)
	require.NoError(t, winner.Start())
	t.Cleanup(func() { _ = winner.Close() })

	loser := newTestServer(t, sock)
	require.ErrorIs(t, loser.Start(), ErrSocketInUse,
		"the sentinel is a contract: main.go keys its fallback off it")
	require.NoError(t, loser.Close(), "closing an unbound server must be harmless")

	// The winner is still reachable: nothing unlinked its socket.
	_, err := os.Stat(sock)
	require.NoError(t, err, "the live socket file must survive")
	req, err := NewRequest(TypeSessionSendInput, "r1",
		SendInputReq{SessionID: "s1", Text: "hello", Submit: true})
	require.NoError(t, err)
	_, err = Request(context.Background(), sock, req)
	require.NoError(t, err, "the winner must still be serving")
}

// TestControlStaleSocketIsReclaimed: a socket file left by a killed process has
// no listener, so it must be removed and rebound. Without this a crash would
// make the stable control path permanently unusable.
func TestControlStaleSocketIsReclaimed(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(t.TempDir(), "stale.sock")

	// Go's UnixListener unlinks on Close, so SetUnlinkOnClose(false) is what
	// leaves the file behind the way a killed process does.
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, ln.Close())
	_, err = os.Stat(sock)
	require.NoError(t, err, "precondition: a socket file is left on disk")

	srv := newTestServer(t, sock)
	require.NoError(t, srv.Start(), "a stale socket file must be reclaimed")
	t.Cleanup(func() { _ = srv.Close() })
}

// TestControlConcurrentStaleReclaim: binding first settles a contest over a
// LIVE socket but not a stale one — two processes can both find nothing
// listening, and the second would unlink the first's now-live socket. The
// reclaim is serialized by a lock file; this pins that exactly one wins and the
// winner is still the one serving.
func TestControlConcurrentStaleReclaim(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(t.TempDir(), "stale-contended.sock")

	seed, err := net.Listen("unix", sock)
	require.NoError(t, err)
	seed.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, seed.Close())

	const racers = 8
	start := make(chan struct{})
	results := make(chan error, racers)
	servers := make([]*UnixServer, racers)
	for i := range servers {
		srv := newTestServer(t, sock)
		servers[i] = srv
		go func() {
			<-start
			results <- srv.Start()
		}()
	}
	close(start)

	var won int
	for i := 0; i < racers; i++ {
		if err := <-results; err == nil {
			won++
		} else {
			require.ErrorIs(t, err, ErrSocketInUse)
		}
	}
	require.Equal(t, 1, won, "exactly one server may reclaim a stale socket")
	for _, srv := range servers {
		t.Cleanup(func() { _ = srv.Close() })
	}

	req, err := NewRequest(TypeSessionSendInput, "r1",
		SendInputReq{SessionID: "s1", Text: "hello", Submit: true})
	require.NoError(t, err)
	_, err = Request(context.Background(), sock, req)
	require.NoError(t, err, "the winner must still own the path after the contention")
}

// TestCloseDoesNotUnlinkASuccessorsSocket mirrors the ipc.Server test of the
// same name: both servers bind a stable per-user path, so both can hand it to a
// successor mid-drain. ln.Close() unlinks at once, but wg.Wait() blocks for as
// long as an in-flight handler runs, and a trailing os.Remove after that drain
// would delete whatever now holds the path — the successor's socket, not ours.
func TestCloseDoesNotUnlinkASuccessorsSocket(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(t.TempDir(), "handoff.sock")

	inHandler := make(chan struct{})
	releaseHandler := make(chan struct{})
	reg := &fakeRegistry{
		targets:   map[string]string{"s1": "@4"},
		onResolve: func() { close(inHandler); <-releaseHandler },
	}
	old := NewUnixServer(sock, NewDispatcher(reg, tmuxctl.NewFake()))
	require.NoError(t, old.Start())

	req, err := NewRequest(TypeSessionSendInput, "r1",
		SendInputReq{SessionID: "s1", Text: "hello", Submit: true})
	require.NoError(t, err)
	go func() { _, _ = Request(context.Background(), sock, req) }()
	select {
	case <-inHandler:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never ran; the drain would not block")
	}

	closed := make(chan struct{})
	go func() { defer close(closed); _ = old.Close() }()

	require.Eventually(t, func() bool {
		_, statErr := os.Stat(sock)
		return os.IsNotExist(statErr)
	}, 5*time.Second, 10*time.Millisecond, "the old listener must release the path")

	successor := NewUnixServer(sock,
		NewDispatcher(&fakeRegistry{targets: map[string]string{"s2": "@9"}}, tmuxctl.NewFake()))
	require.NoError(t, successor.Start(), "the successor binds the freed stable path")
	t.Cleanup(func() { _ = successor.Close() })

	close(releaseHandler)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("old server never finished closing")
	}

	_, err = os.Stat(sock)
	require.NoError(t, err, "the predecessor's Close must not delete the successor's socket file")

	// Reachability, not just presence: the successor must still answer here.
	ping, err := NewRequest(TypeSessionSendInput, "r2",
		SendInputReq{SessionID: "s2", Text: "hi", Submit: true})
	require.NoError(t, err)
	_, err = Request(context.Background(), sock, ping)
	require.NoError(t, err, "the successor must still be reachable at the stable path")
}
