package prdozer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func snapWithRollup(rollup string) *Snapshot {
	return &Snapshot{
		PR: PRDetails{
			Number:         42,
			HeadRefOid:     "head1",
			BaseRefName:    "main",
			State:          "OPEN",
			ReviewDecision: "REVIEW_REQUIRED",
			Mergeable:      "MERGEABLE",
		},
		BaseSHA:      "base1",
		StatusRollup: rollup,
	}
}

func TestComputeChangeset_ReviewRequired_NeedsReviewNotPolish(t *testing.T) {
	t.Parallel()
	// All-green but unapproved. This must NOT be mergeable, must NOT trigger a
	// polish round (no agent can produce a human approval), and must surface as
	// a distinct NeedsReview reason so the caller notifies and stops.
	prev := &State{
		LastCheckAt:     time.Now(),
		LastSeenHeadSHA: "head1",
		LastSeenBaseSHA: "base1",
	}
	snap := snapWithRollup("SUCCESS")
	cs := ComputeChangeset(prev, snap)
	assert.False(t, cs.Mergeable, "REVIEW_REQUIRED must suppress Mergeable")
	assert.True(t, cs.NeedsReview, "missing approval must surface as NeedsReview")
	assert.False(t, cs.NeedsPolish(), "a missing approval is not agent-fixable")
}

func TestComputeChangeset_Conflicting_PolishesAndSuppressesMergeable(t *testing.T) {
	t.Parallel()
	// A CONFLICTING (mergeable_state: dirty) PR schedules ZERO CI runs, so its
	// empty status rollup must not read as "CI hasn't started". It needs a
	// rebase, which is exactly the polish agent's job.
	prev := &State{
		LastCheckAt:     time.Now(),
		LastSeenHeadSHA: "head1",
		LastSeenBaseSHA: "base1",
	}
	snap := snapWithRollup("")
	snap.PR.ReviewDecision = "APPROVED"
	snap.PR.Mergeable = "CONFLICTING"

	cs := ComputeChangeset(prev, snap)
	assert.True(t, cs.Conflicting, "CONFLICTING must be surfaced")
	assert.False(t, cs.Mergeable, "a conflicting PR is never mergeable")
	assert.False(t, cs.Empty(), "a conflicting PR is actionable even with no other signal")
	assert.True(t, cs.NeedsPolish(), "conflict resolution routes to polish")
	assert.False(t, cs.NeedsReview, "an approved-but-dirty PR is not awaiting review")
}

func TestComputeChangeset_ApprovedGreen_IsMergeableNotNeedsReview(t *testing.T) {
	t.Parallel()
	prev := &State{
		LastCheckAt:     time.Now(),
		LastSeenHeadSHA: "head1",
		LastSeenBaseSHA: "base1",
	}
	snap := snapWithRollup("SUCCESS")
	snap.PR.ReviewDecision = "APPROVED"

	cs := ComputeChangeset(prev, snap)
	assert.True(t, cs.Mergeable)
	assert.False(t, cs.NeedsReview, "an approved PR is not awaiting review")
	assert.False(t, cs.NeedsPolish())
}

func TestComputeChangeset_ReviewRequiredButRed_IsNotNeedsReview(t *testing.T) {
	t.Parallel()
	// Unapproved AND red: the agent still has work to do, so this is a polish
	// round, not a "waiting on a human" stop.
	prev := &State{
		LastCheckAt:     time.Now(),
		LastSeenHeadSHA: "head1",
		LastSeenBaseSHA: "base1",
	}
	snap := snapWithRollup("FAILURE")
	cs := ComputeChangeset(prev, snap)
	assert.False(t, cs.NeedsReview, "a red PR is blocked on CI, not on review")
	assert.True(t, cs.NeedsPolish())
}

func TestComputeChangeset_FirstRun_Idle(t *testing.T) {
	t.Parallel()
	prev := &State{}
	snap := snapWithRollup("SUCCESS")
	cs := ComputeChangeset(prev, snap)
	assert.True(t, cs.Empty(), "first-run empty PR should be empty changeset")
	assert.False(t, cs.NeedsPolish())
	assert.False(t, cs.Mergeable, "REVIEW_REQUIRED is not mergeable")
}

