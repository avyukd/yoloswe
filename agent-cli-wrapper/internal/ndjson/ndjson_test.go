package ndjson

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReaderReadLine(t *testing.T) {
	t.Parallel()

	reader := NewReader(strings.NewReader("first\nsecond\n"))

	first, err := reader.ReadLine()
	if err != nil {
		t.Fatalf("ReadLine() first error = %v", err)
	}
	second, err := reader.ReadLine()
	if err != nil {
		t.Fatalf("ReadLine() second error = %v", err)
	}

	if string(first) != "first" {
		t.Fatalf("first line = %q, want %q", first, "first")
	}
	if string(second) != "second" {
		t.Fatalf("second line = %q, want %q", second, "second")
	}

	first[0] = 'F'
	if string(second) != "second" {
		t.Fatalf("mutating first line changed second line to %q", second)
	}

	_, err = reader.ReadLine()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("ReadLine() final error = %v, want io.EOF", err)
	}
}

func TestReaderAcceptsLargeLine(t *testing.T) {
	t.Parallel()

	want := strings.Repeat("x", 70*1024)
	reader := NewReader(strings.NewReader(want + "\n"))

	got, err := reader.ReadLine()
	if err != nil {
		t.Fatalf("ReadLine() error = %v", err)
	}
	if string(got) != want {
		t.Fatalf("ReadLine() length = %d, want %d", len(got), len(want))
	}
}

func TestWriterWrite(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	writer := NewWriter(&buf)

	err := writer.Write(struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}{
		Name:  "build",
		Count: 2,
	})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	want := "{\"name\":\"build\",\"count\":2}\n"
	if got := buf.String(); got != want {
		t.Fatalf("Write() output = %q, want %q", got, want)
	}
}

func TestWriterWriteRaw(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	writer := NewWriter(&buf)

	if err := writer.WriteRaw([]byte(`{"raw":true}`)); err != nil {
		t.Fatalf("WriteRaw() error = %v", err)
	}

	want := "{\"raw\":true}\n"
	if got := buf.String(); got != want {
		t.Fatalf("WriteRaw() output = %q, want %q", got, want)
	}
}

func TestWriterUsesSingleWritePerLine(t *testing.T) {
	t.Parallel()

	writer := NewWriter(&countingWriter{})
	if err := writer.WriteRaw([]byte(`{"raw":true}`)); err != nil {
		t.Fatalf("WriteRaw() error = %v", err)
	}

	cw := writer.w.(*countingWriter)
	if cw.writes != 1 {
		t.Fatalf("write count = %d, want 1", cw.writes)
	}
	if got, want := cw.buf.String(), "{\"raw\":true}\n"; got != want {
		t.Fatalf("WriteRaw() output = %q, want %q", got, want)
	}
}

func TestWriterErrors(t *testing.T) {
	t.Parallel()

	t.Run("marshal", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		err := NewWriter(&buf).Write(func() {})
		if err == nil {
			t.Fatal("Write() error = nil, want marshal error")
		}
		if buf.Len() != 0 {
			t.Fatalf("buffer length after marshal error = %d, want 0", buf.Len())
		}
	})

	t.Run("write", func(t *testing.T) {
		t.Parallel()

		wantErr := errors.New("write failed")
		err := NewWriter(errorWriter{err: wantErr}).WriteRaw([]byte("line"))
		if !errors.Is(err, wantErr) {
			t.Fatalf("WriteRaw() error = %v, want %v", err, wantErr)
		}
	})
}

// TestReaderOversizedLineSurfacesError is the regression test for the fault
// that killed Cursor reviews mid-stream: a line over the scan limit must be
// reported as an error, never swallowed into a clean io.EOF that reads as
// "the reviewer finished with no findings".
func TestReaderOversizedLineSurfacesError(t *testing.T) {
	t.Parallel()

	oversized := `{"data":"` + strings.Repeat("x", maxTokenSize) + `"}`
	reader := NewReader(strings.NewReader(oversized + "\n" + `{"ok":1}` + "\n"))

	_, err := reader.ReadLine()
	if err == nil {
		t.Fatal("ReadLine() over-limit line error = nil, want error")
	}
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("ReadLine() error = %v, want errors.Is(err, ErrLineTooLong)", err)
	}
	if errors.Is(err, io.EOF) {
		t.Fatalf("ReadLine() error = %v, must not be reported as io.EOF", err)
	}
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("ReadLine() error = %v, want the bufio cause preserved", err)
	}

	// The failure latches: a caller that keeps reading must not see the
	// truncated stream turn into a normal end-of-stream on the next call.
	_, err = reader.ReadLine()
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("ReadLine() after overflow = %v, want ErrLineTooLong again", err)
	}
	if errors.Is(err, io.EOF) {
		t.Fatalf("ReadLine() after overflow = %v, must not degrade to io.EOF", err)
	}
}

// TestReaderLargeLineWithinLimit pins the headroom the raised cap buys: a
// payload far past the old 1MB limit still reads cleanly.
func TestReaderLargeLineWithinLimit(t *testing.T) {
	t.Parallel()

	payload := strings.Repeat("y", 4*1024*1024)
	reader := NewReader(strings.NewReader(`{"data":"` + payload + `"}` + "\n"))

	line, err := reader.ReadLine()
	if err != nil {
		t.Fatalf("ReadLine() 4MB line error = %v, want nil", err)
	}
	if want := len(payload) + len(`{"data":""}`); len(line) != want {
		t.Fatalf("ReadLine() length = %d, want %d", len(line), want)
	}

	if _, err := reader.ReadLine(); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadLine() final error = %v, want io.EOF", err)
	}
}

type errorWriter struct {
	err error
}

func (w errorWriter) Write([]byte) (int, error) {
	return 0, w.err
}

type countingWriter struct {
	buf    bytes.Buffer
	writes int
}

func (w *countingWriter) Write(data []byte) (int, error) {
	w.writes++
	return w.buf.Write(data)
}
