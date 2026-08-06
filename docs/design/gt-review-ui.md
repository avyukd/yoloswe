# Ground-Truth Review UI — Design Plan

A local, single-user web UI for **human second-pass review of the code-review
ground-truth dataset**: render each PR's diff with judged findings anchored to
their lines, so an editor can confirm, reject, re-severity, or add what the
judge missed.

Status: **plan only**. No production code written.

---

## 1. Why, in one paragraph

`ground_truth_v3` is the fitness function for every reviewer-config decision.
It is known-incomplete, not merely imperfect: on kernel-8276, 15 unmatched
defect locations recurred across independent runs (14 across two configs)
against a frozen census of 10 true positives. Judges have also made concrete,
accidentally-discovered errors — one froze a GT against a stale branch tip, one
returned an empty census without reading the diff. A human pass is the missing
link, and its only real requirement is that it be **fast enough to actually
happen** and **safe enough not to corrupt the benchmark**.

---

## 2. What I verified before designing (not from memory)

Every number below was measured this session against the live dataset. Several
contradict the brief's starting assumptions, and two change the design.

### Corpus shape

| Measure | Value |
|---|---|
| Records on disk | 836 + `index.json` |
| Records with frozen `ground_truth_v3` | **48** |
| — `harvest_source: pr-polish` (the scoring pool) | **40** |
| — `harvest_source: github` (excluded from scoring) | **8** |
| `census_converged: true` | **18 / 48** |
| Unresolved contested entries, whole corpus | **0** |
| GT entries total | **326** (196 TP / 128 FP / 2 contested) |
| `line: null` (file-level) entries | **5 (1.5%)** |
| Severity spread | low 120, medium 94, nit 63, high 49 |
| TP per PR | median 4, max 16 |
| FP per PR | median 1, max 39 |
| Diff size (renderable) | median 10.5 files / 725 added lines; max 48 files / 12,495 lines |

Two consequences. The `github` tier is on disk but out of the scoring pool
because its GT is bot-derived and ~83% false positives — **the PR list must
show `harvest_source` and default to filtering it out**, or a human will spend
their scarcest resource adjudicating findings that no bake-off will ever read.
And with zero unresolved contested entries corpus-wide, "resolve contested" is
not a live workflow; contested is an audit trail. Show it, don't build a queue
for it.

### Diff reconstructability — better than the brief assumed

The brief expected graceful degradation on a handful of records. Measured:

| State | Count |
|---|---|
| `head_before` + `merge_base_sha` both present locally | **32** |
| Recovered by `git fetch origin refs/pull/<N>/head` + merge-base recompute | **+8** |
| Genuinely unrecoverable (commit exists nowhere) | **8** |

**40 of 48 are renderable, not 32.** The 16 initially-missing records are *not*
all force-pushes. Their `merge_base_sha` is `null` with
`merge_base_error: "head commit not in local repo"` — the harvester could not
compute a merge base because the head was absent *at harvest time*. Once
`refs/pull/<N>/head` is fetched (I ran this; it succeeded), the head is present
and `git merge-base origin/main <head_before>` resolves for 8 of them, yielding
sane diffs (2–39 files).

This matters beyond the UI: **8 records are currently unreplayable for a reason
that is fixable with a fetch and a recompute, not a data loss.** The UI should
surface that state as *actionable* ("fetch and recompute") rather than as a dead
record. I flag this as a dataset-repair finding in its own right — see §9.

### Where findings actually land — the layout-defining measurement

I mapped every non-null GT line against the new-file side of the reconstructed
diff hunks across all 32 immediately-renderable PRs:

| Anchor outcome | Count | Share |
|---|---|---|
| Inside a rendered diff hunk | 139 | **71.6%** |
| In a changed file, but **outside** any hunk | 35 | **18.0%** |
| File **not in the diff at all** | 20 | **10.3%** |
| `line: null` (file-level) | 5 | — |

