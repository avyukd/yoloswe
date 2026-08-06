"""Library helpers for the bramble code-review eval-dataset harvester.

The harvester walks ``~/.bramble/projects/<repo>-<pr>/`` directories
left behind by the ``/pr-polish`` skill and emits one structured JSON
record per PR, suitable for replaying ``bramble code-review`` against
the same commits and the same ``--goal`` text.

The hard parts live here:
  * matching envelope findings back to ``comment_actions`` entries so
    each finding gets a ground-truth label;
  * reconstructing the per-round ``--goal`` text via the pr-polish
    skill's own ``bramble_ops.goal_for_round`` (the goal body is not
    persisted anywhere on disk — it must be regenerated from state);
  * computing the ``git merge-base origin/main <head_before>`` so a
    replay script knows the exact diff scope the reviewer saw.
"""

from __future__ import annotations

import datetime as _dt
import functools
import importlib.util
import json
import os
import re
import subprocess
import sys
import tempfile
import time
from contextlib import contextmanager
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any, Iterable, Literal, Optional

SCHEMA_VERSION = 3

# How a record was sourced. ``pr-polish`` records are full fidelity: they
# carry the local reviewer envelopes (``review_runs``) and the engineer's
# triage. ``github`` records are built from the GitHub API alone, for PRs
# that were never polished locally — they have the same diff scope, goal,
# and comment census, but no ``review_runs`` and no harvest-time labels.
# Both are fully collectable and replayable: ``ground_truth_v3`` is
# judge-produced from the diff and consumes neither.
HarvestSource = Literal["pr-polish", "github"]
DEFAULT_HARVEST_SOURCE: HarvestSource = "pr-polish"

# Where a round's diff scope came from. ``git`` is a local checkout;
# ``github`` is the compare/files API, used when the commit is absent
# locally (a PR never fetched, or a stale checkout).
ScopeResolver = Literal["git", "github"]

# Stands in for a GitHub-sourced record's absent pr-polish directory. Every
# read under a state dir (envelopes, scope hints) already handles "not
# there", so pointing at a path that cannot exist reuses those paths
# instead of threading `if state_dir is None` through the round builder.
_NO_STATE_DIR = Path("/nonexistent/code-review-replay/no-local-state")

# The judged ground-truth block collection mode adds to a per-PR dataset.
# Defined here (the lowest layer) so collect_lib and the harvester share one
# source of truth; the schema key stays `ground_truth_v3` even at schema 4.
GROUND_TRUTH_KEY = "ground_truth_v3"


# ---------------------------------------------------------------------------
# Shared low-level helpers
# ---------------------------------------------------------------------------


