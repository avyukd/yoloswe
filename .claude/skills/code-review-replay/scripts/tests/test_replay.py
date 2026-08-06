"""Unit tests for the code-review replay scorer (replay_lib + replay)."""

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

import collect_lib as cl  # noqa: E402
import harvest_lib as hl  # noqa: E402
import replay  # noqa: E402
import replay_lib as rl  # noqa: E402
import unmatched_lib as ul  # noqa: E402


# A realistic klogfmt run-log fragment (cursor backend logs every tool call).
SAMPLE_RUNLOG = """\
I0507 01:11:19.931402  228289 codereview.go:134] code-review run start run_tag=code-review-replay:kernel-3834:r1:cursor-composer2 pid=228289 cwd=/x backend=cursor model=composer-2 timeout=10m0s
I0507 01:11:21.446877  228289 backend.go:180] reviewer session started run_tag=code-review-replay:kernel-3834:r1:cursor-composer2 session_id=abc model="Composer 2 Fast"
D0507 01:11:40.329692  228289 backend.go:193] tool call start run_tag=code-review-replay:kernel-3834:r1:cursor-composer2 tool="read .../scripts/deploy.py" call_id=tool_001 input_summary=path=x
D0507 01:11:40.427429  228289 backend.go:209] tool call end run_tag=code-review-replay:kernel-3834:r1:cursor-composer2 tool=readToolCall call_id=tool_001 is_error=false result_len=0
D0507 01:11:41.000000  228289 backend.go:193] tool call start run_tag=code-review-replay:kernel-3834:r1:cursor-composer2 tool="grep deploy_rollout" call_id=tool_002 input_summary=pattern=x
D0507 01:11:41.500000  228289 backend.go:209] tool call end run_tag=code-review-replay:kernel-3834:r1:cursor-composer2 tool=grepToolCall call_id=tool_002 is_error=true result_len=0
I0507 01:12:45.717080  228289 backend.go:218] reviewer turn complete run_tag=code-review-replay:kernel-3834:r1:cursor-composer2 success=true duration_ms=84271
I0507 01:12:45.717234  228289 codereview.go:198] code-review run exit run_tag=code-review-replay:kernel-3834:r1:cursor-composer2 status=ok verdict=rejected issue_count=1 max_severity=high total_duration_ms=85785
"""


class ParseRunlogTests(unittest.TestCase):
    def test_parses_metadata_and_calls(self):
        tr = rl.parse_runlog(SAMPLE_RUNLOG)
        self.assertTrue(tr.parsed)
        self.assertEqual(tr.backend, "cursor")
        self.assertEqual(tr.model, "composer-2")
        self.assertTrue(tr.session_started)
        self.assertEqual(tr.total_duration_ms, 85785)
        self.assertEqual(tr.n_tool_calls, 2)
        self.assertEqual(tr.tool_kind_counts, {"read": 1, "grep": 1})

    def test_tool_error_and_durations(self):
        tr = rl.parse_runlog(SAMPLE_RUNLOG)
        self.assertEqual(tr.n_tool_errors, 1)
        read_call = tr.tool_calls[0]
        self.assertEqual(read_call.kind, "read")
        self.assertEqual(read_call.target, "scripts/deploy.py")
        self.assertFalse(read_call.is_error)
        # 01:11:40.427 - 01:11:40.329 = 98ms
        self.assertEqual(read_call.duration_ms, 98)
        grep_call = tr.tool_calls[1]
        self.assertTrue(grep_call.is_error)

    def test_first_tool_latency(self):
        tr = rl.parse_runlog(SAMPLE_RUNLOG)
        # session start 01:11:21.446 -> first tool 01:11:40.329 ≈ 18883ms
        self.assertIsNotNone(tr.first_tool_latency_ms)
        self.assertAlmostEqual(tr.first_tool_latency_ms, 18883, delta=2)

    def test_empty_log_not_parsed(self):
        tr = rl.parse_runlog("")
        self.assertFalse(tr.parsed)
        self.assertEqual(tr.n_tool_calls, 0)

    def test_garbage_lines_ignored(self):
        tr = rl.parse_runlog("not a klog line\nanother\n" + SAMPLE_RUNLOG)
        self.assertTrue(tr.parsed)
        self.assertEqual(tr.n_tool_calls, 2)


def _codex_line(direction: str, ts: str, message: dict) -> str:
    return json.dumps(
        {"timestamp": ts, "direction": direction, "message": message}
    )


# A codex protocol JSONL fragment: header, turn lifecycle, and two
# commandExecution items (one read, one failed search).
SAMPLE_CODEX_PROTOCOL = "\n".join(
    [
        json.dumps({"format": "codex", "version": "1.0"}),
        _codex_line(
            "received",
            "2026-05-21T05:24:42.944Z",
            {"method": "turn/started", "params": {}},
        ),
        _codex_line(
            "received",
            "2026-05-21T05:24:47.150Z",
            {
                "method": "item/started",
                "params": {
                    "item": {
                        "id": "call_A",
                        "type": "commandExecution",
                        "command": '/bin/bash -lc "sed -n 1,80p deploy.py"',
                        "commandActions": [
                            {
                                "type": "read",
                                "name": "deploy.py",
                                "path": "/x/scripts/deploy.py",
                            }
                        ],
                    }
                },
            },
        ),
        _codex_line(
            "received",
            "2026-05-21T05:24:47.350Z",
            {
                "method": "item/completed",
                "params": {
                    "item": {
                        "id": "call_A",
                        "type": "commandExecution",
                        "durationMs": 200,
                        "exitCode": 0,
                        "status": "completed",
                    }
                },
            },
        ),
        _codex_line(
            "received",
            "2026-05-21T05:24:48.000Z",
            {
                "method": "item/started",
                "params": {
                    "item": {
                        "id": "call_B",
                        "type": "commandExecution",
                        "command": '/bin/bash -lc "rg missing_pattern"',
                        "commandActions": [{"type": "search"}],
                    }
                },
            },
        ),
        _codex_line(
            "received",
            "2026-05-21T05:24:48.500Z",
            {
                "method": "item/completed",
                "params": {
                    "item": {
                        "id": "call_B",
                        "type": "commandExecution",
                        "exitCode": 1,
                        "status": "failed",
                    }
                },
            },
        ),
        _codex_line(
            "received",
            "2026-05-21T05:27:53.424Z",
            {"method": "turn/completed", "params": {}},
        ),
    ]
)


