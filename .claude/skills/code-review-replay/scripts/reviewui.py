#!/usr/bin/env python3
"""Local web UI for human review of the code-review ground truth.

The frozen ``ground_truth_v3`` block is the fitness function every reviewer
config is scored against, and it is known-incomplete: on kernel-8276, 15
unmatched defect locations recurred across independent reviewer runs (14
across two configs) against a census of 10 true positives. Judges have also
erred outright. This UI is the human correction pass.

Run it::

    python3 reviewui.py                 # read-only (the default)
    python3 reviewui.py --allow-write   # enable staging human verdicts

**Nothing here writes the dataset.** With ``--allow-write`` the UI stages
verdicts into an overlay under ``~/.bramble/code-review-eval/human-review/``;
folding them into the frozen block is a separate, deliberate command::

    python3 collect.py apply-human-review <target> [--dry-run]

Binds loopback only, single user, no auth by design.
"""

from __future__ import annotations

import argparse
import html
import json
import os
import sys
import webbrowser
from pathlib import Path
from typing import Optional
from urllib.parse import parse_qs, urlparse
from wsgiref.simple_server import WSGIRequestHandler, make_server

SCRIPT_DIR = Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

import collect_lib as cl  # noqa: E402
import harvest_lib as hl  # noqa: E402
import humanreview_lib as hr  # noqa: E402
import reviewui_lib as rl  # noqa: E402

ASSETS_DIR = SCRIPT_DIR / "reviewui_assets"


class _State:
    """Process-wide config. Single-user local tool; no session handling."""

    def __init__(self, *, dataset_dir: Path, eval_root: Path,
                 allow_write: bool, reviewer: str):
        self.dataset_dir = dataset_dir
        self.eval_root = eval_root
        self.allow_write = allow_write
        self.reviewer = reviewer
        self.repo_map = hl.RepoMap.discover([])

    def repo_for(self, record: dict) -> Optional[Path]:
        return self.repo_map.lookup((record.get("pr") or {}).get("repo_name"))

    def load(self, target: str) -> Optional[tuple[Path, dict]]:
        # `target` comes straight off the URL path, so it must be validated
        # before it is concatenated into a filesystem path — otherwise a
        # crafted `../../..` slug reads (and, on the POST path, overwrites)
        # files outside `dataset/`. Raises ValueError; callers turn that into
        # a 400 rather than a traceback.
        path = self.dataset_dir / f"{hr.validate_target(target)}.json"
        if not path.is_file():
            return None
        return path, json.loads(path.read_text())


STATE: Optional[_State] = None


# ===========================================================================
# JSON endpoints
# ===========================================================================


def _api_records() -> dict:
    assert STATE
    records = rl.load_records(STATE.dataset_dir)
    # Suggestion counts drive the default sort — the whole point of the human
    # pass is to spend attention where the census is most likely incomplete.
    # Index the replay dir ONCE: per-record scanning re-parses every archive
    # for every record, and that dir only ever grows.
    replay_dir = STATE.eval_root / "replays"
    replay_index = rl.index_replays(replay_dir)
    counts: dict[str, int] = {}
    staged: dict[str, int] = {}
    for target, rec in records:
        gt = cl.load_ground_truth(rec) or {}
        sugg = rl.load_suggestions(target, gt, replay_dir, index=replay_index)
        counts[target] = sum(1 for s in sugg if s.get("recurrent"))
        # Progress must count verdicts staged but not yet applied, or a
        # half-finished review reads as untouched and sorts back to the top.
        try:
            ov = hr.load_overlay(hr.overlay_path(STATE.eval_root, target))
        except ValueError:
            ov = None  # corrupt overlay: surfaced on the PR page, not here
        staged[target] = len((ov or {}).get("verdicts") or [])
    rows = [
        rl.summarize_record(t, r, STATE.repo_for(r), suggestion_counts=counts,
                            staged_counts=staged)
        for t, r in records
    ]
    rows.sort(key=lambda r: (
        r["pending"] == 0,            # unreviewed first
        -r["suggestions"],            # then most recurrent suggestions
        r["census_converged"],        # then unconverged (likely incomplete)
        r["target"],
    ))
    return {
        "records": rows,
        "allow_write": STATE.allow_write,
        "reviewer": STATE.reviewer,
        "totals": {
            "records": len(rows),
            "renderable": sum(1 for r in rows
                              if r["render_state"] == rl.RENDER_OK),
            "entries": sum(r["entries"] for r in rows),
            "adjudicated": sum(r["adjudicated"] for r in rows),
        },
    }


