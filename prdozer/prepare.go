package prdozer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/bazelment/yoloswe/wt"
)

// BabysitNamespace is the directory under a repo's worktree_root that holds
// ephemeral babysit worktrees. Keeping them in their own namespace does two
// things: it visually separates throwaway runs from the ~150 real worktrees on
// a busy box, and it gives the GC sweeper an unambiguous set to reap. Nothing
// outside this prefix is ever a GC candidate.
const BabysitNamespace = ".babysit"

// PlainWorktreeRoot holds ephemeral clones for "plain"-layout repos, which have
// no bare clone to hang a git worktree off.
const PlainWorktreeRoot = "~/.prdozer/worktrees"

// RunContext is one babysit run's private workspace.
type RunContext struct {
	RunID string
	// WorktreePath is the ephemeral checkout, created by and owned solely by
	// this run. Because it is freshly created under a privately-named path,
	// nothing else can be inside it — so the old dirty-tree and "another agent
	// owns this worktree" checks are unnecessary here.
	WorktreePath string
	// LogDir is under RunsRoot, deliberately OUTSIDE WorktreePath so GC never
	// takes the logs.
	LogDir string
	Branch string
	Layout Layout
	// KeptReason explains why Cleanup declined to remove the worktree, for the
	// notification.
	KeptReason string
	// Kept records that Cleanup declined to remove the worktree.
	Kept bool
}

