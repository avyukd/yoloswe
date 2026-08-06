#!/usr/bin/env python3
"""Replay mode — score a reviewer-under-test against a frozen ground truth.

This is the *cheap* half of the ``/code-review-replay`` skill. Collection
mode (``collect.py``) does the expensive work once: it runs many review +
judge rounds over a PR's diff and freezes a ``ground_truth_v3`` block — the
complete set of judged true / false positives — into the dataset JSON.

Replay mode then evaluates any reviewer config in a single mechanical pass:

  1. Checks out the recorded ``head_before`` in a temporary git worktree.
  2. Builds the ``--goal`` text *independently* (R1: live PR title/body +
     diffstat; R2+: deterministic pr-polish reconstruction). The dataset's
     recorded goal is kept only as a cross-check (``goal_divergence``).
  3. Runs ``bramble code-review`` once per configured backend.
  4. Scores each run's findings **mechanically** against the frozen
     ``ground_truth_v3`` — matched true positive / matched false positive /
     unmatched — and computes precision / recall / F1. No judge sub-agents,
     so a replay is fast and repeatable.

Requires the dataset JSON to carry a ``ground_truth_v3`` block. If it does
not, run collection mode first (``/code-review-replay collect <repo>-<pr>``).

The dataset lives outside the repo (``~/.bramble/code-review-eval/``) — it
holds private PR data and must never be committed.
"""

from __future__ import annotations

import argparse
import json
import os
import random
import shutil
import subprocess
import sys
import tempfile
import time
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Optional

SCRIPT_DIR = Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

import collect_lib as cl  # noqa: E402
import harvest_lib as hl  # noqa: E402
import replay_lib as rl  # noqa: E402
import unmatched_lib as ul  # noqa: E402

# Dataset + scored results live OUTSIDE the repo — they are derived from
# real private PRs and must never be committed. See harvest.py.
EVAL_ROOT = Path.home() / ".bramble" / "code-review-eval"
DEFAULT_DATASET_DIR = EVAL_ROOT / "dataset"
DEFAULT_OUT_DIR = EVAL_ROOT / "replays"
DEFAULT_BRAMBLE_BIN = "bramble"
# Which harvest_source tiers are scoreable by default. GitHub-sourced
# records have no local pr-polish state, so their ground truth rests on bot
# comments alone — measured ~9-14% precision on the kernel corpus. They stay
# on disk (nothing is deleted) but are out of the scoring pool unless
# --source asks for them explicitly.
DEFAULT_REPLAY_SOURCES = frozenset({"pr-polish"})
KNOWN_HARVEST_SOURCES = ("pr-polish", "github")
CODE_REVIEW_LOG_DIR = Path.home() / ".bramble" / "logs" / "code-review"
BRAMBLE_OPS_PATH = (
    SCRIPT_DIR.parents[1] / "pr-polish" / "scripts" / "bramble_ops.py"
)


# ---------------------------------------------------------------------------
# Backend configs
# ---------------------------------------------------------------------------


@dataclass
class BackendConfig:
    name: str  # display name, e.g. "codex-5.4-mini"
    backend: str  # bramble --backend value
    model: str
    extra_args: list[str] = field(default_factory=list)


# Persona variants for --review-prompt-file. bramble runs with cwd set to
# the PR's worktree, so these must be absolute or the path resolves against
# the wrong tree; loadPersonaFile only *warns* on a missing file and falls
# back to the built-in persona, so a relative path would silently score the
# baseline under a variant's name.
PERSONA_DIR = SCRIPT_DIR.parent / "personas"


def _persona_args(stem: str) -> list[str]:
    path = PERSONA_DIR / f"{stem}.md"
    if not path.is_file():
        raise FileNotFoundError(
            f"persona variant {stem!r} not found at {path}. Replay would "
            "silently fall back to the built-in persona and report the "
            "baseline under the variant's name."
        )
    return ["--review-prompt-file", str(path.resolve())]


