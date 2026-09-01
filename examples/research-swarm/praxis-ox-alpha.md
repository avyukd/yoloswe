# Loaded example: `/research-swarm` against praxis-ox-alpha

Paste everything below the rule after `/research-swarm`. It was written on 2026-09-01 (evening
UTC) from `dag/WATCH-CALENDAR.yaml`, `dag/BOOK-STATE-0901.yaml` and the channel files of that
date. **Re-date-check every launch condition and every "file's last word" against the repo on the
day you paste it** — a node written after this example supersedes it.

---

Run a research swarm against `/home/ubuntu/dev/praxis-ox-alpha` (remote `origin` =
`avyukd/praxis-ox-alpha`, base branch `main`). Today is 2026-09-01. Mechanics per
`~/.claude/skills/research-swarm/SKILL.md`; the lane protocol is
`references/praxis-protocol.md`; the brief templates are `references/research-lane.md`.

## Goal

Work the window 2026-09-02 → 2026-09-12 as the standing run protocol prescribes: read every
dated event that lands and record it as the file's last word; keep the monitors exhausted to
their cadence; run the correction unit; prepare and then read the 10 September INVE vote. Find a
name that passes the six conditions if one exists; say so honestly if none does.

## Terminal proof (observables, each checked by a command on the lane branches / the calendar branch)

1. Every dated event in the first-wave table below that LANDED inside the window has a RECORD
   node (named in the table) in its owning dag file, on an open PR. An event that did not land
   has a `NOTHING LANDED as at <UTC>` node instead.
2. The channel registry in `$RUN/OBJECTIVE.md` shows an exhaustion date not older than its
   cadence for: dated-cash-return (`liqmon`/`salemon`, daily, last day provisional),
   `edgarwatch` roster (daily), US liquidation-basis FTS (weekly), Nordic MFN (weekly).
3. From the worktree root of every lane branch at PR time: `python3 tools/yamlcheck.py` →
   `CLEAN`; `python3 tools/futuredate.py` → `no NEW future-dated content`;
   `python3 tools/reconcile.py` → `0 figure(s) ... absent`; `python3 tools/briefcheck.py` →
   `CLEAN`, or every `PROBLEM` it prints is one of the two understood limitations recorded in
   `dag/BOOK-STATE-0901.yaml` `SCANNER_SWEEP_0901_PM` (GYRO `0.415x` segmenter false positive;
   INVE `0.79x` derived, not printed) and is listed as such in OBJECTIVE.
4. Every live-book name (GYRO, INVE, AIV, FDSB) has a next-information date in the calendar
   branch that is either in the future or has been read by a lane (`WATCH-CALENDAR.yaml`
   `WHEN_CAN_EACH_LIVE_NAME_NEXT_PRODUCE_INFORMATION_0901` is the seed).
5. No published figure changed without a `REPUBLISH REQUESTED` block in a PR body; the memo
   `out/final.html` is byte-identical to `main` on every lane branch.
6. Every US close after 20:05 UTC and every UK close after 16:35 UTC inside the window is
   recorded on the calendar branch with the session state the tool printed, and compared to
   `tools/limitprice.py`; a touch is a `[LIMIT TOUCHED]` PR.

"Twelve lanes done" is not the proof; the six items above are.

## Repo, concurrency, models

- Repo `/home/ubuntu/dev/praxis-ox-alpha`, base `main`, bramble repo name `praxis-ox-alpha`
  (open it in the running bramble first). Lane branches `swarm/<lane-id>`; orchestrator branch
  `swarm/calendar-20260902` for calendar fragments, price records and book-state findings.
- `$RUN` = `$HOME/.local/state/research-swarm/praxis-ox-alpha/<timestamp>` (outside the repo;
  praxis has no `.gitignore`).
- Concurrency: **3 lanes** in flight. One EDGAR full-text sweep in flight at a time.
- Models per phase: `hunt` = the bramble default model (omit `-m`); `redteam` = a FRESH session
  on the same worktree, `-m gpt-5.5` if a Codex backend is logged in (second-model independence,
  and then verify its notes file exists — Codex may ignore the reporting instruction), else the
  default model in a new session; `record` = default. `-t builder` for every phase.
- Ledger phases: `hunt:,redteam:,record:,integrate:`.

## Authorities