def iso_utc_now() -> str:
    """Current UTC time as a strict-ISO8601 ``YYYY-MM-DDTHH:MM:SSZ`` string."""
    return _dt.datetime.now(_dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def run_id_stamp() -> str:
    """Current UTC time as a filesystem-safe ``YYYYMMDD-HHMMSS`` run id."""
    return _dt.datetime.now(_dt.timezone.utc).strftime("%Y%m%d-%H%M%S")


def atomic_write_json(path: Path, obj: object) -> Path:
    """Write ``obj`` as pretty JSON to ``path`` atomically.

    The JSON is written to a temp file in the same directory and renamed over
    the target, so a crash mid-write cannot leave a half-written file. The
    parent directory is created if missing.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp_name = tempfile.mkstemp(dir=str(path.parent), suffix=".tmp")
    try:
        with os.fdopen(fd, "w") as fh:
            fh.write(json.dumps(obj, indent=2) + "\n")
        os.replace(tmp_name, path)
    except BaseException:
        # fatal()-style helpers raise SystemExit (a BaseException); clean the
        # temp file on any abnormal exit, then re-raise.
        if os.path.exists(tmp_name):
            os.unlink(tmp_name)
        raise
    return path


Action = Literal[
    "fixed", "false_positive", "wont_fix", "ack", "stale", "flake", "pre_existing"
]
SignalTier = Literal["r1", "final", "final_incomplete", "r1_only"]
MatchStrategy = Literal[
    "exact", "topic_path_line", "topic_path", "topic_only", "none"
]
EnvelopeStatus = Literal["ok", "error", "missing"]
AttributionBasis = Literal[
    "created_at", "unmapped_repo_fallback", "no_timestamp"
]

# Backends pr-polish runs. ``lint`` writes its findings through the same
# envelope schema even though it isn't a bramble model backend, so we
# treat it as a first-class review_run source here.
BACKENDS = ("codex", "cursor", "gemini", "lint")

# GitHub PR-comment sources. These are the comments authored on the PR by
# humans and review bots — distinct from bramble's own reviewer findings.
GITHUB_SOURCES = frozenset({"github-inline", "github-issue", "github-review"})

# Sources in comment_actions that are NOT bramble findings; they're PR
# comments / CI failures recorded for the audit trail. We don't try to
# match envelope findings against them, but we keep them in
# ``raw_comment_actions`` so the dataset is self-describing.
NON_BACKEND_SOURCES = frozenset(GITHUB_SOURCES | {"ci"})

# Sources that pr-polish uses when consensus-merging findings from
# multiple backends. Treated as a wildcard backend in Tier 1 matching.
WILDCARD_BACKEND_SOURCES = frozenset({"sweep", "consensus"})


# ---------------------------------------------------------------------------
# Dataclasses
# ---------------------------------------------------------------------------


@dataclass
class GroundTruth:
    matched_comment_action: bool
    match_strategy: MatchStrategy
    action: Optional[str]
    reason: Optional[str]
    is_real_issue: Optional[bool]
    fixed_in_commit: Optional[str]
    comment_actions_source: Optional[str]


@dataclass
class Finding:
    severity: Optional[str]
    message: str
    suggestion: Optional[str]
    file: Optional[str]
    line: Optional[int]
    confidence: Optional[float]
    invariant: Optional[str]
    sites: Optional[list[dict]]
    ground_truth: GroundTruth


@dataclass
class ReviewRun:
    backend: str
    model: Optional[str]
    session_id: Optional[str]
    review_mode: Optional[str]
    resume_status: Optional[str]
    envelope_status: EnvelopeStatus
    envelope_error: Optional[str]
    verdict: Optional[str]
    summary: Optional[str]
    duration_ms: Optional[int]
    input_tokens: Optional[int]
    output_tokens: Optional[int]
    schema_version: Optional[int]
    findings: list[Finding] = field(default_factory=list)


@dataclass
class HarvestedRound:
    round: int
    signal_tier: SignalTier
    head_before: Optional[str]
    head_after: Optional[str]
    base_branch: str
    merge_base_sha: Optional[str]
    merge_base_resolved: bool
    merge_base_error: Optional[str]
    files_changed: list[str]
    goal_text: Optional[str]
    goal_recoverable: bool
    scope_hints_present: bool
    raw_comment_actions: list[dict]
    review_runs: list[ReviewRun] = field(default_factory=list)
    # Which resolver produced merge_base_sha / files_changed. A pr-polish
    # record whose local checkout went stale can legitimately carry
    # "github" here, so this is per-round rather than per-record.
    merge_base_resolved_by: ScopeResolver = "git"
    files_changed_resolved_by: ScopeResolver = "git"


@dataclass
class PRRecord:
    schema_version: int
    harvested_at: str
    harvester_git_sha: str
    pr: dict
    pr_comments_attribution_basis: AttributionBasis
    pr_comments_fetch_error: Optional[str]
    harvested_rounds: list[HarvestedRound] = field(default_factory=list)
    harvest_source: HarvestSource = DEFAULT_HARVEST_SOURCE


# ---------------------------------------------------------------------------
# Project-dir parsing
# ---------------------------------------------------------------------------


# Match e.g. "kernel-3945", "yoloswe-236", "nebula-81". Reject the doc / branch
# variants: "kernel-doc-naming-rethink-cb9650558e82",
# "yoloswe-branch-feature-meeting-bot".
_PROJECT_DIR_RE = re.compile(r"^(?P<repo>[a-z][a-z0-9]+)-(?P<pr>\d+)$")


def parse_project_dir_name(name: str) -> Optional[tuple[str, str]]:
    """Return ``(repo_name, pr_number)`` or ``None`` for non-PR dirs."""
    m = _PROJECT_DIR_RE.match(name)
    if not m:
        return None
    return m.group("repo"), m.group("pr")


def discover_project_dirs(source_dir: Path) -> list[tuple[Path, str, str]]:
    """List PR-numbered project dirs that contain pr-polish-state.json.

    Returns ``[(dir_path, repo_name, pr_number), ...]`` sorted by name.
    """
    out: list[tuple[Path, str, str]] = []
    if not source_dir.exists():
        return out
    for entry in sorted(source_dir.iterdir()):
        if not entry.is_dir():
            continue
        parsed = parse_project_dir_name(entry.name)
        if parsed is None:
            continue
        if not (entry / "pr-polish-state.json").is_file():
            continue
        repo, pr = parsed
        out.append((entry, repo, pr))
    return out


# ---------------------------------------------------------------------------
# State + envelope parsing
# ---------------------------------------------------------------------------


def parse_state_file(path: Path) -> dict:
    """Load pr-polish-state.json and assert minimal shape."""
    state = json.loads(path.read_text())
    if not isinstance(state, dict):
        raise ValueError(f"state file is not a JSON object: {path}")
    if "rounds" not in state or not isinstance(state["rounds"], list):
        raise ValueError(f"state file missing 'rounds' list: {path}")
    return state


def record_harvest_source(dataset: dict) -> HarvestSource:
    """The harvest source of an on-disk record, defaulting to pr-polish.

    Records written before schema 3 have no ``harvest_source`` key, and all
    of them came from local pr-polish state — so a missing field reads as
    ``"pr-polish"`` rather than as an error. Consumers should call this
    instead of indexing the key directly.
    """
    value = dataset.get("harvest_source")
    return value if value in ("pr-polish", "github") else DEFAULT_HARVEST_SOURCE


def _ancestry_rank(repo_path: Optional[Path], sha: str) -> int:
    """Commits-since-root, as a tie-break when two commits share a timestamp.

    ``git rev-list --count`` gives a monotonic depth along a single history,
    so an ancestor always sorts before its descendant even when both carry
    the same second.
    """
    if repo_path is None:
        return 0
    res = git(repo_path, "rev-list", "--count", sha)
    try:
        return int(res.stdout.strip())
    except (ValueError, AttributeError):
        return 0


def commit_scoped_rounds(
    gh: dict,
    comments: list[dict],
    *,
    repo_path: Optional[Path] = None,
) -> list[dict]:
    """One synthesized round per commit an inline comment actually reviewed.

    GitHub records every inline comment's ``original_commit_id`` — the SHA the
    reviewer was looking at. Pinning every comment to the PR's final head
    instead (see :func:`github_only_state`) does not merely lose coverage: it
    inverts the ground truth. A bot correctly reports a bug, the author fixes
    it, and a judge reading the post-fix code records the claim as a false
    positive. Measured on kernel-8227, *none* of its 107 inline comments were
    written against the head they were being judged at.

    Rounds are ordered oldest-first by commit time so round 1 is the earliest
    reviewed state. Comments whose SHA is unreachable — force-pushed away, ~8%
    on the pilot corpus — are folded into a final round pinned at the PR head
    and flagged ``head_unreachable``, so they degrade visibly instead of
    silently masquerading as same-state review.
    """
    by_sha: dict[str, list[dict]] = {}
    orphans: list[dict] = []
    for c in comments:
        sha = c.get("original_commit_id")
        if not sha:
            orphans.append(c)
            continue
        by_sha.setdefault(sha, []).append(c)

    resolvable: list[tuple[str, Optional[str], list[dict]]] = []
    for sha, rows in by_sha.items():
        t = git_commit_time(repo_path, sha) if repo_path else None
        if t is None:
            orphans.extend(rows)
        else:
            resolvable.append((sha, t, rows))
    # Commit time first, then ancestry, then sha. Rebased or scripted commits
    # can share a timestamp to the second, and a wrong order would mislabel
    # which state was reviewed "first" — so ties fall back to whether one
    # commit is an ancestor of the other.
    resolvable.sort(key=lambda x: (x[1] or "", _ancestry_rank(repo_path, x[0]), x[0]))

    rounds: list[dict] = []
    for i, (sha, _t, rows) in enumerate(resolvable, start=1):
        rounds.append({
            "n": i,
            "head_before": sha,
            "head_after": None,
            "comment_actions": rows,
            "head_unreachable": False,
        })
    if orphans or not rounds:
        rounds.append({
            "n": len(rounds) + 1,
            "head_before": gh.get("head_sha"),
            "head_after": None,
            "comment_actions": orphans,
            # These comments reviewed a commit that no longer exists, so the
            # diff they are judged against is NOT the one they were written
            # about. Consumers must treat their verdicts as lower confidence.
            "head_unreachable": bool(orphans),
        })
    return rounds


def github_only_state(
    gh: dict,
    comments: Optional[list[dict]] = None,
    *,
    repo_path: Optional[Path] = None,
) -> dict:
    """A pr-polish-state-shaped dict for a PR with no local state.

    ``gh`` is a :func:`discover_github_prs` row. With no ``comments`` the
    synthesized state has exactly one round, so
    :func:`select_rounds_to_harvest` yields
    ``[(1, "r1_only")]`` — the same canonical round collection already picks
    — and every downstream reader (``get_round``,
    ``index_comment_verdicts``, ``reconstruct_goal_text``) works unchanged.

    ``completed`` is True because the PR merged: leaving it False would make
    ``--no-include-incomplete`` silently drop every GitHub record. The
    lower fidelity is carried by ``exit_reason`` and by the record's
    ``harvest_source``, not by pretending the PR is unfinished.

    ``comments`` + ``repo_path`` opt into per-commit scoping: each inline
    comment is grouped under the SHA it actually reviewed
    (:func:`commit_scoped_rounds`) instead of all of them being pinned to the
    final head. Without them the single-round shape is preserved for
    backwards compatibility.
    """
    rounds = (
        commit_scoped_rounds(gh, comments, repo_path=repo_path)
        if comments
        else [{
            "n": 1,
            "head_before": gh.get("head_sha"),
            "head_after": None,
            "comment_actions": [],
        }]
    )
    return {
        "rounds": rounds,
        "completed": True,
        "exit_reason": "github_only",
        "branch": gh.get("head_ref"),
        "started_at": gh.get("merged_at"),
    }


def parse_envelope(path: Path) -> tuple[Optional[dict], Optional[str]]:
    """Return ``(envelope_dict, error_message)``.

    A missing file returns ``(None, "envelope file missing")``.
    Malformed JSON returns ``(None, "envelope JSON parse error: ...")``.
    Note: ``envelope["status"] == "error"`` is a *valid* envelope (the
    reviewer ran but failed); the caller surfaces it as
    ``envelope_status="error"``, not as a parse failure.
    """
    if not path.exists():
        return None, "envelope file missing"
    try:
        obj = json.loads(path.read_text())
    except json.JSONDecodeError as e:
        return None, f"envelope JSON parse error: {e}"
    if not isinstance(obj, dict):
        return None, "envelope is not a JSON object"
    return obj, None


def envelope_issues(env: dict) -> list[dict]:
    """The reviewer findings inside a bramble envelope's ``review.issues``.

    Non-dict entries are dropped. Returns ``[]`` for an error envelope or one
    with no review block.
    """
    review = env.get("review") or {}
    return [f for f in (review.get("issues") or []) if isinstance(f, dict)]


# ---------------------------------------------------------------------------
# Round selection
# ---------------------------------------------------------------------------


def select_rounds_to_harvest(state: dict) -> list[tuple[int, SignalTier]]:
    """Pick which rounds carry the highest signal.

    Per the locked-in plan, we only harvest R1 and the final round:
      - R1 = fresh-eyes recall on the original diff
      - Final = precision signal on near-converged code

    Single-round PRs are emitted once with ``r1_only``.
    """
    rounds = state.get("rounds") or []
    if not rounds:
        return []
    ns = sorted({int(r.get("n") or 0) for r in rounds if r.get("n")})
    if not ns:
        return []
    completed = bool(state.get("completed"))
    # Commit-scoped rounds are not pr-polish iterations — each one is a
    # distinct reviewed diff with its own comments. Dropping the middle would
    # discard most of the review states the SHAs were recovered for, so every
    # round is kept.
    if any(r.get("head_unreachable") is not None for r in rounds):
        if len(ns) == 1:
            return [(ns[0], "r1_only")]
        return [(n, "r1" if n == ns[0] else "final") for n in ns]
    if len(ns) == 1:
        return [(ns[0], "r1_only")]
    first, last = ns[0], ns[-1]
    if first == last:
        return [(first, "r1_only")]
    final_tier: SignalTier = "final" if completed else "final_incomplete"
    return [(first, "r1"), (last, final_tier)]


def get_round(state: dict, n: int) -> Optional[dict]:
    for r in state.get("rounds") or []:
        if int(r.get("n") or 0) == n:
            return r
    return None


# ---------------------------------------------------------------------------
# Path / topic normalisation
# ---------------------------------------------------------------------------


def normalize_path(p: Optional[str]) -> Optional[str]:
    """Strip leading './', collapse backslashes, lower drive letters.

    Returns ``None`` if input is None or empty after stripping.
    """
    if p is None:
        return None
    s = p.strip().replace("\\", "/")
    while s.startswith("./"):
        s = s[2:]
    return s or None


_TOKEN_RE = re.compile(r"[a-z0-9]+")


def _tokens(text: str) -> set[str]:
    return {t for t in _TOKEN_RE.findall(text.lower()) if len(t) > 3}


def topic_token_overlap(topic: str, message: str) -> float:
    """Jaccard overlap of >3-char lowercased tokens."""
    if not topic or not message:
        return 0.0
    a, b = _tokens(topic), _tokens(message)
    if not a or not b:
        return 0.0
    return len(a & b) / len(a | b)


def topic_token_containment(topic: str, body: str) -> float:
    """Fraction of the topic's >3-char tokens that appear in ``body``.

    Asymmetric on purpose — unlike :func:`topic_token_overlap`'s Jaccard.
    A PR comment body is long (severity badges, descriptions, code blocks)
    while a recorded ``topic`` is a short summary, so a symmetric metric
    unfairly penalises the body's extra tokens. Containment answers the
    right question: "is this topic about this comment?".
    """
    if not topic or not body:
        return 0.0
    a, b = _tokens(topic), _tokens(body)
    if not a or not b:
        return 0.0
    return len(a & b) / len(a)


def _topic_substring(topic: str, message: str, *, limit: int = 100) -> bool:
    if not topic or not message:
        return False
    t = topic.lower().strip()
    m = message.lower()[:limit]
    return t in m


# ---------------------------------------------------------------------------
# Finding ↔ comment_action matching
# ---------------------------------------------------------------------------

# Tier priorities and action-fix preference for tie-breaking.
_TIER_RANK = {
    "exact": 5,
    "topic_path_line": 4,
    "topic_path": 3,
    "topic_only": 2,
    "none": 0,
}
_ACTION_RANK = {
    "fixed": 3,
    "wont_fix": 2,
    "false_positive": 1,
    "stale": 0,
    "ack": -1,
    "flake": -2,
    "pre_existing": -3,
}


def _candidate_actions(round_data: dict) -> list[dict]:
    """Comment_actions eligible for envelope-finding matching.

    Drops github-* and ci sources — they're audit-trail entries, not
    reviewer findings. Kept ordering as in the state file so ties
    break toward earliest.
    """
    actions = round_data.get("comment_actions") or []
    return [a for a in actions if a.get("source") not in NON_BACKEND_SOURCES]


def match_finding_to_action(
    finding: dict,
    backend: str,
    candidate_actions: list[dict],
) -> tuple[Optional[dict], MatchStrategy]:
    """Best-match strategy for an envelope finding against this round's actions.

    Every tier first requires the action's ``source`` to be ``backend`` (or a
    cross-backend wildcard source such as ``sweep``): a finding never inherits
    the triage of an action recorded by a *different* reviewer.

    The 5 tiers (highest to lowest precision):
      1. ``exact``           — same path + line + severity.
      2. ``topic_path_line`` — same normalized path, line within ±3,
                                topic substring of message[:100].
      3. ``topic_path``      — same normalized path, topic substring of message.
      4. ``topic_only``      — topic-token-overlap > 0.5 (no path requirement).
      5. ``none``            — no match.

    Ties broken by: (tier, action-rank, earliest in list).
    """
    f_path = normalize_path(finding.get("file"))
    f_line = finding.get("line")
    f_sev = finding.get("severity")
    f_msg = finding.get("message") or ""

    best: Optional[tuple[int, int, int, dict, MatchStrategy]] = None

    for idx, a in enumerate(candidate_actions):
        a_path = normalize_path(a.get("path"))
        a_line = a.get("line")
        a_sev = a.get("severity")
        a_src = a.get("source")
        a_topic = a.get("topic") or ""

        strategy: MatchStrategy = "none"

        # An envelope finding from backend X may only inherit the triage of a
        # comment_action recorded by the same backend (or a cross-backend
        # wildcard source like ``sweep``). Without this gate the fuzzier
        # tiers below would let a codex finding adopt a cursor row's
        # ``action`` / ``is_real_issue``. Applies to every tier, not just the
        # exact one.
        backend_ok = a_src == backend or a_src in WILDCARD_BACKEND_SOURCES
        if not backend_ok:
            continue

        # Tier 1 — exact
        if (
            a_path
            and f_path
            and a_path == f_path
            and a_line is not None
            and f_line is not None
            and int(a_line) == int(f_line)
            and a_sev
            and f_sev
            and a_sev == f_sev
        ):
            strategy = "exact"
        elif (
            a_path
            and f_path
            and a_path == f_path
            and a_line is not None
            and f_line is not None
            and abs(int(a_line) - int(f_line)) <= 3
            and _topic_substring(a_topic, f_msg, limit=100)
        ):
            strategy = "topic_path_line"
        elif (
            a_path
            and f_path
            and a_path == f_path
            and _topic_substring(a_topic, f_msg)
        ):
            strategy = "topic_path"
        elif topic_token_overlap(a_topic, f_msg) > 0.5:
            strategy = "topic_only"

        if strategy == "none":
            continue

        tier_rank = _TIER_RANK[strategy]
        action_rank = _ACTION_RANK.get(a.get("action") or "", -10)
        # Negate idx so earlier wins on ties (higher tuple is better).
        key = (tier_rank, action_rank, -idx)
        cur = (tier_rank, action_rank, -idx, a, strategy)
        if best is None or key > best[:3]:
            best = cur

    if best is None:
        return None, "none"
    _, _, _, action, strategy = best
    return action, strategy


def derive_is_real_issue(action: Optional[str]) -> Optional[bool]:
    """Coarse true/false/unknown signal derived from raw action verbatim."""
    if action in {"fixed", "wont_fix"}:
        return True
    if action in {"false_positive", "stale"}:
        return False
    return None  # ack, flake, pre_existing, None → insufficient signal


# ---------------------------------------------------------------------------
# Git helpers
# ---------------------------------------------------------------------------


# Where local repo checkouts live on this machine. ~/worktrees/<name>/main
# is the Conductor-worktree convention; ~/g/<name> the plain-clone one.
_CHECKOUT_GLOBS = ("worktrees/*/main", "g/*")


def discover_repo_roots() -> dict[str, Path]:
    """Find local repo checkouts, keyed by repo name.

    Walks the known checkout locations (``~/worktrees/<name>/main`` and
    ``~/g/<name>``) and keys each git checkout by its repo name — the same
    name that appears in a dataset's ``pr.repo_name`` and in the
    ``~/.bramble/projects/<repo>-<pr>/`` dir names. The ``~/worktrees`` form
    wins on a name collision (it is the active-development checkout).

    Replaces the old ``--repos-root NAME=PATH`` flags: a caller resolves
    repo names against this map instead of being told the paths.
    """
    home = Path.home()
    roots: dict[str, Path] = {}
    for pattern in _CHECKOUT_GLOBS:
        for path in sorted(home.glob(pattern)):
            if not path.is_dir() or not (path / ".git").exists():
                continue
            # ~/worktrees/<name>/main -> name; ~/g/<name> -> name.
            name = (
                path.parent.name
                if path.name == "main"
                else path.name
            )
            # First match wins; _CHECKOUT_GLOBS lists ~/worktrees first.
            roots.setdefault(name, path)
    return roots


@dataclass
class RepoMap:
    """Maps repo name (kernel/yoloswe/nebula) → local checkout path."""

    mapping: dict[str, Path] = field(default_factory=dict)

    @classmethod
    def discover(cls, overrides: Iterable[str] = ()) -> "RepoMap":
        """Auto-discover repo checkouts; ``overrides`` patch specific names.

        The default constructor — no ``--repos-root`` needed. ``overrides``
        is an optional list of ``NAME=PATH`` for a repo checked out somewhere
        :func:`discover_repo_roots` does not look.
        """
        mapping = dict(discover_repo_roots())
        mapping.update(cls.from_flags(overrides).mapping)
        return cls(mapping=mapping)

    @classmethod
    def from_flags(cls, flags: Iterable[str]) -> "RepoMap":
        mapping: dict[str, Path] = {}
        for f in flags:
            if "=" not in f:
                raise ValueError(
                    f"--repo-root expects NAME=PATH, got: {f!r}"
                )
            name, path_s = f.split("=", 1)
            mapping[name.strip()] = Path(path_s.strip()).expanduser()
        return cls(mapping=mapping)

    def lookup(self, repo_name: str) -> Optional[Path]:
        return self.mapping.get(repo_name)


def git(repo_path: Path, *args: str) -> subprocess.CompletedProcess:
    """Run ``git -C <repo_path> <args>``, capturing output, never raising."""
    return subprocess.run(
        ["git", "-C", str(repo_path), *args],
        capture_output=True,
        text=True,
        check=False,
    )


def normalize_remote_url(url: str) -> str:
    """Normalize SSH and HTTPS git remotes to canonical https://host/org/repo."""
    s = url.strip()
    # Strip trailing .git
    if s.endswith(".git"):
        s = s[: -len(".git")]
    # Strip auth user
    if s.startswith("git@"):
        # git@github.com:org/repo
        rest = s[len("git@") :]
        if ":" in rest:
            host, path = rest.split(":", 1)
            return f"https://{host}/{path}"
    if s.startswith("ssh://git@"):
        rest = s[len("ssh://git@") :]
        return f"https://{rest}"
    if s.startswith("https://"):
        return s
    if s.startswith("http://"):
        return "https://" + s[len("http://") :]
    return s


