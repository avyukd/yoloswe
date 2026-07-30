package prdozer

import (
	"context"
	"io"
	"log/slog"
	"testing"

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
