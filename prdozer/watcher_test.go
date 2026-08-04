package prdozer

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	contains map[string]*wt.CmdResult
	calls    [][]string
	mu       sync.Mutex
}

func newFakeGH() *fakeGH {
	return &fakeGH{
		results:  make(map[string]*wt.CmdResult),
		failures: make(map[string]*wt.CmdResult),
		contains: make(map[string]*wt.CmdResult),
	}
}

// addPrefix registers a stdout response for any call whose joined-args starts with prefix.
func (f *fakeGH) addPrefix(prefix, stdout string) {
	f.results[prefix] = &wt.CmdResult{Stdout: stdout}
}

// addContains registers a stdout response for any call whose joined args
// CONTAIN substr. Every GraphQL call shares the `api graphql -f query=` prefix
// and differs only inside the query body, so prefix matching cannot tell the
// commit-count query from the review-thread one.
func (f *fakeGH) addContains(substr, stdout string) {
	f.contains[substr] = &wt.CmdResult{Stdout: stdout}
}

// failPrefix registers a FAILING response (non-zero exit plus stderr) for any
// call whose joined-args starts with prefix, so tests can exercise the paths
// that only run when a gh command actually errors.
func (f *fakeGH) failPrefix(prefix, stderr string) {
	f.failures[prefix] = &wt.CmdResult{Stderr: stderr, ExitCode: 1}
}

// recoverPrefix drops a previously registered failure, so a test can make a gh
// call fail on one tick and succeed on the next — the shape of a transient
// outage, which is the only way to prove a blip does not corrupt saved state.
func (f *fakeGH) recoverPrefix(prefix string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.failures, prefix)
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
	// Substring matches are checked before prefixes: they exist to pick one
	// GraphQL query out of several that share a prefix, so a broad prefix must
	// not shadow them.
	for substr, res := range f.contains {
		if strings.Contains(joined, substr) {
			return res, nil
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
	// completedRounds is how many rounds finished before the error, of any kind.
	// Separate from ranOnce because the real runner counts every round here but
	// keys only `once: true` ones in RanOnceRounds — the whole distinction the
	// no-progress reset depends on.
	completedRounds int
	mu              sync.Mutex
}

func (s *stubPolish) Run(_ context.Context, req PolishRequest) (PolishResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)
	res := PolishResult{CompletedRounds: s.completedRounds}
	for _, key := range s.ranOnce {
		if res.RanOnceRounds == nil {
			res.RanOnceRounds = make(map[string]bool)
		}
		res.RanOnceRounds[key] = true
		// A once round that finished is a round that finished. Mirrors the real
		// runner, which increments CompletedRounds for every round kind.
		res.CompletedRounds++
	}
	if s.err != nil {
		return res, s.err
	}
	res.SessionID, res.Output = "stub", "ok"
	return res, nil
}

// prViewPrefix is the args prefix of the snapshot's `gh pr view` call. Named so
// a test can re-register it and REPLACE the response; a second, differently
// spelled prefix that also matched would be resolved by map iteration order.
const prViewPrefix = "pr view 42 --json number,url,headRefName,baseRefName,headRefOid,state,isDraft,reviewDecision,mergeable,statusCheckRollup"

