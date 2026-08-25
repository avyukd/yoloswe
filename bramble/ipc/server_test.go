package ipc

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPingRoundTrip(t *testing.T) {
	t.Parallel()
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	srv := NewServer(sockPath)
	srv.Handle(RequestPing, func(_ context.Context, _ *Request) (any, error) {
		return "pong", nil
	})
	require.NoError(t, srv.Start())
	defer srv.Close()

	client := NewClient(sockPath)
	require.NoError(t, client.Ping())
}

func TestNewSessionRoundTrip(t *testing.T) {
	t.Parallel()
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	srv := NewServer(sockPath)
	srv.Handle(RequestNewSession, func(_ context.Context, req *Request) (any, error) {
		params := req.Params.(*NewSessionParams)
		return &NewSessionResult{
			SessionID:    "test-session-123",
			WorktreePath: "/tmp/wt/" + params.Branch,
		}, nil
	})
	require.NoError(t, srv.Start())
	defer srv.Close()

	client := NewClient(sockPath)
	resp, err := client.Send(&Request{
		Type: RequestNewSession,
		ID:   "req-1",
		Params: &NewSessionParams{
			SessionType:    "planner",
			Branch:         "feature/foo",
			CreateWorktree: true,
			Prompt:         "implement OAuth",
		},
	})
	require.NoError(t, err)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	require.Equal(t, "req-1", resp.ID)
}

func TestListSessionsRoundTrip(t *testing.T) {
	t.Parallel()
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	srv := NewServer(sockPath)
	srv.Handle(RequestListSessions, func(_ context.Context, _ *Request) (any, error) {
		return &ListSessionsResult{
			Sessions: []SessionSummary{
				{ID: "s1", Type: "planner", Status: "running", WorktreeName: "main"},
			},
		}, nil
	})
	require.NoError(t, srv.Start())
	defer srv.Close()

	client := NewClient(sockPath)
	resp, err := client.Send(&Request{Type: RequestListSessions, ID: "req-2"})
	require.NoError(t, err)
	require.True(t, resp.OK)
}

func TestUnknownRequestType(t *testing.T) {
	t.Parallel()
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	srv := NewServer(sockPath)
	require.NoError(t, srv.Start())
	defer srv.Close()

	client := NewClient(sockPath)
	resp, err := client.Send(&Request{Type: "bogus", ID: "req-3"})
	require.NoError(t, err)
	require.False(t, resp.OK)
	require.Contains(t, resp.Error, "unknown request type")
}

func TestClientFromEnvNotSet(t *testing.T) {
	// Cannot use t.Parallel() with t.Setenv()
	t.Setenv(SockEnvVar, "")
	_, err := NewClientFromEnv()
	require.Error(t, err)
	require.Contains(t, err.Error(), SockEnvVar)
}

func TestStaleSocketRemoved(t *testing.T) {
	t.Parallel()
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	// Create a stale file at the socket path before any server starts.
	require.NoError(t, os.WriteFile(sockPath, []byte("stale"), 0o600))

	// Server should remove the stale file and start successfully.
	srv := NewServer(sockPath)
	srv.Handle(RequestPing, func(_ context.Context, _ *Request) (any, error) {
		return "pong", nil
	})
	require.NoError(t, srv.Start())
	defer srv.Close()

	client := NewClient(sockPath)
	require.NoError(t, client.Ping())
}

func TestNotifyRoundTrip(t *testing.T) {
	t.Parallel()
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	var receivedSessionID string
	srv := NewServer(sockPath)
	srv.Handle(RequestNotify, func(_ context.Context, req *Request) (any, error) {
		params, ok := req.Params.(*NotifyParams)
		if !ok {
			return nil, fmt.Errorf("invalid params type")
		}
		receivedSessionID = params.SessionID
		return "ok", nil
	})
	require.NoError(t, srv.Start())
	defer srv.Close()

	client := NewClient(sockPath)
	resp, err := client.Send(&Request{
		Type:   RequestNotify,
		ID:     "req-notify",
		Params: &NotifyParams{SessionID: "main-builder-abc123"},
	})
	require.NoError(t, err)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	require.Equal(t, "req-notify", resp.ID)
	require.Equal(t, "main-builder-abc123", receivedSessionID)
}

func TestHandlerError(t *testing.T) {
	t.Parallel()
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	srv := NewServer(sockPath)
	srv.Handle(RequestNewSession, func(_ context.Context, _ *Request) (any, error) {
		return nil, fmt.Errorf("worktree not found")
	})
	require.NoError(t, srv.Start())
	defer srv.Close()

	client := NewClient(sockPath)
	resp, err := client.Send(&Request{
		Type:   RequestNewSession,
		ID:     "req-err",
		Params: &NewSessionParams{Prompt: "test"},
	})
	require.NoError(t, err)
	require.False(t, resp.OK)
	require.Equal(t, "req-err", resp.ID)
	require.Contains(t, resp.Error, "worktree not found")
}

