#!/bin/bash
# Regression test for tmux_safe.sh. Runs against a PRIVATE tmux server on its own
# socket, so it can never reach a real session. Reproduces the 2026-08-27 incident
# as an assertion: the historical sweep must be shown selecting a healthy window.
set -u
SW="$(cd "$(dirname "$0")" && pwd)"
SOCK="/tmp/swarmguard-test-$$"
T="tmux -S $SOCK"
PASS=0; FAIL=0
ok(){ PASS=$((PASS+1)); echo "  ok   $1"; }
no(){ FAIL=$((FAIL+1)); echo "  FAIL $1"; }
chk(){ [ "$2" = "$3" ] && ok "$1 ($2)" || no "$1: expected $3, got $2"; }
alive(){ $T list-windows -a -F '#{window_id}' 2>/dev/null | grep -qx -- "$1"; }

cleanup(){ $T kill-server 2>/dev/null; rm -f "$SOCK" "$LOG" 2>/dev/null; }
trap cleanup EXIT

$T new-session -d -s t -n self-window 'sleep 300' 2>/dev/null
$T new-window  -t t -n '!kernel/decoy-bang:0' 'sleep 300'
$T new-window  -t t -n 'kernel/decoy-plain:0' 'sleep 300'
SELFWIN=$($T list-windows -a -F '#{window_id} #{window_name}' | awk '$2=="self-window"{print $1}')
BANG=$(   $T list-windows -a -F '#{window_id} #{window_name}' | awk '$2 ~ /decoy-bang/{print $1}')
PLAIN=$(  $T list-windows -a -F '#{window_id} #{window_name}' | awk '$2 ~ /decoy-plain/{print $1}')
SELFPANE=$($T list-panes -a -F '#{window_id} #{pane_id}' | awk -v w="$SELFWIN" '$1==w{print $2}')

LOG=$(mktemp); export SW_KILL_LOG="$LOG"
# Point the guard at the private server and pretend the self-window is ours.
tmux(){ command tmux -S "$SOCK" "$@"; }; export -f tmux 2>/dev/null || true
export TMUX="$SOCK,0,0" TMUX_PANE="$SELFPANE"
. "$SW/tmux_safe.sh"

echo "== the 2026-08-27 sweep, reproduced =="
HIT=$($T list-windows -a -F '#{window_index} #{window_name}' | grep ' !' | wc -l)
chk "historical sweep selects the HEALTHY bang-named window" "$HIT" "1"
FLAGS=$($T list-windows -a -F '#{window_flags}' | tr -d '\n')
case "$FLAGS" in *'!'*) no "window_flags contains ! (would justify the filter)";;
                    *) ok "window_flags never contains ! (flags=[$FLAGS]) - filter matched NAME";; esac

echo "== the guard =="
safe_kill_window "$SELFWIN" self >/dev/null 2>&1; chk "self refused" "$?" "5"
alive "$SELFWIN" && ok "self SURVIVED" || no "self was KILLED"

safe_kill_window "1" idx >/dev/null 2>&1;  chk "index refused"  "$?" "3"
safe_kill_window "" empty >/dev/null 2>&1; chk "empty refused"  "$?" "3"
safe_kill_window "kernel/decoy-plain:0" name >/dev/null 2>&1; chk "name refused" "$?" "3"

( unset TMUX_PANE; safe_kill_window "$PLAIN" noself >/dev/null 2>&1; exit $? )
chk "fail-closed when TMUX_PANE unset" "$?" "4"
alive "$PLAIN" && ok "target survived fail-closed refusal" || no "target killed with no self"

safe_kill_window "@99999" ghost >/dev/null 2>&1; chk "absent window" "$?" "6"

safe_kill_window "$PLAIN" plain >/dev/null 2>&1; chk "genuine other killed" "$?" "0"
alive "$PLAIN" && no "target still alive" || ok "target is gone"
alive "$SELFWIN" && ok "self still alive after a real kill" || no "self died"

echo "== audit log =="
grep -q 'REFUSE-self' "$LOG" && ok "refusal logged" || no "refusal not logged"
grep -q 'killed' "$LOG"      && ok "kill logged"    || no "kill not logged"

echo; echo "PASS=$PASS FAIL=$FAIL"; [ "$FAIL" -eq 0 ]