class ParseCodexProtocolTests(unittest.TestCase):
    def test_parses_tool_calls(self):
        tr = rl.parse_codex_protocol(SAMPLE_CODEX_PROTOCOL)
        self.assertTrue(tr.parsed)
        self.assertEqual(tr.backend, "codex")
        self.assertTrue(tr.session_started)
        self.assertEqual(tr.n_tool_calls, 2)
        self.assertEqual(tr.tool_kind_counts, {"read": 1, "grep": 1})

    def test_read_target_and_duration(self):
        tr = rl.parse_codex_protocol(SAMPLE_CODEX_PROTOCOL)
        read_call = tr.tool_calls[0]
        self.assertEqual(read_call.kind, "read")
        self.assertEqual(read_call.target, "/x/scripts/deploy.py")
        self.assertEqual(read_call.duration_ms, 200)
        self.assertFalse(read_call.is_error)

    def test_failed_command_is_error(self):
        tr = rl.parse_codex_protocol(SAMPLE_CODEX_PROTOCOL)
        self.assertEqual(tr.n_tool_errors, 1)
        self.assertTrue(tr.tool_calls[1].is_error)

    def test_turn_duration_and_latency(self):
        tr = rl.parse_codex_protocol(SAMPLE_CODEX_PROTOCOL)
        # turn 05:24:42.944 -> 05:27:53.424 ≈ 190480ms
        self.assertAlmostEqual(tr.total_duration_ms, 190480, delta=2)
        # first tool 05:24:47.150 - turn start 05:24:42.944 ≈ 4206ms
        self.assertAlmostEqual(tr.first_tool_latency_ms, 4206, delta=2)

    def test_files_coverage_from_codex_trace(self):
        tr = rl.parse_codex_protocol(SAMPLE_CODEX_PROTOCOL)
        rl.annotate_files_coverage(
            tr, ["scripts/deploy.py", "scripts/other.py"]
        )
        # files_read is the full distinct read set, not a files_changed subset.
        self.assertEqual(tr.files_read, ["/x/scripts/deploy.py"])
        # scripts/other.py was never read; scripts/deploy.py matches by basename.
        self.assertEqual(tr.files_changed_not_read, ["scripts/other.py"])

    def test_empty_protocol_not_parsed(self):
        tr = rl.parse_codex_protocol("")
        self.assertFalse(tr.parsed)
        self.assertEqual(tr.n_tool_calls, 0)

    def test_garbage_lines_ignored(self):
        tr = rl.parse_codex_protocol(
            "not json\n{bad}\n" + SAMPLE_CODEX_PROTOCOL
        )
        self.assertTrue(tr.parsed)
        self.assertEqual(tr.n_tool_calls, 2)

    def test_header_alone_marks_parsed(self):
        tr = rl.parse_codex_protocol(
            json.dumps({"format": "codex", "version": "1.0"})
        )
        self.assertTrue(tr.parsed)
        self.assertEqual(tr.n_tool_calls, 0)


class IsoMsTests(unittest.TestCase):
    def test_z_suffix(self):
        self.assertIsNotNone(rl._iso_ms("2026-05-21T05:24:42.944Z"))

    def test_nanosecond_fraction_truncated(self):
        # codex emits 9-digit fractions; fromisoformat only takes 6.
        self.assertIsNotNone(rl._iso_ms("2026-05-21T05:24:42.944245137Z"))

    def test_offset_form(self):
        self.assertIsNotNone(rl._iso_ms("2026-05-21T05:24:42.944+00:00"))

    def test_none_and_garbage(self):
        self.assertIsNone(rl._iso_ms(None))
        self.assertIsNone(rl._iso_ms("not a timestamp"))


class FilesCoverageTests(unittest.TestCase):
    def test_split_read_vs_not_read(self):
        tr = rl.parse_runlog(SAMPLE_RUNLOG)
        rl.annotate_files_coverage(
            tr, ["scripts/deploy.py", "tests/scripts/test_deploy_rollout.py"]
        )
        self.assertEqual(tr.files_read, ["scripts/deploy.py"])
        self.assertEqual(
            tr.files_changed_not_read, ["tests/scripts/test_deploy_rollout.py"]
        )

    def test_note_when_no_tool_calls(self):
        tr = rl.parse_runlog(
            "I0507 01:11:19.931402  1 codereview.go:134] "
            "code-review run start run_tag=x backend=codex model=gpt\n"
        )
        rl.annotate_files_coverage(tr, ["a.py"])
        self.assertTrue(any("no tool-call records" in n for n in tr.notes))

    def test_note_when_unparsed(self):
        tr = rl.parse_runlog("")
        rl.annotate_files_coverage(tr, ["a.py"])
        self.assertTrue(
            any("no usable execution log" in n for n in tr.notes)
        )

    def test_files_read_is_full_set_not_diff_subset(self):
        # A reviewer that read files OUTSIDE the diff must have them all in
        # files_read — the earlier bug intersected files_read with
        # files_changed, hiding the reviewer's true investigation breadth.
        proto = "\n".join(
            [
                json.dumps({"format": "codex", "version": "1.0"}),
                _codex_line(
                    "received",
                    "2026-05-21T05:24:42.944Z",
                    {"method": "turn/started", "params": {}},
                ),
                _codex_line(
                    "received",
                    "2026-05-21T05:24:43.000Z",
                    {
                        "method": "item/started",
                        "params": {
                            "item": {
                                "id": "c1",
                                "type": "commandExecution",
                                "command": "sed -n 1,80p Dockerfile",
                                "commandActions": [
                                    {
                                        "type": "read",
                                        "path": "/tmp/replay-kernel-4024-r1-xx/Dockerfile",
                                    }
                                ],
                            }
                        },
                    },
                ),
                _codex_line(
                    "received",
                    "2026-05-21T05:24:43.500Z",
                    {
                        "method": "item/started",
                        "params": {
                            "item": {
                                "id": "c2",
                                "type": "commandExecution",
                                "command": "sed -n 1,80p docs/testing.md",
                                "commandActions": [
                                    {
                                        "type": "read",
                                        "path": "/tmp/replay-kernel-4024-r1-xx/docs/testing.md",
                                    }
                                ],
                            }
                        },
                    },
                ),
            ]
        )
        tr = rl.parse_codex_protocol(proto)
        rl.annotate_files_coverage(tr, ["Dockerfile", "nitro.config.ts"])
        # Both reads surface — including docs/testing.md, NOT in the diff.
        self.assertEqual(
            tr.files_read, ["Dockerfile", "docs/testing.md"]
        )
        # The replay-checkout prefix is stripped to repo-relative paths.
        self.assertNotIn(
            "/tmp/replay", "".join(tr.files_read)
        )
        # Coverage diagnostic still flags the unread changed file.
        self.assertEqual(tr.files_changed_not_read, ["nitro.config.ts"])

    def test_files_read_dedups(self):
        # Same file read twice -> one entry in files_read.
        proto = "\n".join(
            [
                json.dumps({"format": "codex", "version": "1.0"}),
                _codex_line(
                    "received",
                    "2026-05-21T05:24:42.944Z",
                    {"method": "turn/started", "params": {}},
                ),
            ]
            + [
                _codex_line(
                    "received",
                    f"2026-05-21T05:24:4{i}.000Z",
                    {
                        "method": "item/started",
                        "params": {
                            "item": {
                                "id": f"c{i}",
                                "type": "commandExecution",
                                "command": "sed -n p a.py",
                                "commandActions": [
                                    {"type": "read", "path": "/x/a.py"}
                                ],
                            }
                        },
                    },
                )
                for i in (3, 4)
            ]
        )
        tr = rl.parse_codex_protocol(proto)
        rl.annotate_files_coverage(tr, [])
        self.assertEqual(tr.files_read, ["/x/a.py"])


