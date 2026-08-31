package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeTarget is a DeliveryTarget backed by a map, so notifier behaviour can be
// driven through every status and runner type without live managers or tmux.
type fakeTarget struct { //nolint:govet // fieldalignment: readability over packing
	mu         sync.Mutex
	sessions   map[SessionID]SessionInfo
	tmuxTarget string
	captured   map[SessionID][]string
	captureErr error
	// captureDelay stands in for how long a real pane capture takes. It is what
	// makes the courier's event handling slow enough to test what happens to the
	// events arriving behind it.
	captureDelay  time.Duration
	captureCount  int
	markedRunning []SessionID
}

func newFakeTarget() *fakeTarget {
	return &fakeTarget{
		sessions:   make(map[SessionID]SessionInfo),
		captured:   make(map[SessionID][]string),
		tmuxTarget: "@7",
	}
}

func (f *fakeTarget) set(id SessionID, status SessionStatus, runner string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prev := f.sessions[id]
	prev.ID = id
	prev.Status = status
	prev.RunnerType = runner
	f.sessions[id] = prev
}

// setChild registers a session as a subagent of parent, keeping whatever
// result paths a test has already attached.
func (f *fakeTarget) setChild(id, parent SessionID, status SessionStatus, runner string) {
	f.set(id, status, runner)
	f.mu.Lock()
	defer f.mu.Unlock()
	info := f.sessions[id]
	info.ParentSessionID = parent
	f.sessions[id] = info
}

// annotate attaches result metadata to an existing session.
func (f *fakeTarget) annotate(id SessionID, fn func(*SessionInfo)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info := f.sessions[id]
	fn(&info)
	f.sessions[id] = info
}

// setBackend records which agent CLI backs a session. The yield checks are
// provider-specific — only claude has a readable composer — so a test has to
// say which CLI it means.
func (f *fakeTarget) setBackend(id SessionID, backend, model string) {
	f.annotate(id, func(i *SessionInfo) {
		i.Backend = backend
		i.Model = model
	})
}

func (f *fakeTarget) SessionInfo(id SessionID) (SessionInfo, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, ok := f.sessions[id]
	return info, ok
}

func (f *fakeTarget) ResolveTmuxTarget(id SessionID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tmuxTarget == "" {
		return "", fmt.Errorf("session %s is not a tmux session", id)
	}
	return f.tmuxTarget, nil
}

func (f *fakeTarget) CapturePaneText(id SessionID, _ int) ([]string, error) {
	f.mu.Lock()
	f.captureCount++
	delay := f.captureDelay
	err := f.captureErr
	lines := f.captured[id]
	f.mu.Unlock()
	if delay > 0 {
		// Outside the lock: this stands in for a real pane capture, which
		// shells out to tmux and holds nothing of the courier's.
		time.Sleep(delay)
	}
	if err != nil {
		return nil, err
	}
	return lines, nil
}

// appendPane mirrors text into every session's pane buffer. A shared buffer is
// enough and keeps the fake from needing to know which session a paste targeted.
// setPane REPLACES every session's pane buffer, for tests that need the pane to
// stop saying what it said before — appendPane only ever adds, so an earlier
// working marker would still be found in the tail.
func (f *fakeTarget) setPane(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id := range f.sessions {
		f.captured[id] = strings.Split(text, "\n")
	}
}

func (f *fakeTarget) appendPane(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id := range f.sessions {
		f.captured[id] = append(f.captured[id], strings.Split(text, "\n")...)
	}
}

func (f *fakeTarget) MarkRunning(id SessionID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info := f.sessions[id]
	if info.Status == StatusIdle {
		info.Status = StatusRunning
		f.sessions[id] = info
	}
	f.markedRunning = append(f.markedRunning, id)
}

// mustInfo returns a registered session, for tests that hand a SessionInfo
// straight to a notifier method.
func (f *fakeTarget) mustInfo(id SessionID) SessionInfo {
	info, _ := f.SessionInfo(id)
	return info
}

