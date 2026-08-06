"""Unit tests for collection mode (collect_lib + collect)."""

from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path

TEST_DIR = Path(__file__).resolve().parent
SCRIPT_DIR = TEST_DIR.parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

import collect  # noqa: E402
import collect_lib as cl  # noqa: E402
import harvest_lib as hl  # noqa: E402


def _fv(file, line, verdict, *, severity="high", topic="t", reason="r",
        surfaced_by=None):
    return {
        "file": file, "line": line, "severity": severity, "topic": topic,
        "verdict": verdict, "reason": reason,
        "surfaced_by": surfaced_by or [],
    }


def _census(file, line, **kw):
    return {"file": file, "line": line, **kw}


class NormalizeFindingPathTests(unittest.TestCase):
    """Worktree-checkout prefixes must be stripped to repo-relative paths.

    Regression test: bramble emits absolute paths prefixed with its WORK_DIR
    worktree; the harvested dataset stores repo-relative paths. Unstripped,
    the same file looked like two files and dedup / GT-matching failed.
    """

    def test_strips_replay_worktree_prefix(self):
        self.assertEqual(
            cl.normalize_finding_path("/tmp/replay-kernel-3945-r1-xy/src/a.py"),
            "src/a.py",
        )

    def test_strips_legacy_crr_prefix(self):
        self.assertEqual(
            cl.normalize_finding_path("/tmp/crr-kernel-4013-r2-wt/src/a.py"),
            "src/a.py",
        )

    def test_strips_session_worktree_prefix(self):
        # collect.py's _worktree_path pins the session worktree at
        # <EVAL_ROOT>/collect/<session>/worktree/ — a bare `worktree` dir.
        self.assertEqual(
            cl.normalize_finding_path(
                "/home/u/.bramble/code-review-eval/collect/"
                "kernel-1-20260521/worktree/src/a.py"
            ),
            "src/a.py",
        )

    def test_strips_session_worktree_prefix_nested_repo_path(self):
        self.assertEqual(
            cl.normalize_finding_path(
                "/home/u/.bramble/code-review-eval/collect/"
                "kernel-253-20260522/worktree/services/api/main.py"
            ),
            "services/api/main.py",
        )

    def test_does_not_strip_bare_worktree_in_repo_path(self):
        # A genuine repo path that happens to contain a `worktree/` segment
        # must NOT be stripped — only the anchored session-worktree prefix is.
        self.assertEqual(
            cl.normalize_finding_path("pkg/worktree/manager.go"),
            "pkg/worktree/manager.go",
        )

    def test_already_relative_passes_through(self):
        self.assertEqual(
            cl.normalize_finding_path("services/api/main.py"),
            "services/api/main.py",
        )

    def test_none_and_empty(self):
        self.assertIsNone(cl.normalize_finding_path(None))
        self.assertIsNone(cl.normalize_finding_path(""))


class SameDefectTests(unittest.TestCase):
    def test_same_path_line_within_slack(self):
        self.assertTrue(cl.same_defect("a.py", 10, "a.py", 12))
        self.assertTrue(cl.same_defect("a.py", 10, "a.py", 13))

    def test_line_outside_slack(self):
        self.assertFalse(cl.same_defect("a.py", 10, "a.py", 14))

    def test_different_path(self):
        self.assertFalse(cl.same_defect("a.py", 10, "b.py", 10))

    def test_path_normalization(self):
        self.assertTrue(cl.same_defect("./a.py", 10, "a.py", 11))

    def test_worktree_prefix_vs_relative_match(self):
        # The exact bug found in the kernel-4158 verification run: a
        # harvested repo-relative path and a bramble absolute worktree path
        # for the SAME file must compare equal.
        self.assertTrue(cl.same_defect(
            "services/api/main.py", 350,
            "/tmp/crr-kernel-4013-r2-wt/services/api/main.py", 350,
        ))

    def test_missing_line_is_file_level_match(self):
        self.assertTrue(cl.same_defect("a.py", None, "a.py", 99))
        self.assertTrue(cl.same_defect("a.py", 10, "a.py", None))


class FileLevelCollisionValidationTests(unittest.TestCase):
    """At most one ``line: null`` verdict per path.

    Regression intent: defect identity is ``(file, line)``, so two
    file-level findings on one path collapse into a single entry and flip
    each other into a contested state no round can resolve. kernel-8229
    emitted four on one file and became uncollectable. Rejecting at the
    input boundary is the fix — see ``_file_level_collision_error`` for why
    keying on topic or occurrence index was measured and rejected instead.
    """

    def test_string_location_error_names_the_fix(self):
        # The commonest judge mistake: a location written as "path:line"
        # instead of an object. The message is what the judge sees when asked
        # to self-correct, so it must show the required shape, not just say
        # "not an object". Two judges in one batch made this error.
        v = {"finding_verdicts": [_fv("a.py", 1, "true_positive")],
             "census": [{"file": "a.py", "line": 1, "severity": "low",
                         "description": "d"}],
             "census_merges": [
                 {"members": ["a.py:1", "a.py:9"], "reason": "same defect"}
             ]}
        err = cl.validate_judge_verdict(v)
        self.assertIsNotNone(err)
        self.assertIn("must be an object", err)
        self.assertIn('"file"', err)
        self.assertIn('"line"', err)
        self.assertIn("a.py:1", err)  # quotes the offending value back

    def test_one_file_level_verdict_is_fine(self):
        v = {"finding_verdicts": [
            _fv("a.py", None, "true_positive"),
            _fv("a.py", 10, "true_positive"),
        ], "census": []}
        self.assertIsNone(cl.validate_judge_verdict(v))

    def test_two_file_level_on_same_path_rejected(self):
        v = {"finding_verdicts": [
            _fv("a.py", None, "true_positive"),
            _fv("a.py", None, "false_positive"),
        ], "census": []}
        err = cl.validate_judge_verdict(v)
        self.assertIsNotNone(err)
        self.assertIn("file-level", err)

    def test_file_level_on_different_paths_is_fine(self):
        v = {"finding_verdicts": [
            _fv("a.py", None, "true_positive"),
            _fv("b.py", None, "true_positive"),
        ], "census": []}
        self.assertIsNone(cl.validate_judge_verdict(v))

    def test_kernel_8229_four_file_level_findings_rejected(self):
        # The exact shape that made kernel-8229 uncollectable.
        v = {"finding_verdicts": [
            _fv("r.py", None, "true_positive"),
            _fv("r.py", None, "false_positive"),
            _fv("r.py", None, "false_positive"),
            _fv("r.py", None, "true_positive"),
        ], "census": []}
        err = cl.validate_judge_verdict(v)
        self.assertIsNotNone(err)
        self.assertIn("Anchor each finding", err)

    def test_agreeing_file_level_verdicts_are_allowed(self):
        # Same location, same ruling: these merge into one entry and pool
        # surfaced_by. That is the intended dedupe, not a collision.
        v = {"finding_verdicts": [
            _fv("r.py", None, "true_positive"),
            _fv("r.py", None, "true_positive"),
        ], "census": []}
        self.assertIsNone(cl.validate_judge_verdict(v))

    def test_conflicting_verdicts_within_line_slack_rejected(self):
        # The second shape of the same bug, found while repairing
        # kernel-8229: anchoring two conflicting findings one row apart
        # re-created the deadlock at the fold layer, below the validator.
        v = {"finding_verdicts": [
            _fv("r.py", 58, "true_positive"),
            _fv("r.py", 59, "false_positive"),
        ], "census": []}
        err = cl.validate_judge_verdict(v)
        self.assertIsNotNone(err)
        self.assertIn("row", err)

    def test_agreeing_verdicts_within_line_slack_are_allowed(self):
        v = {"finding_verdicts": [
            _fv("r.py", 76, "true_positive"),
            _fv("r.py", 78, "true_positive"),
        ], "census": []}
        self.assertIsNone(cl.validate_judge_verdict(v))

    def test_conflicting_verdicts_beyond_line_slack_are_allowed(self):
        # Far enough apart to be distinct defects — must not be rejected.
        v = {"finding_verdicts": [
            _fv("r.py", 58, "true_positive"),
            _fv("r.py", 151, "false_positive"),
        ], "census": []}
        self.assertIsNone(cl.validate_judge_verdict(v))

    def test_paths_normalized_before_comparison(self):
        # A worktree-absolute path and its repo-relative form are one file.
        v = {"finding_verdicts": [
            _fv("/tmp/replay-xyz/a.py", None, "true_positive"),
            _fv("a.py", None, "false_positive"),
        ], "census": []}
        self.assertIsNotNone(cl.validate_judge_verdict(v))

    def test_unsure_file_level_does_not_collide(self):
        # `unsure` sets no ground truth, so it never enters the accumulator.
        v = {"finding_verdicts": [
            _fv("a.py", None, "true_positive"),
            {"file": "a.py", "line": None, "verdict": "unsure",
             "topic": "t", "reason": "r"},
        ], "census": []}
        self.assertIsNone(cl.validate_judge_verdict(v))


