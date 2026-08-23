//go:build integration

package integration

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/llmendpoint"
	"github.com/bazelment/yoloswe/bramble/ipc"
	"github.com/bazelment/yoloswe/bramble/session"
)

const openRouterSettleTimeout = 3 * time.Minute

// TestLiveOpenRouterNewSession proves the public bramble spawn path can carry
// one endpoint/model pair through IPC and tmux into both supported CLIs, and
// that each CLI accepts the resulting configuration and returns model output.
func TestLiveOpenRouterNewSession(t *testing.T) {
	if os.Getenv("OPEN_ROUTER_KEY") == "" {
		t.Skip("OPEN_ROUTER_KEY is unset")
	}

	for _, backend := range []string{session.ProviderClaude, session.ProviderCodex} {
		t.Run(backend, func(t *testing.T) {
			if _, err := exec.LookPath(backend); err != nil {
				t.Skipf("%s is not installed", backend)
			}

			h := newHarness(t, false)
			endpoint := llmendpoint.OpenRouter()
			endpoint.APIKeyEnv = "OPEN_ROUTER_KEY"
			tokens := []string{"OPENROUTER", strings.ToUpper(backend), "BRAMBLE", "LIVE", "SENTINEL"}
			sentinel := strings.Join(tokens, "-")
			result := h.newSession("it-openrouter-"+backend, ipc.NewSessionParams{
				SessionType:  "builder",
				WorktreePath: h.worktreePath,
				Prompt: "Reply with exactly one line formed by joining these tokens with hyphens, in order: " +
					strings.Join(tokens, ", ") + ". Do not read files or run commands.",
				Model:       "stealth/ox-alpha",
				Backend:     backend,
				LLMEndpoint: endpoint,
			})
			id := session.SessionID(result.SessionID)
			dumpPanesOnFailure(t, h, id)

			seenSentinel := false
			h.awaitClearingDialogsFor(id, openRouterSettleTimeout, func(pane string) bool {
				// Claude collapses completed thinking to "Crunched for ...", so
				// remember the sentinel when it is visible rather than requiring
				// transient reasoning text to survive the final idle repaint.
				seenSentinel = seenSentinel || strings.Contains(pane, sentinel)
				return seenSentinel && h.status(id) == "idle"
			}, "%s did not return the OpenRouter sentinel", backend)
			t.Logf("%s OpenRouter pane:\n%s", backend, h.pane(id))
		})
	}
}
