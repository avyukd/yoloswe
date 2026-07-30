package prdozer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/wt"
)

// Watcher polls a single PR and reacts to changes by invoking the polish agent.
type Watcher struct {
	gh         wt.GHRunner
	polish     PolishRunner
	rework     ReworkRunner
	cfg        *Config
	renderer   *render.Renderer
	logger     *slog.Logger
	repo       string
	workDir    string
	self       string
	reworkSpec StepSpec
	polishSpec StepSpec
	pr         int
	dryRun     bool
	// diverged is set by the divergence guard within a tick so Tick can
	// report WHY it stopped. Reset at the top of every tick.
	diverged bool
}

// WithPolishSpec attaches configured polish rounds, replacing the default
// single "/pr-polish" call. Empty keeps the default.
func WithPolishSpec(spec StepSpec) WatcherOption {
	return func(w *Watcher) { w.polishSpec = spec }
}

// WithRework attaches the merge-rework runner and the rounds it should run,
// normally taken from the registry entry's merge_rework.
func WithRework(r ReworkRunner, spec StepSpec) WatcherOption {
	return func(w *Watcher) {
		w.rework = r
		w.reworkSpec = spec
	}
}

// WatcherOption configures a new Watcher.
type WatcherOption func(*Watcher)

// WithRenderer attaches a renderer for terminal output.
func WithRenderer(r *render.Renderer) WatcherOption {
	return func(w *Watcher) { w.renderer = r }
}

// WithSelfLogin tells the watcher which GitHub login to ignore when looking
// for new comments (so prdozer doesn't react to its own comments).
func WithSelfLogin(login string) WatcherOption {
	return func(w *Watcher) { w.self = login }
}

// WithDryRun puts the watcher in observe-only mode — snapshots and changesets
// are computed and logged, but no agent is invoked.
func WithDryRun(dryRun bool) WatcherOption {
	return func(w *Watcher) { w.dryRun = dryRun }
}

// NewWatcher creates a watcher for a single PR.
func NewWatcher(cfg *Config, gh wt.GHRunner, polish PolishRunner, prNumber int, workDir, repo string, logger *slog.Logger, opts ...WatcherOption) *Watcher {
	if logger == nil {
		logger = slog.Default()
	}
	w := &Watcher{
		cfg:     cfg,
		gh:      gh,
		polish:  polish,
		logger:  logger.With("pr", prNumber),
		pr:      prNumber,
		repo:    repo,
		workDir: workDir,
	}
	for _, o := range opts {
		o(w)
	}
	return w
}

// TickResult summarizes what happened in one polling cycle.
type TickResult struct {
	Snapshot  *Snapshot
	Action    LastAction
	Changeset Changeset
	// Diverged reports that NeedsHuman was reached because the PR stopped
	// improving, rather than because it is waiting on an approval. The two need
	// opposite responses, so the notification must tell them apart.
	Diverged bool
	// PolishRounds is the run's cumulative polish count, reported alongside
	// Diverged.
	PolishRounds int
	// RoundsSinceImprovement is the streak the divergence guard actually trips
	// on. Reported separately from PolishRounds because the two diverge the
	// moment a run improves at all: a run can be 17 rounds deep and only 3
	// rounds flat, and saying "17 rounds produced no better result" would be a
	// lie.
	RoundsSinceImprovement int
}

