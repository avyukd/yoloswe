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
}

// NewAgentRework builds a rework runner. runLog may be nil.
func NewAgentRework(renderer *render.Renderer, logger *slog.Logger, runLog *RunLog) *AgentRework {
	if logger == nil {
		logger = slog.Default()
	}
	return &AgentRework{renderer: renderer, logger: logger, runLog: runLog}
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
	provider, err := agent.NewProviderForModel(model)
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
	opts := []agent.ExecuteOption{
		agent.WithProviderWorkDir(req.WorkDir),
		agent.WithProviderPermissionMode(permMode),
		agent.WithProviderModel(modelID),
		// Load-bearing: without it the spawned agent cannot resolve
		// user-level skills, so a prompt invoking one silently resolves to
		// nothing.
		agent.WithProviderKeepUserSettings(),
		agent.WithProviderEventHandler(handler),
		// Rework prompts drive skills that may background a tool call right at
		// turn end. Give the turn room to finish rather than force-completing
		// it mid-flight.
		agent.WithProviderStreamTurnGracePeriod(streamTurnGrace),
	}
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

// streamTurnGrace gives a turn time to settle an outstanding background tool
// call before it is force-completed.
const streamTurnGrace = 60 * time.Second
