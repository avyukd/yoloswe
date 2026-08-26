//go:build integration

// Package integration drives a real bramble binary, in tmux mode, on a real
// (throwaway) git repo, and exercises the subagent path end to end: lineage,
// automatic reporting to a parent, and queued delivery in both directions.
//
// These are the manual reproductions of the bugs that only appear once a real
// CLI is running in a real pane — a paste dropped while the agent's TUI is
// still finalizing, an Enter eaten by tmux copy mode, a session whose status
// never leaves "idle" so its next turn produces no state change. None of them
// are visible from unit tests, and all three silently broke subagent messaging.
//
// Everything is isolated: a private tmux server (`tmux -S`), a private HOME so
// the delivery queue and session store are the test's own, a private
// XDG_RUNTIME_DIR so socket discovery cannot find another bramble, and a
// throwaway worktree repo. Nothing here touches the developer's tmux session,
// their ~/.bramble, or their real repos.
//
// Run them with:
//
//	bazel test //bramble/integration:integration_test --test_output=all
//
// They are tagged manual and never run in CI: they need tmux, a built bramble
// binary, and (for the live-backend cases) a logged-in agent CLI.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/control"
	"github.com/bazelment/yoloswe/bramble/ipc"
	"github.com/bazelment/yoloswe/bramble/session"
)

const (
	repoName = "subagentrepo"
	// settleTimeout bounds waits on a real agent CLI reacting.
	// Real backends have to receive, run, and answer a prompt within it.
	settleTimeout = 90 * time.Second
	pollInterval = 250 * time.Millisecond
)

// harness is one isolated bramble under test.
type harness struct {
	t            *testing.T
	tmuxSocket   string
	worktreePath string
	ipcSock      string
	controlSock  string
	home         string
	stubLog      string
	// Kept so restart can bring bramble back up in place.
	launchCmd  string
	runtimeDir string
	// Tracked because restart brings the replacement up in a new window.
	brambleWindow string
	// One visible dialog should collect one answer, even across TUI repaints.
	answeredDialogs map[dialogKey]bool
}

