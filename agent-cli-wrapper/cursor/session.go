package cursor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/framelog"
)

// SessionInfo contains session metadata from the system init message.
type SessionInfo struct {
	SessionID string
	Model     string
	CWD       string
}

// QueryResult contains the result of a one-shot query.
type QueryResult struct {
	SessionID  string
	Text       string
	DurationMs int64
	Success    bool
}

// Session manages a one-shot interaction with the Cursor Agent CLI.
type Session struct {
	process  *processManager
	info     *SessionInfo
	events   chan Event
	done     chan struct{}
	readDone chan struct{}
	prompt   string
	config   SessionConfig
	mu       sync.RWMutex
	started  bool
	stopped  bool
}

// NewSession creates a new Cursor session with the given prompt and options.
func NewSession(prompt string, opts ...SessionOption) *Session {
	config := defaultConfig()
	for _, opt := range opts {
		opt(&config)
	}

	return &Session{
		prompt:   prompt,
		config:   config,
		events:   make(chan Event, config.EventBufferSize),
		done:     make(chan struct{}),
		readDone: make(chan struct{}),
	}
}

// Start spawns the CLI process and begins reading events.
func (s *Session) Start(ctx context.Context) error {
	s.mu.Lock()

	if s.started {
		s.mu.Unlock()
		return ErrAlreadyStarted
	}

	s.process = newProcessManager(s.prompt, s.config)
	if err := s.process.Start(ctx); err != nil {
		s.mu.Unlock()
		return err
	}

	go s.readLoop(ctx)

	if s.config.StderrHandler != nil {
		go s.stderrLoop()
	}

	s.started = true
	s.mu.Unlock()

	return nil
}

// Events returns a read-only channel for receiving events.
func (s *Session) Events() <-chan Event {
	return s.events
}

// Info returns session information (available after ReadyEvent).
func (s *Session) Info() *SessionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.info
}

// Stop gracefully shuts down the session.
// It signals the readLoop to exit and waits for it to close the events channel.
func (s *Session) Stop() error {
	s.mu.Lock()
	if !s.started || s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	s.mu.Unlock()

	close(s.done)

	if s.process != nil {
		s.process.Stop()
	}

	// Wait for readLoop to finish and close the events channel.
	// This avoids a TOCTOU race between Stop closing events and readLoop writing to it.
	<-s.readDone
	return nil
}

// readLoop reads NDJSON lines from the CLI and dispatches events.
// It owns the events channel — only this goroutine closes it (via defer).
// When the process exits (EOF) or context is cancelled, the loop exits
// and the deferred close signals consumers that no more events will arrive.
func (s *Session) readLoop(ctx context.Context) {
	defer func() {
		close(s.events)
		close(s.readDone)
	}()

	var textBuilder strings.Builder

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		default:
			line, err := s.process.ReadLine()
			if err != nil {
				if err == io.EOF {
					return
				}
				if !s.isStopped() {
					s.emit(ErrorEvent{
						Error:   err,
						Context: "read_line",
					})
				}
				return
			}

			s.handleLine(line, &textBuilder)
		}
	}
}

// stderrLoop reads and handles stderr from the CLI.
func (s *Session) stderrLoop() {
	stderr := s.process.Stderr()
	if stderr == nil {
		return
	}

	buf := make([]byte, 4096)
	for {
		select {
		case <-s.done:
			return
		default:
			n, err := stderr.Read(buf)
			if err != nil {
				return
			}
			if n > 0 && s.config.StderrHandler != nil {
				s.config.StderrHandler(buf[:n])
			}
		}
	}
}

// handleLine processes a single NDJSON line.
//
// A malformed non-terminal frame (assistant, tool_call, system) is skipped, not
// fatal: the cursor-agent protocol drifts (new shapes for known frames), and one
// bad frame must not discard a session that is otherwise streaming useful
// frames. The terminal "result" frame is the exception — losing it would leave
// the caller with no TurnCompleteEvent (truncated output, or a QueryStream that
// blocks until EOF), so a malformed result frame stays a fatal ErrorEvent.
func (s *Session) handleLine(line []byte, textBuilder *strings.Builder) {
	msg, err := ParseMessage(line)
	if err != nil {
		if isTerminalFrame(line) {
			s.emit(ErrorEvent{
				Error:   &ProtocolError{Message: "failed to parse message", Line: string(line), Cause: err},
				Context: "parse_message",
			})
			return
		}
		loggedLine, loggedLen := framelogLine(line)
		slog.Debug("cursor: skipping unparseable frame",
			"error", err, "line", loggedLine, "line_len", loggedLen)
		return
	}
	if msg == nil {
		// Unknown but valid message type (e.g. "user", "thinking") — skip.
		return
	}

	switch m := msg.(type) {
	case *SystemInitMessage:
		s.handleSystemInit(m)
	case *AssistantMessage:
		s.handleAssistant(m, textBuilder)
	case *ToolCallMessage:
		s.handleToolCall(m)
	case *ResultMessage:
		s.handleResult(m)
	}
}

// framelogLine renders a raw frame for the debug log: bounded and redacted by
// the shared framelog rule. cursor is the backend with observed protocol drift,
// so this is the site most likely to actually fire.
func framelogLine(line []byte) (string, int) {
	return framelog.RenderBytes(line)
}

