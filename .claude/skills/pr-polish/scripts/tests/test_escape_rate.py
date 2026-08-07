#!/usr/bin/env python3
"""Tests for escape_rate.py — pr-polish's local-review miss rate."""

from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path

SCRIPTS = Path(__file__).resolve().parents[1]
if str(SCRIPTS) not in sys.path:
    sys.path.insert(0, str(SCRIPTS))

import escape_rate as er  # noqa: E402


def _comment(**kw):
    base = {
        "source": "github-inline",
        "author": "coderabbitai[bot]",
        "is_bot": True,
        "path": "a.py",
        "line": 10,
        "body": "This leaks a file handle on the error path.",
        "created_at": "2026-07-30T12:00:00Z",
    }
    base.update(kw)
    return base


def _state(**kw):
    base = {
        "pr_number": 1,
        "completed": True,
        "exit_reason": "converged",
        "completed_at": "2026-07-30T10:00:00Z",
        "rounds": [],
    }
    base.update(kw)
    return base


def _record(comments, *, files=("a.py", "b.py"), gt=None):
    rec = {
        "harvested_rounds": [
            {"files_changed": list(files), "raw_comment_actions": comments}
        ]
    }
    if gt is not None:
        rec["ground_truth_v3"] = gt
    return rec


class JudgedTruePositivesTests(unittest.TestCase):
    def test_no_ground_truth_is_none_not_empty(self):
        # "Not collected" and "nothing real here" are different claims;
        # only the second licenses calling an escape spurious.
        self.assertIsNone(er.judged_true_positives(None))
        self.assertIsNone(er.judged_true_positives({}))

    def test_empty_true_positives_is_a_real_empty_list(self):
        self.assertEqual(
            er.judged_true_positives({"ground_truth_v3": {"true_positives": []}}),
            [],
        )

    def test_extracts_locations(self):
        got = er.judged_true_positives(
            {"ground_truth_v3": {"true_positives": [
                {"file": "a.py", "line": 10},
                {"file": "b.py", "line": None},
            ]}}
        )
        self.assertEqual(got, [("a.py", 10), ("b.py", None)])


class ScopeSourceTests(unittest.TestCase):
    def test_files_changed_comes_from_the_record_not_state(self):
        # pr-polish state never persists files_changed. Reading only it
        # would report every escape as out-of-scope, inverting the
        # depth-vs-scope signal.
        state = _state()
        self.assertEqual(er.in_scope_files(state), set())
        rec = _record([], files=("svc/a.py",))
        self.assertEqual(er.in_scope_files(state, rec), {"a.py"})


class CommentSourceTests(unittest.TestCase):
    def test_prefers_the_harvested_record_over_the_in_run_snapshot(self):
        with tempfile.TemporaryDirectory() as td:
            sd = Path(td)
            # The in-run snapshot cannot contain post-completion comments.
            (sd / "pp-comments.json").write_text(
                json.dumps({"comments": [_comment(body="stale")]})
            )
            rec = _record([_comment(body="fresh")])
            got = er.load_comments(sd, rec)
            self.assertEqual(len(got), 1)
            self.assertEqual(got[0]["body"], "fresh")

    def test_falls_back_to_snapshot_when_no_record(self):
        with tempfile.TemporaryDirectory() as td:
            sd = Path(td)
            (sd / "pp-comments.json").write_text(
                json.dumps({"comments": [_comment(body="only")]})
            )
            self.assertEqual(len(er.load_comments(sd, None)), 1)

    def test_reads_the_legacy_bare_list_snapshot(self):
        """Both shapes are live on disk; only the wrapped one used to parse.

        ``fetch-comments`` emits the wrapped object today, but 7 state dirs
        (newest 2026-07-18) still hold the legacy bare list. Since ``--all``
        had no per-dir guard, one of those aborted the whole fleet scan — which
        is why the escape rate had never been measured across the corpus.
        """
        with tempfile.TemporaryDirectory() as td:
            sd = Path(td)
            (sd / "pp-comments.json").write_text(
                json.dumps([_comment(body="legacy")])
            )
            got = er.load_comments(sd, None)
            self.assertEqual(len(got), 1)
            self.assertEqual(got[0]["body"], "legacy")

    def test_missing_snapshot_yields_no_comments(self):
        with tempfile.TemporaryDirectory() as td:
            self.assertEqual(er.load_comments(Path(td), None), [])