- Lanes may: read anything; fetch filings (SEC `User-Agent` `avyukd@gmail.com`; check
  `/tmp/opencode/` first); create/edit ONLY the files their brief names; commit on their own
  branch, early and often, with the repo's pre-commit hook active (never `--no-verify`).
- ONLY the orchestrator pushes branches and opens PRs into `avyukd/praxis-ox-alpha` (base
  `main`), one per lane plus one calendar PR, with the body contract in `research-lane.md`.
- NO merging by anyone in the swarm. NO republishing `out/final.html` by anyone in the swarm —
  a changed published conclusion is a `REPUBLISH REQUESTED` block for the human. NO edits to
  `dag/WATCH-CALENDAR.yaml`, `dag/BOOK-STATE-*.yaml`, `tools/limitprice.py` BOOK,
  `tools/edgarwatch.py` BOOK or `tools/briefcheck.py` BRIEF by lanes; the orchestrator carries
  fragments to the calendar branch, and roster/brief changes are flagged for the human, not made.
- NO trading, no orders, no sizing, no limit changes. NO Slack or any other human channel: the PR
  title/body and `$RUN/OBJECTIVE.md` are the only notifications.

## Human gates

1. Merge of every PR (the swarm leaves `merge_sha` empty).
2. Any republish of the memo.
3. Any change to the standing brief text or a tool roster — flagged in `$RUN/<lane>.brief-defects.md`
   and the PR body, never applied.
4. `[LIMIT TOUCHED]`: open the PR at `p0`, put it at the top of OBJECTIVE, keep dated-event lanes
   running, dispatch no new-ground lanes until the human comments on that PR.

## Schedule (orchestrator; materialize lanes only where a row says so)

| when (UTC) | what | command / rule |
|---|---|---|
| every 20 min | health tick | `/loop` per SKILL §4 |
| daily 07:30 Mon–Fri | UK landed-document probe | `python3 tools/uk_trust.py ASLI INOV APTD NESF NCC` — materialize a `dated-event` lane when the awaited document appears (ASLI 30-Jun NAV; INOV Half-year Report; APTD interims) |
| daily 12:00 Mon–Fri | US roster filings | `python3 tools/edgarwatch.py --since <yesterday> --wide` — a material filing on INVE/GYRO/AIV/FDSB/GTIM/ICMB materializes a `p1` dated-event lane; a feed error is an error, not a zero |
| daily after 16:35 | UK close record | `python3 tools/ukpx.py RSE ASLI HWG INOV SEIT APTD NESF FGEN JZCP NCC` → `$RUN/prices/<date>-uk.txt` and a `PRICES_<MMDD>_UK_CLOSE` node on the calendar branch; compare RSE/NESF/FGEN/JZCP to `tools/limitprice.py` limits (the tool knows LSE hours and 2026–27 bank holidays; record its session line) |
| daily after 20:05 | US close record | `python3 tools/uspx.py AIV INVE GYRO FDSB GTIM ICMB` → `$RUN/prices/<date>-us.txt` and `PRICES_<MMDD>_US_CLOSE`; compare to limitprice; GYRO ≤ $4.78 or AIV ≤ $1.67 is `[LIMIT TOUCHED]` (both are good-branch limits; say which branch in the PR) |
| Mon 08:00 | Nordic MFN pull | materialize `nordic-mfn-<MMDD>` (channel-refresh) |
| Mon 13:00 | US liquidation-basis FTS re-sweep | materialize `nail-fts-resweep-<MMDD>` (channel-refresh); serialize behind any other FTS lane |
| daily after 13:00 | dated-cash-return monitor | materialize `liqmon-salemon-<MMDD>` only if the previous day's exhaustion is still PROVISIONAL or a day is uncovered; otherwise the orchestrator runs the three commands read-only and updates the registry |

## First wave (lane id · type · priority · launch condition · ownership · mission · exit gate)

Node names below carry the `_MMDD` suffix of the day the node is expected to be written; a lane
uses the actual UTC date it commits on (`tools/futuredate.py` flags anything later than today).

Launch NOW (fills the three slots):

