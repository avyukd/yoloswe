package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// PaneWriter is the tmux controller surface the courier needs. It lives on the
// consumer side because tmuxctl already imports session.
type PaneWriter interface {
	Paste(ctx context.Context, target, text string) error
	SendEnter(ctx context.Context, target string) error
}

// Delivery is one queued message waiting for its recipient to become idle.
type Delivery struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
	// From is the sender, when there is one. Empty for a message sent from a
	// plain shell rather than from another session.
	From   SessionID `json:"from,omitempty"`
	To     SessionID `json:"to"`
	Text   string    `json:"text"`
	Submit bool      `json:"submit"`
}

// DeliveryTarget is the session registry surface the courier needs.
type DeliveryTarget interface {
	SessionInfo(id SessionID) (SessionInfo, bool)
	SendFollowUp(id SessionID, message string) error
	ResolveTmuxTarget(id SessionID) (string, error)
	// CapturePaneText reads a tmux session's scrollback; tmux-mode subagents
	// have no TUI transcript, so the pane is their result record.
	CapturePaneText(id SessionID, n int) ([]string, error)
	// MarkRunning records that a turn has started. Only bramble knows this for
	// a tmux session it just typed into; see Manager.SetSessionRunning.
	MarkRunning(id SessionID)
}

// registryTarget adapts a *SessionRegistry to DeliveryTarget.
type registryTarget struct{ reg *SessionRegistry }

// NewRegistryDeliveryTarget wraps a registry so it can drive a Courier.
func NewRegistryDeliveryTarget(reg *SessionRegistry) DeliveryTarget {
	return &registryTarget{reg: reg}
}

func (t *registryTarget) SessionInfo(id SessionID) (SessionInfo, bool) {
	info, _, ok := t.reg.GetSessionInfo(id)
	return info, ok
}

func (t *registryTarget) SendFollowUp(id SessionID, message string) error {
	_, mgr, ok := t.reg.GetSessionInfo(id)
	if !ok || mgr == nil {
		return fmt.Errorf("session not found: %s", id)
	}
	return mgr.SendFollowUp(id, message)
}

func (t *registryTarget) ResolveTmuxTarget(id SessionID) (string, error) {
	return t.reg.ResolveTmuxTarget(id)
}

func (t *registryTarget) CapturePaneText(id SessionID, n int) ([]string, error) {
	return t.reg.CapturePaneText(id, n)
}

func (t *registryTarget) MarkRunning(id SessionID) { t.reg.SetSessionRunning(id) }

// Courier delivers text into a session regardless of runner type, queueing while
// the recipient is mid-turn. Queued delivery is durable, per-recipient ordered,
// and written only when the recipient is genuinely ready.
//
// The write paths intentionally converge here: TUI follow-ups can refuse a
// busy session, while tmux paste always reports success even when the recipient
// is not ready for the text.
type Courier struct { //nolint:govet // fieldalignment: grouping by role reads better
	target    DeliveryTarget
	panes     PaneWriter
	dir       string
	resultDir string
	mu        sync.Mutex
	pending   map[SessionID][]Delivery
	// reported remembers which (child, status) pairs have already been
	// reported to a parent, so a child is not announced twice.
	reported map[SessionID]map[SessionStatus]bool
	// writing holds recipients with a write in flight. Direct sends and drains
	// both claim here, because c.mu is not held while a pane write runs and a
	// second writer would land mid-turn.
	writing map[SessionID]bool
	// retryArmed records that a failed delivery has a retry scheduled. Only
	// read by tests, which would otherwise have to wait out retryDelay.
	retryArmed bool
	// heldForDraft records the draft currently holding a delivery and when this
	// exact text was first seen.
	heldForDraft map[SessionID]draftHold
	// staged records text this courier pasted but did not submit. It is the
	// only evidence that a non-empty composer holds our delivery rather than a
	// human draft; the pane cannot prove provenance.
	// Keep this narrow: a stale staged record is worse than none because it can
	// authorize pressing Enter on text the user supplied later.
	staged map[SessionID]string
	// heldForPane records when this exact pane content first read as working.
	// The hold is bounded only while the pane stays static; see notePaneHold.
	heldForPane map[SessionID]paneHold
	// reportedBlocked remembers, per recipient, the composer text an operator
	// has already been warned about, so one standing block is reported once
	// rather than on every retry. See noteBlockedReport.
	reportedBlocked map[SessionID]string
	// reportedUnlanded remembers, per recipient, the pane an operator has
	// already been warned about after a paste failed to land, so a prompt that
	// keeps swallowing pastes is reported once rather than every retryDelay.
	// See noteUnlandedReport.
	reportedUnlanded map[SessionID]string
	// reportedUnverifiable is the same dedup for the opposite outcome: a pane
	// bramble could not read, submitted into anyway. Separate from
	// reportedUnlanded because that record is released the moment a delivery
	// submits, which is exactly when this one must survive.
	reportedUnverifiable map[SessionID]string
	// now is injectable so tests assert elapsed time rather than call count.
	now func() time.Time
	seq uint64
}

// CourierConfig holds the filesystem locations used by a Courier.
type CourierConfig struct {
	// DeliveryDir persists queued deliveries. Empty defaults to
	// ~/.bramble/deliveries.
	DeliveryDir string
	// ResultDir holds subagent result files. Empty defaults to
	// ~/.bramble/research.
	ResultDir string
}

