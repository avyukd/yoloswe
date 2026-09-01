package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSweepReclaimsRealCapturedQueues runs the startup sweep over copies of the
// queues that were actually stuck on the author's machine — 25 reports for one
// parent spanning 4.5 hours, and singles up to two days old. Fixtures, not the
// live directory: the sweep deletes, and another bramble owns ~/.bramble.
func TestSweepReclaimsRealCapturedQueues(t *testing.T) {
	t.Parallel()
	src := os.Getenv("REAL_QUEUE_DIR")
	if src == "" {
		t.Skip("set REAL_QUEUE_DIR to a copy of ~/.bramble/deliveries to run this")
	}
	entries, err := os.ReadDir(src)
	require.NoError(t, err)

	dir := t.TempDir()
	copied := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, e.Name()), data, 0o600))
		copied++
	}
	require.NotZero(t, copied, "the fixture directory held no queues")

	panes := &fakePanes{}
	_, err = NewNotifier(newFakeTarget(), panes, NotifierConfig{LegacyDeliveryDir: dir})
	require.NoError(t, err)

	left, err := filepath.Glob(filepath.Join(dir, "*.json"))
	require.NoError(t, err)
	require.Empty(t, left, "every stuck queue is reclaimed")
	require.Empty(t, panes.recorded(), "and none of it is replayed into a pane")
}
