package prdozer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The blobs below are REAL probe output captured from the live fleet, not
// hand-written fixtures. They carry the exact quirks the parser must handle.

// awsProbeBlob: this box (ming.devbox, aws). Single df row, no /mnt/nvme.
const awsProbeBlob = `__NPROC__
8
__LOAD__
2.77 2.19 2.09 3/3824 1086743
__DF__
Filesystem     1024-blocks      Used Available Capacity Mounted on
/dev/root        506771172 345397612 161357176      69% /
__TMUX__
31
__LEASES__
0
__PRDOZER__
/home/ubuntu/bin/prdozer
__END__
`

// azureProbeBlob: ming-devbox2 (azure). Root is nearly FULL (14GB) but
// /mnt/nvme has 820GB — judging this box by root alone would wrongly exclude
// the most capable machine in the fleet. Note df emits a SECOND header row for
// the extra filesystem.
const azureProbeBlob = `__NPROC__
16
__LOAD__
4.91 10.93 10.40 8/1848 3142536
__DF__
Filesystem     1024-blocks      Used Available Capacity Mounted on
/dev/root        128917488 114537328  14363776      89% /
Filesystem     1024-blocks     Used Available Capacity Mounted on
/dev/md0         906877724 40579812 820157396       5% /mnt/nvme
__TMUX__
15
__LEASES__
0
__PRDOZER__
/home/ming/bin/prdozer
__END__
`

// claw box: no /mnt/nvme, busier tmux.
const clawProbeBlob = `__NPROC__
8
__LOAD__
2.44 2.36 2.94 4/4484 2308778
__DF__
Filesystem     1024-blocks      Used Available Capacity Mounted on
/dev/root        518960100 424911936  94031780      82% /
__TMUX__
38
__LEASES__
0
__PRDOZER__
MISSING
__END__
`

func TestParseProbe_RealAWSBlob(t *testing.T) {
	t.Parallel()
	var hh HostHealth
	require.NoError(t, parseProbe(awsProbeBlob, &hh))
	assert.Equal(t, 8, hh.Cores)
	assert.InDelta(t, 2.77, hh.Load1, 0.001)
	assert.Equal(t, 153, hh.DiskFreeGB, "161357176 KB is ~153 GB")
	assert.Zero(t, hh.NVMeFreeGB, "an AWS box has no /mnt/nvme")
	assert.Equal(t, 31, hh.TmuxWindows)
	assert.Equal(t, 0, hh.Leases)
	assert.True(t, hh.HasPrdozer)
}

func TestParseProbe_RealAzureBlob_UsesNVMe(t *testing.T) {
	t.Parallel()
	// The whole point of parsing by mount point: this box's ROOT is 89% full
	// with 14GB free, which would fail the disk floor, while its NVMe volume
	// has 782GB. Position-based parsing would also break here — df emits a
	// second header row before the /mnt/nvme record.
	var hh HostHealth
	require.NoError(t, parseProbe(azureProbeBlob, &hh))
	assert.Equal(t, 16, hh.Cores)
	assert.InDelta(t, 4.91, hh.Load1, 0.001)
	assert.Equal(t, 13, hh.DiskFreeGB, "root really is nearly full")
	assert.Equal(t, 782, hh.NVMeFreeGB, "the nvme volume must be found by mount point")
	assert.Equal(t, 782, hh.UsableDiskGB(), "usable disk is the nvme volume, not root")
	assert.Equal(t, 15, hh.TmuxWindows)

	// Reachable is set by probeOne, not parseProbe; set it to exercise
	// eligibility on this parsed data.
	hh.Reachable = true
	ok, why := hh.Eligible(ProbeOptions{MinDiskGB: 40})
	assert.True(t, ok, "the azure box must be eligible via nvme, got: %s", why)
}

