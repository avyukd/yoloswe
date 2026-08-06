---
name: pr-polish
description: Fully autonomous PR polish loop. Runs N rounds of local bramble review (codex + cursor, optionally + gemini and/or claude), folds in any existing PR comments and CI failures as round-1 input, fixes findings locally, pushes once at the end.
argument-hint: "[--rounds N] [--gemini] [--claude] [--ask]"
disable-model-invocation: true
---

# PR Polish Loop

Review → triage → fix → commit locally each round. Exit when converged (Step 3.g) or round cap hit, then force-push once. No mid-loop pushes.

Helpers: `python3 $SKILL_DIR/scripts/<helper>.py`. `$SKILL_DIR` = directory containing this `SKILL.md`.

| Script | Role |
|---|---|
| `pr_ops.py` | Identity, comments, CI, state I/O, `round-bundle`, `remote-head` |
| `bramble_ops.py` | Goal text, resume ids, triage, envelope recovery |
| `lint_gate.py` | Diff lint (ruff/golangci/eslint) |
| `scope_gate.py` | `scope-hints.json` for bramble |

Missing/error review streams → log as findings with stderr path cited.

## Arguments

| Flag | Default | Meaning |
|---|---|---|
| `--rounds N` | `5` | Up to N additional rounds this invocation. Budget resets on re-invoke. `--rounds 0` = no-op. |
| `--gemini` | off | Extra reviewer (`gemini-3-flash-preview`). ≥2 sources = consensus. Sets `USE_GEMINI=1`. |
| `--claude` | off | Extra reviewer (`claude` backend, `opus`). Highest per-round cost of the four — reach for it on a diff where codex+cursor disagree or keep missing the same class of bug. Sets `USE_CLAUDE=1`. |
| `--ask` / `--interactive` | off | Enable `AskUserQuestion` at gates (Step 3.g). Default: never block. |

## State tracking

`~/.bramble/projects/<repo>-<pr>/pr-polish-state.json` (or `…-branch-<slug>/…`). Never deleted.

`state-*` subcommands take `ctx` = PR number or `branch:<name>`.

| Command | When |
|---|---|
| `state-load` | Read |
| `state-append-round <ctx> <n> <head_before> [--pr-summary "$PR_SUMMARY"] [--base-branch <base>]` | Round start (`--no-verify-head` only when resuming interrupted round). **Pass both on round 1** — each is written once and frozen. `--pr-summary` is the goal's spine (omit it and the goal loses the PR's purpose from round 2 on); `--base-branch` anchors the "Files in this PR" range to the PR's real base (omit it and a PR stacked on a non-default branch is measured against the repo default). |
| `state-finalize-round <ctx> <n> <head_after> <actions.json> [--envelope …]` | Round end |
| `state-mark-complete <ctx> <reason>` | Exit |

Key fields: `rounds[n].comment_actions` (audit trail), `low_only_streak` (convergence), `session_ids` (resume), `pr_summary` (frozen at round 1; the goal's stable scope anchor), `base_branch` (frozen at round 1; the merge base the PR's file list is measured against).

**Actions file** (the `<actions.json>` arg to `state-finalize-round` / `finalize-and-report`): a JSON **array** of action entries, or an object `{"comment_actions": [...]}` — both are accepted. Per entry:
- `action`: one of `fixed`, `false_positive`, `wont_fix`, `ack`, `stale`, `pre_existing`/`flake` (CI only) — validated; an unknown verb is a loud error naming the entry index.
- `severity`: `high`/`medium`/`low`/`nit` or null (lint advisory) — validated.
- `source`: `claude`/`codex`/`cursor`/`gemini`/`lint`/`github-inline`/`github-issue`/`ci`/`sweep`.
- `path` + `line` (code mode) or `section`/`dimension` (design-doc mode); `notes`/`reason`, `comment_id` (for inline replies).
- Optional v2: `spiral_refix`, `invariant` — and any other key passes through untouched.

## Step 0: Bootstrap

