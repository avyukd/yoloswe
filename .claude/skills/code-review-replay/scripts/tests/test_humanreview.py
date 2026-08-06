"""Unit tests for the human review overlay (humanreview_lib).

The overlay is the only path by which a human edit reaches the benchmark's
source of truth, so these tests concentrate on the ways that path could
corrupt it: non-idempotent application, defect-identity collisions, verdicts
applied to a census that changed underneath them, and an unstrippable pass.
"""

from __future__ import annotations

import copy
import json
import sys
import tempfile
import unittest
from pathlib import Path

TEST_DIR = Path(__file__).resolve().parent
SCRIPT_DIR = TEST_DIR.parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

import collect_lib as cl  # noqa: E402
import humanreview_lib as hr  # noqa: E402


def _entry(file, line, *, severity="high", topic="t", round_n=1):
    return {
        "file": file, "line": line, "severity": severity, "topic": topic,
        "first_seen_round": round_n, "surfaced_by": ["codex"],
        "judge_reason": "because", "reviewer_severity": severity,
        "verdict_history": [{"round": round_n, "verdict": "true_positive",
                             "reason": "because"}],
        "resolved": True, "comment_action_xref": None,
    }


def _gt(tps=(), fps=()):
    return {
        "schema_version": 4, "frozen_at": "2026-01-01T00:00:00Z",
        "collector_git_sha": "abc", "rounds_run": 2,
        "census_converged": True,
        "true_positives": [copy.deepcopy(e) for e in tps],
        "false_positives": [copy.deepcopy(e) for e in fps],
        "contested": [], "per_round_diff": [{"round": 1}], "dataset_xref": {},
    }


def _v(op, file, line, **kw):
    v = {"op": op, "file": file, "line": line, "reviewer": "tester",
         "at": "2026-08-06T00:00:00Z", "reason": "human says so"}
    v.update(kw)
    return v


def _overlay(*verdicts, target="kernel-1", fingerprint="sha256:x"):
    return {
        "schema_version": hr.OVERLAY_SCHEMA_VERSION, "target": target,
        "gt_frozen_at": "2026-01-01T00:00:00Z",
        "gt_fingerprint": fingerprint, "verdicts": list(verdicts),
    }


class IdempotencyTests(unittest.TestCase):
    """Applying one overlay twice must equal applying it once.

    `collect.py fold` is famously NOT idempotent — it appends to
    `per_round_diff` and `verdict_history`, and running it twice on a round
    inflates the record. The human path must not inherit that property,
    because a human will re-apply after editing one verdict in a ten-verdict
    overlay.
    """

    def test_confirm_twice_is_stable(self):
        gt = _gt(tps=[_entry("a/b.py", 10)])
        ov = _overlay(_v(hr.OP_CONFIRM, "a/b.py", 10))
        once, _ = hr.apply_human_review(gt, ov)
        twice, _ = hr.apply_human_review(once, ov)
        self.assertEqual(once, twice)

    def test_reject_twice_does_not_bounce_the_entry_back(self):
        """A second reject must not move the entry back to where it started.

        The entry now lives in `false_positives`, so a naive implementation
        that flips "whichever bucket it is in" would oscillate on every apply.
        """
        gt = _gt(tps=[_entry("a/b.py", 10)])
        ov = _overlay(_v(hr.OP_REJECT, "a/b.py", 10))
        once, _ = hr.apply_human_review(gt, ov)
        twice, _ = hr.apply_human_review(once, ov)
        self.assertEqual(len(once["false_positives"]), 1)
        self.assertEqual(len(once["true_positives"]), 0)
        self.assertEqual(once, twice)

    def test_reseverity_twice_preserves_the_original_judge_severity(self):
        """`judge_severity` must record the JUDGE's value, not the last one.

        Overwriting it on each apply would lose the original after two runs,
        making the strip path unable to restore it.
        """
        gt = _gt(tps=[_entry("a/b.py", 10, severity="high")])
        ov = _overlay(_v(hr.OP_RESEVERITY, "a/b.py", 10, severity="nit"))
        once, _ = hr.apply_human_review(gt, ov)
        twice, _ = hr.apply_human_review(once, ov)
        self.assertEqual(once["true_positives"][0]["judge_severity"], "high")
        self.assertEqual(twice["true_positives"][0]["judge_severity"], "high")
        self.assertEqual(twice["true_positives"][0]["severity"], "nit")

    def test_add_twice_does_not_duplicate(self):
        gt = _gt()
        ov = _overlay(_v(hr.OP_ADD, "a/b.py", 10, severity="medium",
                         topic="missing null check"))
        once, _ = hr.apply_human_review(gt, ov)
        twice, _ = hr.apply_human_review(once, ov)
        self.assertEqual(len(once["true_positives"]), 1)
        self.assertEqual(len(twice["true_positives"]), 1)