func TestParseProbe_MissingPrdozerIsDetected(t *testing.T) {
	t.Parallel()
	var hh HostHealth
	require.NoError(t, parseProbe(clawProbeBlob, &hh))
	assert.False(t, hh.HasPrdozer)
	hh.Reachable = true
	ok, why := hh.Eligible(ProbeOptions{})
	assert.False(t, ok, "a box without prdozer must not be dispatched to")
	assert.Contains(t, why, "PATH")
}

func TestParseProbe_TruncatedOutputIsAnError(t *testing.T) {
	t.Parallel()
	// A probe killed mid-flight must not be silently read as "0 cores, 0
	// load", which would rank it as the least-loaded box in the fleet.
	var hh HostHealth
	err := parseProbe("__NPROC__\n8\n__LOAD__\n", &hh)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "truncated")
}

func TestParseProbe_DFParsedByMountNotPosition(t *testing.T) {
	t.Parallel()
	// Deliberately reorder so /mnt/nvme comes FIRST. Position-based parsing
	// would now mistake the nvme volume for the home filesystem.
	blob := `__NPROC__
4
__LOAD__
0.5 0.4 0.3 1/100 1
__DF__
Filesystem     1024-blocks     Used Available Capacity Mounted on
/dev/md0         906877724 40579812 820157396       5% /mnt/nvme
/dev/root        128917488 114537328  14363776      89% /
__TMUX__
0
__LEASES__
0
__PRDOZER__
/usr/bin/prdozer
__END__
`
	var hh HostHealth
	require.NoError(t, parseProbe(blob, &hh))
	assert.Equal(t, 782, hh.NVMeFreeGB)
	assert.Equal(t, 13, hh.DiskFreeGB)
}

func TestHostHealth_LoadPerCore(t *testing.T) {
	t.Parallel()
	// The azure box has a higher raw load (4.91) than this box (2.77) but
	// twice the cores, so per-core it is the LESS loaded machine. Ranking on
	// raw load would pick the wrong box.
	azure := HostHealth{Cores: 16, Load1: 4.91}
	aws := HostHealth{Cores: 8, Load1: 2.77}
	assert.Less(t, azure.LoadPerCore(), aws.LoadPerCore())

	// A probe that failed to read nproc must not divide by zero.
	assert.InDelta(t, 3.0, HostHealth{Cores: 0, Load1: 3.0}.LoadPerCore(), 0.001)
}

func TestScoreHosts_DeterministicTiebreak(t *testing.T) {
	t.Parallel()
	// Two concurrent dispatches must make the SAME choice, or they both land
	// on a nondeterministic "best" and collide.
	mk := func(name string) HostHealth {
		return HostHealth{Host: name, Reachable: true, Cores: 8, Load1: 1.0, TmuxWindows: 5, DiskFreeGB: 100}
	}
	a := []HostHealth{mk("charlie"), mk("alpha"), mk("bravo")}
	b := []HostHealth{mk("bravo"), mk("charlie"), mk("alpha")}
	ScoreHosts(a)
	ScoreHosts(b)
	assert.Equal(t, "alpha", a[0].Host)
	assert.Equal(t, a[0].Host, b[0].Host, "identical hosts must sort identically regardless of input order")
}

func TestScoreHosts_PrefersLowLoadThenIdleThenDisk(t *testing.T) {
	t.Parallel()
	busy := HostHealth{Host: "busy", Reachable: true, Cores: 8, Load1: 7.0, DiskFreeGB: 500}
	idle := HostHealth{Host: "idle", Reachable: true, Cores: 8, Load1: 0.2, DiskFreeGB: 100}
	dead := HostHealth{Host: "dead", Reachable: false}
	hosts := []HostHealth{dead, busy, idle}
	ScoreHosts(hosts)
	assert.Equal(t, "idle", hosts[0].Host, "lowest load per core wins")
	assert.Equal(t, "dead", hosts[2].Host, "unreachable hosts sort last")
}