class ValidateJudgeVerdictTests(unittest.TestCase):
    def test_valid(self):
        v = {"finding_verdicts": [_fv("a.py", 1, "true_positive")],
             "census": []}
        self.assertIsNone(cl.validate_judge_verdict(v))

    def test_not_an_object(self):
        self.assertIsNotNone(cl.validate_judge_verdict([1, 2]))

    def test_missing_finding_verdicts(self):
        self.assertIsNotNone(cl.validate_judge_verdict({}))

    def test_bad_verdict_value(self):
        v = {"finding_verdicts": [_fv("a.py", 1, "maybe")]}
        self.assertIsNotNone(cl.validate_judge_verdict(v))

    def test_census_must_be_list(self):
        v = {"finding_verdicts": [], "census": {}}
        self.assertIsNotNone(cl.validate_judge_verdict(v))

    def test_census_merges_optional(self):
        v = {"finding_verdicts": [], "census": []}
        self.assertIsNone(cl.validate_judge_verdict(v))

    def test_census_merges_must_be_list(self):
        v = {"finding_verdicts": [], "census_merges": {}}
        self.assertIsNotNone(cl.validate_judge_verdict(v))

    def test_census_merge_needs_two_members(self):
        v = {"finding_verdicts": [],
             "census_merges": [{"members": [{"file": "a.py", "line": 1}]}]}
        self.assertIsNotNone(cl.validate_judge_verdict(v))

    def test_valid_census_merge(self):
        v = {"finding_verdicts": [], "census": [],
             "census_merges": [{"members": [
                 {"file": "a.py", "line": 8},
                 {"file": "a.py", "line": 46}]}]}
        self.assertIsNone(cl.validate_judge_verdict(v))

    def test_finding_verdict_missing_file_rejected(self):
        v = {"finding_verdicts": [
            {"line": 1, "verdict": "true_positive", "severity": "high"}]}
        self.assertIsNotNone(cl.validate_judge_verdict(v))

    def test_finding_verdict_missing_line_rejected(self):
        v = {"finding_verdicts": [
            {"file": "a.py", "verdict": "true_positive", "severity": "high"}]}
        self.assertIsNotNone(cl.validate_judge_verdict(v))

    def test_unsure_verdict_needs_no_location(self):
        # `unsure` sets no GT entry, so a missing location is acceptable.
        v = {"finding_verdicts": [{"verdict": "unsure"}]}
        self.assertIsNone(cl.validate_judge_verdict(v))

    def test_finding_verdict_file_level_line_none_accepted(self):
        # A file-level finding carries line=None — present but null is fine.
        v = {"finding_verdicts": [
            {"file": "a.py", "line": None, "verdict": "true_positive",
             "severity": "low"}]}
        self.assertIsNone(cl.validate_judge_verdict(v))

    def test_census_entry_missing_location_rejected(self):
        v = {"finding_verdicts": [], "census": [{"topic": "no location"}]}
        self.assertIsNotNone(cl.validate_judge_verdict(v))

    def test_census_merge_member_missing_location_rejected(self):
        v = {"finding_verdicts": [],
             "census_merges": [{"members": [
                 {"file": "a.py", "line": 8},
                 {"line": 46}]}]}  # second member has no file
        self.assertIsNotNone(cl.validate_judge_verdict(v))


class MergeJudgeRoundTests(unittest.TestCase):
    def test_routes_tp_and_fp_drops_unsure(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [
                _fv("a.py", 10, "true_positive"),
                _fv("b.py", 5, "false_positive"),
                _fv("c.py", 1, "unsure"),
            ],
            "census": [],
        })
        self.assertEqual(len(c.true_positives), 1)
        self.assertEqual(len(c.false_positives), 1)

    def test_dedupes_same_defect_unions_surfaced_by(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [
                _fv("a.py", 10, "true_positive", surfaced_by=["codex"]),
            ],
            "census": [],
        })
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [
                _fv("a.py", 12, "true_positive", surfaced_by=["cursor"]),
            ],
            "census": [],
        })
        self.assertEqual(len(c.true_positives), 1)
        self.assertEqual(
            sorted(c.true_positives[0].surfaced_by), ["codex", "cursor"]
        )
        self.assertEqual(c.true_positives[0].first_seen_round, 1)

    def test_census_unioned_and_deduped(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [],
            "census": [_census("a.py", 10), _census("c.py", 99)],
        })
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [],
            "census": [_census("a.py", 10), _census("d.py", 1)],
        })
        # a.py:10 not double-counted; d.py:1 added.
        self.assertEqual(len(c.census), 3)