func setupGH(prJSON, runListJSON, baseSHA string) *fakeGH {
	gh := newFakeGH()
	gh.addPrefix(prViewPrefix, prJSON)
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

// A shell round's failure must never be exempted from the brake, however its
// captured output happens to read.
//
// The transient classifier matches error TEXT — that is what lets it recognize
// an untyped upstream 529 — but command rounds embed up to 500 characters of
// captured stdout/stderr because that output is the diagnostic. Without the
// CommandRoundError marker the classifier reads arbitrary program output, and
// these two real failures were measured classifying as transient:
//
//	"exit status 1: request failed with 529 from upstream"  -> http_5xx
//	"exit status 2: ERROR test_retry_on_timeout FAILED"     -> timeout
//
// Exempting those is worse than the over-braking this change set out to fix: a
// genuinely broken PR would loop forever with no cooldown at all.
func TestClassifyAgentFailure_CommandErrorNeverTransient(t *testing.T) {
	t.Parallel()
	w := &Watcher{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	for _, text := range []string{
		"command failed: exit status 1: request failed with 529 from upstream",
		"command failed: exit status 2: ERROR test_retry_on_timeout FAILED",
		"command failed: exit status 1: connection reset by peer",
	} {
		cmdErr := &CommandRoundError{Err: errors.New(text)}
		action, ok := w.classifyAgentFailure("merge rework", cmdErr, text)
		assert.False(t, ok, "a shell round never talks to the provider: %s", text)
		assert.Empty(t, string(action))

		// Control: the SAME text without the marker is exempted. Without this the
		// test would still pass if the strings simply never matched, and would
		// prove nothing about the marker.
		_, bare := w.classifyAgentFailure("merge rework", errors.New(text), text)
		assert.True(t, bare,
			"control: this text must be classifier-matchable, else the case is vacuous: %s", text)
	}
}

// The exemption must still apply to a genuine provider failure on the same
// route, so the marker narrows the classification rather than disabling it.
func TestClassifyAgentFailure_ProviderErrorStillTransient(t *testing.T) {
	t.Parallel()
	w := &Watcher{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	err := errors.New("merge_rework round 1/1: agent execution: API Error: 529 Overloaded")
	action, ok := w.classifyAgentFailure("merge rework", err, err.Error())
	assert.True(t, ok)
	assert.Equal(t, LastActionTransient, action)
}

// A divergence verdict from a previous run must not stop a run looking at
// different code.
//
// State outlives the run that wrote it, which is right for the once-round record
// and the merge-attempt count. RoundsSinceImprovement is different: it says "the
// last N rounds did not improve THIS head". After a rebase or a push the next
// run inherits a judgment about commits that are gone and stops on tick one.
//
// Hand-cleared three times in one day (yoloswe#290, yoloswe#291, kernel#8297).
func TestWatcher_StaleDivergenceVerdict_RetiredWhenHeadMoved(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	gh.setThreads(9)
	polish := &stubPolish{}
	w := newWatcherForTest(t, gh, polish)
	w.cfg.Backoff.MaxRoundsWithoutImprovement = 3
	statePath := StatePath("r", 42)

	// Exactly kernel#8297's file: a terminal verdict scoring a head the PR has
	// since moved past, with a BestHealth that is unbeatable-looking but NOT
	// saturated — so the saturation exemption cannot rescue it.
	stale := &State{
		LastCheckAt:            time.Now(),
		LastSeenHeadSHA:        "an-older-head",
		LastSeenBaseSHA:        "base1",
		LastAction:             LastActionNeedsHuman,
		RoundsSinceImprovement: 3,
		BestHealth:             &PRHealth{UnresolvedThreads: 1},
	}
	require.NoError(t, stale.Save(statePath))

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.NotEqual(t, LastActionNeedsHuman, res.Action,
		"a verdict about commits that are gone must not end the run on tick one")
	assert.False(t, res.Diverged)
	assert.NotEmpty(t, polish.calls, "the run must actually get to work")
}

// The retirement must NOT fire mid-run. Every polish round pushes a commit, so
// the head moves constantly; keying on head movement alone would reset the
// streak every round and disable the guard completely — an unbounded loop,
// which is worse than the false positive being fixed.
func TestWatcher_StaleDivergenceVerdict_NotRetiredMidRun(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	gh.setThreads(9)
	w := newWatcherForTest(t, gh, &stubPolish{})
	w.cfg.Backoff.MaxRoundsWithoutImprovement = 3
	statePath := StatePath("r", 42)

	// Same moved head, but the run is mid-flight: the previous tick POLISHED.
	midRun := &State{
		LastCheckAt:            time.Now(),
		LastSeenHeadSHA:        "an-older-head",
		LastSeenBaseSHA:        "base1",
		LastAction:             LastActionPolished,
		RoundsSinceImprovement: 3,
		BestHealth:             &PRHealth{UnresolvedThreads: 1},
	}
	require.NoError(t, midRun.Save(statePath))

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionNeedsHuman, res.Action,
		"a mid-run streak still stops the run; the head moving is just the last round's push")
	assert.True(t, res.Diverged)
}

// Retiring the verdict must reach DISK, not just the TickResult.
//
// The retirement only helps if the next tick reloads a cleared streak; a reset
// that lives in memory and is dropped on the way to state.json would let the
// stale verdict come back on the very next tick. The other tests here assert on
// the returned action, which the reset survives regardless of whether it was
// saved, so they cannot catch that.
//
// Persistence currently rides on recordSnapshot marking state dirty when the
// head SHA differs — necessarily true here, since a moved head is what triggers
// retirement in the first place — with Tick's healthDirty fold as a second path
// to the same save. Asserting on the reloaded file pins the OUTCOME, so either
// mechanism may change as long as the reset still lands.
func TestWatcher_StaleDivergenceVerdict_ResetIsPersisted(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	gh.setThreads(9)
	w := newWatcherForTest(t, gh, &stubPolish{})
	w.cfg.Backoff.MaxRoundsWithoutImprovement = 3
	statePath := StatePath("r", 42)

	stale := &State{
		LastCheckAt:            time.Now(),
		LastSeenHeadSHA:        "an-older-head",
		LastSeenBaseSHA:        "base1",
		LastAction:             LastActionNeedsHuman,
		RoundsSinceImprovement: 3,
		BestHealth:             &PRHealth{UnresolvedThreads: 1},
	}
	require.NoError(t, stale.Save(statePath))

	_, err := w.Tick(context.Background())
	require.NoError(t, err)

	got, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Zero(t, got.RoundsSinceImprovement,
		"the cleared streak must survive the save, or the next tick inherits it again")
	assert.Equal(t, "head1", got.LastSeenHeadSHA,
		"the scored head must advance, or the retirement re-fires forever")
	// BestHealth is cleared by the retirement and then immediately rebaselined by
	// recordHealth against the live snapshot. Asserted unconditionally: this
	// fixture serves a readable snapshot (setThreads(9), FAILURE rollup), so
	// recordHealth's nil branch always fires and a nil here is a regression, not
	// a legal outcome. Guarding the assertion would let a reset that clears the
	// stale value but never writes a new one pass silently.
	require.NotNil(t, got.BestHealth,
		"retirement must rebaseline BestHealth, not just clear it")
	assert.Equal(t, PRHealth{UnresolvedThreads: 9, CIFailing: true}, *got.BestHealth,
		"BestHealth must come from the current snapshot, not the stale run's unbeatable-looking {1}")
}

// A resumed run looking at the SAME commits keeps the verdict: nothing has
// changed, so the previous judgment still applies.
func TestWatcher_StaleDivergenceVerdict_KeptWhenHeadUnchanged(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	gh.setThreads(9)
	w := newWatcherForTest(t, gh, &stubPolish{})
	w.cfg.Backoff.MaxRoundsWithoutImprovement = 3
	statePath := StatePath("r", 42)

	same := &State{
		LastCheckAt:            time.Now(),
		LastSeenHeadSHA:        "head1", // what the fake gh serves
		LastSeenBaseSHA:        "base1",
		LastAction:             LastActionNeedsHuman,
		RoundsSinceImprovement: 3,
		BestHealth:             &PRHealth{UnresolvedThreads: 1},
	}
	require.NoError(t, same.Save(statePath))

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionNeedsHuman, res.Action,
		"same code, same verdict — re-dispatching must not launder a real stop")
	assert.True(t, res.Diverged)
}

// The stall streak must be retired on resume for the same reason the divergence
// streak is: it is a judgment about a specific head.
//
// Without this, InvocationsSinceRound is the one no-progress counter that
// survives a push. A run stopped at needs_human leaves it at the limit; the next
// run resumes on a NEW head, and the first non-transient failure — one tick,
// wholly unrelated to whatever stalled before — re-crosses the threshold and
// halts again, forcing the state-file hand-edit the divergence retirement was
// added to eliminate.
func TestWatcher_StaleStallStreak_RetiredWhenHeadMoved(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	gh.setThreads(9)
	polish := &stubPolish{}
	w := newWatcherForTest(t, gh, polish)
	statePath := StatePath("r", 42)

	// A previous run that ended on the stall guard alone: the divergence
	// counters never moved, because no round ever returned to score.
	stale := &State{
		LastCheckAt:           time.Now(),
		LastSeenHeadSHA:       "an-older-head",
		LastSeenBaseSHA:       "base1",
		LastAction:            LastActionNeedsHuman,
		InvocationsSinceRound: 2 * w.cfg.Backoff.MaxConsecutiveFailures,
	}
	require.NoError(t, stale.Save(statePath))

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.False(t, res.Stalled,
		"a stall streak about commits that are gone must not stop the resumed run")
	assert.NotEmpty(t, polish.calls, "the run must actually get to work")

	s, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 0, s.InvocationsSinceRound,
		"the retirement must reach disk, or the next tick reloads the same wall")
}

// The mirror case: the same inherited streak on the SAME head is a verdict that
// still applies. Retiring it there would make the guard unresumable-past, i.e.
// no guard at all across restarts.
func TestWatcher_StaleStallStreak_KeptWhenHeadUnchanged(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	gh.setThreads(9)
	polish := &stubPolish{err: fmt.Errorf("boom: round died before returning")}
	w := newWatcherForTest(t, gh, polish)
	statePath := StatePath("r", 42)

	same := &State{
		LastCheckAt:           time.Now(),
		LastSeenHeadSHA:       "head1", // what the fake gh serves
		LastSeenBaseSHA:       "base1",
		LastAction:            LastActionNeedsHuman,
		InvocationsSinceRound: 2*w.cfg.Backoff.MaxConsecutiveFailures - 1,
	}
	require.NoError(t, same.Save(statePath))

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionNeedsHuman, res.Action,
		"same head, same streak — one more fruitless invocation must still stop it")
	assert.True(t, res.Stalled, "and it must be reported as a stall, not an approval block")
}

