package jiradozer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/wt"
)

// DefaultRunLogTTL is how long a terminal run's record is kept after its
// worktree is gone. The record is small and answers "what happened to INF-1234
// last week", so it outlives the worktree by a wide margin.
const DefaultRunLogTTL = 30 * 24 * time.Hour

// GCOptions configures a sweep.
type GCOptions struct {
	RunLogTTL time.Duration
	// Apply performs removals. It is OFF by default, which inverts prdozer's
	// choice deliberately: prdozer only ever reaps inside its own `.babysit`
	// namespace, while these are ordinary `wt` worktrees living beside
	// human-owned ones. A preview is the safe default when a mistake deletes
	// somebody's working directory.
	Apply bool
	// Force reclaims even when the safety checks object. Never reachable by
	// accident; the checks exist because disk pressure must not silently
	// destroy the only copy of a commit.
	Force bool
}

// GCCandidate is one worktree or run-log the sweeper considered.
type GCCandidate struct {
	Target string
	RunID  string
	Kind   string // "worktree" or "runlog"
	Path   string
	// Reason explains a skip, or justifies the removal.
	Reason string
	Age    time.Duration
	// Eligible reports that every condition for removal was met. It is distinct
	// from Removed because a preview removes nothing: without it, every row of
	// a dry run reads as a pending deletion.
	Eligible bool
	Removed  bool
}

// GCResult summarizes a sweep.
type GCResult struct {
	Candidates []GCCandidate
	// Removed counts what was ACTUALLY removed, so a preview's summary can
	// never claim removals that did not happen.
	Removed  int
	Eligible int
	Skipped  int
}

// PRChecker reports whether a pull request has merged.
type PRChecker interface {
	Merged(ctx context.Context, prURL string) (merged bool, err error)
}

// PRResolver finds the PR opened for a branch, for a run whose own record of it
// is missing. Returns "" when there is no PR — a normal outcome for a run that
// failed before opening one.
type PRResolver interface {
	ResolveForBranch(ctx context.Context, branch, dir string) (prURL string, err error)
}

// GHPRResolver asks `gh` which PR a branch opened.
type GHPRResolver struct {
	GH wt.GHRunner
}

// ResolveForBranch looks the PR up from inside the worktree, so the repository
// is unambiguous without the run-log having to record a remote.
func (r GHPRResolver) ResolveForBranch(ctx context.Context, branch, dir string) (string, error) {
	info, err := wt.GetPRByBranch(ctx, r.GH, branch, dir)
	if err != nil {
		return "", err
	}
	if info == nil {
		return "", nil
	}
	return info.URL, nil
}

// resolvePRForBranch recovers a missing PR URL, or returns "".
//
// Failure is deliberately not distinguished from absence here: both mean "this
// sweep cannot prove the work landed", and the caller already fails closed on
// an empty result. The next sweep tries again.
func resolvePRForBranch(ctx context.Context, deps GCDeps, m RunMeta) string {
	if deps.PRByBranch == nil || m.Branch == "" || m.WorktreePath == "" {
		return ""
	}
	url, err := deps.PRByBranch.ResolveForBranch(ctx, m.Branch, m.WorktreePath)
	if err != nil {
		return ""
	}
	return url
}

// GHPRChecker asks `gh` whether a PR merged.
type GHPRChecker struct {
	GH wt.GHRunner
}

