//go:build !unix

package selfexec

import "fmt"

// Exec is unavailable off unix, where there is no execve to replace the
// process image in place.
func Exec(_, _ []string) error {
	return fmt.Errorf("in-place exec restart is not supported on this platform")
}
