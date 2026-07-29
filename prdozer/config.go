package prdozer

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level prdozer configuration.
type Config struct {
	WorkDir      string        `yaml:"work_dir"`
	BaseBranch   string        `yaml:"base_branch"`
	Agent        AgentConfig   `yaml:"agent"`
	Source       SourceConfig  `yaml:"source"`
	Polish       PolishConfig  `yaml:"polish"`
	Backoff      BackoffConfig `yaml:"backoff"`
	MaxBudgetUSD float64       `yaml:"max_budget_usd"`
	PollInterval time.Duration `yaml:"poll_interval"`
}

// AgentConfig selects the agent backend.
type AgentConfig struct {
	Model string `yaml:"model"`
}

// SourceConfig controls how PRs are discovered.
type SourceConfig struct {
	Mode          SourceMode   `yaml:"mode"`           // single | list | all
	PRs           []int        `yaml:"prs"`            // explicit PR numbers (single/list mode)
	Filter        SourceFilter `yaml:"filter"`         // discovery filter (all mode)
	MaxConcurrent int          `yaml:"max_concurrent"` // max parallel polish runs
}

// SourceMode is one of "single", "list", or "all".
type SourceMode string

const (
	SourceModeSingle SourceMode = "single"
	SourceModeList   SourceMode = "list"
	SourceModeAll    SourceMode = "all"
)

// SourceFilter narrows the set of PRs in --all mode.
type SourceFilter struct {
	Author        string   `yaml:"author"`         // gh PR query "author:" value (default "@me")
	Labels        []string `yaml:"labels"`         // require ANY of these labels
	ExcludeLabels []string `yaml:"exclude_labels"` // skip PRs carrying ANY of these labels
}

// MergePolicy selects how a mergeable PR is landed. The distinction matters
// because merge-queue repositories reject an explicit strategy flag — the queue
// itself owns the strategy — so passing --squash there fails the merge outright.
type MergePolicy string

const (
	// MergePolicyQueue arms auto-merge and lets the repository's merge queue
	// pick the strategy. `gh pr merge <N> --auto` with NO strategy flag.
	MergePolicyQueue MergePolicy = "queue"
	// MergePolicySquash squashes on merge. Only valid on repos without a
	// merge queue.
	MergePolicySquash MergePolicy = "squash"
	// MergePolicyRebase rebases on merge, for repos requiring linear history.
	MergePolicyRebase MergePolicy = "rebase"
	// MergePolicyNotify never merges — it reports and stops. This is the safe
	// default: opting a repo into real merging is a deliberate, watched step.
	MergePolicyNotify MergePolicy = "notify"
)

// Valid reports whether p is a known policy.
func (p MergePolicy) Valid() bool {
	switch p {
	case MergePolicyQueue, MergePolicySquash, MergePolicyRebase, MergePolicyNotify:
		return true
	}
	return false
}

// MergeArgs returns the `gh` argv for merging prNumber under this policy, or
// ok=false when the policy never merges.
//
// Two rules are encoded here and must not be relaxed:
//
//  1. **Never pass --delete-branch.** Two recorded incidents (PRs #6283, #3179)
//     had --delete-branch close a PR *unmerged*, because the flag still
//     executes as a side effect when the merge command itself fails. Repos that
//     want branch cleanup set deleteBranchOnMerge server-side.
//  2. **Never pass a strategy flag under MergePolicyQueue.** A merge-queue repo
//     rejects it with "The merge strategy for main is set by the merge queue".
func (p MergePolicy) MergeArgs(prNumber int) ([]string, bool) {
	n := strconv.Itoa(prNumber)
	switch p {
	case MergePolicyQueue:
		// --auto ARMS auto-merge; the queue lands it later. The caller must
		// keep polling until `.merged` is true rather than treating the
		// successful arm as a completed merge.
		return []string{"pr", "merge", n, "--auto"}, true
	case MergePolicySquash:
		return []string{"pr", "merge", n, "--squash"}, true
	case MergePolicyRebase:
		return []string{"pr", "merge", n, "--rebase"}, true
	default:
		return nil, false
	}
}

// PolishConfig controls the agent invocation per tick.
type PolishConfig struct {
	// PermissionMode is passed to the agent provider. Prdozer is designed for
	// unattended background operation, so the default is "bypass" (no
	// interactive prompts). This is a trust-boundary setting — the agent can
	// invoke any tool available on the host. Set to "default" to force
	// per-tool approval, or to any other value accepted by the provider.
	PermissionMode string `yaml:"permission_mode"`
	// MergePolicy selects how AutoMerge lands the PR. Defaults to
	// MergePolicyNotify so a config that enables auto_merge without naming a
	// policy reports instead of guessing a strategy — guessing wrong is what
	// broke kernel merges.
	MergePolicy  MergePolicy `yaml:"merge_policy"`
	MaxBudgetUSD float64     `yaml:"max_budget_usd"` // overrides top-level budget; 0 inherits
	MaxTurns     int         `yaml:"max_turns"`      // cap turns for /pr-polish session
	// RoundsPerTick caps /pr-polish's INTERNAL round loop, which runs inside a
	// single polish.Run() call. Without a cap one tick absorbs unbounded work —
	// kernel#8227 ran 22 rounds over 64 minutes in ONE tick — and since the
	// divergence guard compares health BETWEEN ticks, a tick that never ends is
	// never guarded. Zero omits the flag and uses the skill's own default.
	RoundsPerTick int  `yaml:"rounds_per_tick"`
	Local         bool `yaml:"local"`      // pass --local to /pr-polish
	AutoMerge     bool `yaml:"auto_merge"` // run gh pr merge when PR is mergeable
	// SelfReview mirrors RepoEntry.SelfReview: on a repo with no automated
	// review bots, /pr-polish must ORIGINATE the review rather than react to
	// one. Every other trigger is reactive, so without this a healthy PR on a
	// bot-less repo is declared done having never been reviewed.
	SelfReview bool `yaml:"self_review"`
}

