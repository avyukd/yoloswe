package ipc

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSessionSummaryEncodesTmuxTarget pins the wire name a caller greps for.
// The control socket already calls this field "tmux_target"
// (control.SessionSummary); the IPC surface uses the same key so a caller does
// not have to learn two names for the same tmux window address.
func TestSessionSummaryEncodesTmuxTarget(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(SessionSummary{ID: "sess-1", TmuxTarget: "@42"})
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Equal(t, "@42", decoded["tmux_target"])
}

// TestSessionSummaryOmitsAnEmptyTmuxTarget keeps "no window" distinguishable
// from "a window whose id is the empty string". A caller that reaps windows has
// to be able to tell the two apart; a key that is always present, sometimes
// empty, invites the caller to pass "" straight to tmux, where it means the
// current window.
func TestSessionSummaryOmitsAnEmptyTmuxTarget(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(SessionSummary{ID: "sess-1"})
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	_, present := decoded["tmux_target"]
	require.False(t, present, "an empty tmux target must be omitted, not serialized as \"\"")
}
