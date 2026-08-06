"""Human second-pass review of a frozen ground truth — overlay model.

The frozen ``ground_truth_v3`` block is the benchmark's fitness function, and
it is **known-incomplete**: on kernel-8276, 15 unmatched defect locations
recurred across independent reviewer runs (14 across two configs) against a
frozen census of 10 true positives. Judges have also erred outright — one
froze a GT against a stale branch tip, another returned an empty census
without reading the diff. A human pass is the correction path.

This module is the *safe write path* for that pass. Two rules shape it.

**1. The UI never writes the dataset.** A human's verdicts land in an overlay
file under ``human-review/``; a separate, explicit ``collect.py
apply-human-review`` folds them in. Nothing a human clicks can reach the
benchmark without a deliberate second command.

**2. A human pass is not a judge round.** The tempting implementation — emit a
synthetic judge verdict and reuse ``collect.py fold`` — is wrong in a way that
is easy to miss. ``fold`` is not idempotent (it appends to ``per_round_diff``
and ``verdict_history``), and worse, ``_route_finding_verdict``'s *flip* branch
would treat a human rejection of an existing true positive as a judge
disagreeing with a judge: the entry is moved, marked resolved, **and appended
to `contested`**, permanently recording human-vs-machine disagreement as if two
rounds had disagreed. It would also bump ``rounds_run``, making
``census_converged`` partly a function of human agreement. So this module
rewrites the frozen block directly and additively, touching none of the
round-accounting fields.

Every mutation is stamped with ``human_verdict``, which makes the whole pass
**reversible**: :func:`strip_human_review` restores the pre-human block exactly.
"""

from __future__ import annotations

import copy
import hashlib
import json
import sys
from pathlib import Path
from typing import Optional

SCRIPT_DIR = Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

import collect_lib as cl  # noqa: E402
import harvest_lib as hl  # noqa: E402

OVERLAY_SCHEMA_VERSION = 1

# Where a human pass is staged before it is applied. Deliberately a sibling of
# `dataset/`, never inside it: a half-finished review must not be mistaken for
# dataset content by anything globbing the dataset dir.
OVERLAY_DIRNAME = "human-review"

OP_CONFIRM = "confirm"
OP_REJECT = "reject"
OP_RESEVERITY = "reseverity"
OP_ADD = "add"
VALID_OPS = frozenset({OP_CONFIRM, OP_REJECT, OP_RESEVERITY, OP_ADD})

# The buckets a human op may read or move an entry between. `contested` is an
# audit record, not a live queue (measured: 0 unresolved contested entries
# corpus-wide), so ops never move an entry *into* it.
_BUCKETS = ("true_positives", "false_positives")


# ===========================================================================
# Fingerprinting — detect a GT re-collected after the human reviewed it
# ===========================================================================


def gt_fingerprint(gt: dict) -> str:
    """Stable hash of *which defects* a frozen GT block judges.

    Answers one question: has collection changed this record's finding set
    since the human reviewed it? If so, verdicts cast against the old set may
    name defects that no longer exist, or miss ones that now do.

    What it covers is therefore deliberately narrow — the identity and
    description of each judged entry, pooled across buckets. Four exclusions,
    each load-bearing:

    - **Bucket membership.** ``reject`` MOVES an entry between
      ``true_positives`` and ``false_positives``. Hashing per-bucket would
      make an apply invalidate its own overlay, so the second (idempotent)
      apply would abort with a spurious "ground truth changed" error.
    - **Human-added entries.** ``add`` appends a new entry; counting it would
      likewise break re-application. Entries carrying ``human_verdict`` with
      op ``add`` are skipped.
    - **Human severity overrides.** ``judge_severity`` is preferred over
      ``severity`` so a ``reseverity`` does not invalidate its own overlay.
    - **Re-freeze metadata** (``frozen_at``, ``collector_git_sha``,
      ``per_round_diff``, ``dataset_xref``) — these move for reasons unrelated
      to whether the human's verdicts still apply.

    The net effect: applying an overlay never changes the fingerprint, but a
    genuine re-collection (a judge adding or rewording a finding) does.
    """
    rows = []
    for bucket in ("true_positives", "false_positives", "contested"):
        for e in gt.get(bucket) or []:
            if not isinstance(e, dict):
                continue
            if (e.get("human_verdict") or {}).get("verdict") == OP_ADD:
                continue  # not part of the judged set this review was cast against
            rows.append([
                cl.normalize_finding_path(e.get("file")),
                e.get("line"),
                e.get("judge_severity") or e.get("severity"),
                (e.get("topic") or "").strip(),
            ])
    rows.sort(key=lambda r: (str(r[0]), str(r[1]), str(r[2]), str(r[3])))
    blob = json.dumps(rows, sort_keys=True, separators=(",", ":"))
    return "sha256:" + hashlib.sha256(blob.encode("utf-8")).hexdigest()


