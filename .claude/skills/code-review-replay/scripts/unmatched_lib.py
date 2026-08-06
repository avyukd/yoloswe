"""Triage the ``unmatched`` bucket that precision and recall cannot see.

Replay scores a finding three ways: it lands on a frozen true positive
(``matched_tp``), on a frozen false positive (``matched_fp``), or on
neither (``unmatched``). Precision is computed from the first two only, so
**an unmatched finding is invisible to every headline metric** — it neither
raises recall nor lowers precision.

That is a deliberate choice (the ground truth is a lower bound; scoring
against entries no judge ever ruled on would be fabrication), but it leaves
a blind spot precisely where recall-first work lives. A variant tuned to
surface more defects dumps most of its extra output into ``unmatched``,
where "found a real bug the census missed" and "generated plausible noise"
look identical.

This module supplies the cheap mechanical discriminator: **cross-run
recurrence**. Independent runs are separate samples from the reviewer's
distribution. Noise is drawn from a wide space and rarely collides on the
same line twice; a real defect is a property of the code and collides
readily. Agreement across *different configs* is the stronger form, since
two models with different training converging on one location is weak
evidence of confabulation.

Measured on kernel-8276 (2026-08-05, 9 runs across 2 configs): 27 distinct
unmatched locations, of which 7 recurred and 5 of those recurred across
both configs — a plausible-defect population comparable in size to the
10-entry frozen census.

This ranks candidates; it does not label them. Promoting a recurrent
finding to ground truth requires the collection path (``collect.py``), so
that every variant is scored against a census its own output did not
shape.
"""

from __future__ import annotations

from collections import defaultdict
from dataclasses import dataclass, field
from typing import Iterable, Optional

import collect_lib as cl


@dataclass
class UnmatchedLocation:
    """One (file, line) that findings hit but the frozen GT never judged."""

    file: str
    line: Optional[int]
    runs: set = field(default_factory=set)
    configs: set = field(default_factory=set)

    @property
    def n_runs(self) -> int:
        return len(self.runs)

    @property
    def n_configs(self) -> int:
        return len(self.configs)

    @property
    def recurrent(self) -> bool:
        """Seen by more than one independent run."""
        return self.n_runs >= 2

    @property
    def cross_config(self) -> bool:
        """Seen by more than one config — the stronger signal."""
        return self.n_configs >= 2


def _is_judged(gt: dict, file: str, line: Optional[int]) -> bool:
    """True when the frozen GT already rules on this location."""
    for key in ("true_positives", "false_positives"):
        for e in gt.get(key) or []:
            if cl.same_defect(e.get("file"), e.get("line"), file, line):
                return True
    return False


def collect_unmatched(
    observations: Iterable[tuple[str, str, str, Optional[int]]],
    gt: dict,
) -> list[UnmatchedLocation]:
    """Group unmatched findings by defect identity.

    ``observations`` yields ``(run_id, config, file, line)`` — one entry per
    reported finding location, already expanded from any ``sites`` array.

    Locations are merged under the same ``same_defect`` rule the ground
    truth uses (normalized path, ±3 lines), so a defect reported at :100 in
    one run and :102 in another counts as recurring rather than as two
    singletons. Without that the recurrence signal would be diluted by
    exactly the line drift the GT already tolerates.
    """
    buckets: dict[tuple, UnmatchedLocation] = {}
    for run_id, config, file, line in observations:
        if not file:
            continue
        if _is_judged(gt, file, line):
            continue
        hit = None
        for key, loc in buckets.items():
            if cl.same_defect(loc.file, loc.line, file, line):
                hit = key
                break
        if hit is None:
            hit = (file, line)
            buckets[hit] = UnmatchedLocation(file=file, line=line)
        buckets[hit].runs.add(run_id)
        buckets[hit].configs.add(config)
    return sorted(
        buckets.values(),
        key=lambda x: (-x.n_configs, -x.n_runs, x.file, x.line or 0),
    )


def summarize(locations: list[UnmatchedLocation]) -> dict:
    """Headline counts for a recall-first scoreboard.

    The decision rule these feed: unmatched growth concentrated in
    ``recurrent`` (especially ``cross_config``) is a variant finding things
    the census missed; growth concentrated in ``singleton`` is a variant
    generating noise. Recall alone cannot tell those apart.
    """
    rec = [x for x in locations if x.recurrent]
    return {
        "unmatched_distinct": len(locations),
        "unmatched_recurrent": len(rec),
        "unmatched_cross_config": len([x for x in rec if x.cross_config]),
        "unmatched_singleton": len(locations) - len(rec),
    }


def format_report(locations: list[UnmatchedLocation], limit: int = 20) -> str:
    """Human-readable triage list, strongest candidates first."""
    s = summarize(locations)
    lines = [
        "unmatched findings (not judged by the frozen ground truth)",
        f"  distinct locations : {s['unmatched_distinct']}",
        f"  recurrent (>=2 runs): {s['unmatched_recurrent']}"
        f"  [cross-config: {s['unmatched_cross_config']}]",
        f"  singleton          : {s['unmatched_singleton']}",
    ]
    rec = [x for x in locations if x.recurrent]
    if rec:
        lines.append("")
        lines.append(
            "  candidates for re-collection (recurrence suggests a real "
            "defect the census missed):"
        )
        for loc in rec[:limit]:
            mark = "**" if loc.cross_config else "  "
            lines.append(
                f"  {mark} {loc.file}:{loc.line}  "
                f"{loc.n_runs} runs / {loc.n_configs} configs"
            )
        if len(rec) > limit:
            lines.append(f"  ... and {len(rec) - limit} more")
        lines.append("")
        lines.append(
            "  These are NOT recall credit. Feed them through collect.py so "
            "the census ratchets up through the judged path."
        )
    return "\n".join(lines)
