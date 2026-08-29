#!/usr/bin/env python3
"""Durable lane ledger for subagent-swarm runs.

A LANE is one unit of work moving through an ordered list of PHASES. A phase is
one type of session that does the work. The lane's `status` is where it is in its
life; its `phase` is which phase-session is current. Those are different axes and
are stored separately.

State lives in <run>/state.json; <run>/ledger.md is re-rendered after every write
so the markdown is always current without anyone hand-editing tables.

    ledger.py init  <run> --goal G --phases "implement:opus,cleanup:gpt-5.6-luna"
                          [--base B --target B]
    ledger.py add   <run> --id ID --title T --branch B [--priority p0|p1|p2]
                          [--depends-on "a,b"] [--brief ...]
    ledger.py set   <run> --id ID [--status S] [--phase P] [--session S]
                          [--priority p0|p1|p2] [--worktree P] [--window-id W]
                          [--merge-sha SHA] [--note N]
    ledger.py advance <run> --id ID  # -> next phase, or status=done when exhausted
    ledger.py show  <run>            # print ledger.md
    ledger.py ready <run>            # dependency-ready ids, highest priority first
    ledger.py inflight <run>         # count of lanes with status running
    ledger.py lanes <run> [--status running] [--phase a,b] [--need-worktree]
                          [--config KEY]

`--session` records the session id for the lane's CURRENT phase.
"""

import argparse
import json
import os
import sys

STATUSES = ["planned", "running", "done", "blocked", "failed"]
PRIORITIES = ["p0", "p1", "p2"]
PRIORITY_RANK = {p: i for i, p in enumerate(PRIORITIES)}
MARK = {"planned": "·", "running": "▶", "done": "✓", "blocked": "⏸", "failed": "✗"}


def paths(run):
    return os.path.join(run, "state.json"), os.path.join(run, "ledger.md")


def load(run):
    state_path, _ = paths(run)
    if not os.path.exists(state_path):
        sys.exit(f"no ledger at {run} — run `ledger.py init` first")
    with open(state_path) as f:
        state = json.load(f)
    for task in state.get("tasks", []):
        task.setdefault("priority", "p2")
    return state


def save(run, state):
    state_path, md_path = paths(run)
    os.makedirs(run, exist_ok=True)
    with open(state_path, "w") as f:
        json.dump(state, f, indent=2)
        f.write("\n")
    with open(md_path, "w") as f:
        f.write(render(state, run))
    return md_path


def phase_names(state):
    return [p["name"] for p in state["config"]["phases"]]


def parse_phases(spec):
    """"implement:opus,cleanup:gpt-5.6-luna" -> [{name, model}, ...]"""
    phases = []
    for item in spec.split(","):
        item = item.strip()
        if not item:
            continue
        name, _, model = item.partition(":")
        name = name.strip()
        if not name:
            sys.exit(f"phase spec `{item}` has no name")
        if name in [p["name"] for p in phases]:
            sys.exit(f"duplicate phase name `{name}`")
        phases.append({"name": name, "model": model.strip()})
    if not phases:
        sys.exit("--phases must name at least one phase")
    return phases


def render(state, run):
    cfg = state["config"]
    tasks = state["tasks"]
    names = phase_names(state)
    counts = {s: sum(1 for t in tasks if t["status"] == s) for s in STATUSES}
    tally = " · ".join(f"{s} {counts[s]}" for s in STATUSES if counts[s])

    out = [f"# subagent-swarm — {cfg['goal']}", ""]
    out.append(f"- **run**: `{run}`")
    out.append(f"- **merging into**: `{cfg['target']}`  (base `{cfg['base']}`)")
    out.append("- **phases**: " + " → ".join(
        f"{p['name']} `{p['model']}`" if p["model"] else p["name"] for p in cfg["phases"]))
    out.append(f"- **status**: {tally or 'no lanes yet'}")
    out.append("")
    out.append("| | priority | lane | status | phase | branch | "
               + " | ".join(names) + " | merge |")
    out.append("|---|---|---|---|---|---|" + "---|" * (len(names) + 1))
    for t in tasks:
        cells = " | ".join(code(t["sessions"].get(n, "")) for n in names)
        out.append("| {m} | {priority} | **{id}**<br>{title} | {status} | {phase} | `{branch}` | "
                   "{cells} | {merge} |".format(
                       m=MARK.get(t["status"], "?"), id=t["id"], title=t["title"],
                       priority=t.get("priority", "p2"),
                       status=t["status"], phase=t["phase"] or "—", branch=t["branch"],
                       cells=cells, merge=code(t["merge_sha"])))
    out.append("")

    for t in tasks:
        detail = []
        if t["depends_on"]:
            detail.append(f"- depends on: {', '.join('`%s`' % d for d in t['depends_on'])}")
        if t["worktree"]:
            detail.append(f"- worktree: `{t['worktree']}`")
        if t["brief"]:
            detail.append(f"- brief: {t['brief']}")
        for n in t["notes"]:
            detail.append(f"- note: {n}")
        if detail:
            out.append(f"## {t['id']}")
            out.extend(detail)
            out.append("")
    return "\n".join(out)


def code(v):
    return f"`{v}`" if v else "—"


def find(state, task_id):
    for t in state["tasks"]:
        if t["id"] == task_id:
            return t
    sys.exit(f"no lane `{task_id}` in this run")


