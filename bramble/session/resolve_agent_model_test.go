package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/llmendpoint"
	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/wt"
)

func TestResolveAgentModel_ExactMatchFromGlobalList(t *testing.T) {
	t.Parallel()

	m, err := resolveAgentModel("opus", "", nil)
	require.NoError(t, err)
	assert.Equal(t, "opus", m.ID)
	assert.Equal(t, agent.ProviderClaude, m.Provider)
}

func TestResolveAgentModel_ExactMatchFromRegistry(t *testing.T) {
	t.Parallel()

	avail := agent.NewProviderAvailabilityFromMap(map[string]agent.ProviderStatus{
		agent.ProviderClaude: {Provider: agent.ProviderClaude, Installed: true},
		agent.ProviderCodex:  {Provider: agent.ProviderCodex, Installed: false},
		agent.ProviderGemini: {Provider: agent.ProviderGemini, Installed: false},
		agent.ProviderCursor: {Provider: agent.ProviderCursor, Installed: false},
		agent.ProviderAgy:    {Provider: agent.ProviderAgy, Installed: false},
	})
	reg := agent.NewModelRegistry(avail, nil)

	m, err := resolveAgentModel("opus", "", reg)
	require.NoError(t, err)
	assert.Equal(t, "opus", m.ID)
	assert.Equal(t, agent.ProviderClaude, m.Provider)
}

func TestResolveAgentModel_PrefixFallback(t *testing.T) {
	t.Parallel()

	cases := []struct {
		modelID  string
		provider string
	}{
		{"gpt-future-9000", agent.ProviderCodex},
		{"gemini-99-ultra", agent.ProviderGemini},
		{"cursor-fast", agent.ProviderCursor},
		{"composer-3", agent.ProviderCursor},
		{"agy-pro", agent.ProviderAgy},
		{"claude-opus-5", agent.ProviderClaude},
		{"fable-5", agent.ProviderClaude},
	}

	for _, tc := range cases {
		t.Run(tc.modelID, func(t *testing.T) {
			t.Parallel()
			m, err := resolveAgentModel(tc.modelID, "", nil)
			require.NoError(t, err)
			assert.Equal(t, tc.modelID, m.ID)
			assert.Equal(t, tc.provider, m.Provider)
			assert.Equal(t, tc.modelID, m.Label)
		})
	}
}

func TestResolveAgentModel_UnknownModelFails(t *testing.T) {
	t.Parallel()

	_, err := resolveAgentModel("foo-bar", "", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "foo-bar")
	assert.Contains(t, err.Error(), "gpt-")
}

func TestResolveAgentModel_EmptyModelFails(t *testing.T) {
	t.Parallel()

	_, err := resolveAgentModel("", "", nil)
	require.Error(t, err)
}

// Drives the SpawnOpts route, which is the one every real caller uses: bramble
// main.go fills SpawnOpts from the IPC request. An earlier version of this test
// went through per-manager defaults instead, so it would have stayed green if
// the SpawnOpts plumbing were deleted outright.
func TestManager_ExplicitBackendAllowsOpenRouterModelEndToEnd(t *testing.T) {
	t.Parallel()

	endpoint := llmendpoint.OpenRouter()
	// Inline rather than t.Setenv so the test stays parallel-safe; the manager
	// now refuses to launch an endpoint whose key resolves to nothing.
	endpoint.APIKey = "openrouter-test-key"
	mgr := NewManagerWithConfig(ManagerConfig{
		Provider:    &silentEphemeralProvider{},
		SessionMode: SessionModeTUI,
	})
	t.Cleanup(mgr.Close)

	sid, err := mgr.StartSessionWithOpts(
		SessionTypeBuilder, t.TempDir(), "test prompt", "stealth/ox-alpha",
		SpawnOpts{Backend: ProviderCodex, LLMEndpoint: endpoint},
	)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		info, ok := mgr.GetSessionInfo(sid)
		return ok && info.Status == StatusIdle
	}, 5*time.Second, 10*time.Millisecond)

	info, ok := mgr.GetSessionInfo(sid)
	require.True(t, ok)
	assert.Equal(t, "stealth/ox-alpha", info.Model)
	assert.Equal(t, ProviderCodex, info.Backend)
	assert.NotContains(t, info.ErrorMsg, "unknown model")
}

