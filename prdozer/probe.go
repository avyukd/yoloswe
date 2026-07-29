package prdozer

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// probeScript gathers every signal in ONE ssh round trip. Each command is
// sub-100ms and needs no sudo. Sections are delimited so the parser never has
// to guess which output it is looking at — `df` in particular omits or
// collapses rows depending on the filesystem, so position-based parsing is
// unreliable.
const probeScript = `echo "__NPROC__"; nproc
echo "__LOAD__"; cat /proc/loadavg
echo "__DF__"; df -P "$HOME"; df -P /mnt/nvme 2>/dev/null
echo "__TMUX__"; tmux list-windows -a 2>/dev/null | wc -l
echo "__LEASES__"; ls ~/.prdozer/leases/ 2>/dev/null | wc -l
echo "__PRDOZER__"; command -v prdozer || echo MISSING
echo "__END__"`

// HostHealth is one probed box.
type HostHealth struct {
	Err        error
	Host       string
	SSHUser    string
	PublicDNS  string
	Cores      int
	Load1      float64
	DiskFreeGB int
	// NVMeFreeGB is the free space on /mnt/nvme, which exists only on the
	// Azure box. Zero when absent.
	NVMeFreeGB  int
	TmuxWindows int
	// Leases counts babysit leases already held on that box.
	Leases int
	// HasPrdozer reports whether the prdozer binary is on the target's PATH.
	// Dispatching to a box without it produces a tmux session that dies
	// instantly and looks like a silent no-op.
	HasPrdozer bool
	Reachable  bool
	IsSelf     bool
}

// Target returns the ssh destination for this host.
func (h HostHealth) Target() string {
	if h.SSHUser == "" {
		return h.PublicDNS
	}
	return h.SSHUser + "@" + h.PublicDNS
}

// LoadPerCore is the primary ranking signal.
func (h HostHealth) LoadPerCore() float64 {
	if h.Cores <= 0 {
		return h.Load1
	}
	return h.Load1 / float64(h.Cores)
}

// UsableDiskGB is the free space a run can actually use: the NVMe volume when
// present, otherwise the home filesystem. The Azure box's root is small and
// nearly full while its /mnt/nvme has hundreds of gigabytes, so judging it by
// root alone would wrongly exclude the most capable box in the fleet.
func (h HostHealth) UsableDiskGB() int {
	if h.NVMeFreeGB > h.DiskFreeGB {
		return h.NVMeFreeGB
	}
	return h.DiskFreeGB
}

// ProbeOptions tunes a fleet probe.
type ProbeOptions struct {
	SelfDNS          string
	SelfHostname     string
	SSHTimeout       time.Duration
	MinDiskGB        int
	MaxLeasesPerHost int
}

func (o *ProbeOptions) applyDefaults() {
	if o.SSHTimeout <= 0 {
		o.SSHTimeout = 25 * time.Second
	}
	if o.MinDiskGB <= 0 {
		o.MinDiskGB = 40
	}
	if o.MaxLeasesPerHost <= 0 {
		o.MaxLeasesPerHost = 2
	}
}

// SSHRunner executes a command on a remote host. Tests substitute a fake.
type SSHRunner interface {
	Run(ctx context.Context, target string, script string) (string, error)
}

// DefaultSSHRunner shells out to ssh with the same options the existing fleet
// scripts use.
type DefaultSSHRunner struct {
	Timeout time.Duration
}

func (r DefaultSSHRunner) Run(ctx context.Context, target, script string) (string, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 25 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=8",
		"-o", "StrictHostKeyChecking=accept-new",
		target, script,
	)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); ok && len(exitErr.Stderr) > 0 {
			return string(out), fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return string(out), err
	}
	return string(out), nil
}

func asExitError(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}

// ProbeFleet probes every host concurrently and returns their health, ordered
// best-first by ScoreHosts.
func ProbeFleet(ctx context.Context, ssh SSHRunner, hosts []FleetHost, opts ProbeOptions) []HostHealth {
	opts.applyDefaults()
	out := make([]HostHealth, len(hosts))
	var wg sync.WaitGroup
	for i := range hosts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i] = probeOne(ctx, ssh, hosts[i], opts)
		}(i)
	}
	wg.Wait()
	ScoreHosts(out)
	return out
}

