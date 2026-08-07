# Code Review Benchmark Process

## Objective

Make `/pr-polish`'s approval a trustworthy quality signal: **once it passes, external
reviewers should have nothing substantive left to say.**

This document is the recurring operating procedure for measuring that and improving
it. It covers two metrics, a fixed cadence, and the runbooks to execute each cycle.
The one-off build work lives elsewhere; this is what you run every week and every
month.

## The two metrics

**1. Escape rate** — the North Star. The share of substantive external-reviewer
findings, posted after pr-polish declared the PR done, that pr-polish's own reviewers
never surfaced and its triage never recorded. Each one is a defect the local loop
should have caught.

```
escape_rate = escaped / locatable substantive external findings
```

Only findings with a file path count: a review-level summary has no location to match
on, and its individual findings are already counted as inline rows.

**Always quote the judged rate, not the raw rate.** An escape is a *bot claim*
pr-polish did not match — and bot claims on this corpus measured **~9% precision**
(20 of 224 confirmed by independent judges). So the raw rate mostly counts bot noise.
Measured on the same fleet: **raw 52.6%, judged 10.7% — a 5× inflation.** Reporting
the raw number would have badly overstated how much pr-polish misses, and would have
pointed tuning effort at a problem five times smaller than it appeared.

`escaped_judged` (and the `judged` block in `--all`) restricts the numerator to
escapes a judge confirmed against a frozen `ground_truth_v3`. It is `null` when the PR
has no ground truth — deliberately distinct from zero, since "not collected" and
"nothing real here" are different claims.

`escaped_in_scope` splits **depth** failures from **scope** failures. An escape citing
a file already in `files_changed` means the reviewer looked at the right code and
missed the bug; an out-of-scope escape means it never looked. This distinction decides
which lever to pull, and it has repeatedly shown the problem is depth, not scope.

**2. Benchmark recall/precision** — how a reviewer config scores against frozen ground
truth, mechanically, via `replay.py`. Recall over ground-truth true positives is the
target; precision is the guardrail, because recall alone is trivially gamed by
flagging everything.

### What "good" looks like

- Escape rate falling over time, especially `escaped_in_scope` and `escaped_p1`.
- A shipped config raises **median** recall without regressing **median** precision.
- Escape rate among `verdict == "ready"` runs materially below other exits. If the two
  are equal, the verdict is not carrying signal and its blocker set is wrong.

### What triggers a revert

A promoted config that does not move the fleet escape rate within one measurement
cycle gets reverted rather than kept for appearances. "No measurable effect" is a
result, not a reason to keep a change.

## Escape-rate baseline (2026-08-06)

Re-measured across the full corpus with `escape_rate.py --all`:

| Metric | Value |
|---|---|
| Runs measured / skipped | 274 / 368 |
| Substantive external findings | 1,260 |
| **Raw escape rate** | **61.7%** (777) |
| **Judged escape rate** | **2.7%** (3 of 111, over 21 runs with frozen GT) |
| Escaped P1 | 179 (raw) |
| **Escaped but in scope** | **742 of 777 — 95.5%** |

**Quote the judged rate.** Raw is inflated 23× here, consistent with bot claims measuring
~9–14% precision on this corpus. The raw number counts claims that were never defects.

**This is a new baseline, not a measurement of any change.** The prior recorded figures
(raw 52.6% / judged 10.7%) came from **11** measurable runs; this covers **274**, and 21
judged runs against 5 before. The corpus grew roughly 25×, so the movement is in what is
being measured, not in reviewer quality — do not read 52.6% → 61.7% as a regression.

**Nothing has yet exercised `--diff-base` or `verdict.py`.** Zero pr-polish runs have
completed since those landed: the most recent (`yoloswe-309`, 05:26Z) predates both — it
wrote no verdict and its review logs carry no `--diff-base`. Attribution has to wait for
runs that actually used them.

**The one durable finding: 95.5% of escapes are in scope.** The reviewer already had the
file and missed the bug. The original measurement put this at 87% on 38 findings; it now
holds at 95.5% on 777, across a corpus 25× larger. Widening scope cannot fix a miss on a
file already in scope — this is a depth problem, and it is the most robustly supported
claim in this document.

### Open experiment: a coverage ledger at the orchestrator layer (2026-08-07)

The 95.5% finding says the loss is depth on files already in scope. Every rule in
`/pr-polish` Step 3.c operates on findings that *exist*, and an escape by definition
was never a finding — so triage cannot move this metric except second-order. The
untested hypothesis is to move `coverage-ledger`'s per-file-conclusion obligation out
of the reviewer prompt and into the orchestrator, as a round-level artifact: which
changed hunks did no finding touch, recorded in the actions file, with convergence
gated on it.

**Why it is not shipped.** `coverage-ledger`'s measured advantage inverted once
`--diff-base` landed (0.20 → 0.10 while the baseline went 0.10 → 0.20), and the
reading above is that the two were confounded — the ledger was compensating for a
15×-too-large diff. `/pr-polish` now pins `--diff-base`, which is precisely the
condition under which the ledger stopped paying. Promoting it here would be
promoting on the pre-fix numbers this document says not to trust.

**Pre-registration.** Before any implementation: `escape_rate.py --all`, quoting the
**judged** rate and `escaped_in_scope`. After one measurement cycle, re-run. The
revert rule applies unchanged — if `escaped_in_scope` does not move, revert rather
than keep it for appearances. Note the baseline is not yet measurable: zero
pr-polish runs had completed under `--diff-base` + `verdict.py` as of 2026-08-06, so
the first task is a clean post-change baseline, not the experiment.

## Data boundary

The dataset lives **outside the repository**, at `~/.bramble/code-review-eval/`:

| Path | Contents |
|---|---|
| `dataset/` | Per-PR records + `index.json` (frozen ground truth) |
| `collect/` | Per-PR collection sessions and worktrees |
| `replays/` | Scored replay results |

It carries real private-PR content — file paths, commit SHAs, reviewer findings, PR
bodies. **It must never be committed.** Only aggregate, non-content-bearing metrics
(counts, rates, config names) belong in this document or anywhere in git.

The same rule applies to `~/.bramble/projects/` (pr-polish state) and to escape
reports: `escape-metrics.json` contains comment excerpts and stays on disk.

## Cadence

| Frequency | Action |
|---|---|
| **Weekly** | Harvest newly merged PRs; refresh escape metrics; note the trend. |
| **Monthly**, or on any reviewer config/prompt change | Re-run the bake-off; publish the scoreboard; promote or revert. |
| **On demand** | Corpus top-up when the held-out set is exhausted or the codebase has drifted enough that old PRs no longer represent current review difficulty. |

Weekly is cheap (API calls only). Monthly is expensive (bramble runs per config per
PR) — that asymmetry is deliberate.

## Runbook — corpus refresh