func TestComputeChangeset_FirstRun_KnownFailure(t *testing.T) {
	t.Parallel()
	prev := &State{}
	snap := snapWithRollup("FAILURE")
	cs := ComputeChangeset(prev, snap)
	assert.True(t, cs.CIFailed, "first-run FAILURE rollup should be actionable")
	assert.True(t, cs.NeedsPolish())
}

func TestComputeChangeset_BaseMoved(t *testing.T) {
	t.Parallel()
	prev := &State{
		LastCheckAt:     time.Now(),
		LastSeenHeadSHA: "head1",
		LastSeenBaseSHA: "old-base",
	}
	snap := snapWithRollup("SUCCESS")
	cs := ComputeChangeset(prev, snap)
	assert.True(t, cs.BaseMoved)
	assert.True(t, cs.NeedsPolish())
}

func TestComputeChangeset_CIFailureViaNewRun(t *testing.T) {
	t.Parallel()
	prev := &State{
		LastCheckAt:      time.Now(),
		LastSeenHeadSHA:  "head1",
		LastSeenBaseSHA:  "base1",
		LastSeenCIRunIDs: []int64{100},
	}
	snap := snapWithRollup("PENDING")
	snap.FailedRunIDs = []int64{200}
	cs := ComputeChangeset(prev, snap)
	assert.True(t, cs.CIFailed)
	assert.Equal(t, []int64{200}, cs.NewFailedRuns)
}

func TestComputeChangeset_NewComments_IgnoresSelf(t *testing.T) {
	t.Parallel()
	prev := &State{
		LastCheckAt:        time.Now(),
		LastSeenHeadSHA:    "head1",
		LastSeenBaseSHA:    "base1",
		LastSeenCommentIDs: []string{"c1"},
	}
	snap := snapWithRollup("SUCCESS")
	snap.Comments = []CommentRef{
		{ID: "c1", Author: "alice"},
		{ID: "c2", Author: "bob"},
		{ID: "c3", Author: "me", IsSelf: true},
	}
	cs := ComputeChangeset(prev, snap)
	assert.True(t, cs.NewComments)
	assert.Equal(t, []string{"c2"}, cs.NewCommentIDs)
}

func TestComputeChangeset_CommentIDsFromDifferentSourcesDoNotCollide(t *testing.T) {
	t.Parallel()
	// Simulate the real fetchComments output: inline + issue endpoints each
	// namespace their IDs. Two distinct comments must both count as new.
	prev := &State{
		LastCheckAt:        time.Now(),
		LastSeenHeadSHA:    "head1",
		LastSeenBaseSHA:    "base1",
		LastSeenCommentIDs: []string{"inline:42"},
	}
	snap := snapWithRollup("SUCCESS")
	snap.Comments = []CommentRef{
		{ID: "inline:42", Source: "inline", Author: "alice"}, // already seen
		{ID: "issue:42", Source: "issue", Author: "bob"},     // same numeric id, different endpoint
	}
	cs := ComputeChangeset(prev, snap)
	assert.True(t, cs.NewComments)
	assert.Equal(t, []string{"issue:42"}, cs.NewCommentIDs,
		"the issue-sourced #42 must NOT be silently dropped as a duplicate of inline #42")
}

func TestComputeChangeset_PRClosed_ShortCircuits(t *testing.T) {
	t.Parallel()
	snap := snapWithRollup("SUCCESS")
	snap.PR.State = "MERGED"
	prev := &State{LastCheckAt: time.Now(), LastSeenHeadSHA: "old", LastSeenBaseSHA: "older"}
	cs := ComputeChangeset(prev, snap)
	assert.True(t, cs.PRClosed)
	assert.False(t, cs.NeedsPolish())
	assert.False(t, cs.BaseMoved, "closed PR should short-circuit before computing other diffs")
}