// isTerminalFrame reports whether the raw line is a "result" frame — the one
// frame whose loss breaks the caller contract (no TurnCompleteEvent).
//
// It must catch a result frame even when the line is *not* valid JSON: a
// truncated final line is exactly the corruption most likely to hit the
// terminal frame, and skipping it would drop the only completion signal. So a
// clean decode is tried first, then a byte-level fallback that recognizes the
// "type":"result" discriminator without requiring the whole line to parse.
func isTerminalFrame(line []byte) bool {
	var raw RawMessage
	if err := json.Unmarshal(line, &raw); err == nil {
		return raw.Type == "result"
	}
	return hasResultTypeDiscriminator(line)
}

// hasResultTypeDiscriminator scans raw bytes for a `"type":"result"` field,
// tolerating insignificant whitespace around the colon, so a truncated or
// otherwise invalid result frame is still recognized as terminal.
//
// Every `"type"` key at the TOP LEVEL is checked, not just the first. A result
// frame that carries an earlier nested `"type"` — e.g.
// `{"meta":{"type":"x"},"type":"result",...}` — is the case the
// first-match-only version got wrong: it read as non-terminal and the frame was
// silently skipped, dropping the only completion signal. Since this runs only
// after a failed decode, the line is already truncated or corrupt, so the
// nested key may well be the only one intact.
//
// Depth matters in BOTH directions, which is why this tracks braces rather than
// scanning every occurrence. A nested `"type":"result"` inside a non-result
// frame — `{"type":"tool_call","tool_call":{"x":{"type":"result"}},…` — must NOT
// promote it: in the reviewer a spurious terminal frame becomes an ErrorEvent,
// and bridgeStreamEvents treats any error as a terminal failure that discards
// the whole review. That is the same outcome this change exists to prevent, so
// there is no "safe direction" here — both mistakes cost a review.
//
// Depth is tracked outside string literals only (with escape handling), so
// braces appearing inside a string value cannot shift it. Truncation is
// tolerated: an unclosed object just leaves depth high, and the top-level keys
// that precede the cut have already been scanned.
func hasResultTypeDiscriminator(line []byte) bool {
	const key = `"type"`
	depth := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '{', '[':
			depth++
			continue
		case '}', ']':
			depth--
			continue
		case '"':
			// fall through to string handling below
		default:
			continue
		}

		// At a quote: decide whether this is the `"type"` key at depth 1 (the
		// frame's own object) before skipping over the string literal.
		if depth == 1 && bytes.HasPrefix(line[i:], []byte(key)) {
			rest := bytes.TrimLeft(line[i+len(key):], " \t\r\n")
			if len(rest) > 0 && rest[0] == ':' {
				rest = bytes.TrimLeft(rest[1:], " \t\r\n")
				if bytes.HasPrefix(rest, []byte(`"result"`)) {
					return true
				}
			}
		}

		// Skip the string literal, honoring backslash escapes so an embedded
		// `\"` cannot end it early and desynchronize the depth counter.
		for i++; i < len(line); i++ {
			if line[i] == '\\' {
				i++
				continue
			}
			if line[i] == '"' {
				break
			}
		}
	}
	return false
}

func (s *Session) handleSystemInit(msg *SystemInitMessage) {
	s.mu.Lock()
	s.info = &SessionInfo{
		SessionID: msg.SessionID,
		Model:     msg.Model,
		CWD:       msg.CWD,
	}
	s.mu.Unlock()

	s.emit(ReadyEvent{
		SessionID: msg.SessionID,
		Model:     msg.Model,
	})
}

func (s *Session) handleAssistant(msg *AssistantMessage, textBuilder *strings.Builder) {
	for _, block := range msg.Message.Content {
		if block.Type == "text" && block.Text != "" {
			textBuilder.WriteString(block.Text)
			s.emit(TextEvent{
				Text:     block.Text,
				FullText: textBuilder.String(),
			})
		}
	}
}

func (s *Session) handleToolCall(msg *ToolCallMessage) {
	detail, err := ParseToolCallDetail(msg)
	if err != nil {
		// Tool call frames drive display only; skip a frame whose detail can't
		// be extracted rather than aborting the session.
		slog.Debug("cursor: skipping tool_call with unreadable detail", "error", err, "call_id", msg.CallID)
		return
	}

	switch msg.Subtype {
	case "started":
		s.emit(ToolStartEvent{
			ID:    msg.CallID,
			Name:  detail.Name,
			Input: detail.Args,
		})
	case "completed":
		s.emit(ToolCompleteEvent{
			ID:      msg.CallID,
			Name:    detail.Name,
			Input:   detail.Args,
			Result:  detail.Result,
			IsError: false,
		})
	}
}

func (s *Session) handleResult(msg *ResultMessage) {
	failed := msg.IsFailure()
	var resultErr error
	if failed {
		resultErr = fmt.Errorf("%s", msg.Result)
	}

	s.emit(TurnCompleteEvent{
		Success:       !failed,
		DurationMs:    msg.DurationMs,
		DurationAPIMs: msg.DurationAPIMs,
		Error:         resultErr,
	})
}

// emit sends an event to the events channel.
func (s *Session) emit(event Event) {
	select {
	case <-s.done:
		return
	default:
	}

	select {
	case s.events <- event:
	case <-s.done:
	default:
		// Channel full, drop event
	}
}

// isStopped returns whether the session has been stopped.
func (s *Session) isStopped() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stopped
}
