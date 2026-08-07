package framelog

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The frame's structure is what identifies protocol drift, so redaction must
// keep shape while dropping content. The array-shaped tool_call below is the
// exact wire shape behind the 102-failure cursor incident.
func TestRender_KeepsShapeDropsContent(t *testing.T) {
	line := `{"type":"tool_call","tool_call":[{"readToolCall":{"args":{"path":"/home/alice/.ssh/id_rsa"}}}]}`
	got, n := Render(line)

	assert.Equal(t, len(line), n, "length must report the untruncated frame")
	assert.NotContains(t, got, "/home/alice/.ssh/id_rsa", "a secret value must not reach the log")

	for _, key := range []string{"tool_call", "readToolCall", "args", "path"} {
		assert.Contains(t, got, key, "key %q identifies the drifted field and must survive", key)
	}
	assert.Contains(t, got, "[", "the array shape IS the bug being diagnosed")
}

// Discriminator values say which frame kind and which method broke — the most
// diagnostic bytes in the frame. Redacting them to "<str:9>" would defeat the
// purpose of logging the frame at all.
func TestRender_KeepsDiscriminatorValues(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{"type", `{"type":"tool_call","x":"secret"}`, `"type":"tool_call"`},
		{"subtype", `{"subtype":"started","x":"secret"}`, `"subtype":"started"`},
		{"method", `{"jsonrpc":"2.0","method":"item/started"}`, `"method":"item/started"`},
		{"jsonrpc", `{"jsonrpc":"2.0","x":"secret"}`, `"jsonrpc":"2.0"`},
		{"role", `{"role":"assistant","text":"secret"}`, `"role":"assistant"`},
		{"status", `{"status":"ok","detail":"secret"}`, `"status":"ok"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := Render(tt.line)
			assert.Contains(t, got, tt.want)
			assert.NotContains(t, got, "secret", "non-discriminator values must still be redacted")
		})
	}
}

// A truncated trailing string is the corruption most likely to hit a frame, and
// it must not leak the partial value.
func TestRender_RedactsTruncatedTrailingString(t *testing.T) {
	got, _ := Render(`{"content":"SECRET_TOKEN=abcdef`)
	assert.NotContains(t, got, "SECRET_TOKEN")
	assert.Contains(t, got, "truncated")
}

// A frame may reach the 10MB NDJSON cap; the log keeps a bounded head and still
// reports the true length so truncation is never read as a short frame.
func TestRender_Bounds(t *testing.T) {
	long := `{"a":"` + strings.Repeat("x", MaxLen*3) + `"}`
	got, n := Render(long)

	assert.LessOrEqual(t, len(got), MaxLen+len("...[truncated]"))
	assert.Equal(t, len(long), n, "length must be the untruncated frame length")

	short := `{"type":"result"}`
	got2, n2 := Render(short)
	assert.Equal(t, short, got2, "a short frame passes through unchanged")
	assert.Equal(t, len(short), n2)
}

// Keys are kept verbatim so the drifted field is nameable — but an object keyed
// by user data (a map keyed by file path) must not smuggle content through the
// key side.
func TestRender_RedactsImplausiblyLongKeys(t *testing.T) {
	longKey := "/home/alice/secrets/" + strings.Repeat("deep/", 20) + "id_rsa"
	require.Greater(t, len(longKey), maxKeyLen)

	got, _ := Render(`{"` + longKey + `":1}`)
	assert.NotContains(t, got, "id_rsa", "a user-data key must not reach the log verbatim")
	assert.Contains(t, got, "<key:")
}

// Escapes must not desynchronize the scanner: an embedded \" cannot end a
// string early and turn following content into keys.
func TestRender_HandlesEscapes(t *testing.T) {
	got, _ := Render(`{"note":"he said \"secret\" loudly","type":"result"}`)
	assert.NotContains(t, got, "secret")
	assert.Contains(t, got, `"type":"result"`, "the discriminator after an escaped string must still be found")
}

// Non-string literals carry no user content and keep the frame readable.
func TestRender_KeepsNonStringLiterals(t *testing.T) {
	got, _ := Render(`{"type":"result","duration_ms":1234,"is_error":false,"x":null}`)
	assert.Contains(t, got, "1234")
	assert.Contains(t, got, "false")
	assert.Contains(t, got, "null")
}
