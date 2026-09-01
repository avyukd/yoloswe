package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/bazelment/yoloswe/cliapp"
	"github.com/bazelment/yoloswe/logging/klogfmt"
)

// redirectLogsToFile sends every log record to a file instead of stderr, and
// returns the path it opened.
//
// The TUI owns the terminal. Bramble renders inline rather than in an alternate
// screen, so a single line written to stderr lands in the middle of a painted
// frame and corrupts it until the next full repaint. Diagnostics logged while a
// session is running are exactly the lines most likely to appear, and the
// delivery path could emit one every retry for as long as a recipient
// refuses its mail.
//
// klogfmt.InitToFile calls slog.SetDefault, which also redirects the standard
// library's log package into the default handler. Bramble's own diagnostics all
// go through slog directly — a record bridged from stdlib log carries PC == 0,
// so klogfmt stamps it "???:0]" at Info however the text is worded — but the
// bridge still catches anything a dependency logs that way.
//
// Only the TUI does this. The subcommands under this binary are ordinary CLI
// tools whose operators expect diagnostics on the terminal.
func redirectLogsToFile() (path string, closeLog func(), err error) {
	// cliapp owns the ~/.<tool>/logs convention every tool here writes to.
	logDir, err := cliapp.LogDir("bramble")
	if err != nil {
		return "", nil, fmt.Errorf("resolve log dir: %w", err)
	}
	// Timestamp and pid: several brambles can run at once, and a shared file
	// would interleave their records.
	path = filepath.Join(logDir, fmt.Sprintf("bramble-%s-%d.log",
		time.Now().Format("20060102-150405"), os.Getpid()))

	// Debug: the file has no reason to be terse, and the levels that were
	// suppressed to keep the terminal quiet are the ones worth having here.
	closer, err := klogfmt.InitToFile(path, klogfmt.WithLevel(slog.LevelDebug))
	if err != nil {
		return "", nil, err
	}
	return path, func() { _ = closer() }, nil
}