class StripReplayCwdTests(unittest.TestCase):
    def test_strips_replay_checkout_prefix(self):
        self.assertEqual(
            rl._strip_replay_cwd("/tmp/replay-kernel-4024-r1-abc/services/x.ts"),
            "services/x.ts",
        )

    def test_leaves_non_replay_paths(self):
        self.assertEqual(
            rl._strip_replay_cwd("/home/ubuntu/.claude/skills/review/SKILL.md"),
            "/home/ubuntu/.claude/skills/review/SKILL.md",
        )
        self.assertEqual(rl._strip_replay_cwd("scripts/deploy.py"), "scripts/deploy.py")


class CodexActionKindTests(unittest.TestCase):
    def _one_item_proto(self, command_actions: list[dict]) -> str:
        return "\n".join(
            [
                json.dumps({"format": "codex", "version": "1.0"}),
                _codex_line(
                    "received",
                    "2026-05-21T05:24:42.944Z",
                    {"method": "turn/started", "params": {}},
                ),
                _codex_line(
                    "received",
                    "2026-05-21T05:24:43.000Z",
                    {
                        "method": "item/started",
                        "params": {
                            "item": {
                                "id": "c1",
                                "type": "commandExecution",
                                "command": "cmd",
                                "commandActions": command_actions,
                            }
                        },
                    },
                ),
            ]
        )

    def test_listfiles_maps_to_glob(self):
        # The codex protocol emits `listFiles` (not `list`) for directory
        # enumeration; it must map to the glob kind, not fall through to shell.
        tr = rl.parse_codex_protocol(
            self._one_item_proto([{"type": "listFiles", "name": "libs/"}])
        )
        self.assertEqual(tr.tool_calls[0].kind, "glob")

    def test_read_action_preferred_over_unknown(self):
        # An item with an `unknown` action listed before a `read` action must
        # still be classified `read` so the file target is recovered.
        tr = rl.parse_codex_protocol(
            self._one_item_proto(
                [
                    {"type": "unknown", "path": None},
                    {"type": "read", "path": "/x/found.py"},
                ]
            )
        )
        self.assertEqual(tr.tool_calls[0].kind, "read")
        self.assertEqual(tr.tool_calls[0].target, "/x/found.py")

    def test_unknown_only_stays_shell(self):
        tr = rl.parse_codex_protocol(
            self._one_item_proto([{"type": "unknown", "path": None}])
        )
        self.assertEqual(tr.tool_calls[0].kind, "shell")
        self.assertIsNone(tr.tool_calls[0].target)


class CollectExecutionTraceTests(unittest.TestCase):
    """The round dir is shared by both configs; a codex protocol JSONL must
    only be attributed to the codex run, never the cursor sibling."""

    def _round_dir(self, d: Path) -> Path:
        # A codex protocol JSONL lands in the shared round dir.
        (d / "reviewer-session-20260521-052441.jsonl").write_text(
            SAMPLE_CODEX_PROTOCOL
        )
        return d

    def test_cursor_run_ignores_codex_protocol(self):
        with tempfile.TemporaryDirectory() as d:
            rlog = self._round_dir(Path(d))
            tr = replay.collect_execution_trace(
                run_tag="code-review-replay:x-1:r1:cursor-composer2",
                started_at=0.0,
                round_log=rlog,
                config_name="cursor-composer2",
                backend="cursor",
                files_changed=["a.py"],
            )
            # No klogfmt log found + protocol ignored => empty cursor trace,
            # NOT the codex sibling's 2 tool calls.
            self.assertEqual(tr.n_tool_calls, 0)
            self.assertIsNone(tr.protocol_log_path)

    def test_codex_run_uses_protocol(self):
        with tempfile.TemporaryDirectory() as d:
            rlog = self._round_dir(Path(d))
            tr = replay.collect_execution_trace(
                run_tag="code-review-replay:x-1:r1:codex-5.4-mini",
                started_at=0.0,
                round_log=rlog,
                config_name="codex-5.4-mini",
                backend="codex",
                files_changed=["a.py"],
            )
            self.assertEqual(tr.n_tool_calls, 2)
            self.assertIsNotNone(tr.protocol_log_path)