// fakePanes records tmux writes in order so a test can assert that a paste was
// followed by the Enter that submits it.
type fakePanes struct { //nolint:govet // fieldalignment: readability over packing
	mu       sync.Mutex
	writes   []string
	pasteErr error
	// echo, when set, mirrors a pasted line into the pane a test reads back,
	// standing in for a TUI that accepted the paste.
	echo func(string)
	// onSubmit, when set, stands in for a TUI accepting Enter: the composer
	// clears and the text moves up into the transcript.
	onSubmit func()
	// enterErr, when set, stands in for a tmux send-keys that failed: the text
	// is in the composer and no turn was started.
	enterErr error
}

func (p *fakePanes) Paste(_ context.Context, target, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pasteErr != nil {
		return p.pasteErr
	}
	p.writes = append(p.writes, "paste("+target+"): "+text)
	if p.echo != nil {
		p.echo(text)
	}
	return nil
}

func (p *fakePanes) SendEnter(_ context.Context, target string) error {
	p.mu.Lock()
	onSubmit := p.onSubmit
	enterErr := p.enterErr
	p.writes = append(p.writes, "enter("+target+")")
	p.mu.Unlock()
	if enterErr != nil {
		return enterErr
	}
	if onSubmit != nil {
		onSubmit()
	}
	return nil
}

// pasteCount reports how many pastes reached the pane. Counting the writes
// rather than the echoes: a test whose echo is unset still needs to know
// whether the notifier pasted once or twice.
func (p *fakePanes) pasteCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, w := range p.writes {
		if strings.HasPrefix(w, "paste(") {
			n++
		}
	}
	return n
}

func (p *fakePanes) recorded() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.writes...)
}

// echoPanes stands in for a CLI that echoes a pasted message into its composer.
// It writes real claude chrome because the composer check reads only a locatable
// composer, not a bare line.
func echoPanes(target *fakeTarget) *fakePanes {
	p := &fakePanes{}
	p.echo = func(text string) { target.appendPane(claudeComposerPane(text)) }
	// Submitting scrolls the composer contents up into the transcript, leaving
	// a fresh empty composer behind.
	p.onSubmit = func() { target.appendPane(claudeComposerPane("")) }
	return p
}

// claudeComposerPane renders a composer holding body as claude draws it.
func claudeComposerPane(body string) string {
	return strings.Join([]string{
		"────────────────────────────────────────────",
		"❯ " + body,
		"────────────────────────────────────────────",
		"  ~/wt/branch  main  Opus 4.6  ctx:43%  tokens:20k",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
	}, "\n")
}

// newTestNotifier builds a notifier whose legacy sweep points at a temp dir, so
// a test can never reclaim the developer's real ~/.bramble/deliveries.
func newTestNotifier(t *testing.T, target DeliveryTarget, panes PaneWriter) *Notifier {
	t.Helper()
	n, err := NewNotifier(target, panes, NotifierConfig{LegacyDeliveryDir: t.TempDir()})
	require.NoError(t, err)
	return n
}

// claudeChild registers an idle claude parent with a tmux child of its own and
// returns the child, ready to hand to NotifyParent.
func claudeChild(target *fakeTarget) SessionInfo {
	target.set("parent", StatusIdle, RunnerTypeTmux)
	target.setBackend("parent", "claude", "opus")
	target.setChild("child", "parent", StatusIdle, RunnerTypeTmux)
	target.setPane(claudeComposerPane(""))
	return target.mustInfo("child")
}

func TestAnIdleChildNudgesItsParentOnce(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	panes := echoPanes(target)

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), child)

	require.Equal(t, []string{
		"paste(@7): " + nudgeText,
		"enter(@7)",
	}, panes.recorded(), "a hint is one paste and one Enter, nothing else")
	require.Equal(t, []SessionID{"parent"}, target.markedRunning,
		"submitting a prompt starts a turn, which nothing else reports for a tmux session")
}