class RoundAccountingTests(unittest.TestCase):
    """A human pass is not a judge round and must not look like one."""

    def test_does_not_touch_round_bookkeeping(self):
        gt = _gt(tps=[_entry("a/b.py", 10)])
        ov = _overlay(_v(hr.OP_REJECT, "a/b.py", 10))
        out, _ = hr.apply_human_review(gt, ov)
        self.assertEqual(out["rounds_run"], gt["rounds_run"])
        self.assertEqual(out["per_round_diff"], gt["per_round_diff"])
        self.assertEqual(out["census_converged"], gt["census_converged"])

    def test_reject_does_not_write_contested_or_verdict_history(self):
        """The judge trail stays machine-only.

        `fold`'s flip branch would append a human rejection to `contested`
        and to `verdict_history`, making a human-vs-machine disagreement
        indistinguishable from two judges disagreeing. That conflation is the
        whole reason this module does not reuse `fold`.
        """
        gt = _gt(tps=[_entry("a/b.py", 10)])
        ov = _overlay(_v(hr.OP_REJECT, "a/b.py", 10))
        out, _ = hr.apply_human_review(gt, ov)
        moved = out["false_positives"][0]
        self.assertEqual(out["contested"], [])
        self.assertEqual(len(moved["verdict_history"]), 1)
        self.assertEqual(moved["verdict_history"][0]["round"], 1)

    def test_added_entry_is_marked_not_judge_derived(self):
        gt = _gt()
        ov = _overlay(_v(hr.OP_ADD, "a/b.py", 10, severity="low", topic="x"))
        out, _ = hr.apply_human_review(gt, ov)
        added = out["true_positives"][0]
        self.assertIsNone(added["first_seen_round"])
        self.assertEqual(added["surfaced_by"], ["human"])


