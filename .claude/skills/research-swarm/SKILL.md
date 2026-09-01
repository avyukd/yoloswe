---
name: research-swarm
description: Orchestrate bramble lanes that run investment-research units (dated events, channel refreshes, new ground, corrections, verifiers) against a praxis-style research repo through a hunt → redteam → record → integrate lifecycle with disjoint dag-file ownership and an observable terminal proof. Use when the user types `/research-swarm`.
disable-model-invocation: true
---

# research-swarm

Same machinery as `subagent-swarm`, different work. A lane is one RESEARCH UNIT on one
bramble worktree of the research repo; its output is a `dag/*.yaml` node (plus artifacts),
committed on its branch, red-teamed by a fresh session, gated by the repo's own checkers, and
handed to the human as a PR. The orchestrator owns the goal, the channel registry, dispatch,
verification, and every write to a shared surface. It never trades and never merges.

Mechanics (ledger, watcher, pane polling, snapshots, cleanup, safe kill) are the
`subagent-swarm` scripts; this skill's `scripts/` is a symlink to them. Before the first
spawn, read and run the preflight in
[../subagent-swarm/references/bramble-mechanics.md](../subagent-swarm/references/bramble-mechanics.md).
Read [references/praxis-protocol.md](references/praxis-protocol.md) (the standing run protocol
and the gate commands every lane runs), [references/research-lane.md](references/research-lane.md)
(lifecycle, brief contract, lane-type templates, PR body), and
[references/opinions.md](references/opinions.md) (the doctrine, with citations).

## 0. What this swarm believes

Distilled from FRAMEWORK §0–§3 and the telemachus analyst mandate; full form with citations in
`references/opinions.md`. Every lane brief inherits these; do not paste them, point at them.

- The edge is comprehension, not valuation craft or access. Hunt where the documents are
  dense and public; avoid where the market is structurally better (mega-cap multiples,
  management access, cycle timing).
- The retail edge test: name who is selling and why they must. "The market hasn't noticed"
  is not an answer. "Why is this left for me?" must have a structural reply.
- Read the actual documents. The document that CREATED the fact (proxy, circular,
  prospectus, indenture, purchase agreement exhibit) outranks the one that reports on it.
- Consensus is data, not truth; and 99% of the time the market is right. Steelman the price
  before fighting it.
- "Too Hard", "Pass", and a null are valid, frequent outputs. A run that finds nothing says
  so. Never manufacture a seventh sweep to avoid reporting a null.
- A correction to a published number outranks a new name. Corrections are the highest-value
  unit and are always allowed.
- Every number carries a source and a vintage (accession or RNS id, retrieval date, listing).
  Derived numbers are labelled derived. Superseded numbers are marked, never deleted.
- Never state a return figure that was not derived from a primary filing the lane itself read.
  Finders return facts and accessions; the bar is applied afterwards, never stated to a finder.
- The kill is the cost of finding the survivor, then go get more. An idle lane slot with
  dispatchable expected value is a defect.

## 1. Lanes, units, and the lifecycle

Treat the invocation as the run contract: goal, terminal proof (a set of OBSERVABLES, not a
lane count), repo path and base branch, concurrency, models per phase, authorities, human
gates, schedules, and the first wave. Prompt values always beat the defaults below.

Default lifecycle when the prompt gives none: `hunt -> redteam -> record -> integrate`.

| Phase | Session | Outcome |
|---|---|---|
| `hunt` | fresh, lane worktree | channelcheck first; read the file's LAST word; read the primary filing; compute the six conditions; draft the dag node; commit |
| `redteam` | FRESH session, same worktree | adversarial pass over `FORK_SHA..HEAD` only: arithmetic seam, six conditions, three standing filters, "was every return figure derived from a primary the lane read?", "is every superseded figure annotated?"; MAJOR returns the lane to `hunt` |
| `record` | fresh session, same worktree | run yamlcheck, futuredate, briefcheck, reconcile, limitprice (and currency/xfile where touched); make the node the file's last word; write the calendar fragment; commit; `.done` |
| `integrate` | orchestrator, no session | verify diff and checkers yourself; push the branch; open the PR with the verdict, six-condition table, honest-null statement, literal artifact paths; never merge |