def _api_pr(target: str) -> dict:
    assert STATE
    loaded = STATE.load(target)
    if loaded is None:
        return {"error": f"no dataset record {target}"}
    _, record = loaded
    gt = cl.load_ground_truth(record) or {}
    repo = STATE.repo_for(record)
    revs = rl.resolve_revs(record, repo)

    diff_files: list[rl.DiffFile] = []
    if revs["state"] == rl.RENDER_OK and repo is not None:
        diff_files = rl.parse_diff(
            rl.diff_text(repo, revs["base"], revs["head"]))

    suggestions = rl.load_suggestions(target, gt, STATE.eval_root / "replays")
    ledger = rl.build_ledger(gt, diff_files, suggestions)
    # Report the replay window so a truncated candidate list is never mistaken
    # for the whole picture — the replay dir is an append-only experiment log
    # spanning multiple reviewer regimes.
    replay_window = {
        "archives_read": suggestions[0]["archives_read"] if suggestions else 0,
        "archives_total": suggestions[0]["archives_total"] if suggestions
        else 0,
    }

    # File context for every finding that has no rendered diff line. This is
    # what keeps the ~29% off-hunk population reviewable instead of hidden.
    contexts: dict[str, dict] = {}
    if repo is not None and revs["state"] == rl.RENDER_OK:
        for row in ledger:
            if row["anchor"] not in (rl.ANCHOR_OFF_HUNK, rl.ANCHOR_NOT_IN_DIFF):
                continue
            key = f"{row['file']}:{row['line']}"
            if key in contexts or not row.get("line"):
                continue
            ctx = rl.context_window(repo, revs["head"], row["file"],
                                    int(row["line"]))
            if ctx:
                contexts[key] = ctx

    overlay = hr.load_overlay(hr.overlay_path(STATE.eval_root, target)) or {}
    rnd = rl.canonical_round(record)
    return {
        "target": target,
        "goal_text": rnd.get("goal_text") or "",
        "files_changed": rnd.get("files_changed") or [],
        "harvest_source": record.get("harvest_source") or "pr-polish",
        "census_converged": bool(gt.get("census_converged")),
        "rounds_run": gt.get("rounds_run"),
        "frozen_at": gt.get("frozen_at"),
        "render": revs,
        "diff": [
            {
                "path": df.path,
                "lines": [
                    {"kind": l.kind, "text": l.text, "new": l.new_line,
                     "old": l.old_line}
                    for l in df.lines
                ],
            }
            for df in diff_files
        ],
        "ledger": ledger,
        "replay_window": replay_window,
        "contexts": contexts,
        "overlay": overlay,
        "gt_fingerprint": hr.gt_fingerprint(gt),
        "allow_write": STATE.allow_write,
        "reviewer": STATE.reviewer,
        "stats": hr.human_review_stats(gt),
    }


