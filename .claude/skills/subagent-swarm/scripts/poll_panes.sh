#!/bin/bash
# Report in-flight lanes whose pane is genuinely BLOCKED — sitting on a question
# nobody is there to answer. That is the most common stall there is: the lane is
# not "running" in any useful sense, emits no report, and its branch does not
# move, so every other check shows nothing wrong because nothing has happened.
#
#   poll_panes.sh <run-dir>
#   ROUNDS=1 INTERVAL=1 poll_panes.sh <run-dir>     # single sweep
#
# Detection rules, each of which cost a false alarm to learn:
#   - A busy pane always renders an ELAPSED TIMER. Key on that, never on the verb:
#     backends animate Working/Brewed/Improvising/Processing/Channelling/Grepping
#     and every other word list, so verb-matching reports busy lanes as blocked.
#   - Read tail -20, not tail -8: a running-subagent list ("◯ reuse-review  7m 56s")
#     pushes the busy indicator well above the footer.
#   - A session waiting on its OWN subagents is BUSY. Never interrupt it — that
#     kills the delegated work.
#   - Only the last few lines can be a live question. Question-shaped text further
#     up is usually a finished session's summary of what it skipped.
set -u

RUN="${1:?usage: poll_panes.sh <run-dir>}"
HERE="$(cd "$(dirname "$0")" && pwd)"
ROUNDS="${ROUNDS:-6}"
INTERVAL="${INTERVAL:-600}"

BUSY='\([0-9]+[ms]( [0-9]+s)? ·|for [0-9]+s|esc to interrupt|↓ [0-9.]+k tokens|^[[:space:]]*◯ |[0-9]+m [0-9]+s$'

for _ in $(seq 1 "$ROUNDS"); do
  need=""
  while IFS=$'\t' read -r id _phase _branch wt wid; do
    wt="${wt%/}"
    [ -d "$wt" ] || continue
    for p in $(tmux list-panes -a -F '#{pane_id} #{pane_current_path}' 2>/dev/null \
               | awk -v w="$wt" 'index($2, w) == 1 {print $1}'); do
      txt=$(tmux capture-pane -p -t "$p" 2>/dev/null | grep -v '^[[:space:]]*$')
      [ -z "$txt" ] && continue
      echo "$txt" | tail -20 | grep -qE "$BUSY" && continue
      q=$(echo "$txt" | tail -4 | grep -E '\?[[:space:]]*$|Should I|Would you like|proceed anyway|Do you trust' | tail -1)
      pend=$(echo "$txt" | grep -cE '\[Pasted text' )
      unsent=""
      # Cursor accepts a paste and ignores --submit. Count before sending anything:
      # resending stacks copies, and one lane was found holding 465 of them.
      [ "$pend" -gt 0 ] && unsent=" | ${pend} UNSENT PASTE(S) — do not resend"
      need="$need\n  [$id $p] ${q:-idle, no busy marker}${unsent}"
    done
  done < <(/usr/bin/env python3 "$HERE/ledger.py" lanes "$RUN" --need-worktree 2>/dev/null)

  if [ -n "$need" ]; then
    echo -e "BLOCKED:$need"
    echo
    echo "One paste in a composer -> tmux send-keys -t <pane> Enter."
    echo "A trust-directory dialog -> tmux send-keys -t <pane> '1' then Enter."
    echo "Many stacked pastes -> STOP sending; write HANDOVER-<task>.md instead."
    exit 0
  fi
  sleep "$INTERVAL"
done

echo "NO BLOCKED PANES after ${ROUNDS} round(s)"
