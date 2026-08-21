//go:build unix

package main

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// watchRestartSignal calls handle on every SIGUSR2 until the returned stop func
// runs. SIGUSR2 is the scriptable way to ask a running TUI to swap itself for
// the binary now on disk, for callers that cannot reach the IPC socket.
func watchRestartSignal(handle func()) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR2)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
				slog.Info("received SIGUSR2, requesting in-place restart")
				handle()
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}