// NewCourier creates a courier and loads any existing queue from disk.
func NewCourier(target DeliveryTarget, panes PaneWriter, config CourierConfig) (*Courier, error) {
	deliveryDir, resultDir, err := resolveCourierDirs(config)
	if err != nil {
		return nil, err
	}
	// 0700: queue files hold message text meant for one session's operator.
	if err := os.MkdirAll(deliveryDir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create delivery dir %s: %w", deliveryDir, err)
	}
	if err := os.MkdirAll(resultDir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create result dir %s: %w", resultDir, err)
	}
	c := &Courier{
		target:               target,
		panes:                panes,
		dir:                  deliveryDir,
		resultDir:            resultDir,
		pending:              make(map[SessionID][]Delivery),
		heldForDraft:         make(map[SessionID]draftHold),
		staged:               make(map[SessionID]string),
		heldForPane:          make(map[SessionID]paneHold),
		reportedBlocked:      make(map[SessionID]string),
		reportedUnlanded:     make(map[SessionID]string),
		reportedUnverifiable: make(map[SessionID]string),
		now:                  time.Now,
		reported:             make(map[SessionID]map[SessionStatus]bool),
		writing:              make(map[SessionID]bool),
	}
	if err := c.load(); err != nil {
		return nil, err
	}
	return c, nil
}

func resolveCourierDirs(config CourierConfig) (string, string, error) {
	deliveryDir := config.DeliveryDir
	resultDir := config.ResultDir
	if deliveryDir != "" && resultDir != "" {
		return deliveryDir, resultDir, nil
	}

	if deliveryDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", fmt.Errorf("failed to get home directory: %w", err)
		}
		deliveryDir = filepath.Join(home, ".bramble", "deliveries")
	}
	if resultDir == "" {
		var err error
		resultDir, err = DefaultResultDir()
		if err != nil {
			return "", "", err
		}
	}
	return deliveryDir, resultDir, nil
}

// Send delivers text to a session, writing it now if the recipient is idle and
// queueing it otherwise. Terminal recipients are refused; they can never drain.
func (c *Courier) Send(ctx context.Context, from, to SessionID, text string, submit bool) (queued bool, err error) {
	// A child speaking to its own parent replaces the courier's generated report.
	if from != "" {
		if sender, ok := c.target.SessionInfo(from); ok && sender.ParentSessionID == to {
			c.noteChildSpoke(from)
		}
	}
	return c.deliver(ctx, from, to, text, submit)
}

// deliver is Send without the child-spoke bookkeeping. A failed write queues and
// arms a retry because an already-idle recipient may never emit another idle
// transition for the message to ride.
func (c *Courier) deliver(ctx context.Context, from, to SessionID, text string, submit bool) (queued bool, err error) {
	info, ok := c.target.SessionInfo(to)
	if !ok {
		return false, fmt.Errorf("session not found: %s", to)
	}
	if info.Status.IsTerminal() {
		return false, fmt.Errorf("session %s is %s and cannot receive messages", to, info.Status)
	}

	// Re-read under the write claim; another caller may have started a turn
	// after the first status read.
	writeFailed := false
	if info.Status == StatusIdle && c.claimWrite(to) {
		defer c.releaseWrite(to)
		if fresh, ok := c.target.SessionInfo(to); ok && fresh.Status == StatusIdle {
			err := c.write(ctx, fresh, text, submit)
			if err == nil {
				return false, nil
			}
			logWriteFailure("failed to write delivery, queueing it instead", to, err)
			writeFailed = true
		}
	}
	if err := c.enqueue(from, to, text, submit); err != nil {
		return false, err
	}
	if writeFailed {
		// Retry after releasing the current write claim.
		c.retryLater(ctx, to)
	}
	return true, nil
}

// enqueue appends a delivery and persists it. "Queued" promises restart
// survival, so a persist failure rolls memory back and is returned.
func (c *Courier) enqueue(from, to SessionID, text string, submit bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.seq++
	d := Delivery{
		ID:        fmt.Sprintf("%d-%d", time.Now().UnixNano(), c.seq),
		From:      from,
		To:        to,
		Text:      text,
		Submit:    submit,
		CreatedAt: time.Now(),
	}
	before := c.pending[to]
	c.pending[to] = append(append([]Delivery(nil), before...), d)
	if err := c.persistLocked(to); err != nil {
		if len(before) == 0 {
			delete(c.pending, to)
		} else {
			c.pending[to] = before
		}
		logDeliveryWarn("failed to persist delivery queue", to, err)
		return fmt.Errorf("queue delivery for %s: %w", to, err)
	}
	return nil
}

// Pending returns a copy of the queue for a recipient, oldest first.
func (c *Courier) Pending(to SessionID) []Delivery {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Delivery(nil), c.pending[to]...)
}

// composerHoldsThisDelivery reports whether the composer holds this delivery.
//
// The load-bearing question is provenance: a paste chip looks identical whether
// bramble or a user pasted it, so pane appearance alone cannot justify pressing
// Enter. Only staged can vouch that bramble put this exact text there; otherwise
// the composer is a human draft unless it exactly equals the delivery's first
// line.
//
// Match the message text, never the "[bramble]" prefix: the prefix is
// user-controllable and plain queued messages have none. Captures may truncate
// and composers wrap, so a staged delivery permits a one-way prefix match where
// the visible body is a prefix of the first delivery line. The reverse would
// submit user edits appended to bramble's text.
//
// This asks a narrower question than "did bramble stage something here". A
// composer holding different text is still protected; tmux paste-buffer would
// append the delivery to it and submit both as one prompt.
func composerHoldsThisDelivery(provider, composer, staged, text string) bool {
	// Only providers with a known composer format can answer provenance.
	if !composerReadable(provider) {
		return false
	}
	body, _ := composerBody(composer)
	if body == "" {
		return false
	}
	// pasteFirstLine, not an inline Cut: this is the same operand confirmsComposer,
	// composerHoldsForeignText and pasteVerdict all compare against. Deriving it
	// twice lets the draft check and the verify check disagree about what "our
	// text" looks like, and that disagreement is what flip-flops a delivery
	// between alreadyStaged and foreign until the queue wedges.
	first := pasteFirstLine(text)
	if first == "" {
		return false
	}
	// staged proves only what bramble pasted then, not what is visible now.
	// The visible body must still be a truncation of this delivery's first line.
	if staged != "" && staged == text {
		return strings.HasPrefix(first, body)
	}
	// Without staged provenance, only an exact first-line match can be ours.
	return body == first
}

