package reviewer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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

// TestClaudeBaseSessionOptions_DisablesPlugins pins reproducibility. The
// wrapper's defaults exclude user/project *settings* (--setting-sources "")
// but NOT plugins — that needs WithDisablePlugins, which emits
// --plugin-dir /dev/null. Without it the reviewer's tool surface depends on
// whichever plugins the operator has installed, so the same review on two
// machines can see different tools and the cross-backend eval that reads this
// backend's output stops being a like-for-like comparison.
func TestClaudeBaseSessionOptions_DisablesPlugins(t *testing.T) {
	t.Parallel()
	b := newClaudeBackend(Config{BackendType: BackendClaude, ReadOnly: true})
	args := claudeArgs(t, b.baseSessionOptions()...)
	if !hasFlagValue(args, "--plugin-dir", "/dev/null") {
		t.Errorf("expected --plugin-dir /dev/null (plugins are not excluded by default); args: %v", args)
	}
	// The settings half is the wrapper's default; assert both so a change to
	// either mechanism surfaces here rather than silently halving isolation.
	if !hasFlagValue(args, "--setting-sources", "") {
		t.Errorf("expected --setting-sources \"\"; args: %v", args)
	}
}

// TestClaudeReadOnly_CoversKnownMutatingTools ties the denylist to the repo's
// other enumeration of Claude's mutating surface (isMutatingTool in
// bramble/session/event_handler.go). The two previously disagreed on
// MultiEdit, which meant a "read-only" review might still have had a write
// path. Bash is the one intentional difference — the reviewer needs it.
func TestClaudeReadOnly_CoversKnownMutatingTools(t *testing.T) {
	t.Parallel()
	// Mirrors isMutatingTool minus Bash; keep in step with that switch.
	knownMutating := []string{"Edit", "Write", "MultiEdit", "NotebookEdit"}
	got := make(map[string]bool, len(claudeReadOnlyDisallowedTools))
	for _, tool := range claudeReadOnlyDisallowedTools {
		got[tool] = true
	}
	for _, tool := range knownMutating {
		if !got[tool] {
			t.Errorf("read-only denylist is missing known mutating tool %q", tool)
		}
	}
	if got["Bash"] {
		t.Error("Bash must stay available: the reviewer needs git log/diff/show")
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
	workDir := t.TempDir()
	b := newClaudeBackend(Config{
		BackendType: BackendClaude,
		Model:       "sonnet",
		WorkDir:     workDir,
	})
	opts := b.baseSessionOptions()
	args := claudeArgs(t, opts...)
	if !hasFlagValue(args, "--model", "sonnet") {
		t.Errorf("expected --model sonnet; args: %v", args)
	}
	// WorkDir is not a CLI flag — WithWorkDir sets SessionConfig.WorkDir,
	// which becomes cmd.Dir — so asserting on rendered args cannot see it.
	// Without this check the test would pass identically if the
	// claude.WithWorkDir branch in baseSessionOptions were deleted, and a
	// reviewer launched from the wrong directory reviews the wrong tree.
	cfg := claude.SessionConfig{}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.WorkDir != workDir {
		t.Errorf("WorkDir = %q; want %q", cfg.WorkDir, workDir)
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

// claudeStubCalls records what each attempt was launched with. ctxs holds the
// context handed to that attempt, so tests can assert whether it was cancelled
// — the difference between hard-killing the CLI and letting it stop gracefully.
type claudeStubCalls struct {
	args [][]string
	ctxs []context.Context
}

func stubClaudeQueryStream(t *testing.T, attempts ...claudeAttempt) *claudeStubCalls {
	t.Helper()
	calls := &claudeStubCalls{}
	n := 0
	orig := claudeQueryStream
	t.Cleanup(func() { claudeQueryStream = orig })
	claudeQueryStream = func(ctx context.Context, _ string, opts ...claude.SessionOption) (<-chan claude.Event, error) {
		args, err := claude.NewSession(opts...).CLIArgs()
		if err != nil {
			return nil, err
		}
		calls.args = append(calls.args, args)
		calls.ctxs = append(calls.ctxs, ctx)

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
	return calls
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
	calls := stubClaudeQueryStream(t,
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
	if len(calls.args) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(calls.args))
	}
	if !hasFlagValue(calls.args[0], "--resume", "sess-prior") {
		t.Errorf("attempt 1 must carry --resume sess-prior; args: %v", calls.args[0])
	}
	for _, a := range calls.args[1] {
		if a == "--resume" {
			t.Errorf("attempt 2 must not carry --resume; args: %v", calls.args[1])
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
	calls := stubClaudeQueryStream(t, claudeAttempt{events: []claude.Event{
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
	if len(calls.args) != 1 {
		t.Errorf("a live resume must not retry; attempts = %d", len(calls.args))
	}
}

// TestClaudeBackend_GenuineFailureDoesNotRetry guards the inverse of the
// fallback: a wedged CLI with no resume-miss evidence must surface its error
// rather than silently re-running the whole review on a fresh session.
func TestClaudeBackend_GenuineFailureDoesNotRetry(t *testing.T) {
	calls := stubClaudeQueryStream(t, claudeAttempt{
		err:    errors.New("SDK initialize handshake failed: control request timed out"),
		stderr: "Loaded cached credentials.\n",
	})

	b := newClaudeBackend(Config{BackendType: BackendClaude, ResumeSessionID: "sess-prior"})
	_, err := b.RunPrompt(context.Background(), "review", nil)
	if err == nil {
		t.Fatal("expected the genuine failure to surface")
	}
	if len(calls.args) != 1 {
		t.Errorf("must not retry without resume-miss evidence; attempts = %d", len(calls.args))
	}
}

// TestClaudeBackend_SuccessLetsCLIStopGracefully guards a regression this PR
// already made once: threading an attempt-scoped context into QueryStream is
// right for an abandoned attempt, but the wrapper spawns the CLI with
// exec.CommandContext and no cmd.Cancel override, so cancelling is a bare
// SIGKILL. Firing it on the success path killed the CLI mid-shutdown, while it
// was still flushing the very session transcript the next round resumes from.
//
// The stub's channel stays OPEN after the turn event, standing in for a CLI
// still inside its stdin-close → SIGTERM window. RunPrompt must not have
// cancelled the context by the time it returns its result.
func TestClaudeBackend_SuccessLetsCLIStopGracefully(t *testing.T) {
	ch := make(chan claude.Event, 2)
	ch <- claude.ReadyEvent{Info: claude.SessionInfo{SessionID: "sess-1", Model: "opus"}}
	ch <- claude.TurnCompleteEvent{Success: true}
	// Deliberately NOT closed: the CLI has not finished shutting down.

	// Shrink the grace period: this test only needs to observe that the wait
	// happens, and a real 2s would be dead time in every unit run.
	restore := claudeShutdownGrace
	claudeShutdownGrace = 50 * time.Millisecond
	t.Cleanup(func() { claudeShutdownGrace = restore })

	// Cancellation is expected eventually — the deferred cancel must still run
	// so the context isn't leaked. What matters is *when*: measure the delay
	// between launching the CLI and killing it. Checking ctx.Err() after
	// RunPrompt returns would always read "cancelled" and prove nothing.
	cancelled := make(chan time.Time, 1)
	orig := claudeQueryStream
	t.Cleanup(func() { claudeQueryStream = orig })
	claudeQueryStream = func(ctx context.Context, _ string, _ ...claude.SessionOption) (<-chan claude.Event, error) {
		go func() {
			<-ctx.Done()
			cancelled <- time.Now()
		}()
		return ch, nil
	}

	b := newClaudeBackend(Config{BackendType: BackendClaude})
	start := time.Now()
	if _, err := b.RunPrompt(context.Background(), "review", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case at := <-cancelled:
		// Before the fix this fired within microseconds of the turn event.
		if held := at.Sub(start); held < claudeShutdownGrace {
			t.Errorf("CLI was killed %s after launch, inside its %s shutdown window", held, claudeShutdownGrace)
		}
	case <-time.After(claudeShutdownGrace * 3):
		t.Error("context was never cancelled; the attempt context leaks")
	}
}

// TestWaitClaudeShutdown_ReturnsPromptlyOnClose is the other half: a CLI that
// finishes its graceful stop closes the stream, and we must not sit on the full
// grace period for every successful review.
func TestWaitClaudeShutdown_ReturnsPromptlyOnClose(t *testing.T) {
	t.Parallel()
	ch := make(chan claude.Event, 1)
	ch <- claude.TextEvent{Text: "trailing"}
	close(ch)

	start := time.Now()
	waitClaudeShutdown(ch)
	if elapsed := time.Since(start); elapsed >= claudeShutdownGrace {
		t.Errorf("waited %s on an already-closed stream; want prompt return", elapsed)
	}
}

// TestClaudeBackend_AbandonedAttemptCancelsCLI pins the original leak fix: the
// fallback path must tear the first attempt's CLI down, not leave two sessions
// running at once.
func TestClaudeBackend_AbandonedAttemptCancelsCLI(t *testing.T) {
	calls := stubClaudeQueryStream(t,
		claudeAttempt{
			err:    errors.New("SDK initialize handshake failed: control request timed out"),
			stderr: "No conversation found with session ID: sess-prior\n",
		},
		claudeAttempt{events: []claude.Event{
			claude.ReadyEvent{Info: claude.SessionInfo{SessionID: "sess-fresh"}},
			claude.TurnCompleteEvent{Success: true},
		}},
	)

	b := newClaudeBackend(Config{BackendType: BackendClaude, ResumeSessionID: "sess-prior"})
	if _, err := b.RunPrompt(context.Background(), "review", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(calls.ctxs) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(calls.ctxs))
	}
	if calls.ctxs[0].Err() == nil {
		t.Error("the abandoned first attempt's context must be cancelled so its CLI is torn down")
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

// The backend must carry streamed text into the ReviewResult when the bridge
// fails, not just when it succeeds — that is what turns an idle-timeout kill
// into status="partial" instead of a discarded review. Unit-testing
// reviewPartialResult alone leaves the call site free to regress: swapping it
// back to reviewErrorResult keeps every other test green.
func TestClaudeBackend_PartialTextSurvivesABridgeFailure(t *testing.T) {
	// A schema-valid body: `rejected` requires at least one issue, and a code
	// -mode issue requires file+line+severity+message.
	const body = `{"verdict":"rejected","summary":"s","issues":[` +
		`{"severity":"high","file":"a.go","line":1,"message":"boom"}]}`
	// A stream that says something real and then dies without TurnComplete.
	calls := stubClaudeQueryStream(t, claudeAttempt{events: []claude.Event{
		claude.ReadyEvent{Info: claude.SessionInfo{SessionID: "s1", Model: "opus"}},
		claude.TextEvent{Text: body},
	}})
	_ = calls

	b := newClaudeBackend(Config{BackendType: BackendClaude})
	result, err := b.RunPrompt(context.Background(), "review", nil)
	if err == nil {
		t.Fatal("a stream ending without TurnComplete must still be an error")
	}
	if result == nil || result.ResponseText != body {
		t.Fatalf("the backend dropped the streamed body on the failure path: %+v", result)
	}
	// And the envelope built from it must be partial, not error — the whole
	// point of preserving the text.
	env := BuildEnvelope(result, BackendClaude, "opus", "s1", "")
	if env.Status != StatusPartial {
		t.Errorf("envelope status = %s, want partial", env.Status)
	}
}
