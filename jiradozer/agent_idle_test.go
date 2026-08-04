package jiradozer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	agentpkg "github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/wt"
)

// idleFakeProvider models the two behaviours the watchdog has to tell apart: an
// agent that emits nothing and one that keeps emitting while it works.
//
// Both block until their context ends, so a test that fails does so by the
// watchdog NOT firing (and the fake being released by test cleanup), never by
// the fake returning early and making the assertion vacuous.
type idleFakeProvider struct {
	// emitEvery, when > 0, calls the handler's OnText on that interval,
	// simulating a slow-but-progressing agent.
	emitEvery time.Duration
	// released is closed when Execute returns, so a test can prove the
	// provider was actually cancelled rather than left running.
	released chan struct{}
	ctxErr   error
}

func newIdleFakeProvider(emitEvery time.Duration) *idleFakeProvider {
	return &idleFakeProvider{emitEvery: emitEvery, released: make(chan struct{})}
}

func (p *idleFakeProvider) Name() string { return "fake" }

func (p *idleFakeProvider) Execute(ctx context.Context, _ string, _ *wt.WorktreeContext, opts ...agentpkg.ExecuteOption) (*agentpkg.AgentResult, error) {
	defer close(p.released)
	var cfg agentpkg.ExecuteConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	var tick <-chan time.Time
	if p.emitEvery > 0 {
		ticker := time.NewTicker(p.emitEvery)
		defer ticker.Stop()
		tick = ticker.C
	}
	for {
		select {
		case <-ctx.Done():
			p.ctxErr = ctx.Err()
			return &agentpkg.AgentResult{SessionID: "sess-1"}, ctx.Err()
		case <-tick:
			if cfg.EventHandler != nil {
				cfg.EventHandler.OnText("still working\n")
			}
		}
	}
}

func (p *idleFakeProvider) Events() <-chan agentpkg.AgentEvent { return nil }
func (p *idleFakeProvider) Close() error                       { return nil }

func idleTestRunner(p agentpkg.Provider) agentRunner {
	return agentRunner{
		newProviderForModel: func(agentpkg.AgentModel) (agentpkg.Provider, error) { return p, nil },
		retryBackoffs:       []time.Duration{0},
	}
}

// A silent agent is killed once the gap exceeds idle_timeout, and the error
// identifies the stall rather than looking like a plain cancellation.
func TestRunAgentIdleTimeoutKillsSilentAgent(t *testing.T) {
	provider := newIdleFakeProvider(0)
	runner := idleTestRunner(provider)

	start := time.Now()
	_, err := runner.runAgent(context.Background(), "build", "prompt", StepConfig{
		Model:       "gpt-5.5",
		IdleTimeout: 120 * time.Millisecond,
	}, t.TempDir(), "", nil, discardLogger())

	require.Error(t, err)
	require.ErrorIs(t, err, ErrIdleTimeout,
		"a stall must be distinguishable from a shutdown, or shouldReportFailure stays silent")
	require.Contains(t, err.Error(), "build", "the error should name the stalled step")

	select {
	case <-provider.released:
	case <-time.After(2 * time.Second):
		t.Fatal("provider was never cancelled")
	}
	require.Less(t, time.Since(start), 2*time.Second, "watchdog fired far too late")
}

// The clock measures the gap BETWEEN events, not wall-clock since start, so an
// agent that keeps emitting is never interrupted no matter how long it runs.
func TestRunAgentIdleTimeoutSparesProgressingAgent(t *testing.T) {
	provider := newIdleFakeProvider(20 * time.Millisecond)
	runner := idleTestRunner(provider)

	// Cancel well after several idle_timeouts of wall-clock have elapsed. A
	// wall-clock implementation would have killed this run long before.
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	_, err := runner.runAgent(ctx, "build", "prompt", StepConfig{
		Model:       "gpt-5.5",
		IdleTimeout: 100 * time.Millisecond,
	}, t.TempDir(), "", nil, discardLogger())

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrIdleTimeout,
		"a steadily-progressing agent must never be reported as stalled")
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// idle_timeout: 0 is the documented "disabled" value and the zero value, so a
// config that never opted in must not gain a kill switch by surprise.
func TestRunAgentIdleTimeoutZeroDisablesWatchdog(t *testing.T) {
	provider := newIdleFakeProvider(0)
	runner := idleTestRunner(provider)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	_, err := runner.runAgent(ctx, "build", "prompt", StepConfig{
		Model:       "gpt-5.5",
		IdleTimeout: 0,
	}, t.TempDir(), "", nil, discardLogger())

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrIdleTimeout)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// When the caller cancels too, that is the real reason the run is ending.
// Reporting an idle timeout there would fire a misleading alert on every Ctrl-C
// that happens to land on a quiet moment.
func TestRunAgentIdleTimeoutDefersToParentCancellation(t *testing.T) {
	provider := newIdleFakeProvider(0)
	runner := idleTestRunner(provider)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	_, err := runner.runAgent(ctx, "build", "prompt", StepConfig{
		Model: "gpt-5.5",
		// Long enough that the parent cancellation always wins the race.
		IdleTimeout: 10 * time.Second,
	}, t.TempDir(), "", nil, discardLogger())

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrIdleTimeout)
	require.ErrorIs(t, err, context.Canceled)
}

// A stall must not be retried: ClassifyTransient never sees it, so the run ends
// after exactly one attempt instead of burning another full idle_timeout.
func TestRunAgentIdleTimeoutIsNotRetried(t *testing.T) {
	provider := &countingIdleProvider{idleFakeProvider: newIdleFakeProvider(0)}
	runner := idleTestRunner(provider)

	_, err := runner.runAgent(context.Background(), "build", "prompt", StepConfig{
		Model:            "gpt-5.5",
		IdleTimeout:      80 * time.Millisecond,
		TransientRetries: 3,
	}, t.TempDir(), "", nil, discardLogger())

	require.ErrorIs(t, err, ErrIdleTimeout)
	require.Equal(t, 1, provider.calls, "a stalled agent must not be retried")
}

type countingIdleProvider struct {
	*idleFakeProvider
	calls int
}

func (p *countingIdleProvider) Execute(ctx context.Context, prompt string, wtCtx *wt.WorktreeContext, opts ...agentpkg.ExecuteOption) (*agentpkg.AgentResult, error) {
	p.calls++
	// Each attempt gets a fresh release channel so a retry doesn't panic on a
	// double close.
	p.idleFakeProvider = newIdleFakeProvider(0)
	return p.idleFakeProvider.Execute(ctx, prompt, wtCtx, opts...)
}

// ResolveRound must carry IdleTimeout: every rounds-based step in the bootstrap
// shape (build, validate, ship) would otherwise run with no stall protection.
func TestResolveRoundKeepsIdleTimeoutReachableByWatchdog(t *testing.T) {
	parent := StepConfig{Model: "gpt-5.5", IdleTimeout: 90 * time.Millisecond}
	resolved := ResolveRound(RoundConfig{Prompt: "go"}, parent)
	require.Equal(t, 90*time.Millisecond, resolved.IdleTimeout)

	provider := newIdleFakeProvider(0)
	_, err := idleTestRunner(provider).runAgent(context.Background(), "build", "prompt",
		resolved, t.TempDir(), "", nil, discardLogger())
	require.ErrorIs(t, err, ErrIdleTimeout,
		"the resolved round config must still arm the watchdog")
}
