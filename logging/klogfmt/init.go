package klogfmt

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

type options struct {
	level  slog.Leveler
	writer io.Writer // extra writer for multi-writer setup
}

// Option configures a Handler.
type Option func(*options)

// WithLevel sets the minimum log level.
func WithLevel(l slog.Leveler) Option {
	return func(o *options) { o.level = l }
}

// WithWriter adds an extra writer that receives log output alongside stderr.
func WithWriter(w io.Writer) Option {
	return func(o *options) { o.writer = w }
}

// Init sets slog.SetDefault with a klogfmt handler writing to stderr
// (and any extra writer added via WithWriter).
func Init(opts ...Option) {
	o := options{}
	for _, opt := range opts {
		opt(&o)
	}
	var w io.Writer = os.Stderr
	if o.writer != nil {
		w = io.MultiWriter(os.Stderr, o.writer)
	}
	var handlerOpts []Option
	if o.level != nil {
		handlerOpts = append(handlerOpts, WithLevel(o.level))
	}
	slog.SetDefault(slog.New(New(w, handlerOpts...)))
}

// InitWithLogFile sets up dual-write: stderr + a log file at logPath.
// It creates parent directories as needed. Returns a closer for the log file.
func InitWithLogFile(logPath string, opts ...Option) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	Init(append(opts, WithWriter(f))...)
	return f.Close, nil
}

// InitToFile sends every record to the file at logPath and NOTHING to stderr.
// It creates parent directories as needed and returns a closer for the file.
//
// This is the member of the Init family for a process that does not own its
// terminal. A TUI painting inline is corrupted by a single stderr line landing
// mid-frame, so the writer must be the file alone — Init and InitWithLogFile
// both keep stderr in the writer set and cannot serve that caller.
//
// slog.SetDefault also redirects the standard library's log package into the
// default handler, so log.Printf call sites follow the file without changing.
func InitToFile(logPath string, opts ...Option) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	slog.SetDefault(slog.New(New(f, opts...)))
	// Sync before close, and tolerate a second call: this log is often the only
	// record of a run that ended badly, and its closer is wired to a defer that
	// a caller may also invoke explicitly.
	var once sync.Once
	return func() error {
		var err error
		once.Do(func() {
			_ = f.Sync()
			err = f.Close()
		})
		return err
	}, nil
}

// InitWithLogFileAndLevels sets up a dual-level logger: the file receives all
// records at fileLevel and above; stderr receives only records at stderrLevel
// and above. This lets INFO/DEBUG detail flow to the log file while keeping
// the terminal quiet except for errors.
//
// Returns a cleanup function that closes the log file, and any setup error.
func InitWithLogFileAndLevels(logPath string, fileLevel, stderrLevel slog.Leveler) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	fileH := New(f, WithLevel(fileLevel))
	stderrH := New(os.Stderr, WithLevel(stderrLevel))
	slog.SetDefault(slog.New(&teeHandler{fileH: fileH, stderrH: stderrH}))
	return f.Close, nil
}

// teeHandler fans slog records out to two handlers with independent level
// filters: typically a file handler at DEBUG and a stderr handler at ERROR.
type teeHandler struct {
	fileH   *Handler
	stderrH *Handler
}

func (t *teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return t.fileH.Enabled(ctx, level) || t.stderrH.Enabled(ctx, level)
}

func (t *teeHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	if t.fileH.Enabled(ctx, r.Level) {
		errs = append(errs, t.fileH.Handle(ctx, r))
	}
	if t.stderrH.Enabled(ctx, r.Level) {
		errs = append(errs, t.stderrH.Handle(ctx, r))
	}
	return errors.Join(errs...)
}

func (t *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	fileH := t.fileH.WithAttrs(attrs).(*Handler)
	stderrH := t.stderrH.WithAttrs(attrs).(*Handler)
	return &teeHandler{fileH: fileH, stderrH: stderrH}
}

func (t *teeHandler) WithGroup(name string) slog.Handler {
	fileH := t.fileH.WithGroup(name).(*Handler)
	stderrH := t.stderrH.WithGroup(name).(*Handler)
	return &teeHandler{fileH: fileH, stderrH: stderrH}
}