func TestManager_CodexRejectsDeadChatWire(t *testing.T) {
	t.Parallel()

	endpoint := llmendpoint.OpenRouter()
	endpoint.Wire = llmendpoint.WireAPIChat
	mgr := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTmux})
	t.Cleanup(mgr.Close)

	_, err := mgr.StartSessionWithOpts(
		SessionTypeBuilder,
		t.TempDir(),
		"test prompt",
		"stealth/ox-alpha",
		SpawnOpts{Backend: ProviderCodex, LLMEndpoint: endpoint},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no longer supported")
	assert.Contains(t, err.Error(), "responses")
}

// TestManager_PrefixModelRoutesToCorrectProvider verifies that a model ID
// resolved only by prefix rule selects the right provider in runSession — not
// the Claude default. The assertion strategy: mark the expected provider as
// not installed so the session fails with "provider X is not available"; if
// routing fell through to Claude, the error would name "claude" instead.
func TestManager_PrefixModelRoutesToCorrectProvider(t *testing.T) {
	t.Parallel()

	cases := []struct {
		modelID         string
		unavailProvider string
		availProvider   string // must be installed so the registry accepts the session
	}{
		{"gpt-future-9000", agent.ProviderCodex, agent.ProviderClaude},
		{"gemini-99-ultra", agent.ProviderGemini, agent.ProviderClaude},
		{"cursor-fast-99", agent.ProviderCursor, agent.ProviderClaude},
		{"composer-v9", agent.ProviderCursor, agent.ProviderClaude},
		{"agy-pro", agent.ProviderAgy, agent.ProviderClaude},
		{"fable-5", agent.ProviderClaude, agent.ProviderCodex},
	}

	for _, tc := range cases {
		t.Run(tc.modelID, func(t *testing.T) {
			t.Parallel()

			statusMap := map[string]agent.ProviderStatus{
				agent.ProviderClaude: {Provider: agent.ProviderClaude, Installed: true},
				agent.ProviderCodex:  {Provider: agent.ProviderCodex, Installed: true},
				agent.ProviderGemini: {Provider: agent.ProviderGemini, Installed: true},
				agent.ProviderCursor: {Provider: agent.ProviderCursor, Installed: true},
				agent.ProviderAgy:    {Provider: agent.ProviderAgy, Installed: true},
			}
			// Mark the target provider as not installed so runSession rejects it
			// with a message naming that provider — proving routing chose it.
			statusMap[tc.unavailProvider] = agent.ProviderStatus{Provider: tc.unavailProvider, Installed: false}
			avail := agent.NewProviderAvailabilityFromMap(statusMap)
			reg := agent.NewModelRegistry(avail, nil)

			mgr := NewManagerWithConfig(ManagerConfig{
				ModelRegistry: reg,
				SessionMode:   SessionModeTUI,
			})
			t.Cleanup(mgr.Close)

			sid, err := mgr.StartSession(SessionTypeBuilder, t.TempDir(), "test prompt", tc.modelID)
			require.NoError(t, err)

			require.Eventually(t, func() bool {
				info, ok := mgr.GetSessionInfo(sid)
				return ok && info.Status == StatusFailed && info.ErrorMsg != ""
			}, 5*time.Second, 10*time.Millisecond)

			info, ok := mgr.GetSessionInfo(sid)
			require.True(t, ok)
			require.NotEmpty(t, info.ErrorMsg)
			assert.Contains(t, info.ErrorMsg, tc.unavailProvider,
				"error should name the resolved provider, not the Claude fallback")
		})
	}
}

