package reviewer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/acp"
)

// geminiBackend wraps the Gemini ACP client as a Backend.
// The ACP client process is started once in Start() and kept alive across
// RunPrompt calls to amortize startup cost. Each RunPrompt uses one ACP
// session: it loads ResumeSessionID when configured, falls back to a fresh
// session when resume is unavailable, otherwise starts fresh. FollowUp has no
// implicit shared conversation unless the caller supplied ResumeSessionID,
// unlike the codex backend which can reuse its in-memory thread.
//
// # Verified model IDs (tested against live Gemini CLI, 2026-04-21)
//
//   - gemini-3.1-flash-lite-preview: ~670ms turn, working
//   - gemini-2.5-flash: ~1700ms turn, working
//
// # Known stderr noise
//
// The Gemini CLI emits these lines on every startup; they are harmless:
//
//	Loaded cached credentials.
//	[STARTUP] Phase 'cli_startup' was started but never ended. Skipping metrics.
type geminiBackend struct {
	client *acp.Client
	config Config
}

func newGeminiBackend(config Config) *geminiBackend {
	return &geminiBackend{config: config}
}

func (b *geminiBackend) Start(ctx context.Context) error {
	binaryArgs := []string{"--experimental-acp"}
	if b.config.Model != "" {
		binaryArgs = append(binaryArgs, "--model", b.config.Model)
	}
	client := acp.NewClient(
		acp.WithClientName("gemini-review"),
		acp.WithClientVersion("1.0.0"),
		acp.WithBinaryArgs(binaryArgs...),
		acp.WithStderrHandler(stderrPrefixHandler("gemini")),
	)
	if err := client.Start(ctx); err != nil {
		return fmt.Errorf("gemini: failed to start ACP client: %w", err)
	}
	b.client = client
	return nil
}

func (b *geminiBackend) Stop() error {
	if b.client != nil {
		return b.client.Stop()
	}
	return nil
}

func (b *geminiBackend) RunPrompt(ctx context.Context, prompt string, handler EventHandler) (*ReviewResult, error) {
	if b.client == nil {
		return nil, fmt.Errorf("gemini: backend not started")
	}

	var sessionOpts []acp.SessionOption
	if b.config.WorkDir != "" {
		sessionOpts = append(sessionOpts, acp.WithSessionCWD(b.config.WorkDir))
	}

	var resumeStatus ResumeStatus
	var session *acp.Session
	var err error
	if b.config.ResumeSessionID != "" {
		// Start at Unverified so a bridge crash or non-recognized error
		// from LoadSession still surfaces "resume was attempted" in the
		// envelope, instead of letting omitempty erase the signal.
		resumeStatus = ResumeStatusUnverified
		session, err = b.client.LoadSession(ctx, b.config.ResumeSessionID, sessionOpts...)
		if err != nil && errors.Is(err, acp.ErrSessionNotFound) {
			slog.Warn("gemini resume unavailable; falling back to fresh session", "session_id", b.config.ResumeSessionID, "error", err.Error())
			resumeStatus = ResumeStatusFallback
			session, err = b.client.NewSession(ctx, sessionOpts...)
		} else if err == nil {
			resumeStatus = ResumeStatusOK
		}
	} else {
		session, err = b.client.NewSession(ctx, sessionOpts...)
	}
	if err != nil {
		return reviewErrorResult(resumeStatus, fmt.Errorf("gemini: failed to create session: %w", err))
	}
	resumeStatus = resumeStatusAfterSessionReady(resumeStatus, b.config.ResumeSessionID, session.ID())
	if handler != nil {
		// ACP does not emit a ReadyEvent equivalent with model info; report
		// what we configured so callers see a stable session start.
		handler.OnSessionInfo(session.ID(), b.config.Model)
	}

	// Derived context unblocks the filter goroutine's sends on early return.
	adapterCtx, adapterCancel := context.WithCancel(ctx)
	defer adapterCancel()

	// Prompt blocks until turn complete while events are delivered through
	// the event channel, so the bridge must drain concurrently.
	// ONE channel carrying the bridge's whole outcome. Splitting result from
	// error across two channels forced the reader to reassemble them, and a
	// non-blocking drain cannot observe a value the goroutine has not sent yet:
	// on SIGTERM, signal.NotifyContext cancels ctx and the derived adapterCtx in
	// the same call, so the reader's ctx.Done is ready before the bridge
	// goroutine is even scheduled. The drain returned nil and the preserved text
	// was discarded on precisely the path it was added for. With one channel
	// there are no halves to race.
	type bridgeOutcome struct {
		res *bridgeResult
		err error
	}
	outcome := make(chan bridgeOutcome, 1)
	go func() {
		r, err := bridgeStreamEvents(adapterCtx, filterGeminiEvents(adapterCtx, b.client.Events()), handler, "", b.config.IdleTimeout)
		outcome <- bridgeOutcome{res: r, err: err}
	}()

	_, promptErr := session.Prompt(ctx, prompt)

	if promptErr != nil {
		// Cancel the adapter context to unblock the bridge goroutine on the
		// error path so we can drain any partial output already streamed.
		adapterCancel()
	}

	var r *bridgeResult
	// On cancellation, WAIT for the bridge instead of racing it. Cancelling ctx
	// also cancels adapterCtx, so the bridge is already unblocked and about to
	// send — polling for its value the instant we notice the cancellation is
	// what threw the partial text away. adapterCancel() is called explicitly so
	// this holds even for a cancellation that did not derive from ctx.
	select {
	case <-ctx.Done():
		adapterCancel()
		out := <-outcome
		if out.err != nil {
			return reviewPartialResult(resumeStatus, out.res, ctx.Err())
		}
		// The bridge finished cleanly in the same instant the context died.
		// The review is complete, so keep it rather than reporting a failure.
		r = out.res
	case out := <-outcome:
		if out.err != nil {
			if promptErr != nil {
				return reviewPartialResult(resumeStatus, out.res, fmt.Errorf("gemini: prompt failed: %w (bridge: %v)", promptErr, out.err))
			}
			return reviewPartialResult(resumeStatus, out.res, fmt.Errorf("gemini: %w", out.err))
		}
		r = out.res
	}

	if promptErr != nil {
		return &ReviewResult{
			ResponseText: r.responseText,
			Success:      false,
			DurationMs:   r.durationMs,
			ErrorMessage: promptErr.Error(),
			ResumeStatus: resumeStatus,
		}, fmt.Errorf("gemini: prompt failed: %w", promptErr)
	}

	if tc, ok := r.turnEvent.(acp.TurnCompleteEvent); ok && tc.Error != nil {
		if handler != nil {
			handler.OnError(tc.Error, "turn_complete")
		}
		return &ReviewResult{
			ResponseText: r.responseText,
			Success:      false,
			DurationMs:   r.durationMs,
			ErrorMessage: tc.Error.Error(),
			ResumeStatus: resumeStatus,
		}, fmt.Errorf("gemini turn failed: %w", tc.Error)
	}

	return &ReviewResult{
		ResponseText: r.responseText,
		Success:      r.success,
		DurationMs:   r.durationMs,
		ResumeStatus: resumeStatus,
	}, nil
}