// The live failure this change exists to remove: three sessions on the author's
// machine were wedged for hours because a leftover draft blocked every delivery
// and the courier kept retrying. A hint must simply stay quiet.
func TestADraftInTheComposerSilencesTheNudge(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	target.setPane(claudeComposerPane("half a thought the user is still typing"))
	panes := echoPanes(target)

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), child)

	require.Empty(t, panes.recorded(),
		"a human draft must never be appended to, and there is nothing to queue")
	require.Empty(t, target.markedRunning, "no turn was started")
}

func TestAWorkingPaneSilencesTheNudge(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	// A spinner above the composer is claude's own "turn in flight" chrome. It
	// needs the full frame: the judge locates the composer first and refuses to
	// read a bare line it cannot place.
	target.setPane(strings.Join([]string{
		"● Read(delivery.go)",
		"────────────────────────────────────────────",
		"❯ ",
		"────────────────────────────────────────────",
		"  ~/wt/branch  main  Opus 4.6  ctx:43%  tokens:20k",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
	}, "\n"))
	panes := echoPanes(target)

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), child)

	require.Empty(t, panes.recorded(), "a turn in flight is not interrupted")
}

// Coalescing is the only bookkeeping the notifier keeps, and it is what turns a
// wave of finishing lanes into one line instead of one per lane.
func TestManyChildrenFinishingProduceOneNudge(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("parent", StatusIdle, RunnerTypeTmux)
	target.setBackend("parent", "claude", "opus")
	target.setPane(claudeComposerPane(""))
	panes := &fakePanes{}
	n := newTestNotifier(t, target, panes)

	// Hold the claim the way a real in-flight nudge would, then report a wave.
	require.True(t, n.claimNudge("parent"))
	for i := range 5 {
		id := SessionID(fmt.Sprintf("child-%d", i))
		target.setChild(id, "parent", StatusIdle, RunnerTypeTmux)
		n.NotifyParent(t.Context(), target.mustInfo(id))
	}
	require.Empty(t, panes.recorded(), "a hint already in flight absorbs the rest of the wave")

	n.releaseNudge("parent")
	n.NotifyParent(t.Context(), target.mustInfo("child-0"))
	require.Equal(t, 1, panes.pasteCount(), "once the claim clears, one hint goes out")
}

// The hint carries no child, status, or path on purpose: anything it carried
// could be read after the fact and be wrong. This is what makes issue #330's
// "a replay and a real failure look identical" unrepresentable.
func TestTheNudgeCarriesNoState(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	claudeChild(target)
	target.annotate("child", func(i *SessionInfo) {
		i.Status = StatusFailed
		i.ErrorMsg = "lane died holding uncommitted work"
		i.ResearchFilePath = "/home/u/.bramble/research/child.md"
	})
	panes := echoPanes(target)

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), target.mustInfo("child"))

	for _, w := range panes.recorded() {
		require.NotContains(t, w, "child", "a hint names no session")
		require.NotContains(t, w, "research", "a hint points at no file")
		require.NotContains(t, w, "died", "a hint reports no status")
	}
}

func TestNoParentMeansNoNudge(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("orphan", StatusIdle, RunnerTypeTmux)
	panes := &fakePanes{}

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), target.mustInfo("orphan"))

	require.Empty(t, panes.recorded())
}

func TestATerminalParentIsNotNudged(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	target.set("parent", StatusCompleted, RunnerTypeTmux)
	panes := &fakePanes{}

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), child)

	require.Empty(t, panes.recorded(), "a finished parent can never read it")
}

func TestABusyParentIsNotNudged(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	target.set("parent", StatusRunning, RunnerTypeTmux)
	panes := &fakePanes{}

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), child)

	require.Empty(t, panes.recorded(), "mid-turn text lands in the next prompt, out of context")
}

// A TUI parent has no pane to type into; its turn loop already surfaces child
// state through the model.
func TestATUIParentIsNotPastedInto(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	target.set("parent", StatusIdle, RunnerTypeTUI)
	panes := &fakePanes{}

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), child)

	require.Empty(t, panes.recorded())
}

