// Package selfexec resolves the running binary's own path once, at process
// start, and replaces the process image with it.
//
// The "once, at process start" part is the whole point. os.Executable() on
// Linux reads /proc/self/exe, which the kernel renders as "<path> (deleted)"
// after the file is unlinked — and replacing a binary in place (bazel build,
// install -m 755, brew upgrade) does exactly that. Resolving during package
// initialization pins the answer before any such swap can happen.
package selfexec

import (
	"fmt"
	"os"
	"strings"
)

// deletedSuffix is what Linux appends to /proc/self/exe once the underlying
// file has been unlinked. Stripping it recovers the original path, which is
// also the path a replacement binary now occupies.
const deletedSuffix = " (deleted)"

var self = resolve()

func resolve() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	return trimDeleted(path)
}

func trimDeleted(path string) string {
	return strings.TrimSuffix(path, deletedSuffix)
}

// Path returns the running binary's path, resolved once at process start. It
// returns "" when the path could not be determined; callers that need a
// command to run should fall back to a bare "bramble" PATH lookup.
func Path() string { return self }

// Verify reports whether Exec could succeed right now: the platform supports
// replacing the process image, and Path() names a regular file with an execute
// bit. Callers use this to refuse a doomed restart before tearing anything
// down, rather than discovering the problem after the TUI has already exited.
func Verify() error { return verify(self) }

func verify(path string) error {
	if err := execSupported(); err != nil {
		return err
	}
	if path == "" {
		return fmt.Errorf("executable path is unknown")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not executable", path)
	}
	return nil
}