class ComputeEscapeRateTests(unittest.TestCase):
    def _run(self, state, record):
        with tempfile.TemporaryDirectory() as td:
            sd = Path(td) / "kernel-1"
            sd.mkdir()
            (sd / "pr-polish-state.json").write_text(json.dumps(state))
            ds = Path(td) / "dataset"
            ds.mkdir()
            (ds / "kernel-1.json").write_text(json.dumps(record))
            return er.compute_escape_rate(sd, ds)

    def test_emits_verdict_from_state(self):
        # The aggregate split can only bucket on what this function emits.
        rec = _record([_comment(created_at="2026-07-30T11:00:00Z")])
        out = self._run(
            _state(verdict={"verdict": "not_ready", "blockers": []}), rec
        )
        self.assertEqual(out["verdict"], "not_ready")

    def test_verdict_is_none_when_state_has_no_verdict(self):
        rec = _record([_comment(created_at="2026-07-30T11:00:00Z")])
        self.assertIsNone(self._run(_state(), rec)["verdict"])

    def test_comment_before_completion_is_not_an_escape(self):
        # It was available as round input, so missing it is not an escape.
        rec = _record([_comment(created_at="2026-07-30T09:00:00Z")])
        out = self._run(_state(), rec)
        self.assertIsNone(out["escape_rate"])
        self.assertEqual(out["bot_findings_substantive"], 0)

    def test_locationless_comment_is_not_counted(self):
        # A review-level summary has no location to match on, and its
        # findings already appear as inline rows.
        rec = _record([_comment(path=None, source="github-review")])
        out = self._run(_state(), rec)
        self.assertEqual(out["bot_findings_substantive"], 0)

    def test_uncaught_comment_is_an_escape(self):
        out = self._run(_state(), _record([_comment()]))
        self.assertEqual(out["escaped"], 1)
        self.assertEqual(out["escape_rate"], 1.0)
        self.assertTrue(out["escapes"][0]["in_scope"])

    def test_locally_found_comment_is_not_an_escape(self):
        state = _state(rounds=[
            {"n": 1, "codex_findings": [{"file": "a.py", "line": 10}]},
        ])
        out = self._run(state, _record([_comment()]))
        self.assertEqual(out["escaped"], 0)
        self.assertEqual(out["caught_locally"], 1)

    def test_triaged_comment_is_not_an_escape(self):
        state = _state(rounds=[
            {"n": 1, "comment_actions": [
                {"action": "wont_fix", "path": "a.py", "line": 10}]},
        ])
        self.assertEqual(self._run(state, _record([_comment()]))["escaped"], 0)

    def test_judged_subset_excludes_bot_false_positives(self):
        # The correction that matters: bots ran ~9% precision, so the raw
        # rate mostly counts noise.
        rec = _record(
            [_comment(line=10), _comment(line=50, body="Spurious claim.")],
            gt={"true_positives": [{"file": "a.py", "line": 10}]},
        )
        out = self._run(_state(), rec)
        self.assertEqual(out["escaped"], 2)
        self.assertEqual(out["escaped_judged"], 1)
        self.assertTrue(out["has_ground_truth"])

    def test_without_ground_truth_judged_is_none_not_zero(self):
        out = self._run(_state(), _record([_comment()]))
        self.assertIsNone(out["escaped_judged"])
        self.assertFalse(out["has_ground_truth"])

    def test_p1_detection(self):
        rec = _record([_comment(body="**P1 (blocking)** · races on shutdown")])
        self.assertEqual(self._run(_state(), rec)["escaped_p1"], 1)

    def test_out_of_scope_escape_flagged(self):
        rec = _record([_comment(path="other/z.py")], files=("a.py",))
        out = self._run(_state(), rec)
        self.assertFalse(out["escapes"][0]["in_scope"])
        self.assertEqual(out["escaped_in_scope"], 0)


