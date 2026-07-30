package prdozer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/wt"
)

// fakeProvider records what each polish round actually asked the agent layer
// for. The options runOne builds are otherwise invisible — the real providers
// only reveal them by dispatching a live session.
type fakeProvider struct {
	err     error
	prompts []string
	cfgs    []agent.ExecuteConfig
	closed  int
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Events() <-chan agent.AgentEvent { return nil }

func (f *fakeProvider) Close() error { f.closed++; return nil }

func (f *fakeProvider) Execute(_ context.Context, prompt string, _ *wt.WorktreeContext,
	opts ...agent.ExecuteOption) (*agent.AgentResult, error) {
	var cfg agent.ExecuteConfig
	for _, o := range opts {
		o(&cfg)
	}
	f.prompts = append(f.prompts, prompt)
	f.cfgs = append(f.cfgs, cfg)
	if f.err != nil {
		return nil, f.err
	}
	return &agent.AgentResult{Success: true, Text: "round output", SessionID: "sess-1"}, nil
}

// polisherWithFake returns a polisher whose provider is the fake, so runOne is
// exercised for real up to the provider boundary.
func polisherWithFake(t *testing.T) (*AgentPolisher, *fakeProvider) {
	t.Helper()
	fake := &fakeProvider{}
	p := NewAgentPolisher(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.newProvider = func(agent.AgentModel) (agent.Provider, error) { return fake, nil }
	return p, fake
}

// The default (no spec) path must reach the provider with the step's model and
// effort, the configured caps, and the flag-bearing prompt — the wiring every
// prior round fixed but nothing asserted end to end.
func TestPolishRunOneWiresSpecOverridesIntoProvider(t *testing.T) {
	t.Parallel()
	p, fake := polisherWithFake(t)

	res, err := p.Run(context.Background(), PolishRequest{
		Spec:     StepSpec{Model: "opus", Effort: "high"},
		Model:    "sonnet",
		WorkDir:  t.TempDir(),
		PRNumber: 288,
		Local:    true,
		Cfg: PolishConfig{
			PermissionMode: "acceptEdits",
			RoundsPerTick:  3,
			MaxTurns:       7,
			MaxBudgetUSD:   12.5,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "sess-1", res.SessionID)
	assert.Equal(t, "round output", res.Output)

	require.Len(t, fake.prompts, 1)
	assert.Equal(t, "/pr-polish --local --rounds 3 288", fake.prompts[0],
		"the default call must carry --local, the per-tick round cap, and the PR number")

	high, err := agent.ParseEffort("high")
	require.NoError(t, err)
	cfg := fake.cfgs[0]
	assert.Equal(t, "opus", cfg.Model, "polish.model must beat the top-level agent model")
	assert.Equal(t, high, cfg.Effort, "polish.effort must reach the provider")
	assert.Equal(t, "acceptEdits", cfg.PermissionMode)
	assert.Equal(t, 7, cfg.MaxTurns)
	assert.InDelta(t, 12.5, cfg.MaxBudgetUSD, 0.001)
	assert.Equal(t, 1, fake.closed, "the provider must be closed even on the happy path")
}

// An unparseable effort must fail before the agent is dispatched: paying for a
// session that runs at the wrong effort is worse than a loud error.
func TestPolishRunOneRejectsUnparseableEffort(t *testing.T) {
	t.Parallel()
	p, fake := polisherWithFake(t)

	_, err := p.Run(context.Background(), PolishRequest{
		Spec:    StepSpec{Effort: "turbo"},
		Model:   "sonnet",
		WorkDir: t.TempDir(),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "polish effort")
	assert.Empty(t, fake.prompts, "no session may be dispatched with an unusable effort")
}

// A spec round asking for the default polish gets the wired call, flags and
// all — that is the whole point of exposing DefaultPolishPrompt to templates.
func TestPolishSpecRoundRendersWiredDefaultPrompt(t *testing.T) {
	t.Parallel()
	p, fake := polisherWithFake(t)

	_, err := p.Run(context.Background(), PolishRequest{
		Spec:     StepSpec{Rounds: []RoundSpec{{Prompt: "{{.DefaultPolishPrompt}}"}}},
		Model:    "sonnet",
		WorkDir:  t.TempDir(),
		PRNumber: 288,
		Cfg:      PolishConfig{RoundsPerTick: 2},
	})
	require.NoError(t, err)
	require.Len(t, fake.prompts, 1)
	assert.Equal(t, "/pr-polish --rounds 2 288", fake.prompts[0])
}

// A command round's output must reach the next agent round as PrevOutput, and
// a completed once round must be reported back so the caller can retire it.
func TestPolishRoundsThreadCommandOutputIntoNextPrompt(t *testing.T) {
	t.Parallel()
	p, fake := polisherWithFake(t)

	gather := RoundSpec{Command: "printf 'two findings'"}
	fix := RoundSpec{Prompt: "address: {{.PrevOutput}}", Once: true}

	res, err := p.Run(context.Background(), PolishRequest{
		Spec:     StepSpec{Rounds: []RoundSpec{gather, fix}},
		Model:    "sonnet",
		WorkDir:  t.TempDir(),
		PRNumber: 288,
	})
	require.NoError(t, err)
	require.Len(t, fake.prompts, 1, "the command round must not open an agent session")
	assert.Equal(t, "address: two findings", fake.prompts[0])
	assert.Equal(t, map[string]bool{fix.onceKey(): true}, res.RanOnceRounds,
		"the once round that completed must be reported so it is never repeated")
	// DurationMs is deliberately not asserted on. It is time.Since(start)
	// truncated to whole milliseconds, and both rounds here are a printf and a
	// fake provider — on a fast machine the whole Run finishes inside one
	// millisecond and records a legitimate 0. Asserting non-zero made the test
	// pass or fail on host speed rather than on the once-round accounting it
	// exists to cover.
}

// The polish path's shell rounds carry the same producer obligation as merge
// rework's: a failing command must reach the watcher marked as a shell-round
// failure, so its captured output is never text-matched into the transient
// exemption.
//
// This is the polish half of the boundary that
// TestClassifyAgentFailure_CommandErrorNeverTransient pins on the consumer
// side. That test hand-builds the marker, so it stays green even if
// runPolishCommand reverts to a bare fmt.Errorf — only this assertion on the
// real error would catch it.
func TestPolishCommandFailure_IsNeverTransient(t *testing.T) {
	t.Parallel()
	p, _ := polisherWithFake(t)

	_, err := p.Run(context.Background(), PolishRequest{
		// A genuine failure whose command text mentions a transient token —
		// runPolishCommand embeds the command string in the error, so the
		// classifier would otherwise read it as a provider outage.
		Spec:     StepSpec{Rounds: []RoundSpec{{Command: `echo "connection reset by peer"; exit 1`}}},
		Model:    "sonnet",
		WorkDir:  t.TempDir(),
		PRNumber: 291,
	})
	require.Error(t, err)

	var cmdErr *CommandRoundError
	require.ErrorAs(t, err, &cmdErr,
		"a polish shell round's failure must reach the watcher marked, through the round wrapper")

	w := &Watcher{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	action, ok := w.classifyAgentFailure("polish", err, err.Error())
	assert.False(t, ok, "a shell round never talks to the provider")
	assert.Empty(t, string(action))

	// Control: the same text unmarked IS exempted, so the case is not vacuous.
	_, bare := w.classifyAgentFailure("polish", errors.New(err.Error()), err.Error())
	assert.True(t, bare,
		"control: this error text must be classifier-matchable, else the case proves nothing")
}

// A once round already recorded for this PR is dropped before it can reach the
// provider — the gate has to hold on the real path, not just in activeRounds.
func TestPolishRoundsSkipCompletedOnceRound(t *testing.T) {
	t.Parallel()
	p, fake := polisherWithFake(t)

	simplify := RoundSpec{Prompt: "/simplify-branch", Once: true}
	repeat := RoundSpec{Prompt: "/pr-polish-comments"}

	res, err := p.Run(context.Background(), PolishRequest{
		Spec:           StepSpec{Rounds: []RoundSpec{simplify, repeat}},
		DoneOnceRounds: map[string]bool{simplify.onceKey(): true},
		Model:          "sonnet",
		WorkDir:        t.TempDir(),
		PRNumber:       288,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"/pr-polish-comments"}, fake.prompts)
	assert.Empty(t, res.RanOnceRounds)
}

// effectiveGracePeriod mirrors the provider's resolveGracePeriod: a positive
// override replaces the default, anything else falls back to it. The tests
// below assert on the resolved window rather than on the raw field, because
// the field's zero value is the *good* case and an equality assertion on it
// reads backwards.
func effectiveGracePeriod(cfg agent.ExecuteConfig) time.Duration {
	if cfg.StreamTurnGracePeriod > 0 {
		return cfg.StreamTurnGracePeriod
	}
	return agent.DefaultStreamTurnGracePeriod
}

// The polish route must reach the provider with a grace period long enough for
// a review skill's backgrounded tool call to terminate. This is a regression
// test for a specific production failure, and for the pattern behind it.
//
// /pr-polish drives review skills that background a tool call right at turn
// end. If the turn is force-completed while that call is still outstanding the
// provider reports "stream idle: turn forced complete after grace period gated
// on background", KillGroup tears the reviewers down, and the retry budget
// drains re-running work from scratch. kernel#8031 spent three ticks failing
// that way and entered a 2h cooldown; kernel#8042 failed the same way at round
// 2/3.
//
// The assertion is a lower bound on the resolved window, not an equality check
// on the option: what protects the session is how long the turn actually waits,
// and both "no override" and "an override >= the default" satisfy that. An
// equality check against a prdozer-local constant is what let a 60s override —
// 10x *below* the default it replaced — look like the fix.
func TestPolishGracePeriodCoversReviewerRuntime(t *testing.T) {
	t.Parallel()
	p, fake := polisherWithFake(t)

	_, err := p.Run(context.Background(), PolishRequest{
		Model:    "opus",
		WorkDir:  t.TempDir(),
		PRNumber: 8031,
		Local:    true,
		Cfg:      PolishConfig{RoundsPerTick: 3},
	})
	require.NoError(t, err)
	require.Len(t, fake.cfgs, 1)
	assert.GreaterOrEqual(t, effectiveGracePeriod(fake.cfgs[0]), agent.DefaultStreamTurnGracePeriod,
		"polish drives bramble reviewers that run 3-7+ minutes; its turn grace must not fall below the provider default")
}

// Both agent routes must build the same base provider options.
//
// Three separate defects have come from polish and merge_rework maintaining
// independent option lists: model and effort honored only for rework (#288),
// command logging dropping output on failure (#288), and rework's 60s grace
// override that no one applied to polish — the one divergence where polish had
// it right. Asserting the shared constructor covers what both need makes the
// next divergence fail here rather than in production.
func TestBaseProviderOptsCoversBothRoutes(t *testing.T) {
	t.Parallel()
	var cfg agent.ExecuteConfig
	for _, o := range baseProviderOpts("/w", "bypass", "opus", nil) {
		o(&cfg)
	}
	assert.Equal(t, "/w", cfg.WorkDir)
	assert.Equal(t, "bypass", cfg.PermissionMode)
	assert.Equal(t, "opus", cfg.Model)
	assert.True(t, cfg.KeepUserSettings,
		"without KeepUserSettings a prompt invoking a user-level skill silently resolves to nothing")
	assert.Zero(t, cfg.StreamTurnGracePeriod,
		"prdozer must not override the turn grace period: an override replaces the provider default rather than extending it, and the default is the value tuned for these workloads")
	assert.GreaterOrEqual(t, effectiveGracePeriod(cfg), agent.DefaultStreamTurnGracePeriod,
		"whatever prdozer sets, the resolved window must cover a reviewer's runtime")
}