class FindRunlogByTagTests(unittest.TestCase):
    def test_matches_tag_in_recent_log(self):
        with tempfile.TemporaryDirectory() as d:
            log_dir = Path(d)
            p = log_dir / "code-review-20260507-011119-228289.log"
            p.write_text(SAMPLE_RUNLOG)
            found = rl.find_runlog_by_tag(
                log_dir,
                "code-review-replay:kernel-3834:r1:cursor-composer2",
            )
            self.assertEqual(found, p)

    def test_no_match_returns_none(self):
        with tempfile.TemporaryDirectory() as d:
            log_dir = Path(d)
            (log_dir / "code-review-x.log").write_text(SAMPLE_RUNLOG)
            self.assertIsNone(
                rl.find_runlog_by_tag(log_dir, "nonexistent-tag")
            )

    def test_missing_dir_returns_none(self):
        self.assertIsNone(
            rl.find_runlog_by_tag(Path("/no/such/dir"), "tag")
        )


class GoalDivergenceTests(unittest.TestCase):
    def test_identical_after_whitespace_does_not_diverge(self):
        self.assertFalse(
            rl._materially_diverges("fix the bug", "fix   the\nbug")
        )

    def test_unrelated_goals_diverge(self):
        self.assertTrue(
            rl._materially_diverges(
                "refactor the authentication middleware layer",
                "update documentation for the deployment pipeline",
            )
        )

    def test_dataset_goal_source_skips_reconstruction(self):
        dataset_round = {"round": 1, "goal_text": "the recorded goal text"}
        # prefer=dataset must not touch the repo or gh — pass a bogus path.
        result = rl.build_goal(
            dataset_round,
            repo_path=Path("/no/such/repo"),
            pr_number="1",
            state=None,
            bramble_ops_path=Path("/no/such/bramble_ops.py"),
            prefer="dataset",
        )
        self.assertEqual(result.text, "the recorded goal text")
        self.assertEqual(result.source, "dataset_fallback")
        self.assertFalse(result.goal_divergence)


class SelectDatasetRoundsTests(unittest.TestCase):
    def _dataset(self) -> dict:
        return {
            "harvested_rounds": [
                {"round": 1, "signal_tier": "r1"},
                {"round": 2, "signal_tier": "final"},
            ]
        }

    def test_no_filter_returns_all(self):
        rounds = replay.select_dataset_rounds(self._dataset(), None)
        self.assertEqual(len(rounds), 2)

    def test_r1_filter(self):
        rounds = replay.select_dataset_rounds(self._dataset(), "r1")
        self.assertEqual([r["round"] for r in rounds], [1])

    def test_final_filter_includes_incomplete(self):
        ds = {
            "harvested_rounds": [
                {"round": 1, "signal_tier": "r1"},
                {"round": 3, "signal_tier": "final_incomplete"},
            ]
        }
        rounds = replay.select_dataset_rounds(ds, "final")
        self.assertEqual([r["round"] for r in rounds], [3])


class UnmatchedRecurrenceTests(unittest.TestCase):
    """Cross-run recurrence triages the metric-invisible `unmatched` bucket.

    Regression intent: precision ignores unmatched findings entirely, so a
    recall-first variant's extra output is invisible to the headline
    numbers — "found a real bug the census missed" and "generated plausible
    noise" score identically. Recurrence across independent runs (and
    especially across configs) is the cheap mechanical discriminator.
    """

    GT = {
        "true_positives": [{"file": "a.py", "line": 10}],
        "false_positives": [{"file": "b.py", "line": 5}],
    }

    def test_judged_locations_excluded(self):
        obs = [("r1","c1","a.py",10), ("r2","c1","b.py",5), ("r3","c1","z.py",1)]
        got = ul.collect_unmatched(obs, self.GT)
        self.assertEqual([(g.file,g.line) for g in got], [("z.py",1)])

    def test_line_slack_merges_near_misses(self):
        # Same defect drifting by 2 lines must count as recurrence, not
        # two singletons -- the GT tolerates the same drift.
        obs = [("r1","c1","z.py",100), ("r2","c2","z.py",102)]
        got = ul.collect_unmatched(obs, self.GT)
        self.assertEqual(len(got), 1)
        self.assertTrue(got[0].recurrent)
        self.assertTrue(got[0].cross_config)

    def test_same_run_twice_is_not_recurrence(self):
        obs = [("r1","c1","z.py",1), ("r1","c1","z.py",1)]
        got = ul.collect_unmatched(obs, self.GT)
        self.assertFalse(got[0].recurrent, "one run cannot corroborate itself")

    def test_summary_counts(self):
        obs = [("r1","c1","z.py",1), ("r2","c2","z.py",1), ("r1","c1","q.py",7)]
        s = ul.summarize(ul.collect_unmatched(obs, self.GT))
        self.assertEqual(s["unmatched_distinct"], 2)
        self.assertEqual(s["unmatched_recurrent"], 1)
        self.assertEqual(s["unmatched_cross_config"], 1)
        self.assertEqual(s["unmatched_singleton"], 1)

    def test_cross_config_requires_two_configs(self):
        obs = [("r1","c1","z.py",1), ("r2","c1","z.py",1)]
        got = ul.collect_unmatched(obs, self.GT)
        self.assertTrue(got[0].recurrent)
        self.assertFalse(got[0].cross_config)

    def test_missing_file_ignored(self):
        self.assertEqual(ul.collect_unmatched([("r","c",None,1)], self.GT), [])


