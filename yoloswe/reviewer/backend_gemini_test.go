package reviewer

import (
	"context"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/acp"
)

func TestFormatGeminiToolDisplay(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
		input    map[string]interface{}
		want     string
	}{
		{
			name:     "read_file with path",
			toolName: "read_file",
			input:    map[string]interface{}{"path": "/home/user/project/pkg/file.go"},
			want:     "read .../pkg/file.go",
		},
		{
			name:     "write_file with path",
			toolName: "write_file",
			input:    map[string]interface{}{"path": "/home/user/project/main.go"},
			want:     "write .../project/main.go",
		},
		{
			name:     "run_shell with command",
			toolName: "run_shell",
			input:    map[string]interface{}{"command": "git diff HEAD~1"},
			want:     "shell: git diff HEAD~1",
		},
		{
			name:     "run_shell with long command truncated",
			toolName: "run_shell",
			input:    map[string]interface{}{"command": "git diff HEAD~1 --name-only -- some/very/long/path/that/exceeds/limit"},
			want:     "shell: git diff HEAD~1 --name-only -- some/very/long/p...",
		},
		{
			name:     "bash with command",
			toolName: "bash",
			input:    map[string]interface{}{"command": "ls -la"},
			want:     "shell: ls -la",
		},
		{
			name:     "glob with pattern",
			toolName: "glob",
			input:    map[string]interface{}{"pattern": "**/*.go"},
			want:     "glob **/*.go",
		},
		{
			name:     "grep with pattern",
			toolName: "grep",
			input:    map[string]interface{}{"pattern": "ParseMessage"},
			want:     "grep ParseMessage",
		},
		{
			name:     "list_dir with path",
			toolName: "list_dir",
			input:    map[string]interface{}{"path": "/home/user/project"},
			want:     "ls .../user/project",
		},
		{
			name:     "web_fetch with url",
			toolName: "web_fetch",
			input:    map[string]interface{}{"url": "https://example.com/api"},
			want:     "fetch https://example.com/api",
		},
		{
			name:     "web_search with long query truncated",
			toolName: "web_search",
			input:    map[string]interface{}{"query": "this is a very long search query that exceeds the limit set by our formatter implementation"},
			want:     "search this is a very long search query that exceeds the limit s...",
		},
		{
			name:     "unknown tool with _file suffix",
			toolName: "custom_file",
			input:    nil,
			want:     "custom",
		},
		{
			name:     "unknown tool without suffix",
			toolName: "custom_thing",
			input:    nil,
			want:     "custom_thing",
		},
		{
			name:     "read_file with nil input",
			toolName: "read_file",
			input:    nil,
			want:     "read",
		},
		{
			name:     "read_file with missing path key",
			toolName: "read_file",
			input:    map[string]interface{}{"other": "value"},
			want:     "read",
		},
		{
			name:     "read_file with empty path",
			toolName: "read_file",
			input:    map[string]interface{}{"path": ""},
			want:     "read",
		},
		{
			name:     "read_file with non-string path",
			toolName: "read_file",
			input:    map[string]interface{}{"path": 42},
			want:     "read",
		},
		{
			name:     "edit with path",
			toolName: "edit",
			input:    map[string]interface{}{"path": "/home/user/project/session.go"},
			want:     "edit .../project/session.go",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatGeminiToolDisplay(tt.toolName, tt.input)
			if got != tt.want {
				t.Errorf("formatGeminiToolDisplay(%q, %v) = %q, want %q", tt.toolName, tt.input, got, tt.want)
			}
		})
	}
}