// A stall is our own deadline firing, not the provider going down, so it must
// reach the brake.
//
// The real kernel#8374 string. Three invocations (22:15, 22:45, 23:17) each
// armed a /pr-polish reviewer background join, each was force-completed ten
// minutes later, and each was exempted as a provider outage. 62 minutes and
// three full bootstraps produced zero completed rounds and no brake ever fired.
func TestClassifyAgentFailure_GraceForcedCountsTowardBrake(t *testing.T) {
	t.Parallel()
	w := &Watcher{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	err := errors.New("polish round 1/2: agent execution: transient CLI error: " +
		"stream idle: turn forced complete after grace period gated on background tool_use")
	action, ok := w.classifyAgentFailure("polish", err, err.Error())
	assert.True(t, ok, "a stall is still classified, just not exempted")
	assert.Equal(t, LastActionStalled, action,
		"grace_forced must not share the provider-outage exemption")
}

// The other arm, asserted in the same shape. Without this a wholesale deletion
// of the exemption would still pass the grace_forced test above, and kernel#8031
// would regress silently.
func TestClassifyAgentFailure_ProviderOutageStillExempt(t *testing.T) {
	t.Parallel()
	w := &Watcher{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	for _, text := range []string{
		"polish round 1/3: agent execution: API Error: 529 Overloaded",
		"polish round 1/3: agent execution: API Error: 503 Service Unavailable",
	} {
		action, ok := w.classifyAgentFailure("polish", errors.New(text), text)
		assert.True(t, ok, "%s", text)
		assert.Equal(t, LastActionTransient, action,
			"a provider outage must stay exempt from the brake: %s", text)
	}
}

// End to end: a run that keeps stalling has to stop, where before it looped.
//
// Asserts the full mechanism rather than the classification alone — the action
// has to reach recordSnapshot's failure arm for the cooldown to exist.
func TestWatcher_Tick_RepeatedStalls_TripTheBrake(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	polish := &stubPolish{err: fmt.Errorf("polish round 1/2: agent execution: " +
		"transient CLI error: stream idle: turn forced complete after grace period " +
		"gated on background tool_use")}
	w := newWatcherForTest(t, gh, polish)
	statePath := StatePath("r", 42)

	pre := &State{LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1"}
	require.NoError(t, pre.Save(statePath))

	for range 3 {
		_, err := w.Tick(context.Background())
		require.NoError(t, err)
	}

	s, err := LoadState(statePath)
	require.NoError(t, err)
	// Assert the brake actually ENGAGED, not merely that some counter moved.
	// `> 0` would also pass if stalls were misclassified as LastActionFailed, or
	// if only one of the three ticks counted — both regressions this test exists
	// to catch. The cooldown is the observable consequence, so pin that too.
	assert.GreaterOrEqual(t, s.ConsecutiveFailures, w.cfg.Backoff.MaxConsecutiveFailures,
		"a stall recurs on retry, so every one must count toward the brake")
	assert.Equal(t, LastActionStalled, s.LastAction,
		"the stall must be recorded as such, not laundered into a plain failure")
	assert.False(t, s.CooldownUntil.IsZero(),
		"reaching MaxConsecutiveFailures with a cooldown configured must arm it")
}

// The cooldown alert must name the cause that actually armed the window.
//
// The regression this pins: the reporting path can only reload persisted state,
// and it used to quote LastMergeError. That field is written by the merge path
// alone and is never cleared by a stall, so a run that failed a merge early and
// later stalled its way into a cooldown told the operator to go look at a merge
// error that had nothing to do with why it backed off. Seeded with exactly that
// stale error so a regression reports it rather than the stall.
func TestWatcher_StallCooldown_ReportsStallNotStaleMergeError(t *testing.T) {
	const staleMergeErr = "Pull request is in an unmergeable state"

	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	polish := &stubPolish{err: fmt.Errorf("polish round 1/2: agent execution: " +
		"transient CLI error: stream idle: turn forced complete after grace period " +
		"gated on background tool_use")}
	w := newWatcherForTest(t, gh, polish)
	statePath := StatePath("r", 42)

	pre := &State{
		LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1",
		LastMergeError: staleMergeErr,
	}
	require.NoError(t, pre.Save(statePath))

	for range 3 {
		_, err := w.Tick(context.Background())
		require.NoError(t, err)
	}

	s, err := LoadState(statePath)
	require.NoError(t, err)
	require.False(t, s.CooldownUntil.IsZero(), "the stalls must have armed a cooldown")
	assert.Equal(t, staleMergeErr, s.LastMergeError,
		"the merge field is deliberately left alone — it feeds the rework prompt")
	assert.NotEmpty(t, s.LastCooldownCause, "an armed cooldown must record its cause")
	assert.NotContains(t, s.LastCooldownCause, staleMergeErr,
		"a stall-armed cooldown must not be attributed to an unrelated older merge failure")
	assert.Contains(t, s.LastCooldownCause, "stall",
		"the cause must name the stall that actually armed the window")

	// The cause reaches the operator-facing alert, not just the state file.
	report := CooldownWarning(sampleMeta(), s.CooldownUntil, s.LastCooldownCause)
	assert.Contains(t, report.Detail, "stall")
	assert.NotContains(t, report.Detail, staleMergeErr)
}

// Same regression as the stall case above, on the other arm that reaches the
// cooldown: a PLAIN polish failure. classifyAgentFailure declines to classify a
// non-transient error, so the tick returns LastActionFailed rather than
// LastActionStalled — a different path into the same backoff, and the one that
// still read LastMergeError. A run that failed a merge early and later backed
// off on repeated polish failures blamed that stale merge error.
func TestWatcher_PlainFailureCooldown_ReportsFailureNotStaleMergeError(t *testing.T) {
	const staleMergeErr = "Pull request is in an unmergeable state"
	const polishErr = "polish round 1/2: agent execution: exit status 1"

	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	// Deliberately NOT a transient-looking error: it must fall through
	// classifyAgentFailure to the LastActionFailed arm, which is the arm under
	// test. A transient or grace-forced error would take a different branch.
	polish := &stubPolish{err: errors.New(polishErr)}
	w := newWatcherForTest(t, gh, polish)
	statePath := StatePath("r", 42)

	pre := &State{
		LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1",
		LastMergeError: staleMergeErr,
	}
	require.NoError(t, pre.Save(statePath))

	// MaxConsecutiveFailures is 2, so two failures arm the cooldown — comfortably
	// inside the no-progress guard's 2x limit, so this exercises the failure
	// brake and not the stall stop.
	for range 2 {
		res, err := w.Tick(context.Background())
		require.NoError(t, err)
		require.Equal(t, LastActionFailed, res.Action,
			"the seeded error must reach the plain-failure arm under test")
	}

	s, err := LoadState(statePath)
	require.NoError(t, err)
	require.False(t, s.CooldownUntil.IsZero(), "the failures must have armed a cooldown")
	assert.Equal(t, staleMergeErr, s.LastMergeError,
		"the merge field is deliberately left alone — it feeds the rework prompt")
	assert.NotEmpty(t, s.LastCooldownCause, "an armed cooldown must record its cause")
	assert.NotContains(t, s.LastCooldownCause, staleMergeErr,
		"a polish-armed cooldown must not be attributed to an unrelated older merge failure")
	assert.Contains(t, s.LastCooldownCause, polishErr,
		"the cause must name the polish failure that actually armed the window")

	// The cause reaches the operator-facing alert, not just the state file.
	report := CooldownWarning(sampleMeta(), s.CooldownUntil, s.LastCooldownCause)
	assert.Contains(t, report.Detail, polishErr)
	assert.NotContains(t, report.Detail, staleMergeErr)
}

// A cooldown cleared by a successful tick must not leave its cause behind for
// the next one to misreport.
func TestWatcher_SuccessfulTick_ClearsCooldownCause(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "SUCCESS"), "[]", "new-base")
	// Built before the state is seeded: newWatcherForTest repoints HOME at a
	// temp dir, and StatePath is under HOME — seeding first writes the file
	// where the tick will never look for it.
	w := newWatcherForTest(t, gh, &stubPolish{})

	statePath := StatePath("r", 42)
	pre := &State{
		PRNumber: 42, Repo: "r",
		LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "old-base",
		ConsecutiveFailures: 2,
		CooldownUntil:       time.Now().Add(-time.Hour), // expired: does not short-circuit the tick
		LastCooldownCause:   "polish round stalled: stream idle",
	}
	require.NoError(t, pre.Save(statePath))

	// Any non-failing action reaches recordSnapshot's clearing arm; assert on
	// the arm rather than pinning one specific action, so a fixture change that
	// yields idle instead of polished doesn't fail this test for the wrong
	// reason. The cleared cooldown below is the real precondition.
	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	require.NotContains(t, []LastAction{LastActionFailed, LastActionReworked, LastActionStalled},
		res.Action, "the tick must not have failed, or it would arm a cooldown rather than clear one")

	s, err := LoadState(statePath)
	require.NoError(t, err)
	require.True(t, s.CooldownUntil.IsZero(), "a successful tick clears the cooldown")
	assert.Empty(t, s.LastCooldownCause,
		"the cause must be cleared with the window it describes, or the next cooldown inherits it")
}

// The merge brake arms its own cooldown after the failure switch, so its cause
// must describe the brake rather than whatever this tick's action was.
func TestWatcher_MergeBrakeCause_NamesMergeAttempts(t *testing.T) {
	s := &State{MergeAttempts: 4, LastMergeError: "required status check is failing"}
	got := mergeBrakeCause(s)
	assert.Contains(t, got, "4 merge attempts")
	assert.Contains(t, got, "required status check is failing")

	// No recorded error still has to name the brake, not go blank.
	assert.Contains(t, mergeBrakeCause(&State{MergeAttempts: 3}), "3 merge attempts")
}

// The no-progress guard: invocations that never yield a round must stop the run,
// whatever ate them.
//
// This is the backstop for the CLASS that grace_forced is one instance of. Every
// other guard keys off round COMPLETION, so a run whose rounds die before
// returning shows PolishRounds=0 and RoundsSinceImprovement=0 forever and the
// divergence guard cannot fire. kernel#8374 burned three invocations that way.
//
// Uses the stall error, because that is the class the guard is FOR: a plain
// failure already trips the ordinary cooldown after two ticks and never reaches
// here. Only an error that keeps the run alive tick after tick can burn
// invocations indefinitely, which is exactly what kernel#8374 did.
func TestWatcher_Tick_InvocationsWithoutRounds_StopTheRun(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	polish := &stubPolish{err: fmt.Errorf("polish round 1/2: agent execution: " +
		"transient CLI error: stream idle: turn forced complete after grace period " +
		"gated on background tool_use")}
	w := newWatcherForTest(t, gh, polish)
	statePath := StatePath("r", 42)

	pre := &State{LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1"}
	require.NoError(t, pre.Save(statePath))

	// Cooldown disabled so this test isolates the no-progress guard: otherwise the
	// run pauses on the stall brake and never reaches enough invocations.
	w.cfg.Backoff.Cooldown = 0

	var last LastAction
	for range 6 {
		res, err := w.Tick(context.Background())
		require.NoError(t, err)
		last = res.Action
	}

	s, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, LastActionNeedsHuman, last,
		"invocations that never produce a round must stop the run")
	assert.Equal(t, 0, s.PolishRounds,
		"premise: no round ever completed, so the round-keyed guards saw nothing")
}

// A provider outage must NOT trip the no-progress guard, or D2 quietly
// re-introduces the kernel#8031 over-braking that the transient exemption
// exists to prevent. This is the pairing assertion for the counter.
func TestWatcher_Tick_ProviderOutage_DoesNotTripNoProgressGuard(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	polish := &stubPolish{err: fmt.Errorf(
		"polish round 1/3: agent execution: API Error: 529 Overloaded")}
	w := newWatcherForTest(t, gh, polish)
	statePath := StatePath("r", 42)

	pre := &State{LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1"}
	require.NoError(t, pre.Save(statePath))

	for range 4 {
		_, err := w.Tick(context.Background())
		require.NoError(t, err)
	}

	s, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 0, s.InvocationsSinceRound,
		"a downed provider recovers on its own; it must not count as no-progress")
	assert.Equal(t, LastActionTransient, s.LastAction)
	assert.Len(t, polish.calls, 4, "nothing should be braking a pure outage")
}

// An invocation that COMPLETED rounds is not a no-progress invocation, even
// though it returned an error.
//
// Rounds execute in order and stop at the first error, so a multi-round spec can
// finish round 1 and fail round 2 — which is why PolishResult.RanOnceRounds
// exists and why decideAndAct banks it before the error check. Keying the brake
// on `err != nil` alone counts those productive invocations as barren, and a run
// doing real work on every single tick gets halted for making no progress.
func TestWatcher_Tick_PartiallyCompletedRounds_DoNotCountAsNoProgress(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	// A later round failed, but an earlier once-round finished first.
	polish := &stubPolish{
		err:     fmt.Errorf("polish round 2/2: agent execution: exit status 1"),
		ranOnce: []string{"simplify-branch"},
	}
	w := newWatcherForTest(t, gh, polish)
	statePath := StatePath("r", 42)

	pre := &State{LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1"}
	require.NoError(t, pre.Save(statePath))

	// Well past the limit, had every one of these counted.
	for range 2*w.cfg.Backoff.MaxConsecutiveFailures + 2 {
		res, err := w.Tick(context.Background())
		require.NoError(t, err)
		require.False(t, res.Stalled,
			"an invocation that finished a round is progress, whatever the error says")
	}

	s, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 0, s.InvocationsSinceRound,
		"completed work resets the streak, exactly as a fully returned round does")
	assert.True(t, s.OnceRoundsDone["simplify-branch"],
		"premise: the round really did complete and was banked")
}

// The same rule for REPEATABLE rounds, which is the larger case: most specs have
// no `once: true` rounds at all.
//
// RanOnceRounds cannot answer "did this invocation accomplish anything" — it
// records only once rounds, so an ordinary two-round spec whose first round
// succeeds and second fails leaves it empty while real work was done. Keying the
// reset on it would still false-stop exactly the runs this brake is least
// entitled to touch. CompletedRounds counts every kind, which is why the reset
// uses it.
func TestWatcher_Tick_CompletedRepeatableRounds_DoNotCountAsNoProgress(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	// Round 1 of 2 finished; round 2 failed. No once rounds anywhere in the spec,
	// so RanOnceRounds is empty and only CompletedRounds carries the progress.
	polish := &stubPolish{
		err:             fmt.Errorf("polish round 2/2: agent execution: exit status 1"),
		completedRounds: 1,
	}
	w := newWatcherForTest(t, gh, polish)
	statePath := StatePath("r", 42)

	pre := &State{LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1"}
	require.NoError(t, pre.Save(statePath))

	for range 2*w.cfg.Backoff.MaxConsecutiveFailures + 2 {
		res, err := w.Tick(context.Background())
		require.NoError(t, err)
		require.False(t, res.Stalled,
			"a finished repeatable round is progress just as a once round is")
	}

	s, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 0, s.InvocationsSinceRound,
		"completed work resets the streak whatever the round's kind")
	assert.Empty(t, s.OnceRoundsDone,
		"premise: no once rounds ran, so RanOnceRounds could not have carried this")
}

// The counter must RESET when a round returns, or it climbs monotonically across
// a long healthy run and eventually stops a PR that is working fine.
func TestWatcher_Tick_CompletedRound_ResetsNoProgressCounter(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	polish := &stubPolish{err: fmt.Errorf("boom: round died before returning")}
	w := newWatcherForTest(t, gh, polish)
	statePath := StatePath("r", 42)

	pre := &State{LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1"}
	require.NoError(t, pre.Save(statePath))

	_, err := w.Tick(context.Background())
	require.NoError(t, err)
	s, err := LoadState(statePath)
	require.NoError(t, err)
	require.Equal(t, 1, s.InvocationsSinceRound, "premise: the streak started")

	// Now let the round return.
	polish.err = nil
	_, err = w.Tick(context.Background())
	require.NoError(t, err)

	s, err = LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 0, s.InvocationsSinceRound,
		"a returned round ends the no-progress streak")
}

// A stall that arms the cooldown must name the grace-period error.
//
// cooldownCause reads lastStallError for LastActionStalled, but that field was
// only set by the no-progress guard, which fires at TWICE the brake limit. A
// stall reaches the cooldown well before that, through classifyAgentFailure, so
// the branch was unreachable on the path that actually arms it and every
// stall-armed cooldown persisted the generic fallback.
//
// The merge-rework half of the same invariant is covered by
// TestWatcher_ReworkStallArmedCooldown_NamesTheGraceError.
func TestWatcher_StallArmedCooldown_NamesTheGraceError(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	polish := &stubPolish{err: fmt.Errorf("polish round 1/2: agent execution: " +
		"transient CLI error: stream idle: turn forced complete after grace period " +
		"gated on background tool_use")}
	w := newWatcherForTest(t, gh, polish)
	statePath := StatePath("r", 42)

	pre := &State{LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1"}
	require.NoError(t, pre.Save(statePath))

	// Enough stalls to arm the ordinary cooldown, but below the no-progress
	// guard's 2x threshold — the window where the unreachable branch mattered.
	for range 3 {
		_, err := w.Tick(context.Background())
		require.NoError(t, err)
	}

	s, err := LoadState(statePath)
	require.NoError(t, err)
	require.False(t, s.CooldownUntil.IsZero(), "premise: repeated stalls must arm the cooldown")
	assert.Contains(t, s.LastCooldownCause, "grace period",
		"a stall-armed cooldown must name the grace-period error, not the generic fallback")
}

// A tick whose ONLY change is InvocationsSinceRound must still persist it.
//
// recordSnapshot writes state only when something marked it dirty, so a counter
// that decideAndAct owns but the dirty check does not consult is silently
// dropped: the next tick reloads the old value and the no-progress streak never
// climbs, so the guard that stops a stuck run never fires.
//
// Reaching a counter-only tick takes a specific shape, which is why the check
// went in defensively. Ordinary failing ticks also move ConsecutiveFailures, and
// the first guard trip also transitions LastAction to needs_human — both dirty
// on their own. It is the SECOND guard trip that changes nothing else: the
// action is already needs_human, and needs_human is a terminal arm that already
// zeroed ConsecutiveFailures on its way out. The cooldown is disabled here
// because at the default hour it would skip the ticks before the streak can
// reach the guard's 2x threshold.
func TestWatcher_Tick_CounterOnlyChange_IsPersisted(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	// A PLAIN failure: classifyAgentFailure declines it, so it counts toward the
	// streak without taking the stall or transient arm.
	polish := &stubPolish{err: errors.New("polish round 1/2: exit status 1")}
	w := newWatcherForTest(t, gh, polish)
	w.cfg.Backoff.Cooldown = 0
	statePath := StatePath("r", 42)

	pre := &State{LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1"}
	require.NoError(t, pre.Save(statePath))

	// newWatcherForTest sets MaxConsecutiveFailures=2, so the guard's threshold
	// is 4. These four ticks all dirty something else and are only setup.
	for range 4 {
		_, err := w.Tick(context.Background())
		require.NoError(t, err)
	}
	before, err := LoadState(statePath)
	require.NoError(t, err)
	require.Equal(t, LastActionNeedsHuman, before.LastAction,
		"premise: the streak guard must have fired, so the next tick repeats its action")
	require.Equal(t, 0, before.ConsecutiveFailures,
		"premise: needs_human is a terminal arm, so the failure counter is already zeroed")
	require.True(t, before.CooldownUntil.IsZero(), "premise: no cooldown to clear")
	require.Equal(t, 4, before.InvocationsSinceRound)

	// The counter-only tick: same action, same counters, same head/base, no new
	// comments or runs. Nothing but InvocationsSinceRound moves.
	_, err = w.Tick(context.Background())
	require.NoError(t, err)

	after, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 5, after.InvocationsSinceRound,
		"a tick whose only change is the no-progress counter must still be saved")
}

// The scope brake must stop a run whose rounds all SUCCEED.
//
// This is the kernel#8374 shape and the reason it needs its own guard: every
// liveness check reads that run as healthy. Rounds do not error, so
// ConsecutiveFailures stays 0. Each round closes the findings the previous one
// drew, so BestHealth keeps being beaten and RoundsSinceImprovement resets to 0
// — it sat at zero across eight consecutive ticks. Rounds return, so
// InvocationsSinceRound stays 0. Meanwhile the PR grew from 6 files/+407 to 13
// files/+2509 over 23 commits and five days, and nothing stopped it.
func TestWatcher_ScopeRatchet_StopsAHealthyButGrowingRun(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	w := newWatcherForTest(t, gh, &stubPolish{})
	w.cfg.Backoff.MaxPolishCommits = 3
	statePath := StatePath("r", 42)

	// Deliberately the picture of health by every OTHER guard: no failures, no
	// stalls, and a fresh improvement streak. Only the commit count is high.
	pre := &State{
		LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1",
		PolishCommits: 3, ConsecutiveFailures: 0, RoundsSinceImprovement: 0,
		InvocationsSinceRound: 0,
	}
	require.NoError(t, pre.Save(statePath))

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionNeedsHuman, res.Action,
		"a run that keeps succeeding while the PR keeps growing must still stop")
}

// Below the limit the guard must stay out of the way, or it becomes a round cap
// wearing a different name.
func TestWatcher_ScopeRatchet_SilentBelowTheLimit(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	w := newWatcherForTest(t, gh, &stubPolish{})
	w.cfg.Backoff.MaxPolishCommits = 12
	statePath := StatePath("r", 42)

	pre := &State{
		LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1",
		PolishCommits: 11,
	}
	require.NoError(t, pre.Save(statePath))

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.NotEqual(t, LastActionNeedsHuman, res.Action,
		"one commit below the limit is still ordinary work")
}

// Zero disables the guard, so an operator can opt a repo out without patching.
func TestWatcher_ScopeRatchet_ZeroDisables(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	w := newWatcherForTest(t, gh, &stubPolish{})
	w.cfg.Backoff.MaxPolishCommits = 0
	statePath := StatePath("r", 42)

	pre := &State{
		LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1",
		PolishCommits: 500,
	}
	require.NoError(t, pre.Save(statePath))

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.NotEqual(t, LastActionNeedsHuman, res.Action, "zero must disable the guard")
}

// At the limit, a tick that would only ORIGINATE a review must still run it.
//
// The brake counts commits, and a review-only tick produces none — there is
// nothing for it to measure. On a self_review repo stopping here is the worse
// of the two failures: prdozer is the only reviewer, so the run hands back a
// green PR whose last commit nobody has read, and auto-merge is gated on
// needsSelfReview so the PR cannot land either.
func TestWatcher_ScopeRatchet_LetsTheOwedReviewRun(t *testing.T) {
	prJSON := strings.Replace(okPRJSON, `"reviewDecision": "REVIEW_REQUIRED"`, `"reviewDecision": ""`, 1)
	gh := setupGH(buildPRJSON(prJSON, "SUCCESS"), "[]", "base1")
	gh.setThreads(0)
	gh.addPrefix("api repos/o/r/pulls/42 --jq .merged", "false")
	polish := &stubPolish{}
	w := newWatcherForTest(t, gh, polish)
	w.cfg.Polish.SelfReview = true
	w.cfg.Backoff.MaxPolishCommits = 3
	statePath := StatePath("r", 42)

	// At the limit, with nothing to polish: same head, same base, no failures.
	pre := &State{
		LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1",
		PolishCommits: 3,
	}
	require.NoError(t, pre.Save(statePath))

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionPolished, res.Action,
		"the scope brake counts commits; a review-only tick adds none and still owes a review")
	assert.Len(t, polish.calls, 1, "the owed review must actually run")
	assert.False(t, res.Ratcheted, "nothing was stopped, so nothing may report a scope stop")
}

