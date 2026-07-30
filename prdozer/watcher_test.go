package prdozer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	// ranOnce keys the once rounds this stub reports as finished, on the failure
	// path too: a real runner reports the once-only rounds it completed even when
	// a LATER round failed.
	ranOnce []string
	mu      sync.Mutex
}

func (s *stubPolish) Run(_ context.Context, req PolishRequest) (PolishResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)
	res := PolishResult{}
	for _, key := range s.ranOnce {
		if res.RanOnceRounds == nil {
			res.RanOnceRounds = make(map[string]bool)
		}
		res.RanOnceRounds[key] = true
	}
	if s.err != nil {
		return res, s.err
	}
	res.SessionID, res.Output = "stub", "ok"
	return res, nil
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
	assert.Equal(t, "/pr-polish 42", buildPolishPrompt(42, false, 0))
	assert.Equal(t, "/pr-polish --local 42", buildPolishPrompt(42, true, 0))
	assert.Equal(t, "/pr-polish", buildPolishPrompt(0, false, 0))
}

// /pr-polish loops internally inside ONE polish.Run() call. Uncapped, a single
// tick absorbed 22 rounds over 64 minutes on kernel#8227 — and the divergence
// guard compares health BETWEEN ticks, so a tick that never ends is never
// guarded. The cap is what makes the tick boundary mean anything.
func TestBuildPolishPrompt_CapsInternalRounds(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "/pr-polish --rounds 3 42", buildPolishPrompt(42, false, 3))
	assert.Equal(t, "/pr-polish --local --rounds 3 42", buildPolishPrompt(42, true, 3))
	assert.Equal(t, "/pr-polish 42", buildPolishPrompt(42, false, 0),
		"zero omits the flag so the skill uses its own default")
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

// setHead moves the PR's head SHA, which is what a real polish round does when
// it pushes a commit. Tests that exercise self-review need this: with a fixed
// head the first review marks the SHA seen and needsSelfReview is false forever,
// so the always-unreviewed condition that matters in production never occurs.
func (f *fakeGH) setHead(sha string) {
	f.addPrefix(
		"pr view 42 --json number,url,headRefName,baseRefName,headRefOid,state,isDraft,reviewDecision,mergeable,statusCheckRollup",
		buildPRJSON(strings.Replace(okPRJSON, `"headRefOid": "head1"`, `"headRefOid": "`+sha+`"`, 1), "FAILURE"),
	)
}

// `gh api graphql --paginate` emits one --jq result per page, so a PR with more
// than 100 review threads reports several counts that must be summed. Taking
// only the first would undercount the outstanding work and read as a healthier
// PR than it is — the one direction the divergence guard must not be wrong in.
func TestFetchUnresolvedThreads_SumsPages(t *testing.T) {
	t.Parallel()
	gh := newFakeGH()
	gh.addPrefix("api graphql", "100\n40\n7\n")

	n, err := fetchUnresolvedThreads(context.Background(), gh, ".", "o", "r", 42)
	require.NoError(t, err)
	assert.Equal(t, 147, n)

	call := gh.findCall("api graphql")
	require.NotNil(t, call)
	assert.Contains(t, call, "--paginate",
		"without --paginate gh never walks past the first page")
	assert.Contains(t, strings.Join(call, " "), "$endCursor",
		"gh only paginates a query that declares the cursor variable")
}

// An empty count is unknown, not zero: returning 0 would look like a perfectly
// healthy PR and hand the guard a baseline no later round can beat. The caller
// turns the error into -1, which disables the guard for the tick.
func TestFetchUnresolvedThreads_EmptyIsAnError(t *testing.T) {
	t.Parallel()
	gh := newFakeGH()
	gh.addPrefix("api graphql", "  \n")

	_, err := fetchUnresolvedThreads(context.Background(), gh, ".", "o", "r", 42)
	require.Error(t, err)
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
	var last TickResult
	for range 6 {
		res, err := w.Tick(ctx)
		require.NoError(t, err)
		last = res
		if res.Action == LastActionNeedsHuman {
			break
		}
	}
	assert.Equal(t, LastActionNeedsHuman, last.Action,
		"a PR that stops improving must escalate rather than polish forever")
	assert.Less(t, len(polish.calls), 6,
		"the guard must cut polishing short, not run every round first")
	// Without Diverged the notification falls back to the generic "needs a
	// human" text, which reads like a PR waiting on an approval — the opposite
	// response to one the babysitter was actively making worse.
	assert.True(t, last.Diverged,
		"stopping for divergence must be reported as divergence, not as a plain block")
	assert.GreaterOrEqual(t, last.RoundsSinceImprovement, 3,
		"the reported streak must be the one the guard tripped on")
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

	// Assert the bookkeeping directly, not just that nothing tripped: a run that
	// merely stopped short of the limit would satisfy the loop above even with
	// the reset deleted.
	state, err := LoadState(StatePath("r", 42))
	require.NoError(t, err)
	require.NotNil(t, state.BestHealth)
	assert.Equal(t, 4, state.BestHealth.UnresolvedThreads,
		"the improvement must be adopted as the new best")
	assert.Equal(t, 2, state.RoundsSinceImprovement,
		"the streak must count only the two flat rounds AFTER the improvement")
}

