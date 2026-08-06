package reviewer

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// claudeBackend wraps the Claude Code CLI as a Backend.
//
// Each RunPrompt call is a one-shot execution (no persistent session), matching
// the cursor backend. claude.QueryStream owns the process lifecycle: it starts
// the session, sends the prompt, and stops the session once a TurnCompleteEvent
// lands or the context is cancelled — so Start/Stop here are no-ops and the
// resume-fallback retry below cannot leak the first attempt's process.
//
// # Read-only reviews
//
// Unlike Codex, Claude needs no approval-handler gymnastics: the permission
// mode stays on bypass (so automation never blocks on a prompt) and
// Config.ReadOnly is enforced by refusing to hand the model the write tools at
// all.
//
// The two settings do compose — verified live 2026-08-06 by running the CLI
// with --permission-mode bypassPermissions --disallowed-tools Write and asking
// it to write a file: the model reported "the Write tool isn't available in
// this session". Worth pinning explicitly, because SDK_PROTOCOL.md describes
// bypassPermissions as "bypass all checks", which reads as though the deny list
// would lose.
//
// What that same run also showed: told it could not use Write, the model
// reached for Bash and created the file anyway. ReadOnly withholds the write
// *tools*; it is not a filesystem guarantee. Bash is deliberately still granted
// (a reviewer needs git log/diff/show), so — exactly as with Codex — the prompt
// is the last line of defence against a destructive command. Callers that need
// a real filesystem boundary must sandbox the process.
//
// # Settings isolation
//
// The wrapper defaults SDK sessions to --setting-sources "" (see
// claude.WithKeepUserSettings), so a review does not inherit the operator's
// user/project settings or plugins. That is what we want: two runs of the same
// review on two machines should see the same tool surface.
type claudeBackend struct {
	config Config
}

func newClaudeBackend(config Config) *claudeBackend {
	return &claudeBackend{config: config}
}

// claudeQueryStream is the seam tests substitute to drive RunPrompt's
// two-attempt resume ladder without a live CLI. Production always uses
// claude.QueryStream.
var claudeQueryStream = claude.QueryStream

// Start is a no-op for claude (one-shot per prompt).
func (b *claudeBackend) Start(_ context.Context) error {
	return nil
}

// Stop is a no-op for claude (one-shot per prompt).
func (b *claudeBackend) Stop() error {
	return nil
}

func (b *claudeBackend) RunPrompt(ctx context.Context, prompt string, handler EventHandler) (*ReviewResult, error) {
	opts := b.baseSessionOptions()
	resumeOpts := opts
	var resumeStatus ResumeStatus
	if b.config.ResumeSessionID != "" {
		// Start at Unverified so an early exit (no Ready event) still
		// surfaces "resume was attempted" in the envelope, instead of
		// letting omitempty erase the signal entirely.
		resumeStatus = ResumeStatusUnverified
		resumeOpts = append(append([]claude.SessionOption{}, opts...), claude.WithResume(b.config.ResumeSessionID))
	}

	// A fresh tail per attempt: the fallback attempt must not re-read the
	// first attempt's stderr and mistake it for its own failure.
	tail := newClaudeStderrTail()
	result, err := b.runPromptWithOptions(ctx, prompt, handler, resumeOpts, tail, resumeStatus, b.config.ResumeSessionID)
	if err != nil && b.config.ResumeSessionID != "" && isClaudeResumeUnavailable(err, tail.String()) {
		slog.Warn("claude resume failed; falling back to fresh session", "session_id", b.config.ResumeSessionID, "error", err.Error())
		return b.runPromptWithOptions(ctx, prompt, handler, opts, newClaudeStderrTail(), ResumeStatusFallback, "")
	}
	return result, err
}

// isClaudeResumeUnavailable reports whether a failed attempt failed *because*
// the requested session is gone.
//
// The Go error alone is not enough. The Claude CLI prints
//
//	No conversation found with session ID: <id>
//
// to its stderr and then exits during startup, so what surfaces to the caller
// is the downstream symptom — "SDK initialize handshake failed: control
// request timed out" — which is indistinguishable from a genuinely wedged CLI.
// Verified live 2026-08-06; without consulting stderr the fallback never fires
// and every stale resume id costs a whole review round.
func isClaudeResumeUnavailable(err error, stderrTail string) bool {
	if err == nil {
		return false
	}
	return isResumeUnavailableMessage(err.Error()) || isResumeUnavailableMessage(stderrTail)
}

