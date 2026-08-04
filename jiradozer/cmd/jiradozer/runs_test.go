package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/jiradozer"
)

// seedRun writes a run-log under a redirected HOME and returns its meta.
func seedRun(t *testing.T, meta jiradozer.RunMeta) jiradozer.RunMeta {
	t.Helper()
	rl, err := jiradozer.NewRunLog(meta)
	require.NoError(t, err)
	return rl.Meta()
}

// staleMeta rewrites a run's meta.json with a heartbeat `age` in the past,
// reproducing what a SIGKILLed or rebooted-away run leaves on disk: state
// still "running", heartbeat frozen at the moment it died.
func staleMeta(t *testing.T, meta jiradozer.RunMeta, age time.Duration) {
	t.Helper()
	dir := jiradozer.RunDirFor(meta.Target(), meta.RunID)
	m, err := jiradozer.LoadRunMeta(dir)
	require.NoError(t, err)
	m.HeartbeatAt = time.Now().UTC().Add(-age)
	data, err := json.MarshalIndent(m, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "meta.json"), data, 0o600))
}

func runRunsCmd(t *testing.T, args ...string) string {
	t.Helper()
	cmd := newRunsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	require.NoError(t, cmd.Execute())
	return out.String()
}

func TestRunsJSONIsParseable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedRun(t, jiradozer.RunMeta{RunID: "r1", IssueIdentifier: "INF-1", Repo: "kernel",
		WorktreePath: "/w/INF-1", Branch: "feature/INF-1"})

	var got []jiradozer.RunMeta
	require.NoError(t, json.Unmarshal([]byte(runRunsCmd(t, "--json")), &got))
	require.Len(t, got, 1)
	require.Equal(t, "INF-1", got[0].IssueIdentifier)
	require.Equal(t, "/w/INF-1", got[0].WorktreePath)
	require.Equal(t, jiradozer.RunStateRunning, got[0].State)
	require.False(t, got[0].HeartbeatAt.IsZero(),
		"the dispatcher reads heartbeat_at to judge liveness; it must survive the JSON round trip")
}

// The listing has to surface a dead run, because its State says "running"
// forever — that is exactly what a SIGKILL or a deallocated box leaves behind.
func TestRunsMarksStaleRunning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	live := seedRun(t, jiradozer.RunMeta{RunID: "live", IssueIdentifier: "INF-LIVE", Repo: "kernel"})
	require.Zero(t, live.StaleFor(time.Now().UTC()))

	dead := seedRun(t, jiradozer.RunMeta{RunID: "dead", IssueIdentifier: "INF-DEAD", Repo: "kernel"})
	// Simulate the file a killed process leaves behind. It has to be written
	// directly: UpdateMeta refreshes the heartbeat by design (a deliberate
	// update is itself proof of life), so there is no in-process way to
	// backdate one — which is correct, and means the only faithful
	// reproduction is the stale meta.json on disk.
	staleMeta(t, dead, 2*time.Hour)

	out := runRunsCmd(t)
	require.Contains(t, out, "STALE", "a dead run must not read as healthy")

	var runs []jiradozer.RunMeta
	require.NoError(t, json.Unmarshal([]byte(runRunsCmd(t, "--json")), &runs))
	var deadMeta, liveMeta *jiradozer.RunMeta
	for i := range runs {
		switch runs[i].IssueIdentifier {
		case "INF-DEAD":
			deadMeta = &runs[i]
		case "INF-LIVE":
			liveMeta = &runs[i]
		}
	}
	require.NotNil(t, deadMeta)
	require.NotNil(t, liveMeta)
	// Both report state=running. Only the clock separates them.
	require.Equal(t, jiradozer.RunStateRunning, deadMeta.State)
	require.Equal(t, jiradozer.RunStateRunning, liveMeta.State)
	require.Greater(t, deadMeta.StaleFor(time.Now().UTC()), time.Hour)
	require.Zero(t, liveMeta.StaleFor(time.Now().UTC()))
}

func TestRunsFiltersByIssueRepoAndActive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedRun(t, jiradozer.RunMeta{RunID: "a", IssueIdentifier: "INF-1", Repo: "kernel"})
	seedRun(t, jiradozer.RunMeta{RunID: "b", IssueIdentifier: "INF-2", Repo: "yoloswe"})

	done := seedRun(t, jiradozer.RunMeta{RunID: "c", IssueIdentifier: "INF-3", Repo: "kernel"})
	rl, err := jiradozer.NewRunLog(done)
	require.NoError(t, err)
	require.NoError(t, rl.Finish(jiradozer.RunStateDone, "shipped", nil))

	var got []jiradozer.RunMeta
	require.NoError(t, json.Unmarshal([]byte(runRunsCmd(t, "--json", "--issue", "INF-2")), &got))
	require.Len(t, got, 1)
	require.Equal(t, "INF-2", got[0].IssueIdentifier)

	require.NoError(t, json.Unmarshal([]byte(runRunsCmd(t, "--json", "--repo", "kernel")), &got))
	require.Len(t, got, 2)

	require.NoError(t, json.Unmarshal([]byte(runRunsCmd(t, "--json", "--active")), &got))
	require.Len(t, got, 2, "--active must drop the terminal run")
	for _, r := range got {
		require.False(t, r.State.IsTerminal())
	}
}

// A --description run has no issue identifier, so the task id is the only
// thing correlating it back to the dispatcher's task list.
func TestRunsFiltersByTaskID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedRun(t, jiradozer.RunMeta{RunID: "r1", TaskID: "task-7", Repo: "kernel",
		Description: "tidy the helm chart"})

	var got []jiradozer.RunMeta
	require.NoError(t, json.Unmarshal([]byte(runRunsCmd(t, "--json", "--issue", "task-7")), &got))
	require.Len(t, got, 1)
	require.Equal(t, "task-7", got[0].TaskID)
}

func TestRunsOnEmptyBoxSucceeds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// An empty listing must be valid JSON, not an error and not a bare "null":
	// the dispatcher unmarshals this from every host, including fresh ones.
	var got []jiradozer.RunMeta
	require.NoError(t, json.Unmarshal([]byte(runRunsCmd(t, "--json")), &got))
	require.Empty(t, got)
}

// `dispatch` refuses a duplicate by lease name and tells the operator to look
// the run up by it. That instruction has to work: for a --description run the
// lease name is the ONLY name known before the local tracker assigns one.
func TestRunsFindsARunByTheLeaseNameDispatchPrinted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedRun(t, jiradozer.RunMeta{
		RunID: "r1", Repo: "kernel", Branch: "jiradozer/r1",
		IssueIdentifier: "LOCAL-1", LeaseTarget: "adhoc-deadbeefcafe",
		State: jiradozer.RunStateRunning,
	})
	seedRun(t, jiradozer.RunMeta{
		RunID: "r2", Repo: "kernel", IssueIdentifier: "INF-9", State: jiradozer.RunStateRunning,
	})

	out := runRunsCmd(t, "--issue", "adhoc-deadbeefcafe")
	require.Contains(t, out, "r1")
	require.NotContains(t, out, "r2", "the filter must still exclude other runs")

	// The identifier the tracker later assigned keeps working too.
	require.Contains(t, runRunsCmd(t, "--issue", "LOCAL-1"), "r1")
	require.Contains(t, runRunsCmd(t, "--issue", "INF-9"), "r2")
}
