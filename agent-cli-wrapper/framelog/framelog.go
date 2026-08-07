// Package framelog renders a raw protocol frame for logging.
//
// A frame that failed to parse is the only record of what a backend actually
// sent, and it is worth logging: inferring the wire shape from a Go decode
// error is what made one cursor incident cost 102 reviews. But a frame is also
// untrusted, unbounded, and full of user content — the frames that fail to
// parse are overwhelmingly tool_call frames, carrying exactly the command,
// file_path and content values a reviewer's redaction policy exists to keep out
// of the log.
//
// This package is the single place that resolves those two facts, so the rule
// is stated once rather than re-derived per backend. It lives outside
// internal/ because the reviewer (a separate Go module) is one of its callers;
// an internal package would be importable by the SDKs alone, which is how the
// first version ended up with two constants, two truncation helpers, and
// redaction at exactly one of four sites.
package framelog

import (
	"fmt"
	"strings"
)

// MaxLen bounds a frame written to a log. A line may reach the 10MB NDJSON cap,
// and what identifies the drift — the discriminator and the offending field —
// sits near the start.
const MaxLen = 2048

// discriminatorKeys names the keys whose VALUES survive redaction verbatim.
//
// These are enum-like protocol tokens drawn from a small fixed vocabulary, not
// user content: they say which kind of frame this is and which method broke.
// Redacting them removes the single most diagnostic byte range in the frame —
// `"type":"tool_call"` is the whole finding in the cursor incident, and
// `"type":"<str:9>"` says nothing at all.
var discriminatorKeys = map[string]bool{
	"type":    true,
	"subtype": true,
	"method":  true,
	"jsonrpc": true,
	"role":    true,
	"event":   true,
	"status":  true,
}

// maxKeyLen bounds a key kept verbatim. Keys are normally short protocol
// identifiers, but a frame may be an object keyed by user data (a map keyed by
// file path), so an implausibly long key is redacted like a value.
const maxKeyLen = 64

// Render returns a bounded, redacted rendering of a raw frame, plus the
// frame's true byte length so truncation is never mistaken for a short frame.
//
// String VALUES become a `"<str:N>"` marker; keys and non-string literals are
// kept, so the frame's structure — the shape that identifies the drift —
// stays readable. Values under discriminatorKeys survive verbatim.
func Render(line string) (rendered string, length int) {
	redacted := redactValues(line)
	if len(redacted) > MaxLen {
		return redacted[:MaxLen] + "...[truncated]", len(line)
	}
	return redacted, len(line)
}

// RenderBytes is Render for a []byte frame.
func RenderBytes(line []byte) (string, int) {
	return Render(string(line))
}

// redactValues replaces every JSON string value with a length marker, keeping
// keys, punctuation, non-string literals, and discriminator values.
//
// It scans bytes rather than decoding, because by construction the frame did
// NOT decode — it may be truncated or malformed. A token is a key when the next
// non-space byte after its closing quote is ':'; anything else is a value. A
// trailing unterminated string (the truncation case) is redacted too, so a
// cut-off payload cannot leak.
func redactValues(line string) string {
	var b strings.Builder
	b.Grow(len(line))

	// pendingKey is the key most recently emitted, so a value can be checked
	// against discriminatorKeys.
	pendingKey := ""

	for i := 0; i < len(line); i++ {
		if line[i] != '"' {
			b.WriteByte(line[i])
			continue
		}

		j := i + 1
		terminated := false
		for ; j < len(line); j++ {
			if line[j] == '\\' {
				j++
				continue
			}
			if line[j] == '"' {
				terminated = true
				break
			}
		}
		if !terminated {
			fmt.Fprintf(&b, "\"<str:%d,truncated>", len(line)-(i+1))
			return b.String()
		}

		token := line[i+1 : j]

		k := j + 1
		for k < len(line) && (line[k] == ' ' || line[k] == '\t' || line[k] == '\r' || line[k] == '\n') {
			k++
		}
		isKey := k < len(line) && line[k] == ':'

		switch {
		case isKey && len(token) <= maxKeyLen:
			b.WriteString(line[i : j+1])
			pendingKey = token
		case isKey:
			// Implausibly long key — likely user data used as a map key.
			fmt.Fprintf(&b, "\"<key:%d>\"", len(token))
			pendingKey = ""
		case discriminatorKeys[pendingKey]:
			b.WriteString(line[i : j+1])
			pendingKey = ""
		default:
			fmt.Fprintf(&b, "\"<str:%d>\"", len(token))
			pendingKey = ""
		}
		i = j
	}
	return b.String()
}
