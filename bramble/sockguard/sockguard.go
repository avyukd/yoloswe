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

// ErrInUse reports that another live process serves the socket path.
var ErrInUse = errors.New("socket is already served by a live process")

// liveness bounds the wait for an existing socket to answer. Connecting at all
// is the signal, so this only has to outlast scheduling noise.
const liveness = 250 * time.Millisecond

// InUse reports whether something is already listening on path. Connecting is
// the test, not pinging; a live Unix listener accepts, while a stale file left by
// a killed process refuses with ECONNREFUSED.
//
// Only ECONNREFUSED and os.ErrNotExist prove the path is free. Every other error
// is unclassified and fails closed as in use, because reclaimStale may unlink on
// this verdict. Failing open steals the stable socket path from sessions that
// already have it frozen in their tmux environment.
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

// Listen binds path, reclaiming it only if the file there is stale. It attempts
// the bind before any unlink, then serializes stale-path reclaim with a lock so
// two starters cannot unlink each other's newly bound socket.
//
// Returns ErrInUse when the path belongs to a live process.
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

// reclaimStale takes the path if nothing is listening on it. The advisory lock
// file is never removed; unlinking it can let contenders lock different inodes
// for the same name.
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
	// InUse returns false only for refused or missing paths; unclassified errors
	// fail closed before this unlink.
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

// isAddrInUse reports whether a Unix socket path already exists.
func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE) || errors.Is(err, os.ErrExist)
}
