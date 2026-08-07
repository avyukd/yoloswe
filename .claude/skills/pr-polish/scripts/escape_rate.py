#!/usr/bin/env python3
"""Measure pr-polish's local-review miss rate — the "escape rate".

An **escape** is a substantive finding an external reviewer posted on a PR
that pr-polish's own reviewers did not surface and its triage never
recorded. Each one is a defect the local loop should have caught: the whole
value proposition of /pr-polish is that passing its gate means external
reviewers have nothing substantive left to say.

    escape_rate = escaped / total substantive external findings

This is the North Star metric for tuning bramble code-review. It is
deliberately cheap: no judge sub-agents, no ground truth, just the state
files and comment census already on disk.

``escaped_in_scope`` splits depth failures from scope failures — an escape
citing a file pr-polish already had in ``files_changed`` means the reviewer
looked at the right code and missed the bug (a depth problem), while an
out-of-scope escape means it never looked (a scope problem). Measured on
the kernel corpus, 87% of escapes were in-scope.

**Read the raw rate as an upper bound.** An "escape" here is a bot claim
pr-polish did not match, and bot claims on this corpus were measured at
**~9% precision** (20 of 224 confirmed by independent judges). Most raw
escapes are therefore not real defects. When a PR has a frozen
``ground_truth_v3``, ``--judged`` restricts the numerator to escapes a
judge confirmed as true positives, which is the trustworthy number; the
raw rate is the fallback for PRs with no ground truth yet.

Usage:
    escape_rate.py <state-dir>          # one run
    escape_rate.py --all                # fleet-wide, aggregated
"""

from __future__ import annotations

import argparse
import importlib.util
import json
import re
import sys
from pathlib import Path
from typing import Any, Optional

SCRIPT_DIR = Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

# Reuse the noise filter rather than re-deriving it: it encodes real
# tuning against bot output (summary boilerplate, CodeQL prose, known
# noise authors) that would be easy to get subtly wrong a second time.
# (The filename has dashes, so it needs a path-based import.)
_spec = importlib.util.spec_from_file_location(
    "compare_r1_r2", SCRIPT_DIR / "compare-r1-r2.py"
)
_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_mod)  # type: ignore[union-attr]
is_substantive = _mod.is_substantive

# The producer-side backend roster. ``local_findings`` derives its state
# keys from this rather than restating them, so a backend added to
# pr-polish cannot be silently dropped from the escape-rate denominator.
import bramble_ops  # noqa: E402

# The replay skill's location matcher — the same authority the ground-truth
# scorer uses, so "did the reviewer catch this" means the same thing in both
# places. Imported, never reimplemented: path+topic matching is subtle.
_REPLAY_SCRIPTS = (
    SCRIPT_DIR.parents[1] / "code-review-replay" / "scripts"
)
if str(_REPLAY_SCRIPTS) not in sys.path:
    sys.path.insert(0, str(_REPLAY_SCRIPTS))
try:
    from collect_lib import same_defect  # noqa: E402
except Exception:  # pragma: no cover - replay skill absent
    def same_defect(f1, l1, f2, l2):  # type: ignore[misc]
        if not f1 or not f2:
            return False
        if Path(str(f1)).name != Path(str(f2)).name:
            return False
        if l1 is None or l2 is None:
            return True
        return abs(int(l1) - int(l2)) <= 3


P1_PATTERN = re.compile(r"P1\b|blocking|critical|high severity", re.I)


def _load(path: Path) -> Optional[dict]:
    try:
        return json.loads(path.read_text())
    except (OSError, json.JSONDecodeError):
        return None


def local_findings(state: dict) -> list[dict]:
    """Every finding pr-polish's own reviewers surfaced, across all rounds.

    The per-backend keys are derived from ``bramble_ops.BACKENDS`` rather
    than restated. ``pr_ops._persist_round_findings`` writes
    ``f"{backend}_findings"`` for every entry in that roster, so a
    hand-written list here silently drops any backend added since it was
    typed — and a dropped backend does not look like an error, it looks
    like a clean run. ``compute_escape_rate`` scores an external comment as
    an *escape* when it finds no local finding matching it, so a backend
    missing from this list inflates the escape rate on exactly the runs
    that used it.
    """
    out: list[dict] = []
    for rnd in state.get("rounds") or []:
        for key in (f"{b}_findings" for b in bramble_ops.BACKENDS):
            for f in rnd.get(key) or []:
                if isinstance(f, dict):
                    out.append(f)
    return out


def triaged_locations(state: dict) -> list[tuple[Optional[str], Any]]:
    """Locations the engineer's triage recorded an action for."""
    out: list[tuple[Optional[str], Any]] = []
    for rnd in state.get("rounds") or []:
        for a in rnd.get("comment_actions") or []:
            if a.get("action"):
                out.append((a.get("path"), a.get("line")))
    return out


