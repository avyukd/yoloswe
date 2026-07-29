package prdozer

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/wt"
)

// Watcher polls a single PR and reacts to changes by invoking the polish agent.
type Watcher struct {
	gh         wt.GHRunner
	polish     PolishRunner
	rework     ReworkRunner
	cfg        *Config
	renderer   *render.Renderer
	logger     *slog.Logger
	repo       string
	workDir    string
	self       string
	reworkSpec StepSpec
	pr         int
	dryRun     bool
}

// WithRework attaches the merge-rework runner and the rounds it should run,
// normally taken from the registry entry's merge_rework.
func WithRework(r ReworkRunner, spec StepSpec) WatcherOption {
	return func(w *Watcher) {
		w.rework = r
		w.reworkSpec = spec
	}
}

// WatcherOption configures a new Watcher.
type WatcherOption func(*Watcher)

// WithRenderer attaches a renderer for terminal output.
func WithRenderer(r *render.Renderer) WatcherOption {
	return func(w *Watcher) { w.renderer = r }
}

// WithSelfLogin tells the watcher which GitHub login to ignore when looking
// for new comments (so prdozer doesn't react to its own comments).
func WithSelfLogin(login string) WatcherOption {
	return func(w *Watcher) { w.self = login }
}

// WithDryRun puts the watcher in observe-only mode — snapshots and changesets
// are computed and logged, but no agent is invoked.
func WithDryRun(dryRun bool) WatcherOption {
	return func(w *Watcher) { w.dryRun = dryRun }
}

// NewWatcher creates a watcher for a single PR.
func NewWatcher(cfg *Config, gh wt.GHRunner, polish PolishRunner, prNumber int, workDir, repo string, logger *slog.Logger, opts ...WatcherOption) *Watcher {
	if logger == nil {
		logger = slog.Default()
	}
	w := &Watcher{
		cfg:     cfg,
		gh:      gh,
		polish:  polish,
		logger:  logger.With("pr", prNumber),
		pr:      prNumber,
		repo:    repo,
		workDir: workDir,
	}
	for _, o := range opts {
		o(w)
	}
	return w
}

// TickResult summarizes what happened in one polling cycle.
type TickResult struct {
	Snapshot  *Snapshot
	Action    LastAction
	Changeset Changeset
}

func (w *Watcher) Tick(ctx context.Context) (TickResult, error) {
	statePath := StatePath(w.repo, w.pr)
	state, err := LoadState(statePath)
	if err != nil {
		return TickResult{}, fmt.Errorf("load state: %w", err)
	}
	state.PRNumber = w.pr
	state.Repo = w.repo

	if !state.CooldownUntil.IsZero() && time.Now().Before(state.CooldownUntil) {
		w.logger.Info("in cooldown, skipping tick", "until", state.CooldownUntil)
		w.status("PR #%d in cooldown until %s", w.pr, state.CooldownUntil.Format(time.RFC3339))
		return TickResult{Action: LastActionIdle}, nil
	}

	snapOpts := SnapshotOptions{Self: w.self}
	snap, err := TakeSnapshot(ctx, w.gh, w.workDir, w.pr, snapOpts)
	if err != nil {
		return TickResult{}, fmt.Errorf("snapshot: %w", err)
	}
	cs := ComputeChangeset(state, snap)
	w.logger.Info("snapshot",
		"head", snap.PR.HeadRefOid,
		"base_sha", snap.BaseSHA,
		"rollup", snap.StatusRollup,
		"review", snap.PR.ReviewDecision,
		"comments", len(snap.Comments),
		"failed_runs", len(snap.FailedRunIDs),
		"new_comments", len(cs.NewCommentIDs),
		"new_failed_runs", len(cs.NewFailedRuns),
		"base_moved", cs.BaseMoved,
		"ci_failed", cs.CIFailed,
		"mergeable", cs.Mergeable,
		"pr_closed", cs.PRClosed,
	)

	priorAttempts, priorMergeErr := state.MergeAttempts, state.LastMergeError
	action := w.decideAndAct(ctx, snap, cs, state)
	mergeStateDirty := state.MergeAttempts != priorAttempts || state.LastMergeError != priorMergeErr

	res := TickResult{Snapshot: snap, Changeset: cs, Action: action}
	if w.recordSnapshot(state, snap, action, mergeStateDirty) {
		if err := state.Save(statePath); err != nil {
			// State save failure is serious: the action already ran (maybe merged
			// or polished) and the next tick will reload stale state and could
			// re-trigger it. Surface the error so the outer loop can at least
			// log+alert, and the caller can decide whether to back off.
			return res, fmt.Errorf("save state %q: %w", statePath, err)
		}
	}
	return res, nil
}

