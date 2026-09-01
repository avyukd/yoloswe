package tmuxctl

import (
	"context"

	"github.com/bazelment/yoloswe/bramble/session"
)

// paneWriter adapts a Controller to session.PaneWriter.
//
// The adapter lives here rather than in session because the dependency only
// runs one way: tmuxctl already imports session for PaneStatus, so session
// cannot import tmuxctl back. session.PaneWriter is declared consumer-side for
// that reason, and this is the single place that satisfies it.
type paneWriter struct{ ctl Controller }

// NewPaneWriter wraps a Controller so a session.Notifier can type into panes.
func NewPaneWriter(ctl Controller) session.PaneWriter { return &paneWriter{ctl: ctl} }

func (w *paneWriter) Paste(ctx context.Context, target, text string) error {
	return Paste(ctx, w.ctl, target, text)
}

// Paste writes text into a pane, leaving copy mode first.
//
// The copy-mode step is why this is shared rather than two call sites doing
// ctl.Paste: a pane someone scrolled back in swallows the Enter that would
// submit the text, so the message lands in the composer and simply sits there —
// delivered by every measure bramble can see, and never actually read by the
// agent. Every writer needs that, so a third one cannot forget it.
func Paste(ctx context.Context, ctl Controller, target, text string) error {
	if err := ctl.ExitCopyMode(ctx, target); err != nil {
		return err
	}
	return ctl.Paste(ctx, target, text)
}

// SendEnter submits what was pasted. The interface names the intent rather
// than taking a SpecialKey, so session does not need tmuxctl's key vocabulary
// to describe a write.
func (w *paneWriter) SendEnter(ctx context.Context, target string) error {
	return w.ctl.SendSpecial(ctx, target, KeyEnter)
}