// newHarness starts isolated tmux, repo, HOME/runtime state, and bramble.
// stubAgent installs the scripted backend; false leaves real CLIs on PATH.
func newHarness(t *testing.T, stubAgent bool) *harness {
	t.Helper()
	requireTool(t, "tmux")
	requireTool(t, "git")
	brambleBin := brambleBinary(t)

	root := t.TempDir()
	// A unix socket path is capped at ~107 bytes, and bazel's tmpdir plus a
	// descriptive test name blows straight through that. Both the tmux socket
	// and bramble's own pid-scoped sockets — which land in XDG_RUNTIME_DIR —
	// have to live somewhere shallow. Everything else can use the long path.
	shortRoot := shortTempDir(t)
	h := &harness{
		t:               t,
		tmuxSocket:      filepath.Join(shortRoot, "t.sock"),
		answeredDialogs: map[dialogKey]bool{},
	}
	wtRoot := filepath.Join(root, "worktrees")
	runtimeDir := filepath.Join(shortRoot, "run")

	// The live-backend cases must keep the developer's real HOME: an agent CLI
	// reads its credentials from there, and a logged-out CLI hangs on an
	// interactive prompt rather than failing. The stubbed cases get a private
	// HOME so the delivery queue and session store are the test's own.
	h.home = os.Getenv("HOME")
	if stubAgent {
		h.home = filepath.Join(root, "home")
	}
	for _, dir := range []string{h.home, wtRoot} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	// bramble publishes a stable socket only under a private XDG_RUNTIME_DIR;
	// chmod is explicit because MkdirAll applies the umask.
	// A 0755 runtime dir falls back to the shared temp dir, leaving awaitSockets
	// looking in the wrong place.
	require.NoError(t, os.MkdirAll(runtimeDir, 0o700))
	require.NoError(t, os.Chmod(runtimeDir, 0o700))

	h.worktreePath = seedRepo(t, wtRoot)

	h.stubLog = filepath.Join(root, "stub.log")
	env := []string{
		"HOME=" + h.home,
		"XDG_RUNTIME_DIR=" + runtimeDir,
		"WT_ROOT=" + wtRoot,
		"TERM=xterm-256color",
		// The stand-in records notify failures that --silent would swallow.
		// Without it, a failed hook looks like an agent that simply stayed busy.
		"BRAMBLE_IT_STUB_LOG=" + h.stubLog,
	}
	pathDirs := os.Getenv("PATH")
	if stubAgent {
		// Prepended so provider probes and tmux windows find the stand-in.
		pathDirs = installStubAgent(t, root) + string(os.PathListSeparator) + pathDirs
	}
	env = append(env, "PATH="+pathDirs)

	// PATH cannot ride `tmux -e`: tmux special-cases it and silently drops the
	// override, so the whole environment goes through a shell wrapper instead.
	// (The same trap is documented in fleet/dispatch.go.)
	var exports strings.Builder
	for _, kv := range env {
		exports.WriteString(exportOf(kv))
	}
	h.launchCmd = fmt.Sprintf("%sexec %s --repo %s --session-mode tmux --yolo",
		exports.String(), shellQuote(brambleBin), shellQuote(repoName))
	h.runtimeDir = runtimeDir

	out, err := exec.Command("tmux", "-S", h.tmuxSocket, "new-session", "-d",
		"-x", "200", "-y", "50", "-P", "-F", "#{window_id}",
		"-c", h.worktreePath, h.launchCmd).CombinedOutput()
	require.NoError(t, err, "start bramble under tmux: %s", out)
	h.brambleWindow = strings.TrimSpace(string(out))
	require.NotEmpty(t, h.brambleWindow, "tmux did not report the window it started bramble in")

	// Session windows inherit PATH from bramble's tmux client, but arbitrary
	// variables have to be placed on the server. PATH deliberately rides the
	// shell wrapper above because tmux special-cases and drops it here.
	_, _ = h.tmux("set-environment", "-g", "BRAMBLE_IT_STUB_LOG", h.stubLog)

	t.Cleanup(func() {
		// Startup failures are visible only in the TUI pane.
		if t.Failed() {
			if pane, err := exec.Command("tmux", "-S", h.tmuxSocket,
				"capture-pane", "-p", "-t", h.brambleWindow).Output(); err == nil {
				t.Logf("bramble TUI pane:\n%s", strings.TrimRight(string(pane), "\n \t"))
			}
			if log, err := os.ReadFile(h.stubLog); err == nil && len(log) > 0 {
				t.Logf("stub agent log:\n%s", log)
			}
		}
		_ = exec.Command("tmux", "-S", h.tmuxSocket, "kill-server").Run()
	})

	h.awaitSockets(runtimeDir)
	return h
}

// restart brings bramble back on the same tmux server, store, and HOME so
// existing session windows survive into the re-adoption path.
// The new bramble must find them through ReconcileTmuxSessions.
func (h *harness) restart() {
	h.t.Helper()

	// SIGTERM lets session managers close and write the store; kill-window would
	// leave nothing for the restart to re-adopt.
	// The launch command execs bramble, so pane_pid is the bramble process.
	pid, err := h.tmux("display-message", "-p", "-t", h.brambleWindow, "#{pane_pid}")
	require.NoError(h.t, err, "find the bramble process")
	require.NotEmpty(h.t, pid)
	out0, err := exec.Command("kill", "-TERM", pid).CombinedOutput()
	require.NoError(h.t, err, "signal bramble: %s", out0)

	require.Eventually(h.t, func() bool {
		return ipc.NewClient(h.ipcSock).Ping() != nil
	}, settleTimeout, pollInterval, "the old bramble kept answering after it was signalled")

	// Leave socket files in place: pre-restart windows froze that path, so
	// reclaiming the dead stable name is the behavior under test.
	h.ipcSock, h.controlSock = "", ""

	out, err := exec.Command("tmux", "-S", h.tmuxSocket, "new-window", "-d",
		"-P", "-F", "#{window_id}", "-c", h.worktreePath, h.launchCmd).CombinedOutput()
	require.NoError(h.t, err, "restart bramble under tmux: %s", out)
	h.brambleWindow = strings.TrimSpace(string(out))
	require.NotEmpty(h.t, h.brambleWindow, "tmux did not report the window it restarted bramble in")

	h.awaitSockets(h.runtimeDir)
}