1. **`scanner-suite-0901`** · correction · p1 · now · owns by EXCLUSION (any `dag/*.yaml` and
   `research/` except `INVE.yaml`, `EUROPE-LIQUIDATION-SCOUT.yaml`, and the orchestrator-only
   files; no tool edits). Mission: run from the worktree root `python3 tools/dagstale.py`,
   `tools/xfile.py`, `tools/dupenode.py`, `tools/futuredate.py`, `stale_figure_scan.py`,
   `unverified_claims_scan.py`, `band_consistency_scan.py` (the last three live at the repo
   root), `tools/memocheck.py`, `tools/dagcheck.py`, `tools/omitcheck.py`, `tools/briefcheck.py`,
   `tools/claimprice.py`, `tools/reconcile.py`, then `tools/currency.py` over every figure in
   `limitprice.BOOK` with the snippets READ, then `tools/knownyet.py` / `tools/already.py` on
   anything it proposes as new.
   Non-derivable context: the suite ran at 0901 PM (`BOOK-STATE-0901` `SCANNER_SWEEP_0901_PM`)
   and found no wrong published number; the known-benign hits are listed there and are not
   re-litigated. The parts that did NOT run and are this lane's unit: `currency.py` over all
   nineteen limitprice figures with snippets read (the tool's labels are prompts, 12.407 as
   amended), `xfile.py` after the day's eight republishes, and the roster audit — does
   `tools/edgarwatch.py` BOOK cover every name `limitprice.BOOK` prices (it does not price
   RSE/NESF/FGEN/JZCP, which are UK — say so) and every name the calendar dates (ICMB was
   missing until 0901). Exit gate: every hit classified benign / corrected / belongs-in-excluded;
   corrections written where the reader arrives with the old number grepped across `dag/`,
   `research/`, `out/final.html` (report only), the brief and the tool constants (report only);
   `$RUN/scanner-suite-0901.brief-defects.md` if the brief is wrong. Record: a
   `SCANNER_SUITE_SWARM_<MMDD>` node in each file that received a correction; if no published
   number was wrong there is no dag node — the null goes to the notes and the PR body
   (`BOOK-STATE-*.yaml` is orchestrator-only).

2. **`inve-vote-prep-0901`** · deeper-research · p1 · now · owns `dag/INVE.yaml`,
   `research/INVE/`. The file's last word is
   `THE_LEAKS_FALL_ENTIRELY_ON_THE_DEAL_SIDE_AND_THEY_REVERSE_THE_CONCLUSION_0901` (deal $3.807
   vs no-deal $3.438 after the $9.8m transaction costs; break-even Series C 76.8% of mark;
   **tax leak still unquantified**); do not re-derive that schedule. Mission, two parts:
   (a) quantify the tax leak from the DEFM14A tax section and the 10-K NOL/DTA note (12.79: the
   document that creates the fact), publishing a RANGE with its omissions named (12.405), and
   restate the schedule row `less_tax` with a number or a bounded range; (b) write the
   pre-committed read of the Item 5.07 8-K: fields to record (for / against / abstain / broker
   non-votes; the majority-of-OUTSTANDING threshold with abstentions and non-votes as AGAINST;
   the turnout-independent figure 5,622,257), and what each branch changes (PASS → post-deal
   cash per diluted share IS the price and the 9.4%/yr melt continues in a shell; FAIL → the
   no-deal branch at $3.438 and the receding NRV/2 ceiling). No sizing before 10 September.
   Exit gate: `INVE_TAX_LEAK_QUANTIFIED_<MMDD>` and `INVE_5_07_PRE_COMMITTED_READ_<MMDD>` are
   the file's last words; six conditions restated on both branches; calendar fragment for
   2026-09-10 18:00 UTC.

3. **`nordic-mfn-0901`** · channel-refresh · p2 · now · owns `dag/EUROPE-LIQUIDATION-SCOUT.yaml`.
   Mission: the first weekly MFN keyword pull — `https://mfn.se/all/s/nordic.json?compact=true&limit=100&query=<kw>`
   (the HTML form `/all/s?query=` 301-redirects and silently drops the query — found by the
   first lane on 2026-09-01) for
   `likvidation`, `utskiftning`, `avvikling`, `"return of capital"` over the last seven days;
   open each hit's release (MFN shows titles only); kill on the class (a saneeraus / creditor
   process is not a solvent wind-down). Seed: the Dovre Group 1-Sep release must appear or the
   feed is broken (12.46). Exit gate: node `MFN_WEEKLY_PULL_<MMDD>` with the query set, hits
   opened, kills with reasons, `exhausted_against_population_as_of: <date>`, and the cadence.

Scheduled (dispatch when the launch condition has passed and a slot is free; priorities decide
order when several are due):