func TestPickHost_ExcludesAndExplains(t *testing.T) {
	t.Parallel()
	opts := ProbeOptions{MinDiskGB: 40, MaxLeasesPerHost: 1}
	hosts := []HostHealth{
		{Host: "full", Reachable: true, HasPrdozer: true, Cores: 8, Load1: 0.1, DiskFreeGB: 5},
		{Host: "leased", Reachable: true, HasPrdozer: true, Cores: 8, Load1: 0.2, DiskFreeGB: 500, Leases: 1},
		{Host: "down", Reachable: false, Err: fmt.Errorf("connection refused")},
	}
	_, err := PickHost(hosts, opts)
	require.Error(t, err, "no host is eligible")
	// A dispatch that finds nothing must say why for EVERY candidate.
	assert.Contains(t, err.Error(), "full")
	assert.Contains(t, err.Error(), "5GB free")
	assert.Contains(t, err.Error(), "leased")
	assert.Contains(t, err.Error(), "leases")
	assert.Contains(t, err.Error(), "down")

	good := HostHealth{Host: "good", Reachable: true, HasPrdozer: true, Cores: 8, Load1: 0.3, DiskFreeGB: 500}
	picked, err := PickHost(append(hosts, good), opts)
	require.NoError(t, err)
	assert.Equal(t, "good", picked.Host)
}

// fakeSSH serves canned probe output per target.
type fakeSSH struct {
	out  map[string]string
	errs map[string]error
	seen []string
}

func (f *fakeSSH) Run(_ context.Context, target, _ string) (string, error) {
	f.seen = append(f.seen, target)
	if err, ok := f.errs[target]; ok {
		return "", err
	}
	return f.out[target], nil
}

func TestProbeFleet_MarksUnreachableWithoutFailing(t *testing.T) {
	t.Parallel()
	// One dead box must not prevent the rest of the fleet from being used.
	hosts := []FleetHost{
		{Hostname: "aws-box", PublicDNS: "aws.example", SSHUser: "ubuntu", Cloud: "aws"},
		{Hostname: "azure-box", PublicDNS: "azure.example", SSHUser: "ming", Cloud: "azure"},
		{Hostname: "dead-box", PublicDNS: "dead.example", SSHUser: "ubuntu", Cloud: "aws"},
	}
	ssh := &fakeSSH{
		out: map[string]string{
			"ubuntu@aws.example": awsProbeBlob,
			"ming@azure.example": azureProbeBlob,
		},
		errs: map[string]error{"ubuntu@dead.example": fmt.Errorf("connection timed out")},
	}
	got := ProbeFleet(context.Background(), ssh, hosts, ProbeOptions{})
	require.Len(t, got, 3)

	byName := map[string]HostHealth{}
	for _, h := range got {
		byName[h.Host] = h
	}
	assert.True(t, byName["aws-box"].Reachable)
	assert.True(t, byName["azure-box"].Reachable)
	assert.False(t, byName["dead-box"].Reachable)
	assert.Error(t, byName["dead-box"].Err)
	// Azure is less loaded per core, so it should rank first.
	assert.Equal(t, "azure-box", got[0].Host)
}

func TestProbeFleet_DetectsSelf(t *testing.T) {
	t.Parallel()
	hosts := []FleetHost{{Hostname: "me", PublicDNS: "me.example", SSHUser: "ubuntu"}}
	ssh := &fakeSSH{out: map[string]string{"ubuntu@me.example": awsProbeBlob}}
	got := ProbeFleet(context.Background(), ssh, hosts, ProbeOptions{SelfDNS: "me.example"})
	require.Len(t, got, 1)
	assert.True(t, got[0].IsSelf, "the dispatcher must recognise its own box and not SSH to itself")
}

func TestClaudeUsage_Exhausted(t *testing.T) {
	t.Parallel()
	assert.False(t, ClaudeUsage{FiveHourUtilization: 26}.Exhausted(), "the real observed value should pass")
	assert.False(t, ClaudeUsage{FiveHourUtilization: 90}.Exhausted(), "the threshold itself is not over")
	assert.True(t, ClaudeUsage{FiveHourUtilization: 91}.Exhausted())
}

