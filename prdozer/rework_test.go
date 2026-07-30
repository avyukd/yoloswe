package prdozer

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubRework records calls and returns a configurable error, mirroring
// stubPolish.
type stubRework struct {
	err   error
	calls []ReworkRequest
	mu    sync.Mutex
}

func (s *stubRework) Run(_ context.Context, req ReworkRequest) (ReworkResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)
	if s.err != nil {
		return ReworkResult{}, s.err
	}
	return ReworkResult{Rounds: len(req.Spec.Rounds)}, nil
}

func (s *stubRework) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// oneRoundSpec is a minimal non-empty rework spec.
func oneRoundSpec() StepSpec {
	return StepSpec{Rounds: []RoundSpec{{Prompt: "fix PR #{{.PRNumber}}: {{.MergeError}}"}}}
}

// failingMergeGH serves a mergeable PR whose merge command fails.
func failingMergeGH(t *testing.T) *fakeGH {
	t.Helper()
	gh := approvedGreenGH("false")
	gh.failPrefix("pr merge", "Pull request is in an unmergeable state")
	return gh
}

func TestWatcher_FailedMerge_RoutesToRework(t *testing.T) {
	gh := failingMergeGH(t)
	rework := &stubRework{}
	w := newWatcherForTest(t, gh, &stubPolish{}, WithRework(rework, oneRoundSpec()))
	w.cfg.Polish.AutoMerge = true
	w.cfg.Polish.MergePolicy = MergePolicySquash

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionReworked, res.Action)
	require.Equal(t, 1, rework.callCount())

	got := rework.calls[0]
	assert.Equal(t, 42, got.PRNumber)
	assert.Equal(t, 1, got.Attempt)
	assert.Contains(t, got.MergeError, "unmergeable",
		"the verbatim gh error must reach the agent; a summary loses the diagnosis")
}

func TestWatcher_FailedMerge_NotifyPolicyGoesToNeedsHuman(t *testing.T) {
	// merge_policy notify never merges in the first place, so it can never
	// reach rework — it stops for a human.
	gh := failingMergeGH(t)
	rework := &stubRework{}
	w := newWatcherForTest(t, gh, &stubPolish{}, WithRework(rework, oneRoundSpec()))
	w.cfg.Polish.AutoMerge = true
	w.cfg.Polish.MergePolicy = MergePolicyNotify

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionNeedsHuman, res.Action)
	assert.Zero(t, rework.callCount(), "notify policy must not run rework rounds")
}