class ExpandFindingSitesTests(unittest.TestCase):
    """Class-level findings must be scored at every site they report.

    Regression intent: the reviewer prompt instructs collapsing N sibling
    violations into ONE issue with a ``sites`` array, top-level file/line
    naming one representative. Scoring only the representative credited a
    reviewer with 1 of N *for obeying the contract*. Measured on
    kernel-8276: 46 of 73 issues carried sites and 90 defect locations were
    discarded, including a frozen true positive scored as missed.
    """

    def test_sites_expand_to_one_finding_each(self):
        out = rl.expand_finding_sites([
            {
                "file": "a.py", "line": 10, "severity": "high",
                "invariant": "stale marker",
                "sites": [
                    {"file": "a.py", "line": 10},
                    {"file": "b.py", "line": 42},
                    {"file": "c.py", "line": 7},
                ],
            }
        ])
        self.assertEqual(len(out), 3)
        self.assertEqual(
            {(f["file"], f["line"]) for f in out},
            {("a.py", 10), ("b.py", 42), ("c.py", 7)},
        )
        # Parent metadata rides along to each site.
        self.assertTrue(all(f["severity"] == "high" for f in out))

    def test_finding_without_sites_passes_through(self):
        out = rl.expand_finding_sites([{"file": "a.py", "line": 1}])
        self.assertEqual(out, [{"file": "a.py", "line": 1}])

    def test_non_dict_entries_dropped(self):
        out = rl.expand_finding_sites(["junk", None, {"file": "a.py"}])
        self.assertEqual(out, [{"file": "a.py"}])

    def test_malformed_sites_ignored(self):
        # A sites array of strings must not crash or silently blank the
        # finding's own location.
        out = rl.expand_finding_sites([
            {"file": "a.py", "line": 3, "sites": ["nope", 7]}
        ])
        self.assertEqual(out, [{"file": "a.py", "line": 3, "sites": ["nope", 7]}])

    def test_expanded_sites_reach_the_ground_truth(self):
        # End-to-end: a class-level finding whose *non-representative*
        # site is the real defect must now score as a TP.
        gt = {
            "true_positives": [
                {"file": "b.py", "line": 42, "severity": "high",
                 "topic": "the real one"},
            ],
            "false_positives": [],
        }
        issues = [{
            "file": "a.py", "line": 10, "severity": "high",
            "invariant": "shared rule",
            "sites": [{"file": "a.py", "line": 10},
                      {"file": "b.py", "line": 42}],
        }]
        scored = rl.score_against_frozen_gt(
            backend="codex", model="m", config="c",
            envelope_status="ok", verdict="rejected", duration_ms=1,
            replay_findings=rl.expand_finding_sites(issues),
            ground_truth=gt,
        )
        self.assertEqual(scored.matched_tp, 1)
        self.assertEqual(scored.recall, 1.0)

    def test_sites_collapsing_on_one_gt_entry_does_not_inflate_recall(self):
        # Recall counts DISTINCT ground-truth entries, so three sites that
        # all land on one defect must not read as three catches.
        gt = {
            "true_positives": [
                {"file": "a.py", "line": 10, "severity": "high", "topic": "x"},
                {"file": "z.py", "line": 1, "severity": "high", "topic": "y"},
            ],
            "false_positives": [],
        }
        issues = [{
            "file": "a.py", "line": 10, "severity": "high",
            "sites": [{"file": "a.py", "line": 10},
                      {"file": "a.py", "line": 11},
                      {"file": "a.py", "line": 12}],
        }]
        scored = rl.score_against_frozen_gt(
            backend="codex", model="m", config="c",
            envelope_status="ok", verdict="rejected", duration_ms=1,
            replay_findings=rl.expand_finding_sites(issues),
            ground_truth=gt,
        )
        self.assertEqual(scored.recall, 0.5, "1 of 2 distinct GT defects")


class ScoreAgainstFrozenGtTests(unittest.TestCase):
    """Replay mode's mechanical scoring against a frozen ground_truth_v3."""

    def _gt(self) -> dict:
        return {
            "true_positives": [
                {"file": "a.py", "line": 10, "severity": "high",
                 "topic": "off-by-one"},
                {"file": "c.py", "line": 99, "severity": "high",
                 "topic": "uncaught real bug"},
            ],
            "false_positives": [
                {"file": "b.py", "line": 5, "severity": "low",
                 "topic": "noise"},
            ],
        }

    def test_matched_tp_fp_unmatched(self):
        findings = [
            {"file": "a.py", "line": 11, "severity": "high"},  # TP (±3)
            {"file": "b.py", "line": 5, "severity": "low"},    # FP
            {"file": "d.py", "line": 1, "severity": "medium"}, # unmatched
        ]
        s = rl.score_against_frozen_gt(
            backend="codex", model="gpt-5.4-mini", config="codex-5.4-mini",
            envelope_status="ok", verdict="rejected", duration_ms=1000,
            replay_findings=findings, ground_truth=self._gt(),
        )
        self.assertEqual((s.matched_tp, s.matched_fp, s.unmatched), (1, 1, 1))
        self.assertEqual(s.precision, 0.5)
        self.assertEqual(s.recall, 0.5)  # 1 of 2 GT true_positives
        self.assertEqual(s.f1, 0.5)
        self.assertEqual(s.missed_tp, 1)
        self.assertEqual(
            [m["file"] for m in s.missed_true_positives], ["c.py"]
        )

    def test_perfect_recall_and_precision(self):
        findings = [
            {"file": "a.py", "line": 10, "severity": "high"},
            {"file": "c.py", "line": 99, "severity": "high"},
        ]
        s = rl.score_against_frozen_gt(
            backend="cursor", model="composer-2", config="cursor-composer2",
            envelope_status="ok", verdict="rejected", duration_ms=1,
            replay_findings=findings, ground_truth=self._gt(),
        )
        self.assertEqual(s.precision, 1.0)
        self.assertEqual(s.recall, 1.0)
        self.assertEqual(s.f1, 1.0)
        self.assertEqual(s.missed_tp, 0)

    def test_no_findings_zero_recall_none_precision(self):
        s = rl.score_against_frozen_gt(
            backend="codex", model="m", config="c",
            envelope_status="ok", verdict="accepted", duration_ms=1,
            replay_findings=[], ground_truth=self._gt(),
        )
        # No findings -> precision undefined (no TP/FP to rule on),
        # recall 0 (caught none of 2 real bugs).
        self.assertIsNone(s.precision)
        self.assertEqual(s.recall, 0.0)
        self.assertEqual(s.missed_tp, 2)

    def test_two_findings_near_one_gt_entry(self):
        gt = {
            "true_positives": [
                {"file": "a.py", "line": 10, "severity": "high",
                 "topic": "bug"},
            ],
            "false_positives": [],
        }
        findings = [
            {"file": "a.py", "line": 10, "severity": "high"},
            {"file": "a.py", "line": 12, "severity": "high"},  # within ±3
        ]
        s = rl.score_against_frozen_gt(
            backend="codex", model="m", config="c",
            envelope_status="ok", verdict="rejected", duration_ms=1,
            replay_findings=findings, ground_truth=gt,
        )
        # Both findings match the single GT entry; recall caps at 1.0.
        self.assertEqual(s.matched_tp, 2)
        self.assertEqual(s.recall, 1.0)
        self.assertEqual(s.missed_tp, 0)


