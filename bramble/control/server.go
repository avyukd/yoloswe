package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/bramble/sockguard"
)

// SockEnvVar is the environment variable carrying the control socket path,
// discovered by CLI subcommands (parallel to ipc.SockEnvVar for the legacy IPC).
// The literal lives in package session because session injects it into tmux
// windows and cannot import control (control imports session).
const SockEnvVar = session.ControlSockEnvVar

// UnixServer listens on a Unix domain socket and serves the control protocol
// against a Dispatcher. It is the local transport for bramble's own CLI
// subcommands (send-input, send-key, etc.); the remote transport (hub
// WebSocket) reuses the same Dispatcher and Serve loop.
type UnixServer struct {
	disp       *Dispatcher
	ln         net.Listener
	ctx        context.Context
	cancel     context.CancelFunc
	socketPath string
	wg         sync.WaitGroup
}

// NewUnixServer creates a control server bound to socketPath (not yet started).
func NewUnixServer(socketPath string, disp *Dispatcher) *UnixServer {
	ctx, cancel := context.WithCancel(context.Background())
	return &UnixServer{socketPath: socketPath, disp: disp, ctx: ctx, cancel: cancel}
}

// SocketPath returns the listening socket path.
func (s *UnixServer) SocketPath() string { return s.socketPath }

// ErrSocketInUse reports that another live process already serves the socket
// path, so this server must not unlink it.
//
// Wraps sockguard.ErrInUse, which is where the bind-before-unlink logic now
// lives: this socket and the IPC socket are published together and must make
// the same decision about an occupied path, and the two hand-kept copies had
// already drifted. This package keeps its own sentinel because callers —
// main.go among them — already match on it.
var ErrSocketInUse = fmt.Errorf("control: %w", sockguard.ErrInUse)

// Start binds the control socket.
//
// A stale socket file is removed and rebound, but one that still answers is
// left alone: the path is stable across restarts, and unlinking a live peer's
// socket would strand every session that has it frozen in its environment.
//
// Bind first, then adjudicate, and serialize the stale reclaim under a lock.
// That whole sequence lives in sockguard, which the IPC socket uses too, so the
// two paths published together cannot disagree about an occupied path.
func (s *UnixServer) Start() error {
	ln, err := sockguard.Listen(s.socketPath)
	if err != nil {
		if errors.Is(err, sockguard.ErrInUse) {
			return fmt.Errorf("%w: %s", ErrSocketInUse, s.socketPath)
		}
		return fmt.Errorf("control: %w", err)
	}
	s.ln = ln
	s.wg.Add(1)
	go s.acceptLoop()
	return nil
}

func (s *UnixServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			c := NewJSONConn(conn)
			_ = Serve(s.ctx, c, s.disp)
		}()
	}
}

// Close stops the server and removes the socket file, but only if this server
// bound it. Mirrors ipc.Server.Close: a server whose Start lost to a live peer
// never owned the path, and unlinking it would strand that peer's sessions.
func (s *UnixServer) Close() error {
	s.cancel()
	var err error
	bound := s.ln != nil
	if bound {
		err = s.ln.Close()
	}
	s.wg.Wait()
	// Only a server that bound the path may unlink it; ln is assigned on nothing
	// but a successful bind.
	if bound {
		os.Remove(s.socketPath)
	}
	return err
}