// The allowance is bounded, and the bound is load-bearing.
//
// Every polish round pushes a commit, which moves the head, which makes
// needsSelfReview true again — so an unconditional review exemption would mean
// the brake can never fire on a self_review repo at all, restoring the
// unbounded run it exists to prevent. Past 2*limit the commit count stands on
// its own however the head looks.
func TestWatcher_ScopeRatchet_ReviewAllowanceExpires(t *testing.T) {
	prJSON := strings.Replace(okPRJSON, `"reviewDecision": "REVIEW_REQUIRED"`, `"reviewDecision": ""`, 1)
	gh := setupGH(buildPRJSON(prJSON, "SUCCESS"), "[]", "base1")
	gh.setThreads(0)
	gh.addPrefix("api repos/o/r/pulls/42 --jq .merged", "false")
	polish := &stubPolish{}
	w := newWatcherForTest(t, gh, polish)
	w.cfg.Polish.SelfReview = true
	w.cfg.Backoff.MaxPolishCommits = 3
	statePath := StatePath("r", 42)

	// Identical to the test above but at twice the limit: same unreviewed head,
	// same clean PR, so only the count can be what decides.
	pre := &State{
		LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1",
		PolishCommits: 6,
	}
	require.NoError(t, pre.Save(statePath))

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionNeedsHuman, res.Action,
		"a second allowance is all a run gets; past 2*limit the count stands on its own")
	assert.True(t, res.Ratcheted, "and the stop must still report itself as a scope stop")
	assert.Empty(t, polish.calls, "the run is over — no round may start after the brake fires")
}

