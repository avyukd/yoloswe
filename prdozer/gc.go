package prdozer

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/wt"
)

// Default retention windows. Worktrees are expensive (a kernel checkout plus
// node_modules is not small) so they go early; run logs are cheap and are what
// you actually want weeks later, so they stay far longer.
const (
	DefaultWorktreeTTL = 3 * 24 * time.Hour
	DefaultRunLogTTL   = 30 * 24 * time.Hour
)

// GCOptions configures a sweep.
type GCOptions struct {
	// WorktreeRoots are the repo roots to scan for orphaned .babysit
	// worktrees, typically every worktree_root in the registry.
	WorktreeRoots []string
	WorktreeTTL   time.Duration
	RunLogTTL     time.Duration
	// DryRun reports what would be removed without touching anything. Given a
	// box at 68% disk with ~150 real worktrees, a sweeper that cannot be
	// previewed is a liability.
	DryRun bool
	// Force removes stale worktrees even when they hold unpushed work. Off by
	// default; the whole point of the safety check is that disk pressure never
	// silently destroys an agent's only copy of a commit.
	Force bool
}

// GCCandidate is one item the sweeper considered.
type GCCandidate struct {
	Path string
	// Kind is "worktree" or "runlog".
	Kind string
	// Reason explains a skip, or the removal justification.
	Reason  string
	Age     time.Duration
	Removed bool
}

// GCResult summarizes a sweep.
type GCResult struct {
	Candidates []GCCandidate
	// Removed counts what was ACTUALLY removed; it stays zero under DryRun so
	// the summary line never claims removals that did not happen.
	Removed int
	// Eligible counts what met every removal condition, whether or not it was
	// removed. This is the number a dry run reports.
	Eligible int
	Skipped  int
}

// RunGC reaps orphaned ephemeral worktrees and expired run logs. Orphans arise
// when a box reboots or a run is killed mid-flight, leaving a .babysit
// directory nobody owns.
//
// Only paths inside a BabysitNamespace directory (or PlainWorktreeRoot) are
// ever candidates. A real, human-owned worktree sitting beside them is never
// touched — that is the entire reason ephemeral runs live in their own
// namespace.
func RunGC(ctx context.Context, git wt.GitRunner, opts GCOptions, logger *slog.Logger) (GCResult, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if opts.WorktreeTTL <= 0 {
		opts.WorktreeTTL = DefaultWorktreeTTL
	}
	if opts.RunLogTTL <= 0 {
		opts.RunLogTTL = DefaultRunLogTTL
	}
	var res GCResult
	now := time.Now()

	roots := make([]string, 0, len(opts.WorktreeRoots)+1)
	for _, r := range opts.WorktreeRoots {
		if r == "" {
			continue
		}
		roots = append(roots, filepath.Join(ExpandHome(r), BabysitNamespace))
	}
	roots = append(roots, ExpandHome(PlainWorktreeRoot))

	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			logger.Warn("gc: cannot read worktree namespace", "path", root, "error", err)
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			path := filepath.Join(root, e.Name())
			cand := GCCandidate{Path: path, Kind: "worktree"}
			info, err := e.Info()
			if err != nil {
				cand.Reason = fmt.Sprintf("stat failed: %v", err)
				res.Skipped++
				res.Candidates = append(res.Candidates, cand)
				continue
			}
			cand.Age = now.Sub(info.ModTime())
			if cand.Age < opts.WorktreeTTL {
				cand.Reason = fmt.Sprintf("younger than TTL (%s < %s)", cand.Age.Round(time.Hour), opts.WorktreeTTL)
				res.Skipped++
				res.Candidates = append(res.Candidates, cand)
				continue
			}
			// Never reap work that exists nowhere else, even under disk
			// pressure. A stale worktree costs gigabytes; a lost commit costs
			// an agent's entire run.
			if !opts.Force {
				unclean, reason, err := WorktreeHasUnpushedWork(ctx, git, path, branchOfWorktree(ctx, git, path))
				switch {
				case err != nil:
					cand.Reason = fmt.Sprintf("cannot verify clean: %v", err)
					res.Skipped++
					res.Candidates = append(res.Candidates, cand)
					continue
				case unclean:
					cand.Reason = "has local-only work: " + reason
					res.Skipped++
					res.Candidates = append(res.Candidates, cand)
					continue
				}
			}
			cand.Reason = fmt.Sprintf("orphaned babysit worktree, age %s", cand.Age.Round(time.Hour))
			if !opts.DryRun {
				rc := &RunContext{WorktreePath: path, Layout: layoutOfPath(root)}
				if err := rc.remove(ctx, git, logger); err != nil {
					cand.Reason = fmt.Sprintf("remove failed: %v", err)
					res.Skipped++
					res.Candidates = append(res.Candidates, cand)
					continue
				}
				cand.Removed = true
				res.Removed++
			}
			res.Eligible++
			res.Candidates = append(res.Candidates, cand)
		}
	}

	// Expired run logs. These are pruned on a much longer horizon and only
	// for runs that already reached a terminal state — a still-"running" meta
	// means the run may yet be alive.
	runsRoot := ExpandHome(RunsRoot)
	entries, err := os.ReadDir(runsRoot)
	if err != nil && !os.IsNotExist(err) {
		return res, fmt.Errorf("read runs dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(runsRoot, e.Name())
		cand := GCCandidate{Path: path, Kind: "runlog"}
		info, err := e.Info()
		if err != nil {
			continue
		}
		cand.Age = now.Sub(info.ModTime())
		if cand.Age < opts.RunLogTTL {
			cand.Reason = "younger than run-log TTL"
			res.Skipped++
			continue
		}
		// Keep anything we cannot positively confirm is finished. An
		// unreadable or half-written meta.json means the run may still be
		// alive — "cannot verify" must fail closed, the same way the worktree
		// loop above treats an unverifiable working tree.
		m, merr := LoadRunMeta(path)
		switch {
		case merr != nil:
			cand.Reason = fmt.Sprintf("cannot verify run state: %v", merr)
			res.Skipped++
			res.Candidates = append(res.Candidates, cand)
			continue
		case m.State == TerminalRunning:
			cand.Reason = "run still marked running"
			res.Skipped++
			res.Candidates = append(res.Candidates, cand)
			continue
		}
		cand.Reason = fmt.Sprintf("expired run log, age %s", cand.Age.Round(24*time.Hour))
		if !opts.DryRun {
			if err := os.RemoveAll(path); err != nil {
				cand.Reason = fmt.Sprintf("remove failed: %v", err)
				res.Skipped++
				res.Candidates = append(res.Candidates, cand)
				continue
			}
			cand.Removed = true
			res.Removed++
		}
		res.Eligible++
		res.Candidates = append(res.Candidates, cand)
	}
	return res, nil
}

