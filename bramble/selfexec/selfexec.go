// Package selfexec resolves the running binary's own path once, at process
// start, and replaces the process image with it.
//
// The "once, at process start" part is the whole point. os.Executable() on
// Linux reads /proc/self/exe, which the kernel renders as "<path> (deleted)"
// after the file is unlinked — and replacing a binary in place (bazel build,
// install -m 755, brew upgrade) does exactly that. A lazy os.Executable() call
// therefore starts returning a path that does not exist as soon as anyone
// rebuilds under a long-running process. Resolving during package
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

// self is resolved during package initialization, which runs before main() and
// so before the running binary can be replaced underneath us.
var self = resolve()

func resolve() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	return trimDeleted(path)
}

// trimDeleted strips the kernel's "(deleted)" marker from a /proc/self/exe
// readlink result. Split out from resolve so it can be tested without
// unlinking the running test binary.
func trimDeleted(path string) string {
	return strings.TrimSuffix(path, deletedSuffix)
}

// Path returns the running binary's path, resolved once at process start. It
// returns "" when the path could not be determined; callers that need a
// command to run should fall back to a bare "bramble" PATH lookup.
func Path() string { return self }

// Verify reports whether Path() currently names something we could exec: a
// regular file with an execute bit. Callers use this to refuse a doomed restart
// before tearing anything down, rather than discovering the problem after the
// TUI has already exited.
func Verify() error {
	if self == "" {
		return fmt.Errorf("executable path is unknown")
	}
	info, err := os.Stat(self)
	if err != nil {
		return fmt.Errorf("stat %s: %w", self, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", self)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not executable", self)
	}
	return nil
}