# Mirrors the configs in the existing code-review-eval SKILL.md.
CONFIGS: dict[str, BackendConfig] = {
    "codex-5.4-mini": BackendConfig(
        name="codex-5.4-mini",
        backend="codex",
        model="gpt-5.4-mini",
        extra_args=["--effort", "medium"],
    ),
    "cursor-composer2": BackendConfig(
        name="cursor-composer2",
        backend="cursor",
        model="composer-2",
    ),
    "codex-5.6-luna": BackendConfig(
        name="codex-5.6-luna",
        backend="codex",
        model="gpt-5.6-luna",
        extra_args=["--effort", "medium"],
    ),
    # Effort variants. /pr-polish passes no --effort at all today, so
    # "default" is the incumbent the others must beat — without it in the
    # roster the bake-off would compare medium against high and never
    # measure the change that production would actually see.
    "codex-5.4-mini-default-effort": BackendConfig(
        name="codex-5.4-mini-default-effort",
        backend="codex",
        model="gpt-5.4-mini",
        extra_args=[],
    ),
    "codex-5.4-mini-high": BackendConfig(
        name="codex-5.4-mini-high",
        backend="codex",
        model="gpt-5.4-mini",
        extra_args=["--effort", "high"],
    ),
    "codex-5.6-luna-high": BackendConfig(
        name="codex-5.6-luna-high",
        backend="codex",
        model="gpt-5.6-luna",
        extra_args=["--effort", "high"],
    ),
    # Persona variants, all on codex-5.6-luna. Luna is the control backend
    # for prompt work because it was the only config to complete 3/3 runs in
    # the 2026-07-30 pilot — pairing a prompt variant with a backend that
    # stalls ~30% of the time confounds the prompt effect with the stall
    # rate. Each variant replaces only the persona/focus body; the JSON
    # output contract, goal text, and scope clauses stay machine-owned.
    #
    # NOTE: two of the three suppression clauses these variants drop are
    # persona-only ("Prioritize systemic problems over local ones" and
    # "avoid nit-level comments"), so dropping them has real effect. The
    # third ("do not strain to find something to flag") is ALSO present in
    # the non-overridable codeJSONOutputRules, so no variant can remove it
    # and no variant should be read as testing its absence.
    "luna-no-suppression": BackendConfig(
        name="luna-no-suppression",
        backend="codex",
        model="gpt-5.6-luna",
        extra_args=["--effort", "medium", *_persona_args("no-suppression")],
    ),
    "luna-coverage-ledger": BackendConfig(
        name="luna-coverage-ledger",
        backend="codex",
        model="gpt-5.6-luna",
        extra_args=["--effort", "medium", *_persona_args("coverage-ledger")],
    ),
    # Overfitting risk is real here: the defect archetypes were written
    # against a summary of kernel-8276's ground truth. A lift on kernel-8276
    # alone does NOT promote this variant — it must transfer to a PR that
    # was not consulted while writing it.
    "luna-defect-class-priming": BackendConfig(
        name="luna-defect-class-priming",
        backend="codex",
        model="gpt-5.6-luna",
        extra_args=[
            "--effort",
            "medium",
            *_persona_args("defect-class-priming"),
        ],
    ),
    # Recall-first variants (2026-08-05). The user asked for recall even at
    # some precision cost. These do NOT work by removing the suppression
    # clauses: `luna-no-suppression` already tested that and it held recall
    # flat while halving precision. Permission without procedure just raises
    # temperature. Each of these instead gives an uncertain-but-real defect
    # somewhere to go other than silence.
    "luna-confidence-band": BackendConfig(
        name="luna-confidence-band",
        backend="codex",
        model="gpt-5.6-luna",
        extra_args=["--effort", "medium", *_persona_args("confidence-band")],
    ),
    "luna-adversarial-successor": BackendConfig(
        name="luna-adversarial-successor",
        backend="codex",
        model="gpt-5.6-luna",
        extra_args=[
            "--effort",
            "medium",
            *_persona_args("adversarial-successor"),
        ],
    ),
    "luna-severity-floor": BackendConfig(
        name="luna-severity-floor",
        backend="codex",
        model="gpt-5.6-luna",
        extra_args=["--effort", "medium", *_persona_args("severity-floor")],
    ),
    # Localization variants (2026-08-05). These target a *measured* failure
    # mode, not a guessed one. On kernel-8276, 6 of 10 GT defects were found
    # by 0 of 24 runs — yet 23 of 23 sessions read the exact lines holding
    # them, and 17 of 24 runs *described* the stale-terminal-marker defect
    # in prose while only 2 filed it at a scoreable location. The reviewer
    # was not blind and was not quiet; it attributed the problem to the
    # wrong site. Every never-found defect also sat under a confident
    # docstring asserting the invariant held.
    "luna-file-at-the-read": BackendConfig(
        name="luna-file-at-the-read",
        backend="codex",
        model="gpt-5.6-luna",
        extra_args=["--effort", "medium", *_persona_args("file-at-the-read")],
    ),
    # Ablation: localization discipline WITHOUT the docstring-skepticism
    # section, so a win can be attributed to one half rather than the pair.
    "luna-localize-only": BackendConfig(
        name="luna-localize-only",
        backend="codex",
        model="gpt-5.6-luna",
        extra_args=["--effort", "medium", *_persona_args("localize-only")],
    ),
    # localize-only + a both-directions field sweep. Manual adjudication of
    # the misses (not the scorer) found the reviewer cited ALL THREE sites
    # that CLEAR artifact_id and ZERO sites that WRITE it — 0 citations
    # anywhere in sandbox.py across 36 runs, where GT holds a high-severity
    # writer-site defect at sandbox.py:2422. It traces a field one direction.
    "luna-localize-sweep": BackendConfig(
        name="luna-localize-sweep",
        backend="codex",
        model="gpt-5.6-luna",
        extra_args=[
            "--effort", "medium", *_persona_args("localize-plus-sweep"),
        ],
    ),
    # Round 3: localize + field sweep + caller-already-had-it. Manual
    # adjudication found preview_desired.py:118 (a redundant SELECT on a hot
    # poll path) is mentioned in 0 of 24 runs' prose — genuinely undetected,
    # not mislocalized. The helper reads correctly in isolation; the waste is
    # only visible from the call site, which the reviewer never checks.
    "luna-localize-sweep-reuse": BackendConfig(
        name="luna-localize-sweep-reuse",
        backend="codex",
        model="gpt-5.6-luna",
        extra_args=[
            "--effort", "medium", *_persona_args("localize-sweep-reuse"),
        ],
    ),
    # Round 3: localize-only + a writer-site clause folded INTO the
    # localization step, rather than added as a competing section. Rounds 1-2
    # both showed that stacking a second top-level principle costs hits
    # (file-at-the-read: no new TP, worst noise profile; localize-sweep:
    # HIT 5 -> 3 while volume rose to 12.3/run). The target is still
    # sandbox.py:2422, which 36 runs never cited.
    "luna-localize-writers": BackendConfig(
        name="luna-localize-writers",
        backend="codex",
        model="gpt-5.6-luna",
        extra_args=[
            "--effort", "medium", *_persona_args("localize-writers"),
        ],
    ),
    # Round 4: the winning localize-only base plus ONE SENTENCE in the
    # existing focus list (not a new section — every round that added a
    # section regressed). Targets preview_desired.py:118, an efficiency
    # defect 0 of 24 runs mentioned even in prose.
    "luna-localize-reuse": BackendConfig(
        name="luna-localize-reuse",
        backend="codex",
        model="gpt-5.6-luna",
        extra_args=["--effort", "medium", *_persona_args("localize-reuse")],
    ),
    # Gemini via the CURSOR backend, not the gemini backend — the gemini
    # CLI is rejected at the account level, but cursor proxies Gemini models
    # and works. Note there is no gemini-3.5-pro in `cursor-agent models`
    # (193 models, one Pro tier); these are the two nearest available.
    #
    # Cross-model check: every localization result so far is single-model
    # (gpt-5.6-luna). If the localization lift is a luna quirk rather than a
    # real principle, it will not transfer here.
    # --idle-timeout 8m is REQUIRED, not tuning. Gemini thinks silently for
    # >3 minutes before its first event on a review-sized prompt, so
    # bramble's 3m default kills it mid-thought and the envelope comes back
    # status=error with 0 input / 0 output tokens — indistinguishable from a
    # dead backend. Verified: the same run at 8m returns ok with 4 issues.
    "cursor-gemini-3.1-pro": BackendConfig(
        name="cursor-gemini-3.1-pro",
        backend="cursor",
        model="gemini-3.1-pro",
        extra_args=["--idle-timeout", "8m"],
    ),
    "cursor-gemini-3.5-flash": BackendConfig(
        name="cursor-gemini-3.5-flash",
        backend="cursor",
        model="gemini-3.5-flash",
        extra_args=["--idle-timeout", "8m"],
    ),
    # Cross-model validation of the localization persona (task #24). Every
    # localization result so far is gpt-5.6-luna on kernel-8276, and the
    # persona was written from that PR's own misses — so it could be a luna
    # quirk rather than a transferable principle. These pair the SAME persona
    # with two different model families. Gemini cannot serve here: both
    # gemini models make zero tool calls under the review prompt, and a
    # prompt effect cannot be measured on a model that never reads the code.
    "mini-localize-only": BackendConfig(
        name="mini-localize-only",
        backend="codex",
        model="gpt-5.4-mini",
        extra_args=["--effort", "medium", *_persona_args("localize-only")],
    ),
    "composer-localize-only": BackendConfig(
        name="composer-localize-only",
        backend="cursor",
        model="composer-2.5",
        extra_args=_persona_args("localize-only"),
    ),
    "composer-baseline": BackendConfig(
        name="composer-baseline",
        backend="cursor",
        model="composer-2.5",
    ),
    # Opt-in only. As of 2026-07-30 the gemini CLI client is rejected at the
    # account level ("This client is no longer supported for Gemini Code
    # Assist for individuals"), so this config fails to create a session on
    # an individual account. Kept for workspace accounts and for when the
    # wrapper migrates.
    "gemini-3.1-flash-lite-preview": BackendConfig(
        name="gemini-3.1-flash-lite-preview",
        backend="gemini",
        model="gemini-3.1-flash-lite-preview",
    ),
}