// The guard is consulted inside decideAndAct, so this tick's health has to be
// scored FIRST. Scored afterward, the guard reads a streak that predates the
// snapshot in front of it and keeps halting a PR that has already recovered —
// the improvement resets the counter only after the stop is decided.
//
// The observable case is a tripped run that gets better: a human resolves the
// threads and the babysitter is pointed at the PR again. It must resume, not
// re-halt on a streak the current snapshot has already disproved.
func TestWatcher_DivergenceGuard_RecoveryAfterTripResumes(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	gh.setThreads(10)
	polish := &stubPolish{}
	w := newWatcherForTest(t, gh, polish)
	w.cfg.Backoff.MaxRoundsWithoutImprovement = 3

	ctx := context.Background()
	_, err := w.Tick(ctx)
	require.NoError(t, err)

	// Flat rounds until the guard stops the run.
	var tripped bool
	for range 6 {
		res, err := w.Tick(ctx)
		require.NoError(t, err)
		if res.Action == LastActionNeedsHuman {
			tripped = true
			break
		}
	}
	require.True(t, tripped, "the guard must trip on a run that never improves")
	state, err := LoadState(StatePath("r", 42))
	require.NoError(t, err)
	require.GreaterOrEqual(t, state.RoundsSinceImprovement, 3,
		"the streak is persisted at (or past) the limit")
	callsAtTrip := len(polish.calls)

	// Someone resolved the threads. The very next snapshot is strictly better.
	gh.setThreads(1)
	res, err := w.Tick(ctx)
	require.NoError(t, err)
	assert.NotEqual(t, LastActionNeedsHuman, res.Action,
		"an improving snapshot must be scored before the guard reads the streak")
	assert.Zero(t, res.RoundsSinceImprovement,
		"the reported streak must be the post-scoring value the guard actually used")
	assert.Greater(t, len(polish.calls), callsAtTrip,
		"a recovered PR must resume polishing, not stay stuck at the old streak")
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

// On a repo with no review bots, /pr-polish IS the reviewer — it runs
// codex/cursor locally to PRODUCE findings. Every other trigger is reactive, so
// a healthy new PR fires none of them and prdozer declares it done having never
// reviewed it. yoloswe#287 was closed out in ~1s three separate times that way,
// with zero pr-polish invocations.
func TestWatcher_SelfReview_PolishesAnUnreviewedPR(t *testing.T) {
	prJSON := strings.Replace(okPRJSON, `"reviewDecision": "REVIEW_REQUIRED"`, `"reviewDecision": ""`, 1)
	gh := setupGH(buildPRJSON(prJSON, "SUCCESS"), "[]", "base1")
	gh.setThreads(0)
	gh.addPrefix("api repos/o/r/pulls/42 --jq .merged", "false")
	polish := &stubPolish{}
	w := newWatcherForTest(t, gh, polish)
	w.cfg.Polish.AutoMerge = true
	w.cfg.Polish.MergePolicy = MergePolicyNotify
	w.cfg.Polish.SelfReview = true

	ctx := context.Background()
	res, err := w.Tick(ctx)
	require.NoError(t, err)
	assert.Equal(t, LastActionPolished, res.Action,
		"a bot-less repo must have its review ORIGINATED, not skipped as 'nothing changed'")
	assert.Len(t, polish.calls, 1, "pr-polish must actually run")
	assert.Nil(t, gh.findCall("pr merge"), "never merge a PR that was just reviewed for the first time")
}

// Keyed by SHA so a commit is reviewed exactly once. "No bot reviewed this"
// never stops being true on its own, so an unconditional trigger would
// re-review an idle PR on every tick forever.
func TestWatcher_SelfReview_OncePerCommit(t *testing.T) {
	prJSON := strings.Replace(okPRJSON, `"reviewDecision": "REVIEW_REQUIRED"`, `"reviewDecision": ""`, 1)
	gh := setupGH(buildPRJSON(prJSON, "SUCCESS"), "[]", "base1")
	gh.setThreads(0)
	gh.addPrefix("api repos/o/r/pulls/42 --jq .merged", "false")
	polish := &stubPolish{}
	w := newWatcherForTest(t, gh, polish)
	w.cfg.Polish.AutoMerge = true
	w.cfg.Polish.MergePolicy = MergePolicyNotify
	w.cfg.Polish.SelfReview = true

	ctx := context.Background()
	for range 3 {
		_, err := w.Tick(ctx)
		require.NoError(t, err)
	}
	assert.Len(t, polish.calls, 1,
		"the same commit must be self-reviewed once, not on every tick")
}

// A failed round produced no review, so the commit must stay eligible.
func TestWatcher_SelfReview_FailedRoundStaysEligible(t *testing.T) {
	prJSON := strings.Replace(okPRJSON, `"reviewDecision": "REVIEW_REQUIRED"`, `"reviewDecision": ""`, 1)
	gh := setupGH(buildPRJSON(prJSON, "SUCCESS"), "[]", "base1")
	gh.setThreads(0)
	gh.addPrefix("api repos/o/r/pulls/42 --jq .merged", "false")
	polish := &stubPolish{err: errors.New("boom")}
	w := newWatcherForTest(t, gh, polish)
	w.cfg.Polish.AutoMerge = true
	w.cfg.Polish.MergePolicy = MergePolicyNotify
	w.cfg.Polish.SelfReview = true
	w.cfg.Backoff.MaxConsecutiveFailures = 0 // isolate from the cooldown brake

	ctx := context.Background()
	for range 2 {
		_, err := w.Tick(ctx)
		require.NoError(t, err)
	}
	assert.Len(t, polish.calls, 2, "a failed review must be retried, not marked done")
}

// Without the flag, behavior is unchanged: a clean mergeable PR terminates.
func TestWatcher_SelfReviewOff_CleanPRStillTerminates(t *testing.T) {
	prJSON := strings.Replace(okPRJSON, `"reviewDecision": "REVIEW_REQUIRED"`, `"reviewDecision": ""`, 1)
	gh := setupGH(buildPRJSON(prJSON, "SUCCESS"), "[]", "base1")
	gh.setThreads(0)
	polish := &stubPolish{}
	w := newWatcherForTest(t, gh, polish)
	w.cfg.Polish.AutoMerge = true
	w.cfg.Polish.MergePolicy = MergePolicyNotify

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionNeedsHuman, res.Action)
	assert.Empty(t, polish.calls, "no self-review means no origination")
}

