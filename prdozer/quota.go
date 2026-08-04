package prdozer

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

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