// BackoffConfig caps how aggressively prdozer keeps retrying after failures.
type BackoffConfig struct {
	MaxConsecutiveFailures int           `yaml:"max_consecutive_failures"`
	Cooldown               time.Duration `yaml:"cooldown"`
	// MaxRoundsWithoutImprovement stops polishing when this many consecutive
	// rounds fail to improve on the best PR health seen so far.
	//
	// The existing brake counts only hard failures, so a run whose rounds all
	// "succeed" while the PR gets worse never trips it. kernel#8227 ran
	// seventeen such rounds: unresolved threads went 6 -> 2 -> 11 and CI went
	// red on errors the polish commits introduced.
	//
	// Zero disables the guard. Default 3, matching the failure brake.
	MaxRoundsWithoutImprovement int `yaml:"max_rounds_without_improvement"`
}

// DefaultConfig returns the built-in defaults with validate() applied so the
// no-config path in callers matches the file-backed path: budget inheritance,
// PRDOZER_PERMISSION_MODE env override, etc. all take effect. validate() on the
// built-in defaults cannot realistically fail (model/workdir are preset), but
// treat any failure as a programming error.
func DefaultConfig() *Config {
	c := defaultConfig()
	if err := c.validate(); err != nil {
		panic(fmt.Sprintf("prdozer: DefaultConfig failed to validate: %v", err))
	}
	return &c
}

func defaultConfig() Config {
	return Config{
		Agent:        AgentConfig{Model: "sonnet"},
		WorkDir:      ".",
		BaseBranch:   "main",
		PollInterval: 30 * time.Minute,
		MaxBudgetUSD: 50.0,
		Source: SourceConfig{
			Mode:          SourceModeAll,
			Filter:        SourceFilter{Author: "@me", ExcludeLabels: []string{"wip", "do-not-watch"}},
			MaxConcurrent: 3,
		},
		Polish: PolishConfig{
			Local:          false,
			AutoMerge:      false,
			MaxTurns:       100,
			RoundsPerTick:  3,
			PermissionMode: "bypass",
			MergePolicy:    MergePolicyNotify,
			// MaxBudgetUSD left at zero so validate() inherits the top-level value.
		},
		Backoff: BackoffConfig{
			MaxConsecutiveFailures:      3,
			MaxRoundsWithoutImprovement: 3,
			Cooldown:                    2 * time.Hour,
		},
	}
}

// LoadConfig reads and parses a prdozer YAML config file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := defaultConfig()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.WorkDir = ExpandHome(cfg.WorkDir)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if c.Agent.Model == "" {
		return fmt.Errorf("agent.model is required")
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 30 * time.Minute
	}
	if c.Source.MaxConcurrent <= 0 {
		c.Source.MaxConcurrent = 3
	}
	switch c.Source.Mode {
	case SourceModeSingle, SourceModeList, SourceModeAll, "":
	default:
		return fmt.Errorf("source.mode %q is invalid (want single, list, or all)", c.Source.Mode)
	}
	switch c.Source.Mode {
	case SourceModeSingle, SourceModeList:
		if len(c.Source.PRs) == 0 {
			return fmt.Errorf("source.mode %q requires source.prs to be non-empty", c.Source.Mode)
		}
	case SourceModeAll:
		// Fail loudly when the user supplied an explicit list of PR numbers in
		// a mode that ignores them. defaultConfig() seeds mode=all, so a YAML
		// that sets source.prs without mode would silently fall through to
		// discovery — watching the wrong PR set.
		if len(c.Source.PRs) > 0 {
			return fmt.Errorf("source.mode \"all\" does not accept source.prs (got %d entries); set source.mode to \"single\" or \"list\"", len(c.Source.PRs))
		}
	}
	if c.Source.Mode == SourceModeAll && c.Source.Filter.Author == "" {
		c.Source.Filter.Author = "@me"
	}
	// Nested polish budget inherits the top-level value when unset.
	if c.Polish.MaxBudgetUSD <= 0 && c.MaxBudgetUSD > 0 {
		c.Polish.MaxBudgetUSD = c.MaxBudgetUSD
	}
	if c.Polish.PermissionMode == "" {
		c.Polish.PermissionMode = "bypass"
	}
	if c.Polish.MergePolicy == "" {
		c.Polish.MergePolicy = MergePolicyNotify
	}
	if !c.Polish.MergePolicy.Valid() {
		return fmt.Errorf("polish.merge_policy %q is invalid (want queue, squash, rebase, or notify)", c.Polish.MergePolicy)
	}
	if envMode := strings.TrimSpace(os.Getenv("PRDOZER_PERMISSION_MODE")); envMode != "" {
		c.Polish.PermissionMode = envMode
	}
	if err := ValidateWorkDir(c.WorkDir); err != nil {
		return err
	}
	return nil
}

// ValidateWorkDir checks that path exists and is a directory (skips "" and ".").
func ValidateWorkDir(path string) error {
	if path != "" && path != "." {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("work_dir %q: %w", path, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("work_dir %q is not a directory", path)
		}
	}
	return nil
}

// ExpandHome replaces a leading ~ with the user's home directory.
func ExpandHome(path string) string {
	if strings.HasPrefix(path, "~/") || path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}
