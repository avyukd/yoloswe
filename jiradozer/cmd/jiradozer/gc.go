package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/cliapp"
	"github.com/bazelment/yoloswe/jiradozer"
	"github.com/bazelment/yoloswe/wt"
)

// newGCCmd reclaims worktrees whose PR has landed.
//
// This is not optional maintenance. exec always finishes with the PR merely
// open, so it always keeps its worktree — meaning without a sweeper every
// dispatched task leaks one permanently, and on a large repo those are
// gigabytes each.
func newGCCmd() *cobra.Command {
	var (
		apply bool
		force bool
		ttl   time.Duration
	)
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Reclaim worktrees whose PR has merged, and expire old run logs",
		Long: "Previews by default. Pass --apply to actually remove anything.\n\n" +
			"Only worktrees recorded in a run-log are ever candidates: these are ordinary\n" +
			"wt worktrees living beside human-owned ones, so the run-log is what marks a\n" +
			"directory as jiradozer's. Eligibility is decided by asking GitHub whether the\n" +
			"PR merged — never by the run's own terminal state, which only ever means\n" +
			"\"a PR is open\".",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app := cliapp.FromContext(cmd.Context())
			ctx := cmd.Context()

			runs, err := jiradozer.ListRuns()
			if err != nil {
				return err
			}
			removers, err := removersForRuns(runs)
			if err != nil {
				return err
			}

			res, err := jiradozer.RunGC(ctx, jiradozer.GCDeps{
				Git:        &wt.DefaultGitRunner{},
				PR:         jiradozer.GHPRChecker{GH: &wt.DefaultGHRunner{}},
				PRByBranch: jiradozer.GHPRResolver{GH: &wt.DefaultGHRunner{}},
				Removers:   removers,
				LeaseHeld:  leaseHeld,
			}, jiradozer.GCOptions{Apply: apply, Force: force, RunLogTTL: ttl}, app.Logger)
			if err != nil {
				return err
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ACTION\tKIND\tTARGET\tPATH\tREASON")
			for i := range res.Candidates {
				c := &res.Candidates[i]
				action := "keep"
				switch {
				case c.Removed:
					action = "removed"
				case c.Eligible:
					action = "would-remove"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", action, c.Kind, c.Target, c.Path, c.Reason)
			}
			if err := tw.Flush(); err != nil {
				return err
			}

			if apply {
				fmt.Fprintf(cmd.OutOrStdout(), "\n%d removed, %d kept\n", res.Removed, res.Skipped)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "\n%d eligible, %d kept (preview only — pass --apply to remove)\n",
					res.Eligible, res.Skipped)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "Actually remove; without it this only previews")
	cmd.Flags().BoolVar(&force, "force", false, "Reclaim even when a worktree holds uncommitted work")
	cmd.Flags().DurationVar(&ttl, "runlog-ttl", jiradozer.DefaultRunLogTTL, "Expire terminal run logs older than this")
	return cmd
}

// removersForRuns builds one wt.Manager per (worktree root, repo) a run-log
// mentions.
//
// Keyed by root as well as repo because each run records the root it was
// created under, and those can differ — WT_ROOT changes, or older runs were
// made elsewhere. A manager built on this process's root would look for those
// worktrees in a directory they never occupied and quietly reclaim nothing.
// Records predating WTRoot have it empty; EffectiveWTRoot recovers the root
// from their recorded worktree path, so a root migration does not strand them.
// Only a record that can name no root at all falls back to the current one.
func removersForRuns(runs []jiradozer.RunMeta) (map[string]jiradozer.WorktreeRemover, error) {
	root, err := resolveWTRoot()
	if err != nil {
		return nil, err
	}
	out := map[string]jiradozer.WorktreeRemover{}
	for i := range runs {
		m := &runs[i]
		if m.Repo == "" || out[m.RemoverKey()] != nil {
			continue
		}
		runRoot := m.EffectiveWTRoot()
		if runRoot == "" {
			runRoot = root
		}
		out[m.RemoverKey()] = &wtAdapter{mgr: wt.NewManager(runRoot, m.Repo)}
	}
	return out, nil
}

// leaseHeld reports whether a live worker owns this target on this box.
//
// It tests the lock, never the file's existence: Release deliberately leaves
// the file behind (removing it races a process about to flock it), so a file
// count is a count of tasks this box has EVER run. prdozer's probe made exactly
// that mistake and every host permanently excluded itself after two runs.
// The error return is the third answer, the same shape worktreeExists uses: a
// lease this box cannot open is not a lease nobody holds, and gc reads "not
// held" as clearance to delete somebody's checkout.
func leaseHeld(target string) (bool, error) {
	f, err := os.OpenFile(jiradozer.LeasePath(target), os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil // no lease file at all: nothing has ever run this here
		}
		return false, err
	}
	defer f.Close()
	// Any flock failure counts as held. EWOULDBLOCK is the expected one, and the
	// rest (EINTR, ENOLCK) are equally inconclusive — erring toward "held" here
	// only ever costs a sweep.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true, nil
	}
	// We just took it ourselves, which proves nobody else had it.
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, nil
}
