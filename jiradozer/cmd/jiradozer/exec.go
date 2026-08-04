package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/cliapp"
	"github.com/bazelment/yoloswe/jiradozer"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
	"github.com/bazelment/yoloswe/jiradozer/tracker/local"
	"github.com/bazelment/yoloswe/wt"
)

// execArgs are the flags for a single self-contained task execution.
type execArgs struct {
	issueID      string
	description  string
	taskID       string
	repo         string
	configPath   string
	branchPrefix string
	modelID      string
	skipPhases   string
	autoApprove  string
	tmuxSession  string
	maxBudget    float64
	force        bool
	keepWorktree bool
}

// newExecCmd builds the worker that a dispatcher places on a box.
//
// It is deliberately self-contained: everything the team-mode orchestrator does
// around a child subprocess — claim the issue, create the worktree, record what
// happened, report a failure — happens inside this one process instead. That is
// what lets it be dropped on any host with no supervisor watching it.
func newExecCmd(args *execArgs) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "exec",
		Short: "Run one task end-to-end on this box",
		Long: "Claim an issue, create its worktree, run the workflow, and record the outcome.\n\n" +
			"Unlike `run`, this owns the whole lifecycle, so it needs no parent process.\n" +
			"Progress is written to a run-log (see `jiradozer runs`) and mirrored to the\n" +
			"issue as a start and an end comment.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app := cliapp.FromContext(cmd.Context())
			return runExec(cmd.Context(), app, *args)
		},
	}
	f := cmd.Flags()
	f.StringVar(&args.issueID, "issue", "", "Issue identifier to work (e.g. INF-1234)")
	f.StringVar(&args.description, "description", "", "Ad-hoc task description; uses the local tracker instead of an issue")
	f.StringVar(&args.taskID, "task-id", "", "Correlation id from the dispatcher's task list")
	f.StringVar(&args.repo, "repo", "", "wt-managed repository name (required)")
	f.StringVar(&args.branchPrefix, "branch-prefix", "", "Override source.branch_prefix")
	f.StringVar(&args.modelID, "model", "", "Override agent.model")
	f.StringVar(&args.skipPhases, "skip-phases", "", "Comma-separated phases to skip")
	f.StringVar(&args.autoApprove, "auto-approve", "", "Auto-approve review gates")
	f.StringVar(&args.tmuxSession, "tmux-session", "", "Record the tmux session this run was dispatched into")
	f.Float64Var(&args.maxBudget, "max-budget", 0, "Override max_budget_usd")
	f.BoolVar(&args.force, "force", false, "Proceed even when the issue is already claimed by another host")
	f.BoolVar(&args.keepWorktree, "keep-worktree", false, "Never reclaim the worktree (default already keeps it)")
	return cmd
}

// loadExecConfig reuses run's loader so exec and run cannot drift in how they
// interpret a config file, then applies exec's own overrides.
//
// --description forces the local tracker and auto-approves every gate, matching
// run's behaviour: an ad-hoc task has no human watching a review gate, and a
// dispatched one has no terminal to watch it from either.
func loadExecConfig(args execArgs) (*jiradozer.Config, error) {
	cfg, err := loadRunConfig(runArgs{
		configPath:  args.configPath,
		description: args.description,
		issueID:     args.issueID,
		modelID:     args.modelID,
		maxBudget:   args.maxBudget,
		skipPhases:  args.skipPhases,
		autoApprove: args.autoApprove,
	})
	if err != nil {
		return nil, err
	}
	if args.branchPrefix != "" {
		cfg.Source.BranchPrefix = args.branchPrefix
	}
	return cfg, nil
}

// resolveExecWTManager builds a wt.Manager for an explicitly named repo.
//
// The repo cannot be inferred here the way `run` infers it. wt.GetCurrentRepoName
// resolves from os.Getwd() and falls back to `git remote get-url origin` in the
// cwd — but a dispatched worker starts under `tmux new-session -d`, whose cwd is
// $HOME: not a wt worktree, not a git repo. Team mode never hit this because its
// supervisor always ran from inside the repo. So --repo is required, and saying
// so plainly beats failing later with "not in a wt-managed repository".
func resolveExecWTManager(repo string) (*wt.Manager, error) {
	if repo == "" {
		return nil, errors.New("--repo is required: a dispatched worker starts in $HOME, so the repository cannot be inferred from the working directory")
	}
	root, err := resolveWTRoot()
	if err != nil {
		return nil, err
	}
	return wt.NewManager(root, repo), nil
}

