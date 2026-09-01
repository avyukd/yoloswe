# Research swarm

## What was retrofitted

`subagent-swarm` runs software-engineering lanes: one task per bramble worktree, phases
`swe → clean → review → integrate`, a ledger, a watcher, a `/loop` tick, and PRs. The same
machinery now runs investment-research lanes against a praxis-style research repo
(`/home/ubuntu/dev/praxis-ox-alpha`), through a new skill `.claude/skills/research-swarm/`:

- a research lifecycle, `hunt → redteam → record → integrate`, mapped one-to-one onto the
  existing phase mechanics (a lane's `phase` and `status` are still separate axes in
  `ledger.py`; `.done` files, notes files, `FORK_SHA..HEAD` scoping and fresh sessions per
  phase are unchanged);
- six lane types with brief templates (`dated-event`, `channel-refresh`, `new-ground`,
  `correction`, `deeper-research`, `verifier`), each naming its ownership, its reserved
  actions, its exit gate and its literal report paths;
- the research repo's own standing run protocol and mechanical gates
  (`channelcheck`, `limitprice`, `reconcile`, `currency`, `yamlcheck`, `futuredate`,
  `briefcheck`, `edgarwatch`, `liqmon`, `salemon`, `uspx`, `ukpx`) as the lane's first steps
  and the record phase's checklist;
- the doctrine the lanes inherit, distilled with citations to `FRAMEWORK.md` section and rule
  numbers and to the telemachus analyst mandate;
- orchestrator rules that only make sense for research: dispatch by expected value (dated
  event > correction > refresh > new ground), a channel registry with exhaustion dates and
  null streaks, an observable terminal proof, and "every `.done` is a claim: read the diff
  and run the checkers yourself";
- a loaded example, `examples/research-swarm/praxis-ox-alpha.md`, built from the repo's own
  calendar and channel files as of 2026-09-01.

## What was deliberately not changed

- **No Go code.** bramble, `wt`, the delegator and the IPC protocol are untouched. The skill
  uses `bramble new-session`, `list-sessions`, `send-input` and tmux exactly as
  `subagent-swarm` does.
- **`subagent-swarm` itself.** Its `SKILL.md`, references and scripts are unmodified so upstream
  merges stay clean. `research-swarm/scripts` is a relative symlink to
  `../subagent-swarm/scripts`; `ledger.py`, `watch_lanes.sh`, `tmux_safe.sh`, `poll_panes.sh`,
  `snapshot_at_risk.sh` and `audit_cleanup.sh` are not duplicated. If the scripts ever grow a
  research-specific need, the change belongs upstream, behind a flag.
- **The research repo.** Nothing in praxis-ox-alpha is modified by installing the skill. Lanes
  work on bramble worktrees of it and produce branches and PRs; the human merges.

## How a research lane maps onto bramble worktrees and PRs

| subagent-swarm | research-swarm |
|---|---|
| a task with a scoped code change | one research UNIT: a dag node (plus artifacts) on one name or channel |
| `swe` session implements and proves | `hunt` session: `channelcheck` first, the file's last word, the primary filing, the six conditions, a draft node, a commit |
| `clean` session simplifies the diff | `redteam` session: adversarial pass over `FORK_SHA..HEAD` with a fixed checklist; MAJOR sends the lane back to `hunt` |
| `review` session applies the review gate | `record` session: the repo's checkers from the worktree root, the node made the file's last word, fragments for shared files, a commit |
| `integrate`: PR / merge as authorized | `integrate`: the orchestrator re-runs the checkers, pushes `swarm/<lane>`, opens one PR with the verdict, the six-condition table, the honest-null statement and literal paths; never merges |
| a `.done` verified against commits | a `.done` verified against the diff AND the checkers AND the file's last word |
| terminal proof: the feature works | terminal proof: a set of observables the prompt names (exhaustion dates within cadence, next-information nodes present, `reconcile.py` zero, RECORD nodes on open PRs) |

The worktree is the isolation boundary, the branch is the unit of review, and the PR is the
handoff — none of that changed. What changed is what "done" means: a research lane is done when
its node is the last word of its file, its figures trace to primaries it read, the checkers are
clean, and a fresh session failed to break it.

## The ownership rule for dag files

`dag/*.yaml` files are append-mostly notes that the repo's tools parse by regex; two sessions
appending to the same file on different branches produce a merge conflict at exactly the place
that matters (the end of the file). So:

- a lane may create or edit only the dag files (and `research/<NAME>/` artifacts) named in
  its brief, and no two running lanes name the same file; the orchestrator assigns names and
  channels so ownership is disjoint, and a `correction` lane is briefed with an exclusion list;
- shared surfaces — `dag/WATCH-CALENDAR.yaml`, `dag/BOOK-STATE-*.yaml`, the memo
  `out/final.html`, and the roster constants inside `tools/limitprice.py`,
  `tools/edgarwatch.py`, `tools/briefcheck.py` — are orchestrator-only; lanes write fragments
  and requests into the run directory (`$RUN/calendar/<lane>.yaml`, `$RUN/<lane>.republish.md`,
  `$RUN/<lane>.brief-defects.md`) and the orchestrator carries them onto one calendar branch
  with its own PR, appended serially;
- the run directory lives outside the repo (`$HOME/.local/state/research-swarm/<repo>/<ts>`),
  because the research repo has no `.gitignore` and a lane's `git add` must never be able to
  pick up swarm state.

## Known limits

- **Regex-YAML fragility.** The dag files must parse (`tools/yamlcheck.py`, enforced by the
  repo's pre-commit hook in every worktree) and are read by regex anchors (`found:`, `status:`,
  `budget: { total:`, `UPPER_SNAKE_MMDD:` node names). A lane that restructures a node, quotes
  a `>` folded scalar differently, or writes a `:` inside prose unquoted can break a checker
  silently or block its own commit. The skill tells lanes to append and never restructure; it
  cannot make that impossible.
- **FTS lag and rate limits.** EDGAR full-text indexes same-day filings late, so the last day
  of every monitor window is provisional and re-run next day; `forms=` under-returns; XML
  13D/G cover pages are not indexed. Yahoo answers 429; the surviving free US source is the
  Nasdaq screener (one whole-universe request); several lanes hammering EDGAR FTS at once will
  rate-limit all of them, so the orchestrator serializes FTS-heavy lanes. A feed error is an
  error, never a zero.
- **Prices.** `uspx.py` and `ukpx.py` carry session guards (US 13:30–20:00 UTC; LSE hours plus
  2026–27 bank holidays) so an intraday print is never recorded as a close; the daily close
  record is therefore a scheduled orchestrator job after 20:05 UTC / 16:35 UTC, and lanes
  that quote a price must record the tool's session line beside it.
- **The memo artifact.** `out/final.html` is hand-edited HTML with a dozen checkers pointed at
  it; republishing is a human/orchestrator action. The swarm only ever produces a
  `REPUBLISH REQUESTED` block in a PR body. A lane whose finding changes a published conclusion
  is not finished until that request exists, and the human is not obliged to act on it.
- **Backends.** A Codex or Gemini redteam lane gives second-model independence but cannot be
  given a reliable reporting instruction; the orchestrator must verify its notes file exists
  and read the pane. Bramble's status is accurate whatever backend ran; the notes are not.
- **Nothing here trades, sizes, merges, or notifies a human channel.** The PR title/body and
  `$RUN/OBJECTIVE.md` are the only outputs a person is expected to read.