# ---------------------------------------------------------------------------
# Bramble invocation
# ---------------------------------------------------------------------------


def run_bramble_code_review(
    *,
    bramble_bin: str,
    cfg: BackendConfig,
    goal: str,
    cwd: Path,
    envelope_file: Path,
    protocol_log_dir: Path,
    log_dir: Path,
    run_tag: str,
    diff_base: Optional[str] = None,
    timeout_seconds: int = 900,
    verbose: bool = False,
) -> tuple[int, str, float]:
    """Run bramble code-review once.

    Returns ``(exit_code, stderr_tail, started_at_epoch)``. ``started_at`` is
    used afterwards to locate the klogfmt run log by mtime + tag.
    """
    args = [
        bramble_bin,
        "code-review",
        "--backend",
        cfg.backend,
        "--model",
        cfg.model,
        "--skip-test-execution",
        "--verbose",
        "--timeout",
        f"{timeout_seconds}s",
        "--goal",
        goal,
        "--envelope-file",
        str(envelope_file),
        "--protocol-log-dir",
        str(protocol_log_dir),
    ]
    # State the reviewed range explicitly. The worktree is detached at
    # head_before, so without this the agent guesses — and the natural
    # guess, `git diff main...HEAD`, three-dots against a local base
    # branch that has long since advanced past this PR's merge base.
    # Measured on kernel-8276: 336 files instead of 22, and the guess
    # varied run to run, making diff scope an uncontrolled variable
    # underneath every score.
    if diff_base:
        args += ["--diff-base", diff_base]
    args += [*cfg.extra_args]
    env = os.environ.copy()
    env["BRAMBLE_RUN_TAG"] = run_tag
    env["WORK_DIR"] = str(cwd)

    log_dir.mkdir(parents=True, exist_ok=True)
    protocol_log_dir.mkdir(parents=True, exist_ok=True)
    stderr_path = log_dir / f"{cfg.name}-stderr.txt"
    stdout_path = log_dir / f"{cfg.name}-stdout.txt"

    if verbose:
        print(
            f"  $ {bramble_bin} code-review --backend {cfg.backend} "
            f"--model {cfg.model} [...] (cwd={cwd}, tag={run_tag})",
            file=sys.stderr,
        )
    started_at = time.time()
    with open(stderr_path, "wb") as ferr, open(stdout_path, "wb") as fout:
        try:
            proc = subprocess.run(
                args,
                cwd=str(cwd),
                env=env,
                stdout=fout,
                stderr=ferr,
                check=False,
                timeout=timeout_seconds + 60,  # CLI's own --timeout is the inner clock
            )
            rc = proc.returncode
        except subprocess.TimeoutExpired:
            rc = -1

    tail = ""
    try:
        tail = stderr_path.read_text(errors="replace")[-2000:]
    except OSError:
        pass
    return rc, tail, started_at


def parse_envelope_file(path: Path) -> Optional[dict]:
    try:
        return json.loads(path.read_text())
    except (OSError, json.JSONDecodeError):
        return None


def _protocol_log_for_run(
    round_log: Path, started_at: float
) -> Optional[Path]:
    """The codex protocol JSONL written during this run, if any.

    ``--protocol-log-dir`` is the round dir, shared by both configs in the
    round. Caller must already have established this is a codex run — a
    cursor/gemini run writes no protocol JSONL, and the codex sibling's file
    falls inside its mtime window, so calling this for a non-codex run would
    mis-attribute the sibling's trace. Pick the newest file modified at/after
    this run's start.
    """
    best: Optional[tuple[float, Path]] = None
    for p in round_log.glob("reviewer-session-*.jsonl"):
        try:
            mt = p.stat().st_mtime
        except OSError:
            continue
        if mt + 1.0 < started_at:  # 1s slack for clock skew
            continue
        if best is None or mt > best[0]:
            best = (mt, p)
    return best[1] if best else None


