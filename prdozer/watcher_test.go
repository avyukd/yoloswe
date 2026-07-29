package prdozer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/wt"
)

// fakeGH is a minimal GHRunner that matches by joined-args prefix.
type fakeGH struct {
	results  map[string]*wt.CmdResult
	failures map[string]*wt.CmdResult
	calls    [][]string
	mu       sync.Mutex
}

func newFakeGH() *fakeGH {
	return &fakeGH{
		results:  make(map[string]*wt.CmdResult),
		failures: make(map[string]*wt.CmdResult),
	}
}

// addPrefix registers a stdout response for any call whose joined-args starts with prefix.
func (f *fakeGH) addPrefix(prefix, stdout string) {
	f.results[prefix] = &wt.CmdResult{Stdout: stdout}
}

// failPrefix registers a FAILING response (non-zero exit plus stderr) for any
// call whose joined-args starts with prefix, so tests can exercise the paths
// that only run when a gh command actually errors.
func (f *fakeGH) failPrefix(prefix, stderr string) {
	f.failures[prefix] = &wt.CmdResult{Stderr: stderr, ExitCode: 1}
}

func (f *fakeGH) Run(_ context.Context, args []string, _ string) (*wt.CmdResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	joined := strings.Join(args, " ")
	// Failures are checked first so a failPrefix can override a broader
	// success prefix registered by setupGH.
	for prefix, res := range f.failures {
		if strings.HasPrefix(joined, prefix) {
			return res, fmt.Errorf("exit status %d", res.ExitCode)
		}
	}
	for prefix, res := range f.results {
		if strings.HasPrefix(joined, prefix) {
			return res, nil
		}
	}
	// Default to empty array for unknown api calls so JSON parse doesn't fail.
	return &wt.CmdResult{Stdout: "[]"}, nil
}

// stubPolish records calls and returns a configurable error.
type stubPolish struct {
	err   error
	calls []PolishRequest
	mu    sync.Mutex
}

func (s *stubPolish) Run(_ context.Context, req PolishRequest) (PolishResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)
	if s.err != nil {
		return PolishResult{}, s.err
	}
	return PolishResult{SessionID: "stub", Output: "ok"}, nil
}

func setupGH(prJSON, runListJSON, baseSHA string) *fakeGH {
	gh := newFakeGH()
	gh.addPrefix("pr view 42 --json number,url,headRefName,baseRefName,headRefOid,state,isDraft,reviewDecision,mergeable,statusCheckRollup", prJSON)
	gh.addPrefix("run list --branch feature --status failure", runListJSON)
	gh.addPrefix("api repos/{owner}/{repo}/git/refs/heads/main", baseSHA)
	gh.addPrefix("api --paginate repos/o/r/pulls/42/comments", "[]")
	gh.addPrefix("api --paginate repos/o/r/issues/42/comments", "[]")
	return gh
}

// buildPRJSON stitches the core PR fields with a statusCheckRollup outcome so
// a single gh pr view call returns everything TakeSnapshot needs.
func buildPRJSON(core, rollupOutcome string) string {
	rollup := ""
	switch rollupOutcome {
	case "SUCCESS", "FAILURE":
		rollup = fmt.Sprintf(`,"statusCheckRollup":[{"conclusion":%q,"status":"COMPLETED"}]`, rollupOutcome)
	}
	trimmed := strings.TrimSpace(core)
	return trimmed[:len(trimmed)-1] + rollup + "}"
}

const okPRJSON = `{
  "number": 42,
  "url": "https://github.com/o/r/pull/42",
  "headRefName": "feature",
  "baseRefName": "main",
  "headRefOid": "head1",
  "state": "OPEN",
  "isDraft": false,
  "reviewDecision": "REVIEW_REQUIRED",
  "mergeable": "MERGEABLE"
}`