The corpus is built from **PRs that went through `/pr-polish`**. Direct GitHub
harvesting is no longer part of the refresh cycle — see
[Why pr-polish-only](#why-pr-polish-only) for the measurement behind that.

```bash
cd <repo>   # the yoloswe worktree holding the skills
SCRIPTS=.claude/skills/code-review-replay/scripts

# 0. Pull polish state + git objects from the rest of the fleet first —
#    most of the corpus is produced on other boxes. See "Collecting polish
#    state from the fleet" below; skipping this silently caps the corpus at
#    whatever this one box happened to polish.

# 1. Harvest PRs with local pr-polish history. Already-harvested PRs are
#    refreshed in place; frozen ground truth is preserved.
python3 $SCRIPTS/harvest.py --verbose

# 2. Confirm nothing is malformed and no ground truth was lost.
python3 $SCRIPTS/collect.py validate --all | tail -3
```

Expect `0 malformed`. "N with warnings" is normal — the common warning is simply
"ground truth not collected".

**The corpus grows by running `/pr-polish` on more PRs, not by harvesting harder.**
This is the binding constraint on benchmark size: the harvester can only see PRs that
already have local polish state, so corpus growth is a matter of routing more PRs
through the polish loop going forward — **on any box in the fleet**, then
centralized (next section).

### Collecting polish state from the fleet

`/pr-polish` runs on every devbox, so most of the corpus lives on *other* machines.
`~/magent/fleet/*.json` is the host registry (`public_dns` + `ssh_user`). One sweep of
a 3-host fleet found **515 PRs with polish state that the harvest box had never
seen**, against 291 local — the fleet held the majority of the corpus.

Two things must be centralized, and **the second is the one that gets missed**:

1. **The state** — `~/.bramble/projects/<repo>-<pr>/pr-polish-state.json`.
2. **The git objects those states reference.** A state file is worthless without the
   commit it was reviewed at: `collect setup` pins a worktree to the canonical round's
   `head_before`, and an unreachable SHA means the PR cannot be collected at all.

On the first sweep, only **17%** of imported canonical SHAs were reachable locally.
Recovery runs cheapest-first:

```bash
# 1. Fetch PR head refs in bulk — survives branch deletion, no API calls.
#    Filter to numeric PR ids: branch-named polish dirs (branch-feature-inf-417)
#    are not PRs, and one bad ref aborts the whole fetch.
cd ~/worktrees/<repo>/main
split -l 60 /tmp/pr-numbers.txt /tmp/chunk-
for c in /tmp/chunk-*; do
  git fetch -q --no-tags origin \
    $(while read n; do echo "+refs/pull/$n/head:refs/crr/$n"; done < "$c")
done

# 2. Whatever is still missing may exist only in a remote box's object store.
#    Bare-SHA fetch fails without uploadpack.allowAnySHA1InWant, so publish
#    refs on the remote, fetch the namespace, then delete them.
ssh <host> 'cd ~/worktrees/<repo>/main
  while read s; do git cat-file -e "${s}^{commit}" 2>/dev/null &&
    git update-ref "refs/crr-export/$s" "$s"; done < /tmp/straggler-shas.txt'
git fetch -q --no-tags <host>:worktrees/<repo>/.bare \
  "+refs/crr-export/*:refs/crr-import/*"
ssh <host> 'cd ~/worktrees/<repo>/main
  git for-each-ref --format="%(refname)" refs/crr-export/ |
    while read r; do git update-ref -d "$r"; done'
```

Measured on the first sweep: **17% → 71%** after step 1, **→ 81%** after step 2.
The remaining ~15% are force-pushed commits that exist nowhere; skip those PRs rather
than harvesting a record whose diff cannot be reconstructed.

Then copy state for the reachable set only, and **never overwrite an existing local
project dir** — a local run is higher fidelity than a copy:

```bash
rsync -a -r --files-from=/tmp/harvestable.txt <host>:.bramble/projects/ \
  ~/.bramble/projects/
```

`--files-from` needs an explicit `-r`: `-a` implies recursion normally, but not when
the file list names directories, and without it you get empty dirs and a harvest that
silently finds no state. Verify every copied dir actually parses before harvesting.

All worktrees on a host share one `.bare` object store, so per-worktree reachability
counts are the same objects counted repeatedly — check once against the common dir.

### Why pr-polish-only

The GitHub-backed source (`--github-repo`) could reach ~8× more PRs, but the ground
truth it produced was dominated by noise. Measured on the kernel corpus:

| | pr-polish | github |
|---|---|---|
| Records harvested | 37 | 146 |
| With frozen GT | 19 | 10 |
| True positives | **107** | 40 |
| False positives | **41** | 201 |
| TP:FP ratio | **2.6 : 1** | 0.2 : 1 |

Half the GT'd PRs carried **73% of all true positives**, at a **13× better** TP:FP
ratio. The cause is structural, not incidental: a GitHub-sourced record has no local
review state, so its ground truth rests on external bot comments alone — and bots
measured **~9–14% precision** on this corpus. Scoring a reviewer against that set
rewards it for reproducing bot noise.

A pr-polish record does not have this problem, because it does not have to *infer*
what the reviewer saw. Each round persists its own `head_before` and triages comments
against it. The scoping is recorded, not reconstructed.

**Nothing is deleted.** The 146 GitHub records stay on disk, including the 10 with
frozen ground truth that cost real judge tokens. They are excluded from *scoring*:
`replay.py` samples only `harvest_source: "pr-polish"` by default. Pass
`--source github` to audit the excluded tier; naming a GitHub-sourced PR explicitly
still runs, but prints a warning that its numbers are not comparable.

The GitHub code paths (`--github-repo`, `--commit-scoped`, `resolve_diff_scope`'s API
fallback) remain in the harvester and remain tested. They are dormant, not removed —
if the precision picture changes, the tier can be re-enabled with a flag rather than
a rewrite.

<details>
<summary>Superseded: per-commit comment scoping (<code>--commit-scoped</code>)</summary>

This flag was built to fix a real defect in the GitHub tier: without it every inline
comment is pinned to the PR's *final* merged head, which **inverts the ground
truth** — a bot correctly reports a bug, the author fixes it, and the judge reading
post-fix code records the claim as a false positive. On kernel-8227, *none* of its
107 inline comments were written against the head they were being judged at, and 19
of its 28 commits were review-fix commits.

It is retained and tested but no longer on the refresh path, because pr-polish
records already carry per-round `head_before`. Two findings from the work are worth
keeping, should the tier ever be revived:

- **Comment density is the real obstacle.** Across the corpus, 1,166 inline comments
  spread over 430 distinct (PR, SHA) reviewed states — **2.7 per state**, and 37% of
  states hold exactly one comment. One judge round per SHA would multiply fixed cost
  while shrinking the evidence each round sees. Any revival needs density-aware
  grouping (merge sparse states into ones sharing their files), not one round per SHA.
- **Most comments are not inline at all.** Of 5,709 harvested comments, 4,225 carry
  no author, path, or line — they are issue-level PR conversation and no SHA work
  makes them judgeable. A further 318 have a path but no `original_commit_id`, a
  recoverable pool larger than the entire force-pushed tier.

**Force-pushed SHAs are a smaller problem than they first appear.** An unreachable
commit is usually just unfetched: on kernel-8227, 12 of 12 reviewed SHAs looked
unreachable until `refs/pull/N/head` was fetched, after which only 5 were genuinely
orphaned. `ensure_commit_present()` performs that fetch during `collect setup`, so
reachability measured before it runs overstates the loss.

</details>

### Selecting what to collect

Raw count is not the goal. A benchmark of clean diffs cannot separate two configs,
because most diffs have nothing to find. Prefer, in order:

1. PRs with **known escapes** (a substantive external finding pr-polish missed).
2. PRs with substantive external review generally.
3. A random tail for representativeness.

### Collecting ground truth

Per PR: `setup` → round loop (review → `build-prompt` → judge sub-agent → `fold`) →
`freeze`. Two settings make this affordable at corpus scale:

- **`--include-seeds`** on `build-prompt` passes inline external-reviewer comments as
  *candidate defect locations*. They are neither findings nor ground truth — starting
  from real candidates instead of rediscovering them cuts discovery effort, which is
  the dominant cost.

  **Measured: external bots run ~9% precision on this corpus.** Across a 10-PR pilot,
  judges verdicted 224 bot-posted inline claims and confirmed **20**, rejecting 204.
  So seeds must never be auto-promoted to ground truth: a benchmark built that way
  would be ~91% noise and would reward reviewers for parroting bots. The judge rules
  on the code, every time.

  The health check on a seeded round is the **reject ratio**. Judges that confirm
  nearly everything have been captured by the seeds — treat that as a broken run, not
  a fast one. In the pilot, judges rejected roughly 10× what they confirmed.
- **`--round-budget 2` or `3`**, not the default 10, and **expect most PRs not to
  converge**. Measured over a 10-PR pilot, a second round (different reviewer model)
  saturated only **1 of 6** PRs and grew the cumulative census **+21%** — with no
  decay in yield, so a third round has no visible stopping point either.

  **Accept `census_converged: false` and treat recall as an upper bound.** For a
  bake-off this is the right trade: every config is scored against the *same* frozen
  set, so absolute recall is overstated but the relative ranking — the thing the
  bake-off exists to produce — stays valid. Paying ~50% more per round to move a
  number that shifts all configs together is poor value. Reserve extra rounds for a
  PR whose ground truth will be quoted on its own.

Collection is **embarrassingly parallel across PRs** and strictly serial within one
(`setup` isolates a session and worktree per PR), so drive several at once. Use one
strong backend during collection — the judge's census is what produces ground truth;
extra backends mainly add finding coverage, and that belongs in scoring.

**Budget before committing.** Measure per-PR tokens/rounds/wall-clock on ~10 PRs
first. If the per-PR cost is far above expectation, re-scope then — not at PR 60.

## Runbook — bake-off

```bash
bazel build //bramble:bramble   # never score a stale ~/bin/bramble

python3 $SCRIPTS/replay.py \
  --bramble-bin "$(bazel info bazel-bin)/bramble/bramble_/bramble" \
  --config codex-5.4-mini --config codex-5.6-luna \
  --print-markdown
```

Variants worth testing, cheapest first: `--effort medium`/`high` versus the default;
alternative models; prompt variants via `--review-prompt-file`; and an N-sample union
(two runs, union of findings) versus one run.

### Budget it from measurement, not intuition

**A replay run costs ~8 minutes, not ~1.** Measured: 414s and 557s for two scored
reviews. Replay is slower than a collection-phase review because it scores both the
`r1` and `final` tiers unless `--tier` narrows it, so a "PR" is two reviews per config
per run. The full protocol at 3 configs x 32 PRs x 3 runs is therefore **~10h at
8-way parallelism**, not the ~45min a per-review guess of 75s suggests. Pass
`--tier r1` to halve it; drop to a 16-PR sample for ~2.5h.

**Pilot on one PR before committing the full matrix.** Pick a PR whose GT has
*both* true and false positives so precision and recall are both measurable — a
TP-only record cannot fail a config on precision. One such pilot (kernel-8276, GT
10 TP / 4 FP) is what produced every number in this section.

### What the pilot established

| Config | Valid runs | Median recall | Per-run |
|---|---|---|---|
| `codex-5.6-luna` | 3/3 | **0.10** | 0.20, 0.10, 0.10 |
| `cursor-composer2` | 3/4 | 0.00 | 0.00, 0.10, 0.00 |
| `codex-5.4-mini` | 2/4 | 0.00 | 0.00, 0.00 |

**Single-pass recall against a saturated census is ~0.10.** The GT held 10
judge-confirmed defects from 4 collection rounds; the best single reviewer run found
2. A single review misses roughly 90% of what multi-round collection finds — an
effect an order of magnitude larger than the gap between any two configs. Read config
comparisons in that light: they are second-order next to the review-count effect.

**Precision is near-uninformative at low finding volume.** With 0-2 findings per run,
any reviewer that stays quiet scores 1.00. Precision is a guardrail against
recall-gaming, not a ranking signal, and at this volume it cannot separate configs.

**Backend stalls hit ~27% of attempts, unevenly** — one config lost 2 of 4 runs while
another lost 0 of 3. A stalled run is *not* a zero-recall review, so `replay.py` now
retries it (`--stall-retries`, default 2). Without that, a stall-prone config looks
worse than it is and medians are computed over uneven run counts. An `ok` envelope
with zero findings is never retried: that is a real result, and retrying it would
bias the sample toward chatty configs.

**The scoring pool is pr-polish-sourced PRs only.** `replay.py` samples on
`harvest_source`, defaulting to `pr-polish`; GitHub-sourced records are on disk but
out of the pool, because their ground truth is bot-derived and ~83% false positives
(see [Why pr-polish-only](#why-pr-polish-only)). `--source github` includes them for
auditing. Naming such a PR explicitly still runs it, with a warning — **never quote
those numbers alongside pr-polish scores**, since the two denominators measure
different things.

### Tuning the prompt for recall (2026-08-05)

> **Superseded in part — read [The reviewer was reading the wrong
> diff](#the-reviewer-was-reading-the-wrong-diff-2026-08-05) first.** Every
> measurement in this subsection was taken while the reviewer was inferring its own
> diff scope, and on kernel-8276 it inferred a 336-file diff instead of the change's
> 22. Re-running the tuning PR after the fix **doubled the baseline and erased the
> winning variant's lead**, so the ranking below is not safe to act on. The
> *diagnosis* (the reviewer is near-mute, not mis-aimed) and the *negative result*
> (deleting suppression clauses costs precision without buying recall) both still
> hold — those do not depend on diff size.

The pilot's `~0.10` recall raised the obvious question — *is the reviewer picking the
wrong defects, or is it not picking?* On kernel-8276 the answer was unambiguous.
Across 8 successful runs of 3 configs the reviewer emitted **12 findings total**
(median 1.5/run) against a diff holding **10 real defects**, at precision **1.00**.
Only 2 of the 10 GT defects were ever hit by anyone; **9 of 10 sat in files the
reviewer was explicitly handed.** The reviewer was accurate and nearly mute — a
volume problem, not a targeting one, and independent confirmation at the single-diff
level of the depth-not-scope diagnosis.

That pointed at the prompt's suppression clauses. There are **three**, and the
distinction between them turns out to matter:

| Clause | Lives in | Overridable? |
|---|---|---|
| `Prioritize systemic problems over local ones` | persona body | **yes** |
| `avoid nit-level comments unless they block understanding` | persona body | **yes** |
| `do not strain to find something to flag` | `codeJSONOutputRules` | **no** |

The third is in the machine-owned output contract that `--review-prompt-file` never
replaces, so **no persona variant can test its absence** — a fact worth stating
plainly, because it is easy to write a variant that appears to ablate it and does
not. The first two are persona-only and were live suppressors of exactly the
categories being missed: kernel-8276's GT contains 2 `low` and 1 `nit` defect.

Three variants were run on `codex-5.6-luna` (the only pilot config with a 3/3
completion rate — pairing a prompt variant with a stall-prone backend confounds the
prompt effect with the stall rate), 3 runs each, 0 stalls:

| Config | Median recall | Median precision | Findings | Distinct GT hit (3-run union) |
|---|---|---|---|---|
| `codex-5.6-luna` (baseline) | 0.10 | 1.00 | 8 | 2/10 |
| `luna-no-suppression` | 0.10 | **0.50** | 11 | 1/10 |
| **`luna-coverage-ledger`** | **0.20** | **1.00** | 13 | **4/10** |
| `luna-defect-class-priming` | 0.10 | 1.00 | 9 | 2/10 |

**Deleting the brakes does not produce recall — it produces noise.**
`no-suppression` removes the two persona suppressors and changes little else. Recall
stayed flat while precision *halved*: it was the only variant to land on known false
positives. This is the single most useful negative result here, because "just stop
telling it to be quiet" is the intuitive fix and it measurably does not work.

**What worked was reframing the obligation, not lifting the limit.**
`coverage-ledger` requires an explicit per-file conclusion across the diff — a floor
on *coverage*, never on bug count — paired with a hard grounding requirement ("do not
report a suspicion you could not confirm by reading the lines"). Recall doubled on
this PR with precision intact (see the held-out results below for how much of that
survives on diffs the persona was not written against). The defects it newly caught
are precisely the predicted classes:
the `preview_converge_back.py` DB/in-memory state-desync pair (both `high`) and the
`nit`-severity contract drift that `avoid nit-level comments` had been suppressing.

**Priming with defect archetypes did nothing** — notable because those archetypes
were written from this PR's own ground truth and still produced no lift. A variant
that cannot overfit its own answer key is unlikely to be learning a transferable
skill; that null result is cheap evidence against the whole approach.

Two cautions on reading this. The per-run finding counts are tiny, so a variant
moving 1.5 → 2.2 findings is a handful of findings, not a trend — the 3-run union
column is the more robust signal. And **a win on the PR you tuned against is not a
result**: `coverage-ledger` was designed while looking at kernel-8276's misses, so it
is promotable only if the lift transfers to PRs that were never consulted. Run the
held-out check before touching the Go default.

**Held-out transfer — a lift, but not a clean sweep.** Run on PRs never consulted
while writing the persona (3+ runs each, `--tier r1`):

| PR | baseline median R | `coverage-ledger` median R | |
|---|---|---|---|
| kernel-8229 | 0.20 | **0.40** | ✅ |
| kernel-3896 | 0.17 | **0.33** | ✅ |
| kernel-4023 | 0.40 | **0.30** | ❌ regression |
| **pooled (all runs)** | **0.200** (n=9) | **0.333** (n=11) | |

Precision stayed **1.00** for both configs on every held-out PR — the variant is not
buying recall with noise anywhere.

**Report the loss, not just the pooled win.** kernel-4023 is the PR where the
baseline was already strongest (0.40, its best result anywhere), and the ledger came
in below it. Two of three held-out PRs improved and the pool rose ~65%, but "recall
roughly doubles" is *not* what these numbers say — the honest claim is that recall
improves on average, with per-PR variance wide enough to swallow the effect on an
individual diff. With 3-4 runs per PR the per-PR medians rest on very few draws, so
kernel-4023 may be noise rather than a real regression; distinguishing those needs
more runs on that PR, not more PRs.

Note the baseline's held-out recall (~0.20) is double its kernel-8276 recall (0.10):
kernel-8276 is a harder-than-typical diff, so read absolute numbers per-PR and
compare configs only within a PR.

Two operational lessons from running this, both worth avoiding next time. Older
records' `head_before` commits are often absent from a local checkout — the branch
merged and was deleted — and replay failed with a bare `fatal: invalid reference`,
which reads like dataset corruption; `git fetch origin refs/pull/<N>/head` recovers
them, and replay now says so in the error (one PR was force-pushed away and is
permanently unreplayable). And a driver script that computed `bazel info bazel-bin`
*before* `cd`-ing into the workspace silently produced a bad binary path, so a whole
config's runs reported "done" while scoring nothing. **A batch that completes without
producing scores is not a null result — check the logs before reading the table.**

Variant personas live in `.claude/skills/code-review-replay/personas/` and are wired
as `replay.py` configs. `_persona_args` raises on a missing file rather than letting
bramble's warn-and-fall-back path run the baseline under the variant's name — a
silent fallback would report a wrong number that looks exactly like a valid null
result. Two tests pin the seam: `PersonaVariantConfigTests` (paths absolute and
non-empty) and Go's `TestPersonaOverride_ReplacesSuppressorsKeepsContract` (the
override lands, the two persona suppressors go, and goal text, the skip-test clause,
and the non-overridable do-not-strain copy all survive).

### The reviewer was reading the wrong diff (2026-08-05)

Auditing the reviewer's recorded sessions after the bake-off turned up something
that sits underneath every number above: **bramble never told the reviewer what the
diff was.** The prompt says "the diff" a dozen times and never defines it, and no
base ref was passed, so the agent reconstructed one — and its natural reconstruction
is wrong in exactly the situation replay creates.

Replay checks out a **detached worktree at `head_before`**. The agent, reasonably,
ran `git diff main...HEAD`. But local `main` has advanced far past the PR's merge
base, and the three-dot form diffs against the *merge base of main and HEAD* — which
is now some commit well after the PR landed. Reproduced on kernel-8276:

```
PR's true merge base   35e2b581      →  22 files
merge-base(main, HEAD) 07b98e9c      → 336 files
```

**The reviewer was hunting 10 defects across 336 files instead of 22.** Worse, the
guess was not stable: across 15 recorded sessions some runs used `main...HEAD`, some
`merge-base`, some both — so diff scope was an *uncontrolled variable* underneath
every score, and a plausible contributor to the run-to-run recall spread.

The ground truth was never affected: `collect.py build-prompt` hands the judge an
explicit `git -C <worktree> diff <merge_base>..<head_before>` plus the record's
`files_changed`. Only the reviewer side was guessing — the two halves of the
benchmark were scoped differently, which is the worst version of this bug because
nothing looks wrong in either half alone.

**Fix:** `--diff-base` / `--diff-head` on `bramble code-review`, emitted as a
machine-owned clause (so a persona variant cannot drop it) that states the exact
`git diff BASE..HEAD` and explicitly warns off `main...HEAD`. `replay.py` passes the
frozen record's `merge_base_sha`, so reviewer and judge now scope the diff
identically. Verified end-to-end on a live run: **12 commands using the correct base,
0 using `main...HEAD`**, against 8 wrong-scope commands in the pre-fix run of the
same PR.

Two deliberate design choices, both about not reintroducing a silent failure:

- A malformed revision is **rejected at the CLI** rather than silently producing a
  prompt with no scope clause — otherwise the run looks correct while the agent goes
  back to guessing. `ValidGitRevision` accepts what git revisions actually use and
  rejects whitespace, quotes, shell metacharacters, a leading `-`, and `..` (which
  would silently turn one revision into a range).
- `--diff-base` in design-doc mode is an **error, not a warning**: a doc review has
  no diff, and silently ignoring a scope the caller computed is the same class of bug.

**Re-measured on kernel-8276 after the fix** (same PR, same tier, 3+ runs per arm):

| Config | pre-fix median R | post-fix median R | precision |
|---|---|---|---|
| `codex-5.6-luna` (baseline) | 0.10 | **0.20** | 1.00 → 1.00 |
| `luna-coverage-ledger` | 0.20 | **0.10** | 1.00 → 1.00 |

**The baseline doubled — and the persona variant's advantage disappeared.** That is
the honest reading, and it is a warning about the earlier bake-off rather than a
result to celebrate. The most plausible explanation is that the two are *confounded*:
`coverage-ledger` works by forcing an explicit per-file conclusion across the diff,
which is exactly the discipline that helps when the diff is 15× too large and full of
unrelated files. Correct the scope and the problem it was compensating for is gone.

So **the prompt-variant ranking is now unsettled.** Every number in the section above
was measured against a mis-scoped diff, and while both arms shared that handicap —
which is why the relative comparison seemed sound — the handicap was not neutral
between them: it plausibly *favoured* the variant designed to cope with it. The
held-out transfer results are subject to the same doubt.

Do not promote `coverage-ledger` on the strength of the pre-fix numbers. Re-run the
bake-off — tuning PR and held-out set — with `--diff-base` in place. n=3 per arm here
cannot separate a real reversal from noise, so treat this table as a reason to
re-measure, not as evidence the variant is harmful.

The general lesson is worth keeping: **an uncontrolled variable in the harness can
survive a controlled A/B**, because "both arms had it" only protects you when the
variable is independent of what you are testing. Verify the harness measures what you
think before trusting a ranking it produces.

**Promoted to `/pr-polish` (2026-08-06).** It reviews a live PR branch, so its implicit
`main...HEAD` is usually right — but "usually" is doing real work there. A stale local
`main` narrows the diff (the reviewer never sees code the PR touched); a `main` that has
advanced widens it, exactly as here. All three reviewer invocations in
`pr-polish/SKILL.md` now pass `--diff-base "$(git merge-base origin/${BASE:-main} HEAD)"`,
keyed on the PR's own `baseRefName` rather than a hardcoded `main`.

The computation is failure-tolerant by design: a detached HEAD, a missing remote, or a
base branch absent locally leaves `DIFF_BASE` empty, the flag is omitted, and the reviewer
falls back to the previous inferred-scope behaviour rather than erroring. Verified to
survive `set -euo pipefail`, which the reviewer launch script uses.

This is the first benchmark finding promoted to production. It is a scope correction, not
a tuning change — no recall claim rides on it — so it does not need the held-out
validation the persona work does. **Re-measure escape rate after one cycle** to confirm it
moves the fleet number; revert per the criteria above if it does not.

### Ground truth in unmodified files — incomplete-change defects (2026-08-06)

**14 of 196 frozen true positives (7.1%), across 11 of 48 records, sit in files the PR
did not modify.** The first reading of this was that they are unreachable and should be
excluded from the denominator. That reading was wrong, and the topics say why:

> *"callbacks **still** use raw tenant comparison"* · *"callsites **still** missing
> `protect=True`"* · *"docstrings **still** claim PDFs are converted"* · *"template skill
> **still** instructs builders to install the removed extra"* · *"env vars not wired into
> the deploy-worker pod"*

These are **incomplete changes**: the diff establishes a rule, adds a field that must stay
in sync, or migrates some call sites, and leaves siblings behind. kernel-8276's entry is
the same shape — the judge recorded `routes/sandbox.py:2422` as *"the same missing
invariant as `archive_cascade.py:87`, anchored at a sibling writer."*

**The fix for these is to edit the unmodified file.** They are defects *in this change*,
they are among the most valuable things a reviewer catches, and they are not unreachable:
the whole worktree is on disk. What suppressed them was a prompt instruction —
*"reporting issues in code outside this diff is not [expected]"* — added alongside
`--diff-base` to stop the reviewer wandering. It stopped rather more than that.

The clause now reads: report a defect in an unchanged file **when this change is what
makes it wrong**, naming the incomplete-change shape explicitly, while still excluding a
pre-existing problem the diff neither creates nor touches. A test pins both halves and
fails if the suppressing sentence returns.

`collect_lib.unchanged_file_true_positives()` surfaces these so a miss can be
*attributed* — "missed a defect in changed code" and "missed an unfixed sibling site"
need different fixes. It is deliberately **not** a licence to shrink the denominator:
excluding them would score a reviewer as perfect while it waves through half-finished
migrations. `validate_dataset` warns and says so.

A guard worth keeping: a record with an empty `files_changed` flags nothing. Empty means
"unknown", not "nothing changed", and guessing would reclassify every entry.

### Measuring the corrected clause (2026-08-06)

Rerunning both configs under the corrected clause, 6 runs each, with the new wording
verified in the binary *and* in the live session logs:

| Arm | HIT | issues/run | citations in unmodified files |
|---|---|---|---|
| luna — old clause | **5** | 3.0 | 0 |
| luna — new clause | 4 | 4.5 | 2 (1 genuine) |
| composer — old clause | 2 | 2.7 | 2 |
| composer — new clause | 2 | 1.8 | 0 |

**The clause did not recover the target defect.** Neither model found
`sandbox.py:2422` — the single unmodified-file entry in this record — and luna's HIT fell
5 → 4.

Of luna's two new out-of-diff citations, only one survives inspection:

- `activities.py:247` — **genuine.** `workflows/session_deletion/activities.py` exists, is
  outside the diff, and the finding reasons correctly across module boundaries about
  `stamp_session_archived_at`. Precisely the behaviour the clause targets.
- `archive_cascade.py:74` — **not an out-of-diff finding at all.** The reviewer emitted
  `src/api-gateway-service/` (hyphens) where the real path is `src/api_gateway_service/`
  (underscores). A hallucinated path for a file that *is* in the diff. Worth noting as its
  own failure mode: a mistyped path silently converts an in-scope finding into an
  unmatched one.

**Keep the clause, but do not claim a recall win.** The old wording provably suppressed a
class worth 14 of 196 corpus true positives, and one real cross-module finding appeared
under the new one. That justifies the change on reasoning. It does not demonstrate a
measured improvement, and a 1-hit swing at n=6 is inside the noise band this benchmark has
shown repeatedly.

**The instrument is wrong for the question.** kernel-8276 has exactly *one* unmodified-file
GT entry, so it can barely register the effect. Measure this on `kernel-3860` (3 of 3
entries) or `kernel-8188` (2 of 5), where the class dominates the record.

### Measuring recall-first work honestly

Optimizing for recall "even at some precision cost" breaks the scoreboard in a way worth
understanding before reading any such variant's numbers.

**Precision cannot see the cost, and recall cannot see the benefit.** A finding scores
three ways: it lands on a frozen true positive, on a frozen false positive, or on
neither. That third bucket — `unmatched` — is excluded from precision entirely, and
contributes nothing to recall. Measured on kernel-8276: **8 of 20 baseline findings were
unmatched.** A recall-first variant dumps most of its extra output into exactly that
bucket, where "found a real defect the census missed" and "generated plausible noise"
score *identically*, which is to say: not at all.

Two facts make this concrete rather than theoretical:

- Precision's denominator is tiny. Across 6 baseline runs, `matched_tp + matched_fp` was
  12. A precision of 1.00 is **one flipped finding from 0.92**. Report the counts, not
  the ratio.
- Several unmatched locations recur across independent runs *and across configs* — the
  signature of a real defect, since noise is drawn from a wide space and rarely collides
  twice on the same line.

**The discriminator is cross-run recurrence**, and it is free. `unmatched_lib.py` groups
unmatched findings by the same `same_defect` identity the GT uses (±3 lines, so ordinary
line drift counts as agreement rather than as two singletons) and reports:

| bucket | meaning |
|---|---|
| `unmatched_recurrent` | ≥2 independent runs hit it — candidate real defect |
| `unmatched_cross_config` | ≥2 *different configs* hit it — the strong form |
| `unmatched_singleton` | one run, one config — probable noise |

Run it with `replay.py --unmatched-report`. On the post-fix kernel-8276 archives this
surfaced 3 recurrent locations (2 cross-config) against 5 singletons.

**Decision rule for a recall-first variant:** promote when distinct TPs caught rises,
`matched_fp` stays near baseline, and unmatched growth concentrates in `recurrent`.
Reject when unmatched growth concentrates in `singleton` — *regardless of what recall
did*. That is `no-suppression` in a different costume, and the singleton concentration
is the tell that would have caught it a round earlier.

Recurrent findings are **not** recall credit. Feed them through `collect.py` so the
census ratchets upward through the judged path; scoring them ad hoc would mean every
variant is graded against a ground truth its own output shaped.

### What the missed defects actually look like (2026-08-05)

Six persona variants had failed to move recall, so instead of designing a seventh, the
individual misses were examined. The per-defect hit rate on kernel-8276 across 24 runs is
not a gentle distribution — it is bimodal:

| hits / 24 | defect |
|---|---|
| **21** | `archive_cascade.py:87` — unlink clears `artifact_id`, leaves the sibling marker |
| 7 / 3 / 2 | three test-coverage and nit findings |
| **0** | `preview_converge_back.py:332` and `:537` — DB cleared, in-memory copy not synced |
| **0** | `archive_cascade.py:100` — repurposes a PLA-695 lifecycle column |
| **0** | `preview_desired.py:118` — redundant SELECT on a hot poll path |
| **0** | `test_archive_cascade.py:29` — unscoped `project_id=` path untested |
| **0** | `sandbox.py:2422` — writer sites don't clear the terminal marker |

**Six of ten defects were found by zero runs.** Three facts explain why, and none of them
is "the reviewer didn't look":

1. **It read the exact lines.** 23 of 23 sessions ran a command covering
   `preview_converge_back.py:332`. Every session opened every implicated file.
2. **It described the defect.** 17 of 24 runs named a stale/superseded terminal-marker
   problem in their summary or an issue message. One summary reads: *"terminal-generation
   state survives setpoint cleanup."* That is the bug.
3. **It filed it somewhere else.** Only **2 of 24** cited a line inside
   `preview_converge_back.py`, and **none** landed within the ±3 slack of :332 or :537.
   The rest attributed it to `archive_cascade.py:87` — the DB write — rather than to the
   function that fails to sync the in-memory copy.

So the dominant failure is **localization, not detection**. The reviewer holds the right
model of the bug and cites the wrong line, and the scorer — correctly, since a finding
you cannot act on at the cited line is not actionable — reads that as a miss. No amount
of "find more" prompting addresses this; every recall-first variant left the
described-but-not-localized gap exactly where it was.

A second pattern runs through all six: **each sits under confident prose asserting the
invariant holds.** `archive_cascade.py:100`'s docstring says it is *"Independent of the
PLA-695 `deletion_status` lifecycle"* — and the defect is that it repurposes a PLA-695
column. `preview_converge_back.py:537` carries a comment saying *"retire it before the
gate"* immediately above a gate that reads the un-retired in-memory value. Good prose by
a careful author is where these hide, because the prose is what stops the reader looking.

**One miss is not localization.** `preview_desired.py:118` (a redundant SELECT on a hot
path) appears in **0 of 24** runs' prose — it is genuinely undetected, not mislocalized.
Efficiency-on-a-hot-path is a separate gap and needs a separate mechanism; don't let the
localization story absorb it.

The diagnostic that separates these is worth keeping: for a given known defect, count how
many runs *describe* it versus how many *cite a matching line*. Aggregate recall cannot
tell "never saw it" from "saw it, cited the wrong line", and those need opposite fixes.

### Encoding the localization principle — the first variant that worked

The diagnosis above says the reviewer knows the bug and cites the wrong line. So the
principle to encode is not "find more" but **"finish the finding"**:

> Name the bad state in one phrase. Find the line that **creates** it — the write, the
> assignment, the omission. Find the line that **consumes** it — the later read that
> behaves wrongly. Cite the creating site as `file`/`line`, the consuming site in
> `sites`. *If you can describe a defect but cannot point at the line where the wrong
> thing happens, you have not finished the finding.*

Run as `luna-localize-only`, with `luna-file-at-the-read` adding a second section
telling the reviewer to treat docstrings as claims under test. 6 runs each:

| Config | distinct TP (of 10) | matched_fp | median R | findings/run | cites right file | within ±3 |
|---|---|---|---|---|---|---|
| `codex-5.6-luna` (baseline) | 4 | 4 | 0.15 | 7.5 | 0/6 | 0 |
| **`luna-localize-only`** | **5** | 10 | **0.20** | 9.0 | **6/6** | 2 |
| `luna-file-at-the-read` | 4 | 6 | 0.15 | 8.0 | 5/6 | 0 |

**`localize-only` is the first variant in seven to beat the baseline**, and it caught
`preview_converge_back.py:332` — a *high*-severity defect that 24 prior runs across 6
configs never found. The precision cost (`matched_fp` 4 → 10) is the trade that was
explicitly authorized.

**The docstring-skepticism half did not help and looks dilutive.** `file-at-the-read` is
the same persona plus that section; it moved right-file citations (0 → 5) but produced no
new true positive and carries the worst noise profile of any config (24 unmatched, 17 of
them singletons). Two principles competing for attention appear to be worse than one
applied well — a result only visible because the ablation was run alongside.

**A scorer caveat this exposed.** The ±3 line slack is too tight for this defect class.
The create-site (`:537`) and consume-site (`:543`) of one defect are **6 lines apart**, so
a reviewer citing the consuming gate — a correct, actionable citation — scores as a miss.
`localize-only` produced **5 citations in the 4–10 line band** on top of its 2 inside the
slack. Those are very likely correct findings the scorer rejects. Before reading any
localization result as final, decide whether the slack should widen for
create/consume-pair defects, or whether such GT entries should carry both sites. Widening
slack indiscriminately would also make false-positive matching sloppier, so this is a
real trade, not an oversight to patch away.

### Reading the misses by hand, not through the scorer

The ±3 identity rule answers "did this citation match a frozen entry", which is the right
question for a benchmark and the wrong one for *improving the reviewer*. Adjudicating the
misses against the actual code — reading the diff at `head_before` — found three things
the scorer structurally cannot report.

**1. Two GT gaps, confirmed in source.** The invariant "clearing `artifact_id` must also
clear `preview_converge_terminal_artifact_id`" has **three** sites in this diff. GT
records one (`archive_cascade.py:87`). The reviewer also cites `sandbox_gc.py:563`
(`sandbox.artifact_id = None`) and `inf_2822_drain_archived_setpoints.py:206`
(`.values(artifact_id=None)`) — both verified real, both scored as `unmatched` noise.
The census is incomplete on a defect it already knows about.

**2. A near-miss that is correct, and one that is not.** These look identical to the
scorer and are opposites:

- `:543` vs GT `:537` (Δ6) — **correct**. `:537` is the create-site (the
  `_clear_superseded_terminal_generation` call); `:543` is the consume-site (the
  `_generation_already_terminal` gate that reads the stale value). One defect, two
  actionable lines, 6 apart. The slack rejects it.
- `:87` vs GT `:100` (Δ13) — **not correct**. `:87` is inside
  `unlink_execution_setpoint`; the column-repurposing defect is in
  `stamp_session_archived_at`. Proximity is coincidental.

So *don't* widen the slack indiscriminately — it would absorb the second case along with
the first. The principled fix is for a GT entry to carry both the create-site and the
consume-site when they differ, and to match either.

**3. A new failure mode the aggregate metrics hid: one-directional field tracing.**
Across **36 runs** the reviewer cited **all three** sites that *clear* `artifact_id` and
**zero** sites that *write* it — there is not a single citation anywhere in `sandbox.py`,
where GT holds a high-severity writer-site defect at `:2422`. It searches "who nulls this
field" and never "who sets it." If clearing A obliges clearing B, every site that *sets*
A carries the same obligation, and those writer sites usually live in a different module
from the cleanup code being read.

Encoded as `personas/localize-plus-sweep.md`: enumerate every site that clears, writes,
and reads the field before writing the finding.

**Two residual misses are not localization at all**, and shouldn't be absorbed into that
story:

- `preview_desired.py:118` — a redundant SELECT on a hot poll path. **0 of 24** runs
  mention it even in prose. Genuinely undetected; an efficiency-class gap needing its own
  mechanism.
- `test_archive_cascade.py:29` — a test that *does not exist*. There is no line where the
  wrong thing happens, so an absence-of-code defect may be structurally unfileable under a
  `(file, line)` identity. Worth deciding how the benchmark should represent these at all.

### Gemini needs a longer idle timeout (2026-08-05)

There is **no `gemini-3.5-pro`** in `cursor-agent models` (193 models, exactly one Pro
tier). The available Gemini models are `gemini-3.1-pro`, `gemini-3.5-flash`, and the
3.6-flash effort tiers.

Running them through bramble's **cursor** backend — not the `gemini` backend, which is
rejected at the account level — initially returned `status: error` with
`0 input / 0 output tokens` and *"review idle: no events for 3m0s (stalled backend)"*.
That reads like a dead backend, and it is not. The diagnosis chain was:

1. `cursor-agent -p --model gemini-3.1-pro` → replies fine.
2. Same with `--output-format stream-json` (bramble's actual invocation) → streams
   correctly, identical event shape to composer-2.5.
3. Same with a tool-using prompt → 16 tool calls, completes.
4. bramble's full review prompt at the default 3m idle timeout → stalls.
   **At `--idle-timeout 8m` → `ok`, 4 issues.**

Gemini thinks *silently* for over three minutes before emitting its first event on a
review-sized prompt. Bramble's idle timer resets on events, not on progress, so a long
silent prefix trips it. Both Gemini configs therefore carry
`--idle-timeout 8m` as a **requirement, not tuning**.

The general lesson: a `status: error` envelope with zero tokens is not evidence the model
is unavailable. Before writing a backend off, check the CLI directly, then the CLI with
bramble's exact flags, then the real prompt — the failure was three layers below where it
surfaced.

### Iterating to convergence, and the two ceilings that stop you

Successive rounds, each designed from hand-adjudicating the prior round's misses. Metric
is HIT = a citation within the ±3 identity slack; all runs `gpt-5.6-luna`, 6 per round,
persona is the only variable.

| Round | Mechanism | HIT | issues/run | sites/issue |
|---|---|---|---|---|
| 0 | baseline | 4 | 3.0 | 2.50 |
| **1** | **localization: name the bad state, cite create-site + consume-site** | **5** | 3.0 | 3.00 |
| 1.5 | + docstring-skepticism *section* | 4 | 3.0 | — |
| 2 | + field-sweep *section* | 3 | 4.0 | 3.08 |
| 3 | writer clause folded into the localization step | 4 | 5.2 | 2.55 |

**Round 1 is the winner and nothing since has beaten it.** Three consecutive rounds
regressed, which is the stopping condition.

**A measurement correction worth keeping.** Early rounds appeared to raise "findings/run"
to 12–13. That number counted *sites* after `expand_finding_sites`, not issues. Issues per
run is **flat at 3.0** across the baseline and every persona. The personas do not change
how much the reviewer says — they change **where it points**. Round 1 wins by raising
sites-per-issue from 2.50 to 3.00 *with the extra sites landing accurately*; rounds 2–3
add issues whose extra sites drift off the GT lines. Always separate issue count from site
count before concluding a variant is "more verbose".

**The additive-instruction effect is real and repeatable.** Every round that added a
*section* to a working persona lost hits, including round 3, which folded its clause into
an existing step and still regressed. The reviewer behaves as if it has a fixed attention
budget: more instructions produce more issues and worse localization. Prefer replacing a
clause over appending one, and re-measure after every addition.

**Ceiling 1 — one GT defect is unreachable.** `sandbox.py:2422` is **not in
`files_changed`** and `git diff` shows **zero changed hunks** in that file. The judge
censused a real defect in *unmodified* code. A diff-scoped reviewer cannot be expected to
file it, so the achievable maximum is **9/10, not 10/10**, and every recall figure in this
document understates the reviewer by that entry. Worth deciding whether collection should
censusing outside the diff at all, or mark such entries so scoring can exclude them.

**Ceiling 2 — the ±3 slack rejects correct citations.** Two of Round 1's NEAR misses are
real hits on adjudication: `:543` vs GT `:537` (create-site vs consume-site of one defect,
6 lines apart) and `:279` vs GT `:275` (GT anchors to the `allow_dml(db)` line; the
citation sits in the same test body). Round 1's true score is therefore **7 of 9
reachable**, not the 5 of 10 the scorer reports. A third NEAR — `:87` vs GT `:100`, Δ13 —
is *not* a hit (different function), so widening the slack indiscriminately would launder
a miss into a pass. The principled fix is for a GT entry to carry both sites when the
create and consume locations differ.

**What remains genuinely missed**, after all rounds:

- `preview_desired.py:118` — redundant SELECT on a hot poll path; **0 of 24** runs mention
  it even in prose. Efficiency-class, and the one mechanism aimed at it has not yet been
  measured on a non-regressed base.
- `test_archive_cascade.py:29` — a test that does not exist. GT anchors it to the import
  block; 2 of 41 runs mention the defect and both file it in a different file. Probably
  unfileable under a `(file, line)` identity, which is a benchmark-design question rather
  than a prompt one.
- `archive_cascade.py:100` — column repurposing beneath a docstring asserting
  independence. Never detected, and the persona explicitly written to target it did not
  help.

### Recall-first variants, and the point where prompt tuning should stop

Asked to pursue recall *even at some precision cost*, three further variants were run
(6 runs each, after both harness fixes). None removed suppression clauses — that was
already measured and failed — and each instead gave an uncertain-but-real defect
somewhere to go other than silence.

| Config | findings/run | distinct TP (of 10) | matched_fp | unmatched recurrent / singleton |
|---|---|---|---|---|
| `codex-5.6-luna` (baseline) | 7.5 | **4** | 4 | 9 / 7 |
| `luna-confidence-band` | 6.0 | 3 | 2 | 7 / 5 |
| `luna-severity-floor` | 7.2 | 2 | 5 | 8 / 6 |
| `luna-adversarial-successor` | 7.0 | 1 | 5 | 9 / 5 |

**No variant beat the baseline**, and they did not even buy the extra volume they were
designed to buy — all four sit at 6.0–7.5 findings/run.

Two diagnostics keep this from being over-read:

- **`confidence-band` never engaged its own mechanism.** It was built around reporting
  sub-0.9-confidence suspicions; **0 of its 16 findings carried confidence below 0.9**,
  identical to baseline. That is an *uninformative* null — the variant didn't fail, it
  never ran the thing being tested. Always check that a variant's mechanism fired before
  recording its result.
- **`severity-floor` did engage** — the only config to emit `low`-severity findings (6 of
  them) — and still gained no recall while `matched_fp` rose 4 → 5. That null is real.

Six persona mechanisms have now failed to move recall on this PR: removing suppression,
coverage obligation, defect-class priming, confidence banding, severity-floor
authorization, and adversarial reframing. At some point the consistent answer across
unrelated interventions stops being about the prompt.

**The likelier explanation is the denominator.** Every config produces 12–16 unmatched
locations, of which **7–9 recur across independent runs** — a candidate-defect population
comparable in size to the entire 10-entry frozen census, on a diff whose census was
declared converged. Recall of "0.15" against a census that is plausibly missing as many
defects as it contains is not measuring the reviewer.

**Recommendation: stop tuning the persona against this census.** Adjudicate the recurrent
unmatched findings — human pass or another collection round — re-freeze, and re-measure
only then. Prompt work against a census this incomplete is fitting to noise, and every
variant above is evidence of how little signal there is to fit.

### Class-level findings were being scored at one site (2026-08-05)

A second harness bug, found while designing the above. The prompt *instructs* collapsing
N sibling violations of one invariant into a single issue carrying a `sites` array, with
the top-level `file`/`line` naming one representative. `replay.py` scored only that
representative — so a reviewer that **obeyed the contract** was credited with 1 of N.

Measured on kernel-8276: **46 of 73 issues carried a `sites` array, and 90 reported
defect locations were discarded before scoring.**

`replay_lib.expand_finding_sites` now scores every reported site. But re-scoring the
archives produced a result worth recording, because it contradicts the obvious
expectation: **visible findings went 20 → 41 while recall did not move at all** — the
same 5 distinct GT entries — and precision fell 1.00 → 0.67. Every extra site either
re-hit an already-caught defect or landed on a known false positive.

So this was a correctness and *honesty* fix, not a recall lever. The previously-reported
1.00 precision was partly an artifact of not counting the sites the reviewer actually
claimed. Do not read the lower number as a regression, and do not expect this fix to
raise anyone's recall.

### The three-run rule

**Run every variant at least 3 times per PR and compare distributions, not single
draws.** Reviewer output is substantially nondeterministic: the same config, on a
byte-identical diff and goal, has produced four different verdicts across four runs —
including differing on *which* defect it found. A single-run A/B measures the coin
flip, not the config. Report medians with spread.

### Reading the scoreboard

- **Low recall, high precision** — conservative: what it flags is real, but it misses
  bugs. See `missed_true_positives`.
- **Low precision** — noisy: it repeats known false positives (`matched_fp`).
- **Many `unmatched`** — it surfaced things the dataset never judged. These are
  candidates for the next collection round, **not** wins. Ground truth is a lower
  bound on real bugs; scoring above it means collection missed something.

## Promotion checklist

1. Median recall over ground-truth true positives **rises**.
2. Median precision **does not regress**; `matched_fp` does not climb.
3. The **held-out split** (~20% of the corpus, never used for tuning) confirms the
   gain. A variant tuned on the corpus can overfit it.
4. Promote: reviewer flags into pr-polish's invocation; a winning prompt into the Go
   default, keeping the file override for future experiments.
5. Re-measure the fleet escape rate after one cycle.
6. **Revert if the escape rate does not move.**

## Corpus hygiene

- **Rotate the held-out split** periodically, and never tune against it. Once a
  variant has been evaluated on it, its independence is partly spent.
- **Re-collect** a PR when replay surfaces `unmatched` findings that turn out to be
  real — that means the frozen ground truth is incomplete.
- **Retire stale PRs** whose code has drifted far enough that the diff no longer
  represents current review difficulty.
- **`index.json` is rebuilt from every on-disk record**, so a filtered harvest cannot
  truncate it. If entries ever go missing, that invariant has regressed — check it
  before trusting any sampling that reads the index.
- **Corpus size is bounded by pr-polish adoption, not by harvesting.** The scoring
  pool currently stands at **19 GT'd PRs out of 37 pr-polish records** — short of the
  100+ target, and not closable by harvesting more aggressively. Growth comes from
  routing more PRs through the polish loop. Treat the pool size as a tracked number
  each cycle: if it is flat, the benchmark is not getting stronger no matter how many
  bake-offs run against it.

## The defect-identity collision, and what it cost

Defect identity in the accumulator is `(normalized_file, line)` with `_LINE_SLACK`
rows of tolerance. Two judge verdicts that collide on that key merge into one entry —
and if their verdicts *disagree*, the entry flips with each additional colliding
verdict, landing in `contested` where no later round can resolve it: every resolution
is immediately re-flipped by the next. Two shapes trigger it:

- two file-level (`line: null`) findings on the same path, and
- two line-level findings within `_LINE_SLACK` rows in the same file.

Observed on kernel-8229, whose one round emitted four file-level findings on
`reservation_archive_release.py`: they became a single entry cycling
`TP → FP → FP → TP`, and the PR could not converge no matter how many times a judge
re-ruled it. Eight collected PRs carried the shape.

**The fix is two-layered.** `_entry_matches` no longer lets a file-level verdict
re-rule line-level entries (`None` matches only `None`), and
`validate_judge_verdict` rejects any two *conflicting* verdicts that share an
identity — at the input boundary, before a fold can corrupt anything. Agreeing
verdicts at one location still merge, which is the intended dedupe. The judge prompt
also states the rule directly, so judges avoid emitting the shape rather than having
it caught after the fact — across a subsequent 8-PR re-collection, **zero** verdicts
tripped the validator on this.

**What it cost, measured.** The eight affected PRs were re-collected from scratch.
Their false-positive counts fell from **122 to 8** — a ~15× inflation, caused by the
collision splitting single contradictory rulings into repeated entries. True positives
fell too (27 → 15), which is the same effect in the other direction: entries that had
flipped into `contested` and back were being counted as independent confirmations.
Censuses grew substantially over the same re-collection.

The lesson worth keeping: a corrupt `contested` list does not stay contained. It
inflates the noise floor a benchmark measures precision against, so "only the
bookkeeping is wrong" was too generous a reading — the headline numbers moved by an
order of magnitude once the records were rebuilt.

Two further PRs (kernel-8229, kernel-8331) were blocked outright by the same
collision — unresolvable contested entries meant no amount of re-judging could
converge them. Both were recovered after the fix: kernel-8331 needed only a clean
re-fold of its existing verdicts, and kernel-8229 needed one duplicate verdict
dropped. Neither required a fresh review.

Detect the shape in any verdict file with:

```bash
python3 - <<'PY'
import json, glob, os, sys, collections
sys.path.insert(0, ".claude/skills/code-review-replay/scripts")
import collect_lib as cl
for vf in glob.glob(os.path.expanduser(
        "~/.bramble/code-review-eval/collect/*/r*/judge-verdict.json")):
    c = collections.Counter()
    for v in json.load(open(vf)).get("finding_verdicts") or []:
        if v.get("line") is None and v.get("verdict") != "unsure":
            c[cl.normalize_finding_path(v.get("file")) or ""] += 1
    for path, n in c.items():
        if n > 1:
            print(f"{vf}: {n} file-level verdicts on {path}")
PY
```

## Validating that a frozen GT is *correct*

`collect.py validate` checks that a record is **well-formed**. It does not check that
it is **right**, and the difference is not academic — two real defects were caught only
by looking past the schema:

- A judge returned an **empty census** on an 18-file authz diff. Structurally perfect.
  A replacement judge, asked to actually read `derived_grants.py`, found four real
  defects including a self-perpetuating state bug where an expired-but-unrevoked row
  occupies the active-key slot so the drift can never heal.
- A record's `head_before` was a **stale local branch tip**, so its diff read as 218
  files / +16,496 when the PR is 3 files / +208. Every census entry would have been
  keyed to code no reviewer will ever see. `validate` passed it without comment.

So run **two** checks before trusting a batch:

**1. Structural** — `collect.py validate --all`. Expect `0 malformed`; "N with warnings"
is normal.

**2. Scope** — compare each record's `files_changed` against GitHub's own count. A ratio
near 1.0 is right; slightly under is expected (harvest excludes deleted/binary files).
Anything above ~1.5x means the stored head is not the PR's head:

```bash
python3 - <<'PY'
import json, glob, os, subprocess
ds = os.path.expanduser("~/.bramble/code-review-eval/dataset")
for f in sorted(glob.glob(f"{ds}/kernel-*.json")):
    d = json.load(open(f))
    if not d.get("ground_truth_v3"):
        continue
    num = os.path.basename(f)[:-5].split("-")[1]
    stored = len(((d.get("harvested_rounds") or [{}])[0]).get("files_changed") or [])
    out = subprocess.run(["gh", "api", f"repos/<owner>/<repo>/pulls/{num}",
                          "--jq", ".changed_files"], capture_output=True, text=True)
    gh = int(out.stdout.strip()) if out.stdout.strip().isdigit() else None
    if gh and stored / gh > 1.5:
        print(f"{num}: stored {stored} vs github {gh}  ({stored/gh:.1f}x) INFLATED")
PY
```

Repair via GitHub's compare API, which yields the true merge base and file list:
`gh api repos/<owner>/<repo>/compare/<base>...<head>`. Then re-setup and re-collect
that PR — its old verdicts are scoped to the wrong diff and must be discarded.

**3. Semantic (blind re-judge)** — the check that actually tests correctness. Re-judge a
sampled PR on a fresh worktree with the same reviewer findings and seeds but **no
visibility into the frozen verdicts**, then compare. On kernel-8462 this returned 3/3
verdict agreement, with severity drifting one step on one entry — consistent with the
earlier finding that verdicts are stable while severity is not, which is why
`severity_mismatches` is scored separately from precision and recall.

Expect the blind judge to census defects the frozen set lacks (7 on kernel-8462,
including two high-severity). Those are **GT gaps to feed back into collection**, not
reviewer wins — and they are direct evidence that `census_converged: false` on a record
is honest rather than pessimistic.

## Known failure modes

| Signature | Cause | Response |
|---|---|---|
| `review idle: no events for 3m0s (stalled backend)` | codex stalls, notably under parallel load | Retry. Never feed the envelope to the judge. |
| `This client is no longer supported for Gemini Code Assist` | gemini CLI decommissioned at the account level | Not retryable; exclude the backend. |
| Envelope with `status: "error"` | Any backend failure | `build-prompt` refuses these by design. A failed review is **not** a finding-free review — folding one in tells the judge a reviewer looked and found nothing, inflating the apparent false-negative rate. |
| Same config, different verdicts across runs | Reviewer nondeterminism | Expected. Invalidates single-run comparisons; see the three-run rule. |
| Zero escapes reported for every run | Reading `pp-comments.json` instead of the harvested record | That file is captured *during* the run, so it cannot contain post-completion comments. The metric needs the dataset's re-fetched census. |
| Every escape reported out-of-scope | Reading `files_changed` from pr-polish state | State does not persist it; only the harvested record computes it. Inverts the depth-vs-scope signal. |
| A bulk harvest emits `API rate limit exceeded` per PR and keeps going | GitHub's **secondary** rate limit, which fires while `gh api rate_limit` still shows thousands of core requests remaining — so quota checks look fine | `_run_gh` now backs off (30s/90s/240s) and `harvest.py` **stops the run** on an unrecovered rate limit. Without the stop, every remaining PR silently degrades to the state-recorded comment set and writes a record that looks harvested but has no external review census. Re-run after the reset; harvested PRs are skipped. Delete any records written during the degraded window. |
| `fold` aborts: `census_merges[N].members must be a list of >=2 census locations` | A judge invented its own merge shape (e.g. `{file, merged_from}`) | Translate it faithfully to `{"members": [...], "reason": ...}` rather than discarding — the reasoning is usually sound, only the shape is wrong. Keep the schema warning in the judge prompt. |
| `census_converged: false` with a stable census | `unresolved_contested > 0` | Convergence needs contested findings *resolved*, not just a stable census. Judges must give a binding verdict on every `contested_findings` entry; say so explicitly in their prompt. |
| `fold` reports `unresolved_contested` > 0 that no judge round can clear — resolving it re-opens it next fold | Two file-level (`line: null`) verdicts on one path collapsed into one entry and flip each other | `validate_judge_verdict` now rejects this at the input boundary. For verdicts already on disk, anchor all but one finding to the line it concerns and re-fold. See "Known-affected datasets" above. |
| `unresolved_contested` never clears no matter how many times the judge is re-prompted | **Fixed.** The flip branch used to mark an entry `resolved=False` and move it to `contested` *only*. That was unresolvable by construction — the disagreement is created by that very verdict, so no round could re-rule an entry that was not contested when the round began — and worse, it dropped the entry from the TP/FP buckets, so the defect vanished from the frozen ground truth rather than merely being flagged. A later round's judge is now treated as the authority: its verdict is binding, the entry lands in the bucket that verdict names, and `contested` becomes a permanent audit record (`resolved=True`) instead of a quarantine queue. | Nothing to do. If an old record still shows the flag, delete its `cumulative.json`, re-fold each round once in order, and re-freeze — kernel-8229 and kernel-8329 both recovered this way, and 8229 converged. |
| A bake-off config scores 0 recall and looks strictly worse than its rivals | Some of its runs stalled (`status: error`) and were scored as zero-recall reviews. Stalls are not evenly distributed across configs — one lost 2 of 4 runs while another lost 0 of 3 | `replay.py` retries stalled runs (`--stall-retries`, default 2). Check the per-run `envelope_status` before believing a config's median, and never mix 2-run and 3-run medians in one table. A 2-run median is also unstable by construction: one config's median moved 0.05 -> 0.00 when its third run landed. |
| A judge sub-agent reports "idle/available" but its verdict file is unchanged | The agent stopped without doing the work. An idle notification is **not** evidence of progress, and silence is indistinguishable from considered agreement | Check the verdict file's **mtime** against when you sent the request. If it predates your message, the agent did nothing — replace it rather than accept its prior output. Observed: a judge returned an empty census on an 18-file diff, went idle three times under challenge without touching the file, and a replacement judge then found 4 real defects including a self-perpetuating state bug. |
| A judge sub-agent never starts, silently | Spawn failed (commonly tmux pane exhaustion) but nothing surfaces as an error | Confirm liveness in the process list after spawning; don't infer it from the spawn call returning. Stop finished agents to free panes before spawning more. |
| A judge dies mid-run (`Connection closed mid-response`) | Transient API failure | Nothing is written, so re-spawn. Instruct judges to **write the verdict file before any optional exploration** — a focused verdict that lands beats an exhaustive one that never does. |
| Every reviewer run stalls with `review idle: no events for 3m0s` | Backend degradation, not concurrency or effort | Test one model against another before concluding it is model-specific. Measured in one window: `gpt-5.6-sol` succeeded 1/16 across high effort, medium effort, solo, and staggered; `gpt-5.4-mini` succeeded 7/8 on the same PRs and box. Quarantine every `status: error` envelope as `.STALLED.json` — it carries zero findings and would tell the judge a reviewer looked and found nothing. |
| `fold` run twice on the same round inflates `per_round_diff` and `verdict_history` | `fold` is **not idempotent** — it appends and mutates | Delete `cumulative.json` and re-fold each round once, in order. Read the fold output *before* freezing: `should_continue: false` at budget exhaustion looks identical to true convergence unless `census_converged` is read separately. |
| A later round's `census` omits an entry an earlier round recorded | Judge silently dropped it instead of declaring a merge | **Not data loss** — the accumulator is union-only, so `cumulative.json` retains it and only an explicit `census_merges` can collapse entries. Observed once in the pilot (a medium-severity entry vanished from a round-2 verdict; the frozen census still carried it). Worth noticing as a judge-compliance signal, but the ground truth is protected by construction. |

## Related

- Mechanics: `.claude/skills/code-review-replay/SKILL.md`
- Consumer of the tuned config: `.claude/skills/pr-polish/SKILL.md`
- Escape metric: `.claude/skills/pr-polish/scripts/escape_rate.py`