4. **`ncc-takeup-0902`** · dated-event · p1 · ≥ 2026-09-02 07:30 UTC · owns `dag/NCC.yaml`.
   Last word: `MY_PRE_COMMITMENT_WAS_ALREADY_REFUTED_BY_THIS_FILE_0901` and `STAND_DOWN_0830`:
   the 119/107/97p ladder is DISARMED at any take-up because the price test (>132p) is breached
   at 145p; a buyback at the market price is value-neutral to the stub. Mission: READ FOR THE
   RECORD ONLY the "Results of Tender Offer" RNS (`python3 tools/uk_trust.py NCC`): take-up % of
   the 117,241,379-share capacity, shares accepted, post-settlement count, the embedded stub
   price at the tape, and the GBP13–18m guidance band if restated. Do NOT compute a return. Do
   NOT arm anything. Reopen is a price event (< 132p whole-co on the tape), handled by the UK
   close record, not by this lane. Exit gate: `NCC_TAKE_UP_RNS_RECORDED_0902`,
   `what_this_changes: NOTHING`.

5. **`liqmon-salemon-0902`** · channel-refresh · p1 · ≥ 2026-09-02 13:00 UTC · creates and owns
   `dag/DATED-CASH-RETURN-MONITOR.yaml` (the running log for `liqmon` / `salemon` / `edgarwatch`
   refreshes; seed its first node from `BOOK-STATE-0901` `CHANNEL_REFRESH_0901_PM` and cite it).
   Mission: `python3 tools/liqmon.py 2026-09-01 2026-09-02`;
   `python3 tools/salemon.py 2026-09-01 2026-09-02 0.5`;
   `python3 tools/edgarwatch.py --since 2026-09-01 --wide`. 2026-09-01 was marked PROVISIONAL
   for FTS lag; this run makes it firm and marks 2026-09-02 provisional. Feeds must answer
   (`ok`/`err` counts in the node); every hit opened; a hit that survives the SPAC/boilerplate
   filters is a follow-up request in notes, not sized here. Exit gate:
   `DATED_CASH_RETURN_REFRESH_0902` with exhaustion dates.

6. **`spire-rule26-0903`** · dated-event · p2 · ≥ 2026-09-03 16:30 UTC · owns
   `dag/UK-OFFER-PERIOD-REGISTER.yaml`. Toscafund's Rule 2.6 deadline is 17:00 London 3-Sep
   (register row `Spire_Healthcare`, in period since 18-Sep-2025). Read for the record only:
   firm offer / walk / extension. Not the class (operating company on a takeover spread; a
   spread cannot clear 100% absolute). Log the outcome for or against the register-pattern
   inference (PE name → Bidco, deadline "to be determined" = a firm offer being papered). If no
   RNS by 18:00 UTC, record `NOTHING LANDED` and stop. Exit gate: `SPIRE_RULE_2_6_OUTCOME_0903`.

7. **`genel-rule26-0904`** · dated-event · p2 · ≥ 2026-09-04 16:30 UTC · depends on
   `spire-rule26-0903` (same file) · owns `dag/UK-OFFER-PERIOD-REGISTER.yaml`. DNO Iraq's Rule
   2.6 deadline 17:00 London 4-Sep (register row `Genel_Energy`, in period since 7-Aug-2026;
   71.4p at 66% of range). Same shape as Spire. Exit gate: `GENEL_RULE_2_6_OUTCOME_0904`.

8. **`nesf-vote-0904`** · dated-event · p1 · ≥ 2026-09-04 16:30 UTC, or when
   `uk_trust.py NESF` shows the results RNS, whichever is later · owns
   `dag/CONTINUATION-VOTE-CHANNEL.yaml`. Last word: WATCH-CALENDAR
   `PRE_COMMITTED_READS_2_TO_4_SEP_0901_PM` — a GATE, not a payout; board recommends AGAINST;
   base rate is that it fails. FAIL → one line, nothing changes. PASS → one node with the vote
   count and the board's next-steps wording; NESF becomes a UK wind-down candidate WITHOUT a
   timetable; at 50p vs 73.2p NAV it is 0.683x, fails the 0.5x entry; the absolute-half ceiling
   is 36.6p and c2 is NOT_COMPUTABLE until a realisation timetable is published. A limit at
   36.6p is written ONLY if a timetable inside 1.71 years appears. No purchase follows from the
   vote in either branch. Exit gate: `NESF_DISCONTINUATION_VOTE_0904` with the six conditions
   run on PASS only.