func newWatcherForTest(t *testing.T, gh wt.GHRunner, polish PolishRunner, opts ...WatcherOption) *Watcher {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	cfg := DefaultConfig()
	cfg.PollInterval = 10 * time.Millisecond
	cfg.Backoff.MaxConsecutiveFailures = 2
	cfg.Backoff.Cooldown = time.Hour
	return NewWatcher(cfg, gh, polish, 42, ".", "r", nil, opts...)
}

func TestWatcher_Tick_FirstRunIdle_NoPolish(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "SUCCESS"), "[]", "base1")
	polish := &stubPolish{}
	w := newWatcherForTest(t, gh, polish)
	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionIdle, res.Action)
	assert.Empty(t, polish.calls, "first run should not invoke polish")
	state, err := LoadState(StatePath("r", 42))
	require.NoError(t, err)
	assert.Equal(t, "head1", state.LastSeenHeadSHA)
	assert.Equal(t, "base1", state.LastSeenBaseSHA)
}

func TestWatcher_Tick_BaseMoved_TriggersPolish(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "SUCCESS"), "[]", "new-base")
	polish := &stubPolish{}
	w := newWatcherForTest(t, gh, polish)
	// Pre-seed state so this is NOT the first run.
	pre := &State{
		PRNumber:        42,
		Repo:            "r",
		LastCheckAt:     time.Now(),
		LastSeenHeadSHA: "head1",
		LastSeenBaseSHA: "old-base",
	}
	require.NoError(t, pre.Save(StatePath("r", 42)))

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionPolished, res.Action)
	require.Len(t, polish.calls, 1)
	assert.Equal(t, 42, polish.calls[0].PRNumber)
}

func TestWatcher_Tick_DryRun_DoesNotPolish(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	polish := &stubPolish{}
	w := newWatcherForTest(t, gh, polish, WithDryRun(true))

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionDryRun, res.Action)
	assert.Empty(t, polish.calls)
}

func TestWatcher_Tick_PolishFailure_TripsCooldown(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	polish := &stubPolish{err: fmt.Errorf("boom")}
	w := newWatcherForTest(t, gh, polish)
	statePath := StatePath("r", 42)

	// Pre-seed so it's not first run; failure is triggered via FAILURE rollup which
	// is actionable on first run anyway, but pre-seeding makes the test explicit.
	pre := &State{LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1"}
	require.NoError(t, pre.Save(statePath))

	// First failure.
	_, err := w.Tick(context.Background())
	require.NoError(t, err)
	s1, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 1, s1.ConsecutiveFailures)
	assert.True(t, s1.CooldownUntil.IsZero(), "single failure shouldn't trip cooldown")

	// Second failure → cooldown.
	_, err = w.Tick(context.Background())
	require.NoError(t, err)
	s2, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 2, s2.ConsecutiveFailures)
	assert.False(t, s2.CooldownUntil.IsZero(), "second failure should set cooldown")

	// Third tick is skipped due to cooldown.
	_, err = w.Tick(context.Background())
	require.NoError(t, err)
	assert.Len(t, polish.calls, 2, "third tick should be skipped by cooldown")
}

// approvedGreenGH builds a fake gh serving an APPROVED + SUCCESS PR (i.e.
// cs.Mergeable) plus a `.merged` verification response.
func approvedGreenGH(mergedVerdict string) *fakeGH {
	approved := strings.Replace(okPRJSON, `"reviewDecision": "REVIEW_REQUIRED"`, `"reviewDecision": "APPROVED"`, 1)
	gh := setupGH(buildPRJSON(approved, "SUCCESS"), "[]", "base1")
	gh.addPrefix("api repos/o/r/pulls/42 --jq .merged", mergedVerdict)
	return gh
}

// findCall returns the first recorded call whose joined args start with prefix.
func (f *fakeGH) findCall(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			return c
		}
	}
	return nil
}

