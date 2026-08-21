//go:build !unix

package main

import "log/slog"

// watchRestartSignal is a no-op off unix, which has no SIGUSR2 — and no execve
// for the restart it would trigger.
func watchRestartSignal(_ func()) func() {
	slog.Warn("SIGUSR2 restart control is not supported on this platform")
	return func() {}
}
