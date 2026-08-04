package main

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/jiradozer"
)

// newRunsCmd lists the run-logs recorded on this box.
//
// --json is the form the dispatcher skill uses: it reads this over ssh per
// host, so the output has to be parseable without column-guessing. The table
// is for a human reading one box.
func newRunsCmd() *cobra.Command {
	var (
		issueFilter string
		repoFilter  string
		activeOnly  bool
		asJSON      bool
	)
	cmd := &cobra.Command{
		Use:   "runs",
		Short: "List task runs recorded on this box",
		Long: "List the run-logs under " + jiradozer.RunsRoot + ", newest first.\n\n" +
			"STALE marks a run whose state is still `running` but whose heartbeat has\n" +
			"stopped — the shape a SIGKILL, an OOM, a reboot or a deallocated VM leaves\n" +
			"behind. State alone cannot distinguish that from a healthy long run.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			runs, err := jiradozer.ListRuns()
			if err != nil {
				return err
			}

			filtered := make([]jiradozer.RunMeta, 0, len(runs))
			for i := range runs {
				r := &runs[i]
				if issueFilter != "" && r.IssueIdentifier != issueFilter && r.TaskID != issueFilter {
					continue
				}
				if repoFilter != "" && r.Repo != repoFilter {
					continue
				}
				if activeOnly && r.State.IsTerminal() {
					continue
				}
				filtered = append(filtered, *r)
			}

			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(filtered)
			}

			now := time.Now().UTC()
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "STARTED\tTARGET\tREPO\tRUN\tSTATE\tPHASE\tHEARTBEAT\tWORKTREE")
			for i := range filtered {
				r := &filtered[i]
				state := string(r.State)
				if stale := r.StaleFor(now); stale > 0 {
					state = fmt.Sprintf("%s(STALE %s)", r.State, stale.Truncate(time.Second))
				}
				heartbeat := "-"
				if !r.HeartbeatAt.IsZero() {
					heartbeat = r.HeartbeatAt.Format(time.RFC3339)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					r.StartedAt.Format(time.RFC3339), r.Target(), r.Repo, r.RunID,
					state, orDash(r.Phase), heartbeat, orDash(r.WorktreePath))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&issueFilter, "issue", "", "Only list runs for this issue identifier or task id")
	cmd.Flags().StringVar(&repoFilter, "repo", "", "Only list runs for this repo")
	cmd.Flags().BoolVar(&activeOnly, "active", false, "Only list runs that have not reached a terminal state")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit run metadata as JSON")
	return cmd
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