def get_repo_url(repo_path: Optional[Path]) -> Optional[str]:
    """Return the normalized origin URL of the local repo, or None."""
    if repo_path is None or not repo_path.exists():
        return None
    res = git(repo_path, "config", "--get", "remote.origin.url")
    if res.returncode != 0:
        return None
    url = res.stdout.strip()
    if not url:
        return None
    return normalize_remote_url(url)


def compute_merge_base(
    repo_path: Optional[Path],
    head_sha: str,
    base_branch: str = "origin/main",
) -> tuple[Optional[str], bool, Optional[str]]:
    """Return (merge_base_sha, resolved, error_message)."""
    if repo_path is None:
        return None, False, "no repo mapping"
    if not repo_path.exists():
        return None, False, f"repo path does not exist: {repo_path}"
    # Verify head_sha is present
    res = git(repo_path, "rev-parse", "--verify", f"{head_sha}^{{commit}}")
    if res.returncode != 0:
        return None, False, "head commit not in local repo"
    res = git(repo_path, "merge-base", base_branch, head_sha)
    if res.returncode != 0:
        return None, False, (res.stderr.strip() or "merge-base failed")
    sha = res.stdout.strip()
    return (sha or None), bool(sha), None if sha else "merge-base returned empty"


def compute_files_changed(
    repo_path: Optional[Path],
    base_sha: str,
    head_sha: str,
) -> tuple[list[str], Optional[str]]:
    """List of repo-relative paths changed between two commits."""
    if repo_path is None or not repo_path.exists():
        return [], "no repo mapping"
    res = git(repo_path, "diff", "--name-only", f"{base_sha}..{head_sha}")
    if res.returncode != 0:
        return [], (res.stderr.strip() or "git diff failed")
    return [ln.strip() for ln in res.stdout.splitlines() if ln.strip()], None


