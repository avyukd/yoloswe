package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/cliapp"
	"github.com/bazelment/yoloswe/prdozer"
	"github.com/bazelment/yoloswe/wt"
)

// addBabysitCommands registers the babysit subcommand tree. The root command
// keeps its original flat flag set for back-compat.
func addBabysitCommands(root *cobra.Command) {
	root.AddCommand(newBabysitCmd(), newBabysitLocalCmd(), newFleetCmd(), newRunsCmd(), newGCCmd())
}

// resolveEntry loads the registry and resolves one repo, the shared prelude of
// both babysit commands. The registry itself is returned alongside the entry
// because fleet-wide settings (notification) live on it rather than per repo.
func resolveEntry(registryPath, ownerRepo string) (prdozer.RepoEntry, *prdozer.Registry, error) {
	reg, err := prdozer.LoadRegistry(registryPath)
	if err != nil {
		return prdozer.RepoEntry{}, nil, err
	}
	entry, err := reg.Resolve(ownerRepo)
	if err != nil {
		return prdozer.RepoEntry{}, nil, err
	}
	return entry, reg, nil
}

func newBabysitCmd() *cobra.Command {
	var (
		prRef        string
		here         bool
		host         string
		registryPath string
		dryRun       bool
		keepWorktree bool
		minDiskGB    int
	)
	cmd := &cobra.Command{
		Use:   "babysit",
		Short: "Drive a PR to merge on the best available devbox",
		Long: "Picks a healthy devbox from the fleet, prepares an ephemeral worktree there, " +
			"and runs polish -> CI-green -> merge to completion, reworking failed merges " +
			"and reporting terminal state to Slack.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app := cliapp.FromContext(cmd.Context())
			ownerRepo, prNumber, err := prdozer.ParsePRRef(prRef)
			if err != nil {
				return err
			}
			entry, reg, err := resolveEntry(registryPath, ownerRepo)
			if err != nil {
				return err
			}

			// Quota is a fleet-GLOBAL gate: the OAuth session is shared across
			// every box, so a run dispatched under an exhausted quota dies
			// mid-polish wherever it lands.
			if usage, qerr := prdozer.CheckClaudeQuota(cmd.Context()); qerr != nil {
				app.Logger.Warn("could not read claude quota; proceeding", "error", qerr)
			} else if usage.Exhausted() && !dryRun {
				return fmt.Errorf("%w: five-hour utilization is %.0f%%",
					prdozer.ErrQuotaExhausted, usage.FiveHourUtilization)
			}

			if here {
				return runBabysitLocal(cmd.Context(), ownerRepo, prNumber, entry, reg, keepWorktree, false)
			}

			ssh := prdozer.DefaultSSHRunner{}
			plan, err := prdozer.PlanDispatch(cmd.Context(), ssh, prdozer.DispatchOptions{
				OwnerRepo:    ownerRepo,
				PRNumber:     prNumber,
				RegistryPath: registryPath,
				Host:         host,
				DryRun:       dryRun,
				KeepWorktree: keepWorktree,
				Probe:        prdozer.ProbeOptions{MinDiskGB: minDiskGB},
			}, app.Logger)
			if err != nil {
				// Print the score table even on failure: "no eligible host" is
				// only actionable if you can see why each was rejected.
				if len(plan.Scores) > 0 {
					printScores(cmd.OutOrStdout(), plan.Scores)
				}
				return err
			}

			// --dry-run must print the EXACT command it would run plus the
			// full score table: this is the primary debugging surface for
			// dispatch.
			if dryRun {
				printScores(cmd.OutOrStdout(), plan.Scores)
				fmt.Fprintf(cmd.OutOrStdout(), "\nchosen: %s\n", plan.Chosen.Host)
				if plan.RanLocal {
					fmt.Fprintf(cmd.OutOrStdout(), "would run IN-PROCESS (chosen host is this box)\n")
					return nil
				}
				fmt.Fprintf(cmd.OutOrStdout(), "would run:\n%s\n", plan.Command)
				return nil
			}

			if plan.RanLocal {
				app.Logger.Info("chosen host is this box; running in-process", "host", plan.Chosen.Host)
				return runBabysitLocal(cmd.Context(), ownerRepo, prNumber, entry, reg, keepWorktree, false)
			}
			if err := prdozer.Dispatch(cmd.Context(), ssh, prdozer.DispatchRequest{
				Host:         plan.Chosen,
				OwnerRepo:    ownerRepo,
				PRNumber:     prNumber,
				RegistryPath: registryPath,
				KeepWorktree: keepWorktree,
			}, app.Logger); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "dispatched to %s (tmux session %q)\n",
				plan.Chosen.Host, prdozer.TmuxSessionName(ownerRepo, prNumber))
			return nil
		},
	}
	cmd.Flags().StringVar(&prRef, "pr", "", "PR to babysit, as owner/repo#123 (required)")
	cmd.Flags().BoolVar(&here, "here", false, "Run the worker in-process instead of dispatching")
	cmd.Flags().StringVar(&host, "host", "", "Pin a specific devbox instead of scoring the fleet")
	cmd.Flags().StringVar(&registryPath, "registry", prdozer.DefaultRegistryPath, "Path to the repo registry")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the score table and the exact SSH command, then exit")
	cmd.Flags().BoolVar(&keepWorktree, "keep-worktree", false, "Do not GC the ephemeral worktree")
	cmd.Flags().IntVar(&minDiskGB, "min-disk-gb", 0, "Minimum usable free disk to accept a host (default 40)")
	_ = cmd.MarkFlagRequired("pr")
	return cmd
}