class DefectIdentityTests(unittest.TestCase):
    """The ±3-row / null-to-null identity rule, applied to human input."""

    def test_line_slack_matches_a_nearby_entry(self):
        """A verdict 2 rows off must hit the existing entry, not miss it.

        Reviewers and humans anchor the same defect at slightly different
        lines; `_LINE_SLACK` exists so that counts as agreement.
        """
        gt = _gt(tps=[_entry("a/b.py", 10)])
        ov = _overlay(_v(hr.OP_CONFIRM, "a/b.py", 12))
        out, notes = hr.apply_human_review(gt, ov)
        self.assertIn("human_verdict", out["true_positives"][0])
        self.assertFalse(any("SKIPPED" in n for n in notes))

    def test_file_level_verdict_does_not_rerule_line_level_entries(self):
        """The kernel-8229 regression, in human form.

        `same_defect` lets a file-level location subsume every line-level one
        in the file. If the human path used that rule, ONE file-level verdict
        would silently re-rule every entry in the file. Identity here must be
        the strict `_entry_matches` rule: None matches only None.
        """
        gt = _gt(tps=[_entry("a/b.py", 10), _entry("a/b.py", 58)])
        ov = _overlay(_v(hr.OP_REJECT, "a/b.py", None))
        out, notes = hr.apply_human_review(gt, ov)
        # Nothing moved: there is no file-level entry to reject.
        self.assertEqual(len(out["true_positives"]), 2)
        self.assertEqual(len(out["false_positives"]), 0)
        self.assertTrue(any("SKIPPED" in n for n in notes))

    def test_file_level_and_line_level_coexist_as_distinct_entries(self):
        gt = _gt(tps=[_entry("a/b.py", None), _entry("a/b.py", 58)])
        ov = _overlay(_v(hr.OP_REJECT, "a/b.py", None))
        out, _ = hr.apply_human_review(gt, ov)
        self.assertEqual(len(out["false_positives"]), 1)
        self.assertIsNone(out["false_positives"][0]["line"])
        self.assertEqual(len(out["true_positives"]), 1)
        self.assertEqual(out["true_positives"][0]["line"], 58)

    def test_add_onto_an_existing_entry_annotates_instead_of_duplicating(self):
        """An `add` within slack of an entry must not create a colliding pair.

        Two entries at one identity is precisely the shape
        `_file_level_collision_error` rejects at the judge boundary.
        """
        gt = _gt(tps=[_entry("a/b.py", 10)])
        ov = _overlay(_v(hr.OP_ADD, "a/b.py", 11, severity="low", topic="x"))
        out, notes = hr.apply_human_review(gt, ov)
        self.assertEqual(len(out["true_positives"]), 1)
        self.assertTrue(any("annotated it instead" in n for n in notes))

    def test_path_normalization_matches_worktree_absolute_paths(self):
        gt = _gt(tps=[_entry("src/a.py", 10)])
        ov = _overlay(_v(hr.OP_CONFIRM, "/tmp/replay-kernel-1-r1-xy/src/a.py",
                         10))
        out, notes = hr.apply_human_review(gt, ov)
        self.assertIn("human_verdict", out["true_positives"][0])
        self.assertFalse(any("SKIPPED" in n for n in notes))


class OverlayValidationTests(unittest.TestCase):

    def test_accepts_a_well_formed_overlay(self):
        ov = _overlay(_v(hr.OP_CONFIRM, "a/b.py", 10))
        self.assertIsNone(hr.validate_overlay(ov))

    def test_rejects_missing_line_key(self):
        """An omitted line must not be silently read as file-level."""
        v = _v(hr.OP_CONFIRM, "a/b.py", 10)
        del v["line"]
        self.assertIn("missing 'line'", hr.validate_overlay(_overlay(v)) or "")

    def test_allows_null_line(self):
        self.assertIsNone(hr.validate_overlay(
            _overlay(_v(hr.OP_CONFIRM, "a/b.py", None))))

    def test_rejects_bad_severity(self):
        err = hr.validate_overlay(
            _overlay(_v(hr.OP_RESEVERITY, "a/b.py", 10, severity="critical")))
        self.assertIn("severity", err or "")

    def test_rejects_add_without_topic(self):
        err = hr.validate_overlay(
            _overlay(_v(hr.OP_ADD, "a/b.py", 10, severity="low")))
        self.assertIn("topic", err or "")

    def test_rejects_missing_reviewer(self):
        v = _v(hr.OP_CONFIRM, "a/b.py", 10)
        del v["reviewer"]
        self.assertIn("reviewer", hr.validate_overlay(_overlay(v)) or "")

    def test_rejects_conflicting_ops_within_line_slack(self):
        """Two ops 2 rows apart target ONE entry — the later would win silently."""
        err = hr.validate_overlay(_overlay(
            _v(hr.OP_CONFIRM, "a/b.py", 10),
            _v(hr.OP_REJECT, "a/b.py", 12)))
        self.assertIsNotNone(err)
        self.assertIn("collides", err)

    def test_allows_same_op_repeated_at_one_location(self):
        self.assertIsNone(hr.validate_overlay(_overlay(
            _v(hr.OP_CONFIRM, "a/b.py", 10),
            _v(hr.OP_CONFIRM, "a/b.py", 11))))

    def test_file_level_and_line_level_are_not_a_collision(self):
        self.assertIsNone(hr.validate_overlay(_overlay(
            _v(hr.OP_CONFIRM, "a/b.py", None),
            _v(hr.OP_REJECT, "a/b.py", 58))))


