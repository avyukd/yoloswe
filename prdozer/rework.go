package prdozer

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/multiagent/agent"
)

// ReworkRunner drives the merge_rework rounds after a failed merge.
type ReworkRunner interface {
	Run(ctx context.Context, req ReworkRequest) (ReworkResult, error)
}

// ReworkRequest is everything a rework step needs.
type ReworkRequest struct {
	WorkDir string
	Repo    string
	Branch  string
	PRURL   string
	// MergeError is the verbatim gh stderr. Passing the real message matters:
	// "dequeued", "unmergeable" and "strategy rejected" demand completely
	// different fixes, and a summarized error loses that.
	MergeError  string
	MergePolicy MergePolicy
	Spec        StepSpec
	Model       string
	Cfg         PolishConfig
	PRNumber    int
	Attempt     int
}

// ReworkResult reports what the rounds produced.
type ReworkResult struct {
	// RoundOutputs is one entry per executed round, in order.
	RoundOutputs []string
	Rounds       int
}

// AgentRework runs merge_rework rounds: prompt rounds open an agent session,
// command rounds run via sh -c. It reuses the same provider wiring as
// AgentPolisher so both paths behave identically.
type AgentRework struct {
	renderer *render.Renderer
	logger   *slog.Logger
	// runLog, when set, receives per-round logs and events.
	runLog *RunLog
	// newProvider builds the agent provider a round runs on. It is a field only
	// so tests can substitute a fake and observe the options runAgent assembles;
	// production always leaves it at agent.NewProviderForModel. AgentPolisher
	// carries the same seam — without it here, nothing could assert that both
	// routes really share baseProviderOpts.
	newProvider func(agent.AgentModel) (agent.Provider, error)
}

// NewAgentRework builds a rework runner. runLog may be nil.
func NewAgentRework(renderer *render.Renderer, logger *slog.Logger, runLog *RunLog) *AgentRework {
	if logger == nil {
		logger = slog.Default()
	}
	return &AgentRework{
		renderer:    renderer,
		logger:      logger,
		runLog:      runLog,
		newProvider: agent.NewProviderForModel,
	}
}

// commandTimeout bounds a single shell round so a hung command cannot wedge
// the watcher loop forever.
const commandTimeout = 10 * time.Minute

// CommandRoundError marks a failure that came from a shell round rather than
// from the provider.
//
// The transient exemption classifies by matching the rendered error text, which
// is what lets it recognize an untyped upstream 529. Command rounds deliberately
// embed up to 500 characters of captured stdout/stderr in their error, because
// that output is usually the diagnostic — so without this marker the matcher is
// reading arbitrary program output and a genuine failure whose output merely
// MENTIONS a transient-sounding token is exempted from the failure brake.
//
// Measured, not hypothetical: "exit status 1: request failed with 529 from
// upstream" classifies http_5xx, and a failing test named
// "test_retry_on_timeout" classifies timeout. Both are real failures that would
// have skipped the brake, which is worse than the over-braking this change set
// out to fix — a broken PR would loop forever with no cooldown.
//
// A shell round never talks to the provider, so a command failure is never a
// provider outage and never needs the exemption.
type CommandRoundError struct{ Err error }

func (e *CommandRoundError) Error() string { return e.Err.Error() }
func (e *CommandRoundError) Unwrap() error { return e.Err }

// Run executes the configured rounds in order. Each round's output is threaded
// into the next round's template as PrevOutput, so a command round can gather
// evidence (e.g. `gh pr view --json mergeStateStatus`) that the following agent
// round reasons about.
func (r *AgentRework) Run(ctx context.Context, req ReworkRequest) (ReworkResult, error) {
	if req.Spec.Empty() {
		return ReworkResult{}, nil
	}
	var res ReworkResult
	prevOutput := ""

	for i, round := range req.Spec.Rounds {
		data := ReworkData{
			Repo:        req.Repo,
			MergeError:  req.MergeError,
			Branch:      req.Branch,
			PRURL:       req.PRURL,
			MergePolicy: string(req.MergePolicy),
			PrevOutput:  prevOutput,
			PRNumber:    req.PRNumber,
			Attempt:     req.Attempt,
		}

		var (
			out string
			err error
		)
		if round.IsCommand() {
			out, err = r.runCommand(ctx, req, round.Command, data)
		} else {
			out, err = r.runAgent(ctx, req, round.Prompt, data, i+1)
		}
		if err != nil {
			return res, fmt.Errorf("merge_rework round %d/%d: %w", i+1, len(req.Spec.Rounds), err)
		}

		res.Rounds++
		res.RoundOutputs = append(res.RoundOutputs, out)
		prevOutput = out

		if r.runLog != nil {
			name := fmt.Sprintf("rework-a%d-r%d", req.Attempt, i+1)
			if logErr := r.runLog.WriteRoundLog(name, out); logErr != nil {
				r.logger.Warn("could not write rework round log", "round", name, "error", logErr)
			}
		}
	}
	return res, nil
}