func (w *Watcher) Tick(ctx context.Context) (TickResult, error) {
	statePath := StatePath(w.repo, w.pr)
	state, err := LoadState(statePath)
	if err != nil {
		return TickResult{}, fmt.Errorf("load state: %w", err)
	}
	state.PRNumber = w.pr
	state.Repo = w.repo

	if !state.CooldownUntil.IsZero() && time.Now().Before(state.CooldownUntil) {
		w.logger.Info("in cooldown, skipping tick", "until", state.CooldownUntil)
		w.status("PR #%d in cooldown until %s", w.pr, state.CooldownUntil.Format(time.RFC3339))
		return TickResult{Action: LastActionIdle}, nil
	}

	snapOpts := SnapshotOptions{Self: w.self}
	snap, err := TakeSnapshot(ctx, w.gh, w.workDir, w.pr, snapOpts)
	if err != nil {
		return TickResult{}, fmt.Errorf("snapshot: %w", err)
	}
	cs := ComputeChangeset(state, snap)

	// Fold this tick's observed health into the run's best-so-far BEFORE
	// deciding what to do. The divergence guard runs inside decideAndAct, so
	// scoring afterward would hand it the PREVIOUS tick's streak: a snapshot
	// that is strictly better still trips the guard, because the reset lands
	// only after the stop has already been decided. (Flat rounds charged on
	// ticks 2-4 leave the streak at the limit; if the polish from tick 4 finally
	// improves the PR, tick 5 must not halt it.) It also keeps the streak in the
	// log line, the w.status message, and TickResult all reading the same value.
	//
	// recordHealth reads state.LastAction, which recordSnapshot does not update
	// until the end of the tick, so it still charges against the PREVIOUS tick's
	// action from here.
	healthDirty := w.recordHealth(state, snap)

	w.logger.Info("snapshot",
		"head", snap.PR.HeadRefOid,
		"base_sha", snap.BaseSHA,
		"rollup", snap.StatusRollup,
		"review", snap.PR.ReviewDecision,
		"comments", len(snap.Comments),
		"failed_runs", len(snap.FailedRunIDs),
		"new_comments", len(cs.NewCommentIDs),
		"new_failed_runs", len(cs.NewFailedRuns),
		"base_moved", cs.BaseMoved,
		"ci_failed", cs.CIFailed,
		"mergeable", cs.Mergeable,
		"pr_closed", cs.PRClosed,
		// The divergence guard's input. Logged on every tick so a run that
		// stops (or fails to stop) can be explained from the log alone.
		"unresolved_threads", snap.UnresolvedThreads,
		"rounds_since_improvement", state.RoundsSinceImprovement,
	)

	w.diverged = false
	priorAttempts, priorMergeErr := state.MergeAttempts, state.LastMergeError
	priorSelfReviewed := state.SelfReviewedSHA
	// Counted, not compared by identity: decideAndAct adds keys to the SAME map
	// when one already exists, so a pointer comparison would see no change.
	priorOnceDone := len(state.OnceRoundsDone)
	action := w.decideAndAct(ctx, snap, cs, state)
	mergeStateDirty := state.MergeAttempts != priorAttempts ||
		state.LastMergeError != priorMergeErr ||
		state.SelfReviewedSHA != priorSelfReviewed ||
		// Load-bearing: without this a newly completed once round is never saved,
		// so the next tick reloads without it and runs it again.
		len(state.OnceRoundsDone) != priorOnceDone

	res := TickResult{Snapshot: snap, Changeset: cs, Action: action,
		Diverged: w.diverged, PolishRounds: state.PolishRounds,
		RoundsSinceImprovement: state.RoundsSinceImprovement}
	if w.recordSnapshot(state, snap, action, mergeStateDirty || healthDirty) {
		if err := state.Save(statePath); err != nil {
			// State save failure is serious: the action already ran (maybe merged
			// or polished) and the next tick will reload stale state and could
			// re-trigger it. Surface the error so the outer loop can at least
			// log+alert, and the caller can decide whether to back off.
			return res, fmt.Errorf("save state %q: %w", statePath, err)
		}
	}
	return res, nil
}

