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
	// ChangesRequested is set when reviewDecision is CHANGES_REQUESTED. Unlike
	// NeedsReview this IS agent-fixable and drives polish: the reviewer asked
	// for specific changes, and addressing them is exactly the polish loop's
	// job. It deliberately does not distinguish bot reviewers from human ones —
	// requested changes are actionable feedback either way, and on kernel the
	// blocking reviewers are usually bots (sycamore-groot, coderabbitai).
	//
	// It must be its own trigger rather than relying on NewComments: the review
	// comments are consumed as "seen" on an early tick, after which nothing
	// would drive the loop and the flag would sit set forever.
	ChangesRequested bool
}

// Empty reports whether nothing actionable is present — no new failed CI, no
// new comments, base hasn't moved, and no standing block on the PR.
//
// ChangesRequested is included even though it is standing state rather than an
// event: a changes-requested PR is actionable on every tick until the flag
// clears, and reporting it as "empty" while NeedsPolish is true would be a
// contradiction waiting to mislead a reader.
func (c Changeset) Empty() bool {
	return !c.BaseMoved && !c.CIFailed && !c.NewComments && !c.PRClosed &&
		!c.Conflicting && !c.ChangesRequested
}

// NeedsPolish reports whether prdozer should invoke the polish agent.
//
// Unaddressed feedback (NewComments, ChangesRequested) is checked FIRST and
// polishes even a mergeable PR. Past that point mergeable does mean done: a
// green mergeable PR with only BaseMoved/CIFailed/Conflicting left to consider
// has nothing for the agent to do.
//
// NeedsReview is deliberately NOT a polish trigger: a missing human approval is
// not something the agent can fix, so it must route to a notification instead.
// Conflicting IS a trigger — the rebase is exactly the agent's job — and it
// fires even when no other signal changed, because a dirty PR produces no new
// CI runs to notice.
func (c Changeset) NeedsPolish() bool {
	if c.PRClosed {
		return false
	}
	// Unaddressed review feedback is polish-worthy even on a MERGEABLE PR.
	// GitHub calls a PR mergeable when it has no conflicts and passes required
	// checks — that says nothing about whether a reviewer's comments were
	// handled. Treating mergeable as "done" let yoloswe#287 finish in 1.5s with
	// new_comments=1 and pr-polish never invoked at all.
	if c.NewComments || c.ChangesRequested {
		return true
	}
	// For the remaining signals, mergeable really does mean nothing to do: a
	// green mergeable PR has no failing CI and no conflict to resolve, and a
	// moved base that still merges cleanly needs no rebase.
	if c.Mergeable {
		return false
	}
	return c.BaseMoved || c.CIFailed || c.Conflicting
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
	if reviewSatisfied(snap.PR.ReviewDecision) &&
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
	// A reviewer asked for changes. This is work, not a wall: address the
	// comments, push, and re-request review so the flag can clear. Unlike the
	// other triggers this is NOT suppressed on the first run — the flag is
	// standing state rather than an event, so there is nothing to "already
	// know", and suppressing it would mean pointing prdozer at a
	// changes-requested PR does nothing at all on the tick that matters.
	if snap.PR.ReviewDecision == "CHANGES_REQUESTED" {
		cs.ChangesRequested = true
	}

	if !firstRun &&
		snap.PR.ReviewDecision == "REVIEW_REQUIRED" &&
		snap.StatusRollup == StatusSuccess &&
		snap.PR.Mergeable == "MERGEABLE" &&
		!cs.BaseMoved && !cs.CIFailed && !cs.NewComments {
		cs.NeedsReview = true
	}

	return cs
}

// reviewSatisfied reports whether a PR's review gate is clear.
//
// GitHub returns an EMPTY reviewDecision when the repository requires no
// approving review at all — not just when a review is pending. Treating "" as
// unapproved makes a genuinely ready PR idle forever: it is neither mergeable
// (which wanted "APPROVED") nor needs-review (which wanted "REVIEW_REQUIRED"),
// so it falls through both branches and no terminal state is ever reached.
// Observed on bazelment/yoloswe#284: mergeStateStatus CLEAN, every check
// green, reviewDecision "" — and prdozer ticked idle indefinitely.
//
// A repo that DOES require review reports "REVIEW_REQUIRED" until approved, so
// accepting "" here cannot merge past a real review requirement.
func reviewSatisfied(decision string) bool {
	switch decision {
	case "APPROVED", "":
		return true
	default:
		// REVIEW_REQUIRED, CHANGES_REQUESTED, or anything new GitHub adds:
		// fail closed rather than assume a gate we do not recognize is open.
		return false
	}
}
