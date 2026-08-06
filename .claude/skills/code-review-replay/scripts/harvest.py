#!/usr/bin/env python3
"""Harvest past bramble code-review runs into a structured eval dataset.

Walks ``~/.bramble/projects/<repo>-<pr>/`` directories produced by the
``/pr-polish`` skill and emits one JSON record per PR (R1 + final round
only), plus a top-level ``index.json``. The per-PR record carries
ground-truth labels derived from ``comment_actions.action`` and enough
metadata to replay ``bramble code-review`` apple-to-apple later.

See ``README.md`` for the dataset schema and matching rules.
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
from pathlib import Path
from typing import Optional

SCRIPT_DIR = Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

import harvest_lib as hl  # noqa: E402

REPO_ROOT = Path(__file__).resolve().parents[4]  # yoloswe worktree root
DEFAULT_BRAMBLE_OPS = (
    REPO_ROOT / ".claude" / "skills" / "pr-polish" / "scripts" / "bramble_ops.py"
)
DEFAULT_SOURCE_DIR = Path.home() / ".bramble" / "projects"
# The dataset lives OUTSIDE the repo: it is derived from real private PRs
# (file paths, commit SHAs, reviewer findings) and must never be committed.
# It sits next to ~/.bramble/projects/, the pr-polish data it is built from.
DEFAULT_OUT_DIR = Path.home() / ".bramble" / "code-review-eval" / "dataset"


# Defined in harvest_lib (which now needs it too for the GitHub fallback);
# re-exported here so existing callers and tests keep working.
repo_slug = hl.repo_slug_from_url


def fetch_pr_summary(repo_url: Optional[str], pr_number: str) -> Optional[str]:
    """Best-effort `gh pr view` fetch. Returns None on any failure."""
    slug = repo_slug(repo_url)
    if not slug or not pr_number:
        return None
    try:
        res = subprocess.run(
            ["gh", "pr", "view", pr_number, "-R", slug, "--json", "title,body"],
            capture_output=True,
            text=True,
            check=False,
            timeout=15,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return None
    if res.returncode != 0:
        return None
    try:
        obj = json.loads(res.stdout)
    except json.JSONDecodeError:
        return None
    title = obj.get("title") or ""
    body = obj.get("body") or ""
    text = (title + "\n\n" + body).strip()
    return text or None


def _log(verbose: bool, msg: str) -> None:
    if verbose:
        print(msg, file=sys.stderr)


def main(argv: Optional[list[str]] = None) -> int:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--source-dir", type=Path, default=DEFAULT_SOURCE_DIR)
    p.add_argument("--out-dir", type=Path, default=DEFAULT_OUT_DIR)
    p.add_argument(
        "--repo-root",
        action="append",
        default=[],
        metavar="NAME=PATH",
        dest="repo_root",
        help="Override auto-discovery for one repo (NAME=PATH). Repeatable; "
        "rarely needed — repo checkouts are found automatically.",
    )
    p.add_argument(
        "--bramble-ops-path",
        type=Path,
        default=DEFAULT_BRAMBLE_OPS,
        help="Path to the pr-polish bramble_ops.py (for goal-text reconstruction).",
    )
    p.add_argument(
        "--include-incomplete",
        action=argparse.BooleanOptionalAction,
        default=True,
        help="Include PRs where the polish loop did not converge. "
        "Pass --no-include-incomplete to harvest only converged PRs "
        "(default: include).",
    )
    p.add_argument(
        "--skip-pr-summary",
        action="store_true",
        help="Skip `gh pr view` for PR summaries (R1 goal_text will be null).",
    )
    p.add_argument(
        "--skip-pr-comments",
        action="store_true",
        help=(
            "Skip `gh api` PR-comment fetch. github comments fall back to the "
            "state-recorded comment_actions set (no created_at; "
            "attribution_basis=no_timestamp)."
        ),
    )
    p.add_argument(
        "--only",
        action="append",
        default=[],
        help="Filter to specific project dir names (e.g. kernel-3945). Repeatable.",
    )
    gh_group = p.add_argument_group(
        "GitHub source (deprecated — not part of the refresh cycle)",
        "Harvest merged PRs straight from GitHub, including ones never "
        "polished locally. All default off, so omitting them leaves "
        "today's local-only behavior unchanged. GitHub-sourced records "
        "carry the same diff scope, goal, and comment census but no "
        "review_runs (there are no local envelopes), so "
        "`build-prompt --include-harvested` contributes nothing for them. "
        "DEPRECATED: these records' ground truth rests on external bot "
        "comments alone (~9-14% precision on the kernel corpus: 40 true "
        "positives vs 201 false positives across 10 PRs, against 107/41 "
        "from 19 pr-polish PRs), so replay.py excludes them from scoring "
        "by default. The corpus is grown by running /pr-polish on more "
        "PRs, not by harvesting GitHub. Kept working and tested in case "
        "the precision picture changes. See "
        "docs/design/code-review-benchmark-process.md.",
    )
    gh_group.add_argument(
        "--github-repo",
        action="append",
        default=[],
        metavar="OWNER/REPO",
        help="Enable the GitHub source for this repo. Repeatable.",
    )
    gh_group.add_argument(
        "--github-merged-since",
        metavar="YYYY-MM-DD",
        help="Only PRs merged on or after this date.",
    )
    gh_group.add_argument(
        "--github-limit",
        type=int,
        default=100,
        help="Max PRs to pull per repo (default: 100).",
    )
    gh_group.add_argument(
        "--github-skip-harvested",
        action=argparse.BooleanOptionalAction,
        default=True,
        help="Skip PRs already in the dataset (default: skip). The dataset "
        "dir is the cache, so a re-run costs almost nothing.",
    )
    gh_group.add_argument(
        "--github-only",
        action="store_true",
        help="Skip local pr-polish discovery entirely (bulk backfill).",
    )
    gh_group.add_argument(
        "--commit-scoped",
        action="store_true",
        help="Emit one round per commit an inline comment actually reviewed "
        "(from its original_commit_id) instead of pinning every comment to "
        "the PR's final head. Without this, a bug a bot correctly reported "
        "and the author then fixed is judged against the post-fix code and "
        "recorded as a false positive.",
    )
    p.add_argument("--dry-run", action="store_true")
    p.add_argument("--verbose", "-v", action="store_true")
    args = p.parse_args(argv)

    try:
        repo_map = hl.RepoMap.discover(args.repo_root)
    except ValueError as e:
        print(f"error: {e}", file=sys.stderr)
        return 2
    _log(args.verbose,
         f"discovered repo roots: {sorted(repo_map.mapping)}")

    if not args.bramble_ops_path.exists():
        print(
            f"warning: bramble_ops.py not found at {args.bramble_ops_path}; "
            "R2+ goal reconstruction will fail",
            file=sys.stderr,
        )

    harvester_sha = hl.harvester_git_sha(REPO_ROOT)
    harvested_at = hl.iso_utc_now()

    # Candidates from both sources, unified into
    # (state_dir | None, repo_name, pr_number, harvest_source, gh | None).
    candidates: list[tuple] = []
    discovery_degraded = False
    if not args.github_only:
        projects = hl.discover_project_dirs(args.source_dir)
        if args.only:
            only = set(args.only)
            projects = [(d, r, n) for (d, r, n) in projects if d.name in only]
        candidates = [(d, r, n, "pr-polish", None) for (d, r, n) in projects]
        _log(args.verbose, f"discovered {len(candidates)} project dirs")

    if args.github_repo:
        # Local records are strictly higher fidelity (they carry the
        # reviewer envelopes), and write_pr_record preserves only
        # ground_truth_v3 — so a GitHub row for a PR we already polished
        # locally would *downgrade* the record. Local always wins.
        seen = {f"{r}-{n}" for (_, r, n, _, _) in candidates}
        skip = set(seen)
        if args.github_skip_harvested:
            skip |= hl.already_harvested(args.out_dir)
        for slug in args.github_repo:
            rows, err = hl.discover_github_prs(
                slug,
                merged_since=args.github_merged_since,
                limit=args.github_limit,
                exclude=skip,
            )
            if err:
                print(f"warning: {slug}: {err}", file=sys.stderr)
                discovery_degraded = True
            _log(args.verbose, f"{slug}: {len(rows)} GitHub candidate(s)")
            for row in rows:
                if args.only and f"{row['repo_name']}-{row['pr_number']}" not in set(
                    args.only
                ):
                    continue
                candidates.append(
                    (None, row["repo_name"], row["pr_number"], "github", row)
                )

    if not candidates:
        print(
            "no harvest candidates: no PR-numbered project dirs under "
            f"{args.source_dir}"
            + (" and no GitHub matches" if args.github_repo else ""),
            file=sys.stderr,
        )
        return 2

    records: list[hl.PRRecord] = []
    partial = False
    rate_limited = False

    for state_dir, repo_name, pr_number, harvest_source, gh in candidates:
        _log(
            args.verbose,
            f"-> {repo_name}-{pr_number}"
            + (" [github]" if harvest_source == "github" else ""),
        )
        repo_path = repo_map.lookup(repo_name)
        repo_url = hl.get_repo_url(repo_path)
        # A GitHub candidate may have no local checkout; discovery already
        # knows its slug, so comment/summary fetches still work.
        if not repo_url and gh and gh.get("slug"):
            repo_url = f"https://github.com/{gh['slug']}"
        slug = repo_slug(repo_url)
        pr_summary: Optional[str] = None
        if not args.skip_pr_summary:
            pr_summary = fetch_pr_summary(repo_url, pr_number)
            if pr_summary is None:
                _log(args.verbose, f"   pr_summary unavailable; R1 goal will be null")
                partial = True

        fetched_comments: Optional[list] = None
        comments_err: Optional[str] = None
        fetch_attempted = not args.skip_pr_comments
        if fetch_attempted:
            if slug:
                fetched_comments, comments_err = hl.fetch_pr_comments(
                    slug, pr_number
                )
                if comments_err:
                    _log(args.verbose, f"   pr-comment fetch issue: {comments_err}")
                    partial = True
                    # A rate limit is not a per-PR problem: every remaining
                    # PR will fail identically and silently degrade to the
                    # state-recorded comment set, producing a corpus of
                    # records that look harvested but carry no external
                    # review census. Stop while the dataset is still clean.
                    if hl.is_rate_limit_error(comments_err):
                        print(
                            "error: GitHub rate limit hit after "
                            f"{len(records)} PR(s); stopping so the "
                            "remaining PRs are not written with missing "
                            "comment data. Re-run once the limit resets — "
                            "already-harvested PRs are skipped.",
                            file=sys.stderr,
                        )
                        rate_limited = True
                        break
                else:
                    _log(
                        args.verbose,
                        f"   fetched {len(fetched_comments or [])} PR comment(s)",
                    )
            else:
                comments_err = "no repo slug (repo checkout not discovered)"
                _log(args.verbose, f"   {comments_err}; github comments fall back")
                fetch_attempted = False
                partial = True

        try:
            record = hl.build_pr_record(
                state_dir,
                repo_name,
                pr_number,
                repo_map=repo_map,
                pr_summary=pr_summary,
                harvester_sha=harvester_sha,
                harvested_at=harvested_at,
                bramble_ops_path=args.bramble_ops_path,
                include_incomplete=args.include_incomplete,
                fetched_pr_comments=fetched_comments,
                pr_comments_fetch_error=comments_err,
                fetch_attempted=fetch_attempted,
                harvest_source=harvest_source,
                gh=gh,
                commit_scoped=args.commit_scoped,
            )
        except Exception as e:
            print(
                f"error: failed to build record for "
                f"{repo_name}-{pr_number}: {e}",
                file=sys.stderr,
            )
            partial = True
            continue
        if record is None:
            _log(args.verbose, f"   skipped")
            continue
        records.append(record)
        if args.dry_run:
            _log(args.verbose,
                 f"   would write: {len(record.harvested_rounds)} rounds, "
                 f"{sum(len(r.review_runs) for r in record.harvested_rounds)} review_runs")
            continue
        path = hl.write_pr_record(args.out_dir, record)
        _log(args.verbose, f"   wrote {path}")

    if not args.dry_run and records:
        index = hl.build_index(
            records,
            generated_at=harvested_at,
            harvester_sha=harvester_sha,
            out_dir=args.out_dir,
        )
        idx_path = hl.write_index(args.out_dir, index)
        _log(args.verbose, f"wrote {idx_path}")

    print(
        f"harvested {len(records)} PR(s) "
        f"({'dry-run' if args.dry_run else 'written'})"
        + (" — STOPPED EARLY: rate limited" if rate_limited else ""),
        file=sys.stderr,
    )

    if not records:
        return 2
    return 1 if partial else 0


if __name__ == "__main__":
    raise SystemExit(main())
