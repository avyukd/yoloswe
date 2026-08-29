package main

import (
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRedirectLogsToFileCapturesStdlibLog is the load-bearing property of the
// redirect: it must capture the standard library's log package, not just slog.
// The delivery courier's warnings — the ones that corrupted the TUI — go
// through log.Printf, so a redirect that only moved slog would fix nothing.
func TestRedirectLogsToFileCapturesStdlibLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// redirectLogsToFile replaces the process-wide default handler, and the
	// file it points at is closed below. Without this, every later test in this
	// package that logs writes to a closed descriptor.
	defer slog.SetDefault(slog.Default())

	path, closeLog, err := redirectLogsToFile()
	if err != nil {
		t.Fatalf("redirectLogsToFile: %v", err)
	}
	defer closeLog()

	if got := filepath.Dir(path); got != filepath.Join(home, ".bramble", "logs") {
		t.Errorf("log path %q is not under ~/.bramble/logs", path)
	}

	slog.Info("a slog record")
	log.Printf("WARNING: a stdlib log record")
	closeLog()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "a slog record") {
		t.Errorf("slog record missing from log file:\n%s", body)
	}
	if !strings.Contains(body, "a stdlib log record") {
		t.Errorf("stdlib log record missing from log file (the slog.SetDefault bridge is what carries it):\n%s", body)
	}
}