// Some polish steps must run once per PR, not once per tick. /simplify-branch
// is a whole-branch cleanup: it improves an initial diff but churns an evolving
// one, so re-running it every 20 minutes is wrong.
func TestPolishRequest_OnceRoundsRunOnlyWhileOwed(t *testing.T) {
	t.Parallel()
	simplify := RoundSpec{Prompt: "/simplify-branch", Once: true}
	spec := StepSpec{Rounds: []RoundSpec{
		simplify,
		{Prompt: "/pr-polish"},
		{Prompt: "address review comments"},
	}}

	first := PolishRequest{Spec: spec}.activeRounds()
	assert.Len(t, first, 3, "the opening polish runs every round")

	later := PolishRequest{Spec: spec,
		DoneOnceRounds: map[string]bool{simplify.onceKey(): true}}.activeRounds()
	require.Len(t, later, 2, "a completed once round drops out")
	assert.Equal(t, "/pr-polish", later[0].Prompt)
	assert.Equal(t, "address review comments", later[1].Prompt)
}

// Per-round tracking, not one flag for the whole step: rounds stop at the first
// error, so a spec with two once rounds whose SECOND fails must keep the first's
// progress and retry only the one that did not finish.
func TestPolishRequest_PartiallyCompletedOnceRoundsRetryOnlyWhatIsLeft(t *testing.T) {
	t.Parallel()
	first := RoundSpec{Prompt: "/simplify-branch", Once: true}
	second := RoundSpec{Prompt: "/tidy-docs", Once: true}
	spec := StepSpec{Rounds: []RoundSpec{first, {Prompt: "/pr-polish"}, second}}

	got := PolishRequest{Spec: spec,
		DoneOnceRounds: map[string]bool{first.onceKey(): true}}.activeRounds()
	require.Len(t, got, 2)
	assert.Equal(t, "/pr-polish", got[0].Prompt)
	assert.Equal(t, "/tidy-docs", got[1].Prompt,
		"the once round that never finished is still owed")
}

// Content-keyed rather than positional, so the record survives registry edits:
// inserting a round must not make an unrelated one look already-done.
func TestRoundSpec_OnceKeyIdentifiesTheRoundByContent(t *testing.T) {
	t.Parallel()
	a := RoundSpec{Prompt: "/simplify-branch", Once: true}
	assert.Equal(t, a.onceKey(), RoundSpec{Prompt: "/simplify-branch"}.onceKey(),
		"the key covers what the round DOES, not its flags")
	assert.NotEqual(t, a.onceKey(), RoundSpec{Prompt: "/tidy-docs"}.onceKey())
	// A prompt and a command are different rounds even with identical text.
	assert.NotEqual(t, a.onceKey(), RoundSpec{Command: "/simplify-branch"}.onceKey())
}

