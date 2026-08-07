---
name: pr-polish
description: Fully autonomous PR polish loop. Runs N rounds of local bramble review (codex + cursor, optionally + gemini and/or claude), folds in any existing PR comments and CI failures as round-1 input, fixes findings locally, pushes once at the end.
argument-hint: "[--rounds N] [--gemini] [--claude] [--ask]"
disable-model-invocation: true
---

# PR Polish Loop

Review → triage → fix → commit locally each round. Exit when converged (Step 3.g) or round cap hit, then force-push once. No mid-loop pushes (why: `references/why-defer-push.md`).

Helpers: `python3 $SKILL_DIR/scripts/<helper>.py`. `$SKILL_DIR` = directory containing this `SKILL.md`.

| Script | Role |
|---|---|
| `pr_ops.py` | Identity, comments, CI, state I/O, `pr-summary`, `round-bundle`, `remote-head` |
| `bramble_ops.py` | Goal text, resume ids, triage, envelope recovery |
| `lint_gate.py` | Diff lint (ruff/golangci/eslint) |
| `scope_gate.py` | `scope-hints.json` for bramble |

Missing/error review streams → log as findings with stderr path cited. A `status: "partial"` envelope (the reviewer found things, then hit the idle timeout) keeps its findings **and** reports the failure — triage sees both.

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
| `state-append-round <ctx> <n> <head_before> [--pr-summary "$PR_SUMMARY"] [--base-branch <base>]` | Round start (`--no-verify-head` only when resuming interrupted round). **Pass both on round 1.** |
| `state-finalize-round <ctx> <n> <head_after> <actions.json> [--envelope …]` | Round end |
| `state-mark-complete <ctx> <reason>` | Exit |

**Round-1 freeze:** `--pr-summary` and `--base-branch` are written once per *series* and not overwritten by later rounds in it; they're read back out of state. A new series (`state-is-new-series` = 1 — prior loop completed, or the branch was rewritten) re-anchors both, since the old values can describe a branch that no longer exists after a squash or a retarget. `--pr-summary` is the goal's spine — omit it and the goal loses the PR's purpose from round 2 on. `--base-branch` anchors the "Files in this PR" range to the PR's real base — omit it and a PR stacked on a non-default branch is measured against the repo default. Passing them again on later rounds is a no-op.

Key fields: `rounds[n].comment_actions` (audit trail), `low_only_streak` (convergence), `session_ids` (resume), `pr_summary` + `base_branch` (frozen; see above).

**Actions file** (the `<actions.json>` arg to `state-finalize-round` / `finalize-and-report`): a JSON **array** of action entries, or an object `{"comment_actions": [...]}` — both accepted. Per entry:
- `action`: one of `fixed`, `false_positive`, `wont_fix`, `ack`, `stale`, `pre_existing`/`flake` (CI only) — validated; an unknown verb is a loud error naming the entry index.
- `severity`: `high`/`medium`/`low`/`nit` or null (lint advisory) — validated.
- `source`: `claude`/`codex`/`cursor`/`gemini`/`lint`/`github-inline`/`github-issue`/`ci`/`sweep`.
- `path` + `line` (code mode) or `section`/`dimension` (design-doc mode); `notes`/`reason`, `comment_id` (for inline replies).
- Optional v2: `spiral_refix`, `invariant` — and any other key passes through untouched.
- Optional v3 (Step 3.d/3.e evidence): `sites_found`/`sites_fixed`, `negative_check`.

## Step 0: Bootstrap

```bash
PREFLIGHT=$(python3 $SKILL_DIR/scripts/pr_ops.py preflight)
export BRAMBLE_BIN=$(echo "$PREFLIGHT" | jq -r .bramble_bin)
export SKILL_DIR=$(echo "$PREFLIGHT" | jq -r .skill_dir)
GIT_SYNC=$(echo "$PREFLIGHT" | jq -r .git_sync_path)
if [ "$(echo "$PREFLIGHT" | jq -r '.errors | length')" != "0" ]; then
  echo "$PREFLIGHT" | jq -r '.errors[]' >&2; exit 1
fi
# Warnings are non-fatal — print, don't abort. Common one: this PR modifies
# bramble's own review code but BRAMBLE_BIN resolved to a stale PATH binary.
# Then build from this branch (`bazel build //bramble/...`) and re-export
# BRAMBLE_BIN=$(pwd)/bazel-bin/bramble/bramble_/bramble before the round.
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