func TestReworkAfterFailedMerge_DecisionTree(t *testing.T) {
	cases := []struct {
		rework         ReworkRunner
		name           string
		policy         MergePolicy
		reviewDecision string
		want           LastAction
		spec           StepSpec
	}{
		{
			name:           "notify policy stops for a human",
			policy:         MergePolicyNotify,
			reviewDecision: "APPROVED",
			spec:           oneRoundSpec(),
			rework:         &stubRework{},
			want:           LastActionNeedsHuman,
		},
		{
			name:           "missing approval stops for a human",
			policy:         MergePolicySquash,
			reviewDecision: "REVIEW_REQUIRED",
			spec:           oneRoundSpec(),
			rework:         &stubRework{},
			want:           LastActionNeedsHuman,
		},
		{
			name:           "no rounds configured is a plain failure",
			policy:         MergePolicySquash,
			reviewDecision: "APPROVED",
			spec:           StepSpec{},
			rework:         &stubRework{},
			want:           LastActionFailed,
		},
		{
			name:           "no runner configured is a plain failure",
			policy:         MergePolicySquash,
			reviewDecision: "APPROVED",
			spec:           oneRoundSpec(),
			rework:         nil,
			want:           LastActionFailed,
		},
		{
			name:           "a failing rework round is a plain failure",
			policy:         MergePolicySquash,
			reviewDecision: "APPROVED",
			spec:           oneRoundSpec(),
			rework:         &stubRework{err: fmt.Errorf("agent died")},
			want:           LastActionFailed,
		},
		{
			name:           "otherwise rework runs",
			policy:         MergePolicySquash,
			reviewDecision: "APPROVED",
			spec:           oneRoundSpec(),
			rework:         &stubRework{},
			want:           LastActionReworked,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWatcherForTest(t, newFakeGH(), &stubPolish{}, WithRework(tc.rework, tc.spec))
			w.cfg.Polish.MergePolicy = tc.policy
			snap := &Snapshot{PR: PRDetails{
				Number:         42,
				URL:            "https://github.com/o/r/pull/42",
				HeadRefName:    "feature",
				ReviewDecision: tc.reviewDecision,
			}}
			state := &State{MergeAttempts: 1, LastMergeError: "boom"}

			got := w.reworkAfterFailedMerge(context.Background(), snap, state)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestWatcher_MergeAttempts_IncrementAcrossTicks(t *testing.T) {
	// Attempt numbering must survive across ticks so the notification can
	// honestly report "attempt N" rather than restarting at 1.
	gh := failingMergeGH(t)
	rework := &stubRework{}
	w := newWatcherForTest(t, gh, &stubPolish{}, WithRework(rework, oneRoundSpec()))
	w.cfg.Polish.AutoMerge = true
	w.cfg.Polish.MergePolicy = MergePolicySquash
	// Keep cooldown out of the way for this test.
	w.cfg.Backoff.MaxConsecutiveFailures = 0

	statePath := StatePath("r", 42)
	for i := 1; i <= 3; i++ {
		_, err := w.Tick(context.Background())
		require.NoError(t, err)
		s, err := LoadState(statePath)
		require.NoError(t, err)
		assert.Equal(t, i, s.MergeAttempts, "attempt count must persist across ticks")
		assert.NotEmpty(t, s.LastMergeError, "the merge error must persist for the next rework")
	}
	assert.Equal(t, 3, rework.callCount())
	assert.Equal(t, 3, rework.calls[2].Attempt, "the agent is told which attempt this is")
}

// TestWatcher_RepeatedRework_TripsCooldown is the load-bearing test for the
// unbounded-retry design.
//
// Merge attempts are deliberately unbounded; the ONLY brake is
// backoff.max_consecutive_failures -> cooldown. That brake works only if
// LastActionReworked does not reset ConsecutiveFailures. If rework were treated
// as a success, the counter would clear on every pass, cooldown could never
// trip, and a PR that cannot merge would churn agent rounds forever — burning
// quota silently. Without this test the design has no brake at all.
func TestWatcher_RepeatedRework_TripsCooldown(t *testing.T) {
	gh := failingMergeGH(t)
	rework := &stubRework{}
	w := newWatcherForTest(t, gh, &stubPolish{}, WithRework(rework, oneRoundSpec()))
	w.cfg.Polish.AutoMerge = true
	w.cfg.Polish.MergePolicy = MergePolicySquash
	// newWatcherForTest sets MaxConsecutiveFailures=2, Cooldown=1h.
	statePath := StatePath("r", 42)

	// First rework: counts as a failure, but not yet at the threshold.
	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	require.Equal(t, LastActionReworked, res.Action)
	s1, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 1, s1.ConsecutiveFailures,
		"rework must COUNT as a failure for backoff even though it is not terminal")
	assert.True(t, s1.CooldownUntil.IsZero())

	// Second rework: hits the threshold and trips the cooldown.
	res, err = w.Tick(context.Background())
	require.NoError(t, err)
	require.Equal(t, LastActionReworked, res.Action)
	s2, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 2, s2.ConsecutiveFailures)
	assert.False(t, s2.CooldownUntil.IsZero(),
		"repeated rework MUST trip the cooldown; it is the only brake on unbounded retries")

	// Third tick is skipped entirely, so the loop actually stops churning.
	before := rework.callCount()
	_, err = w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, before, rework.callCount(),
		"a tripped cooldown must stop further rework rounds")
}