class FingerprintTests(unittest.TestCase):
    """The guard against applying stale verdicts to a re-collected census."""

    def test_stable_across_reordering(self):
        a = _gt(tps=[_entry("a/b.py", 10), _entry("c/d.py", 20)])
        b = _gt(tps=[_entry("c/d.py", 20), _entry("a/b.py", 10)])
        self.assertEqual(hr.gt_fingerprint(a), hr.gt_fingerprint(b))

    def test_ignores_refreeze_only_metadata(self):
        """Re-freezing with unchanged findings must not invalidate a review."""
        a = _gt(tps=[_entry("a/b.py", 10)])
        b = _gt(tps=[_entry("a/b.py", 10)])
        b["frozen_at"] = "2027-09-09T00:00:00Z"
        b["collector_git_sha"] = "different"
        b["per_round_diff"] = [{"round": 9}]
        self.assertEqual(hr.gt_fingerprint(a), hr.gt_fingerprint(b))

    def test_changes_when_a_finding_is_added(self):
        a = _gt(tps=[_entry("a/b.py", 10)])
        b = _gt(tps=[_entry("a/b.py", 10), _entry("c/d.py", 20)])
        self.assertNotEqual(hr.gt_fingerprint(a), hr.gt_fingerprint(b))

    def test_survives_its_own_application(self):
        """Applying an overlay must not invalidate that overlay.

        Regression: the first implementation hashed each bucket separately,
        so a `reject` (which MOVES an entry between buckets) and an `add`
        (which appends one) both changed the fingerprint. The second,
        idempotent apply then aborted with a spurious "ground truth changed"
        error — caught only in end-to-end testing, because the original test
        exercised `reseverity` alone. All four ops are checked here.
        """
        gt = _gt(tps=[_entry("a/b.py", 10, severity="high"),
                      _entry("c/d.py", 20), _entry("e/f.py", 30)],
                 fps=[_entry("g/h.py", 40)])
        before = hr.gt_fingerprint(gt)
        ov = _overlay(
            _v(hr.OP_CONFIRM, "c/d.py", 20),
            _v(hr.OP_REJECT, "a/b.py", 10),
            _v(hr.OP_RESEVERITY, "e/f.py", 30, severity="nit"),
            _v(hr.OP_ADD, "new/x.py", 5, severity="high", topic="found"),
            fingerprint=before)
        out, _ = hr.apply_human_review(gt, ov)
        self.assertEqual(
            hr.gt_fingerprint(out), before,
            "applying an overlay must not change the fingerprint it pinned")

    def test_still_detects_a_genuine_recollection(self):
        """The guard must not be so loose it stops catching real drift."""
        gt = _gt(tps=[_entry("a/b.py", 10)])
        before = hr.gt_fingerprint(gt)
        recollected = _gt(tps=[_entry("a/b.py", 10),
                               _entry("brand/new.py", 3)])
        self.assertNotEqual(hr.gt_fingerprint(recollected), before)

    def test_detects_a_reworded_finding(self):
        gt = _gt(tps=[_entry("a/b.py", 10, topic="original wording")])
        other = _gt(tps=[_entry("a/b.py", 10, topic="judge reworded it")])
        self.assertNotEqual(hr.gt_fingerprint(gt), hr.gt_fingerprint(other))


