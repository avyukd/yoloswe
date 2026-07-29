package prdozer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/notify"
	"github.com/bazelment/yoloswe/wt"
)

// BabysitOptions configures one local babysit run — the worker side, executing
// on whichever box was chosen.
type BabysitOptions struct {
	Notify       NotifyConfig
	OwnerRepo    string
	Entry        RepoEntry
	PRNumber     int
	PollInterval time.Duration
	KeepWorktree bool
	Once         bool
}

// Babysitter runs a PR to a terminal state on the local box: prepare an
// ephemeral worktree, loop polish → CI-green → merge (reworking failed
// merges), then notify and GC.
type Babysitter struct {
	gh       wt.GHRunner
	git      wt.GitRunner
	renderer *render.Renderer
	logger   *slog.Logger
	opts     BabysitOptions
}

// NewBabysitter builds a worker.
func NewBabysitter(gh wt.GHRunner, git wt.GitRunner, renderer *render.Renderer, logger *slog.Logger, opts BabysitOptions) *Babysitter {
	if logger == nil {
		logger = slog.Default()
	}
	return &Babysitter{gh: gh, git: git, renderer: renderer, logger: logger, opts: opts}
}

// Run executes the babysit loop to a terminal state.
//
// The lease is acquired HERE, inside the worker process, not by the dispatcher
// wrapping the command in flock(1) — that does not hold (see AcquireLease).
// It is held for the run's whole lifetime.
func (b *Babysitter) Run(ctx context.Context) (state TerminalState, err error) {
	o := b.opts
	if cerr := o.Entry.CheckUsable(o.OwnerRepo); cerr != nil {
		return TerminalFailed, cerr
	}

	lease, err := AcquireLease(o.OwnerRepo, o.PRNumber)
	if err != nil {
		// Another babysitter already owns this PR. That is a normal outcome
		// for a duplicate dispatch, not a failure to alert on.
		return TerminalFailed, err
	}
	defer func() { _ = lease.Release() }()

	// GitDir, not WorktreeRoot: under the "wt" layout the root is the bare-repo
	// parent, and gh cannot resolve a PR from outside a work tree.
	prs, err := fetchByNumbers(ctx, b.gh, o.Entry.GitDir(), []int{o.PRNumber})
	if err != nil {
		return TerminalFailed, fmt.Errorf("look up PR #%d: %w", o.PRNumber, err)
	}
	if len(prs) == 0 {
		return TerminalFailed, fmt.Errorf("PR #%d not found", o.PRNumber)
	}
	pr := prs[0]

	runID, err := NewRunID()
	if err != nil {
		return TerminalFailed, err
	}

	runLog, err := NewRunLog(RunMeta{
		Repo:        o.OwnerRepo,
		PRNumber:    o.PRNumber,
		RunID:       runID,
		Branch:      pr.HeadRefName,
		PRURL:       pr.URL,
		TmuxSession: TmuxSessionName(o.OwnerRepo, o.PRNumber),
		MergePolicy: o.Entry.MergePolicy,
	})
	if err != nil {
		return TerminalFailed, err
	}
	started := time.Now()
	notifier := o.Notify.Notifier()

	rc, err := PrepareWorktree(ctx, b.git, o.Entry, pr, runID, b.logger)
	if err != nil {
		// Scrub before persisting or Slacking: run logs live on disk and the
		// report is published.
		detail := safeErrString(err)
		_ = runLog.Finish(TerminalFailed, detail)
		b.report(ctx, notifier, runLog, TerminalFailed, detail, "", time.Since(started))
		return TerminalFailed, err
	}
	if uerr := runLog.UpdateMeta(func(m *RunMeta) { m.WorktreePath = rc.WorktreePath }); uerr != nil {
		b.logger.Warn("could not record worktree path", "error", uerr)
	}
	b.event(runLog, "prepared", rc.WorktreePath, nil)

	state, detail := b.loop(ctx, rc, runLog, pr)

	// GC on every terminal state. Cleanup itself refuses to discard unpushed
	// work, keeping the worktree and saying why.
	keptPath := ""
	if o.KeepWorktree {
		keptPath = rc.WorktreePath
		b.logger.Info("keeping worktree (--keep-worktree)", "path", rc.WorktreePath)
	} else {
		if cerr := rc.Cleanup(ctx, b.git, false, b.logger); cerr != nil {
			b.logger.Warn("worktree cleanup failed", "error", cerr)
			keptPath = rc.WorktreePath
		} else if rc.Kept {
			keptPath = rc.WorktreePath
			detail = strings.TrimSpace(detail + "\nWorktree kept: " + rc.KeptReason)
		}
	}

	if uerr := runLog.UpdateMeta(func(m *RunMeta) { m.WorktreeKept = keptPath != "" }); uerr != nil {
		b.logger.Warn("could not record worktree-kept flag", "error", uerr)
	}
	if ferr := runLog.Finish(state, detail); ferr != nil {
		b.logger.Warn("could not finalize run meta", "error", ferr)
	}
	b.report(ctx, notifier, runLog, state, detail, keptPath, time.Since(started))
	return state, nil
}