// claudeUsageTokens maps a claude turn's usage onto the reviewer's
// (input, output) token pair.
//
// TotalInputTokens, not Usage.InputTokens: under Anthropic's accounting the
// latter counts only *fresh* prompt tokens, with cache-creation and cache-read
// reported separately. Codex's same-named field is already the full prompt
// total, so reporting the fresh count here would make claude look artificially
// cheap in the cross-backend token comparison — most of all on resumed turns,
// where nearly the whole context is served from cache.
func claudeUsageTokens(u claude.TurnUsage) (input, output int64) {
	return int64(u.TotalInputTokens()), int64(u.OutputTokens)
}

// claudeStderrTailLimit bounds the retained stderr tail. Only the last few KiB
// matter — the resume-miss line is printed immediately before the CLI exits —
// and an unbounded buffer would grow with every noisy backend line for the
// whole review.
const claudeStderrTailLimit = 8 << 10

// claudeStderrTail forwards each stderr chunk to the operator-facing handler
// while retaining a bounded tail for failure classification. The CLI writes
// stderr from a reader goroutine, so all access is mutex-guarded.
type claudeStderrTail struct {
	forward func([]byte)
	buf     []byte
	mu      sync.Mutex
}

func newClaudeStderrTail() *claudeStderrTail {
	return &claudeStderrTail{forward: stderrPrefixHandler("claude")}
}

func (t *claudeStderrTail) handle(data []byte) {
	t.mu.Lock()
	t.buf = append(t.buf, data...)
	if len(t.buf) > claudeStderrTailLimit {
		t.buf = t.buf[len(t.buf)-claudeStderrTailLimit:]
	}
	t.mu.Unlock()
	if t.forward != nil {
		t.forward(data)
	}
}

func (t *claudeStderrTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// claudeReadOnlyDisallowedTools names the tools withheld when Config.ReadOnly
// is set. This is the file-mutation surface of the default tool set; Bash is
// deliberately not in the list because a reviewer needs it to inspect the diff
// (git log/diff/show), which is the same trade-off the Codex backend makes with
// its approval handler.
var claudeReadOnlyDisallowedTools = []string{"Write", "Edit", "NotebookEdit"}

// claudeAlwaysDisallowedTools names tools withheld from every claude review,
// read-only or not.
//
// ReportFindings is the Claude CLI's own structured code-review channel. Left
// available, the model treats it as the place its findings belong and replies
// in prose — leaving the reviewer's JSON contract unfulfilled and the envelope
// reporting "no JSON object found in response". Observed live on a resumed
// turn (2026-08-06): the model called ReportFindings twice and returned a prose
// summary. Withholding the tool leaves the response text as the only channel,
// which is exactly the contract BuildJSONPromptWithScope asks for.
var claudeAlwaysDisallowedTools = []string{"ReportFindings"}

func (b *claudeBackend) baseSessionOptions() []claude.SessionOption {
	var opts []claude.SessionOption
	if b.config.Model != "" {
		opts = append(opts, claude.WithModel(b.config.Model))
	}
	if b.config.WorkDir != "" {
		opts = append(opts, claude.WithWorkDir(b.config.WorkDir))
	}
	if effort, ok := claudeEffortLevel(b.config.Effort); ok {
		opts = append(opts, claude.WithEffort(effort))
	}
	// Bypass keeps automation from stalling on an approval prompt; ReadOnly is
	// enforced by withholding the write tools rather than by denying approvals.
	opts = append(opts, claude.WithPermissionMode(claude.PermissionModeBypass))
	disallowed := append([]string{}, claudeAlwaysDisallowedTools...)
	if b.config.ReadOnly {
		disallowed = append(disallowed, claudeReadOnlyDisallowedTools...)
	}
	opts = append(opts, claude.WithDisallowedTools(disallowed...))
	return opts
}

// claudeEffortLevel maps the reviewer's free-form --effort string onto a
// claude.EffortLevel. An empty value returns ok=false so no --effort flag is
// passed and the CLI/model default applies.
//
// An unrecognized value warns and is dropped rather than failing the run: the
// flag is shared with the codex backend, whose accepted vocabulary differs, and
// losing a whole review round to a typo'd effort level is a worse outcome than
// running at the model default.
func claudeEffortLevel(effort string) (claude.EffortLevel, bool) {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "":
		return "", false
	case "auto":
		// Explicitly "let the model decide" — same wire effect as unset.
		return "", false
	case "low":
		return claude.EffortLow, true
	case "medium", "med":
		return claude.EffortMed, true
	case "high":
		return claude.EffortHigh, true
	case "max":
		return claude.EffortMax, true
	default:
		slog.Warn("unrecognized effort for claude backend; using model default",
			"effort", effort,
			"supported", "low, medium, high, max")
		return "", false
	}
}