// noteBlockedReport reports once per distinct blocking draft. Without this, a
// draft that sits past composerHoldGrace logs once per retry.
//
// Presence, not value: every dedup helper here must test `seen && prev == key`
// rather than `c.m[to] == key`. An empty key is a real key — an unreadable
// composer and a pane no capture succeeded on both produce one — and comparing
// against the map's zero value makes that case indistinguishable from "never
// reported", swallowing the FIRST report. In each of these the swallowed line is
// the only signal the operator gets, and the emptiest key is the most degraded
// pane, so the collision silences exactly the worst case. Same distinction
// composerHoldsThisDelivery already draws for staged provenance.
func (c *Courier) noteBlockedReport(to SessionID, composer string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if prev, seen := c.reportedBlocked[to]; seen && prev == composer {
		return false
	}
	c.reportedBlocked[to] = composer
	return true
}

// noteUnlandedReport reports once per distinct pane that swallowed a paste.
// Without it, a prompt that never accepts the text warns every retryDelay for
// the life of the process, exactly as noteBlockedReport prevents for drafts.
//
// Keyed on the pane's fingerprint rather than a bare bool so a pane that
// changes — a new screen, a different turn — is worth reporting again.
func (c *Courier) noteUnlandedReport(to SessionID, fingerprint string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if prev, seen := c.reportedUnlanded[to]; seen && prev == fingerprint {
		return false
	}
	c.reportedUnlanded[to] = fingerprint
	return true
}

// clearUnlandedReport forgets the reported pane once a paste lands or the
// recipient goes away. Without it "once per stuck run" silently becomes "once
// per process", and the next genuine stall would never be reported.
func (c *Courier) clearUnlandedReport(to SessionID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.reportedUnlanded, to)
}

// noteUnverifiableReport reports once per distinct unreadable pane submitted
// into. Kept separate from noteUnlandedReport because that record is cleared on
// every submit, and this warning describes a submit.
func (c *Courier) noteUnverifiableReport(to SessionID, fingerprint string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if prev, seen := c.reportedUnverifiable[to]; seen && prev == fingerprint {
		return false
	}
	c.reportedUnverifiable[to] = fingerprint
	return true
}

// clearUnverifiableReport forgets the reported pane once one becomes readable
// again. Without it "once per unreadable run" becomes "once per process", and
// every later delivery into that pane is lost in silence — worse than the stall
// case, which at least keeps its delivery queued.
//
// Deliberately NOT called beside clearUnlandedReport: the !v.readable arm falls
// through to that line, so releasing there would erase the record it just set
// and warn on every delivery instead of once. The condition ends where the pane
// proves readable, which is the landed arm and the alreadyStaged path.
func (c *Courier) clearUnverifiableReport(to SessionID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.reportedUnverifiable, to)
}

// composerHoldGrace is wall-clock time, not a retry count: Drain has multiple
// callers, so attempts measure how often bramble looked, not how long the draft
// has been sitting there.
const composerHoldGrace = 5 * time.Minute

// draftHold is one recipient's current composer hold.
type draftHold struct {
	// firstSeen is when this exact text was first observed.
	firstSeen time.Time
	// text is the draft holding the delivery; any change restarts the hold.
	text string
}

// noteDraftHold reports whether one unchanged draft has outlived
// composerHoldGrace. A changed draft restarts the clock so active typing is
// waited on, not raced.
//
// The elapsed-time bound matters because wrapped composer content can make every
// keystroke look like a fresh pane update; counting Drain calls would expire an
// actively edited draft.
func (c *Courier) noteDraftHold(to SessionID, draft string) (expired bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	held, ok := c.heldForDraft[to]
	if !ok || held.text != draft {
		c.heldForDraft[to] = draftHold{text: draft, firstSeen: c.now()}
		return false
	}
	return c.now().Sub(held.firstSeen) >= composerHoldGrace
}

// paneHold is one recipient's current working-verdict hold.
type paneHold struct {
	// firstSeen is when the pane was first seen working with this content.
	firstSeen time.Time
	// fingerprint is that content; any repaint restarts the hold.
	fingerprint string
}

// paneHoldGrace bounds only static "working" verdicts. A real turn repaints, so
// notePaneHold restarts this clock whenever the pane content changes; what
// expires is a stale false positive that would otherwise strand queued mail.
// It is longer than composerHoldGrace because working panes clear the hold by
// repainting, while abandoned drafts may never clear themselves.
//
// Keep this tied to composerHoldGrace: both are escape hatches from indefinite
// holds, but only a working pane has a built-in sign of life.
const paneHoldGrace = 15 * time.Minute

// notePaneHold reports whether one unchanged working verdict has outlived
// paneHoldGrace.
//
// Wall clock, not frame count, for the same reason noteDraftHold uses elapsed
// time: Drain can be called by retries, idle transitions, sweeps, and direct
// sends.
func (c *Courier) notePaneHold(to SessionID, pane []string) (expired bool) {
	fingerprint := paneFingerprint(pane)
	c.mu.Lock()
	defer c.mu.Unlock()
	held, ok := c.heldForPane[to]
	if !ok || held.fingerprint != fingerprint {
		// A repaint means the verdict is live, so restart the stale-frame clock.
		c.heldForPane[to] = paneHold{fingerprint: fingerprint, firstSeen: c.now()}
		return false
	}
	return c.now().Sub(held.firstSeen) >= paneHoldGrace
}