// The allowance covers the review, not the polishing. A tick with real work
// queued is exactly what the brake exists to stop, unreviewed head or not.
func TestWatcher_ScopeRatchet_StopsWorkOnAnUnreviewedHead(t *testing.T) {
	// CI is failing, so NeedsPolish() is true: this tick would push commits.
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	gh.setThreads(0)
	polish := &stubPolish{}
	w := newWatcherForTest(t, gh, polish)
	w.cfg.Polish.SelfReview = true
	w.cfg.Backoff.MaxPolishCommits = 3
	statePath := StatePath("r", 42)

	pre := &State{
		LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1",
		PolishCommits: 3,
	}
	require.NoError(t, pre.Save(statePath))

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionNeedsHuman, res.Action,
		"an unreviewed head must not buy a commit-producing round past the limit")
	assert.True(t, res.Ratcheted)
	assert.Empty(t, polish.calls, "the brake fired, so no round may run")
}

// The scope guard is only as good as the pushes it manages to count, so the
// counter has its own tests — the three above pre-seed PolishCommits and never
// exercise the code that produces it.
//
// scopeSnapshot is a snapshot with the fields the counter reads, and nothing
// else: head, commit list, diff size.
func scopeSnapshot(head string, commits, additions int) *Snapshot {
	rows := make([]commitRow, commits)
	for i := range rows {
		rows[i] = commitRow{OID: fmt.Sprintf("c%d", i)}
	}
	return &Snapshot{PR: PRDetails{HeadRefOid: head, Commits: rows, Additions: additions}}
}

