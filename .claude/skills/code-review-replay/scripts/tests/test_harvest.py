"""Unit + integration tests for the eval-dataset harvester."""

from __future__ import annotations

import json
import os
import sys
import tempfile
import unittest
from pathlib import Path

TEST_DIR = Path(__file__).resolve().parent
SCRIPT_DIR = TEST_DIR.parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

import harvest_lib as hl  # noqa: E402

KERNEL_3945_DIR = Path.home() / ".bramble" / "projects" / "kernel-3945"
BRAMBLE_OPS_PATH = (
    SCRIPT_DIR.parents[1] / "pr-polish" / "scripts" / "bramble_ops.py"
)


class ParseProjectDirNameTests(unittest.TestCase):
    def test_pr_numbered(self):
        self.assertEqual(hl.parse_project_dir_name("kernel-3945"), ("kernel", "3945"))
        self.assertEqual(hl.parse_project_dir_name("yoloswe-236"), ("yoloswe", "236"))
        self.assertEqual(hl.parse_project_dir_name("nebula-81"), ("nebula", "81"))

    def test_skips_doc_and_branch(self):
        self.assertIsNone(
            hl.parse_project_dir_name("kernel-doc-naming-rethink-cb9650558e82")
        )
        self.assertIsNone(
            hl.parse_project_dir_name("yoloswe-branch-feature-meeting-bot")
        )
        self.assertIsNone(
            hl.parse_project_dir_name("yoloswe-doc-meetingbot-architecture-e09ea41ac75e")
        )


class NormalizePathTests(unittest.TestCase):
    def test_leading_dot_slash(self):
        self.assertEqual(hl.normalize_path("./services/x.py"), "services/x.py")

    def test_none_and_empty(self):
        self.assertIsNone(hl.normalize_path(None))
        self.assertIsNone(hl.normalize_path(""))
        self.assertIsNone(hl.normalize_path("   "))

    def test_backslashes(self):
        self.assertEqual(hl.normalize_path("a\\b\\c.py"), "a/b/c.py")


class TopicTokenOverlapTests(unittest.TestCase):
    def test_zero_when_empty(self):
        self.assertEqual(hl.topic_token_overlap("", "anything"), 0.0)
        self.assertEqual(hl.topic_token_overlap("anything", ""), 0.0)

    def test_overlap(self):
        # Topic tokens are a subset of message tokens -> overlap > 0.5.
        topic = "deadline cache keyed by project"
        message = "deadline cache keyed by project identifier"
        self.assertGreater(hl.topic_token_overlap(topic, message), 0.5)

    def test_no_overlap(self):
        self.assertLess(
            hl.topic_token_overlap("flag toggle bool", "completely unrelated text"),
            0.3,
        )


class TopicTokenContainmentTests(unittest.TestCase):
    def test_zero_when_empty(self):
        self.assertEqual(hl.topic_token_containment("", "anything"), 0.0)
        self.assertEqual(hl.topic_token_containment("anything", ""), 0.0)

    def test_containment_ignores_extra_body_tokens(self):
        # The kernel-4189 shape: topic is a summary; the body has a verbatim
        # heading plus severity/description boilerplate. Symmetric Jaccard
        # drops below 0.5 here, but containment stays high.
        topic = "concurrent recreate leaves partial drops if one drop fails"
        body = (
            "### Concurrent recreate leaves partial drops\n\n"
            "**Medium Severity**\n\n<!-- DESCRIPTION START -->"
        )
        self.assertGreater(hl.topic_token_containment(topic, body), 0.6)
        self.assertLess(hl.topic_token_overlap(topic, body), 0.5)

    def test_full_containment(self):
        self.assertEqual(
            hl.topic_token_containment("alpha beta", "alpha beta gamma delta"),
            1.0,
        )


class DeriveIsRealIssueTests(unittest.TestCase):
    def test_table(self):
        self.assertIs(hl.derive_is_real_issue("fixed"), True)
        self.assertIs(hl.derive_is_real_issue("wont_fix"), True)
        self.assertIs(hl.derive_is_real_issue("false_positive"), False)
        self.assertIs(hl.derive_is_real_issue("stale"), False)
        self.assertIsNone(hl.derive_is_real_issue("ack"))
        self.assertIsNone(hl.derive_is_real_issue("flake"))
        self.assertIsNone(hl.derive_is_real_issue("pre_existing"))
        self.assertIsNone(hl.derive_is_real_issue(None))
        self.assertIsNone(hl.derive_is_real_issue("garbage"))


class MatchFindingTests(unittest.TestCase):
    def test_exact_match(self):
        finding = {
            "file": "services/x.py",
            "line": 94,
            "severity": "high",
            "message": "the preview restore deadline cache is still keyed by project",
        }
        actions = [
            {
                "source": "codex",
                "path": "services/x.py",
                "line": 94,
                "severity": "high",
                "topic": "deadline cache is still keyed",
                "action": "fixed",
            },
        ]
        match, strategy = hl.match_finding_to_action(finding, "codex", actions)
        self.assertEqual(strategy, "exact")
        self.assertIsNotNone(match)
        self.assertEqual(match["action"], "fixed")

    def test_sweep_source_wildcard_backend(self):
        finding = {
            "file": "x.py",
            "line": 10,
            "severity": "medium",
            "message": "msg",
        }
        actions = [
            {
                "source": "sweep",
                "path": "x.py",
                "line": 10,
                "severity": "medium",
                "topic": "...",
                "action": "fixed",
            },
        ]
        match, strategy = hl.match_finding_to_action(finding, "cursor", actions)
        self.assertEqual(strategy, "exact")
        self.assertEqual(match["source"], "sweep")

    def test_topic_path_line_fallback(self):
        finding = {
            "file": "./x.py",
            "line": 12,  # ±3 of action.line=10
            "severity": "high",  # severity mismatch -> Tier 1 fails
            "message": "the preview restore deadline cache is still keyed",
        }
        actions = [
            {
                "source": "codex",
                "path": "x.py",
                "line": 10,
                "severity": "medium",
                "topic": "deadline cache is still keyed",
                "action": "fixed",
            },
        ]
        match, strategy = hl.match_finding_to_action(finding, "codex", actions)
        self.assertEqual(strategy, "topic_path_line")
        self.assertEqual(match["action"], "fixed")

    def test_topic_only_when_no_path(self):
        finding = {
            "file": "x.py",
            "line": 99,
            "severity": "low",
            "message": "the preview restore deadline cache is still keyed by project",
        }
        actions = [
            {
                "source": "codex",
                "path": None,
                "line": None,
                "severity": None,
                "topic": "deadline cache is still keyed",
                "action": "wont_fix",
                "reason": "by design",
            },
        ]
        match, strategy = hl.match_finding_to_action(finding, "codex", actions)
        self.assertEqual(strategy, "topic_only")
        self.assertEqual(match["action"], "wont_fix")

    def test_no_match(self):
        finding = {
            "file": "x.py",
            "line": 1,
            "severity": "low",
            "message": "totally unrelated content here",
        }
        actions = [
            {
                "source": "codex",
                "path": "y.py",
                "line": 50,
                "severity": "high",
                "topic": "something else entirely about flags",
                "action": "fixed",
            },
        ]
        match, strategy = hl.match_finding_to_action(finding, "codex", actions)
        self.assertIsNone(match)
        self.assertEqual(strategy, "none")

    def test_fuzzy_tiers_reject_foreign_backend_source(self):
        # A codex finding must not inherit a cursor action's triage via the
        # fuzzier topic-based tiers — only same-backend (or wildcard) sources
        # are eligible.
        finding = {
            "file": "x.py",
            "line": 12,  # ±3 of action.line=10 -> would be topic_path_line
            "severity": "high",
            "message": "the preview restore deadline cache is still keyed",
        }
        cursor_action = {
            "source": "cursor",
            "path": "x.py",
            "line": 10,
            "severity": "medium",
            "topic": "deadline cache is still keyed",
            "action": "false_positive",
            "reason": "cursor said so",
        }
        match, strategy = hl.match_finding_to_action(
            finding, "codex", [cursor_action]
        )
        self.assertIsNone(match)
        self.assertEqual(strategy, "none")
        # Same action, queried as a cursor finding, still matches.
        match, strategy = hl.match_finding_to_action(
            finding, "cursor", [cursor_action]
        )
        self.assertEqual(strategy, "topic_path_line")
        self.assertEqual(match["action"], "false_positive")

    def test_fixed_preferred_over_false_positive_on_tie(self):
        finding = {
            "file": "x.py",
            "line": 10,
            "severity": "high",
            "message": "the preview restore deadline cache is still keyed",
        }
        actions = [
            {
                "source": "codex",
                "path": "x.py",
                "line": 10,
                "severity": "high",
                "topic": "deadline cache is still keyed",
                "action": "false_positive",
                "reason": "not actually",
            },
            {
                "source": "codex",
                "path": "x.py",
                "line": 10,
                "severity": "high",
                "topic": "deadline cache is still keyed",
                "action": "fixed",
            },
        ]
        match, _ = hl.match_finding_to_action(finding, "codex", actions)
        self.assertEqual(match["action"], "fixed")