// paneFingerprint distinguishes one captured frame from the next.
func paneFingerprint(lines []string) string {
	return strings.Join(lines, "\n")
}

// clearPaneHold ends the current run of working verdicts.
func (c *Courier) clearPaneHold(to SessionID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.heldForPane, to)
}

// noteStaged records an unsubmitted paste; see composerHoldsThisDelivery for
// why the pane cannot prove provenance.
func (c *Courier) noteStaged(to SessionID, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.staged[to] = text
}

// stagedText reports what this courier last pasted to a recipient without
// submitting it, or "" if nothing is outstanding.
func (c *Courier) stagedText(to SessionID) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.staged[to]
}

// clearStaged forgets an unsubmitted paste. The record may outlive only an
// attempt that actually left text in the composer and could not submit it.
//
// The record still does not prove the composer holds that text now; retries must
// read the pane again before using staged as provenance.
func (c *Courier) clearStaged(to SessionID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.staged, to)
}

// clearDraftHold ends one standing composer block. The block report goes with
// it so the same text can be reported again if it blocks a later delivery.
// Otherwise "warn once per blocking draft" becomes "warn once per session text
// for the process lifetime".
func (c *Courier) clearDraftHold(to SessionID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.heldForDraft, to)
	delete(c.reportedBlocked, to)
}

// retryDelay is how long a failed delivery waits before it is tried again.
// Long enough that a recipient stuck behind a modal is not hammered, short
// enough that a transient tmux error costs one pause rather than a turn.
const retryDelay = 30 * time.Second

// retryLater schedules one more drain for a recipient whose write failed. There
// is no ticker; each failed attempt arms one low-rate retry.
func (c *Courier) retryLater(ctx context.Context, to SessionID) {
	c.mu.Lock()
	c.retryArmed = true
	c.mu.Unlock()

	// Release the cancellation hook when the timer fires. A hold is steady
	// state, not a rare failure: errComposerBusy and errPaneBusy each arm one
	// retry per retryDelay for the whole life of the hold, and a composer hold
	// has no built-in end. Discarding the stop func would leave one registration
	// per retry on the courier's process-lifetime context, freed only at
	// shutdown.
	//
	// Ordering: the timer goroutine and this one both touch stopHook, and the
	// timer may fire before context.AfterFunc has returned, so the handoff goes
	// through a channel rather than a bare variable. The buffered send never
	// blocks this goroutine, and the receive takes the zero value only if the
	// send has not happened yet — in which case there is no hook to release.
	stopHook := make(chan func() bool, 1)
	timer := time.AfterFunc(retryDelay, func() {
		select {
		case stop := <-stopHook:
			stop()
		default:
		}
		if ctx.Err() != nil {
			return
		}
		c.Drain(ctx, to)
	})
	// AfterFunc runs the hook immediately when ctx is already cancelled, so this
	// still stops a timer armed during shutdown.
	stopHook <- context.AfterFunc(ctx, func() { timer.Stop() })
}

// claimWrite reserves the right to write to a recipient, reporting whether it
// got it. A caller that does not must not write: it queues instead.
func (c *Courier) claimWrite(to SessionID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writing[to] {
		return false
	}
	c.writing[to] = true
	return true
}

func (c *Courier) releaseWrite(to SessionID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.writing, to)
}

// Drain writes at most one queued delivery. A write starts the recipient's next
// turn, so the rest must ride later idle transitions. Failed writes stay queued
// and arm a retry because an already-idle recipient may never transition again.
// Terminal recipients are discarded because they can never drain.
//
// Draining the whole queue would recreate the bug this type avoids: later
// messages would either be refused by TUI SendFollowUp or pasted into a tmux pane
// whose newly started turn is already in progress.
func (c *Courier) Drain(ctx context.Context, to SessionID) {
	c.mu.Lock()
	queue := c.pending[to]
	if len(queue) == 0 {
		c.mu.Unlock()
		return
	}
	next := queue[0]
	c.mu.Unlock()

	if !c.claimWrite(to) {
		// Another write is already in flight; that write will leave the
		// recipient running, and this queue rides the next idle transition.
		return
	}
	defer c.releaseWrite(to)

	info, ok := c.target.SessionInfo(to)
	if !ok {
		// Not "gone" -- just not visible from here yet. Dropping on lookup
		// failure would delete persisted queues during restart before
		// reconciliation can re-adopt sessions.
		return
	}
	if info.Status.IsTerminal() {
		c.discard(to)
		return
	}
	if info.Status != StatusIdle {
		return
	}

	if err := c.write(ctx, info, next.Text, next.Submit); err != nil {
		logWriteFailure("failed to write queued delivery", to, err)
		c.retryLater(ctx, to)
		return
	}

	c.mu.Lock()
	if remaining := c.pending[to]; len(remaining) <= 1 {
		delete(c.pending, to)
	} else {
		// Pending already returns a copy; reslicing keeps fan-in drains linear.
		c.pending[to] = remaining[1:]
	}
	err := c.persistLocked(to)
	c.mu.Unlock()

	if err != nil {
		// Delivery is deliberately at-least-once: a duplicate report is noise,
		// while a dropped report can leave a parent waiting forever.
		logDeliveryWarn("delivery written but queue not persisted (may be redelivered after a restart)", to, err)
	}
}