// A push is charged for every commit it carried, not for the fact of the push.
//
// This is the difference between a cap that works and one that never fires: a
// polish invocation commits once per round and force-pushes ONCE at the end, so
// per-push counting reads a 5-round invocation as a single commit. A limit of
// 12 would then sit around 60 commits deep — three times the 23 that made the
// guard necessary in the first place.
func TestRecordHealth_ChargesEveryCommitInAPush(t *testing.T) {
	t.Parallel()
	w := NewWatcher(DefaultConfig(), nil, nil, 42, ".", "r", nil)
	state := &State{
		LastAction: LastActionPolished, LastSeenHeadSHA: "head1",
		LastSeenCommitCount: 4,
	}

	w.recordHealth(state, scopeSnapshot("head2", 9, 100))

	assert.Equal(t, 5, state.PolishCommits,
		"a push that added five commits costs five, not one")
	assert.Equal(t, 1, state.PolishRounds)
}

// A tick whose health is unreadable must still charge the push.
//
// Health() reports !ok whenever the unresolved-thread lookup failed, and
// recordSnapshot advances LastSeenHeadSHA and LastSeenCommitCount at the end of
// the tick regardless — so a push charged behind the health gate is not merely
// deferred, it is lost, and the run gets those commits for free.
func TestRecordHealth_ChargesPushWhenHealthIsUnreadable(t *testing.T) {
	t.Parallel()
	w := NewWatcher(DefaultConfig(), nil, nil, 42, ".", "r", nil)
	snap := scopeSnapshot("head2", 6, 100)
	snap.UnresolvedThreads = -1 // the thread query failed
	_, ok := snap.Health()
	require.False(t, ok, "guarding the premise: this snapshot must be unreadable")

	state := &State{
		LastAction: LastActionPolished, LastSeenHeadSHA: "head1",
		LastSeenCommitCount: 2,
	}

	dirty := w.recordHealth(state, snap)

	assert.Equal(t, 4, state.PolishCommits,
		"a failed side query does not un-happen the round that pushed")
	assert.True(t, dirty, "the new count must be persisted, or the next tick reloads without it")
}

// A round that only replied to comments has not grown the PR, and the scope
// brake is about growth.
func TestRecordHealth_UnmovedHeadCostsNothing(t *testing.T) {
	t.Parallel()
	w := NewWatcher(DefaultConfig(), nil, nil, 42, ".", "r", nil)
	state := &State{
		LastAction: LastActionPolished, LastSeenHeadSHA: "head1",
		LastSeenCommitCount: 4,
	}

	w.recordHealth(state, scopeSnapshot("head1", 4, 100))

	assert.Zero(t, state.PolishCommits, "no push, no growth, no charge")
	assert.Equal(t, 1, state.PolishRounds, "the round still ran")
}

// A rebase or squash can leave the branch no longer than it was. The head moved,
// so a round's worth of growth happened; charging zero would hand a squashing
// run an unlimited budget. Same floor covers a gh payload with no commit list
// and state written before the field existed.
func TestRecordHealth_ShrinkingPushStillCostsOne(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		commits, seen int
	}{
		{"squashed", 2, 7},
		{"no commit list from gh", 0, 7},
		{"state predates the field", 9, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := NewWatcher(DefaultConfig(), nil, nil, 42, ".", "r", nil)
			state := &State{
				LastAction: LastActionPolished, LastSeenHeadSHA: "head1",
				LastSeenCommitCount: tc.seen,
			}
			w.recordHealth(state, scopeSnapshot("head2", tc.commits, 100))
			assert.Equal(t, 1, state.PolishCommits,
				"an uncountable push still costs the floor of one")
		})
	}
}

