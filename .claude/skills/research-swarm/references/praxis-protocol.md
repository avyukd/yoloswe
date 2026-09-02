# The standing run protocol

This is the brief every research lane embodies, kept faithful to the operator's own text, plus
the mechanical gate commands that make it checkable. Repo paths are relative to the research
repo root (`/home/ubuntu/dev/praxis-ox-alpha` today); every `tools/*.py` reads `dag/`,
`research/`, `out/final.html` and `tools/limitprice.py` by relative path, so **run them from the
worktree root or they read nothing**.

## THE BAR

A candidate must offer **>=100% absolute return AND 50–100% IRR — both halves** — and pass six
conditions:

| # | condition | why (FRAMEWORK) | how a lane tests it |
|---|---|---|---|
| 1 | entry <= 0.5x NRV | the absolute half; `limit(T) <= NRV/2` at every horizon, so a name above NRV/2 fails at ANY date (12.392) | price at the OFFER divided by NRV from the primary; `tools/limitprice.py` prints the limit per name |
| 2 | completion <= 1.71 years | beyond `ln2/ln1.5 = 1.7095y` the IRR half binds and decays `1/1.5^T` (12.60, limitprice docstring) | the issuer's own stated completion date, quoted verbatim; no date → NOT_COMPUTABLE, never invented |
| 3 | NRV stable or rising | a 0.5x entry against a melting mark is a loss, not a margin (12.404) | six periods of net assets from XBRL companyfacts or the statements; state the trend as a number |
| 4 | NRV net of its own costs | a going-concern book value overstates what is receivable (12.404, BOOK-STATE-0901 basis note) | ASC 205-30 / liquidation NAV is already net; a book value is not — deduct burn to the payout date or say there is no date to deduct to |
| 5 | the instrument is buyable | liquidating-trust units are usually non-transferable; the tag screen finds the unbuyable half (12.402) | ticker, exchange, a live quote with volume, and transferability from the governing document |
| 6 | asset-priced, not yield-priced | income buyers pay premiums to asset value (royalty trusts at 3.3–3.7x PV-10) | what does the marginal holder own it for: the assets, or the distribution? |

A status line naming only the half you passed reads as a pass (12.338). Write both halves,
every time: `absolute: +19.7% FAIL · irr: 51–85% PASS → FAIL`.

The bar is applied by the orchestrator and the redteam to the lane's honest valuation. **A hunt
brief never states the threshold to a finder** (12.51: when the threshold was stated it was met
every time, by construction; when removed the same universe priced at fair value).

## FIRST, EVERY LANE, IN ORDER

1. **`python3 tools/channelcheck.py "<what you're about to screen>"`** — exit 1 means prior work
   exists; the tool prints the files and their VERDICT lines. Read them. Run it across at least
   three synonyms (hyphen/space variants are folded; different words are not). A lane that opens
   new ground must show three zeros in its notes. Exit 0 prints
   `NOT PREVIOUSLY SWEPT (by this term). Still check a synonym.`
2. **Read the file's LAST word on a name, not its most findable one.** Operationally:
   `tail -n 120 dag/<NAME>.yaml`; `grep -nE '^[A-Z_0-9]+:' dag/<NAME>.yaml | tail -5` and read
   those nodes in full; `grep -n -iE 'STAND_DOWN|WITHDRAWN|SUPERSEDED|REFUTED|KILL|do not re' dag/<NAME>.yaml`.
   The 2026-09-01 NCC failure is the model: a pre-commitment was written on an 8/27 node while
   `STAND_DOWN_0830` sat further down the same file. Then run
   `python3 before_working.py <TICKER> [keyword]` (repo root) for artifacts, node statuses and
   `[open*]` (answered-but-watching) markers, and `python3 tools/knownyet.py <figure>` /
   `python3 tools/already.py <figure> ...` before claiming anything is new (exit 1 = already
   published).
   **Date-check the node you build on:** its `found:` date, its `_MMDD` suffix, and whether a
   later node supersedes it. Today's date is the date on your clock, not the date in the brief.