class SeverityScoringTests(unittest.TestCase):
    """Replay records severity accuracy against the judge-set GT severity.

    The GT entry's `severity` is the judge's canonical verdict; a finding
    that matched location but reported a different severity is a mismatch —
    a separate signal that does NOT move precision/recall/F1.
    """

    def _gt(self):
        return {
            "true_positives": [
                {"file": "a.py", "line": 10, "severity": "high",
                 "topic": "real bug"},
                {"file": "b.py", "line": 20, "severity": "low",
                 "topic": "minor"},
            ],
            "false_positives": [],
        }

    def test_severity_match_and_mismatch_counted(self):
        findings = [
            {"file": "a.py", "line": 10, "severity": "high"},   # match
            {"file": "b.py", "line": 20, "severity": "high"},   # GT says low
        ]
        s = rl.score_against_frozen_gt(
            backend="codex", model="m", config="c",
            envelope_status="ok", verdict="rejected", duration_ms=1,
            replay_findings=findings, ground_truth=self._gt(),
        )
        self.assertEqual(s.matched_tp, 2)
        self.assertEqual(s.severity_mismatches, 1)
        # P/R/F1 unaffected — a severity miss is not a match miss.
        self.assertEqual(s.precision, 1.0)
        self.assertEqual(s.recall, 1.0)
        rows = {r["file"]: r for r in s.finding_scores}
        self.assertTrue(rows["a.py"]["severity_match"])
        self.assertEqual(rows["a.py"]["gt_severity"], "high")
        self.assertFalse(rows["b.py"]["severity_match"])
        self.assertEqual(rows["b.py"]["gt_severity"], "low")
        self.assertEqual(rows["b.py"]["finding_severity"], "high")

    def test_all_severities_correct_zero_mismatches(self):
        findings = [
            {"file": "a.py", "line": 10, "severity": "high"},
            {"file": "b.py", "line": 20, "severity": "low"},
        ]
        s = rl.score_against_frozen_gt(
            backend="codex", model="m", config="c",
            envelope_status="ok", verdict="rejected", duration_ms=1,
            replay_findings=findings, ground_truth=self._gt(),
        )
        self.assertEqual(s.severity_mismatches, 0)

    def test_missing_finding_severity_is_mismatch(self):
        findings = [{"file": "a.py", "line": 10}]  # no severity
        s = rl.score_against_frozen_gt(
            backend="codex", model="m", config="c",
            envelope_status="ok", verdict="rejected", duration_ms=1,
            replay_findings=findings, ground_truth=self._gt(),
        )
        self.assertEqual(s.matched_tp, 1)
        self.assertEqual(s.severity_mismatches, 1)


class SelectReplayTargetsTests(unittest.TestCase):
    """Replay's no-target default: sample GT-collected PRs from the index."""

    def _index(self, d: Path) -> Path:
        (d / "index.json").write_text(json.dumps({
            "schema_version": 2,
            "prs": [
                {"file": "kernel-1.json", "ground_truth_collected": True},
                {"file": "kernel-2.json", "ground_truth_collected": True},
                {"file": "kernel-3.json", "ground_truth_collected": False},
            ],
        }))
        return d

    def test_samples_only_gt_collected_prs(self):
        with tempfile.TemporaryDirectory() as d:
            ds = self._index(Path(d))
            picked = replay.select_replay_targets(dataset_dir=ds, sample=5)
            # kernel-3 has no GT -> excluded; only 1 and 2 are eligible.
            self.assertEqual(sorted(picked), ["kernel-1", "kernel-2"])

    def test_sample_caps_the_count(self):
        with tempfile.TemporaryDirectory() as d:
            ds = self._index(Path(d))
            picked = replay.select_replay_targets(dataset_dir=ds, sample=1)
            self.assertEqual(len(picked), 1)
            self.assertIn(picked[0], ("kernel-1", "kernel-2"))

    def test_no_gt_collected_raises(self):
        with tempfile.TemporaryDirectory() as d:
            (Path(d) / "index.json").write_text(json.dumps({
                "schema_version": 2,
                "prs": [{"file": "kernel-3.json",
                         "ground_truth_collected": False}],
            }))
            with self.assertRaises(SystemExit):
                replay.select_replay_targets(
                    dataset_dir=Path(d), sample=5)

    def test_legacy_entries_without_harvest_source_count_as_pr_polish(self):
        # Pre-schema-3 records predate the GitHub source entirely, so a
        # missing harvest_source must not silently drop them from scoring.
        with tempfile.TemporaryDirectory() as d:
            ds = self._index(Path(d))
            picked = replay.select_replay_targets(dataset_dir=ds, sample=5)
            self.assertEqual(sorted(picked), ["kernel-1", "kernel-2"])


class StalledRunDetectionTests(unittest.TestCase):
    """A stalled backend is retried; a genuine empty review is not.

    Regression intent: scoring a stalled run as zero-recall both understates
    the config and, across a bake-off matrix, computes medians over uneven
    run counts. Measured at 27% of attempts on a 3-config pilot — and
    unevenly: one config lost 2 of 4 runs while another lost 0 of 3, which
    would have made the stall-prone config look worse than it is.

    The converse matters just as much: a reviewer that ran and honestly
    found nothing must NOT be retried, or the sample skews toward configs
    that happen to be chatty.
    """

    def test_missing_envelope_is_stalled(self):
        self.assertTrue(replay._is_stalled_run(None))

    def test_error_status_is_stalled(self):
        self.assertTrue(replay._is_stalled_run({"status": "error"}))

    def test_absent_status_is_stalled(self):
        self.assertTrue(replay._is_stalled_run({}))

    def test_ok_with_zero_findings_is_NOT_stalled(self):
        # The reviewer ran and found nothing. That is a real result and
        # belongs in the score — retrying it would bias the sample.
        env = {"status": "ok",
               "review": {"verdict": "accepted", "issues": []}}
        self.assertFalse(replay._is_stalled_run(env))

    def test_ok_with_findings_is_NOT_stalled(self):
        env = {"status": "ok",
               "review": {"verdict": "rejected",
                          "issues": [{"file": "a.py", "line": 1}]}}
        self.assertFalse(replay._is_stalled_run(env))