Build `$PR_SUMMARY` with the helper — do not hand-roll it:

```bash
SUMMARY_JSON=$(python3 $SKILL_DIR/scripts/pr_ops.py pr-summary)   # one call
PR_SUMMARY=$(echo "$SUMMARY_JSON" | jq -r .pr_summary)
echo "$SUMMARY_JSON" | jq '{source, dropped}'                     # log what it used
```

When the PR has a usable description, **that description is the summary** — the author's own statement of intent beats a generated commit list for telling a reviewer what the change is FOR, and therefore what is out of scope. The helper strips only machine-emitted additions: bot-appended blocks (`<!-- CURSOR_SUMMARY -->` and friends) and `Generated with`/`Co-Authored-By`/bot-review-footer trailers. A review bot's restatement of the diff is the *worst* possible goal text — it anchors the reviewer on a summary of the code instead of the intent behind it.

**Author-written prose is never stripped, whatever the heading says.** A "drop `## Deployment Notes`" rule was tried and removed — measured, it had no true positives and deleted real review input (security-posture changes, stacked-PR ordering, migration semantics) that authors filed under those headings. Heading names don't predict whether the prose under them matters to a reviewer.

`source` reports `pr-body`, `pr-body+diffstat`, or `commits-diffstat` (the fallback when there's no PR, no body, or the body strips to nothing); `dropped` lists what was removed, so a surprising goal is traceable.

Round 1 `--goal` = `$PR_SUMMARY`; later rounds use `round-bundle` / `bramble_ops.py goal`, which **leads with the frozen `$PR_SUMMARY`** and appends the action-history briefing (prior fixed/skipped + the PR's own file list, plus any invariant/streak notes). No inter-round diff is embedded — a diff pinned to the prior round's HEAD is wrong after a rebase, and bramble re-reads the working tree anyway.

The action history is per-turn and covers the prior round only — with one exception. **Settled declines accumulate across the whole run:** every `wont_fix`/`false_positive` from any prior round, with its reason, under a "do not re-raise these" heading. A decline is a decision, not context, and one the next reviewer can't see is indistinguishable from an unnoticed one — it returns every round, costs a triage slot, and tempts a re-fix of something already decided. Measured on #8374: re-litigation stopped the round this block appeared, and none of the five frozen items came back over the six rounds after.

Pass this same `<base>` to `state-append-round --base-branch` (Step 3.a0): the file list is measured `origin/<base>...HEAD` and must share the merge base `$PR_SUMMARY` was built from.

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
  a0) open the round in state (--pr-summary + --base-branch on round 1)
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

### a0) Open the round in state

Every round starts here — `round-bundle` (Step 3.b) reads `pr_summary` and `base_branch` back out of state, so a round that never opened produces a goal with no PR purpose and a file list anchored to the repo default.

```bash
python3 $SKILL_DIR/scripts/pr_ops.py state-append-round "$CTX" {ROUND} $(git rev-parse HEAD) \
  --pr-summary "$PR_SUMMARY" --base-branch "$BASE"
```

Add `--no-verify-head` only when resuming an interrupted round.

### a) WIP commit

If dirty: `git add -A && git commit -m "pr-polish: round N snapshot"`.

### b) Launch reviewers

Always use `round-bundle` for `$LOG_DIR`, `$GOAL`, resume ids — do not hand-roll attempt index.

```bash
# Bind from the invocation flags, substituting 1/0 as literals like every other
# orchestrator var. Read in three places below (launch, --stream, --envelope).
# Leaving one unset is NOT neutral: `[ "$USE_CLAUDE" = "1" ]` is false, so the
# reviewer silently never launches and looks like you never asked for it.
USE_GEMINI=0   # 1 when --gemini was passed
USE_CLAUDE=0   # 1 when --claude was passed

BUNDLE=$(python3 $SKILL_DIR/scripts/pr_ops.py round-bundle "$CTX" {ROUND})
LOG_DIR=$(echo "$BUNDLE" | jq -r .log_dir)
GOAL=$(echo "$BUNDLE" | jq -r .goal_text)
CODEX_RESUME=$(echo "$BUNDLE" | jq -r '.resume_ids.codex')
CURSOR_RESUME=$(echo "$BUNDLE" | jq -r '.resume_ids.cursor')
GEMINI_RESUME=$(echo "$BUNDLE" | jq -r '.resume_ids.gemini')
CLAUDE_RESUME=$(echo "$BUNDLE" | jq -r '.resume_ids.claude')
[ "{ROUND}" = "1" ] && GOAL="$PR_SUMMARY"
mkdir -p "$LOG_DIR"

SCOPE_HINTS=$(python3 $SKILL_DIR/scripts/scope_gate.py --state-dir "$STATE_DIR" 2>"$LOG_DIR/scope-gate-stderr.txt")

# Pin the reviewed range: unpinned, the agent's inferred `main...HEAD` drifts
# with the local base branch (measured: 336 files instead of 22, varying run to
# run). Best-effort — an unresolvable base falls back to inferred scope, which
# must be LOUD and is NOT a hard failure. See references/why-pin-diff-base.md.
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

The warning branch writes no state. When it fires, YOU must add an `ack` entry (source `sweep`, notes naming the unpinned scope) to this round's actions file in step (f) — nothing else records which rounds were scoped by inference.

**The join rule: launch every reviewer inside ONE `run_in_background` Bash job, then wait only for that job's single completion notification before triaging** — steps b→c in one turn, no tool calls in between. Streaming per-reviewer output is visibility only, never a cue to act; don't poll envelopes, `sleep`, `ScheduleWakeup`, or end the turn with a "standing by" reply. Non-interactive runs (e.g. jiradozer, one bounded agent turn) have no harness to re-invoke you on a wakeup, so a yielded turn strands the round permanently.

Two non-obvious properties: `wait` returns as soon as every child has *exited*, so a crashed reviewer never hangs the round; and the join's **exit code is not how failure is detected** — multi-PID `wait` reports only the last PID's status, so failure surfaces after the join, in triage, via a missing/empty envelope. Background: `references/why-one-background-join.md`.

Substitute the concrete `{ROUND}`/`$REPO`/`$PR_NUMBER`/`$GOAL`/`$SCOPE_HINTS`/`$LOG_DIR`/resume-id/`$BRAMBLE_BIN`/`$SKILL_DIR` values in, then run the **whole script as one call**. No `bash -c` wrapper, no nested quoting. Every reviewer uses this template, differing only in the four substitutions tabled below:

Every launch is literal below. The `${VAR:+…}` idiom is load-bearing — an empty
resume id must drop the flag entirely, not pass an empty string — and the
per-reviewer `BRAMBLE_RUN_TAG` is how runs are attributed.

```bash
# INVARIANT: every reviewer launch ends with `PIDS+=($!)`; nothing else touches
# the wait list. Timeouts: --idle-timeout 8m kills only a stalled backend (a
# review making progress runs as long as it needs), `timeout 2400` is the
# absolute backstop, lint gets 120s so a static pass can't hold the join.
# 8m, not 5m: codex's transport has a server-side ~300s keepalive after which it
# reconnects and resumes on its own, so a 5m client bound raced the backend's
# own recovery and killed live turns (measured on #8682 r2: 12min of review
# holding ~2M input tokens, discarded). The backstop is 2x the old 1200s to stay
# well clear of the idle bound — a real review on a large diff runs 18min+.
# `set -o pipefail` keeps each subshell's status the reviewer's, not `sed`'s 0.
PIDS=()

( set -o pipefail; BRAMBLE_RUN_TAG=pr-polish:$REPO:$PR_NUMBER:codex:r{ROUND} \
  timeout 2400 $BRAMBLE_BIN code-review --backend codex --model gpt-5.6-luna --effort medium \
    --skip-test-execution --verbose --idle-timeout 8m \
    --goal "$GOAL" --scope-hints-file "$SCOPE_HINTS" $DIFF_BASE_ARG \
    ${CODEX_RESUME:+--resume-session-id "$CODEX_RESUME"} \
    --envelope-file "$LOG_DIR/codex-envelope.json" \
  2>&1 | tee "$LOG_DIR/codex-stderr.txt" | sed 's/^/[codex] /' ) &
PIDS+=($!)

( set -o pipefail; BRAMBLE_RUN_TAG=pr-polish:$REPO:$PR_NUMBER:cursor:r{ROUND} \
  timeout 2400 $BRAMBLE_BIN code-review --backend cursor --model composer-2.5 \
    --skip-test-execution --verbose --idle-timeout 8m \
    --goal "$GOAL" --scope-hints-file "$SCOPE_HINTS" $DIFF_BASE_ARG \
    ${CURSOR_RESUME:+--resume-session-id "$CURSOR_RESUME"} \
    --envelope-file "$LOG_DIR/cursor-envelope.json" \
  2>&1 | tee "$LOG_DIR/cursor-stderr.txt" | sed 's/^/[cursor] /' ) &
PIDS+=($!)

( set -o pipefail; timeout 120 python3 $SKILL_DIR/scripts/lint_gate.py \
    --state-dir "$STATE_DIR" --round {ROUND} --log-dir "$LOG_DIR" \
  2>&1 | tee "$LOG_DIR/lint-stderr.txt" | sed 's/^/[lint] /' ) &
PIDS+=($!)

if [ "$USE_GEMINI" = "1" ]; then
  ( set -o pipefail; BRAMBLE_RUN_TAG=pr-polish:$REPO:$PR_NUMBER:gemini:r{ROUND} \
    timeout 2400 $BRAMBLE_BIN code-review --backend gemini --model gemini-3-flash-preview \
      --skip-test-execution --verbose --idle-timeout 8m \
      --goal "$GOAL" --scope-hints-file "$SCOPE_HINTS" $DIFF_BASE_ARG \
      ${GEMINI_RESUME:+--resume-session-id "$GEMINI_RESUME"} \
      --envelope-file "$LOG_DIR/gemini-envelope.json" \
    2>&1 | tee "$LOG_DIR/gemini-stderr.txt" | sed 's/^/[gemini] /' ) &
  PIDS+=($!)
fi

if [ "$USE_CLAUDE" = "1" ]; then
  ( set -o pipefail; BRAMBLE_RUN_TAG=pr-polish:$REPO:$PR_NUMBER:claude:r{ROUND} \
    timeout 2400 $BRAMBLE_BIN code-review --backend claude --model opus \
      --skip-test-execution --verbose --idle-timeout 8m \
      --goal "$GOAL" --scope-hints-file "$SCOPE_HINTS" $DIFF_BASE_ARG \
      ${CLAUDE_RESUME:+--resume-session-id "$CLAUDE_RESUME"} \
      --envelope-file "$LOG_DIR/claude-envelope.json" \
    2>&1 | tee "$LOG_DIR/claude-stderr.txt" | sed 's/^/[claude] /' ) &
  PIDS+=($!)
fi

# Join on EVERY launched reviewer so triage never starts while one is still
# running or has yet to write its envelope. A skipped reviewer is simply one
# fewer element — the wait can't desync from the launches.
wait "${PIDS[@]}"
```

Before triage: `recover-envelope` on each stream path (idempotent). A reviewer that exited without a valid envelope → `stream-missing` finding, not a deadlock.

**`stream-missing` requires that no envelope file exists — check before you write it.** On #8682 r1 the orchestrator recorded `ack … no envelope` while `codex-envelope.json` was on disk with `status: ok` and 3 findings (including a 0.98-confidence bug that then took four more rounds to rediscover). `finalize-and-report` now rejects a round that ignores an envelope present in `$LOG_DIR`, so this fails loudly rather than silently costing a reviewer. If a stream really produced nothing, `cat` the envelope's `status`/`error` and cite it — "the backend stalled" is a claim about the backend, and codex keeps its own log (`~/.codex/logs_2.sqlite`, table `logs`) that will say whether it actually did.

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

**Buckets are nested under `.action_plan`, not top-level** (the top level carries `total`, `unique`, `consensus`, `single_critical`, `single_medium`, `low_acks`, `spiral_matches`). Reading `.must_fix` yields `null` for every bucket — indistinguishable from an empty plan, which Step 3.g treats as convergence, so the loop exits reporting success with every finding unread. Cross-check the stderr census (`[triage] action_plan: must_fix=0 consider_fix=10 … total=13`): a non-zero `total` with empty buckets means you read the wrong key.

```bash
jq '.action_plan | {must_fix: (.must_fix|length), consider_fix: (.consider_fix|length),
                    batch_ack: (.batch_ack|length), escalate: (.escalate|length)}' triage.json