Lane types, each with a brief template in `references/research-lane.md`:
`dated-event` (a filing or vote that has LANDED; if nothing landed, say so),
`channel-refresh` (re-run a monitor against its population; record the exhaustion date),
`new-ground` (zero across ≥3 `channelcheck` synonyms before it may open),
`correction` (a scanner hit or a published number that is wrong; ownership by exclusion),
`deeper-research` (a named gap on an existing name, e.g. "read the outside date in the
purchase agreement"), `verifier` (adversarial red-team of a PUBLISHED conclusion, output is
CONFIRMED / REFUTED with the refuting document quoted).

One unit per lane. Priorities: `p0` a limit touched, a live-book event landed, or a published
number found wrong; `p1` a dated event or a correction lane; `p2` refreshes and new ground.
Keep phase separate from status. Recurring monitors (daily close record, weekly MFN pull,
weekly FTS re-sweep) are schedules; materialize a lane when due, never hold a slot for one.

~~~bash
SW=~/.claude/skills/subagent-swarm/scripts     # or ~/.claude/skills/research-swarm/scripts
RUN=$HOME/.local/state/research-swarm/$(basename "$REPO")/$(date +%Y%m%d-%H%M%S)   # OUTSIDE the repo
python3 "$SW/ledger.py" init "$RUN" --goal "<goal>" \
  --phases "hunt:,redteam:,record:,integrate:" --base main --target main
python3 "$SW/ledger.py" add "$RUN" --id ncc-takeup-0902 --title "NCC take-up RNS, record only" \
  --branch swarm/ncc-takeup-0902 --priority p1
~~~

The run directory lives outside the repo on purpose: the research repo may have no
`.gitignore`, and nothing a lane can `git add` may ever include swarm state.

## 2. Ownership and reserved actions

- A lane may create or edit only the `dag/*.yaml` files (and `research/<NAME>/*.md`
  artifacts) its brief names. Two running lanes never own the same file. The orchestrator
  assigns names and channels so ownership is disjoint; a `correction` lane is briefed with an
  EXCLUSION list (every file owned by a running lane plus the orchestrator-only files) and
  reports any correction that belongs in an excluded file to its notes for a follow-up lane.
- Orchestrator-only, always: `dag/WATCH-CALENDAR.yaml`, `dag/BOOK-STATE-*.yaml`, the memo
  artifact (`out/final.html`) and any republish of it, roster constants in
  `tools/limitprice.py` / `tools/edgarwatch.py` / `tools/briefcheck.py` unless a brief assigns
  them, pushing, PR creation, and any human notification. A lane that has a calendar entry,
  a brief defect, or a republish request writes it to `$RUN/calendar/<lane>.yaml`,
  `$RUN/<lane>.brief-defects.md`, or `$RUN/<lane>.republish.md`; the orchestrator carries it.
- Nobody in the swarm merges, trades, sizes, or posts to a human channel. A limit touched is a
  `p0` lane and a `[LIMIT TOUCHED]` prefix on OBJECTIVE.md and the PR title, nothing more.
- Lanes commit on their own branch only, early and often, with the hook installed: the
  research repo's pre-commit runs `tools/yamlcheck.py` and a lane must never use `--no-verify`.

## 3. The orchestrator

`$RUN/OBJECTIVE.md` is the control page that survives compaction. It carries the goal, the
terminal-proof observables and which are currently evidenced, the CHANNEL REGISTRY, the
schedule table with next-due times, authorities and gates, literal recovery coordinates, the
loop command, integrated work, prioritized gaps, and the next action.

Channel registry, one row per channel the run touches:
`channel | exhaustion date | population (what was enumerated) | re-sweep trigger | null streak | last lane`.
Seed it from `grep -n 'exhausted_against_population_as_of\|exhaustion_dates_to_carry\|re_sweep_trigger' dag/*.yaml`.
Three consecutive nulls on the SAME ground mark the channel exhausted with today's date and
switch ground; nulls on different grounds do not count against each other.

Dispatch by expected value, in this order: a dated event that has actually landed; a
correction to a published number; a channel refresh whose exhaustion date is older than its
cadence; new ground. A `deeper-research` lane ranks by the delta of the gap it closes; a
`verifier` lane is due whenever a PR would change a published figure or the prompt names a
conclusion to test. Refill freed slots immediately. Serialize EDGAR full-text-heavy lanes:
one FTS sweep in flight at a time, or the feed rate-limits every lane at once.

