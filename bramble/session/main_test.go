package session

import (
	"fmt"
	"os"
	"testing"
)

// TestMain gives the package a private HOME.
//
// Two reasons, both load-bearing. Bazel's test sandbox defines no HOME at all,
// so anything resolving os.UserHomeDir — the delivery queue, the session store,
// a subagent's result file — fails outright. And when HOME *is* defined, tests
// that write under it would write into the developer's real ~/.bramble and
// leave their fixtures in it.
//
// Set for the whole package rather than per test so tests keep running in
// parallel: t.Setenv forbids t.Parallel.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "bramble-session-test-home-")
	if err != nil {
		panic("create test home: " + err.Error())
	}
	if err := os.Setenv("HOME", home); err != nil {
		panic("set test home: " + err.Error())
	}
	code := m.Run()
	if code == 0 {
		if err := assertDefaultResultDirClean(); err != nil {
			fmt.Fprintf(os.Stderr, "session test pollution: %v\n", err)
			code = 1
		}
	}
	os.RemoveAll(home)
	os.Exit(code)
}

// assertDefaultResultDirClean fails if any test wrote a subagent result file
// into the shared default dir instead of an isolated temp dir.
func assertDefaultResultDirClean() error {
	dir, err := DefaultResultDir()
	if err != nil {
		return fmt.Errorf("resolve default result dir: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read default result dir %s: %w", dir, err)
	}
	if len(entries) == 0 {
		return nil
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return fmt.Errorf("default result dir %s has %d file(s) after tests: %v", dir, len(entries), names)
}
