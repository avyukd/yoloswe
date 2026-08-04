package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/cliapp"
	"github.com/bazelment/yoloswe/fleet"
	"github.com/bazelment/yoloswe/jiradozer"
)

// jiradozerTool describes this binary to the shared fleet dispatcher.
var jiradozerTool = fleet.Tool{Name: "jiradozer", LeaseDir: jiradozer.LeaseDir}

// newDispatchCmd places one task on the best available box.
//
// The split is deliberate: this decides WHERE, and `exec` does the work. What
// to dispatch, how many at once, and when to retry stay with the caller — a
// skill or a human — because those are judgment calls that change per batch,
// while host selection has a right answer that belongs in code.
func newDispatchCmd(x *execArgs) *cobra.Command {
	var (
		host      string
		here      bool
		dryRun    bool
		minDiskGB int
		skipQuota bool
	)
	cmd := &cobra.Command{
		Use:   "dispatch",
		Short: "Run one task on the best available devbox",
		Long: "Probes the fleet, ranks it, and starts `jiradozer exec` there under tmux.\n\n" +
			"When the winner is this box the worker runs IN-PROCESS, so the command\n" +
			"exiting means the run FINISHED — not that it was handed off.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app := cliapp.FromContext(cmd.Context())
			ctx := cmd.Context()

			if (x.issueID == "") == (x.description == "") {
				return errors.New("exactly one of --issue or --description is required")
			}
			if x.repo == "" {
				return errors.New("--repo is required")
			}

			// Quota is a fleet-GLOBAL gate, never a per-host ranking signal: the
			// OAuth session is shared across every box, so a run dispatched under
			// an exhausted quota dies mid-step wherever it lands.
			if !skipQuota && !dryRun {
				if usage, err := fleet.CheckClaudeQuota(ctx); err != nil {
					app.Logger.Warn("could not read claude quota; proceeding", "error", err)
				} else if usage.Exhausted() {
					return fmt.Errorf("claude quota exhausted: five-hour utilization is %.0f%% (use --skip-quota-check to override)",
						usage.FiveHourUtilization)
				}
			}

			if here {
				return runExec(ctx, app, *x)
			}

			hosts, err := fleet.Load(fleet.DefaultFleetDir)
			if err != nil {
				return err
			}
			if host != "" {
				hosts = filterHosts(hosts, host)
				if len(hosts) == 0 {
					return fmt.Errorf("no fleet host named %q", host)
				}
			}

			selfHostname, _ := os.Hostname()
			opts := fleet.ProbeOptions{
				SelfDNS:      fleet.SelfPublicDNS(fleet.DevboxConfigPath),
				SelfHostname: selfHostname,
				MinDiskGB:    minDiskGB,
			}
			ssh := fleet.DefaultSSHRunner{}
			scores := fleet.Probe(ctx, ssh, jiradozerTool, hosts, opts)

			// Refuse a second run for a task some box is already working. The
			// lock label catches this cross-host too, but only after the worktree
			// exists; the lease is the cheaper and earlier signal.
			//
			// The target is derived the SAME way the worker will derive it — a
			// --description run with no --task-id included. Deriving it any other
			// way here would silently disable this check for exactly the case
			// that has no tracker-side claim to fall back on.
			target := leaseTarget(*x)
			if target != "" {
				if holder, busy := fleet.FindLeaseHolder(scores, target); busy {
					return fmt.Errorf("%s is already running on %s (lease held); use `jiradozer runs --issue %s --json` there to check on it",
						target, holder.Host, target)
				}
			}

			chosen, err := fleet.PickHost(scores, jiradozerTool, opts)
			if err != nil {
				// Print the table even on failure: "no eligible host" is only
				// actionable if you can see why each was rejected.
				printScores(cmd.OutOrStdout(), scores, opts)
				return err
			}

			req := fleet.Request{
				Host:        chosen,
				SessionName: tmuxSessionName(target, x.repo),
				Args:        dispatchArgs(*x, tmuxSessionName(target, x.repo)),
			}

			if dryRun {
				printScores(cmd.OutOrStdout(), scores, opts)
				fmt.Fprintf(cmd.OutOrStdout(), "\nchosen: %s\n", chosen.Host)
				if chosen.IsSelf {
					fmt.Fprintf(cmd.OutOrStdout(), "would run IN-PROCESS (chosen host is this box)\n")
					return nil
				}
				fmt.Fprintf(cmd.OutOrStdout(), "would run:\n%s\n", req.SSHCommand())
				return nil
			}

			if chosen.IsSelf {
				app.Logger.Info("chosen host is this box; running in-process", "host", chosen.Host)
				return runExec(ctx, app, *x)
			}
			if err := fleet.Dispatch(ctx, ssh, jiradozerTool, req, app.Logger); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "dispatched %s to %s (tmux session %q)\n",
				target, chosen.Host, req.SessionName)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&x.issueID, "issue", "", "Issue identifier to work (e.g. INF-1234)")
	f.StringVar(&x.description, "description", "", "Ad-hoc task description")
	f.StringVar(&x.taskID, "task-id", "", "Correlation id from the dispatcher's task list")
	f.StringVar(&x.repo, "repo", "", "wt-managed repository name (required)")
	f.StringVar(&x.branchPrefix, "branch-prefix", "", "Override source.branch_prefix")
	f.StringVar(&x.modelID, "model", "", "Override agent.model")
	f.StringVar(&x.skipPhases, "skip-phases", "", "Comma-separated phases to skip")
	f.StringVar(&x.autoApprove, "auto-approve", "", "Auto-approve review gates")
	f.Float64Var(&x.maxBudget, "max-budget", 0, "Override max_budget_usd")
	f.BoolVar(&x.force, "force", false, "Proceed even when the issue is already claimed")
	f.StringVar(&host, "host", "", "Pin a specific devbox instead of ranking the fleet")
	f.BoolVar(&here, "here", false, "Run on this box without probing")
	f.BoolVar(&dryRun, "dry-run", false, "Print the score table and the exact SSH command, then exit")
	f.IntVar(&minDiskGB, "min-disk-gb", 0, "Minimum usable free disk to accept a host (default 40)")
	f.BoolVar(&skipQuota, "skip-quota-check", false, "Dispatch even when the shared Claude quota is nearly spent")
	return cmd
}

