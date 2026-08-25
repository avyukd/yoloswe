package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

// Handler processes an IPC request and returns a result or error.
type Handler func(ctx context.Context, req *Request) (any, error)

// Server listens on a Unix domain socket and dispatches JSON requests to handlers.
type Server struct {
	listener   net.Listener
	handlers   map[RequestType]Handler
	ctx        context.Context
	cancel     context.CancelFunc
	socketPath string
	wg         sync.WaitGroup
}

// NewServer creates a new IPC server but does not start listening.
func NewServer(socketPath string) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		socketPath: socketPath,
		handlers:   make(map[RequestType]Handler),
		ctx:        ctx,
		cancel:     cancel,
	}
}

// Handle registers a handler for a request type.
func (s *Server) Handle(reqType RequestType, handler Handler) {
	s.handlers[reqType] = handler
}

// SocketPath returns the path to the Unix domain socket.
func (s *Server) SocketPath() string {
	return s.socketPath
}

// ErrSocketInUse reports that another live process is already serving the
// socket path. Callers that can fall back to a different path check for this;
// everything else treats it as fatal.
var ErrSocketInUse = errors.New("socket is already served by a live process")

// socketLiveness bounds the wait for an existing socket to answer. A live
// server accepts immediately — this only has to outlast scheduling noise, not
// a slow handler, because connecting at all is the whole signal.
const socketLiveness = 250 * time.Millisecond

// socketInUse reports whether something is already listening on path.
//
// Connecting is the test, not pinging: a Unix socket that accepts a connection
// has a live listener behind it, and a stale file left by a killed process
// refuses with ECONNREFUSED. Sending a request would additionally require the
// peer to be a bramble that speaks this protocol, which is a stronger claim
// than needed and would hang on a peer that accepts but never replies.
func socketInUse(path string) bool {
	conn, err := net.DialTimeout("unix", path, socketLiveness)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Start begins listening on the Unix domain socket.
//
// A socket file left behind by a dead process is removed and rebound; one that
// still answers belongs to a live server and is left strictly alone. The
// distinction matters because the path is stable across restarts: unlinking it
// blindly would steal every running session's callback address from whichever
// bramble is still serving them, and those sessions have that path frozen in
// their tmux window environment with no way to learn a new one.
//
// The bind is attempted BEFORE any unlink, so the check and the claim cannot
// race. A liveness probe followed by os.Remove has a window in which two
// brambles both see the path free, one binds, and the other then unlinks the
// live socket out from under it — stranding exactly the sessions this stability
// was added to protect. Binding first makes the kernel the arbiter: only one
// listener can hold a path, and EADDRINUSE means somebody already does.
func (s *Server) Start() error {
	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		if !isAddrInUse(err) {
			return fmt.Errorf("failed to listen on %s: %w", s.socketPath, err)
		}
		// A file is in the way. Only its owner can say whether it is live.
		if socketInUse(s.socketPath) {
			return fmt.Errorf("%w: %s", ErrSocketInUse, s.socketPath)
		}
		// Nothing answered, so the file is stale: reclaim it and retry once.
		if rmErr := os.Remove(s.socketPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return fmt.Errorf("failed to remove stale socket %s: %w", s.socketPath, rmErr)
		}
		ln, err = net.Listen("unix", s.socketPath)
		if err != nil {
			// Lost the retry to another process that bound between the unlink
			// and here. It is live by construction, so do not touch it.
			if isAddrInUse(err) {
				return fmt.Errorf("%w: %s", ErrSocketInUse, s.socketPath)
			}
			return fmt.Errorf("failed to listen on %s: %w", s.socketPath, err)
		}
	}
	s.listener = ln

	s.wg.Add(1)
	go s.acceptLoop()

	return nil
}

// isAddrInUse reports whether a listen failed because the path is already
// taken, which for a Unix socket is any existing file at that path.
func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE) || errors.Is(err, os.ErrExist)
}

// Close shuts down the server, closes the listener, waits for in-flight connections, and removes the socket file.
func (s *Server) Close() error {
	s.cancel()
	var err error
	if s.listener != nil {
		err = s.listener.Close()
	}
	s.wg.Wait()
	os.Remove(s.socketPath)
	return err
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if s.ctx.Err() != nil {
				return // shutting down
			}
			log.Printf("ipc: accept error: %v", err)
			continue
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)

	var req Request
	if err := dec.Decode(&req); err != nil {
		resp := Response{ID: req.ID, OK: false, Error: "invalid request: " + err.Error()}
		enc.Encode(resp) //nolint:errcheck
		return
	}

	handler, ok := s.handlers[req.Type]
	if !ok {
		resp := Response{ID: req.ID, OK: false, Error: fmt.Sprintf("unknown request type: %s", req.Type)}
		enc.Encode(resp) //nolint:errcheck
		return
	}

	// Re-decode params into the correct type based on request type
	if err := s.decodeParams(&req); err != nil {
		resp := Response{ID: req.ID, OK: false, Error: "invalid params: " + err.Error()}
		enc.Encode(resp) //nolint:errcheck
		return
	}

	result, err := handler(s.ctx, &req)
	if err != nil {
		resp := Response{ID: req.ID, OK: false, Error: err.Error()}
		enc.Encode(resp) //nolint:errcheck
		return
	}

	resp := Response{ID: req.ID, OK: true, Result: result}
	enc.Encode(resp) //nolint:errcheck
}

// decodeParams re-marshals req.Params (which is a map after initial decode)
// into the correct typed struct.
func (s *Server) decodeParams(req *Request) error {
	if req.Params == nil {
		return nil
	}
	raw, err := json.Marshal(req.Params)
	if err != nil {
		return err
	}
	switch req.Type {
	case RequestNewSession:
		var p NewSessionParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		req.Params = &p
	case RequestNotify:
		var p NotifyParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		req.Params = &p
	case RequestCapturePane:
		var p CapturePaneParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		req.Params = &p
	case RequestRestart:
		var p RestartParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		req.Params = &p
	default:
		// No typed params needed
	}
	return nil
}