**A diff-only UI would silently hide 28% of the findings it exists to review.**
This is the single most important design constraint here, and it is not
something the brief anticipated. Causes are legitimate, not corruption: judges
census defects in code the PR *touches the behavior of* but does not edit
(kernel-3860's three `NOFILE` entries), and `±3` slack plus post-hoc line drift
pushes others just past a 3-line context window.

The rendering model therefore cannot be "render the diff, attach findings."
It must be **"render the finding set, and give every finding a home"** — with
the diff as the primary but not exclusive surface. See §5.

---

## 3. Stack decision: Python, extending the existing skill scripts

**Recommendation: Python, as a new `reviewui.py` alongside `collect.py` in
`.claude/skills/code-review-replay/scripts/`, serving a server-rendered page
with `http.server`/`wsgiref` and vanilla JS.**

Reasoning, in priority order:

1. **The identity rule cannot be reimplemented.** `same_defect`, `_entry_matches`,
   `normalize_finding_path`, and `_LINE_SLACK` are subtle, load-bearing, and have
   already cost this project one 15× false-positive inflation when they went
   wrong. A Python UI **imports `collect_lib`** and gets the exact rule. A Go UI
   would have to port it, and a ported copy is a second source of truth that will
   drift — precisely the failure mode the brief's constraint #1 warns about.
2. **`unmatched_lib.collect_unmatched` is directly reusable** for the suggestion
   feed. In Go it is a rewrite.
3. **The write path is Python.** Any fold-in subcommand lives in `collect.py`
   regardless of UI language, so Go would split one feature across two languages.
4. **No Bazel/gazelle friction.** A Go binary needs `bazel run //:gazelle` and
   BUILD maintenance for a single-user local tool that never ships.

Go would win if this needed to be a durable multi-user service. It does not.

**Dependencies: standard library only.** No Flask, no FastAPI. The server is
~150 lines of `wsgiref.simple_server` with a handful of JSON endpoints; adding a
dependency to a skill-scripts directory that currently has none is a cost with
no matching benefit. Frontend is one HTML page with vanilla JS — the diff view
is a table and some fetch calls, not an application.

Testing follows the existing convention: `scripts/tests/test_reviewui.py`,
pure-function tests over the anchoring and overlay logic, no server in the loop.

---

## 4. The write path — the riskiest part, reasoned explicitly

The brief asks me to justify this specifically. Here is the reasoning and the
decision.

### The constraint

`fold` is **not idempotent** — it appends to `per_round_diff` and `verdict_history`
and mutates buckets. Running it twice on one round inflates the record; the
documented recovery is "delete `cumulative.json`, re-fold each round once, in
order." Hand-editing the dataset JSON is explicitly forbidden. So the human's
verdicts cannot be written by the UI directly, and cannot be laundered through a
naive second `fold` either.

### Options considered

**A. UI writes `ground_truth_v3` directly.** Rejected outright. It bypasses every
validator (`validate_judge_verdict`, `_file_level_collision_error`), and one bad
write corrupts the benchmark's source of truth with no audit trail.

**B. UI emits a synthetic judge verdict and calls the existing `fold`.** Tempting
— it reuses the whole validated path. **Rejected**, for a reason specific to how
`_route_finding_verdict` works: folding a human verdict as "round N+1" makes the
human a *judge round*, so a human rejection of an existing TP takes the **flip**
branch. That moves the entry to FP, marks it `resolved=True`, *and* appends it to
`contested` — permanently recording a machine-vs-human disagreement as if two
judges disagreed. It also bumps `rounds_run` and appends to `per_round_diff`,
corrupting the convergence bookkeeping: `census_converged` would then be
computed partly from human input, so a record could "converge" because a human
agreed with it. The human pass is a **different kind of authority** from a judge
round, and collapsing the two loses that distinction irreversibly.

**C. Overlay file + explicit `collect.py apply-human-review` subcommand.**
**Chosen.**

### The design

**The UI only ever writes one file, and it is not the dataset.**

```
~/.bramble/code-review-eval/human-review/<repo>-<pr>.json
```

Append-only, one record per PR, carrying:

```jsonc
{
  "schema_version": 1,
  "target": "kernel-8276",
  "gt_frozen_at": "2026-05-22T16:51:06Z",   // the GT this pass reviewed
  "gt_fingerprint": "sha256:...",            // hash of the frozen TP/FP/contested block
  "verdicts": [
    {
      "op": "confirm" | "reject" | "reseverity" | "add",
      "file": "path/x.py",
      "line": 30,                            // null legal, = file-level
      "severity": "high",                    // required for reseverity/add
      "topic": "...",                        // required for add
      "reason": "human rationale",
      "reviewer": "mingzhao",
      "at": "2026-08-05T21:00:00Z"
    }
  ]
}
```

Then, separately and deliberately:

```bash
python3 collect.py apply-human-review kernel-8276 [--dry-run]
```

which is a **new subcommand**, not a reuse of `fold`. Its properties:

- **Reads the overlay, validates it, and rewrites the frozen block in place.**
  It does *not* touch `cumulative.json`, does not bump `rounds_run`, and does not
  append to `per_round_diff`. The human pass is not a round.
- **Idempotent by construction.** Every op is keyed by `(normalized_file, line)`
  under `_entry_matches` (the strict null-to-null rule, *not* `same_defect` —
  reusing the loose rule here would let one file-level human verdict re-rule
  every line-level entry in that file, which is exactly the kernel-8229 bug).
  Re-applying the same overlay is a no-op: the entry already carries that
  `human_verdict`, so the op is skipped, not re-appended.
- **Refuses on fingerprint mismatch.** If `gt_fingerprint` no longer matches the
  record's frozen block, the GT was re-collected after the human reviewed it.
  The subcommand aborts with a message telling the operator to re-review, rather
  than applying stale verdicts to a changed census. This is the guard against the
  stale-branch-tip class of error, applied to the human path.
- **Runs `validate_judge_verdict`-equivalent checks**, including
  `_file_level_collision_error` on the combined set, before writing anything.
- **Writes via `hl.atomic_write_json`** and refreshes `index.json` through
  `cl.refresh_index_entry` — the same path `freeze` uses.

### How a human verdict is represented in the frozen block

Additively, in a way that is **reversible and attributable** and that no existing
consumer misreads:

- `confirm` — adds `human_verdict: {verdict, reviewer, at, reason}` to the entry.
  Bucket unchanged. Pure annotation.
- `reject` — **moves** the entry between `true_positives` and `false_positives`,
  stamps `human_verdict`, and stamps `human_moved_from: "true_positives"`. It does
  **not** append to `contested` and does **not** append to `verdict_history` —
  `verdict_history` is the judge-round trail and must stay machine-only, or the
  provenance of a verdict becomes unreadable.
- `reseverity` — sets `severity`, preserving the prior value as
  `judge_severity`. Note `reviewer_severity` already means "what the reviewer
  reported", so a third name is required; overloading it would corrupt the
  existing severity-drift analysis.
- `add` — appends a new entry with `first_seen_round: null`,
  `surfaced_by: ["human"]`, and `human_verdict`, so it is visibly not
  judge-derived.

**Every mutation carries `human_verdict`, so the entire human pass is
separable.** A `--strip-human-review` inverse restores the pre-human block
exactly: reverse the moves via `human_moved_from`, restore `judge_severity`,
drop `first_seen_round: null` entries, delete the stamps. That is the
reversibility requirement met concretely rather than aspirationally.

### What this deliberately does not do

It does not touch `census_converged`. A human confirming findings is not census
convergence, and letting a human pass flip that flag would make the
convergence signal — already the weakest number in the dataset at 18/48 — mean
two different things.

---

## 5. Rendering model

Given the 28% off-hunk measurement, the layout is **finding-first with diff
context**, not diff-first.

**PR list** (`/`): one row per GT record — target, `harvest_source` (github tier
visually de-emphasized and filtered out by default), TP/FP/contested counts,
`census_converged`, human-pass progress (`n/m adjudicated`), and render state
(`renderable` / `needs-fetch` / `unrecoverable`). Sort default: pr-polish,
unreviewed, most unmatched-suggestions first — that ordering puts the
highest-value work on top.

**PR detail** (`/pr/<target>`): two panes.

- *Left, the finding ledger* — every GT entry plus every unmatched suggestion,
  grouped by anchor class: `in-hunk`, `in-file-off-hunk`, `file-level`,
  `not-in-diff`. This grouping is what guarantees nothing is hidden; the three
  non-primary classes hold 28% of the corpus and are exactly the entries a
  diff-only view drops. Each row: severity, topic, bucket, `surfaced_by`,
  `judge_reason` (expandable), and the four actions.
- *Right, the code surface* — for `in-hunk`, the reconstructed diff with the
  finding pinned to its line. For `in-file-off-hunk`, the **file at
  `head_before`** windowed ±20 lines around the finding, clearly labeled as
  file-context-not-diff. For `not-in-diff`, the same but flagged, since the file
  isn't part of the change at all. For `file-level`, the file header with no line
  gutter.

Diff reconstruction is `git -C <repo> diff <merge_base>..<head_before>`, matching
what `collect.py build-prompt` hands the judge, so the human sees exactly the
scope the judge was asked to rule on. Repos auto-discover at
`~/worktrees/<name>/main` then `~/g/<name>`. Rendering is server-side; the 12,495-line
outlier is paginated per file rather than streamed whole.

**Adding a finding**: click a line in either surface. The UI pre-fills `(file,
line)` from the click, then — critically — **runs `same_defect` against the
existing GT before accepting it** and warns "this is within ±3 lines of an
existing entry; you are editing that entry, not adding a new one." This is
constraint #1 enforced at the input boundary rather than discovered at fold
time.

---

## 6. The suggestion feed

Measured across 66 scored replay archives, unmatched findings exist for **5
PRs**:

| PR | distinct | recurrent | cross-config | singleton |
|---|---|---|---|---|
| kernel-8276 | 32 | 15 | 14 | 17 |
| kernel-3896 | 5 | 4 | 3 | 1 |
| kernel-8229 | 6 | 1 | 0 | 5 |
| kernel-4023 | 2 | 2 | 0 | 0 |
| kernel-8377 | 1 | 0 | 0 | 1 |

Note the scope reality: **the highest-value input the brief describes currently
exists for 5 of 48 PRs**, and is heavily concentrated in one. That is not a
reason to skip it — kernel-8276's 15 recurrent locations against a 10-entry
census is the whole thesis — but it does mean the suggestion feed should be
built as a *ranked overlay on the ledger*, not as a separate mode that is empty
for 90% of the corpus.

Ingest by globbing `replays/*-scored.json`, joining on `dataset_file`, walking
`rounds[].runs[].finding_scores[]` for `outcome == "unmatched"`, and passing
`(run_id, config, file, line)` tuples straight into
`unmatched_lib.collect_unmatched`. Rank cross-config above recurrent above
singleton. Adjudicating one is just an `add` op — it flows through the identical
overlay path, so a promoted suggestion is indistinguishable from any other human
addition, which is correct: the census should ratchet up through one door only.

---

## 7. Read-only by default

The server starts read-only. Mutation requires `--allow-write`, and even then the
UI writes only the overlay — never the dataset. Applying to the dataset is a
separate, explicit CLI invocation the human runs deliberately. Three gates
between a stray click and the benchmark: overlay-only writes, an explicit flag,
and a separate apply step with `--dry-run`.

Binds `127.0.0.1` on an ephemeral port. No auth — single-user, loopback, and the
brief says don't over-engineer it. Nothing from `~/.bramble/code-review-eval/`
is ever copied into the repo tree; the server reads it in place and the overlay
is written back under `~/.bramble/`.

---

## 8. Build order

1. **Read path**: loader, repo discovery, diff reconstruction, anchor
   classification, PR list. Ships value immediately — this alone makes the GT
   inspectable for the first time.
2. **Ledger + code surface**, including the three non-hunk anchor classes.
3. **Overlay writes** (`confirm`/`reject`/`reseverity`/`add`) with the
   `same_defect` guard at input.
4. **`collect.py apply-human-review`** + `--strip-human-review`, with tests
   covering idempotency, fingerprint mismatch, and the null-line collision rule.
5. **Suggestion feed.**

Steps 1–2 are read-only and carry no risk to the dataset. Step 4 is where the
tests matter most, and it is deliberately last among the mutating pieces so the
overlay format is settled before anything writes to the benchmark.

---

## 9. Findings outside the UI's scope, surfaced because I hit them

1. **8 records are recoverable, not lost.** `merge_base_sha: null` on 16 records
   is a harvest-time artifact of a missing head, not a force-push. Fetching
   `refs/pull/<N>/head` and recomputing the merge base recovers 8, taking the
   renderable pool from 32 to 40 and the *replayable* pool up correspondingly.
   This is a `harvest.py`/`collect.py` repair, not a UI feature, and it is worth
   doing independently of this project. The remaining 8 are genuinely gone.
2. **The 8 `github`-tier GT records are a trap for a human reviewer.** They carry
   real frozen GT that cost judge tokens, but are excluded from scoring. Without
   an explicit filter, a human would burn effort on findings no bake-off reads.
3. **`census_converged` is true for only 18/48.** Consistent with the doc's
   guidance to treat recall as an upper bound; worth showing prominently in the
   list so the human prioritizes unconverged records, where the census is most
   likely incomplete.

---

## 10. Open questions

1. **Attribution identity** — hardcode `$USER`, or a `--reviewer` flag? Matters
   only if this ever runs on more than one box.
2. **Should `apply-human-review` refuse on an unconverged census?** I lean no —
   unconverged records are exactly where human input is most valuable — but it is
   a defensible gate.
3. **Do you want the `github` tier hidden entirely, or filtered-by-default?** I
   assumed the latter; the records are real and may be worth auditing.