# ===========================================================================
# Overlay validation
# ===========================================================================


def validate_overlay(obj: object) -> Optional[str]:
    """Return an error string if ``obj`` is not a well-formed overlay.

    Mirrors :func:`collect_lib.validate_judge_verdict`'s posture: reject at the
    input boundary, before anything can be written into the frozen block.
    """
    if not isinstance(obj, dict):
        return "overlay is not a JSON object"
    if obj.get("schema_version") != OVERLAY_SCHEMA_VERSION:
        return (
            f"overlay schema_version must be {OVERLAY_SCHEMA_VERSION}, got "
            f"{obj.get('schema_version')!r}"
        )
    if not obj.get("target"):
        return "overlay missing 'target'"
    verdicts = obj.get("verdicts")
    if not isinstance(verdicts, list):
        return "overlay missing 'verdicts' list"

    for i, v in enumerate(verdicts):
        if not isinstance(v, dict):
            return f"verdicts[{i}] is not an object"
        op = v.get("op")
        if op not in VALID_OPS:
            return f"verdicts[{i}].op must be one of {sorted(VALID_OPS)}"
        if not v.get("file") or not isinstance(v.get("file"), str):
            return f"verdicts[{i}] missing 'file'"
        if "line" not in v:
            # `None` is legal (a file-level finding) but the key must be
            # present, so an omitted line can never be silently read as
            # file-level. Same rule as `_loc_error`.
            return f"verdicts[{i}] missing 'line' (use null for file-level)"
        line = v.get("line")
        if line is not None and not isinstance(line, int):
            return f"verdicts[{i}].line must be an integer or null"
        if op in (OP_RESEVERITY, OP_ADD):
            if v.get("severity") not in cl.VALID_SEVERITIES:
                return (
                    f"verdicts[{i}].severity must be one of "
                    f"{sorted(cl.VALID_SEVERITIES)} for op={op!r}"
                )
        if op == OP_ADD and not (v.get("topic") or "").strip():
            return f"verdicts[{i}] op=add requires a non-empty 'topic'"
        if not v.get("reviewer"):
            return f"verdicts[{i}] missing 'reviewer' (attribution required)"

    dup = _overlay_collision_error(verdicts)
    if dup:
        return dup
    return None


def _overlay_collision_error(verdicts: list) -> Optional[str]:
    """Reject two ops that collapse onto one defect identity.

    The same trap ``collect_lib._file_level_collision_error`` guards: identity
    is ``(normalized_file, line)`` with ±3 rows of slack, so two ops a few rows
    apart target ONE entry. Left unchecked, the second silently overwrites the
    first and the human never learns their edit was discarded.

    Two ops at one identity are only a conflict when they disagree — the same
    op repeated is idempotent by design and passes.
    """
    located = [
        (i, v) for i, v in enumerate(verdicts)
        if isinstance(v, dict) and v.get("file")
    ]
    for pos, (i, v) in enumerate(located):
        for j, w in located[:pos]:
            v_null, w_null = v.get("line") is None, w.get("line") is None
            if v_null != w_null:
                # File-level and line-level are distinct entries by
                # construction — see `collect_lib._entry_matches`.
                continue
            if not cl.same_defect(v.get("file"), v.get("line"),
                                  w.get("file"), w.get("line")):
                continue
            if v.get("op") == w.get("op") and (
                v.get("severity") == w.get("severity")
            ):
                continue
            return (
                f"verdicts[{i}] (op={v.get('op')!r} at "
                f"{cl.normalize_finding_path(v.get('file'))}:{v.get('line')}) "
                f"collides with verdicts[{j}] (op={w.get('op')!r} at line "
                f"{w.get('line')}). Defect identity is (file, line) with "
                f"{cl._LINE_SLACK}-row slack, so these target the SAME entry "
                "and the later would silently overwrite the earlier. Keep one."
            )
    return None


