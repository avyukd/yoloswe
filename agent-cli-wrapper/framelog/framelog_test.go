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

// A discriminator's value is only the scalar string immediately after its
// colon. When the value is an array or object, the strings inside are payload
// and must redact — pendingKey surviving a '[' leaked them verbatim.
func TestRender_DiscriminatorWithStructuredValueDoesNotLeak(t *testing.T) {
	for _, line := range []string{
		`{"type":["SECRET_TOKEN"]}`,
		`{"jsonrpc":["SECRET_TOKEN"]}`,
		`{"method":{"nested":"SECRET_TOKEN"}}`,
		`{"type":[{"deep":"SECRET_TOKEN"}]}`,
		`{"status":["SECRET_TOKEN"],"type":"result"}`,
	} {
		got, _ := Render(line)
		assert.NotContains(t, got, "SECRET_TOKEN",
			"a structured discriminator value must redact its contents: %s", line)
	}
}

// A discriminator following a structural token must still be recognised — the
// pendingKey reset must not over-clear.
func TestRender_DiscriminatorAfterStructuralTokenStillKept(t *testing.T) {
	got, _ := Render(`{"params":{"x":"secret"},"type":"result"}`)
	assert.Contains(t, got, `"type":"result"`)
	assert.NotContains(t, got, "secret")
}

// The SDK debug sites render on every skipped frame, and a frame may reach the
// 10MB NDJSON cap. Redacting the whole frame before bounding it would mean a
// full scan and a multi-MB allocation to produce 2KB of log.
func TestRender_BoundsBeforeRedacting(t *testing.T) {
	huge := `{"type":"result","blob":"` + strings.Repeat("x", 4*1024*1024) + `"}`

	got, n := Render(huge)
	assert.Equal(t, len(huge), n, "the true frame length must still be reported")
	assert.LessOrEqual(t, len(got), MaxLen+len("...[truncated]"))
	assert.Contains(t, got, "truncated")

	// Assert on BYTES allocated, not allocation COUNT: a redact-first
	// implementation allocates one big builder, which a count threshold waves
	// through. The bound must hold in bytes.
	assertBoundedAlloc(t, "Render", func() { _, _ = Render(huge) })
}

// assertBoundedAlloc fails if fn allocates on the order of its input rather
// than on the order of MaxLen. Uses AllocedBytesPerOp because the regression
// being guarded is one large allocation, which an allocation COUNT cannot see.
func assertBoundedAlloc(t *testing.T, name string, fn func()) {
	t.Helper()
	res := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			fn()
		}
	})
	const budget = 16 * MaxLen // generous headroom; still orders below a 4MB frame
	assert.Less(t, int(res.AllocedBytesPerOp()), budget,
		"%s must allocate on the order of MaxLen, not of the input frame (got %d bytes/op)",
		name, res.AllocedBytesPerOp())
}

// The SDK hot paths all enter through RenderBytes, so the bound must hold
// there. Converting []byte->string before bounding copied the whole frame:
// measured 4.2MB/op through RenderBytes against 2KB/op through Render, which
// meant round 3's fix reached only the reviewer's cold path.
func TestRenderBytes_BoundsBeforeConverting(t *testing.T) {
	huge := []byte(`{"type":"result","blob":"` + strings.Repeat("x", 4*1024*1024) + `"}`)

	got, n := RenderBytes(huge)
	assert.Equal(t, len(huge), n, "the true frame length must still be reported")
	assert.LessOrEqual(t, len(got), MaxLen+len("...[truncated]"))
	assert.Contains(t, got, "truncated")
	assert.Contains(t, got, `"type":"result"`, "the discriminator must survive bounding")

	assertBoundedAlloc(t, "RenderBytes", func() { _, _ = RenderBytes(huge) })
}

// A short frame through RenderBytes is unchanged and reports no truncation.
func TestRenderBytes_ShortFrameUnchanged(t *testing.T) {
	line := []byte(`{"type":"result","x":"secret"}`)
	got, n := RenderBytes(line)
	assert.Equal(t, len(line), n)
	assert.Contains(t, got, `"type":"result"`)
	assert.NotContains(t, got, "secret")
	assert.NotContains(t, got, "truncated")
}

// A cut mid-token is safe: it lands in the unterminated-string branch and is
// redacted rather than emitted.
func TestRender_SliceMidTokenStillRedacts(t *testing.T) {
	line := `{"type":"result","secret":"` + strings.Repeat("S", MaxLen*2)
	got, _ := Render(line)
	assert.NotContains(t, got, strings.Repeat("S", 100),
		"a value cut by the bound must not survive verbatim")
}