class NormalizeRemoteUrlTests(unittest.TestCase):
    def test_ssh_to_https(self):
        self.assertEqual(
            hl.normalize_remote_url("git@github.com:anthropics/kernel.git"),
            "https://github.com/anthropics/kernel",
        )

    def test_https_unchanged(self):
        self.assertEqual(
            hl.normalize_remote_url("https://github.com/x/y.git"),
            "https://github.com/x/y",
        )

    def test_ssh_url_form(self):
        self.assertEqual(
            hl.normalize_remote_url("ssh://git@github.com/x/y.git"),
            "https://github.com/x/y",
        )


class ResolveDiffScopeTests(unittest.TestCase):
    """Local git first, GitHub fallback — with gh stubbed, never networked."""

    GH = {"head_sha": "h" * 40, "base_sha": "b" * 40}

    def _with_gh_stub(self, fn, *, responses, record_calls=None):
        import subprocess as _sp

        real_run = hl.subprocess.run

        def fake_run(cmd, *a, **kw):
            # Only intercept `gh`; real git must still run so the local-path
            # test exercises actual merge-base resolution.
            if cmd[0] != "gh":
                return real_run(cmd, *a, **kw)
            endpoint = cmd[-1]
            if record_calls is not None:
                record_calls.append(endpoint)
            for key, payload in responses.items():
                if key in endpoint:
                    return _sp.CompletedProcess(
                        cmd, 0, stdout=json.dumps(payload), stderr=""
                    )
            return _sp.CompletedProcess(cmd, 1, stdout="", stderr="no stub")

        hl.subprocess.run = fake_run
        try:
            return fn()
        finally:
            hl.subprocess.run = real_run

    def test_no_head_before_anywhere(self):
        scope = hl.resolve_diff_scope({}, repo_path=None)
        self.assertIsNone(scope.head_before)
        self.assertFalse(scope.merge_base_resolved)
        self.assertEqual(scope.merge_base_error, "no head_before")

    def test_head_before_falls_back_to_github_head_sha(self):
        # No local state round data, but a discovered GitHub row.
        calls: list[str] = []
        scope = self._with_gh_stub(
            lambda: hl.resolve_diff_scope(
                {}, repo_path=None, gh=self.GH, slug="org/repo",
                pr_number="42",
            ),
            responses={
                "compare/": {"merge_base_commit": {"sha": "m" * 40}},
                "files": [{"filename": "a.py"}, {"filename": "b.py"}],
            },
            record_calls=calls,
        )
        self.assertEqual(scope.head_before, "h" * 40)
        self.assertEqual(scope.merge_base_sha, "m" * 40)
        self.assertTrue(scope.merge_base_resolved)
        self.assertEqual(scope.merge_base_resolved_by, "github")
        self.assertEqual(scope.files_changed, ["a.py", "b.py"])
        self.assertEqual(scope.files_changed_resolved_by, "github")

    def test_local_git_wins_and_makes_no_api_call(self):
        # The whole point of preferring git: it must not spend API budget.
        calls: list[str] = []
        with tempfile.TemporaryDirectory() as td:
            repo = Path(td)
            hl.git(repo, "init", "-q")
            hl.git(repo, "config", "user.email", "t@t")
            hl.git(repo, "config", "user.name", "t")
            (repo / "f.txt").write_text("x")
            hl.git(repo, "add", "f.txt")
            hl.git(repo, "commit", "-qm", "one")
            head = hl.git(repo, "rev-parse", "HEAD").stdout.strip()
            scope = self._with_gh_stub(
                lambda: hl.resolve_diff_scope(
                    {"head_before": head},
                    repo_path=repo,
                    base_branch=head,  # merge-base of a commit with itself
                    gh=self.GH, slug="org/repo", pr_number="42",
                ),
                responses={}, record_calls=calls,
            )
        self.assertTrue(scope.merge_base_resolved)
        self.assertEqual(scope.merge_base_resolved_by, "git")
        self.assertEqual(calls, [])

    def test_github_error_is_appended_to_the_git_error(self):
        # The git error says why the local path failed (e.g. "fetch this
        # commit"), which is the actionable half — it must not be lost.
        scope = self._with_gh_stub(
            lambda: hl.resolve_diff_scope(
                {"head_before": "h" * 40},
                repo_path=None, gh=self.GH, slug="org/repo", pr_number="42",
            ),
            responses={"compare/": {"no_merge_base": True}},
        )
        self.assertFalse(scope.merge_base_resolved)
        self.assertIn("no repo mapping", scope.merge_base_error)
        self.assertIn("github:", scope.merge_base_error)

    def test_no_slug_means_local_only(self):
        scope = hl.resolve_diff_scope(
            {"head_before": "h" * 40}, repo_path=None,
        )
        self.assertFalse(scope.merge_base_resolved)
        self.assertEqual(scope.merge_base_resolved_by, "git")
        self.assertEqual(scope.files_changed, [])


class HarvestSourceTests(unittest.TestCase):
    """Provenance marking, and back-compat for pre-schema-3 records."""

    def test_missing_key_reads_as_pr_polish(self):
        # Every record written before schema 3 came from local pr-polish
        # state, so an absent key is not an error.
        self.assertEqual(hl.record_harvest_source({}), "pr-polish")
        self.assertEqual(
            hl.record_harvest_source({"schema_version": 2}), "pr-polish"
        )

    def test_explicit_values_round_trip(self):
        self.assertEqual(
            hl.record_harvest_source({"harvest_source": "github"}), "github"
        )
        self.assertEqual(
            hl.record_harvest_source({"harvest_source": "pr-polish"}),
            "pr-polish",
        )

    def test_unknown_value_falls_back(self):
        self.assertEqual(
            hl.record_harvest_source({"harvest_source": "nonsense"}),
            "pr-polish",
        )

    def test_index_entry_from_disk_defaults_old_records(self):
        with tempfile.TemporaryDirectory() as d:
            out_dir = Path(d)
            # A schema-2 record with no harvest_source key.
            (out_dir / "kernel-4189.json").write_text(json.dumps({
                "schema_version": 2,
                "pr": {"repo_name": "kernel", "pr_number": "4189",
                       "repo_url": "u", "pr_url": "u",
                       "completed": True, "total_rounds": 1},
                "harvested_rounds": [],
            }))
            index = hl.build_index(
                [], generated_at="t", harvester_sha="abc", out_dir=out_dir,
            )
            self.assertEqual(index["prs"][0]["harvest_source"], "pr-polish")