def collect_execution_trace(
    *,
    run_tag: str,
    started_at: float,
    round_log: Path,
    config_name: str,
    backend: str,
    files_changed: list[str],
) -> rl.ExecutionTrace:
    """Find + parse this run's execution log into a structured trace.

    The codex backend logs tool calls only to its protocol JSONL (klogfmt
    stays near-empty), so codex runs are parsed from the protocol JSONL and
    cursor/gemini from the klogfmt run log. The source is chosen by
    ``backend`` — NOT by which log has more rows — because the round dir is
    shared and a cursor run would otherwise pick up the codex sibling's
    protocol file. Logs are copied into the round dir so the artifact is
    self-contained.
    """
    # --- klogfmt run log (all backends; authoritative for cursor/gemini) ---
    runlog = rl.find_runlog_by_tag(
        CODE_REVIEW_LOG_DIR, run_tag, after_mtime=started_at
    )
    text = ""
    runlog_copy: Optional[Path] = None
    if runlog is not None:
        try:
            text = runlog.read_text(errors="replace")
            runlog_copy = round_log / f"{config_name}-runlog.log"
            runlog_copy.write_text(text)
        except OSError:
            runlog_copy = None
    klog_trace = rl.parse_runlog(text)

    # --- codex protocol JSONL (codex runs only) ----------------------------
    protocol_copy: Optional[Path] = None
    codex_trace: Optional[rl.ExecutionTrace] = None
    if backend == "codex":
        protocol_path = _protocol_log_for_run(round_log, started_at)
        if protocol_path is not None:
            try:
                ptext = protocol_path.read_text(errors="replace")
                protocol_copy = round_log / f"{config_name}-protocol.jsonl"
                protocol_copy.write_text(ptext)
                codex_trace = rl.parse_codex_protocol(ptext)
            except OSError:
                codex_trace = None

    # codex -> protocol trace (klogfmt is near-empty); else -> klogfmt.
    trace = codex_trace if codex_trace is not None else klog_trace
    trace.runlog_path = str(runlog_copy) if runlog_copy else None
    trace.protocol_log_path = str(protocol_copy) if protocol_copy else None

    rl.annotate_files_coverage(trace, files_changed)
    return trace


# ---------------------------------------------------------------------------
# Worktree management
# ---------------------------------------------------------------------------


class TempWorktree:
    """A scratch git worktree at a specific commit, auto-removed on exit."""

    def __init__(self, repo_path: Path, sha: str, label: str):
        self.repo_path = repo_path
        self.sha = sha
        self.path = Path(tempfile.mkdtemp(prefix=f"replay-{label}-"))
        # Pre-empt the mkdtemp's empty dir — git worktree add requires the
        # target dir not to exist.
        self.path.rmdir()

    def __enter__(self) -> Path:
        res = hl.git(
            self.repo_path, "worktree", "add", "--detach",
            str(self.path), self.sha,
        )
        if res.returncode != 0:
            raise RuntimeError(
                f"git worktree add failed: "
                f"{res.stderr.strip() or '(no stderr)'}"
                f"{self._missing_commit_hint()}"
            )
        return self.path

    def _missing_commit_hint(self) -> str:
        """Turn "invalid reference" into the command that fixes it.

        The usual cause is an older dataset record whose head_before was
        never fetched into this checkout — the branch is merged and gone,
        so only refs/pull/N/head still names the commit. Raw git output
        ("fatal: invalid reference: <sha>") reads like a corrupt dataset
        and sends you auditing the record instead of running one fetch.
        collect.py setup already recovers this on the collection side;
        replay only needed to say so.
        """
        probe = hl.git(
            self.repo_path, "cat-file", "-e", f"{self.sha}^{{commit}}"
        )
        if probe.returncode == 0:
            return ""  # commit exists; the failure is something else
        return (
            f"\n  commit {self.sha[:12]} is not in {self.repo_path}. "
            "If the PR branch was merged and deleted, fetch its pull ref:\n"
            f"    git -C {self.repo_path} fetch origin "
            "refs/pull/<PR>/head\n"
            "  If it was force-pushed away, the commit is unrecoverable "
            "and the record cannot be replayed."
        )

    def __exit__(self, *exc):
        # Best-effort cleanup: --force in case bramble left dirty files.
        hl.git(self.repo_path, "worktree", "remove", "--force", str(self.path))
        if self.path.exists():
            shutil.rmtree(self.path, ignore_errors=True)


def select_dataset_rounds(
    dataset: dict, tier_filter: Optional[str]
) -> list[dict]:
    rounds = dataset.get("harvested_rounds") or []
    if tier_filter:
        rounds = [
            r
            for r in rounds
            if r.get("signal_tier") == tier_filter
            or (
                tier_filter == "r1"
                and r.get("signal_tier") in {"r1", "r1_only"}
            )
            or (
                tier_filter == "final"
                and r.get("signal_tier") in {"final", "final_incomplete"}
            )
        ]
    return rounds


def _load_pr_polish_state(repo_pr: str) -> Optional[dict]:
    """Load ~/.bramble/projects/<repo>-<pr>/pr-polish-state.json if present.

    Needed for R2+ goal reconstruction. R1 doesn't need it (PR body suffices).
    """
    path = (
        Path.home()
        / ".bramble"
        / "projects"
        / repo_pr
        / "pr-polish-state.json"
    )
    if not path.is_file():
        return None
    try:
        return json.loads(path.read_text())
    except (OSError, json.JSONDecodeError):
        return None


# Replay mode — run the reviewer-under-test, score mechanically vs frozen GT
# ---------------------------------------------------------------------------
#
# Replay mode is the cheap path: it spawns NO judge sub-agents. It runs
# `bramble code-review` once per (round, config) and scores each run's
# findings mechanically against the `ground_truth_v3` block collection mode
# already froze into the dataset JSON. This is what makes evaluating a new
# reviewer config a fast scoring pass instead of a multi-agent session.


@dataclass
class ReplayResult:
    schema_version: int
    phase: str  # "replay-scored"
    generated_at: str
    pr: dict
    dataset_file: str
    bramble_bin: str
    ground_truth_frozen_at: str
    ground_truth_census_converged: bool
    rounds: list[dict] = field(default_factory=list)