def in_scope_files(state: dict, record: Optional[dict] = None) -> set[str]:
    """Basenames of every file the diff changed.

    ``pr-polish-state.json`` does not persist ``files_changed`` — only the
    harvested record computes it from the merge base — so the dataset is
    the real source here. Reading only the state would silently report
    every escape as out-of-scope, inverting the depth-vs-scope signal this
    metric exists to give.
    """
    out: set[str] = set()
    for rnd in (state.get("rounds") or []):
        for f in rnd.get("files_changed") or []:
            out.add(Path(str(f)).name)
    for rnd in ((record or {}).get("harvested_rounds") or []):
        for f in rnd.get("files_changed") or []:
            out.add(Path(str(f)).name)
    return out


def load_comments(state_dir: Path, record: Optional[dict]) -> list[dict]:
    """External review comments for this PR, preferring a post-run census.

    ``pp-comments.json`` is a snapshot taken *during* the run, so it
    structurally cannot contain the comments that arrive after pr-polish
    exits — which are exactly the escapes. The harvested dataset re-fetches
    from GitHub after the fact, so it is the correct source; the in-run
    snapshot is only a fallback (and will report zero escapes).
    """
    if record:
        out: list[dict] = []
        for rnd in record.get("harvested_rounds") or []:
            for a in rnd.get("raw_comment_actions") or []:
                if str(a.get("source", "")).startswith("github"):
                    out.append(a)
        if out:
            return out
    blob = _load(state_dir / "pp-comments.json")
    return (blob or {}).get("comments") or []


def judged_true_positives(record: Optional[dict]) -> Optional[list[tuple]]:
    """Locations a judge confirmed as real, or None if no frozen GT.

    Returning None (rather than an empty list) matters: "no ground truth
    collected" and "ground truth says nothing here is real" are different
    claims, and only the second one licenses calling an escape spurious.
    """
    gt = (record or {}).get("ground_truth_v3")
    if not isinstance(gt, dict):
        return None
    return [
        (e.get("file"), e.get("line"))
        for e in (gt.get("true_positives") or [])
    ]


def compute_escape_rate(
    state_dir: Path, dataset_dir: Optional[Path] = None
) -> Optional[dict]:
    """Escape metrics for one pr-polish run, or None if unmeasurable."""
    state = _load(state_dir / "pr-polish-state.json")
    if not state:
        return None
    completed_at = state.get("completed_at")
    if not completed_at:
        return None

    record = _load(dataset_dir / f"{state_dir.name}.json") if dataset_dir else None
    comments = load_comments(state_dir, record)
    if not comments:
        return None
    locals_ = local_findings(state)
    triaged = triaged_locations(state)
    scope = in_scope_files(state, record)
    judged = judged_true_positives(record)

    total = 0
    escaped: list[dict] = []
    for c in comments:
        if not is_substantive(c):
            continue
        # Only findings posted after the loop declared itself done are
        # escapes; earlier ones were available as round input.
        created = c.get("created_at")
        if not created or created <= completed_at:
            continue
        path, line = c.get("path"), c.get("line")
        # Only locatable findings count. A review-level comment with no
        # path is usually a summary ("3 issues found") whose individual
        # findings are already counted as inline rows — including them
        # would both double-count and make "did the reviewer catch this"
        # unanswerable, since there is no location to match on.
        if not path:
            continue
        total += 1
        caught = any(
            same_defect(path, line, f.get("file") or f.get("path"), f.get("line"))
            for f in locals_
        ) or any(same_defect(path, line, tp, tl) for tp, tl in triaged)
        if caught:
            continue
        body = c.get("body") or ""
        # None => no ground truth for this PR, so we cannot say whether the
        # claim was real. Only an actual frozen GT can mark it spurious.
        if judged is None:
            confirmed = None
        else:
            confirmed = any(
                same_defect(path, line, jf, jl) for jf, jl in judged
            )
        escaped.append(
            {
                "author": c.get("author"),
                "path": path,
                "line": line,
                "severity": "P1" if P1_PATTERN.search(body) else "other",
                "in_scope": bool(path) and Path(str(path)).name in scope,
                "judge_confirmed": confirmed,
                "body_excerpt": body[:200],
            }
        )

    return {
        "schema_version": 1,
        "state_dir": str(state_dir),
        "pr": state.get("pr_number"),
        "exit_reason": state.get("exit_reason"),
        # The computed verdict, when the run wrote one. Distinct from
        # exit_reason: a run can exit `converged` and still be `not_ready`
        # (e.g. an unverified fix claim), so the two must be bucketed apart.
        "verdict": (state.get("verdict") or {}).get("verdict"),
        "completed_at": completed_at,
        "bot_findings_substantive": total,
        "caught_locally": total - len(escaped),
        "escaped": len(escaped),
        "escaped_p1": sum(1 for e in escaped if e["severity"] == "P1"),
        "escaped_in_scope": sum(1 for e in escaped if e["in_scope"]),
        "escape_rate": (len(escaped) / total) if total else None,
        # Judge-confirmed subset: the trustworthy numerator. None when this
        # PR has no frozen ground truth to check against.
        "escaped_judged": (
            sum(1 for e in escaped if e["judge_confirmed"])
            if judged is not None else None
        ),
        "has_ground_truth": judged is not None,
        "escapes": escaped,
    }