// write puts text into the session using whichever path its runner supports.
func (c *Courier) write(ctx context.Context, info SessionInfo, text string, submit bool) (err error) {
	// Anything written starts the recipient's next turn, so whatever it says at
	// the end of that turn is fresh news for its parent.
	//
	// Only a write that succeeded, though. Every error path reaches here having
	// started no turn, so the recipient's next idle is the SAME turn the parent
	// was already told about; re-arming there sends a duplicate report, which
	// for a tmux child also costs another 2000-line pane capture and another
	// result file.
	//
	// A hold is the common shape of that — errPaneBusy answers every turn
	// bramble's bookkeeping calls idle while the pane still shows work, and
	// errComposerBusy repeats every retryDelay for as long as a user's draft
	// sits in the composer — but an outright failure is the one that mattered
	// in issues #330 and #331: a message sent to a child whose tmux window was
	// killed fails, and re-arming on it let the next stale idle event for that
	// reaped session be reported to the parent as a fresh answer.
	defer func() {
		if err == nil {
			c.resetIdleReport(info.ID)
		}
	}()

	switch info.RunnerType {
	case RunnerTypeTUI:
		// The TUI turn loop delivers a follow-up as a real prompt; Submit is meaningless here.
		return c.target.SendFollowUp(info.ID, text)
	case RunnerTypeTmux, RunnerTypeTmuxTracked:
		if c.panes == nil {
			return fmt.Errorf("no tmux writer configured")
		}
		target, err := c.target.ResolveTmuxTarget(info.ID)
		if err != nil {
			return err
		}
		provider := providerForSession(info)
		// Never write into a pane with a positive working verdict: bramble's
		// bookkeeping can say idle before the CLI prompt is ready, and codex can
		// drop a paste in that gap. Unknown panes fail open, because treating an
		// unreadable pane as busy would strand mail.
		//
		// That fail-open choice is deliberate only for unknowns. A known-working
		// pane holds the delivery, because writing there loses the message and can
		// mark the session running for a turn that never started.
		//
		// Capture once so the working check and composer check answer against
		// the same frame and do not widen the paste-to-Enter window.
		paneLines := c.capturePaneFor(info.ID, provider)
		if working, known := paneSaysWorking(provider, paneLines); known && working {
			// Nothing was written, so an old staged record cannot vouch for the
			// composer after the turn changes it.
			c.clearStaged(info.ID)
			if !c.notePaneHold(info.ID, paneLines) {
				return errPaneBusy
			}
			// A static "working" pane past paneHoldGrace is a stuck verdict, not
			// a long turn. Deliver anyway and log the bounded risk.
			// False positives include interrupted tool lines and spinner-shaped
			// echoed text; because the pane is static, waiting longer will not
			// produce a different verdict.
			logDeliveryWarn("pane has read as working past the grace period; delivering anyway",
				info.ID, errPaneBusy)
		}
		c.clearPaneHold(info.ID)
		// Never write into a human draft: tmux paste-buffer appends, and Enter
		// would submit the draft and this message as one prompt.
		alreadyStaged := false
		if composerText, draft, known := composerDraftText(provider, paneLines); known && draft {
			switch {
			case composerHoldsThisDelivery(provider, composerText, c.stagedText(info.ID), text):
				// This delivery is already staged. Do not paste again; tmux
				// would append a second copy and submit both as one prompt.
				// This branch is also not a stale-draft warning: no human draft was
				// held, and the composer matched this delivery.
				slog.Debug("delivery is already staged in the composer; submitting without re-pasting",
					"session", info.ID)
				alreadyStaged = true
				// The composer was read and matched this delivery, so the pane is
				// demonstrably readable again. This path skips verification
				// entirely, so it is the other place the unreadable run ends.
				c.clearUnverifiableReport(info.ID)
			case !c.noteDraftHold(info.ID, composerText):
				// A human draft is in the way; any staged record is no longer ours.
				c.clearStaged(info.ID)
				return errComposerBusy
			default:
				// The unchanged draft has outlived composerHoldGrace, but fail
				// closed: pasting would append to it, and submitting a chip could
				// press Enter on a user's text. Report once and keep the delivery
				// queued until the composer clears.
				// maxDeliveryAge only prunes at process start and deletes instead
				// of delivering, so nothing here can safely retire the delivery.
				if c.noteBlockedReport(info.ID, composerText) {
					logDeliveryWarn("composer has held unchanged text past the grace period; delivery stays queued until the composer clears",
						info.ID, errComposerBusy)
				}
				return errComposerBusy
			}
		}
		c.clearDraftHold(info.ID)
		if !alreadyStaged {
			if err := c.panes.Paste(ctx, target, text); err != nil {
				// Nothing was staged, so no record may survive.
				c.clearStaged(info.ID)
				return err
			}
			// Record before any later failure; the retry has no other provenance
			// for text left in the composer.
			c.noteStaged(info.ID, text)
		}
		// Confirm required providers before pressing Enter: codex can report
		// idle before its prompt can accept a paste. But silence is not a
		// negative; re-pasting when the pane is unreadable appends duplicate
		// copies and never submits them. alreadyStaged is stronger than this
		// probe because the composer was read and matched to this delivery.
		//
		// Check pasteVerifyRequired before probing. Providers whose verdict is
		// ignored should not pay the sleeps and capture round trips, and probing
		// widens the window between the draft check and SendEnter.
		if !alreadyStaged && pasteVerifyRequired(provider) {
			v := c.pasteVerdict(ctx, info.ID, provider, text)
			switch {
			case v.landed:
				// Confirmed in the composer. The pane just proved it can be
				// read, so any unreadable-pane warning is over.
				c.clearUnverifiableReport(info.ID)
			case !v.readable:
				// Unreadable is silence, not a negative. Trust tmux paste-buffer;
				// re-pasting here is the duplicate-copy loop.
				//
				// Warn rather than Debug, because this is the ONE arm that can
				// lose a delivery. Submitting dequeues it permanently, so if the
				// pane was an alt-screen pager or an exited CLI, the paste went
				// nowhere, Enter went to something else, and a parent waiting on
				// that report waits forever — the exact outcome the at-least-once
				// comment in Drain names. Every other arm either lands the text or
				// keeps it queued. Deduped per frame by reportedUnverifiable,
				// which is separate from the stall record precisely because that
				// one is released on every submit and this describes a submit.
				if c.noteUnverifiableReport(info.ID, v.fingerprint) {
					logDeliveryWarn("submitting into a pane bramble cannot read; if the paste did not land the delivery is lost",
						info.ID, errPasteUnverifiable)
				}
			case v.foreign:
				// Someone else's text is in the composer, so this is not a
				// dropped paste and a re-paste would append a second copy to
				// their unsent line. The usual cause is a user who started
				// typing after the draft check and before this verdict; the
				// window is the verification budget, up to ~1.8s wide.
				//
				// Fail closed exactly as the draft check does: drop provenance,
				// since a composer we do not own cannot be our staged record,
				// and keep the delivery queued for a later idle composer.
				// Pressing Enter is equally unsafe — it would submit their line
				// with our text riding on it.
				c.clearStaged(info.ID)
				return errComposerBusy
			default:
				// Readable, absent, and the composer is empty: a real dropped
				// paste. Retry once, then queue.
				if err := c.panes.Paste(ctx, target, text); err != nil {
					c.clearStaged(info.ID)
					return err
				}
				c.noteStaged(info.ID, text)
				retry := c.pasteVerdict(ctx, info.ID, provider, text)
				if retry.foreign {
					// Interference arrived during the retry. Same rule.
					c.clearStaged(info.ID)
					return errComposerBusy
				}
				if !retry.landed && retry.readable {
					// Erroring keeps the delivery queued. Report once per pane:
					// the retry timer fires every retryDelay for as long as the
					// prompt keeps refusing, and warning each time buries the
					// signal it exists to give.
					if c.noteUnlandedReport(info.ID, retry.fingerprint) {
						logDeliveryWarn("paste did not reach the prompt; delivery stays queued until it is accepted",
							info.ID, errPasteUnlanded)
					}
					return errPasteUnlanded
				}
			}
			// Every arm that reaches here goes on to submit: the paste landed,
			// the pane went silent, or the re-paste repaired it. The arms that
			// leave the recipient stalled have already returned.
		}
		// Outside the verification block on purpose. The likeliest way a stall
		// ends is the paste finally landing, which the NEXT attempt sees as
		// alreadyStaged — and that path skips verification entirely. Releasing
		// only inside it would turn "once per stall" into "once per session for
		// the life of the process", the failure clearUnlandedReport exists to
		// prevent. Every arm still reaching here goes on to submit; the ones
		// that leave the recipient stalled returned above.
		c.clearUnlandedReport(info.ID)
		// Only an unsubmitted staged record must be recognizable on retry.
		if !submit && !alreadyStaged && !c.pasteIsReadableAsText(ctx, info.ID, provider, text) {
			// A paste chip cannot be matched as text on retry. Drop provenance
			// so the next attempt fails closed on an unidentified draft.
			// Keeping a record no comparison can use would let the retry paste on
			// top of its own chip and submit both.
			c.clearStaged(info.ID)
		}
		if !submit {
			// Leave the staged record for the unsubmitted text in the composer.
			return nil
		}
		if err := c.panes.SendEnter(ctx, target); err != nil {
			// Keep provenance only when retry can recognize the text.
			if !alreadyStaged && !c.pasteIsReadableAsText(ctx, info.ID, provider, text) {
				c.clearStaged(info.ID)
			}
			return err
		}
		// Submitted: the composer is empty, so provenance must not survive.
		c.clearStaged(info.ID)
		// Do not read back Enter: submitted prompts echo near the composer, so a
		// scrape cannot distinguish pending from submitted. Mark running because
		// a submitted prompt started a turn.
		c.target.MarkRunning(info.ID)
		return nil
	case "":
		// The runner type is only set once runSession picks one. A session
		// still pending has no way to receive anything yet.
		return fmt.Errorf("session %s has not started yet", info.ID)
	default:
		return fmt.Errorf("session %s has unknown runner type %q", info.ID, info.RunnerType)
	}
}

