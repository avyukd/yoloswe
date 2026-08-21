package agent

import (
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/cursor"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCursorProvider_Name(t *testing.T) {
	p := NewCursorProvider()
	assert.Equal(t, "cursor", p.Name())
}

func TestCursorProvider_EventsChannel(t *testing.T) {
	p := NewCursorProvider()
	ch := p.Events()
	require.NotNil(t, ch)
	// Should be the same channel each time
	assert.Equal(t, (<-chan AgentEvent)(p.events), ch)
}

func TestCursorProvider_Close(t *testing.T) {
	p := NewCursorProvider()
	err := p.Close()
	assert.NoError(t, err)

	// Events channel should be closed after Close()
	_, ok := <-p.Events()
	assert.False(t, ok, "events channel should be closed")
}

// Pin what actually reaches the CLI, not just the model rule in isolation:
// Execute forwarding cfg.Model straight to cursor.WithModel is the regression
// that killed cursor sessions, and a test of the rule alone still passes
// through it.
func TestCursorSessionOpts_Model(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  string
	}{
		{name: "unset", model: "", want: ""},
		{name: "claude default applyOptions fills in", model: "sonnet", want: ""},
		{name: "another claude alias", model: "opus", want: ""},
		{name: "bramble placeholder", model: "cursor-default", want: ""},
		{name: "another provider's model", model: "gpt-5.5", want: ""},
		{name: "real cursor model passes through", model: "composer-2.5", want: "composer-2.5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got cursor.SessionConfig
			for _, opt := range cursorSessionOpts(ExecuteConfig{Model: tt.model}) {
				opt(&got)
			}
			assert.Equal(t, tt.want, got.Model)
		})
	}
}

// Cursor refuses to run in a directory it has not been told to trust, and every
// caller here drives it non-interactively.
func TestCursorSessionOpts_AlwaysTrusts(t *testing.T) {
	var got cursor.SessionConfig
	for _, opt := range cursorSessionOpts(ExecuteConfig{}) {
		opt(&got)
	}
	assert.True(t, got.Trust)
}