class CensusConvergenceTests(unittest.TestCase):
    def test_needs_at_least_two_rounds(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [_fv("a.py", 10, "true_positive")],
            "census": [_census("a.py", 10)],
        })
        # Round 1 alone: covered, but cannot be "unchanged vs prior".
        self.assertFalse(cl.census_converged(c))

    def test_converges_when_stable_and_covered(self):
        c = cl.CumulativeGT()
        for r in (1, 2):
            cl.merge_judge_round(c, r, {
                "finding_verdicts": [_fv("a.py", 10, "true_positive")],
                "census": [_census("a.py", 10)],
            })
        self.assertTrue(cl.census_converged(c))

    def test_converges_despite_uncovered_census_item(self):
        # A real bug no reviewer caught is the recall signal replay exists to
        # measure — it must NOT block convergence. Gating on full coverage
        # made saturation unreachable on any diff with a low/nit defect the
        # reviewers skip: the run would spend its whole round budget and
        # still freeze as "unconverged".
        c = cl.CumulativeGT()
        for r in (1, 2):
            cl.merge_judge_round(c, r, {
                "finding_verdicts": [_fv("a.py", 10, "true_positive")],
                # c.py:99 is a real bug NO finding caught -> uncovered.
                "census": [_census("a.py", 10), _census("c.py", 99)],
            })
        self.assertTrue(cl.census_converged(c))
        # Still reported, just not gating.
        self.assertEqual(len(cl.census_uncovered(c)), 1)

    def test_converges_on_the_round_that_adds_nothing(self):
        # Regression for kernel-8276: the census grew 8 -> 10 -> 12 and then
        # round 4 added nothing, but convergence still reported False and
        # `should_continue` stayed True, so a saturated run would have burnt
        # its whole 10-round budget. A round that censuses no new defect is
        # the saturation signal.
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [_fv("a.py", 10, "true_positive")],
            "census": [_census("a.py", 10)],
        })
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [_fv("a.py", 10, "true_positive")],
            "census": [_census("a.py", 10), _census("b.py", 20)],
        })
        self.assertFalse(cl.census_converged(c))
        # Round 3 re-cites both, censusing nothing new -> saturated.
        cl.merge_judge_round(c, 3, {
            "finding_verdicts": [_fv("a.py", 10, "true_positive")],
            "census": [_census("a.py", 10), _census("b.py", 20)],
        })
        self.assertEqual(c.per_round_diff[-1]["new_census_items"], [])
        self.assertTrue(cl.census_converged(c))

    def test_unresolved_contested_still_blocks(self):
        # Dropping the coverage gate must not weaken the contested gate.
        c = cl.CumulativeGT()
        for r in (1, 2):
            cl.merge_judge_round(c, r, {
                "finding_verdicts": [_fv("a.py", 10, "true_positive")],
                "census": [_census("a.py", 10)],
            })
        self.assertTrue(cl.census_converged(c))
        c.contested.append(
            cl.GTEntry(
                file="z.py", line=1, topic="x",
                severity="high", first_seen_round=2, resolved=False,
            )
        )
        self.assertFalse(cl.census_converged(c))

    def test_no_converge_when_census_grew(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [_fv("a.py", 10, "true_positive")],
            "census": [_census("a.py", 10)],
        })
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [
                _fv("a.py", 10, "true_positive"),
                _fv("c.py", 99, "true_positive"),
            ],
            # New census item in round 2 -> not unchanged.
            "census": [_census("a.py", 10), _census("c.py", 99)],
        })
        self.assertFalse(cl.census_converged(c))

    def test_empty_census_converges_after_two_quiet_rounds(self):
        c = cl.CumulativeGT()
        for r in (1, 2):
            cl.merge_judge_round(c, r, {"finding_verdicts": [], "census": []})
        self.assertTrue(cl.census_converged(c))


class CensusMergeTests(unittest.TestCase):
    """The judge can declare two census items to be one defect.

    Regression test: an early round split a file-scoped test-coverage gap
    into two line numbers; no reviewer finding ever cites the lower line, so
    the line-precise coverage check kept it permanently uncovered and
    convergence could never be reached.
    """

    def test_merge_collapses_census_items(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [],
            "census": [_census("t.py", 8), _census("t.py", 46)],
        })
        self.assertEqual(len(c.census), 2)
        # Round 2 declares the two t.py entries one defect.
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [],
            "census": [_census("t.py", 8), _census("t.py", 46)],
            "census_merges": [{"members": [
                {"file": "t.py", "line": 8},
                {"file": "t.py", "line": 46}]}],
        })
        # Collapsed to one — the first member (line 8) is canonical.
        self.assertEqual(len(c.census), 1)
        self.assertEqual(c.census[0]["line"], 8)

    def test_merge_lets_one_finding_cover_a_split_defect(self):
        c = cl.CumulativeGT()
        # Round 1: defect censused at two lines; a finding only at line 46.
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [_fv("t.py", 46, "true_positive")],
            "census": [_census("t.py", 8), _census("t.py", 46)],
        })
        # t.py:8 has no finding within +/-3 -> uncovered, no convergence.
        self.assertEqual(len(cl.census_uncovered(c)), 1)
        # Round 2: the judge merges the two census items into one defect.
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [_fv("t.py", 46, "true_positive")],
            "census": [_census("t.py", 8), _census("t.py", 46)],
            "census_merges": [{"members": [
                {"file": "t.py", "line": 8},
                {"file": "t.py", "line": 46}]}],
        })
        # The merged item is now covered by the line-46 finding. A merge is
        # a reinterpretation, not new information — the merged item still
        # answers to both member keys, so the key set is unchanged and
        # round 2 converges.
        self.assertEqual(len(cl.census_uncovered(c)), 0)
        self.assertTrue(cl.census_converged(c))

    def test_merge_of_absent_items_is_noop(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [],
            "census": [_census("a.py", 1)],
            # members reference census entries that don't exist
            "census_merges": [{"members": [
                {"file": "z.py", "line": 1},
                {"file": "z.py", "line": 2}]}],
        })
        self.assertEqual(len(c.census), 1)

    def test_merge_with_finding_location_member_covers_census(self):
        # The kernel-4050 verification case: the judge declares a census
        # item and a FINDING location (not itself censused, >LINE_SLACK
        # away) to be one defect. The finding location must land in the
        # census entry's merged_locations so a TP finding there covers it.
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            # A TP finding 20 lines from the censused line — no auto-match.
            "finding_verdicts": [_fv("hooks.ts", 1358, "true_positive")],
            "census": [_census("hooks.ts", 1338)],
        })
        # Line-precise: 1358 vs 1338 is outside +/-3 -> uncovered.
        self.assertEqual(len(cl.census_uncovered(c)), 1)
        # Round 2: the judge declares 1338 and 1358 one defect. 1358 is a
        # finding location, not a census item — only 1338 is in the census.
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [_fv("hooks.ts", 1358, "true_positive")],
            "census": [_census("hooks.ts", 1338)],
            "census_merges": [{"members": [
                {"file": "hooks.ts", "line": 1338},
                {"file": "hooks.ts", "line": 1358}]}],
        })
        # The census still has one entry (the finding location is NOT
        # added as a census item) — but it now carries 1358 in
        # merged_locations, so the TP finding there covers it.
        self.assertEqual(len(c.census), 1)
        self.assertEqual(len(cl.census_uncovered(c)), 0)
        # A merge re-describes a defect already censused; it adds no NEW
        # census item, so it does not by itself hold convergence open.
        self.assertTrue(cl.census_converged(c))
        cl.merge_judge_round(c, 3, {
            "finding_verdicts": [_fv("hooks.ts", 1358, "true_positive")],
            "census": [_census("hooks.ts", 1338)],
            "census_merges": [{"members": [
                {"file": "hooks.ts", "line": 1338},
                {"file": "hooks.ts", "line": 1358}]}],
        })
        self.assertEqual(len(cl.census_uncovered(c)), 0)
        self.assertTrue(cl.census_converged(c))


