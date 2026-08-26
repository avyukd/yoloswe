package sockguard

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConcurrentReclaimYieldsExactlyOneWinner re-executes this test binary as
// children selected by raceChildEnv.
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

// childrenSettled reports whether every child has recorded a verdict AND none
// is still inside Listen, so no sibling can be in its check-unlink-bind window.
// The parent cannot wg.Wait here because the winner is waiting on this signal.
//
// The second condition is not redundant, and omitting it was a flake worth a
// CI failure. A .settled marker is written after Listen returns, so a child
// blocked on reclaimStale's flock has written nothing and does not hold the
// count back. The parent released the winner, the winner closed — and closing a
// Go unix listener UNLINKS its path — so the straggler emerged, found a stale
// file, and won legitimately. Two winners, with the production lock never
// violated: the harness had manufactured a second race after declaring the
// first one over. It took a loaded machine to show (1/10 under CPU contention,
// 0/80 idle), which is why CI saw it first.
func childrenSettled(gate string, want int) bool {
	settled, err := filepath.Glob(gate + ".settled.*")
	if err != nil || len(settled) < want {
		return false
	}
	inflight, err := filepath.Glob(gate + ".inflight.*")
	return err == nil && len(inflight) == 0
}

func childrenSpinning(gate string, want int) bool {
	matches, err := filepath.Glob(gate + ".arrived.*")
	return err == nil && len(matches) >= want
}

// raceChild waits on a gate so sibling processes enter check-unlink-bind
// together. The winner stays live until every sibling has settled, so a second
// win means another process unlinked a live socket.
func raceChild() int {
	path := os.Getenv(raceSockEnv)
	gate := os.Getenv(raceGateEnv)
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
	// Bracket Listen with an in-flight marker. .settled alone cannot express
	// "still racing": it is written only after Listen RETURNS, so a child
	// blocked on reclaimStale's flock has recorded nothing and is invisible to
	// the parent — which then released the winner while that child was still
	// mid-reclaim. See childrenSettled.
	inflight := gate + fmt.Sprintf(".inflight.%d", os.Getpid())
	if f, ferr := os.Create(inflight); ferr == nil {
		f.Close()
	}
	ln, err := Listen(path)
	_ = os.Remove(inflight)
	if f, ferr := os.Create(gate + fmt.Sprintf(".settled.%d", os.Getpid())); ferr == nil {
		f.Close()
	}
	if err != nil {
		return 0
	}
	fmt.Println(raceWonMarker)
	// A timed hold lets a delayed sibling find a stale file again and win
	// legitimately; waiting for the parent keeps the winner's socket live
	// while any sibling can still be racing.
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

// TestListenReclaimsAStaleSocketFile pins crash recovery: a socket file left
// behind by a killed process has no listener, so it is safe to unlink and bind.
func TestListenReclaimsAStaleSocketFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "s.sock")

	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	require.NoError(t, ln.Close())
	// Closing a Go unix listener unlinks it, so put the stale file back.
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	got, err := Listen(path)
	require.NoError(t, err, "a file no process is serving must be reclaimed")
	t.Cleanup(func() { got.Close() })
}

// TestListenRefusesALiveSocket pins the stable callback address: unlinking a
// live socket would steal it from sessions that already have the path frozen in
// their tmux window environment.
func TestListenRefusesALiveSocket(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "s.sock")

	live, err := Listen(path)
	require.NoError(t, err)
	t.Cleanup(func() { live.Close() })

	_, err = Listen(path)
	require.ErrorIs(t, err, ErrInUse, "a live peer's socket is never unlinked")
}

// TestConcurrentReclaimYieldsExactlyOneWinner pins what the lock file is for
// across processes. Goroutines in one process share an open file description,
// so they do not contend for flock the way two brambles do.
//
// Binding first lets the kernel arbitrate a live socket, but it cannot order a
// stale path. Without the lock, two processes can both fail the bind, both see
// no listener, then one can unlink and bind while the other unlinks that now
// live socket and binds over it.
//
// The loser is left serving a path that no longer refers to it, while sessions
// holding that address talk to the winner.
func TestConcurrentReclaimYieldsExactlyOneWinner(t *testing.T) {
	t.Parallel()
	if os.Getenv(raceChildEnv) != "" {
		return // this process is a child; see TestMain
	}
	for round := 0; round < raceRounds; round++ {
		t.Run(fmt.Sprintf("round-%d", round), func(t *testing.T) {
			raceOnce(t)
		})
	}
}

// raceRounds repeats the race enough for the missing-lock detector to fire
// reliably without adding a production test hook between InUse and unlink.
// Local mutation runs at this count kept the locked path green while making the
// missing-lock variant fail often enough to be useful.
const raceRounds = 12

func raceOnce(t *testing.T) {
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
	// Wait for every child to spin on the gate; otherwise they queue and the
	// check-unlink-bind window never opens.
	require.Eventually(t, func() bool {
		return childrenSpinning(gate, racers)
	}, 10*time.Second, 20*time.Millisecond, "children must reach the gate before it opens")
	require.NoError(t, os.WriteFile(gate, nil, 0o600))

	// Release the winner only after every child has recorded a verdict; until
	// then a delayed loser could still unlink the live socket under test.
	go func() {
		defer func() { _ = os.WriteFile(done, nil, 0o600) }()
		// require.Eventually cannot fail this helper goroutine correctly; the
		// win-count assertion below reports the timeout result.
		deadline := time.Now().Add(30 * time.Second)
		for !childrenSettled(gate, racers) && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
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

// TestInUseDistinguishesLiveFromStale pins connect-only liveness: requiring a
// protocol reply would hang on a peer that accepts but never answers.
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

// TestInUseFailsClosedOnAnUnclassifiableError pins fail-closed behavior:
// anything besides refusal or absence must read as in use because reclaimStale
// unlinks paths reported as free.
//
// A path longer than sun_path injects EINVAL, exercising the same branch as any
// real ambiguous dial failure.
// That includes failures such as timeouts against a saturated backlog, EMFILE,
// or EINTR, where treating the path as free would steal a live process address.
func TestInUseFailsClosedOnAnUnclassifiableError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// sun_path is 108 bytes on Linux; anything past it cannot be dialed at all.
	tooLong := filepath.Join(dir, strings.Repeat("n", 200)+".sock")
	_, err := net.DialTimeout("unix", tooLong, 250*time.Millisecond)
	require.Error(t, err, "the injected path must fail the dial for this test to mean anything")
	require.False(t, errors.Is(err, syscall.ECONNREFUSED), "must not be a refusal")
	require.False(t, errors.Is(err, os.ErrNotExist), "must not be an absent path")

	assert.True(t, InUse(tooLong), "an error InUse cannot classify must read as in use, never as free")
}