// loop ticks the watcher until the PR reaches a terminal state.
func (b *Babysitter) loop(ctx context.Context, rc *RunContext, runLog *RunLog, pr DiscoveredPR) (TerminalState, string) {
	o := b.opts
	cfg := b.watcherConfig(rc)

	polish := PolishRunner(NewAgentPolisher(b.renderer, b.logger))
	rework := ReworkRunner(NewAgentRework(b.renderer, b.logger, runLog))

	w := NewWatcher(cfg, b.gh, polish, o.PRNumber, rc.WorktreePath, o.OwnerRepo, b.logger,
		WithRenderer(b.renderer),
		WithRework(rework, o.Entry.MergeRework),
	)

	interval := o.PollInterval
	if interval <= 0 {
		interval = o.Entry.PollInterval
	}
	notifier := o.Notify.Notifier()
	cooldownReported := time.Time{}

	for {
		res, err := w.Tick(ctx)
		if err != nil {
			safe := safeErrString(err)
			b.event(runLog, "tick_error", safe, nil)
			b.logger.Error("tick failed", "error", safe)
			// A tick error is transient (a gh hiccup, a state write); keep
			// polling rather than abandoning a PR mid-flight.
		} else {
			b.event(runLog, "tick", string(res.Action), map[string]any{
				"mergeable": res.Changeset.Mergeable,
				"ci_failed": res.Changeset.CIFailed,
			})
			if err := b.recordRounds(runLog, res.Action); err != nil {
				b.logger.Warn("could not update run meta", "error", err)
			}
			if terminal, detail, done := terminalFor(res, pr); done {
				return terminal, detail
			}
		}

		// A tripped cooldown is reported as a non-terminal warning, once per
		// cooldown window: unbounded retry must stay visible.
		if state, serr := LoadState(StatePath(o.OwnerRepo, o.PRNumber)); serr == nil &&
			!state.CooldownUntil.IsZero() && state.CooldownUntil.After(cooldownReported) {
			cooldownReported = state.CooldownUntil
			b.event(runLog, "cooldown", state.LastMergeError, map[string]any{"until": state.CooldownUntil})
			Report(ctx, b.logger, notifier, CooldownWarning(runLog.Meta(), state.CooldownUntil, state.LastMergeError))
		}

		if o.Once {
			return TerminalRunning, "single tick requested"
		}
		select {
		case <-ctx.Done():
			return TerminalRunning, "context cancelled"
		case <-time.After(interval):
		}
	}
}

// terminalFor maps a tick result onto a terminal state, if it is one.
func terminalFor(res TickResult, pr DiscoveredPR) (TerminalState, string, bool) {
	switch res.Action {
	case LastActionMerged:
		return TerminalMerged, fmt.Sprintf("PR #%d merged.", pr.Number), true
	case LastActionClosed:
		return TerminalClosed, fmt.Sprintf("PR #%d was closed without merging.", pr.Number), true
	case LastActionNeedsHuman:
		// Say WHICH kind of stuck. "Needs a human" alone does not distinguish a
		// PR waiting on an approval from one the babysitter was actively making
		// worse — and those call for opposite responses.
		if res.Diverged {
			return TerminalNeedsHuman, fmt.Sprintf(
				"PR #%d stopped improving: %d polish rounds produced no better result, so the run was halted before it degraded the PR further. Review the pushed commits before resuming.",
				pr.Number, res.PolishRounds), true
		}
		return TerminalNeedsHuman, "This PR needs a human: it is blocked on something no agent round can resolve (a review approval, or a merge policy that never merges).", true
	}
	return "", "", false
}

func (b *Babysitter) recordRounds(runLog *RunLog, action LastAction) error {
	switch action {
	case LastActionPolished:
		return runLog.UpdateMeta(func(m *RunMeta) { m.PolishRounds++ })
	case LastActionMerged, LastActionArmed, LastActionReworked:
		if state, err := LoadState(StatePath(b.opts.OwnerRepo, b.opts.PRNumber)); err == nil {
			return runLog.UpdateMeta(func(m *RunMeta) { m.MergeAttempt = state.MergeAttempts })
		}
	}
	return nil
}

