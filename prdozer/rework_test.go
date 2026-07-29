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