// NewRunID returns a short random identifier distinguishing concurrent runs on
// the same PR.
func NewRunID() (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate run id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// BabysitWorktreePath returns the ephemeral worktree path for a run. The run ID
// is what makes two concurrent runs on one PR — or a run against a branch you
// happen to have checked out yourself — impossible to collide.
func BabysitWorktreePath(e RepoEntry, prNumber int, runID string) string {
	if e.Layout == LayoutPlain {
		return filepath.Join(ExpandHome(PlainWorktreeRoot), fmt.Sprintf("%s-%s", runDirLeaf(prNumber, runID), "clone"))
	}
	return filepath.Join(ExpandHome(e.WorktreeRoot), BabysitNamespace, runDirLeaf(prNumber, runID))
}

func runDirLeaf(prNumber int, runID string) string {
	return fmt.Sprintf("%d-%s", prNumber, runID)
}

// PrepareWorktree creates a private, ephemeral checkout of the PR's head branch.
func PrepareWorktree(ctx context.Context, git wt.GitRunner, e RepoEntry, pr DiscoveredPR, runID string, logger *slog.Logger) (*RunContext, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if pr.HeadRefName == "" {
		return nil, fmt.Errorf("PR #%d has no head branch", pr.Number)
	}
	// Fork PRs need remote wiring we deliberately don't guess at. Fail with a
	// clear message rather than producing a worktree tracking the wrong ref.
	if pr.IsCrossRepository {
		return nil, fmt.Errorf("PR #%d is from a fork (%s); cross-repository PRs are not supported", pr.Number, pr.HeadRepoOwner)
	}

	path := BabysitWorktreePath(e, pr.Number, runID)
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("worktree path %s already exists", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create worktree parent: %w", err)
	}

	rc := &RunContext{
		RunID:        runID,
		WorktreePath: path,
		LogDir:       RunDirFor(repoSlugOrDefault(pr.URL), pr.Number, runID),
		Branch:       pr.HeadRefName,
		Layout:       e.Layout,
	}

	root := ExpandHome(e.WorktreeRoot)
	// Fetch first: origin/<branch> must be current, or the worktree starts
	// stale and the first polish round rebases onto yesterday's base.
	if _, err := git.Run(ctx, []string{"fetch", "origin", pr.HeadRefName}, root); err != nil {
		return nil, fmt.Errorf("fetch origin/%s: %w", pr.HeadRefName, err)
	}

	switch e.Layout {
	case LayoutPlain:
		// A plain clone has no worktree support. Clone with --reference so the
		// object store is shared and the copy stays cheap.
		if res, err := git.Run(ctx, []string{
			"clone", "--reference", root, "--branch", pr.HeadRefName, root, path,
		}, ""); err != nil {
			return nil, fmt.Errorf("clone %s: %w", path, gitErr(err, res))
		}
	default:
		// --detach, deliberately. Git refuses to check out one branch in two
		// worktrees at once, which would make two concurrent babysit runs on
		// the same PR impossible — and concurrent runs are exactly what the
		// per-run ID is designed to allow. A detached HEAD at origin/<branch>
		// gives the run the branch's content; pushes name the branch
		// explicitly (see pushBranch), so the local ref is never needed.
		if res, err := git.Run(ctx, []string{
			"worktree", "add", "--detach", path, "origin/" + pr.HeadRefName,
		}, root); err != nil {
			return nil, fmt.Errorf("worktree add %s: %w", path, gitErr(err, res))
		}
	}
	logger.Info("prepared ephemeral worktree",
		"path", path, "branch", pr.HeadRefName, "layout", e.Layout, "run_id", runID)
	return rc, nil
}

// Cleanup removes the ephemeral worktree, subject to one hard safety
// condition: it never discards work that exists nowhere else.
//
// Every other piece of a run's state lives in the PR or in LogDir, both of
// which survive. A commit that exists only in this worktree does not — so if
// the tree is dirty or ahead of origin, Cleanup first tries to push, re-checks,
// and if it still isn't clean KEEPS the worktree and reports why. Losing an
// agent's committed work to a disk-space sweep is not an acceptable trade.
//
// force skips the push attempt and the safety check entirely; it is for the GC
// sweeper's explicit --force path, not for normal terminal-state cleanup.
func (rc *RunContext) Cleanup(ctx context.Context, git wt.GitRunner, force bool, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	if _, err := os.Stat(rc.WorktreePath); os.IsNotExist(err) {
		return nil
	}

	if !force {
		unclean, reason, err := WorktreeHasUnpushedWork(ctx, git, rc.WorktreePath, rc.Branch)
		if err != nil {
			// Can't prove it's safe — keep it. An unreadable worktree is
			// exactly when blind removal is most dangerous.
			rc.Kept, rc.KeptReason = true, fmt.Sprintf("could not verify worktree is clean: %v", err)
			return nil
		}
		if unclean {
			logger.Info("worktree has unpushed work; pushing before cleanup",
				"path", rc.WorktreePath, "reason", reason)
			if perr := pushBranch(ctx, git, rc.WorktreePath, rc.Branch); perr != nil {
				rc.Kept, rc.KeptReason = true, fmt.Sprintf("unpushed work and push failed: %v", perr)
				logger.Warn("keeping worktree: push failed", "path", rc.WorktreePath, "error", perr)
				return nil
			}
			// Re-check: the push may have covered commits but not a dirty
			// working tree.
			stillUnclean, reason2, err2 := WorktreeHasUnpushedWork(ctx, git, rc.WorktreePath, rc.Branch)
			if err2 != nil || stillUnclean {
				rc.Kept = true
				rc.KeptReason = fmt.Sprintf("still has local-only work after push: %s", reason2)
				if err2 != nil {
					rc.KeptReason = fmt.Sprintf("could not re-verify after push: %v", err2)
				}
				logger.Warn("keeping worktree", "path", rc.WorktreePath, "reason", rc.KeptReason)
				return nil
			}
		}
	}

	return rc.remove(ctx, git, logger)
}

// remove deletes the checkout itself. The BRANCH is never deleted: the PR owns
// it, and deleteBranchOnMerge already handles cleanup server-side. Deleting it
// here is how a PR ends up closed-but-unmerged.
func (rc *RunContext) remove(ctx context.Context, git wt.GitRunner, logger *slog.Logger) error {
	if rc.Layout == LayoutPlain {
		if err := os.RemoveAll(rc.WorktreePath); err != nil {
			return fmt.Errorf("remove clone %s: %w", rc.WorktreePath, err)
		}
		logger.Info("removed ephemeral clone", "path", rc.WorktreePath)
		return nil
	}
	// `git worktree remove` must run from inside the repository, not from the
	// caller's cwd (which is arbitrary for a long-running daemon). Run it from
	// the worktree itself, then prune from the parent repo — which we can only
	// reach once the worktree is gone, so resolve it first.
	repoDir := rc.parentRepoDir(ctx, git)
	if res, err := git.Run(ctx, []string{"worktree", "remove", "--force", rc.WorktreePath}, rc.WorktreePath); err != nil {
		return fmt.Errorf("worktree remove %s: %w", rc.WorktreePath, gitErr(err, res))
	}
	// Prune so the parent repo's administrative entry goes away too.
	if repoDir != "" {
		if _, err := git.Run(ctx, []string{"worktree", "prune"}, repoDir); err != nil {
			logger.Warn("worktree prune failed", "error", err)
		}
	}
	logger.Info("removed ephemeral worktree", "path", rc.WorktreePath)
	return nil
}

// parentRepoDir resolves the repository a worktree belongs to, so post-removal
// commands have somewhere valid to run. Returns "" when it can't be determined,
// in which case the caller skips the prune rather than guessing.
func (rc *RunContext) parentRepoDir(ctx context.Context, git wt.GitRunner) string {
	res, err := git.Run(ctx, []string{"rev-parse", "--path-format=absolute", "--git-common-dir"}, rc.WorktreePath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

// WorktreeHasUnpushedWork reports whether path holds state that exists nowhere
// else: uncommitted changes, or commits not present on origin/<branch>.
func WorktreeHasUnpushedWork(ctx context.Context, git wt.GitRunner, path, branch string) (bool, string, error) {
	res, err := git.Run(ctx, []string{"status", "--porcelain"}, path)
	if err != nil {
		return false, "", fmt.Errorf("git status: %w", gitErr(err, res))
	}
	if strings.TrimSpace(res.Stdout) != "" {
		return true, "uncommitted changes in the working tree", nil
	}

	// Commits present locally but not on the remote branch.
	res, err = git.Run(ctx, []string{"log", "--oneline", fmt.Sprintf("origin/%s..HEAD", branch)}, path)
	if err != nil {
		// A missing origin/<branch> (deleted after merge, never pushed) means
		// we cannot prove the commits are safe elsewhere. Report unknown
		// rather than clean.
		return false, "", fmt.Errorf("git log origin/%s..HEAD: %w", branch, gitErr(err, res))
	}
	if strings.TrimSpace(res.Stdout) != "" {
		return true, "commits not present on origin/" + branch, nil
	}
	return false, "", nil
}

// pushBranch pushes the worktree's HEAD to the PR's branch, so a commit that
// exists only here survives the worktree's removal.
//
// The refspec is explicitly HEAD:refs/heads/<branch> because the worktree runs
// on a detached HEAD (see PrepareWorktree) — a bare `push origin <branch>`
// would push the stale local ref, not the work that was just done here.
//
// --force-with-lease=<branch> supplies the safety: the push is refused if the
// remote branch is not where this checkout last saw it, so concurrent work is
// never clobbered.
//
// --force-if-includes is deliberately NOT used despite being the usual
// companion flag. It validates against the reflog of a tracking branch, which a
// detached HEAD does not have, so it can never be satisfied here and rejects
// every push with "remote ref updated since checkout" — which would mean the
// worktree is always kept and the commit never actually published.
//
// The lease is pinned to an EXPLICIT expected SHA: the remote-tracking ref as
// this worktree currently sees it. Never refresh that ref (`git fetch`) before
// pushing — a refresh moves the lease to whatever the remote now holds, so the
// comparison trivially succeeds and force-with-lease silently overwrites the
// very concurrent commit it exists to protect.
func pushBranch(ctx context.Context, git wt.GitRunner, path, branch string) error {
	remoteRef := "refs/remotes/origin/" + branch
	res, err := git.Run(ctx, []string{"rev-parse", remoteRef}, path)
	if err != nil {
		return fmt.Errorf("resolve %s for push lease: %w", remoteRef, gitErr(err, res))
	}
	expected := strings.TrimSpace(res.Stdout)
	if expected == "" {
		return fmt.Errorf("could not resolve %s for push lease", remoteRef)
	}

	res, err = git.Run(ctx, []string{
		"push",
		fmt.Sprintf("--force-with-lease=%s:%s", branch, expected),
		"origin", "HEAD:refs/heads/" + branch,
	}, path)
	if err != nil {
		return gitErr(err, res)
	}
	return nil
}

// stderrOf safely reads a possibly-nil result's stderr, for error messages.
func stderrOf(res *wt.CmdResult) string {
	if res == nil {
		return ""
	}
	return strings.TrimSpace(res.Stderr)
}

func gitErr(err error, res *wt.CmdResult) error {
	if s := stderrOf(res); s != "" {
		return fmt.Errorf("%w: %s", err, s)
	}
	return err
}

// repoSlugOrDefault extracts "owner/repo" from a PR URL, falling back to a
// placeholder so a malformed URL still produces a usable log directory rather
// than aborting the run.
func repoSlugOrDefault(prURL string) string {
	owner, repo, err := repoSlugFromURL(prURL)
	if err != nil {
		return "unknown"
	}
	return owner + "/" + repo
}