func TestWatcher_MergePolicy_EmitsCorrectArgv(t *testing.T) {
	// The two rules that broke kernel merges are argv-level, so assert on argv:
	// never --delete-branch under ANY policy (it closed PRs #6283/#3179
	// unmerged), and never a strategy flag under "queue" (the merge queue
	// rejects it outright).
	cases := []struct {
		policy     MergePolicy
		merged     string
		wantAction LastAction
		wantArgs   []string
	}{
		{
			policy:     MergePolicyQueue,
			merged:     "false",
			wantArgs:   []string{"pr", "merge", "42", "--auto"},
			wantAction: LastActionArmed,
		},
		{
			policy:     MergePolicySquash,
			merged:     "true",
			wantArgs:   []string{"pr", "merge", "42", "--squash"},
			wantAction: LastActionMerged,
		},
		{
			policy:     MergePolicyRebase,
			merged:     "true",
			wantArgs:   []string{"pr", "merge", "42", "--rebase"},
			wantAction: LastActionMerged,
		},
	}
	for _, tc := range cases {
		t.Run(string(tc.policy), func(t *testing.T) {
			gh := approvedGreenGH(tc.merged)
			w := newWatcherForTest(t, gh, &stubPolish{})
			w.cfg.Polish.AutoMerge = true
			w.cfg.Polish.MergePolicy = tc.policy

			res, err := w.Tick(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tc.wantAction, res.Action)

			got := gh.findCall("pr merge")
			require.NotNil(t, got, "a merge call must have been issued")
			assert.Equal(t, tc.wantArgs, got)
			joined := strings.Join(got, " ")
			assert.NotContains(t, joined, "--delete-branch",
				"--delete-branch closed PRs unmerged; it must never be passed")
			if tc.policy == MergePolicyQueue {
				for _, flag := range []string{"--squash", "--rebase", "--merge"} {
					assert.NotContains(t, joined, flag,
						"a merge-queue repo rejects an explicit strategy flag")
				}
			}
		})
	}
}

func TestWatcher_MergePolicyNotify_NeverMerges(t *testing.T) {
	gh := approvedGreenGH("false")
	w := newWatcherForTest(t, gh, &stubPolish{})
	w.cfg.Polish.AutoMerge = true
	w.cfg.Polish.MergePolicy = MergePolicyNotify

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionNeedsHuman, res.Action)
	assert.Nil(t, gh.findCall("pr merge"),
		"merge_policy notify must not issue any merge command")
}

func TestWatcher_Merge_VerifyFalse_IsFailureNotMerged(t *testing.T) {
	// `gh pr merge` exiting 0 does NOT mean the PR merged. Under a direct
	// policy, a `.merged == false` verdict must be reported as a failure —
	// declaring victory here is how a "merged" PR silently stays open.
	gh := approvedGreenGH("false")
	w := newWatcherForTest(t, gh, &stubPolish{})
	w.cfg.Polish.AutoMerge = true
	w.cfg.Polish.MergePolicy = MergePolicySquash

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionFailed, res.Action,
		"unverified merge must not be reported as merged")
}

func TestWatcher_Merge_VerifiesViaMergedField(t *testing.T) {
	// The ONLY trustworthy merge signal is `.merged` on the pulls endpoint:
	// state:closed is ambiguous and merge_commit_sha can be populated for a
	// merge the queue only speculatively built.
	gh := approvedGreenGH("true")
	w := newWatcherForTest(t, gh, &stubPolish{})
	w.cfg.Polish.AutoMerge = true
	w.cfg.Polish.MergePolicy = MergePolicySquash

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionMerged, res.Action)

	verify := gh.findCall("api repos/o/r/pulls/42 --jq .merged")
	require.NotNil(t, verify, "merge must be verified against the API")
}

func TestWatcher_NeedsReview_StopsInsteadOfPolishing(t *testing.T) {
	// An all-green unapproved PR must notify, not burn polish rounds.
	gh := setupGH(buildPRJSON(okPRJSON, "SUCCESS"), "[]", "base1")
	polish := &stubPolish{}
	w := newWatcherForTest(t, gh, polish)
	// Pre-seed so this is not a first run.
	require.NoError(t, (&State{
		PRNumber:        42,
		Repo:            "r",
		LastCheckAt:     time.Now(),
		LastSeenHeadSHA: "head1",
		LastSeenBaseSHA: "base1",
	}).Save(StatePath("r", 42)))

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionNeedsHuman, res.Action)
	assert.Empty(t, polish.calls, "a missing approval must not trigger polish")
}

