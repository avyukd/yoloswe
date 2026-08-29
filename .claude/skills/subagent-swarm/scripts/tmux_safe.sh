#!/bin/bash
# Kill a tmux window only when it is provably not you.
#
# 2026-08-27T09:09:15Z: an orchestrator reaped "dead" windows with
#   tmux list-windows -F '#{window_index} #{window_name}' | grep ' !'
# believing ' !' matched tmux's dead/bell FLAG. That format string emits no flags
# field at all, so it matched the NAME -- and bramble prefixes '!' to the name of a
# window that WANTS ATTENTION. Every match was a healthy running session; every
# genuinely dead window was missed. killed=18, sessions running 2-9.5h, itself
# among them, 336 minutes lost, a human needed to restart it.
#
# Two tmux behaviours make a naive guard fail OPEN. Both measured:
#   display-message -p -t '@99999'  ->  ""     rc=0   (a bad target is not an error)
#   display-message -p -t ''        ->  "@86"  rc=0   (an EMPTY -t means the ACTIVE window)
# So an unset $TMUX_PANE does not fail -- it answers with somebody else's window,
# and a guard comparing against that protects the wrong one while killing the real
# self. Hence: validate $TMUX_PANE BEFORE interpolating it, test existence with
# list-windows (not display-message), and fail closed on any doubt.
set -u

# Every kill and every refusal is appended here, so an incident is reconstructable
# instead of being inferred from session-store timestamps after the fact.
: "${SW_KILL_LOG:=${RUN:-}/kill-log.tsv}"

sw_log() {  # sw_log <action> <target> <label> <detail>
  case "$SW_KILL_LOG" in ''|/kill-log.tsv) return 0 ;; esac
  [ -d "$(dirname "$SW_KILL_LOG")" ] || return 0
  printf '%s\t%s\t%s\t%s\t%s\t%s\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" "$2" "${3:-}" "${4:-}" "${SELF:-}" \
    >> "$SW_KILL_LOG" 2>/dev/null || true
}

# resolve_self: print THIS shell's own tmux window id (@N), or fail non-zero.
resolve_self() {
  [ -n "${TMUX:-}" ] || { echo "tmux_safe: not inside tmux" >&2; return 1; }
  # Checked before interpolation: an empty -t silently means "the active window".
  case "${TMUX_PANE:-}" in
    %[0-9]*) ;;
    *) echo "tmux_safe: \$TMUX_PANE absent/malformed ('${TMUX_PANE:-}')" >&2; return 1 ;;
  esac
  local w
  w=$(tmux display-message -p -t "$TMUX_PANE" '#{window_id}' 2>/dev/null)
  case "$w" in
    @[0-9]*) printf '%s\n' "$w" ;;
    *) echo "tmux_safe: could not resolve self window from $TMUX_PANE" >&2; return 1 ;;
  esac
}

# window_for_session <bramble-session-id> -- exact, live, name-independent.
# bramble injects BRAMBLE_SESSION_ID into each window (tmux_runner.go newWindowArgs
# uses `new-window -e`) and the pane pid IS the CLI process. This is the ONLY lookup
# that works for a LIVE session: bramble writes ~/.bramble/sessions/** on COMPLETION,
# so a running lane has no record there (measured: 25 live sessions, 0 on-disk rows),
# and `bramble list-sessions` omits the window id entirely.
window_for_session() {
  local want="${1:?usage: window_for_session <session-id>}" wid pid sid
  while read -r wid pid; do
    [ -n "$pid" ] || continue
    sid=$(tr '\0' '\n' < "/proc/$pid/environ" 2>/dev/null | sed -n 's/^BRAMBLE_SESSION_ID=//p')
    [ "$sid" = "$want" ] && { printf '%s\n' "$wid"; return 0; }
  done < <(tmux list-panes -a -F '#{window_id} #{pane_pid}' 2>/dev/null)
  return 1
}

# safe_kill_window <@window-id> [label]
#   rc 0 killed | 3 malformed target | 4 self unresolvable | 5 target IS self | 6 no such window
safe_kill_window() {
  local target="${1:-}" label="${2:-}" self
  case "$target" in
    @[0-9]*) ;;
    *) echo "REFUSE: '$target' is not a @N window id (never a name, index or flag)" >&2
       sw_log REFUSE-malformed "$target" "$label" ""; return 3 ;;
  esac
  self=$(resolve_self) || {
    echo "REFUSE: cannot identify self, so cannot prove $target is not me" >&2
    sw_log REFUSE-no-self "$target" "$label" ""; return 4
  }
  if [ "$target" = "$self" ]; then
    echo "REFUSE: $target IS THIS SESSION ($label)" >&2
    sw_log REFUSE-self "$target" "$label" "self=$self"; return 5
  fi
  tmux list-windows -a -F '#{window_id}' 2>/dev/null | grep -qx -- "$target" || {
    echo "skip: $target no longer exists" >&2
    sw_log skip-absent "$target" "$label" ""; return 6
  }
  local path
  path=$(tmux list-panes -t "$target" -F '#{pane_current_path}' 2>/dev/null | head -1)
  tmux kill-window -t "$target" && {
    echo "killed $target ${label:+($label)}"
    sw_log killed "$target" "$label" "path=$path self=$self"
  }
}