// controlAnswers reports whether a live control server is behind path.
//
// Connecting is the test: sending a request would require the peer to speak the
// protocol and could hang on a listener that accepts but never replies.
func controlAnswers(path string) bool {
	conn, err := control.DialUnix(path)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// awaitSockets pings candidates instead of trusting glob order. Stable and
// pid-scoped sockets can both exist, and stale pid-scoped names sort before the
// live stable name this restart path needs to find.
// The glob spans both spellings; the first candidate that answers wins.
// The same rule is used for IPC and control sockets so stale files cannot poison
// later control requests.
func (h *harness) awaitSockets(runtimeDir string) {
	h.t.Helper()
	require.Eventually(h.t, func() bool {
		socks, _ := filepath.Glob(filepath.Join(runtimeDir, "bramble*.sock"))
		h.ipcSock, h.controlSock = "", ""
		for _, s := range socks {
			if strings.Contains(filepath.Base(s), "control") {
				if h.controlSock == "" && controlAnswers(s) {
					h.controlSock = s
				}
				continue
			}
			if h.ipcSock == "" && ipc.NewClient(s).Ping() == nil {
				h.ipcSock = s
			}
		}
		return h.ipcSock != "" && h.controlSock != ""
	}, settleTimeout, pollInterval, "bramble never came up on its sockets")
}

// --- talking to the bramble under test ---------------------------------------

func (h *harness) newSession(reqID string, params ipc.NewSessionParams) ipc.NewSessionResult {
	h.t.Helper()
	params.RepoName = repoName
	resp, err := ipc.NewClient(h.ipcSock).Send(&ipc.Request{
		Type:   ipc.RequestNewSession,
		ID:     reqID,
		Params: &params,
	})
	require.NoError(h.t, err)
	require.Truef(h.t, resp.OK, "%s failed: %s", reqID, resp.Error)

	var result ipc.NewSessionResult
	requireDecode(h.t, resp.Result, &result)
	require.NotEmpty(h.t, result.SessionID)
	return result
}

// spawn creates a session and returns its ID. A parent of "" is a top-level
// session; anything else makes the new session that session's subagent.
func (h *harness) spawn(sessionType, model, parent, prompt string) session.SessionID {
	h.t.Helper()
	result := h.newSession("it-new-session", ipc.NewSessionParams{
		SessionType:     sessionType,
		WorktreePath:    h.worktreePath,
		Model:           model,
		ParentSessionID: parent,
		Prompt:          prompt,
	})
	return session.SessionID(result.SessionID)
}

// spawnOnNewWorktree creates a subagent with a worktree of its own rather than
// its parent's, the way `new-session --create-worktree -b <branch>` does, and
// returns the session along with the tree bramble actually made.
//
// It deliberately passes no worktree path: that is what makes bramble create
// one, and it is also what would silently fall back to inheriting the parent's
// tree if worktree creation stopped happening.
func (h *harness) spawnOnNewWorktree(sessionType, model, parent, branch, base, prompt string) (session.SessionID, string) {
	h.t.Helper()
	result := h.newSession("it-new-session-wt", ipc.NewSessionParams{
		SessionType:     sessionType,
		Branch:          branch,
		BaseBranch:      base,
		CreateWorktree:  true,
		Model:           model,
		ParentSessionID: parent,
		Prompt:          prompt,
	})
	require.NotEmptyf(h.t, result.WorktreePath, "no worktree path came back for branch %s", branch)
	return session.SessionID(result.SessionID), result.WorktreePath
}

func (h *harness) gitIn(dir string, args ...string) string {
	h.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoErrorf(h.t, err, "git %s in %s: %s", strings.Join(args, " "), dir, out)
	return strings.TrimSpace(string(out))
}

func (h *harness) sessions() []ipc.SessionSummary {
	h.t.Helper()
	resp, err := ipc.NewClient(h.ipcSock).Send(&ipc.Request{
		Type: ipc.RequestListSessions, ID: "it-list",
	})
	require.NoError(h.t, err)
	require.True(h.t, resp.OK, "list-sessions failed: %s", resp.Error)

	var result ipc.ListSessionsResult
	requireDecode(h.t, resp.Result, &result)
	return result.Sessions
}

func (h *harness) status(id session.SessionID) string {
	h.t.Helper()
	for _, s := range h.sessions() {
		if s.ID == string(id) {
			return s.Status
		}
	}
	return ""
}

func (h *harness) awaitStatus(id session.SessionID, want ...string) {
	h.t.Helper()
	require.Eventuallyf(h.t, func() bool {
		got := h.status(id)
		for _, w := range want {
			if got == w {
				return true
			}
		}
		return false
	}, settleTimeout, pollInterval, "session %s never reached %v (last: %s)", id, want, h.status(id))
}

// send delivers text to a session over the control plane. queue holds it until
// the recipient is idle instead of typing into a live turn.
func (h *harness) send(from, to session.SessionID, text string, queue bool) (control.SendInputResult, error) {
	h.t.Helper()
	req, err := control.NewRequest(control.TypeSessionSendInput, "it-send",
		control.SendInputReq{
			SessionID: string(to), From: string(from),
			Text: text, Submit: true, Queue: queue,
		})
	require.NoError(h.t, err)

	ctx, cancel := context.WithTimeout(context.Background(), settleTimeout)
	defer cancel()
	resp, err := control.Request(ctx, h.controlSock, req)
	require.NoError(h.t, err)

	var result control.SendInputResult
	if err := resp.DecodeResponse(&result); err != nil {
		return control.SendInputResult{}, err
	}
	return result, nil
}

func (h *harness) pane(id session.SessionID) string {
	h.t.Helper()
	resp, err := ipc.NewClient(h.ipcSock).Send(&ipc.Request{
		Type:   ipc.RequestCapturePane,
		ID:     "it-capture",
		Params: &ipc.CapturePaneParams{SessionID: string(id), Lines: 200},
	})
	if err != nil || !resp.OK {
		return ""
	}
	var result ipc.CapturePaneResult
	requireDecode(h.t, resp.Result, &result)
	return strings.Join(result.Lines, "\n")
}

func (h *harness) awaitPane(id session.SessionID, want, because string) {
	h.t.Helper()
	h.awaitPaneCond(id, func() bool { return strings.Contains(h.pane(id), want) },
		"%s: %q never appeared in %s's pane", because, want, id)
}

func (h *harness) countInPane(id session.SessionID, want string) int {
	h.t.Helper()
	return strings.Count(h.pane(id), want)
}

// tmuxTargetOf resolves a session's tmux window, for tests that need to poke
// the pane directly rather than going through bramble.
func (h *harness) tmuxTargetOf(id session.SessionID) string {
	h.t.Helper()
	req, err := control.NewRequest(control.TypeSessionList, "it-sessions", nil)
	require.NoError(h.t, err)
	ctx, cancel := context.WithTimeout(context.Background(), settleTimeout)
	defer cancel()
	resp, err := control.Request(ctx, h.controlSock, req)
	require.NoError(h.t, err)

	var result control.SessionListResult
	require.NoError(h.t, resp.DecodeResponse(&result))
	for _, s := range result.Sessions {
		if s.ID == string(id) {
			return s.TmuxTarget
		}
	}
	h.t.Fatalf("session %s has no tmux target", id)
	return ""
}

func (h *harness) tmux(args ...string) (string, error) {
	h.t.Helper()
	full := append([]string{"-S", h.tmuxSocket}, args...)
	out, err := exec.Command("tmux", full...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// deliveryQueueLen counts recipients, not messages. Tests that mean "my message
// is held" should use queuedFor because parent reports may be racing too.
// The courier persists one file per recipient, holding that recipient's queue.
func (h *harness) deliveryQueueLen() int {
	h.t.Helper()
	files, _ := filepath.Glob(filepath.Join(h.home, ".bramble", "deliveries", "*.json"))
	return len(files)
}

// queuedFor reports how many messages are held for one session.
func (h *harness) queuedFor(id session.SessionID) int {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.home, ".bramble", "deliveries", string(id)+".json"))
	if err != nil {
		return 0 // no file means nothing queued for this recipient
	}
	var queue []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &queue); err != nil {
		h.t.Fatalf("delivery queue for %s is not readable: %v", id, err)
	}
	return len(queue)
}