def already_harvested(out_dir: Path) -> set[str]:
    """``{"kernel-3945", ...}`` for every record already in the dataset.

    The dataset dir is the cache: a written record is the memoized result,
    so an incremental re-harvest is a set-difference rather than a second
    caching layer with its own invalidation story.
    """
    out: set[str] = set()
    if not out_dir.is_dir():
        return out
    for path in out_dir.glob("*.json"):
        if path.name != "index.json":
            out.add(path.stem)
    return out


def discover_github_prs(
    slug: str,
    *,
    merged_since: Optional[str] = None,
    limit: int = 100,
    exclude: Optional[set[str]] = None,
) -> tuple[list[dict], Optional[str]]:
    """Merged PRs from GitHub as candidate harvest rows.

    This is the second discovery source alongside
    :func:`discover_project_dirs`: it reaches PRs that were never polished
    locally, which is the difference between harvesting ~3% of a busy repo
    and harvesting most of it.

    Returns ``(rows, error)``; a failure yields ``([], message)`` so the
    caller degrades to local-only discovery rather than crashing.

    Note ``baseRefOid`` is the base branch tip *now*, not the PR's merge
    base — :func:`resolve_diff_scope` still calls the compare API to get
    the real merge base.
    """
    repo_name = slug.rsplit("/", 1)[-1]
    cmd = [
        "gh", "pr", "list", "-R", slug,
        "--state", "merged",
        "--limit", str(limit),
        "--json", "number,mergedAt,headRefOid,baseRefOid,title,headRefName",
    ]
    if merged_since:
        cmd += ["--search", f"merged:>={merged_since}"]
    try:
        res = subprocess.run(
            cmd, capture_output=True, text=True, check=False, timeout=60
        )
    except (FileNotFoundError, subprocess.TimeoutExpired) as e:
        return [], f"gh pr list failed: {e}"
    if res.returncode != 0:
        return [], f"gh pr list exit {res.returncode}: {res.stderr.strip()}"
    try:
        rows = json.loads(res.stdout or "[]")
    except json.JSONDecodeError as e:
        return [], f"gh pr list JSON parse error: {e}"
    if not isinstance(rows, list):
        return [], "gh pr list did not return a list"

    exclude = exclude or set()
    out: list[dict] = []
    for r in rows:
        number = r.get("number")
        if number is None:
            continue
        pr_number = str(number)
        if f"{repo_name}-{pr_number}" in exclude:
            continue
        out.append(
            {
                "repo_name": repo_name,
                "pr_number": pr_number,
                "slug": slug,
                "head_sha": r.get("headRefOid"),
                "base_sha": r.get("baseRefOid"),
                "head_ref": r.get("headRefName"),
                "merged_at": r.get("mergedAt"),
                "title": r.get("title"),
            }
        )
    return out, None


def repo_slug_from_url(repo_url: Optional[str]) -> Optional[str]:
    """Extract ``org/repo`` from a normalized https GitHub URL, or None."""
    if not repo_url or "github.com/" not in repo_url:
        return None
    return repo_url.split("github.com/", 1)[1] or None


def gh_merge_base(
    slug: str, base_sha: str, head_sha: str
) -> tuple[Optional[str], Optional[str]]:
    """Merge base via the compare API. Returns ``(sha, error)``."""
    obj, err = _gh_api_object(slug, f"compare/{base_sha}...{head_sha}")
    if obj is None:
        return None, err
    sha = ((obj.get("merge_base_commit") or {}).get("sha")) or None
    return sha, None if sha else "compare returned no merge_base_commit"


def gh_files_changed(
    slug: str, pr_number: str
) -> tuple[list[str], Optional[str]]:
    """Changed paths via the PR files API. Returns ``(paths, error)``."""
    rows, err = _gh_api(slug, f"pulls/{pr_number}/files?per_page=100")
    if err:
        return [], err
    return [r["filename"] for r in rows if r.get("filename")], None


@dataclass
class DiffScope:
    """One round's resolved diff scope, and which resolver produced it."""

    head_before: Optional[str]
    merge_base_sha: Optional[str]
    merge_base_resolved: bool
    merge_base_error: Optional[str]
    files_changed: list[str]
    merge_base_resolved_by: ScopeResolver = "git"
    files_changed_resolved_by: ScopeResolver = "git"


def resolve_diff_scope(
    round_data: dict,
    *,
    repo_path: Optional[Path],
    base_branch: str = "origin/main",
    gh: Optional[dict] = None,
    slug: Optional[str] = None,
    pr_number: Optional[str] = None,
) -> DiffScope:
    """Resolve a round's diff scope, local git first, GitHub as fallback.

    Local git is preferred: it is free, offline, and authoritative for a
    checkout that has the commit. The GitHub fallback covers two cases —
    a PR that was never polished locally (no state, no fetch), and a
    pr-polish record whose checkout has since gone stale ("head commit not
    in local repo"), which previously made the record unusable for
    collection.

    ``gh``/``slug``/``pr_number`` are optional; without them this is exactly
    the old local-only behavior.
    """
    head_before = round_data.get("head_before") or (
        gh.get("head_sha") if gh else None
    )
    if not head_before:
        return DiffScope(None, None, False, "no head_before", [])

    mb_sha, mb_resolved, mb_err = compute_merge_base(
        repo_path, head_before, base_branch
    )
    mb_by: ScopeResolver = "git"
    if not mb_resolved and slug and gh:
        base_sha = gh.get("base_sha")
        if base_sha:
            api_sha, api_err = gh_merge_base(slug, base_sha, head_before)
            if api_sha:
                mb_sha, mb_resolved, mb_err, mb_by = api_sha, True, None, "github"
            else:
                # Keep the git error too — it says why the local path failed,
                # which is the actionable half (e.g. "fetch this commit").
                mb_err = f"{mb_err}; github: {api_err}"

    files_changed: list[str] = []
    files_by: ScopeResolver = "git"
    files_err: Optional[str] = "merge base unresolved"
    if mb_resolved and mb_sha:
        files_changed, files_err = compute_files_changed(
            repo_path, mb_sha, head_before
        )
    # Branch on the *error*, not on emptiness: a diff with no changed files
    # is a valid answer, and treating it as failure would spend an API call
    # on every such round.
    if files_err and slug and pr_number:
        api_files, _ = gh_files_changed(slug, pr_number)
        if api_files:
            files_changed, files_by = api_files, "github"

    return DiffScope(
        head_before=head_before,
        merge_base_sha=mb_sha,
        merge_base_resolved=mb_resolved,
        merge_base_error=mb_err,
        files_changed=files_changed,
        merge_base_resolved_by=mb_by,
        files_changed_resolved_by=files_by,
    )


def harvester_git_sha(repo_path: Path) -> str:
    """SHA of the harvester repo at run time (yoloswe). Best-effort."""
    res = git(repo_path, "rev-parse", "HEAD")
    if res.returncode != 0:
        return ""
    return res.stdout.strip()


def _to_utc_epoch(ts: Optional[str]) -> Optional[float]:
    """Parse an ISO8601 timestamp (any tz offset, or trailing ``Z``) to a UTC
    epoch float. Returns None when ``ts`` is falsy or unparseable.

    Both GitHub ``created_at`` (UTC ``Z``) and git ``%cI`` (committer-local
    offset) reach here, so callers can order them chronologically instead of
    string-comparing two different timezone representations.
    """
    if not ts:
        return None
    s = ts.strip()
    if s.endswith("Z"):
        s = s[:-1] + "+00:00"
    try:
        dt = _dt.datetime.fromisoformat(s)
    except ValueError:
        return None
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=_dt.timezone.utc)
    return dt.timestamp()