func (w *Watcher) decideAndAct(ctx context.Context, snap *Snapshot, cs Changeset, state *State) LastAction {
	switch {
	case cs.PRClosed:
		w.status("PR #%d is %s — nothing to do", w.pr, snap.PR.State)
		if snap.PR.State == "MERGED" {
			return LastActionMerged
		}
		return LastActionClosed
	case cs.Mergeable && w.cfg.Polish.AutoMerge && w.cfg.Polish.MergePolicy == MergePolicyNotify:
		// Explicitly configured never to merge: report and stop rather than
		// idling forever on a PR that is ready to land.
		w.status("PR #%d is mergeable but merge_policy is %q — not merging", w.pr, MergePolicyNotify)
		return LastActionNeedsHuman
	case cs.Mergeable && w.cfg.Polish.AutoMerge && !w.dryRun:
		state.MergeAttempts++
		outcome, err := w.merge(ctx, snap)
		if err != nil {
			// Scrub once, at the source: this message is logged, persisted to
			// the state file, and fed verbatim into the rework prompt.
			safe := safeErrString(err)
			w.logger.Error("auto-merge failed",
				"error", safe, "policy", w.cfg.Polish.MergePolicy, "attempt", state.MergeAttempts)
			w.status("PR #%d merge attempt %d failed: %s", w.pr, state.MergeAttempts, safe)
			state.LastMergeError = safe
			return w.reworkAfterFailedMerge(ctx, snap, state)
		}
		state.LastMergeError = ""
		if outcome == mergeOutcomeArmed {
			w.status("PR #%d auto-merge armed — waiting for the merge queue", w.pr)
			return LastActionArmed
		}
		w.status("PR #%d merged", w.pr)
		return LastActionMerged
	case cs.Mergeable:
		w.status("PR #%d is mergeable — idle", w.pr)
		return LastActionIdle
	case cs.NeedsReview:
		// Green on every axis prdozer can influence; only a human approval is
		// missing. Polishing again would burn rounds against a wall.
		w.status("PR #%d is green but awaiting human review approval", w.pr)
		return LastActionNeedsHuman
	case !cs.NeedsPolish():
		w.status("PR #%d unchanged — idle", w.pr)
		return LastActionIdle
	}

	if w.dryRun {
		w.status("PR #%d would polish (base_moved=%t ci_failed=%t new_comments=%d) — dry run, skipping",
			w.pr, cs.BaseMoved, cs.CIFailed, len(cs.NewCommentIDs))
		return LastActionDryRun
	}
	if w.polish == nil {
		w.logger.Warn("no polish runner configured; skipping action")
		return LastActionIdle
	}
	w.status("PR #%d polishing (base_moved=%t ci_failed=%t new_comments=%d changes_requested=%t)",
		w.pr, cs.BaseMoved, cs.CIFailed, len(cs.NewCommentIDs), cs.ChangesRequested)
	req := PolishRequest{
		PRNumber: w.pr,
		WorkDir:  w.workDir,
		Local:    w.cfg.Polish.Local,
		Cfg:      w.cfg.Polish,
		Model:    w.cfg.Agent.Model,
	}
	if _, err := w.polish.Run(ctx, req); err != nil {
		// Log the scrubbed STRING, not a wrapped error: a provider error can
		// embed the endpoint config (API key / key-bearing env var), and these
		// messages are persisted and Slacked.
		safe := safeErrString(err)
		w.logger.Error("polish failed", "error", safe)
		w.status("PR #%d polish failed: %s", w.pr, safe)
		return LastActionFailed
	}
	w.status("PR #%d polish completed", w.pr)

	// A CHANGES_REQUESTED verdict is sticky: it stays set until the reviewer
	// looks again. Without an explicit re-request the fixes just sit there and
	// the flag never clears, so the loop would keep re-polishing the same
	// already-addressed comments forever.
	if cs.ChangesRequested {
		w.rerequestReviews(ctx, snap)
	}
	return LastActionPolished
}