class SeedCandidatesTests(unittest.TestCase):
    """Seeds are candidate locations, never findings or ground truth."""

    def _round(self, actions):
        return {"raw_comment_actions": actions}

    def test_extracts_inline_comments_with_location(self):
        out = collect.seed_candidates(self._round([
            {"source": "github-inline", "path": "a.py", "line": 10,
             "author": "coderabbitai[bot]", "body": "Off-by-one here."},
        ]))
        self.assertEqual(len(out), 1)
        self.assertEqual(out[0]["file"], "a.py")
        self.assertEqual(out[0]["line"], 10)
        self.assertEqual(out[0]["claim"], "Off-by-one here.")

    def test_skips_non_inline_and_locationless(self):
        out = collect.seed_candidates(self._round([
            {"source": "github-issue", "body": "LGTM"},
            {"source": "github-inline", "body": "no path"},
            {"source": "github-inline", "path": "a.py", "body": ""},
            {"source": "codex", "path": "a.py", "body": "local finding"},
        ]))
        self.assertEqual(out, [])

    def test_filters_bot_status_noise(self):
        out = collect.seed_candidates(self._round([
            {"source": "github-inline", "path": "a.py", "line": 1,
             "body": "Groot review started for abc123."},
            {"source": "github-inline", "path": "a.py", "line": 2,
             "body": "**Actionable comments posted: 0**"},
            {"source": "github-inline", "path": "b.py", "line": 3,
             "body": "This leaks a file handle on the error path."},
        ]))
        self.assertEqual([c["file"] for c in out], ["b.py"])

    def test_respects_limit(self):
        actions = [
            {"source": "github-inline", "path": f"f{i}.py", "line": i,
             "body": "real finding text"}
            for i in range(60)
        ]
        self.assertEqual(len(collect.seed_candidates(self._round(actions))), 40)
        self.assertEqual(
            len(collect.seed_candidates(self._round(actions), limit=5)), 5
        )

    def test_claim_is_truncated(self):
        out = collect.seed_candidates(self._round([
            {"source": "github-inline", "path": "a.py", "line": 1,
             "body": "x" * 5000},
        ]))
        self.assertEqual(len(out[0]["claim"]), 1200)


class EnsureCommitPresentTests(unittest.TestCase):
    """A GitHub-sourced record names a commit the checkout may not have."""

    def _repo(self, td: str) -> tuple[Path, str]:
        repo = Path(td)
        hl.git(repo, "init", "-q")
        hl.git(repo, "config", "user.email", "t@t")
        hl.git(repo, "config", "user.name", "t")
        (repo / "f.txt").write_text("x")
        hl.git(repo, "add", "f.txt")
        hl.git(repo, "commit", "-qm", "one")
        return repo, hl.git(repo, "rev-parse", "HEAD").stdout.strip()

    def test_present_commit_makes_no_fetch(self):
        # The common case must stay offline — no network on every setup.
        calls: list[list[str]] = []
        real_git = collect.hl.git

        def spy(repo, *args):
            calls.append(list(args))
            return real_git(repo, *args)

        with tempfile.TemporaryDirectory() as td:
            repo, head = self._repo(td)
            collect.hl.git = spy
            try:
                collect.ensure_commit_present(repo, head, "42")
            finally:
                collect.hl.git = real_git
        self.assertFalse(any(a and a[0] == "fetch" for a in calls))

    def test_missing_commit_is_fetched_then_verified(self):
        with tempfile.TemporaryDirectory() as td:
            repo, _ = self._repo(td)
            missing = "0" * 40
            # No `origin` remote, so the fetch cannot succeed -> the error
            # must name the force-push/deleted-fork cause, not just fail.
            with self.assertRaises(SystemExit) as cm:
                collect.ensure_commit_present(repo, missing, "42")
            msg = str(cm.exception)
            self.assertIn("000000000000", msg)
            self.assertIn("pull/42/head", msg)
            self.assertIn("force-pushed", msg)

    def test_fetch_is_attempted_before_giving_up(self):
        calls: list[list[str]] = []
        real_git = collect.hl.git

        def spy(repo, *args):
            calls.append(list(args))
            return real_git(repo, *args)

        with tempfile.TemporaryDirectory() as td:
            repo, _ = self._repo(td)
            collect.hl.git = spy
            try:
                with self.assertRaises(SystemExit):
                    collect.ensure_commit_present(repo, "0" * 40, "99")
            finally:
                collect.hl.git = real_git
        self.assertTrue(
            any(a[:3] == ["fetch", "origin", "pull/99/head"] for a in calls)
        )


class FindingsFromEnvelopeTests(unittest.TestCase):
    """A failed review must never read as a finding-free review.

    Regression for kernel-8276: codex stalled on the idle watchdog and
    gemini's client was decommissioned. Both still wrote an envelope, but
    with status=error and an empty review — folding those in would have told
    the judge two reviewers looked and found nothing.
    """

    def _write(self, td: str, payload: dict) -> Path:
        p = Path(td) / "envelope.json"
        p.write_text(json.dumps(payload))
        return p

    def test_ok_envelope_yields_findings(self):
        with tempfile.TemporaryDirectory() as td:
            p = self._write(td, {
                "status": "ok",
                "review": {"verdict": "rejected", "issues": [
                    {"file": "a.py", "line": 10, "severity": "high",
                     "message": "boom"},
                ]},
            })
            out = collect._findings_from_envelope(p, "cursor")
            self.assertEqual(len(out), 1)
            self.assertEqual(out[0]["surfaced_by"], ["cursor"])

    def test_ok_envelope_with_no_issues_is_a_real_zero(self):
        # An honest "accepted, found nothing" must still pass through — it
        # is a legitimate data point, unlike a backend failure.
        with tempfile.TemporaryDirectory() as td:
            p = self._write(td, {
                "status": "ok",
                "review": {"verdict": "accepted", "issues": []},
            })
            self.assertEqual(collect._findings_from_envelope(p, "cursor"), [])

    def test_error_envelope_is_refused(self):
        with tempfile.TemporaryDirectory() as td:
            p = self._write(td, {
                "status": "error",
                "error": "codex: review idle: no events for 3m0s",
                "review": {},
            })
            with self.assertRaises(SystemExit) as cm:
                collect._findings_from_envelope(p, "codex")
            self.assertIn("status='error'", str(cm.exception))

    def test_status_absent_is_tolerated(self):
        # Older envelopes predate the status field; absence is not failure.
        with tempfile.TemporaryDirectory() as td:
            p = self._write(td, {
                "review": {"verdict": "accepted", "issues": []},
            })
            self.assertEqual(collect._findings_from_envelope(p, "cursor"), [])


class CommentActionXrefTests(unittest.TestCase):
    def _harvested(self):
        return [{
            "review_runs": [{
                "backend": "codex",
                "findings": [
                    {"file": "a.py", "line": 10,
                     "ground_truth": {"is_real_issue": True}},
                    {"file": "b.py", "line": 5,
                     "ground_truth": {"is_real_issue": False}},
                ],
            }],
        }]

    def test_agreement_and_disagreement(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [
                _fv("a.py", 10, "true_positive"),   # agrees with harvest
                _fv("b.py", 5, "true_positive"),    # disagrees (harvest=False)
            ],
            "census": [],
        })
        xref = cl.comment_action_xref(c, self._harvested())
        self.assertEqual(xref["comparisons"], 2)
        self.assertEqual(xref["agreements"], 1)
        self.assertEqual(xref["comment_action_agreement_rate"], 0.5)
        self.assertEqual(len(xref["disagreements"]), 1)

    def test_null_harvest_label_skipped(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [_fv("z.py", 1, "true_positive")],
            "census": [],
        })
        xref = cl.comment_action_xref(c, self._harvested())
        # No harvested finding at z.py:1 -> nothing to compare.
        self.assertEqual(xref["comparisons"], 0)
        self.assertIsNone(xref["comment_action_agreement_rate"])


