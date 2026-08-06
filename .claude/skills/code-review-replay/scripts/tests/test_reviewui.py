"""Unit tests for the review UI read path (reviewui_lib).

The load-bearing property here is **completeness**: 28% of real ground-truth
findings do not land inside a rendered diff hunk (measured across 32 records:
71.6% in-hunk, 18.0% off-hunk, 10.3% not-in-diff). A UI that anchors findings
only to diff lines would silently hide them, so these tests pin the anchor
classifier and the ledger's no-drop guarantee.
"""

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

import reviewui_lib as rl  # noqa: E402

DIFF = """diff --git a/src/a.py b/src/a.py
index 1111111..2222222 100644
--- a/src/a.py
+++ b/src/a.py
@@ -10,6 +10,8 @@ def f():
     ctx_ten
     ctx_eleven
-    removed_line
+    added_twelve
+    added_thirteen
     ctx_fourteen
     ctx_fifteen
@@ -100,3 +102,4 @@ def g():
     ctx_onezerotwo
+    added_onezerothree
     ctx_onezerofour
diff --git a/src/b.py b/src/b.py
index 3333333..4444444 100644
--- a/src/b.py
+++ b/src/b.py
@@ -1,3 +1,4 @@
 first
+second_added
 third
"""


class ParseDiffTests(unittest.TestCase):

    def test_splits_files(self):
        files = rl.parse_diff(DIFF)
        self.assertEqual([f.path for f in files], ["src/a.py", "src/b.py"])

    def test_tracks_new_side_line_numbers(self):
        files = {f.path: f for f in rl.parse_diff(DIFF)}
        a = files["src/a.py"]
        # First hunk starts at new line 10 and renders 10..15.
        self.assertIn(10, a.new_lines)
        self.assertIn(12, a.new_lines)
        self.assertIn(15, a.new_lines)
        # Second hunk starts at 102.
        self.assertIn(102, a.new_lines)
        self.assertIn(103, a.new_lines)
        # The gap between hunks is not rendered.
        self.assertNotIn(50, a.new_lines)

    def test_deleted_lines_carry_no_new_line_number(self):
        files = {f.path: f for f in rl.parse_diff(DIFF)}
        dels = [l for l in files["src/a.py"].lines if l.kind == "del"]
        self.assertTrue(dels)
        self.assertTrue(all(d.new_line is None for d in dels))

    def test_handles_empty_input(self):
        self.assertEqual(rl.parse_diff(""), [])

    def test_added_file_does_not_leak_a_line_into_the_previous_file(self):
        """`--- /dev/null` is a header, not a deleted line.

        Found by rendering kernel-8276 in a browser: the old-side pattern
        anchored on `--- a/`, so an ADDED file's `--- /dev/null` fell through
        to the `-` branch and was drawn as a code line reading `-- /dev/null`
        at the tail of the preceding file.
        """
        diff = (
            "diff --git a/kept.py b/kept.py\n"
            "--- a/kept.py\n+++ b/kept.py\n"
            "@@ -1,1 +1,2 @@\n one\n+two\n"
            "diff --git a/new.py b/new.py\n"
            "new file mode 100644\n"
            "--- /dev/null\n+++ b/new.py\n"
            "@@ -0,0 +1,1 @@\n+alpha\n"
        )
        files = {f.path: f for f in rl.parse_diff(diff)}
        self.assertEqual(set(files), {"kept.py", "new.py"})
        self.assertNotIn(
            "dev/null",
            " ".join(l.text for l in files["kept.py"].lines),
            "the /dev/null header leaked into the previous file's lines")
        self.assertEqual(
            [l.kind for l in files["new.py"].lines if l.kind != "hunk"],
            ["add"])

    def test_deleted_file_is_skipped(self):
        diff = (
            "diff --git a/gone.py b/gone.py\n"
            "deleted file mode 100644\n"
            "--- a/gone.py\n+++ /dev/null\n"
            "@@ -1,1 +0,0 @@\n-was here\n"
            "diff --git a/kept.py b/kept.py\n"
            "--- a/kept.py\n+++ b/kept.py\n"
            "@@ -1,0 +1,1 @@\n+added\n"
        )
        files = rl.parse_diff(diff)
        self.assertEqual([f.path for f in files], ["kept.py"])