// rerequestReviews asks every reviewer who requested changes to look again.
//
// Best-effort by design: the polish work is already pushed, so failing to
// re-request is worth a warning but must not turn a successful round into a
// failed one (which would count toward the backoff that trips cooldown).
func (w *Watcher) rerequestReviews(ctx context.Context, snap *Snapshot) {
	reviewers := snap.ChangesRequestedBy
	if len(reviewers) == 0 {
		w.logger.Warn("changes requested but no reviewer identified; skipping re-request")
		return
	}
	for _, r := range reviewers {
		args := []string{"pr", "edit", strconv.Itoa(w.pr), "--add-reviewer", r}
		if res, err := w.gh.Run(ctx, args, w.workDir); err != nil {
			// Bot reviewers frequently cannot be re-requested through this API
			// (an app is not a requestable reviewer). That is expected, not a
			// run failure: those bots re-review on push anyway.
			w.logger.Warn("could not re-request review",
				"reviewer", r, "error", safeErrString(ghError(err, res)))
			continue
		}
		w.status("PR #%d re-requested review from %s", w.pr, r)
	}
}

// reworkAfterFailedMerge decides what happens after a merge attempt fails.
//
// A merge can fail in ways /pr-polish will never fix, because they are about
// LANDING rather than code quality: the queue dequeued the PR, a speculative
// batch build went red, the base moved, a required check flipped after
// approval. Those get the repo's merge_rework rounds; the next tick then
// re-snapshots and re-evaluates from scratch.
//
// Two cases are terminal instead, because no agent round can resolve them:
// a merge policy that never merges, and a missing human approval.
func (w *Watcher) reworkAfterFailedMerge(ctx context.Context, snap *Snapshot, state *State) LastAction {
	switch {
	case w.cfg.Polish.MergePolicy == MergePolicyNotify:
		w.status("PR #%d merge failed and merge_policy is %q — stopping", w.pr, MergePolicyNotify)
		return LastActionNeedsHuman
	case snap.PR.ReviewDecision == "REVIEW_REQUIRED":
		w.status("PR #%d merge failed and needs a human approval — stopping", w.pr)
		return LastActionNeedsHuman
	case w.rework == nil || w.reworkSpec.Empty():
		// Nothing configured to try. Report a plain failure so backoff still
		// applies, rather than silently idling.
		w.logger.Warn("merge failed and no merge_rework configured", "pr", w.pr)
		return LastActionFailed
	}

	req := ReworkRequest{
		WorkDir:     w.workDir,
		Repo:        w.repo,
		Branch:      snap.PR.HeadRefName,
		PRURL:       snap.PR.URL,
		MergeError:  state.LastMergeError,
		MergePolicy: w.cfg.Polish.MergePolicy,
		Spec:        w.reworkSpec,
		Model:       w.cfg.Agent.Model,
		Cfg:         w.cfg.Polish,
		PRNumber:    w.pr,
		Attempt:     state.MergeAttempts,
	}
	w.status("PR #%d running merge rework (attempt %d)", w.pr, state.MergeAttempts)
	if _, err := w.rework.Run(ctx, req); err != nil {
		// Scrub before logging: a provider error can embed the endpoint
		// config that produced it, and these logs are persisted and Slacked.
		safe := safeErrString(err)
		w.logger.Error("merge rework failed", "error", safe, "attempt", state.MergeAttempts)
		w.status("PR #%d merge rework failed: %s", w.pr, safe)
		return LastActionFailed
	}
	w.status("PR #%d merge rework completed — will retry merge next tick", w.pr)
	return LastActionReworked
}

// mergeOutcome is the result of one merge attempt.
type mergeOutcome int

