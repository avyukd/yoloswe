package prdozer

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/multiagent/agent"
)

// PolishRunner runs the /pr-polish skill for a single PR. The interface lets
// tests substitute a fake without standing up a real Claude session.
type PolishRunner interface {
	Run(ctx context.Context, req PolishRequest) (PolishResult, error)
}

// PolishRequest carries everything the polish runner needs.
type PolishRequest struct {
	// Spec, when non-empty, replaces the single "/pr-polish" call with its
	// rounds. Rounds marked `once` are dropped once DoneOnceRounds keys them.
	Spec StepSpec
	// DoneOnceRounds keys the `once: true` rounds this PR has already completed,
	// so they are dropped from this call. The caller owns the decision — it is
	// the only side that knows the PR's history (see State.OnceRoundsDone) — so
	// the polisher simply obeys. A whole-branch pass like /simplify-branch
	// improves an initial diff but churns an evolving one, which is what these
	// rounds are for.
	DoneOnceRounds map[string]bool
	WorkDir        string
	Model          string
	Branch         string
	PRURL          string
	Repo           string
	Cfg            PolishConfig
	PRNumber       int
	Local          bool
}

// PolishResult captures what came out of the polish session.
type PolishResult struct {
	// RanOnceRounds keys the `once: true` rounds that finished in this call. The
	// caller unions them into the PR's record so they are never repeated.
	//
	// Per round rather than one all-or-nothing flag, and reported by the runner
	// rather than assumed by the caller, because only the runner knows how far it
	// got. Rounds execute in order and stop at the first error, so a spec with
	// two once rounds whose second fails must keep the first's progress: a single
	// bool would either lose it (re-running a completed whole-branch pass) or
	// over-claim the second (dropping it for the life of the PR).
	RanOnceRounds map[string]bool
	// CompletedRounds counts the rounds that finished in this call, of EVERY
	// kind. RanOnceRounds cannot answer "did this invocation accomplish
	// anything" because it records only `once: true` rounds — a spec of ordinary
	// repeatable rounds whose first succeeds and second fails leaves it empty
	// while real work was done.
	//
	// The no-progress brake needs exactly that question: a run whose rounds keep
	// completing before a later one fails is making progress on every tick, and
	// halting it as barren is a false stop.
	SessionID       string
	Output          string
	DurationMs      int64
	CompletedRounds int
}

// AgentPolisher invokes /pr-polish through multiagent/agent — the same path
// jiradozer uses to drive Claude sessions.
type AgentPolisher struct {
	renderer *render.Renderer
	logger   *slog.Logger
	// newProvider builds the agent provider a round runs on. It is a field only
	// so tests can substitute a fake: everything runOne resolves — the model,
	// the effort, the permission mode, the per-tick caps — is observable
	// nowhere else, and asserting it through the real providers would mean
	// dispatching a live agent session.
	newProvider func(agent.AgentModel) (agent.Provider, error)
}

func NewAgentPolisher(renderer *render.Renderer, logger *slog.Logger) *AgentPolisher {
	if logger == nil {
		logger = slog.Default()
	}
	return &AgentPolisher{renderer: renderer, logger: logger, newProvider: agent.NewProviderForModel}
}

func (p *AgentPolisher) Run(ctx context.Context, req PolishRequest) (PolishResult, error) {
	if rounds := req.activeRounds(); len(rounds) > 0 {
		return p.runRounds(ctx, req, rounds)
	}
	return p.runOne(ctx, req, buildPolishPrompt(req.PRNumber, req.Local, req.Cfg.RoundsPerTick))
}

// activeRounds returns the rounds to run this tick: the repeatable ones plus
// any once-only round this PR has not completed yet.
//
// Returning empty routes Run back to the default single "/pr-polish" call. That
// matters for a spec whose rounds are ALL once-only: later ticks then still
// polish rather than silently doing nothing, which would leave a PR needing
// work with no way to get it.
func (req PolishRequest) activeRounds() []RoundSpec {
	if req.Spec.Empty() {
		return nil
	}
	out := make([]RoundSpec, 0, len(req.Spec.Rounds))
	for _, r := range req.Spec.Rounds {
		if r.Once && req.DoneOnceRounds[r.onceKey()] {
			continue
		}
		out = append(out, r)
	}
	return out
}