func TestLoadFleet_RealSchema(t *testing.T) {
	t.Parallel()
	// Exactly the schema the live ~/magent/fleet/*.json files use.
	dir := t.TempDir()
	write := func(name, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	write("ming-devbox2.json", `{
  "cloud": "azure", "cron_style": "manifest", "hostname": "ming-devbox2",
  "public_dns": "ming-devbox2.adevbox.sycloud.ai", "registered": "2026-07-26",
  "roles": "all", "ssh_user": "ming", "sync_offset": "3"
}`)
	write("ming.devbox.json", `{
  "cloud": "aws", "cron_style": "legacy", "hostname": "ip-172-31-23-21",
  "public_dns": "ming.devbox.sycloud.ai", "registered": "2026-07-26",
  "roles": "", "ssh_user": "ubuntu", "sync_offset": ""
}`)
	write("notes.txt", "ignored")

	hosts, err := LoadFleet(dir)
	require.NoError(t, err)
	require.Len(t, hosts, 2, "non-JSON files are ignored")
	// Sorted by hostname for determinism.
	assert.Equal(t, "ip-172-31-23-21", hosts[0].Hostname)
	assert.Equal(t, "ubuntu@ming.devbox.sycloud.ai", hosts[0].Target())
	assert.False(t, hosts[0].HasNVMe(), "aws boxes have no /mnt/nvme")
	assert.True(t, hosts[1].HasNVMe(), "the azure box does")
}

func TestLoadFleet_MalformedEntryFailsLoudly(t *testing.T) {
	t.Parallel()
	// Silently skipping a broken entry would dispatch across a subset of the
	// fleet while looking healthy.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not json"), 0o600))
	_, err := LoadFleet(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse")
}

func TestSelfPublicDNS(t *testing.T) {
	t.Parallel()
	// The real file on this box contains only this one line.
	path := filepath.Join(t.TempDir(), "devbox-config")
	require.NoError(t, os.WriteFile(path, []byte("PUBLIC_DNS=ming.devbox.sycloud.ai\n"), 0o600))
	assert.Equal(t, "ming.devbox.sycloud.ai", SelfPublicDNS(path))
	assert.Empty(t, SelfPublicDNS(filepath.Join(t.TempDir(), "missing")))
}

func TestFleetHost_IsSelf(t *testing.T) {
	t.Parallel()
	h := FleetHost{Hostname: "ip-172-31-23-21", PublicDNS: "ming.devbox.sycloud.ai"}
	assert.True(t, h.IsSelf("ming.devbox.sycloud.ai", ""), "matched by public DNS")
	assert.True(t, h.IsSelf("", "ip-172-31-23-21"), "falls back to hostname")
	assert.False(t, h.IsSelf("other.example", "other-host"))
}

// The probe must report leases actually HELD, not lease files present.
// Release() deliberately leaves the file behind, so a file count is a count of
// babysits the box has ever run — and with MaxLeasesPerHost=2 every host
// permanently excluded itself after two runs. Observed 2026-07-30: two of three
// boxes reported "already holds 2 babysit leases" with zero live workers.
func TestProbeCommand_CountsHeldLeasesNotFiles(t *testing.T) {
	t.Parallel()
	assert.Contains(t, probeScript, "flock -n",
		"lease occupancy must be tested with flock, not by counting files")
	assert.NotContains(t, probeScript, "ls ~/.prdozer/leases/ 2>/dev/null | wc -l",
		"the file-count form is what caused the fleet to exclude every host")
}

// A stale lease file (no holder) must not make a host ineligible.
func TestEligible_StaleLeaseFilesDoNotExcludeAHost(t *testing.T) {
	t.Parallel()
	h := HostHealth{
		Host: "box", Reachable: true, HasPrdozer: true,
		Cores: 8, Load1: 1.0, DiskFreeGB: 100,
		Leases: 0, // files may exist on disk; none are held
	}
	ok, reason := h.Eligible(ProbeOptions{MaxLeasesPerHost: 2, MinDiskGB: 40})
	assert.True(t, ok, "a host with no HELD leases is eligible: %s", reason)
}