func (w *Watcher) decideAndAct(ctx context.Context, snap *Snapshot, cs Changeset, state *State) LastAction {
	switch {
	case cs.PRClosed:
		w.status("PR #%d is %s — nothing to do", w.pr, snap.PR.State)
		if snap.PR.State == "MERGED" {
			return LastActionMerged
		}
		return LastActionClosed
	// NOTE: every cs.Mergeable branch below is guarded by !cs.NeedsPolish().
	// Mergeable means "no conflicts, required checks pass" — it says nothing
	// about whether reviewer feedback was addressed. Without the guard, a PR
	// with unhandled comments short-circuits to a terminal state and pr-polish
	// never runs (yoloswe#287 finished in 1.5s that way), and worse, a repo on
	// a real merge policy would land the PR with the feedback outstanding.
	case cs.Mergeable && !cs.NeedsPolish() && !w.needsSelfReview(snap, state) && w.cfg.Polish.AutoMerge && w.cfg.Polish.MergePolicy == MergePolicyNotify:
		// Explicitly configured never to merge: report and stop rather than
		// idling forever on a PR that is ready to land.
		w.status("PR #%d is mergeable but merge_policy is %q — not merging", w.pr, MergePolicyNotify)
		return LastActionNeedsHuman
	case cs.Mergeable && !cs.NeedsPolish() && !w.needsSelfReview(snap, state) && w.cfg.Polish.AutoMerge && !w.dryRun:
		state.MergeAttempts++
		outcome, err := w.merge(ctx, snap)
		if err != nil {
			// Scrub once, at the source: this message is logged, persisted to
			// the state file, and fed verbatim into the rework prompt.
			safe := safeErrString(err)
			// A GitHub outage means the attempt never got a verdict, so it is not
			// a merge rejection and must not be charged to EITHER brake. Roll back
			// the increment before returning: MergeAttempts is documented as
			// counting real rejections (it is what the merge brake measures), and
			// nothing else resets it, so leaving an outage in it would let a run of
			// 529s trip the cooldown exactly the way this PR exists to prevent.
			// Rework is skipped too — there is nothing for an agent to fix.
			if action, ok := w.classifyMergeFailure(err, safe); ok {
				state.MergeAttempts--
				return action
			}
			w.logger.Error("auto-merge failed",
				"error", safe, "policy", w.cfg.Polish.MergePolicy, "attempt", state.MergeAttempts)
			w.status("PR #%d merge attempt %d failed: %s", w.pr, state.MergeAttempts, safe)
			state.LastMergeError = safe
			return w.reworkAfterFailedMerge(ctx, snap, state)
		}
		state.LastMergeError = ""
		if outcome == mergeOutcomeArmed {
			w.status("PR #%d auto-merge armed — waiting for the merge queue", w.pr)
			return LastActionArmed
		}
		w.status("PR #%d merged", w.pr)
		return LastActionMerged
	case cs.Mergeable && !cs.NeedsPolish() && !w.needsSelfReview(snap, state):
		w.status("PR #%d is mergeable — idle", w.pr)
		return LastActionIdle
	case cs.NeedsReview:
		// Green on every axis prdozer can influence; only a human approval is
		// missing. Polishing again would burn rounds against a wall.
		w.status("PR #%d is green but awaiting human review approval", w.pr)
		return LastActionNeedsHuman
	case !cs.NeedsPolish() && !w.needsSelfReview(snap, state):
		w.status("PR #%d unchanged — idle", w.pr)
		return LastActionIdle
	case w.diverging(snap, state):
		// More polish rounds are not producing a better PR. Stop and hand it
		// to a human rather than burning rounds — and money — making it worse.
		//
		// Ordered ahead of the self-review path on purpose, even though that
		// means a bot-less repo can stop with an unreviewed head commit. Every
		// polish round pushes a commit, which moves the head SHA, which makes
		// needsSelfReview true again — so a self_review repo re-arms its own
		// trigger forever and this guard is the ONLY thing that can end the
		// loop. The first review is never at risk: the guard needs a non-zero
		// RoundsSinceImprovement, which only prior polish rounds can produce.
		//
		// diverging() grants a saturated metric or an unreviewed head ONE extra
		// allowance (see there), which is why this ordering still terminates: the
		// exemption expires at 2*limit, so a run whose head is perpetually
		// unreviewed — or perpetually saturated — still stops.
		//
		// What the stop DOES owe the human is a note when it lands on an
		// unreviewed head: on a self_review repo prdozer is the only reviewer,
		// so "not improving" and "nobody has looked at the last commit" are
		// different things to walk into.
		w.diverged = true
		best := state.BestHealth
		unreviewed := w.needsSelfReview(snap, state)
		w.status("PR #%d is not improving after %d rounds (best: %d unresolved, ci_failing=%t, head_reviewed=%t) — needs a human",
			w.pr, state.RoundsSinceImprovement, best.UnresolvedThreads, best.CIFailing, !unreviewed)
		w.logger.Warn("divergence guard tripped",
			"rounds_since_improvement", state.RoundsSinceImprovement,
			"polish_rounds", state.PolishRounds,
			"best_unresolved", best.UnresolvedThreads,
			"best_ci_failing", best.CIFailing,
			"head_unreviewed", unreviewed)
		return LastActionNeedsHuman
	}

	if w.dryRun {
		w.status("PR #%d would polish (base_moved=%t ci_failed=%t new_comments=%d) — dry run, skipping",
			w.pr, cs.BaseMoved, cs.CIFailed, len(cs.NewCommentIDs))
		return LastActionDryRun
	}
	if w.polish == nil {
		w.logger.Warn("no polish runner configured; skipping action")
		return LastActionIdle
	}
	w.status("PR #%d polishing (base_moved=%t ci_failed=%t new_comments=%d changes_requested=%t)",
		w.pr, cs.BaseMoved, cs.CIFailed, len(cs.NewCommentIDs), cs.ChangesRequested)
	req := PolishRequest{
		PRNumber: w.pr,
		WorkDir:  w.workDir,
		Local:    w.cfg.Polish.Local,
		Cfg:      w.cfg.Polish,
		Model:    w.cfg.Agent.Model,
		Spec:     w.polishSpec,
		Repo:     w.repo,
		Branch:   snap.PR.HeadRefName,
		PRURL:    snap.PR.URL,
		// Rounds marked `once` are owed until this PR has completed them. Cloned,
		// not shared: the request crosses the PolishRunner boundary, and handing
		// out the live state map would let a runner mutate persisted state (and
		// make the request's contents change under it as this tick records more).
		DoneOnceRounds: maps.Clone(state.OnceRoundsDone),
	}
	polishRes, err := w.polish.Run(ctx, req)
	// Recorded before the error check: a spec whose LATER round failed has still
	// run the once-only rounds before it, and repeating one is the churn `once`
	// exists to prevent. The runner reports what actually finished.
	for key := range polishRes.RanOnceRounds {
		if state.OnceRoundsDone == nil {
			state.OnceRoundsDone = make(map[string]bool)
		}
		state.OnceRoundsDone[key] = true
	}
	if err != nil {
		// Log the scrubbed STRING, not a wrapped error: a provider error can
		// embed the endpoint config (API key / key-bearing env var), and these
		// messages are persisted and Slacked.
		safe := safeErrString(err)
		if action, ok := w.classifyAgentRoundFailure("polish", err, safe); ok {
			return action
		}
		w.logger.Error("polish failed", "error", safe)
		w.status("PR #%d polish failed: %s", w.pr, safe)
		return LastActionFailed
	}
	w.status("PR #%d polish completed", w.pr)

	// Mark this commit as self-reviewed. Recorded on SUCCESS only: a failed
	// round has produced no review, so retrying it is correct. Note the SHA is
	// the one we reviewed, not whatever the round just pushed — a new commit
	// legitimately warrants a fresh review on the next tick.
	if w.cfg.Polish.SelfReview && snap.PR.HeadRefOid != "" {
		state.SelfReviewedSHA = snap.PR.HeadRefOid
	}

	// A CHANGES_REQUESTED verdict is sticky: it stays set until the reviewer
	// looks again. Without an explicit re-request the fixes just sit there and
	// the flag never clears, so the loop would keep re-polishing the same
	// already-addressed comments forever.
	if cs.ChangesRequested {
		w.rerequestReviews(ctx, snap)
	}
	return LastActionPolished
}