// TestWatcher_AlternatingReworkAndPolish_StillTripsCooldown is the real-world
// version of the brake test, and the one that matters.
//
// REGRESSION: the merge-retry loop does not repeat reworks back-to-back — it
// ALTERNATES. Rework rebases and pushes, which moves HEAD and re-triggers CI,
// so the next tick polishes instead. Polish succeeds, which reset
// ConsecutiveFailures to zero, so the counter never passed 1, the cooldown
// never tripped, no Slack warning ever fired, and an unmergeable PR burned
// agent budget forever. Backoff must therefore key off MergeAttempts, which
// nothing resets.
func TestWatcher_AlternatingReworkAndPolish_StillTripsCooldown(t *testing.T) {
	gh := failingMergeGH(t)
	rework := &stubRework{}
	polish := &stubPolish{}
	w := newWatcherForTest(t, gh, polish, WithRework(rework, oneRoundSpec()))
	w.cfg.Polish.AutoMerge = true
	w.cfg.Polish.MergePolicy = MergePolicySquash
	// Threshold of 2 merge attempts (newWatcherForTest sets Cooldown=1h).
	w.cfg.Backoff.MaxConsecutiveFailures = 2
	statePath := StatePath("r", 42)

	// Simulate the alternation directly: a successful polish tick between two
	// merge attempts. Seed a state that looks like "one merge attempt already
	// happened, then a polish succeeded and cleared ConsecutiveFailures".
	require.NoError(t, (&State{
		LastCheckAt:         time.Now(),
		LastSeenHeadSHA:     "head1",
		LastSeenBaseSHA:     "base1",
		MergeAttempts:       1,
		ConsecutiveFailures: 0, // <- what a successful polish leaves behind
		LastAction:          LastActionPolished,
	}).Save(statePath))

	// Next tick attempts the merge again (attempt 2) and reworks.
	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	require.Equal(t, LastActionReworked, res.Action)

	s, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 2, s.MergeAttempts)
	assert.False(t, s.CooldownUntil.IsZero(),
		"the brake must trip on accumulated MERGE ATTEMPTS; a successful polish in "+
			"between must not launder the failure count away")
}

func TestWatcher_MergeBrake_ResumesAfterCooldown(t *testing.T) {
	// Post-cooldown the run must get a fresh allowance rather than re-tripping
	// on every subsequent tick, and MergeAttempts must keep climbing so the
	// notification can honestly say "attempt N".
	gh := failingMergeGH(t)
	w := newWatcherForTest(t, gh, &stubPolish{}, WithRework(&stubRework{}, oneRoundSpec()))
	w.cfg.Polish.AutoMerge = true
	w.cfg.Polish.MergePolicy = MergePolicySquash
	w.cfg.Backoff.MaxConsecutiveFailures = 2
	statePath := StatePath("r", 42)

	// A run that already cooled down once, at attempt 2, now expired.
	require.NoError(t, (&State{
		LastCheckAt:         time.Now().Add(-2 * time.Hour),
		LastSeenHeadSHA:     "head1",
		LastSeenBaseSHA:     "base1",
		MergeAttempts:       2,
		CooldownFromAttempt: 2,
		CooldownUntil:       time.Now().Add(-time.Hour),
	}).Save(statePath))

	// Attempt 3: within the fresh allowance, so no immediate re-trip.
	_, err := w.Tick(context.Background())
	require.NoError(t, err)
	s, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 3, s.MergeAttempts, "attempt numbering keeps climbing across cooldowns")
	assert.True(t, s.CooldownUntil.IsZero() || s.CooldownUntil.Before(time.Now()),
		"one attempt past a cooldown must not immediately re-trip")

	// Attempt 4 reaches the threshold again (2 attempts since the last
	// cooldown), so the run backs off rather than retrying without limit.
	_, err = w.Tick(context.Background())
	require.NoError(t, err)
	s, err = LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 4, s.MergeAttempts)
	assert.False(t, s.CooldownUntil.IsZero(), "the brake re-engages on the next batch of attempts")

	// And the cooldown actually stops further work.
	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionIdle, res.Action, "a tripped cooldown skips the tick")
	s, err = LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 4, s.MergeAttempts, "no further merge attempts while cooling down")
}