// errComposerBusy keeps a delivery queued while a human draft is in the way.
var errComposerBusy = errors.New("composer holds an unsubmitted draft")

// errPaneBusy keeps a delivery queued while the recipient pane shows work in flight.
var errPaneBusy = errors.New("pane shows a turn still in flight")

// errPasteUnlanded keeps a delivery queued when a readable prompt did not take
// the paste, even after the one re-paste that repairs a dropped one.
//
// A sentinel, not a bare fmt.Errorf, so logWriteFailure can recognize it and
// leave the reporting to noteUnlandedReport's dedup. Without that, the generic
// fall-through logged a second, undeduped warning per retry and cancelled the
// dedup out — the same trap the errComposerBusy arm documents.
var errPasteUnlanded = errors.New("paste did not reach the prompt")

// errPasteUnverifiable labels the one arm that submits without evidence: the
// pane could not testify either way, so bramble trusts tmux paste-buffer and
// presses Enter. It is never returned — the delivery proceeds — and exists only
// to give that warning a stable identity in the log.
var errPasteUnverifiable = errors.New("pane could not confirm the paste")

// paneSaysWorking asks the recipient's pane whether a turn is running. Unknown
// means fail open: deliver rather than strand mail on an unreadable pane.
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
		// Unknown chrome fails open: deliver, do not wedge.
		return ""
	}
	return agentModel.Provider
}

// capturePaneFor reads the recipient's pane once when any check can use it.
func (c *Courier) capturePaneFor(id SessionID, provider string) []string {
	// Avoid a tmux round-trip when every provider-keyed check would be unknown.
	if !providerHasIdleProbe(provider) && !composerReadable(provider) {
		return nil
	}
	lines, err := c.target.CapturePaneText(id, pasteVerifyLines)
	if err != nil {
		// Capture failure is unknown, not busy; fail open to avoid stranding mail.
		return nil
	}
	return lines
}