// dispatchArgs renders the remote `jiradozer exec` invocation. Values are
// passed raw; the fleet dispatcher quotes them.
func dispatchArgs(x execArgs, session string) []string {
	args := []string{"exec", "--repo", x.repo}
	// --config MUST be forwarded. The remote worker starts with cwd $HOME, so
	// the default relative "jiradozer.yaml" resolves to nothing there and the
	// run dies before doing any work.
	//
	// It is forwarded HOME-RELATIVE whenever it lives under this user's home.
	// The homes differ per box — the Azure devbox runs as "ming", the AWS boxes
	// as "ubuntu" — while ~/magent is synced fleet-wide, so a local absolute
	// path names a file that exists on no other machine. This matters even when
	// the caller typed a tilde, because their shell expanded it before jiradozer
	// ever saw the value.
	if x.configPath != "" {
		args = append(args, "--config", portableConfigPath(x.configPath))
	}
	if x.issueID != "" {
		args = append(args, "--issue", x.issueID)
	}
	if x.description != "" {
		args = append(args, "--description", x.description)
	}
	// An ordered slice, NOT a map: Go randomizes map iteration, which would
	// make the rendered command differ between identical invocations — so a
	// --dry-run could never be compared against what actually ran.
	for _, kv := range [][2]string{
		{"--task-id", x.taskID},
		{"--branch-prefix", x.branchPrefix},
		{"--model", x.modelID},
		{"--skip-phases", x.skipPhases},
		{"--auto-approve", x.autoApprove},
	} {
		if kv[1] != "" {
			args = append(args, kv[0], kv[1])
		}
	}
	if x.maxBudget > 0 {
		args = append(args, "--max-budget", fmt.Sprintf("%.2f", x.maxBudget))
	}
	if x.force {
		args = append(args, "--force")
	}
	// Record the session in the run-log so a reader can attach to it. Without
	// this the run knows nothing about how it was started.
	args = append(args, "--tmux-session", session)
	return args
}

// portableConfigPath rewrites a path so it resolves on the target box: paths
// under this user's home become ~-relative for the worker to expand against
// its own home, and anything else is made absolute since a relative path means
// nothing from $HOME.
func portableConfigPath(p string) string {
	if strings.HasPrefix(p, "~") {
		return p
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, err := filepath.Rel(home, abs); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Join("~", rel)
		}
	}
	return abs
}

// tmuxSessionName follows the fleet convention so the window is recognisable
// in a session list.
func tmuxSessionName(target, repo string) string {
	if target == "" {
		target = "adhoc"
	}
	return fmt.Sprintf("jiradozer/%s/%s", repo, target)
}

func filterHosts(hosts []fleet.Host, name string) []fleet.Host {
	var out []fleet.Host
	for i := range hosts {
		if strings.EqualFold(hosts[i].Hostname, name) || strings.EqualFold(hosts[i].PublicDNS, name) {
			out = append(out, hosts[i])
		}
	}
	return out
}

func printScores(w interface{ Write([]byte) (int, error) }, scores []fleet.HostHealth, opts fleet.ProbeOptions) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tLOAD/CORE\tDISK\tTMUX\tLEASES\tBIN\tSELF\tSTATUS")
	for i := range scores {
		h := &scores[i]
		status := "eligible"
		if ok, why := h.Eligible(jiradozerTool, opts); !ok {
			status = why
		}
		fmt.Fprintf(tw, "%s\t%.2f\t%dGB\t%d\t%d\t%t\t%t\t%s\n",
			h.Host, h.LoadPerCore(), h.UsableDiskGB(),
			h.TmuxWindows, h.Leases(), h.HasBinary, h.IsSelf, status)
	}
	_ = tw.Flush()
}