// layoutOfPath infers how to remove a path: entries under PlainWorktreeRoot are
// standalone clones, everything else is a git worktree.
func layoutOfPath(root string) Layout {
	if strings.HasPrefix(root, ExpandHome(PlainWorktreeRoot)) {
		return LayoutPlain
	}
	return LayoutWT
}

// branchOfWorktree reports which remote branch a babysit worktree corresponds
// to, or "" if it can't be determined.
//
// Babysit worktrees run on a DETACHED HEAD (see PrepareWorktree), so
// `rev-parse --abbrev-ref HEAD` returns the literal "HEAD" rather than a branch
// name — comparing against "origin/HEAD" would error and make every worktree
// look unverifiable, so the sweeper would never reap anything. Ask git which
// remote branches contain this commit instead.
//
// Returning "" is safe by construction: WorktreeHasUnpushedWork then fails to
// resolve the ref, reports an error, and the caller keeps the worktree.
func branchOfWorktree(ctx context.Context, git wt.GitRunner, path string) string {
	// A checked-out branch (the plain-clone layout) is the easy case.
	if res, err := git.Run(ctx, []string{"rev-parse", "--abbrev-ref", "HEAD"}, path); err == nil {
		if name := strings.TrimSpace(res.Stdout); name != "" && name != "HEAD" {
			return name
		}
	}
	// Detached: find a remote branch containing HEAD. Prefer the first match;
	// an ambiguous commit on several branches is still safe to compare against
	// any one of them, since we only ask "is HEAD already published".
	res, err := git.Run(ctx, []string{
		"branch", "--remotes", "--contains", "HEAD", "--format=%(refname:short)",
	}, path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		name := strings.TrimSpace(line)
		// Skip symbolic entries like "origin/HEAD -> origin/main".
		if name == "" || strings.Contains(name, "->") {
			continue
		}
		return strings.TrimPrefix(name, "origin/")
	}
	return ""
}
