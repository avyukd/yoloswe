package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/fleet"
)

func TestDispatchArgsCarryTheTaskToTheRemoteExec(t *testing.T) {
	args := dispatchArgs(execArgs{
		issueID: "INF-1234", repo: "kernel", taskID: "t-7", modelID: "opus",
	}, "jiradozer/kernel/INF-1234")

	joined := strings.Join(args, " ")
	assert.Equal(t, "exec", args[0], "the remote command must be exec, never run")
	assert.Contains(t, joined, "--repo kernel",
		"--repo is required remotely: cwd under tmux is $HOME, so it cannot be inferred")
	assert.Contains(t, joined, "--issue INF-1234")
	assert.Contains(t, joined, "--task-id t-7")
	assert.Contains(t, joined, "--model opus")
	assert.Contains(t, joined, "--tmux-session jiradozer/kernel/INF-1234",
		"the run-log must record the session so a reader can attach to it")
}

// Go randomizes map iteration, so building the argv from a map would render a
// different command on every invocation — making a --dry-run impossible to
// compare against what actually ran.
func TestDispatchArgsAreDeterministic(t *testing.T) {
	x := execArgs{
		issueID: "INF-1", repo: "kernel", taskID: "t", branchPrefix: "feature",
		modelID: "opus", skipPhases: "plan", autoApprove: "all", maxBudget: 12.5,
	}
	first := dispatchArgs(x, "s")
	for i := 0; i < 50; i++ {
		require.Equal(t, first, dispatchArgs(x, "s"),
			"the rendered command must be byte-identical across invocations")
	}
}

func TestDispatchArgsOmitEmptyFlags(t *testing.T) {
	args := dispatchArgs(execArgs{issueID: "INF-1", repo: "kernel"}, "s")
	joined := strings.Join(args, " ")
	for _, flag := range []string{"--task-id", "--model", "--skip-phases", "--auto-approve", "--max-budget", "--force"} {
		assert.NotContains(t, joined, flag, "an unset flag must not be sent")
	}
}

func TestDispatchArgsCarryADescriptionTask(t *testing.T) {
	args := dispatchArgs(execArgs{description: "tidy the helm chart", repo: "kernel", taskID: "t-1"}, "s")
	joined := strings.Join(args, " ")
	assert.Contains(t, joined, "--description")
	assert.Contains(t, joined, "tidy the helm chart")
	assert.NotContains(t, joined, "--issue")
}

// A task description is free text, so it must survive the nested tmux/sh
// quoting rather than being interpreted by the remote shell.
//
// This EXECUTES the generated command instead of grepping it. A substring check
// cannot tell a quoted payload from an executable one — the dangerous text
// appears in the command string either way — so it would pass against a
// genuinely injectable command.
func TestDispatchedDescriptionCannotEscapeIntoTheRemoteShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("needs a POSIX shell")
	}
	bin := t.TempDir()
	canary := filepath.Join(t.TempDir(), "pwned")

	// Stand-in for the remote jiradozer: prints its argv, one per line.
	fakeTool := filepath.Join(bin, "jiradozer")
	require.NoError(t, os.WriteFile(fakeTool,
		[]byte("#!/bin/sh\nfor a in \"$@\"; do echo \"$a\"; done\n"), 0o755))
	// Stand-in for tmux: new-session -d -s NAME COMMAND -> run COMMAND.
	require.NoError(t, os.WriteFile(filepath.Join(bin, "tmux"),
		[]byte("#!/bin/sh\nexec sh -c \"$5\"\n"), 0o755))

	payload := "fix it'; touch " + canary + "; echo '"
	req := fleet.Request{
		Host:        fleet.HostHealth{PublicDNS: "b.example", HasBinary: true, BinaryPath: fakeTool},
		SessionName: "jiradozer/kernel/adhoc",
		Args:        dispatchArgs(execArgs{description: payload, repo: "kernel"}, "jiradozer/kernel/adhoc"),
	}

	run := exec.Command("sh", "-c", req.RemoteCommand())
	// The fake tmux must win over any real one, or the command silently never
	// executes and every assertion below passes vacuously.
	run.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := run.CombinedOutput()
	require.NoError(t, err, "output: %s", out)
	require.NotEmpty(t, strings.TrimSpace(string(out)),
		"the stub tool produced no argv — the command never ran, so this test proves nothing")

	require.NoFileExists(t, canary,
		"the description escaped its quoting and ran as a command")
	// The payload must arrive as ONE argument, byte-identical.
	assert.Contains(t, strings.Split(string(out), "\n"), payload,
		"the description must reach the worker intact, not split or mangled")
}

func TestTmuxSessionNameIsRecognisable(t *testing.T) {
	assert.Equal(t, "jiradozer/kernel/INF-1234", tmuxSessionName("INF-1234", "kernel"))
	assert.Equal(t, "jiradozer/kernel/adhoc", tmuxSessionName("", "kernel"),
		"a --description run still needs a nameable session")
}

func TestFilterHostsMatchesNameOrDNS(t *testing.T) {
	hosts := []fleet.Host{
		{Hostname: "ip-172-31", PublicDNS: "ming2.claw.sycloud.ai"},
		{Hostname: "azure-box", PublicDNS: "ming-devbox2.adevbox.sycloud.ai"},
	}
	require.Len(t, filterHosts(hosts, "azure-box"), 1)
	require.Len(t, filterHosts(hosts, "ming2.claw.sycloud.ai"), 1)
	require.Empty(t, filterHosts(hosts, "nope"))
}

// The lease is fleet-visible, so a second dispatch for a task already running
// somewhere is refused before it can create a duplicate worktree and PR.
func TestFindLeaseHolderBlocksADuplicateDispatch(t *testing.T) {
	scores := []fleet.HostHealth{
		{Host: "a", HeldLeases: []string{"INF-1.lock"}},
		{Host: "b"},
	}
	holder, busy := fleet.FindLeaseHolder(scores, "INF-1")
	require.True(t, busy)
	assert.Equal(t, "a", holder.Host)

	_, busy = fleet.FindLeaseHolder(scores, "INF-2")
	assert.False(t, busy)
}
