//go:build integration

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/llmendpoint"
	"github.com/bazelment/yoloswe/bramble/ipc"
	"github.com/bazelment/yoloswe/bramble/session"
)

const (
	openRouterSettleTimeout = 3 * time.Minute
	claudeMaxAttempts       = 10
)

type openRouterAttemptResult struct {
	pane         string
	status       string
	sentinelSeen bool
	timedOut     bool
}

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
			prompt := "Reply with exactly one line formed by joining these tokens with hyphens, in order: " +
				strings.Join(tokens, ", ") + ". Do not read files or run commands."
			// The assertion below scans the whole tmux pane, which also shows
			// the prompt the CLI echoed back. The prompt therefore lists the
			// tokens comma-separated while the sentinel joins them with
			// hyphens, so only the model's own answer can match. That is the
			// difference between this test proving the endpoint works and it
			// passing on an empty turn — pin it rather than leave it to a
			// future edit to preserve by accident.
			if strings.Contains(prompt, sentinel) {
				t.Fatalf("prompt contains the sentinel %q verbatim; the pane assertion would pass on the echoed prompt alone\nprompt: %s", sentinel, prompt)
			}

			maxAttempts := 1
			if backend == session.ProviderClaude {
				maxAttempts = claudeMaxAttempts
			}
			for attempt := 1; attempt <= maxAttempts; attempt++ {
				result := h.newSession(fmt.Sprintf("it-openrouter-%s-%d", backend, attempt), ipc.NewSessionParams{
					SessionType:  "builder",
					WorktreePath: h.worktreePath,
					Prompt:       prompt,
					Model:        "stealth/ox-alpha",
					Backend:      backend,
					LLMEndpoint:  endpoint,
				})
				id := session.SessionID(result.SessionID)
				dumpPanesOnFailure(t, h, id)

				attemptResult := h.waitForOpenRouterAttempt(id, sentinel, openRouterSettleTimeout)
				if attemptResult.timedOut {
					t.Fatalf("%s attempt %d/%d did not complete within %s\n--- pane ---\n%s",
						backend, attempt, maxAttempts, openRouterSettleTimeout, attemptResult.pane)
				}
				if attemptResult.status != string(session.StatusIdle) {
					t.Fatalf("%s attempt %d/%d ended with status %q\n--- pane ---\n%s",
						backend, attempt, maxAttempts, attemptResult.status, attemptResult.pane)
				}
				if attemptResult.sentinelSeen {
					t.Logf("%s OpenRouter attempt %d/%d pane:\n%s", backend, attempt, maxAttempts, attemptResult.pane)
					return
				}

				if backend != session.ProviderClaude {
					t.Fatalf("%s completed without sentinel %q\n--- pane ---\n%s", backend, sentinel, attemptResult.pane)
				}
				if attempt == maxAttempts {
					t.Fatalf("claude CLI completed without sentinel %q after %d fresh sessions\n--- last pane ---\n%s",
						sentinel, maxAttempts, attemptResult.pane)
				}
				// Direct OpenRouter Anthropic-wire probes returned thinking+text
				// 6/6. The intermittent empty successful turn occurs at the Claude
				// CLI/wrapper boundary: handleResult permits completion without a
				// text block. Idle is terminal for that turn, so waiting longer cannot
				// produce text; retry in a fresh Bramble/Claude session instead.
				t.Logf("claude CLI completed attempt %d/%d without the sentinel; retrying in a fresh session",
					attempt, maxAttempts)
			}
		})
	}
}

func (h *harness) waitForOpenRouterAttempt(id session.SessionID, sentinel string, timeout time.Duration) openRouterAttemptResult {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	result := openRouterAttemptResult{}
	for time.Now().Before(deadline) {
		result.pane = h.pane(id)
		// Claude collapses completed thinking to "Crunched for ...", so
		// remember the sentinel when visible instead of requiring transient
		// reasoning text to survive the final idle repaint.
		result.sentinelSeen = result.sentinelSeen || strings.Contains(result.pane, sentinel)
		result.status = h.status(id)
		switch result.status {
		case string(session.StatusIdle), string(session.StatusCompleted), string(session.StatusFailed), string(session.StatusStopped):
			return result
		}
		h.answerStartupDialogs(id, result.pane)
		time.Sleep(pollInterval)
	}
	result.timedOut = true
	return result
}
