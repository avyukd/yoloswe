"""Read path for the ground-truth review UI — diff reconstruction + anchoring.

The UI's job is to show a human every judged finding against the code it
concerns. The non-obvious part is that **a diff-only view is not sufficient**.

Measured across all 32 immediately-renderable GT records (2026-08-06):

    inside a rendered diff hunk        139   71.6%
    in a changed file, outside a hunk   35   18.0%
    file not in the diff at all         20   10.3%

**28% of findings do not land in a rendered hunk.** Judges legitimately
census defects in code the PR affects but does not edit, and ±3 rows of
identity slack plus post-hoc line drift push others just past a 3-line
context window. A UI that renders the diff and attaches findings to it would
silently hide more than a quarter of the set it exists to review — and the
hidden ones skew toward exactly the "the census missed something" cases the
human pass is for.

So this module classifies every finding into an :class:`Anchor` class, and the
UI gives each class its own home. Nothing is dropped for want of a diff line.
"""

from __future__ import annotations

import json
import re
import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path
from typing import Iterable, Optional

SCRIPT_DIR = Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

import collect_lib as cl  # noqa: E402
import harvest_lib as hl  # noqa: E402
import humanreview_lib as hr  # noqa: E402
import unmatched_lib as ul  # noqa: E402

EVAL_ROOT = Path.home() / ".bramble" / "code-review-eval"
DATASET_DIR = EVAL_ROOT / "dataset"
REPLAY_DIR = EVAL_ROOT / "replays"

# Anchor classes, in the order the ledger presents them.
ANCHOR_IN_HUNK = "in-hunk"
ANCHOR_OFF_HUNK = "in-file-off-hunk"
ANCHOR_FILE_LEVEL = "file-level"
ANCHOR_NOT_IN_DIFF = "not-in-diff"
ANCHOR_ORDER = (ANCHOR_IN_HUNK, ANCHOR_OFF_HUNK, ANCHOR_FILE_LEVEL,
                ANCHOR_NOT_IN_DIFF)

# Lines of file context shown around an off-hunk finding.
CONTEXT_RADIUS = 20

# Render states for a record.
RENDER_OK = "renderable"
RENDER_NEEDS_FETCH = "needs-fetch"
RENDER_UNRECOVERABLE = "unrecoverable"

