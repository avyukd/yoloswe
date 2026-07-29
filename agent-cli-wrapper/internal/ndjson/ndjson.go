// Package ndjson provides utilities for reading and writing newline-delimited JSON.
package ndjson

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// maxTokenSize bounds a single NDJSON line. Agent CLIs emit whole tool results
// as one line, and a large file read or diff comfortably exceeds 1MB — the
// previous limit, which killed real Cursor review sessions mid-stream. 10MB
// matches the other scanners in this repo (bramble/sessionanalysis/stats.go,
// bramble/replay/claude.go).
//
// A cap is not a correctness guarantee: a big enough line still exceeds it. It
// bounds memory, and ErrLineTooLong makes the overflow explicit rather than
// letting the stream look like a clean EOF. Callers must treat that error as a
// truncated stream, never as "the agent finished with nothing to say".
const maxTokenSize = 10 * 1024 * 1024 // 10MB

// initialBufferSize is the starting scan buffer; bufio grows it toward
// maxTokenSize as needed, so this only affects small-line allocation.
const initialBufferSize = 64 * 1024

// ErrLineTooLong indicates a single NDJSON line exceeded maxTokenSize. The
// stream is truncated at that point and cannot be resynchronized: bufio.Scanner
// does not report where the oversized line ends, so every later line is
// unreliable too. Callers must surface this as a failure.
var ErrLineTooLong = errors.New("ndjson: line exceeds maximum size")

// Reader reads newline-delimited JSON from an io.Reader.
type Reader struct {
	scanner *bufio.Scanner
	// failed latches a terminal error so a caller that keeps reading after an
	// oversized line gets the same failure instead of a misleading io.EOF.
	// bufio.Scanner refuses to scan after an error, which without this latch
	// would render as a clean end-of-stream.
	failed error
}

// NewReader creates a new NDJSON reader.
func NewReader(r io.Reader) *Reader {
	scanner := bufio.NewScanner(r)
	// Set a larger buffer for potentially large JSON messages
	scanner.Buffer(make([]byte, initialBufferSize), maxTokenSize)
	return &Reader{scanner: scanner}
}

// ReadLine reads the next JSON line as raw bytes.
// Returns io.EOF when there are no more lines.
//
// An oversized line returns an error wrapping ErrLineTooLong, and every
// subsequent call returns it again — a truncated stream never degrades into
// io.EOF, which callers would read as normal completion.
func (r *Reader) ReadLine() ([]byte, error) {
	if r.failed != nil {
		return nil, r.failed
	}
	if r.scanner.Scan() {
		// Return a copy of the bytes since Scanner reuses the buffer
		line := r.scanner.Bytes()
		result := make([]byte, len(line))
		copy(result, line)
		return result, nil
	}
	if err := r.scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			// Translate bufio's opaque "token too long" into an error that
			// names the limit and is matchable with errors.Is.
			r.failed = fmt.Errorf("%w (%d bytes); stream truncated: %w",
				ErrLineTooLong, maxTokenSize, err)
			return nil, r.failed
		}
		r.failed = err
		return nil, err
	}
	return nil, io.EOF
}

// Writer writes newline-delimited JSON to an io.Writer.
type Writer struct {
	w  io.Writer
	mu sync.Mutex
}

// NewWriter creates a new NDJSON writer.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w}
}

// Write writes a value as a JSON line.
func (w *Writer) Write(v interface{}) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	data, err := json.Marshal(v)
	if err != nil {
		return err
	}

	return w.writeLineLocked(append(data, '\n'))
}

// WriteRaw writes raw bytes as a line.
func (w *Writer) WriteRaw(data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	line := make([]byte, 0, len(data)+1)
	line = append(line, data...)
	line = append(line, '\n')
	return w.writeLineLocked(line)
}

func (w *Writer) writeLineLocked(data []byte) error {
	_, err := w.w.Write(data)
	return err
}