def new_overlay(target: str, gt: dict) -> dict:
    """An empty overlay pinned to the GT block it was reviewed against."""
    return {
        "schema_version": OVERLAY_SCHEMA_VERSION,
        "target": target,
        "gt_frozen_at": gt.get("frozen_at") or "",
        "gt_fingerprint": gt_fingerprint(gt),
        "verdicts": [],
    }


def overlay_path(eval_root: Path, target: str) -> Path:
    return eval_root / OVERLAY_DIRNAME / f"{target}.json"


def load_overlay(path: Path) -> Optional[dict]:
    if not path.is_file():
        return None
    return json.loads(path.read_text())


def save_overlay(path: Path, overlay: dict) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    return hl.atomic_write_json(path, overlay)


def upsert_verdict(overlay: dict, verdict: dict) -> dict:
    """Add or replace a verdict in the overlay, keyed by defect identity.

    A human who adjudicates the same finding twice (changed their mind, or
    re-clicked) must end with ONE verdict, not two that collide at apply time.
    Identity is the strict ``_entry_matches`` rule so a file-level verdict
    never displaces a line-level one in the same file.
    """
    verdicts = overlay.setdefault("verdicts", [])
    for i, existing in enumerate(verdicts):
        if _same_identity(existing, verdict):
            verdicts[i] = verdict
            return overlay
    verdicts.append(verdict)
    return overlay


def _same_identity(a: dict, b: dict) -> bool:
    """Strict ``(file, line)`` identity — ``None`` matches only ``None``.

    Uses ``collect_lib._entry_matches``' rule rather than the looser
    ``same_defect``. The difference is load-bearing: ``same_defect`` lets a
    file-level location subsume every line-level one in the file, which would
    make a single file-level human verdict silently re-rule unrelated entries
    — the kernel-8229 collision, in a new costume.
    """
    a_line, b_line = a.get("line"), b.get("line")
    if (a_line is None) != (b_line is None):
        return False
    return cl.same_defect(a.get("file"), a_line, b.get("file"), b_line)


# ===========================================================================
# Apply / strip
# ===========================================================================


def _find_in_bucket(gt: dict, bucket: str, v: dict) -> Optional[dict]:
    for e in gt.get(bucket) or []:
        if isinstance(e, dict) and _same_identity(e, v):
            return e
    return None


def _locate(gt: dict, v: dict) -> tuple[Optional[str], Optional[dict]]:
    """Find the entry a verdict targets. Returns ``(bucket_name, entry)``."""
    for bucket in _BUCKETS:
        found = _find_in_bucket(gt, bucket, v)
        if found is not None:
            return bucket, found
    return None, None


def _stamp(v: dict) -> dict:
    return {
        "verdict": v.get("op"),
        "reviewer": v.get("reviewer"),
        "at": v.get("at") or hl.iso_utc_now(),
        "reason": str(v.get("reason") or ""),
    }


