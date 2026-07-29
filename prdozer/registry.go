package prdozer

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"
)

// Layout describes how a repo's checkouts are arranged on disk, which decides
// how an ephemeral babysit worktree is created.
type Layout string

const (
	// LayoutWT is the bare-clone + worktree arrangement (`.bare` plus sibling
	// worktree directories), where `git worktree add` is cheap.
	LayoutWT Layout = "wt"
	// LayoutPlain is a single ordinary clone with no worktree support, where a
	// babysit run instead clones with --reference to share the object store.
	LayoutPlain Layout = "plain"
)

// Valid reports whether l is a known layout.
func (l Layout) Valid() bool {
	return l == LayoutWT || l == LayoutPlain
}

// Registry maps repositories to everything a babysit run needs to know about
// them: where their checkouts live, which flow to run, and how (or whether) to
// merge. It is loaded from ~/magent/prdozer/registry.yaml.
type Registry struct {
	Repos    map[string]RepoEntry `yaml:"repos"`
	Defaults RepoEntry            `yaml:"defaults"`
	// Notify is fleet-wide: the destination is a property of the operator, not
	// of the repository. Per-repo slack_target selects the recipient within it.
	Notify NotifyConfig `yaml:"notify"`
}

// RepoEntry is the per-repository configuration. Zero-valued fields inherit
// from Defaults during Resolve.
type RepoEntry struct {
	// WorktreeRoot is where this repo's checkouts live. It may be empty at
	// load time — several repos referenced in docs have no local clone — and
	// is only fatal when that repo is actually targeted.
	WorktreeRoot string `yaml:"worktree_root"`
	Layout       Layout `yaml:"layout"`
	BaseBranch   string `yaml:"base_branch"`
	// Flow names the strategy used to bring the PR to green. Only "pr-polish"
	// is implemented; it stays a string so another flow can be added without a
	// schema change.
	Flow        string      `yaml:"flow"`
	MergePolicy MergePolicy `yaml:"merge_policy"`
	// SlackTarget selects the notification destination, e.g. "@ming".
	SlackTarget string `yaml:"slack_target"`
	Model       string `yaml:"model"`
	// RequiredChecks names the checks that must pass. Prefer the aggregating
	// gate job over its individual sub-jobs.
	RequiredChecks []string `yaml:"required_checks"`
	// MergeRework declares the rounds run after a failed merge. A repo that
	// sets this REPLACES the default rounds entirely rather than appending, so
	// a repo-specific playbook is never diluted by a generic one.
	MergeRework  StepSpec      `yaml:"merge_rework"`
	PollInterval time.Duration `yaml:"poll_interval"`
	MaxBudgetUSD float64       `yaml:"max_budget_usd"`
}

// StepSpec is a sequence of rounds run as one logical step. It mirrors
// jiradozer's StepConfig/RoundConfig shape so the mental model transfers.
type StepSpec struct {
	Model  string      `yaml:"model"`
	Effort string      `yaml:"effort"`
	Rounds []RoundSpec `yaml:"rounds"`
}

// Empty reports whether this step has nothing to run.
func (s StepSpec) Empty() bool { return len(s.Rounds) == 0 }

// RoundSpec is exactly one of an agent prompt or a shell command.
type RoundSpec struct {
	Prompt  string `yaml:"prompt"`
	Command string `yaml:"command"`
}

// IsCommand reports whether this round runs a shell command rather than an
// agent session.
func (r RoundSpec) IsCommand() bool { return r.Command != "" }

// ReworkData is the template context available to merge_rework rounds. Field
// names are part of the config contract — renaming one breaks every registry
// template that references it.
type ReworkData struct {
	Repo string
	// MergeError is the verbatim gh stderr. The agent needs the real message:
	// "dequeued", "not mergeable" and "strategy rejected" call for completely
	// different fixes.
	MergeError  string
	Branch      string
	PRURL       string
	MergePolicy string
	// PrevOutput is the output of the preceding round, so a command round can
	// feed evidence to the agent round after it.
	PrevOutput string
	PRNumber   int
	Attempt    int
}

