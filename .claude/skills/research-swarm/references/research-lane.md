# The research lane

Use this lifecycle when the invocation does not define another one. Prompt-supplied models,
effort, tools, skips, gates and approvals always win. Mechanics (spawn, record, watch, reap,
recover) are in `../../subagent-swarm/references/bramble-mechanics.md`; this file is the lane's
semantics.

| Phase | Session | Exit evidence | Next |
|---|---|---|---|
| `hunt` | fresh agent, lane worktree on `swarm/<lane>` off base | commit(s) on the branch containing the dag node (and any `research/<NAME>/` artifact), `$RUN/<lane>.hunt.notes.md`, `$RUN/<lane>.hunt.done` written LAST | `redteam` |
| `redteam` | FRESH agent, same worktree, scoped to `FORK_SHA..HEAD` | `$RUN/<lane>.redteam.notes.md` ending in one verdict line `VERDICT: MAJOR|MINOR|CLEAR`, `$RUN/<lane>.redteam.done`; no dag edits | `hunt` on MAJOR, else `record` |
| `record` | fresh agent, same worktree | checkers clean from the worktree root; node is the file's last word; calendar fragment / republish request / brief-defects written if applicable; commit; `$RUN/<lane>.record.notes.md`; `$RUN/<lane>.record.done` | `integrate` |
| `integrate` | orchestrator | diff and checkers re-run by the orchestrator; branch pushed; PR open with the body contract; ledger `merge_sha` left empty (the human merges) | done |

A MAJOR redteam finding re-enters `hunt` on the same lane with the finding recorded as a
ledger note and pasted verbatim into the new hunt brief. Replace the lane when the finding
changed the unit's shape (a different name, a different file).

## Brief contract

Give each phase only what it cannot derive:

- the mission — one unit, one sentence, and the lane type;
- non-derivable context: the file's last word on the name (node names, not prose), the event
  time in UTC, the launch condition, settled decisions ("the ladder is disarmed at any take-up");
- ownership: the literal `dag/*.yaml` and `research/<NAME>/` paths the lane may create or edit
  (a `research/<NAME>/` directory is created if absent; one artifact per unit, named after the
  lane), and the sibling exclusions;
- reserved actions (never: WATCH-CALENDAR, BOOK-STATE, `out/final.html`, tool rosters unless
  assigned, push, PR, merge, any human channel, any order);
- the exit gate for this phase;
- literal report paths: `$RUN/<lane>.<phase>.notes.md`, `$RUN/<lane>.<phase>.done`,
  `$RUN/calendar/<lane>.yaml`, `$RUN/<lane>.republish.md`, `$RUN/<lane>.brief-defects.md`;
- pointers, not copies: `~/.claude/skills/research-swarm/references/praxis-protocol.md` and
  `opinions.md`, and the repo's `FRAMEWORK.md` rules by number.

Do not paste the bar's threshold into a `hunt` brief (12.51). Do not paste generic research
advice. When a checker refuses a commit, the lane establishes which side is wrong before
editing anything; a supported finding that "the fix belongs in a file I do not own" is a valid
phase outcome written to notes.

## Report paths and `.done` rules

- Notes are the lane's report; the orchestrator reads them, the pane is only a nudge.
- A phase that changes the branch commits BEFORE writing `.done`, and writes `.done` last.
- `.done` is a claim. The orchestrator verifies `git -C "$WORKTREE" log --oneline
  "$PHASE_START_SHA..HEAD"` and `git status --porcelain` before advancing.
- A lane blocked on a question it cannot answer from documents writes
  `BLOCKED: <what is needed>` as the last line of its notes and still writes `.done`.

## Lane-type templates

Fill every `<...>`. Ownership must be disjoint from every running lane.

### dated-event

- Mission: read `<filing/RNS/vote result>` for `<NAME>` that landed at `<UTC time>` and record
  it in `dag/<NAME>.yaml`. If nothing has landed at launch, record `NOTHING LANDED as at <UTC>`
  and stop; do not derive the pre-committed read a second time.
- Non-derivable context: the file's last word is `<node name(s)>`; the settled decision is
  `<e.g. ladder disarmed / read for the record only / a gate not a payout>`; what to record is
  `<the fields>`; what NOT to compute is `<e.g. a return>`.
- Ownership: `dag/<NAME>.yaml` (append one node) and `research/<NAME>/<lane>.md` if a document
  read needs an artifact.
- Exit gate (hunt): the node quotes the document by id and date, records the fields, states
  `what_this_changes`, and runs the six conditions ONLY if the last word says the event can
  produce a candidate.
