package prdozer

// Changeset describes what's different between a stored State and the latest
// Snapshot. The watcher uses these flags to decide whether to invoke the
// /pr-polish agent.
type Changeset struct {
	NewCommentIDs []string // human/bot comment IDs not in State.LastSeenCommentIDs
	NewFailedRuns []int64  // failed CI run IDs not in State.LastSeenCIRunIDs
	BaseMoved     bool     // base SHA differs from State.LastSeenBaseSHA
	HeadMoved     bool     // head SHA differs from State.LastSeenHeadSHA
	CIFailed      bool     // status rollup says FAILURE OR there are new failed runs
	NewComments   bool     // at least one new non-self comment
	Mergeable     bool     // PR is APPROVED + checks SUCCESS + base unchanged
	PRClosed      bool     // PR state is CLOSED or MERGED
	// NeedsReview is set when the only thing standing between the PR and a
	// merge is a human approval (reviewDecision REVIEW_REQUIRED) — everything
	// else is green. This is NOT agent-fixable: more /pr-polish rounds cannot
	// manufacture an approval, so the babysitter must notify and stop rather
	// than churn. A BLOCKED-but-all-green PR is almost always this case.
	NeedsReview bool
	// Conflicting is set when gh reports mergeable == "CONFLICTING"
	// (mergeable_state: dirty). The branch needs a rebase, which /pr-polish
	// can do. Critically, a dirty PR schedules ZERO CI runs, so an empty
	// status rollup here means "blocked", never "CI hasn't started yet".
	Conflicting bool
}

// Empty reports whether nothing actionable changed (no new failed CI, no new
// comments, base hasn't moved). The watcher uses this to decide "nothing to
// do, sleep until next tick".
func (c Changeset) Empty() bool {
	return !c.BaseMoved && !c.CIFailed && !c.NewComments && !c.PRClosed && !c.Conflicting
}

// NeedsPolish reports whether prdozer should invoke the polish agent.
// Mergeable PRs do NOT need polish even if other flags are true (e.g. a fresh
// commit moved HEAD but checks are green).
//
// NeedsReview is deliberately NOT a polish trigger: a missing human approval is
// not something the agent can fix, so it must route to a notification instead.
// Conflicting IS a trigger — the rebase is exactly the agent's job — and it
// fires even when no other signal changed, because a dirty PR produces no new
// CI runs to notice.
func (c Changeset) NeedsPolish() bool {
	if c.PRClosed || c.Mergeable {
		return false
	}
	return c.BaseMoved || c.CIFailed || c.NewComments || c.Conflicting
}

// ComputeChangeset diffs the snapshot against the previously persisted State.
// On first run (prev is zero), observed comments/runs are recorded but don't
// trigger polish — we only react to a current FAILURE rollup, so prdozer
// doesn't silently swallow a known-broken PR.
func ComputeChangeset(prev *State, snap *Snapshot) Changeset {
	cs := Changeset{}

	if snap.PR.State == "CLOSED" || snap.PR.State == "MERGED" {
		cs.PRClosed = true
		return cs
	}

	firstRun := prev == nil || prev.LastCheckAt.IsZero()

	if !firstRun && prev.LastSeenHeadSHA != "" && prev.LastSeenHeadSHA != snap.PR.HeadRefOid {
		cs.HeadMoved = true
	}
	// Fail-open on a missing baseline: if we have a current base SHA but the
	// previous tick recorded no baseline (e.g. a transient fetchBaseSHA
	// failure), treat recovery as a base move. Otherwise a real base move that
	// happened during the gap is silently absorbed as the new baseline.
	if !firstRun && snap.BaseSHA != "" && prev.LastSeenBaseSHA != snap.BaseSHA {
		cs.BaseMoved = true
	}

	seenComments := make(map[string]bool)
	if prev != nil {
		for _, id := range prev.LastSeenCommentIDs {
			seenComments[id] = true
		}
	}
	for _, c := range snap.Comments {
		if c.IsSelf {
			continue
		}
		if !seenComments[c.ID] {
			cs.NewCommentIDs = append(cs.NewCommentIDs, c.ID)
		}
	}
	if !firstRun && len(cs.NewCommentIDs) > 0 {
		cs.NewComments = true
	}

	seenRuns := make(map[int64]bool)
	if prev != nil {
		for _, id := range prev.LastSeenCIRunIDs {
			seenRuns[id] = true
		}
	}
	for _, id := range snap.FailedRunIDs {
		if !seenRuns[id] {
			cs.NewFailedRuns = append(cs.NewFailedRuns, id)
		}
	}
	if snap.StatusRollup == StatusFailure || (!firstRun && len(cs.NewFailedRuns) > 0) {
		cs.CIFailed = true
	}

	// A conflicting (dirty) PR needs a rebase, which the polish agent can do.
	// Surface it explicitly: a dirty PR schedules no CI runs at all, so the
	// empty rollup it produces must not be misread as "CI still pending".
	if snap.PR.Mergeable == "CONFLICTING" {
		cs.Conflicting = true
	}

	// Require an explicit SUCCESS rollup AND an explicit MERGEABLE verdict from
	// gh. An empty rollup (no checks yet, pending, or unknown) must NOT count as
	// mergeable — otherwise auto-merge can fire before CI has even started.
	if snap.PR.ReviewDecision == "APPROVED" &&
		snap.StatusRollup == StatusSuccess &&
		snap.PR.Mergeable == "MERGEABLE" &&
		!cs.BaseMoved && !cs.CIFailed {
		cs.Mergeable = true
	}

	// Everything is green except a human approval. Distinguish this from a
	// generic not-mergeable so the caller can stop instead of polishing: no
	// number of agent rounds produces a review.
	//
	// Suppressed on the first run, like every other trigger: the first tick
	// records what it observes without reacting. Without this guard, merely
	// pointing prdozer at any PR that is waiting on a reviewer would
	// immediately emit a terminal "needs human" notification.
	if !firstRun &&
		snap.PR.ReviewDecision == "REVIEW_REQUIRED" &&
		snap.StatusRollup == StatusSuccess &&
		snap.PR.Mergeable == "MERGEABLE" &&
		!cs.BaseMoved && !cs.CIFailed && !cs.NewComments {
		cs.NeedsReview = true
	}

	return cs
}