func probeOne(ctx context.Context, ssh SSHRunner, h FleetHost, opts ProbeOptions) HostHealth {
	hh := HostHealth{
		Host:      h.Hostname,
		SSHUser:   h.SSHUser,
		PublicDNS: h.PublicDNS,
		IsSelf:    h.IsSelf(opts.SelfDNS, opts.SelfHostname),
	}
	raw, err := ssh.Run(ctx, h.Target(), probeScript)
	if err != nil {
		hh.Err = err
		return hh
	}
	if perr := parseProbe(raw, &hh); perr != nil {
		hh.Err = perr
		return hh
	}
	hh.Reachable = true
	return hh
}

// parseProbe reads the delimited probe output into hh.
func parseProbe(raw string, hh *HostHealth) error {
	sections := splitSections(raw)
	if _, ok := sections["END"]; !ok {
		return fmt.Errorf("probe output truncated (no __END__ marker)")
	}

	if v := firstLine(sections["NPROC"]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse nproc %q: %w", v, err)
		}
		hh.Cores = n
	}
	if v := firstLine(sections["LOAD"]); v != "" {
		fields := strings.Fields(v)
		if len(fields) == 0 {
			return fmt.Errorf("parse loadavg %q", v)
		}
		f, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return fmt.Errorf("parse load1 %q: %w", fields[0], err)
		}
		hh.Load1 = f
	}

	// Parse df BY MOUNT POINT, never by row position: df collapses or omits
	// rows, and the optional /mnt/nvme block means the row count varies
	// between hosts.
	mounts := parseDF(sections["DF"])
	for mount, freeKB := range mounts {
		switch {
		case mount == "/mnt/nvme":
			hh.NVMeFreeGB = int(freeKB / 1024 / 1024)
		default:
			// The home filesystem is whatever df reported for $HOME; on these
			// boxes that is "/". Take the largest non-nvme mount so an
			// unusual layout still yields a sane figure.
			if gb := int(freeKB / 1024 / 1024); gb > hh.DiskFreeGB {
				hh.DiskFreeGB = gb
			}
		}
	}

	if v := firstLine(sections["TMUX"]); v != "" {
		// tmux list-windows counts WINDOWS, not sessions: one session with 30
		// windows is a busy box, and list-sessions would report it as "1".
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			hh.TmuxWindows = n
		}
	}
	if v := firstLine(sections["LEASES"]); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			hh.Leases = n
		}
	}
	if v := firstLine(sections["PRDOZER"]); v != "" && v != "MISSING" {
		hh.HasPrdozer = true
	}
	return nil
}

// splitSections turns the __MARKER__-delimited blob into a map.
func splitSections(raw string) map[string]string {
	out := make(map[string]string)
	current := ""
	var buf []string
	flush := func() {
		if current != "" {
			out[current] = strings.Join(buf, "\n")
		}
		buf = nil
	}
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "__") && strings.HasSuffix(trimmed, "__") && len(trimmed) > 4 {
			flush()
			current = strings.Trim(trimmed, "_")
			continue
		}
		buf = append(buf, line)
	}
	flush()
	return out
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// parseDF maps mount point -> available KB, skipping headers. `df -P`
// guarantees one record per line (no wrapping), with Available in field 3 and
// the mount point last.
func parseDF(s string) map[string]int64 {
	out := make(map[string]int64)
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		if fields[0] == "Filesystem" {
			continue
		}
		avail, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			continue
		}
		out[fields[len(fields)-1]] = avail
	}
	return out
}

// Eligible reports whether this host can accept a new babysit run.
func (h HostHealth) Eligible(opts ProbeOptions) (bool, string) {
	opts.applyDefaults()
	switch {
	case !h.Reachable:
		if h.Err != nil {
			return false, "unreachable: " + h.Err.Error()
		}
		return false, "unreachable"
	case !h.HasPrdozer:
		return false, "prdozer not on PATH"
	case h.UsableDiskGB() < opts.MinDiskGB:
		return false, fmt.Sprintf("only %dGB free (need %dGB)", h.UsableDiskGB(), opts.MinDiskGB)
	case h.Leases >= opts.MaxLeasesPerHost:
		return false, fmt.Sprintf("already holds %d babysit leases", h.Leases)
	}
	return true, ""
}