```bash
PREFLIGHT=$(python3 $SKILL_DIR/scripts/pr_ops.py preflight)
export BRAMBLE_BIN=$(echo "$PREFLIGHT" | jq -r .bramble_bin)
export SKILL_DIR=$(echo "$PREFLIGHT" | jq -r .skill_dir)
GIT_SYNC=$(echo "$PREFLIGHT" | jq -r .git_sync_path)
if [ "$(echo "$PREFLIGHT" | jq -r '.errors | length')" != "0" ]; then
  echo "$PREFLIGHT" | jq -r '.errors[]' >&2; exit 1
fi
# Non-fatal advisories — print but don't abort. The common one: this PR modifies
# bramble's own review code but BRAMBLE_BIN resolved to a (possibly stale) PATH
# binary. If you see that, build from this branch (`bazel build //bramble/...`)
# and re-export BRAMBLE_BIN=$(pwd)/bazel-bin/bramble/bramble_/bramble before the
# round so the review runs against the code under test.
echo "$PREFLIGHT" | jq -r '.warnings[]? | "[preflight warning] " + .' >&2
python3 $SKILL_DIR/scripts/pr_ops.py identify
```

Pin: `$CTX`, `$STATE_DIR`, `$STATE_FILE`, `$BRANCH`, `$PR_NUMBER`, `$REPO`, `$BASE`
(`identify`'s `base` field — the PR's `baseRefName`, or the detected default
branch in branch-only mode; used for the review's diff scope in Step 3.b). 

`pr_number: null` → skip PR-comment/CI fetch.

## Step 0.5: Resume check

```bash
python3 $SKILL_DIR/scripts/pr_ops.py state-load $CTX
IS_NEW_SERIES=$(python3 $SKILL_DIR/scripts/pr_ops.py state-is-new-series $CTX $ROUND)
```

`IS_NEW_SERIES=1` before `state-append-round`: re-fetch comments/CI, fresh bramble sessions.

| Condition | Action |
|---|---|
| No state | Fresh run |
| `pr_number` mismatch | Step 3.g integrity gate → default `pr-mismatch-abort` |
| Heartbeat stale (>2h) + not completed | `state-mark-abandoned $CTX` |
| HEAD == `last_commit_at_round_start` | Resume interrupted round |
| HEAD differs (fresh heartbeat) | Next round on current HEAD |

`additional_rounds_run = 0` at start; increment each finalized round.

## Step 1: Sync base

Use `$GIT_SYNC` from preflight (not a hardcoded path):

```bash
python3 "$GIT_SYNC" --verbose --no-push
```

`--no-push` required — push only at Step 4.

Dirty tree (no in-progress round to resume) → `state-mark-complete <ctx> dirty-tree-preflight`, exit.
Conflict (exit 2) → `state-mark-complete <ctx> sync-conflict`, Final Summary, exit.

Build `$PR_SUMMARY` (≤10 lines): `git log --oneline origin/<base>..HEAD` + diff-stat, where `<base>` is `identify`'s `base`. Round 1 `--goal` = `$PR_SUMMARY`; later rounds use `round-bundle` / `bramble_ops.py goal`, which **leads with the frozen `$PR_SUMMARY`** and appends the action-history briefing (prior fixed/skipped + the PR's own file list, plus any invariant/streak notes). No inter-round diff is embedded — bramble re-reads the working tree, and a diff pinned to the prior round's HEAD is wrong after a rebase.

Pass the same `<base>` to `state-append-round --base-branch` on round 1 (see Step 3.f/State tracking): the file list is measured `origin/<base>...HEAD`, and it must be anchored to the same merge base `$PR_SUMMARY` was built from.

## Step 2: Fetch PR comments + CI

When `pr_number` not null (also re-fetch when `IS_NEW_SERIES=1` in round loop):

```bash
python3 $SKILL_DIR/scripts/pr_ops.py fetch-comments > $STATE_DIR/pp-comments.json
python3 $SKILL_DIR/scripts/pr_ops.py ci-failed-tests > $STATE_DIR/pp-ci.json
```

Triage reads these only when `IS_NEW_SERIES=1`. Still run bramble every round.

## Step 3: Round loop

```
additional_rounds_run = 0
while additional_rounds_run < --rounds:
  a) WIP commit if dirty
  b) scope_gate → round-bundle → one bg join: launch reviewers (codex+cursor+lint[+gemini][+claude]), wait on exit
  c) triage → action plan
  d) apply fixes
  e) quality gates + local commit if changed (NO push)
  f) finalize round state
  g) convergence check
  additional_rounds_run += 1