func newBabysitLocalCmd() *cobra.Command {
	var (
		ownerRepo    string
		prNumber     int
		registryPath string
		keepWorktree bool
		once         bool
	)
	cmd := &cobra.Command{
		Use:   "babysit-local",
		Short: "Run the babysit worker on this box (normally invoked by dispatch)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			entry, reg, err := resolveEntry(registryPath, ownerRepo)
			if err != nil {
				return err
			}
			return runBabysitLocal(cmd.Context(), ownerRepo, prNumber, entry, reg, keepWorktree, once)
		},
	}
	cmd.Flags().StringVar(&ownerRepo, "repo", "", "Repository as owner/repo (required)")
	cmd.Flags().IntVar(&prNumber, "pr", 0, "PR number (required)")
	cmd.Flags().StringVar(&registryPath, "registry", prdozer.DefaultRegistryPath, "Path to the repo registry")
	cmd.Flags().BoolVar(&keepWorktree, "keep-worktree", false, "Do not GC the ephemeral worktree")
	cmd.Flags().BoolVar(&once, "once", false, "Run a single tick then exit")
	_ = cmd.MarkFlagRequired("repo")
	_ = cmd.MarkFlagRequired("pr")
	return cmd
}

func runBabysitLocal(ctx context.Context, ownerRepo string, prNumber int, entry prdozer.RepoEntry, reg *prdozer.Registry, keepWorktree, once bool) error {
	app := cliapp.FromContext(ctx)
	gh := &wt.DefaultGHRunner{}
	if err := wt.CheckGitHubAuth(ctx, gh); err != nil {
		return err
	}
	b := prdozer.NewBabysitter(gh, &wt.DefaultGitRunner{}, app.Renderer, app.Logger, prdozer.BabysitOptions{
		OwnerRepo:    ownerRepo,
		PRNumber:     prNumber,
		Entry:        entry,
		KeepWorktree: keepWorktree,
		Once:         once,
		Notify:       reg.Notify.WithTarget(entry.SlackTarget),
	})
	state, err := b.Run(ctx)
	if err != nil {
		return err
	}
	app.Logger.Info("babysit finished", "repo", ownerRepo, "pr", prNumber, "state", state)
	return nil
}

func newFleetCmd() *cobra.Command {
	fleetCmd := &cobra.Command{Use: "fleet", Short: "Inspect the devbox fleet"}
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Probe every devbox and print load, disk, tmux windows, and leases",
		RunE: func(cmd *cobra.Command, _ []string) error {
			hosts, err := prdozer.LoadFleet(prdozer.FleetDir)
			if err != nil {
				return err
			}
			selfHostname, _ := os.Hostname()
			scores := prdozer.ProbeFleet(cmd.Context(), prdozer.DefaultSSHRunner{}, hosts, prdozer.ProbeOptions{
				SelfDNS:      prdozer.SelfPublicDNS(prdozer.DevboxConfigPath),
				SelfHostname: selfHostname,
			})
			printScores(cmd.OutOrStdout(), scores)
			return nil
		},
	}
	fleetCmd.AddCommand(statusCmd)
	return fleetCmd
}