func runExec(ctx context.Context, app *cliapp.App, args execArgs) (runErr error) {
	logger := app.Logger

	// Under an orchestrator, children suppress their own failure report because
	// the parent is the single reporter. exec has no parent, so inheriting that
	// variable would silence every failure on the fleet with nothing left to
	// speak for it. Refuse rather than run un-alerted.
	if os.Getenv(jiradozer.OrchestratedEnvVar) != "" {
		return fmt.Errorf("%s is set: exec owns its own lifecycle and must not run under an orchestrator", jiradozer.OrchestratedEnvVar)
	}
	if (args.issueID == "") == (args.description == "") {
		return errors.New("exactly one of --issue or --description is required")
	}

	wtMgr, err := resolveExecWTManager(args.repo)
	if err != nil {
		return err
	}

	cfg, err := loadExecConfig(args)
	if err != nil {
		return err
	}

	runID, err := jiradozer.NewRunID()
	if err != nil {
		return err
	}

	// The lease target is whatever names this task on this box. It is taken
	// FIRST: it is kernel-backed, so it is the one claim that is correct even
	// when this process is SIGKILLed.
	lease, err := jiradozer.AcquireLease(leaseTarget(args))
	if err != nil {
		return err
	}
	// Released last, after the run-log is final, so a reader that observes "no
	// lease held" also observes a settled meta.json rather than a torn view.
	defer func() {
		if err := lease.Release(); err != nil {
			logger.Warn("failed to release lease", "path", lease.Path(), "error", err)
		}
	}()

	x := &execRun{
		app:            app,
		logger:         logger,
		cfg:            cfg,
		args:           args,
		wtMgr:          wtMgr,
		runID:          runID,
		lookupPR:       defaultPRLookup,
		removeWorktree: discardRemover(wtMgr),
	}

	// exec owns its own failure reporting, for the same reason it refuses to run
	// under an orchestrator: nothing else is watching. The EXTERNAL sink is the
	// one that matters here — finish() already writes the error to the run-log
	// and posts it as the end comment, so passing a tracker poster as well would
	// duplicate that comment on every failure.
	defer func() {
		if !shouldReportFailure(runErr) {
			return
		}
		var notifier jiradozer.Notifier
		if cfg.Notify.SlackWebhook != "" {
			notifier = jiradozer.SlackWebhookNotifier{WebhookURL: cfg.Notify.SlackWebhook}
		}
		jiradozer.ReportFailure(ctx, logger, nil, "", notifier, jiradozer.FailureReport{
			Tool:          "jiradozer",
			Target:        x.reportTarget(),
			Step:          jiradozer.FailingStepFromError(runErr),
			Err:           runErr,
			BuildRevision: app.Build.ShortRevision(),
			LogPath:       app.LogPath,
		})
	}()

	return x.run(ctx)
}

// execRun holds one task execution's resolved dependencies.
type execRun struct {
	app     *cliapp.App
	logger  *slog.Logger
	cfg     *jiradozer.Config
	wtMgr   *wt.Manager
	rl      *jiradozer.RunLog
	tracker tracker.IssueTracker
	issue   *tracker.Issue
	// lookupPR resolves the PR opened for a branch. Injected so the recording
	// path can be tested without a GitHub round trip.
	lookupPR func(ctx context.Context, branch, dir string) (*wt.PRInfo, error)
	// removeWorktree tears a checkout down. Injected for the same reason: the
	// teardown path only runs when something else already failed, so it needs a
	// test that does not depend on a real repository.
	removeWorktree func(ctx context.Context, branch string) error
	runID          string
	branch         string
	args           execArgs
}

// defaultPRLookup asks gh which PR exists for a branch, from inside the
// worktree so the repository is unambiguous.
func defaultPRLookup(ctx context.Context, branch, dir string) (*wt.PRInfo, error) {
	return wt.GetPRByBranch(ctx, &wt.DefaultGHRunner{}, branch, dir)
}

