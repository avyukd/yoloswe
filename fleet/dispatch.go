package fleet

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
)

// Request describes a handoff to a target box.
//
//nolint:govet // fieldalignment: Host first reads better than packing order.
type Request struct {
	// SessionName is the tmux session the worker runs under, so it stays
	// attachable and recognisable in a session list.
	SessionName string
	// Args are the tool's own subcommand and flags, appended after its resolved
	// absolute path. Values are quoted here; callers pass them raw.
	Args []string
	Host HostHealth
}

// RemoteCommand builds the command run on the target box.
//
// flock(1) is deliberately ABSENT. Wrapping the tmux invocation in `flock -n`
// looks like mutual exclusion but is not: tmux daemonizes, flock(1) exits, and
// the lock drops while the worker keeps running (verified). The worker instead
// takes its lease itself at startup, holding it for its real lifetime.
func (r Request) RemoteCommand() string {
	// Use the path the probe actually resolved. A non-interactive SSH shell
	// does not include ~/bin (verified), so a bare tool name here would produce
	// a tmux session that dies instantly with "command not found" —
	// indistinguishable from a silent no-op.
	inner := []string{r.Host.BinaryPath}
	for _, a := range r.Args {
		inner = append(inner, shellQuote(a))
	}

	// Set PATH by wrapping the command in a shell.
	//
	// An absolute path fixes the tool itself but not the agent: the tool SPAWNS
	// a CLI as a child and resolves it from PATH, and `claude` lives in
	// ~/.local/bin, which a non-interactive SSH shell omits. Without this the
	// worker starts, detects work correctly, then every round dies with
	// `exec: "claude": executable file not found in $PATH` — a failure with
	// nothing to do with the task.
	//
	// `tmux -e PATH=...` does NOT work here and was tried first: tmux treats
	// PATH specially and silently ignores the override, so the session still
	// runs with the tmux server's PATH. (`-e FOO=bar` DOES work, which makes the
	// failure easy to misdiagnose — verify with PATH itself, never a proxy
	// variable.) The shell wrapper costs an extra quoting level but takes effect.
	cmd := strings.Join(inner, " ")
	if pathEnv := r.remotePathEnv(); pathEnv != "" {
		cmd = "export PATH=" + shellQuote(pathEnv) + "; exec " + cmd
	}
	return fmt.Sprintf("tmux new-session -d -s %s %s",
		shellQuote(r.SessionName),
		shellQuote("sh -c "+shellQuote(cmd)),
	)
}

// remotePathEnv builds the PATH the worker needs on the target box.
//
// The home directory differs per box (the Azure devbox runs as "ming", the AWS
// boxes as "ubuntu"), and tmux -e takes a literal value with no shell expansion
// — so $HOME cannot be used. The probe already resolved the binary to an
// absolute path and its parent IS the home bin dir, so the home root is
// derivable without another round trip. Returns "" when it cannot be derived,
// leaving PATH untouched rather than setting a wrong one.
func (r Request) remotePathEnv() string {
	bin := r.Host.BinaryPath
	if !strings.HasPrefix(bin, "/") {
		return ""
	}
	binDir := filepath.Dir(bin) // /home/ming/bin OR /home/ming/.local/bin
	home := filepath.Dir(binDir)
	// Both install shapes are in use: a symlink at ~/bin on boxes that build the
	// binary, and a copied artifact at ~/.local/bin on boxes with no worktree.
	// Walking up one level from the second yields ~/.local, which would produce
	// a PATH containing ~/.local/.local/bin and silently omit the real one.
	if filepath.Base(home) == ".local" {
		home = filepath.Dir(home)
	}
	if home == "/" || home == "." {
		return ""
	}
	localBin := filepath.Join(home, ".local", "bin")
	if binDir == localBin {
		return localBin + ":" + filepath.Join(home, "bin") + ":" + remoteBasePath
	}
	return localBin + ":" + binDir + ":" + remoteBasePath
}

// remoteBasePath is the PATH a non-interactive SSH shell provides (verified on
// both the Azure and AWS boxes). The wrapper replaces PATH wholesale rather
// than prepending, so the base entries have to be carried explicitly.
const remoteBasePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// SSHCommand renders the full ssh invocation, which is what a dry run prints.
func (r Request) SSHCommand() string {
	return fmt.Sprintf("ssh -o BatchMode=yes -o ConnectTimeout=8 -o StrictHostKeyChecking=accept-new %s %s",
		r.Host.Target(), shellQuote(r.RemoteCommand()))
}

// shellQuote single-quotes a value for safe use in a remote shell command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Dispatch hands the run off to the target box under tmux, so the session is
// attachable and survives the SSH connection closing.
func Dispatch(ctx context.Context, ssh SSHRunner, tool Tool, req Request, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	// Fail loudly rather than half-dispatching into a session that dies
	// instantly and looks like a silent no-op.
	if !req.Host.HasBinary {
		return fmt.Errorf("%s is not on %s's PATH", tool.Name, req.Host.Host)
	}
	logger.Info("dispatching run",
		"tool", tool.Name, "host", req.Host.Host,
		"tmux_session", req.SessionName, "args", strings.Join(req.Args, " "))

	out, err := ssh.Run(ctx, req.Host.Target(), req.RemoteCommand())
	if err != nil {
		return fmt.Errorf("dispatch to %s: %w (output: %s)", req.Host.Host, err, strings.TrimSpace(out))
	}
	return nil
}
