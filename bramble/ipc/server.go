package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"

	"github.com/bazelment/yoloswe/bramble/sockguard"
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
//
// Wraps sockguard.ErrInUse: the bind logic is shared with the control socket
// (the two are published together and must make the same decision), while this
// package keeps its own sentinel because callers already match on it.
var ErrSocketInUse = fmt.Errorf("ipc: %w", sockguard.ErrInUse)

// Start begins listening on the Unix domain socket.
//
// A socket file left behind by a dead process is removed and rebound; one that
// still answers belongs to a live server and is left strictly alone. The
// distinction matters because the path is stable across restarts: unlinking it
// blindly would steal every running session's callback address from whichever
// bramble is still serving them, and those sessions have that path frozen in
// their tmux window environment with no way to learn a new one.
//
// The bind is attempted BEFORE any unlink, so the common case never touches a
// file it does not own, and the reclaim of a stale file is serialized by a lock
// file. Both are needed. Binding first makes the kernel the arbiter for a live
// socket, but it cannot order the stale path: two processes can both fail the
// bind, both find nothing listening, and then one unlinks and binds while the
// other unlinks that now-live socket and binds over it — stranding the first,
// which keeps serving a path that no longer refers to it.
func (s *Server) Start() error {
	if err := s.Bind(); err != nil {
		return err
	}
	s.Serve()
	return nil
}

// Bind acquires the socket without accepting anything yet.
//
// Splitting bind from serve is what lets a caller publish the address it
// actually bound BEFORE any request can be handled. That ordering is
// load-bearing here: this server creates sessions, a session snapshots the
// socket paths at creation and keeps them for life, and the path is stable
// across restarts — so a tmux window left by a previous bramble can fire into
// it the instant it binds. A session built in that window before the path was
// published would carry an empty address and stay mute forever, which is the
// failure this split exists to close.
//
// Call Serve once the path has been published. Start does both, for callers
// with nothing to publish.
func (s *Server) Bind() error {
	ln, err := sockguard.Listen(s.socketPath)
	if err != nil {
		if errors.Is(err, sockguard.ErrInUse) {
			return fmt.Errorf("%w: %s", ErrSocketInUse, s.socketPath)
		}
		return err
	}
	s.listener = ln
	return nil
}

// Serve starts accepting on an already-bound listener. No-op if Bind failed.
func (s *Server) Serve() {
	if s.listener == nil {
		return
	}
	s.wg.Add(1)
	go s.acceptLoop()
}

// Close shuts down the server, waits for in-flight connections, and removes the
// socket file — but only if this server is the one that bound it.
//
// The ownership rule Start establishes has to hold here too: a server whose
// Start returned ErrSocketInUse never owned the path, and unlinking it would
// delete a live peer's socket, stranding every window that has that address
// frozen in its environment. `srv := New(path); defer srv.Close()` before a
// Start that may lose is the natural Go shape, so this must be safe by
// construction rather than by caller discipline.
func (s *Server) Close() error {
	s.cancel()
	var err error
	if s.listener != nil {
		// Do not unlink separately. *net.UnixListener unlinks the path inside
		// Close for a listener it created, so this already removes our socket —
		// and it does so while we still hold it. An explicit os.Remove after
		// wg.Wait would run arbitrarily later, once in-flight handlers drain, by
		// which time a successor bramble can have bound the now-free stable path.
		// Removing then deletes the SUCCESSOR's socket file: it keeps serving an
		// unlinked inode while every window's baked BRAMBLE_SOCK resolves to
		// nothing, which is exactly the stranding the stable path prevents.
		// Under the old PID-scoped names no other process could hold this path
		// during Close, so the hazard arrived with the stable name.
		err = s.listener.Close()
	}
	s.wg.Wait()
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
