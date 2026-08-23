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
	"context"
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

	t.Run("claude/messages", func(t *testing.T) {
		if _, err := exec.LookPath("claude"); err != nil {
			t.Skip("claude CLI not on PATH")
		}
		runClaudeOpenRouter(t, endpoint)
	})

	t.Run("codex/responses", func(t *testing.T) {
		if _, err := exec.LookPath("codex"); err != nil {
			t.Skip("codex CLI not on PATH")
		}
		runCodexOpenRouter(t, endpoint)
	})
}

func runClaudeOpenRouter(t *testing.T, endpoint llmendpoint.Endpoint) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), openRouterTimeout)
	defer cancel()

	session := claude.NewSession(
		claude.WithModel(openRouterModel),
		claude.WithWorkDir(t.TempDir()),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithDisablePlugins(),
		claude.WithLLMEndpoint(endpoint),
	)
	if err := session.Start(ctx); err != nil {
		t.Fatalf("claude session start: %v", err)
	}
	defer session.Stop()

	if _, err := session.SendMessage(ctx, openRouterPrompt); err != nil {
		t.Fatalf("claude SendMessage: %v", err)
	}

	// ox-alpha emits thinking before text. The CLI's normal output allowance
	// leaves ample room for both; only the eventual text is asserted here.
	response, err := drainClaudeForLLMEndpoint(ctx, session)
	if err != nil {
		t.Fatalf("claude turn drain: %v", err)
	}
	t.Logf("claude→openrouter response: %s", truncate(response, 200))
	if !containsSecret(response, openRouterSentinel) {
		t.Fatalf("claude→openrouter did not echo sentinel %q; got %s",
			openRouterSentinel, truncate(response, 500))
	}
}

func runCodexOpenRouter(t *testing.T, endpoint llmendpoint.Endpoint) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), openRouterTimeout)
	defer cancel()

	client := codex.NewClient(
		codex.WithClientName("agent-cli-wrapper-openrouter-test"),
		codex.WithClientVersion("1.0.0"),
		codex.WithLLMEndpoint(endpoint),
	)
	if err := client.Start(ctx); err != nil {
		t.Fatalf("codex client start: %v", err)
	}
	defer client.Stop()

	thread, err := client.CreateThread(ctx,
		codex.WithModel(openRouterModel),
		codex.WithWorkDir(t.TempDir()),
		codex.WithApprovalPolicy(codex.ApprovalPolicyFullAuto),
		codex.WithSandbox("read-only"),
	)
	if err != nil {
		t.Fatalf("codex CreateThread: %v", err)
	}
	if err := thread.WaitReady(ctx); err != nil {
		t.Fatalf("codex thread WaitReady: %v", err)
	}

	result, err := thread.Ask(ctx, openRouterPrompt)
	if err != nil {
		t.Fatalf("codex Ask: %v", err)
	}
	if !result.Success {
		t.Fatalf("codex turn failed: %v\nfull text: %s",
			result.Error, truncate(result.FullText, 500))
	}
	t.Logf("codex→openrouter response: %s", truncate(result.FullText, 200))
	if !containsSecret(result.FullText, openRouterSentinel) {
		t.Fatalf("codex→openrouter did not echo sentinel %q; got %s",
			openRouterSentinel, truncate(result.FullText, 500))
	}
}

// Compile-time guards keep the option signatures used by this live test from
// silently drifting into no-ops.
var (
	_ claude.SessionOption = claude.WithLLMEndpoint(llmendpoint.Endpoint{})
	_ codex.ClientOption   = codex.WithLLMEndpoint(llmendpoint.Endpoint{})
)