- Paths: `$RUN/<lane>.hunt.notes.md`, `$RUN/<lane>.hunt.done`; calendar fragment if the
  document dates a next observable.

### channel-refresh

- Mission: re-run `<monitor and exact command with window>` against `<population>`; record the
  exhaustion date and any hit in `<channel file>`.
- Non-derivable context: last exhaustion date `<date>`; days marked PROVISIONAL `<dates>`;
  seed names that must appear or the run is broken `<names>` (12.46).
- Ownership: `dag/<CHANNEL>.yaml`, and `research/<TOPIC>-SWEEP-<DATE>.md` if the sweep is
  large enough to need an artifact.
- Exit gate: feed-health counts (ok / err / nodata) printed in the node; every hit opened, not
  counted; `exhausted_against_population_as_of:` updated; a hit that survives becomes a
  follow-up request in notes (the orchestrator opens a new lane; this lane does not size it).
- Paths: as above.

### new-ground

- Mission: open `<channel>` after `channelcheck` returned zero across `<three synonyms>`.
- Non-derivable context: the instrument route (`<feed/API>`), the coverage limit to state,
  the bench note that mentioned the idea (`<file:line>`).
- Ownership: create `dag/<CHANNEL>-CHANNEL.yaml` (name it so the synonyms find it; add a
  `STRUCTURAL_EXCLUSIONS_RECORDED_SO_CHANNELCHECK_FINDS_THEM` block), plus
  `research/<TOPIC>-SWEEP-<DATE>.md`.
- Exit gate: population enumerated with the route named; each name killed on a condition with
  its source; `WHAT_THIS_RUN_PRODUCED_HONESTLY`; `re_sweep_trigger`.

### correction

- Mission: run `<scanner list>` from the worktree root; READ every hit; correct any published
  number that is wrong where the reader arrives; grep the old number everywhere it lives.
- Non-derivable context: what the suite found last time and dismissed (`<node>`), so a
  known-benign hit is not re-litigated.
- Ownership BY EXCLUSION: any `dag/*.yaml` and `research/` file EXCEPT `<list of every file
  owned by a running lane>` and the orchestrator-only files. Data-row edits in
  `tools/limitprice.py` BOOK / `tools/edgarwatch.py` BOOK / `tools/briefcheck.py` BRIEF only
  when this brief assigns them; never logic.
- Exit gate: each hit classified `benign | corrected | belongs-in-<excluded file>`; every
  correction has the old and new figure, the source, and the list of surfaces grepped; brief
  defects go to `$RUN/<lane>.brief-defects.md`; a changed published figure goes to
  `$RUN/<lane>.republish.md`.

### deeper-research

- Mission: close the named gap `<question>` on `<NAME>` from `<the document that creates the
  fact>` (12.79), e.g. the outside date in the purchase agreement exhibit.
- Non-derivable context: the node that names the gap (`<node>`), what is already answered by
  siblings and artifacts (`before_working.py` output summarized as node names), the branch of
  the thesis it feeds.
- Ownership: `dag/<NAME>.yaml`, `research/<NAME>/<lane>.md`.
- Exit gate: the answer quoted verbatim with section number; its effect on each of the six
  conditions; `what_this_changes`; a new dated observable → calendar fragment.

### verifier

- Mission: adversarially test the PUBLISHED conclusion `<quote it>` on `<NAME>` from its own
  cited sources only (telemachus evaluate-artifact: artifact + sources, no journals).
- Non-derivable context: the cards and nodes that carry the conclusion, the accession numbers
  cited.
- Ownership: `dag/<NAME>.yaml` (append a `VERIFIER_<MMDD>` node) and
  `dag/VERIFIER-DISPOSITIONS.yaml` only if the brief assigns it.
- Exit gate: `CONFIRMED | REFUTED | UNVERIFIABLE-BY-CONSTRUCTION`, each finding naming the
  claim, the document that fails to support it or the absence, and what would falsify the
  finding itself; a refutation that changes a published figure → `$RUN/<lane>.republish.md`.

## The redteam checklist (fresh session; the whole brief)

Scope: `git diff "$FORK_SHA..HEAD" -- dag/ research/` and the documents it cites. Do not read
the hunt session's notes. Every finding quotes the line and names the document. Verdict is
MAJOR if any item 1–6 fails.

1. **Arithmetic seam** (FRAMEWORK §11): recompute every stated total from its displayed
   components; grep the expression for every named term (a defined probability, discount,
   share block or liability that does not appear in the arithmetic has been omitted);
   reconcile every count to the latest filing's own table; compute the CEILING before the
   central case (12.367 corollary) — a name can be right, cheap and structurally unable to
   clear 100%.