// startupDialog is a prompt an agent CLI puts in front of its own prompt.
// Left unanswered, the live test times out looking like a bramble bug.
// Fresh worktrees commonly trigger trust, model-deprecation, and rate-limit
// prompts before the agent can take a turn.
type startupDialog struct {
	name string
	// match is specific on purpose: answering the wrong modal picks a menu item.
	match []string
	keys []string
	// fatal dialogs are reported instead of cleared.
	fatal string
}

// dialogKey keeps one session's answered dialog from suppressing another's.
type dialogKey struct {
	id   session.SessionID
	name string
}

var startupDialogs = []startupDialog{
	{
		// Claude directory trust. Default is "Yes, I trust this folder".
		name:  "claude folder trust",
		match: []string{"Is this a project you created or one you trust", "I trust this folder"},
		keys:  []string{"Enter"},
	},
	{
		// This modal must fail closed. Answering "Yes" can forward the user's
		// Anthropic credential to the third-party endpoint; "No" can let the
		// test pass against the wrong provider.
		// In a correct run ANTHROPIC_API_KEY is shadowed before the CLI starts, so
		// seeing the modal means that protection regressed.
		name:  "claude custom API key",
		match: []string{"Detected a custom API key in your environment", "Do you want to use this API key"},
		fatal: "claude prompted for a custom API key: the ANTHROPIC_API_KEY shadow in tmuxRunner.endpointEnv has regressed. " +
			"Answering it would either send this machine's real Anthropic credential to the third-party endpoint (Yes) " +
			"or run the session against the default provider while the test still passes (No).",
	},
	{
		// Codex directory trust. Default is "Yes, continue".
		name:  "codex directory trust",
		match: []string{"Do you trust the contents of this directory", "Yes, continue"},
		keys:  []string{"Enter"},
	},
	{
		// Keep the requested model; silently switching makes the run untraceable.
		name:  "codex model deprecation",
		match: []string{"will be deprecated soon", "Use existing model"},
		keys:  []string{"Down", "Enter"},
	},
	{
		// Escape dismisses without switching model.
		name:  "codex rate-limit switch",
		match: []string{"Approaching rate limits", "Switch to"},
		keys:  []string{"Escape"},
	},
}