// ScoreHosts orders hosts best-first: lowest load per core, then fewest tmux
// windows, then most free disk. Ties break on hostname so two concurrent
// dispatches make the SAME deterministic choice rather than both racing for a
// nondeterministic "best".
func ScoreHosts(hosts []HostHealth) {
	sort.SliceStable(hosts, func(i, j int) bool {
		a, b := hosts[i], hosts[j]
		if a.Reachable != b.Reachable {
			return a.Reachable
		}
		if la, lb := a.LoadPerCore(), b.LoadPerCore(); la != lb {
			return la < lb
		}
		if a.TmuxWindows != b.TmuxWindows {
			return a.TmuxWindows < b.TmuxWindows
		}
		if a.UsableDiskGB() != b.UsableDiskGB() {
			return a.UsableDiskGB() > b.UsableDiskGB()
		}
		return a.Host < b.Host
	})
}

// PickHost returns the best eligible host, or an error explaining why every
// candidate was rejected — a dispatch that silently finds nothing is
// impossible to debug.
func PickHost(hosts []HostHealth, opts ProbeOptions) (HostHealth, error) {
	ScoreHosts(hosts)
	var reasons []string
	for _, h := range hosts {
		if ok, _ := h.Eligible(opts); ok {
			return h, nil
		}
		_, why := h.Eligible(opts)
		reasons = append(reasons, fmt.Sprintf("%s: %s", h.Host, why))
	}
	return HostHealth{}, fmt.Errorf("no eligible host (%s)", strings.Join(reasons, "; "))
}

// ClaudeUsage is the slice of claude_limits.py output prdozer cares about.
type ClaudeUsage struct {
	FiveHourUtilization float64
	SevenDayUtilization float64
}

type claudeLimitsDoc struct {
	Default struct {
		Error *string `json:"error"`
		Usage struct {
			FiveHour struct {
				Utilization float64 `json:"utilization"`
			} `json:"five_hour"`
			SevenDay struct {
				Utilization float64 `json:"utilization"`
			} `json:"seven_day"`
		} `json:"usage"`
	} `json:"default"`
}

// ClaudeLimitsScript is the fleet-wide quota reporter.
const ClaudeLimitsScript = "~/magent/scripts/claude_limits.py"

// CheckClaudeQuota reads the shared Claude quota.
//
// This is a fleet-GLOBAL gate, never a per-host ranking signal: the OAuth
// session is shared across every box, which is exactly why magent.cron tags
// the limits job "primary" rather than "all". Query it once on the control box
// and refuse to dispatch if exhausted, rather than burning a run that dies
// mid-polish.
func CheckClaudeQuota(ctx context.Context) (ClaudeUsage, error) {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "python3", ExpandHome(ClaudeLimitsScript), "--json").Output()
	if err != nil {
		return ClaudeUsage{}, fmt.Errorf("run claude_limits.py: %w", err)
	}
	var doc claudeLimitsDoc
	if err := json.Unmarshal(out, &doc); err != nil {
		return ClaudeUsage{}, fmt.Errorf("parse claude_limits output: %w", err)
	}
	if doc.Default.Error != nil && *doc.Default.Error != "" {
		return ClaudeUsage{}, fmt.Errorf("claude_limits reported: %s", *doc.Default.Error)
	}
	return ClaudeUsage{
		FiveHourUtilization: doc.Default.Usage.FiveHour.Utilization,
		SevenDayUtilization: doc.Default.Usage.SevenDay.Utilization,
	}, nil
}

// QuotaExhaustedThreshold is the five-hour utilization above which prdozer
// refuses to start a new run.
const QuotaExhaustedThreshold = 90.0

// Exhausted reports whether quota is too tight to start a run.
func (u ClaudeUsage) Exhausted() bool {
	return u.FiveHourUtilization > QuotaExhaustedThreshold
}
