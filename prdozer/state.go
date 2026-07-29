// Package prdozer watches GitHub pull requests and keeps them merge-ready by
// invoking the /pr-polish skill whenever the PR's base moves, CI fails, or new
// review comments arrive. The Go side is orchestration only — the actual code
// fixing is delegated to the agent.
package prdozer

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// maxSeenIDs caps the LastSeen* slices so high-traffic PRs don't grow state
// files without bound. Oldest (lowest by sort order) entries drop first.
const maxSeenIDs = 500

// LastAction is the prdozer-side action recorded after a tick.
type LastAction string

const (
	LastActionInit     LastAction = ""
	LastActionIdle     LastAction = "idle"
	LastActionPolished LastAction = "polished"
	LastActionMerged   LastAction = "merged"
	LastActionClosed   LastAction = "closed"
	LastActionFailed   LastAction = "failed"
	LastActionDryRun   LastAction = "dry_run"
	// LastActionArmed means auto-merge was armed on a merge-queue repo. The
	// PR has NOT landed yet — the queue lands it asynchronously — so this is a
	// non-terminal state that keeps the watcher polling until `.merged` is true.
	LastActionArmed LastAction = "armed"
	// LastActionNeedsHuman is terminal: something only a person can resolve
	// (a missing review approval, a fork PR, merge_policy "notify"). Polishing
	// again cannot help, so the run stops and notifies.
	LastActionNeedsHuman LastAction = "needs_human"
	// LastActionReworked means a failed merge was routed through the
	// merge_rework rounds. It is NOT terminal — the next tick re-snapshots and
	// re-evaluates from scratch — but it deliberately counts as a failure for
	// backoff purposes. See recordSnapshot: merge attempts are unbounded, and
	// the cooldown is the only brake, so rework must never reset the failure
	// counter or the loop churns forever.
	LastActionReworked LastAction = "reworked"
)

// State is the per-PR persisted state, used to detect change between ticks
// and to back off after repeated failures.
type State struct {
	LastCheckAt     time.Time `json:"last_check_at,omitempty"`
	CooldownUntil   time.Time `json:"cooldown_until,omitempty"`
	LastSeenHeadSHA string    `json:"last_seen_head_sha,omitempty"`
	// SelfReviewedSHA is the head SHA prdozer last originated a review for on a
	// self_review repo. Keyed by SHA so the review happens once per commit: an
	// unconditional trigger would re-review an idle PR on every tick forever.
	SelfReviewedSHA string     `json:"self_reviewed_sha,omitempty"`
	LastSeenBaseSHA string     `json:"last_seen_base_sha,omitempty"`
	LastAction      LastAction `json:"last_action,omitempty"`
	Repo            string     `json:"repo,omitempty"`
	// LastMergeError is the verbatim gh stderr from the most recent failed
	// merge, carried across ticks so the rework agent sees the real message.
	LastMergeError string `json:"last_merge_error,omitempty"`
	// BestHealth is the healthiest PR state observed so far; PolishRounds counts
	// rounds run since it was set.
	//
	// Together these detect DIVERGENCE: a PR getting worse round over round
	// rather than better. Observed on kernel#8227, where seventeen polish rounds
	// each fixed things while new findings arrived faster — unresolved threads
	// went 6 -> 2 -> 11 and CI went red on errors the polish commits themselves
	// introduced. Every round reported success, so nothing in the existing
	// backoff (which only counts hard failures) ever fired.
	BestHealth          *PRHealth `json:"best_health,omitempty"`
	LastSeenCommentIDs  []string  `json:"last_seen_comment_ids,omitempty"`
	LastSeenCIRunIDs    []int64   `json:"last_seen_ci_run_ids,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures,omitempty"`
	PRNumber            int       `json:"pr_number"`
	// MergeAttempts counts merge attempts across the whole run, surviving
	// cooldowns so a resumed run keeps climbing (and the Slack message can
	// honestly say "attempt 9") rather than restarting its numbering.
	MergeAttempts int `json:"merge_attempts,omitempty"`
	// CooldownFromAttempt records MergeAttempts as of the last merge-brake
	// cooldown, so the brake measures attempts SINCE that point. Without it a
	// run that resumes past its cooldown would re-trip on every later tick.
	CooldownFromAttempt int `json:"cooldown_from_attempt,omitempty"`
	// PolishRounds counts polish rounds run since BestHealth was set.
	PolishRounds int `json:"polish_rounds,omitempty"`
	// RoundsSinceImprovement counts consecutive polish rounds that failed to
	// beat BestHealth. Reset to zero whenever a new best is reached.
	RoundsSinceImprovement int `json:"rounds_since_improvement,omitempty"`
}

// PRHealth is the cheap, externally-observable measure of whether a PR is
// getting better. Both fields come from data the snapshot already fetches, so
// tracking them costs no extra API calls.
type PRHealth struct {
	// UnresolvedThreads counts review threads that are neither resolved nor
	// outdated — the work a reviewer is still asking for.
	UnresolvedThreads int `json:"unresolved_threads"`
	// CIFailing reports whether the status rollup is FAILURE. A round that
	// turns CI red has made the PR strictly worse even if it resolved threads.
	CIFailing bool `json:"ci_failing"`
}

// BetterThan reports whether h is a strict improvement over other.
//
// Green CI dominates: going from red to green is an improvement even if the
// thread count rose, because a red PR cannot merge at all. Otherwise fewer
// unresolved threads wins. Equal state is NOT an improvement — a round that
// changed nothing is exactly what the divergence guard must count.
func (h PRHealth) BetterThan(other PRHealth) bool {
	if h.CIFailing != other.CIFailing {
		return !h.CIFailing
	}
	return h.UnresolvedThreads < other.UnresolvedThreads
}

// LoadState reads the state file at path. Returns a zero State (no error) when
// the file does not exist.
func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{}, nil
		}
		return nil, fmt.Errorf("read state %s: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	return &s, nil
}

// Save writes the state to path, creating parent directories as needed.
func (s *State) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write state %s: %w", path, err)
	}
	return nil
}

// MergeSeenComments returns true iff the merge added at least one ID.
func (s *State) MergeSeenComments(ids []string) bool {
	merged, changed := mergeSortedCapped(s.LastSeenCommentIDs, ids)
	s.LastSeenCommentIDs = merged
	return changed
}

// MergeSeenRuns returns true iff the merge added at least one ID.
func (s *State) MergeSeenRuns(ids []int64) bool {
	merged, changed := mergeSortedCapped(s.LastSeenCIRunIDs, ids)
	s.LastSeenCIRunIDs = merged
	return changed
}

func mergeSortedCapped[T cmp.Ordered](existing, incoming []T) ([]T, bool) {
	if len(incoming) == 0 {
		return existing, false
	}
	seen := make(map[T]struct{}, len(existing)+len(incoming))
	for _, v := range existing {
		seen[v] = struct{}{}
	}
	changed := false
	for _, v := range incoming {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			changed = true
		}
	}
	if !changed {
		return existing, false
	}
	out := make([]T, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	slices.Sort(out)
	if len(out) > maxSeenIDs {
		out = out[len(out)-maxSeenIDs:]
	}
	return out, true
}

// StatePath returns the canonical state-file path for a given repo and PR.
// Mirrors the layout used by the /pr-polish skill so both files coexist under
// the same project directory.
func StatePath(repo string, prNumber int) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	dir := filepath.Join(home, ".bramble", "projects", fmt.Sprintf("%s-%d", repo, prNumber))
	return filepath.Join(dir, "prdozer-state.json")
}
