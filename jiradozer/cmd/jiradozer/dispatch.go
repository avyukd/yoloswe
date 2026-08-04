package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
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

			// --here skips host SELECTION. It must not skip the two things below,
			// and it used to skip both by returning here.
			if here {
				// The duplicate guard is a safety property, not part of host
				// selection. For a --description run the fleet lease is the ONLY
				// cross-host exclusion there is — no tracker label backs it up —
				// so bypassing it let a second local run start while another box
				// was already working the task.
				//
				// It runs BEFORE the dry-run return so a preview answers the
				// question a preview is for: would this run. Printing "would run"
				// without checking makes --dry-run --here disagree with the real
				// --here run it is previewing, and the plain dry-run path in this
				// same command already guards first (narrowToPin, below) — a
				// preview that skips the check is how you find out about a held
				// lease only after committing.
				if err := guardDuplicateRun(ctx, *x, minDiskGB, app.Logger); err != nil {
					return err
				}
				// A dry run must never execute, whatever the dispatch shape: the
				// moment `--dry-run --here` does the work, --dry-run has stopped
				// meaning dry-run and there is no safe way to preview anything.
				if dryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "would run IN-PROCESS on this box (--here)\n")
					return nil
				}
				return runExec(ctx, app, *x)
			}

			hosts, err := fleet.Load(fleet.DefaultFleetDir)
			if err != nil {
				return err
			}
			// A typo'd --host is answered from the loaded fleet, before any ssh
			// round trip. The fleet itself is NOT narrowed here: the duplicate
			// guard below has to see every box, so the pin is applied to the
			// probe RESULTS instead (see narrowToPin).
			if host != "" && len(filterHosts(hosts, host)) == 0 {
				return fmt.Errorf("no fleet host named %q", host)
			}

			selfHostname, _ := os.Hostname()
			opts := fleet.ProbeOptions{
				SelfDNS:      fleet.SelfPublicDNS(fleet.DevboxConfigPath),
				SelfHostname: selfHostname,
				MinDiskGB:    minDiskGB,
			}
			ssh := fleet.DefaultSSHRunner{}
			scores := fleet.Probe(ctx, ssh, jiradozerTool, hosts, opts)

			// The target is derived the SAME way the worker will derive it — a
			// --description run with no --task-id included. Deriving it any other
			// way here would silently disable this check for exactly the case
			// that has no tracker-side claim to fall back on.
			target := leaseTarget(*x)
			scores, err = narrowToPin(scores, target, host, x.force)
			if err != nil {
				return err
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
	f.BoolVar(&x.force, "force", false, "Proceed past a stale claim, or past a fleet that could not be fully probed (never past a lease actually held)")
	f.StringVar(&host, "host", "", "Pin a specific devbox instead of ranking the fleet")
	f.BoolVar(&here, "here", false, "Run on this box, skipping host selection (the duplicate-run guard still applies)")
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

// guardDuplicateRun applies the fleet-wide duplicate check without selecting a
// host, so --here gets the same protection a dispatching run gets.
//
// It reuses narrowToPin with an empty pin rather than re-deriving the rule:
// the ordering inside there (check every box, THEN narrow) is the whole point,
// and a second copy of the check is how the two paths drift apart.
//
// A box with no fleet inventory at all has no cross-host concern, so an ABSENT
// registry degrades to a no-op — but only that one case. Any other load failure
// (malformed entry, permission denied) means the fleet view is WRONG rather than
// empty, and fleet.Load is explicit that a partial view must not be treated as a
// complete one. Downgrading those to a warning is the same fail-open shape this
// function exists to remove: a guard that answers "nobody is running it" because
// it could not look is indistinguishable from one that checked.
// The fleet dir and ssh runner are indirected so a test can drive the REAL
// `--here` branch. The guard's whole value is that it runs on that path, and a
// probe hardcoded to DefaultSSHRunner can only be exercised by reaching a live
// fleet — which means the one ordering that matters would go untested.
var (
	guardFleetDir = fleet.DefaultFleetDir
	guardSSH      = fleet.SSHRunner(fleet.DefaultSSHRunner{})
)

func guardDuplicateRun(ctx context.Context, x execArgs, minDiskGB int, logger *slog.Logger) error {
	target := leaseTarget(x)
	if target == "" {
		return nil
	}
	hosts, err := fleet.Load(guardFleetDir)
	if errors.Is(err, fs.ErrNotExist) {
		logger.Warn("no fleet inventory on this box; cross-host duplicate guard skipped",
			"target", target, "error", err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot rule out a second run of %s: %w", target, err)
	}
	selfHostname, _ := os.Hostname()
	scores := fleet.Probe(ctx, guardSSH, jiradozerTool, hosts, fleet.ProbeOptions{
		SelfDNS:      fleet.SelfPublicDNS(fleet.DevboxConfigPath),
		SelfHostname: selfHostname,
		MinDiskGB:    minDiskGB,
	})
	_, err = narrowToPin(scores, target, "", x.force)
	return err
}

// narrowToPin runs the duplicate-run guard against the WHOLE probed fleet and
// only then applies --host.
//
// The order is the point, which is why both halves live in one function rather
// than as two calls a future edit could reorder. The guard refuses a second run
// for a task some box is already working: the lock label catches that
// cross-host too, but only after the worktree exists, so the lease is the
// cheaper and earlier signal. Narrowing first would have made --host a way to
// bypass it — pin the idle box and the busy one is simply not in the slice
// being searched — and for a --description run, which has no tracker-side claim
// to fall back on, the lease is the ONLY fleet-wide exclusion there is.
func narrowToPin(scores []fleet.HostHealth, target, pin string, force bool) ([]fleet.HostHealth, error) {
	if target != "" {
		if holder, busy := fleet.FindLeaseHolder(scores, target); busy {
			// Not overridable by --force. A held lease is a live worker on a
			// box that answered, which is a different thing from the stale
			// tracker label --force exists to wave through.
			return nil, fmt.Errorf("%s is already running on %s (lease held); use `jiradozer runs --issue %s --json` there to check on it",
				target, holder.Host, target)
		}
		// "Nobody holds it" is only an ANSWER if every box was asked. A host
		// whose probe failed reports no leases at all — indistinguishable from
		// a host that genuinely holds none — so a worker on an ssh-down box is
		// invisible here, and for a --description run this lease is the only
		// cross-host exclusion there is. Fail closed, exactly as `fleet runs`
		// does when part of the fleet cannot be read: a partial view that reads
		// as complete is how "nothing is running" becomes a wrong answer.
		if down := unreachableHosts(scores); len(down) > 0 && !force {
			return nil, fmt.Errorf("cannot rule out a second run of %s: %d of %d hosts could not be probed (%s); retry when they answer, or pass --force to dispatch on an incomplete fleet view",
				target, len(down), len(scores), strings.Join(down, ", "))
		}
	}
	if pin == "" {
		return scores, nil
	}
	var out []fleet.HostHealth
	for i := range scores {
		if strings.EqualFold(scores[i].Host, pin) || strings.EqualFold(scores[i].PublicDNS, pin) {
			out = append(out, scores[i])
		}
	}
	if len(out) == 0 {
		// Unreachable from dispatch: the pin was matched against these same two
		// fields in the loaded fleet before probing. Say so rather than let
		// PickHost report "no eligible host", which would send an operator
		// looking at disk and load for a name that simply did not match.
		return nil, fmt.Errorf("host %q matched the fleet but not its probe results", pin)
	}
	return out, nil
}

// unreachableHosts names the boxes whose probe did not come back, so the caller
// can say WHICH part of the fleet it could not see. Sorted, because the probe
// runs concurrently and an error message that reorders between identical runs
// is one an operator cannot diff.
func unreachableHosts(scores []fleet.HostHealth) []string {
	var out []string
	for i := range scores {
		if !scores[i].Reachable {
			out = append(out, scores[i].Host)
		}
	}
	slices.Sort(out)
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