func TestWatcher_QueuePolicy_RepeatedDequeueStillBrakes(t *testing.T) {
	// Under "queue", a repeatedly-dequeued PR re-arms cleanly every tick.
	// LastActionArmed is a success, so ConsecutiveFailures alone would never
	// bound it — MergeAttempts must.
	gh := approvedGreenGH("false") // arms fine, never actually merges
	w := newWatcherForTest(t, gh, &stubPolish{}, WithRework(&stubRework{}, oneRoundSpec()))
	w.cfg.Polish.AutoMerge = true
	w.cfg.Polish.MergePolicy = MergePolicyQueue
	w.cfg.Backoff.MaxConsecutiveFailures = 2
	statePath := StatePath("r", 42)

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	require.Equal(t, LastActionArmed, res.Action)
	res, err = w.Tick(context.Background())
	require.NoError(t, err)
	require.Equal(t, LastActionArmed, res.Action)

	s, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 2, s.MergeAttempts)
	assert.False(t, s.CooldownUntil.IsZero(),
		"a PR the queue keeps dequeuing must eventually back off, not re-arm forever")
}

func TestWatcher_SuccessfulMergeClearsMergeError(t *testing.T) {
	gh := approvedGreenGH("true")
	w := newWatcherForTest(t, gh, &stubPolish{}, WithRework(&stubRework{}, oneRoundSpec()))
	w.cfg.Polish.AutoMerge = true
	w.cfg.Polish.MergePolicy = MergePolicySquash

	statePath := StatePath("r", 42)
	// Seed the SHAs too, so this is a settled non-first-run tick with nothing
	// to polish — otherwise a phantom base-move sends it down the polish path.
	require.NoError(t, (&State{
		LastCheckAt:     time.Now(),
		LastSeenHeadSHA: "head1",
		LastSeenBaseSHA: "base1",
		MergeAttempts:   4,
		LastMergeError:  "an old failure",
	}).Save(statePath))

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	require.Equal(t, LastActionMerged, res.Action)

	s, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Empty(t, s.LastMergeError, "a successful merge must clear the stale error")
	assert.Equal(t, 5, s.MergeAttempts, "the successful attempt still counts")
}

func TestAgentRework_EmptySpecIsNoop(t *testing.T) {
	t.Parallel()
	r := NewAgentRework(nil, nil, nil)
	res, err := r.Run(context.Background(), ReworkRequest{Spec: StepSpec{}})
	require.NoError(t, err)
	assert.Zero(t, res.Rounds)
}

func TestAgentRework_CommandRoundsThreadOutputForward(t *testing.T) {
	t.Parallel()
	// A command round gathers evidence; the next round must be able to read it
	// via PrevOutput. That threading is what makes a "diagnose then fix"
	// sequence possible.
	r := NewAgentRework(nil, nil, nil)
	res, err := r.Run(context.Background(), ReworkRequest{
		WorkDir:  t.TempDir(),
		PRNumber: 42,
		Spec: StepSpec{Rounds: []RoundSpec{
			{Command: `echo "state for PR {{.PRNumber}}"`},
			{Command: `echo "saw: {{.PrevOutput}}"`},
		}},
	})
	require.NoError(t, err)
	require.Equal(t, 2, res.Rounds)
	assert.Contains(t, res.RoundOutputs[0], "state for PR 42")
	assert.Contains(t, res.RoundOutputs[1], "saw: state for PR 42",
		"each round's output must be visible to the next")
}