```

Header: `## Round N (M / --rounds)` — N absolute, M = `additional_rounds_run + 1`.

**Orchestrator vars** (`$LOG_DIR`, `$CTX`, etc.): substitute concrete values into each Bash call — fresh shell every time, no persistent `$VAR`.

### a) WIP commit

If dirty: `git add -A && git commit -m "pr-polish: round N snapshot"`. Bramble snapshots working tree at launch.

### b) Launch reviewers

Always use `round-bundle` for `$LOG_DIR`, `$GOAL`, resume ids — do not hand-roll attempt index.

```bash
# Opt-in reviewer toggles. These are read in three places below (launch,
# --stream, --envelope) and nothing else sets them, so bind them from the
# invocation flags here — substituting 1/0 as literals like every other
# orchestrator var. Leaving one unset is not neutral: `[ "$USE_CLAUDE" = "1" ]`
# is false, so the reviewer silently never launches and its absence looks
# identical to "you didn't ask for it".
USE_GEMINI=0   # 1 when --gemini was passed
USE_CLAUDE=0   # 1 when --claude was passed

BUNDLE=$(python3 $SKILL_DIR/scripts/pr_ops.py round-bundle "$CTX" {ROUND})
LOG_DIR=$(echo "$BUNDLE" | jq -r .log_dir)
GOAL=$(echo "$BUNDLE" | jq -r .goal_text)
CODEX_RESUME=$(echo "$BUNDLE" | jq -r '.resume_ids.codex')
CURSOR_RESUME=$(echo "$BUNDLE" | jq -r '.resume_ids.cursor')
GEMINI_RESUME=$(echo "$BUNDLE" | jq -r '.resume_ids.gemini')   # used by the --gemini launch
CLAUDE_RESUME=$(echo "$BUNDLE" | jq -r '.resume_ids.claude')   # used by the --claude launch
[ "{ROUND}" = "1" ] && GOAL="$PR_SUMMARY"
mkdir -p "$LOG_DIR"

SCOPE_HINTS=$(python3 $SKILL_DIR/scripts/scope_gate.py --state-dir "$STATE_DIR" 2>"$LOG_DIR/scope-gate-stderr.txt")

# Pin the reviewed range. Without --diff-base the agent infers the diff, and
# the natural inference — `git diff main...HEAD` — is wrong whenever the local
# base branch has drifted: a stale main narrows the diff (the reviewer never
# sees code the PR touched), an advanced main widens it with unrelated
# commits. Measured on a replayed PR: 336 files instead of 22, and the guess
# varied run to run.
#
# The pin is best-effort — a detached HEAD or a missing origin/main leaves it
# empty and every reviewer falls back to inferred scope. That fallback must be
# LOUD. Silently omitting the flag restores the exact failure mode it exists to
# prevent, and an unpinned round is indistinguishable from a pinned one in the
# log, so its findings get compared against pinned rounds as if the ranges
# matched. Do not hard-fail instead: branch contexts (`pr_number: null`,
# `branch:<name>`) legitimately run without a resolvable base, and aborting
# there would break the loop for a case that still reviews usefully.
#
# The block below only warns; it writes no state. When it fires, YOU must add
# an `ack` entry (source `sweep`, notes naming the unpinned scope) to this
# round's actions file in step (f), so the state file says which rounds were
# scoped by inference. Nothing else records it.
DIFF_BASE=$(git merge-base "origin/${BASE:-main}" HEAD 2>/dev/null || true)
DIFF_BASE_ARG=""
if [ -n "$DIFF_BASE" ]; then
  DIFF_BASE_ARG="--diff-base $DIFF_BASE"
else
  echo "[pr-polish] WARNING: no merge-base for origin/${BASE:-main}..HEAD — reviewers run on INFERRED diff scope this round; findings are not range-comparable with pinned rounds" | tee "$LOG_DIR/diff-base-unpinned.txt" >&2
fi

[ "$IS_NEW_SERIES" = "1" ] && [ "$PR_NUMBER" != "null" ] && {
  python3 $SKILL_DIR/scripts/pr_ops.py fetch-comments > $STATE_DIR/pp-comments.json
  python3 $SKILL_DIR/scripts/pr_ops.py ci-failed-tests > $STATE_DIR/pp-ci.json
}
```