// reportTarget names this run in a failure alert. It prefers the resolved issue
// identifier, then whatever the caller named, so an alert is never anonymous
// even when the failure happened before the issue was fetched.
func (x *execRun) reportTarget() string {
	if x.issue != nil && x.issue.Identifier != "" {
		return x.issue.Identifier
	}
	if x.args.issueID != "" {
		return x.args.issueID
	}
	if x.args.taskID != "" {
		return x.args.taskID
	}
	return describeTarget(x.args.description)
}

func (x *execRun) run(ctx context.Context) (runErr error) {
	// For a tracker-backed run the client is needed before the worktree, to
	// fetch the issue and read its claim. For --description the local tracker
	// is rooted inside the run directory, which does not exist yet, so it is
	// built later. The two orders are why exec cannot reuse run()'s flow.
	if x.args.issueID != "" {
		t, err := createTracker(x.cfg, x.args.issueID)
		if err != nil {
			return err
		}
		x.tracker = t

		issue, err := t.FetchIssue(ctx, x.args.issueID)
		if err != nil {
			return fmt.Errorf("fetch issue: %w", err)
		}
		x.issue = issue
		if err := x.checkNotClaimed(issue); err != nil {
			return err
		}
	}

	if err := x.createWorktree(ctx); err != nil {
		return err
	}

	if err := x.startRunLog(); err != nil {
		// The run-log IS gc's ownership namespace — a worktree no run-log claims
		// is never a candidate, because these sit beside human-owned ones with
		// nothing else marking them as jiradozer's. So a worktree that outlives
		// a failed startRunLog is orphaned PERMANENTLY. Nothing has run in it
		// yet, so tearing it down here loses nothing.
		x.discardUnclaimedWorktree(ctx)
		return err
	}

	// From here the run is recorded, so every exit path settles the run-log.
	defer func() { x.finish(runErr) }()

	heartbeatCtx, stopBeating := context.WithCancel(ctx)
	stopHeartbeat := x.rl.StartHeartbeat(heartbeatCtx, jiradozer.HeartbeatInterval, func(err error) {
		x.logger.Warn("heartbeat write failed", "error", err)
	})
	defer func() {
		stopBeating()
		// Wait for the beat goroutine before finish() writes the terminal meta,
		// or a late beat could resurrect the heartbeat of a finished run.
		stopHeartbeat()
	}()

	if x.args.description != "" {
		if err := x.createLocalIssue(); err != nil {
			return err
		}
	}

	x.claim(ctx)
	x.postStartComment(ctx)

	wf := jiradozer.NewWorkflow(x.tracker, x.issue, x.cfg, x.logger)
	wf.SetRenderer(x.app.Renderer)
	wf.OnTransition = x.recordPhase
	return wf.Run(ctx)
}

// recordPhase mirrors a workflow step into the run-log.
//
// Phase is the only thing that makes a remote worker's progress readable from
// another box: `jiradozer fleet runs` reads meta.json over ssh and can see
// neither this host's log nor its terminal. Best-effort — a run must not die
// because a status write failed.
func (x *execRun) recordPhase(step jiradozer.WorkflowStep) {
	if err := x.rl.UpdateMeta(func(m *jiradozer.RunMeta) { m.Phase = step.String() }); err != nil {
		x.logger.Warn("failed to record workflow phase", "step", step, "error", err)
	}
}

// checkNotClaimed turns the lock label into an actual check.
//
// The label has always been written and removed but never READ: discovery
// suppresses by an in-memory set, and the tracker's filter has OR semantics
// with no exclusion key, so nothing could act on it. That was harmless while a
// single supervisor owned every pickup. Across a fleet it is the only
// cross-host claim there is — the flock lease cannot see another machine.
func (x *execRun) checkNotClaimed(issue *tracker.Issue) error {
	if x.args.force || !slices.Contains(issue.Labels, jiradozer.LockLabel) {
		return nil
	}
	return fmt.Errorf("%s is already claimed (label %q). Another host may be working it; check `jiradozer runs --json` across the fleet, then re-run with --force if that claim is stale",
		issue.Identifier, jiradozer.LockLabel)
}

