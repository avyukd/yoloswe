package reviewer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// claudeArgs renders the CLI flags a set of session options produces. Asserting
// on the flags rather than on SessionConfig fields keeps these tests honest:
// an option that silently stopped reaching the command line would still set the
// struct field, and the review would run with the wrong tool surface.
func claudeArgs(t *testing.T, opts ...claude.SessionOption) []string {
	t.Helper()
	args, err := claude.NewSession(opts...).CLIArgs()
	if err != nil {
		t.Fatalf("CLIArgs() error: %v", err)
	}
	return args
}

// hasFlagValue reports whether args contains flag immediately followed by value.
func hasFlagValue(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestClaudeBackend_StartStopAreNoops(t *testing.T) {
	t.Parallel()
	b := newClaudeBackend(Config{BackendType: BackendClaude, Model: DefaultClaudeModel})
	if err := b.Start(context.Background()); err != nil {
		t.Errorf("Start should be a no-op, got error: %v", err)
	}
	// Stop must be safe both after Start and without one — QueryStream owns
	// the process lifecycle, so there is never anything for Stop to tear down.
	if err := b.Stop(); err != nil {
		t.Errorf("Stop should be a no-op, got error: %v", err)
	}
	if err := newClaudeBackend(Config{}).Stop(); err != nil {
		t.Errorf("Stop before Start should be a no-op, got error: %v", err)
	}
}

// TestClaudeBaseSessionOptions_ReadOnlyWithholdsWriteTools is the load-bearing
// read-only assertion: Config.ReadOnly must translate into the write tools not
// being granted. Claude has no approval handler to fall back on, so if this
// regresses a "read-only" review can silently edit the worktree under review.
func TestClaudeBaseSessionOptions_ReadOnlyWithholdsWriteTools(t *testing.T) {
	t.Parallel()
	b := newClaudeBackend(Config{BackendType: BackendClaude, ReadOnly: true})
	args := claudeArgs(t, b.baseSessionOptions()...)
	for _, tool := range claudeReadOnlyDisallowedTools {
		if !hasFlagValue(args, "--disallowed-tools", tool) {
			t.Errorf("read-only session must disallow %q; args: %v", tool, args)
		}
	}
	// Bash stays available: the reviewer needs git log/diff/show to read the
	// change under review.
	if hasFlagValue(args, "--disallowed-tools", "Bash") {
		t.Errorf("read-only session must not disallow Bash; args: %v", args)
	}
}

func TestClaudeBaseSessionOptions_WritableGrantsWriteTools(t *testing.T) {
	t.Parallel()
	b := newClaudeBackend(Config{BackendType: BackendClaude, ReadOnly: false})
	args := claudeArgs(t, b.baseSessionOptions()...)
	for _, tool := range claudeReadOnlyDisallowedTools {
		if hasFlagValue(args, "--disallowed-tools", tool) {
			t.Errorf("ReadOnly=false must not disallow %q; args: %v", tool, args)
		}
	}
}

// TestClaudeBaseSessionOptions_AlwaysWithholdsReportFindings pins the output
// contract. ReportFindings is the CLI's own review channel; if the model can
// reach it, it routes findings there and replies in prose, and the envelope
// comes back "no JSON object found in response" — a whole review round lost.
// It must be withheld regardless of ReadOnly, since this is about the output
// channel, not write permission.
func TestClaudeBaseSessionOptions_AlwaysWithholdsReportFindings(t *testing.T) {
	t.Parallel()
	for _, readOnly := range []bool{true, false} {
		b := newClaudeBackend(Config{BackendType: BackendClaude, ReadOnly: readOnly})
		args := claudeArgs(t, b.baseSessionOptions()...)
		if !hasFlagValue(args, "--disallowed-tools", "ReportFindings") {
			t.Errorf("ReadOnly=%v: ReportFindings must always be withheld; args: %v", readOnly, args)
		}
	}
}

// TestClaudeBaseSessionOptions_PermissionModeBypass guards the automation
// contract: any mode that can raise an approval prompt hangs a non-interactive
// review until the idle timeout kills it.
func TestClaudeBaseSessionOptions_PermissionModeBypass(t *testing.T) {
	t.Parallel()
	b := newClaudeBackend(Config{BackendType: BackendClaude, ReadOnly: true})
	args := claudeArgs(t, b.baseSessionOptions()...)
	if !hasFlagValue(args, "--permission-mode", string(claude.PermissionModeBypass)) {
		t.Errorf("expected --permission-mode %s; args: %v", claude.PermissionModeBypass, args)
	}
}

func TestClaudeBaseSessionOptions_ModelAndWorkDir(t *testing.T) {
	t.Parallel()
	b := newClaudeBackend(Config{
		BackendType: BackendClaude,
		Model:       "sonnet",
		WorkDir:     t.TempDir(),
	})
	args := claudeArgs(t, b.baseSessionOptions()...)
	if !hasFlagValue(args, "--model", "sonnet") {
		t.Errorf("expected --model sonnet; args: %v", args)
	}
}

func TestClaudeBaseSessionOptions_EffortFlag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		effort   string
		want     string
		wantFlag bool
	}{
		{name: "empty omits flag", effort: ""},
		{name: "auto omits flag", effort: "auto"},
		{name: "unknown omits flag", effort: "ultra"},
		{name: "low", effort: "low", wantFlag: true, want: "low"},
		{name: "medium", effort: "medium", wantFlag: true, want: "medium"},
		{name: "high is case-insensitive", effort: "HIGH", wantFlag: true, want: "high"},
		{name: "max", effort: "max", wantFlag: true, want: "max"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := newClaudeBackend(Config{BackendType: BackendClaude, Effort: tt.effort})
			args := claudeArgs(t, b.baseSessionOptions()...)
			found := false
			for _, a := range args {
				if a == "--effort" {
					found = true
				}
			}
			if found != tt.wantFlag {
				t.Fatalf("effort %q: --effort present = %v, want %v; args: %v", tt.effort, found, tt.wantFlag, args)
			}
			if tt.wantFlag && !hasFlagValue(args, "--effort", tt.want) {
				t.Errorf("effort %q: expected --effort %s; args: %v", tt.effort, tt.want, args)
			}
		})
	}
}

func TestClaudeEffortLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		effort string
		want   claude.EffortLevel
		wantOK bool
	}{
		{effort: "", wantOK: false},
		{effort: "   ", wantOK: false},
		{effort: "auto", wantOK: false},
		{effort: "nonsense", wantOK: false},
		{effort: "low", want: claude.EffortLow, wantOK: true},
		{effort: "med", want: claude.EffortMed, wantOK: true},
		{effort: "medium", want: claude.EffortMed, wantOK: true},
		{effort: " High ", want: claude.EffortHigh, wantOK: true},
		{effort: "max", want: claude.EffortMax, wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.effort, func(t *testing.T) {
			t.Parallel()
			got, ok := claudeEffortLevel(tt.effort)
			if ok != tt.wantOK {
				t.Fatalf("claudeEffortLevel(%q) ok = %v, want %v", tt.effort, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("claudeEffortLevel(%q) = %q, want %q", tt.effort, got, tt.want)
			}
		})
	}
}

// claudeSessionRecorder records OnSessionInfo calls. The package's shared
// recordingHandler (bridge_test.go) deliberately drops them, and session info
// is exactly what this adapter exists to deliver.
type claudeSessionRecorder struct {
	recordingHandler
	sessionIDs []string
	models     []string
}

func (h *claudeSessionRecorder) OnSessionInfo(sessionID, model string) {
	h.sessionIDs = append(h.sessionIDs, sessionID)
	h.models = append(h.models, model)
}

func collectFilteredClaudeEvents(ctx context.Context, h EventHandler, onSession func(string), events ...claude.Event) []claude.Event {
	in := make(chan claude.Event, len(events))
	for _, e := range events {
		in <- e
	}
	close(in)
	adapter := &claudeEventAdapter{handler: h, events: in, onSession: onSession}
	var out []claude.Event
	for e := range adapter.filtered(ctx) {
		out = append(out, e)
	}
	return out
}