def git_commit_time(repo_path: Optional[Path], sha: str) -> Optional[str]:
    """Committer date of ``sha`` as a strict-ISO8601 string, or None.

    Used to derive per-round time boundaries: pr-polish stores each round's
    ``head_before``/``head_after`` SHAs but no round timestamps, so the only
    way to bucket a PR comment's ``created_at`` into a round is to resolve the
    round-boundary commits to their commit times.

    ``%cI`` carries the committer's local timezone offset, *not* UTC — so
    consumers must order these via :func:`_to_utc_epoch`, never raw string
    comparison against a UTC ``Z`` timestamp.
    """
    if repo_path is None or not repo_path.exists() or not sha:
        return None
    res = git(repo_path, "show", "-s", "--format=%cI", f"{sha}^{{commit}}")
    if res.returncode != 0:
        return None
    out = res.stdout.strip()
    return out or None


# ---------------------------------------------------------------------------
# PR-comment fetch + verdict join + round attribution
# ---------------------------------------------------------------------------


RATE_LIMIT_MARKER = "rate limit exceeded"
# Bulk harvests reliably trip GitHub's *secondary* rate limit, which fires
# while the core quota still shows thousands remaining. Retrying immediately
# just burns the rest of the run: every subsequent PR fails the same way and
# silently degrades to the state-recorded comment set.
_RATE_LIMIT_BACKOFF_S = (30, 90, 240)


def is_rate_limit_error(msg: Optional[str]) -> bool:
    """True when a gh error message is a rate-limit refusal (primary or secondary)."""
    return bool(msg) and RATE_LIMIT_MARKER in msg.lower()


def _run_gh(
    args: list[str], endpoint: str, *, sleep=time.sleep
) -> tuple[Optional[subprocess.CompletedProcess], Optional[str]]:
    """Run a ``gh`` command, backing off through a rate limit.

    Returns ``(completed_process, error)``. A rate-limit refusal is retried
    on the fixed backoff schedule; anything else returns immediately. When
    the retries are exhausted the error is still returned, so callers keep
    their existing degrade-or-fail behavior.
    """
    last_err: Optional[str] = None
    for attempt in range(len(_RATE_LIMIT_BACKOFF_S) + 1):
        try:
            res = subprocess.run(
                args, capture_output=True, text=True, check=False, timeout=30
            )
        except (FileNotFoundError, subprocess.TimeoutExpired) as e:
            return None, f"gh api {endpoint} failed: {e}"
        if res.returncode == 0:
            return res, None
        last_err = (
            f"gh api {endpoint} exit {res.returncode}: {res.stderr.strip()}"
        )
        if not is_rate_limit_error(res.stderr):
            return res, last_err
        if attempt < len(_RATE_LIMIT_BACKOFF_S):
            delay = _RATE_LIMIT_BACKOFF_S[attempt]
            print(
                f"   rate limited on {endpoint}; sleeping {delay}s "
                f"(attempt {attempt + 1}/{len(_RATE_LIMIT_BACKOFF_S)})",
                file=sys.stderr,
            )
            sleep(delay)
    return None, last_err


def _gh_api(slug: str, endpoint: str) -> tuple[list[dict], Optional[str]]:
    """Run ``gh api --paginate repos/<slug>/<endpoint>``; best-effort.

    Returns ``(rows, error)``. Any failure (gh missing, network, non-zero
    exit, bad JSON) returns ``([], <message>)`` — the harvester degrades to
    the state-recorded comment set rather than crashing.
    """
    res, err = _run_gh(
        ["gh", "api", "--paginate", f"repos/{slug}/{endpoint}"], endpoint
    )
    if err:
        return [], err
    out = res.stdout.strip()
    if not out:
        return [], None
    try:
        obj = json.loads(out)
    except json.JSONDecodeError as e:
        return [], f"gh api {endpoint} JSON parse error: {e}"
    if not isinstance(obj, list):
        return [], f"gh api {endpoint} did not return a list"
    return obj, None


def _gh_api_object(slug: str, endpoint: str) -> tuple[Optional[dict], Optional[str]]:
    """``gh api repos/<slug>/<endpoint>`` for an endpoint returning an object.

    Separate from :func:`_gh_api` because that one paginates and demands a
    list; ``compare/{base}...{head}`` returns a single object. Same
    best-effort contract: any failure returns ``(None, <message>)``.
    """
    res, err = _run_gh(["gh", "api", f"repos/{slug}/{endpoint}"], endpoint)
    if err:
        return None, err
    out = res.stdout.strip()
    if not out:
        return None, f"gh api {endpoint} returned nothing"
    try:
        obj = json.loads(out)
    except json.JSONDecodeError as e:
        return None, f"gh api {endpoint} JSON parse error: {e}"
    if not isinstance(obj, dict):
        return None, f"gh api {endpoint} did not return an object"
    return obj, None


def fetch_pr_comments(
    slug: str, pr_number: str
) -> tuple[list[dict], Optional[str]]:
    """Fetch all PR comments (inline + issue + review) from GitHub.

    Mirrors the read path of pr-polish's ``pr_ops.fetch-comments``, but
    standalone — pr-polish is not importable as a package, and the harvester
    only needs the fetch, not the noise-filtering triage (the eval dataset is
    a *complete census*: dropping bot-summary noise would discard reference
    data the judge wants to see).

    Structural drops only: inline replies (the parent carries the signal) and
    empty / APPROVED / DISMISSED reviews. Returns ``(comments, error)`` where
    each row is ``{id, source, author, is_bot, path, line, body, created_at,
    original_commit_id}``. ``error`` is non-None when any endpoint failed; the
    partial result (if any) is still returned.
    """
    if not slug or not pr_number:
        return [], "missing repo slug or pr number"

    errors: list[str] = []
    inline, err = _gh_api(slug, f"pulls/{pr_number}/comments")
    if err:
        errors.append(err)
    issues, err = _gh_api(slug, f"issues/{pr_number}/comments")
    if err:
        errors.append(err)
    reviews, err = _gh_api(slug, f"pulls/{pr_number}/reviews")
    if err:
        errors.append(err)

    comments: list[dict] = []

    for c in inline:
        if c.get("in_reply_to_id"):
            continue  # reply; the parent comment is the one we keep
        user = c.get("user") or {}
        comments.append(
            {
                "id": c.get("id"),
                "source": "github-inline",
                "author": user.get("login"),
                "is_bot": user.get("type") == "Bot",
                "path": c.get("path"),
                "line": c.get("line"),
                "body": c.get("body") or "",
                "created_at": c.get("created_at"),
                "original_commit_id": c.get("original_commit_id"),
            }
        )

    for c in issues:
        user = c.get("user") or {}
        comments.append(
            {
                "id": c.get("id"),
                "source": "github-issue",
                "author": user.get("login"),
                "is_bot": user.get("type") == "Bot",
                "path": None,
                "line": None,
                "body": c.get("body") or "",
                "created_at": c.get("created_at"),
                "original_commit_id": None,
            }
        )

    for r in reviews:
        if r.get("state") in {"APPROVED", "DISMISSED"}:
            continue
        body = (r.get("body") or "").strip()
        if not body:
            continue
        user = r.get("user") or {}
        comments.append(
            {
                "id": r.get("id"),
                "source": "github-review",
                "author": user.get("login"),
                "is_bot": user.get("type") == "Bot",
                "path": None,
                "line": None,
                "body": body,
                "created_at": r.get("submitted_at"),
                "original_commit_id": None,
            }
        )

    return comments, ("; ".join(errors) if errors else None)


@dataclass
class CommentVerdictIndex:
    """Lookup tables for joining fetched PR comments to recorded verdicts.

    ``by_id`` keys on ``comment_id`` — the precise join. ``by_topic`` is the
    fallback for ``comment_actions`` rows pr-polish recorded with
    ``comment_id: null`` (older / buggy runs): it keys on
    ``(source, normalized topic)`` so a fetched comment can still be joined by
    matching that topic as a substring of its body.
    """

    by_id: dict[Any, dict] = field(default_factory=dict)
    by_topic: list[dict] = field(default_factory=list)


def index_comment_verdicts(state: dict) -> CommentVerdictIndex:
    """Index every github ``comment_action`` verdict for later joining.

    pr-polish re-triages every open PR comment on each series start, so a
    single ``comment_id`` recurs across many rounds' ``comment_actions``. The
    *first* occurrence (earliest round ``n``) is where the engineer's real
    verdict was set; later rows are re-fetched echoes. We key by ``comment_id``
    and keep the earliest, recording the round it was triaged in.

    Rows with ``comment_id: null`` (some older pr-polish runs never recorded
    the id) cannot be keyed precisely — they go into ``by_topic`` instead so
    the topic-substring fallback in :func:`build_pr_comments` can recover them.
    """
    idx = CommentVerdictIndex()
    rounds = sorted(
        (r for r in (state.get("rounds") or []) if r.get("n")),
        key=lambda r: int(r.get("n") or 0),
    )
    for r in rounds:
        n = int(r.get("n") or 0)
        for a in r.get("comment_actions") or []:
            if a.get("source") not in GITHUB_SOURCES:
                continue
            row = {
                "action": a.get("action"),
                "reason": a.get("reason"),
                "comment_actions_source": a.get("source"),
                "commit_sha": a.get("commit_sha"),
                "triaged_in_round": n,
            }
            cid = a.get("comment_id")
            if cid is not None:
                if cid not in idx.by_id:
                    idx.by_id[cid] = row
                continue
            topic = (a.get("topic") or "").strip()
            if topic:
                idx.by_topic.append({**row, "source": a.get("source"), "topic": topic})
    return idx