// newFleetCmd groups the cross-host views.
func newFleetCmd() *cobra.Command {
	fleetCmd := &cobra.Command{Use: "fleet", Short: "Inspect the devbox fleet"}
	fleetCmd.AddCommand(newFleetStatusCmd(), newFleetRunsCmd())
	return fleetCmd
}

func newFleetStatusCmd() *cobra.Command {
	var minDiskGB int
	return &cobra.Command{
		Use:   "status",
		Short: "Probe every devbox and print load, disk, tmux windows, and held leases",
		RunE: func(cmd *cobra.Command, _ []string) error {
			hosts, err := fleet.Load(fleet.DefaultFleetDir)
			if err != nil {
				return err
			}
			selfHostname, _ := os.Hostname()
			opts := fleet.ProbeOptions{
				SelfDNS:      fleet.SelfPublicDNS(fleet.DevboxConfigPath),
				SelfHostname: selfHostname,
				MinDiskGB:    minDiskGB,
			}
			scores := fleet.Probe(cmd.Context(), fleet.DefaultSSHRunner{}, jiradozerTool, hosts, opts)
			printScores(cmd.OutOrStdout(), scores, opts)
			return nil
		},
	}
}

// newFleetRunsCmd answers "what is every dispatched task doing right now".
//
// This is the gather half of scatter-gather. Run-logs are per-host local files,
// so without it a caller has to iterate hosts by hand and re-derive which is
// which.
func newFleetRunsCmd() *cobra.Command {
	var (
		asJSON     bool
		activeOnly bool
		issue      string
	)
	cmd := &cobra.Command{
		Use:   "runs",
		Short: "Aggregate `jiradozer runs` across every devbox",
		RunE: func(cmd *cobra.Command, _ []string) error {
			hosts, err := fleet.Load(fleet.DefaultFleetDir)
			if err != nil {
				return err
			}
			var extra []string
			if activeOnly {
				extra = append(extra, "--active")
			}
			if issue != "" {
				extra = append(extra, "--issue", issue)
			}
			results := fleet.GatherRuns(cmd.Context(), fleet.DefaultSSHRunner{}, jiradozerTool, hosts, extra...)

			type row struct {
				jiradozer.RunMeta
				FleetHost string `json:"fleet_host"`
			}
			var rows []row
			var failures []fleet.HostRuns
			// Unreadable ROWS are surfaced for the same reason unreadable HOSTS
			// are: silently dropping one makes a truncated or corrupt reply look
			// like a box with fewer runs than it actually has.
			unreadable := map[string]int{}
			for _, hr := range results {
				if hr.Err != nil {
					// Kept, not dropped: an unreachable box looks identical to an
					// idle one otherwise, and "nothing is running anywhere" is a
					// conclusion a caller acts on.
					failures = append(failures, hr)
					continue
				}
				for _, raw := range hr.Runs {
					var m jiradozer.RunMeta
					if err := json.Unmarshal(raw, &m); err != nil {
						unreadable[hr.Host]++
						continue
					}
					rows = append(rows, row{RunMeta: m, FleetHost: hr.Host})
				}
			}

			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if err := enc.Encode(rows); err != nil {
					return err
				}
			} else {
				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "HOST\tTARGET\tREPO\tRUN\tSTATE\tPHASE\tBRANCH")
				for i := range rows {
					r := &rows[i]
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
						r.FleetHost, r.Target(), r.Repo, r.RunID, r.State, orDash(r.Phase), orDash(r.Branch))
				}
				if err := tw.Flush(); err != nil {
					return err
				}
			}

			// Never let an unreadable host pass silently: a partial view that
			// reads as complete is how "no runs anywhere" becomes a wrong answer.
			for _, f := range failures {
				fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: %s could not be read: %v\n", f.Host, f.Err)
			}
			// Sorted so two identical fleets print identical output; Go randomizes
			// map iteration.
			for _, host := range slices.Sorted(maps.Keys(unreadable)) {
				fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: skipped %d unparseable run record(s) from %s\n",
					unreadable[host], host)
			}
			if len(failures) > 0 {
				return fmt.Errorf("%d of %d hosts could not be read; this view is incomplete",
					len(failures), len(results))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit merged run metadata as JSON")
	cmd.Flags().BoolVar(&activeOnly, "active", false, "Only list runs that have not reached a terminal state")
	cmd.Flags().StringVar(&issue, "issue", "", "Only list runs for this issue identifier or task id")
	return cmd
}
