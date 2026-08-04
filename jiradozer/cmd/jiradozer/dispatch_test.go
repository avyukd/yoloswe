package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/cliapp"
	"github.com/bazelment/yoloswe/fleet"
	"github.com/bazelment/yoloswe/jiradozer"
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

// The guard above only holds if the name dispatch probes for is the name the
// worker's lease file actually has. Hand-writing "INF-1" on both sides tests
// fleet's matcher and nothing else — it would keep passing if either end
// stopped canonicalizing, or if sanitizeForRunDir and fleet.SanitizeSlug
// drifted apart. So derive both ends the way production does, and spell the
// issue differently on each, which is the case the guard exists for.
func TestDispatchProbesForTheLeaseNameTheWorkerWillHold(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// exec's side: the lease a worker handed the URL form would hold, as a
	// probe of that box would report it.
	workerLease := jiradozer.LeasePath(leaseTarget(execArgs{issueID: "https://github.com/acme/app/issues/42"}))
	scores := []fleet.HostHealth{
		{Host: "b"},
		{Host: "a", HeldLeases: []string{filepath.Base(workerLease)}},
	}

	// dispatch's side: the same issue, shorthand.
	holder, busy := fleet.FindLeaseHolder(scores, leaseTarget(execArgs{issueID: "acme/app#42"}))
	require.True(t, busy, "the same issue spelled another way must still find the holder")
	assert.Equal(t, "a", holder.Host)

	_, busy = fleet.FindLeaseHolder(scores, leaseTarget(execArgs{issueID: "acme/app#43"}))
	assert.False(t, busy, "and a different issue must not collide with it")
}

// --host must not be a way around the duplicate-run guard.
//
// The guard searches the probe results, so narrowing the fleet to the pinned
// box before searching would hide a lease held anywhere else: pin the idle
// machine and the busy one is simply not in the slice being examined. Since a
// --description run has no tracker-side claim to fall back on, that would make
// the pin the only thing standing between two workers and the same task.
func TestPinningAHostDoesNotHideALeaseHeldElsewhere(t *testing.T) {
	scores := []fleet.HostHealth{
		{Host: "busy", Reachable: true, HeldLeases: []string{"INF-1.lock"}},
		{Host: "idle", Reachable: true, PublicDNS: "idle.example"},
	}

	_, err := narrowToPin(scores, "INF-1", "idle", false)
	require.Error(t, err, "the lease on `busy` must be found even though `idle` was pinned")
	assert.Contains(t, err.Error(), "busy", "the operator needs to be told which box has it")

	// The pin still narrows once the guard has passed, by either name form.
	for _, pin := range []string{"idle", "IDLE.example"} {
		out, err := narrowToPin(scores, "INF-2", pin, false)
		require.NoError(t, err, "a different task is not blocked by this lease")
		require.Len(t, out, 1, "ranking still sees only the pinned host")
		assert.Equal(t, "idle", out[0].Host)
	}

	// And an unpinned dispatch keeps the whole fleet to rank over.
	out, err := narrowToPin(scores, "INF-2", "", false)
	require.NoError(t, err)
	assert.Len(t, out, 2)
}

// A host whose probe failed reports no leases at all, which is indistinguishable
// from a host that genuinely holds none. So "nobody holds it" is not an answer
// until every box has answered: a worker on an ssh-down machine would otherwise
// be invisible, and for a --description run this lease is the only cross-host
// exclusion there is. Same fail-closed rule `fleet runs` applies to a partial
// view.
func TestAnUnprobeableHostIsNotProofThatNobodyHoldsTheLease(t *testing.T) {
	scores := []fleet.HostHealth{
		{Host: "answered", Reachable: true},
		{Host: "ssh-down", Err: errors.New("connection refused")},
		{Host: "also-down", Err: errors.New("connection refused")},
	}

	_, err := narrowToPin(scores, "INF-1", "", false)
	require.Error(t, err, "an incomplete fleet view cannot clear a task to run")
	assert.Contains(t, err.Error(), "INF-1")
	assert.Contains(t, err.Error(), "2 of 3")
	assert.Contains(t, err.Error(), "also-down, ssh-down",
		"the unreachable boxes are named, and sorted so identical fleets print identically")
	assert.Contains(t, err.Error(), "--force", "the refusal has to say how to proceed anyway")

	// --force is the documented way past an incomplete view.
	out, err := narrowToPin(scores, "INF-1", "", true)
	require.NoError(t, err)
	assert.Len(t, out, 3)

	// A fully-probed fleet holding no lease is a real answer, not a refusal.
	_, err = narrowToPin([]fleet.HostHealth{{Host: "answered", Reachable: true}}, "INF-1", "", false)
	assert.NoError(t, err)
}

// --force waves through uncertainty, never a live worker: a lease actually held
// on a box that answered is a fact, not a stale claim.
func TestForceDoesNotOverrideALeaseThatIsActuallyHeld(t *testing.T) {
	scores := []fleet.HostHealth{
		{Host: "busy", Reachable: true, HeldLeases: []string{"INF-1.lock"}},
		{Host: "ssh-down", Err: errors.New("connection refused")},
	}

	_, err := narrowToPin(scores, "INF-1", "", true)
	require.ErrorContains(t, err, "already running on busy")
}

