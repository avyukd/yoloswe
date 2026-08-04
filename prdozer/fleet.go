package prdozer

import (
	"context"

	"github.com/bazelment/yoloswe/fleet"
)

// The fleet registry, probing, ranking and tmux handoff now live in the shared
// //fleet module so prdozer and jiradozer cannot drift on the parts that were
// expensive to get right — holding a lease inside the worker, resolving an
// absolute binary path, and setting PATH through a shell rather than tmux -e.
//
// These aliases keep prdozer's own vocabulary intact at its call sites.
type (
	FleetHost        = fleet.Host
	HostHealth       = fleet.HostHealth
	ProbeOptions     = fleet.ProbeOptions
	SSHRunner        = fleet.SSHRunner
	DefaultSSHRunner = fleet.DefaultSSHRunner
)

const (
	FleetDir         = fleet.DefaultFleetDir
	DevboxConfigPath = fleet.DevboxConfigPath
)

// Tool describes prdozer to the shared dispatcher.
var Tool = fleet.Tool{Name: "prdozer", LeaseDir: LeaseDir}

func LoadFleet(dir string) ([]FleetHost, error) { return fleet.Load(dir) }
func SelfPublicDNS(path string) string          { return fleet.SelfPublicDNS(path) }
func ScoreHosts(hosts []HostHealth)             { fleet.ScoreHosts(hosts) }

func ProbeFleet(ctx context.Context, ssh SSHRunner, hosts []FleetHost, opts ProbeOptions) []HostHealth {
	return fleet.Probe(ctx, ssh, Tool, hosts, opts)
}

func PickHost(hosts []HostHealth, opts ProbeOptions) (HostHealth, error) {
	return fleet.PickHost(hosts, Tool, opts)
}

// Eligible reports whether a host can accept a new babysit run.
func Eligible(h HostHealth, opts ProbeOptions) (bool, string) {
	return h.Eligible(Tool, opts)
}