9. **`nail-fts-resweep-0908`** · channel-refresh · p2 · ≥ 2026-09-08 13:00 UTC (weekly) ·
   owns `dag/US-LIQUIDATION-CHANNEL.yaml`. Last word:
   `THE_PROPERLY_AIMED_SWEEP_39_ISSUERS_AND_A_TIMING_PROBLEM_0901` (statement-title phrases,
   not the XBRL tag; 39 issuers walked; the tradability gate first; FREVS switches basis on its
   next 10-Q). Mission: the four phrases over 10-Q/10-K for filings dated 2026-09-01 → run date
   (late Q2 10-Qs and NT filers); seed GYRO and AIV must appear over a wider window or the run
   is broken; new issuers only — the 39 are excluded by CIK; tradability and basis verified
   before any number. Exit gate: `NAIL_FTS_RESWEEP_<MMDD>` with population, exclusions, kills,
   the FREVS status, and the exhaustion date.

10. **`gyro-cmt-0908`** · dated-event · p1 · ≥ 2026-09-08 21:00 UTC · owns `dag/GYRO.yaml`,
    `research/GYRO/`. Last word on the maturity: `THE_10_OCTOBER_MATURITY_PRICED_0831` (the
    borrower's option; extension rate resets 3.75% → ~7.23% off the 5-yr UST; debt service
    $275,091 → $390,302; both covenant tests uncomputable from filings) and the 0901 nodes
    ending `THE_COMPLETION_DATE_HAS_BEEN_PULLED_IN_ZERO_TIMES_IN_NINE_YEARS_0901`. Mission:
    record the weekly-average 5-yr CMT for the week ending 4-Sep AS THE LOAN AMENDMENT DEFINES
    IT (read the definition quoted in the 0831 node; do not assume the H.15 series), the
    resulting extension rate and annual debt service against $875,362 of NOI, and
    `edgarwatch --cik 1589061 --since 2026-09-01` for any 8-K on the maturity. No verdict change
    unless an 8-K lands. Exit gate: `GYRO_EXTENSION_RATE_FIXED_<MMDD>`; a calendar fragment for
    2026-10-10.

