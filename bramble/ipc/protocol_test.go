package ipc

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSessionSummaryEncodesTmuxTarget(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(SessionSummary{ID: "sess-1", TmuxTarget: "@42"})
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Equal(t, "@42", decoded["tmux_target"])
}

func TestSessionSummaryOmitsAnEmptyTmuxTarget(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(SessionSummary{ID: "sess-1"})
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	_, present := decoded["tmux_target"]
	require.False(t, present, "an empty tmux target must be omitted, not serialized as \"\"")
}