// rerequestReviews asks every reviewer who requested changes to look again.
//
// Only HUMAN reviewers are re-requested. GitHub rejects a bot with HTTP 422
// ("Reviews may only be requested from collaborators") because an app is not a
// collaborator — and they do not need it: measured on kernel#8227, coderabbitai
// re-reviewed 75 seconds after the polish push with no request at all. Asking
// anyway would produce a guaranteed error on every round.
//
// Uses the REST endpoint rather than `gh pr edit --add-reviewer`. On kernel,
// gh's GraphQL path touches repository.pullRequest.projectCards, which is
// deprecated and errors the whole mutation — the command fails while looking
// like a harmless deprecation warning.
//
// Best-effort by design: the polish work is already pushed, so a failure to
// re-request is worth a warning but must not turn a successful round into a
// failed one, which would count toward the backoff that trips cooldown.
func (w *Watcher) rerequestReviews(ctx context.Context, snap *Snapshot) {
	var humans []string
	for _, r := range snap.ChangesRequestedBy {
		if snap.IsBotReviewer[r] {
			w.logger.Debug("skipping bot re-request; bots re-review on push", "reviewer", r)
			continue
		}
		humans = append(humans, r)
	}
	if len(humans) == 0 {
		return
	}
	owner, repo, err := repoSlugFromURL(snap.PR.URL)
	if err != nil {
		w.logger.Warn("cannot re-request review: unparseable PR URL", "error", err)
		return
	}
	for _, r := range humans {
		args := []string{
			"api", "--method", "POST",
			fmt.Sprintf("repos/%s/%s/pulls/%d/requested_reviewers", owner, repo, w.pr),
			"-f", "reviewers[]=" + r,
		}
		if res, err := w.gh.Run(ctx, args, w.workDir); err != nil {
			w.logger.Warn("could not re-request review",
				"reviewer", r, "error", safeErrString(ghError(err, res)))
			continue
		}
		w.status("PR #%d re-requested review from %s", w.pr, r)
	}
}

// classifyTransientFailure reports whether a failure was a provider-side outage
// rather than a fact about this PR, and if so returns the action that keeps it
// off the failure brake.
//
// A provider outage is not a fact about this PR. Counting it toward the failure
// brake spends the budget meant for "this PR is not converging" on "Anthropic
// returned 529", and three of those bought kernel#8031 a two-hour cooldown while
// nothing was wrong with the branch. The same reasoning applies to the merge
// path: GitHub being unreachable says nothing about whether the PR can land.
//
// Verified against the real strings: an API 529 classifies http_5xx and a
// force-completed turn classifies grace_forced, while a bare "exit status 1"
// stays non-transient and still counts.
//
// Every arm that turns a failure into a LastAction routes through here, so the
// exemption cannot drift between the polish, merge-rework, and merge paths — a
// provider blip has to mean the same thing whichever stage hit it.
//
// safe must already be scrubbed: a provider error can embed the endpoint config
// (API key / key-bearing env var), and these messages are persisted and Slacked.
//
// textOK says whether an UNMARKED error may still be exempted by matching its
// text. It is false on the merge path, where the only trustworthy signal is a
// producer marker — see classifyMergeFailure.
func (w *Watcher) classifyTransientFailure(stage string, err error, safe string, textOK bool) (LastAction, bool) {
	// A shell round never talks to the provider, so it can never have suffered a
	// provider outage. Checked BEFORE classification rather than after: the
	// classifier matches error text, and a command error carries captured
	// stdout/stderr, so a real failure whose output mentions "529" or "timeout"
	// would otherwise be exempted from the brake. See CommandRoundError.
	var cmdErr *CommandRoundError
	if errors.As(err, &cmdErr) {
		return "", false
	}
	// A marked GitHub outage is trusted directly rather than re-derived from the
	// text. The producer knows the call never got a verdict; the text does not,
	// because ghError embeds GitHub's stderr verbatim. See GitHubOutageError.
	var outageErr *GitHubOutageError
	if errors.As(err, &outageErr) {
		w.logger.Warn(stage+" hit a GitHub outage; not counting it toward the failure brake",
			"error", safe)
		w.status("PR #%d %s interrupted (github unreachable) — retrying next tick", w.pr, stage)
		return LastActionTransient, true
	}
	if !textOK {
		return "", false
	}
	transient, reason := agent.ClassifyTransient(err)
	if !transient {
		return "", false
	}
	w.logger.Warn(stage+" hit a transient provider error; not counting it toward the failure brake",
		"error", safe, "reason", reason)
	w.status("PR #%d %s interrupted (%s) — retrying next tick", w.pr, stage, reason)
	return LastActionTransient, true
}