# Minimum topic-token containment to accept a null-id verdict join — the
# fraction of the recorded ``topic``'s tokens that appear in the comment body.
# Containment (not Jaccard) because the body is long and the topic is a short
# summary; see :func:`topic_token_containment`.
_TOPIC_JOIN_CONTAINMENT = 0.6


def _join_verdict(
    comment: dict, idx: CommentVerdictIndex, used_topic_rows: set[int]
) -> dict:
    """Look up a fetched comment's verdict.

    Tiers, highest precision first:
      1. by ``comment_id`` — the precise join.
      2. recorded ``topic`` is a case-insensitive substring of the body.
      3. topic-token containment in the body exceeds
         ``_TOPIC_JOIN_CONTAINMENT`` (the recorded topic is a summary, not a
         verbatim quote).

    ``used_topic_rows`` holds ``id()`` of by_topic rows already consumed, so a
    null-id verdict joins to at most one fetched comment. Within tier 3 the
    best (highest-containment) unused row wins.
    """
    cid = comment.get("id")
    if cid is not None and cid in idx.by_id:
        return idx.by_id[cid]

    body = comment.get("body") or ""
    src = comment.get("source")
    if not body:
        return {}
    body_lower = body.lower()

    eligible = [
        row
        for row in idx.by_topic
        if id(row) not in used_topic_rows and row.get("source") == src
    ]

    # Tier 2 — substring.
    for row in eligible:
        if row["topic"].lower() in body_lower:
            used_topic_rows.add(id(row))
            return row

    # Tier 3 — token containment; pick the best match above threshold.
    best_row: Optional[dict] = None
    best_containment = _TOPIC_JOIN_CONTAINMENT
    for row in eligible:
        c = topic_token_containment(row["topic"], body)
        if c > best_containment:
            best_containment = c
            best_row = row
    if best_row is not None:
        used_topic_rows.add(id(best_row))
        return best_row
    return {}


def _round_boundary_times(
    state: dict, repo_path: Optional[Path]
) -> tuple[list[tuple[int, Optional[str]]], bool]:
    """Per-round ``(n, head_before_commit_time)`` plus a ``times_resolved`` flag.

    ``times_resolved`` is True only when every round's ``head_before`` resolved
    to a commit time — otherwise comment attribution must fall back.
    """
    rounds = sorted(
        (r for r in (state.get("rounds") or []) if r.get("n")),
        key=lambda r: int(r.get("n") or 0),
    )
    out: list[tuple[int, Optional[str]]] = []
    all_resolved = bool(rounds)
    for r in rounds:
        n = int(r.get("n") or 0)
        t = git_commit_time(repo_path, r.get("head_before") or "")
        if t is None:
            all_resolved = False
        out.append((n, t))
    return out, all_resolved


def attribute_comment_to_round(
    created_at: Optional[str],
    round_times: list[tuple[int, Optional[str]]],
) -> Optional[int]:
    """Round ``n`` whose ``[head_before(n), head_before(n+1))`` window holds
    ``created_at``.

    A comment created before round 1's boundary attributes to round 1; one at
    or after the last round's boundary attributes to the last round. Rounds
    whose boundary time is None are skipped for window edges but still eligible
    as the final fallback. Returns None only when ``round_times`` is empty.
    """
    usable = [(n, e) for (n, t) in round_times if (e := _to_utc_epoch(t))]
    if not round_times:
        return None
    if not usable:
        # No boundary times at all — attribute everything to the last round.
        return round_times[-1][0]
    created_epoch = _to_utc_epoch(created_at)
    if created_epoch is None:
        return usable[-1][0]
    chosen = usable[0][0]
    for n, e in usable:
        if created_epoch >= e:
            chosen = n
        else:
            break
    return chosen


def build_pr_comments(
    state: dict,
    fetched: list[dict],
    repo_path: Optional[Path],
    *,
    fetch_attempted: bool,
) -> tuple[list[dict], AttributionBasis]:
    """Join fetched PR comments to their verdicts and attribute each to a round.

    Returns ``(comments, attribution_basis)``. Each comment row carries the
    fetched fields plus ``action`` / ``reason`` / ``comment_actions_source``
    (joined by ``comment_id``; null when GitHub returned a comment pr-polish
    never triaged) and ``attributed_round``.

    ``attribution_basis``:
      * ``created_at``            — round boundary commit times resolved; the
                                     comment's ``created_at`` was bucketed.
      * ``unmapped_repo_fallback``— repo not mapped / SHAs unresolvable; every
                                     comment attributes to round 1.
      * ``no_timestamp``          — gh fetch was skipped or wholly failed; the
                                     state-recorded github comment_actions are
                                     used instead, attributed to their round.
    """
    idx = index_comment_verdicts(state)

    if not fetch_attempted or not fetched:
        # No fresh fetch — reconstruct from the state-recorded comment_actions.
        # Each is attributed to the round it was first triaged in. Both keyed
        # and null-id (by_topic) verdict rows are emitted so nothing is lost.
        rows: list[dict] = []
        for cid, v in idx.by_id.items():
            rows.append(
                {
                    "comment_id": cid,
                    "source": v["comment_actions_source"],
                    "author": None,
                    "is_bot": None,
                    "path": None,
                    "line": None,
                    "body": None,
                    "created_at": None,
                    "original_commit_id": None,
                    "action": v["action"],
                    "reason": v["reason"],
                    "comment_actions_source": v["comment_actions_source"],
                    "attributed_round": v["triaged_in_round"],
                }
            )
        for v in idx.by_topic:
            rows.append(
                {
                    "comment_id": None,
                    "source": v["source"],
                    "author": None,
                    "is_bot": None,
                    "path": None,
                    "line": None,
                    "body": None,
                    "created_at": None,
                    "original_commit_id": None,
                    "action": v["action"],
                    "reason": v["reason"],
                    "comment_actions_source": v["comment_actions_source"],
                    "attributed_round": v["triaged_in_round"],
                }
            )
        return rows, "no_timestamp"

    round_times, times_resolved = _round_boundary_times(state, repo_path)
    basis: AttributionBasis = "created_at" if times_resolved else "unmapped_repo_fallback"

    used_topic_rows: set[int] = set()
    rows = []
    for c in fetched:
        v = _join_verdict(c, idx, used_topic_rows)
        if times_resolved:
            attributed = attribute_comment_to_round(c.get("created_at"), round_times)
        else:
            attributed = round_times[0][0] if round_times else None
        rows.append(
            {
                "comment_id": c.get("id"),
                "source": c.get("source"),
                "author": c.get("author"),
                "is_bot": c.get("is_bot"),
                "path": c.get("path"),
                "line": c.get("line"),
                "body": c.get("body"),
                "created_at": c.get("created_at"),
                "original_commit_id": c.get("original_commit_id"),
                "action": v.get("action"),
                "reason": v.get("reason"),
                "comment_actions_source": v.get("comment_actions_source"),
                "attributed_round": attributed,
            }
        )
    return rows, basis


def fold_comment_to_harvested_round(
    attributed_round: Optional[int], harvested_round_ns: list[int]
) -> Optional[int]:
    """Pick which *harvested* round a PR comment is emitted on.

    The harvester only emits R1 + final, so a comment attributed to a middle
    round must fold onto the nearest harvested round without crossing forward:
    ``attributed_round <= r1.n`` -> r1, else -> the final harvested round.
    Returns None only when no rounds were harvested.
    """
    if not harvested_round_ns:
        return None
    ns = sorted(harvested_round_ns)
    if attributed_round is None:
        return ns[-1]
    first = ns[0]
    if attributed_round <= first:
        return first
    return ns[-1]


# ---------------------------------------------------------------------------
# Goal-text reconstruction
# ---------------------------------------------------------------------------


@functools.lru_cache(maxsize=None)
def _load_bramble_ops(bramble_ops_path: Path):
    """Dynamic import of pr-polish's bramble_ops module.

    pr-polish isn't a package, so we import it by file path. Side effect:
    it inserts its own directory onto ``sys.path`` (to resolve ``_common``).
    Cached per path — the module is identical across every PR in a run, so
    re-exec'ing it each round is wasted work.
    """
    spec = importlib.util.spec_from_file_location(
        "_bramble_ops_for_harvester", bramble_ops_path
    )
    if spec is None or spec.loader is None:
        raise ImportError(f"cannot load spec for {bramble_ops_path}")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@contextmanager
def _chdir(path: Path):
    prev = Path.cwd()
    os.chdir(path)
    try:
        yield
    finally:
        os.chdir(prev)