2. **Six conditions**, each with a verdict and the source line, or an explicit "not run because
   `<last word>`". Both halves of the bar stated (12.338).
3. **Three standing filters**: pre-funded-warrant direction test, burn vs runway, attribution
   including the contingent-liabilities note.
4. **Provenance**: was every return, NRV, count, date and price derived from a primary the lane
   read (accession / RNS id / exhibit name quoted)? A figure with no primary is MAJOR. A
   scout-sourced figure without its label is MAJOR (12.71).
5. **Supersession**: is every figure the node replaces marked, and is the old number gone from
   every surface the lane owns (12.2, 12.152)? Did the lane build on the file's LAST word
   (grep the file for a later node contradicting the one cited)?
6. **Price and date hygiene**: offer not mid (12.84); close not intraday print (12.256); the
   session state recorded; listing tagged (12.17); `found:` is today; no future `_MMDD`.
7. **Frame completeness** (12.405): for any constructed comparison, are "what leaves, what
   arrives, what is consumed" listed per branch with unpriced items named? A point estimate
   where a range was honest is MINOR unless it is published.
8. **The label matches the body** (12.338): a headline or status word the body does not
   support is MAJOR — only the headline survives into summaries.

Write `$RUN/<lane>.redteam.notes.md` with numbered findings, then the single line
`VERDICT: MAJOR|MINOR|CLEAR`, then `.done`.

## The record checklist (fresh session)

From the worktree root: `python3 tools/yamlcheck.py` · `python3 tools/futuredate.py` ·
`python3 tools/briefcheck.py` · `python3 tools/reconcile.py` · `python3 tools/limitprice.py`,
plus `python3 tools/currency.py <figure>` for every figure the lane changed that also lives in
the memo, and `python3 tools/xfile.py` / `python3 tools/dagstale.py` when the lane touched a
`px/anchor/ratio/discount/nav` value. Paste each tool's last lines into the record notes.
Then: confirm the node is the file's last word (`grep -nE '^[A-Z_0-9]+:' <file> | tail -1`);
apply MINOR redteam findings; write the calendar fragment / republish request / brief-defects
if owed; commit; `.done`.

## Integrate (orchestrator only)

Re-run the record checklist yourself in the lane worktree; read the diff. Then:

~~~bash
git -C "$WORKTREE" push -u origin "swarm/$ID"
gh pr create --repo <owner/research-repo> --base main --head "swarm/$ID" \
  --title "[<lane-type>] $ID: <one-line verdict>" --body-file "$RUN/$ID.pr.md"
~~~

`$RUN/$ID.pr.md` contains, in order:

1. **Verdict** — one paragraph: what landed or was found, what it changes (usually nothing),
   the honest-null statement if null ("A null on `<ground>`; feeds read successfully so the
   zero is real; exhausted as of `<date>`").
2. **Six conditions** — the table with per-row verdict and source, or the one line saying why
   they were not run.
3. **Redteam** — `VERDICT: ...` and the MINOR findings applied.
4. **Checkers** — last line of yamlcheck, futuredate, briefcheck, reconcile as run by the
   orchestrator.
5. **Artifacts** — literal paths: the dag node name, `research/...` files, `$RUN/<lane>.*.notes.md`.
6. **REPUBLISH REQUESTED** — only if `$RUN/<lane>.republish.md` exists: card, old figure, new
   figure, proposed label. The swarm does not republish.
7. **Calendar** — the fragment text if one was written (it is carried on the calendar PR, not
   this one).

Prefix the title with `[LIMIT TOUCHED]` when a close is at or below a limitprice limit.
Never merge; leave `merge_sha` empty; the ledger lane is `done` when the PR URL is recorded
as a note.

## Mechanics specific to research lanes

- Fork every lane from a commit containing the current base (`git fetch origin main` first).
  Record `FORK_SHA` at spawn; redteam and record are scoped to `FORK_SHA..HEAD`.
- Fresh sessions per phase, same worktree. `-t builder` for every phase (the type carries no
  research meaning); `-m` from the prompt's per-phase model, omitted for the default.
- The research repo's pre-commit hook lives in its common git dir and fires in every
  worktree; a lane must not disable it. If `yamlcheck` fails on a file the lane did not touch,
  that is a finding for the orchestrator, not a reason for `--no-verify`.
- Network-heavy phases (an FTS sweep, a universe pull) are serialized by the orchestrator:
  one at a time, recorded in OBJECTIVE's schedule table.
- A lane reads `/tmp/opencode/` before fetching, sets a descriptive `User-Agent` for SEC,
  and records the session state (`OPEN`/`CLOSED`) next to any price.
