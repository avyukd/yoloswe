package prdozer

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/wt"
)

// Watcher polls a single PR and reacts to changes by invoking the polish agent.
type Watcher struct {
	gh       wt.GHRunner
	polish   PolishRunner
	cfg      *Config
	renderer *render.Renderer
	logger   *slog.Logger
	repo     string
	workDir  string
	self     string
	pr       int
	dryRun   bool
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

	action := w.decideAndAct(ctx, snap, cs)
	res := TickResult{Snapshot: snap, Changeset: cs, Action: action}
	if w.recordSnapshot(state, snap, action) {
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

func (w *Watcher) decideAndAct(ctx context.Context, snap *Snapshot, cs Changeset) LastAction {
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
		outcome, err := w.merge(ctx, snap)
		if err != nil {
			w.logger.Error("auto-merge failed", "error", err, "policy", w.cfg.Polish.MergePolicy)
			w.status("PR #%d auto-merge failed: %v", w.pr, err)
			return LastActionFailed
		}
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
	w.status("PR #%d polishing (base_moved=%t ci_failed=%t new_comments=%d)",
		w.pr, cs.BaseMoved, cs.CIFailed, len(cs.NewCommentIDs))
	req := PolishRequest{
		PRNumber: w.pr,
		WorkDir:  w.workDir,
		Local:    w.cfg.Polish.Local,
		Cfg:      w.cfg.Polish,
		Model:    w.cfg.Agent.Model,
	}
	if _, err := w.polish.Run(ctx, req); err != nil {
		w.logger.Error("polish failed", "error", err)
		w.status("PR #%d polish failed: %v", w.pr, err)
		return LastActionFailed
	}
	w.status("PR #%d polish completed", w.pr)
	return LastActionPolished
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
func (w *Watcher) recordSnapshot(s *State, snap *Snapshot, action LastAction) bool {
	if action == LastActionDryRun {
		return false
	}

	firstRun := s.LastCheckAt.IsZero()
	dirty := firstRun

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
	case LastActionFailed:
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
	if dirty {
		s.LastCheckAt = snap.TakenAt
	}
	return dirty
}

func (w *Watcher) status(format string, args ...interface{}) {
	if w.renderer != nil {
		w.renderer.Status(fmt.Sprintf(format, args...))
	}
}