def reconstruct_goal_text(
    state: dict,
    round_n: int,
    head_before: Optional[str],
    pr_summary: Optional[str],
    *,
    bramble_ops_path: Path,
    repo_path: Optional[Path],
) -> tuple[Optional[str], bool]:
    """Return (goal_text, goal_recoverable).

    R1 returns ``pr_summary`` verbatim — unrecoverable if pr_summary is None.
    R2+ calls ``bramble_ops.goal_for_round`` which is deterministic given
    state. ``repo_path`` is the cwd for the git subprocess calls
    ``goal_for_round`` makes internally; if missing, we still try (with the
    process's existing cwd) and treat any failure as unrecoverable.
    """
    if round_n < 2:
        if pr_summary:
            return pr_summary, True
        return None, False
    try:
        mod = _load_bramble_ops(bramble_ops_path)
    except Exception:
        return None, False
    try:
        if repo_path is not None and repo_path.exists():
            with _chdir(repo_path):
                text = mod.goal_for_round(
                    round_n,
                    pr_summary or "",
                    state,
                    head_before=head_before,
                )
        else:
            text = mod.goal_for_round(
                round_n,
                pr_summary or "",
                state,
                head_before=head_before,
            )
    except Exception:
        return None, False
    if not text:
        return None, False
    return text, True


# ---------------------------------------------------------------------------
# Per-round assembly
# ---------------------------------------------------------------------------


def _attempt_dirs(round_dir: Path) -> list[Path]:
    """Attempt subdirs (``a1``, ``a2``, …) newest last, or [] if flat.

    pr-polish used to write envelopes straight into ``rN/``; it now writes
    them into a per-attempt ``rN/aM/``. Both layouts are still on disk, so
    the harvester has to read either one.
    """
    if not round_dir.is_dir():
        return []
    attempts = []
    for child in round_dir.iterdir():
        if not child.is_dir():
            continue
        m = re.fullmatch(r"a(\d+)", child.name)
        if m:
            attempts.append((int(m.group(1)), child))
    return [d for _, d in sorted(attempts)]


def _envelope_path(state_dir: Path, round_n: int, backend: str) -> Path:
    """Envelope for a backend, preferring the last attempt that has one.

    A retried round leaves an envelope in each attempt it got that far; the
    last one is the outcome pr-polish acted on, so that is the one the
    dataset should carry.
    """
    round_dir = state_dir / f"r{round_n}"
    name = f"{backend}-envelope.json"
    for attempt_dir in reversed(_attempt_dirs(round_dir)):
        candidate = attempt_dir / name
        if candidate.exists():
            return candidate
    return round_dir / name


def _scope_hints_present(state_dir: Path, round_n: int) -> bool:
    # pr-polish writes scope-hints.json into the round dir when scope
    # exploration produced hints. Absence = single-package PR. It stays at
    # the round level in both the flat and per-attempt layouts.
    return (state_dir / f"r{round_n}" / "scope-hints.json").exists()


def _build_finding(raw: dict, backend: str, candidates: list[dict]) -> Finding:
    action, strategy = match_finding_to_action(raw, backend, candidates)
    if action is None:
        gt = GroundTruth(
            matched_comment_action=False,
            match_strategy="none",
            action=None,
            reason=None,
            is_real_issue=None,
            fixed_in_commit=None,
            comment_actions_source=None,
        )
    else:
        gt = GroundTruth(
            matched_comment_action=True,
            match_strategy=strategy,
            action=action.get("action"),
            reason=action.get("reason"),
            is_real_issue=derive_is_real_issue(action.get("action")),
            fixed_in_commit=action.get("commit_sha"),
            comment_actions_source=action.get("source"),
        )
    return Finding(
        severity=raw.get("severity"),
        message=raw.get("message") or "",
        suggestion=raw.get("suggestion"),
        file=raw.get("file"),
        line=raw.get("line"),
        confidence=raw.get("confidence"),
        invariant=raw.get("invariant"),
        sites=raw.get("sites"),
        ground_truth=gt,
    )


def _build_review_run(
    state_dir: Path,
    round_n: int,
    backend: str,
    candidates: list[dict],
) -> Optional[ReviewRun]:
    """Returns None when the envelope is absent (skip silently)."""
    env_path = _envelope_path(state_dir, round_n, backend)
    env, err = parse_envelope(env_path)
    if env is None:
        # Older PRs don't have gemini envelopes; we treat missing envelopes
        # as "this backend didn't run for this round" and drop them rather
        # than littering the dataset with empty placeholders.
        return None

    env_status: EnvelopeStatus
    raw_status = env.get("status")
    if raw_status == "ok":
        env_status = "ok"
    elif raw_status == "error":
        env_status = "error"
    else:
        env_status = "ok" if env.get("review") else "error"

    review = env.get("review") or {}

    findings: list[Finding] = []
    if env_status == "ok":
        for raw in envelope_issues(env):
            findings.append(_build_finding(raw, backend, candidates))

    return ReviewRun(
        backend=backend,
        model=env.get("model"),
        session_id=env.get("session_id"),
        review_mode=env.get("review_mode"),
        resume_status=env.get("resume_status") or None,
        envelope_status=env_status,
        envelope_error=env.get("error") if env_status == "error" else None,
        verdict=review.get("verdict"),
        summary=review.get("summary"),
        duration_ms=env.get("duration_ms"),
        input_tokens=env.get("input_tokens"),
        output_tokens=env.get("output_tokens"),
        schema_version=env.get("schema_version"),
        findings=findings,
    )


def build_harvested_round(
    state: dict,
    state_dir: Optional[Path],
    round_n: int,
    signal_tier: SignalTier,
    *,
    repo_path: Optional[Path],
    pr_summary: Optional[str],
    bramble_ops_path: Path,
    pr_comments_for_round: list[dict],
    base_branch: str = "origin/main",
    gh: Optional[dict] = None,
    slug: Optional[str] = None,
    pr_number: Optional[str] = None,
) -> HarvestedRound:
    """Assemble one harvested round.

    ``pr_comments_for_round`` is the subset of the PR-global, verdict-joined,
    round-attributed github comments (see ``build_pr_comments``) that fold onto
    this harvested round. They replace the github-* rows that used to be copied
    verbatim from ``comment_actions`` — non-github sources still come straight
    from this round's ``comment_actions``.

    ``state_dir`` is None for a GitHub-sourced record (no local pr-polish
    directory exists). Rather than branching every read, it is coerced to a
    path that cannot exist: the envelope and scope-hint lookups then take
    their already-tested "missing" paths and yield ``review_runs=[]`` /
    ``scope_hints_present=False``.
    """
    if state_dir is None:
        state_dir = _NO_STATE_DIR
    round_data = get_round(state, round_n) or {}
    head_before = round_data.get("head_before")
    head_after = round_data.get("head_after")

    scope = resolve_diff_scope(
        round_data,
        repo_path=repo_path,
        base_branch=base_branch,
        gh=gh,
        slug=slug,
        pr_number=pr_number,
    )
    head_before = scope.head_before

    goal_text, goal_recoverable = reconstruct_goal_text(
        state,
        round_n,
        head_before,
        pr_summary,
        bramble_ops_path=bramble_ops_path,
        repo_path=repo_path,
    )

    candidates = _candidate_actions(round_data)

    review_runs: list[ReviewRun] = []
    for backend in BACKENDS:
        run_ = _build_review_run(state_dir, round_n, backend, candidates)
        if run_ is not None:
            review_runs.append(run_)

    # Non-github comment_actions stay keyed to the round they were recorded in;
    # github comments are PR-global and folded in by the caller.
    non_github = [
        a
        for a in (round_data.get("comment_actions") or [])
        if a.get("source") not in GITHUB_SOURCES
    ]

    return HarvestedRound(
        round=round_n,
        signal_tier=signal_tier,
        head_before=head_before,
        head_after=head_after,
        base_branch=base_branch,
        merge_base_sha=scope.merge_base_sha,
        merge_base_resolved=scope.merge_base_resolved,
        merge_base_error=scope.merge_base_error,
        files_changed=scope.files_changed,
        goal_text=goal_text,
        goal_recoverable=goal_recoverable,
        scope_hints_present=_scope_hints_present(state_dir, round_n),
        raw_comment_actions=non_github + list(pr_comments_for_round),
        review_runs=review_runs,
        merge_base_resolved_by=scope.merge_base_resolved_by,
        files_changed_resolved_by=scope.files_changed_resolved_by,
    )


# ---------------------------------------------------------------------------
# Top-level builder
# ---------------------------------------------------------------------------


