package jiradozer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"strconv"
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
// Both outcomes fail closed at the caller, so the error changes no decision —
// but it changes what the sweep can tell a human. "This branch has no PR" and
// "gh could not answer" are different problems with different fixes, and a
// preview that prints one reason for both sends an operator looking for a PR
// that is sitting there merged.
func resolvePRForBranch(ctx context.Context, deps GCDeps, m RunMeta) (string, error) {
	if deps.PRByBranch == nil || m.Branch == "" || m.WorktreePath == "" {
		return "", nil
	}
	return deps.PRByBranch.ResolveForBranch(ctx, m.Branch, m.WorktreePath)
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
// It goes through `gh api`, NOT `gh pr view --json merged`: `merged` is not a
// field `gh pr view` accepts, so that form fails on every call with "Unknown
// JSON field". The failure is invisible in the worst possible way — Merged
// returns an error, gc fails closed, and every worktree is kept with a plausible
// "could not determine PR state" reason. A sweeper that never sweeps looks
// exactly like a cautious one, so the leak it exists to fix goes on silently.
//
// `gh pr view` does expose mergedAt, which is nearly equivalent, but .merged via
// the REST API is the authoritative answer and the one worth depending on here.
//
// The two spellings are easy to conflate, so to be concrete: `merged` is absent
// from `gh pr view`'s field set but present as a boolean in the response of REST
// GET /repos/{owner}/{repo}/pulls/{number}, which is the endpoint below. It
// prints bare `true`/`false` under `--jq .merged`.
func (c GHPRChecker) Merged(ctx context.Context, prURL string) (bool, error) {
	owner, repo, number, err := parsePRURL(prURL)
	if err != nil {
		return false, err
	}
	endpoint := fmt.Sprintf("repos/%s/%s/pulls/%d", owner, repo, number)
	res, err := c.GH.Run(ctx, []string{"api", endpoint, "--jq", ".merged"}, "")
	if err != nil {
		return false, fmt.Errorf("gh api %s: %w", endpoint, err)
	}
	switch strings.TrimSpace(res.Stdout) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		// Anything else is not an answer. Returning false here would read as
		// "not merged" and keep the worktree, which is the safe direction — but
		// it would also hide a broken query forever, which is how this bug
		// survived. Fail loudly instead.
		return false, fmt.Errorf("gh api %s: unexpected .merged value %q", endpoint, strings.TrimSpace(res.Stdout))
	}
}

// prURLRE matches the PR URLs recorded in a run-log.
//
// Anchored at the host on purpose. The substring "github.com/o/r/pull/1" also
// occurs in "https://notgithub.com/o/r/pull/1", so an unanchored pattern would
// answer that URL with github.com's o/r — the wrong repository, on the one path
// that authorises deleting a worktree. A URL this does not match fails loudly in
// parsePRURL instead, which keeps the worktree.
//
// github.com only, matching repoSlugFromURL in prdozer: nothing here plumbs a
// host through to `gh`, so an enterprise URL has no correct answer to give and
// is rejected rather than silently rewritten to github.com.
//
// The trailing boundary keeps "/pull/12x" from reading as PR 12, while still
// allowing the "/files" and "#discussion_r…" tails gh and browsers append.
var prURLRE = regexp.MustCompile(`^(?:https?://)?github\.com/([^/?#]+)/([^/?#]+)/pull/(\d+)(?:[/?#]|$)`)

// parsePRURL pulls owner/repo/number out of a PR URL.
func parsePRURL(prURL string) (owner, repo string, number int, err error) {
	m := prURLRE.FindStringSubmatch(prURL)
	if m == nil {
		return "", "", 0, fmt.Errorf("cannot parse PR URL %q", prURL)
	}
	n, err := strconv.Atoi(m[3])
	if err != nil {
		return "", "", 0, fmt.Errorf("cannot parse PR number in %q: %w", prURL, err)
	}
	return m[1], m[2], n, nil
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
	//
	// It returns an error rather than a bare bool because "I could not check"
	// and "nobody holds it" must not arrive as the same value: gc treats the
	// latter as clearance to delete a checkout.
	LeaseHeld func(target string) (bool, error)
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

// worktreeExists reports whether a recorded worktree is still on disk.
//
// The error return is a third answer, not a variant of "no": a stat that fails
// for any reason other than "not there" — EACCES on a parent, EIO on a flaky
// mount, a dead NFS server — has established nothing about the path. Both
// callers fail closed on it, because both of the actions it gates are
// irreversible (removing a checkout, expiring the record that owns one) while
// the check itself is free to retry on the next sweep.
func worktreeExists(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func gcWorktree(ctx context.Context, deps GCDeps, opts GCOptions, m RunMeta, logger *slog.Logger) GCCandidate {
	c := GCCandidate{
		Kind:   "worktree",
		Path:   m.WorktreePath,
		Target: m.Target(),
		RunID:  m.RunID,
		Age:    time.Since(m.StartedAt),
	}

	exists, err := worktreeExists(m.WorktreePath)
	if err != nil {
		// An unanswered question is not permission to reclaim. Everything below
		// this point reasons about a directory gc has not established is there,
		// and the sweep runs again in an hour — waiting costs nothing.
		c.Reason = fmt.Sprintf("could not verify the worktree path: %v", err)
		return c
	}
	if !exists {
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
	if deps.LeaseHeld != nil {
		key := m.LeaseKey()
		if key == "" {
			// The record cannot name its own lock, so there is no question to
			// ask — and an unasked question is not permission to reclaim. Only
			// pre-LeaseTarget --description runs land here, and they are
			// exactly the shape where a guessed name would answer "not held"
			// about a directory a worker is still using.
			c.Reason = "the run does not record which lease it holds — inspect before reclaiming"
			return c
		}
		held, err := deps.LeaseHeld(key)
		if err != nil {
			c.Reason = fmt.Sprintf("could not verify the task's lease: %v", err)
			return c
		}
		if held {
			c.Reason = "a worker still holds this task's lease"
			return c
		}
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
		resolved, err := resolvePRForBranch(ctx, deps, m)
		if err != nil {
			// Same non-decision as an empty result — keep the worktree — but say
			// which one it was, so a stranded checkout reads as a gh problem to
			// retry rather than a branch nobody ever opened a PR for.
			c.Reason = fmt.Sprintf("could not look up a PR for this branch: %v", err)
			return c
		}
		prURL = resolved
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
		// The effective root, not the raw field: that is the root the lookup
		// keyed on, so it is the one a human needs in order to explain the miss.
		c.Reason = fmt.Sprintf("no worktree manager for repo %q under %q", m.Repo, m.EffectiveWTRoot())
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
		switch exists, err := worktreeExists(m.WorktreePath); {
		case err != nil:
			// "I could not tell" is not "it is gone". Dropping the record on a
			// transient stat failure orphans the directory for good: nothing
			// else marks it as jiradozer's, so no later sweep can find it.
			c.Eligible = false
			c.Reason = fmt.Sprintf("could not verify the worktree path: %v", err)
			return c, true
		case exists:
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