// watcherConfig derives the watcher config from the registry entry.
func (b *Babysitter) watcherConfig(rc *RunContext) *Config {
	cfg := DefaultConfig()
	cfg.WorkDir = rc.WorktreePath
	cfg.BaseBranch = b.opts.Entry.BaseBranch
	if b.opts.Entry.Model != "" {
		cfg.Agent.Model = b.opts.Entry.Model
	}
	if b.opts.Entry.MaxBudgetUSD > 0 {
		cfg.MaxBudgetUSD = b.opts.Entry.MaxBudgetUSD
		cfg.Polish.MaxBudgetUSD = b.opts.Entry.MaxBudgetUSD
	}
	cfg.Polish.MergePolicy = b.opts.Entry.MergePolicy
	cfg.Polish.SelfReview = b.opts.Entry.SelfReview
	// AutoMerge is on so the loop can reach a merge; the POLICY decides
	// whether anything actually lands. merge_policy "notify" reports and stops.
	cfg.Polish.AutoMerge = true
	cfg.Source.Mode = SourceModeList
	cfg.Source.PRs = []int{b.opts.PRNumber}
	return cfg
}

func (b *Babysitter) event(runLog *RunLog, kind, detail string, fields map[string]any) {
	if err := runLog.Append(kind, detail, fields); err != nil {
		b.logger.Warn("could not append run event", "kind", kind, "error", err)
	}
}

func (b *Babysitter) report(ctx context.Context, n notify.Notifier, runLog *RunLog, state TerminalState, detail, keptPath string, elapsed time.Duration) {
	Report(ctx, b.logger, n, RunReport{
		Meta:             runLog.Meta(),
		State:            state,
		Detail:           detail,
		WorktreeKeptPath: keptPath,
		Elapsed:          elapsed,
	})
}

// DispatchOptions configures the control-side `babysit` command.
type DispatchOptions struct {
	OwnerRepo    string
	RegistryPath string
	Host         string
	Probe        ProbeOptions
	PRNumber     int
	Here         bool
	DryRun       bool
	KeepWorktree bool
}

// DispatchResult reports what the dispatcher decided.
type DispatchResult struct {
	Command  string
	Scores   []HostHealth
	Chosen   HostHealth
	RanLocal bool
}

// PlanDispatch probes the fleet and picks a target without acting, so both the
// real dispatch and --dry-run share one decision path — a dry run that
// computes a different answer than the real thing is worse than none.
func PlanDispatch(ctx context.Context, ssh SSHRunner, opts DispatchOptions, logger *slog.Logger) (DispatchResult, error) {
	if logger == nil {
		logger = slog.Default()
	}
	hosts, err := LoadFleet(FleetDir)
	if err != nil {
		return DispatchResult{}, err
	}
	if opts.Probe.SelfDNS == "" {
		opts.Probe.SelfDNS = SelfPublicDNS(DevboxConfigPath)
	}
	if opts.Probe.SelfHostname == "" {
		opts.Probe.SelfHostname, _ = os.Hostname()
	}

	if opts.Host != "" {
		hosts = filterHosts(hosts, opts.Host)
		if len(hosts) == 0 {
			return DispatchResult{}, fmt.Errorf("host %q is not in the fleet registry", opts.Host)
		}
	}

	scores := ProbeFleet(ctx, ssh, hosts, opts.Probe)
	for i := range scores {
		if !scores[i].Reachable {
			logger.Warn("host unreachable during probe", "host", scores[i].Host, "error", scores[i].Err)
		}
	}
	chosen, err := PickHost(scores, opts.Probe)
	if err != nil {
		return DispatchResult{Scores: scores}, err
	}

	req := DispatchRequest{
		Host:         chosen,
		OwnerRepo:    opts.OwnerRepo,
		PRNumber:     opts.PRNumber,
		RegistryPath: opts.RegistryPath,
		KeepWorktree: opts.KeepWorktree,
	}
	return DispatchResult{
		Scores:  scores,
		Chosen:  chosen,
		Command: req.SSHCommand(),
		// Self-dispatch runs in-process: never SSH to yourself.
		RanLocal: chosen.IsSelf,
	}, nil
}

func filterHosts(hosts []FleetHost, name string) []FleetHost {
	var out []FleetHost
	for i := range hosts {
		if hosts[i].Hostname == name || hosts[i].PublicDNS == name {
			out = append(out, hosts[i])
		}
	}
	return out
}

// ErrQuotaExhausted reports that the shared Claude quota is too tight to start
// a run.
var ErrQuotaExhausted = errors.New("claude quota exhausted")
