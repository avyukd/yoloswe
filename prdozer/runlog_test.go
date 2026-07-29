package prdozer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRunLog(t *testing.T) *RunLog {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	rl, err := NewRunLog(RunMeta{
		Repo:        "sycamore-labs/kernel",
		PRNumber:    8123,
		RunID:       "a3f9",
		Branch:      "feature/x",
		PRURL:       "https://github.com/sycamore-labs/kernel/pull/8123",
		MergePolicy: MergePolicyQueue,
	})
	require.NoError(t, err)
	return rl
}

func TestRunLog_WritesMetaAtStart(t *testing.T) {
	rl := newTestRunLog(t)
	// meta.json exists from the very beginning, so a harvester can find a run
	// that later dies without ever updating it.
	got, err := LoadRunMeta(rl.Dir())
	require.NoError(t, err)
	assert.Equal(t, TerminalRunning, got.State)
	assert.Equal(t, 8123, got.PRNumber)
	assert.Equal(t, "a3f9", got.RunID)
	assert.NotEmpty(t, got.Host, "host must be recorded; runs are keyed by box implicitly")
	assert.False(t, got.StartedAt.IsZero())
}

func TestRunLog_DirIsOutsideAnyWorktree(t *testing.T) {
	rl := newTestRunLog(t)
	// The whole point: logs must not live under the worktree that GC removes.
	assert.Contains(t, rl.Dir(), filepath.Join(".prdozer", "runs"))
	assert.NotContains(t, rl.Dir(), BabysitNamespace)
	// The repo slug's "/" must not have become a path separator.
	assert.Equal(t, "sycamore-labs-kernel-8123-a3f9", filepath.Base(rl.Dir()))
}

func TestRunLog_FinishRecordsTerminalState(t *testing.T) {
	rl := newTestRunLog(t)
	require.NoError(t, rl.Finish(TerminalMerged, "landed via queue"))

	got, err := LoadRunMeta(rl.Dir())
	require.NoError(t, err)
	assert.Equal(t, TerminalMerged, got.State)
	assert.False(t, got.EndedAt.IsZero())
	assert.Equal(t, "landed via queue", got.Note)
}

func TestRunLog_EventsAreAppendOnly(t *testing.T) {
	rl := newTestRunLog(t)
	require.NoError(t, rl.Append("tick", "snapshot taken", map[string]any{"rollup": "FAILURE"}))
	require.NoError(t, rl.Append("polish", "round 1", nil))
	require.NoError(t, rl.Append("merge", "attempt 1", map[string]any{"policy": "queue"}))

	data, err := os.ReadFile(filepath.Join(rl.Dir(), "events.jsonl"))
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.Len(t, lines, 3, "each event is exactly one line; nothing is overwritten")

	var first Event
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &first))
	assert.Equal(t, "tick", first.Kind)
	assert.Equal(t, "FAILURE", first.Fields["rollup"])
	assert.False(t, first.At.IsZero())

	var last Event
	require.NoError(t, json.Unmarshal([]byte(lines[2]), &last))
	assert.Equal(t, "merge", last.Kind, "order is preserved: this is an audit trail")
}

func TestRunLog_WriteRoundLog(t *testing.T) {
	rl := newTestRunLog(t)
	require.NoError(t, rl.WriteRoundLog("polish-r1", "agent output here"))
	data, err := os.ReadFile(filepath.Join(rl.Dir(), "polish-r1.log"))
	require.NoError(t, err)
	assert.Equal(t, "agent output here", string(data))
}

func TestRunLog_UpdateMetaPersists(t *testing.T) {
	rl := newTestRunLog(t)
	require.NoError(t, rl.UpdateMeta(func(m *RunMeta) {
		m.PolishRounds = 3
		m.MergeAttempt = 2
		m.WorktreeKept = true
		m.Note = "kept: unpushed commits"
	}))
	got, err := LoadRunMeta(rl.Dir())
	require.NoError(t, err)
	assert.Equal(t, 3, got.PolishRounds)
	assert.Equal(t, 2, got.MergeAttempt)
	assert.True(t, got.WorktreeKept)
	assert.Equal(t, "kept: unpushed commits", got.Note)
}

func TestListRuns_NewestFirstAndTolerantOfJunk(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	older, err := NewRunLog(RunMeta{
		Repo: "o/r", PRNumber: 1, RunID: "old",
		StartedAt: time.Now().Add(-2 * time.Hour).UTC(),
	})
	require.NoError(t, err)
	newer, err := NewRunLog(RunMeta{
		Repo: "o/r", PRNumber: 2, RunID: "new",
		StartedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	// A run killed before its first write leaves a dir with no meta.json. It
	// must be skipped, not break the whole listing.
	require.NoError(t, os.MkdirAll(filepath.Join(ExpandHome(RunsRoot), "o-r-3-broken"), 0o755))

	runs, err := ListRuns()
	require.NoError(t, err)
	require.Len(t, runs, 2, "the meta-less directory is skipped")
	assert.Equal(t, "new", runs[0].RunID, "newest first")
	assert.Equal(t, "old", runs[1].RunID)
	assert.Equal(t, newer.Meta().RunID, runs[0].RunID)
	assert.Equal(t, older.Meta().RunID, runs[1].RunID)
}

func TestListRuns_MissingRootIsNotAnError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runs, err := ListRuns()
	require.NoError(t, err, "no runs yet is not a failure")
	assert.Empty(t, runs)
}
