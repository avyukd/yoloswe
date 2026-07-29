package prdozer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// FleetDir holds one JSON file per registered devbox.
const FleetDir = "~/magent/fleet"

// DevboxConfigPath identifies the box prdozer is currently running on.
const DevboxConfigPath = "~/.devbox-config"

// FleetHost is one devbox as recorded in the registry.
//
// These are PROVISIONING FACTS ONLY, never liveness — a deleted box keeps its
// file, and roles/cron_style reflect what was true at registration. Every
// candidate must therefore be probed before it is dispatched to.
type FleetHost struct {
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
func (h FleetHost) Target() string {
	if h.SSHUser == "" {
		return h.PublicDNS
	}
	return h.SSHUser + "@" + h.PublicDNS
}

// HasNVMe reports whether this host is expected to have a /mnt/nvme volume.
// Only the Azure box does, so probing keys off cloud rather than assuming the
// mount exists everywhere.
func (h FleetHost) HasNVMe() bool {
	return strings.EqualFold(h.Cloud, "azure")
}

// LoadFleet reads every *.json in dir, sorted by hostname for deterministic
// ordering. An unparseable file is an error rather than a silent skip: a
// malformed registry entry means the fleet view is wrong, and silently
// dispatching to a subset is worse than failing loudly.
func LoadFleet(dir string) ([]FleetHost, error) {
	root := ExpandHome(dir)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read fleet dir %s: %w", root, err)
	}
	var hosts []FleetHost
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(root, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		var h FleetHost
		if err := json.Unmarshal(data, &h); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if h.PublicDNS == "" {
			return nil, fmt.Errorf("%s: public_dns is required", path)
		}
		hosts = append(hosts, h)
	}
	slices.SortFunc(hosts, func(a, b FleetHost) int {
		return strings.Compare(a.Hostname, b.Hostname)
	})
	return hosts, nil
}

// SelfPublicDNS reads this box's own public DNS from ~/.devbox-config, so the
// dispatcher can recognise itself and run in-process instead of SSHing to
// itself. Returns "" when the file is absent or has no PUBLIC_DNS.
func SelfPublicDNS(path string) string {
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

// IsSelf reports whether h is the box we are running on. It compares the
// public DNS from ~/.devbox-config first (the authoritative identity) and
// falls back to the OS hostname.
func (h FleetHost) IsSelf(selfDNS, selfHostname string) bool {
	if selfDNS != "" && strings.EqualFold(h.PublicDNS, selfDNS) {
		return true
	}
	return selfHostname != "" && strings.EqualFold(h.Hostname, selfHostname)
}