// TestClaudeEventAdapter_ReadyEventDeliversSessionInfo covers the whole reason
// the adapter exists: bridgeStreamEvents never dispatches KindReady, so without
// this interception session_id and model would be absent from every envelope
// and --resume-session-id would have nothing to resume from.
func TestClaudeEventAdapter_ReadyEventDeliversSessionInfo(t *testing.T) {
	t.Parallel()
	h := &claudeSessionRecorder{}
	var seen []string
	got := collectFilteredClaudeEvents(context.Background(), h, func(id string) { seen = append(seen, id) },
		claude.ReadyEvent{Info: claude.SessionInfo{SessionID: "sess-1", Model: "opus"}},
	)
	if len(got) != 0 {
		t.Errorf("ReadyEvent must not be forwarded to the bridge, got %d events: %v", len(got), got)
	}
	if len(h.sessionIDs) != 1 || h.sessionIDs[0] != "sess-1" {
		t.Errorf("session ids = %v, want [sess-1]", h.sessionIDs)
	}
	if len(h.models) != 1 || h.models[0] != "opus" {
		t.Errorf("models = %v, want [opus]", h.models)
	}
	if len(seen) != 1 || seen[0] != "sess-1" {
		t.Errorf("onSession ids = %v, want [sess-1]", seen)
	}
}

func TestClaudeEventAdapter_ForwardsNonReadyEvents(t *testing.T) {
	t.Parallel()
	got := collectFilteredClaudeEvents(context.Background(), &claudeSessionRecorder{}, nil,
		claude.ReadyEvent{Info: claude.SessionInfo{SessionID: "sess-1"}},
		claude.TextEvent{Text: "hello"},
		claude.ToolStartEvent{Name: "Read", ID: "t1"},
		claude.TurnCompleteEvent{Success: true, DurationMs: 42},
	)
	if len(got) != 3 {
		t.Fatalf("expected 3 forwarded events, got %d: %v", len(got), got)
	}
	if _, ok := got[0].(claude.TextEvent); !ok {
		t.Errorf("got[0] = %T, want claude.TextEvent", got[0])
	}
	if _, ok := got[1].(claude.ToolStartEvent); !ok {
		t.Errorf("got[1] = %T, want claude.ToolStartEvent", got[1])
	}
	if _, ok := got[2].(claude.TurnCompleteEvent); !ok {
		t.Errorf("got[2] = %T, want claude.TurnCompleteEvent", got[2])
	}
}

// TestClaudeEventAdapter_NilHandlerIsSafe covers the FollowUp/no-renderer path:
// the adapter must not panic when no handler was wired.
func TestClaudeEventAdapter_NilHandlerIsSafe(t *testing.T) {
	t.Parallel()
	got := collectFilteredClaudeEvents(context.Background(), nil, nil,
		claude.ReadyEvent{Info: claude.SessionInfo{SessionID: "sess-1"}},
		claude.TextEvent{Text: "hi"},
	)
	if len(got) != 1 {
		t.Fatalf("expected 1 forwarded event, got %d", len(got))
	}
}

// TestClaudeEventAdapter_ContextCancellation guards against a goroutine leak
// when the bridge returns early (e.g. on an idle timeout) and stops reading.
func TestClaudeEventAdapter_ContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	// An unbuffered channel nobody writes to would block forever; the adapter
	// must fall out on the cancelled context instead.
	in := make(chan claude.Event)
	adapter := &claudeEventAdapter{events: in}
	var got []claude.Event
	for e := range adapter.filtered(ctx) {
		got = append(got, e)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 events with cancelled context, got %d", len(got))
	}
}

