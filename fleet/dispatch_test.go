package fleet

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingSSH struct {
	err    error
	target string
	cmd    string
}

func (s *recordingSSH) Run(_ context.Context, target, cmd string) (string, error) {
	s.target, s.cmd = target, cmd
	if s.err != nil {
		return "boom", s.err
	}
	return "", nil
}

func babysitRequest() Request {
	return Request{
		Host: HostHealth{
			Host: "box", PublicDNS: "box.example", SSHUser: "ubuntu",
			HasBinary: true, BinaryPath: "/home/ubuntu/bin/prdozer",
		},
		SessionName: "babysit/kernel#8123",
		Args:        []string{"babysit-local", "--repo", "sycamore-labs/kernel", "--pr", "8123"},
	}
}

func TestRemoteCommandRunsUnderAnAttachableTmuxSession(t *testing.T) {
	t.Parallel()
	cmd := babysitRequest().RemoteCommand()
	assert.Contains(t, cmd, "tmux new-session -d")
	assert.Contains(t, cmd, "babysit/kernel#8123",
		"the session must be attachable by a recognisable name")
	assert.Contains(t, cmd, "babysit-local")
	assert.Contains(t, cmd, "sycamore-labs/kernel")
}

// Wrapping the tmux invocation in flock(1) LOOKS like mutual exclusion and is
// not: tmux daemonizes, flock exits, and the lock drops while the worker keeps
// running. The worker takes its own lease instead.
func TestRemoteCommandNeverWrapsTmuxInFlock(t *testing.T) {
	t.Parallel()
	assert.NotContains(t, babysitRequest().RemoteCommand(), "flock")
}

// A non-interactive SSH shell omits ~/bin, so a bare tool name produces a tmux
// session that dies instantly with "command not found" — which looks exactly
// like a silent no-op.
func TestRemoteCommandUsesTheResolvedAbsolutePath(t *testing.T) {
	t.Parallel()
	cmd := babysitRequest().RemoteCommand()
	assert.Contains(t, cmd, "/home/ubuntu/bin/prdozer")
}

// The tool resolves the agent CLI from PATH, and `claude` lives in
// ~/.local/bin, which a non-interactive SSH shell omits. Without this the
// worker starts, detects work, then every round dies with
// `exec: "claude": executable file not found in $PATH`.
//
// tmux -e PATH= is silently ignored (tmux special-cases PATH; -e FOO=bar DOES
// work, which makes it easy to misdiagnose), hence the shell wrapper.
func TestRemoteCommandPutsUserLocalBinOnPathViaAShell(t *testing.T) {
	t.Parallel()
	cmd := babysitRequest().RemoteCommand()
	assert.Contains(t, cmd, "/home/ubuntu/.local/bin")
	assert.Contains(t, cmd, "sh -c")
	assert.Contains(t, cmd, "export PATH=")
	assert.NotContains(t, cmd, "tmux -e", "tmux -e silently ignores PATH")
	// The base entries a non-interactive shell would have provided must be
	// carried explicitly, because the wrapper replaces PATH wholesale.
	assert.Contains(t, cmd, "/usr/bin")
}

// Rather than set a wrong PATH, set none.
func TestRemoteCommandOmitsPathWhenItCannotBeDerived(t *testing.T) {
	t.Parallel()
	bare := Request{
		Host:        HostHealth{PublicDNS: "b.example", HasBinary: true, BinaryPath: "prdozer"},
		SessionName: "s",
		Args:        []string{"babysit-local"},
	}
	cmd := bare.RemoteCommand()
	assert.NotContains(t, cmd, "export PATH=")
	// The binary still runs; only the PATH export is dropped. Args are quoted
	// individually, so assert on them separately rather than on a joined string.
	assert.Contains(t, cmd, "prdozer")
	assert.Contains(t, cmd, "babysit-local")
}

func TestSSHCommandIsPrintableForADryRun(t *testing.T) {
	t.Parallel()
	got := babysitRequest().SSHCommand()
	assert.Contains(t, got, "ssh -o BatchMode=yes -o ConnectTimeout=8 -o StrictHostKeyChecking=accept-new")
	assert.Contains(t, got, "ubuntu@box.example")
	assert.Contains(t, got, "babysit-local")
}

func TestShellQuoteEscapesSingleQuotes(t *testing.T) {
	t.Parallel()
	assert.Equal(t, `'plain'`, shellQuote("plain"))
	assert.Equal(t, `'it'\''s'`, shellQuote("it's"))
}

// Half-dispatching into a session that dies instantly is indistinguishable
// from a silent no-op, so refuse loudly instead.
func TestDispatchRefusesAHostWithoutTheBinary(t *testing.T) {
	t.Parallel()
	req := babysitRequest()
	req.Host.HasBinary = false

	ssh := &recordingSSH{}
	err := Dispatch(context.Background(), ssh, Tool{Name: "prdozer"}, req, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not on")
	assert.Empty(t, ssh.cmd, "nothing may be sent to an unusable host")
}

func TestDispatchSendsTheCommandToTheChosenHost(t *testing.T) {
	t.Parallel()
	ssh := &recordingSSH{}
	require.NoError(t, Dispatch(context.Background(), ssh, Tool{Name: "prdozer"}, babysitRequest(), nil))
	assert.Equal(t, "ubuntu@box.example", ssh.target)
	assert.Contains(t, ssh.cmd, "tmux new-session -d")
}

func TestDispatchSurfacesARemoteFailure(t *testing.T) {
	t.Parallel()
	ssh := &recordingSSH{err: errors.New("connection refused")}
	err := Dispatch(context.Background(), ssh, Tool{Name: "prdozer"}, babysitRequest(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
	assert.Contains(t, err.Error(), "boom", "remote output belongs in the error")
}

// Args are quoted here so callers can pass raw values — an issue title or a
// description would otherwise break out of the nested shell quoting.
func TestRemoteCommandQuotesArgumentsContainingSpaces(t *testing.T) {
	t.Parallel()
	req := Request{
		Host:        HostHealth{PublicDNS: "b.example", HasBinary: true, BinaryPath: "/home/ming/bin/jiradozer"},
		SessionName: "jd/INF-1",
		Args:        []string{"exec", "--description", "fix the thing; rm -rf /"},
	}
	cmd := req.RemoteCommand()
	assert.Contains(t, cmd, "fix the thing")
	// The dangerous fragment must be inside quoting, never a bare command.
	assert.False(t, strings.Contains(cmd, "; rm -rf / "),
		"an unquoted argument would let a task description run as a command")
}

// Both install shapes are real: a symlink at ~/bin on a box that builds the
// binary, and a copied artifact at ~/.local/bin on a box carrying no worktree.
// Deriving the home root by walking up one level breaks on the second, yielding
// ~/.local/.local/bin and dropping the directory the binary actually lives in.
func TestRemotePathEnvHandlesALocalBinInstall(t *testing.T) {
	t.Parallel()
	req := Request{
		Host:        HostHealth{PublicDNS: "b.example", HasBinary: true, BinaryPath: "/home/ming/.local/bin/jiradozer"},
		SessionName: "s",
		Args:        []string{"exec"},
	}
	cmd := req.RemoteCommand()
	assert.Contains(t, cmd, "/home/ming/.local/bin")
	assert.NotContains(t, cmd, "/home/ming/.local/.local/bin",
		"the home root must not be derived as ~/.local")
	assert.Contains(t, cmd, "/usr/bin", "the base PATH entries must still be carried")
}
