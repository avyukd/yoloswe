package prdozer

import (
	"context"

	"github.com/bazelment/yoloswe/fleet"
)

// ClaudeUsage is the shared Claude quota reading. The gate itself lives in
// //fleet: it is a property of the fleet, not of either tool, because the OAuth
// session is shared across every box.
type ClaudeUsage = fleet.ClaudeUsage

const (
	ClaudeLimitsScript      = fleet.ClaudeLimitsScript
	QuotaExhaustedThreshold = fleet.QuotaExhaustedThreshold
)

func CheckClaudeQuota(ctx context.Context) (ClaudeUsage, error) { return fleet.CheckClaudeQuota(ctx) }