// classifyAgentRoundFailure is the agent-round entry point: an agent error is
// usually untyped, so text matching is the only signal available and is allowed.
func (w *Watcher) classifyAgentRoundFailure(stage string, err error, safe string) (LastAction, bool) {
	return w.classifyTransientFailure(stage, err, safe, true)
}

// classifyMergeFailure is the merge-path entry point, and is MARKER-ONLY.
//
// Text matching is refused here because a merge error is GitHub's verdict about
// this PR rendered into a string, and ghError embeds that stderr verbatim. Real
// rejections carry check names, and check names contain words: a required check
// called "test_connection_timeout" matches the classifier's "timeout" token, so
// a text-based exemption would roll back MergeAttempts, skip rework, and spend
// neither brake on a PR that can never land — a permanent silent loop, which is
// strictly worse than the over-braking this change set out to fix.
//
// Only a producer that knows the call never got a verdict may exempt the merge
// path, by marking its error GitHubOutageError.
func (w *Watcher) classifyMergeFailure(err error, safe string) (LastAction, bool) {
	return w.classifyTransientFailure("merge", err, safe, false)
}

// reworkAfterFailedMerge decides what happens after a merge attempt fails.
//
// A merge can fail in ways /pr-polish will never fix, because they are about
// LANDING rather than code quality: the queue dequeued the PR, a speculative
// batch build went red, the base moved, a required check flipped after
// approval. Those get the repo's merge_rework rounds; the next tick then
// re-snapshots and re-evaluates from scratch.
//
// Two cases are terminal instead, because no agent round can resolve them:
// a merge policy that never merges, and a missing human approval.
func (w *Watcher) reworkAfterFailedMerge(ctx context.Context, snap *Snapshot, state *State) LastAction {
	switch {
	case w.cfg.Polish.MergePolicy == MergePolicyNotify:
		w.status("PR #%d merge failed and merge_policy is %q — stopping", w.pr, MergePolicyNotify)
		return LastActionNeedsHuman
	case snap.PR.ReviewDecision == "REVIEW_REQUIRED":
		w.status("PR #%d merge failed and needs a human approval — stopping", w.pr)
		return LastActionNeedsHuman
	case w.rework == nil || w.reworkSpec.Empty():
		// Nothing configured to try. Report a plain failure so backoff still
		// applies, rather than silently idling.
		w.logger.Warn("merge failed and no merge_rework configured", "pr", w.pr)
		return LastActionFailed
	}

	req := ReworkRequest{
		WorkDir:     w.workDir,
		Repo:        w.repo,
		Branch:      snap.PR.HeadRefName,
		PRURL:       snap.PR.URL,
		MergeError:  state.LastMergeError,
		MergePolicy: w.cfg.Polish.MergePolicy,
		Spec:        w.reworkSpec,
		Model:       w.cfg.Agent.Model,
		Cfg:         w.cfg.Polish,
		PRNumber:    w.pr,
		Attempt:     state.MergeAttempts,
	}
	w.status("PR #%d running merge rework (attempt %d)", w.pr, state.MergeAttempts)
	if _, err := w.rework.Run(ctx, req); err != nil {
		// Scrub before logging: a provider error can embed the endpoint
		// config that produced it, and these logs are persisted and Slacked.
		safe := safeErrString(err)
		if action, ok := w.classifyAgentRoundFailure("merge rework", err, safe); ok {
			return action
		}
		w.logger.Error("merge rework failed", "error", safe, "attempt", state.MergeAttempts)
		w.status("PR #%d merge rework failed: %s", w.pr, safe)
		return LastActionFailed
	}
	w.status("PR #%d merge rework completed — will retry merge next tick", w.pr)
	return LastActionReworked
}

// mergeOutcome is the result of one merge attempt.
type mergeOutcome int

const (
	// mergeOutcomeMerged means the merge is confirmed landed: the GitHub API
	// reported `.merged == true`.
	mergeOutcomeMerged mergeOutcome = iota
	// mergeOutcomeArmed means auto-merge was armed successfully but the queue
	// has not landed the PR yet. Keep polling.
	mergeOutcomeArmed
)

// merge lands the PR according to the configured policy and then VERIFIES the
// result against the GitHub API.
//
// Verification is not optional paranoia. `gh pr merge` exiting 0 does not mean
// the PR merged: under `--auto` it means auto-merge was armed, and a merge
// queue can report a populated merge_commit_sha for a candidate it only
// speculatively built. Likewise `state: CLOSED` is ambiguous — a PR closed
// without merging looks identical. The single unambiguous signal is
// `.merged == true` on the pulls endpoint, so that is the only thing we trust.
// needsSelfReview reports whether prdozer must ORIGINATE a review for this
// commit, on a repo that has no automated review bots.
//
// Every other polish trigger is reactive — new comments, CI failure, a moved
// base, a conflict. On a bot-less repo a healthy new PR fires none of them, so
// prdozer would declare it done without ever reviewing it: yoloswe#287 was
// closed out in ~1 second, three separate times, having never invoked
// /pr-polish. On kernel the bots (sycamore-groot, coderabbitai) supply the
// findings and the reactive model is enough; on yoloswe /pr-polish IS the
// reviewer, running codex/cursor locally to produce them.
//
// Keyed by head SHA so a commit is reviewed exactly once. An unconditional
// trigger would re-review an idle PR on every tick forever — the reactive
// triggers are self-limiting because comments get marked seen, but "no bot
// reviewed this" never stops being true on its own.
func (w *Watcher) needsSelfReview(snap *Snapshot, state *State) bool {
	if !w.cfg.Polish.SelfReview || snap.PR.HeadRefOid == "" {
		return false
	}
	return state.SelfReviewedSHA != snap.PR.HeadRefOid
}