// sampleReworkData supplies non-zero values so eager validation traverses
// {{- if .X}} branches that a zero-value pass would skip.
var sampleReworkData = ReworkData{
	Repo:        "sycamore-labs/kernel",
	MergeError:  "Pull request is in an unmergeable state",
	Branch:      "feature/example",
	PRURL:       "https://github.com/sycamore-labs/kernel/pull/8123",
	MergePolicy: string(MergePolicyQueue),
	PrevOutput:  "MERGEABLE",
	PRNumber:    8123,
	Attempt:     2,
}

// DefaultRegistryPath is where the fleet-shared registry lives, beside the
// existing ~/magent/jdozer/ configs.
const DefaultRegistryPath = "~/magent/prdozer/registry.yaml"

// LoadRegistry reads and validates a registry YAML file. Validation is eager:
// every prompt/command template in every repo is parsed and executed against
// both a zero-value and a filled sample, so a typo behind a conditional branch
// cannot stay hidden until the one moment a merge actually fails.
func LoadRegistry(path string) (*Registry, error) {
	data, err := os.ReadFile(ExpandHome(path))
	if err != nil {
		return nil, fmt.Errorf("read registry: %w", err)
	}
	var r Registry
	if err := yaml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse registry: %w", err)
	}
	if err := r.validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

func (r *Registry) validate() error {
	if err := validateEntry("defaults", r.Defaults); err != nil {
		return err
	}
	// Iterate in sorted order so a registry with several bad entries always
	// reports the same one first.
	for _, name := range r.RepoNames() {
		if err := validateEntry(name, r.Repos[name]); err != nil {
			return err
		}
	}
	return nil
}

func validateEntry(name string, e RepoEntry) error {
	if e.MergePolicy != "" && !e.MergePolicy.Valid() {
		return fmt.Errorf("%s: merge_policy %q is invalid (want queue, squash, rebase, or notify)", name, e.MergePolicy)
	}
	if e.Layout != "" && !e.Layout.Valid() {
		return fmt.Errorf("%s: layout %q is invalid (want wt or plain)", name, e.Layout)
	}
	return validateStepSpec(name+".merge_rework", e.MergeRework)
}