class GithubOnlyStateTests(unittest.TestCase):
    """The synthesized state must land on the canonical round unchanged."""

    def _gh(self):
        return {
            "repo_name": "kernel", "pr_number": "4102",
            "head_sha": "a" * 40, "base_sha": "b" * 40,
            "merged_at": "2026-07-29T00:00:00Z", "head_ref": "feat/x",
        }

    def test_yields_exactly_the_canonical_round(self):
        state = hl.github_only_state(self._gh())
        self.assertEqual(hl.select_rounds_to_harvest(state), [(1, "r1_only")])

    def test_head_before_comes_from_github(self):
        state = hl.github_only_state(self._gh())
        self.assertEqual(hl.get_round(state, 1)["head_before"], "a" * 40)

    def test_completed_so_include_incomplete_does_not_drop_it(self):
        # A merged PR is not "incomplete"; the lower fidelity is carried by
        # exit_reason + harvest_source, not by faking an unfinished run.
        state = hl.github_only_state(self._gh())
        self.assertTrue(state["completed"])
        self.assertEqual(state["exit_reason"], "github_only")

    def test_no_comment_actions_to_join(self):
        state = hl.github_only_state(self._gh())
        self.assertEqual(hl.get_round(state, 1)["comment_actions"], [])


class DiscoverGithubPRsTests(unittest.TestCase):
    """gh pr list parsing, filtering, and argv shape — never networked."""

    ROWS = [
        {"number": 8361, "mergedAt": "2026-07-30T22:19:17Z",
         "headRefOid": "a" * 40, "baseRefOid": "b" * 40,
         "headRefName": "feat/x", "title": "one"},
        {"number": 8356, "mergedAt": "2026-07-30T20:56:18Z",
         "headRefOid": "c" * 40, "baseRefOid": "d" * 40,
         "headRefName": "fix/y", "title": "two"},
    ]

    def _call(self, *, rows=None, rc=0, captured=None, **kwargs):
        import subprocess as _sp

        real_run = hl.subprocess.run

        def fake_run(cmd, *a, **kw):
            if captured is not None:
                captured.extend(cmd)
            return _sp.CompletedProcess(
                cmd, rc,
                stdout=json.dumps(self.ROWS if rows is None else rows),
                stderr="boom" if rc else "",
            )

        hl.subprocess.run = fake_run
        try:
            return hl.discover_github_prs("org/kernel", **kwargs)
        finally:
            hl.subprocess.run = real_run

    def test_parses_rows_into_candidates(self):
        rows, err = self._call()
        self.assertIsNone(err)
        self.assertEqual(len(rows), 2)
        self.assertEqual(rows[0]["repo_name"], "kernel")
        self.assertEqual(rows[0]["pr_number"], "8361")
        self.assertEqual(rows[0]["head_sha"], "a" * 40)
        self.assertEqual(rows[0]["base_sha"], "b" * 40)
        self.assertEqual(rows[0]["slug"], "org/kernel")

    def test_exclude_filters_already_harvested(self):
        rows, _ = self._call(exclude={"kernel-8361"})
        self.assertEqual([r["pr_number"] for r in rows], ["8356"])

    def test_merged_since_becomes_a_search_arg(self):
        captured: list[str] = []
        self._call(captured=captured, merged_since="2026-07-23")
        self.assertIn("--search", captured)
        self.assertIn("merged:>=2026-07-23", captured)

    def test_no_merged_since_sends_no_search(self):
        captured: list[str] = []
        self._call(captured=captured)
        self.assertNotIn("--search", captured)

    def test_failure_returns_error_not_raise(self):
        rows, err = self._call(rc=1)
        self.assertEqual(rows, [])
        self.assertIn("exit 1", err)

    def test_rows_without_a_number_are_skipped(self):
        rows, _ = self._call(rows=[{"title": "no number"}])
        self.assertEqual(rows, [])


class AlreadyHarvestedTests(unittest.TestCase):
    def test_lists_record_stems_and_skips_index(self):
        with tempfile.TemporaryDirectory() as d:
            out = Path(d)
            (out / "kernel-1.json").write_text("{}")
            (out / "kernel-2.json").write_text("{}")
            (out / "index.json").write_text("{}")
            (out / "notes.txt").write_text("x")
            self.assertEqual(
                hl.already_harvested(out), {"kernel-1", "kernel-2"}
            )

    def test_missing_dir_is_empty(self):
        self.assertEqual(
            hl.already_harvested(Path("/nonexistent/xyz")), set()
        )


class BuildPRRecordGithubSourceTests(unittest.TestCase):
    """End-to-end: a record for a PR with no local pr-polish state at all."""

    GH = {
        "repo_name": "kernel", "pr_number": "4102", "slug": "org/kernel",
        "head_sha": "h" * 40, "base_sha": "b" * 40,
        "merged_at": "2026-07-29T00:00:00Z", "head_ref": "feat/x",
    }

    def _build(self):
        import subprocess as _sp

        real_run = hl.subprocess.run

        def fake_run(cmd, *a, **kw):
            if cmd[0] != "gh":
                return real_run(cmd, *a, **kw)
            endpoint = cmd[-1]
            if "compare/" in endpoint:
                payload = {"merge_base_commit": {"sha": "m" * 40}}
            elif "files" in endpoint:
                payload = [{"filename": "svc/a.py"}]
            else:
                payload = []
            return _sp.CompletedProcess(
                cmd, 0, stdout=json.dumps(payload), stderr=""
            )

        hl.subprocess.run = fake_run
        try:
            return hl.build_pr_record(
                None,  # no local state dir
                "kernel",
                "4102",
                repo_map=hl.RepoMap({}),
                pr_summary="Fix the thing\n\nBody text.",
                harvester_sha="abc",
                harvested_at="2026-07-30T00:00:00Z",
                bramble_ops_path=Path("/nonexistent/bramble_ops.py"),
                fetch_attempted=False,
                harvest_source="github",
                gh=self.GH,
            )
        finally:
            hl.subprocess.run = real_run

    def test_builds_a_usable_single_round_record(self):
        rec = self._build()
        self.assertIsNotNone(rec)
        self.assertEqual(rec.harvest_source, "github")
        self.assertEqual(rec.schema_version, 3)
        self.assertEqual(len(rec.harvested_rounds), 1)
        rnd = rec.harvested_rounds[0]
        self.assertEqual(rnd.signal_tier, "r1_only")
        self.assertEqual(rnd.head_before, "h" * 40)

    def test_diff_scope_resolves_via_github(self):
        rnd = self._build().harvested_rounds[0]
        self.assertTrue(rnd.merge_base_resolved)
        self.assertEqual(rnd.merge_base_sha, "m" * 40)
        self.assertEqual(rnd.merge_base_resolved_by, "github")
        self.assertEqual(rnd.files_changed, ["svc/a.py"])
        self.assertEqual(rnd.files_changed_resolved_by, "github")

    def test_no_review_runs_without_local_envelopes(self):
        # The documented fidelity cost: no local envelopes exist, so
        # --include-harvested contributes nothing for this record.
        self.assertEqual(self._build().harvested_rounds[0].review_runs, [])
        self.assertFalse(
            self._build().harvested_rounds[0].scope_hints_present
        )

    def test_goal_text_is_the_pr_summary(self):
        rnd = self._build().harvested_rounds[0]
        self.assertEqual(rnd.goal_text, "Fix the thing\n\nBody text.")
        self.assertTrue(rnd.goal_recoverable)

    def test_urls_fall_back_to_the_discovered_slug(self):
        # No local checkout means get_repo_url returns nothing.
        rec = self._build()
        self.assertEqual(rec.pr["repo_url"], "https://github.com/org/kernel")
        self.assertEqual(
            rec.pr["pr_url"], "https://github.com/org/kernel/pull/4102"
        )

    def test_requires_gh_row(self):
        with self.assertRaises(ValueError):
            hl.build_pr_record(
                None, "kernel", "4102",
                repo_map=hl.RepoMap({}), pr_summary=None,
                harvester_sha="abc", harvested_at="t",
                bramble_ops_path=Path("/nonexistent"),
                harvest_source="github", gh=None,
            )


