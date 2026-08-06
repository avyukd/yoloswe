# Known Blind Spots

Patterns that review backends consistently miss. Check for **regressions** every round — if a backend starts catching a previously-blind pattern, that's progress worth noting.

| Pattern | Missed By | Example | First Seen |
|---------|-----------|---------|------------|
| **Registry duplicated across skills** — a value is enumerated in two places and the change updates only one. The stale copy fails *silently* because the consumer treats an unknown entry as "didn't run". | cursor, codex, claude-sonnet | PR #307 added `"claude"` to `bramble_ops.BACKENDS` but not `harvest_lib.BACKENDS`, so `/pr-polish --claude` rounds vanish from the code-review-replay dataset. Caught only by claude-opus. | 2026-08-06 |
| **Option-vs-doc mismatch** — a doc comment claims an isolation/safety property that the code never configures, because the SDK splits it across two independent options. | codex, claude-opus, claude-sonnet | PR #307's package doc claimed reviews inherit neither settings nor plugins, but `baseSessionOptions` never called `WithDisablePlugins()`; `--setting-sources ""` and `--plugin-dir` are separate (process.go:73 vs :137). Caught only by cursor, and only on turn 3. | 2026-08-06 |
| **Two enumerations of the same concept disagreeing** — same domain (mutating tools), different lists, no shared source. | cursor, codex, claude-sonnet | `claudeReadOnlyDisallowedTools` vs `isMutatingTool` disagree on `MultiEdit`. Caught only by claude-opus. | 2026-08-06 |

## Adding New Entries

When you identify a blind spot during an eval run:
1. Add a row with the pattern, which backends missed it, a concrete example, and the eval date
2. In subsequent runs, check if the pattern is still missed
3. If a backend starts catching it, update the row and note when it was resolved