func (x *execRun) createWorktree(ctx context.Context) error {
	prefix := x.args.branchPrefix
	if prefix == "" {
		prefix = x.cfg.Source.BranchPrefix
	}
	if prefix == "" {
		prefix = "jiradozer"
	}
	leaf := x.args.issueID
	if leaf == "" {
		leaf = x.args.taskID
	}
	if leaf == "" {
		leaf = x.runID
	}
	x.branch = prefix + "/" + sanitizeBranchLeaf(leaf)

	goal := x.args.description
	if x.issue != nil {
		goal = x.issue.Title
	}
	path, err := x.wtMgr.New(ctx, x.branch, x.cfg.BaseBranch, goal)
	if err != nil {
		return fmt.Errorf("create worktree for %s: %w", x.branch, err)
	}
	// Everything downstream — the agent, the local tracker, the workflow — runs
	// against this directory.
	x.cfg.WorkDir = path
	x.logger.Info("worktree created", "branch", x.branch, "path", path)
	return nil
}

// discardRemover builds the teardown used for a checkout no run-log claims.
//
// force=true deliberately. The only thing that can be in there is whatever
// wt.New's post-create hooks wrote, and hooks routinely leave untracked build
// output — exactly what an unforced `git worktree remove` refuses on. Refusing
// protects no work here (none has run yet); it strands a directory gc can never
// see. Named rather than inlined so the force decision has a test.
func discardRemover(mgr *wt.Manager) func(context.Context, string) error {
	return func(ctx context.Context, branch string) error {
		return mgr.Remove(ctx, branch, true, true)
	}
}

// discardUnclaimedWorktree removes a worktree that no run-log will ever claim.
//
// Only ever called between createWorktree and a FAILED startRunLog: at that
// point the checkout is a fresh branch off base with nothing in it, so this can
// destroy no work. It is best-effort and loud — if the removal itself fails the
// path is logged, because that directory is now invisible to `jiradozer gc` and
// only a human can find it.
func (x *execRun) discardUnclaimedWorktree(ctx context.Context) {
	if x.cfg.WorkDir == "" || x.branch == "" || x.removeWorktree == nil {
		return
	}
	if err := x.removeWorktree(ctx, x.branch); err != nil {
		x.logger.Error("could not remove the worktree of a run that never started; it is not tracked by any run-log and gc will never see it",
			"path", x.cfg.WorkDir, "branch", x.branch, "error", err)
		return
	}
	x.logger.Info("removed the worktree of a run that never started", "branch", x.branch)
}

// startRunLog records the run BEFORE any tracker mutation, so a crash from here
// on is visible to a sweeper. If it were written at the end instead, a run that
// died mid-flight would leave an untracked worktree and no trace of who made
// it — the leak this exists to prevent.
func (x *execRun) startRunLog() error {
	meta := jiradozer.RunMeta{
		RunID:        x.runID,
		TaskID:       x.args.taskID,
		Description:  x.args.description,
		LeaseTarget:  leaseTarget(x.args),
		Repo:         x.args.repo,
		Branch:       x.branch,
		BaseBranch:   x.cfg.BaseBranch,
		WorktreePath: x.cfg.WorkDir,
		TmuxSession:  x.args.tmuxSession,
		LogPath:      x.app.LogPath,
		State:        jiradozer.RunStateRunning,
	}
	if wtRoot, err := resolveWTRoot(); err == nil {
		meta.WTRoot = wtRoot
	}
	if x.issue != nil {
		meta.IssueIdentifier = x.issue.Identifier
		meta.IssueID = x.issue.ID
		if x.issue.URL != nil {
			meta.IssueURL = *x.issue.URL
		}
	} else {
		meta.IssueIdentifier = x.args.issueID
	}

	rl, err := jiradozer.NewRunLog(meta)
	if err != nil {
		return err
	}
	x.rl = rl
	x.logger.Info("run started", "run_id", x.runID, "run_dir", rl.Dir(), "target", meta.Target())
	return nil
}