3. **`python3 tools/limitprice.py`, `python3 tools/reconcile.py`, `python3 tools/currency.py <figure>`
   before quoting any figure.** limitprice is the authority table for entry, NRV, horizon and
   limit per name (pass a date `YYYY-MM-DD` as argv[1] to reproduce a past run); reconcile
   proves every limitprice figure is PRESENT in the memo (`0 figure(s) ... absent` is clean);
   currency reports CURRENT / SUPERSEDED per occurrence and its reliable signal is *no current
   occurrence* — read the snippets, the labels are prompts.

## THEN ONE UNIT per lane, in this preference order

1. **A DATED EVENT that has actually landed.** Check filings first: `python3 tools/edgarwatch.py
   --since <date> [--wide] [--cik N]` for US names (a feed error is an ERROR, never a zero);
   `python3 tools/uk_trust.py <TIDM>` for the RNS table of an LSE name. If nothing landed, the
   node says `NOTHING LANDED as at <UTC timestamp>` and the lane's unit is over — do not
   re-derive the pre-committed read.
2. **A CHANNEL REFRESH.** Channels exhaust against a population; record
   `exhausted_against_population_as_of: <date>` and the `re_sweep_trigger:` in the channel
   file. The monitors: `python3 tools/liqmon.py <start> <end>` (announcement moment,
   8-K/proxies), `python3 tools/salemon.py <start> <end> [min_ratio]` (asset-sale precursor),
   `python3 tools/edgarwatch.py --since <date>` (book roster). EDGAR FTS indexes same-day
   filings with a lag: mark the last day PROVISIONAL and re-run it next day.
3. **NEW GROUND** — zero across >=3 `channelcheck` synonyms; the channel file is created with
   `channel:`, `status:`, `built:`, `exhausted_against_population_as_of:`, `re_sweep_trigger:`,
   `coverage_limit:` and a `WHAT_THIS_RUN_PRODUCED_HONESTLY:` block, as in
   `dag/CA-WINDDOWN-CHANNEL.yaml`.
4. **A CORRECTION to a published number** — highest value, always allowed, outranks a new
   name. Scanner suite (every hit is READ, never counted): `dagstale.py`, `xfile.py`,
   `dupenode.py`, `futuredate.py`, `stale_figure_scan.py` (root), `unverified_claims_scan.py`
   (root), `band_consistency_scan.py` (root), `memocheck.py`, `dagcheck.py`, `omitcheck.py`,
   `briefcheck.py`, `claimprice.py`, `reconcile.py`, `currency.py` over every figure in
   `limitprice.BOOK`. A correction is written where the reader ARRIVES (a banner at the TOP of
   an artifact, immediately after the source line; a new node at the END of a dag file with a
   one-line `SUPERSEDED BY <node>` pointer next to the old one), and the old number is grepped
   across `dag/`, `research/`, `out/final.html`, the standing brief and the tool constants
   (12.2, 12.23 addendum, 12.152).

## STANDING FILTERS before believing any per-share gap

- **Pre-funded warrants.** `WeightedAverageNumberOfSharesOutstandingBasic /
  EntityCommonStockSharesOutstanding` from companyfacts; **> 1.25 is a red flag** (12.61). It is
  a DIRECTION test, not a ratio test (12.123): read the cover, the EPS note and the equity
  note for pre-funded warrants and convertible preferred, and take the diluted count from
  the issuer's own anti-dilutive table (12.42).
- **Burn vs runway.** Screen the burn in the same pass as the cash (12.62). A net-cash discount
  is real only with low burn OR a committed return mechanism (adopted plan, declared
  distribution with a record date, compulsory redemption). Check the recent filing list for an
  S-4 or a 425, which refutes a cash-return thesis in one call (12.77).
- **ATTRIBUTION — where the cash actually goes.** `per-share cash = cash x (fraction still
  attributable to today's holders) / (true share count)`; every net-cash idea in the book
  failed one of the three (12.67). Read the contingent-liabilities note, not only the cash
  flows (12.228). For a buyback: has it EXECUTED (Part II Item 2, discount RSU withholding
  rows), who does the shrinking denominator benefit, has any standstill / pill / DGCL 203 been
  amended, is there litigation in the DEFA14A supplements (12.70). For a wind-down: who holds
  the senior claim and the fee (ICMB: the adviser's affiliate holds the notes and has no reason
  to hurry).