class FreezeTests(unittest.TestCase):
    def test_freeze_writes_block_in_place(self):
        with tempfile.TemporaryDirectory() as d:
            ds_path = Path(d) / "demo-1.json"
            ds_path.write_text(json.dumps({
                "schema_version": 2,
                "pr": {"repo_name": "demo", "pr_number": "1"},
                "harvested_rounds": [{"round": 1}],
            }))
            c = cl.CumulativeGT()
            cl.merge_judge_round(c, 1, {
                "finding_verdicts": [_fv("a.py", 10, "true_positive")],
                "census": [_census("a.py", 10)],
            })
            gt = cl.build_ground_truth(
                c, collector_git_sha="abc", harvested_rounds=[]
            )
            cl.freeze(ds_path, gt)
            reloaded = json.loads(ds_path.read_text())
            # Harvested fields untouched; block added.
            self.assertEqual(reloaded["schema_version"], 2)
            self.assertIn("ground_truth_v3", reloaded)
            block = reloaded["ground_truth_v3"]
            self.assertEqual(
                block["schema_version"], cl.GROUND_TRUTH_SCHEMA_VERSION
            )
            self.assertEqual(len(block["true_positives"]), 1)
            self.assertIn("contested", block)

    def test_load_ground_truth(self):
        self.assertIsNone(cl.load_ground_truth({"pr": {}}))
        self.assertIsNotNone(
            cl.load_ground_truth({"ground_truth_v3": {"schema_version": 3}})
        )


class SessionRoundTripTests(unittest.TestCase):
    """collect.py's CumulativeGT serialize/restore across rounds."""

    def test_cumulative_survives_save_load(self):
        with tempfile.TemporaryDirectory() as d:
            session = Path(d)
            c = cl.CumulativeGT()
            cl.merge_judge_round(c, 1, {
                "finding_verdicts": [
                    _fv("a.py", 10, "true_positive", surfaced_by=["codex"]),
                    _fv("b.py", 5, "false_positive"),
                ],
                "census": [_census("a.py", 10), _census("c.py", 99)],
            })
            collect.save_cumulative(session, c)
            restored = collect.load_cumulative(session)
            self.assertEqual(len(restored.true_positives), 1)
            self.assertEqual(len(restored.false_positives), 1)
            self.assertEqual(len(restored.census), 2)
            self.assertEqual(restored.rounds_run, 1)
            # last_round_census_keys must round-trip as a set of tuples so
            # the next round's convergence check still works.
            self.assertIsInstance(restored.last_round_census_keys, set)
            cl.merge_judge_round(restored, 2, {
                "finding_verdicts": [
                    _fv("a.py", 10, "true_positive"),
                    _fv("c.py", 99, "true_positive"),
                ],
                "census": [_census("a.py", 10), _census("c.py", 99)],
            })
            self.assertTrue(cl.census_converged(restored))

    def test_fresh_session_is_empty(self):
        with tempfile.TemporaryDirectory() as d:
            c = collect.load_cumulative(Path(d))
            self.assertEqual(c.rounds_run, 0)
            self.assertEqual(c.true_positives, [])


class CanonicalRoundTests(unittest.TestCase):
    def test_prefers_r1(self):
        ds = {"harvested_rounds": [
            {"round": 1, "signal_tier": "r1"},
            {"round": 5, "signal_tier": "final"},
        ]}
        self.assertEqual(collect._canonical_round(ds)["round"], 1)

    def test_r1_only(self):
        ds = {"harvested_rounds": [{"round": 1, "signal_tier": "r1_only"}]}
        self.assertEqual(collect._canonical_round(ds)["round"], 1)


class DedupeFindingsTests(unittest.TestCase):
    def test_unions_surfaced_by_across_backends(self):
        merged = collect._dedupe_findings([
            {"file": "a.py", "line": 10, "surfaced_by": ["codex"]},
            {"file": "a.py", "line": 11, "surfaced_by": ["cursor"]},
            {"file": "b.py", "line": 1, "surfaced_by": ["gemini"]},
        ])
        self.assertEqual(len(merged), 2)
        a = next(m for m in merged if m["file"] == "a.py")
        self.assertEqual(sorted(a["surfaced_by"]), ["codex", "cursor"])


def _git(repo: Path, *args: str) -> str:
    import subprocess
    r = subprocess.run(["git", "-C", str(repo), *args],
                       capture_output=True, text=True, check=True)
    return r.stdout.strip()


def _make_repo_two_commits(repo: Path) -> tuple[str, str]:
    """A throwaway git repo: bug.py has a defect at HEAD~1, fixed at HEAD.

    Returns (head_before, head_after). The judge worktree must be pinned to
    head_before so it sees the BUGGY version, not the fixed one.
    """
    import subprocess
    repo.mkdir(parents=True, exist_ok=True)
    subprocess.run(["git", "-C", str(repo), "init", "-q"], check=True)
    _git(repo, "config", "user.email", "t@t.t")
    _git(repo, "config", "user.name", "t")
    (repo / "bug.py").write_text("x = 1 / 0  # BUG: div by zero\n")
    _git(repo, "add", "bug.py")
    _git(repo, "commit", "-q", "-m", "buggy")
    head_before = _git(repo, "rev-parse", "HEAD")
    (repo / "bug.py").write_text("x = 1 / 1  # fixed\n")
    _git(repo, "add", "bug.py")
    _git(repo, "commit", "-q", "-m", "fix")
    head_after = _git(repo, "rev-parse", "HEAD")
    return head_before, head_after


def _demo_dataset(head_before: str, merge_base: str) -> dict:
    return {
        "schema_version": 2,
        "pr": {"repo_name": "demo", "pr_number": "1"},
        "harvested_rounds": [{
            "round": 1, "signal_tier": "r1",
            "head_before": head_before, "head_after": head_before,
            "merge_base_sha": merge_base, "merge_base_resolved": True,
            "merge_base_error": None, "base_branch": "main",
            "files_changed": ["bug.py"],
            "goal_text": "fix the bug",
            "raw_comment_actions": [],
            "review_runs": [],
        }],
    }