func TestComputeChangeset_Mergeable(t *testing.T) {
	t.Parallel()
	prev := &State{
		LastCheckAt:     time.Now(),
		LastSeenHeadSHA: "head1",
		LastSeenBaseSHA: "base1",
	}
	snap := snapWithRollup("SUCCESS")
	snap.PR.ReviewDecision = "APPROVED"
	cs := ComputeChangeset(prev, snap)
	assert.True(t, cs.Mergeable)
	assert.False(t, cs.NeedsPolish(), "mergeable should not trigger polish")
}

func TestComputeChangeset_EmptyRollupNotMergeable(t *testing.T) {
	t.Parallel()
	prev := &State{
		LastCheckAt:     time.Now(),
		LastSeenHeadSHA: "head1",
		LastSeenBaseSHA: "base1",
	}
	snap := snapWithRollup("")
	snap.PR.ReviewDecision = "APPROVED"
	cs := ComputeChangeset(prev, snap)
	assert.False(t, cs.Mergeable,
		"empty status rollup (no checks yet / pending / unknown) must NOT be treated as mergeable")
}

func TestComputeChangeset_GhMergeableConflicting(t *testing.T) {
	t.Parallel()
	prev := &State{
		LastCheckAt:     time.Now(),
		LastSeenHeadSHA: "head1",
		LastSeenBaseSHA: "base1",
	}
	snap := snapWithRollup("SUCCESS")
	snap.PR.ReviewDecision = "APPROVED"
	snap.PR.Mergeable = "CONFLICTING"
	cs := ComputeChangeset(prev, snap)
	assert.False(t, cs.Mergeable,
		"CONFLICTING PRs must not be flagged mergeable even with APPROVED + SUCCESS")
}

func TestComputeChangeset_BaseRecoveryFromMissingBaseline(t *testing.T) {
	t.Parallel()
	// Transient base-SHA fetch failure: prev tick recorded no baseline. Fail
	// open — when the next successful tick produces a base SHA, treat it as a
	// base move, not silently as the new ground truth. Otherwise a real move
	// that happened during the outage is invisible.
	prev := &State{
		LastCheckAt:     time.Now(),
		LastSeenHeadSHA: "head1",
		LastSeenBaseSHA: "",
	}
	snap := snapWithRollup("SUCCESS")
	snap.BaseSHA = "recovered-base"
	cs := ComputeChangeset(prev, snap)
	assert.True(t, cs.BaseMoved,
		"recovery from an empty baseline must be treated as a base move")
	assert.True(t, cs.NeedsPolish())
}

func TestComputeChangeset_BaseMovedSuppressesMergeable(t *testing.T) {
	t.Parallel()
	prev := &State{
		LastCheckAt:     time.Now(),
		LastSeenHeadSHA: "head1",
		LastSeenBaseSHA: "old-base",
	}
	snap := snapWithRollup("SUCCESS")
	snap.PR.ReviewDecision = "APPROVED"
	cs := ComputeChangeset(prev, snap)
	assert.True(t, cs.BaseMoved)
	assert.False(t, cs.Mergeable, "moving base requires a rebase first")
	assert.True(t, cs.NeedsPolish())
}

// GitHub returns an EMPTY reviewDecision when a repo requires no approving
// review — not only when one is pending. Treating "" as unapproved stranded a
// genuinely ready PR: not mergeable (wanted "APPROVED"), not needs-review
// (wanted "REVIEW_REQUIRED"), so it ticked idle forever with no terminal state.
// Observed live on bazelment/yoloswe#284.
func TestComputeChangeset_EmptyReviewDecisionIsMergeable(t *testing.T) {
	t.Parallel()
	prev := &State{
		LastCheckAt:     time.Now(),
		LastSeenHeadSHA: "head1",
		LastSeenBaseSHA: "base1",
	}
	snap := snapWithRollup(StatusSuccess)
	snap.PR.ReviewDecision = "" // repo requires no review
	snap.PR.Mergeable = "MERGEABLE"

	cs := ComputeChangeset(prev, snap)
	assert.True(t, cs.Mergeable,
		"a green PR on a repo with no review requirement must be mergeable")
	assert.False(t, cs.NeedsReview,
		"no review is required, so this is not a needs-human state")
}