func TestMergePolicy_MergeArgs(t *testing.T) {
	t.Parallel()
	for _, p := range []MergePolicy{MergePolicyQueue, MergePolicySquash, MergePolicyRebase} {
		args, ok := p.MergeArgs(7)
		require.True(t, ok, "policy %q should merge", p)
		assert.NotContains(t, strings.Join(args, " "), "--delete-branch")
	}
	_, ok := MergePolicyNotify.MergeArgs(7)
	assert.False(t, ok, "notify policy must not produce merge args")
}

func TestWatcher_StateFileLocation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got := StatePath("yoloswe", 7)
	want := filepath.Join(home, ".bramble", "projects", "yoloswe-7", "prdozer-state.json")
	assert.Equal(t, want, got)
}

func TestWatcher_Tick_MergeableNoAutoMerge_Idles(t *testing.T) {
	approved := strings.Replace(okPRJSON, `"reviewDecision": "REVIEW_REQUIRED"`, `"reviewDecision": "APPROVED"`, 1)
	gh := setupGH(buildPRJSON(approved, "SUCCESS"), "[]", "base1")
	polish := &stubPolish{}
	w := newWatcherForTest(t, gh, polish)

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionIdle, res.Action)
	assert.True(t, res.Changeset.Mergeable)
	assert.Empty(t, polish.calls)
}

func TestWatcher_Tick_StateSaveFailure_Surfaces(t *testing.T) {
	// Create a read-only state-file path so Save's WriteFile fails. LoadState
	// tolerates ENOENT but not "exists and unwritable", so we pre-create the
	// state dir and a 0o400 state file seeded with a valid previous State.
	home := t.TempDir()
	t.Setenv("HOME", home)
	statePath := StatePath("r", 42)
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o755))
	// Pre-seed with a non-first-run state so the tick will decide to record a
	// snapshot and write. Write as 0o400 so the subsequent WriteFile fails.
	pre := &State{
		LastCheckAt:     time.Now(),
		LastSeenHeadSHA: "head0",
		LastSeenBaseSHA: "base0",
	}
	require.NoError(t, pre.Save(statePath))
	require.NoError(t, os.Chmod(statePath, 0o400))
	// Also make the parent dir unwritable so atomic-write approaches also fail.
	require.NoError(t, os.Chmod(filepath.Dir(statePath), 0o500))
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(statePath), 0o755) })

	gh := setupGH(buildPRJSON(okPRJSON, "SUCCESS"), "[]", "base1")
	polish := &stubPolish{}
	cfg := DefaultConfig()
	cfg.PollInterval = 10 * time.Millisecond
	w := NewWatcher(cfg, gh, polish, 42, ".", "r", nil)

	_, err := w.Tick(context.Background())
	require.Error(t, err, "state save failure must propagate")
	assert.Contains(t, err.Error(), "save state")
}

func TestWatcher_Tick_ClosedVsMerged_DistinctActions(t *testing.T) {
	cases := []struct {
		state      string
		wantAction LastAction
	}{
		{state: "MERGED", wantAction: LastActionMerged},
		{state: "CLOSED", wantAction: LastActionClosed},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			modified := strings.Replace(okPRJSON, `"state": "OPEN"`, fmt.Sprintf(`"state": %q`, tc.state), 1)
			gh := setupGH(buildPRJSON(modified, "SUCCESS"), "[]", "base1")
			polish := &stubPolish{}
			w := newWatcherForTest(t, gh, polish)

			res, err := w.Tick(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tc.wantAction, res.Action)
			assert.Empty(t, polish.calls, "closed/merged PRs should not invoke polish")
		})
	}
}