// TestIsClaudeResumeUnavailable covers the fallback trigger. The live-observed
// shape (2026-08-06) is the load-bearing case: the CLI prints the resume miss
// to stderr and exits during startup, so the Go error the caller sees is only
// the downstream handshake timeout. Classifying on the error alone never fires
// the fallback and every stale resume id costs a review round.
func TestIsClaudeResumeUnavailable(t *testing.T) {
	t.Parallel()
	handshakeTimeout := errors.New("claude query failed: SDK initialize handshake failed: control request timed out")
	tests := []struct {
		name       string
		err        error
		stderrTail string
		want       bool
	}{
		{
			name:       "live shape: miss on stderr, timeout in error",
			err:        handshakeTimeout,
			stderrTail: "[claude stderr] No conversation found with session ID: 00000000-0000-4000-8000-000000000000\n",
			want:       true,
		},
		{
			name: "miss surfaced in the error itself",
			err:  errors.New("claude: no conversation found with session ID: abc"),
			want: true,
		},
		{
			// Same downstream symptom, no resume-miss evidence: a genuinely
			// wedged CLI must fail loudly rather than silently re-running
			// the whole review on a fresh session.
			name:       "handshake timeout with unrelated stderr is not a resume miss",
			err:        handshakeTimeout,
			stderrTail: "[claude stderr] Loaded cached credentials.\n",
			want:       false,
		},
		{
			name:       "no error means no fallback",
			err:        nil,
			stderrTail: "No conversation found with session ID: abc",
			want:       false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isClaudeResumeUnavailable(tt.err, tt.stderrTail); got != tt.want {
				t.Errorf("isClaudeResumeUnavailable() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestClaudeStderrTail_RetainsBoundedTail checks the buffer keeps the *end* of
// a noisy stream — the resume-miss line is printed immediately before exit, so
// dropping the tail (rather than the head) would lose exactly the evidence
// classification depends on.
func TestClaudeStderrTail_RetainsBoundedTail(t *testing.T) {
	t.Parallel()
	tail := newClaudeStderrTail()
	tail.forward = nil // don't spam the test log
	tail.handle([]byte(strings.Repeat("x", claudeStderrTailLimit*2)))
	tail.handle([]byte("No conversation found with session ID: abc"))

	got := tail.String()
	if len(got) > claudeStderrTailLimit {
		t.Errorf("tail len = %d, want <= %d", len(got), claudeStderrTailLimit)
	}
	if !strings.Contains(got, "No conversation found") {
		t.Error("tail must retain the most recent output")
	}
}

// TestClaudeStderrTail_ForwardsToOperator guards that adding capture didn't
// silently swallow the backend's stderr — operators and the per-round
// -stderr.txt files depend on it.
func TestClaudeStderrTail_ForwardsToOperator(t *testing.T) {
	t.Parallel()
	var forwarded []string
	tail := newClaudeStderrTail()
	tail.forward = func(b []byte) { forwarded = append(forwarded, string(b)) }
	tail.handle([]byte("boom"))
	if len(forwarded) != 1 || forwarded[0] != "boom" {
		t.Errorf("forwarded = %v, want [boom]", forwarded)
	}
	if tail.String() != "boom" {
		t.Errorf("String() = %q, want boom", tail.String())
	}
}

// stubClaudeQueryStream replaces the claudeQueryStream seam for one test,
// recording the flags each attempt was launched with and replaying a scripted
// outcome per attempt. Restores the real seam on cleanup.
//
// attempts[i] drives attempt i: when stderr is non-empty it is pushed through
// the session's stderr handler (that is how the CLI reports a resume miss);
// when err is non-nil the attempt fails; otherwise the events are delivered.
type claudeAttempt struct {
	err    error
	stderr string
	events []claude.Event
}

func stubClaudeQueryStream(t *testing.T, attempts ...claudeAttempt) *[][]string {
	t.Helper()
	var argsPerAttempt [][]string
	n := 0
	orig := claudeQueryStream
	t.Cleanup(func() { claudeQueryStream = orig })
	claudeQueryStream = func(_ context.Context, _ string, opts ...claude.SessionOption) (<-chan claude.Event, error) {
		args, err := claude.NewSession(opts...).CLIArgs()
		if err != nil {
			return nil, err
		}
		argsPerAttempt = append(argsPerAttempt, args)

		cfg := claude.SessionConfig{}
		for _, o := range opts {
			o(&cfg)
		}
		var a claudeAttempt
		if n < len(attempts) {
			a = attempts[n]
		}
		n++
		if a.stderr != "" && cfg.StderrHandler != nil {
			cfg.StderrHandler([]byte(a.stderr))
		}
		if a.err != nil {
			return nil, a.err
		}
		ch := make(chan claude.Event, len(a.events))
		for _, e := range a.events {
			ch <- e
		}
		close(ch)
		return ch, nil
	}
	return &argsPerAttempt
}

// TestClaudeBackend_ResumeFallbackLadder drives the real RunPrompt through the
// live failure shape: attempt 1 asks to resume, the CLI reports the miss on
// stderr and dies with an unrelated-looking handshake timeout, and attempt 2
// must run fresh and report ResumeStatusFallback.
//
// The prior version of this test built the option slice itself and asserted it
// rendered --resume — true by construction, and equally true if RunPrompt had
// stopped appending WithResume altogether. This one calls RunPrompt.
func TestClaudeBackend_ResumeFallbackLadder(t *testing.T) {
	args := stubClaudeQueryStream(t,
		claudeAttempt{
			err:    errors.New("SDK initialize handshake failed: control request timed out"),
			stderr: "No conversation found with session ID: sess-prior\n",
		},
		claudeAttempt{events: []claude.Event{
			claude.ReadyEvent{Info: claude.SessionInfo{SessionID: "sess-fresh", Model: "opus"}},
			claude.TextEvent{Text: `{"verdict":"accepted"}`},
			claude.TurnCompleteEvent{Success: true, DurationMs: 7, Usage: claude.TurnUsage{
				InputTokens: 10, CacheReadTokens: 90, OutputTokens: 5,
			}},
		}},
	)

	b := newClaudeBackend(Config{BackendType: BackendClaude, ResumeSessionID: "sess-prior"})
	result, err := b.RunPrompt(context.Background(), "review", nil)
	if err != nil {
		t.Fatalf("expected fallback to succeed, got error: %v", err)
	}
	if result.ResumeStatus != ResumeStatusFallback {
		t.Errorf("resume status = %q, want %q", result.ResumeStatus, ResumeStatusFallback)
	}
	if result.ResponseText != `{"verdict":"accepted"}` {
		t.Errorf("response text = %q, want the second attempt's output", result.ResponseText)
	}
	if len(*args) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(*args))
	}
	if !hasFlagValue((*args)[0], "--resume", "sess-prior") {
		t.Errorf("attempt 1 must carry --resume sess-prior; args: %v", (*args)[0])
	}
	for _, a := range (*args)[1] {
		if a == "--resume" {
			t.Errorf("attempt 2 must not carry --resume; args: %v", (*args)[1])
		}
	}
	// Cache-inclusive: 10 fresh + 90 cache-read.
	if result.InputTokens != 100 {
		t.Errorf("input tokens = %d, want 100 (fresh + cache)", result.InputTokens)
	}
}

// TestClaudeBackend_ResumeSucceedsWithoutRetry pins the other half of the
// ladder: when the requested session is live, there must be exactly one
// attempt and the status must be ok — a backend that always retried would pass
// the fallback test above while silently doubling every resumed round.
func TestClaudeBackend_ResumeSucceedsWithoutRetry(t *testing.T) {
	args := stubClaudeQueryStream(t, claudeAttempt{events: []claude.Event{
		claude.ReadyEvent{Info: claude.SessionInfo{SessionID: "sess-prior", Model: "opus"}},
		claude.TurnCompleteEvent{Success: true},
	}})

	b := newClaudeBackend(Config{BackendType: BackendClaude, ResumeSessionID: "sess-prior"})
	result, err := b.RunPrompt(context.Background(), "review", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ResumeStatus != ResumeStatusOK {
		t.Errorf("resume status = %q, want %q", result.ResumeStatus, ResumeStatusOK)
	}
	if len(*args) != 1 {
		t.Errorf("a live resume must not retry; attempts = %d", len(*args))
	}
}

// TestClaudeBackend_GenuineFailureDoesNotRetry guards the inverse of the
// fallback: a wedged CLI with no resume-miss evidence must surface its error
// rather than silently re-running the whole review on a fresh session.
func TestClaudeBackend_GenuineFailureDoesNotRetry(t *testing.T) {
	args := stubClaudeQueryStream(t, claudeAttempt{
		err:    errors.New("SDK initialize handshake failed: control request timed out"),
		stderr: "Loaded cached credentials.\n",
	})

	b := newClaudeBackend(Config{BackendType: BackendClaude, ResumeSessionID: "sess-prior"})
	_, err := b.RunPrompt(context.Background(), "review", nil)
	if err == nil {
		t.Fatal("expected the genuine failure to surface")
	}
	if len(*args) != 1 {
		t.Errorf("must not retry without resume-miss evidence; attempts = %d", len(*args))
	}
}

func TestClaudeUsageTokens_IncludesCacheTokens(t *testing.T) {
	t.Parallel()
	in, out := claudeUsageTokens(claude.TurnUsage{
		InputTokens:         7,
		CacheCreationTokens: 300,
		CacheReadTokens:     1200,
		OutputTokens:        42,
	})
	// Reporting only the 7 fresh tokens is the regression this guards: on a
	// resumed turn nearly the whole context is cache-served, so claude would
	// look ~200x cheaper than it is in the cross-backend comparison.
	if in != 1507 {
		t.Errorf("input = %d, want 1507 (7 + 300 + 1200)", in)
	}
	if out != 42 {
		t.Errorf("output = %d, want 42", out)
	}
}