func (b *claudeBackend) runPromptWithOptions(ctx context.Context, prompt string, handler EventHandler, opts []claude.SessionOption, tail *claudeStderrTail, resumeStatus ResumeStatus, requestedResumeID string) (*ReviewResult, error) {
	// Appended here, not in baseSessionOptions, so each attempt gets its own
	// tail (see RunPrompt).
	opts = append(append([]claude.SessionOption{}, opts...), claude.WithStderrHandler(tail.handle))

	// One derived context drives both QueryStream and the adapter, and is
	// cancelled on every return path.
	//
	// The adapter goroutine needs it so an early return (idle timeout,
	// fallback retry) doesn't leak it. QueryStream needs it for a second
	// reason: its proxy goroutine forwards events with `case out <- evt` and
	// only gives up on ctx.Done. Once the bridge stops reading, an
	// attempt-scoped context is the only thing that unblocks that send and
	// lets the deferred session.Stop run — passing the caller's ctx instead
	// would keep the CLI session alive until the whole review ends, which on
	// the resume-fallback path means both attempts' sessions running at once.
	queryCtx, cancelQuery := context.WithCancel(ctx)
	defer cancelQuery()

	events, err := claudeQueryStream(queryCtx, prompt, opts...)
	if err != nil {
		return reviewErrorResult(resumeStatus, fmt.Errorf("claude query failed: %w", err))
	}

	adapterCtx := queryCtx

	var sessionMu sync.Mutex
	var actualSessionID string
	adapter := &claudeEventAdapter{
		handler: handler,
		events:  events,
		onSession: func(id string) {
			sessionMu.Lock()
			defer sessionMu.Unlock()
			actualSessionID = id
		},
	}
	bridged, err := bridgeStreamEvents(adapterCtx, adapter.filtered(adapterCtx), handler, "", b.config.IdleTimeout)
	sessionMu.Lock()
	readySessionID := actualSessionID
	sessionMu.Unlock()
	if readySessionID != "" {
		resumeStatus = resumeStatusAfterSessionReady(resumeStatus, requestedResumeID, readySessionID)
	}
	if err != nil {
		return reviewErrorResult(resumeStatus, fmt.Errorf("claude: %w", err))
	}

	result := &ReviewResult{
		ResponseText: bridged.responseText,
		Success:      bridged.success,
		DurationMs:   bridged.durationMs,
		ResumeStatus: resumeStatus,
	}

	// Token usage and turn-level errors live on the raw turn event.
	if tc, ok := bridged.turnEvent.(claude.TurnCompleteEvent); ok {
		result.InputTokens, result.OutputTokens = claudeUsageTokens(tc.Usage)
		if tc.Error != nil {
			if handler != nil {
				handler.OnError(tc.Error, "turn_complete")
			}
			result.Success = false
			result.ErrorMessage = tc.Error.Error()
			return result, fmt.Errorf("claude turn failed: %w", tc.Error)
		}
	}

	return result, nil
}

// claudeEventAdapter re-emits claude events, handling ReadyEvent out of band.
//
// ReadyEvent maps to agentstream.KindReady, which bridgeStreamEvents does not
// dispatch to the handler — so without this adapter OnSessionInfo would never
// fire and both session_id and model would drop out of the result envelope
// (breaking --resume-session-id for every downstream caller).
type claudeEventAdapter struct {
	handler   EventHandler
	events    <-chan claude.Event
	onSession func(string)
}

// filtered returns a channel that re-emits claude events with ReadyEvent
// intercepted. The context unblocks sends when the consumer
// (bridgeStreamEvents) returns early, preventing goroutine leaks.
func (a *claudeEventAdapter) filtered(ctx context.Context) <-chan claude.Event {
	out := make(chan claude.Event)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-a.events:
				if !ok {
					return
				}
				if e, isReady := ev.(claude.ReadyEvent); isReady {
					if a.onSession != nil {
						a.onSession(e.Info.SessionID)
					}
					if a.handler != nil {
						a.handler.OnSessionInfo(e.Info.SessionID, e.Info.Model)
					}
					continue
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}