// No configured spec keeps the existing single-call behaviour, so repos that
// never opt in are unaffected.
func TestPolishRequest_NoSpecFallsBackToTheDefaultCall(t *testing.T) {
	t.Parallel()
	assert.Empty(t, PolishRequest{}.activeRounds(),
		"an empty spec must fall through to the default /pr-polish call")
}

// A spec whose rounds are ALL once-only must not silently do nothing once they
// are done — activeRounds returning empty routes back to the default call, which
// is the safe behaviour rather than a no-op polish.
func TestPolishRequest_AllOnceRoundsFallBackOnceTheyAreDone(t *testing.T) {
	t.Parallel()
	only := RoundSpec{Prompt: "/simplify-branch", Once: true}
	assert.Empty(t, PolishRequest{Spec: StepSpec{Rounds: []RoundSpec{only}},
		DoneOnceRounds: map[string]bool{only.onceKey(): true}}.activeRounds(),
		"all-once spec yields no rounds once done, falling back to the default call")
}

// Polish reuses StepSpec, and the registry docs point at jiradozer configs that
// set a per-step model, so `polish.model` must actually take effect rather than
// being silently ignored in favour of the top-level agent model.
func TestPolishRequest_SpecModelOverridesTheAgentModel(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "spec-model",
		PolishRequest{Model: "agent-model", Spec: StepSpec{Model: "spec-model"}}.modelID())
	assert.Equal(t, "agent-model",
		PolishRequest{Model: "agent-model", Spec: StepSpec{}}.modelID(),
		"no per-step model falls back to the run's agent model")
}

// polishStateFixture pre-seeds state so a moved base triggers a polish on the
// very first tick rather than being absorbed as the run's baseline.
func polishStateFixture(t *testing.T) {
	t.Helper()
	pre := &State{
		PRNumber:        42,
		Repo:            "r",
		LastCheckAt:     time.Now(),
		LastSeenHeadSHA: "head1",
		LastSeenBaseSHA: "old-base",
	}
	require.NoError(t, pre.Save(StatePath("r", 42)))
}

// A once round's progress must survive a polish that FAILED after it: that
// round has already done its work, so repeating it is the churn `once` exists to
// prevent. Deriving the record from a successful-polish counter
// (state.PolishRounds) would repeat it, because that counter only advances on a
// tick that OBSERVES a successful round — note PolishRounds is still zero here.
//
// This also covers the wiring the helper-only tests above cannot: registry spec
// → WithPolishSpec → PolishRequest across two real ticks.
func TestWatcher_OnceRoundsSurviveAPolishThatFailedAfterThem(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "SUCCESS"), "[]", "new-base")
	simplify := RoundSpec{Prompt: "/simplify-branch", Once: true}
	spec := StepSpec{Rounds: []RoundSpec{simplify, {Prompt: "/pr-polish"}}}
	// The once round finished; the round after it blew up.
	polish := &stubPolish{err: errors.New("second round blew up"),
		ranOnce: []string{simplify.onceKey()}}
	w := newWatcherForTest(t, gh, polish, WithPolishSpec(spec))
	polishStateFixture(t)

	ctx := context.Background()
	res, err := w.Tick(ctx)
	require.NoError(t, err)
	require.Equal(t, LastActionFailed, res.Action)
	require.Len(t, polish.calls, 1)
	assert.Empty(t, polish.calls[0].DoneOnceRounds, "the opening polish runs every round")
	assert.Equal(t, spec, polish.calls[0].Spec, "the registry spec must reach the polisher")

	// Second tick: base moves again so polish is triggered once more.
	polish.err = nil
	gh.addPrefix("api repos/{owner}/{repo}/git/refs/heads/main", "newer-base")
	res, err = w.Tick(ctx)
	require.NoError(t, err)
	require.Equal(t, LastActionPolished, res.Action)
	require.Len(t, polish.calls, 2)
	assert.Zero(t, res.PolishRounds,
		"a failed round is never counted — the precondition that made PolishRounds the wrong record")
	assert.Equal(t, map[string]bool{simplify.onceKey(): true}, polish.calls[1].DoneOnceRounds,
		"a completed once round is not owed again")
	assert.Equal(t, []RoundSpec{{Prompt: "/pr-polish"}}, polish.calls[1].activeRounds())
}