Every `.done`, idle report, and pane hint is a claim. Before advancing a lane: read
`git -C "$WORKTREE" diff "$FORK_SHA..HEAD" -- dag/ research/` yourself; confirm the node is
the file's last word and carries `found: <today>`, its sources by accession or RNS id, and a
six-condition block (or an explicit null statement); then run, from the worktree root,
`python3 tools/yamlcheck.py && python3 tools/futuredate.py && python3 tools/reconcile.py && python3 tools/briefcheck.py`.
A `.done` with no commit is not done. A return figure with no accession is a MAJOR finding
whatever the lane wrote. A hunt node that contradicts a later node in the same file (the
NCC 0901 failure) is sent back to `hunt` with the later node named.

Integrate only when the prompt authorizes PRs: push `swarm/<lane>` to the research repo's
remote and open one PR per lane against the base branch using the body contract in
`references/research-lane.md` (verdict paragraph, six-condition table, honest-null
statement if null, redteam verdict, checker output, literal artifact paths, and a
`REPUBLISH REQUESTED` block when a published conclusion changed). Calendar fragments and
book-state findings go on one orchestrator branch `swarm/calendar-<YYYYMMDD>` with its own PR,
appended serially, so lane PRs never touch the shared files and never conflict.

The terminal proof is the prompt's observables, checked by command, not "N lanes done".
Typical: every channel in the registry has an exhaustion date not older than its cadence;
every live-book name has a dated next-information node; `tools/reconcile.py` reports zero
absent figures; every dated event in the wave has a RECORD node on an open PR. Keep an
uncovered dimension as an explicit proof gap until a command evidences it or the user
narrows the goal.

## 4. Loop reminder

After the first dispatch, arm the watcher and schedule a health tick every 20 minutes unless
the prompt says otherwise. Longer schedules (close-price record, weekly pulls) live in the
OBJECTIVE schedule table and run when due. The reminder carries literal recovery state:

~~~text
/loop Orchestrating research-swarm: <goal>. SELF=<session id>. RUN=<absolute run dir>.
BRAMBLE_SOCK=<literal socket path>. REPO=<absolute research repo path>.
Read <absolute run dir>/OBJECTIVE.md and run the tick in
~/.claude/skills/research-swarm/SKILL.md §4. Terminal proof: <observables, verbatim>.
Before ending a nonterminal tick: persist the channel registry, gaps and priorities,
apply calendar fragments, arm one watcher, and schedule the next tick.
~~~

Each tick:

1. Re-read `OBJECTIVE.md`. List due schedules (price record after 20:00 UTC US / 16:35 UTC
   UK, weekly pulls, event launch times that have passed) and the shortest path to proof.
2. Drain `$RUN/*.done` and report files; run `poll_panes.sh "$RUN"` once; verify each claim
   against the diff and the checkers (§3) before advancing, reworking, or integrating.
3. Advance lanes: `hunt -> redteam` (spawn a FRESH session on the same worktree), `redteam ->
   record` or back to `hunt` on MAJOR, `record -> integrate` (orchestrator). Run
   `snapshot_at_risk.sh "$RUN"` before any reap.
4. Apply new `$RUN/calendar/*.yaml` fragments to the calendar branch; run due schedules
   yourself (read-only tools) and turn any touched limit or landed filing into a `p0`/`p1`
   lane; update the channel registry (exhaustion dates, null streaks).
5. Reassess the proof observables, materialize due work, reprioritize by expected value, fill
   free slots; update OBJECTIVE and the ledger; `audit_cleanup.sh "$RUN"`; reap the old
   watcher and arm one new watcher in separate calls; schedule the next tick.

After compaction, first confirm the loop exists and recreate it from the reminder. Stop only
when every terminal-proof observable is evidenced by a command, all lanes are terminal,
every authorized PR is open (none merged by the swarm), the calendar PR carries every
fragment, and cleanup is accounted for. If lanes finish before then, staff the next gap;
if nothing has expected value before the next dated event, say so in OBJECTIVE and wait —
waiting for a dated event is a valid posture and a null is a valid report.

Report: PRs opened (with the one-paragraph verdict each), corrections to published numbers,
honest nulls with exhaustion dates, republish requests awaiting the human, blockers with
unblock conditions, the channel registry, and the ledger path.
