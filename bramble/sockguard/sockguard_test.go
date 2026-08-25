package sockguard

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The cross-process race test re-executes this test binary as its own child.
// The child is selected by raceChildEnv before any test runs, so it does the
// race and exits rather than running the suite.
const (
	raceChildEnv  = "SOCKGUARD_RACE_CHILD"
	raceSockEnv   = "SOCKGUARD_RACE_SOCK"
	raceGateEnv   = "SOCKGUARD_RACE_GATE"
	raceDoneEnv   = "SOCKGUARD_RACE_DONE"
	raceWonMarker = "SOCKGUARD-WON"
)

func TestMain(m *testing.M) {
	if os.Getenv(raceChildEnv) == "" {
		os.Exit(m.Run())
	}
	os.Exit(raceChild())
}

// childrenSettled reports whether every child has recorded a verdict — won or
// lost — so no sibling can still be inside its check-unlink-bind window.
//
// A condition, not a clock. The parent cannot wg.Wait here: the winner is
// itself waiting on the signal this produces, so that would deadlock. Each
// child instead writes a verdict file the moment Listen returns, whichever way
// it went, and the count of those is exactly "the window is closed".
func childrenSettled(gate string, want int) bool {
	matches, err := filepath.Glob(gate + ".settled.*")
	return err == nil && len(matches) >= want
}

// childrenSpinning reports whether every child has announced it is waiting on
// the gate.
func childrenSpinning(gate string, want int) bool {
	matches, err := filepath.Glob(gate + ".arrived.*")
	return err == nil && len(matches) >= want
}

// raceChild contends for one stale socket path and reports whether it won.
//
// It waits on a gate file so every sibling reaches the check-unlink-bind
// sequence at the same moment; without that they arrive in turn and the window
// the lock protects never opens. The winner then holds the socket until every
// sibling has settled, so a second reclaimer would have to unlink a LIVE socket
// to succeed — which is precisely the regression being tested for.
func raceChild() int {
	path := os.Getenv(raceSockEnv)
	gate := os.Getenv(raceGateEnv)
	// Announce arrival, then spin on the gate. The parent waits for every
	// child to arrive before opening it, so all of them enter the window
	// together rather than in spawn order.
	if f, err := os.Create(gate + fmt.Sprintf(".arrived.%d", os.Getpid())); err == nil {
		f.Close()
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(gate); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return 1
		}
	}
	ln, err := Listen(path)
	// Record the verdict before doing anything else with it: the parent
	// releases the winner only once every child has settled, and a child that
	// announced nothing would hold the whole test open.
	if f, ferr := os.Create(gate + fmt.Sprintf(".settled.%d", os.Getpid())); ferr == nil {
		f.Close()
	}
	if err != nil {
		return 0
	}
	fmt.Println(raceWonMarker)
	// Hold the socket until the PARENT says every sibling has finished, rather
	// than for a fixed span. A timed hold makes the test flaky in the one
	// direction that matters: a sibling descheduled past the window finds a
	// stale file again and legitimately wins, so two winners are reported with
	// no bug present. Waiting on the parent removes the race from the test
	// itself — while any sibling can still be racing, the winner is still live,
	// so a second win is only ever the defect.
	deadline = time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(os.Getenv(raceDoneEnv)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	ln.Close()
	return 0
}

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

// TestConcurrentReclaimYieldsExactlyOneWinner pins what the lock file is for,
// across PROCESSES — which is the only place it can be shown, because flock is
// held per open file description and two goroutines in one process do not
// contend for it the way two brambles do.
//
// Binding first makes the kernel arbitrate a LIVE socket, but it cannot order
// the stale path: without the lock two processes can both fail the bind, both
// find nothing listening, and then one unlinks and binds while the other
// unlinks that now-live socket and binds over it. The loser is left serving a
// path that no longer refers to it, while every session holding that address
// talks to the winner.
//
// Each child races for the same stale path and announces a win; exactly one
// may. The window is opened by having every child announce arrival and then
// spin on a shared gate file, so they enter check-unlink-bind together instead
// of in spawn order.
//
// Measured both directions rather than assumed: 15/15 green with the lock in
// place, 2/10 red with the flock call removed. So it is a real detector of the
// regression but a probabilistic one — it will not catch every reintroduction in
// a single run, while it never fails spuriously with the lock present. An
// earlier in-process version caught nothing at all: goroutines share an open
// file description, so they never contend for the flock the way two brambles
// do.
//
// The winner holds the socket until the parent reports every child settled,
// rather than for a fixed span. A timed hold was flaky in the one direction
// that matters: a sibling descheduled past the window finds a stale file again
// and legitimately wins, failing the assertion with no bug present.
func TestConcurrentReclaimYieldsExactlyOneWinner(t *testing.T) {
	t.Parallel()
	if os.Getenv(raceChildEnv) != "" {
		return // this process is a child; see TestMain
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "s.sock")
	require.NoError(t, os.WriteFile(path, nil, 0o600)) // a stale file to race for
	gate := filepath.Join(dir, "gate")
	done := filepath.Join(dir, "done")

	const racers = 6
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		out []string
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run", "TestConcurrentReclaimYieldsExactlyOneWinner")
			cmd.Env = append(os.Environ(), raceChildEnv+"=1", raceSockEnv+"="+path,
				raceGateEnv+"="+gate, raceDoneEnv+"="+done)
			b, _ := cmd.CombinedOutput()
			mu.Lock()
			out = append(out, string(b))
			mu.Unlock()
		}()
	}
	// Release every child at once, so they contend rather than queue. The
	// children are already spinning on the gate by now — they are spawned
	// above and do nothing until it appears — so opening it puts them all into
	// the check-unlink-bind window together, which is the only way the window
	// the lock protects is ever open at all.
	require.Eventually(t, func() bool {
		return childrenSpinning(gate, racers)
	}, 10*time.Second, 20*time.Millisecond, "children must reach the gate before it opens")
	require.NoError(t, os.WriteFile(gate, nil, 0o600))

	// Every loser exits as soon as it loses, so once no child can still be
	// inside its check-unlink-bind window the winner may release the socket.
	// Signalling that explicitly is what keeps a second, legitimate win — and
	// therefore a spurious failure — out of the result.
	go func() {
		defer func() { _ = os.WriteFile(done, nil, 0o600) }()
		// Wait for the window to close, not for a duration: once every child
		// has recorded a verdict, none can still be about to unlink.
		require.Eventually(t, func() bool {
			return childrenSettled(gate, racers)
		}, 30*time.Second, 5*time.Millisecond, "every child must settle")
	}()
	wg.Wait()

	won := 0
	for _, o := range out {
		if strings.Contains(o, raceWonMarker) {
			won++
		}
	}
	assert.Equal(t, 1, won,
		"exactly one process may take a stale path; a second unlinks the winner's live socket (got %d of %d)", won, racers)
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