def run_replay(
    dataset_path: Path,
    *,
    repos_root: hl.RepoMap,
    configs: list[BackendConfig],
    tier_filter: Optional[str],
    bramble_bin: str,
    goal_source: str,
    timeout_seconds: int,
    log_root: Path,
    verbose: bool,
    strict: bool = False,
    stall_retries: int = 2,
) -> tuple[ReplayResult, Path]:
    """Run the reviewer-under-test and score it against the frozen GT.

    Requires the dataset JSON to carry a ``ground_truth_v3`` block (built by
    collection mode). Each (round, config) run is scored with
    ``replay_lib.score_against_frozen_gt`` — purely mechanical, no sub-agents.

    The frozen GT is run through ``collect_lib.validate_dataset`` before any
    scoring: structural ``errors`` always abort (the metrics would be
    meaningless), and quality ``warnings`` (unconverged census, unresolved
    contested rows, low harvest agreement) are printed to stderr — under
    ``strict`` they abort too, so a benchmark never silently reports
    precision/recall against a weak ground truth.
    """
    dataset = json.loads(dataset_path.read_text())
    gt = cl.load_ground_truth(dataset)
    if gt is None:
        raise RuntimeError(
            f"{dataset_path.name} has no '{cl.GROUND_TRUTH_KEY}' block — run "
            f"collection mode first: /code-review-replay collect "
            f"{dataset_path.stem}"
        )

    errors, warnings = cl.validate_dataset(dataset)
    if errors:
        joined = "; ".join(errors)
        raise RuntimeError(
            f"{dataset_path.name} frozen ground truth is malformed — "
            f"scoring would be meaningless: {joined}"
        )
    if warnings:
        for w in warnings:
            print(f"  warn: {dataset_path.name}: {w}", file=sys.stderr)
        if strict:
            raise RuntimeError(
                f"{dataset_path.name} frozen ground truth has "
                f"{len(warnings)} quality warning(s) and --strict is set"
            )

    pr = dataset.get("pr") or {}
    repo_name = pr.get("repo_name") or ""
    pr_number = pr.get("pr_number") or ""
    repo_pr = f"{repo_name}-{pr_number}"
    repo_path = repos_root.lookup(repo_name)
    if repo_path is None or not repo_path.exists():
        raise RuntimeError(
            f"no local checkout discovered for repo {repo_name!r} "
            f"(need ~/worktrees/{repo_name}/main or ~/g/{repo_name})"
        )

    rounds_to_replay = select_dataset_rounds(dataset, tier_filter)
    if not rounds_to_replay:
        raise RuntimeError(
            f"no rounds match --tier {tier_filter!r} in {dataset_path.name}"
        )

    # Round 1's recorded goal_text is the PR summary; pr-polish's
    # goal_for_round falls back to it for a pristine R2+ round, so thread it
    # into build_goal so R2+ reconstruction matches the frozen goal.
    pr_summary = next(
        (
            r.get("goal_text")
            for r in (dataset.get("harvested_rounds") or [])
            if int(r.get("round") or 0) == 1 and r.get("goal_text")
        ),
        None,
    )

    state = _load_pr_polish_state(repo_pr)
    log_root = log_root / f"{repo_pr}-{hl.run_id_stamp()}"
    rounds_out: list[dict] = []

    for dr in rounds_to_replay:
        head_before = dr.get("head_before")
        if not head_before:
            print(
                f"  round {dr.get('round')}: no head_before, skipping",
                file=sys.stderr,
            )
            continue

        goal = rl.build_goal(
            dr,
            repo_path=repo_path,
            pr_number=str(pr_number),
            state=state,
            bramble_ops_path=BRAMBLE_OPS_PATH,
            prefer=goal_source,
            pr_summary=pr_summary,
        )
        round_n = dr.get("round")
        signal_tier = dr.get("signal_tier")
        round_label = f"{repo_pr}-r{round_n}"
        round_log = log_root / f"r{round_n}"
        files_changed = list(dr.get("files_changed") or [])
        # The frozen record's merge base is the authoritative diff scope —
        # the same range the collection judge was given. Passing it to the
        # reviewer is what makes the two agree on "the diff".
        merge_base = dr.get("merge_base_sha") or None

        if verbose:
            print(
                f"-> round {round_n} ({signal_tier}) "
                f"head_before={head_before[:10]} goal_source={goal.source}",
                file=sys.stderr,
            )

        run_dicts: list[dict] = []
        with TempWorktree(repo_path, head_before, round_label) as wt:
            for cfg in configs:
                envelope_path = round_log / f"{cfg.name}-envelope.json"
                round_log.mkdir(parents=True, exist_ok=True)
                run_tag = (
                    f"code-review-replay:{repo_pr}:r{round_n}:{cfg.name}"
                )
                if verbose:
                    print(f"   config {cfg.name}...", file=sys.stderr)
                # A stalled backend is not a zero-recall review — it is a
                # review that never ran. Scoring it as "found nothing" both
                # understates the config and, across a matrix, computes
                # medians over uneven run counts. Measured on a 3-config
                # pilot: 27% of attempts stalled, unevenly (one config lost
                # 2 of 4 runs, another 0 of 3). So retry in-place rather
                # than leaving the caller to notice and patch by hand.
                for attempt in range(stall_retries + 1):
                    rc, stderr_tail, started_at = run_bramble_code_review(
                        bramble_bin=bramble_bin,
                        cfg=cfg,
                        goal=goal.text,
                        cwd=wt,
                        envelope_file=envelope_path,
                        protocol_log_dir=round_log,
                        log_dir=round_log,
                        run_tag=(
                            run_tag if not attempt
                            else f"{run_tag}:retry{attempt}"
                        ),
                        diff_base=merge_base,
                        timeout_seconds=timeout_seconds,
                        verbose=verbose,
                    )
                    env = parse_envelope_file(envelope_path)
                    if not _is_stalled_run(env):
                        break
                    if attempt < stall_retries:
                        print(
                            f"   {cfg.name}: stalled backend, retrying "
                            f"({attempt + 1}/{stall_retries})",
                            file=sys.stderr,
                        )
                if env is None:
                    scored = rl.ScoredRunV3(
                        backend=cfg.backend,
                        model=cfg.model,
                        config=cfg.name,
                        envelope_status="missing",
                        verdict=None,
                        duration_ms=None,
                        n_findings_replay=0,
                        matched_tp=0,
                        matched_fp=0,
                        unmatched=0,
                        gt_true_positives=len(gt.get("true_positives") or []),
                        missed_tp=len(gt.get("true_positives") or []),
                        severity_mismatches=0,
                        precision=None,
                        recall=None,
                        f1=None,
                        score_error=(
                            f"no envelope (rc={rc}); stderr tail: "
                            f"{stderr_tail[-400:]}"
                        ),
                    )
                    run_dicts.append(rl.scored_run_v3_to_dict(scored))
                    continue

                review = env.get("review") or {}
                replay_findings = rl.expand_finding_sites(
                    review.get("issues") or []
                )
                env_status = env.get("status") or (
                    "ok" if review else "error"
                )
                scored = rl.score_against_frozen_gt(
                    backend=env.get("backend") or cfg.backend,
                    model=env.get("model") or cfg.model,
                    config=cfg.name,
                    envelope_status=env_status,
                    verdict=review.get("verdict"),
                    duration_ms=env.get("duration_ms"),
                    replay_findings=replay_findings,
                    ground_truth=gt,
                )
                run_dicts.append(rl.scored_run_v3_to_dict(scored))

        rounds_out.append(
            {
                "round": round_n,
                "signal_tier": signal_tier,
                "head_before": head_before,
                "merge_base_sha": dr.get("merge_base_sha"),
                "files_changed": len(files_changed),
                "goal_source": goal.source,
                "goal_divergence": goal.goal_divergence,
                "runs": run_dicts,
            }
        )

    result = ReplayResult(
        schema_version=rl.REPLAY_SCHEMA_VERSION,
        phase="replay-scored",
        generated_at=hl.iso_utc_now(),
        pr=pr,
        dataset_file=dataset_path.name,
        bramble_bin=bramble_bin,
        ground_truth_frozen_at=gt.get("frozen_at") or "",
        ground_truth_census_converged=bool(gt.get("census_converged")),
        rounds=rounds_out,
    )
    log_root.mkdir(parents=True, exist_ok=True)
    return result, log_root