// pasteVerify bounds how long a paste is given to show up in the pane before
// it is treated as dropped. Short: this only covers a TUI finishing its
// previous turn, not real latency.
const (
	pasteVerifyAttempts = 12
	pasteVerifyInterval = 150 * time.Millisecond
	pasteVerifyLines    = 40
	pasteProbeLen       = 24
)

// pasteIsReadableAsText reports whether retry could recognize this paste as
// text. pasteVerdict may accept a chip as arrival evidence, but a chip cannot
// support staged provenance later.
func (c *Courier) pasteIsReadableAsText(ctx context.Context, id SessionID, provider, text string) bool {
	if ctx.Err() != nil {
		return false
	}
	probe := pasteProbe(text)
	if probe == "" {
		return false // nothing distinctive to look for later either
	}
	lines, err := c.target.CapturePaneText(id, pasteVerifyLines)
	if err != nil {
		return false
	}
	// Share pasteConfirmed's scan scope and its anchoring, but require text
	// rather than a chip: the composer shows the head of the first line, the
	// transcript tail echoes it whole.
	first := pasteFirstLine(text)
	return scanForPaste(provider, lines,
		func(line string) bool { return confirmsComposer(line, first, nil) },
		func(line string) bool { return strings.Contains(line, probe) })
}

// pasteVerdict reports whether the paste is visible, whether the pane was
// readable enough to make absence meaningful, and whether the composer holds
// text that is not ours. Silence is not a negative: when the pane cannot be
// captured or the composer cannot be located, re-pasting appends duplicate
// copies and never submits them. Only a readable pane that does not show the
// paste is a real negative.
//
// foreign splits that negative in two, because a re-paste is the right repair
// for only one of them. An empty composer is a dropped paste. A composer holding
// someone else's text is interference — a user typing inside the verification
// window, most often — and pasting there appends to their unsent line. The
// caller cannot re-derive this: by the time it sees (false, true) the capture is
// gone, so the shape is decided here where the lines are still in hand.
//
// The search is intentionally bounded to a probe, not the whole message. TUIs
// wrap and decorate long prompts, and some providers render a paste chip instead
// of echoing text; pasteConfirmed decides which evidence is acceptable.
func (c *Courier) pasteVerdict(ctx context.Context, id SessionID, provider, text string) (v pasteJudgement) {
	probe := pasteProbe(text)
	first := pasteFirstLine(text)
	if probe == "" {
		// Nothing distinctive to look for; do not block delivery.
		v.landed, v.readable = true, true
		return v
	}
	// One budget: only providers whose verdict is required reach here.
	for i := 0; i < pasteVerifyAttempts; i++ {
		// Wait before retries so a paste has a frame to paint.
		if i > 0 {
			select {
			case <-ctx.Done():
				return v
			case <-time.After(pasteVerifyInterval):
			}
		}
		lines, err := c.target.CapturePaneText(id, pasteVerifyLines)
		if err != nil {
			continue
		}
		if pasteConfirmed(provider, lines, first, probe) {
			v.landed, v.readable = true, true
			return v
		}
		// A located composer that lacks the text is a negative; an obscured
		// composer is silence, and the final capture decides readability. The
		// last capture decides foreign for the same reason: it is the one that
		// describes the pane the caller is about to write into.
		v.fingerprint = paneFingerprint(lines)
		v.readable = !pasteEvidenceObscured(provider, lines)
		v.foreign = composerHoldsForeignText(provider, lines, first)
	}
	return v
}

// pasteJudgement is what one verification budget concluded about a pane.
//
// fingerprint identifies the last capture judged, kept so a caller reporting a
// stall can tell one stuck frame from the next without holding the capture
// itself. See pasteVerdict for why the last capture is the authoritative one.
type pasteJudgement struct {
	fingerprint string
	landed      bool
	readable    bool
	foreign     bool
}

// pasteProbe picks the tail of the first line, not the head. Subagent reports
// share a long prefix, so a head-anchored probe can match an earlier echoed
// report and "confirm" a paste that was dropped. The tail is not collision-free,
// but it usually spans the varying session-id region; see
// TestCodexPaneVerdictIsBoundedByWhatBrambleCanSee. Keep it one bounded line
// because TUIs wrap and decorate long prompts.
//
// A positive probe is treated as "this paste arrived", so collisions are
// dangerous: the next SendEnter may land on an empty composer while MarkRunning
// records a turn that never started.
func pasteProbe(text string) string {
	first := pasteFirstLine(text)
	// Slice on a rune boundary. A byte cut can split a multi-byte rune, and the
	// resulting invalid UTF-8 matches nothing in the pane, so a paste that did
	// land reads as dropped.
	if r := []rune(first); len(r) > pasteProbeLen {
		first = string(r[len(r)-pasteProbeLen:])
	}
	return first
}

// pasteFirstLine is what a composer holding this delivery shows: composers render
// from the start of the text, so the head is what survives a narrow pane.
func pasteFirstLine(text string) string {
	first, _, _ := strings.Cut(text, "\n")
	return strings.TrimSpace(first)
}

// discard drops a recipient's queue and per-recipient courier state.
func (c *Courier) discard(to SessionID) {
	c.mu.Lock()
	// Release records before checking the queue. A terminal session can have no
	// pending mail but still own staged/hold state from an earlier attempt.
	// delete on absent keys is cheap; leaked records can affect a later session
	// with the same ID.
	delete(c.staged, to)
	delete(c.heldForPane, to)
	delete(c.heldForDraft, to)
	delete(c.reportedBlocked, to)
	delete(c.reportedUnlanded, to)
	delete(c.reportedUnverifiable, to)
	if _, queued := c.pending[to]; !queued {
		// No queue, so nothing on disk to unlink.
		c.mu.Unlock()
		return
	}
	delete(c.pending, to)
	err := c.persistLocked(to)
	c.mu.Unlock()
	if err != nil {
		logDeliveryWarn("failed to clear delivery queue", to, err)
	}
}

