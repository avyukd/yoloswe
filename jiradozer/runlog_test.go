package jiradozer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// runsHome redirects RunsRoot at the HOME it expands from, so a test never
// writes into the real ~/.jiradozer/runs.
func runsHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestNewRunLogWritesRunningMetaImmediately(t *testing.T) {
	runsHome(t)

	rl, err := NewRunLog(RunMeta{RunID: "abc123", IssueIdentifier: "INF-1234", Repo: "kernel"})
	require.NoError(t, err)

	// The point of writing at start is that a run which dies before doing
	// anything else is still discoverable on disk.
	raw, err := os.ReadFile(filepath.Join(rl.Dir(), "meta.json"))
	require.NoError(t, err)
	var m RunMeta
	require.NoError(t, json.Unmarshal(raw, &m))

	require.Equal(t, RunStateRunning, m.State)
	require.Equal(t, "INF-1234", m.IssueIdentifier)
	require.False(t, m.StartedAt.IsZero())
	require.False(t, m.HeartbeatAt.IsZero(), "a run with no heartbeat can never be judged stale")
	require.Equal(t, os.Getpid(), m.PID)
	require.NotEmpty(t, m.Host)
	require.Equal(t, rl.Dir(), m.LogDir)
}

// A GitHub-style identifier must not turn into nested directories.
func TestRunDirForSanitizesIdentifier(t *testing.T) {
	runsHome(t)
	dir := RunDirFor("acme/app#42", "abc123")
	require.Equal(t, filepath.Base(dir), "acme-app-42-abc123")
	require.Equal(t, filepath.Dir(dir), ExpandHome(RunsRoot))
}

// The heartbeat is what separates "still going" from "the box went away".
func TestStaleForDistinguishesDeadFromSlow(t *testing.T) {
	now := time.Now().UTC()

	live := RunMeta{State: RunStateRunning, HeartbeatAt: now.Add(-HeartbeatInterval)}
	require.Zero(t, live.StaleFor(now), "one interval is a healthy beat")

	// A single missed beat is tolerated: a slow write or a briefly paused VM
	// must not be reported as a death.
	blip := RunMeta{State: RunStateRunning, HeartbeatAt: now.Add(-2 * HeartbeatInterval)}
	require.Zero(t, blip.StaleFor(now))

	dead := RunMeta{State: RunStateRunning, HeartbeatAt: now.Add(-30 * time.Minute)}
	require.Greater(t, dead.StaleFor(now), 25*time.Minute,
		"a SIGKILLed run keeps state=running forever; only the clock exposes it")

	// A terminal run is not stale, however old — it stopped on purpose.
	finished := RunMeta{State: RunStateDone, HeartbeatAt: now.Add(-72 * time.Hour)}
	require.Zero(t, finished.StaleFor(now))
}

func TestRunLogHeartbeatAdvancesAndFinishFreezesIt(t *testing.T) {
	runsHome(t)

	rl, err := NewRunLog(RunMeta{RunID: "abc123", IssueIdentifier: "INF-1", Repo: "kernel"})
	require.NoError(t, err)
	first := rl.Meta().HeartbeatAt

	time.Sleep(5 * time.Millisecond)
	require.NoError(t, rl.Heartbeat())
	require.True(t, rl.Meta().HeartbeatAt.After(first), "heartbeat must advance")

	require.NoError(t, rl.Finish(RunStateDone, "shipped", nil))
	afterFinish := rl.Meta().HeartbeatAt

	time.Sleep(5 * time.Millisecond)
	require.NoError(t, rl.Heartbeat())
	require.Equal(t, afterFinish, rl.Meta().HeartbeatAt,
		"a terminal run must not keep looking alive")

	m, err := LoadRunMeta(rl.Dir())
	require.NoError(t, err)
	require.Equal(t, RunStateDone, m.State)
	require.Equal(t, "shipped", m.Note)
	require.False(t, m.EndedAt.IsZero())
}

func TestRunLogFinishRecordsError(t *testing.T) {
	runsHome(t)
	rl, err := NewRunLog(RunMeta{RunID: "r1", IssueIdentifier: "INF-2", Repo: "kernel"})
	require.NoError(t, err)

	require.NoError(t, rl.Finish(RunStateFailed, "", ErrIdleTimeout))
	m, err := LoadRunMeta(rl.Dir())
	require.NoError(t, err)
	require.Equal(t, RunStateFailed, m.State)
	require.Contains(t, m.Error, "idle timeout")
}

func TestStartHeartbeatBeatsUntilStopped(t *testing.T) {
	runsHome(t)
	rl, err := NewRunLog(RunMeta{RunID: "r1", IssueIdentifier: "INF-3", Repo: "kernel"})
	require.NoError(t, err)
	before := rl.Meta().HeartbeatAt

	ctx, cancel := context.WithCancel(context.Background())
	stop := rl.StartHeartbeat(ctx, 5*time.Millisecond, func(error) { t.Error("heartbeat failed") })

	require.Eventually(t, func() bool {
		return rl.Meta().HeartbeatAt.After(before)
	}, time.Second, 5*time.Millisecond)

	cancel()
	stop()

	// stop() waits for the goroutine, so nothing can write meta after this —
	// which is what lets a caller Finish() without racing the beat.
	settled := rl.Meta().HeartbeatAt
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, settled, rl.Meta().HeartbeatAt)
}

