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
		WithPolishSpec(o.Entry.Polish),
	)

	interval := o.PollInterval
	if interval <= 0 {
		interval = o.Entry.PollInterval
	}
	notifier := o.Notify.Notifier()
	cooldownReported := time.Time{}

	for {
		tickStart := time.Now()
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
			// LastCooldownCause, not LastMergeError: the cooldown trips on a
			// failure streak that stalls and reworks feed too, so the merge field
			// would misattribute every non-merge cause. Written and cleared with
			// the window itself, so it always describes THIS cooldown.
			b.event(runLog, "cooldown", state.LastCooldownCause, map[string]any{"until": state.CooldownUntil})
			Report(ctx, b.logger, notifier, CooldownWarning(runLog.Meta(), state.CooldownUntil, state.LastCooldownCause))
		}

		if o.Once {
			return TerminalRunning, "single tick requested"
		}
		if !waitForNextPoll(ctx, nextPollDelay(interval, time.Since(tickStart))) {
			return TerminalRunning, "context cancelled"
		}
	}
}

// nextPollDelay reports how long to wait before the next tick.
//
// poll_interval is meant to bound how OFTEN prdozer looks at the PR, but the
// loop used to sleep the full interval after each tick finished — so the wait
// was added to however long the tick took rather than measured from its start.
// A polish round that ran 29 minutes then slept 20 produced a 49-minute gap on
// a 20-minute interval.
//
// Measured on kernel#8374: 4.1 hours of agent work spread over 11.5 hours of
// wall-clock, with observed gaps of 96, 81, 73, 62, 59, 55, 55, 51, 43, 33, 33,
// 29 and 20 minutes against a 20-minute setting. About 64% of that run was
// waiting, nearly all of it this drift.
//
// The correction is to measure the interval from the tick's START. If the tick
// already outlasted it, the next look is overdue and runs immediately — which
// is what collapses those 96- and 81-minute gaps back to the 20 minutes that
// was configured.
//
// Deliberately NOT here: skipping the wait outright after a round that
// polished. Polishing does push commits, and CI and reviewers do re-trigger —
// but neither reports back within the zero seconds an unpaced re-tick waits,
// so the immediate next tick sees no new external signal. What it does see is
// a moved head SHA, which re-arms self_review (see the divergence guard in
// watcher.go: "a self_review repo re-arms its own trigger forever and this
// guard is the ONLY thing that can end the loop"). Pacing is the other brake
// on that loop, and dropping it lets polish rounds chain back-to-back on agent
// budget until divergence trips. A long polish round already gets its
// immediate re-tick from the overdue rule above; a short one is exactly the
// case where chaining is cheap and the wait is worth keeping.
func nextPollDelay(interval, elapsed time.Duration) time.Duration {
	if remaining := interval - elapsed; remaining > 0 {
		return remaining
	}
	return 0
}

// waitForNextPoll pauses until the next tick is due, reporting false if the
// context was cancelled instead.
//
// The zero-wait case still checks cancellation. It is reached whenever a tick
// outlasts the interval — precisely the long, expensive ticks after which a
// shutdown is most likely to be pending — and a bare `continue` there would
// start another one instead of stopping.
func waitForNextPoll(ctx context.Context, wait time.Duration) bool {
	if wait <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(wait):
		return true
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
		//
		// A stall is the third kind, and the one where the generic message is
		// most misleading: nothing is blocked on a reviewer and nothing about the
		// PR got worse — the rounds never returned. Reported first because a
		// stalled run never completes a round, so it cannot also have diverged.
		if res.Stalled {
			return TerminalNeedsHuman, fmt.Sprintf(
				"PR #%d stalled: %d polish invocations produced no completed round, so the run was halted rather than burning further attempts. Nothing is wrong with the PR itself — check the agent backend. Last error: %s",
				pr.Number, res.InvocationsSinceRound, res.StallError), true
		}
		if res.Diverged {
			// Report the streak the guard tripped on, NOT the run's cumulative
			// PolishRounds: a run that improved several times before stalling
			// would otherwise claim "17 rounds produced no better result" when
			// only the last 3 were flat.
			return TerminalNeedsHuman, fmt.Sprintf(
				"PR #%d stopped improving: %d polish rounds produced no better result (%d rounds total), so the run was halted before it degraded the PR further. Review the pushed commits before resuming.",
				pr.Number, res.RoundsSinceImprovement, res.PolishRounds), true
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

	// Narrowing before the probe is safe HERE, and only here: prdozer has no
	// fleet-wide duplicate-run guard for a pin to hide from. Its lease is a
	// flock taken inside the worker, so it excludes a second babysitter on ONE
	// box; nothing asks the rest of the fleet whether this PR is already being
	// babysat. jiradozer's dispatch does ask, which is why the equivalent pin
	// there is applied to the probe RESULTS instead — see narrowToPin in
	// jiradozer/cmd/jiradozer/dispatch.go. If prdozer ever grows the same
	// preflight, this filter has to move below the probe with it.
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
