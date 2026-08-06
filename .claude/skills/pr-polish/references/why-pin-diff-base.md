# Why `--diff-base` is pinned (and why the fallback must be loud)

Without `--diff-base` the reviewer infers the range, and the natural inference —
`git diff main...HEAD` — is wrong whenever the local base branch has drifted. A
stale `main` narrows the diff so the reviewer never sees code the PR touched; an
advanced `main` widens it with unrelated commits. Measured on a replayed PR:
**336 files reviewed instead of 22**, and the guess varied run to run.

The pin is best-effort. A detached HEAD or a missing `origin/<base>` leaves
`DIFF_BASE` empty and every reviewer falls back to inferred scope. That fallback
has to be loud for two reasons:

1. Silently omitting the flag restores the exact failure mode the pin exists to
   prevent.
2. An unpinned round is otherwise indistinguishable from a pinned one in the
   log, so its findings get compared against pinned rounds as if the ranges
   matched.

Do **not** hard-fail instead. Branch contexts (`pr_number: null`,
`branch:<name>`) legitimately run without a resolvable base, and aborting there
would break the loop for a case that still reviews usefully.

The warning block writes no state, so the orchestrator must add the `ack` /
`sweep` entry to the round's actions file itself — nothing else records which
rounds were scoped by inference.
