package prdozer

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/bazelment/yoloswe/fleet"
)

// LeaseDir holds one lock file per babysat PR, on the box actually running it.
//
// Leases deliberately do NOT go through the git-synced memory directory:
// ~/magent/claude-shared-memory/.gitattributes sets `MEMORY.md merge=union`,
// and memory-sync runs on a 5-minute per-box stagger. Union merge silently
// accepts BOTH of two competing claims, which is precisely wrong for mutual
// exclusion — and devbox-register.sh states the principle outright, that the
// registry records provisioning facts and "never live state ... which would be
// stale the moment it is committed". A flock on the target box is atomic and
// self-enforcing.
const LeaseDir = "~/.prdozer/leases"

// LeasePath returns the lock file for a repo/PR pair.
func LeasePath(ownerRepo string, prNumber int) string {
	return filepath.Join(ExpandHome(LeaseDir), fmt.Sprintf("%s-%d.lock", sanitizeSlug(ownerRepo), prNumber))
}

// Lease is a held flock. It must stay held for the worker's whole lifetime.
type Lease struct {
	f    *os.File
	path string
}

// AcquireLease takes an exclusive non-blocking flock for a repo/PR pair.
//
// The lock is held INSIDE the worker process rather than by wrapping the
// dispatch command in flock(1). This was verified experimentally: wrapping
// `tmux new-session -d` in `flock -n` does NOT keep the lock held, because
// tmux daemonizes and the flock(1) process exits immediately — the lock was
// re-acquirable while the worker was still running, which would let two
// babysitters work the same PR.
//
// Returns ErrLeaseHeld when another process already owns it.
func AcquireLease(ownerRepo string, prNumber int) (*Lease, error) {
	path := LeasePath(ownerRepo, prNumber)
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

// ErrLeaseHeld reports that another babysitter already owns this PR's lease.
var ErrLeaseHeld = errLeaseHeld{}

type errLeaseHeld struct{}

func (errLeaseHeld) Error() string { return "babysit lease already held" }

// Release drops the lock. The file is left behind deliberately: its presence
// is harmless, and removing it races with another process that may have just
// opened it and be about to flock.
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
func (l *Lease) Path() string { return l.path }

// TmuxSessionName is the attachable session the worker runs under. It follows
// the observed fleet convention so the window is recognisable in a session list.
func TmuxSessionName(ownerRepo string, prNumber int) string {
	repo := ownerRepo
	if _, after, ok := strings.Cut(ownerRepo, "/"); ok {
		repo = after
	}
	return fmt.Sprintf("babysit/%s#%d", repo, prNumber)
}

// DispatchRequest describes a handoff to a target box.
type DispatchRequest struct {
	OwnerRepo    string
	RegistryPath string
	Host         HostHealth
	PRNumber     int
	KeepWorktree bool
}

// toFleet renders prdozer's babysit-local invocation for the shared dispatcher.
// The quoting, PATH wrapper and deliberate absence of flock(1) all live there —
// see fleet.Request.RemoteCommand for why each is the way it is.
func (r DispatchRequest) toFleet() fleet.Request {
	args := []string{"babysit-local", "--repo", r.OwnerRepo, "--pr", fmt.Sprintf("%d", r.PRNumber)}
	if r.RegistryPath != "" {
		args = append(args, "--registry", r.RegistryPath)
	}
	if r.KeepWorktree {
		args = append(args, "--keep-worktree")
	}
	return fleet.Request{
		Host:        r.Host,
		SessionName: TmuxSessionName(r.OwnerRepo, r.PRNumber),
		Args:        args,
	}
}

// RemoteCommand builds the command run on the target box.
func (r DispatchRequest) RemoteCommand() string { return r.toFleet().RemoteCommand() }

// SSHCommand renders the full ssh invocation, which is what --dry-run prints.
func (r DispatchRequest) SSHCommand() string { return r.toFleet().SSHCommand() }

// Dispatch hands the run off to the target box under tmux.
func Dispatch(ctx context.Context, ssh SSHRunner, req DispatchRequest, logger *slog.Logger) error {
	return fleet.Dispatch(ctx, ssh, Tool, req.toFleet(), logger)
}