def _api_verdict(target: str, body: dict) -> dict:
    """Stage one verdict into the overlay. Never touches the dataset."""
    assert STATE
    if not STATE.allow_write:
        return {"error": "server is read-only; restart with --allow-write"}
    loaded = STATE.load(target)
    if loaded is None:
        return {"error": f"no dataset record {target}"}
    _, record = loaded
    gt = cl.load_ground_truth(record) or {}

    path = hr.overlay_path(STATE.eval_root, target)
    overlay = hr.load_overlay(path) or hr.new_overlay(target, gt)

    if body.get("op") == "_clear":
        # Withdraw a staged verdict — the human changed their mind before
        # applying. Identity-matched so it removes the right one.
        probe = {"file": body.get("file"), "line": body.get("line")}
        overlay["verdicts"] = [
            v for v in overlay.get("verdicts") or []
            if not hr.same_identity(v, probe)
        ]
        hr.save_overlay(path, overlay)
        return {"ok": True, "overlay": overlay}

    verdict = {
        "op": body.get("op"),
        "file": body.get("file"),
        "line": body.get("line"),
        "severity": body.get("severity"),
        "topic": body.get("topic"),
        "reason": body.get("reason") or "",
        "reviewer": STATE.reviewer,
        "at": hl.iso_utc_now(),
    }
    verdict = {k: v for k, v in verdict.items()
               if v is not None or k == "line"}

    hr.upsert_verdict(overlay, verdict)
    err = hr.validate_overlay(overlay)
    if err:
        return {"error": err}
    hr.save_overlay(path, overlay)
    return {"ok": True, "overlay": overlay}


def _api_check_identity(target: str, body: dict) -> dict:
    """Warn before a human 'adds' a finding that an entry already covers.

    Defect identity is ``(file, line)`` with ±3 rows of slack, so a new
    finding a couple of rows from an existing one is an EDIT of that entry,
    not an addition. Catching it here — at input — is what keeps the human's
    intent and the scorer's arithmetic aligned.
    """
    assert STATE
    loaded = STATE.load(target)
    if loaded is None:
        return {"error": f"no dataset record {target}"}
    _, record = loaded
    gt = cl.load_ground_truth(record) or {}
    file, line = body.get("file"), body.get("line")
    probe = {"file": file, "line": line}
    for bucket in ("true_positives", "false_positives"):
        for e in gt.get(bucket) or []:
            # Must use the SAME rule `apply_human_review` will use, not the
            # looser `same_defect`. Under `same_defect` a file-level entry
            # subsumes every line in its file, so the UI would warn "this
            # edits an existing entry" for a line the apply step would in
            # fact add as a distinct one — a warning that mispredicts the
            # write is worse than none.
            if hr.same_identity(e, probe):
                return {
                    "collision": True, "bucket": bucket,
                    "file": e.get("file"), "line": e.get("line"),
                    "topic": e.get("topic"),
                    "message": (
                        f"within {cl._LINE_SLACK} lines of an existing "
                        f"{bucket[:-1]} at {e.get('file')}:{e.get('line')} — "
                        "adding here edits that entry rather than creating a "
                        "new one"
                    ),
                }
    return {"collision": False}


# ===========================================================================
# WSGI
# ===========================================================================


def _json_response(start_response, payload: dict, status="200 OK"):
    body = json.dumps(payload).encode("utf-8")
    start_response(status, [
        ("Content-Type", "application/json; charset=utf-8"),
        ("Content-Length", str(len(body))),
        ("Cache-Control", "no-store"),
    ])
    return [body]


def _asset(start_response, name: str, content_type: str):
    path = ASSETS_DIR / name
    if not path.is_file():
        start_response("404 Not Found", [("Content-Type", "text/plain")])
        return [b"missing asset"]
    body = path.read_bytes()
    start_response("200 OK", [
        ("Content-Type", content_type),
        ("Content-Length", str(len(body))),
        ("Cache-Control", "no-store"),
    ])
    return [body]


def application(environ, start_response):
    # Every target-bearing endpoint validates its slug (see
    # `hr.validate_target`) and signals a bad one with ValueError. Catching it
    # here keeps one rejection path for all of them, so a traversal attempt
    # gets a clean 400 instead of a 500 with a traceback.
    try:
        return _dispatch(environ, start_response)
    except ValueError as e:
        return _json_response(start_response, {"error": str(e)},
                              "400 Bad Request")