// Merged reports whether the PR has landed.
//
// It asks `.merged` and nothing else. state/mergeStateStatus/reviewDecision all
// go stale or return UNKNOWN on a closed PR and none of them say "merged", and
// mergeCommit is populated for a merge queue's speculative build well before
// anything lands — so any of those would authorise deleting a worktree whose
// work is not yet in main.
func (c GHPRChecker) Merged(ctx context.Context, prURL string) (bool, error) {
	res, err := c.GH.Run(ctx, []string{"pr", "view", prURL, "--json", "merged"}, "")
	if err != nil {
		return false, fmt.Errorf("gh pr view %s: %w", prURL, err)
	}
	var out struct {
		Merged bool `json:"merged"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &out); err != nil {
		return false, fmt.Errorf("parse gh output for %s: %w", prURL, err)
	}
	return out.Merged, nil
}

// WorktreeRemover removes a reclaimed worktree. Same contract as
// WorktreeManager.RemoveWorktree, narrowed to what the sweeper needs.
type WorktreeRemover interface {
	RemoveWorktree(ctx context.Context, nameOrBranch string, deleteBranch, force bool) error
}

// GCDeps are the sweeper's injectable collaborators.
type GCDeps struct {
	Git wt.GitRunner
	PR  PRChecker
	// PRByBranch recovers a PR URL the run itself failed to record. Optional:
	// nil simply means a run with no recorded PR is never reclaimed.
	PRByBranch PRResolver
	// Removers is keyed by RunMeta.RemoverKey() — worktree root AND repo, since
	// a manager built on the wrong root looks in a directory the worktree never
	// occupied.
	Removers map[string]WorktreeRemover
	// LeaseHeld reports whether a live worker still owns this target on this
	// box. The lease is the authoritative liveness signal — it is held inside
	// the worker, so the kernel drops it on any death.
	LeaseHeld func(target string) bool
}

// RunGC reclaims worktrees whose PR has landed, then expires old run-logs.
//
// The ownership rule is what makes this safe: a worktree is only ever a
// candidate if a run-log claims it. These are ordinary `wt` worktrees sitting
// beside human-owned ones, with no namespace separating them, so the run-log IS
// the namespace. Nothing unclaimed is ever touched.
//
// A run's own terminal state authorises nothing. exec always finishes with a PR
// merely OPEN, so reclaiming on terminal state would delete the branch's only
// checkout while the PR was still awaiting review. The question is always "did
// this PR land", asked of GitHub.
func RunGC(ctx context.Context, deps GCDeps, opts GCOptions, logger *slog.Logger) (GCResult, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if opts.RunLogTTL <= 0 {
		opts.RunLogTTL = DefaultRunLogTTL
	}

	runs, err := ListRuns()
	if err != nil {
		return GCResult{}, err
	}

	var res GCResult
	now := time.Now().UTC()
	for i := range runs {
		m := runs[i]
		if m.WorktreePath != "" {
			res.add(gcWorktree(ctx, deps, opts, m, logger))
		}
		if c, ok := gcRunLog(opts, m, now); ok {
			res.add(c)
		}
	}
	return res, nil
}

func (r *GCResult) add(c GCCandidate) {
	r.Candidates = append(r.Candidates, c)
	switch {
	case c.Removed:
		r.Removed++
		r.Eligible++
	case c.Eligible:
		r.Eligible++
	default:
		r.Skipped++
	}
}

func gcWorktree(ctx context.Context, deps GCDeps, opts GCOptions, m RunMeta, logger *slog.Logger) GCCandidate {
	c := GCCandidate{
		Kind:   "worktree",
		Path:   m.WorktreePath,
		Target: m.Target(),
		RunID:  m.RunID,
		Age:    time.Since(m.StartedAt),
	}

	if _, err := os.Stat(m.WorktreePath); os.IsNotExist(err) {
		c.Reason = "worktree already gone"
		return c
	}
	// A live run must never lose the directory out from under it. The lease is
	// asked first because it is the cheapest and most reliable signal, and
	// because a still-running exec can have any state at all in its meta.
	//
	// LeaseKey, not Target: a --description run holds a lock named for its
	// description but reports a local-tracker identifier as its target, and
	// asking about the wrong lock name answers "not held" — which reads as
	// permission to delete a live worker's directory.
	if deps.LeaseHeld != nil && deps.LeaseHeld(m.LeaseKey()) {
		c.Reason = "a worker still holds this task's lease"
		return c
	}
	if !m.State.IsTerminal() {
		// Not terminal AND no lease: the run died without settling. Its
		// worktree may hold the only copy of whatever it did, so it is a case
		// for a human, not a sweeper.
		c.Reason = fmt.Sprintf("run is %s with no live lease — inspect before reclaiming (stale for %s)",
			m.State, m.StaleFor(time.Now().UTC()).Truncate(time.Second))
		return c
	}
	prURL := m.PRURL
	if prURL == "" {
		// Recover rather than give up. A run records its PR at shutdown, which
		// is exactly when a transient gh/GitHub failure is most likely — and
		// without a second chance that one bad minute would strand the worktree
		// on disk forever, since nothing ever revisits a terminal run's meta.
		// The branch is recorded independently, so the PR is still findable.
		prURL = resolvePRForBranch(ctx, deps, m)
	}
	if prURL == "" {
		c.Reason = "no PR recorded; cannot prove the work landed"
		return c
	}

	merged, err := deps.PR.Merged(ctx, prURL)
	if err != nil {
		// Fail closed. An unreachable GitHub is not evidence a PR landed.
		c.Reason = fmt.Sprintf("could not determine PR state: %v", err)
		return c
	}
	if !merged {
		c.Reason = "PR has not merged"
		return c
	}

	if reason := dirtyWorktreeReason(ctx, deps.Git, m.WorktreePath); reason != "" && !opts.Force {
		c.Reason = reason
		return c
	}

	c.Eligible = true
	c.Reason = "PR merged; worktree reclaimable"
	if !opts.Apply {
		return c
	}

	remover := deps.Removers[m.RemoverKey()]
	if remover == nil {
		c.Eligible = false
		c.Reason = fmt.Sprintf("no worktree manager for repo %q under %q", m.Repo, m.WTRoot)
		return c
	}
	// Remove through the manager (git worktree remove + prune), never rm -rf,
	// so the bare repo's metadata stays consistent.
	//
	// opts.Force is forwarded rather than hardcoded false: it is the flag that
	// already waved the dirty-worktree check through above, and git would
	// otherwise refuse the very removal the operator just authorised.
	if err := remover.RemoveWorktree(ctx, m.Branch, true, opts.Force); err != nil {
		c.Eligible = false
		c.Reason = fmt.Sprintf("remove failed: %v", err)
		logger.Warn("gc: worktree removal failed", "path", m.WorktreePath, "branch", m.Branch, "error", err)
		return c
	}
	c.Removed = true
	logger.Info("gc: reclaimed worktree", "path", m.WorktreePath, "branch", m.Branch, "target", m.Target())
	return c
}

// dirtyWorktreeReason returns a non-empty reason when a worktree still holds
// work that exists nowhere else.
//
// Uncommitted changes are the case that genuinely needs care: a squashed merge
// contains every COMMIT, so local commits are usually pre-rebase originals
// already in main, but nothing preserves an uncommitted edit.
func dirtyWorktreeReason(ctx context.Context, git wt.GitRunner, path string) string {
	if git == nil {
		return ""
	}
	res, err := git.Run(ctx, []string{"status", "--porcelain"}, path)
	if err != nil {
		return fmt.Sprintf("could not check for uncommitted work: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "" {
		return "uncommitted changes exist here and nowhere else"
	}
	return ""
}

func gcRunLog(opts GCOptions, m RunMeta, now time.Time) (GCCandidate, bool) {
	if !m.State.IsTerminal() || m.LogDir == "" {
		return GCCandidate{}, false
	}
	age := now.Sub(m.EndedAt)
	if m.EndedAt.IsZero() || age < opts.RunLogTTL {
		return GCCandidate{}, false
	}
	c := GCCandidate{
		Kind: "runlog", Path: m.LogDir, Target: m.Target(), RunID: m.RunID, Age: age,
		Eligible: true, Reason: fmt.Sprintf("terminal for %s", age.Truncate(time.Hour)),
	}
	// Never expire a record whose worktree is still on disk: the record is the
	// only thing that marks that directory as jiradozer's, so dropping it first
	// would orphan the worktree permanently.
	if m.WorktreePath != "" {
		if _, err := os.Stat(m.WorktreePath); err == nil {
			c.Eligible = false
			c.Reason = "worktree still present; the run-log is its only ownership record"
			return c, true
		}
	}
	if !opts.Apply {
		return c, true
	}
	if err := os.RemoveAll(m.LogDir); err != nil {
		c.Eligible = false
		c.Reason = fmt.Sprintf("remove failed: %v", err)
		return c, true
	}
	c.Removed = true
	return c, true
}
