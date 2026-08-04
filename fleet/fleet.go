// Package fleet places work on Ming's devbox fleet: it reads the registry,
// probes hosts for capacity, ranks them, and hands a run off under tmux.
//
// It is shared by prdozer and jiradozer. Everything tool-specific — the binary
// name, its subcommand and flags, where it keeps leases — arrives through Tool,
// so the parts that were expensive to get right (holding a lease inside the
// worker, resolving an absolute binary path, setting PATH through a shell
// rather than tmux -e) are written once.
package fleet

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// DefaultFleetDir holds one JSON file per registered devbox.
const DefaultFleetDir = "~/magent/fleet"

// DevboxConfigPath identifies the box we are currently running on.
const DevboxConfigPath = "~/.devbox-config"

// Host is one devbox as recorded in the registry.
//
// These are PROVISIONING FACTS ONLY, never liveness — a deleted box keeps its
// file, and roles/cron_style reflect what was true at registration. Every
// candidate must therefore be probed before it is dispatched to.
type Host struct {
	Cloud      string `json:"cloud"`
	CronStyle  string `json:"cron_style"`
	Hostname   string `json:"hostname"`
	PublicDNS  string `json:"public_dns"`
	Registered string `json:"registered"`
	Roles      string `json:"roles"`
	SSHUser    string `json:"ssh_user"`
	SyncOffset string `json:"sync_offset"`
}

// Target returns the ssh destination for this host.
func (h Host) Target() string {
	if h.SSHUser == "" {
		return h.PublicDNS
	}
	return h.SSHUser + "@" + h.PublicDNS
}

// HasNVMe reports whether this host is expected to have a /mnt/nvme volume.
// Only the Azure box does, so probing keys off cloud rather than assuming the
// mount exists everywhere.
func (h Host) HasNVMe() bool {
	return strings.EqualFold(h.Cloud, "azure")
}

// Load reads every *.json in dir, sorted by hostname for deterministic
// ordering. An unparseable file is an error rather than a silent skip: a
// malformed registry entry means the fleet view is wrong, and silently
// dispatching to a subset is worse than failing loudly.
func Load(dir string) ([]Host, error) {
	if dir == "" {
		dir = DefaultFleetDir
	}
	root := ExpandHome(dir)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read fleet dir %s: %w", root, err)
	}
	var hosts []Host
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(root, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		var h Host
		if err := json.Unmarshal(data, &h); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if h.PublicDNS == "" {
			return nil, fmt.Errorf("%s: public_dns is required", path)
		}
		hosts = append(hosts, h)
	}
	slices.SortFunc(hosts, func(a, b Host) int {
		return strings.Compare(a.Hostname, b.Hostname)
	})
	return hosts, nil
}

// SelfPublicDNS reads this box's own public DNS from ~/.devbox-config, so the
// dispatcher can recognise itself and run in-process instead of SSHing to
// itself. Returns "" when the file is absent or has no PUBLIC_DNS.
func SelfPublicDNS(path string) string {
	if path == "" {
		path = DevboxConfigPath
	}
	data, err := os.ReadFile(ExpandHome(path))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "PUBLIC_DNS="); ok {
			return strings.Trim(strings.TrimSpace(rest), `"'`)
		}
	}
	return ""
}

// IsSelf reports whether h is the box we are running on. It compares the public
// DNS from ~/.devbox-config first (the authoritative identity) and falls back
// to the OS hostname.
func (h Host) IsSelf(selfDNS, selfHostname string) bool {
	if selfDNS != "" && strings.EqualFold(h.PublicDNS, selfDNS) {
		return true
	}
	return selfHostname != "" && strings.EqualFold(h.Hostname, selfHostname)
}

// ExpandHome resolves a leading ~ against the current user's home directory.
func ExpandHome(path string) string {
	if strings.HasPrefix(path, "~/") || path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}

// SanitizeSlug makes a value safe as one path segment. A repo slug or a GitHub
// issue identifier contains "/" and "#", which must not become directories.
func SanitizeSlug(s string) string {
	return strings.NewReplacer("/", "-", string(filepath.Separator), "-", "#", "-", " ", "-").Replace(s)
}