func TestAppendAndLoadEvents(t *testing.T) {
	runsHome(t)
	rl, err := NewRunLog(RunMeta{RunID: "r1", IssueIdentifier: "INF-4", Repo: "kernel"})
	require.NoError(t, err)

	require.NoError(t, rl.Append("phase", "build", map[string]any{"round": 1}))
	require.NoError(t, rl.Append("phase", "validate", nil))

	events, err := LoadEvents(rl.Dir())
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, "build", events[0].Detail)
	require.EqualValues(t, 1, events[0].Fields["round"])
	require.Equal(t, "validate", events[1].Detail)

	// A live process is appending to this file, so a torn trailing line must
	// not fail the read.
	f, err := os.OpenFile(filepath.Join(rl.Dir(), "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(`{"kind":"phase","det`)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	events, err = LoadEvents(rl.Dir())
	require.NoError(t, err)
	require.Len(t, events, 2, "a partial trailing line is skipped, not fatal")
}

func TestListRunsNewestFirstAndSkipsUnreadable(t *testing.T) {
	runsHome(t)

	older, err := NewRunLog(RunMeta{RunID: "old", IssueIdentifier: "INF-10", Repo: "kernel",
		StartedAt: time.Now().UTC().Add(-time.Hour)})
	require.NoError(t, err)
	require.NotNil(t, older)

	newer, err := NewRunLog(RunMeta{RunID: "new", IssueIdentifier: "INF-11", Repo: "kernel"})
	require.NoError(t, err)
	require.NotNil(t, newer)

	// A run killed before its first write leaves a directory with no meta.json.
	// It must not break the listing.
	require.NoError(t, os.MkdirAll(filepath.Join(ExpandHome(RunsRoot), "INF-99-broken"), 0o755))

	runs, err := ListRuns()
	require.NoError(t, err)
	require.Len(t, runs, 2)
	require.Equal(t, "INF-11", runs[0].IssueIdentifier, "newest first")
	require.Equal(t, "INF-10", runs[1].IssueIdentifier)
}

func TestListRunsOnMissingRootIsEmptyNotError(t *testing.T) {
	runsHome(t)
	runs, err := ListRuns()
	require.NoError(t, err)
	require.Empty(t, runs)
}

// A reader on another host tails meta.json over ssh while the run rewrites it.
// The write must be atomic, so a reader never parses a half-written file.
func TestWriteMetaIsAtomic(t *testing.T) {
	runsHome(t)
	rl, err := NewRunLog(RunMeta{RunID: "r1", IssueIdentifier: "INF-5", Repo: "kernel"})
	require.NoError(t, err)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = rl.UpdateMeta(func(m *RunMeta) { m.Phase = "build" })
		}
	}()

	// Every read must parse. Tolerating failures here would make the whole
	// test vacuous — a torn write is precisely a read that does not parse.
	var reads, tornReads int
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		reads++
		m, err := LoadRunMeta(rl.Dir())
		if err != nil {
			tornReads++
			continue
		}
		require.Equal(t, "INF-5", m.IssueIdentifier)
	}
	close(stop)
	<-done

	require.Greater(t, reads, 50, "too few reads to have raced a writer")
	require.Zero(t, tornReads, "a concurrent reader must never observe a half-written meta.json")

	// No temp files may survive a completed write.
	entries, err := os.ReadDir(rl.Dir())
	require.NoError(t, err)
	for _, e := range entries {
		require.NotContains(t, e.Name(), ".tmp")
	}
}

func TestRunMetaTargetFallsBackToTaskIDThenRunID(t *testing.T) {
	require.Equal(t, "INF-1", RunMeta{IssueIdentifier: "INF-1", TaskID: "t9", RunID: "r"}.Target())
	require.Equal(t, "t9", RunMeta{TaskID: "t9", RunID: "r"}.Target(),
		"a --description run has no identifier; the task id is what correlates it")
	require.Equal(t, "r", RunMeta{RunID: "r"}.Target())
}

// A record with no wt_root still knows where its checkout is. Reading the root
// back off that path is what keeps pre-WTRoot runs reclaimable after WT_ROOT
// moves — the ambient root would send gc looking in the new tree, where those
// worktrees have never been.
func TestEffectiveWTRootRecoversTheRootFromAPreWTRootRecord(t *testing.T) {
	t.Run("recorded root wins", func(t *testing.T) {
		m := RunMeta{
			WTRoot: "/roots/recorded", Repo: "kernel", Branch: "feature/INF-1",
			WorktreePath: "/roots/elsewhere/kernel/feature/INF-1",
		}
		require.Equal(t, "/roots/recorded", m.EffectiveWTRoot())
	})

	t.Run("derived from the worktree path", func(t *testing.T) {
		m := RunMeta{
			Repo: "kernel", Branch: "feature/INF-1",
			WorktreePath: "/roots/old/kernel/feature/INF-1",
		}
		require.Equal(t, "/roots/old", m.EffectiveWTRoot(),
			"a slash-bearing branch must be trimmed whole, not one segment at a time")
	})

	t.Run("unknowable stays empty", func(t *testing.T) {
		require.Empty(t, RunMeta{Repo: "kernel", Branch: "b"}.EffectiveWTRoot(),
			"no path to read")
		require.Empty(t, RunMeta{WorktreePath: "/roots/old/kernel/b"}.EffectiveWTRoot(),
			"no repo or branch to trim with")
		require.Empty(t, RunMeta{
			Repo: "kernel", Branch: "b", WorktreePath: "/somewhere/else/entirely",
		}.EffectiveWTRoot(), "a path that is not our layout tells us nothing")
	})
}