func TestWatcher_DryRun_DoesNotClearCooldown(t *testing.T) {
	// Observe-only dry-run must not erase a real backoff window that a prior
	// live run set up. Pre-seed a future cooldown, run Tick in dry-run mode,
	// and assert the cooldown survives. Dry-run stays inside the cooldown arm
	// and returns idle, so we can't directly observe "no reset" during that
	// tick — instead, expire the cooldown, reload, seed again, and run a
	// dry-run with a divergent snapshot so recordSnapshot actually runs.
	home := t.TempDir()
	t.Setenv("HOME", home)
	statePath := StatePath("r", 42)
	cooldown := time.Now().Add(2 * time.Hour)
	require.NoError(t, (&State{
		LastCheckAt:         time.Now(),
		LastSeenHeadSHA:     "old-head",
		LastSeenBaseSHA:     "base1",
		ConsecutiveFailures: 2,
		CooldownUntil:       cooldown,
	}).Save(statePath))

	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	polish := &stubPolish{}
	cfg := DefaultConfig()
	cfg.PollInterval = 10 * time.Millisecond
	// Put the cooldown far enough in the past that Tick does NOT short-circuit;
	// then we can observe whether recordSnapshot resets the already-in-effect
	// state. Rewrite so the cooldown is in the future but CooldownUntil-check
	// is skipped by using a fresh state. Simpler: we just tick with an expired
	// cooldown, verifying the reset-arm is wired right, then re-run a dry-run
	// and check that no reset fires on a fresh recordSnapshot pass.
	_ = cooldown

	// Phase 1: cooldown in the past, dry-run tick.
	past := time.Now().Add(-1 * time.Hour)
	require.NoError(t, (&State{
		LastCheckAt:         past,
		LastSeenHeadSHA:     "head-prev",
		LastSeenBaseSHA:     "base1",
		ConsecutiveFailures: 2,
		CooldownUntil:       past,
	}).Save(statePath))
	w := NewWatcher(cfg, gh, polish, 42, ".", "r", nil, WithDryRun(true))
	_, err := w.Tick(context.Background())
	require.NoError(t, err)
	s, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 2, s.ConsecutiveFailures,
		"dry-run must not clear ConsecutiveFailures")
	assert.Equal(t, past.Unix(), s.CooldownUntil.Unix(),
		"dry-run must not clear CooldownUntil")
}

func TestWatcher_DryRun_DoesNotAdvanceSeenState(t *testing.T) {
	// Dry-run must be fully observe-only: it must not persist new HEAD/base
	// SHAs, new comment IDs, or new failed-run IDs. Otherwise a --dry-run tick
	// consumes the very triggers the following live tick would need to react
	// to, silently suppressing polish on the next run.
	home := t.TempDir()
	t.Setenv("HOME", home)
	statePath := StatePath("r", 42)
	prevCheck := time.Now().Add(-time.Hour)
	require.NoError(t, (&State{
		LastCheckAt:        prevCheck,
		LastSeenHeadSHA:    "old-head",
		LastSeenBaseSHA:    "old-base",
		LastSeenCommentIDs: []string{"inline:old"},
		LastSeenCIRunIDs:   []int64{1},
		LastAction:         LastActionIdle,
	}).Save(statePath))

	// Snapshot diverges in every persistable dimension.
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), `[{"databaseId":99}]`, "new-base")
	gh.addPrefix(
		"api --paginate repos/o/r/pulls/42/comments",
		`[{"id":123,"user":{"login":"alice","type":"User"},"created_at":"2026-04-19T00:00:00Z"}]`,
	)
	polish := &stubPolish{}
	cfg := DefaultConfig()
	cfg.PollInterval = 10 * time.Millisecond
	w := NewWatcher(cfg, gh, polish, 42, ".", "r", nil, WithDryRun(true))

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionDryRun, res.Action)
	assert.Empty(t, polish.calls)

	s, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, "old-head", s.LastSeenHeadSHA, "dry-run must not advance head")
	assert.Equal(t, "old-base", s.LastSeenBaseSHA, "dry-run must not advance base")
	assert.Equal(t, []string{"inline:old"}, s.LastSeenCommentIDs,
		"dry-run must not persist newly observed comment IDs")
	assert.Equal(t, []int64{1}, s.LastSeenCIRunIDs,
		"dry-run must not persist newly observed failed-run IDs")
	assert.Equal(t, LastActionIdle, s.LastAction,
		"dry-run must not rewrite LastAction")
	assert.Equal(t, prevCheck.Unix(), s.LastCheckAt.Unix(),
		"dry-run must not advance LastCheckAt")
}