def _dispatch(environ, start_response):
    path = urlparse(environ.get("PATH_INFO", "/")).path
    method = environ.get("REQUEST_METHOD", "GET")

    if path in ("/", "/index.html"):
        return _asset(start_response, "index.html", "text/html; charset=utf-8")
    if path == "/app.js":
        return _asset(start_response, "app.js",
                      "application/javascript; charset=utf-8")
    if path == "/app.css":
        return _asset(start_response, "app.css", "text/css; charset=utf-8")

    if path == "/api/records":
        return _json_response(start_response, _api_records())

    if path.startswith("/api/pr/"):
        target = path[len("/api/pr/"):]
        return _json_response(start_response, _api_pr(target))

    if path.startswith("/api/verdict/") and method == "POST":
        target = path[len("/api/verdict/"):]
        try:
            size = int(environ.get("CONTENT_LENGTH") or 0)
            body = json.loads(environ["wsgi.input"].read(size) or b"{}")
        except (ValueError, json.JSONDecodeError) as e:
            return _json_response(start_response, {"error": f"bad body: {e}"},
                                  "400 Bad Request")
        return _json_response(start_response, _api_verdict(target, body))

    if path.startswith("/api/check/") and method == "POST":
        target = path[len("/api/check/"):]
        try:
            size = int(environ.get("CONTENT_LENGTH") or 0)
            body = json.loads(environ["wsgi.input"].read(size) or b"{}")
        except (ValueError, json.JSONDecodeError) as e:
            return _json_response(start_response, {"error": f"bad body: {e}"},
                                  "400 Bad Request")
        return _json_response(start_response,
                              _api_check_identity(target, body))

    start_response("404 Not Found", [("Content-Type", "text/plain")])
    return [b"not found"]


class _QuietHandler(WSGIRequestHandler):
    def log_message(self, fmt, *args):  # noqa: A003
        pass


def main(argv: Optional[list[str]] = None) -> int:
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("--dataset-dir", type=Path, default=rl.DATASET_DIR)
    p.add_argument("--eval-root", type=Path, default=rl.EVAL_ROOT)
    p.add_argument("--port", type=int, default=0,
                   help="0 picks a free port (default), avoiding collisions.")
    p.add_argument("--host", default="127.0.0.1",
                   help="Loopback by default; this tool has no auth.")
    p.add_argument(
        "--allow-write", action="store_true",
        help="Permit staging verdicts into the human-review overlay. Even "
             "then the dataset is never written — that needs "
             "`collect.py apply-human-review`.")
    p.add_argument("--reviewer", default=os.environ.get("USER") or "unknown",
                   help="Attribution recorded on each verdict.")
    p.add_argument("--open", action="store_true",
                   help="Open a browser at the served URL.")
    p.add_argument("--verbose", "-v", action="store_true")
    args = p.parse_args(argv)

    global STATE
    STATE = _State(dataset_dir=args.dataset_dir, eval_root=args.eval_root,
                   allow_write=args.allow_write, reviewer=args.reviewer)

    if not args.dataset_dir.is_dir():
        print(f"error: no dataset dir at {args.dataset_dir}", file=sys.stderr)
        return 2

    handler = WSGIRequestHandler if args.verbose else _QuietHandler
    with make_server(args.host, args.port, application,
                     handler_class=handler) as httpd:
        port = httpd.server_address[1]
        url = f"http://{args.host}:{port}/"
        n = len(rl.load_records(args.dataset_dir))
        print(f"ground-truth review UI  →  {url}")
        print(f"  {n} records with frozen ground truth")
        print(f"  mode: {'READ-WRITE (overlay only)' if args.allow_write else 'READ-ONLY'}")
        print(f"  reviewer: {args.reviewer}")
        if args.allow_write:
            print("  verdicts stage to "
                  f"{args.eval_root / hr.OVERLAY_DIRNAME}/<target>.json")
            print("  apply with: python3 collect.py apply-human-review "
                  "<target> --dry-run")
        print("Ctrl-C to stop.")
        if args.open:
            webbrowser.open(url)
        try:
            httpd.serve_forever()
        except KeyboardInterrupt:
            print("\nstopped.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