// TestManager_UnknownModelLandsInStatusFailed verifies that the full manager
// path fails clearly when a session is started with an unrecognized model ID
// that has no curated entry and no recognized prefix.
func TestManager_UnknownModelLandsInStatusFailed(t *testing.T) {
	t.Parallel()

	avail := agent.NewProviderAvailabilityFromMap(map[string]agent.ProviderStatus{
		agent.ProviderClaude: {Provider: agent.ProviderClaude, Installed: true},
		agent.ProviderCodex:  {Provider: agent.ProviderCodex, Installed: true},
		agent.ProviderGemini: {Provider: agent.ProviderGemini, Installed: true},
		agent.ProviderCursor: {Provider: agent.ProviderCursor, Installed: true},
		agent.ProviderAgy:    {Provider: agent.ProviderAgy, Installed: true},
	})
	reg := agent.NewModelRegistry(avail, nil)

	mgr := NewManagerWithConfig(ManagerConfig{
		ModelRegistry: reg,
		SessionMode:   SessionModeTUI,
	})
	t.Cleanup(mgr.Close)

	sid, err := mgr.StartSession(SessionTypeBuilder, t.TempDir(), "test prompt", "foo-bar")
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		info, ok := mgr.GetSessionInfo(sid)
		return ok && info.Status == StatusFailed && info.ErrorMsg != ""
	}, 5*time.Second, 10*time.Millisecond)

	info, ok := mgr.GetSessionInfo(sid)
	require.True(t, ok)
	require.NotEmpty(t, info.ErrorMsg)
	assert.Contains(t, info.ErrorMsg, "foo-bar")
	assert.Contains(t, info.ErrorMsg, "gpt-")

	lines := mgr.GetSessionOutput(sid)
	var hasError bool
	for _, l := range lines {
		if l.Type == OutputTypeError && l.Content != "" {
			hasError = true
		}
	}
	assert.True(t, hasError, "expected at least one OutputTypeError line")
}

// Only the env var *name* is guaranteed to cross IPC from `bramble new-session`;
// this process is the one that reads it. Without this guard the wrappers omit
// the auth headers while still setting the base URL, and the session launches
// pointed at the endpoint with no credential — a remote 401 that names nothing.
func TestManager_UnresolvedEndpointKeyIsNamed(t *testing.T) {
	t.Parallel()

	endpoint := llmendpoint.OpenRouter()
	endpoint.APIKeyEnv = "BRAMBLE_TEST_ABSENT_OPENROUTER_KEY"
	mgr := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTmux})
	t.Cleanup(mgr.Close)

	_, err := mgr.StartSessionWithOpts(
		SessionTypeBuilder, t.TempDir(), "test prompt", "stealth/ox-alpha",
		SpawnOpts{Backend: ProviderCodex, LLMEndpoint: endpoint},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BRAMBLE_TEST_ABSENT_OPENROUTER_KEY",
		"the error must name the variable the operator typed")
}

// The guard must key off resolution, not off APIKeyEnv: an inline key (the
// shape `bramble new-session` now sends after resolving in the client) has no
// env var to read and must launch.
func TestManager_InlineEndpointKeySatisfiesTheGuard(t *testing.T) {
	t.Parallel()

	endpoint := llmendpoint.OpenRouter()
	endpoint.APIKeyEnv = "BRAMBLE_TEST_ABSENT_OPENROUTER_KEY"
	endpoint.APIKey = "resolved-by-the-client"
	mgr := NewManagerWithConfig(ManagerConfig{
		Provider:    &silentEphemeralProvider{},
		SessionMode: SessionModeTUI,
	})
	t.Cleanup(mgr.Close)

	_, err := mgr.StartSessionWithOpts(
		SessionTypeBuilder, t.TempDir(), "test prompt", "stealth/ox-alpha",
		SpawnOpts{Backend: ProviderCodex, LLMEndpoint: endpoint},
	)
	require.NoError(t, err)
}