def main():
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)

    p = sub.add_parser("init")
    p.add_argument("run")
    p.add_argument("--goal", required=True)
    p.add_argument("--phases", required=True,
                   help='ordered, e.g. "implement:opus,cleanup:gpt-5.6-luna"')
    p.add_argument("--base", default="main")
    p.add_argument("--target", default="")

    p = sub.add_parser("add")
    p.add_argument("run")
    p.add_argument("--id", required=True)
    p.add_argument("--title", required=True)
    p.add_argument("--branch", required=True)
    p.add_argument("--priority", choices=PRIORITIES, default="p2")
    p.add_argument("--depends-on", default="")
    p.add_argument("--brief", default="")

    p = sub.add_parser("set")
    p.add_argument("run")
    p.add_argument("--id", required=True)
    p.add_argument("--status", choices=STATUSES)
    p.add_argument("--phase")
    p.add_argument("--session", help="session id for the lane's current phase")
    p.add_argument("--priority", choices=PRIORITIES)
    p.add_argument("--worktree")
    p.add_argument("--window-id")
    p.add_argument("--merge-sha")
    p.add_argument("--note")

    p = sub.add_parser("advance")
    p.add_argument("run")
    p.add_argument("--id", required=True)

    p = sub.add_parser("lanes")
    p.add_argument("run")
    p.add_argument("--status", default="running",
                   help="comma-separated statuses to include (default: running)")
    p.add_argument("--phase", default="", help="comma-separated phase allowlist")
    p.add_argument("--need-worktree", action="store_true",
                   help="skip lanes with no recorded worktree (warns on stderr)")
    p.add_argument("--config", metavar="KEY", help="print one config value and exit")

    for name in ("show", "ready", "inflight"):
        sub.add_parser(name).add_argument("run")

    a = ap.parse_args()

    if a.cmd == "init":
        state = {"config": {"goal": a.goal, "phases": parse_phases(a.phases),
                            "base": a.base, "target": a.target or a.base}, "tasks": []}
        print(save(a.run, state))
        return

    state = load(a.run)

    if a.cmd == "add":
        if any(t["id"] == a.id for t in state["tasks"]):
            sys.exit(f"lane `{a.id}` already exists")
        state["tasks"].append({
            "id": a.id, "title": a.title, "branch": a.branch, "brief": a.brief,
            "depends_on": [d.strip() for d in a.depends_on.split(",") if d.strip()],
            "priority": a.priority, "status": "planned", "phase": "",
            "sessions": {}, "worktree": "",
            "window_id": "", "merge_sha": "", "notes": [],
        })
        print(save(a.run, state))

    elif a.cmd == "set":
        t = find(state, a.id)
        if a.phase is not None and a.phase not in phase_names(state):
            sys.exit(f"`{a.phase}` is not a phase of this run "
                     f"({', '.join(phase_names(state))})")
        for field, value in (("status", a.status), ("phase", a.phase),
                             ("priority", a.priority),
                             ("worktree", a.worktree), ("merge_sha", a.merge_sha),
                             ("window_id", a.window_id)):
            if value is not None:
                t[field] = value
        if a.session is not None:
            if not t["phase"]:
                sys.exit(f"lane `{a.id}` has no current phase — set --phase first")
            t["sessions"][t["phase"]] = a.session
        if a.note:
            t["notes"].append(a.note)
        print(save(a.run, state))

    elif a.cmd == "advance":
        t = find(state, a.id)
        names = phase_names(state)
        if not t["phase"]:
            t["phase"], t["status"] = names[0], "running"
        else:
            i = names.index(t["phase"]) if t["phase"] in names else -1
            if i < 0:
                sys.exit(f"lane `{a.id}` is on unknown phase `{t['phase']}`")
            if i + 1 < len(names):
                t["phase"], t["status"] = names[i + 1], "running"
            else:
                t["status"] = "done"
        print(f"{a.id}: status={t['status']} phase={t['phase'] or '—'}", file=sys.stderr)
        print(save(a.run, state))

    elif a.cmd == "show":
        print(render(state, a.run))

    elif a.cmd == "ready":
        done = {t["id"] for t in state["tasks"] if t["status"] == "done"}
        tasks = sorted(state["tasks"],
                       key=lambda t: PRIORITY_RANK.get(t.get("priority", "p2"), 2))
        for t in tasks:
            if t["status"] == "planned" and all(d in done for d in t["depends_on"]):
                print(t["id"])

    elif a.cmd == "inflight":
        print(sum(1 for t in state["tasks"] if t["status"] == "running"))

    elif a.cmd == "lanes":
        if a.config:
            print(state["config"].get(a.config, ""))
            return
        want_status = {s.strip() for s in a.status.split(",") if s.strip()}
        want_phase = {p.strip() for p in a.phase.split(",") if p.strip()}
        for t in state["tasks"]:
            if want_status and t.get("status") not in want_status:
                continue
            if want_phase and t.get("phase") not in want_phase:
                continue
            wt = t.get("worktree") or ""
            if a.need_worktree and not wt:
                print(f"warn: {t['id']} has no worktree recorded in the ledger",
                      file=sys.stderr)
                continue
            print("\t".join([t["id"], t.get("phase", ""), t.get("branch", ""), wt,
                             t.get("window_id", "")]))


if __name__ == "__main__":
    main()