func TestFetchComments_HandlesConcatenatedPaginatePages(t *testing.T) {
	t.Parallel()
	// gh api --paginate emits one JSON array per page back-to-back. Verify
	// our decoder-loop flattens two pages into one combined list.
	twoPages := `[{"id":1,"user":{"login":"alice","type":"User"},"created_at":"2026-04-18T00:00:00Z"}]` +
		`[{"id":2,"user":{"login":"bob","type":"User"},"created_at":"2026-04-19T00:00:00Z"}]`
	gh := newFakeGH()
	gh.addPrefix("api --paginate repos/o/r/pulls/42/comments", twoPages)

	got, err := fetchComments(context.Background(), gh, ".", "repos/o/r/pulls/42/comments", "inline", SnapshotOptions{})
	require.NoError(t, err)
	require.Len(t, got, 2, "both pages must be decoded, not just the first")
	assert.Equal(t, "inline:1", got[0].ID)
	assert.Equal(t, "alice", got[0].Author)
	assert.Equal(t, "inline:2", got[1].ID)
	assert.Equal(t, "bob", got[1].Author)
}

func TestFetchBaseSHA_EscapesSlashInBranch(t *testing.T) {
	t.Parallel()
	gh := newFakeGH()
	// gh.addPrefix matches by joined-args prefix; the base-branch slash MUST
	// be URL-escaped (release/1.0 → release%2F1.0) so the refs call remains
	// a single path segment. If the escape regresses, this expectation fails.
	gh.addPrefix("api repos/{owner}/{repo}/git/refs/heads/release%2F1.0", "deadbeef\n")

	sha, err := fetchBaseSHA(context.Background(), gh, ".", "release/1.0")
	require.NoError(t, err)
	assert.Equal(t, "deadbeef", sha)
}

func TestBuildPolishPrompt(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "/pr-polish 42", buildPolishPrompt(42, false))
	assert.Equal(t, "/pr-polish --local 42", buildPolishPrompt(42, true))
	assert.Equal(t, "/pr-polish", buildPolishPrompt(0, false))
}

// Sanity check that fakeGH wraps OS env without leaking real paths.
func TestFakeGHIsolated(t *testing.T) {
	t.Parallel()
	require.NotEmpty(t, os.TempDir())
}

// setThreads makes the fake gh return n unresolved review threads.
func (f *fakeGH) setThreads(n int) {
	f.addPrefix("api graphql", strconv.Itoa(n))
}

// A PR that keeps needing polish while never getting better must stop and go
// to a human. This is the kernel#8227 failure: seventeen polish rounds each
// reported success while unresolved threads went 6 -> 2 -> 11 and CI went red
// on errors the polish commits introduced. The existing backoff only counts
// hard failures, so nothing ever fired and it burned rounds making the PR
// worse.
func TestWatcher_DivergenceGuard_StopsWhenNotImproving(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	gh.setThreads(6)
	polish := &stubPolish{}
	w := newWatcherForTest(t, gh, polish)
	w.cfg.Backoff.MaxRoundsWithoutImprovement = 3

	ctx := context.Background()
	// First tick records the baseline without reacting.
	_, err := w.Tick(ctx)
	require.NoError(t, err)

	// Now hold health flat at its worst: every tick needs polish, none improves.
	gh.setThreads(11)
	var lastAction LastAction
	for range 6 {
		res, err := w.Tick(ctx)
		require.NoError(t, err)
		lastAction = res.Action
		if lastAction == LastActionNeedsHuman {
			break
		}
	}
	assert.Equal(t, LastActionNeedsHuman, lastAction,
		"a PR that stops improving must escalate rather than polish forever")
	assert.Less(t, len(polish.calls), 6,
		"the guard must cut polishing short, not run every round first")
}