const (
	// mergeOutcomeMerged means the merge is confirmed landed: the GitHub API
	// reported `.merged == true`.
	mergeOutcomeMerged mergeOutcome = iota
	// mergeOutcomeArmed means auto-merge was armed successfully but the queue
	// has not landed the PR yet. Keep polling.
	mergeOutcomeArmed
)

// merge lands the PR according to the configured policy and then VERIFIES the
// result against the GitHub API.
//
// Verification is not optional paranoia. `gh pr merge` exiting 0 does not mean
// the PR merged: under `--auto` it means auto-merge was armed, and a merge
// queue can report a populated merge_commit_sha for a candidate it only
// speculatively built. Likewise `state: CLOSED` is ambiguous — a PR closed
// without merging looks identical. The single unambiguous signal is
// `.merged == true` on the pulls endpoint, so that is the only thing we trust.
func (w *Watcher) merge(ctx context.Context, snap *Snapshot) (mergeOutcome, error) {
	policy := w.cfg.Polish.MergePolicy
	args, ok := policy.MergeArgs(w.pr)
	if !ok {
		return 0, fmt.Errorf("merge policy %q does not merge", policy)
	}

	// Re-derive owner/repo from the PR's own URL rather than trusting a
	// separately-configured slug, and assert the PR we are about to mutate is
	// the one we snapshotted. A stale PR number has previously been pointed at
	// an unrelated PR; the head-ref assertion below is what catches that.
	owner, repo, err := repoSlugFromURL(snap.PR.URL)
	if err != nil {
		return 0, fmt.Errorf("derive owner/repo for merge: %w", err)
	}
	if snap.PR.Number != w.pr {
		return 0, fmt.Errorf("refusing to merge: snapshot is PR #%d but watcher targets #%d", snap.PR.Number, w.pr)
	}

	res, err := w.gh.Run(ctx, args, w.workDir)
	if err != nil {
		return 0, ghError(err, res)
	}

	merged, err := w.verifyMerged(ctx, owner, repo)
	if err != nil {
		return 0, fmt.Errorf("verify merge: %w", err)
	}
	if merged {
		return mergeOutcomeMerged, nil
	}
	if policy == MergePolicyQueue {
		// Expected: --auto arms the queue, which lands the PR asynchronously.
		return mergeOutcomeArmed, nil
	}
	// A direct merge policy reported success but the PR is not merged. Treat
	// this as a failure rather than silently declaring victory.
	return 0, fmt.Errorf("merge command succeeded but PR #%d is not merged (policy %q)", w.pr, policy)
}

// verifyMerged reports the authoritative merged bit from the GitHub API.
func (w *Watcher) verifyMerged(ctx context.Context, owner, repo string) (bool, error) {
	res, err := w.gh.Run(ctx, []string{
		"api", fmt.Sprintf("repos/%s/%s/pulls/%d", owner, repo, w.pr),
		"--jq", ".merged",
	}, w.workDir)
	if err != nil {
		return false, ghError(err, res)
	}
	return strings.EqualFold(strings.TrimSpace(res.Stdout), "true"), nil
}