class ClassifyAnchorTests(unittest.TestCase):
    """Every finding must get a home — 28% of real ones are not in a hunk."""

    def setUp(self):
        self.files = rl.parse_diff(DIFF)

    def test_line_inside_a_hunk(self):
        self.assertEqual(
            rl.classify_anchor("src/a.py", 12, self.files),
            rl.ANCHOR_IN_HUNK)

    def test_line_in_changed_file_but_outside_any_hunk(self):
        """18% of real findings land here — the largest hidden class."""
        self.assertEqual(
            rl.classify_anchor("src/a.py", 50, self.files),
            rl.ANCHOR_OFF_HUNK)

    def test_file_not_in_the_diff_at_all(self):
        """10% of real findings: code the PR affects but does not edit."""
        self.assertEqual(
            rl.classify_anchor("src/never_touched.py", 5, self.files),
            rl.ANCHOR_NOT_IN_DIFF)

    def test_null_line_is_file_level(self):
        self.assertEqual(
            rl.classify_anchor("src/a.py", None, self.files),
            rl.ANCHOR_FILE_LEVEL)

    def test_null_line_on_an_untouched_file_is_still_file_level(self):
        """File-level wins over not-in-diff: there is no line to place."""
        self.assertEqual(
            rl.classify_anchor("src/never_touched.py", None, self.files),
            rl.ANCHOR_FILE_LEVEL)

    def test_absolute_worktree_path_normalizes_before_matching(self):
        """bramble emits WORK_DIR-absolute paths; the GT stores relative ones.

        Unstripped, the same file looks like two files and every finding in
        it would be misclassified as not-in-diff.
        """
        self.assertEqual(
            rl.classify_anchor("/tmp/replay-kernel-1-r1-xy/src/a.py", 12,
                               self.files),
            rl.ANCHOR_IN_HUNK)


def _gt(tps=(), fps=(), contested=()):
    def e(f, ln, **kw):
        d = {"file": f, "line": ln, "severity": "high", "topic": "t",
             "judge_reason": "r", "surfaced_by": ["codex"],
             "first_seen_round": 1}
        d.update(kw)
        return d
    return {
        "true_positives": [e(*t) for t in tps],
        "false_positives": [e(*t) for t in fps],
        "contested": [e(*t) for t in contested],
    }


class LedgerTests(unittest.TestCase):
    """The ledger is the completeness guarantee — nothing may be dropped."""

    def setUp(self):
        self.files = rl.parse_diff(DIFF)

    def test_every_judged_entry_appears(self):
        gt = _gt(tps=[("src/a.py", 12), ("src/a.py", 50)],
                 fps=[("src/gone.py", 7), ("src/a.py", None)])
        rows = rl.build_ledger(gt, self.files)
        self.assertEqual(len(rows), 4)

    def test_covers_all_four_anchor_classes(self):
        gt = _gt(tps=[("src/a.py", 12), ("src/a.py", 50)],
                 fps=[("src/gone.py", 7), ("src/a.py", None)])
        groups = rl.group_by_anchor(rl.build_ledger(gt, self.files))
        self.assertEqual(len(groups[rl.ANCHOR_IN_HUNK]), 1)
        self.assertEqual(len(groups[rl.ANCHOR_OFF_HUNK]), 1)
        self.assertEqual(len(groups[rl.ANCHOR_NOT_IN_DIFF]), 1)
        self.assertEqual(len(groups[rl.ANCHOR_FILE_LEVEL]), 1)

    def test_buckets_are_labeled(self):
        gt = _gt(tps=[("src/a.py", 12)], fps=[("src/a.py", 13)])
        rows = rl.build_ledger(gt, self.files)
        self.assertEqual(
            {r["bucket"] for r in rows},
            {"true_positives", "false_positives"})

    def test_contested_entry_already_in_a_bucket_is_not_duplicated(self):
        """`contested` is an audit record, not a separate queue.

        A flipped entry lives in BOTH its bucket and `contested`, so listing
        both unfiltered would double-count it in the ledger.
        """
        gt = _gt(tps=[("src/a.py", 12)], contested=[("src/a.py", 12)])
        rows = rl.build_ledger(gt, self.files)
        self.assertEqual(len(rows), 1)

    def test_contested_only_entry_is_surfaced(self):
        gt = _gt(tps=[("src/a.py", 12)], contested=[("src/b.py", 2)])
        rows = rl.build_ledger(gt, self.files)
        self.assertEqual(len(rows), 2)
        self.assertIn("contested", {r["bucket"] for r in rows})

    def test_suggestions_are_appended_and_marked(self):
        gt = _gt(tps=[("src/a.py", 12)])
        sugg = [{"file": "src/b.py", "line": 2, "n_runs": 3, "n_configs": 2,
                 "recurrent": True, "cross_config": True,
                 "configs": ["codex", "cursor"]}]
        rows = rl.build_ledger(gt, self.files, sugg)
        self.assertEqual(len(rows), 2)
        s = [r for r in rows if r["kind"] == "suggestion"][0]
        self.assertTrue(s["cross_config"])
        self.assertEqual(s["anchor"], rl.ANCHOR_IN_HUNK)

    def test_human_verdict_is_exposed(self):
        gt = _gt(tps=[("src/a.py", 12)])
        gt["true_positives"][0]["human_verdict"] = {
            "verdict": "confirm", "reviewer": "t", "at": "now", "reason": ""}
        rows = rl.build_ledger(gt, self.files)
        self.assertEqual(rows[0]["human_verdict"]["verdict"], "confirm")


