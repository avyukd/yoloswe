//go:build integration
// +build integration

// Manual integration tests that exercise the claude and codex CLIs against
// OpenRouter's real API. These tests are excluded from `bazel test //...` by
// the manual tag on the integration target.
//
// Run:
//
//	OPEN_ROUTER_KEY=... bazel test \
//	    //agent-cli-wrapper/integration:integration_test \
//	    --test_filter=TestLLMEndpoint_OpenRouter \
//	    --test_tag_filters=integration \
//	    --test_env=OPEN_ROUTER_KEY \
//	    --test_output=streamed
package integration

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/llmendpoint"
)

const (
	openRouterAPIKeyEnv = "OPEN_ROUTER_KEY"
	openRouterModel     = "stealth/ox-alpha"
	openRouterSentinel  = "OPENROUTER-ORANGE-FALCON-83"
	openRouterPrompt    = "Reply with exactly this single token, nothing else: " + openRouterSentinel
	openRouterTimeout   = 3 * time.Minute
)

// TestLLMEndpoint_OpenRouter proves that both wrappers can drive their real
// CLI through the OpenRouter preset. The key override is intentional: this
// host stores the credential in OPEN_ROUTER_KEY rather than the preset's
// conventional OPENROUTER_API_KEY.
func TestLLMEndpoint_OpenRouter(t *testing.T) {
	if os.Getenv(openRouterAPIKeyEnv) == "" {
		t.Skipf("%s not set; export the key (see ~/.keys.sh) and re-run", openRouterAPIKeyEnv)
	}

	endpoint := llmendpoint.OpenRouter()
	endpoint.APIKeyEnv = openRouterAPIKeyEnv
	smoke := llmEndpointSmokeConfig{
		label:             "openrouter",
		model:             openRouterModel,
		prompt:            openRouterPrompt,
		sentinel:          openRouterSentinel,
		timeout:           openRouterTimeout,
		clientName:        "agent-cli-wrapper-openrouter-test",
		claudeMaxAttempts: 10,
	}

	t.Run("claude/messages", func(t *testing.T) {
		if _, err := exec.LookPath("claude"); err != nil {
			t.Skip("claude CLI not on PATH")
		}
		// Direct OpenRouter Anthropic-wire probes returned thinking+text 6/6
		// with this model and prompt. The intermittent successful no-text
		// turn occurs only through the Claude CLI/wrapper boundary. The shared
		// drain checks both streamed text and the canonical completed turn;
		// when both are empty, it retries in a fresh bounded session because
		// TurnComplete is terminal and waiting cannot recover later text.
		runClaudeLLMEndpoint(t, endpoint, smoke)
	})

	t.Run("codex/responses", func(t *testing.T) {
		if _, err := exec.LookPath("codex"); err != nil {
			t.Skip("codex CLI not on PATH")
		}
		// OpenRouter intermittently rejects this free stealth model with
		// "currently experiencing high demand". That is upstream capacity,
		// not endpoint wiring; keep it as a clearly labelled hard failure so
		// the smoke test never reports a false pass.
		runCodexLLMEndpoint(t, endpoint, smoke)
	})
}

// Compile-time guards keep the option signatures used by this live test from
// silently drifting into no-ops.
var (
	_ claude.SessionOption = claude.WithLLMEndpoint(llmendpoint.Endpoint{})
	_ codex.ClientOption   = codex.WithLLMEndpoint(llmendpoint.Endpoint{})
)