# ---------------------------------------------------------------------------
# Output writing + rendering
# ---------------------------------------------------------------------------


def write_json(out_dir: Path, name: str, obj: dict) -> Path:
    return hl.atomic_write_json(out_dir / name, obj)



def render_replay_markdown(result: "ReplayResult") -> str:
    """Pretty-print the replay-mode scored result (mechanical, no judges)."""
    out: list[str] = []
    pr = result.pr
    out.append(
        f"# Replay scored — {pr.get('repo_name')}-{pr.get('pr_number')}"
    )
    out.append("")
    out.append(f"- Generated: {result.generated_at}")
    out.append(
        f"- Ground truth: frozen {result.ground_truth_frozen_at or '?'}"
        + (
            ""
            if result.ground_truth_census_converged
            else "  ⚠ census did NOT converge — recall denominator may be"
            " incomplete"
        )
    )
    out.append(
        "- Metrics are computed mechanically against the frozen "
        "`ground_truth_v3` set — no judge sub-agents."
    )
    out.append(
        "- `Sev✗` = matched true positives the reviewer reported at the "
        "wrong severity (a separate signal — it does not move P/R/F1)."
    )
    out.append("")

    def _pct(x: Optional[float]) -> str:
        return "—" if x is None else f"{x:.2f}"

    for rd in result.rounds:
        out.append(
            f"## Round {rd['round']} ({rd['signal_tier']}) — "
            f"head_before={(rd['head_before'] or '')[:10]}, "
            f"{rd['files_changed']} files changed"
        )
        if rd.get("goal_divergence"):
            out.append("- ⚠ Reconstructed goal diverged from the dataset goal.")
        out.append("")
        out.append(
            "| Config | Status | Findings | TP/FP/unmatched | Missed | "
            "Sev✗ | Precision | Recall | F1 | Time |"
        )
        out.append(
            "|--------|--------|----------|-----------------|--------|"
            "------|-----------|--------|----|------|"
        )
        for r in rd["runs"]:
            t_s = (
                "—"
                if r.get("duration_ms") is None
                else f"{r['duration_ms'] / 1000:.0f}s"
            )
            sev = r.get("severity_mismatches")
            sev_s = "—" if sev is None else str(sev)
            out.append(
                f"| {r['config']} | {r['envelope_status']} | "
                f"{r['n_findings_replay']} | "
                f"{r['matched_tp']}/{r['matched_fp']}/{r['unmatched']} | "
                f"{r['missed_tp']} | {sev_s} | {_pct(r.get('precision'))} | "
                f"{_pct(r.get('recall'))} | {_pct(r.get('f1'))} | {t_s} |"
            )
        out.append("")
        for r in rd["runs"]:
            if r.get("score_error"):
                out.append(f"### {r['config']} — ⚠ {r['score_error']}")
                out.append("")
                continue
            missed = r.get("missed_true_positives") or []
            if not missed:
                continue
            out.append(f"### {r['config']} — missed real issues")
            out.append("")
            for m in missed:
                loc = f"{m.get('file', '?')}:{m.get('line', '?')}"
                out.append(
                    f"- [{m.get('severity', '?')}] {loc} — "
                    f"{m.get('topic', '')}"
                )
            out.append("")
    return "\n".join(out)


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def diagnose_missing_dataset(dataset_path: Path, target: str) -> str:
    """Build an actionable message when the dataset file is absent."""
    lines = [f"error: dataset file not found: {dataset_path}", ""]

    parsed = (
        hl.parse_project_dir_name(target)
        if not target.endswith(".json")
        else None
    )
    if parsed is None and not target.endswith(".json"):
        lines.append(
            f"  '{target}' is not a <repo>-<pr> id (expected e.g. kernel-3945)."
        )
        return "\n".join(lines)

    projects_dir = Path.home() / ".bramble" / "projects"
    source_dir = projects_dir / target if parsed else None

    if source_dir is not None and (source_dir / "pr-polish-state.json").is_file():
        lines.append(
            f"  The pr-polish source for {target} still exists at {source_dir}."
        )
        lines.append(
            "  Run collection mode to build the dataset + frozen ground "
            "truth, then re-run this replay:"
        )
        lines.append("")
        lines.append(f"    /code-review-replay collect {target}")
        lines.append("")
        lines.append("  (or harvest the raw dataset directly:)")
        lines.append("")
        lines.append(
            "    python3 .claude/skills/code-review-replay/scripts/harvest.py"
            f" --only {target} --verbose"
        )
        return "\n".join(lines)

    if not projects_dir.exists():
        lines.append(
            f"  No pr-polish data at all under {projects_dir} — nothing to harvest."
        )
    else:
        lines.append(
            f"  No pr-polish source dir for {target} under {projects_dir}."
        )
        available = sorted(
            d.name
            for d in projects_dir.iterdir()
            if d.is_dir()
            and hl.parse_project_dir_name(d.name)
            and (d / "pr-polish-state.json").is_file()
        )
        if available:
            lines.append("  PRs that CAN be harvested right now:")
            for name in available[:25]:
                lines.append(f"    {name}")
            if len(available) > 25:
                lines.append(f"    ... and {len(available) - 25} more")
    lines.append("")
    lines.append(
        "  The dataset is built from `/pr-polish` run history; without that "
        "source data the PR cannot be replayed."
    )
    return "\n".join(lines)