11. **`jzcp-agm-0908`** · dated-event · p2 · ≥ 2026-09-08 14:00 UTC (AGM 13:30 BST) · owns
    `dag/JZCP.L.yaml`. Last word: `THE_AGM_NOTICE_READ_AND_IT_SETTLES_IT_0831` — there is NO
    continuation or discontinuation resolution (BOOK-STATE-0901's "whether a continuation
    resolution exists at all" is superseded by it; do not re-ask). Mission: read the AGM results
    RNS; record whether resolutions 9 (market buyback, 14.99%) and 10 (CFC off-market) passed
    and any wind-down timetable language in the chairman's statement or "other business".
    `FINAL_VERDICT_KILLED_ON_THE_CLOCK` stands unless a DATE appears. Exit gate:
    `JZCP_AGM_RESULT_0908`.

12. **`inve-vote-0910`** · dated-event · p0 · ≥ 2026-09-10 21:00 UTC AND only after
    `edgarwatch --cik 1036044 --since 2026-09-10` shows the Item 5.07 8-K (poll at the 12:00
    UTC schedule daily until 2026-09-16) · depends on `inve-vote-prep-0901` · owns
    `dag/INVE.yaml`, `research/INVE/`. Mission: the read pre-committed by
    `INVE_5_07_PRE_COMMITTED_READ_<MMDD>`, field by field; six conditions on the branch that
    obtained; a `REPUBLISH REQUESTED` file is expected on either branch (the largest binary in
    the live book changes a published conclusion). `redteam` is mandatory before the PR;
    the redteam brief carries the prep node by name. Exit gate: `INVE_VOTE_RESULT_0911`.

## Forward calendar (materialize when due; from `dag/WATCH-CALENDAR.yaml`)

| date (London/NY local as recorded) | name | event | owner file |
|---|---|---|---|
| 2026-09-02 | ASLI | last cum-entitlement day for the 6.6p B share — a diary note, NOT a lane (reclassified as failing the absolute half) | — |
| early Sep, undated | ASLI | 30-Jun-2026 NAV announcement (investegate filters NAV RNS — use `uk_trust.py` on the LSE feed) | `dag/ASLI.yaml` |
| early Sep, undated | APTD | interim results; Formal Sale Process timetable | `dag/APTD.L.yaml` |
| early Sep, undated | INOV | Half-year Report — gates the 16-Sep Final Tender Price Determination (slipped twice; last year's landed 18-Sep) | `dag/INOV.L.yaml` |
| 2026-09-14 | ELME | Riverside ($250.0m) contracted to close no later than this date; delisting trigger is Riverside + term-loan repayment | `dag/ELME.yaml` |
| 2026-09-16 / 09-18 / 09-23 13:00 London | INOV | price determination / price & entitlement announced / election deadline — ACTIVE ELECTION REQUIRED | `dag/INOV.L.yaml` |
| 2026-09-17 | FGEN | continuation vote (board in favour; 14% discount — narrow) | `dag/CONTINUATION-VOTE-CHANNEL.yaml` |
| 2026-09-18 | TRP (ASX) | general meeting — delisting (75%) and buy-back both must pass | the ASX file the register names |
| 2026-09-29 | PIA (ASX), FREVS, GTIM | buy-back outcome; special meeting on the Plan of Voluntary Liquidation (majority of ALL shares); covenant quarter-end (measured later) | `dag/FREVS.yaml`, `dag/GTIM.yaml` |
| 2026-10-10 | GYRO | 2021 Mortgage Loan initial maturity, $4,507,473 | `dag/GYRO.yaml` |
| 2026-10-21 | RML (TSXV) | Third Circuit oral argument on the PDVH sale appeals; OFAC licence notice undated | `dag/AWARD-VEHICLE-CHANNEL.yaml` |
| early Nov | AIV, FDSB, GYRO | Q3 10-Qs — AIV's is the single most important scheduled document in the book | per name |

## Channel registry seed (copy into OBJECTIVE and keep current)

| channel | exhausted as of | population | re-sweep trigger | null streak | last lane |
|---|---|---|---|---|---|
| dated-cash-return (`liqmon`/`salemon`) | 2026-08-31 firm, 09-01 provisional | EDGAR 8-K + proxies, six phrasings | daily | 0 | BOOK-STATE-0901 |
| `edgarwatch` roster | 2026-09-01 19:45 UTC | INVE GYRO AIV FDSB GTIM ICMB | daily | 0 | BOOK-STATE-0901 |
| US liquidation-basis FTS | 2026-09-01 | 39 issuers (statement titles) | weekly; FREVS basis switch | 0 | US-LIQUIDATION-CHANNEL |
| Nordic MFN | scouted 2026-09-01, not swept | not enumerable; feed works | weekly pull | 0 | EUROPE-LIQUIDATION-SCOUT |
| Canadian wind-downs | 2026-09-01 | 6 names, 5 dead, FCA killed | a new SEDAR+ wind-up release or FCA.U ≤ US$2.66 | 1 | CA-WINDDOWN-CHANNEL |
| BDC wind-downs | 2026-09-01 | 4 listed, ICMB event watch | ICMB Item 1.01 8-K | 1 | BDC-WINDDOWN-CHANNEL |
| award vehicles | 2026-09-01 | GRZ, RML | OFAC docket notice; Third Circuit 21-Oct | 1 | AWARD-VEHICLE-CHANNEL |
| US Ch11 equity | 2026-09-01 | 9 debtors, all cancel equity | an 8-K giving old equity a recovery; a UST equity-committee notice | 1 | US-CH11-EQUITY-CHANNEL |
| continuation votes | NESF 4-Sep, FGEN 17-Sep pending | — | the votes | 0 | CONTINUATION-VOTE-CHANNEL |

The four `1`s are nulls on DIFFERENT grounds and do not stack; the three-null rule fires only on
the same ground.

## Environment notes for lane briefs

SEC `User-Agent` `avyukd@gmail.com`; already-fetched filings under `/tmp/opencode/`;
`query1.finance.yahoo.com` answers 429 (use `tools/uspx.py`); LSE quotes via
`tools/ukpx.py` / `tools/uk_trust.py`; MFN for Nordics; EQS not URL-searchable; EDGAR FTS
`forms=` under-returns and XML 13D/G cover pages are not indexed (walk the submissions JSON);
the pre-commit hook runs `tools/yamlcheck.py` in every worktree.