class CommitScopedRoundsTests(unittest.TestCase):
    """Each comment must be judged against the commit it actually reviewed.

    Pinning every comment to the PR's final head inverts the ground truth: a
    bot correctly reports a bug, the author fixes it, and a judge reading the
    post-fix code records the claim as a false positive.
    """

    def _repo(self, td):
        repo = Path(td)
        hl.git(repo, "init", "-q")
        hl.git(repo, "config", "user.email", "t@t")
        hl.git(repo, "config", "user.name", "t")
        shas = []
        for i in range(3):
            (repo / f"f{i}.py").write_text("x\n")
            hl.git(repo, "add", "-A")
            hl.git(repo, "commit", "-qm", f"c{i}")
            shas.append(hl.git(repo, "rev-parse", "HEAD").stdout.strip())
        return repo, shas

    def _c(self, sha, path="a.py"):
        return {"source": "github-inline", "original_commit_id": sha,
                "path": path, "body": "x"}

    def test_one_round_per_reviewed_commit(self):
        with tempfile.TemporaryDirectory() as td:
            repo, shas = self._repo(td)
            rounds = hl.commit_scoped_rounds(
                {"head_sha": shas[-1]},
                [self._c(shas[0]), self._c(shas[1]), self._c(shas[1])],
                repo_path=repo,
            )
            self.assertEqual(len(rounds), 2)
            self.assertEqual([len(r["comment_actions"]) for r in rounds], [1, 2])

    def test_rounds_are_ordered_oldest_first(self):
        # Round 1 must be the earliest reviewed state, so the r1 tier keeps
        # meaning "fresh eyes".
        with tempfile.TemporaryDirectory() as td:
            repo, shas = self._repo(td)
            rounds = hl.commit_scoped_rounds(
                {"head_sha": shas[-1]},
                [self._c(shas[2]), self._c(shas[0])],
                repo_path=repo,
            )
            self.assertEqual(rounds[0]["head_before"], shas[0])
            self.assertEqual(rounds[1]["head_before"], shas[2])

    def test_unreachable_sha_is_flagged_not_hidden(self):
        # Force-pushed commits (~8% of the pilot corpus) cannot be judged at
        # the state they reviewed; folding them in silently would look like
        # same-state review.
        with tempfile.TemporaryDirectory() as td:
            repo, shas = self._repo(td)
            rounds = hl.commit_scoped_rounds(
                {"head_sha": shas[-1]},
                [self._c(shas[0]), self._c("0" * 40)],
                repo_path=repo,
            )
            last = rounds[-1]
            self.assertTrue(last["head_unreachable"])
            self.assertEqual(last["head_before"], shas[-1])
            self.assertEqual(len(last["comment_actions"]), 1)

    def test_comment_without_a_sha_is_an_orphan(self):
        with tempfile.TemporaryDirectory() as td:
            repo, shas = self._repo(td)
            c = self._c(shas[0]); del c["original_commit_id"]
            rounds = hl.commit_scoped_rounds(
                {"head_sha": shas[-1]}, [c], repo_path=repo)
            self.assertEqual(len(rounds), 1)
            self.assertTrue(rounds[0]["head_unreachable"])

    def test_no_comments_still_yields_one_round(self):
        with tempfile.TemporaryDirectory() as td:
            repo, shas = self._repo(td)
            rounds = hl.commit_scoped_rounds(
                {"head_sha": shas[-1]}, [], repo_path=repo)
            self.assertEqual(len(rounds), 1)
            self.assertEqual(rounds[0]["head_before"], shas[-1])

    def test_every_round_is_kept_not_just_first_and_last(self):
        # The default rule keeps only R1 + final, which would discard most of
        # the review states the SHAs were recovered for.
        with tempfile.TemporaryDirectory() as td:
            repo, shas = self._repo(td)
            state = hl.github_only_state(
                {"head_sha": shas[-1]},
                [self._c(s) for s in shas],
                repo_path=repo,
            )
            picked = hl.select_rounds_to_harvest(state)
            self.assertEqual(len(picked), len(state["rounds"]))
            self.assertEqual(picked[0][1], "r1")

    def test_opt_in_only(self):
        # Without comments the legacy single-round shape is preserved.
        state = hl.github_only_state({"head_sha": "a" * 40})
        self.assertEqual(len(state["rounds"]), 1)
        self.assertEqual(
            hl.select_rounds_to_harvest(state), [(1, "r1_only")]
        )


class EnvelopePathTests(unittest.TestCase):
    """pr-polish writes envelopes flat (``rN/``) or per-attempt (``rN/aM/``).

    Both layouts are live in ``~/.bramble/projects``; reading only the flat
    one silently harvested zero review_runs for every recent PR.
    """

    def _state_dir(self, td: str) -> Path:
        return Path(td) / "kernel-1"

    def test_flat_layout(self):
        with tempfile.TemporaryDirectory() as td:
            state = self._state_dir(td)
            (state / "r1").mkdir(parents=True)
            env = state / "r1" / "codex-envelope.json"
            env.write_text("{}")
            self.assertEqual(hl._envelope_path(state, 1, "codex"), env)

    def test_attempt_layout(self):
        with tempfile.TemporaryDirectory() as td:
            state = self._state_dir(td)
            (state / "r1" / "a1").mkdir(parents=True)
            env = state / "r1" / "a1" / "codex-envelope.json"
            env.write_text("{}")
            self.assertEqual(hl._envelope_path(state, 1, "codex"), env)

    def test_last_attempt_wins(self):
        # A retried round leaves an envelope per attempt; the last is the
        # outcome pr-polish acted on. a10 must sort after a2, not before.
        with tempfile.TemporaryDirectory() as td:
            state = self._state_dir(td)
            for a in ("a1", "a2", "a10"):
                (state / "r1" / a).mkdir(parents=True)
                (state / "r1" / a / "codex-envelope.json").write_text("{}")
            self.assertEqual(
                hl._envelope_path(state, 1, "codex"),
                state / "r1" / "a10" / "codex-envelope.json",
            )

    def test_falls_back_to_last_attempt_that_has_one(self):
        # A backend that failed to produce an envelope on the final attempt
        # still has its earlier one harvested rather than being dropped.
        with tempfile.TemporaryDirectory() as td:
            state = self._state_dir(td)
            for a in ("a1", "a2"):
                (state / "r1" / a).mkdir(parents=True)
            env = state / "r1" / "a1" / "cursor-envelope.json"
            env.write_text("{}")
            self.assertEqual(hl._envelope_path(state, 1, "cursor"), env)

    def test_missing_envelope_returns_round_level_path(self):
        with tempfile.TemporaryDirectory() as td:
            state = self._state_dir(td)
            (state / "r1").mkdir(parents=True)
            self.assertFalse(hl._envelope_path(state, 1, "gemini").exists())

    def test_scope_hints_stay_at_round_level(self):
        with tempfile.TemporaryDirectory() as td:
            state = self._state_dir(td)
            (state / "r1" / "a1").mkdir(parents=True)
            self.assertFalse(hl._scope_hints_present(state, 1))
            (state / "r1" / "scope-hints.json").write_text("{}")
            self.assertTrue(hl._scope_hints_present(state, 1))


class SelectRoundsTests(unittest.TestCase):
    def test_completed_multi_round(self):
        state = {
            "completed": True,
            "rounds": [{"n": 1}, {"n": 2}, {"n": 3}],
        }
        self.assertEqual(
            hl.select_rounds_to_harvest(state), [(1, "r1"), (3, "final")]
        )

    def test_incomplete_multi_round(self):
        state = {
            "completed": False,
            "rounds": [{"n": 1}, {"n": 2}],
        }
        self.assertEqual(
            hl.select_rounds_to_harvest(state), [(1, "r1"), (2, "final_incomplete")]
        )

    def test_single_round(self):
        state = {"completed": True, "rounds": [{"n": 1}]}
        self.assertEqual(hl.select_rounds_to_harvest(state), [(1, "r1_only")])

    def test_empty(self):
        self.assertEqual(hl.select_rounds_to_harvest({"rounds": []}), [])
        self.assertEqual(hl.select_rounds_to_harvest({}), [])