```

**Ownership:** own pre-existing code in touched files. `must_fix` unless false positive (cite file:line). Low/nit → fix if trivial else `ack`. Skips: `false_positive`, `wont_fix`, `stale`.

**Scope contract — the PR's remit is fixed at round 1.** "Touched files" means files the PR touched when you *first saw it*, not files a later round dragged in. Otherwise ownership ratchets: each round's fix touches new files that the next round then owns, and the PR grows without limit while every round still looks productive. Before fixing anything outside the round-1 file set, ask whether it is *this PR's job*:

- **Outside the round-1 files** → `wont_fix`, naming where it belongs ("not in this PR's diff — worth a ticket against `<owner>`").
- **A flaw in a fix an earlier round of this run made** → genuinely yours, fix it. But if this is the **second** round correcting the same mechanism, stop patching it and apply *Second round on the same predicate* below — re-derive the rule, don't add another case and don't escalate.
- **New machinery to support a fix** (a cache, a latch, a reaper) → prefer the smaller change that needs none; new machinery draws new findings, which is how a two-file fix becomes a thirteen-file one.
- **A fix guarding a destructive action** (unlink, overwrite, kill, revoke, force-push) → state what must be **true** for it to be safe, never which inputs are unsafe; a blocklist draws one finding per bad shape a reviewer can still imagine. Measured on #8374: five consecutive rounds each blocked one more malformed `.git` pointer, ending the round the check became "require the marker git itself writes".

**Second round on the same predicate — the largest single cycle cost.** A second round flagging the same predicate means it is written in examples, not in a rule, and examples are inexhaustible. Don't add another regex shape or another arm: answer the question the code is actually asking, once, with a test for the case that made you ask. Measured on #8374: one predicate ate **twelve rounds** while each fix answered "which stderr shape latches?", and **zero** after one round answered "is this failure the pod's own passing state?".

A scope decline is a real outcome, not a gap: record it as `wont_fix` with the reason.

**Invariants:** same `invariant` from ≥2 reviewers → consensus on all sites. Prefer producer-side fix.

**Spirals:** single-source may auto-demote to stale if evidence gone (±10 lines) or cited line was in prior round's diff. Multi-source → escalate. Default (no `--ask`): re-fix once (`spiral_refix: true`), stop on 2nd recurrence.

**Escalating to the human — only for what a human holds:** a priority call, a risk appetite, a product decision, an ops tradeoff you cannot observe. This is about escalations *you* raise; the `escalate` bucket triage emits for a multi-source spiral is mechanical, so handle it per **Spirals** above rather than re-triaging it against this rule. Reviewers contradicting each other is **not** grounds — two backends disagreeing about your own code's semantics is settled by reading the code. Write the one sentence naming what you need *from the human*; if you cannot, you are not blocked — check instead whether the two positions approximate one rule from opposite sides. Measured on #8374: an escalation billed as a human design call was two camps patching opposite legs of one predicate, and resolved in a single round.

Empty plan (`.action_plan.must_fix` and `.action_plan.consider_fix` both empty, **and** the stderr census agrees — `total=0`, or every finding accounted for in `batch_ack`/`batch_stale`) → converged, Step 3.g.

### d) Apply fixes

Fix the invariant, not the citation: name rule → scan sibling sites → fix at shallowest layer (line, helper, producer). Group cross-backend findings by underlying problem. Update docs/tests in same commit when contract changes. Log extra sites as `source: "sweep"`. Record every finding in `comment_actions`; don't silently drop stale items. GitHub inline replies happen in `state-finalize-round`.

**A finding is a hypothesis about a class, not a work order.** The reviewer names the one instance they saw, so their suggested fix is scoped to it — apply it verbatim and the next round finds the next instance.

**Research the class before editing.** When a finding names a mechanism, helper, contract, or an `invariant` seen in a prior round, dispatch one subagent per class (group cross-backend findings first) to enumerate every site and judge, against `$PR_SUMMARY` and the round-1 file set, which are this PR's job. Skip it for typos and single-line local fixes. It researches and recommends; **you decide and edit** — its scope call is input, the Step 3.c Scope contract still binds. Record `sites_found`/`sites_fixed`; `M < N` needs a reason in `notes`. An agent that always answers "1 of 1" is rubber-stamping — say so rather than keep paying for it.

**Don't claim what you haven't checked.** "always", "only", "every", "both" belong in a comment only when a named test enforces them; a claim about code you didn't open is one grep away from being verified. An overclaiming comment is worse than the bug — it tells the next reader not to look.

**A partially-adopted abstraction is worse than none.** Extract a helper to be "the one rule" and every call site converts in the same edit, or it reads as if the rule holds when it doesn't.

### e) Quality gates + commit

Skip if no file changes. Run project gates, then commit locally (`pr-polish round {ROUND}: …`). **No push.** Check sibling sites/tests/docs before commit; record intentional gaps as `ack`.

**A green test proves nothing until you have seen it red.** Revert the fix, watch the new test fail, restore. Record `negative_check: true`. A test that passes either way is worse than none — it is counted as coverage.

**State the ordering on any concurrency edit.** A select/channel/goroutine change says in a comment what happens when the other side hasn't run yet; otherwise a fix that never fires reads as done.

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

(`state-finalize-round` has the same finalize semantics, without the round summary hints.)

### g) Convergence

Stop when any:
- Zero findings
- Empty triage plan
- `low_only_streak >= 2` (every low fixed or `ack`/`wont_fix` with reason)
- Top finding documented false positive + prior round had no `must_fix`

**Acknowledged ≠ resolved.** None of the above fire while a high/critical finding (this round or a prior one) is still only `ack`'d/`wont_fix`'d without a cited reason — a deferred high issue keeps the loop open. A `wont_fix`/`false_positive` with a real rationale is a resolution and does not block convergence; a bare `ack` on a high/critical does.

**Nobody looked ≠ nothing to find.** "Zero findings" only converges a batch when at least one reviewer actually **produced a verdict** — read `rounds[n].stream_status` (persisted per backend from each envelope's own `status`), not the length of `<backend>_findings`. An empty findings array is produced equally by a clean diff and by a backend that never returned, and the batch protocol makes that gap expensive: the batch pushes on it, and the external reviewers become the first real bar.

**Count only the model reviewers** — `codex`/`cursor`/`gemini`/`claude`. `lint` is in `stream_status` too and `lint_gate.py` hardcodes `"status": "ok"`, so "every stream is error" is never literally true and a rule phrased that way never fires. `verdict.py`'s `_NON_REVIEWER_STREAMS` is the same exclusion. A round where every *model reviewer* is `error`/`absent` (`ok`/`partial` are live) is **inconclusive** — retry it once (it does not consume `--rounds`), then exit `reviewers-unavailable`. Push anyway (the work is real), but say in the Final Summary that the local bar was never met.

Budget exhausted → Final Summary; `--ask` to continue, else `capped-at-max`.

| Gate | `--ask` | Default |
|---|---|---|
| PR mismatch | Ask | Abort `pr-mismatch-abort` |
| Rounds exhausted | Ask | Stop `capped-at-max` |
| Spiral | Ask | Re-fix once; 2nd or multi-source → `spiral-escalated` |
| No stream returned a verdict | Ask | Retry once, then `reviewers-unavailable` |

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

# --repo-root is load-bearing: without it fix-claim verification silently
# SKIPS, reporting `fix_claims: {}` with no blocker, and every
# `action: "fixed"` row is taken on faith.
python3 $SKILL_DIR/scripts/verdict.py "$STATE_DIR" --repo-root "$(pwd)" --write || true
```