func validateStepSpec(label string, s StepSpec) error {
	for i, round := range s.Rounds {
		switch {
		case round.Prompt != "" && round.Command != "":
			return fmt.Errorf("%s.rounds[%d]: set exactly one of prompt or command, not both", label, i)
		case round.Prompt == "" && round.Command == "":
			return fmt.Errorf("%s.rounds[%d]: set exactly one of prompt or command", label, i)
		case round.Prompt != "":
			if err := validateReworkTemplate(fmt.Sprintf("%s.rounds[%d].prompt", label, i), round.Prompt); err != nil {
				return err
			}
		default:
			if err := validateReworkTemplate(fmt.Sprintf("%s.rounds[%d].command", label, i), round.Command); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateReworkTemplate parses tmpl once and executes it against a zero-value
// and a filled sample. The two passes matter: a zero-value pass alone skips
// {{- if .X}} branches, which is exactly where a typo hides.
func validateReworkTemplate(label, tmpl string) error {
	t, err := template.New(label).Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return fmt.Errorf("%s template: %w", label, err)
	}
	for _, sample := range []ReworkData{{}, sampleReworkData} {
		if err := t.Execute(io.Discard, sample); err != nil {
			return fmt.Errorf("%s template: %w", label, err)
		}
	}
	return nil
}

// RepoNames returns the configured repositories in sorted order.
func (r *Registry) RepoNames() []string {
	names := make([]string, 0, len(r.Repos))
	for name := range r.Repos {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Resolve returns the fully-merged entry for ownerRepo, with Defaults filled in
// beneath the repo's own settings.
//
// An unknown repo is an error listing the known keys rather than a silent
// fallback to defaults: a typo'd repo would otherwise inherit
// merge_policy "notify" and look like it simply never finished.
func (r *Registry) Resolve(ownerRepo string) (RepoEntry, error) {
	e, ok := r.Repos[ownerRepo]
	if !ok {
		return RepoEntry{}, fmt.Errorf("repo %q is not in the registry (known: %s)",
			ownerRepo, strings.Join(r.RepoNames(), ", "))
	}
	return r.merged(e), nil
}

// merged fills zero-valued fields of e from Defaults and applies built-in
// fallbacks.
func (r *Registry) merged(e RepoEntry) RepoEntry {
	d := r.Defaults
	if e.WorktreeRoot == "" {
		e.WorktreeRoot = d.WorktreeRoot
	}
	if e.Layout == "" {
		e.Layout = d.Layout
	}
	if e.BaseBranch == "" {
		e.BaseBranch = d.BaseBranch
	}
	if e.Flow == "" {
		e.Flow = d.Flow
	}
	if e.MergePolicy == "" {
		e.MergePolicy = d.MergePolicy
	}
	if e.SlackTarget == "" {
		e.SlackTarget = d.SlackTarget
	}
	if e.Model == "" {
		e.Model = d.Model
	}
	if len(e.RequiredChecks) == 0 {
		e.RequiredChecks = d.RequiredChecks
	}
	// merge_rework REPLACES rather than appends: a repo that declares its own
	// rounds gets exactly those, never its rounds plus the generic default.
	if e.MergeRework.Empty() {
		e.MergeRework = d.MergeRework
	}
	if e.PollInterval == 0 {
		e.PollInterval = d.PollInterval
	}
	if e.MaxBudgetUSD == 0 {
		e.MaxBudgetUSD = d.MaxBudgetUSD
	}

	// Built-in fallbacks for anything still unset.
	if e.Layout == "" {
		e.Layout = LayoutWT
	}
	if e.BaseBranch == "" {
		e.BaseBranch = "main"
	}
	if e.Flow == "" {
		e.Flow = "pr-polish"
	}
	// Default to the policy that never merges. Opting a repo into real
	// merging must be an explicit, deliberate act.
	if e.MergePolicy == "" {
		e.MergePolicy = MergePolicyNotify
	}
	if e.PollInterval == 0 {
		e.PollInterval = 20 * time.Minute
	}
	e.WorktreeRoot = ExpandHome(e.WorktreeRoot)
	return e
}

// CheckUsable reports whether this entry has everything needed to actually run.
// It is deliberately separate from load-time validation: a registry listing a
// repo with no local clone must load fine and only fail when that repo is the
// one being targeted.
func (e RepoEntry) CheckUsable(ownerRepo string) error {
	if e.WorktreeRoot == "" {
		return fmt.Errorf("repo %q has no worktree_root configured; add one to the registry or clone the repo locally", ownerRepo)
	}
	info, err := os.Stat(e.WorktreeRoot)
	if err != nil {
		return fmt.Errorf("repo %q worktree_root %q: %w", ownerRepo, e.WorktreeRoot, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("repo %q worktree_root %q is not a directory", ownerRepo, e.WorktreeRoot)
	}
	if e.Flow != "pr-polish" {
		return fmt.Errorf("repo %q flow %q is not implemented (want pr-polish)", ownerRepo, e.Flow)
	}
	return nil
}

// RenderRound renders a round's template with the supplied data.
func RenderRound(tmpl string, data ReworkData) (string, error) {
	t, err := template.New("round").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse round template: %w", err)
	}
	var sb strings.Builder
	if err := t.Execute(&sb, data); err != nil {
		return "", fmt.Errorf("render round template: %w", err)
	}
	return sb.String(), nil
}

// ParsePRRef splits an "owner/repo#123" reference into its parts. Both halves
// are required: a bare number would leave the repo ambiguous, and dispatching
// the wrong repo's PR is not a recoverable mistake.
func ParsePRRef(ref string) (ownerRepo string, prNumber int, err error) {
	ref = strings.TrimSpace(ref)
	owner, num, ok := strings.Cut(ref, "#")
	if !ok {
		return "", 0, fmt.Errorf("PR reference %q must be of the form owner/repo#123", ref)
	}
	owner = strings.TrimSpace(owner)
	if strings.Count(owner, "/") != 1 || strings.HasPrefix(owner, "/") || strings.HasSuffix(owner, "/") {
		return "", 0, fmt.Errorf("PR reference %q must name a repo as owner/repo", ref)
	}
	n, convErr := parsePositiveInt(strings.TrimSpace(num))
	if convErr != nil {
		return "", 0, fmt.Errorf("PR reference %q: %w", ref, convErr)
	}
	return owner, n, nil
}

func parsePositiveInt(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("missing PR number")
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid PR number %q", s)
		}
		n = n*10 + int(c-'0')
	}
	if n == 0 {
		return 0, fmt.Errorf("PR number must be positive")
	}
	return n, nil
}