// Only prdozer's own rounds are charged: a human pushing to the branch between
// ticks must not spend the budget prdozer is being held to.
func TestRecordHealth_OnlyPolishedTicksAreCharged(t *testing.T) {
	t.Parallel()
	w := NewWatcher(DefaultConfig(), nil, nil, 42, ".", "r", nil)
	state := &State{
		LastAction: LastActionIdle, LastSeenHeadSHA: "head1",
		LastSeenCommitCount: 4,
	}

	w.recordHealth(state, scopeSnapshot("head2", 9, 100))

	assert.Zero(t, state.PolishCommits, "prdozer did not push this")
	assert.Zero(t, state.PolishRounds)
}

// The counter must survive a full tick: recordHealth charges the push, and
// recordSnapshot then advances the baselines it charged against, so the next
// tick starts from the new head and count rather than double-charging.
func TestWatcher_Tick_ScopeCounterAdvancesAcrossTicks(t *testing.T) {
	pushed := buildPRJSON(`{
  "number": 42,
  "url": "https://github.com/o/r/pull/42",
  "headRefName": "feature",
  "baseRefName": "main",
  "headRefOid": "head2",
  "state": "OPEN",
  "isDraft": false,
  "reviewDecision": "REVIEW_REQUIRED",
  "mergeable": "MERGEABLE",
  "additions": 900,
  "commits": [{"oid":"c1"},{"oid":"c2"},{"oid":"c3"},{"oid":"c4"},{"oid":"c5"}]
}`, "FAILURE")
	gh := setupGH(pushed, "[]", "base1")
	w := newWatcherForTest(t, gh, &stubPolish{})
	statePath := StatePath("r", 42)

	// The previous tick polished and the round pushed three commits onto the
	// two the PR already had.
	pre := &State{
		LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1",
		LastSeenCommitCount: 2, LastAction: LastActionPolished,
	}
	require.NoError(t, pre.Save(statePath))

	_, err := w.Tick(context.Background())
	require.NoError(t, err)

	after, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 3, after.PolishCommits, "three commits arrived on that push")
	assert.Equal(t, 5, after.LastSeenCommitCount, "the baseline moves to what was just seen")
	assert.Equal(t, "head2", after.LastSeenHeadSHA)

	// A second tick with nothing new must not re-charge the same push.
	_, err = w.Tick(context.Background())
	require.NoError(t, err)
	after, err = LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 3, after.PolishCommits, "the same push must not be charged twice")
}

// `gh pr view --json commits` runs an unpaginated `commits(first:100)`, so on a
// PR already 100 commits deep the list length is a cap, not a count. Left
// unfixed, that pins LastSeenCommitCount at 100 forever: every later push looks
// like zero growth and is charged the floor of one, so a limit of 12 tolerates
// an unbounded number of agent commits — the brake under-counting, the one
// direction it is explicitly built not to fail in.
func TestCommitCount_ExactCountBeatsTheTruncatedList(t *testing.T) {
	t.Parallel()
	saturated := make([]commitRow, 100)

	assert.Equal(t, 137, PRDetails{Commits: saturated, TotalCommits: 137}.CommitCount(),
		"the GraphQL scalar is the real count; the list is capped at 100")
	assert.Zero(t, PRDetails{Commits: saturated}.CommitCount(),
		"a saturated list is unknown, not 100 — reporting the cap as a count "+
			"poisons the baseline and fakes growth on the next tick")
	assert.Equal(t, 99, PRDetails{Commits: saturated[:99]}.CommitCount(),
		"under the cap the list is exact, so it is still the fallback")
	assert.Zero(t, PRDetails{}.CommitCount(),
		"no list and no count is unknown, and the caller floors it")
}

// The end-to-end version of the above: a real tick on a 100-commit PR must
// charge the push its true delta, which means the count has to come from
// GraphQL and survive all the way into PolishCommits.
func TestWatcher_ChargesRealDeltaOnAPRPastTheCommitListCap(t *testing.T) {
	// The list gh serves is truncated at 100 whatever the PR really carries.
	rows := make([]string, 100)
	for i := range rows {
		rows[i] = fmt.Sprintf(`{"oid":"c%d"}`, i)
	}
	pushed := buildPRJSON(fmt.Sprintf(`{
  "number": 42,
  "url": "https://github.com/o/r/pull/42",
  "headRefName": "feature",
  "baseRefName": "main",
  "headRefOid": "head2",
  "state": "OPEN",
  "isDraft": false,
  "reviewDecision": "REVIEW_REQUIRED",
  "mergeable": "MERGEABLE",
  "additions": 900,
  "commits": [%s]
}`, strings.Join(rows, ",")), "FAILURE")
	gh := setupGH(pushed, "[]", "base1")
	// GraphQL reports the count as a scalar, so it is immune to the cap.
	gh.addContains("commits{ totalCount }", "137\n")
	w := newWatcherForTest(t, gh, &stubPolish{})
	statePath := StatePath("r", 42)

	// Last tick polished and saw 130 commits; the round pushed seven more.
	pre := &State{
		LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1",
		LastSeenCommitCount: 130, LastAction: LastActionPolished,
	}
	require.NoError(t, pre.Save(statePath))

	_, err := w.Tick(context.Background())
	require.NoError(t, err)

	after, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 7, after.PolishCommits,
		"seven commits arrived; the truncated list would have charged the floor of one")
	assert.Equal(t, 137, after.LastSeenCommitCount,
		"the baseline is the exact count, not the 100-entry cap")
}

// When the exact query fails there is nothing better than the truncated list,
// and falling back to it must keep the pre-existing behaviour rather than
// charging zero — a snapshot that reads as "no commits" would hand a growing PR
// a free tick.
func TestWatcher_FallsBackToTheCommitListWhenTheExactCountFails(t *testing.T) {
	pushed := buildPRJSON(`{
  "number": 42,
  "url": "https://github.com/o/r/pull/42",
  "headRefName": "feature",
  "baseRefName": "main",
  "headRefOid": "head2",
  "state": "OPEN",
  "isDraft": false,
  "reviewDecision": "REVIEW_REQUIRED",
  "mergeable": "MERGEABLE",
  "additions": 900,
  "commits": [{"oid":"c1"},{"oid":"c2"},{"oid":"c3"},{"oid":"c4"},{"oid":"c5"}]
}`, "FAILURE")
	gh := setupGH(pushed, "[]", "base1")
	gh.failPrefix("api graphql -f query=query($owner:String!,$repo:String!,$pr:Int!){", "gone")
	w := newWatcherForTest(t, gh, &stubPolish{})
	statePath := StatePath("r", 42)

	pre := &State{
		LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1",
		LastSeenCommitCount: 2, LastAction: LastActionPolished,
	}
	require.NoError(t, pre.Save(statePath))

	_, err := w.Tick(context.Background())
	require.NoError(t, err)

	after, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 3, after.PolishCommits, "the list still counts when the scalar is unavailable")
	assert.Equal(t, 5, after.LastSeenCommitCount)
}