// createLocalIssue builds the --description mode issue.
//
// The local tracker is rooted in the RUN directory, not the worktree. Rooted in
// the worktree, the issue JSON and every step comment, phase label and failure
// report would live inside a directory that gets reclaimed — and would be
// invisible to anything reading this box remotely. Same reasoning that puts the
// run-log outside the worktree.
func (x *execRun) createLocalIssue() error {
	dir := filepath.Join(x.rl.Dir(), "issues")
	lt, err := local.NewTracker(dir)
	if err != nil {
		return fmt.Errorf("create local tracker: %w", err)
	}
	x.tracker = lt

	title := jiradozer.GenerateTitle(x.args.description)
	issue, err := lt.CreateIssue(title, x.args.description)
	if err != nil {
		return fmt.Errorf("create local issue: %w", err)
	}
	x.issue = issue
	return x.rl.UpdateMeta(func(m *jiradozer.RunMeta) {
		m.IssueID = issue.ID
		if m.IssueIdentifier == "" {
			m.IssueIdentifier = issue.Identifier
		}
	})
}

// claim attaches the lock label. Best-effort: a tracker hiccup must not abort a
// run whose worktree already exists, and the flock lease still holds locally.
func (x *execRun) claim(ctx context.Context) {
	if err := x.tracker.AddLabel(ctx, x.issue.ID, jiradozer.LockLabel); err != nil {
		x.logger.Warn("failed to add lock label", "issue", x.issue.Identifier, "error", err)
		return
	}
	x.logger.Info("claimed issue", "issue", x.issue.Identifier, "label", jiradozer.LockLabel)
}

// postStartComment records where this run is happening.
//
// It doubles as a recoverable index. Run-logs are per-host local files, so when
// a box is stopped — which happens routinely — its runs vanish from view and an
// issue would otherwise sit labelled with nothing saying who took it. This
// comment survives on the tracker.
func (x *execRun) postStartComment(ctx context.Context) {
	host, _ := os.Hostname()
	var b strings.Builder
	fmt.Fprintf(&b, "**jiradozer** started on `%s`\n\n", host)
	fmt.Fprintf(&b, "- run: `%s`\n", x.runID)
	fmt.Fprintf(&b, "- branch: `%s`\n", x.branch)
	fmt.Fprintf(&b, "- worktree: `%s`\n", x.cfg.WorkDir)
	fmt.Fprintf(&b, "- run log: `%s`\n", x.rl.Dir())
	if x.app.LogPath != "" {
		fmt.Fprintf(&b, "- log: `%s`\n", x.app.LogPath)
	}
	if x.args.tmuxSession != "" {
		fmt.Fprintf(&b, "- tmux: `%s`\n", x.args.tmuxSession)
	}
	if x.args.taskID != "" {
		fmt.Fprintf(&b, "- task: `%s`\n", x.args.taskID)
	}
	fmt.Fprintf(&b, "\nStatus: `jiradozer runs --issue %s --json` on that host.", x.rl.Meta().Target())

	if _, err := x.tracker.PostComment(ctx, x.issue.ID, b.String()); err != nil {
		x.logger.Warn("failed to post start comment", "issue", x.issue.Identifier, "error", err)
	}
}

// finish settles the run-log, releases the claim and posts the end comment.
// It runs on every exit path including a failure, because a run that stops
// without recording why is the case a dispatcher cannot act on.
func (x *execRun) finish(runErr error) {
	// The run context is likely already cancelled by now, so use a fresh one:
	// the final tracker writes matter most exactly when the run is ending.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 30*time.Second)
	defer cancel()

	state := jiradozer.RunStateDone
	switch {
	case runErr == nil:
	case errors.Is(runErr, context.Canceled), errors.Is(runErr, context.DeadlineExceeded):
		state = jiradozer.RunStateCancelled
	default:
		state = jiradozer.RunStateFailed
	}

	// Record the PR before settling the meta. This is not decoration: gc keys
	// reclamation solely on PRURL, so a run that finishes without one keeps its
	// worktree FOREVER — and exec always keeps its worktree. Resolving it here,
	// from the branch, rather than threading it out of the workflow means it is
	// recorded however the PR came to exist (create_pr, a resumed run, or a
	// human opening it mid-flight).
	pr := x.resolvePR(ctx)

	// The worktree is deliberately kept. A terminal state here means "a PR is
	// open", never "it merged", so nothing at this point authorises deleting
	// the branch's only checkout. `jiradozer gc` reclaims it later by asking
	// whether the PR actually landed.
	if err := x.rl.UpdateMeta(func(m *jiradozer.RunMeta) {
		m.WorktreeKept = true
		m.WorktreeKeptReason = "terminal state is an open PR, not a merge; reclaimed by `jiradozer gc`"
		if pr != nil {
			m.PRURL = pr.URL
			m.PRNumber = pr.Number
		}
	}); err != nil {
		x.logger.Warn("failed to record worktree retention", "error", err)
	}
	if err := x.rl.Finish(state, "", runErr); err != nil {
		x.logger.Warn("failed to write terminal run meta", "error", err)
	}

	if x.tracker != nil && x.issue != nil {
		x.postEndComment(ctx, state, runErr)
		if err := x.tracker.RemoveLabel(ctx, x.issue.ID, jiradozer.LockLabel); err != nil {
			x.logger.Warn("failed to release lock label", "issue", x.issue.Identifier, "error", err)
		}
	}
	x.logger.Info("run finished", "run_id", x.runID, "state", state, "run_dir", x.rl.Dir())
}