def _is_stalled_run(env: Optional[dict]) -> bool:
    """Whether an envelope represents a backend that never produced a review.

    Distinguishes "the reviewer ran and found nothing" (a real zero-recall
    result that belongs in the score) from "the backend stalled" (no result
    at all). Only the latter is retried — retrying a genuine empty review
    would bias the sample toward configs that happen to be chatty.
    """
    if env is None:
        return True  # no envelope written at all
    if (env.get("status") or "") != "ok":
        return True
    return False


def _target_harvest_source(dataset_dir: Path, target: str) -> Optional[str]:
    """``harvest_source`` for one target, or None if it can't be determined.

    Best-effort: a target may be a path, may not be in the index yet, or may
    predate schema 3. None means "don't warn" — never a hard failure, since
    this only decorates an explicitly requested run.
    """
    name = Path(target).name
    stem = name[:-5] if name.endswith(".json") else name
    index_path = dataset_dir / "index.json"
    if not index_path.is_file():
        return None
    try:
        index = json.loads(index_path.read_text())
    except (OSError, json.JSONDecodeError):
        return None
    for e in index.get("prs") or []:
        f = e.get("file") or ""
        if f == f"{stem}.json":
            return e.get("harvest_source") or "pr-polish"
    return None


def select_replay_targets(
    *, dataset_dir: Path, sample: int, sources: Optional[set] = None
) -> list[str]:
    """Randomly sample ``sample`` PRs that have a frozen ground truth.

    The pool is ``index.json``'s ``ground_truth_collected`` flag — no
    per-file scan. Used when ``replay.py`` is given no positional target.

    ``sources`` restricts the pool by ``harvest_source``; it defaults to
    ``DEFAULT_REPLAY_SOURCES`` (pr-polish only). GitHub-sourced records are
    excluded because their ground truth is built from bot comments alone,
    which measured ~9-14% precision on this corpus: 10 such PRs contributed
    40 true positives against 201 false positives, versus 107/41 from 19
    pr-polish PRs. Scoring against that inverted ratio rewards a reviewer
    for reproducing bot noise. Pass ``--source github`` to include them
    anyway (e.g. to audit the excluded tier); records stay on disk either
    way, so this is a scoring filter, not a deletion.
    """
    index_path = dataset_dir / "index.json"
    if not index_path.is_file():
        raise SystemExit(f"error: no index.json under {dataset_dir}")
    index = json.loads(index_path.read_text())
    allowed = DEFAULT_REPLAY_SOURCES if sources is None else sources
    entries = [
        e
        for e in index.get("prs") or []
        if e.get("ground_truth_collected") and e.get("file", "").endswith(
            ".json"
        )
    ]
    # Records written before schema 3 carry no harvest_source; they predate
    # the GitHub source entirely, so they are pr-polish by construction.
    pool = [
        e["file"][:-5]  # strip ".json"
        for e in entries
        if (e.get("harvest_source") or "pr-polish") in allowed
    ]
    if not pool:
        excluded = len(entries) - len(pool)
        raise SystemExit(
            "error: no PR in the dataset has a frozen ground truth from "
            f"source(s) {sorted(allowed)}"
            + (
                f" ({excluded} GT'd PR(s) excluded by --source)"
                if excluded
                else " — run `/code-review-replay collect` first"
            )
        )
    return sorted(random.sample(pool, min(sample, len(pool))))