// An explicit --backend means the model ID is that backend's own, so there is
// no sensible default to substitute. Defaulting anyway launched
// `codex -m sonnet` against the endpoint and surfaced a remote 400 instead of
// naming the missing flag — and left resolveAgentModel's empty-model guard
// unreachable from this path.
func TestManager_ExplicitBackendWithoutModelNamesTheMissingFlag(t *testing.T) {
	t.Parallel()

	endpoint := llmendpoint.OpenRouter()
	endpoint.APIKey = "openrouter-test-key"
	mgr := NewManagerWithConfig(ManagerConfig{
		Provider:    &silentEphemeralProvider{},
		SessionMode: SessionModeTUI,
	})
	t.Cleanup(mgr.Close)

	// Synchronously, so `bramble new-session` prints it on stderr rather than a
	// session ID followed by a background failure the operator has to go
	// looking for in the TUI.
	_, err := mgr.StartSessionWithOpts(
		SessionTypeBuilder, t.TempDir(), "test prompt", "",
		SpawnOpts{Backend: ProviderCodex, LLMEndpoint: endpoint},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "model must not be empty")
	assert.NotContains(t, err.Error(), "sonnet", "the default must not have been substituted")
}

// ResumeSession does not go through startSessionWithID, so the credential
// guard installed there left the resume path uncovered — and a persisted
// endpoint is exactly the case that needs it: SessionToStored writes it via
// Redacted(), so the rehydrated endpoint has no APIKey and re-resolves through
// APIKeyEnv. A TUI whose environment lacks that variable would resume the
// window against the endpoint with the user's own credentials.
func TestManager_ResumeChecksEndpointCredential(t *testing.T) {
	t.Parallel()

	endpoint := llmendpoint.OpenRouter()
	endpoint.APIKeyEnv = "BRAMBLE_TEST_ABSENT_OPENROUTER_KEY"
	mgr := NewManagerWithConfig(ManagerConfig{
		Provider:    &silentEphemeralProvider{},
		SessionMode: SessionModeTUI,
	})
	t.Cleanup(mgr.Close)

	// Register the session directly: startSessionWithID would reject the
	// unresolvable key up front, which is the very check this path skips.
	sid := SessionID("builder-resumed")
	mgr.mu.Lock()
	mgr.sessions[sid] = &Session{
		ID:           sid,
		Type:         SessionTypeBuilder,
		Status:       StatusCompleted,
		WorktreePath: t.TempDir(),
		Model:        "stealth/ox-alpha",
		Backend:      ProviderCodex,
		LLMEndpoint:  endpoint,
		CLISessionID: "cli-abc123",
		Progress:     &SessionProgress{LastActivity: time.Now()},
	}
	mgr.mu.Unlock()

	require.NoError(t, mgr.ResumeSession(sid, "continue"))
	require.Eventually(t, func() bool {
		info, ok := mgr.GetSessionInfo(sid)
		return ok && info.Status == StatusFailed
	}, 5*time.Second, 10*time.Millisecond)

	info, ok := mgr.GetSessionInfo(sid)
	require.True(t, ok)
	assert.Contains(t, info.ErrorMsg, "BRAMBLE_TEST_ABSENT_OPENROUTER_KEY",
		"a resumed session must name the unresolved variable, not fail on a remote 401")
}

// endpointRecordingProvider captures the ExecuteConfig its turn was given, so a
// test can assert on what actually reached the provider rather than on what the
// manager was asked for.
type endpointRecordingProvider struct {
	endpoint llmendpoint.Endpoint
	mu       sync.Mutex
	seen     bool
}

func (p *endpointRecordingProvider) Name() string                    { return "endpoint-recording" }
func (p *endpointRecordingProvider) Events() <-chan agent.AgentEvent { return nil }
func (p *endpointRecordingProvider) Close() error                    { return nil }

func (p *endpointRecordingProvider) Execute(_ context.Context, prompt string, _ *wt.WorktreeContext, opts ...agent.ExecuteOption) (*agent.AgentResult, error) {
	cfg := agent.ExecuteConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	p.mu.Lock()
	p.endpoint = cfg.LLMEndpoint
	p.seen = true
	p.mu.Unlock()
	return &agent.AgentResult{Text: "ok: " + prompt, Success: true}, nil
}