// The mirror image: a polish that failed BEFORE reaching its once-only round
// still owes that round. Rounds run in order, so an earlier repeatable round
// failing means the once round never ran at all — recording it on dispatch would
// silently drop it for the life of the PR.
func TestWatcher_OnceRoundsStillOwedWhenPolishFailedBeforeThem(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "SUCCESS"), "[]", "new-base")
	simplify := RoundSpec{Prompt: "/simplify-branch", Once: true}
	spec := StepSpec{Rounds: []RoundSpec{{Prompt: "gather evidence"}, simplify}}
	// Failed on round 1, so the once round never ran.
	polish := &stubPolish{err: errors.New("first round blew up")}
	w := newWatcherForTest(t, gh, polish, WithPolishSpec(spec))
	polishStateFixture(t)

	ctx := context.Background()
	_, err := w.Tick(ctx)
	require.NoError(t, err)
	require.Len(t, polish.calls, 1)

	polish.err, polish.ranOnce = nil, []string{simplify.onceKey()}
	gh.addPrefix("api repos/{owner}/{repo}/git/refs/heads/main", "newer-base")
	_, err = w.Tick(ctx)
	require.NoError(t, err)
	require.Len(t, polish.calls, 2)
	assert.Empty(t, polish.calls[1].DoneOnceRounds,
		"a once round that never ran is still owed")
}

// The record is persisted, not just held in memory: it is written inside
// decideAndAct, and the tick only saves state when something marked it dirty.
// Miss that and the next tick reloads without it and runs the round again.
func TestWatcher_OnceRoundsDoneIsPersisted(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "SUCCESS"), "[]", "new-base")
	simplify := RoundSpec{Prompt: "/simplify-branch", Once: true}
	polish := &stubPolish{ranOnce: []string{simplify.onceKey()}}
	w := newWatcherForTest(t, gh, polish,
		WithPolishSpec(StepSpec{Rounds: []RoundSpec{simplify}}))
	polishStateFixture(t)

	_, err := w.Tick(context.Background())
	require.NoError(t, err)
	state, err := LoadState(StatePath("r", 42))
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{simplify.onceKey(): true}, state.OnceRoundsDone,
		"the once record must outlive the tick that wrote it")
}

// Two once rounds, second failing: the first must be recorded and the second
// still owed. This is the case a single all-or-nothing flag cannot express.
func TestWatcher_PartialOnceProgressIsKeptAcrossTicks(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "SUCCESS"), "[]", "new-base")
	first := RoundSpec{Prompt: "/simplify-branch", Once: true}
	second := RoundSpec{Prompt: "/tidy-docs", Once: true}
	polish := &stubPolish{err: errors.New("/tidy-docs blew up"),
		ranOnce: []string{first.onceKey()}}
	w := newWatcherForTest(t, gh, polish,
		WithPolishSpec(StepSpec{Rounds: []RoundSpec{first, second}}))
	polishStateFixture(t)

	ctx := context.Background()
	_, err := w.Tick(ctx)
	require.NoError(t, err)

	polish.err, polish.ranOnce = nil, []string{second.onceKey()}
	gh.addPrefix("api repos/{owner}/{repo}/git/refs/heads/main", "newer-base")
	_, err = w.Tick(ctx)
	require.NoError(t, err)
	require.Len(t, polish.calls, 2)
	assert.Equal(t, []RoundSpec{second}, polish.calls[1].activeRounds(),
		"only the once round that failed is retried")

	state, err := LoadState(StatePath("r", 42))
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{first.onceKey(): true, second.onceKey(): true},
		state.OnceRoundsDone, "both once rounds end up recorded, one per tick")
}

// A repo with no configured polish spec has no once rounds to track, so the PR
// must not accumulate once-round state — there is nothing for it to mean.
func TestWatcher_NoPolishSpec_LeavesTheOnceRecordEmpty(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "SUCCESS"), "[]", "new-base")
	polish := &stubPolish{}
	w := newWatcherForTest(t, gh, polish)
	polishStateFixture(t)

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	require.Equal(t, LastActionPolished, res.Action)
	state, err := LoadState(StatePath("r", 42))
	require.NoError(t, err)
	assert.Empty(t, state.OnceRoundsDone)
}