class ComputeMergeBaseTests(unittest.TestCase):
    def test_no_repo_mapping(self):
        sha, resolved, err = hl.compute_merge_base(None, "abc123")
        self.assertIsNone(sha)
        self.assertFalse(resolved)
        self.assertIn("no repo mapping", err)

    def test_nonexistent_repo_path(self):
        sha, resolved, err = hl.compute_merge_base(
            Path("/nonexistent/path"), "abc123"
        )
        self.assertIsNone(sha)
        self.assertFalse(resolved)

    def test_bogus_commit_in_real_repo(self):
        # Use the yoloswe worktree itself as a real repo.
        repo = Path(__file__).resolve().parents[4]
        sha, resolved, err = hl.compute_merge_base(
            repo, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
        )
        self.assertIsNone(sha)
        self.assertFalse(resolved)


class ReconstructGoalTextTests(unittest.TestCase):
    def test_r1_with_pr_summary(self):
        text, ok = hl.reconstruct_goal_text(
            state={"rounds": []},
            round_n=1,
            head_before=None,
            pr_summary="some PR summary",
            bramble_ops_path=BRAMBLE_OPS_PATH,
            repo_path=None,
        )
        self.assertEqual(text, "some PR summary")
        self.assertTrue(ok)

    def test_r1_without_pr_summary(self):
        text, ok = hl.reconstruct_goal_text(
            state={"rounds": []},
            round_n=1,
            head_before=None,
            pr_summary=None,
            bramble_ops_path=BRAMBLE_OPS_PATH,
            repo_path=None,
        )
        self.assertIsNone(text)
        self.assertFalse(ok)


@unittest.skipUnless(
    KERNEL_3945_DIR.exists() and BRAMBLE_OPS_PATH.exists(),
    "kernel-3945 fixture or bramble_ops.py not present",
)
class BuildPRRecordKernel3945Snapshot(unittest.TestCase):
    """Integration test against the real ~/.bramble/projects/kernel-3945."""

    def test_snapshot(self):
        record = hl.build_pr_record(
            KERNEL_3945_DIR,
            "kernel",
            "3945",
            repo_map=hl.RepoMap(),  # no repo mapping → merge_base unresolved
            pr_summary=None,
            harvester_sha="testsha",
            harvested_at="2026-05-20T00:00:00Z",
            bramble_ops_path=BRAMBLE_OPS_PATH,
            fetch_attempted=False,  # offline integration test
        )
        self.assertIsNotNone(record)
        self.assertEqual(record.schema_version, 3)
        # A record built from a local pr-polish dir is full fidelity.
        self.assertEqual(record.harvest_source, "pr-polish")
        self.assertEqual(record.pr["repo_name"], "kernel")
        self.assertEqual(record.pr["pr_number"], "3945")
        # kernel-3945 had 5 rounds, completed=True → R1 + R5
        self.assertEqual(len(record.harvested_rounds), 2)
        r1, r_final = record.harvested_rounds
        self.assertEqual(r1.round, 1)
        self.assertEqual(r1.signal_tier, "r1")
        self.assertEqual(r_final.round, 5)
        self.assertEqual(r_final.signal_tier, "final")

        # Fetch was skipped → github comments come from the state fallback.
        self.assertEqual(record.pr_comments_attribution_basis, "no_timestamp")

        # R1 should have at least codex + cursor review_runs.
        backends_r1 = {rr.backend for rr in r1.review_runs}
        self.assertIn("codex", backends_r1)

        # Cursor R1 envelope was status=error in the real fixture.
        cursor = next((rr for rr in r1.review_runs if rr.backend == "cursor"), None)
        if cursor is not None:
            self.assertIn(cursor.envelope_status, {"ok", "error"})
            if cursor.envelope_status == "error":
                self.assertEqual(cursor.findings, [])

        # At least one finding ground-truthed as 'fixed' across the run.
        all_actions = [
            f.ground_truth.action
            for rr in r1.review_runs
            for f in rr.findings
        ]
        self.assertIn("fixed", all_actions)

        # No repo mapping → merge_base must be unresolved for both rounds.
        for hr in record.harvested_rounds:
            self.assertFalse(hr.merge_base_resolved)
            self.assertIsNone(hr.merge_base_sha)

    def test_write_round_trip(self):
        record = hl.build_pr_record(
            KERNEL_3945_DIR,
            "kernel",
            "3945",
            repo_map=hl.RepoMap(),
            pr_summary=None,
            harvester_sha="testsha",
            harvested_at="2026-05-20T00:00:00Z",
            bramble_ops_path=BRAMBLE_OPS_PATH,
            fetch_attempted=False,
        )
        with tempfile.TemporaryDirectory() as td:
            out = Path(td)
            path = hl.write_pr_record(out, record)
            self.assertTrue(path.exists())
            loaded = json.loads(path.read_text())
            self.assertEqual(loaded["pr"]["pr_number"], "3945")
            self.assertEqual(len(loaded["harvested_rounds"]), 2)
            self.assertEqual(loaded["schema_version"], 3)
            self.assertEqual(loaded["harvest_source"], "pr-polish")
            self.assertEqual(
                loaded["pr_comments_attribution_basis"], "no_timestamp"
            )


class IndexCommentVerdictsTests(unittest.TestCase):
    def test_dedup_keeps_earliest_round(self):
        # comment id 999 recurs in rounds 8, 10, 12 (the yoloswe-236 shape).
        # The round-8 verdict is the real one; later rows are re-fetched echoes.
        state = {
            "rounds": [
                {
                    "n": 8,
                    "comment_actions": [
                        {
                            "source": "github-inline",
                            "comment_id": 999,
                            "action": "fixed",
                            "reason": None,
                        }
                    ],
                },
                {
                    "n": 10,
                    "comment_actions": [
                        {
                            "source": "github-inline",
                            "comment_id": 999,
                            "action": "ack",
                            "reason": "echo",
                        }
                    ],
                },
                {
                    "n": 12,
                    "comment_actions": [
                        {
                            "source": "github-inline",
                            "comment_id": 999,
                            "action": "ack",
                            "reason": "echo",
                        }
                    ],
                },
            ]
        }
        idx = hl.index_comment_verdicts(state)
        self.assertEqual(set(idx.by_id.keys()), {999})
        self.assertEqual(idx.by_id[999]["action"], "fixed")
        self.assertEqual(idx.by_id[999]["triaged_in_round"], 8)
        self.assertEqual(idx.by_topic, [])

    def test_null_id_goes_to_topic_index(self):
        state = {
            "rounds": [
                {
                    "n": 1,
                    "comment_actions": [
                        {"source": "codex", "comment_id": None, "action": "fixed"},
                        {
                            "source": "github-inline",
                            "comment_id": None,
                            "action": "fixed",
                            "topic": "concurrent recreate leaves partial drops",
                        },
                        {
                            "source": "github-review",
                            "comment_id": 5,
                            "action": "wont_fix",
                        },
                    ],
                }
            ]
        }
        idx = hl.index_comment_verdicts(state)
        # Keyed index: only the github comment with a real id.
        self.assertEqual(set(idx.by_id.keys()), {5})
        self.assertEqual(idx.by_id[5]["action"], "wont_fix")
        # Topic index: the null-id github row (codex non-github row excluded).
        self.assertEqual(len(idx.by_topic), 1)
        self.assertEqual(idx.by_topic[0]["action"], "fixed")
        self.assertEqual(idx.by_topic[0]["source"], "github-inline")


