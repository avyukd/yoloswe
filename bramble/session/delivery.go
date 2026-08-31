package session

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// PaneWriter is the tmux controller surface the notifier needs. It lives on the
// consumer side because tmuxctl already imports session.
type PaneWriter interface {
	Paste(ctx context.Context, target, text string) error
	SendEnter(ctx context.Context, target string) error
}

// DeliveryTarget is the session registry surface the notifier needs.
type DeliveryTarget interface {
	SessionInfo(id SessionID) (SessionInfo, bool)
	ResolveTmuxTarget(id SessionID) (string, error)
	// CapturePaneText reads a tmux session's scrollback. The notifier reads it
	// only to decide whether to stay quiet, never to verify what it wrote.
	CapturePaneText(id SessionID, n int) ([]string, error)
	// MarkRunning records that a turn has started. Only bramble knows this for
	// a tmux session it just typed into; see Manager.SetSessionRunning.
	MarkRunning(id SessionID)
}

// registryTarget adapts a *SessionRegistry to DeliveryTarget.
type registryTarget struct{ reg *SessionRegistry }

// NewRegistryDeliveryTarget wraps a registry so it can drive a Notifier.
func NewRegistryDeliveryTarget(reg *SessionRegistry) DeliveryTarget {
	return &registryTarget{reg: reg}
}

func (t *registryTarget) SessionInfo(id SessionID) (SessionInfo, bool) {
	info, _, ok := t.reg.GetSessionInfo(id)
	return info, ok
}

func (t *registryTarget) ResolveTmuxTarget(id SessionID) (string, error) {
	return t.reg.ResolveTmuxTarget(id)
}

func (t *registryTarget) CapturePaneText(id SessionID, n int) ([]string, error) {
	return t.reg.CapturePaneText(id, n)
}

func (t *registryTarget) MarkRunning(id SessionID) { t.reg.SetSessionRunning(id) }

// Notifier hints to a parent session that one of its subagents changed state.
//
// It is deliberately not a delivery mechanism. Completion is recorded by the
// lane itself — a `.done` file, a commit, a branch — and read by the
// orchestrator's poll, which verifies every claim against git before acting on
// it. A pane is a shared, human-owned surface with no addressing and no
// acknowledgement, so anything pushed into one is a guess; the only safe use is
// a hint that costs nothing when it is wrong.
//
// So a notification here is:
//
//   - droppable: never queued, never persisted, never retried. A hint that does
//     not land costs one poll interval of latency, not a lost report. This is
//     what lets every heuristic that used to guard a durable queue go away.
//   - stateless: it carries no payload and no history, so it cannot go stale and
//     a duplicate is harmless rather than ambiguous.
//   - yielding: any doubt about the pane — a draft in the composer, an
//     unreadable frame, a turn in flight — means stay silent and let the poll
//     do its job.
//
// The previous design pushed a generated report with an at-least-once queue.
// That queue could not be both safe for a human's half-typed line and reliable,
// and it chose reliability: undeliverable mail accumulated for days and replayed
// after restarts, so a stale report and a real failure became indistinguishable.
type Notifier struct {
	target DeliveryTarget
	panes  PaneWriter
	// pendingNudge coalesces: a parent already holding an unsent hint gets one
	// line, not one per child. Cleared when the hint is written or abandoned.
	pendingNudge map[SessionID]bool
	mu           sync.Mutex
}

// NotifierConfig is retained for callers that pin filesystem locations. The
// notifier keeps no state on disk; ResearchDir still governs the manager's own
// result files (see Manager.writeResearchFile).
type NotifierConfig struct {
	// LegacyDeliveryDir is swept once at startup. Empty defaults to
	// ~/.bramble/deliveries.
	LegacyDeliveryDir string
}

// NewNotifier creates a notifier and reclaims any queue left by the old courier.
func NewNotifier(target DeliveryTarget, panes PaneWriter, config NotifierConfig) (*Notifier, error) {
	n := &Notifier{
		target:       target,
		panes:        panes,
		pendingNudge: make(map[SessionID]bool),
	}
	n.sweepLegacyQueue(config.LegacyDeliveryDir)
	return n, nil
}

// legacyDeliveryDirName is the directory the retired courier persisted to.
const legacyDeliveryDirName = "deliveries"

// sweepLegacyQueue deletes queues written by the durable courier.
//
// They are deleted rather than delivered on purpose. Every entry is a generated
// report whose ground truth is the run directory and git, and the queues found
// in practice were hours to days old — one held 23 reports spanning 4.5 hours,
// another held ten status updates each announcing that it superseded the last.
// Delivering that history would reproduce exactly the noise this change removes.
func (n *Notifier) sweepLegacyQueue(dir string) {
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return
		}
		dir = filepath.Join(home, ".bramble", legacyDeliveryDirName)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if err := os.Remove(path); err != nil {
			continue
		}
		// Once per file, so a queue that had been silently accumulating is
		// visible in the log rather than vanishing without a trace.
		slog.Info("discarded a queued delivery left by the retired courier; "+
			"lane completion is read from the run directory and git",
			"file", path)
	}
}

// nudgeText is the whole payload. It names no child, no status, and no path:
// the run directory holds all of that, and a hint that carried state could go
// stale, which is the failure this design exists to prevent.
const nudgeText = "[bramble] subagent activity — check your run directory"