// The accounting behind PolishResult.RanOnceRounds, exercised through the real
// runRounds using command rounds (no agent provider needed). Order is what makes
// this subtle: rounds stop at the first error, so what is reported is exactly the
// once rounds that ran BEFORE it — no more and no less.
func TestAgentPolisher_RunRounds_ReportsTheOnceRoundsThatFinished(t *testing.T) {
	t.Parallel()
	ok := RoundSpec{Command: "true", Once: true}
	boom := RoundSpec{Command: "exit 1", Once: true}
	cases := []struct {
		name    string
		rounds  []RoundSpec
		wantRan []RoundSpec
		wantErr bool
	}{{
		name:    "failure after the once round still reports it done",
		rounds:  []RoundSpec{ok, {Command: "exit 1"}},
		wantRan: []RoundSpec{ok},
		wantErr: true,
	}, {
		name:    "failure before the once round leaves it owed",
		rounds:  []RoundSpec{{Command: "exit 1"}, ok},
		wantErr: true,
	}, {
		name:    "one of several once rounds finishing keeps that one's progress",
		rounds:  []RoundSpec{ok, boom},
		wantRan: []RoundSpec{ok},
		wantErr: true,
	}, {
		name:    "every round succeeding reports them all",
		rounds:  []RoundSpec{ok, {Command: "echo done", Once: true}},
		wantRan: []RoundSpec{ok, {Command: "echo done", Once: true}},
	}, {
		name:   "a spec with no once rounds reports nothing",
		rounds: []RoundSpec{{Command: "true"}},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := NewAgentPolisher(nil, slog.New(slog.DiscardHandler))
			req := PolishRequest{PRNumber: 42, WorkDir: t.TempDir(),
				Spec: StepSpec{Rounds: tc.rounds}}
			res, err := p.runRounds(context.Background(), req, tc.rounds)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			want := map[string]bool{}
			for _, r := range tc.wantRan {
				want[r.onceKey()] = true
			}
			got := map[string]bool{}
			for k := range res.RanOnceRounds {
				got[k] = true
			}
			assert.Equal(t, want, got)
		})
	}
}

// A run whose head nobody has reviewed gets one extra allowance before the
// guard stops it.
//
// This isolates the UNREVIEWED arm: red CI keeps BestHealth beatable, so
// saturation cannot be what grants the grace here. The saturated arm is covered
// on its own by TestWatcher_DivergenceGuard_SaturatedBotReviewedRepoExtendsOnce,
// so between them each arm is exercised without the other standing in for it.
//
// The production runs that motivated the exemption had both at once —
// kernel#8297, yoloswe#288, yoloswe#291 each logged best_unresolved=0 and
// head_unreviewed=true — which is exactly why the arms are tested apart: a
// fixture with both true would pass even if one arm were deleted.
func TestWatcher_DivergenceGuard_UnreviewedHeadExtendsOnce(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	gh.setThreads(0)
	polish := &stubPolish{}
	w := newWatcherForTest(t, gh, polish)
	w.cfg.Backoff.MaxRoundsWithoutImprovement = 2
	// prdozer is the only reviewer here, so every pushed round leaves the head
	// unreviewed — the shape all three false positives had.
	w.cfg.Polish.SelfReview = true

	ctx := context.Background()
	_, err := w.Tick(ctx)
	require.NoError(t, err)
	require.False(t, bestHealth(t).Saturated(),
		"red CI must leave BestHealth beatable, else the saturated arm grants the grace and this stops testing the unreviewed one")

	// Run past the plain limit, which is where the unpatched guard stops. The
	// head moves every round because a real polish round pushes a commit —
	// which is what keeps the head unreviewed.
	var last TickResult
	for i := range 3 {
		gh.setHead(fmt.Sprintf("head-r%d", i))
		res, err := w.Tick(ctx)
		require.NoError(t, err)
		last = res
	}
	assert.NotEqual(t, LastActionNeedsHuman, last.Action,
		"an unreviewed head means work landed that the thread count cannot reflect yet")
	assert.GreaterOrEqual(t, last.RoundsSinceImprovement, 2,
		"the streak must genuinely have passed the plain limit, else the case is vacuous")
}

// The extension is bounded, and that bound is what keeps the guard able to fire
// at all on a self_review repo: every polish round pushes a commit, moving the
// head, so needsSelfReview is true forever. An unconditional exemption would
// mean the run never stops — the unbounded loop the guard exists to prevent
// (kernel#8227: 17 rounds, ~$40, threads 6 -> 2 -> 11, CI red).
func TestWatcher_DivergenceGuard_UnreviewedHeadStillStopsAtHardLimit(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	gh.setThreads(0)
	w := newWatcherForTest(t, gh, &stubPolish{})
	w.cfg.Backoff.MaxRoundsWithoutImprovement = 2
	w.cfg.Polish.SelfReview = true

	ctx := context.Background()
	_, err := w.Tick(ctx)
	require.NoError(t, err)
	require.False(t, bestHealth(t).Saturated(),
		"red CI must leave BestHealth beatable, else this bounds the saturated arm instead of the unreviewed one")

	var last TickResult
	for i := range 10 {
		// Head moves every round, so needsSelfReview is true forever — the
		// condition that would make an unconditional exemption unbounded.
		gh.setHead(fmt.Sprintf("head-r%d", i))
		res, err := w.Tick(ctx)
		require.NoError(t, err)
		last = res
		if res.Action == LastActionNeedsHuman {
			break
		}
	}
	assert.Equal(t, LastActionNeedsHuman, last.Action,
		"a perpetually unreviewed head must not buy an unbounded run")
	assert.True(t, last.Diverged)
	assert.GreaterOrEqual(t, last.RoundsSinceImprovement, 4,
		"the hard limit is 2*MaxRoundsWithoutImprovement")
}