Launch every reviewer **inside one `run_in_background` Bash job** (the "join"): the script starts each `bramble code-review` (and the lint gate) with `&`, records their PIDs, then `wait`s on all of them. `wait` returns when *every* child has **exited** — the true all-done signal — and returns promptly if a reviewer crashes without writing an envelope (no hanging to the ceiling on a dead process). The job streams each reviewer's stderr (tee'd to its `-stderr.txt` and to the job's own stdout, so you see per-reviewer progress, including the periodic `[code-review] heartbeat …` lines), and fires **one** completion notification when the join returns. Each reviewer self-kills on inactivity via `--idle-timeout 5m` (a review making steady progress runs as long as it needs; only a stalled backend trips); the outer `timeout 1200` is just an absolute backstop so a wedged process can't outlive the round.

**Wait ONLY for that one join's completion notification — then triage.** The per-reviewer output you see streaming is for visibility only; it is **not** a signal to act. Do not act on any single reviewer finishing, do not Read the envelope/`-stderr.txt` files in a loop, do not `sleep`-poll, do not call `ScheduleWakeup`, and do not end the turn with a text-only "standing by / awaiting notification" reply. You have nothing to do until the join notifies you that every reviewer has exited — acting before then strands the round or spams the log. This skill may run non-interactively (e.g. driven by jiradozer with one bounded agent turn): there is no harness to re-invoke you on a wakeup or task-notification, so a yielded turn strands the round. The single `run_in_background` join is the only sanctioned wait: it blocks in one tool call and returns when all reviewers exit (each bounded by `--idle-timeout 5m`, with a `timeout 1200` absolute backstop).

Arm the join in **one** `run_in_background` Bash call (steps b→c in one turn — no tool calls between launch and the completion notification):

Substitute the concrete `$LOG_DIR`/`$GOAL`/`$SCOPE_HINTS`/`$REPO`/`$PR_NUMBER`/resume-id values into this script (orchestrator vars — fresh shell, no persistent `$VAR`), then `run_in_background` the **whole script as one call**. No `bash -c` wrapper, no nested quoting.