class ArgparseHelpTests(unittest.TestCase):
    """`--help` must render.

    Regression intent: argparse runs help strings through %-formatting, so a
    literal `%` in one (e.g. "~9-14% precision") raises ValueError and takes
    down `--help` entirely — while every other invocation keeps working, so
    it is easy to ship unnoticed.
    """

    def test_help_renders_without_format_error(self):
        import argparse
        import contextlib
        import io

        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            with self.assertRaises(SystemExit) as cm:
                replay.main(["--help"])
        self.assertEqual(cm.exception.code, 0)
        self.assertIn("--source", buf.getvalue())


class DiffBaseArgTests(unittest.TestCase):
    """The reviewer must be told the exact diff range.

    Regression intent: replay checks out a detached worktree at
    head_before, so with no --diff-base the agent guesses. The natural
    guess, `git diff main...HEAD`, three-dots against a local base branch
    that has advanced past the PR's merge base — measured on kernel-8276
    as 336 files instead of 22, varying run to run. That made diff scope
    an uncontrolled variable underneath every benchmark score.
    """

    def _capture_args(self, **kw):
        seen = {}

        def fake_run(args, **kwargs):
            seen["args"] = args

            class R:
                returncode = 0

            return R()

        orig = replay.subprocess.run
        replay.subprocess.run = fake_run
        try:
            with tempfile.TemporaryDirectory() as tmp:
                replay.run_bramble_code_review(
                    bramble_bin="/bin/true",
                    cfg=replay.CONFIGS["codex-5.4-mini"],
                    goal="g",
                    cwd=Path(tmp),
                    envelope_file=Path(tmp) / "env.json",
                    protocol_log_dir=Path(tmp) / "plog",
                    log_dir=Path(tmp) / "log",
                    run_tag="t",
                    **kw,
                )
        finally:
            replay.subprocess.run = orig
        return seen["args"]

    def test_diff_base_is_passed_through(self):
        args = self._capture_args(diff_base="deadbeef" * 5)
        self.assertIn("--diff-base", args)
        self.assertEqual(args[args.index("--diff-base") + 1], "deadbeef" * 5)

    def test_omitted_when_no_base(self):
        # A record with no merge_base must not pass an empty flag value.
        args = self._capture_args(diff_base=None)
        self.assertNotIn("--diff-base", args)

    def test_extra_args_survive_diff_base(self):
        # The persona flag lives in extra_args; appending --diff-base must
        # not displace it.
        seen = {}

        def fake_run(args, **kwargs):
            seen["args"] = args

            class R:
                returncode = 0

            return R()

        orig = replay.subprocess.run
        replay.subprocess.run = fake_run
        try:
            with tempfile.TemporaryDirectory() as tmp:
                replay.run_bramble_code_review(
                    bramble_bin="/bin/true",
                    cfg=replay.CONFIGS["luna-coverage-ledger"],
                    goal="g",
                    cwd=Path(tmp),
                    envelope_file=Path(tmp) / "env.json",
                    protocol_log_dir=Path(tmp) / "plog",
                    log_dir=Path(tmp) / "log",
                    run_tag="t",
                    diff_base="abc123",
                )
        finally:
            replay.subprocess.run = orig
        args = seen["args"]
        self.assertIn("--diff-base", args)
        self.assertIn("--review-prompt-file", args)
        self.assertIn("--effort", args)


class MissingCommitHintTests(unittest.TestCase):
    """An absent head_before must name the fix, not just fail.

    Regression intent: older records whose PR branch was merged and
    deleted fail with a bare "fatal: invalid reference: <sha>", which
    reads like dataset corruption and sends you auditing the record
    instead of running one `git fetch refs/pull/N/head`.
    """

    def _repo(self, tmp: str) -> Path:
        repo = Path(tmp) / "repo"
        repo.mkdir()
        hl.git(repo, "init", "-q")
        hl.git(repo, "config", "user.email", "t@example.com")
        hl.git(repo, "config", "user.name", "t")
        (repo / "f.txt").write_text("hello\n")
        hl.git(repo, "add", "f.txt")
        hl.git(repo, "commit", "-q", "-m", "init")
        return repo

    def test_absent_commit_gets_fetch_hint(self):
        absent = "0" * 40
        with tempfile.TemporaryDirectory() as tmp:
            repo = self._repo(tmp)
            with self.assertRaises(RuntimeError) as cm:
                with replay.TempWorktree(repo, absent, "x"):
                    pass
            msg = str(cm.exception)
            self.assertIn("is not in", msg)
            self.assertIn("refs/pull/<PR>/head", msg)
            self.assertIn("force-pushed", msg)

    def test_present_commit_adds_no_hint(self):
        # A worktree failure for any *other* reason must not be
        # mislabelled as a missing commit.
        with tempfile.TemporaryDirectory() as tmp:
            repo = self._repo(tmp)
            sha = hl.git(repo, "rev-parse", "HEAD").stdout.strip()
            wt = replay.TempWorktree(repo, sha, "y")
            with wt:
                pass
            self.assertEqual(wt._missing_commit_hint(), "")


class PersonaVariantConfigTests(unittest.TestCase):
    """Persona-variant configs must point at a real file, absolutely.

    Regression intent: bramble's `loadPersonaFile` only *warns* when the
    `--review-prompt-file` path is missing or empty, then silently falls
    back to the built-in persona. A variant with a bad path therefore
    reviews with the baseline prompt and reports the baseline's score
    under the variant's name — a wrong benchmark number that looks like a
    valid null result. `_persona_args` raises instead, and the paths must
    be absolute because bramble runs with cwd set to the PR worktree.
    """

    PERSONA_CONFIGS = (
        "luna-no-suppression",
        "luna-coverage-ledger",
        "luna-defect-class-priming",
        "luna-confidence-band",
        "luna-adversarial-successor",
        "luna-severity-floor",
        "luna-file-at-the-read",
        "luna-localize-only",
        "luna-localize-sweep",
        "luna-localize-sweep-reuse",
        "luna-localize-writers",
        "luna-localize-reuse",
        "mini-localize-only",
        "composer-localize-only",
    )

    def test_persona_configs_resolve_to_existing_absolute_files(self):
        for name in self.PERSONA_CONFIGS:
            with self.subTest(config=name):
                args = replay.CONFIGS[name].extra_args
                self.assertIn("--review-prompt-file", args)
                path = Path(args[args.index("--review-prompt-file") + 1])
                self.assertTrue(path.is_absolute(), f"{name}: not absolute")
                self.assertTrue(path.is_file(), f"{name}: missing {path}")
                self.assertTrue(
                    path.read_text().strip(),
                    f"{name}: empty persona would silently fall back",
                )

    def test_missing_persona_raises_rather_than_falling_back(self):
        with self.assertRaises(FileNotFoundError):
            replay._persona_args("no-such-persona-variant")