// With no target there is nothing to guard, so an unreachable box must not
// block a dispatch it has no bearing on.
func TestAnIncompleteFleetOnlyBlocksATaskThatHasALeaseName(t *testing.T) {
	scores := []fleet.HostHealth{
		{Host: "answered", Reachable: true},
		{Host: "ssh-down", Err: errors.New("connection refused")},
	}

	out, err := narrowToPin(scores, "", "", false)
	require.NoError(t, err)
	assert.Len(t, out, 2)
}

// A pin that matched the loaded fleet but matches nothing in the probe results
// cannot happen — both sides compare the same two fields. Say so rather than
// let PickHost answer "no eligible host", which would send an operator looking
// at disk and load for a name that simply did not match.
func TestAPinThatSurvivesLoadingButNotProbingIsNamedAsSuch(t *testing.T) {
	_, err := narrowToPin([]fleet.HostHealth{{Host: "a", Reachable: true}}, "INF-1", "b", false)
	require.ErrorContains(t, err, `host "b"`)
	assert.NotContains(t, err.Error(), "eligible")
}

// The remote worker starts with cwd $HOME, so the default relative
// "jiradozer.yaml" resolves to nothing there and the run dies before doing any
// work. The config path must be forwarded, in a form the target can resolve.
func TestDispatchArgsForwardAConfigPathTheTargetCanResolve(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// The common case, and the subtle one: the caller's SHELL expands ~ before
	// jiradozer ever sees the value, so what arrives is a local absolute path.
	// Homes differ per box — this user is "ming", the AWS boxes run as "ubuntu"
	// — while ~/magent is synced fleet-wide, so it must go over home-relative
	// or it names a file that exists on no other machine.
	local := filepath.Join(home, "magent", "jdozer", "jiradozer_kernel.yaml")
	joined := strings.Join(dispatchArgs(execArgs{
		issueID: "INF-1", repo: "kernel", configPath: local}, "s"), " ")
	assert.Contains(t, joined, "--config ~/magent/jdozer/jiradozer_kernel.yaml")
	assert.NotContains(t, joined, home, "a local home path resolves on no other box")

	// An explicit tilde is already portable and passes through untouched.
	joined = strings.Join(dispatchArgs(execArgs{
		issueID: "INF-1", repo: "kernel", configPath: "~/magent/x.yaml"}, "s"), " ")
	assert.Contains(t, joined, "--config ~/magent/x.yaml")

	// A path outside home cannot be made portable, so it goes absolute — a
	// relative path would mean nothing from $HOME.
	joined = strings.Join(dispatchArgs(execArgs{
		issueID: "INF-1", repo: "kernel", configPath: "/etc/jd.yaml"}, "s"), " ")
	assert.Contains(t, joined, "--config /etc/jd.yaml")
}

// runDispatchCmd executes the real dispatch command so ORDERING is what is
// under test. The bug this covers was purely an ordering mistake — `--here`
// returned above both guards — which no unit test of a helper could have seen.
func runDispatchCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var x execArgs
	cmd := newDispatchCmd(&x)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	cmd.SetContext(cliapp.WithApp(context.Background(), &cliapp.App{Logger: testMainLogger(t)}))
	err := cmd.Execute()
	return out.String(), err
}

// A dry run must never execute, whatever the dispatch shape. `--dry-run --here`
// used to fall straight into runExec and do the work for real.
//
// The assertion is load-bearing in a specific way: --repo names a repo that does
// not exist and no config is passed, so if the dry run ever executes, runExec
// fails and this returns an error. Passing REQUIRES not having run.
func TestDryRunHereNeverExecutes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	out, err := runDispatchCmd(t, "--here", "--dry-run", "--skip-quota-check",
		"--description", "should never run", "--repo", "no-such-repo-exists")

	require.NoError(t, err, "a dry run reached execution and failed there: %s", out)
	assert.Contains(t, out, "would run IN-PROCESS")
	// Nothing may be claimed by a preview.
	require.NoDirExists(t, filepath.Join(os.Getenv("HOME"), ".jiradozer", "runs"))
	require.NoDirExists(t, filepath.Join(os.Getenv("HOME"), ".jiradozer", "leases"))
}

// --here skips host SELECTION, not the duplicate guard. With no fleet inventory
// readable there is no cross-host concern, so the guard degrades to a no-op
// rather than blocking the local escape hatch.
func TestGuardDuplicateRunDegradesWithoutAFleet(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no ~/magent/fleet here

	err := guardDuplicateRun(context.Background(),
		execArgs{description: "x", taskID: "t-1", repo: "yoloswe"}, 0, testMainLogger(t))
	require.NoError(t, err, "an absent fleet must not block a local run")
}

// A --description run has no tracker-side claim, so its lease target is derived
// from its CONTENT and is stable across invocations. That is what makes the
// duplicate guard work for ad-hoc tasks at all — a per-run identifier would be
// unique every time and could never collide with the run it needs to exclude.
func TestAdHocLeaseTargetIsStableAcrossRuns(t *testing.T) {
	a := execArgs{description: "tidy the helm chart", repo: "yoloswe"}
	first, second := leaseTarget(a), leaseTarget(a)

	require.NotEmpty(t, first)
	require.Equal(t, first, second, "an ad-hoc target must be reproducible, or it can never collide")

	// Different task, or same task in a different repo, must not collide.
	require.NotEqual(t, first, leaseTarget(execArgs{description: "something else", repo: "yoloswe"}))
	require.NotEqual(t, first, leaseTarget(execArgs{description: "tidy the helm chart", repo: "kernel"}))
}