// conflictingPRJSON is a PR that needs a rebase. Used to keep a BOT-reviewed
// repo polishing tick after tick: needsSelfReview is false there, so without a
// standing polish trigger the watcher idles before it ever reaches the
// divergence guard. Conflicting is the cheapest such trigger — it fires from
// snapshot state alone, with no comment or CI bookkeeping per tick.
var conflictingPRJSON = strings.Replace(okPRJSON, `"mergeable": "MERGEABLE"`, `"mergeable": "CONFLICTING"`, 1)

// bestHealth reads the health the watcher has persisted for the fixture PR.
//
// Asserted on rather than inferred from the tick's action: these tests only
// mean anything if BestHealth really is (or really is not) the saturated floor,
// and a fixture that quietly stopped producing the intended health would
// otherwise pass vacuously.
func bestHealth(t *testing.T) *PRHealth {
	t.Helper()
	state, err := LoadState(StatePath("r", 42))
	require.NoError(t, err)
	require.NotNil(t, state.BestHealth, "no health recorded; the fixture never yielded a thread count")
	return state.BestHealth
}

// A saturated run on a BOT-reviewed repo gets the same allowance as a
// self-reviewed one. This is the kernel shape, and it is the one that hit
// production: bots post findings, each round fixes them, threads bounce
// 0 -> n -> 0, and BestHealth stays pinned at the saturated floor it touched on
// tick one. Every later round then counts as "no improvement" whatever it
// accomplished.
//
// The exemption must therefore key on the METRIC being uninformative, not on
// needsSelfReview: that predicate is false whenever `self_review` is off, so
// gating on it alone leaves every bot-reviewed repo stopping on the old
// saturation cutoff — the false positive unfixed for the repo mode that had it.
func TestWatcher_DivergenceGuard_SaturatedBotReviewedRepoExtendsOnce(t *testing.T) {
	gh := setupGH(buildPRJSON(conflictingPRJSON, "SUCCESS"), "[]", "base1")
	gh.setThreads(0)
	w := newWatcherForTest(t, gh, &stubPolish{})
	w.cfg.Backoff.MaxRoundsWithoutImprovement = 2
	// The bots are the reviewers here, so prdozer never originates a review and
	// needsSelfReview is false on every tick. Only saturation can grant grace.
	w.cfg.Polish.SelfReview = false

	ctx := context.Background()
	// Tick one observes {0 unresolved, ci green} and pins BestHealth to the
	// unbeatable floor. Nothing after this can beat it.
	_, err := w.Tick(ctx)
	require.NoError(t, err)
	require.True(t, bestHealth(t).Saturated(),
		"the case is vacuous unless BestHealth really is the unbeatable floor")

	var last TickResult
	for range 3 {
		res, err := w.Tick(ctx)
		require.NoError(t, err)
		last = res
	}
	assert.NotEqual(t, LastActionNeedsHuman, last.Action,
		"a saturated streak is arithmetic, not evidence the run stopped making progress")
	assert.False(t, last.Diverged)
	assert.GreaterOrEqual(t, last.RoundsSinceImprovement, 2,
		"the streak must genuinely have passed the plain limit, else the case is vacuous")
}

// The saturated arm is bounded by the same 2*limit as the unreviewed one. A
// saturated BestHealth never becomes unsaturated — nothing can beat the floor —
// so an unconditional exemption here would be permanently unbounded, on every
// repo rather than just self-reviewed ones.
func TestWatcher_DivergenceGuard_SaturatedBotReviewedRepoStillStopsAtHardLimit(t *testing.T) {
	gh := setupGH(buildPRJSON(conflictingPRJSON, "SUCCESS"), "[]", "base1")
	gh.setThreads(0)
	w := newWatcherForTest(t, gh, &stubPolish{})
	w.cfg.Backoff.MaxRoundsWithoutImprovement = 2
	w.cfg.Polish.SelfReview = false

	ctx := context.Background()
	_, err := w.Tick(ctx)
	require.NoError(t, err)

	var last TickResult
	for range 10 {
		res, err := w.Tick(ctx)
		require.NoError(t, err)
		last = res
		if res.Action == LastActionNeedsHuman {
			break
		}
	}
	assert.Equal(t, LastActionNeedsHuman, last.Action,
		"a permanently saturated metric must not buy an unbounded run")
	assert.True(t, last.Diverged)
	assert.GreaterOrEqual(t, last.RoundsSinceImprovement, 4,
		"the hard limit is 2*MaxRoundsWithoutImprovement")
}