// dialogTailLines limits matching to the pane bottom; dismissed dialog text
// remains in scrollback and would otherwise be answered again.
const dialogTailLines = 30

func paneTail(pane string) string {
	lines := strings.Split(pane, "\n")
	kept := make([]string, 0, dialogTailLines)
	for i := len(lines) - 1; i >= 0 && len(kept) < dialogTailLines; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		kept = append(kept, lines[i])
	}
	return strings.Join(kept, "\n")
}

// answerStartupDialogs answers each recognized dialog once until it disappears.
// Keying on full screen contents would treat every TUI repaint as a new dialog.
// Dismissed dialog text remains in scrollback above the prompt, so matching only
// "have I seen these bytes before" re-sends Enter into the live session.
func (h *harness) answerStartupDialogs(id session.SessionID, pane string) bool {
	h.t.Helper()
	tail := paneTail(pane)
	if tail == "" {
		return false
	}
	for _, d := range startupDialogs {
		matched := true
		for _, m := range d.match {
			if !strings.Contains(tail, m) {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		key := dialogKey{id: id, name: d.name}
		if h.answeredDialogs[key] {
			return false // already answered; waiting for it to go away
		}

		if d.fatal != "" {
			h.t.Fatalf("%s dialog in %s: %s\n--- pane ---\n%s", d.name, id, d.fatal, pane)
		}

		target := h.tmuxTargetOf(id)
		h.t.Logf("answering %q dialog in %s with %v", d.name, id, d.keys)
		for _, k := range d.keys {
			if _, err := h.tmux("send-keys", "-t", target, k); err != nil {
				h.t.Logf("failed to answer %q: %v", d.name, err)
				return false
			}
			time.Sleep(300 * time.Millisecond)
		}
		h.answeredDialogs[key] = true
		return true
	}

	// Nothing matched: any dialog previously answered is gone, so a fresh
	// appearance of it may be answered again.
	for _, d := range startupDialogs {
		delete(h.answeredDialogs, dialogKey{id: id, name: d.name})
	}
	return false
}

// awaitClearingDialogs answers first-run dialogs while polling the condition.
// It captures once per iteration so the condition, dialog answerer, and failure
// dump all see the same pane without doubling tmux captures.
// This is the live-test replacement for require.Eventually when a trust prompt
// may be blocking the agent from acting.
func (h *harness) awaitClearingDialogs(id session.SessionID, cond func(pane string) bool, failf string, args ...any) {
	h.t.Helper()
	h.awaitClearingDialogsFor(id, settleTimeout, cond, failf, args...)
}

func (h *harness) awaitClearingDialogsFor(id session.SessionID, timeout time.Duration, cond func(pane string) bool, failf string, args ...any) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var pane string
	for time.Now().Before(deadline) {
		pane = h.pane(id)
		if cond(pane) {
			return
		}
		h.answerStartupDialogs(id, pane)
		time.Sleep(pollInterval)
	}
	h.t.Fatalf(failf+"\n--- pane ---\n%s", append(args, pane)...)
}

// awaitPaneCond captures the pane only after timeout. Putting h.pane() in
// require.Eventuallyf arguments captures before the wait begins.
// On failure, that would print the pre-wait screen instead of the screen that
// actually timed out.
func (h *harness) awaitPaneCond(id session.SessionID, cond func() bool, failf string, args ...any) {
	h.t.Helper()
	deadline := time.Now().Add(settleTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(pollInterval)
	}
	h.t.Fatalf(failf+"\n--- pane ---\n%s", append(args, h.pane(id))...)
}

// neverDuring dumps the pane from the moment the forbidden condition appears.
func (h *harness) neverDuring(id session.SessionID, d time.Duration, cond func() bool, failf string, args ...any) {
	h.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			h.t.Fatalf(failf+"\n--- pane ---\n%s", append(args, h.pane(id))...)
		}
		time.Sleep(pollInterval)
	}
}

