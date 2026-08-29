# Bramble mechanics

These are the swarm-specific mechanics whose failure is silent or destructive. Keep run
policy in `SKILL.md` and lane semantics in `pr-lane.md`.

## Preflight and run location

Run before the first spawn:

~~~bash
SELF=$(ps -o args= -p "$(ps -o ppid= -p $$)" | sed -n "s/.*session-id '\([^']*\)'.*/\1/p")
export BRAMBLE_SOCK="${XDG_RUNTIME_DIR:-/tmp}/bramble-$(id -u).sock"
SW=~/.claude/skills/subagent-swarm/scripts
test -S "$BRAMBLE_SOCK"
. "$SW/tmux_safe.sh"
resolve_self
bramble ping
bramble new-session --help | rg -- '--parent'
bramble list-sessions | rg -- "$SELF"
~~~

`BRAMBLE_SESSION_ID` is normally empty inside a session. Pass `--parent "$SELF"` on
every spawn; otherwise completed lanes report nowhere. If the client accepts `--parent`
but new sessions still arrive orphaned, the running TUI is stale. Restarting it kills live
sessions, so that decision belongs to the user.

Resolve the run against the current orchestrator branch:

~~~bash
TARGET=$(git rev-parse --abbrev-ref HEAD)
BASE=$(git symbolic-ref --quiet refs/remotes/origin/HEAD | sed 's#refs/remotes/origin/##')
BASE=${BASE:-main}
git fetch origin "$BASE"
SHARED=$(realpath -m "$(git rev-parse --path-format=absolute --git-common-dir)/../.shared")
RUN="$SHARED/subagent-swarm/$(date +%Y%m%d-%H%M%S)"
~~~

Require a clean orchestrator worktree. Every lane must fork from a commit containing the
current `TARGET`. Bramble's `-f` resolves against the remote; when `TARGET` has local
commits, create the worktree from local `TARGET` and spawn with `-w` instead.

## Spawn and record

For a remote-backed first phase:

~~~bash
bramble new-session -r "$REPO" --create-worktree -b "$BRANCH" -f "$BASE" \
  --parent "$SELF" -t "$TYPE" -m "$MODEL" -g "$ID" -p "$BRIEF"
~~~

Always pass `-r`; repository inference can choose the TUI's unrelated repository.
Dependent or locally based lanes use an explicit worktree:

~~~bash
git worktree add -b "$BRANCH" "$WORKTREE" "$TARGET"
bramble new-session -w "$WORKTREE" --parent "$SELF" \
  -t "$TYPE" -m "$MODEL" -p "$BRIEF"
~~~

Record the literal phase, session id, `realpath` worktree, and fork SHA immediately:

~~~bash
python3 "$SW/ledger.py" set "$RUN" --id "$ID" --status running \
  --phase "$PHASE" --session "$SESSION" --worktree "$(realpath "$WORKTREE")"
~~~

Briefs must contain literal report paths; child environments do not point back to the run.
Confirm the wave with `bramble list-sessions --parent "$SELF"` and inspect fresh panes.
Codex commonly stops on the directory-trust dialog; submit its displayed trust choice
and then submit the composer before treating the lane as live.

## Track and communicate

`<lane>.<phase>.done` and idle notifications are claims. Before transitioning:

~~~bash
git -C "$WORKTREE" log --oneline "$PHASE_START_SHA..HEAD"
git -C "$WORKTREE" status --porcelain
~~~

For a phase expected to edit, no commit means it is not complete. For a read-only phase,
verify its specified artifact or review gate. Recheck HEAD immediately before integration;
cleanup and review sessions can amend after an early idle signal.

For a live deploy or measurement, verify the authoritative release revision advanced and
the intended change is present on the running target. A successful deploy command,
readiness, or a downstream endpoint can all describe the previous revision.

Run `snapshot_at_risk.sh "$RUN"` every tick. It backs up lanes with uncommitted work to
`refs/backup/<lane>` without changing their index or HEAD. Never use an empty branch or
an idle report as evidence that no backup is needed.

Use the run directory for reports. Queue live-session nudges, then inspect the pane:
Codex can fire idle mid-turn and Cursor can leave pasted instructions unsubmitted.
`poll_panes.sh "$RUN"` detects questions, trust prompts, and stacked pastes. Do not resend
while an earlier paste is still visible. Confirm a nudge started work via the pane timer or
moving git state; replace a wedged session on the same worktree rather than debugging its
composer indefinitely.

## Watch and reap

Use one watcher. Reap and arm it in separate calls so the kill pattern cannot match the
new watcher's own argv:

~~~bash
pkill -f '[w]atch_lanes[.]sh'
pgrep -f '[w]atch_lanes[.]sh' | wc -l
"$SW/watch_lanes.sh" "$RUN" &
~~~

The count must be zero before arming. The watcher wakes on a new `.done` or static lane,
not on every commit.

Kill a lane session before removing its worktree. Resolve the tmux window from the session
id and use the fail-closed helper:

~~~bash
. "$SW/tmux_safe.sh"
safe_kill_window "$(window_for_session "$SESSION")" "$ID"
~~~

Never kill by window name, index, or worktree path. Before deleting a branch or worktree,
verify the authorized integration artifact directly; ancestry alone is insufficient after
squash or rewritten history. Then remove the worktree, branch, and `refs/backup/<lane>`.
`audit_cleanup.sh "$RUN"` reports leaked sessions, panes, worktrees, branches, and refs.

## Recover

The loop reminder carries the only required coordinates:

~~~bash
cat "$RUN/OBJECTIVE.md"
python3 "$SW/ledger.py" show "$RUN"
ls "$RUN"/*.done 2>/dev/null
bramble list-sessions --parent "$SELF"
cat ~/.bramble/deliveries/"$SELF".json 2>/dev/null
~~~

A running ledger lane with no live session either finished without reporting or died.
Inspect its branch, worktree, pane history, and report files before deciding which.
Select live evidence by identity and recency, never by list position; record which instance
and timestamp produced any result used to advance a lane.