func TestReviewSatisfied(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"APPROVED":          true,
		"":                  true, // repo requires no review
		"REVIEW_REQUIRED":   false,
		"CHANGES_REQUESTED": false,
		"SOMETHING_NEW":     false, // unknown gates fail closed
	}
	for decision, want := range cases {
		t.Run(decision, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, want, reviewSatisfied(decision))
		})
	}
}

// A CHANGES_REQUESTED verdict is work, not a wall: the reviewer asked for
// specific changes and addressing them is exactly what the polish loop does.
// It must be its OWN trigger — the review comments get consumed as "seen" on
// an early tick, after which nothing would drive the loop and the flag would
// sit set forever.
func TestComputeChangeset_ChangesRequestedDrivesPolish(t *testing.T) {
	t.Parallel()
	prev := &State{
		LastCheckAt:     time.Now(),
		LastSeenHeadSHA: "head1",
		LastSeenBaseSHA: "base1",
	}
	snap := snapWithRollup(StatusSuccess)
	snap.PR.ReviewDecision = "CHANGES_REQUESTED"
	snap.PR.Mergeable = "MERGEABLE"

	cs := ComputeChangeset(prev, snap)
	assert.True(t, cs.ChangesRequested, "a changes-requested verdict must be surfaced")
	assert.True(t, cs.NeedsPolish(), "requested changes route to polish")
	assert.False(t, cs.Mergeable, "never merge past a changes-requested verdict")
	assert.False(t, cs.NeedsReview, "this is agent-fixable, not a needs-human stop")
	assert.False(t, cs.Empty(), "a changes-requested PR is actionable")
}

// Unlike the event-shaped triggers, this one is standing state: suppressing it
// on the first tick would mean pointing prdozer at a changes-requested PR does
// nothing on the tick that matters.
func TestComputeChangeset_ChangesRequestedFiresOnFirstRun(t *testing.T) {
	t.Parallel()
	snap := snapWithRollup(StatusSuccess)
	snap.PR.ReviewDecision = "CHANGES_REQUESTED"
	snap.PR.Mergeable = "MERGEABLE"

	cs := ComputeChangeset(&State{}, snap) // zero state == first run
	assert.True(t, cs.ChangesRequested, "standing state must fire even on first run")
	assert.True(t, cs.NeedsPolish())
}

func TestChangesRequestedBy(t *testing.T) {
	t.Parallel()
	got := changesRequestedBy([]reviewRow{
		{State: "CHANGES_REQUESTED", Author: struct {
			Login string `json:"login"`
		}{Login: "sycamore-groot[bot]"}},
		{State: "APPROVED", Author: struct {
			Login string `json:"login"`
		}{Login: "someone"}},
		{State: "CHANGES_REQUESTED", Author: struct {
			Login string `json:"login"`
		}{Login: "coderabbitai[bot]"}},
		{State: "CHANGES_REQUESTED", Author: struct {
			Login string `json:"login"`
		}{Login: "sycamore-groot[bot]"}}, // duplicate
	})
	// The [bot] suffix is a display form the reviewer API does not accept.
	assert.Equal(t, []string{"sycamore-groot", "coderabbitai"}, got)
}

// Bots cannot be re-requested: GitHub answers 422 "Reviews may only be
// requested from collaborators" because an app is not a collaborator. They also
// do not need it — measured on kernel#8227, coderabbitai re-reviewed 75 seconds
// after the polish push with no request at all.
func TestBotReviewers(t *testing.T) {
	t.Parallel()
	got := botReviewers([]reviewRow{
		{State: "CHANGES_REQUESTED", Author: struct {
			Login string `json:"login"`
		}{Login: "sycamore-groot[bot]"}},
		{State: "CHANGES_REQUESTED", Author: struct {
			Login string `json:"login"`
		}{Login: "a-human"}},
	})
	assert.True(t, got["sycamore-groot"], "keyed by the stripped login, matching ChangesRequestedBy")
	assert.False(t, got["a-human"], "humans are re-requestable and must not be skipped")
}
