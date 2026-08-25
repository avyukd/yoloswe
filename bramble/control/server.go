package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/bazelment/yoloswe/bramble/session"
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
// path, so this server must not unlink it. Mirrors ipc.ErrSocketInUse — the two
// sockets are published together and have to make the same choice, but control
// does not import ipc (ipc would be the odd dependency here, and the check is
// six lines).
var ErrSocketInUse = errors.New("control: socket is already served by a live process")

// socketInUse reports whether something is already listening on path. A live
// listener accepts; a socket file left by a killed process refuses.
func socketInUse(path string) bool {
	conn, err := net.DialTimeout("unix", path, 250*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Start binds the control socket.
//
// A stale socket file is removed and rebound, but one that still answers is
// left alone: the path is stable across restarts, and unlinking a live peer's
// socket would strand every session that has it frozen in its environment.
//
// Bind first, then adjudicate — mirrors ipc.Server.Start, and for the same
// reason: probing liveness and then unlinking leaves a window where two
// processes both see the path free and the second removes the first's live
// socket. Letting the kernel refuse the bind closes that window.
func (s *UnixServer) Start() error {
	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		if !isAddrInUse(err) {
			return fmt.Errorf("control: listen %s: %w", s.socketPath, err)
		}
		if socketInUse(s.socketPath) {
			return fmt.Errorf("%w: %s", ErrSocketInUse, s.socketPath)
		}
		if rmErr := os.Remove(s.socketPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return fmt.Errorf("control: remove stale socket: %w", rmErr)
		}
		ln, err = net.Listen("unix", s.socketPath)
		if err != nil {
			if isAddrInUse(err) {
				return fmt.Errorf("%w: %s", ErrSocketInUse, s.socketPath)
			}
			return fmt.Errorf("control: listen %s: %w", s.socketPath, err)
		}
	}
	s.ln = ln
	s.wg.Add(1)
	go s.acceptLoop()
	return nil
}

// isAddrInUse reports whether a listen failed because the path is already
// taken. Mirrors ipc's helper; control does not import ipc.
func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE) || errors.Is(err, os.ErrExist)
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

// Close stops the server, closes the listener, waits for in-flight connections,
// and removes the socket file.
func (s *UnixServer) Close() error {
	s.cancel()
	var err error
	if s.ln != nil {
		err = s.ln.Close()
	}
	s.wg.Wait()
	os.Remove(s.socketPath)
	return err
}