## RECORD

- The record is `dag/*.yaml`. The files must parse (`python3 tools/yamlcheck.py`, enforced by
  the repo's pre-commit hook, which lane worktrees share) AND the tools read them by regex, so
  keep the anchors intact: `found: YYYY-MM-DD` indented under the node, `status:` words,
  `budget: { total: N, spent: M }`, node names `UPPER_SNAKE_MMDD:` at column 0. Do not restructure
  or "fix" existing nodes; append.
- A swarm node, minimal shape (prose fields are `>` folded scalars; keep `:` inside quotes):

  ~~~yaml
  NCC_TAKE_UP_RNS_RECORDED_0902:
    found: 2026-09-02
    lane: ncc-takeup-0902            # swarm lane id, so the PR and the node cross-reference
    status: RECORD_ONLY               # RECORD_ONLY | NULL | KILL | EVENT_WATCH | CORRECTION | CANDIDATE
    sources:
      rns: "NCC Group plc, 'Results of Tender Offer', RNS 2026-09-02 07:00 London, <url>"
    THE_FACTS: >
      take-up 61.3% of capacity; 71,870,000 shares accepted; post-settlement count ...
    THE_SIX_CONDITIONS: "not run -- the file's last word (STAND_DOWN_0830) disarms the ladder at any take-up; price test breached at 145p"
    what_this_changes: "NOTHING. No limit, no verdict, no published figure."
    memo: "NOT REPUBLISHED"
  ~~~

  A null node states the ground, the instrument, the window, the population enumerated, the
  seed check that proves the zero is real (12.46: a feed that answered), and the exhaustion date.
- `python3 tools/futuredate.py` must be clean: a node dated later than today, or a `_MMDD`
  suffix in the future, is a defect (12.339; the baseline silences the known-benign cohort).
- Commit on the lane branch: `git commit -m "<lane-id> <phase>: <what changed, one line>"` with
  the `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>` trailer. Never `--no-verify`.
  Never `git add -A` from anywhere but the worktree root, and never add swarm state.
- **Republish the memo ONLY if a published conclusion changed** — and in the swarm, republishing
  is orchestrator/human-only. A lane whose finding changes a published conclusion writes
  `$RUN/<lane>.republish.md` (which card, the old figure, the new figure, the label to use) and
  the orchestrator puts a `REPUBLISH REQUESTED` block in the PR body. "Nothing published
  changed" is written in the node (`memo: "NOT REPUBLISHED -- ..."`).
- **A dated observable found** (a deadline, a docket date, a record date, a determination date)
  is written as a calendar fragment at `$RUN/calendar/<lane>.yaml` in the
  `dag/WATCH-CALENDAR.yaml` `DATED_TRIGGERS` shape (`date`, `name`, `event`, `why_it_matters`,
  `what_to_read`); the orchestrator appends it to the calendar branch.

## HONEST NULLS

A run that finds nothing must say so, in the node and in the PR body. Never state a return
figure not derived from a primary filing the lane actually read; a scout-sourced figure carries
its provenance label (12.71) and never enters a verdict. Three consecutive nulls on the same
ground → the orchestrator marks the channel exhausted (date) and switches ground; nulls on
different grounds do not stack (see `dag/BDC-WINDDOWN-CHANNEL.yaml`'s closing paragraph).
"Waiting for the dated events" is a valid posture and is reported as such
(`BOOK-STATE-0901` §`A_NULL_RUN_AND_A_STATEMENT_ABOUT_DIMINISHING_RETURNS`).

## Price discipline (before any ratio is written)

- **Buy at the OFFER, not the mid** (12.84; RSE's 3% spread moved the answer across the bar).
- **A close is a settled fact; a quote is a moment** (12.256). `python3 tools/uspx.py AIV INVE
  GYRO FDSB GTIM` labels its output `INTRADAY PRINT -- NOT A CLOSE` inside 13:30–20:00 UTC on
  weekdays and errs toward OPEN; run it after 20:00 UTC for closes. `python3 tools/ukpx.py RSE
  ASLI HWG INOV SEIT APTD` knows LSE hours and England & Wales bank holidays for 2026–2027 and
  returns UNKNOWN for an unloaded year; run it after 16:35 UTC. Record the session state the
  tool printed next to the price.
- A fallback that matches anything cannot report absence (12.343): a scraper's keyed selector
  returning empty IS the result. HTTP 429 is UNRESOLVED, not unchanged (12.46).
- Tag every date and price to a listing (12.17): "the BMV ex-date is X; the ADS ex-date is Y".

## Environment facts a lane needs

- SEC EDGAR needs a descriptive `User-Agent` (the repo uses `avyukd@gmail.com`); EDGAR FTS
  `forms=` silently under-returns — query without it and post-filter on `root_forms` (a list;
  the value reads `SCHEDULE 13D`, not `SC 13D`). FTS does not index modern XML 13D/G cover
  pages, only their exhibits; walk `data.sec.gov/submissions/CIK##########.json` per name.
- `api.nasdaq.com/api/screener/stocks` returns the whole US universe in one request and is the
  only free US source that survived rate limits; `query1.finance.yahoo.com` answers 429;
  stooq is behind a JS challenge; stockanalysis is client-rendered.
- LSE: `api.londonstockexchange.com/api/gw/lse/instruments/alldata/<TIDM>` for quotes,
  `investegate.co.uk/company/<TIDM>` for ~55 server-rendered RNS headlines (its advanced
  search is broken). Nordics: `https://mfn.se/all/s/nordic.json?compact=true&limit=100&query=<kw>`
  — a JSON Feed with server-side full-text search over release bodies (keywords: likvidation,
  utskiftning, avvikling; "return of capital" is token-matched, not phrase-matched, and ~40% of
  those requests answer HTTP 500 — retry). The HTML form `https://mfn.se/all/s?query=<kw>`
  301-redirects to `/all/s/nordic` and SILENTLY DROPS THE QUERY, returning the unfiltered feed;
  found by the first Nordic lane on 2026-09-01. Germany/DACH: EQS IS searchable server-side at
  `https://www.eqs-news.com/search-results/?searchtype=news&searchword=<kw>` (25 a page,
  `/search-results/page/N/?...` to page; body-matched, token-matched) — `tools/eqsnews.py`;
  the earlier "EQS is not URL-searchable" note was the wrong URL. Market-wide title pulls:
  `tools/tdnet.py` (Japan, by date), `tools/ukrns.py` (UK, by date), `tools/asxanns.py` (ASX,
  two-day window, run daily), `tools/nzxanns.py` (NZX, latest 200, run daily),
  `tools/euronextnews.py` (Paris/Amsterdam/Brussels/Lisbon/Oslo/Dublin — NOT Milan or Athens —
  via `live.euronext.com/en/listview/company-press-release/all?combine=<word>&page=N`,
  server-rendered, 50 a page; the title filter matches EACH WORD separately, so pass single
  words only; `--read <nid>` works for releases carried as PDF attachments, but nodes that
  redirect to `/products/equities/company-news/<slug>` hit an AWS WAF challenge, HTTP 202).
  `tools/jsesens.py` (JSE via Sharenet, anonymous two-day window only), `tools/emarketnews.py`
  (Borsa Italiana via emarketstorage.it: `titolo=` is a stemmed title filter, `mercato=` honoured,
  `data_from` IGNORED so the tool filters dates itself, `cerca=` is body full-text).
  `tools/hkexnews.py` (HKEXnews titleSearchServlet: needs the search-page cookie first, else every
  query is recordCnt 0; pull a WHOLE DAY with empty title and t1code=-2, filter locally on LONG_TEXT --
  the servlet's own title filter is unusable). `tools/dailypulls.sh` runs all of them.
  ASX: `asx.api.markitdigital.com/asx-research/1.0/companies/<ticker>/announcements`.
- Already-fetched filings live under `/tmp/opencode/` (does not survive a reboot); check before
  re-pulling.
- One EDGAR FTS sweep in flight at a time across the swarm; the orchestrator serializes them.