// resolvePR asks GitHub which PR this run's branch opened, or nil.
//
// A missing PR is a NORMAL outcome, not an error: a run that failed before
// create_pr, or one whose build produced no changes, legitimately has none. So
// this logs at debug and returns nil rather than warning — but a PR that exists
// and is not recorded leaks a worktree permanently, which is why the lookup
// happens on every exit path instead of only the successful one.
func (x *execRun) resolvePR(ctx context.Context) *wt.PRInfo {
	if x.lookupPR == nil || x.branch == "" || x.cfg.WorkDir == "" {
		return nil
	}
	pr, err := x.lookupPR(ctx, x.branch, x.cfg.WorkDir)
	if err != nil {
		x.logger.Debug("no PR resolved for branch", "branch", x.branch, "error", err)
		return nil
	}
	if pr == nil || pr.URL == "" {
		return nil
	}
	x.logger.Info("recorded PR for run", "branch", x.branch, "pr", pr.URL)
	return pr
}

func (x *execRun) postEndComment(ctx context.Context, state jiradozer.RunState, runErr error) {
	host, _ := os.Hostname()
	m := x.rl.Meta()
	var b strings.Builder
	fmt.Fprintf(&b, "**jiradozer** finished on `%s` — `%s`\n\n", host, state)
	fmt.Fprintf(&b, "- run: `%s`\n", x.runID)
	fmt.Fprintf(&b, "- branch: `%s`\n", x.branch)
	fmt.Fprintf(&b, "- duration: %s\n", time.Since(m.StartedAt).Truncate(time.Second))
	if m.PRURL != "" {
		fmt.Fprintf(&b, "- PR: %s\n", m.PRURL)
	}
	fmt.Fprintf(&b, "- worktree kept at `%s` (reclaimed by `jiradozer gc` once the PR lands)\n", x.cfg.WorkDir)
	if runErr != nil {
		fmt.Fprintf(&b, "\nError:\n```\n%s\n```\n", jiradozer.Truncate(runErr.Error(), 2000))
	}
	if _, err := x.tracker.PostComment(ctx, x.issue.ID, b.String()); err != nil {
		x.logger.Warn("failed to post end comment", "issue", x.issue.Identifier, "error", err)
	}
}

// leaseTarget names this task for the flock lease.
//
// It must be DERIVED, never random, and `dispatch` must compute the same value
// this worker will. The lease is what lets a dispatcher refuse a second run for
// work some box is already doing, and that check happens before the worker
// exists — so a per-run identifier would make every --description dispatch
// unique and two concurrent dispatches of the same task would both proceed,
// producing duplicate worktrees and duplicate PRs.
//
// A description is hashed rather than used verbatim because it is free-form
// multi-line text and this becomes a lock FILENAME.
func leaseTarget(x execArgs) string {
	if x.issueID != "" {
		return x.issueID
	}
	if x.taskID != "" {
		return x.taskID
	}
	if x.description == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(x.description)))
	return "adhoc-" + hex.EncodeToString(sum[:6])
}

// sanitizeBranchLeaf keeps an identifier usable as one branch path segment.
// GitHub identifiers like "acme/app#42" would otherwise nest the branch.
func sanitizeBranchLeaf(s string) string {
	return strings.NewReplacer("/", "-", "#", "-", " ", "-", "~", "-", "^", "-", ":", "-").Replace(s)
}
