package agent

import (
	"testing"

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

func TestCursorModelArg(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  string
	}{
		{name: "unset", model: "", want: ""},
		{name: "claude default is not a cursor model", model: "sonnet", want: ""},
		{name: "bramble placeholder is not a cursor model", model: "cursor-default", want: ""},
		{name: "real cursor model passes through", model: "composer-2.5", want: "composer-2.5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, cursorModelArg(tt.model))
		})
	}
}