// diverging reports whether polishing has stopped making the PR better.
//
// It is deliberately evaluated BEFORE the polish call and AFTER this tick's
// recordHealth, so the decision uses the outcome of work already done rather
// than predicting the next round — and so a snapshot that finally improves is
// scored before it can be used to stop the run.
func (w *Watcher) diverging(snap *Snapshot, state *State) bool {
	limit := w.cfg.Backoff.MaxRoundsWithoutImprovement
	if limit <= 0 || state.BestHealth == nil {
		return false
	}
	// An unreadable thread count means we cannot judge improvement; never stop
	// a run on missing data.
	if _, ok := snap.Health(); !ok {
		return false
	}
	if state.RoundsSinceImprovement < limit {
		return false
	}
	// The streak alone is not enough, because the metric it counts can stop
	// carrying information.
	//
	// BestHealth is a floor: BetterThan is strict, so once a run touches
	// {0 unresolved, ci green} nothing can ever beat it — 0 < 0 is false. From
	// that moment every polish round increments RoundsSinceImprovement no matter
	// what it accomplishes, and the guard degenerates into a plain round counter.
	//
	// Three production runs stopped this way, each having done real work:
	// kernel#8297 (5 commits, 3 new bugs found), yoloswe#288 (6 of 7 rounds
	// landed fixes, including a High-severity gating bug), yoloswe#291 (3 rounds:
	// the rework-arm exemption, multi-tick brake coverage, and producer-level
	// tests that caught a hole in the round before it). All three logged
	// best_unresolved=0 — saturated — and all three logged head_unreviewed=true.
	//
	// So require corroboration: a stop needs the streak AND some independent
	// sign that the streak still means something. Two signs qualify, and they
	// are deliberately OR'd rather than one standing in for the other:
	//
	//   saturated  — BestHealth is already the unbeatable floor (PRHealth.
	//                Saturated), so the streak is arithmetic, not measurement.
	//                Repo-agnostic: it is a property of the metric, not of who
	//                reviews the PR.
	//   unreviewed — the run pushed commits whose effect is not in the thread
	//                count yet, which is the opposite of evidence that it has
	//                stopped making progress.
	//
	// The saturated arm is why this is not gated on needsSelfReview alone.
	// needsSelfReview is false whenever `self_review` is off, so an
	// unreviewed-only test would leave every bot-reviewed repo (kernel) stopping
	// on the old saturation cutoff — the exact false positive this guard change
	// exists to remove, unfixed for the repo mode that hit it. "Must prdozer
	// ORIGINATE a review" and "is this metric still informative" are different
	// questions, and only the second one generalizes.
	//
	// A genuinely stuck run is still caught, because divergence that matters is
	// not saturated: kernel#8227 (17 rounds, ~$40, threads 6 -> 2 -> 11, CI red)
	// had red CI and never reached 0 threads, so neither arm fires and it stops
	// at `limit` exactly as before.
	//
	// The grace is BOUNDED, and that bound is load-bearing. On a self_review
	// repo every polish round pushes a commit, which moves the head, which makes
	// needsSelfReview true again; a saturated run likewise stays saturated
	// forever, since nothing can beat the floor. So an unconditional exemption
	// on either arm would mean the guard can never fire there at all, restoring
	// the unbounded run it exists to prevent. Doubling the limit buys a run a
	// second allowance to show progress and no more: past 2*limit the streak
	// stands on its own however the head or the metric looks.
	saturated := state.BestHealth.Saturated()
	unreviewed := w.needsSelfReview(snap, state)
	if state.RoundsSinceImprovement < 2*limit && (saturated || unreviewed) {
		w.logger.Info("divergence streak reached but the streak is not yet evidence; extending once",
			"rounds_since_improvement", state.RoundsSinceImprovement,
			"hard_limit", 2*limit,
			"best_saturated", saturated,
			"head_unreviewed", unreviewed,
			"head", snap.PR.HeadRefOid)
		return false
	}
	return true
}