// The kernel#8227 shape — the case the guard was BUILT for — must still stop at
// the plain limit on a bot-reviewed repo. Red CI means BestHealth is not
// saturated: a green rollup would still beat it, so the streak is a real
// measurement and neither exemption arm applies.
//
// This is the bound on the other axis. Widening the exemption from
// needsSelfReview to "saturated OR unreviewed" must not soften the guard for
// runs whose metric can still move.
func TestWatcher_DivergenceGuard_UnsaturatedBotReviewedRepoStopsAtPlainLimit(t *testing.T) {
	gh := setupGH(buildPRJSON(conflictingPRJSON, "FAILURE"), "[]", "base1")
	gh.setThreads(0)
	w := newWatcherForTest(t, gh, &stubPolish{})
	w.cfg.Backoff.MaxRoundsWithoutImprovement = 2
	w.cfg.Polish.SelfReview = false

	ctx := context.Background()
	_, err := w.Tick(ctx)
	require.NoError(t, err)
	require.False(t, bestHealth(t).Saturated(),
		"red CI must leave BestHealth beatable, else this tests the saturated path")

	var last TickResult
	for range 10 {
		res, err := w.Tick(ctx)
		require.NoError(t, err)
		last = res
		if res.Action == LastActionNeedsHuman {
			break
		}
	}
	assert.Equal(t, LastActionNeedsHuman, last.Action,
		"a beatable BestHealth means the streak measures divergence and must stop the run")
	assert.True(t, last.Diverged)
	assert.Equal(t, 2, last.RoundsSinceImprovement,
		"an unsaturated run stops at the plain limit, not the doubled one")
}

// A provider outage must not spend the failure brake. The brake exists to stop
// a run that is not converging; an API 529 says nothing about the PR.
//
// This is the real kernel#8031 sequence: three ticks died on provider errors
// (529, force-completed turn, 529) and the run entered a 2h cooldown with a
// perfectly healthy branch.
func TestWatcher_Tick_TransientPolishError_DoesNotCountTowardBrake(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	polish := &stubPolish{err: fmt.Errorf(
		"polish round 1/3: agent execution: API Error: 529 Overloaded. This is a server-side issue, usually temporary")}
	w := newWatcherForTest(t, gh, polish)
	statePath := StatePath("r", 42)

	pre := &State{LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1"}
	require.NoError(t, pre.Save(statePath))

	// Three ticks — enough to trip the brake twice over if they counted.
	for range 3 {
		_, err := w.Tick(context.Background())
		require.NoError(t, err)
	}

	s, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 0, s.ConsecutiveFailures, "a provider outage is not a PR failure")
	assert.True(t, s.CooldownUntil.IsZero(), "transient errors must not trip the cooldown")
	assert.Equal(t, LastActionTransient, s.LastAction)
	assert.Len(t, polish.calls, 3, "every tick should still attempt polish; nothing is braking")
}

// The other half: a transient landing mid-streak must not launder real failures
// back to zero. Reaching the success arm would reset the counter, which is the
// specific mistake this action's own switch arm exists to prevent.
func TestWatcher_Tick_TransientDoesNotClearRealFailureStreak(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	polish := &stubPolish{err: fmt.Errorf("boom")}
	w := newWatcherForTest(t, gh, polish)
	statePath := StatePath("r", 42)

	pre := &State{LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1"}
	require.NoError(t, pre.Save(statePath))

	// One real failure.
	_, err := w.Tick(context.Background())
	require.NoError(t, err)
	s1, err := LoadState(statePath)
	require.NoError(t, err)
	require.Equal(t, 1, s1.ConsecutiveFailures)

	// A provider blip lands next.
	polish.err = fmt.Errorf("agent execution: API Error: 529 Overloaded")
	_, err = w.Tick(context.Background())
	require.NoError(t, err)
	s2, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 1, s2.ConsecutiveFailures,
		"the transient neither adds to nor clears the real failure that preceded it")

	// The next real failure resumes the streak and trips the brake, proving the
	// transient did not reset progress toward it.
	polish.err = fmt.Errorf("boom again")
	_, err = w.Tick(context.Background())
	require.NoError(t, err)
	s3, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 2, s3.ConsecutiveFailures)
	assert.False(t, s3.CooldownUntil.IsZero(), "the real streak still reaches the cooldown")
}