class AttributeCommentToRoundTests(unittest.TestCase):
    ROUND_TIMES = [
        (1, "2026-05-07T00:00:00Z"),
        (4, "2026-05-07T06:00:00Z"),
        (6, "2026-05-07T12:00:00Z"),
    ]

    def test_inside_middle_window(self):
        self.assertEqual(
            hl.attribute_comment_to_round(
                "2026-05-07T07:30:00Z", self.ROUND_TIMES
            ),
            4,
        )

    def test_before_first_round(self):
        self.assertEqual(
            hl.attribute_comment_to_round(
                "2026-05-06T23:00:00Z", self.ROUND_TIMES
            ),
            1,
        )

    def test_after_last_round(self):
        self.assertEqual(
            hl.attribute_comment_to_round(
                "2026-05-09T00:00:00Z", self.ROUND_TIMES
            ),
            6,
        )

    def test_empty_created_at_goes_last(self):
        self.assertEqual(
            hl.attribute_comment_to_round(None, self.ROUND_TIMES), 6
        )

    def test_no_round_times(self):
        self.assertIsNone(hl.attribute_comment_to_round("2026-05-07T00:00:00Z", []))

    def test_unresolved_times_attribute_last(self):
        # All boundary times None -> attribute everything to the last round.
        rt = [(1, None), (2, None), (3, None)]
        self.assertEqual(hl.attribute_comment_to_round("2026-05-07Z", rt), 3)

    def test_mixed_timezone_offsets_compared_chronologically(self):
        # Boundary times in committer-local offsets (git %cI), comment in
        # UTC Z (GitHub). Raw string compare would mis-order these; epoch
        # compare must not. round 1 boundary 2026-05-07T00:00:00-05:00 ==
        # 05:00Z; round 2 boundary 2026-05-07T10:00:00+02:00 == 08:00Z.
        rt = [
            (1, "2026-05-07T00:00:00-05:00"),  # 05:00Z
            (2, "2026-05-07T10:00:00+02:00"),  # 08:00Z
        ]
        # Comment at 06:00Z falls inside round 1's window (05:00Z..08:00Z).
        self.assertEqual(
            hl.attribute_comment_to_round("2026-05-07T06:00:00Z", rt), 1
        )
        # Comment at 09:00Z is after round 2's boundary.
        self.assertEqual(
            hl.attribute_comment_to_round("2026-05-07T09:00:00Z", rt), 2
        )
        # Comment at 04:00Z is before round 1's boundary -> round 1.
        self.assertEqual(
            hl.attribute_comment_to_round("2026-05-07T04:00:00Z", rt), 1
        )

    def test_unparseable_created_at_attributes_last(self):
        self.assertEqual(
            hl.attribute_comment_to_round("not-a-timestamp", self.ROUND_TIMES),
            6,
        )


class FoldCommentToHarvestedRoundTests(unittest.TestCase):
    def test_middle_round_folds_to_final(self):
        # attributed round 4, harvested rounds {1, 7} -> emit on final (7).
        self.assertEqual(hl.fold_comment_to_harvested_round(4, [1, 7]), 7)

    def test_round_one_folds_to_r1(self):
        self.assertEqual(hl.fold_comment_to_harvested_round(1, [1, 7]), 1)

    def test_before_r1_folds_to_r1(self):
        self.assertEqual(hl.fold_comment_to_harvested_round(1, [1, 7]), 1)

    def test_none_attribution_folds_to_final(self):
        self.assertEqual(hl.fold_comment_to_harvested_round(None, [1, 7]), 7)

    def test_no_harvested_rounds(self):
        self.assertIsNone(hl.fold_comment_to_harvested_round(4, []))


class BuildPRCommentsTests(unittest.TestCase):
    def _state(self):
        return {
            "rounds": [
                {
                    "n": 1,
                    "head_before": "sha1",
                    "comment_actions": [
                        {
                            "source": "github-inline",
                            "comment_id": 100,
                            "action": "fixed",
                            "reason": None,
                        }
                    ],
                },
                {"n": 4, "head_before": "sha4", "comment_actions": []},
            ]
        }

    def test_fetch_skipped_uses_state_fallback(self):
        rows, basis = hl.build_pr_comments(
            self._state(), [], None, fetch_attempted=False
        )
        self.assertEqual(basis, "no_timestamp")
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["comment_id"], 100)
        self.assertEqual(rows[0]["action"], "fixed")
        self.assertEqual(rows[0]["attributed_round"], 1)
        self.assertIsNone(rows[0]["body"])  # no fetched body in fallback

    def test_unmapped_repo_fallback(self):
        # Fetch produced comments but no repo -> can't resolve round times.
        fetched = [
            {
                "id": 100,
                "source": "github-inline",
                "author": "cursor[bot]",
                "is_bot": True,
                "path": "a.py",
                "line": 5,
                "body": "issue here",
                "created_at": "2026-05-07T07:00:00Z",
                "original_commit_id": "abc",
            },
            {
                "id": 200,  # GitHub comment pr-polish never triaged
                "source": "github-issue",
                "author": "human",
                "is_bot": False,
                "path": None,
                "line": None,
                "body": "looks good",
                "created_at": "2026-05-07T08:00:00Z",
                "original_commit_id": None,
            },
        ]
        rows, basis = hl.build_pr_comments(
            self._state(), fetched, None, fetch_attempted=True
        )
        self.assertEqual(basis, "unmapped_repo_fallback")
        self.assertEqual(len(rows), 2)
        by_id = {r["comment_id"]: r for r in rows}
        # Verdict joined for the triaged comment.
        self.assertEqual(by_id[100]["action"], "fixed")
        self.assertEqual(by_id[100]["body"], "issue here")
        # Untriaged comment is still emitted, action null (complete census).
        self.assertIsNone(by_id[200]["action"])
        # Unmapped repo -> everything attributes to round 1.
        self.assertEqual(by_id[100]["attributed_round"], 1)
        self.assertEqual(by_id[200]["attributed_round"], 1)

    def test_topic_substring_fallback_join(self):
        # pr-polish recorded the verdict with comment_id=null (older run);
        # the fetched comment carries a real id but the join must still
        # succeed by matching the recorded topic as a substring of the body.
        state = {
            "rounds": [
                {
                    "n": 1,
                    "head_before": "sha1",
                    "comment_actions": [
                        {
                            "source": "github-inline",
                            "comment_id": None,
                            "action": "wont_fix",
                            "reason": "by design",
                            "topic": "concurrent recreate leaves partial drops",
                        }
                    ],
                }
            ]
        }
        fetched = [
            {
                "id": 555,
                "source": "github-inline",
                "author": "cursor[bot]",
                "is_bot": True,
                "path": "deploy.py",
                "line": None,
                "body": "### Concurrent recreate leaves partial drops\n\nMedium",
                "created_at": "2026-05-07T07:00:00Z",
                "original_commit_id": "abc",
            }
        ]
        rows, _ = hl.build_pr_comments(
            state, fetched, None, fetch_attempted=True
        )
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["action"], "wont_fix")
        self.assertEqual(rows[0]["reason"], "by design")

    def test_topic_containment_fallback_join(self):
        # Recorded topic is a summary phrase LONGER than the body's verbatim
        # heading -> no substring match, but token containment recovers it.
        state = {
            "rounds": [
                {
                    "n": 1,
                    "head_before": "sha1",
                    "comment_actions": [
                        {
                            "source": "github-inline",
                            "comment_id": None,
                            "action": "wont_fix",
                            "reason": "by design",
                            "topic": (
                                "concurrent recreate leaves partial drops "
                                "if one drop fails"
                            ),
                        }
                    ],
                }
            ]
        }
        fetched = [
            {
                "id": 555,
                "source": "github-inline",
                "author": "cursor[bot]",
                "is_bot": True,
                "path": "deploy.py",
                "line": None,
                "body": (
                    "### Concurrent recreate leaves partial drops\n\n"
                    "**Medium Severity**\n\n<!-- DESCRIPTION START -->"
                ),
                "created_at": "2026-05-07T07:00:00Z",
                "original_commit_id": "abc",
            }
        ]
        rows, _ = hl.build_pr_comments(
            state, fetched, None, fetch_attempted=True
        )
        self.assertEqual(rows[0]["action"], "wont_fix")

    def test_topic_fallback_consumed_once(self):
        # One null-id verdict must not join to two fetched comments.
        state = {
            "rounds": [
                {
                    "n": 1,
                    "head_before": "sha1",
                    "comment_actions": [
                        {
                            "source": "github-inline",
                            "comment_id": None,
                            "action": "fixed",
                            "topic": "shared topic phrase",
                        }
                    ],
                }
            ]
        }
        fetched = [
            {
                "id": 1,
                "source": "github-inline",
                "author": "a",
                "is_bot": True,
                "path": "p",
                "line": 1,
                "body": "shared topic phrase here",
                "created_at": "2026-05-07T07:00:00Z",
                "original_commit_id": None,
            },
            {
                "id": 2,
                "source": "github-inline",
                "author": "a",
                "is_bot": True,
                "path": "p",
                "line": 2,
                "body": "shared topic phrase also here",
                "created_at": "2026-05-07T08:00:00Z",
                "original_commit_id": None,
            },
        ]
        rows, _ = hl.build_pr_comments(
            state, fetched, None, fetch_attempted=True
        )
        joined = [r for r in rows if r["action"] == "fixed"]
        self.assertEqual(len(joined), 1)


