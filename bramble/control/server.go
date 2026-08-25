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
	// bound records that this server actually acquired the socket, so Close
	// unlinks only a path this process owns. See Close.
	bound bool
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
// Bind first, then adjudicate, and serialize the stale reclaim under a lock —
// mirrors ipc.Server.Start, and for the same reasons. Binding first lets the
// kernel arbitrate a live socket; the lock is what stops two processes from
// both judging the same file stale and the second unlinking the first's
// now-live socket.
func (s *UnixServer) Start() error {
	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		if !isAddrInUse(err) {
			return fmt.Errorf("control: listen %s: %w", s.socketPath, err)
		}
		ln, err = reclaimStaleSocket(s.socketPath)
		if err != nil {
			return err
		}
	}
	s.ln = ln
	s.bound = true
	s.wg.Add(1)
	go s.acceptLoop()
	return nil
}

// reclaimStaleSocket takes the path if nothing is listening on it, holding a
// lock file across the check-unlink-bind sequence. Mirrors ipc's helper; the
// lock file is never removed, because unlinking a lock reintroduces the race it
// exists to prevent.
func reclaimStaleSocket(socketPath string) (net.Listener, error) {
	lock, err := os.OpenFile(socketPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("control: open socket lock %s: %w", socketPath, err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, fmt.Errorf("control: lock socket %s: %w", socketPath, err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	if socketInUse(socketPath) {
		return nil, fmt.Errorf("%w: %s", ErrSocketInUse, socketPath)
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("control: remove stale socket: %w", err)
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		if isAddrInUse(err) {
			return nil, fmt.Errorf("%w: %s", ErrSocketInUse, socketPath)
		}
		return nil, fmt.Errorf("control: listen %s: %w", socketPath, err)
	}
	return ln, nil
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
// Close stops the server and removes the socket file, but only if this server
// bound it. Mirrors ipc.Server.Close: a server whose Start lost to a live peer
// never owned the path, and unlinking it would strand that peer's sessions.
func (s *UnixServer) Close() error {
	s.cancel()
	var err error
	if s.ln != nil {
		err = s.ln.Close()
	}
	s.wg.Wait()
	if s.bound {
		os.Remove(s.socketPath)
	}
	return err
}