def main(argv: Optional[list[str]] = None) -> int:
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument(
        "target",
        nargs="?",
        help="Dataset target (e.g. kernel-3834) or path to a JSON file. "
        "Omit to randomly sample --sample PRs from the dataset.",
    )
    p.add_argument(
        "--sample",
        type=int,
        default=5,
        help="When no target is given, how many GT-collected PRs to "
        "randomly score (default 5).",
    )
    p.add_argument("--dataset-dir", type=Path, default=DEFAULT_DATASET_DIR)
    p.add_argument("--out-dir", type=Path, default=DEFAULT_OUT_DIR)
    p.add_argument(
        "--repo-root",
        action="append",
        default=[],
        metavar="NAME=PATH",
        dest="repo_root",
        help="Override repo-root auto-discovery for one repo. Rarely needed.",
    )
    p.add_argument(
        "--config",
        action="append",
        default=[],
        choices=sorted(CONFIGS.keys()),
        help="Backend config to run. Repeatable. Default: codex-5.4-mini + cursor-composer2.",
    )
    p.add_argument(
        "--tier",
        choices=["r1", "final"],
        help="Filter rounds by signal_tier. Default: both.",
    )
    p.add_argument(
        "--bramble-bin",
        default=DEFAULT_BRAMBLE_BIN,
        help="Path to the bramble binary.",
    )
    p.add_argument(
        "--goal-source",
        choices=["auto", "dataset"],
        default="auto",
        help=(
            "auto (default): build the --goal independently (R1 from the "
            "live PR, R2+ reconstructed); dataset: use the dataset's "
            "recorded goal verbatim (pre-redesign behaviour, for debugging)."
        ),
    )
    p.add_argument(
        "--timeout-seconds",
        type=int,
        default=900,
        help="Per-bramble-call timeout (default 900s = 15m).",
    )
    p.add_argument(
        "--log-root",
        type=Path,
        default=Path(tempfile.gettempdir()) / "code-review-replay",
    )
    p.add_argument("--verbose", "-v", action="store_true")
    p.add_argument(
        "--strict",
        action="store_true",
        help="Treat frozen-GT quality warnings (unconverged census, "
        "unresolved contested rows, low harvest agreement) as fatal. "
        "Structural errors always abort regardless of this flag.",
    )
    p.add_argument(
        "--unmatched-report",
        action="store_true",
        help=(
            "After scoring, triage the unmatched bucket by cross-run "
            "recurrence. Precision ignores unmatched findings, so this is "
            "the only view that distinguishes a recall-first variant "
            "finding real defects the census missed from one generating "
            "noise. Recurrent hits are re-collection candidates, not "
            "recall credit."
        ),
    )
    p.add_argument(
        "--print-markdown",
        action="store_true",
        help="Print a Markdown summary to stdout.",
    )
    p.add_argument(
        "--stall-retries",
        type=int,
        default=2,
        help="Retries when a backend stalls and writes no usable envelope "
        "(default 2). A stalled run is not a zero-recall review — scoring it "
        "as one both understates the config and, across a matrix, computes "
        "medians over uneven run counts. Measured at 27%% of attempts on a "
        "3-config pilot, unevenly distributed. Set 0 to disable.",
    )
    p.add_argument(
        "--source",
        action="append",
        default=[],
        choices=KNOWN_HARVEST_SOURCES,
        help="Which harvest_source tiers to draw sampled PRs from "
        f"(default: {sorted(DEFAULT_REPLAY_SOURCES)}). GitHub-sourced GT is "
        # argparse runs help strings through %-formatting, so a literal
        # percent must be doubled or --help raises ValueError.
        "built from bot comments alone (~9-14%% precision) and is excluded "
        "from scoring by default; pass --source github to audit it. "
        "Repeatable.",
    )
    args = p.parse_args(argv)

    # ---- resolve which PR(s) to score ------------------------------------
    sources = set(args.source) if args.source else set(DEFAULT_REPLAY_SOURCES)
    if args.target:
        # An explicit target is honored even when its source is outside the
        # scoring pool — but say so, so a number never lands in a writeup
        # without the caveat attached.
        targets = [args.target]
        src = _target_harvest_source(args.dataset_dir, args.target)
        if src is not None and src not in sources:
            print(
                f"warning: {args.target} has harvest_source={src!r}, which is "
                "outside the default scoring pool "
                f"({sorted(DEFAULT_REPLAY_SOURCES)}). Its ground truth comes "
                "from bot comments with no pr-polish triage; precision and "
                "recall against it are not comparable to pr-polish-sourced "
                "scores.",
                file=sys.stderr,
            )
    else:
        try:
            targets = select_replay_targets(
                dataset_dir=args.dataset_dir,
                sample=args.sample,
                sources=sources,
            )
        except SystemExit as e:
            print(e, file=sys.stderr)
            return 2
        print(
            f"no target given — sampling {len(targets)} GT-collected PR(s): "
            f"{', '.join(targets)}",
            file=sys.stderr,
        )

    try:
        repo_map = hl.RepoMap.discover(args.repo_root)
    except ValueError as e:
        print(f"error: {e}", file=sys.stderr)
        return 2

    if not args.config:
        configs = [CONFIGS["codex-5.4-mini"], CONFIGS["cursor-composer2"]]
    else:
        configs = [CONFIGS[name] for name in args.config]

    # ---- score each target ----------------------------------------------
    worst = 0
    results = []
    for target in targets:
        dataset_path = (
            Path(target) if target.endswith(".json")
            else args.dataset_dir / f"{target}.json"
        )
        if not dataset_path.exists():
            print(
                diagnose_missing_dataset(dataset_path, target),
                file=sys.stderr,
            )
            worst = 2
            continue
        try:
            result, _ = run_replay(
                dataset_path,
                repos_root=repo_map,
                configs=configs,
                tier_filter=args.tier,
                bramble_bin=args.bramble_bin,
                goal_source=args.goal_source,
                timeout_seconds=args.timeout_seconds,
                log_root=args.log_root,
                verbose=args.verbose,
                strict=args.strict,
                stall_retries=args.stall_retries,
            )
        except RuntimeError as e:
            print(f"error scoring {target}: {e}", file=sys.stderr)
            worst = 2
            continue
        repo_pr = f"{result.pr.get('repo_name')}-{result.pr.get('pr_number')}"
        stamp = hl.run_id_stamp()
        path = write_json(
            args.out_dir, f"{repo_pr}-{stamp}-scored.json", asdict(result)
        )
        print(f"wrote {path}", file=sys.stderr)
        results.append(result)

    if args.print_markdown:
        for result in results:
            print(render_replay_markdown(result))
            print()

    # Unmatched triage. Precision ignores this bucket entirely, so without
    # it a recall-first variant's extra output is invisible: a real defect
    # the census missed and a plausible hallucination score identically.
    # Recurrence across independent runs separates them cheaply.
    if args.unmatched_report:
        _print_unmatched_report(results)
    return worst


def _unmatched_observations(results: list) -> list[tuple]:
    """Flatten scored results into (run_id, config, file, line) tuples."""
    obs = []
    for result in results:
        for rnd in result.rounds:
            run_id = f"{result.pr.get('pr_number')}-r{rnd.round}"
            for run in rnd.runs:
                cfg = run.get("config") or run.get("backend") or "?"
                # A run id must be unique per *run*, not per round, or two
                # runs of one config would look like self-corroboration.
                rid = f"{run_id}-{run.get('started_at') or id(run)}"
                for fs in run.get("finding_scores") or []:
                    if fs.get("outcome") != "unmatched":
                        continue
                    obs.append((rid, cfg, fs.get("file"), fs.get("line")))
    return obs


def _print_unmatched_report(results: list) -> None:
    gts = [r.ground_truth for r in results if getattr(r, "ground_truth", None)]
    if not gts:
        return
    merged = {
        "true_positives": [e for g in gts for e in g.get("true_positives", [])],
        "false_positives": [
            e for g in gts for e in g.get("false_positives", [])
        ],
    }
    locs = ul.collect_unmatched(_unmatched_observations(results), merged)
    print()
    print(ul.format_report(locs))


if __name__ == "__main__":
    raise SystemExit(main())
