package prdozer

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
// the observed fleet convention so the window is recognisable in a session
// list.
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

// RemoteCommand builds the command run on the target box.
//
// flock(1) is deliberately ABSENT here. Wrapping the tmux invocation in
// `flock -n` looks like mutual exclusion but is not: tmux daemonizes, flock(1)
// exits, and the lock drops while the worker keeps running (verified). The
// worker instead takes the lease itself at startup via AcquireLease, holding it
// for its real lifetime.
func (r DispatchRequest) RemoteCommand() string {
	// Use the path the probe actually resolved. A non-interactive SSH shell
	// does not include ~/bin on PATH (verified), so a bare "prdozer" here
	// would produce a tmux session that dies instantly with "command not
	// found" — indistinguishable from a silent no-op.
	bin := r.Host.PrdozerPath
	if bin == "" {
		bin = "prdozer"
	}
	inner := []string{
		bin, "babysit-local",
		"--repo", shellQuote(r.OwnerRepo),
		"--pr", fmt.Sprintf("%d", r.PRNumber),
	}
	if r.RegistryPath != "" {
		inner = append(inner, "--registry", shellQuote(r.RegistryPath))
	}
	if r.KeepWorktree {
		inner = append(inner, "--keep-worktree")
	}
	// Put the user bin dirs on PATH via tmux -e rather than wrapping the
	// command in `sh -c "export PATH=...; exec ..."`. Both work, but the
	// wrapper needs three levels of nested shell quoting, and --dry-run is
	// meant to print something a human can paste and run.
	//
	// This matters because an absolute path fixes prdozer itself but not the
	// agent: prdozer SPAWNS the CLI as a child and resolves it from PATH, and
	// `claude` lives in ~/.local/bin, which a non-interactive SSH shell omits.
	// Without it the worker starts, detects work correctly, then every polish
	// round dies with `exec: "claude": executable file not found in $PATH` —
	// a failure with nothing to do with the PR.
	parts := []string{"tmux", "new-session", "-d"}
	if pathEnv := r.remotePathEnv(); pathEnv != "" {
		parts = append(parts, "-e", shellQuote("PATH="+pathEnv))
	}
	parts = append(parts, "-s",
		shellQuote(TmuxSessionName(r.OwnerRepo, r.PRNumber)),
		shellQuote(strings.Join(inner, " ")),
	)
	return strings.Join(parts, " ")
}

// remotePathEnv builds the PATH the worker needs on the target box.
//
// The home directory differs per box (the Azure devbox runs as "ming", the AWS
// boxes as "ubuntu"), and tmux -e takes a literal value with no shell
// expansion — so $HOME cannot be used. The probe already resolved prdozer to an
// absolute path, and its parent IS the home bin dir, so the home root is
// derivable without another round trip. Returns "" when it cannot be derived,
// leaving PATH untouched rather than setting a wrong one.
func (r DispatchRequest) remotePathEnv() string {
	bin := r.Host.PrdozerPath
	if !strings.HasPrefix(bin, "/") {
		return ""
	}
	binDir := filepath.Dir(bin)  // /home/ming/bin
	home := filepath.Dir(binDir) // /home/ming
	if home == "/" || home == "." {
		return ""
	}
	return filepath.Join(home, ".local", "bin") + ":" + binDir + ":" + remoteBasePath
}

// remoteBasePath is the PATH a non-interactive SSH shell provides (verified on
// both the Azure and AWS boxes). tmux -e replaces PATH wholesale rather than
// prepending, so the base entries have to be carried explicitly.
const remoteBasePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// SSHCommand renders the full ssh invocation, which is what --dry-run prints.
func (r DispatchRequest) SSHCommand() string {
	return fmt.Sprintf("ssh -o BatchMode=yes -o ConnectTimeout=8 -o StrictHostKeyChecking=accept-new %s %s",
		r.Host.Target(), shellQuote(r.RemoteCommand()))
}

// shellQuote single-quotes a value for safe use in a remote shell command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Dispatch hands the run off to the target box under tmux, so the session is
// attachable and survives the SSH connection closing.
func Dispatch(ctx context.Context, ssh SSHRunner, req DispatchRequest, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	// Fail loudly rather than half-dispatching into a session that dies
	// instantly and looks like a silent no-op.
	if !req.Host.HasPrdozer {
		return fmt.Errorf("prdozer is not on %s's PATH", req.Host.Host)
	}
	cmd := req.RemoteCommand()
	logger.Info("dispatching babysit run",
		"host", req.Host.Host, "repo", req.OwnerRepo, "pr", req.PRNumber,
		"tmux_session", TmuxSessionName(req.OwnerRepo, req.PRNumber))

	out, err := ssh.Run(ctx, req.Host.Target(), cmd)
	if err != nil {
		return fmt.Errorf("dispatch to %s: %w (output: %s)", req.Host.Host, err, strings.TrimSpace(out))
	}
	return nil
}