def build_pr_record(
    state_dir: Optional[Path],
    repo_name: str,
    pr_number: str,
    *,
    repo_map: RepoMap,
    pr_summary: Optional[str],
    harvester_sha: str,
    harvested_at: str,
    bramble_ops_path: Path,
    include_incomplete: bool = True,
    fetched_pr_comments: Optional[list[dict]] = None,
    pr_comments_fetch_error: Optional[str] = None,
    fetch_attempted: bool = True,
    harvest_source: HarvestSource = DEFAULT_HARVEST_SOURCE,
    gh: Optional[dict] = None,
    commit_scoped: bool = False,
) -> Optional[PRRecord]:
    """Build a per-PR record. Returns None if the PR should be skipped.

    ``fetched_pr_comments`` is the result of ``fetch_pr_comments`` (or None
    when the caller skipped the fetch). PR comments are PR-global: they are
    verdict-joined + round-attributed once here, then folded onto the harvested
    rounds. When the fetch was skipped or failed, the github comments recorded
    in the state's ``comment_actions`` are used as a degraded fallback.

    ``harvest_source="github"`` builds a record for a PR that was never
    polished locally: ``gh`` (a :func:`discover_github_prs` row) stands in
    for the state file via :func:`github_only_state`, and ``state_dir`` may
    be None. The resulting record has ``review_runs=[]`` — there are no
    local envelopes — but the same diff scope, goal, and comment census.
    """
    # Resolved before state synthesis: per-commit scoping needs the checkout
    # to order reviewed SHAs by commit time and to spot unreachable ones.
    repo_path = repo_map.lookup(repo_name)

    if harvest_source == "github":
        if gh is None:
            raise ValueError("harvest_source='github' requires gh")
        inline = [
            c for c in (fetched_pr_comments or [])
            if c.get("source") == "github-inline" and c.get("original_commit_id")
        ] if commit_scoped else []
        state = github_only_state(gh, inline, repo_path=repo_path)
    else:
        state = parse_state_file(state_dir / "pr-polish-state.json")

    completed = bool(state.get("completed"))
    if not completed and not include_incomplete:
        return None

    rounds_to_harvest = select_rounds_to_harvest(state)
    if not rounds_to_harvest:
        return None

    repo_url = get_repo_url(repo_path)
    # A GitHub-sourced PR may have no local checkout at all, so fall back to
    # the slug discovery already gave us rather than losing the URLs.
    if not repo_url and gh and gh.get("slug"):
        repo_url = f"https://github.com/{gh['slug']}"
    pr_url = f"{repo_url}/pull/{pr_number}" if repo_url else None
    slug = repo_slug_from_url(repo_url)

    pr_comments, attribution_basis = build_pr_comments(
        state,
        fetched_pr_comments or [],
        repo_path,
        fetch_attempted=fetch_attempted,
    )

    harvested_ns = [n for n, _ in rounds_to_harvest]
    comments_by_harvested_round: dict[int, list[dict]] = {n: [] for n in harvested_ns}
    for c in pr_comments:
        target = fold_comment_to_harvested_round(
            c.get("attributed_round"), harvested_ns
        )
        if target is not None:
            comments_by_harvested_round[target].append(c)

    harvested_rounds = [
        build_harvested_round(
            state,
            state_dir,
            n,
            tier,
            repo_path=repo_path,
            pr_summary=pr_summary,
            bramble_ops_path=bramble_ops_path,
            pr_comments_for_round=comments_by_harvested_round.get(n, []),
            gh=gh,
            slug=slug,
            pr_number=pr_number,
        )
        for n, tier in rounds_to_harvest
    ]

    return PRRecord(
        schema_version=SCHEMA_VERSION,
        harvested_at=harvested_at,
        harvester_git_sha=harvester_sha,
        pr={
            "repo_name": repo_name,
            "repo_url": repo_url,
            "pr_number": pr_number,
            "pr_url": pr_url,
            "branch": state.get("branch"),
            "started_at": state.get("started_at"),
            "completed": completed,
            "exit_reason": state.get("exit_reason"),
            "total_rounds": len(state.get("rounds") or []),
        },
        pr_comments_attribution_basis=attribution_basis,
        pr_comments_fetch_error=pr_comments_fetch_error,
        harvested_rounds=harvested_rounds,
        harvest_source=harvest_source,
    )


# ---------------------------------------------------------------------------
# Output writing
# ---------------------------------------------------------------------------


def record_to_dict(record: PRRecord) -> dict:
    return asdict(record)


def write_pr_record(out_dir: Path, record: PRRecord) -> Path:
    """Atomic write of <repo>-<pr>.json. Returns the final path.

    Preserves an existing ``ground_truth_v3`` block: re-harvesting a PR that
    was already collected refreshes the harvested fields but keeps the
    judged ground truth intact (the harvester cannot regenerate it — it is
    expensive judged data written by collection mode after harvest).
    """
    final = out_dir / f"{record.pr['repo_name']}-{record.pr['pr_number']}.json"
    payload = record_to_dict(record)
    if final.is_file():
        try:
            existing = json.loads(final.read_text())
        except (OSError, json.JSONDecodeError):
            existing = {}
        if isinstance(existing.get(GROUND_TRUTH_KEY), dict):
            payload[GROUND_TRUTH_KEY] = existing[GROUND_TRUTH_KEY]
    return atomic_write_json(final, payload)


def _index_gt_fields(out_dir: Path, file_name: str) -> dict:
    """Read the per-PR file's collection-quality fields for the index.

    ``ground_truth_collected`` tells a dataset consumer, from the index
    alone, whether collection has run for a PR; ``census_converged`` whether
    that collection converged. Both are ``None``/``False`` until
    ``/code-review-replay collect`` has frozen a ``ground_truth_v3`` block —
    so a freshly harvested PR shows ``ground_truth_collected: false``.
    """
    path = out_dir / file_name
    if not path.is_file():
        return {"ground_truth_collected": False, "census_converged": None}
    try:
        per_pr = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError):
        return {"ground_truth_collected": False, "census_converged": None}
    gt = per_pr.get(GROUND_TRUTH_KEY)
    if not isinstance(gt, dict):
        return {"ground_truth_collected": False, "census_converged": None}
    return {
        "ground_truth_collected": True,
        "census_converged": bool(gt.get("census_converged")),
    }


def _pr_sort_key(entry: dict) -> tuple:
    """Sort PR numbers numerically when they are numeric, else lexically."""
    num = str(entry.get("pr_number") or "")
    return (0, int(num), "") if num.isdigit() else (1, 0, num)


def _index_entry_from_record_file(path: Path) -> Optional[dict]:
    """Rebuild one index entry from an already-written dataset file.

    Used for PRs this run did not harvest, so a filtered run still emits a
    complete manifest. Returns None for anything that is not a readable PR
    record (a stray file in the dataset dir must not abort the harvest).
    """
    try:
        rec = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError):
        return None
    pr = rec.get("pr")
    if not isinstance(pr, dict) or not pr.get("pr_number"):
        return None
    return {
        "repo_name": pr.get("repo_name"),
        "repo_url": pr.get("repo_url"),
        "pr_number": pr.get("pr_number"),
        "pr_url": pr.get("pr_url"),
        "file": path.name,
        "completed": pr.get("completed"),
        "total_rounds": pr.get("total_rounds"),
        "harvested_rounds": len(rec.get("harvested_rounds") or []),
        "harvest_source": record_harvest_source(rec),
        **_index_gt_fields(path.parent, path.name),
    }


def build_index(
    records: list[PRRecord],
    *,
    generated_at: str,
    harvester_sha: str,
    out_dir: Path,
) -> dict:
    """Build ``index.json`` — the dataset-wide manifest.

    ``out_dir`` is read (after :func:`write_pr_record` has written every
    per-PR file) so each entry can carry ``ground_truth_collected`` /
    ``census_converged`` — letting a consumer find collected, converged PRs
    without opening every per-PR file.

    The index covers **every** dataset file in ``out_dir``, not just this
    run's ``records``. A filtered harvest (``--only``) would otherwise
    rewrite the manifest down to the PRs it touched, dropping every
    previously collected PR — and ``replay.py`` samples its targets from
    this file's ``ground_truth_collected`` flag, so those frozen ground
    truths would silently stop being replayable while still sitting on disk.
    Entries are rebuilt from the on-disk records, so the index self-heals.
    """
    prs = []
    seen: set[str] = set()
    for r in records:
        file_name = f"{r.pr['repo_name']}-{r.pr['pr_number']}.json"
        seen.add(file_name)
        prs.append(
            {
                "repo_name": r.pr["repo_name"],
                "repo_url": r.pr["repo_url"],
                "pr_number": r.pr["pr_number"],
                "pr_url": r.pr["pr_url"],
                "file": file_name,
                "completed": r.pr["completed"],
                "total_rounds": r.pr["total_rounds"],
                "harvested_rounds": len(r.harvested_rounds),
                "harvest_source": r.harvest_source,
                **_index_gt_fields(out_dir, file_name),
            }
        )

    for path in sorted(out_dir.glob("*.json")):
        if path.name == "index.json" or path.name in seen:
            continue
        entry = _index_entry_from_record_file(path)
        if entry is not None:
            prs.append(entry)

    prs.sort(key=lambda e: (e.get("repo_name") or "", _pr_sort_key(e)))
    return {
        "schema_version": SCHEMA_VERSION,
        "generated_at": generated_at,
        "harvester_git_sha": harvester_sha,
        "prs": prs,
    }


def write_index(out_dir: Path, index: dict) -> Path:
    return atomic_write_json(out_dir / "index.json", index)
