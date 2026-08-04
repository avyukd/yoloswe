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
	// LastActionTransient means the polish round died on a provider-side
	// failure — an API 5xx, a rate limit — rather than on anything about this PR.
	//
	// It deliberately neither increments nor resets ConsecutiveFailures. Not
	// increment, because the failure brake exists to stop a run that is not
	// converging, and a 529 is not evidence of that: three of them spent
	// kernel#8031's entire brake budget and imposed a 2h cooldown while the
	// branch was fine. Not reset, because a provider blip in the middle of a
	// genuine failure streak must not launder that streak back to zero.
	//
	// A force-completed turn is NOT one of these; see LastActionStalled.
	LastActionTransient LastAction = "transient"
	// LastActionStalled means the round was cut off because OUR grace period
	// expired while the agent still had work in flight (reason grace_forced).
	//
	// Split out of LastActionTransient, which it used to share. The two look
	// alike — both arrive as a "transient CLI error" and neither is a verdict on
	// the code — but they differ in the one way the brake cares about:
	//
	//   http_5xx     the PROVIDER was down. Nothing local changed, the next
	//                attempt is genuinely likely to succeed, and retrying costs
	//                one round. Exempting it is right (kernel#8031).
	//   grace_forced OUR deadline fired against a local condition that is still
	//                there on the next attempt. Retrying re-runs the round from
	//                scratch and hits the same wall.
	//
	// Measured on kernel#8374: three invocations at 22:15, 22:45 and 23:17 each
	// armed a /pr-polish reviewer background join, each was force-completed ten
	// minutes later, and each was exempted from the brake. 62 minutes and three
	// full bootstraps produced zero completed rounds, and because the exemption
	// also kept PolishRounds at zero, no other guard could see it either.
	//
	// So this counts toward ConsecutiveFailures, which makes the existing
	// max_consecutive_failures/cooldown brake apply. It is not terminal: the
	// cooldown is the brake, and a stall that clears on its own still recovers.
	LastActionStalled LastAction = "stalled"
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
	// OnceRoundsDone keys the polish rounds marked `once: true` that this PR has
	// already completed (RoundSpec.onceKey). It gates PolishRequest.
	//
	// Per round, not one flag for the whole step: rounds stop at the first error,
	// so a spec with two once rounds whose second fails must keep the first's
	// progress rather than repeat it on the next tick.
	//
	// Per PR, not per process: this file outlives any single babysit run, and
	// that is the behaviour these rounds want. A whole-branch pass like
	// /simplify-branch is worth running on a fresh diff; a PR resumed after
	// twenty polish rounds no longer has one, so a restart must not re-run it.
	//
	// Unioned from PolishResult.RanOnceRounds rather than derived from
	// PolishRounds, which counts only rounds whose result a later tick actually
	// OBSERVED — a failed round, or a snapshot with no thread count, leaves it at
	// zero and would re-run every once round on the next tick.
	OnceRoundsDone map[string]bool `json:"once_rounds_done,omitempty"`
	LastCheckAt    time.Time       `json:"last_check_at,omitempty"`
	CooldownUntil  time.Time       `json:"cooldown_until,omitempty"`
	// LastCooldownCause is the scrubbed reason the CURRENT cooldown window was
	// armed, written at the moment CooldownUntil is set and cleared with it.
	//
	// The reporting path can only reload persisted state, and LastMergeError is
	// not that reason: the cooldown trips on ConsecutiveFailures, which a stall
	// or a rework increments just as a rejected merge does. Quoting
	// LastMergeError there names a stale merge error from earlier in the run, or
	// on a polish-only stall names nothing at all. Kept separate from
	// LastMergeError rather than overloading it because that field is fed
	// verbatim into the rework prompt, where a stall message does not belong.
	LastCooldownCause string `json:"last_cooldown_cause,omitempty"`
	LastSeenHeadSHA   string `json:"last_seen_head_sha,omitempty"`
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
	// BestHealth is the healthiest PR state observed so far;
	// RoundsSinceImprovement counts the rounds run since it was last beaten.
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
	// PolishRounds counts every polish round this run has seen the result of,
	// cumulative and never reset. It is the "how much work did this run do"
	// figure; the divergence guard measures RoundsSinceImprovement instead.
	PolishRounds int `json:"polish_rounds,omitempty"`
	// RoundsSinceImprovement counts consecutive polish rounds that failed to
	// beat BestHealth. Reset to zero whenever a new best is reached.
	RoundsSinceImprovement int `json:"rounds_since_improvement,omitempty"`
	// PolishCommits counts commits prdozer's own rounds have added to this PR,
	// cumulative across every run and never reset.
	//
	// Per PR, not per run, because scope is a property of the PR: kernel#8374
	// reached 23 commits and +2509/-88 across 13 files via two runs of 3 and 4
	// rounds each, so any per-run cap would have caught none of it.
	//
	// This is the one brake that is not a liveness check. All the others watch
	// for a run that FAILS (ConsecutiveFailures), STAGNATES
	// (RoundsSinceImprovement) or never returns (InvocationsSinceRound).
	// #8374 evaded every one of them by succeeding: each round genuinely closed
	// the findings the previous round drew, so BestHealth kept being beaten and
	// the streak kept resetting to zero — it sat at 0 across eight consecutive
	// ticks while the diff grew sixfold. A run can be perfectly healthy by every
	// existing measure and still ratchet scope indefinitely.
	PolishCommits int `json:"polish_commits,omitempty"`
	// BaselineAdditions is the PR's diff size when prdozer first saw it, so
	// growth can be reported against where the PR actually started rather than
	// against zero.
	BaselineAdditions int `json:"baseline_additions,omitempty"`
	// InvocationsSinceRound counts polish invocations that ended WITHOUT the
	// round returning a result. Reset to zero whenever one completes.
	//
	// This is the backstop for a blind spot the other guards share by
	// construction: PolishRounds, RoundsSinceImprovement and BestHealth all
	// advance only when a round returns, so a run whose rounds are eaten before
	// they return is invisible to every one of them. kernel#8374 burned three
	// invocations and 62 minutes with all three reading zero — the divergence
	// guard could not fire because, as far as it could tell, nothing had
	// happened yet.
	//
	// LastActionStalled fixes the grace_forced instance of that; this counter
	// catches the CLASS, so the next failure mode that eats a round before it
	// returns does not reopen the same hole. It counts INVOCATIONS rather than
	// elapsed time because the cost being bounded is per-attempt: each one
	// re-runs bootstrap from scratch.
	InvocationsSinceRound int `json:"invocations_since_round,omitempty"`
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

// Saturated reports whether h is the best state PRHealth can express, so that
// BetterThan(h) is false for EVERY other value.
//
// This is the property that makes a saturated BestHealth stop carrying
// information: green CI cannot be improved on, and 0 unresolved threads cannot
// be beaten because BetterThan is strict (0 < 0 is false). Once a run touches
// this state its BestHealth is frozen there, and RoundsSinceImprovement counts
// rounds rather than measuring divergence.
//
// A method rather than an inline `x == 0 && !y` at the one call site: the
// invariant it encodes is a property of BetterThan, and the two must be read
// and changed together. Adding a field to PRHealth that BetterThan orders on
// obliges this to grow the same clause.
func (h PRHealth) Saturated() bool {
	return h.UnresolvedThreads == 0 && !h.CIFailing
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