def _split_by(rows: list[dict], key: str) -> dict[str, dict]:
    """Bucket scored rows by ``key``, with an escape rate per bucket."""
    out: dict[str, dict] = {}
    for r in rows:
        b = out.setdefault(
            r.get(key) or "unknown", {"runs": 0, "total": 0, "escaped": 0}
        )
        b["runs"] += 1
        b["total"] += r["bot_findings_substantive"]
        b["escaped"] += r["escaped"]
    for b in out.values():
        b["escape_rate"] = (b["escaped"] / b["total"]) if b["total"] else None
    return out


def aggregate(rows: list[dict]) -> dict:
    """Fleet-wide totals, split by computed verdict and by exit reason.

    ``by_verdict`` is the acceptance test for the verdict work: escapes
    among runs the verdict called ``ready`` should be materially rarer than
    among the rest. ``by_exit_reason`` is the older proxy for the same
    question, kept because it is the only signal available for runs that
    predate the verdict (they bucket as ``unknown``) — and because the two
    diverging is itself the finding: a run can exit ``converged`` while its
    verdict is ``not_ready``.
    """
    scored = [r for r in rows if r.get("escape_rate") is not None]
    total = sum(r["bot_findings_substantive"] for r in scored)
    escaped = sum(r["escaped"] for r in scored)
    by_reason = _split_by(scored, "exit_reason")
    by_verdict = _split_by(scored, "verdict")
    gt_rows = [r for r in scored if r.get("has_ground_truth")]
    gt_total = sum(r["bot_findings_substantive"] for r in gt_rows)
    gt_escaped = sum(r["escaped_judged"] or 0 for r in gt_rows)
    return {
        "runs_measured": len(scored),
        # The trustworthy slice: only PRs with frozen ground truth, and
        # only escapes a judge confirmed as real defects.
        "judged": {
            "runs": len(gt_rows),
            "bot_findings_substantive": gt_total,
            "escaped_judged": gt_escaped,
            "escape_rate_judged": (gt_escaped / gt_total) if gt_total else None,
        },
        "runs_skipped": len(rows) - len(scored),
        "bot_findings_substantive": total,
        "escaped": escaped,
        "escaped_p1": sum(r["escaped_p1"] for r in scored),
        "escaped_in_scope": sum(r["escaped_in_scope"] for r in scored),
        "escape_rate": (escaped / total) if total else None,
        "by_exit_reason": by_reason,
        "by_verdict": by_verdict,
    }


def main(argv: Optional[list[str]] = None) -> int:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("state_dir", nargs="?", type=Path)
    p.add_argument(
        "--all",
        action="store_true",
        dest="scan_all",
        help="Scan every pr-polish state dir and aggregate.",
    )
    p.add_argument(
        "--projects-root",
        type=Path,
        default=Path.home() / ".bramble" / "projects",
    )
    p.add_argument(
        "--dataset-dir",
        type=Path,
        default=Path.home() / ".bramble" / "code-review-eval" / "dataset",
        help="Harvested dataset, used for the post-run comment census. "
        "Without it only the in-run snapshot is available, which cannot "
        "contain escapes by construction.",
    )
    p.add_argument(
        "--write",
        action="store_true",
        help="Write escape-metrics.json into each measured state dir.",
    )
    args = p.parse_args(argv)

    if args.scan_all:
        rows = []
        for d in sorted(args.projects_root.iterdir()):
            if not d.is_dir():
                continue
            row = compute_escape_rate(d, args.dataset_dir)
            if row is None:
                continue
            rows.append(row)
            if args.write:
                (d / "escape-metrics.json").write_text(json.dumps(row, indent=2))
        print(json.dumps(aggregate(rows), indent=2))
        return 0

    if not args.state_dir:
        print("error: pass a state dir or --all", file=sys.stderr)
        return 2
    row = compute_escape_rate(args.state_dir, args.dataset_dir)
    if row is None:
        print(
            f"error: {args.state_dir} has no measurable run "
            "(missing state, comments, or completed_at)",
            file=sys.stderr,
        )
        return 2
    if args.write:
        (args.state_dir / "escape-metrics.json").write_text(
            json.dumps(row, indent=2)
        )
    print(json.dumps(row, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