class WorktreeTests(unittest.TestCase):
    """The session worktree must be pinned at head_before, never live HEAD.

    Regression test: an earlier design pointed the judge at the live repo
    checkout, so it inspected already-fixed code and wrongly verdicted real
    findings as false positives.
    """

    def test_worktree_pinned_to_head_before(self):
        with tempfile.TemporaryDirectory() as d:
            repo = Path(d) / "repo"
            head_before, head_after = _make_repo_two_commits(repo)
            session = Path(d) / "session"
            session.mkdir()
            try:
                wt = collect.ensure_worktree(session, repo, head_before)
                # The working tree must show the BUGGY version — the repo's
                # own HEAD is head_after, but the worktree is pinned.
                self.assertIn("BUG", (wt / "bug.py").read_text())
                self.assertEqual(
                    _git(wt, "rev-parse", "HEAD"), head_before
                )
                self.assertEqual(wt, collect._worktree_path(session))
            finally:
                collect.remove_worktree(session, repo)
            self.assertFalse((session / "worktree").exists())

    def test_ensure_is_idempotent(self):
        with tempfile.TemporaryDirectory() as d:
            repo = Path(d) / "repo"
            head_before, _ = _make_repo_two_commits(repo)
            session = Path(d) / "session"
            session.mkdir()
            try:
                w1 = collect.ensure_worktree(session, repo, head_before)
                w2 = collect.ensure_worktree(session, repo, head_before)
                self.assertEqual(w1, w2)
                self.assertEqual(_git(w2, "rev-parse", "HEAD"), head_before)
            finally:
                collect.remove_worktree(session, repo)

    def test_setup_pins_worktree_and_records_meta(self):
        with tempfile.TemporaryDirectory() as d:
            repo = Path(d) / "repo"
            head_before, _ = _make_repo_two_commits(repo)
            merge_base = _git(repo, "rev-list", "--max-parents=0", "HEAD")
            ds_dir = Path(d) / "ds"
            ds_dir.mkdir()
            (ds_dir / "demo-1.json").write_text(
                json.dumps(_demo_dataset(head_before, merge_base)))
            repo_map = hl.RepoMap(mapping={"demo": repo})
            result = collect.setup(
                target="demo-1", dataset_dir=ds_dir,
                session_root=Path(d) / "sessions", repo_map=repo_map)
            session = Path(result["session"])
            # The worktree is pinned at head_before (shows the buggy code).
            wt = Path(result["worktree"])
            self.assertIn("BUG", (wt / "bug.py").read_text())
            self.assertEqual(_git(wt, "rev-parse", "HEAD"), head_before)
            # Session meta records target + source repo.
            meta = collect.load_session_meta(session)
            self.assertEqual(meta["target"], "demo-1")
            self.assertEqual(meta["source_repo"], str(repo))
            collect.remove_worktree(session, repo)

    def test_setup_fails_fast_on_unresolved_merge_base(self):
        # An unresolved merge base would force head_before..head_before (an
        # empty diff); collection must abort rather than judge an empty scope.
        with tempfile.TemporaryDirectory() as d:
            repo = Path(d) / "repo"
            head_before, _ = _make_repo_two_commits(repo)
            merge_base = _git(repo, "rev-list", "--max-parents=0", "HEAD")
            ds = _demo_dataset(head_before, merge_base)
            ds["harvested_rounds"][0]["merge_base_resolved"] = False
            ds["harvested_rounds"][0]["merge_base_error"] = "no checkout"
            ds_dir = Path(d) / "ds"
            ds_dir.mkdir()
            (ds_dir / "demo-1.json").write_text(json.dumps(ds))
            repo_map = hl.RepoMap(mapping={"demo": repo})
            with self.assertRaises(SystemExit) as cm:
                collect.setup(
                    target="demo-1", dataset_dir=ds_dir,
                    session_root=Path(d) / "sessions", repo_map=repo_map)
            self.assertIn("merge base", str(cm.exception))

    def test_build_prompt_points_repo_path_at_pinned_worktree(self):
        with tempfile.TemporaryDirectory() as d:
            repo = Path(d) / "repo"
            head_before, _ = _make_repo_two_commits(repo)
            merge_base = _git(repo, "rev-list", "--max-parents=0", "HEAD")
            ds_dir = Path(d) / "ds"
            ds_dir.mkdir()
            (ds_dir / "demo-1.json").write_text(
                json.dumps(_demo_dataset(head_before, merge_base)))
            repo_map = hl.RepoMap(mapping={"demo": repo})
            result = collect.setup(
                target="demo-1", dataset_dir=ds_dir,
                session_root=Path(d) / "sessions", repo_map=repo_map)
            session = Path(result["session"])
            try:
                prompt_path = collect.build_prompt(
                    session=session, round_n=1, envelopes=[],
                    include_harvested=False, dataset_dir=ds_dir)
                prompt = json.loads(prompt_path.read_text())
                # repo_path is the single pinned session worktree.
                self.assertEqual(
                    prompt["repo_path"],
                    str(collect._worktree_path(session)))
                self.assertIn(
                    str(collect._worktree_path(session)),
                    prompt["diff_ref"]["diff_command"])
            finally:
                collect.remove_worktree(session, repo)


class FileLevelVerdictIdentityTests(unittest.TestCase):
    """A ``line: null`` verdict must not re-rule line-level entries.

    Regression intent: ``_entry_matches`` used ``same_defect``, whose
    file-level-subsumes-line-level rule is right for *scoring* but wrong for
    *folding*. One ``line: null`` verdict matched every line-level entry in
    the same file and re-ruled them all. Observed on kernel-8229: four
    file-level verdicts in one file walked line 58 through
    TP -> TP -> TP -> FP -> FP -> TP, parking it in ``contested`` where no
    later round could resolve it. Two PRs were left uncollectable.
    """

    def test_file_level_verdict_does_not_flip_line_level_entry(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [_fv("a.py", 58, "true_positive")],
            "census": [],
        })
        # A *different* defect in the same file, reported file-level.
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [_fv("a.py", None, "false_positive")],
            "census": [],
        })
        # Two distinct entries, nothing contested: the line-level TP stands
        # and the file-level FP is its own row.
        self.assertEqual(len(c.contested), 0)
        self.assertEqual([e.line for e in c.true_positives], [58])
        self.assertEqual([e.line for e in c.false_positives], [None])

    def test_line_level_verdict_does_not_flip_file_level_entry(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [_fv("a.py", None, "true_positive")],
            "census": [],
        })
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [_fv("a.py", 14, "false_positive")],
            "census": [],
        })
        self.assertEqual(len(c.contested), 0)
        self.assertEqual([e.line for e in c.true_positives], [None])
        self.assertEqual([e.line for e in c.false_positives], [14])

    def test_file_level_entries_still_match_each_other(self):
        # null-to-null is the same defect, so a genuine flip still registers.
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [_fv("a.py", None, "true_positive")],
            "census": [],
        })
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [_fv("a.py", None, "false_positive")],
            "census": [],
        })
        self.assertEqual(len(c.contested), 1)
        self.assertIsNone(c.contested[0].line)

    def test_kernel_8229_shape_converges(self):
        """The exact shape that deadlocked kernel-8229.

        One file, four line-level defects plus a file-level one, both rounds
        agreeing. Before the fix the file-level verdict re-ruled the
        line-level entries repeatedly and left them contested forever.

        Only one file-level verdict here: two of them on the same path *are*
        one defect by construction, so a TP and an FP among them is a genuine
        flip and would contest correctly. The bug was never about that — it
        was file-level bleeding into line-level.
        """
        # 76 and 78 are within _LINE_SLACK, so they collapse into one entry
        # by design — the distinct line-level defects here are 58, 76, 97.
        line_fvs = [_fv("r.py", n, "true_positive") for n in (58, 76, 78, 97)]
        file_fvs = [_fv("r.py", None, "true_positive")]
        c = cl.CumulativeGT()
        for rnd in (1, 2):
            cl.merge_judge_round(c, rnd, {
                "finding_verdicts": line_fvs + file_fvs, "census": [],
            })
        # Both rounds agreed on everything, so nothing may be contested and
        # the run must be able to converge.
        self.assertEqual(cl.unresolved_contested(c), [])
        self.assertTrue(cl.census_converged(c))
        self.assertEqual(
            sorted(e.line for e in c.true_positives if e.line is not None),
            [58, 76, 97],
        )
        # The file-level verdict stands as its own entry, not folded into one
        # of the line-level ones.
        self.assertEqual(
            [e.line for e in c.true_positives if e.line is None], [None]
        )

    def test_line_slack_still_groups_nearby_lines(self):
        # The strict rule is about None, not about slack: two verdicts a few
        # rows apart are still the same defect and must still flip.
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [_fv("a.py", 100, "true_positive")],
            "census": [],
        })
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [_fv("a.py", 100, "false_positive")],
            "census": [],
        })
        self.assertEqual(len(c.contested), 1)


