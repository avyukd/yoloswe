package selfexec

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPathIsResolvedAndStable(t *testing.T) {
	t.Parallel()

	// Resolved at package init, so it must already be populated and must not
	// change between calls — that stability is what lets a rebuild swap the
	// file underneath us without moving the target.
	require.NotEmpty(t, Path())
	assert.Equal(t, Path(), Path())
	assert.True(t, filepath.IsAbs(Path()), "expected an absolute path, got %q", Path())
}

func TestTrimDeletedStripsKernelMarker(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"unlinked binary", "/usr/local/bin/bramble (deleted)", "/usr/local/bin/bramble"},
		{"live binary", "/usr/local/bin/bramble", "/usr/local/bin/bramble"},
		{"marker only in the middle is not a suffix", "/opt/a (deleted)/bramble", "/opt/a (deleted)/bramble"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, trimDeleted(tt.in))
		})
	}
}

func TestVerifyAcceptsTheRunningBinary(t *testing.T) {
	t.Parallel()

	// The test binary itself is a regular executable file, so Verify must pass
	// against it. This is the happy path a restart takes.
	require.NoError(t, Verify())
}

func TestVerifyRejectsUnusableTargets(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	missing := filepath.Join(dir, "gone")
	notExec := filepath.Join(dir, "plain")
	require.NoError(t, os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o600))
	subdir := filepath.Join(dir, "adirectory")
	require.NoError(t, os.Mkdir(subdir, 0o755))

	tests := []struct {
		name      string
		path      string
		errSubstr string
	}{
		{"unknown path", "", "unknown"},
		{"missing file", missing, "stat"},
		{"not executable", notExec, "not executable"},
		{"directory", subdir, "not a regular file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Not parallel: these swap the package-level self path.
			restore := self
			self = tt.path
			t.Cleanup(func() { self = restore })

			err := Verify()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errSubstr)
		})
	}
}