func (r *AgentRework) runCommand(ctx context.Context, req ReworkRequest, tmpl string, data ReworkData) (string, error) {
	cmdStr, err := RenderRound(tmpl, data)
	if err != nil {
		return "", err
	}
	cctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	r.logger.Info("merge_rework command round", "pr", req.PRNumber, "command", truncate(cmdStr, 200))
	cmd := exec.CommandContext(cctx, "sh", "-c", cmdStr)
	cmd.Dir = req.WorkDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		// A command round's job is usually to GATHER evidence for the next
		// round. Its output is valuable even on a non-zero exit (that is often
		// the diagnostic), so return it alongside the error rather than
		// discarding it.
		return string(out), &CommandRoundError{
			Err: fmt.Errorf("command failed: %w: %s", err, truncate(strings.TrimSpace(string(out)), 500)),
		}
	}
	return string(out), nil
}

func (r *AgentRework) runAgent(ctx context.Context, req ReworkRequest, tmpl string, data ReworkData, round int) (string, error) {
	prompt, err := RenderRound(tmpl, data)
	if err != nil {
		return "", err
	}

	modelID := req.Spec.Model
	if modelID == "" {
		modelID = req.Model
	}
	model, ok := agent.ModelByID(modelID)
	if !ok {
		return "", fmt.Errorf("unknown model %q", modelID)
	}
	newProvider := r.newProvider
	if newProvider == nil {
		newProvider = agent.NewProviderForModel
	}
	provider, err := newProvider(model)
	if err != nil {
		return "", fmt.Errorf("create provider: %w", err)
	}
	defer provider.Close()

	logHandler := newPolishLogHandler(r.logger, req.PRNumber)
	var handler agent.EventHandler = logHandler
	if r.renderer != nil {
		handler = &compositeHandler{handlers: []agent.EventHandler{logHandler, &rendererHandler{r: r.renderer}}}
	}

	permMode := req.Cfg.PermissionMode
	if permMode == "" {
		permMode = "bypass"
	}
	opts := baseProviderOpts(req.WorkDir, permMode, modelID, handler)
	if req.Spec.Effort != "" {
		// Parse rather than cast: a typo'd effort should fail loudly here, not
		// be passed through to the provider as an unrecognized value.
		effort, err := agent.ParseEffort(req.Spec.Effort)
		if err != nil {
			return "", fmt.Errorf("merge_rework effort: %w", err)
		}
		opts = append(opts, agent.WithProviderEffort(effort))
	}
	if req.Cfg.MaxTurns > 0 {
		opts = append(opts, agent.WithProviderMaxTurns(req.Cfg.MaxTurns))
	}
	if req.Cfg.MaxBudgetUSD > 0 {
		opts = append(opts, agent.WithProviderMaxBudgetUSD(req.Cfg.MaxBudgetUSD))
	}

	r.logger.Info("merge_rework agent round",
		"pr", req.PRNumber, "round", round, "attempt", req.Attempt, "model", modelID)

	result, err := provider.Execute(ctx, prompt, nil, opts...)
	if err != nil {
		return "", fmt.Errorf("agent execution: %w", err)
	}
	if !result.Success {
		if result.Error != nil {
			return result.Text, result.Error
		}
		return result.Text, fmt.Errorf("merge_rework session failed (no error returned)")
	}
	return result.Text, nil
}

// baseProviderOpts builds the provider options every prdozer agent session
// needs, whatever route it came in on.
//
// It exists because the polish and merge_rework paths kept diverging. Each was
// written as its own option list, and each time polish was the one that lost a
// behavior rework already had: polish.model/effort honored only for rework
// (#288 round 1), and command rounds logging only on success and dropping their
// output on failure (#288 rounds 6 and the follow-up).
//
// Deliberately absent: WithProviderStreamTurnGracePeriod. Both routes drive
// skills that may background a tool call right at turn end, and the provider
// default (agent.DefaultStreamTurnGracePeriod, 10m) is tuned for exactly that —
// bramble reviewers under /pr-polish routinely run 3-7+ minutes before emitting
// a terminal event. merge_rework carried a hardcoded 60s override from #283
// whose stated intent was to widen that window; because a positive override
// replaces the default rather than extending it, it narrowed the window 10x
// instead. Polish, which set nothing, was the route that had it right.
//
// This is why kernel#8031 burned three ticks into a 2h cooldown with "stream
// idle: turn forced complete after grace period gated on background", and why
// kernel#8042 failed the same way at round 2/3. Unifying the two routes on the
// provider default fixes rework and leaves polish where it already was; any
// future tightening belongs in a config knob, not a constant here.
//
// Callers append their own route-specific options (effort, turn caps, budget);
// this covers only what both must have.
func baseProviderOpts(workDir, permMode, modelID string, handler agent.EventHandler) []agent.ExecuteOption {
	return []agent.ExecuteOption{
		agent.WithProviderWorkDir(workDir),
		agent.WithProviderPermissionMode(permMode),
		agent.WithProviderModel(modelID),
		// Load-bearing: without it the spawned agent cannot resolve user-level
		// skills, so a prompt invoking one silently resolves to nothing.
		agent.WithProviderKeepUserSettings(),
		agent.WithProviderEventHandler(handler),
	}
}