func printScores(w interface{ Write([]byte) (int, error) }, scores []prdozer.HostHealth) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tCORES\tLOAD1\tLOAD/CORE\tDISK_GB\tWINDOWS\tLEASES\tPRDOZER\tSELF\tSTATUS")
	for i := range scores {
		h := &scores[i]
		status := "ok"
		if ok, why := prdozer.Eligible(*h, prdozer.ProbeOptions{}); !ok {
			status = why
		}
		fmt.Fprintf(tw, "%s\t%d\t%.2f\t%.2f\t%d\t%d\t%d\t%t\t%t\t%s\n",
			h.Host, h.Cores, h.Load1, h.LoadPerCore(), h.UsableDiskGB(),
			h.TmuxWindows, h.Leases(), h.HasBinary, h.IsSelf, status)
	}
	_ = tw.Flush()
}

func newRunsCmd() *cobra.Command {
	var (
		repoFilter string
		prFilter   int
		asJSON     bool
	)
	cmd := &cobra.Command{
		Use:   "runs",
		Short: "List babysit runs recorded on this box",
		RunE: func(cmd *cobra.Command, _ []string) error {
			runs, err := prdozer.ListRuns()
			if err != nil {
				return err
			}
			filtered := make([]prdozer.RunMeta, 0, len(runs))
			for i := range runs {
				r := &runs[i]
				if repoFilter != "" && r.Repo != repoFilter {
					continue
				}
				if prFilter != 0 && r.PRNumber != prFilter {
					continue
				}
				filtered = append(filtered, *r)
			}
			// The table is for humans; --json is for the ops skill, which
			// otherwise has to parse tabwriter columns over ssh.
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(filtered)
			}
			runs = filtered
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "STARTED\tREPO\tPR\tRUN\tSTATE\tPOLISH\tMERGES\tLOGS")
			for i := range runs {
				r := &runs[i]
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%d\t%d\t%s\n",
					r.StartedAt.Format(time.RFC3339), r.Repo, r.PRNumber, r.RunID,
					r.State, r.PolishRounds, r.MergeAttempt, r.LogDir)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&repoFilter, "repo", "", "Only list runs for this owner/repo")
	cmd.Flags().IntVar(&prFilter, "pr", 0, "Only list runs for this PR number")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit run metadata as JSON")
	return cmd
}

func newGCCmd() *cobra.Command {
	var (
		ttl          time.Duration
		runLogTTL    time.Duration
		dryRun       bool
		force        bool
		registryPath string
	)
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Reap orphaned ephemeral worktrees and expired run logs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app := cliapp.FromContext(cmd.Context())
			var roots []string
			if reg, err := prdozer.LoadRegistry(registryPath); err == nil {
				for _, name := range reg.RepoNames() {
					if entry, rerr := reg.Resolve(name); rerr == nil && entry.WorktreeRoot != "" {
						roots = append(roots, entry.WorktreeRoot)
					}
				}
			} else {
				app.Logger.Warn("could not load registry; sweeping only the plain-clone root", "error", err)
			}

			res, err := prdozer.RunGC(cmd.Context(), &wt.DefaultGitRunner{}, prdozer.GCOptions{
				WorktreeRoots: roots,
				WorktreeTTL:   ttl,
				RunLogTTL:     runLogTTL,
				DryRun:        dryRun,
				Force:         force,
			}, app.Logger)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ACTION\tKIND\tPATH\tREASON")
			for _, c := range res.Candidates {
				// Key off Eligible, not dryRun: a dry run removes nothing, so
				// every row would otherwise read as a pending deletion — including
				// the ones being deliberately kept.
				action := "skip"
				switch {
				case c.Removed:
					action = "removed"
				case c.Eligible:
					action = "would-remove"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", action, c.Kind, c.Path, c.Reason)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "\n%d would be removed, %d skipped (dry run: nothing was removed)\n",
					res.Eligible, res.Skipped)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "\n%d removed, %d skipped\n", res.Removed, res.Skipped)
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&ttl, "ttl", prdozer.DefaultWorktreeTTL, "Reap ephemeral worktrees older than this")
	cmd.Flags().DurationVar(&runLogTTL, "run-log-ttl", prdozer.DefaultRunLogTTL, "Prune run logs older than this")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print what would be removed and why, without removing")
	cmd.Flags().BoolVar(&force, "force", false, "Remove even worktrees holding unpushed work (dangerous)")
	cmd.Flags().StringVar(&registryPath, "registry", prdozer.DefaultRegistryPath, "Path to the repo registry")
	return cmd
}
