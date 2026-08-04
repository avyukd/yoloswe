package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedSSH answers per target and records every command it was asked to run.
type scriptedSSH struct {
	out  map[string]string
	err  map[string]error
	cmds map[string]string
	mu   sync.Mutex
}

func (s *scriptedSSH) Run(_ context.Context, target, cmd string) (string, error) {
	s.mu.Lock()
	if s.cmds == nil {
		s.cmds = map[string]string{}
	}
	s.cmds[target] = cmd
	s.mu.Unlock()
	if err := s.err[target]; err != nil {
		return "", err
	}
	return s.out[target], nil
}

func (s *scriptedSSH) cmdFor(target string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmds[target]
}

func runsHosts() []Host {
	return []Host{
		{Hostname: "a", PublicDNS: "a.example", SSHUser: "ubuntu"},
		{Hostname: "b", PublicDNS: "b.example", SSHUser: "ming"},
	}
}

// The regression this exists to prevent: the probe learned to find a tool under
// ~/.local/bin (the install shape used on boxes carrying no worktree) while the
// gather still looked only at ~/bin. That box is eligible for dispatch and
// invisible to `fleet runs` at the same time, so a scatter-gather loop concludes
// "nothing is running" about the host it just dispatched to.
func TestGatherRunsResolvesTheBinaryExactlyLikeTheProbe(t *testing.T) {
	t.Parallel()
	ssh := &scriptedSSH{out: map[string]string{"ubuntu@a.example": "[]", "ming@b.example": "[]"}}

	GatherRuns(context.Background(), ssh, testTool, runsHosts())

	got := ssh.cmdFor("ubuntu@a.example")
	assert.Contains(t, got, `$HOME/.local/bin/prdozer`,
		"a box with the tool only under ~/.local/bin must still be readable")
	assert.Contains(t, got, `$HOME/bin/prdozer`)
	assert.Contains(t, got, "command -v prdozer")
	// Same expression, not merely a similar one: two call sites that resolve
	// differently is exactly the split-brain being fixed.
	assert.Contains(t, got, testTool.resolveBinExpr())
}

// An absent binary must fail the host loudly. Silently running `MISSING runs
// --json` would surface as an opaque shell error, and a dropped row would look
// like an idle box.
func TestGatherRunsFailsTheHostWhenTheToolIsMissing(t *testing.T) {
	t.Parallel()
	ssh := &scriptedSSH{out: map[string]string{"ubuntu@a.example": "[]", "ming@b.example": "[]"}}

	GatherRuns(context.Background(), ssh, testTool, runsHosts())

	assert.Contains(t, ssh.cmdFor("ubuntu@a.example"), binMissing)
	assert.Contains(t, ssh.cmdFor("ubuntu@a.example"), "exit 127")
}

func TestGatherRunsReturnsOneRowPerHostInOrder(t *testing.T) {
	t.Parallel()
	ssh := &scriptedSSH{out: map[string]string{
		"ubuntu@a.example": `[{"run_id":"aaa"},{"run_id":"bbb"}]`,
		"ming@b.example":   `[]`,
	}}

	got := GatherRuns(context.Background(), ssh, testTool, runsHosts())

	require.Len(t, got, 2)
	assert.Equal(t, "a", got[0].Host)
	assert.Len(t, got[0].Runs, 2)
	assert.False(t, got[0].Empty)
	assert.Equal(t, "b", got[1].Host)
	assert.True(t, got[1].Empty, "a reachable box with no runs is not the same as an unreadable one")
}

// An unreachable box must never look like an idle one: "nothing is running
// anywhere" is a conclusion a dispatcher acts on.
func TestGatherRunsKeepsAnUnreachableHostAsAnError(t *testing.T) {
	t.Parallel()
	ssh := &scriptedSSH{
		out: map[string]string{"ming@b.example": "[]"},
		err: map[string]error{"ubuntu@a.example": errors.New("connection refused")},
	}

	got := GatherRuns(context.Background(), ssh, testTool, runsHosts())

	require.Len(t, got, 2)
	require.Error(t, got[0].Err)
	assert.Contains(t, got[0].Err.Error(), "connection refused")
	assert.False(t, got[0].Empty, "an error row must not also read as empty")
	assert.NoError(t, got[1].Err)
}

func TestGatherRunsSurfacesUnparseableOutput(t *testing.T) {
	t.Parallel()
	ssh := &scriptedSSH{out: map[string]string{
		"ubuntu@a.example": "not json at all",
		"ming@b.example":   "[]",
	}}

	got := GatherRuns(context.Background(), ssh, testTool, runsHosts())

	require.Error(t, got[0].Err)
	assert.Contains(t, got[0].Err.Error(), "parse a output")
}

// Extra args are the caller's filters (--active, --issue INF-1). They are
// quoted here because an issue identifier is caller-supplied text.
func TestGatherRunsQuotesExtraArguments(t *testing.T) {
	t.Parallel()
	ssh := &scriptedSSH{out: map[string]string{"ubuntu@a.example": "[]", "ming@b.example": "[]"}}

	GatherRuns(context.Background(), ssh, testTool, runsHosts(), "--issue", "a b; rm -rf /")

	got := ssh.cmdFor("ubuntu@a.example")
	assert.Contains(t, got, "'--issue'")
	assert.Contains(t, got, `'a b; rm -rf /'`)
	assert.False(t, strings.Contains(got, "; rm -rf / "),
		"an unquoted filter would let a caller's text run as a command")
}

// Rows stay raw so this package never has to know either tool's run schema.
func TestGatherRunsLeavesRowsAsRawJSON(t *testing.T) {
	t.Parallel()
	ssh := &scriptedSSH{out: map[string]string{
		"ubuntu@a.example": `[{"run_id":"aaa","unknown_field":1}]`,
		"ming@b.example":   `[]`,
	}}

	got := GatherRuns(context.Background(), ssh, testTool, runsHosts())

	require.Len(t, got[0].Runs, 1)
	var row map[string]any
	require.NoError(t, json.Unmarshal(got[0].Runs[0], &row))
	assert.Equal(t, "aaa", row["run_id"])
	assert.EqualValues(t, 1, row["unknown_field"], "a field this package does not model must survive")
}