// awaitReady handles first-run dialogs before waiting for idle.
func (h *harness) awaitReady(id session.SessionID) {
	h.t.Helper()
	h.awaitClearingDialogs(id, func(string) bool { return h.status(id) == "idle" },
		"session %s never reached its prompt", id)
}

// awaitPaneClearingDialogs waits for text while clearing startup dialogs.
func (h *harness) awaitPaneClearingDialogs(id session.SessionID, want, because string) {
	h.t.Helper()
	h.awaitClearingDialogs(id, func(pane string) bool { return strings.Contains(pane, want) },
		"%s: %q never appeared in %s's pane", because, want, id)
}

// longTurnSeconds is how long a "keep busy" prompt occupies an agent. Long
// enough that a queued message is unambiguously held across a live turn, short
// enough not to dominate the suite.
const longTurnSeconds = 20

// longTurnPrompt shells out because generated text is not a reliable clock; a
// sleep gives every backend the same live turn to queue behind.
func longTurnPrompt(done string) string {
	return fmt.Sprintf(
		"Run this exact shell command and wait for it to finish: sleep %d. "+
			"Then reply with exactly one line and nothing else: %s. "+
			"Do not read or edit any files.", longTurnSeconds, done)
}

// awaitWorking requires both running status and prompt echo; status alone can
// be true before the CLI has started the turn under test.
func (h *harness) awaitWorking(id session.SessionID, promptEcho string) {
	h.t.Helper()
	h.awaitClearingDialogs(id, func(pane string) bool {
		return h.status(id) == "running" && strings.Contains(pane, promptEcho)
	}, "session %s never started working", id)
}

// reportedResultPath returns the newest report path; the path, not just the
// marker text, proves the parent can read the child output.
func reportedResultPath(pane string) (string, bool) {
	matches := resultPathRE.FindAllStringSubmatch(pane, -1)
	if len(matches) == 0 {
		return "", false
	}
	return matches[len(matches)-1][1], true
}

var resultPathRE = regexp.MustCompile(`result:\s+(\S+)`)

// queuedTextFor returns the on-disk queue a restarted bramble would read.
func (h *harness) queuedTextFor(to session.SessionID) string {
	h.t.Helper()
	files, _ := filepath.Glob(filepath.Join(h.home, ".bramble", "deliveries", "*.json"))
	var b strings.Builder
	for _, f := range files {
		if !strings.Contains(filepath.Base(f), string(to)) {
			continue
		}
		if data, err := os.ReadFile(f); err == nil {
			b.Write(data)
		}
	}
	return b.String()
}