func (p *endpointRecordingProvider) observed() (llmendpoint.Endpoint, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.endpoint, p.seen
}

// Asserting on session metadata proves only that the flags were parsed. This
// drives the whole TUI chain — session.LLMEndpoint to providerRunner to
// WithProviderLLMEndpoint to the provider's ExecuteConfig — so deleting any
// link fails here. The endpoint is attached once after the provider branch in
// runSession, which is what makes this cover every branch rather than the one
// this fake happens to take.
func TestManager_EndpointReachesTheProviderTurn(t *testing.T) {
	t.Parallel()

	endpoint := llmendpoint.OpenRouter()
	endpoint.APIKey = "openrouter-test-key"
	provider := &endpointRecordingProvider{}
	mgr := NewManagerWithConfig(ManagerConfig{
		Provider:    provider,
		SessionMode: SessionModeTUI,
	})
	t.Cleanup(mgr.Close)

	_, err := mgr.StartSessionWithOpts(
		SessionTypeBuilder, t.TempDir(), "test prompt", "stealth/ox-alpha",
		SpawnOpts{Backend: ProviderCodex, LLMEndpoint: endpoint},
	)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, seen := provider.observed()
		return seen
	}, 5*time.Second, 10*time.Millisecond, "provider turn never ran")

	got, _ := provider.observed()
	assert.Equal(t, endpoint, got, "the session's endpoint must reach the provider turn intact")
}

// agent.CLIModelArg drops the model flag entirely when the id belongs to
// another provider, so without this rule `--backend codex --model opus`
// launched codex with no -m — running codex's default model while bramble
// recorded and displayed "opus". Rejecting the pair at the producer is what
// keeps that unreachable rather than papering over it at each consumer.
func TestManager_BackendAndModelMustAgree(t *testing.T) {
	t.Parallel()

	mgr := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTmux})
	t.Cleanup(mgr.Close)

	_, err := mgr.StartSessionWithOpts(
		SessionTypeBuilder, t.TempDir(), "test prompt", "opus",
		SpawnOpts{Backend: ProviderCodex},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "opus")
	assert.Contains(t, err.Error(), ProviderCodex)
	assert.Contains(t, err.Error(), ProviderClaude, "the error must name the backend the model does belong to")
}

// The rule must not reject the case the PR exists for: a third-party id the
// curated registry has never heard of is exactly what --backend carries.
func TestManager_UncuratedModelIsAllowedWithExplicitBackend(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateBackendModel(ProviderCodex, "stealth/ox-alpha"))
	require.NoError(t, validateBackendModel(ProviderClaude, "stealth/ox-alpha"))
	// A curated id whose provider matches is equally fine.
	require.NoError(t, validateBackendModel(ProviderClaude, "opus"))
	// And no backend means no pairing to check.
	require.NoError(t, validateBackendModel("", "opus"))
}

// Redacted() drops Headers, so a persisted header-bearing endpoint comes back
// without them and fails opaquely against a gateway that requires one. Refuse
// at creation, where the caller can still act on it.
func TestManager_HeaderBearingEndpointIsRefusedWhenPersisted(t *testing.T) {
	t.Parallel()

	endpoint := llmendpoint.OpenRouter()
	endpoint.APIKey = "openrouter-test-key"
	endpoint.Headers = map[string]string{"X-Tenant": "acme"}

	// The guard is about what survives the store, so it keys off the store.
	require.NoError(t, validatePersistableEndpoint(endpoint, false),
		"a manager with no store never rehydrates, so headers cannot vanish")
	err := validatePersistableEndpoint(endpoint, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "header")
	assert.Contains(t, err.Error(), "resume")

	// Redacted() really does drop them — the premise of the guard, asserted
	// rather than assumed, since a future change to Redacted would make this
	// rule obsolete rather than merely wrong.
	assert.Empty(t, endpoint.Redacted().Headers)
	assert.NotEmpty(t, endpoint.Headers, "Redacted must not mutate the original")
}
