# prdozer

Watch GitHub pull requests and keep them merge-ready by invoking the
`/pr-polish` skill in response to base moves, CI failures, or new review
comments.

Same shape as [`jiradozer`](../jiradozer/), but PR-driven instead of
issue-driven. The Go side is orchestration only; the actual fixing is
delegated to the `/pr-polish` skill.

For design details, motivation, and trade-offs, see [`docs/design.md`](docs/design.md).

## What it does

For every PR you point it at, prdozer ticks at a configured interval. On
each tick it snapshots the PR via `gh` and decides whether anything changed:

| Trigger        | Detection                                                  | Action                |
|----------------|------------------------------------------------------------|-----------------------|
| Base moved     | `origin/<base>` advanced past last seen SHA                | invoke `/pr-polish`   |
| CI failed      | `statusCheckRollup == FAILURE` or new failed run on head   | invoke `/pr-polish`   |
| New comments   | inline/issue comment from non-self author since last tick  | invoke `/pr-polish`   |
| Mergeable      | approved + green CI + base unchanged                       | idle (or auto-merge)  |
| PR closed      | state != `OPEN`                                            | record + stop         |

When work is needed, prdozer hands the PR to `/pr-polish` via the agent CLI
wrapper. The skill performs rebase/conflict resolution, CI/bot waiting, and
code fixes; prdozer just orchestrates.

## Install / build

```
bazel build //prdozer/cmd/prdozer:prdozer
```

The binary lands at `bazel-bin/prdozer/cmd/prdozer/prdozer_/prdozer`.
Either run it from there or copy to `$PATH`.

## Usage

```
prdozer --pr 1234                  # watch one PR
prdozer --pr 1234,1235,1240        # watch a list
prdozer --all                      # watch all open PRs matching source.filter
prdozer --once                     # one tick per PR, then exit (good for cron)
prdozer --dry-run                  # detect changes and log decisions, don't invoke the agent
prdozer --local                    # pass --local to /pr-polish (skip CI/bot wait)
prdozer --auto-merge               # merge PRs that become mergeable
prdozer --poll-interval 5m         # override config poll interval
prdozer --config prdozer.yaml -v   # explicit config + verbose output
```

`--once` is the friendly mode for cron / systemd timers; without it prdozer
runs as a long-lived poller.

### Typical workflows

**One-shot dry run on a single PR (verify what would happen):**

```
prdozer --pr 1234 --once --dry-run -v
```

**Long-running watcher in tmux:**

```
prdozer --all --poll-interval 30m -v
```

**Cron-driven (every 30m via systemd timer or crontab):**

```
prdozer --all --once
```

**Faster local iteration (skip CI wait):**

```
prdozer --pr 1234 --once --local -v
```

## Configuration

A config file is optional — without one, prdozer uses built-in defaults and
only the CLI flags control behavior. See [`prdozer.example.yaml`](prdozer.example.yaml)
for every option.

Quick reference for the defining choices:

| Field                            | Default       | Notes                                        |
|----------------------------------|---------------|----------------------------------------------|
| `poll_interval`                  | `30m`         | Re-check cadence per PR                      |
| `source.mode`                    | `all`         | `single` / `list` / `all`                    |
| `source.filter.author`           | `@me`         | gh `--author` filter                         |
| `source.filter.exclude_labels`   | `[wip, do-not-watch]` | Skip PRs carrying any of these       |
| `source.max_concurrent`          | `3`           | Max parallel polish sessions                 |
| `polish.local`                   | `false`       | Pass `--local` to `/pr-polish`               |
| `polish.auto_merge`              | `false`       | Merge PRs that become mergeable              |
| `polish.max_turns`               | `100`         | Turn cap per polish session                  |
| `polish.max_budget_usd`          | `20.0`        | Per-session budget                           |
| `backoff.max_consecutive_failures` | `3`         | Trip cooldown after this many failures       |
| `backoff.cooldown`               | `2h`          | Skip ticks for the cooled-down PR            |

CLI precedence: flags beat config beats defaults.

## State

Per PR, kept under
`~/.bramble/projects/<repo>-<pr>/prdozer-state.json`:

- `last_check_at`, `last_seen_head_sha`, `last_seen_base_sha`
- `last_seen_comment_ids`, `last_seen_ci_run_ids` (dedup sets)
- `last_action`: `idle` / `polished` / `merged` / `failed` / `dry_run`
- `consecutive_failures`, `cooldown_until` (back-off control)

Coexists with `pr-polish-state.json` written by the skill itself — they're
separate files in the same project directory.