class AggregateTests(unittest.TestCase):
    def test_judged_block_only_counts_gt_runs(self):
        rows = [
            {"bot_findings_substantive": 10, "escaped": 5, "escaped_p1": 1,
             "escaped_in_scope": 4, "escape_rate": 0.5, "exit_reason": "converged",
             "has_ground_truth": True, "escaped_judged": 1},
            {"bot_findings_substantive": 10, "escaped": 5, "escaped_p1": 0,
             "escaped_in_scope": 3, "escape_rate": 0.5, "exit_reason": "converged",
             "has_ground_truth": False, "escaped_judged": None},
        ]
        agg = er.aggregate(rows)
        self.assertEqual(agg["escape_rate"], 0.5)
        self.assertEqual(agg["judged"]["runs"], 1)
        self.assertEqual(agg["judged"]["escaped_judged"], 1)
        self.assertAlmostEqual(agg["judged"]["escape_rate_judged"], 0.1)

    def test_by_exit_reason_split(self):
        rows = [
            {"bot_findings_substantive": 4, "escaped": 1, "escaped_p1": 0,
             "escaped_in_scope": 1, "escape_rate": 0.25,
             "exit_reason": "converged", "has_ground_truth": False,
             "escaped_judged": None},
            {"bot_findings_substantive": 4, "escaped": 3, "escaped_p1": 0,
             "escaped_in_scope": 2, "escape_rate": 0.75,
             "exit_reason": "capped-at-max", "has_ground_truth": False,
             "escaped_judged": None},
        ]
        agg = er.aggregate(rows)
        self.assertEqual(agg["by_exit_reason"]["converged"]["escape_rate"], 0.25)
        self.assertEqual(
            agg["by_exit_reason"]["capped-at-max"]["escape_rate"], 0.75)

    def test_by_verdict_splits_independently_of_exit_reason(self):
        # Both runs exited `converged`; the verdict disagrees on one. Bucketing
        # on exit_reason alone collapses them and hides that the not_ready run
        # is the one leaking escapes — which is the whole point of the split.
        rows = [
            {"bot_findings_substantive": 4, "escaped": 1, "escaped_p1": 0,
             "escaped_in_scope": 1, "escape_rate": 0.25,
             "exit_reason": "converged", "verdict": "ready",
             "has_ground_truth": False, "escaped_judged": None},
            {"bot_findings_substantive": 4, "escaped": 3, "escaped_p1": 0,
             "escaped_in_scope": 2, "escape_rate": 0.75,
             "exit_reason": "converged", "verdict": "not_ready",
             "has_ground_truth": False, "escaped_judged": None},
        ]
        agg = er.aggregate(rows)
        self.assertEqual(agg["by_exit_reason"]["converged"]["runs"], 2)
        self.assertEqual(agg["by_verdict"]["ready"]["escape_rate"], 0.25)
        self.assertEqual(agg["by_verdict"]["not_ready"]["escape_rate"], 0.75)

    def test_runs_without_a_verdict_bucket_as_unknown(self):
        # Runs predating the verdict work carry no `verdict` key; they must
        # still be counted, not dropped from the split.
        rows = [
            {"bot_findings_substantive": 4, "escaped": 1, "escaped_p1": 0,
             "escaped_in_scope": 1, "escape_rate": 0.25,
             "exit_reason": "converged", "has_ground_truth": False,
             "escaped_judged": None},
        ]
        agg = er.aggregate(rows)
        self.assertEqual(agg["by_verdict"]["unknown"]["runs"], 1)


class BackendRosterTests(unittest.TestCase):
    """``local_findings`` must read every backend pr-polish writes.

    ``pr_ops._persist_round_findings`` writes ``f"{backend}_findings"`` for
    each entry in ``bramble_ops.BACKENDS``. A backend this reader misses is
    dropped silently — its findings never enter ``local_findings``, so every
    external comment it caught is scored as an *escape*. The failure inflates
    the metric on exactly the runs that used the missing backend, and nothing
    in the output contradicts it. These tests fail if the roster is ever
    restated by hand and drifts.
    """

    def test_reads_every_backend_in_the_roster(self):
        import bramble_ops

        rounds = [{
            "n": 1,
            **{f"{b}_findings": [{"file": f"{b}.py", "line": 1}]
               for b in bramble_ops.BACKENDS},
        }]
        found = {f["file"] for f in er.local_findings(_state(rounds=rounds))}
        self.assertEqual(
            found, {f"{b}.py" for b in bramble_ops.BACKENDS},
            "local_findings dropped a backend that pr_ops writes; its "
            "findings would be miscounted as escapes",
        )

    def test_claude_findings_are_counted(self):
        # The concrete regression: claude joined the roster after this
        # reader was written, so its findings were invisible here.
        state = _state(rounds=[
            {"n": 1, "claude_findings": [{"file": "a.py", "line": 10}]},
        ])
        out = self._run_escape(state)
        self.assertEqual(out["escaped"], 0)
        self.assertEqual(out["caught_locally"], 1)

    def _run_escape(self, state):
        with tempfile.TemporaryDirectory() as td:
            sd = Path(td) / "kernel-1"
            sd.mkdir()
            (sd / "pr-polish-state.json").write_text(json.dumps(state))
            ds = Path(td) / "dataset"
            ds.mkdir()
            (ds / "kernel-1.json").write_text(
                json.dumps(_record([_comment()])))
            return er.compute_escape_rate(sd, ds)


if __name__ == "__main__":
    unittest.main()
