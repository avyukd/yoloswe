#!/bin/bash
# Back up uncommitted work in in-flight lanes without touching the lane's index
# or HEAD — an agent mid-turn must not find its git state changed underneath it.
#
#   snapshot_at_risk.sh <run-dir>
#
# Writes a real commit via plumbing at refs/backup/<task>: keeps authorship and
# parentage, recoverable with ordinary git, survives a session death.
#
# The brief asks lanes to commit early, which is not protection: a queued message
# only lands when the lane's turn ENDS, and the risk window is the middle of a
# long turn. Lanes have reached 298k tokens with 92 uncommitted files while a
# polite request sat in the queue. When a control depends on the other party
# acting, add one that does not.
#
# Guard on "has uncommitted work", never "has no commits" — the first version
# skipped any lane with >=1 commit, switching itself off the moment the
# commit-early pressure started working.
#
# Release the ref (git update-ref -d refs/backup/<task>) once the lane is
# committed AND clean, or merged — see audit_cleanup.sh.
set -u

RUN="${1:?usage: snapshot_at_risk.sh <run-dir>}"
HERE="$(cd "$(dirname "$0")" && pwd)"

while IFS=$'\t' read -r id _phase _branch wt wid; do
  [ -d "$wt" ] || continue
  # NOTE: --porcelain collapses an untracked DIRECTORY to one line, so this count
  # is a presence check only, never a file count. Count via the index below.
  [ -z "$(git -C "$wt" status --porcelain 2>/dev/null)" ] && continue

  (
    cd "$wt" || exit 0
    IDX="$(mktemp -u /tmp/swarm_bkidx_XXXXXX)"
    GIT_INDEX_FILE="$IDX" git read-tree HEAD 2>/dev/null
    GIT_INDEX_FILE="$IDX" git add -A 2>/dev/null
    tree=$(GIT_INDEX_FILE="$IDX" git write-tree 2>/dev/null)
    files=$(GIT_INDEX_FILE="$IDX" git diff --cached --name-only HEAD 2>/dev/null | wc -l)
    rm -f "$IDX"
    [ -z "$tree" ] && exit 0

    prev=$(git rev-parse -q --verify "refs/backup/$id" 2>/dev/null)
    if [ -n "$prev" ] && [ "$(git rev-parse "$prev^{tree}" 2>/dev/null)" = "$tree" ]; then
      exit 0    # nothing new since the last snapshot
    fi

    c=$(git commit-tree "$tree" -p HEAD -m "backup($id): orchestrator snapshot of uncommitted work

Written by the swarm orchestrator via plumbing while the session was mid-turn.
The session's own index and HEAD were NOT touched." 2>/dev/null)
    [ -z "$c" ] && exit 0
    git update-ref "refs/backup/$id" "$c"
    echo "BACKUP $id -> refs/backup/$id ($files files)"
  )
done < <(/usr/bin/env python3 "$HERE/ledger.py" lanes "$RUN" --need-worktree 2>/dev/null)