class ResolveRevsTests(unittest.TestCase):
    """Missing commits are a normal state, not an error state."""

    def test_no_repo_is_unrecoverable(self):
        rec = {"harvested_rounds": [{"head_before": "a" * 40,
                                     "merge_base_sha": "b" * 40}]}
        got = rl.resolve_revs(rec, None)
        self.assertEqual(got["state"], rl.RENDER_UNRECOVERABLE)

    def test_missing_head_reports_the_fetch_remedy(self):
        """A missing head is usually just unfetched, and the UI should say so."""
        with tempfile.TemporaryDirectory() as td:
            rec = {"harvested_rounds": [{"head_before": "a" * 40,
                                         "merge_base_sha": "b" * 40}]}
            got = rl.resolve_revs(rec, Path(td))
            self.assertEqual(got["state"], rl.RENDER_NEEDS_FETCH)
            self.assertIn("refs/pull", got["detail"])

    def test_empty_record_does_not_crash(self):
        got = rl.resolve_revs({}, None)
        self.assertEqual(got["state"], rl.RENDER_UNRECOVERABLE)


class SummarizeRecordTests(unittest.TestCase):

    def test_reports_counts_and_source_badge(self):
        rec = {
            "pr": {"repo_name": "kernel", "pr_number": "1"},
            "harvest_source": "github",
            "harvested_rounds": [{"head_before": "a" * 40,
                                  "files_changed": ["x.py"]}],
            "ground_truth_v3": _gt(tps=[("a.py", 1)], fps=[("b.py", 2)]),
        }
        got = rl.summarize_record("kernel-1", rec, None)
        self.assertEqual(got["harvest_source"], "github")
        self.assertEqual(got["true_positives"], 1)
        self.assertEqual(got["false_positives"], 1)
        self.assertEqual(got["entries"], 2)
        self.assertEqual(got["pending"], 2)

    def test_staged_count_cannot_drive_pending_below_zero(self):
        """Staged progress must never make a PR look finished.

        Regression (cursor, PR #309 round 3): round 2 subtracted
        `len(overlay.verdicts)` from `pending`, but an `add` targets a
        location the census does not yet hold — it is new work, not a pending
        entry resolved. Staging promotions alone could drain `pending` to 0
        and sort a wholly unreviewed PR out of the queue. The caller now
        counts only verdicts matching an existing entry; this clamp is the
        second line of defence.
        """
        rec = {"pr": {}, "harvested_rounds": [],
               "ground_truth_v3": _gt(tps=[("a.py", 1), ("b.py", 2)])}
        got = rl.summarize_record("k-1", rec, None,
                                  staged_counts={"k-1": 99})
        self.assertEqual(got["entries"], 2)
        self.assertEqual(got["pending"], 0)
        self.assertEqual(got["staged"], 2, "staged is clamped to pending")

    def test_staged_count_reduces_pending_normally(self):
        rec = {"pr": {}, "harvested_rounds": [],
               "ground_truth_v3": _gt(tps=[("a.py", 1), ("b.py", 2)])}
        got = rl.summarize_record("k-1", rec, None, staged_counts={"k-1": 1})
        self.assertEqual(got["pending"], 1)
        self.assertEqual(got["staged"], 1)

    def test_defaults_missing_harvest_source_to_pr_polish(self):
        """Schema-2 records predate the field and read as pr-polish."""
        rec = {"pr": {}, "harvested_rounds": [],
               "ground_truth_v3": _gt(tps=[("a.py", 1)])}
        self.assertEqual(
            rl.summarize_record("kernel-1", rec, None)["harvest_source"],
            "pr-polish")