// DrainIdle delivers to recipients that were already idle when queues were
// loaded; Watch only sees later transitions.
func (c *Courier) DrainIdle(ctx context.Context) {
	c.mu.Lock()
	recipients := make([]SessionID, 0, len(c.pending))
	for to := range c.pending {
		recipients = append(recipients, to)
	}
	c.mu.Unlock()

	// Drain re-checks status; busy or missing recipients are no-ops here.
	for _, to := range recipients {
		c.Drain(ctx, to)
	}
}

// Watch drains a session's queue whenever it becomes idle. It returns an
// unsubscribe function and runs until ctx is canceled.
func (c *Courier) Watch(ctx context.Context, mgr *Manager) func() {
	// Reporting and draining are slow; watchStateChanges queues events so the
	// one transition a parent report rides is not dropped while this handler runs.
	// This callback can capture large panes, write result files, paste text, and
	// read it back; the state-change source must not use a lossy buffer here.
	return watchStateChanges(ctx, mgr, func(evt SessionStateChangeEvent) {
		// One transition can both drain queued mail and report child progress.
		// Prefer the event snapshot because completed tmux sessions can be
		// removed from the manager before this callback runs.
		child := evt.Info
		if child.ID == "" {
			child, _ = c.target.SessionInfo(evt.SessionID)
		}
		// Report only real transitions; re-adoption same-status events exist to
		// drain restored mail, not to re-announce child state after every restart.
		if evt.OldStatus != evt.NewStatus {
			c.reportToParent(ctx, child)
			// Re-arm reporting for turns the courier did not start.
			if evt.NewStatus == StatusRunning {
				c.resetIdleReport(evt.SessionID)
			}
		}
		switch {
		case evt.NewStatus == StatusIdle:
			c.Drain(ctx, evt.SessionID)
		case evt.NewStatus.IsTerminal():
			// Terminal sessions will never drain.
			c.discard(evt.SessionID)
			c.forgetChild(evt.SessionID)
		}
	})
}

// --- persistence -------------------------------------------------------------

// queuePath returns the on-disk file backing a recipient's queue.
func (c *Courier) queuePath(to SessionID) (string, error) {
	// Keep the path contained even if sanitization regresses.
	return containedPath(c.dir, sanitizeFileName(string(to))+".json")
}

// persistLocked writes the current queue to disk. The caller must hold c.mu so
// concurrent reports to the same parent cannot overwrite each other's queued
// state; otherwise the loss appears only after restart, where persistence matters.
//
// The trade is a small file write in the critical section, at the rate subagents
// finish turns.
func (c *Courier) persistLocked(to SessionID) error {
	path, err := c.queuePath(to)
	if err != nil {
		return err
	}
	queue := c.pending[to]
	if len(queue) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(queue, "", "  ")
	if err != nil {
		return err
	}
	// 0600: a queue file holds message text meant for one session's operator.
	return writeFileAtomic(path, data, 0o600)
}

// load reads every queue file back into memory.
func (c *Courier) load() error {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(c.dir, e.Name()))
		if err != nil {
			continue
		}
		var queue []Delivery
		if err := json.Unmarshal(data, &queue); err != nil || len(queue) == 0 {
			continue
		}
		sort.SliceStable(queue, func(i, j int) bool {
			return queue[i].CreatedAt.Before(queue[j].CreatedAt)
		})
		// Age out queues whose recipients vanished without a terminal transition.
		fresh := queue[:0]
		for _, d := range queue {
			if time.Since(d.CreatedAt) < maxDeliveryAge {
				fresh = append(fresh, d)
			}
		}
		if len(fresh) == 0 {
			// Reclaim the file too, or every start re-prunes it.
			_ = os.Remove(filepath.Join(c.dir, e.Name()))
			continue
		}
		c.pending[fresh[0].To] = fresh
	}
	return nil
}

// maxDeliveryAge is generous enough for restart downtime but eventually reclaims
// queues for sessions that vanished without a terminal transition.
const maxDeliveryAge = 7 * 24 * time.Hour

// logDeliveryWarn reports non-fatal courier problems from paths with no caller
// to return them to.
func logDeliveryWarn(msg string, to SessionID, err error) {
	slog.Warn(msg, "session", to, "error", err)
}

// quietHolds are the errors write() returns for a delivery that is waiting
// rather than failing, with the line each one logs at Debug.
//
// Debug, not warn, for one reason shared by all three: every one of them
// repeats every retryDelay for as long as the condition stands, so warning here
// is an endless line. Where an operator-visible signal is wanted, it comes from
// a deduped report at the point the condition is detected — noteBlockedReport
// for a draft, noteUnlandedReport for a refused paste — and a second undeduped
// line here would only cancel that dedup out.
var quietHolds = []struct {
	err error
	msg string
}{
	{errComposerBusy, "holding delivery: recipient has an unsubmitted draft"},
	{errPaneBusy, "holding delivery: recipient's pane shows a turn in flight"},
	{errPasteUnlanded, "holding delivery: the recipient's prompt did not take the paste"},
}

// logWriteFailure reports real write failures while classifying the holds above
// as queued waiting states.
func logWriteFailure(failMsg string, to SessionID, err error) {
	for _, h := range quietHolds {
		if errors.Is(err, h.err) {
			slog.Debug(h.msg, "session", to, "error", err)
			return
		}
	}
	logDeliveryWarn(failMsg, to, err)
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
//
// It sits beside the delivery queue and is part of the user-visible
// ~/.bramble/research/<id>.md path.
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