def apply_human_review(gt: dict, overlay: dict) -> tuple[dict, list[str]]:
    """Fold an overlay's verdicts into a frozen GT block.

    Returns ``(new_gt, notes)``. The input ``gt`` is not mutated.

    **Idempotent**: every op is a function of the target entry's *current*
    state, and re-applying an identical overlay produces an identical block.
    Deliberately does NOT touch ``rounds_run``, ``per_round_diff``,
    ``verdict_history``, or ``census_converged`` — a human pass is not a round,
    and letting a human flip the convergence flag would make that already-weak
    signal (18 of 48 records) mean two different things.
    """
    out = copy.deepcopy(gt)
    notes: list[str] = []

    for v in overlay.get("verdicts") or []:
        op = v.get("op")
        bucket, entry = _locate(out, v)
        loc = f"{cl.normalize_finding_path(v.get('file'))}:{v.get('line')}"

        if op == OP_ADD:
            if entry is not None:
                # The UI guards this at input, but an overlay can be
                # hand-written. Adding a second entry here would create
                # exactly the colliding pair `_file_level_collision_error`
                # exists to prevent, so degrade to annotating the entry that
                # already occupies the identity.
                entry["human_verdict"] = _stamp(v)
                notes.append(
                    f"add at {loc} matches existing {bucket} entry "
                    f"(within {cl._LINE_SLACK} rows) — annotated it instead "
                    "of adding a duplicate"
                )
                continue
            out.setdefault("true_positives", []).append({
                "file": v.get("file"),
                "line": v.get("line"),
                "severity": v.get("severity"),
                "topic": str(v.get("topic") or ""),
                # `None` marks this as not judge-derived: no collection round
                # ever saw it. Consumers that group by round must not read it
                # as round 0.
                "first_seen_round": None,
                "surfaced_by": ["human"],
                "judge_reason": str(v.get("reason") or ""),
                "reviewer_severity": None,
                "verdict_history": [],
                "resolved": True,
                "comment_action_xref": None,
                "human_verdict": _stamp(v),
            })
            notes.append(f"added true positive at {loc}")
            continue

        if entry is None:
            notes.append(f"SKIPPED {op} at {loc} — no matching GT entry")
            continue

        if op == OP_CONFIRM:
            entry["human_verdict"] = _stamp(v)
            notes.append(f"confirmed {bucket[:-1]} at {loc}")

        elif op == OP_RESEVERITY:
            # Preserve the judge's severity under its own key. Note
            # `reviewer_severity` already means "what the REVIEWER reported"
            # and feeds severity-drift analysis — overloading it would
            # corrupt that measurement.
            if "judge_severity" not in entry:
                entry["judge_severity"] = entry.get("severity")
            entry["severity"] = v.get("severity")
            entry["human_verdict"] = _stamp(v)
            notes.append(
                f"severity {entry.get('judge_severity')!r} -> "
                f"{v.get('severity')!r} at {loc}"
            )

        elif op == OP_REJECT:
            # `human_moved_from` records the ORIGINAL bucket and is written
            # once. The destination is derived from IT, not from the entry's
            # current bucket — otherwise a second apply reads the
            # already-moved entry and flips it straight back, oscillating on
            # every run. That is the exact non-idempotency `fold` suffers
            # from, and the reason this op names an absolute destination
            # rather than "the other one".
            origin = entry.get("human_moved_from") or bucket
            entry["human_moved_from"] = origin
            target_bucket = (
                "false_positives" if origin == "true_positives"
                else "true_positives"
            )
            entry["human_verdict"] = _stamp(v)
            if bucket != target_bucket:
                out[bucket] = [e for e in out.get(bucket) or []
                               if e is not entry]
                out.setdefault(target_bucket, []).append(entry)
                notes.append(
                    f"rejected: moved {loc} {bucket} -> {target_bucket}")
            else:
                notes.append(f"rejected: {loc} already in {target_bucket}")

    return out, notes


def strip_human_review(gt: dict) -> tuple[dict, int]:
    """Remove every human annotation, restoring the pre-human block.

    The inverse of :func:`apply_human_review`. This is what makes the human
    pass safe to run on the benchmark's source of truth: a bad pass is
    undone, not archaeologically repaired.
    """
    out = copy.deepcopy(gt)
    removed = 0

    # Drop human-added entries first — they have no pre-human existence.
    for bucket in _BUCKETS:
        rows = out.get(bucket) or []
        kept = []
        for e in rows:
            if isinstance(e, dict) and (e.get("human_verdict") or {}).get(
                "verdict"
            ) == OP_ADD and e.get("first_seen_round") is None:
                removed += 1
                continue
            kept.append(e)
        out[bucket] = kept

    # Reverse moves, restore severities, drop stamps.
    for bucket in _BUCKETS:
        for e in list(out.get(bucket) or []):
            if not isinstance(e, dict):
                continue
            origin = e.pop("human_moved_from", None)
            if "judge_severity" in e:
                e["severity"] = e.pop("judge_severity")
            if e.pop("human_verdict", None) is not None:
                removed += 1
            if origin and origin != bucket:
                out[bucket] = [x for x in out[bucket] if x is not e]
                out.setdefault(origin, []).append(e)

    return out, removed


def human_review_stats(gt: dict) -> dict:
    """Count how much of a frozen block a human has adjudicated."""
    total = 0
    adjudicated = 0
    added = 0
    for bucket in _BUCKETS:
        for e in gt.get(bucket) or []:
            if not isinstance(e, dict):
                continue
            total += 1
            hv = e.get("human_verdict") or {}
            if hv:
                adjudicated += 1
                if hv.get("verdict") == OP_ADD:
                    added += 1
    return {
        "entries": total,
        "adjudicated": adjudicated,
        "human_added": added,
        "pending": total - adjudicated,
    }