// NotifyParent hints to a child's parent that the child changed state.
//
// Every failure mode is a silent return. There is no error to report because
// there is no promise to break: the orchestrator's poll is the delivery path.
func (n *Notifier) NotifyParent(ctx context.Context, child SessionInfo) {
	if child.ParentSessionID == "" {
		return
	}
	parent, ok := n.target.SessionInfo(child.ParentSessionID)
	if !ok || parent.Status.IsTerminal() {
		return
	}
	n.nudge(ctx, parent)
}

// nudge writes at most one disposable line into a parent's pane.
func (n *Notifier) nudge(ctx context.Context, parent SessionInfo) {
	if !n.claimNudge(parent.ID) {
		// A hint for this parent is already in flight or already waiting to be
		// written. One line is as informative as ten.
		return
	}
	defer n.releaseNudge(parent.ID)

	// A TUI session has no pane to hint into, and its turn loop already
	// surfaces child state through the model.
	if !isTmuxRunner(parent.RunnerType) || n.panes == nil {
		return
	}
	if parent.Status != StatusIdle {
		return
	}
	target, err := n.target.ResolveTmuxTarget(parent.ID)
	if err != nil {
		return
	}

	provider := providerForSession(parent)
	lines := n.capturePaneFor(parent.ID, provider)

	// Yield on any sign the pane is not free. Unlike the courier, an unreadable
	// or busy pane is not a problem to be solved with a retry or a grace
	// period: it is simply a hint not worth giving.
	if working, known := paneSaysWorking(provider, lines); known && working {
		return
	}
	// Never type into a human's half-written line. tmux paste-buffer appends,
	// so an Enter here would submit their draft with this text riding on it.
	//
	// Only claude is protected, because only claude's composer can be read: its
	// "❯" is a real prompt glyph, while cursor and codex render placeholder
	// text that vanishes the moment a user types, making a draft
	// indistinguishable from a CLI still booting. Their panes are a documented
	// gap rather than a solved case — but the exposure is one disposable line
	// rather than a queue of reports, and nothing retries it.
	if _, draft, known := composerDraftText(provider, lines); known && draft {
		return
	}

	if err := n.panes.Paste(ctx, target, nudgeText); err != nil {
		return
	}
	if err := n.panes.SendEnter(ctx, target); err != nil {
		return
	}
	// A submitted prompt started a turn; nothing else reports that for a tmux
	// session bramble just typed into.
	n.target.MarkRunning(parent.ID)
}

// claimNudge reserves the right to hint at a parent, reporting whether it got
// it. Coalescing is the only bookkeeping the notifier keeps.
func (n *Notifier) claimNudge(to SessionID) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.pendingNudge[to] {
		return false
	}
	n.pendingNudge[to] = true
	return true
}

func (n *Notifier) releaseNudge(to SessionID) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.pendingNudge, to)
}

// nudgeCaptureLines is how much pane the yield checks read. It matches the
// depth the composer and working probes were measured against.
const nudgeCaptureLines = 40

// capturePaneFor reads the recipient's pane once for both yield checks.
func (n *Notifier) capturePaneFor(id SessionID, provider string) []string {
	// Avoid a tmux round-trip when every provider-keyed check would be unknown.
	if !providerHasIdleProbe(provider) && !composerReadable(provider) {
		return nil
	}
	lines, err := n.target.CapturePaneText(id, nudgeCaptureLines)
	if err != nil {
		return nil
	}
	return lines
}

// Watch hints to parents as their children change state. It returns an
// unsubscribe function and runs until ctx is canceled.
func (n *Notifier) Watch(ctx context.Context, mgr *Manager) func() {
	return watchStateChanges(ctx, mgr, func(evt SessionStateChangeEvent) {
		// Only real transitions. A re-adoption event repeats a status the
		// parent has already seen and is not news.
		if evt.OldStatus == evt.NewStatus {
			return
		}
		child := evt.Info
		if child.ID == "" {
			child, _ = n.target.SessionInfo(evt.SessionID)
		}
		n.NotifyParent(ctx, child)
	})
}

// paneSaysWorking asks the recipient's pane whether a turn is running.
func paneSaysWorking(provider string, lines []string) (working, known bool) {
	if len(lines) == 0 || !providerHasIdleProbe(provider) {
		return false, false
	}
	return paneShowsWorking(provider, lines)
}

// providerForSession resolves which agent CLI backs a session. The nil registry
// is intentional: explicit Backend values short-circuit, and installed-provider
// filtering would not change the Provider returned here.
func providerForSession(info SessionInfo) string {
	agentModel, err := resolveAgentModel(info.Model, info.Backend, nil)
	if err != nil {
		return ""
	}
	return agentModel.Provider
}

// parentSessionID reads the session's parent under the same lock as other
// session fields, even though the value is set once.
func (s *Session) parentSessionID() SessionID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ParentSessionID
}

// resultDirName is the ~/.bramble/ subdirectory for parent-readable result
// files. Keep it under $HOME, not os.TempDir: result files may be read hours
// later, and a world-writable temp dir cannot be secured against pre-created
// directories or symlinks from inside this process.
const resultDirName = "research"

// DefaultResultDir returns ~/.bramble/research.
func DefaultResultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home for result dir: %w", err)
	}
	return filepath.Join(home, ".bramble", resultDirName), nil
}

// ResultFilePath returns the result path under dir, creating the directory. An
// empty dir uses DefaultResultDir; tests pass their own directory.
func ResultFilePath(dir string, id SessionID) (string, error) {
	if dir == "" {
		var err error
		dir, err = DefaultResultDir()
		if err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create result dir: %w", err)
	}
	return containedPath(dir, sanitizeFileName(string(id))+".md")
}
