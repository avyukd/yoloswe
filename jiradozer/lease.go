package jiradozer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LeaseDir holds one lock file per in-flight task, on the box actually running
// it.
//
// Leases deliberately do NOT go through any git-synced directory. A synced
// registry records provisioning facts, never live state — it would be stale the
// moment it was committed, and a union-merged file silently accepts BOTH of two
// competing claims, which is precisely wrong for mutual exclusion. A flock on
// the target box is atomic and self-enforcing.
//
// Note the scope: a lease excludes two workers ON ONE BOX. Cross-host exclusion
// is the tracker's job (see LockLabel) because there is no shared filesystem to
// lock against.
const LeaseDir = "~/.jiradozer/leases"

// LeasePath returns the lock file for a task target.
func LeasePath(target string) string {
	return filepath.Join(ExpandHome(LeaseDir), sanitizeForRunDir(target)+".lock")
}

// Lease is a held flock. It must stay held for the worker's whole lifetime.
type Lease struct {
	f    *os.File
	path string
}

// ErrLeaseHeld reports that another worker on this box already owns the target.
var ErrLeaseHeld = errors.New("task lease already held")

// AcquireLease takes an exclusive non-blocking flock for a task target.
//
// The lock is held INSIDE the worker process rather than by wrapping the
// dispatch command in flock(1). Wrapping `tmux new-session -d` in `flock -n`
// looks like mutual exclusion but is not: tmux daemonizes, flock(1) exits, and
// the lock drops while the worker keeps running — so two workers could take the
// same task. Holding it here also means the kernel releases it on any death,
// including SIGKILL, which no cleanup path could guarantee.
func AcquireLease(target string) (*Lease, error) {
	path := LeasePath(target)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create lease dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lease %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("%w: %s", ErrLeaseHeld, path)
	}
	// Record who holds it, purely for humans reading the file.
	_ = f.Truncate(0)
	if _, err := f.WriteAt([]byte(fmt.Sprintf("pid=%d\n", os.Getpid())), 0); err != nil {
		// A cosmetic failure must not defeat a lock we successfully hold.
		_ = err
	}
	return &Lease{f: f, path: path}, nil
}

// Release drops the lock. The file is left behind deliberately: its presence is
// harmless, and removing it races a process that may have just opened it and be
// about to flock.
//
// The consequence for readers: count HELD locks with `flock -n`, never lock
// files. A file count is a count of tasks this box has ever run.
func (l *Lease) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	closeErr := l.f.Close()
	l.f = nil
	if err != nil {
		return err
	}
	return closeErr
}

// Path returns the lease file's path.
func (l *Lease) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}