// recordSnapshot mutates s to reflect snap and action, returning true iff any
// persistable field changed (so the caller can skip disk writes on no-op ticks).
// Dry-run is fully observe-only: it skips every persisted-state mutation so a
// later live tick still sees the same triggers (new HEAD, new comments, new
// failed CI runs) and can react to them.
//
// mergeStateDirty tells it that decideAndAct already mutated MergeAttempts or
// LastMergeError on s. Without that signal a tick whose only change was a
// merge attempt would look unchanged and skip the disk write, losing the
// attempt count the unbounded-retry design depends on.
func (w *Watcher) recordSnapshot(s *State, snap *Snapshot, action LastAction, mergeStateDirty bool) bool {
	if action == LastActionDryRun {
		return false
	}

	firstRun := s.LastCheckAt.IsZero()
	dirty := firstRun || mergeStateDirty

	if s.LastSeenHeadSHA != snap.PR.HeadRefOid {
		s.LastSeenHeadSHA = snap.PR.HeadRefOid
		dirty = true
	}
	if snap.BaseSHA != "" && s.LastSeenBaseSHA != snap.BaseSHA {
		s.LastSeenBaseSHA = snap.BaseSHA
		dirty = true
	}

	commentIDs := make([]string, 0, len(snap.Comments))
	for _, c := range snap.Comments {
		commentIDs = append(commentIDs, c.ID)
	}
	if s.MergeSeenComments(commentIDs) {
		dirty = true
	}
	if s.MergeSeenRuns(snap.FailedRunIDs) {
		dirty = true
	}

	if s.LastAction != action {
		s.LastAction = action
		dirty = true
	}
	switch action {
	// LastActionReworked shares the failure arm so a straight run of reworks
	// still trips the ordinary backoff.
	case LastActionFailed, LastActionReworked:
		s.ConsecutiveFailures++
		dirty = true
		if w.cfg.Backoff.MaxConsecutiveFailures > 0 && s.ConsecutiveFailures >= w.cfg.Backoff.MaxConsecutiveFailures && w.cfg.Backoff.Cooldown > 0 {
			s.CooldownUntil = time.Now().Add(w.cfg.Backoff.Cooldown)
			w.logger.Warn("entering cooldown after repeated failures",
				"failures", s.ConsecutiveFailures,
				"until", s.CooldownUntil,
			)
		}
	case LastActionPolished, LastActionMerged, LastActionClosed, LastActionIdle,
		LastActionArmed, LastActionNeedsHuman:
		// A real successful/terminal tick clears any prior backoff.
		if s.ConsecutiveFailures != 0 || !s.CooldownUntil.IsZero() {
			dirty = true
		}
		s.ConsecutiveFailures = 0
		s.CooldownUntil = time.Time{}
	}

	// The merge-attempt brake. ConsecutiveFailures alone CANNOT bound merge
	// retries, because the real retry loop alternates rather than repeating:
	// rework rebases and pushes, which moves HEAD and re-triggers CI, so the
	// next tick polishes, polish succeeds, and the counter resets to zero.
	// Empirically the counter never exceeds 1, so the cooldown never trips and
	// an unmergeable PR burns agent budget forever with no Slack visibility.
	// (Same shape under MergePolicyQueue: a repeatedly-dequeued PR re-arms
	// cleanly each tick, which is also a "success".)
	//
	// MergeAttempts is the signal that actually tracks merge progress, and
	// nothing resets it — so brake on that instead. Merge attempts remain
	// unbounded in the sense that the run resumes after each cooldown and
	// keeps climbing; the cooldown just rate-limits it and makes it visible.
	if w.mergeBrakeTripped(s) {
		s.CooldownUntil = time.Now().Add(w.cfg.Backoff.Cooldown)
		s.CooldownFromAttempt = s.MergeAttempts
		dirty = true
		w.logger.Warn("entering cooldown after repeated merge attempts",
			"merge_attempts", s.MergeAttempts,
			"until", s.CooldownUntil,
		)
	}
	if dirty {
		s.LastCheckAt = snap.TakenAt
	}
	return dirty
}

// mergeBrakeTripped reports whether enough merge attempts have accumulated
// since the last cooldown to warrant another one.
//
// It measures attempts SINCE the previous cooldown (CooldownFromAttempt) so a
// run that resumes after backing off gets a fresh allowance rather than
// re-tripping on every subsequent tick forever.
func (w *Watcher) mergeBrakeTripped(s *State) bool {
	maxAttempts := w.cfg.Backoff.MaxConsecutiveFailures
	if maxAttempts <= 0 || w.cfg.Backoff.Cooldown <= 0 {
		return false
	}
	// Already cooling down; don't extend the window on every tick.
	if !s.CooldownUntil.IsZero() && time.Now().Before(s.CooldownUntil) {
		return false
	}
	return s.MergeAttempts-s.CooldownFromAttempt >= maxAttempts
}

func (w *Watcher) status(format string, args ...interface{}) {
	if w.renderer != nil {
		w.renderer.Status(fmt.Sprintf(format, args...))
	}
}