// recordHealth folds this tick's observed health into the run's best-so-far,
// which is what the divergence guard measures against.
//
// Called on every tick rather than only after polishing, so a PR that improves
// on its own — a reviewer resolving threads, a flaky job going green on rerun —
// still resets the counter. Only polish rounds increment it.
//
// The snapshot was taken BEFORE this tick's action, so it measures the result
// of the PREVIOUS round, not the one about to run. That lag is deliberate: CI
// needs time to run after a push, so judging a round immediately would read a
// stale-green rollup and call a regression an improvement. The cost is that the
// guard trips one tick later than the round that actually caused the
// divergence.
func (w *Watcher) recordHealth(state *State, snap *Snapshot) bool {
	h, ok := snap.Health()
	if !ok {
		return false
	}
	// A round counts only when one is actually outstanding — i.e. the PREVIOUS
	// tick polished and this snapshot is the first look at its result. Charging
	// on the current action instead would mis-attribute by one tick; charging on
	// every tick would count a PR that is merely waiting for CI, having done no
	// work at all.
	polished := state.LastAction == LastActionPolished
	if polished {
		// Cumulative: counted whether or not the round helped, so the terminal
		// report can say how much work the run did as well as how long it has
		// been stuck.
		state.PolishRounds++
	}
	if state.BestHealth == nil || h.BetterThan(*state.BestHealth) {
		state.BestHealth = &h
		state.RoundsSinceImprovement = 0
		return true
	}
	if polished {
		state.RoundsSinceImprovement++
	}
	return polished
}

func (w *Watcher) merge(ctx context.Context, snap *Snapshot) (mergeOutcome, error) {
	policy := w.cfg.Polish.MergePolicy
	args, ok := policy.MergeArgs(w.pr)
	if !ok {
		return 0, fmt.Errorf("merge policy %q does not merge", policy)
	}

	// Re-derive owner/repo from the PR's own URL rather than trusting a
	// separately-configured slug, and assert the PR we are about to mutate is
	// the one we snapshotted. A stale PR number has previously been pointed at
	// an unrelated PR; the head-ref assertion below is what catches that.
	owner, repo, err := repoSlugFromURL(snap.PR.URL)
	if err != nil {
		return 0, fmt.Errorf("derive owner/repo for merge: %w", err)
	}
	if snap.PR.Number != w.pr {
		return 0, fmt.Errorf("refusing to merge: snapshot is PR #%d but watcher targets #%d", snap.PR.Number, w.pr)
	}

	res, err := w.gh.Run(ctx, args, w.workDir)
	if err != nil {
		// Distinguish "GitHub rejected this merge" from "GitHub was unreachable"
		// WITHOUT reading the rejection text. Parsing this stderr is not an
		// option: it is GitHub's verdict rendered into a string, real rejections
		// name the failing check, and a check called "test_connection_timeout"
		// would read as an outage — see classifyMergeFailure.
		//
		// So ask instead of guess. verifyMerged is a read-only probe on the same
		// endpoint: if it answers, GitHub is up and this failure was a real
		// verdict; if it too fails, the API is unreachable and the merge never got
		// one. That makes the outage marker come from an observation rather than
		// from the untrusted text.
		mergeErr := ghError(err, res)
		if _, probeErr := w.verifyMerged(ctx, owner, repo); probeErr != nil {
			return 0, &GitHubOutageError{Err: mergeErr}
		}
		return 0, mergeErr
	}

	merged, err := w.verifyMerged(ctx, owner, repo)
	if err != nil {
		return 0, fmt.Errorf("verify merge: %w", err)
	}
	if merged {
		return mergeOutcomeMerged, nil
	}
	if policy == MergePolicyQueue {
		// Expected: --auto arms the queue, which lands the PR asynchronously.
		return mergeOutcomeArmed, nil
	}
	// A direct merge policy reported success but the PR is not merged. Treat
	// this as a failure rather than silently declaring victory.
	return 0, fmt.Errorf("merge command succeeded but PR #%d is not merged (policy %q)", w.pr, policy)
}

// verifyMerged reports the authoritative merged bit from the GitHub API.
//
// This is a read-only probe, so a failure here is never GitHub rejecting the
// merge — it is GitHub being unreachable. That makes it unambiguously an outage,
// and it is marked as one so the failure brake is not spent on it.
func (w *Watcher) verifyMerged(ctx context.Context, owner, repo string) (bool, error) {
	res, err := w.gh.Run(ctx, []string{
		"api", fmt.Sprintf("repos/%s/%s/pulls/%d", owner, repo, w.pr),
		"--jq", ".merged",
	}, w.workDir)
	if err != nil {
		return false, &GitHubOutageError{Err: ghError(err, res)}
	}
	return strings.EqualFold(strings.TrimSpace(res.Stdout), "true"), nil
}