@unittest.skipUnless(
    KERNEL_3945_DIR.exists() and BRAMBLE_OPS_PATH.exists(),
    "kernel-3945 fixture or bramble_ops.py not present",
)
class BuildPRRecordMiddleRoundFoldTests(unittest.TestCase):
    """A github comment authored during a middle round must not be dropped."""

    def test_middle_round_comment_folds_onto_final(self):
        # kernel-3945 had 5 rounds -> harvested rounds are {1, 5}. A fetched
        # comment with no matching round time (unmapped repo) folds onto r1;
        # to exercise the *final*-fold path we attribute via no_timestamp
        # fallback is not enough -> use a synthetic comment whose verdict was
        # recorded in a middle round so the state fallback attributes it there.
        # Simplest deterministic check: unmapped fetch -> all fold onto r1,
        # and the comment is present (never dropped).
        fetched = [
            {
                "id": 77777,
                "source": "github-inline",
                "author": "cursor[bot]",
                "is_bot": True,
                "path": "x.py",
                "line": 1,
                "body": "synthetic middle-round comment",
                "created_at": "2026-05-07T07:00:00Z",
                "original_commit_id": "abc",
            }
        ]
        record = hl.build_pr_record(
            KERNEL_3945_DIR,
            "kernel",
            "3945",
            repo_map=hl.RepoMap(),  # unmapped -> unmapped_repo_fallback
            pr_summary=None,
            harvester_sha="t",
            harvested_at="2026-05-20T00:00:00Z",
            bramble_ops_path=BRAMBLE_OPS_PATH,
            fetched_pr_comments=fetched,
            fetch_attempted=True,
        )
        self.assertIsNotNone(record)
        self.assertEqual(
            record.pr_comments_attribution_basis, "unmapped_repo_fallback"
        )
        # The synthetic comment must appear exactly once across harvested rounds.
        all_ids = [
            c.get("comment_id")
            for hr in record.harvested_rounds
            for c in hr.raw_comment_actions
        ]
        self.assertEqual(all_ids.count(77777), 1)
        # Non-github comment_actions still present on their own rounds.
        for hr in record.harvested_rounds:
            for c in hr.raw_comment_actions:
                if c.get("source") not in hl.GITHUB_SOURCES:
                    # Reviewer findings keep their original schema (no
                    # attributed_round key).
                    self.assertNotIn("attributed_round", c)


class RateLimitBackoffTests(unittest.TestCase):
    """_run_gh backs off through a rate limit instead of failing instantly.

    A bulk harvest reliably trips GitHub's *secondary* rate limit while the
    core quota still reads thousands remaining. Without backoff, every
    remaining PR fails identically and degrades to the state-recorded
    comment set — producing records that look harvested but carry no
    external review census.
    """

    RATE_ERR = (
        "gh: API rate limit exceeded for user ID 5767792. (HTTP 403)"
    )

    def _stub(self, outcomes, slept):
        """outcomes: list of (returncode, stderr) consumed per attempt."""
        import subprocess as _sp

        calls = {"n": 0}

        def fake_run(cmd, *a, **kw):
            i = min(calls["n"], len(outcomes) - 1)
            rc, err = outcomes[i]
            calls["n"] += 1
            return _sp.CompletedProcess(
                cmd, rc, stdout="[]" if rc == 0 else "", stderr=err
            )

        return fake_run, calls

    def test_detects_rate_limit_message(self):
        self.assertTrue(hl.is_rate_limit_error(self.RATE_ERR))
        self.assertTrue(
            hl.is_rate_limit_error("You have exceeded a secondary RATE LIMIT EXCEEDED")
        )
        self.assertFalse(hl.is_rate_limit_error("404 Not Found"))
        self.assertFalse(hl.is_rate_limit_error(None))

    def test_retries_then_succeeds(self):
        slept = []
        fake_run, calls = self._stub(
            [(1, self.RATE_ERR), (1, self.RATE_ERR), (0, "")], slept
        )
        real = hl.subprocess.run
        hl.subprocess.run = fake_run
        try:
            res, err = hl._run_gh(
                ["gh", "api", "x"], "x", sleep=slept.append
            )
        finally:
            hl.subprocess.run = real
        self.assertIsNone(err)
        self.assertEqual(res.returncode, 0)
        self.assertEqual(calls["n"], 3)
        # Backed off before each retry, on the documented schedule.
        self.assertEqual(slept, list(hl._RATE_LIMIT_BACKOFF_S[:2]))

    def test_non_rate_limit_error_does_not_retry(self):
        slept = []
        fake_run, calls = self._stub([(1, "404 Not Found")], slept)
        real = hl.subprocess.run
        hl.subprocess.run = fake_run
        try:
            _, err = hl._run_gh(["gh", "api", "x"], "x", sleep=slept.append)
        finally:
            hl.subprocess.run = real
        self.assertIn("404", err)
        self.assertEqual(calls["n"], 1)
        self.assertEqual(slept, [])

    def test_exhausted_retries_returns_the_rate_limit_error(self):
        slept = []
        fake_run, calls = self._stub([(1, self.RATE_ERR)], slept)
        real = hl.subprocess.run
        hl.subprocess.run = fake_run
        try:
            _, err = hl._run_gh(["gh", "api", "x"], "x", sleep=slept.append)
        finally:
            hl.subprocess.run = real
        # Caller must still see a rate-limit error so it can stop the run.
        self.assertTrue(hl.is_rate_limit_error(err))
        self.assertEqual(calls["n"], len(hl._RATE_LIMIT_BACKOFF_S) + 1)
        self.assertEqual(slept, list(hl._RATE_LIMIT_BACKOFF_S))


class FetchPRCommentsTests(unittest.TestCase):
    """fetch_pr_comments with gh stubbed via monkeypatched subprocess.run."""

    def _run_with_stub(self, responses):
        """responses: dict endpoint-substring -> JSON-serializable payload."""
        import subprocess as _sp

        real_run = hl.subprocess.run

        def fake_run(cmd, *a, **kw):
            endpoint = cmd[-1]  # repos/<slug>/<endpoint>
            for key, payload in responses.items():
                if key in endpoint:
                    return _sp.CompletedProcess(
                        cmd, 0, stdout=json.dumps(payload), stderr=""
                    )
            return _sp.CompletedProcess(cmd, 0, stdout="[]", stderr="")

        hl.subprocess.run = fake_run
        try:
            return hl.fetch_pr_comments("org/repo", "236")
        finally:
            hl.subprocess.run = real_run

    def test_classifies_three_sources(self):
        comments, err = self._run_with_stub(
            {
                "pulls/236/comments": [
                    {
                        "id": 1,
                        "user": {"login": "cursor[bot]", "type": "Bot"},
                        "path": "a.py",
                        "line": 9,
                        "body": "inline issue",
                        "created_at": "2026-05-07T00:00:00Z",
                        "original_commit_id": "c1",
                    },
                    {
                        "id": 2,
                        "in_reply_to_id": 1,  # reply -> dropped
                        "user": {"login": "human", "type": "User"},
                        "body": "reply",
                        "created_at": "2026-05-07T01:00:00Z",
                    },
                ],
                "issues/236/comments": [
                    {
                        "id": 3,
                        "user": {"login": "human", "type": "User"},
                        "body": "top-level note",
                        "created_at": "2026-05-07T02:00:00Z",
                    }
                ],
                "pulls/236/reviews": [
                    {
                        "id": 4,
                        "state": "COMMENTED",
                        "user": {"login": "codex[bot]", "type": "Bot"},
                        "body": "review body",
                        "submitted_at": "2026-05-07T03:00:00Z",
                    },
                    {
                        "id": 5,
                        "state": "APPROVED",  # dropped
                        "user": {"login": "human", "type": "User"},
                        "body": "lgtm",
                        "submitted_at": "2026-05-07T04:00:00Z",
                    },
                    {
                        "id": 6,
                        "state": "COMMENTED",
                        "user": {"login": "human", "type": "User"},
                        "body": "   ",  # empty -> dropped
                        "submitted_at": "2026-05-07T05:00:00Z",
                    },
                ],
            }
        )
        self.assertIsNone(err)
        ids = {c["id"]: c for c in comments}
        self.assertEqual(set(ids), {1, 3, 4})  # reply, APPROVED, empty dropped
        self.assertEqual(ids[1]["source"], "github-inline")
        self.assertEqual(ids[1]["original_commit_id"], "c1")
        self.assertEqual(ids[3]["source"], "github-issue")
        self.assertEqual(ids[4]["source"], "github-review")
        self.assertEqual(ids[4]["created_at"], "2026-05-07T03:00:00Z")

    def test_missing_slug(self):
        comments, err = hl.fetch_pr_comments("", "236")
        self.assertEqual(comments, [])
        self.assertIsNotNone(err)