// A transient GraphQL failure on a PR past the cap must not poison the
// baseline. If the truncated 100 were persisted over a real 137, the next
// successful tick would read 37 commits of growth that never happened and slam
// a limit of 12 shut on a run that pushed two commits.
func TestWatcher_TransientCountFailureDoesNotPoisonTheBaseline(t *testing.T) {
	rows := make([]string, 100)
	for i := range rows {
		rows[i] = fmt.Sprintf(`{"oid":"c%d"}`, i)
	}
	prJSON := func(head string) string {
		return buildPRJSON(fmt.Sprintf(`{
  "number": 42,
  "url": "https://github.com/o/r/pull/42",
  "headRefName": "feature",
  "baseRefName": "main",
  "headRefOid": %q,
  "state": "OPEN",
  "isDraft": false,
  "reviewDecision": "REVIEW_REQUIRED",
  "mergeable": "MERGEABLE",
  "additions": 900,
  "commits": [%s]
}`, head, strings.Join(rows, ",")), "FAILURE")
	}
	const countQuery = "api graphql -f query=query($owner:String!,$repo:String!,$pr:Int!){"

	gh := setupGH(prJSON("head2"), "[]", "base1")
	gh.failPrefix(countQuery, "gone")
	w := newWatcherForTest(t, gh, &stubPolish{})
	statePath := StatePath("r", 42)

	// The PR really carries 137 commits and the last tick knew it.
	pre := &State{
		LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1",
		LastSeenCommitCount: 137, LastAction: LastActionPolished,
	}
	require.NoError(t, pre.Save(statePath))

	// Tick one: a push landed, but the exact count is unavailable.
	_, err := w.Tick(context.Background())
	require.NoError(t, err)
	after, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 1, after.PolishCommits, "an unknown count charges the floor of one")
	assert.Equal(t, 137, after.LastSeenCommitCount,
		"the truncated 100 must not overwrite the last authoritative count")

	// Tick two: GraphQL recovers and another two commits have landed.
	gh.recoverPrefix(countQuery)
	gh.addContains("commits{ totalCount }", "139\n")
	// Same key setupGH registered, so this REPLACES the response rather than
	// racing it: two prefixes both matching would resolve by map order.
	gh.addPrefix(prViewPrefix, prJSON("head3"))

	_, err = w.Tick(context.Background())
	require.NoError(t, err)
	after, err = LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 3, after.PolishCommits,
		"one floored tick plus the two commits actually pushed — not 39")
	assert.Equal(t, 139, after.LastSeenCommitCount)
}

// The stop must reach the operator as a SCOPE stop. The guard sets w.ratcheted
// precisely because "needs a human" otherwise reads as "waiting on an approval",
// and a flag nothing carries out of the tick cannot do that.
func TestWatcher_ScopeRatchet_ReportsItselfOnTheResult(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	w := newWatcherForTest(t, gh, &stubPolish{})
	w.cfg.Backoff.MaxPolishCommits = 3
	statePath := StatePath("r", 42)

	pre := &State{
		LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1",
		PolishCommits: 4,
	}
	require.NoError(t, pre.Save(statePath))

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	require.Equal(t, LastActionNeedsHuman, res.Action)
	assert.True(t, res.Ratcheted,
		"without this the terminal report calls a scope stop an approval block")
	assert.Equal(t, 4, res.PolishCommits)
	assert.Equal(t, 3, res.PolishCommitLimit)
	assert.False(t, res.Diverged, "the scope guard fired, not the divergence guard")
}

// The scope guard must not block a PR that is already DONE.
//
// It sits below the mergeable branches in decideAndAct, which reads like a
// bypass and was flagged as one. It is deliberate: the guard exists to stop
// FURTHER polishing, and a mergeable PR is APPROVED with checks green — a human
// has already looked at the scope and said yes. Hoisting the guard above the
// merge would strand exactly the PRs that grew large and then got reviewed
// anyway, leaving them needing a human for a decision a human just made.
func TestWatcher_ScopeRatchet_DoesNotBlockAnApprovedGreenPR(t *testing.T) {
	gh := approvedGreenGH("true")
	w := newWatcherForTest(t, gh, &stubPolish{})
	w.cfg.Polish.AutoMerge = true
	w.cfg.Polish.MergePolicy = MergePolicySquash
	w.cfg.Backoff.MaxPolishCommits = 3
	statePath := StatePath("r", 42)

	pre := &State{
		LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1",
		PolishCommits: 99, // far past the cap
	}
	require.NoError(t, pre.Save(statePath))

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionMerged, res.Action,
		"an approved, green PR is finished; the scope brake stops polishing, not merging")
	assert.False(t, res.Ratcheted)
}

// Merge rework grows the PR too, and is the easier one to forget.
//
// It runs an agent against the branch to rebase or resolve conflicts against a
// failed merge, pushing commits exactly like a polish round does. Charging only
// LastActionPolished lets a run that keeps failing its merge ratchet for free —
// the same unbounded growth the guard exists to stop, reached by the other door.
func TestRecordHealth_ChargesMergeReworkPushes(t *testing.T) {
	t.Parallel()
	w := NewWatcher(DefaultConfig(), nil, nil, 42, ".", "r", nil)
	state := &State{
		LastAction: LastActionReworked, LastSeenHeadSHA: "head1",
		LastSeenCommitCount: 4,
	}

	w.recordHealth(state, scopeSnapshot("head2", 6, 100))

	assert.Equal(t, 2, state.PolishCommits, "a rework that pushed twice costs two")
	assert.Zero(t, state.PolishRounds, "a rework is not a polish round")
}

// An invocation that pushes and THEN fails must still be billed.
//
// Both runners preserve the rounds that succeeded and return the later round's
// error, so a spec that pushed three commits before dying lands in state as
// LastActionFailed — or Stalled, or Transient. Charging only the clean-exit
// actions hands those pushes to the run for free, and a run that fails its last
// round every time would ratchet without ever tripping the cap.
func TestRecordHealth_ChargesPushesThatEndedInFailure(t *testing.T) {
	t.Parallel()
	for _, action := range []LastAction{LastActionFailed, LastActionStalled, LastActionTransient} {
		t.Run(string(action), func(t *testing.T) {
			t.Parallel()
			w := NewWatcher(DefaultConfig(), nil, nil, 42, ".", "r", nil)
			state := &State{
				LastAction: action, LastSeenHeadSHA: "head1", LastSeenCommitCount: 4,
			}

			w.recordHealth(state, scopeSnapshot("head2", 7, 100))

			assert.Equal(t, 3, state.PolishCommits,
				"the rounds that landed before the error still grew the PR")
		})
	}
}

// The other side of the same rule: ticks where no agent ran are never charged,
// so a human pushing to an idle PR does not spend prdozer's budget.
func TestRecordHealth_TicksWithoutAnAgentAreNeverCharged(t *testing.T) {
	t.Parallel()
	for _, action := range []LastAction{
		LastActionInit, LastActionIdle, LastActionMerged, LastActionClosed,
		LastActionArmed, LastActionNeedsHuman, LastActionDryRun,
	} {
		name := string(action)
		if name == "" {
			name = "init"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := NewWatcher(DefaultConfig(), nil, nil, 42, ".", "r", nil)
			state := &State{
				LastAction: action, LastSeenHeadSHA: "head1", LastSeenCommitCount: 4,
			}

			w.recordHealth(state, scopeSnapshot("head2", 9, 100))

			assert.Zero(t, state.PolishCommits, "prdozer ran nothing; this push is not its growth")
		})
	}
}
