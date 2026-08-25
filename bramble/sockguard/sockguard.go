// Package sockguard binds a Unix domain socket whose path is stable across
// restarts, without ever unlinking one a live process is still serving.
//
// It exists because bramble publishes two such sockets — the IPC socket and the
// control socket — and they are published together, so they must make the same
// decision about a path they find occupied. Keeping the logic in one place is
// what stops a change to the liveness test or the lock from landing on one
// socket and silently not the other.
package sockguard

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

// ErrInUse reports that another live process already serves the socket path, so
// the caller must not unlink it. Callers wrap this in their own sentinel where
// their API already promises one.
var ErrInUse = errors.New("socket is already served by a live process")

// liveness bounds the wait for an existing socket to answer. A live server
// accepts immediately — this only has to outlast scheduling noise, not a slow
// handler, because connecting at all is the whole signal.
const liveness = 250 * time.Millisecond

// InUse reports whether something is already listening on path.
//
// Connecting is the test, not pinging: a Unix socket that accepts a connection
// has a live listener behind it, and a stale file left by a killed process
// refuses with ECONNREFUSED. Sending a request would additionally require the
// peer to speak a particular protocol, which is a stronger claim than needed
// and would hang on a peer that accepts but never replies.
//
// Only two answers mean the path is free, and they are named positively rather
// than by excluding the failures anyone thought of: the peer REFUSED the
// connection, or there is no file to connect to. Every other error is one this
// function could not classify — a dial that timed out against a saturated
// listener backlog, EMFILE from fd exhaustion, EINTR — and an unclassified
// error is not evidence of absence. It is reported as in use.
//
// Failing closed here costs a fallback to a pid-scoped path; failing open costs
// the live peer its address. reclaimStale unlinks on this verdict, so a false
// "free" strands every window whose tmux environment froze the stable path,
// which is exactly the failure this package was extracted to prevent. This
// check runs on every start that finds a file present — net.Listen fails with
// EADDRINUSE whether or not a peer is alive — so it is not a rare path.
func InUse(path string) bool {
	conn, err := net.DialTimeout("unix", path, liveness)
	if err == nil {
		conn.Close()
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
		return false
	}
	return true
}

// Listen binds path, reclaiming it only if the file there is stale.
//
// The bind is attempted BEFORE any unlink, so the common case never touches a
// file it does not own, and the reclaim of a stale file is serialized by a lock
// file. Both are needed. Binding first makes the kernel the arbiter for a live
// socket, but it cannot order the stale path: two processes can both fail the
// bind, both find nothing listening, and then one unlinks and binds while the
// other unlinks that now-live socket and binds over it — stranding the first,
// which keeps serving a path that no longer refers to it.
//
// Returns ErrInUse when the path belongs to a live process. Unlinking it
// blindly would steal every running session's callback address from whichever
// process is still serving them, and those sessions have the path frozen in
// their tmux window environment with no way to learn a new one.
func Listen(path string) (net.Listener, error) {
	ln, err := net.Listen("unix", path)
	if err == nil {
		return ln, nil
	}
	if !isAddrInUse(err) {
		return nil, fmt.Errorf("failed to listen on %s: %w", path, err)
	}
	return reclaimStale(path)
}

// reclaimStale takes the path if nothing is listening on it, holding a lock
// file across the whole check-unlink-bind sequence so two processes cannot both
// decide the same file is stale.
//
// The lock is advisory (flock) and its file is never removed: unlinking a lock
// is what reintroduces the race it prevents, since two processes can then hold
// locks on different inodes for the same name. One empty file per socket path
// is a small price.
func reclaimStale(path string) (net.Listener, error) {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("failed to open socket lock for %s: %w", path, err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, fmt.Errorf("failed to lock socket %s: %w", path, err)
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	// Re-test under the lock. Another process may have reclaimed and bound the
	// path while this one waited, in which case it is live and must be left be.
	if InUse(path) {
		return nil, fmt.Errorf("%w: %s", ErrInUse, path)
	}
	// The path refused the connection (or vanished) and no other reclaimer can
	// be running, so the file is stale. InUse answers false only for those two,
	// so an error it could not classify has already returned ErrInUse above
	// rather than reaching this unlink.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("failed to remove stale socket %s: %w", path, err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		if isAddrInUse(err) {
			return nil, fmt.Errorf("%w: %s", ErrInUse, path)
		}
		return nil, fmt.Errorf("failed to listen on %s: %w", path, err)
	}
	return ln, nil
}

// isAddrInUse reports whether a listen failed because the path is already
// taken, which for a Unix socket is any existing file at that path.
func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE) || errors.Is(err, os.ErrExist)
}