```bash
# One background join: launch each reviewer with `&`, recording every PID into
# the PIDS array, then `wait "${PIDS[@]}"`. The array is the safety rail — the
# join can't desync from the launches (no hand-maintained `wait $A $B $C` line
# to forget a PID on), and skipping a reviewer just means one fewer element.
#
# INVARIANT: every reviewer launch ends with `PIDS+=($!)`. Nothing else touches
# the wait list.
#
# Substitute these orchestrator vars into the script before running (fresh
# shell, no persistent $VAR): {ROUND}, $REPO, $PR_NUMBER, $GOAL, $SCOPE_HINTS,
# $LOG_DIR, $CODEX_RESUME/$CURSOR_RESUME/$GEMINI_RESUME, $BRAMBLE_BIN, $SKILL_DIR.
#
# Per reviewer: output is tee'd to its -stderr.txt AND to this job's stdout
# (prefixed) so per-reviewer progress — incl. periodic `[code-review] heartbeat …`
# lines — streams live. --idle-timeout 5m kills a stalled backend; the outer
# `timeout 1200` is an absolute backstop (lint gets 120s — it's a fast static
# pass and must not hold the join for 20 minutes). `set -o pipefail` keeps each
# subshell's status the reviewer's real one (not the trailing `sed`'s 0).
#
# NOTE: the join exit code is NOT how reviewer failure is detected — `wait` with
# multiple PIDs returns only the LAST one's status, so a crashed reviewer can be
# masked by a later success. The join's only job is to block until ALL reviewers
# have EXITED (one completion notification). Per-reviewer failure is detected
# AFTER the join, in triage: a crashed/timed-out reviewer leaves no/empty
# envelope → `recover-envelope` + a `stream-missing` finding. So failures
# surface via envelopes, not the join's exit status.
PIDS=()

( set -o pipefail; BRAMBLE_RUN_TAG=pr-polish:$REPO:$PR_NUMBER:codex:r{ROUND} \
  timeout 1200 $BRAMBLE_BIN code-review --backend codex --model gpt-5.6-luna --effort medium \
    --skip-test-execution --verbose --idle-timeout 5m \
    --goal "$GOAL" --scope-hints-file "$SCOPE_HINTS" $DIFF_BASE_ARG \
    ${CODEX_RESUME:+--resume-session-id "$CODEX_RESUME"} \
    --envelope-file "$LOG_DIR/codex-envelope.json" \
  2>&1 | tee "$LOG_DIR/codex-stderr.txt" | sed 's/^/[codex] /' ) &
PIDS+=($!)

( set -o pipefail; BRAMBLE_RUN_TAG=pr-polish:$REPO:$PR_NUMBER:cursor:r{ROUND} \
  timeout 1200 $BRAMBLE_BIN code-review --backend cursor --model composer-2.5 \
    --skip-test-execution --verbose --idle-timeout 5m \
    --goal "$GOAL" --scope-hints-file "$SCOPE_HINTS" $DIFF_BASE_ARG \
    ${CURSOR_RESUME:+--resume-session-id "$CURSOR_RESUME"} \
    --envelope-file "$LOG_DIR/cursor-envelope.json" \
  2>&1 | tee "$LOG_DIR/cursor-stderr.txt" | sed 's/^/[cursor] /' ) &
PIDS+=($!)

( set -o pipefail; timeout 120 python3 $SKILL_DIR/scripts/lint_gate.py \
    --state-dir "$STATE_DIR" --round {ROUND} --log-dir "$LOG_DIR" \
  2>&1 | tee "$LOG_DIR/lint-stderr.txt" | sed 's/^/[lint] /' ) &
PIDS+=($!)

# --gemini only: an extra reviewer launched the same way; it appends to PIDS
# like any other, so when --gemini is off it's simply absent — the wait can't
# break.
if [ "$USE_GEMINI" = "1" ]; then
  ( set -o pipefail; BRAMBLE_RUN_TAG=pr-polish:$REPO:$PR_NUMBER:gemini:r{ROUND} \
    timeout 1200 $BRAMBLE_BIN code-review --backend gemini --model gemini-3-flash-preview \
      --skip-test-execution --verbose --idle-timeout 5m \
      --goal "$GOAL" --scope-hints-file "$SCOPE_HINTS" $DIFF_BASE_ARG \
      ${GEMINI_RESUME:+--resume-session-id "$GEMINI_RESUME"} \
      --envelope-file "$LOG_DIR/gemini-envelope.json" \
    2>&1 | tee "$LOG_DIR/gemini-stderr.txt" | sed 's/^/[gemini] /' ) &
  PIDS+=($!)
fi

# --claude only: same shape again. Opus is the slowest and priciest of the
# four, so it stays opt-in rather than joining the default codex+cursor pair.
if [ "$USE_CLAUDE" = "1" ]; then
  ( set -o pipefail; BRAMBLE_RUN_TAG=pr-polish:$REPO:$PR_NUMBER:claude:r{ROUND} \
    timeout 1200 $BRAMBLE_BIN code-review --backend claude --model opus \
      --skip-test-execution --verbose --idle-timeout 5m \
      --goal "$GOAL" --scope-hints-file "$SCOPE_HINTS" $DIFF_BASE_ARG \
      ${CLAUDE_RESUME:+--resume-session-id "$CLAUDE_RESUME"} \
      --envelope-file "$LOG_DIR/claude-envelope.json" \
    2>&1 | tee "$LOG_DIR/claude-stderr.txt" | sed 's/^/[claude] /' ) &
  PIDS+=($!)
fi

# Join on EVERY launched reviewer so triage never starts while a reviewer is
# still running or has yet to write its envelope.
wait "${PIDS[@]}"
```