## Logging

Mirrors `jiradozer`:

- **Human-facing status** lines go to stderr via `render.Renderer`.
  `--verbosity quiet|normal|verbose|debug`, `--color auto|always|never`,
  `-v` for verbose shorthand.
- **Structured logs** go to `~/.prdozer/logs/prdozer-<ts>-<pid>.log` at
  debug level; `slog` warnings/errors also surface on stderr.
- **Agent stream output** flows through `render.Renderer` live — the same
  way you see `/pr-polish` running interactively.

The active log path is printed to stderr at startup:

```
[Status] Logging to /home/you/.prdozer/logs/prdozer-20260419-053353-1618187.log
```

## Failure handling & back-off

If `/pr-polish` returns an error, the watcher records the failure and tries
again on the next tick. After `backoff.max_consecutive_failures` consecutive
failures (default 3), that PR enters a `backoff.cooldown` window (default
2h) during which it is skipped. Other PRs are unaffected. A successful
polish or idle tick clears the failure count.

## Testing

```
bazel test //prdozer/...        # unit tests
scripts/lint.sh                 # workspace lint (run before pushing)
```

Unit tests use a `fakeGH` (prefix-matched stdout map) and a `stubPolish`
recorder — no real `gh` or Claude CLI invocations. See
[`docs/design.md`](docs/design.md#testing-strategy) for the testing strategy.

## Babysitting a PR to merge

`prdozer babysit` drives a single PR all the way to merge on whichever
devbox is healthiest, rather than requiring you to already be sitting in
the right worktree.

```
prdozer babysit --pr sycamore-labs/kernel#8123      # pick a box and dispatch
prdozer babysit --pr <ref> --dry-run                # score table + exact ssh command
prdozer babysit --pr <ref> --here                   # run in-process, no dispatch
prdozer fleet status                                # probe table for every box
prdozer runs                                        # runs recorded on this box
prdozer gc --dry-run                                # preview what GC would reap
```

What a run does: pick a host → prepare a private worktree at
`<worktree_root>/.babysit/<pr>-<runid>` → loop polish / CI-green / merge,
routing failed merges through the repo's `merge_rework` rounds → notify on
terminal state → GC the worktree. Logs land in
`~/.prdozer/runs/<repo>-<pr>-<runid>/` and **outlive** the worktree.

Per-repo behaviour comes from `~/magent/prdozer/registry.yaml`: worktree
root, layout, flow, merge policy, and the rework rounds.

### Merge policy

`merge_policy` decides how (and whether) a PR lands. It defaults to
`notify` everywhere — opting a repo into real merging is a deliberate,
watched step.

| Policy | Behaviour |
|---|---|
| `notify` | Never merges. Reports and stops. **Default.** |
| `queue` | `gh pr merge --auto`, no strategy flag — the merge queue owns the strategy. |
| `squash` | `gh pr merge --squash`. Only for repos without a merge queue. |
| `rebase` | `gh pr merge --rebase`, for repos requiring linear history. |

Two rules are enforced in code and must not be relaxed: **never pass
`--delete-branch`** (it has closed PRs *unmerged* when the merge command
failed), and **never pass a strategy flag on a queue repo** (it is
rejected outright). Every merge is verified with
`gh api repos/<o>/<r>/pulls/<N> --jq .merged` — the only unambiguous
signal, since `gh pr merge` exiting 0 does not mean the PR merged.

### Safety properties

- **One babysitter per PR**, enforced by an flock held inside the worker
  process for its whole lifetime.
- **GC never discards unpushed work.** It pushes first, re-checks, and
  keeps the worktree (saying so in the notification) if still unclean.
- **Unbounded merge retries stay visible.** Rework counts as a failure for
  backoff, so repeated failure trips the 2h cooldown and Slacks a warning.

## Limitations

- **Fork PRs are unsupported.** Cross-repository PRs fail with a clear
  message rather than guessing at remote setup.
- **gh auth.** Requires `gh auth login` to be set up; checked at startup.
- **Post-merge delivery is out of scope.** Terminal state is "merged";
  docker-publish → Deploy to Dev → prod stays with the kernel-local
  `sy:post-approve-babysit` skill.

## See also

- [`docs/design.md`](docs/design.md) — full design doc with diagrams and
  rationale.
- [`prdozer.example.yaml`](prdozer.example.yaml) — annotated config reference.
- [`../jiradozer/`](../jiradozer/) — sibling tool, same shape, issue-driven.
- `/pr-polish` skill — the agent prompt that does the actual fixing.