// TestStaleSocketIsRebound: a socket file left behind by a killed bramble has
// no listener, so the path is free and must be reclaimed. Without this a crash
// would make the stable path unusable until someone deleted it by hand.
func TestStaleSocketIsRebound(t *testing.T) {
	t.Parallel()
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	// Abandon the file without unlinking, as a SIGKILL would. Go removes the
	// socket on a graceful Close, so SetUnlinkOnClose(false) is what makes this
	// a crash rather than a clean shutdown.
	first := NewServer(sockPath)
	require.NoError(t, first.Start())
	unixLn, ok := first.listener.(*net.UnixListener)
	require.True(t, ok, "expected a unix listener")
	unixLn.SetUnlinkOnClose(false)
	require.NoError(t, unixLn.Close())
	require.FileExists(t, sockPath, "the socket file outlives the listener")

	second := NewServer(sockPath)
	second.Handle(RequestPing, func(_ context.Context, _ *Request) (any, error) {
		return "pong", nil
	})
	require.NoError(t, second.Start(), "a stale socket must be reclaimed")
	defer second.Close()

	require.NoError(t, NewClient(sockPath).Ping())
}

// TestLiveSocketIsNotStolen is what protects a second bramble from stranding
// the first one's sessions. The path is stable across restarts and frozen into
// every tmux window's environment, so unlinking a socket someone is still
// serving would silently cut off every session that depends on it.
func TestLiveSocketIsNotStolen(t *testing.T) {
	t.Parallel()
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	first := NewServer(sockPath)
	first.Handle(RequestPing, func(_ context.Context, _ *Request) (any, error) {
		return "pong", nil
	})
	require.NoError(t, first.Start())
	defer first.Close()

	second := NewServer(sockPath)
	err := second.Start()
	require.Error(t, err, "a live socket must not be taken over")
	require.ErrorIs(t, err, ErrSocketInUse, "callers fall back on this specific error")

	// The incumbent is still serving: the whole point of refusing.
	require.NoError(t, NewClient(sockPath).Ping(), "the first server still answers")
}

// TestSocketPathSurvivesRestart is the end-to-end shape of the bug this fixes:
// a session holds one address for its whole life, and a bramble that comes back
// at the same path must be reachable at it.
func TestSocketPathSurvivesRestart(t *testing.T) {
	t.Parallel()
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	first := NewServer(sockPath)
	first.Handle(RequestPing, func(_ context.Context, _ *Request) (any, error) {
		return "pong", nil
	})
	require.NoError(t, first.Start())
	require.NoError(t, first.Close())

	// What a session baked into its environment before the restart.
	client := NewClient(sockPath)

	second := NewServer(sockPath)
	second.Handle(RequestPing, func(_ context.Context, _ *Request) (any, error) {
		return "pong", nil
	})
	require.NoError(t, second.Start())
	defer second.Close()

	require.NoError(t, client.Ping(), "the address a live session holds still works")
}

// TestConcurrentStartNeverUnlinksALiveSocket pins the invariant the stable
// socket path depends on: exactly one server binds, and the loser must never
// remove the winner's socket.
//
// Checking liveness and then unlinking is not atomic — both processes can
// observe the path as free, one binds, and the other then removes the live
// socket out from under it. Every session holding that path in its frozen tmux
// environment is stranded at that moment, which is the failure the stable path
// was introduced to fix.
func TestConcurrentStartNeverUnlinksALiveSocket(t *testing.T) {
	t.Parallel()
	sockPath := filepath.Join(t.TempDir(), "contended.sock")

	const racers = 8
	start := make(chan struct{})
	results := make(chan error, racers)
	servers := make([]*Server, racers)

	for i := range servers {
		srv := NewServer(sockPath)
		srv.Handle(RequestPing, func(_ context.Context, _ *Request) (any, error) {
			return "pong", nil
		})
		servers[i] = srv
		go func() {
			<-start // release them together so the binds actually contend
			results <- srv.Start()
		}()
	}
	close(start)

	var won int
	for i := 0; i < racers; i++ {
		err := <-results
		if err == nil {
			won++
			continue
		}
		require.ErrorIs(t, err, ErrSocketInUse,
			"a loser must report the path as taken, not fail some other way")
	}
	require.Equal(t, 1, won, "exactly one server may hold the socket")

	for _, srv := range servers {
		defer srv.Close()
	}

	// The decisive assertion: the winner is still reachable. If any loser had
	// unlinked and rebound, this either fails or reaches a different listener.
	require.NoError(t, NewClient(sockPath).Ping(),
		"the live socket must still be served after the contention")
}

// TestStaleSocketFileIsReclaimed is the other half: a socket file left behind by
// a killed process has no listener, so it must be removed and rebound rather
// than treated as live. Without this a crash would make the stable path
// permanently unusable.
func TestStaleSocketFileIsReclaimed(t *testing.T) {
	t.Parallel()
	sockPath := filepath.Join(t.TempDir(), "stale.sock")

	// Leave a bound socket file with no listener behind it, the way a killed
	// process does. Go's UnixListener unlinks on Close, so a normal
	// Listen/Close cannot reproduce it — SetUnlinkOnClose(false) is what makes
	// the file outlive its listener.
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, ln.Close())
	_, err = os.Stat(sockPath)
	require.NoError(t, err, "precondition: a socket file is left on disk")

	srv := NewServer(sockPath)
	srv.Handle(RequestPing, func(_ context.Context, _ *Request) (any, error) {
		return "pong", nil
	})
	require.NoError(t, srv.Start(), "a stale socket file must be reclaimed")
	defer srv.Close()

	require.NoError(t, NewClient(sockPath).Ping())
}