class LoadSuggestionsTests(unittest.TestCase):

    def test_reads_unmatched_findings_and_ranks_recurrence(self):
        with tempfile.TemporaryDirectory() as td:
            d = Path(td)
            for i in (1, 2):
                (d / f"kernel-1-2026010{i}-scored.json").write_text(json.dumps({
                    "dataset_file": "kernel-1.json",
                    "rounds": [{"runs": [{
                        "config": f"cfg{i}",
                        "finding_scores": [
                            {"file": "src/x.py", "line": 10,
                             "outcome": "unmatched"},
                            {"file": "src/y.py", "line": 5,
                             "outcome": "matched_tp"},
                        ],
                    }]}],
                }))
            got = rl.load_suggestions("kernel-1", {}, d)
        self.assertEqual(len(got), 1)
        self.assertTrue(got[0]["cross_config"])
        self.assertEqual(got[0]["n_runs"], 2)

    def test_ignores_other_prs(self):
        with tempfile.TemporaryDirectory() as td:
            d = Path(td)
            (d / "kernel-9-scored.json").write_text(json.dumps({
                "dataset_file": "kernel-9.json",
                "rounds": [{"runs": [{"config": "c", "finding_scores": [
                    {"file": "z.py", "line": 1, "outcome": "unmatched"}]}]}],
            }))
            self.assertEqual(rl.load_suggestions("kernel-1", {}, d), [])

    def test_no_replays_is_empty_not_an_error(self):
        with tempfile.TemporaryDirectory() as td:
            self.assertEqual(rl.load_suggestions("kernel-1", {}, Path(td)), [])

    def test_reads_only_the_most_recent_window(self):
        """The replay dir is an append-only log across reviewer regimes.

        kernel-8276 alone carries 93 scored archives spanning configurations
        from before and after the `--diff-base` fix. Summing all of history
        inflates the candidate list with locations no current config reports,
        so only the most recent `window` archives count — and the counts are
        reported so the truncation is visible rather than silent.
        """
        with tempfile.TemporaryDirectory() as td:
            d = Path(td)
            for i in range(1, 6):
                (d / f"kernel-1-2026010{i}-scored.json").write_text(json.dumps({
                    "dataset_file": "kernel-1.json",
                    "generated_at": f"2026-01-0{i}T00:00:00Z",
                    "rounds": [{"runs": [{
                        "config": "cfg",
                        "finding_scores": [
                            {"file": f"src/old{i}.py", "line": 1,
                             "outcome": "unmatched"},
                            {"file": "src/persistent.py", "line": 9,
                             "outcome": "unmatched"},
                        ],
                    }]}],
                }))
            got = rl.load_suggestions("kernel-1", {}, d, window=2)

        files = {s["file"] for s in got}
        self.assertIn("src/persistent.py", files)
        # Only the two newest archives are read, so the three oldest
        # single-run locations must not appear.
        self.assertNotIn("src/old1.py", files)
        self.assertNotIn("src/old3.py", files)
        self.assertEqual(got[0]["archives_read"], 2)
        self.assertEqual(got[0]["archives_total"], 5)

    def test_index_matches_unindexed_results(self):
        """The index is a speed-up, so it must not change what is returned.

        `/api/records` calls this once per record; scanning the replay dir
        each time re-parses every archive (48 records x 153 archives, ~2s
        today, growing with every bake-off). `index_replays` hoists that into
        one pass — but only if both paths agree.
        """
        with tempfile.TemporaryDirectory() as td:
            d = Path(td)
            for i in (1, 2, 3):
                (d / f"kernel-1-2026010{i}-scored.json").write_text(json.dumps({
                    "dataset_file": "kernel-1.json",
                    "generated_at": f"2026-01-0{i}T00:00:00Z",
                    "rounds": [{"runs": [{"config": f"cfg{i}",
                                          "finding_scores": [
                        {"file": "src/x.py", "line": 10,
                         "outcome": "unmatched"}]}]}],
                }))
            (d / "kernel-9-scored.json").write_text(json.dumps({
                "dataset_file": "kernel-9.json",
                "generated_at": "2026-01-01T00:00:00Z",
                "rounds": [{"runs": [{"config": "c", "finding_scores": [
                    {"file": "z.py", "line": 1, "outcome": "unmatched"}]}]}],
            }))
            plain = rl.load_suggestions("kernel-1", {}, d)
            idx = rl.index_replays(d)
            indexed = rl.load_suggestions("kernel-1", {}, d, index=idx)

        self.assertEqual(plain, indexed)
        self.assertEqual(indexed[0]["n_runs"], 3)
        # The index must not leak another PR's archives into this one.
        self.assertEqual(len(idx["kernel-1"]), 3)
        self.assertEqual(len(idx["kernel-9"]), 1)

    def test_window_zero_reads_everything(self):
        with tempfile.TemporaryDirectory() as td:
            d = Path(td)
            for i in (1, 2, 3):
                (d / f"kernel-1-2026010{i}-scored.json").write_text(json.dumps({
                    "dataset_file": "kernel-1.json",
                    "generated_at": f"2026-01-0{i}T00:00:00Z",
                    "rounds": [{"runs": [{"config": "cfg", "finding_scores": [
                        {"file": f"src/x{i}.py", "line": 1,
                         "outcome": "unmatched"}]}]}],
                }))
            got = rl.load_suggestions("kernel-1", {}, d, window=0)
        self.assertEqual(len(got), 3)
        self.assertEqual(got[0]["archives_read"], 3)


if __name__ == "__main__":
    unittest.main()