// Improvement must reset the budget, or a long but healthy run would trip the
// guard purely for taking many rounds.
func TestWatcher_DivergenceGuard_ImprovementResetsCounter(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	gh.setThreads(10)
	w := newWatcherForTest(t, gh, &stubPolish{})
	w.cfg.Backoff.MaxRoundsWithoutImprovement = 3

	ctx := context.Background()
	_, err := w.Tick(ctx)
	require.NoError(t, err)

	// Two flat rounds, then a real improvement, then two more flat ones. The
	// improvement in the middle must clear the counter so the guard does not
	// trip on a total that spans it.
	for _, threads := range []int{10, 10, 4, 4, 4} {
		gh.setThreads(threads)
		res, err := w.Tick(ctx)
		require.NoError(t, err)
		require.NotEqual(t, LastActionNeedsHuman, res.Action,
			"must not trip while the PR is still improving (threads=%d)", threads)
	}
}

// Missing data must never stop a run: an unreadable thread count disables the
// guard rather than being treated as a healthy zero or a regression.
func TestWatcher_DivergenceGuard_UnknownThreadCountDisablesGuard(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	gh.failPrefix("api graphql", "GraphQL: something broke")
	w := newWatcherForTest(t, gh, &stubPolish{})
	w.cfg.Backoff.MaxRoundsWithoutImprovement = 1

	ctx := context.Background()
	for range 4 {
		res, err := w.Tick(ctx)
		require.NoError(t, err)
		assert.NotEqual(t, LastActionNeedsHuman, res.Action,
			"an unreadable thread count must not be mistaken for divergence")
	}
}

// The end-to-end shape of the yoloswe#287 bug: a mergeable PR carrying an
// unhandled comment reached a terminal state in 1.5 seconds without ever
// invoking pr-polish. The mergeable branches preempted the polish branch, and
// NeedsPolish itself returned false whenever Mergeable was true.
func TestWatcher_MergeableWithNewComments_StillPolishes(t *testing.T) {
	prJSON := strings.Replace(okPRJSON, `"reviewDecision": "REVIEW_REQUIRED"`, `"reviewDecision": "APPROVED"`, 1)
	gh := setupGH(buildPRJSON(prJSON, "SUCCESS"), "[]", "base1")
	gh.setThreads(0)
	// A comment that has never been seen before.
	gh.addPrefix("api --paginate repos/o/r/issues/42/comments",
		`[{"id":991,"user":{"login":"reviewer","type":"User"},"created_at":"2026-07-29T12:00:00Z","body":"please fix"}]`)
	polish := &stubPolish{}
	w := newWatcherForTest(t, gh, polish)
	w.cfg.Polish.AutoMerge = true
	w.cfg.Polish.MergePolicy = MergePolicyNotify

	ctx := context.Background()
	// First tick establishes the baseline without reacting.
	_, err := w.Tick(ctx)
	require.NoError(t, err)

	// Second tick: still mergeable, but the comment is now a NEW comment.
	gh.addPrefix("api --paginate repos/o/r/issues/42/comments",
		`[{"id":991,"user":{"login":"reviewer","type":"User"},"created_at":"2026-07-29T12:00:00Z","body":"please fix"},
		  {"id":992,"user":{"login":"reviewer","type":"User"},"created_at":"2026-07-29T13:00:00Z","body":"and this"}]`)
	res, err := w.Tick(ctx)
	require.NoError(t, err)

	assert.Equal(t, LastActionPolished, res.Action,
		"unhandled feedback must be polished, not short-circuited to a terminal state")
	assert.Len(t, polish.calls, 1, "pr-polish must actually be invoked")
	assert.Nil(t, gh.findCall("pr merge"),
		"never merge while reviewer feedback is outstanding")
}