func TestAgentRework_CommandFailureAborts(t *testing.T) {
	t.Parallel()
	r := NewAgentRework(nil, nil, nil)
	_, err := r.Run(context.Background(), ReworkRequest{
		WorkDir: t.TempDir(),
		Spec: StepSpec{Rounds: []RoundSpec{
			{Command: `echo "diagnostic detail" >&2; exit 3`},
		}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "round 1/1", "the failing round must be identified")
	assert.Contains(t, err.Error(), "diagnostic detail",
		"the command's output is the diagnosis and must be surfaced")
}

func TestAgentRework_BadTemplateFails(t *testing.T) {
	t.Parallel()
	r := NewAgentRework(nil, nil, nil)
	_, err := r.Run(context.Background(), ReworkRequest{
		WorkDir: t.TempDir(),
		Spec:    StepSpec{Rounds: []RoundSpec{{Command: `echo {{.Nope}}`}}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Nope")
}

func TestRedactSecrets(t *testing.T) {
	t.Parallel()
	// Provider errors can embed the endpoint config that produced them, and
	// these strings reach disk (run logs) and Slack. CodeQL flags the flow as
	// go/clear-text-logging; scrub rather than trust the error's shape.
	cases := []struct {
		name        string
		in          string
		mustNotHave string
		mustHave    string
	}{
		{
			name:        "api key assignment",
			in:          "dial failed: api_key=sk-abcdefghijklmnop endpoint=https://x",
			mustNotHave: "sk-abcdefghijklmnop",
			mustHave:    "[REDACTED]",
		},
		{
			name:        "colon form with spaces",
			in:          "config: {token: ghp_ABCDEFGHIJKLMNOPQRST}",
			mustNotHave: "ghp_ABCDEFGHIJKLMNOPQRST",
			mustHave:    "[REDACTED]",
		},
		{
			name:        "bare key prefix",
			in:          "auth rejected for sk-livekey1234567890abc",
			mustNotHave: "sk-livekey1234567890abc",
			mustHave:    "auth rejected",
		},
		{
			name:        "authorization header",
			in:          "Authorization: Bearer-xyz123456789",
			mustNotHave: "Bearer-xyz123456789",
			mustHave:    "[REDACTED]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := redactSecrets(tc.in)
			assert.NotContains(t, got, tc.mustNotHave, "the secret must not survive redaction")
			assert.Contains(t, got, tc.mustHave)
		})
	}
}

func TestRedactSecrets_KeepsOrdinaryDiagnostics(t *testing.T) {
	t.Parallel()
	// Over-redaction would destroy the diagnostic value the rework agent needs.
	in := "Pull request is in an unmergeable state; base moved to abc1234"
	assert.Equal(t, in, redactSecrets(in))
}

func TestSafeErrString_ScrubsThroughWrapping(t *testing.T) {
	t.Parallel()
	// A secret buried in a wrapped error must not survive, and the
	// surrounding diagnostic context must.
	sentinel := fmt.Errorf("auth rejected: token=sk-secret123456789")
	wrapped := fmt.Errorf("merge rework round 1/2: %w", sentinel)

	got := safeErrString(wrapped)
	assert.NotContains(t, got, "sk-secret123456789", "the secret must not survive")
	assert.Contains(t, got, "merge rework round 1/2", "the context must survive")
	assert.Contains(t, got, "auth rejected")
	assert.Empty(t, safeErrString(nil), "a nil error renders as empty")
}

// The failure brake exempts provider outages wherever an agent round hits one,
// not just in polish. Merge rework runs the same kind of agent against the same
// backend, so a 529 there is exactly as uninformative about the PR — counting it
// would spend the brake on the outage and impose a cooldown on a healthy branch.
func TestWatcher_FailedMerge_TransientReworkErrorDoesNotCountTowardBrake(t *testing.T) {
	gh := failingMergeGH(t)
	rework := &stubRework{err: fmt.Errorf(
		"merge rework round 1/1: agent execution: API Error: 529 Overloaded. This is a server-side issue, usually temporary")}
	w := newWatcherForTest(t, gh, &stubPolish{}, WithRework(rework, oneRoundSpec()))
	w.cfg.Polish.AutoMerge = true
	w.cfg.Polish.MergePolicy = MergePolicySquash

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionTransient, res.Action,
		"a provider outage during rework is not a PR failure")

	s, err := LoadState(StatePath("r", 42))
	require.NoError(t, err)
	assert.Equal(t, 0, s.ConsecutiveFailures)
	assert.True(t, s.CooldownUntil.IsZero(), "transient rework errors must not trip the cooldown")
}

// The transient exemption covers ConsecutiveFailures ONLY — the merge-attempt
// brake still applies, and that is deliberate.
//
// The two counters answer different questions. ConsecutiveFailures asks "is this
// run converging?", and a 529 is no evidence either way, so it is exempt.
// MergeAttempts asks "how many times has GitHub rejected this merge?" — and each
// increment here is a real rejection that happened BEFORE rework was reached
// (see decideAndAct: MergeAttempts++ precedes w.merge). A provider outage in the
// rework that follows does not un-reject the merge; it means the PR is still
// unmergeable AND nothing is fixing it, which is exactly the budget burn the
// merge brake exists to rate-limit. Cooldown here is a rate limiter, not a
// verdict: CooldownFromAttempt hands the run a fresh allowance afterwards
// (TestWatcher_MergeBrake_ResumesAfterCooldown).
//
// Pinned because the single-tick tests above never reach the threshold, so
// nothing else would catch a change to this behaviour in either direction.
func TestWatcher_RepeatedTransientRework_StillTripsMergeBrake(t *testing.T) {
	gh := failingMergeGH(t)
	rework := &stubRework{err: fmt.Errorf(
		"merge rework round 1/1: agent execution: API Error: 529 Overloaded. This is a server-side issue, usually temporary")}
	w := newWatcherForTest(t, gh, &stubPolish{}, WithRework(rework, oneRoundSpec()))
	w.cfg.Polish.AutoMerge = true
	w.cfg.Polish.MergePolicy = MergePolicySquash
	// newWatcherForTest sets MaxConsecutiveFailures=2, Cooldown=1h.
	statePath := StatePath("r", 42)

	// First failed merge + transient rework: one real merge rejection recorded,
	// no failure-brake spend.
	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	require.Equal(t, LastActionTransient, res.Action)
	s1, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 1, s1.MergeAttempts, "the merge really was attempted and rejected")
	assert.Equal(t, 0, s1.ConsecutiveFailures, "the outage must not spend the failure brake")
	assert.True(t, s1.CooldownUntil.IsZero(), "one merge attempt is below the threshold")

	// Second one reaches the merge-attempt threshold. ConsecutiveFailures is
	// still zero — so this cooldown comes from MergeAttempts alone, which is the
	// whole point of having a separate brake.
	res, err = w.Tick(context.Background())
	require.NoError(t, err)
	require.Equal(t, LastActionTransient, res.Action)
	s2, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 2, s2.MergeAttempts)
	assert.Equal(t, 0, s2.ConsecutiveFailures,
		"transient rework must never spend the failure brake, however often it repeats")
	assert.False(t, s2.CooldownUntil.IsZero(),
		"repeated REJECTED MERGES must still rate-limit, even when rework died on an outage")
	assert.Equal(t, 2, s2.CooldownFromAttempt,
		"the cooldown records its attempt watermark so the run gets a fresh allowance")
}

// The non-transient half of the same arm: an ordinary rework failure still
// counts, so the exemption above cannot be read as "rework never brakes".
func TestWatcher_FailedMerge_RealReworkErrorStillCountsTowardBrake(t *testing.T) {
	gh := failingMergeGH(t)
	rework := &stubRework{err: fmt.Errorf("merge rework round 1/1: exit status 1")}
	w := newWatcherForTest(t, gh, &stubPolish{}, WithRework(rework, oneRoundSpec()))
	w.cfg.Polish.AutoMerge = true
	w.cfg.Polish.MergePolicy = MergePolicySquash

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionFailed, res.Action)

	s, err := LoadState(StatePath("r", 42))
	require.NoError(t, err)
	assert.Equal(t, 1, s.ConsecutiveFailures, "a real rework failure still spends the brake")
}