// modelID resolves which model this polish runs on.
//
// A configured spec may override the top-level agent model, exactly as
// merge_rework does — Polish reuses StepSpec, and the registry docs point at
// jiradozer configs that set a per-step model, so silently ignoring
// `polish.model` would misconfigure without a word.
func (req PolishRequest) modelID() string {
	if req.Spec.Model != "" {
		return req.Spec.Model
	}
	return req.Model
}

// runRounds executes a configured polish step, threading each round's output
// into the next as PrevOutput so a command round can gather evidence the
// following agent round reasons about.
func (p *AgentPolisher) runRounds(ctx context.Context, req PolishRequest, rounds []RoundSpec) (PolishResult, error) {
	var res PolishResult
	prev := ""
	start := time.Now()
	for i, round := range rounds {
		data := ReworkData{
			Repo:       req.Repo,
			Branch:     req.Branch,
			PRURL:      req.PRURL,
			PrevOutput: prev,
			// So a round can ask for the wired default call rather than a bare
			// "/pr-polish", which would drop the per-tick --rounds cap.
			DefaultPolishPrompt: buildPolishPrompt(req.PRNumber, req.Local, req.Cfg.RoundsPerTick),
			PRNumber:            req.PRNumber,
		}
		var (
			out string
			err error
		)
		if round.IsCommand() {
			out, err = p.runPolishCommand(ctx, req, round.Command, data)
		} else {
			prompt, rerr := RenderRound(round.Prompt, data)
			if rerr != nil {
				return res, fmt.Errorf("polish round %d/%d: render: %w", i+1, len(rounds), rerr)
			}
			r, rerr := p.runOne(ctx, req, prompt)
			out, err = r.Output, rerr
			if rerr == nil {
				res.SessionID = r.SessionID
			}
		}
		if err != nil {
			// res carries RanOnceRounds and CompletedRounds for the rounds that DID
			// finish, so the caller keeps their progress even though this call
			// failed.
			res.DurationMs = time.Since(start).Milliseconds()
			return res, fmt.Errorf("polish round %d/%d: %w", i+1, len(rounds), err)
		}
		// Counted for EVERY round kind, before the once-only bookkeeping below:
		// this is the signal for "did anything finish", which RanOnceRounds
		// cannot carry because it is silent on repeatable rounds.
		res.CompletedRounds++
		if round.Once {
			if res.RanOnceRounds == nil {
				res.RanOnceRounds = make(map[string]bool)
			}
			res.RanOnceRounds[round.onceKey()] = true
		}
		prev = out
		res.Output = out
	}
	res.DurationMs = time.Since(start).Milliseconds()
	return res, nil
}