// An unreadable pane is silence, not a problem to solve. The notifier proceeds
// because refusing every pane it cannot parse would silence hints for every
// backend without claude's chrome — and a wrong hint costs nothing.
func TestAnUnreadablePaneStillGetsAHint(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	target.captureErr = fmt.Errorf("pane is gone")
	panes := &fakePanes{}

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), child)

	require.Equal(t, 1, panes.pasteCount(),
		"a hint is disposable, so an unreadable pane is not a reason to withhold it")
}

func TestAFailedPasteIsDroppedNotRetried(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	panes := &fakePanes{pasteErr: fmt.Errorf("tmux said no")}
	n := newTestNotifier(t, target, panes)

	n.NotifyParent(t.Context(), child)

	require.Empty(t, target.markedRunning, "a failed paste starts no turn")
	// Nothing was retained: the claim is released, so the next event is free to
	// try again, but nothing is scheduled to do so on its own.
	require.True(t, n.claimNudge("parent"), "a failed hint leaves no claim behind")
}

// The queues found in practice were hours to days old, and one held ten status
// updates each announcing that it superseded the last. Replaying that history
// is precisely the noise being removed, so it is deleted rather than delivered.
func TestStartupDiscardsQueuesLeftByTheRetiredCourier(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stale := filepath.Join(dir, "babysit-prod-planner-bcc6f26d.json")
	require.NoError(t, os.WriteFile(stale, []byte(`[{"to":"p","text":"stale report"}]`), 0o600))
	keep := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(keep, []byte("unrelated"), 0o600))

	target := newFakeTarget()
	panes := &fakePanes{}
	_, err := NewNotifier(target, panes, NotifierConfig{LegacyDeliveryDir: dir})
	require.NoError(t, err)

	require.NoFileExists(t, stale, "a stale queue is reclaimed, not replayed")
	require.FileExists(t, keep, "only the courier's own .json queues are swept")
	require.Empty(t, panes.recorded(), "nothing from the old queue is delivered")
}

func TestASweepOfAMissingDirectoryIsNotAnError(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	_, err := NewNotifier(target, &fakePanes{}, NotifierConfig{
		LegacyDeliveryDir: filepath.Join(t.TempDir(), "never-existed"),
	})
	require.NoError(t, err)
}

// Concurrency: the claim is the only shared state, so two events for one parent
// must still produce one hint.
func TestConcurrentNotificationsNudgeOnce(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	panes := echoPanes(target)
	n := newTestNotifier(t, target, panes)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			n.NotifyParent(t.Context(), child)
		}()
	}
	close(start)
	wg.Wait()

	// The first hint marks the parent running, so every later attempt sees a
	// busy parent regardless of how the claims interleaved.
	require.Equal(t, 1, panes.pasteCount(), "one hint reaches the pane")
}

// TestAHintDoesNotHintBack pins termination.
//
// A hint is typed into the parent's pane and submitted, which starts a turn —
// so the parent goes running, then idle again. If that idle were itself
// hint-worthy the pair would volley forever, filling both panes. It is not:
// hints follow a *child's* transition, and a top-level parent has no parent to
// tell. The integration suite caught the raw pane count that made this look
// like a loop; this pins the actual rule.
func TestAHintDoesNotHintBack(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	panes := echoPanes(target)
	n := newTestNotifier(t, target, panes)

	n.NotifyParent(t.Context(), child)
	require.Equal(t, 1, panes.pasteCount(), "the child's finish hints once")

	// The parent is now running because the hint submitted a prompt. Its own
	// return to idle is the transition that would close the loop.
	target.set("parent", StatusIdle, RunnerTypeTmux)
	n.NotifyParent(t.Context(), target.mustInfo("parent"))

	require.Equal(t, 1, panes.pasteCount(),
		"a parent going idle must not hint: it has no parent, and a volley would never stop")
}
