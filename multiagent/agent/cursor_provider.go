package agent

import (
	"context"
	"strings"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/cursor"
	"github.com/bazelment/yoloswe/wt"
)

// CursorProvider wraps the Cursor Agent SDK behind the Provider interface.
// Each Execute call creates a one-shot session (no persistent state).
type CursorProvider struct {
	events chan AgentEvent
}

// NewCursorProvider creates a new Cursor provider.
func NewCursorProvider() *CursorProvider {
	return &CursorProvider{
		events: make(chan AgentEvent, 100),
	}
}

func (p *CursorProvider) Name() string { return "cursor" }

func (p *CursorProvider) Execute(ctx context.Context, prompt string, wtCtx *wt.WorktreeContext, opts ...ExecuteOption) (*AgentResult, error) {
	cfg := applyOptions(opts)
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	// Cursor has no reasoning-effort knob — fail fast rather than silently
	// dropping the requested level. EffortAuto is the explicit "use the
	// provider default" sentinel, which a no-knob provider already satisfies,
	// so it passes through.
	if cfg.Effort != "" && cfg.Effort != EffortAuto {
		return nil, EffortUnsupportedError(p.Name(), cfg.Effort)
	}

	// Build full prompt with worktree context
	fullPrompt := prompt
	if wtCtx != nil {
		fullPrompt = wtCtx.FormatForPrompt() + "\n\n" + prompt
	}

	// Create session
	session := cursor.NewSession(fullPrompt, cursorSessionOpts(cfg)...)
	if err := session.Start(ctx); err != nil {
		return nil, err
	}
	defer session.Stop()

	// Tee session events: one copy for bridgeEvents (handler + AgentEvent channel),
	// one copy for local result collection. This avoids duplicating bridgeEvents logic.
	bridgeCh := make(chan cursor.Event, 100)
	bridgeStop := make(chan struct{})
	bridgeDone := make(chan struct{})
	go func() {
		bridgeEvents(bridgeCh, cfg.EventHandler, p.events, bridgeStop, "", nil)
		close(bridgeDone)
	}()
	defer func() {
		close(bridgeStop)
		<-bridgeDone
	}()
	defer close(bridgeCh)

	var resultText strings.Builder

	for evt := range session.Events() {
		// Forward to bridge goroutine (blocking; bridgeStop signals cancellation).
		select {
		case bridgeCh <- evt:
		case <-bridgeStop:
		}

		// Collect result locally
		switch e := evt.(type) {
		case cursor.TextEvent:
			resultText.WriteString(e.Text)
		case cursor.TurnCompleteEvent:
			agentResult := &AgentResult{
				Text:       resultText.String(),
				Success:    e.Success,
				DurationMs: e.DurationMs,
			}
			if e.Error != nil {
				agentResult.Error = e.Error
			}
			return agentResult, nil
		case cursor.ErrorEvent:
			return nil, e.Error
		}
	}

	// Channel closed without TurnCompleteEvent — treat as an error
	// even if we accumulated partial text.
	return nil, cursor.ErrSessionClosed
}

func (p *CursorProvider) Events() <-chan AgentEvent { return p.events }

func (p *CursorProvider) Close() error {
	close(p.events)
	return nil
}

// cursorSessionOpts builds the cursor session options for one Execute call.
// It is split out of Execute so a test can assert what actually reaches the
// CLI: pinning the model rule on its own leaves Execute free to stop calling
// it, which is the regression that broke cursor sessions in the first place.
func cursorSessionOpts(cfg ExecuteConfig) []cursor.SessionOption {
	var opts []cursor.SessionOption
	// applyOptions defaults Model to "sonnet" when the caller named nothing;
	// CLIModelArg strips it as a Claude ID, along with placeholders and any
	// other provider's models.
	if model := CLIModelArg(cfg.Model, ProviderCursor); model != "" {
		opts = append(opts, cursor.WithModel(model))
	}
	if cfg.WorkDir != "" {
		opts = append(opts, cursor.WithWorkDir(cfg.WorkDir))
	}
	// Cursor requires --trust for non-interactive use
	opts = append(opts, cursor.WithTrust())
	if !cfg.LLMEndpoint.IsZero() {
		opts = append(opts, cursor.WithLLMEndpoint(cfg.LLMEndpoint))
	}
	return opts
}
