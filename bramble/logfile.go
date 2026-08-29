package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/bazelment/yoloswe/logging/klogfmt"
)

// redirectLogsToFile sends every log record to a file instead of stderr, and
// returns the path it opened.
//
// The TUI owns the terminal. Bramble renders inline rather than in an alternate
// screen, so a single line written to stderr lands in the middle of a painted
// frame and corrupts it until the next full repaint. Diagnostics logged while a
// session is running are exactly the lines most likely to appear, and the
// delivery courier can emit one every retryDelay for as long as a recipient
// refuses its mail.
//
// The writer is the file ALONE, never an io.MultiWriter with stderr, and that
// is what makes this work for more than slog: slog.SetDefault also redirects
// the standard library's log package into the default handler, so the log.Printf
// calls in session and ipc follow the same file without those call sites
// changing. klogfmt.Init and InitWithLogFile cannot be used here for the same
// reason in reverse — both keep stderr in the writer set.
//
// Only the TUI does this. The subcommands under this binary are ordinary CLI
// tools whose operators expect diagnostics on the terminal.
func redirectLogsToFile() (path string, closeLog func(), err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", nil, fmt.Errorf("determine home directory: %w", err)
	}
	logDir := filepath.Join(home, ".bramble", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("create log dir %q: %w", logDir, err)
	}
	// Timestamp and pid: several brambles can run at once, and a shared file
	// would interleave their records.
	path = filepath.Join(logDir, fmt.Sprintf("bramble-%s-%d.log",
		time.Now().Format("20060102-150405"), os.Getpid()))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return "", nil, fmt.Errorf("open log file %q: %w", path, err)
	}

	// Debug: the file has no reason to be terse, and the levels that were
	// suppressed to keep the terminal quiet are the ones worth having here.
	slog.SetDefault(slog.New(klogfmt.New(f, klogfmt.WithLevel(slog.LevelDebug))))

	var closed bool
	return path, func() {
		if closed {
			return
		}
		closed = true
		_ = f.Sync()
		_ = f.Close()
	}, nil
}