// recordSnapshot mutates s to reflect snap and action, returning true iff any
// persistable field changed (so the caller can skip disk writes on no-op ticks).
// Dry-run is fully observe-only: it skips every persisted-state mutation so a
// later live tick still sees the same triggers (new HEAD, new comments, new
// failed CI runs) and can react to them.
//
// mergeStateDirty tells it that decideAndAct already mutated MergeAttempts or
// LastMergeError on s. Without that signal a tick whose only change was a
// merge attempt would look unchanged and skip the disk write, losing the
// attempt count the unbounded-retry design depends on.
func (w *Watcher) recordSnapshot(s *State, snap *Snapshot, action LastAction, mergeStateDirty bool) bool {
	if action == LastActionDryRun {
		return false
	}

	firstRun := s.LastCheckAt.IsZero()
	dirty := firstRun || mergeStateDirty

	if s.LastSeenHeadSHA != snap.PR.HeadRefOid {
		s.LastSeenHeadSHA = snap.PR.HeadRefOid
		dirty = true
	}
	if snap.BaseSHA != "" && s.LastSeenBaseSHA != snap.BaseSHA {
		s.LastSeenBaseSHA = snap.BaseSHA
		dirty = true
	}

	commentIDs := make([]string, 0, len(snap.Comments))
	for _, c := range snap.Comments {
		commentIDs = append(commentIDs, c.ID)
	}
	if s.MergeSeenComments(commentIDs) {
		dirty = true
	}
	if s.MergeSeenRuns(snap.FailedRunIDs) {
		dirty = true
	}

	if s.LastAction != action {
		s.LastAction = action
		dirty = true
	}
	switch action {
	// A provider-side failure is not a fact about the PR, so it neither counts
	// toward the brake nor clears a streak already accumulated. Both arms below
	// are explicit case lists, so an unlisted action would already fall through
	// untouched; naming it here states the intent and makes adding it to the
	// success list — which would let one 529 launder a real streak back to
	// zero — a visible change rather than a silent one.
	case LastActionTransient:
		// ConsecutiveFailures deliberately untouched. Scoped to THIS counter: the
		// merge-attempt brake below still applies, because its increments are real
		// merge rejections that happened before rework was ever reached, and an
		// outage in the rework does not un-reject them. See
		// TestWatcher_RepeatedTransientRework_StillTripsMergeBrake.
	// LastActionReworked shares the failure arm so a straight run of reworks
	// still trips the ordinary backoff.
	case LastActionFailed, LastActionReworked:
		s.ConsecutiveFailures++
		dirty = true
		if w.cfg.Backoff.MaxConsecutiveFailures > 0 && s.ConsecutiveFailures >= w.cfg.Backoff.MaxConsecutiveFailures && w.cfg.Backoff.Cooldown > 0 {
			s.CooldownUntil = time.Now().Add(w.cfg.Backoff.Cooldown)
			w.logger.Warn("entering cooldown after repeated failures",
				"failures", s.ConsecutiveFailures,
				"until", s.CooldownUntil,
			)
		}
	case LastActionPolished, LastActionMerged, LastActionClosed, LastActionIdle,
		LastActionArmed, LastActionNeedsHuman:
		// A real successful/terminal tick clears any prior backoff.
		if s.ConsecutiveFailures != 0 || !s.CooldownUntil.IsZero() {
			dirty = true
		}
		s.ConsecutiveFailures = 0
		s.CooldownUntil = time.Time{}
	}

	// The merge-attempt brake. ConsecutiveFailures alone CANNOT bound merge
	// retries, because the real retry loop alternates rather than repeating:
	// rework rebases and pushes, which moves HEAD and re-triggers CI, so the
	// next tick polishes, polish succeeds, and the counter resets to zero.
	// Empirically the counter never exceeds 1, so the cooldown never trips and
	// an unmergeable PR burns agent budget forever with no Slack visibility.
	// (Same shape under MergePolicyQueue: a repeatedly-dequeued PR re-arms
	// cleanly each tick, which is also a "success".)
	//
	// MergeAttempts is the signal that actually tracks merge progress, and
	// nothing resets it — so brake on that instead. Merge attempts remain
	// unbounded in the sense that the run resumes after each cooldown and
	// keeps climbing; the cooldown just rate-limits it and makes it visible.
	if w.mergeBrakeTripped(s) {
		s.CooldownUntil = time.Now().Add(w.cfg.Backoff.Cooldown)
		s.CooldownFromAttempt = s.MergeAttempts
		dirty = true
		w.logger.Warn("entering cooldown after repeated merge attempts",
			"merge_attempts", s.MergeAttempts,
			"until", s.CooldownUntil,
		)
	}
	if dirty {
		s.LastCheckAt = snap.TakenAt
	}
	return dirty
}

// mergeBrakeTripped reports whether enough merge attempts have accumulated
// since the last cooldown to warrant another one.
//
// It measures attempts SINCE the previous cooldown (CooldownFromAttempt) so a
// run that resumes after backing off gets a fresh allowance rather than
// re-tripping on every subsequent tick forever.
func (w *Watcher) mergeBrakeTripped(s *State) bool {
	maxAttempts := w.cfg.Backoff.MaxConsecutiveFailures
	if maxAttempts <= 0 || w.cfg.Backoff.Cooldown <= 0 {
		return false
	}
	// Already cooling down; don't extend the window on every tick.
	if !s.CooldownUntil.IsZero() && time.Now().Before(s.CooldownUntil) {
		return false
	}
	return s.MergeAttempts-s.CooldownFromAttempt >= maxAttempts
}

func (w *Watcher) status(format string, args ...interface{}) {
	if w.renderer != nil {
		w.renderer.Status(fmt.Sprintf(format, args...))
	}
}