Reasons: `converged`, `all-low`, `false-positive-top`, `capped-at-max`, `spiral-escalated`, `pr-mismatch-abort`, `sync-conflict`, `dirty-tree-preflight`, `user-paused`, `abandoned`, `reviewers-unavailable`.

Print: metrics, round table, full `comment_actions` table (`Round | Source | Path:Line | Severity | Action | Notes`), state file path.

**Report `verdict.py`'s output, do not re-derive it.** Its `blockers` are checkable
facts; prose that contradicts one is the failure this replaces. `|| true` keeps the
non-zero exit from aborting the summary — the verdict itself carries the signal.

## Measuring this loop's quality

`scripts/escape_rate.py` measures the **escape rate** — substantive findings external reviewers posted after this loop declared the PR done, which its own reviewers never surfaced. Each is a defect the loop should have caught, so it is the metric this skill is tuned against.

```bash
python3 $SKILL_DIR/scripts/escape_rate.py <state-dir>   # one run
python3 $SKILL_DIR/scripts/escape_rate.py --all         # fleet-wide
```

It needs the harvested dataset for its post-run comment census — `pp-comments.json` is captured *during* the run and so cannot contain the escapes. `escaped_in_scope` separates depth failures (reviewer saw the file, missed the bug) from scope failures.

Full operating procedure — cadence, bake-off runbook, promotion checklist: `docs/design/code-review-benchmark-process.md`.