The single completion notification = every reviewer has exited (each bounded by `--idle-timeout 5m` for stalls and a `timeout 1200` absolute backstop; lint by `timeout 120`). The `wait` returns once all PIDs exit, and a crashed reviewer exits immediately, so a dead process never hangs the round. All resume ids (`CODEX_RESUME`/`CURSOR_RESUME`/`GEMINI_RESUME`/`CLAUDE_RESUME`) come from the round-prep `round-bundle` block above.

Before triage: `recover-envelope` on each stream path (idempotent). A reviewer that exited without a valid envelope → `stream-missing` finding, not a deadlock.

### c) Triage

```bash
python3 $SKILL_DIR/scripts/bramble_ops.py triage $STATE_FILE \
  --stream codex=$LOG_DIR/codex-envelope.json \
  --stream cursor=$LOG_DIR/cursor-envelope.json \
  --stream lint=$LOG_DIR/lint-envelope.json \
  $( [ "$USE_GEMINI" = "1" ] && echo --stream gemini=$LOG_DIR/gemini-envelope.json ) \
  $( [ "$USE_CLAUDE" = "1" ] && echo --stream claude=$LOG_DIR/claude-envelope.json ) \
  $( [ "$IS_NEW_SERIES" = "1" ] && [ "$PR_NUMBER" != "null" ] && \
     echo --pr-comments $STATE_DIR/pp-comments.json --ci-failures $STATE_DIR/pp-ci.json )
```

Buckets → `must_fix` / `consider_fix` / `batch_ack` / `batch_stale` / `escalate`.

**They are nested under `.action_plan`, not top-level.** The top level carries
`total`, `unique`, `consensus`, `single_critical`, `single_medium`, `low_acks`,
`spiral_matches`. Reading `.must_fix` instead of `.action_plan.must_fix` yields
`null` for every bucket, which looks exactly like an empty plan — and Step 3.g
treats an empty plan as convergence, so the loop exits reporting success with
every finding unread.

`triage` prints a census to stderr for this reason; check it against the JSON
before concluding anything is empty:

```
[triage] action_plan: must_fix=0 consider_fix=10 batch_ack=3 batch_stale=0 escalate=0 total=13
```

`total=13` with all buckets `0` means you read the wrong key, not that there is
nothing to do. To read the plan:

```bash
jq '.action_plan | {must_fix: (.must_fix|length), consider_fix: (.consider_fix|length),
                    batch_ack: (.batch_ack|length), escalate: (.escalate|length)}' triage.json
```

**Ownership:** own pre-existing code in touched files. `must_fix` unless false positive (cite file:line). Low/nit → fix if trivial else `ack`. Skips: `false_positive`, `wont_fix`, `stale`.

**Scope contract — the PR's remit is fixed at round 1.** "Touched files" means files the PR touched when you *first saw it*, not files a later round dragged in. Otherwise ownership ratchets: each round's fix touches new files, which the next round then owns, and the PR grows without limit — while every round looks productive, because each one really is closing the previous round's findings.

Before fixing anything outside the round-1 file set, ask whether this finding is *this PR's job*. Usually it isn't:

- **Outside the round-1 files** → `wont_fix`, and say where it belongs ("not in this PR's diff — worth a ticket against `<owner>`"). You already do this when you notice; make it the rule rather than the lucky round.
- **A flaw in a fix an earlier round of this same run made** → fix it, that is genuinely yours. But if the same subsystem needs a *third* correction, stop and escalate: three passes at one mechanism means the design is wrong, and the fourth commit will not settle it.
- **New machinery to support a fix** (a cache, a latch, a reaper) → prefer the smaller change that needs none. New machinery draws new findings, which is how a two-file fix becomes a thirteen-file one.

When you decline on scope, record it as `wont_fix` with the reason — a declined finding is a real outcome, not a gap.

**Invariants:** same `invariant` from ≥2 reviewers → consensus on all sites. Prefer producer-side fix.

**Spirals:** single-source may auto-demote to stale if evidence gone (±10 lines) or cited line was in prior round's diff. Multi-source → escalate. Default (no `--ask`): re-fix once (`spiral_refix: true`), stop on 2nd recurrence.

Empty plan (`.action_plan.must_fix` and `.action_plan.consider_fix` both empty,
**and** the stderr census agrees — `total=0`, or every finding accounted for in
`batch_ack`/`batch_stale`) → converged, Step 3.g. A non-zero `total` with empty
buckets is a misread, not convergence.