class StripTests(unittest.TestCase):
    """A human pass must be fully reversible."""

    def test_round_trip_restores_the_original_block(self):
        gt = _gt(tps=[_entry("a/b.py", 10, severity="high"),
                      _entry("c/d.py", 20)],
                 fps=[_entry("e/f.py", 30, severity="low")])
        ov = _overlay(
            _v(hr.OP_CONFIRM, "c/d.py", 20),
            _v(hr.OP_REJECT, "a/b.py", 10),
            _v(hr.OP_RESEVERITY, "e/f.py", 30, severity="nit"),
            _v(hr.OP_ADD, "g/h.py", 40, severity="medium", topic="new bug"),
        )
        applied, _ = hr.apply_human_review(gt, ov)
        stripped, removed = hr.strip_human_review(applied)

        self.assertGreater(removed, 0)
        for bucket in ("true_positives", "false_positives"):
            key = lambda e: (e["file"], str(e["line"]))  # noqa: E731
            self.assertEqual(
                sorted(stripped[bucket], key=key),
                sorted(gt[bucket], key=key),
                f"{bucket} did not round-trip",
            )

    def test_strip_is_a_noop_on_an_untouched_block(self):
        gt = _gt(tps=[_entry("a/b.py", 10)])
        stripped, removed = hr.strip_human_review(gt)
        self.assertEqual(removed, 0)
        self.assertEqual(stripped, gt)


class ValidatorCompatibilityTests(unittest.TestCase):
    """A human-annotated block must stay consumable by existing tooling."""

    def test_applied_block_passes_validate_dataset(self):
        gt = _gt(tps=[_entry("a/b.py", 10)])
        ov = _overlay(
            _v(hr.OP_REJECT, "a/b.py", 10),
            _v(hr.OP_ADD, "c/d.py", 20, severity="medium", topic="new"),
        )
        applied, _ = hr.apply_human_review(gt, ov)
        dataset = {"schema_version": 3, "harvest_source": "pr-polish",
                   "ground_truth_v3": applied, "harvested_rounds": []}
        errors, _ = cl.validate_dataset(dataset)
        self.assertEqual(errors, [])


class UpsertTests(unittest.TestCase):

    def test_replaces_a_verdict_at_the_same_identity(self):
        ov = _overlay()
        hr.upsert_verdict(ov, _v(hr.OP_CONFIRM, "a/b.py", 10))
        hr.upsert_verdict(ov, _v(hr.OP_REJECT, "a/b.py", 11))
        self.assertEqual(len(ov["verdicts"]), 1)
        self.assertEqual(ov["verdicts"][0]["op"], hr.OP_REJECT)

    def test_keeps_file_level_and_line_level_separate(self):
        ov = _overlay()
        hr.upsert_verdict(ov, _v(hr.OP_CONFIRM, "a/b.py", None))
        hr.upsert_verdict(ov, _v(hr.OP_REJECT, "a/b.py", 58))
        self.assertEqual(len(ov["verdicts"]), 2)


class StatsTests(unittest.TestCase):

    def test_counts_pending_and_adjudicated(self):
        gt = _gt(tps=[_entry("a/b.py", 10), _entry("c/d.py", 20)],
                 fps=[_entry("e/f.py", 30)])
        ov = _overlay(_v(hr.OP_CONFIRM, "a/b.py", 10))
        applied, _ = hr.apply_human_review(gt, ov)
        stats = hr.human_review_stats(applied)
        self.assertEqual(stats["entries"], 3)
        self.assertEqual(stats["adjudicated"], 1)
        self.assertEqual(stats["pending"], 2)


class OverlayRoundTripTests(unittest.TestCase):

    def test_save_and_load(self):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            path = hr.overlay_path(root, "kernel-1")
            ov = _overlay(_v(hr.OP_CONFIRM, "a/b.py", 10))
            hr.save_overlay(path, ov)
            self.assertEqual(hr.load_overlay(path), ov)

    def test_load_missing_returns_none(self):
        with tempfile.TemporaryDirectory() as td:
            self.assertIsNone(
                hr.load_overlay(hr.overlay_path(Path(td), "nope")))


if __name__ == "__main__":
    unittest.main()
