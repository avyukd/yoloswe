package sockguard

import (
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListenReclaimsAStaleSocketFile: a socket file left behind by a killed
// process has no listener, so it is safe to unlink and rebind. This is the case
// that makes a stable socket path usable at all — without it a crash would
// leave bramble unable to bind its own name.
func TestListenReclaimsAStaleSocketFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "s.sock")

	// A socket file with nothing behind it, exactly as a SIGKILL leaves one.
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	require.NoError(t, ln.Close())
	// Closing a Go unix listener unlinks it, so put the stale file back.
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	got, err := Listen(path)
	require.NoError(t, err, "a file no process is serving must be reclaimed")
	t.Cleanup(func() { got.Close() })
}

// TestListenRefusesALiveSocket: the path is stable across restarts, so
// unlinking one a live process still serves would steal every running session's
// callback address — and those sessions have the path frozen in their tmux
// window environment with no way to learn a new one.
func TestListenRefusesALiveSocket(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "s.sock")

	live, err := Listen(path)
	require.NoError(t, err)
	t.Cleanup(func() { live.Close() })

	_, err = Listen(path)
	require.ErrorIs(t, err, ErrInUse, "a live peer's socket is never unlinked")
}

// TestConcurrentReclaimYieldsExactlyOneWinner: however many racers contend for
// one stale path, exactly one may end up serving it. A second winner means the
// first's live socket was unlinked out from under it, leaving it serving a path
// that no longer refers to it while every session holding that address talks to
// the other process.
//
// This asserts the outcome, not the mechanism. It does not by itself prove the
// flock is load-bearing — in-process goroutines do not reliably interleave in
// the check-unlink-bind window, and this still passes with the lock removed.
// The lock is what makes the property hold across PROCESSES, which is the case
// bramble actually has (two brambles started at once) and which no in-process
// test can stage.
func TestConcurrentReclaimYieldsExactlyOneWinner(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "s.sock")
	require.NoError(t, os.WriteFile(path, nil, 0o600)) // a stale file to race for

	const racers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []net.Listener
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ln, err := Listen(path)
			if err != nil {
				return
			}
			mu.Lock()
			winners = append(winners, ln)
			mu.Unlock()
		}()
	}
	wg.Wait()
	for _, ln := range winners {
		t.Cleanup(func() { ln.Close() })
	}
	assert.Len(t, winners, 1,
		"exactly one racer may take a stale path; a second would unlink the winner's live socket")
}

// TestInUseDistinguishesLiveFromStale: connecting IS the test. A live listener
// accepts immediately; a stale file refuses with ECONNREFUSED. Requiring a
// protocol reply instead would hang on a peer that accepts but never answers.
func TestInUseDistinguishesLiveFromStale(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	stale := filepath.Join(dir, "stale.sock")
	require.NoError(t, os.WriteFile(stale, nil, 0o600))
	assert.False(t, InUse(stale), "a file with no listener is stale")

	livePath := filepath.Join(dir, "live.sock")
	live, err := net.Listen("unix", livePath)
	require.NoError(t, err)
	t.Cleanup(func() { live.Close() })
	assert.True(t, InUse(livePath), "a socket that accepts has a live process behind it")

	assert.False(t, InUse(filepath.Join(dir, "absent.sock")), "an absent path is not in use")
}