_HUNK_RE = re.compile(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@")
_NEWFILE_RE = re.compile(r"^\+\+\+ (?:b/)?(.*)$")
# Matches the old-side header in every form git emits: `--- a/path`, and
# `--- /dev/null` for an added file. Anchoring only on `a/` left the /dev/null
# variant to fall through to the `-` (deletion) branch, where it was rendered
# as a code line reading `-- /dev/null` at the tail of the PREVIOUS file.
_OLDFILE_RE = re.compile(r"^--- (?:a/|/dev/null)")


# ===========================================================================
# Dataset loading
# ===========================================================================


def load_records(dataset_dir: Path = DATASET_DIR) -> list[tuple[str, dict]]:
    """Every record carrying a frozen ground truth, as ``(target, record)``.

    ``index.json`` is skipped; it is a rollup, not a record.
    """
    out = []
    for path in sorted(dataset_dir.glob("*.json")):
        if path.name == "index.json":
            continue
        try:
            rec = json.loads(path.read_text())
        except (json.JSONDecodeError, OSError):
            continue
        if cl.load_ground_truth(rec):
            out.append((path.stem, rec))
    return out


def canonical_round(record: dict) -> dict:
    rounds = record.get("harvested_rounds") or []
    return rounds[0] if rounds else {}


# ===========================================================================
# Git plumbing
# ===========================================================================


def _git(repo: Path, *args: str) -> subprocess.CompletedProcess:
    return hl.git(repo, *args)


def _has_commit(repo: Path, sha: Optional[str]) -> bool:
    if not sha:
        return False
    return _git(repo, "cat-file", "-e", f"{sha}^{{commit}}").returncode == 0


def resolve_revs(record: dict, repo: Optional[Path]) -> dict:
    """Work out what this record can render, and how.

    Returns ``{state, head, base, base_source, detail}``.

    ``merge_base_sha`` is often ``null`` on older records — not because the
    PR was force-pushed, but because the harvester could not compute a base
    while the head commit was missing locally (``merge_base_error: "head
    commit not in local repo"``). Measured: of 16 such records, 8 recover
    completely once the head is fetched and the base recomputed against the
    recorded ``base_branch``. So a missing base is reported as *recoverable*,
    not as a dead record.
    """
    rnd = canonical_round(record)
    head = rnd.get("head_before")
    base = rnd.get("merge_base_sha")
    base_branch = rnd.get("base_branch") or "origin/main"

    if repo is None:
        return {"state": RENDER_UNRECOVERABLE, "head": head, "base": base,
                "base_source": None, "detail": "no local checkout for repo"}

    if not _has_commit(repo, head):
        return {
            "state": RENDER_NEEDS_FETCH, "head": head, "base": base,
            "base_source": None,
            "detail": "head_before not present locally — try "
                      "`git fetch origin refs/pull/<N>/head`",
        }

    if _has_commit(repo, base):
        return {"state": RENDER_OK, "head": head, "base": base,
                "base_source": "recorded", "detail": ""}

    # Head is present but the base is not — recompute it.
    proc = _git(repo, "merge-base", base_branch, head)
    if proc.returncode == 0 and proc.stdout.strip():
        return {
            "state": RENDER_OK, "head": head, "base": proc.stdout.strip(),
            "base_source": "recomputed",
            "detail": f"merge base recomputed against {base_branch} "
                      "(record stored null)",
        }
    return {"state": RENDER_UNRECOVERABLE, "head": head, "base": base,
            "base_source": None,
            "detail": f"cannot resolve a merge base against {base_branch}"}


def diff_text(repo: Path, base: str, head: str) -> str:
    """The PR's diff, scoped exactly as ``collect.py build-prompt`` scopes it.

    Two-dot ``base..head``, never ``main...HEAD``. The three-dot form diffs
    against the merge base of *local main* and head, which on a long-merged PR
    is some commit well after it landed — the bug that had the reviewer
    hunting 10 defects across 336 files instead of 22.
    """
    proc = _git(repo, "diff", "--no-color", f"{base}..{head}")
    return proc.stdout if proc.returncode == 0 else ""


def file_at(repo: Path, sha: str, path: str) -> Optional[list[str]]:
    proc = _git(repo, "show", f"{sha}:{path}")
    if proc.returncode != 0:
        return None
    return proc.stdout.splitlines()


# ===========================================================================
# Diff parsing
# ===========================================================================


@dataclass
class DiffLine:
    kind: str          # "add" | "del" | "ctx" | "hunk"
    text: str
    new_line: Optional[int] = None
    old_line: Optional[int] = None


@dataclass
class DiffFile:
    path: str
    lines: list[DiffLine] = field(default_factory=list)

    @property
    def new_lines(self) -> set[int]:
        """New-side line numbers this file's hunks actually render."""
        return {ln.new_line for ln in self.lines if ln.new_line is not None}


def parse_diff(text: str) -> list[DiffFile]:
    """Parse unified diff text into per-file line lists.

    Tracks new-side line numbers so a finding's line can be matched against
    what is actually rendered. Only the new side matters: GT lines are
    recorded against ``head_before``.
    """
    files: list[DiffFile] = []
    cur: Optional[DiffFile] = None
    new_ln = old_ln = None

    for raw in text.splitlines():
        m = _NEWFILE_RE.match(raw)
        if m:
            path = m.group(1)
            # A deleted file's new side is /dev/null — there is nothing to
            # render and no new-side line numbers. Match both spellings: the
            # `b/` prefix is optional in the pattern, so this arrives as
            # `/dev/null` here and as `dev/null` when git wrote `+++ b/`.
            if path in ("/dev/null", "dev/null"):
                cur = None
                new_ln = old_ln = None
                continue
            cur = DiffFile(path=hl.normalize_path(path) or path)
            files.append(cur)
            new_ln = old_ln = None
            continue
        if _OLDFILE_RE.match(raw) or raw.startswith("diff --git"):
            continue
        m = _HUNK_RE.match(raw)
        if m:
            if cur is not None:
                cur.lines.append(DiffLine(kind="hunk", text=raw))
            new_ln = int(m.group(1))
            # Old-side start, for completeness in the rendered gutter.
            om = re.match(r"^@@ -(\d+)", raw)
            old_ln = int(om.group(1)) if om else None
            continue
        if cur is None or new_ln is None:
            continue
        if raw.startswith("+"):
            cur.lines.append(DiffLine("add", raw[1:], new_line=new_ln))
            new_ln += 1
        elif raw.startswith("-"):
            cur.lines.append(DiffLine("del", raw[1:], old_line=old_ln))
            if old_ln is not None:
                old_ln += 1
        elif raw.startswith(" "):
            cur.lines.append(
                DiffLine("ctx", raw[1:], new_line=new_ln, old_line=old_ln))
            new_ln += 1
            if old_ln is not None:
                old_ln += 1
        # "\ No newline at end of file" and anything else is not a code line.
    return files


# ===========================================================================
# Anchoring
# ===========================================================================


def classify_anchor(
    file: Optional[str], line: Optional[int], diff_files: list[DiffFile]
) -> str:
    """Which surface should host this finding.

    See the module docstring: 28% of real findings are not in a hunk, so this
    must never answer "nowhere".
    """
    if line is None:
        return ANCHOR_FILE_LEVEL
    norm = cl.normalize_finding_path(file)
    for df in diff_files:
        if cl.normalize_finding_path(df.path) != norm:
            continue
        return ANCHOR_IN_HUNK if line in df.new_lines else ANCHOR_OFF_HUNK
    return ANCHOR_NOT_IN_DIFF


def _entry_view(entry: dict, bucket: str, diff_files: list[DiffFile]) -> dict:
    hv = entry.get("human_verdict") or {}
    return {
        "kind": "gt",
        "bucket": bucket,
        "file": entry.get("file"),
        "line": entry.get("line"),
        "severity": entry.get("severity"),
        "judge_severity": entry.get("judge_severity"),
        "topic": entry.get("topic") or "",
        "judge_reason": entry.get("judge_reason") or "",
        "surfaced_by": entry.get("surfaced_by") or [],
        "first_seen_round": entry.get("first_seen_round"),
        "reviewer_severity": entry.get("reviewer_severity"),
        "human_verdict": hv or None,
        "human_moved_from": entry.get("human_moved_from"),
        "anchor": classify_anchor(entry.get("file"), entry.get("line"),
                                  diff_files),
    }


def build_ledger(
    gt: dict, diff_files: list[DiffFile], suggestions: Iterable[dict] = ()
) -> list[dict]:
    """Every finding a human should see, each with a home.

    Ground-truth entries first (true positives, then false positives, then
    contested-only entries), followed by unmatched replay suggestions. The
    ledger is the completeness guarantee: its length must equal the number of
    judged entries plus suggestions, so nothing can be lost to rendering.
    """
    rows: list[dict] = []
    seen: list[tuple[Optional[str], Optional[int]]] = []

    for bucket in ("true_positives", "false_positives"):
        for e in gt.get(bucket) or []:
            if not isinstance(e, dict):
                continue
            rows.append(_entry_view(e, bucket, diff_files))
            seen.append((e.get("file"), e.get("line")))

    # A contested entry normally also lives in a TP/FP bucket (it is an audit
    # record, not a quarantine). Surface only ones that do not, so the ledger
    # neither double-counts nor drops them.
    for e in gt.get("contested") or []:
        if not isinstance(e, dict):
            continue
        if any(cl.same_defect(e.get("file"), e.get("line"), f, ln)
               for f, ln in seen):
            continue
        rows.append(_entry_view(e, "contested", diff_files))

    for s in suggestions:
        rows.append({
            "kind": "suggestion",
            "bucket": "unmatched",
            "file": s.get("file"),
            "line": s.get("line"),
            "severity": None,
            "topic": (
                f"unmatched in {s.get('n_runs')} run(s) / "
                f"{s.get('n_configs')} config(s)"
            ),
            "judge_reason": "",
            "surfaced_by": sorted(s.get("configs") or []),
            "n_runs": s.get("n_runs"),
            "n_configs": s.get("n_configs"),
            "recurrent": s.get("recurrent"),
            "cross_config": s.get("cross_config"),
            "human_verdict": None,
            "anchor": classify_anchor(s.get("file"), s.get("line"),
                                      diff_files),
        })
    return rows


def group_by_anchor(rows: list[dict]) -> dict[str, list[dict]]:
    groups: dict[str, list[dict]] = {k: [] for k in ANCHOR_ORDER}
    for r in rows:
        groups.setdefault(r.get("anchor") or ANCHOR_NOT_IN_DIFF, []).append(r)
    return groups


def context_window(
    repo: Path, head: str, path: str, line: int, radius: int = CONTEXT_RADIUS
) -> Optional[dict]:
    """File content around an off-hunk finding, read at ``head_before``.

    This is what gives the 28% of findings that miss the diff somewhere real
    to live. Labeled as file context, never as diff, so a human is never
    misled about whether the PR touched these lines.
    """
    content = file_at(repo, head, path)
    if content is None:
        return None
    start = max(1, line - radius)
    end = min(len(content), line + radius)
    return {
        "path": path,
        "start": start,
        "end": end,
        "total": len(content),
        "lines": [
            {"n": n, "text": content[n - 1]} for n in range(start, end + 1)
        ],
    }


# ===========================================================================
# Suggestion feed — unmatched replay findings
# ===========================================================================


# How many of a PR's most recent scored archives feed the suggestion list.
#
# The replay dir is an append-only experiment log, not a current-state view:
# kernel-8276 alone has 93 scored archives spanning several distinct reviewer
# configurations, including runs from BEFORE the `--diff-base` fix (when the
# reviewer inferred a 336-file diff instead of the PR's 22). Summing all of
# history conflates those regimes and inflates the candidate list with
# locations no current config would report.
#
# Recurrence still needs several runs to mean anything, so the window must be
# wide enough to see repetition and narrow enough to stay within one regime.
# `load_suggestions` reports how many archives it read and how many exist, so
# the truncation is visible rather than silent.
DEFAULT_REPLAY_WINDOW = 12


def index_replays(replay_dir: Path = REPLAY_DIR) -> dict[str, list[tuple]]:
    """One pass over the replay dir, grouping archives by dataset target.

    `/api/records` needs suggestion counts for every record. Calling
    :func:`load_suggestions` per record re-globs and re-parses the whole
    replay dir each time — 48 records x 153 archives today (~2s), and the dir
    is append-only, so the cost climbs with every bake-off. Building the index
    once and passing it in keeps the list endpoint linear in archives rather
    than quadratic.
    """
    index: dict[str, list[tuple]] = {}
    for path in sorted(replay_dir.glob("*-scored.json")):
        try:
            doc = json.loads(path.read_text())
        except (json.JSONDecodeError, OSError):
            continue
        target = (doc.get("dataset_file") or "")[:-5]
        if not target:
            continue
        index.setdefault(target, []).append(
            (doc.get("generated_at") or "", path.stem, doc))
    return index


def load_suggestions(
    target: str, gt: dict, replay_dir: Path = REPLAY_DIR, *,
    window: int = DEFAULT_REPLAY_WINDOW,
    index: Optional[dict[str, list[tuple]]] = None,
) -> list[dict]:
    """Unmatched replay findings for one PR, ranked by cross-run recurrence.

    Reuses :func:`unmatched_lib.collect_unmatched` so the grouping obeys the
    same ±3-row identity rule the ground truth uses. Recurrence is the cheap
    discriminator between "found a real defect the census missed" and
    "generated plausible noise" — noise is drawn from a wide space and rarely
    collides twice on one line.

    Only the ``window`` most recent archives are read; see
    :data:`DEFAULT_REPLAY_WINDOW` for why aggregating all history is wrong.
    Pass ``window=0`` to read every archive.
    """
    if index is not None:
        docs = list(index.get(target) or [])
    else:
        docs = []
        for path in sorted(replay_dir.glob("*-scored.json")):
            try:
                doc = json.loads(path.read_text())
            except (json.JSONDecodeError, OSError):
                continue
            if (doc.get("dataset_file") or "")[:-5] != target:
                continue
            docs.append((doc.get("generated_at") or "", path.stem, doc))

    total = len(docs)
    docs.sort(key=lambda d: d[0])
    if window and total > window:
        docs = docs[-window:]

    observations: list[tuple[str, str, str, Optional[int]]] = []
    for _, run_id, doc in docs:
        for rnd in doc.get("rounds") or []:
            for run in rnd.get("runs") or []:
                config = run.get("config") or run.get("config_name") or "?"
                for fs in run.get("finding_scores") or []:
                    if fs.get("outcome") != "unmatched":
                        continue
                    observations.append(
                        (run_id, config, fs.get("file"), fs.get("line")))
    if not observations:
        return []
    locs = ul.collect_unmatched(observations, gt)
    return [
        {
            "file": loc.file, "line": loc.line,
            "n_runs": loc.n_runs, "n_configs": loc.n_configs,
            "recurrent": loc.recurrent, "cross_config": loc.cross_config,
            "configs": sorted(loc.configs),
            "archives_read": len(docs),
            "archives_total": total,
        }
        for loc in locs
    ]


# ===========================================================================
# Record summaries — the PR list
# ===========================================================================


def summarize_record(
    target: str, record: dict, repo: Optional[Path], *,
    suggestion_counts: Optional[dict] = None,
    staged_counts: Optional[dict] = None,
) -> dict:
    gt = cl.load_ground_truth(record) or {}
    revs = resolve_revs(record, repo)
    stats = hr.human_review_stats(gt)
    rnd = canonical_round(record)
    return {
        "target": target,
        "repo": (record.get("pr") or {}).get("repo_name"),
        "pr_number": (record.get("pr") or {}).get("pr_number"),
        # Shown as a badge, not filtered: github-tier records carry real
        # frozen GT (17 TP / 26 FP across 8 records) but sit outside the
        # bake-off scoring pool, so a human should see the label and decide.
        "harvest_source": record.get("harvest_source") or "pr-polish",
        "census_converged": bool(gt.get("census_converged")),
        "true_positives": len(gt.get("true_positives") or []),
        "false_positives": len(gt.get("false_positives") or []),
        "contested": len(gt.get("contested") or []),
        "files_changed": len(rnd.get("files_changed") or []),
        "render_state": revs["state"],
        "render_detail": revs["detail"],
        "base_source": revs.get("base_source"),
        "adjudicated": stats["adjudicated"],
        "entries": stats["entries"],
        # Staged-but-unapplied verdicts count as progress: a review left
        # half-finished in the overlay is not untouched work, and treating it
        # as such sorts it back to the top of the queue every session.
        #
        # The caller must pass only verdicts that resolve an EXISTING entry —
        # an `add` creates new work rather than closing pending work. Clamped
        # to `stats["pending"]` as well as to 0 so a miscount can never make a
        # PR look finished; the sort key depends on this.
        "staged": min((staged_counts or {}).get(target, 0), stats["pending"]),
        "pending": max(
            0, stats["pending"] - min((staged_counts or {}).get(target, 0),
                                      stats["pending"])),
        "human_added": stats["human_added"],
        "suggestions": (suggestion_counts or {}).get(target, 0),
        "goal_text": (rnd.get("goal_text") or "")[:200],
    }