// filterGeminiEvents re-emits ACP events, dropping infrastructure events
// (ClientReady/SessionCreated) and normalizing tool names on start/update
// events so the bridge and downstream renderer see the display name.
func filterGeminiEvents(ctx context.Context, events <-chan acp.Event) <-chan acp.Event {
	out := make(chan acp.Event)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				switch e := ev.(type) {
				case acp.ClientReadyEvent, acp.SessionCreatedEvent:
					// agentstream.KindUnknown; bridge would drop anyway.
					continue
				case acp.ToolCallStartEvent:
					e.ToolName = formatGeminiToolDisplay(e.ToolName, e.Input)
					ev = e
				case acp.ToolCallUpdateEvent:
					e.ToolName = formatGeminiToolDisplay(e.ToolName, e.Input)
					ev = e
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

// geminiToolDisplay renders Gemini's ACP tool calls for terminal output.
// Entries here reflect tools observed in live Gemini CLI runs; unknown names
// fall through to geminiFallbackName (strip _file/_text/_dir).
var geminiToolDisplay = toolDisplay{
	tools: map[string]toolInfo{
		"read_file":  {Display: "read", ArgKey: "path", ArgFormat: argFormatPath},
		"write_file": {Display: "write", ArgKey: "path", ArgFormat: argFormatPath},
		"edit":       {Display: "edit", ArgKey: "path", ArgFormat: argFormatPath},
		"list_dir":   {Display: "ls", ArgKey: "path", ArgFormat: argFormatPath},
		"run_shell":  {Display: "shell", ArgKey: "command", ArgFormat: argFormatCommand},
		"bash":       {Display: "shell", ArgKey: "command", ArgFormat: argFormatCommand},
		"glob":       {Display: "glob", ArgKey: "pattern", ArgFormat: argFormatPlain},
		"grep":       {Display: "grep", ArgKey: "pattern", ArgFormat: argFormatPlain},
		"web_fetch":  {Display: "fetch", ArgKey: "url", ArgFormat: argFormatLongIdentifier},
		"web_search": {Display: "search", ArgKey: "query", ArgFormat: argFormatQuery},
	},
	fallback: geminiFallbackName,
}

// geminiFallbackName trims common suffixes from snake_case tool names.
func geminiFallbackName(name string) string {
	for _, suffix := range []string{"_file", "_text", "_dir"} {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix)
		}
	}
	return name
}

func formatGeminiToolDisplay(name string, input map[string]interface{}) string {
	return geminiToolDisplay.format(name, input)
}