class ContestedVerdictTests(unittest.TestCase):
    """A finding judged inconsistently across rounds must be quarantined,
    never silently kept in TP or FP.

    Regression intent: the accumulator used to keep whichever round came
    first, so a converged dataset could contain a finding the judges never
    actually agreed on.
    """

    def test_flip_is_binding_and_lands_in_the_new_bucket(self):
        """A later round's verdict overrides an earlier one.

        Regression intent: the flip used to mark the entry ``resolved=False``
        and leave it in ``contested`` only. That stranded it — the frozen GT
        scores the TP/FP buckets, so a contested-only entry vanished from the
        ground truth — and it could never be cleared, because the
        disagreement is *created* by this verdict, so no round could re-rule
        an entry that was not contested when the round began. Observed on
        kernel-8329 with a 2-round budget.
        """
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [_fv("a.py", 10, "true_positive")],
            "census": [],
        })
        self.assertEqual(len(c.true_positives), 1)
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [_fv("a.py", 10, "false_positive")],
            "census": [],
        })
        # Round 2 wins: the defect is now a false positive, and is scoreable.
        self.assertEqual(len(c.true_positives), 0)
        self.assertEqual(len(c.false_positives), 1)
        # The disagreement is still auditable...
        self.assertEqual(len(c.contested), 1)
        # ...but it is settled, so it does not block convergence.
        self.assertTrue(c.contested[0].resolved)
        self.assertEqual(cl.unresolved_contested(c), [])
        # Both verdicts remain in the history.
        kinds = [h["verdict"] for h in c.false_positives[0].verdict_history]
        self.assertEqual(kinds, ["true_positive", "false_positive"])

    def test_flip_does_not_block_convergence(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [_fv("a.py", 10, "true_positive")],
            "census": [],
        })
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [_fv("a.py", 10, "false_positive")],
            "census": [],
        })
        # Census empty + stable and the flip is binding -> converged.
        self.assertEqual(len(cl.unresolved_contested(c)), 0)
        self.assertTrue(cl.census_converged(c))

    def test_flip_back_in_a_third_round_keeps_the_latest_verdict(self):
        c = cl.CumulativeGT()
        for rnd, verdict in ((1, "true_positive"), (2, "false_positive"),
                             (3, "true_positive")):
            cl.merge_judge_round(c, rnd, {
                "finding_verdicts": [_fv("a.py", 10, verdict)], "census": [],
            })
        self.assertEqual(len(c.true_positives), 1)
        self.assertEqual(len(c.false_positives), 0)
        self.assertEqual(cl.unresolved_contested(c), [])

    def test_round_verdict_resolves_contested(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [_fv("a.py", 10, "true_positive")],
            "census": [],
        })
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [_fv("a.py", 10, "false_positive")],
            "census": [],
        })
        # Round 3's judge re-rules it false_positive — binding.
        cl.merge_judge_round(c, 3, {
            "finding_verdicts": [_fv("a.py", 10, "false_positive")],
            "census": [],
        })
        # `contested` is a permanent audit record of the disagreement, not a
        # quarantine queue — the entry stays listed but is settled, and it is
        # the TP/FP buckets that carry the scoreable ground truth.
        self.assertEqual(len(c.contested), 1)
        self.assertTrue(c.contested[0].resolved)
        self.assertEqual(len(c.false_positives), 1)
        self.assertTrue(c.false_positives[0].resolved)
        # All three rounds are in the history.
        self.assertEqual(len(c.false_positives[0].verdict_history), 3)
        # Census empty+stable, no unresolved contested -> converged.
        self.assertTrue(cl.census_converged(c))

    def test_agreement_across_rounds_is_not_contested(self):
        c = cl.CumulativeGT()
        for r in (1, 2):
            cl.merge_judge_round(c, r, {
                "finding_verdicts": [_fv("a.py", 10, "true_positive")],
                "census": [],
            })
        # Same verdict twice — accumulates, never contested.
        self.assertEqual(len(c.contested), 0)
        self.assertEqual(len(c.true_positives), 1)
        self.assertEqual(len(c.true_positives[0].verdict_history), 2)

    def test_unsure_does_not_flip_or_resolve(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [_fv("a.py", 10, "true_positive")],
            "census": [],
        })
        # An `unsure` on the same defect carries no ground truth.
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [_fv("a.py", 10, "unsure")],
            "census": [],
        })
        self.assertEqual(len(c.true_positives), 1)
        self.assertEqual(len(c.contested), 0)


class JudgeSeverityValidationTests(unittest.TestCase):
    def test_tp_verdict_needs_valid_severity(self):
        v = {"finding_verdicts": [
            {"file": "a.py", "line": 1, "verdict": "true_positive",
             "severity": "critical"}]}  # not in vocabulary
        self.assertIsNotNone(cl.validate_judge_verdict(v))

    def test_tp_verdict_missing_severity_rejected(self):
        v = {"finding_verdicts": [
            {"file": "a.py", "line": 1, "verdict": "true_positive"}]}
        self.assertIsNotNone(cl.validate_judge_verdict(v))

    def test_valid_severities_accepted(self):
        for sev in ("high", "medium", "low", "nit"):
            v = {"finding_verdicts": [
                {"file": "a.py", "line": 1, "verdict": "false_positive",
                 "severity": sev}]}
            self.assertIsNone(cl.validate_judge_verdict(v), sev)

    def test_unsure_verdict_needs_no_severity(self):
        v = {"finding_verdicts": [
            {"file": "a.py", "line": 1, "verdict": "unsure"}]}
        self.assertIsNone(cl.validate_judge_verdict(v))


class ValidateDatasetTests(unittest.TestCase):
    def _gt_block(self, **over):
        block = {
            "schema_version": cl.GROUND_TRUTH_SCHEMA_VERSION,
            "census_converged": True,
            "rounds_run": 2,
            "true_positives": [
                {"file": "a.py", "line": 10, "severity": "high"}],
            "false_positives": [],
            "contested": [],
            "per_round_diff": [],
            "dataset_xref": {"comment_action_agreement_rate": 1.0},
        }
        block.update(over)
        return {"schema_version": 2, "harvested_rounds": [], **(
            {"ground_truth_v3": block})}

    def test_clean_dataset_no_errors_no_warnings(self):
        errs, warns = cl.validate_dataset(self._gt_block())
        self.assertEqual(errs, [])
        self.assertEqual(warns, [])

    def test_uncollected_dataset_warns(self):
        errs, warns = cl.validate_dataset(
            {"schema_version": 2, "harvested_rounds": []})
        self.assertEqual(errs, [])
        self.assertTrue(any("not collected" in w for w in warns))

    def test_missing_severity_is_error(self):
        ds = self._gt_block()
        del ds["ground_truth_v3"]["true_positives"][0]["severity"]
        errs, _ = cl.validate_dataset(ds)
        self.assertTrue(any("severity" in e for e in errs))

    def test_not_converged_warns(self):
        errs, warns = cl.validate_dataset(
            self._gt_block(census_converged=False))
        self.assertEqual(errs, [])
        self.assertTrue(any("did not converge" in w for w in warns))

    def test_unresolved_contested_warns(self):
        ds = self._gt_block(contested=[
            {"file": "x.py", "line": 1, "severity": "high",
             "resolved": False}])
        errs, warns = cl.validate_dataset(ds)
        self.assertTrue(any("contested" in w for w in warns))

    def test_low_agreement_warns(self):
        ds = self._gt_block(
            dataset_xref={"comment_action_agreement_rate": 0.3})
        _, warns = cl.validate_dataset(ds)
        self.assertTrue(any("agreement" in w for w in warns))

    def test_budget_exhausted_warns(self):
        _, warns = cl.validate_dataset(
            self._gt_block(rounds_run=4), round_budget=4)
        self.assertTrue(any("budget" in w for w in warns))

    def test_schema_3_block_warns(self):
        _, warns = cl.validate_dataset(self._gt_block(schema_version=3))
        self.assertTrue(any("schema" in w for w in warns))