class BuildIndexTests(unittest.TestCase):
    def _record(self) -> "hl.PRRecord":
        return hl.PRRecord(
            schema_version=2,
            harvested_at="2026-05-20T00:00:00Z",
            harvester_git_sha="abc",
            pr={
                "repo_name": "kernel",
                "repo_url": "https://github.com/anthropics/kernel",
                "pr_number": "3945",
                "pr_url": "https://github.com/anthropics/kernel/pull/3945",
                "branch": None,
                "started_at": "2026-05-16T00:34:34Z",
                "completed": True,
                "exit_reason": "converged",
                "total_rounds": 5,
            },
            pr_comments_attribution_basis="created_at",
            pr_comments_fetch_error=None,
            harvested_rounds=[],
        )

    def test_index_shape(self):
        with tempfile.TemporaryDirectory() as d:
            index = hl.build_index(
                [self._record()],
                generated_at="2026-05-20T00:00:00Z",
                harvester_sha="abc",
                out_dir=Path(d),
            )
            self.assertEqual(index["schema_version"], 3)
            self.assertEqual(len(index["prs"]), 1)
            entry = index["prs"][0]
            self.assertEqual(entry["pr_number"], "3945")
            self.assertEqual(entry["file"], "kernel-3945.json")
            self.assertEqual(entry["harvested_rounds"], 0)
            self.assertEqual(entry["harvest_source"], "pr-polish")
            # No per-PR file on disk -> GT not collected.
            self.assertFalse(entry["ground_truth_collected"])
            self.assertIsNone(entry["census_converged"])

    def test_index_reflects_collected_ground_truth(self):
        with tempfile.TemporaryDirectory() as d:
            out_dir = Path(d)
            (out_dir / "kernel-3945.json").write_text(json.dumps({
                "schema_version": 2,
                "ground_truth_v3": {
                    "schema_version": 4, "census_converged": True,
                },
            }))
            index = hl.build_index(
                [self._record()],
                generated_at="2026-05-20T00:00:00Z",
                harvester_sha="abc",
                out_dir=out_dir,
            )
            entry = index["prs"][0]
            self.assertTrue(entry["ground_truth_collected"])
            self.assertTrue(entry["census_converged"])


    def test_index_keeps_prs_this_run_did_not_harvest(self):
        # A filtered harvest (--only) must not shrink the manifest to the
        # PRs it touched: replay.py samples its targets from this file's
        # ground_truth_collected flag, so dropping an entry silently makes a
        # frozen ground truth unreplayable while it still sits on disk.
        with tempfile.TemporaryDirectory() as d:
            out_dir = Path(d)
            # A previously collected PR this run does NOT harvest.
            (out_dir / "kernel-4189.json").write_text(json.dumps({
                "schema_version": 2,
                "pr": {
                    "repo_name": "kernel", "pr_number": "4189",
                    "repo_url": "https://github.com/x/kernel",
                    "pr_url": "https://github.com/x/kernel/pull/4189",
                    "completed": True, "total_rounds": 3,
                },
                "harvested_rounds": [{"round": 1}, {"round": 3}],
                "ground_truth_v3": {
                    "schema_version": 4, "census_converged": True,
                },
            }))
            # This run harvests only kernel-3945.
            (out_dir / "kernel-3945.json").write_text(json.dumps({
                "schema_version": 2,
            }))
            index = hl.build_index(
                [self._record()],
                generated_at="2026-05-20T00:00:00Z",
                harvester_sha="abc",
                out_dir=out_dir,
            )
            by_pr = {e["pr_number"]: e for e in index["prs"]}
            self.assertIn("3945", by_pr)
            self.assertIn("4189", by_pr)
            carried = by_pr["4189"]
            self.assertTrue(carried["ground_truth_collected"])
            self.assertTrue(carried["census_converged"])
            self.assertEqual(carried["harvested_rounds"], 2)

    def test_index_skips_unreadable_and_non_record_files(self):
        # A stray file in the dataset dir must not abort the harvest.
        with tempfile.TemporaryDirectory() as d:
            out_dir = Path(d)
            (out_dir / "notes.json").write_text("{}")
            (out_dir / "broken.json").write_text("{not json")
            index = hl.build_index(
                [self._record()],
                generated_at="2026-05-20T00:00:00Z",
                harvester_sha="abc",
                out_dir=out_dir,
            )
            self.assertEqual([e["pr_number"] for e in index["prs"]], ["3945"])

    def test_index_sorts_pr_numbers_numerically(self):
        with tempfile.TemporaryDirectory() as d:
            out_dir = Path(d)
            for num in ("999", "8276"):
                (out_dir / f"kernel-{num}.json").write_text(json.dumps({
                    "schema_version": 2,
                    "pr": {
                        "repo_name": "kernel", "pr_number": num,
                        "repo_url": "u", "pr_url": "u",
                        "completed": True, "total_rounds": 1,
                    },
                    "harvested_rounds": [],
                }))
            index = hl.build_index(
                [], generated_at="t", harvester_sha="abc", out_dir=out_dir,
            )
            # Lexical order would put 8276 before 999.
            self.assertEqual(
                [e["pr_number"] for e in index["prs"]], ["999", "8276"]
            )


class WritePrRecordTests(unittest.TestCase):
    """Re-harvesting a PR must not destroy a collected ground_truth_v3."""

    def _record(self):
        return hl.PRRecord(
            schema_version=2,
            harvested_at="2026-05-20T00:00:00Z",
            harvester_git_sha="abc",
            pr={"repo_name": "kernel", "repo_url": "u", "pr_number": "1",
                "pr_url": "u/pull/1", "branch": None, "started_at": "t",
                "completed": True, "exit_reason": "converged",
                "total_rounds": 1},
            pr_comments_attribution_basis="created_at",
            pr_comments_fetch_error=None,
            harvested_rounds=[],
        )

    def test_reharvest_preserves_ground_truth(self):
        with tempfile.TemporaryDirectory() as d:
            out_dir = Path(d)
            # First harvest.
            hl.write_pr_record(out_dir, self._record())
            # Collection freezes a GT block into the file.
            path = out_dir / "kernel-1.json"
            data = json.loads(path.read_text())
            data["ground_truth_v3"] = {"schema_version": 4,
                                       "census_converged": True}
            path.write_text(json.dumps(data))
            # Re-harvest — the GT block must survive.
            hl.write_pr_record(out_dir, self._record())
            reloaded = json.loads(path.read_text())
            self.assertIn("ground_truth_v3", reloaded)
            self.assertTrue(
                reloaded["ground_truth_v3"]["census_converged"]
            )


if __name__ == "__main__":
    unittest.main()