func TestGeminiFallbackName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"create_file", "create"},
		{"delete_file", "delete"},
		{"read_text", "read"},
		{"list_dir", "list"},
		{"custom_thing", "custom_thing"},
		{"tool", "tool"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := geminiFallbackName(tt.input)
			if got != tt.want {
				t.Errorf("geminiFallbackName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func makeGeminiEventChan(events ...acp.Event) <-chan acp.Event {
	ch := make(chan acp.Event, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch
}

func collectFilteredGeminiEvents(ctx context.Context, events ...acp.Event) []acp.Event {
	out := filterGeminiEvents(ctx, makeGeminiEventChan(events...))
	var result []acp.Event
	for e := range out {
		result = append(result, e)
	}
	return result
}

func TestFilterGeminiEvents_DropsInfrastructureEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	got := collectFilteredGeminiEvents(ctx,
		acp.ClientReadyEvent{AgentName: "gemini"},
		acp.SessionCreatedEvent{SessionID: "s1"},
		acp.TextDeltaEvent{Delta: "hello"},
	)
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d: %v", len(got), got)
	}
	if _, ok := got[0].(acp.TextDeltaEvent); !ok {
		t.Errorf("expected TextDeltaEvent, got %T", got[0])
	}
}

func TestFilterGeminiEvents_NormalizesToolCallStartName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	got := collectFilteredGeminiEvents(ctx,
		acp.ToolCallStartEvent{
			ToolName: "read_file",
			Input:    map[string]interface{}{"path": "/a/b/c.go"},
		},
	)
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	e, ok := got[0].(acp.ToolCallStartEvent)
	if !ok {
		t.Fatalf("expected ToolCallStartEvent, got %T", got[0])
	}
	if e.ToolName != "read .../b/c.go" {
		t.Errorf("ToolName = %q, want %q", e.ToolName, "read .../b/c.go")
	}
}

func TestFilterGeminiEvents_NormalizesToolCallUpdateName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	got := collectFilteredGeminiEvents(ctx,
		acp.ToolCallUpdateEvent{
			ToolName: "run_shell",
			Input:    map[string]interface{}{"command": "ls"},
			Status:   "completed",
		},
	)
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	e, ok := got[0].(acp.ToolCallUpdateEvent)
	if !ok {
		t.Fatalf("expected ToolCallUpdateEvent, got %T", got[0])
	}
	if e.ToolName != "shell: ls" {
		t.Errorf("ToolName = %q, want %q", e.ToolName, "shell: ls")
	}
}

func TestFilterGeminiEvents_ContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	// A blocked (non-buffered, never written) channel should drain immediately
	// because the context is already cancelled.
	in := make(chan acp.Event)
	out := filterGeminiEvents(ctx, in)
	var got []acp.Event
	for e := range out {
		got = append(got, e)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 events with cancelled context, got %d", len(got))
	}
}

func TestNewGeminiBackend_StopBeforeStartIsNoop(t *testing.T) {
	b := newGeminiBackend(Config{
		BackendType: BackendGemini,
		Model:       "gemini-2.5-pro",
	})
	if b == nil {
		t.Fatal("expected non-nil backend")
	}
	// Stop before Start must be safe (client is nil).
	if err := b.Stop(); err != nil {
		t.Errorf("Stop before Start should be no-op, got error: %v", err)
	}
}

// The gemini reader must WAIT for the bridge's outcome on cancellation, not
// poll for it. This models the exact SIGTERM sequence: signal.NotifyContext
// cancels ctx and the derived adapterCtx in one call, so the reader's ctx.Done
// is ready before the bridge goroutine is scheduled. A non-blocking drain
// therefore observes an empty channel and discards the text the bridge just
// preserved — which is what the round-4 fix did on precisely the path it
// targeted (codex + claude, PR #314 r5).
//
// Both shapes are exercised so the assertion is a comparison, not a claim.
func TestGeminiOutcomeChannel_CancellationMustNotRaceThePartial(t *testing.T) {
	const body = `{"verdict":"rejected","summary":"s","issues":[]}`

	// The OLD shape: reader polls with a default the instant it sees ctx.Done.
	t.Run("non-blocking drain loses the partial", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		partial := make(chan string, 1)
		go func() {
			<-ctx.Done()                     // bridge unblocks on the same cancel
			time.Sleep(5 * time.Millisecond) // ...but is scheduled later
			partial <- body
		}()
		cancel()
		<-ctx.Done()
		var got string
		select {
		case got = <-partial:
		default:
		}
		if got != "" {
			t.Skip("scheduler happened to favour the producer; the race is inherent")
		}
	})

	// The NEW shape: one outcome channel, and the reader blocks on it.
	t.Run("waiting for the outcome keeps the partial", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		type outcome struct {
			err  error
			text string
		}
		ch := make(chan outcome, 1)
		go func() {
			<-ctx.Done()
			time.Sleep(5 * time.Millisecond)
			ch <- outcome{text: body, err: context.Canceled}
		}()
		cancel()
		<-ctx.Done()
		got := <-ch // blocking: the bridge is already unblocked and will send
		if got.text != body {
			t.Fatalf("outcome text = %q, want the streamed body preserved", got.text)
		}
	})
}