class RefreshIndexEntryTests(unittest.TestCase):
    def test_patches_one_entry_leaves_others(self):
        with tempfile.TemporaryDirectory() as d:
            out = Path(d)
            (out / "kernel-1.json").write_text(json.dumps({
                "schema_version": 2,
                "ground_truth_v3": {"schema_version": 4,
                                    "census_converged": True}}))
            (out / "kernel-2.json").write_text(json.dumps(
                {"schema_version": 2}))
            (out / "index.json").write_text(json.dumps({
                "schema_version": 2,
                "prs": [
                    {"file": "kernel-1.json", "ground_truth_collected": False,
                     "census_converged": None},
                    {"file": "kernel-2.json", "ground_truth_collected": False,
                     "census_converged": None},
                ]}))
            cl.refresh_index_entry(out, "kernel-1")
            index = json.loads((out / "index.json").read_text())
            e1 = next(e for e in index["prs"] if e["file"] == "kernel-1.json")
            e2 = next(e for e in index["prs"] if e["file"] == "kernel-2.json")
            self.assertTrue(e1["ground_truth_collected"])
            self.assertTrue(e1["census_converged"])
            # The untouched entry stays as it was.
            self.assertFalse(e2["ground_truth_collected"])

    def test_no_index_is_noop(self):
        with tempfile.TemporaryDirectory() as d:
            self.assertIsNone(cl.refresh_index_entry(Path(d), "kernel-1"))


class SelectTargetsTests(unittest.TestCase):
    """Collection's scan: which PRs `collect` processes by default."""

    def _fixture(self, d: Path) -> tuple[Path, Path]:
        """Build a projects dir (3 PRs) + a dataset dir (1 collected)."""
        src = d / "projects"
        ds = d / "dataset"
        src.mkdir()
        ds.mkdir()
        for pr in ("kernel-1", "kernel-2", "kernel-3"):
            (src / pr).mkdir()
            (src / pr / "pr-polish-state.json").write_text("{}")
        # kernel-2 already has a frozen ground truth.
        (ds / "kernel-2.json").write_text(json.dumps(
            {"schema_version": 2,
             "ground_truth_v3": {"schema_version": 4}}))
        # kernel-3 has a dataset file but no GT block (harvested, not collected).
        (ds / "kernel-3.json").write_text(json.dumps({"schema_version": 2}))
        return ds, src

    def test_default_scan_is_uncollected_only(self):
        with tempfile.TemporaryDirectory() as d:
            ds, src = self._fixture(Path(d))
            targets, collectable = collect.select_targets(
                dataset_dir=ds, source_dir=src, only=[], sample=None)
            self.assertEqual(len(collectable), 3)
            # kernel-2 is collected -> excluded; 1 and 3 remain.
            self.assertEqual(targets, ["kernel-1", "kernel-3"])

    def test_sample_caps_the_target_count(self):
        with tempfile.TemporaryDirectory() as d:
            ds, src = self._fixture(Path(d))
            targets, _ = collect.select_targets(
                dataset_dir=ds, source_dir=src, only=[], sample=1)
            self.assertEqual(len(targets), 1)
            self.assertIn(targets[0], ("kernel-1", "kernel-3"))

    def test_only_overrides_the_scan(self):
        with tempfile.TemporaryDirectory() as d:
            ds, src = self._fixture(Path(d))
            targets, _ = collect.select_targets(
                dataset_dir=ds, source_dir=src,
                only=["kernel-2"], sample=None)
            # --only is honored verbatim, even for an already-collected PR.
            self.assertEqual(targets, ["kernel-2"])

    def test_discover_collectable_flags_collected(self):
        with tempfile.TemporaryDirectory() as d:
            ds, src = self._fixture(Path(d))
            by_target = {
                c["target"]: c["collected"]
                for c in collect.discover_collectable(
                    dataset_dir=ds, source_dir=src)
            }
            self.assertFalse(by_target["kernel-1"])
            self.assertTrue(by_target["kernel-2"])
            self.assertFalse(by_target["kernel-3"])  # harvested != collected


class RepoRootDiscoveryTests(unittest.TestCase):
    def test_discover_keys_by_repo_name(self):
        # discover_repo_roots reads the real ~/worktrees + ~/g; assert it
        # returns a name->dir map and every value is a git checkout.
        roots = hl.discover_repo_roots()
        self.assertIsInstance(roots, dict)
        for name, path in roots.items():
            self.assertTrue((path / ".git").exists(), f"{name}: {path}")

    def test_repomap_discover_merges_overrides(self):
        rm = hl.RepoMap.discover(["zzz-custom=/tmp/zzz"])
        self.assertEqual(rm.lookup("zzz-custom"), Path("/tmp/zzz"))


if __name__ == "__main__":
    unittest.main()


class UnchangedFileTruePositiveTests(unittest.TestCase):
    """GT entries in unmodified files are incomplete-change defects.

    Regression intent: a judge censuses the diff *and its neighbourhood*,
    so it can record a real defect in a file the PR never modified. A
    reviewer scoped to the diff cannot report it, so the entry silently
    subtracts from every recall figure. Measured across the corpus on
    2026-08-06: 14 of 196 true positives (7.1%) in 11 of 48 records, and
    one record entirely out of scope.
    """

    def _rec(self, changed, tp_files):
        return {
            "schema_version": 3,
            "harvested_rounds": [{"files_changed": changed}],
            "ground_truth_v3": {
                "schema_version": 4,
                "true_positives": [
                    {"file": f, "line": 1, "severity": "high", "topic": "t"}
                    for f in tp_files
                ],
                "false_positives": [],
                "contested": [],
                "per_round_diff": [],
                "census_converged": True,
            },
        }

    def test_flags_entry_outside_files_changed(self):
        rec = self._rec(["a.py"], ["a.py", "b.py"])
        oos = cl.unchanged_file_true_positives(rec)
        self.assertEqual([e["file"] for e in oos], ["b.py"])

    def test_all_in_scope_returns_empty(self):
        rec = self._rec(["a.py", "b.py"], ["a.py", "b.py"])
        self.assertEqual(cl.unchanged_file_true_positives(rec), [])

    def test_empty_files_changed_flags_nothing(self):
        # An empty scope means "unknown", not "nothing was changed".
        # Guessing would reclassify every entry and mask the real ones.
        rec = self._rec([], ["a.py", "b.py"])
        self.assertEqual(cl.unchanged_file_true_positives(rec), [])

    def test_missing_rounds_flags_nothing(self):
        rec = self._rec(["a.py"], ["b.py"])
        rec["harvested_rounds"] = []
        self.assertEqual(cl.unchanged_file_true_positives(rec), [])

    def test_validate_warns_not_errors(self):
        # Warn so the miss can be attributed, not so the denominator can
        # be shrunk — see the helper docstring.
        errors, warnings = cl.validate_dataset(self._rec(["a.py"], ["a.py", "b.py"]))
        self.assertEqual(errors, [])
        self.assertTrue(
            any("did not modify" in w for w in warnings),
            f"expected an out-of-diff warning, got {warnings}",
        )