class ReplaySourceFilterTests(unittest.TestCase):
    """GitHub-sourced GT is out of the scoring pool by default.

    Its ground truth rests on bot comments with no pr-polish triage — ~9-14%
    precision on the kernel corpus — so scoring against it rewards a
    reviewer for reproducing bot noise.
    """

    def _index(self, d: Path) -> Path:
        (d / "index.json").write_text(json.dumps({
            "schema_version": 3,
            "prs": [
                {"file": "kernel-1.json", "ground_truth_collected": True,
                 "harvest_source": "pr-polish"},
                {"file": "kernel-2.json", "ground_truth_collected": True,
                 "harvest_source": "github"},
                {"file": "kernel-3.json", "ground_truth_collected": True},
            ],
        }))
        return d

    def test_github_excluded_by_default(self):
        with tempfile.TemporaryDirectory() as d:
            ds = self._index(Path(d))
            picked = replay.select_replay_targets(dataset_dir=ds, sample=9)
            self.assertEqual(sorted(picked), ["kernel-1", "kernel-3"])

    def test_explicit_github_source_includes_it(self):
        with tempfile.TemporaryDirectory() as d:
            ds = self._index(Path(d))
            picked = replay.select_replay_targets(
                dataset_dir=ds, sample=9, sources={"github"})
            self.assertEqual(sorted(picked), ["kernel-2"])

    def test_both_sources_yields_everything(self):
        with tempfile.TemporaryDirectory() as d:
            ds = self._index(Path(d))
            picked = replay.select_replay_targets(
                dataset_dir=ds, sample=9,
                sources={"github", "pr-polish"})
            self.assertEqual(
                sorted(picked), ["kernel-1", "kernel-2", "kernel-3"])

    def test_all_excluded_reports_the_exclusion(self):
        # "no GT anywhere" and "GT exists but you filtered it out" are
        # different problems; the message must not conflate them.
        with tempfile.TemporaryDirectory() as d:
            (Path(d) / "index.json").write_text(json.dumps({
                "schema_version": 3,
                "prs": [{"file": "kernel-2.json",
                         "ground_truth_collected": True,
                         "harvest_source": "github"}],
            }))
            with self.assertRaises(SystemExit) as cm:
                replay.select_replay_targets(dataset_dir=Path(d), sample=5)
            self.assertIn("excluded by --source", str(cm.exception))

    def test_target_harvest_source_lookup(self):
        with tempfile.TemporaryDirectory() as d:
            ds = self._index(Path(d))
            self.assertEqual(
                replay._target_harvest_source(ds, "kernel-2"), "github")
            self.assertEqual(
                replay._target_harvest_source(ds, "kernel-1"), "pr-polish")
            # Legacy entry with no harvest_source -> pr-polish.
            self.assertEqual(
                replay._target_harvest_source(ds, "kernel-3"), "pr-polish")
            # Unknown target must not raise; it only decorates a warning.
            self.assertIsNone(
                replay._target_harvest_source(ds, "kernel-404"))

    def test_target_harvest_source_accepts_json_filename(self):
        with tempfile.TemporaryDirectory() as d:
            ds = self._index(Path(d))
            self.assertEqual(
                replay._target_harvest_source(ds, "kernel-2.json"), "github")

    def test_target_harvest_source_missing_index_is_none(self):
        with tempfile.TemporaryDirectory() as d:
            self.assertIsNone(
                replay._target_harvest_source(Path(d), "kernel-1"))


class RunReplayValidationTests(unittest.TestCase):
    """run_replay must validate the frozen GT before scoring.

    The validation runs before any repo lookup or bramble spawn, so these
    are fast unit tests with a bogus RepoMap.
    """

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
        return {
            "schema_version": 2,
            "pr": {"repo_name": "demo", "pr_number": "1"},
            "harvested_rounds": [],
            "ground_truth_v3": block,
        }

    def _run(self, dataset: dict, *, strict: bool = False):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / "demo-1.json"
            path.write_text(json.dumps(dataset))
            return replay.run_replay(
                path,
                repos_root=hl.RepoMap(mapping={}),
                configs=[],
                tier_filter=None,
                bramble_bin="bramble",
                goal_source="auto",
                timeout_seconds=1,
                log_root=Path(d) / "logs",
                verbose=False,
                strict=strict,
            )

    def test_no_ground_truth_raises(self):
        with self.assertRaises(RuntimeError) as cm:
            self._run({"schema_version": 2, "harvested_rounds": []})
        self.assertIn("ground_truth", str(cm.exception))

    def test_malformed_gt_aborts(self):
        # Drop a required severity -> validate_dataset reports a structural
        # error -> run_replay must abort before any scoring.
        ds = self._gt_block()
        del ds["ground_truth_v3"]["true_positives"][0]["severity"]
        with self.assertRaises(RuntimeError) as cm:
            self._run(ds)
        self.assertIn("malformed", str(cm.exception))

    def test_quality_warning_does_not_abort_without_strict(self):
        # An unconverged census is a quality warning, not a structural
        # error: without --strict it must not abort here. It still fails
        # later (no repo checkout), but not on the warning.
        ds = self._gt_block(census_converged=False)
        with self.assertRaises(RuntimeError) as cm:
            self._run(ds, strict=False)
        self.assertIn("no local checkout", str(cm.exception))

    def test_quality_warning_aborts_under_strict(self):
        ds = self._gt_block(census_converged=False)
        with self.assertRaises(RuntimeError) as cm:
            self._run(ds, strict=True)
        self.assertIn("--strict", str(cm.exception))


if __name__ == "__main__":
    unittest.main()
