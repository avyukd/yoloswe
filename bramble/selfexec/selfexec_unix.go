//go:build unix

package selfexec

import "syscall"

func execSupported() error { return nil }

// Exec replaces the current process image with the binary at Path(), keeping
// the PID. It only returns on failure — on success there is no "after".
//
// Preserving the PID is load-bearing for bramble: its IPC and control socket
// paths are derived from os.Getpid(), and those paths are baked into the
// environment of every tmux window it has ever created. An exec restart
// re-binds the same paths; a spawn-and-exit restart would strand them.
//
// Deferred functions do not run across an exec, so callers must have completed
// their cleanup before calling this.
func Exec(argv, env []string) error {
	return syscall.Exec(Path(), argv, env)
}