// runPolishCommand runs a shell round, bounded so a hung command cannot wedge
// the watcher loop.
func (p *AgentPolisher) runPolishCommand(ctx context.Context, req PolishRequest, tmpl string, data ReworkData) (string, error) {
	cmdStr, err := RenderRound(tmpl, data)
	if err != nil {
		return "", fmt.Errorf("render command: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	// Log BEFORE running, as merge_rework's command round does: a hung command
	// sits here for the whole commandTimeout, and a log written only on success
	// is exactly the log a stalled watcher never produces.
	p.logger.Info("polish command round", "pr", req.PRNumber, "command", truncate(cmdStr, 200))
	cmd := exec.CommandContext(cctx, "sh", "-c", cmdStr)
	cmd.Dir = req.WorkDir
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, &CommandRoundError{
			Err: fmt.Errorf("command %q: %w", truncate(cmdStr, 80), err),
		}
	}
	return text, nil
}

func (p *AgentPolisher) runOne(ctx context.Context, req PolishRequest, prompt string) (PolishResult, error) {
	modelID := req.modelID()
	model, ok := agent.ModelByID(modelID)
	if !ok {
		return PolishResult{}, fmt.Errorf("unknown model %q", modelID)
	}
	newProvider := p.newProvider
	if newProvider == nil {
		newProvider = agent.NewProviderForModel
	}
	provider, err := newProvider(model)
	if err != nil {
		return PolishResult{}, fmt.Errorf("create provider: %w", err)
	}
	defer provider.Close()

	logger := p.logger
	logger.Info("invoking pr-polish",
		"pr", req.PRNumber,
		"local", req.Local,
		"model", modelID,
		"work_dir", req.WorkDir,
	)
	logger.Debug("pr-polish prompt", "prompt", prompt)

	logHandler := newPolishLogHandler(logger, req.PRNumber)
	var handler agent.EventHandler = logHandler
	if p.renderer != nil {
		handler = &compositeHandler{handlers: []agent.EventHandler{logHandler, &rendererHandler{r: p.renderer}}}
	}

	permMode := req.Cfg.PermissionMode
	if permMode == "" {
		permMode = "bypass"
	}
	opts := baseProviderOpts(req.WorkDir, permMode, modelID, handler)
	if req.Spec.Effort != "" {
		// Parse rather than cast: a typo'd effort should fail loudly here, not be
		// passed through to the provider as an unrecognized value.
		effort, err := agent.ParseEffort(req.Spec.Effort)
		if err != nil {
			return PolishResult{}, fmt.Errorf("polish effort: %w", err)
		}
		opts = append(opts, agent.WithProviderEffort(effort))
	}
	if req.Cfg.MaxTurns > 0 {
		opts = append(opts, agent.WithProviderMaxTurns(req.Cfg.MaxTurns))
	}
	if req.Cfg.MaxBudgetUSD > 0 {
		opts = append(opts, agent.WithProviderMaxBudgetUSD(req.Cfg.MaxBudgetUSD))
	}

	start := time.Now()
	result, err := provider.Execute(ctx, prompt, nil, opts...)
	if err != nil {
		return PolishResult{}, fmt.Errorf("agent execution: %w", err)
	}
	if !result.Success {
		if result.Error != nil {
			return PolishResult{}, result.Error
		}
		return PolishResult{}, fmt.Errorf("pr-polish session failed (no error returned)")
	}
	logger.Info("pr-polish completed",
		"pr", req.PRNumber,
		"session_id", result.SessionID,
		"input_tokens", result.Usage.InputTokens,
		"output_tokens", result.Usage.OutputTokens,
		"cost_usd", result.Usage.CostUSD,
		"duration_ms", result.DurationMs,
	)
	return PolishResult{
		SessionID:  result.SessionID,
		Output:     result.Text,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

func buildPolishPrompt(prNumber int, local bool, rounds int) string {
	var sb strings.Builder
	sb.WriteString("/pr-polish")
	if local {
		sb.WriteString(" --local")
	}
	// Bound the skill's INTERNAL round loop.
	//
	// /pr-polish runs its own loop inside a single polish.Run() call, so without
	// a cap one prdozer tick can absorb an unbounded amount of work: kernel#8227
	// ran 22 internal rounds across 64 minutes in ONE tick. The divergence guard
	// compares health BETWEEN ticks, so a tick that never ends is a tick the
	// guard is never consulted on — the runaway it exists to stop becomes
	// invisible to it.
	//
	// Capping rounds per tick makes the tick boundary meaningful: prdozer
	// re-snapshots, re-evaluates health, and can stop. Work is not lost — the
	// skill's budget resets on re-invoke and its state file persists, so the
	// next tick resumes where this one left off.
	if rounds > 0 {
		sb.WriteString(fmt.Sprintf(" --rounds %d", rounds))
	}
	if prNumber > 0 {
		sb.WriteString(fmt.Sprintf(" %d", prNumber))
	}
	return sb.String()
}

// polishLogHandler mirrors jiradozer's logEventHandler shape.
type polishLogHandler struct {
	logger     *slog.Logger
	toolStarts map[string]time.Time
	textBuf    strings.Builder
	pr         int
}

func newPolishLogHandler(logger *slog.Logger, prNumber int) *polishLogHandler {
	return &polishLogHandler{
		logger:     logger,
		pr:         prNumber,
		toolStarts: make(map[string]time.Time),
	}
}

func (h *polishLogHandler) flushText() {
	if h.textBuf.Len() > 0 {
		h.logger.Debug("agent text", "pr", h.pr, "text", truncate(h.textBuf.String(), 200))
		h.textBuf.Reset()
	}
}

func (h *polishLogHandler) OnSessionInit(sessionID string) {
	h.logger.Info("agent session init", "pr", h.pr, "session_id", sessionID)
}

func (h *polishLogHandler) OnText(text string) {
	h.textBuf.WriteString(text)
	if strings.Contains(text, "\n") || h.textBuf.Len() > 200 {
		h.flushText()
	}
}

func (h *polishLogHandler) OnThinking(thinking string) {
	h.flushText()
	h.logger.Debug("agent thinking", "pr", h.pr, "thinking", truncate(thinking, 200))
}

func (h *polishLogHandler) OnToolStart(name, id string, _ map[string]interface{}) {
	h.flushText()
	h.toolStarts[id] = time.Now()
}

func (h *polishLogHandler) OnToolComplete(name, id string, input map[string]interface{}, _ interface{}, isError bool) {
	attrs := []any{"pr", h.pr, "tool", name}
	if summary := render.FormatToolInput(name, input); summary != "" {
		attrs = append(attrs, "input", summary)
	}
	if start, ok := h.toolStarts[id]; ok {
		attrs = append(attrs, "duration", time.Since(start).Round(100*time.Millisecond))
		delete(h.toolStarts, id)
	}
	if isError {
		attrs = append(attrs, "error", true)
	}
	h.logger.Debug("tool", attrs...)
}

func (h *polishLogHandler) OnTurnComplete(turn int, success bool, durationMs int64, costUSD float64) {
	h.flushText()
	h.logger.Debug("turn complete",
		"pr", h.pr,
		"turn", turn,
		"success", success,
		"duration", fmt.Sprintf("%.1fs", float64(durationMs)/1000),
		"cost", fmt.Sprintf("$%.4f", costUSD),
	)
}

func (h *polishLogHandler) OnError(err error, ctx string) {
	h.flushText()
	clear(h.toolStarts)
	h.logger.Debug("agent error", "pr", h.pr, "error", err, "context", ctx)
}

func (h *polishLogHandler) OnRetry(attempt, max int, tool, _ string) {
	h.flushText()
	h.logger.Info("retry on tool error", "pr", h.pr, "attempt", attempt, "max", max, "tool", tool)
}

func (h *polishLogHandler) OnRetryAbort(reason, tool, _ string) {
	h.flushText()
	h.logger.Info("retry loop aborted", "pr", h.pr, "reason", reason, "tool", tool)
}

type rendererHandler struct {
	r *render.Renderer
}

func (h *rendererHandler) OnText(text string)  { h.r.Text(text) }
func (h *rendererHandler) OnThinking(t string) { h.r.Thinking(t) }
func (h *rendererHandler) OnToolStart(name, id string, _ map[string]interface{}) {
	h.r.ToolStart(name, id)
}
func (h *rendererHandler) OnToolComplete(name, id string, input map[string]interface{}, result interface{}, isError bool) {
	h.r.ToolComplete(name, input)
	if result != nil || isError {
		h.r.ToolResultForTool(name, id, result, isError)
	}
}
func (h *rendererHandler) OnTurnComplete(turn int, success bool, durationMs int64, costUSD float64) {
	h.r.TurnSummary(turn, success, durationMs, costUSD)
}
func (h *rendererHandler) OnError(err error, ctx string) { h.r.Error(err, ctx) }
func (h *rendererHandler) OnRetry(attempt, max int, tool, _ string) {
	h.r.Status(fmt.Sprintf("Retry %d/%d: tool error in %s", attempt, max, tool))
}
func (h *rendererHandler) OnRetryAbort(reason, tool, _ string) {
	h.r.Status(fmt.Sprintf("Retry loop aborted (%s) on tool %s", reason, tool))
}

type compositeHandler struct {
	handlers []agent.EventHandler
}

func (c *compositeHandler) OnText(text string) {
	for _, h := range c.handlers {
		h.OnText(text)
	}
}
func (c *compositeHandler) OnThinking(t string) {
	for _, h := range c.handlers {
		h.OnThinking(t)
	}
}
func (c *compositeHandler) OnToolStart(name, id string, input map[string]interface{}) {
	for _, h := range c.handlers {
		h.OnToolStart(name, id, input)
	}
}
func (c *compositeHandler) OnToolComplete(name, id string, input map[string]interface{}, result interface{}, isError bool) {
	for _, h := range c.handlers {
		h.OnToolComplete(name, id, input, result, isError)
	}
}
func (c *compositeHandler) OnTurnComplete(turn int, success bool, durationMs int64, costUSD float64) {
	for _, h := range c.handlers {
		h.OnTurnComplete(turn, success, durationMs, costUSD)
	}
}
func (c *compositeHandler) OnError(err error, ctx string) {
	for _, h := range c.handlers {
		h.OnError(err, ctx)
	}
}
func (c *compositeHandler) OnSessionInit(sessionID string) {
	for _, h := range c.handlers {
		if sh, ok := h.(agent.SessionInitHandler); ok {
			sh.OnSessionInit(sessionID)
		}
	}
}
func (c *compositeHandler) OnRetry(attempt, max int, tool, excerpt string) {
	for _, h := range c.handlers {
		if rh, ok := h.(agent.RetryHandler); ok {
			rh.OnRetry(attempt, max, tool, excerpt)
		}
	}
}
func (c *compositeHandler) OnRetryAbort(reason, tool, excerpt string) {
	for _, h := range c.handlers {
		if rh, ok := h.(agent.RetryHandler); ok {
			rh.OnRetryAbort(reason, tool, excerpt)
		}
	}
}

// secretPattern matches the shapes a leaked credential takes in an error
// string: an api-key/token/secret assignment, or a recognizable key prefix.
// Matching is case-insensitive and covers `k=v`, `k: v`, and `k=` forms.
var secretPattern = regexp.MustCompile(
	`(?i)((?:api[_-]?key|apikey|token|secret|password|authorization)\s*[:=]\s*)\S+` +
		`|\b(?:sk|pk|ghp|gho|ghs|ghu|github_pat)[-_][A-Za-z0-9_\-]{8,}`)

// redactSecrets scrubs credential-looking substrings from text before it is
// logged.
//
// Agent-provider errors can embed the endpoint configuration that produced
// them, which carries an API key or the name/value of the env var holding one
// (CodeQL go/clear-text-logging flags exactly this flow). Run logs are written
// to disk under ~/.prdozer/runs and fanned out to Slack, so anything reaching
// them is effectively published — scrub rather than trust the error's shape.
func redactSecrets(s string) string {
	return secretPattern.ReplaceAllString(s, "${1}[REDACTED]")
}

// safeErrString renders err as a scrubbed string. This is the form to hand to
// a logger, a run-log entry, or a notification.
//
// It deliberately returns a STRING rather than a wrapped error. A wrapper that
// redacts in Error() still carries the unredacted original underneath, so
// go/clear-text-logging follows the taint straight through it, and any
// downstream code that unwraps would see the raw message. Converting at the
// boundary severs the flow outright.
func safeErrString(err error) string {
	if err == nil {
		return ""
	}
	return redactSecrets(err.Error())
}

func truncate(s string, maxLen int) string {
	r := []rune(s)
	if len(r) > maxLen {
		return string(r[:maxLen]) + "..."
	}
	return s
}