### d) Apply fixes

Fix the invariant, not the citation: name rule → scan sibling sites → fix at shallowest layer (line, helper, producer). Group cross-backend findings by underlying problem. Update docs/tests in same commit when contract changes. Log extra sites as `source: "sweep"`. Record every finding in `comment_actions`; don't silently drop stale items. GitHub inline replies happen in `state-finalize-round`.

### e) Quality gates + commit

Skip if no file changes. Run project gates, then commit locally (`pr-polish round {ROUND}: …`). **No push.** Check sibling sites/tests/docs before commit; record intentional gaps as `ack`.

### f) Finalize

```bash
python3 $SKILL_DIR/scripts/pr_ops.py finalize-and-report $CTX $ROUND $(git rev-parse HEAD) \
  $STATE_DIR/actions-r$ROUND.json \
  --envelope codex=$LOG_DIR/codex-envelope.json \
  --envelope cursor=$LOG_DIR/cursor-envelope.json \
  --envelope lint=$LOG_DIR/lint-envelope.json \
  $( [ "$USE_GEMINI" = "1" ] && echo --envelope gemini=$LOG_DIR/gemini-envelope.json ) \
  $( [ "$USE_CLAUDE" = "1" ] && echo --envelope claude=$LOG_DIR/claude-envelope.json )
```

(`state-finalize-round` works too; `finalize-and-report` adds round summary hints.)

### g) Convergence

Stop when any:
- Zero findings
- Empty triage plan
- `low_only_streak >= 2` (every low fixed or `ack`/`wont_fix` with reason)
- Top finding documented false positive + prior round had no `must_fix`

**Acknowledged ≠ resolved.** None of the above fire while a high/critical finding (this
round or a prior one) is still only `ack`'d/`wont_fix`'d without a cited reason — a deferred
high issue keeps the loop open. A `wont_fix`/`false_positive` with a real rationale is a
resolution and does not block convergence; a bare `ack` on a high/critical does.

Budget exhausted → Final Summary; `--ask` to continue, else `capped-at-max`.

| Gate | `--ask` | Default |
|---|---|---|
| PR mismatch | Ask | Abort `pr-mismatch-abort` |
| Rounds exhausted | Ask | Stop `capped-at-max` |
| Spiral | Ask | Re-fix once; 2nd or multi-source → `spiral-escalated` |

## Step 4: Push

```bash
SYNC=$(python3 $SKILL_DIR/scripts/pr_ops.py remote-head "$BRANCH")
if [ "$(echo "$SYNC" | jq -r .in_sync)" != "true" ]; then
  git push --force-with-lease --force-if-includes origin HEAD   # or -u on first push
fi
```

Use `remote-head` not `git rev-parse origin/<branch>` (worktree lag).

## Step 5: Summary

```bash
python3 $SKILL_DIR/scripts/pr_ops.py state-mark-complete $CTX <reason>
```

Reasons: `converged`, `all-low`, `false-positive-top`, `capped-at-max`, `spiral-escalated`, `pr-mismatch-abort`, `sync-conflict`, `dirty-tree-preflight`, `user-paused`, `abandoned`.

Print: metrics, round table, full `comment_actions` table (`Round | Source | Path:Line | Severity | Action | Notes`), state file path, ready/not-ready verdict.

## Measuring this loop's quality

`scripts/escape_rate.py` measures the **escape rate** — substantive findings external
reviewers posted after this loop declared the PR done, that its own reviewers never
surfaced. Each is a defect the local loop should have caught, so it is the metric
this skill is tuned against.

```bash
python3 $SKILL_DIR/scripts/escape_rate.py <state-dir>   # one run
python3 $SKILL_DIR/scripts/escape_rate.py --all         # fleet-wide
```

It needs the harvested dataset for a post-run comment census: `pp-comments.json` is
captured *during* the run and so cannot contain the escapes. `escaped_in_scope`
separates depth failures (reviewer saw the file, missed the bug) from scope failures.

Full operating procedure — cadence, bake-off runbook, promotion checklist:
`docs/design/code-review-benchmark-process.md`.
