"""Offline quality-gate adapter (2026-09-21 sandbox hardening 07; contract in docs/rag/evaluation.md §5).

The adapter reports these things as *separate* fields and never folds one into
another:

* candidate hit      — an observation ID was in the retrieved candidate pool
* visible evidence   — the observation's original body was actually visible in
                        the packed evidence (Unicode ``[start, end)`` spans on
                        the original body; summary text is not raw evidence)
* forbidden / scope  — forbidden observation IDs and out-of-room observations,
                        counted separately for the candidate pool and the
                        visible set
* person violation   — resolved person IDs outside the caller-provided scope
                        (``None`` scope is "no scope provided", not "empty")
* time violation     — observations outside hard time bounds
* answer support     — semantic support of the generated answer: only a human
                        reviewer can set it, otherwise ``not_evaluated``
* abstention         — empty retrieval, an abstaining answer, and *correct*
                        abstention are three different facts
* tool outcome       — only ``success`` counts as success

Gold is the message-level contract from ``tools/evaluation/evidence_metrics``
(schema_version 1).  No second gold schema is introduced here; this module only
adds the run-trace and human-review inputs and the derived report.

Nothing here declares a model qualified.  ``gate_decision`` is always
``not_decided`` because the decision belongs to a human reviewer.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import sys
from collections import Counter, defaultdict
from dataclasses import dataclass, field
from datetime import datetime
from pathlib import Path
from typing import Any, Iterable, Mapping, Sequence

_EVAL_DIR = Path(__file__).resolve().parents[1]
if str(_EVAL_DIR) not in sys.path:
    sys.path.insert(0, str(_EVAL_DIR))

from evidence_metrics import (  # noqa: E402
    GoldQuestion,
    GoldSet,
    QuestionEvaluation,
    evaluate_retrieval,
    load_gold,
)
from rag_upgrade.index_binding import BindingError, assert_gold_bound_to_index  # noqa: E402

NOT_EVALUATED = "not_evaluated"

ANSWER_OUTCOMES = frozenset({"answered", "abstained", "error", "cancelled", "not_run"})
TOOL_OUTCOMES = frozenset(
    {"success", "error", "unconfigured", "disabled", "needs_input", "unknown_outcome", "not_called"}
)
#: Span sources that count as visible *raw* evidence.
RAW_SPAN_SOURCES = frozenset({"raw", "leaf", "parent", "matched", "expanded"})
#: Span sources that are generated text and never count as raw evidence.
NON_RAW_SPAN_SOURCES = frozenset({"summary", "assistant_history", "session_summary", "generated"})
SUPPORT_VERDICTS = frozenset({"supported", "partial", "unsupported"})
ATTRIBUTION_VERDICTS = frozenset({"correct", "incorrect", "not_applicable"})


class TraceContractError(ValueError):
    """A run trace violates the adapter contract (broken adapter, not a miss)."""


class ReviewContractError(ValueError):
    """A human review record is incomplete or contradicts gold."""


class PromotionRefusedError(ReviewContractError):
    """Gold cannot be promoted to human_reviewed."""


class StaleGoldError(ValueError):
    """Gold observation IDs no longer match the current corpus bodies."""


# --------------------------------------------------------------------------- inputs


def _str_tuple(value: Any, field_name: str) -> tuple[str, ...]:
    if value is None:
        return ()
    if isinstance(value, (str, bytes, bytearray)) or not isinstance(value, Sequence):
        raise TraceContractError(f"{field_name} must be a list of strings")
    out: list[str] = []
    for index, item in enumerate(value):
        if not isinstance(item, str) or not item.strip():
            raise TraceContractError(f"{field_name}[{index}] must be a non-empty string")
        out.append(item)
    return tuple(out)


def _optional_str_tuple(value: Any, field_name: str) -> tuple[str, ...] | None:
    if value is None:
        return None
    return _str_tuple(value, field_name)


@dataclass(frozen=True, slots=True)
class VisibleSpan:
    """One visible slice of an original observation body.

    ``start``/``end`` are Unicode code-point offsets on the *original* body,
    end exclusive.  They are never offsets into a formatted excerpt.
    ``complete`` is an optional cross-check from the producer; it must agree
    with the offsets or the trace is rejected.
    """

    observation_id: str
    start: int
    end: int
    source: str = "raw"
    complete: bool | None = None

    @classmethod
    def from_mapping(cls, raw: Mapping[str, Any], index: int = 0) -> "VisibleSpan":
        prefix = f"visible_spans[{index}]"
        if not isinstance(raw, Mapping):
            raise TraceContractError(f"{prefix} must be an object")
        oid = raw.get("observation_id")
        if not isinstance(oid, str) or not oid.strip():
            raise TraceContractError(f"{prefix}.observation_id must be a non-empty string")
        start = raw.get("start", raw.get("char_start"))
        end = raw.get("end", raw.get("char_end"))
        for name, value in (("start", start), ("end", end)):
            if isinstance(value, bool) or not isinstance(value, int):
                raise TraceContractError(f"{prefix}.{name} must be an integer")
        if start < 0 or end <= start:
            raise TraceContractError(f"{prefix} must satisfy 0 <= start < end")
        source = raw.get("source", "raw")
        if not isinstance(source, str) or not source:
            raise TraceContractError(f"{prefix}.source must be a string")
        if source not in RAW_SPAN_SOURCES and source not in NON_RAW_SPAN_SOURCES:
            raise TraceContractError(f"{prefix}.source {source!r} is not a known span source")
        complete = raw.get("complete")
        if complete is not None and not isinstance(complete, bool):
            raise TraceContractError(f"{prefix}.complete must be a boolean when present")
        return cls(observation_id=oid, start=start, end=end, source=source, complete=complete)

    @property
    def raw(self) -> bool:
        return self.source in RAW_SPAN_SOURCES


@dataclass(frozen=True, slots=True)
class ToolOutcome:
    name: str
    outcome: str
    detail: str | None = None

    @classmethod
    def from_mapping(cls, raw: Mapping[str, Any], index: int = 0) -> "ToolOutcome":
        prefix = f"tools[{index}]"
        if not isinstance(raw, Mapping):
            raise TraceContractError(f"{prefix} must be an object")
        name = raw.get("name")
        if not isinstance(name, str) or not name.strip():
            raise TraceContractError(f"{prefix}.name must be a non-empty string")
        outcome = raw.get("outcome")
        if outcome not in TOOL_OUTCOMES:
            raise TraceContractError(
                f"{prefix}.outcome must be one of {sorted(TOOL_OUTCOMES)}; ok/data flags are not outcomes"
            )
        detail = raw.get("detail")
        if detail is not None and not isinstance(detail, str):
            raise TraceContractError(f"{prefix}.detail must be a string when present")
        return cls(name=name, outcome=outcome, detail=detail)


@dataclass(frozen=True, slots=True)
class RunCase:
    """What the runtime (08) reports for one gold question.

    ``candidate_observation_ids`` is the ranked candidate pool by observation
    ID (matched leaves, before any packing/truncation).  ``visible_spans`` are
    the spans actually shown to the model.  Scope fields keep the None-vs-empty
    distinction of the COMMON contract.
    """

    question_id: str
    candidate_observation_ids: tuple[str, ...]
    visible_spans: tuple[VisibleSpan, ...] = ()
    answer_outcome: str = "not_run"
    answer_text: str | None = None
    tools: tuple[ToolOutcome, ...] = ()
    allowed_person_ids: tuple[str, ...] | None = None
    resolved_person_ids: tuple[str, ...] = ()
    hard_time_bounds: tuple[str | None, str | None] | None = None
    time_violation_observation_ids: tuple[str, ...] = ()
    label: str = "actual"

    @classmethod
    def from_mapping(cls, raw: Mapping[str, Any]) -> "RunCase":
        if not isinstance(raw, Mapping):
            raise TraceContractError("run case must be an object")
        qid = raw.get("question_id")
        if not isinstance(qid, str) or not qid.strip():
            raise TraceContractError("run case question_id must be a non-empty string")
        candidates = _str_tuple(
            raw.get("candidate_observation_ids", raw.get("retrieved_observation_ids")),
            f"{qid}.candidate_observation_ids",
        )
        spans_raw = raw.get("visible_spans") or ()
        if isinstance(spans_raw, (str, bytes, bytearray)) or not isinstance(spans_raw, Sequence):
            raise TraceContractError(f"{qid}.visible_spans must be a list")
        spans = tuple(VisibleSpan.from_mapping(item, i) for i, item in enumerate(spans_raw))
        answer = raw.get("answer") or {}
        if not isinstance(answer, Mapping):
            raise TraceContractError(f"{qid}.answer must be an object")
        outcome = answer.get("outcome", "not_run")
        if outcome not in ANSWER_OUTCOMES:
            raise TraceContractError(f"{qid}.answer.outcome must be one of {sorted(ANSWER_OUTCOMES)}")
        text = answer.get("text")
        if text is not None and not isinstance(text, str):
            raise TraceContractError(f"{qid}.answer.text must be a string when present")
        tools_raw = raw.get("tools") or ()
        if isinstance(tools_raw, (str, bytes, bytearray)) or not isinstance(tools_raw, Sequence):
            raise TraceContractError(f"{qid}.tools must be a list")
        tools = tuple(ToolOutcome.from_mapping(item, i) for i, item in enumerate(tools_raw))
        person = raw.get("person") or {}
        if not isinstance(person, Mapping):
            raise TraceContractError(f"{qid}.person must be an object")
        allowed_people = _optional_str_tuple(
            person.get("allowed_person_ids"), f"{qid}.person.allowed_person_ids"
        )
        resolved_people = _str_tuple(person.get("resolved_person_ids"), f"{qid}.person.resolved_person_ids")
        time_raw = raw.get("time") or {}
        if not isinstance(time_raw, Mapping):
            raise TraceContractError(f"{qid}.time must be an object")
        bounds_raw = time_raw.get("hard_bounds")
        bounds: tuple[str | None, str | None] | None
        if bounds_raw is None:
            bounds = None
        else:
            if not isinstance(bounds_raw, Mapping):
                raise TraceContractError(f"{qid}.time.hard_bounds must be an object")
            lo = bounds_raw.get("start")
            hi = bounds_raw.get("end")
            for name, value in (("start", lo), ("end", hi)):
                if value is not None and not isinstance(value, str):
                    raise TraceContractError(f"{qid}.time.hard_bounds.{name} must be an ISO string or null")
            bounds = (lo, hi)
        reported = _str_tuple(
            time_raw.get("violation_observation_ids"), f"{qid}.time.violation_observation_ids"
        )
        label = raw.get("label", "actual")
        if not isinstance(label, str) or not label:
            raise TraceContractError(f"{qid}.label must be a non-empty string")
        return cls(
            question_id=qid,
            candidate_observation_ids=candidates,
            visible_spans=spans,
            answer_outcome=outcome,
            answer_text=text,
            tools=tools,
            allowed_person_ids=allowed_people,
            resolved_person_ids=resolved_people,
            hard_time_bounds=bounds,
            time_violation_observation_ids=reported,
            label=label,
        )


@dataclass(frozen=True, slots=True)
class ReviewVerdict:
    """A human reviewer's semantic verdict for one question.

    A verdict without a reviewer is not a verdict.  Fields left ``None`` stay
    ``not_evaluated`` in the report.
    """

    question_id: str
    reviewer: str
    answer_support: str | None = None
    person_attribution: str | None = None
    abstention_correct: bool | None = None
    gold_verdict: str | None = None
    note: str = ""

    @classmethod
    def from_mapping(cls, raw: Mapping[str, Any]) -> "ReviewVerdict":
        if not isinstance(raw, Mapping):
            raise ReviewContractError("review must be an object")
        qid = raw.get("question_id")
        if not isinstance(qid, str) or not qid.strip():
            raise ReviewContractError("review question_id must be a non-empty string")
        reviewer = raw.get("reviewer")
        if not isinstance(reviewer, str) or not reviewer.strip():
            raise ReviewContractError(f"review {qid} has no reviewer; unreviewed input is not a verdict")
        support = raw.get("answer_support")
        if support is not None and support not in SUPPORT_VERDICTS:
            raise ReviewContractError(f"review {qid}.answer_support must be one of {sorted(SUPPORT_VERDICTS)}")
        attribution = raw.get("person_attribution")
        if attribution is not None and attribution not in ATTRIBUTION_VERDICTS:
            raise ReviewContractError(
                f"review {qid}.person_attribution must be one of {sorted(ATTRIBUTION_VERDICTS)}"
            )
        abstain_ok = raw.get("abstention_correct")
        if abstain_ok is not None and not isinstance(abstain_ok, bool):
            raise ReviewContractError(f"review {qid}.abstention_correct must be a boolean")
        gold_verdict = raw.get("gold_verdict")
        if gold_verdict is not None and gold_verdict not in {"accept", "reject", "fix"}:
            raise ReviewContractError(f"review {qid}.gold_verdict must be accept/reject/fix")
        note = raw.get("note", "")
        if not isinstance(note, str):
            raise ReviewContractError(f"review {qid}.note must be a string")
        return cls(
            question_id=qid,
            reviewer=reviewer,
            answer_support=support,
            person_attribution=attribution,
            abstention_correct=abstain_ok,
            gold_verdict=gold_verdict,
            note=note,
        )


def load_run_cases(source: Sequence[Mapping[str, Any]] | Mapping[str, Any] | str | Path) -> dict[str, RunCase]:
    """Load run cases from a list, a ``{"cases": [...]}`` mapping, or a JSON path."""

    if isinstance(source, (str, Path)):
        raw = json.loads(Path(source).read_text(encoding="utf-8"))
    else:
        raw = source
    if isinstance(raw, Mapping):
        raw = raw.get("cases", raw.get("questions"))
    if isinstance(raw, (str, bytes, bytearray)) or not isinstance(raw, Sequence):
        raise TraceContractError("run cases must be a list or a mapping with a 'cases' list")
    cases: dict[str, RunCase] = {}
    for item in raw:
        case = RunCase.from_mapping(item)
        if case.question_id in cases:
            raise TraceContractError(f"duplicate run case for question {case.question_id}")
        cases[case.question_id] = case
    return cases


def load_reviews(source: Sequence[Mapping[str, Any]] | Mapping[str, Any] | str | Path) -> dict[str, ReviewVerdict]:
    if isinstance(source, (str, Path)):
        raw = json.loads(Path(source).read_text(encoding="utf-8"))
    else:
        raw = source
    if isinstance(raw, Mapping):
        raw = raw.get("reviews", raw.get("verdicts"))
    if isinstance(raw, (str, bytes, bytearray)) or not isinstance(raw, Sequence):
        raise ReviewContractError("reviews must be a list or a mapping with a 'reviews' list")
    out: dict[str, ReviewVerdict] = {}
    for item in raw:
        verdict = ReviewVerdict.from_mapping(item)
        if verdict.question_id in out:
            raise ReviewContractError(f"duplicate review for question {verdict.question_id}")
        out[verdict.question_id] = verdict
    return out


# --------------------------------------------------------------------------- spans


def merge_intervals(intervals: Iterable[tuple[int, int]]) -> list[tuple[int, int]]:
    ordered = sorted((int(a), int(b)) for a, b in intervals if b > a)
    merged: list[tuple[int, int]] = []
    for start, end in ordered:
        if merged and start <= merged[-1][1]:
            merged[-1] = (merged[-1][0], max(merged[-1][1], end))
        else:
            merged.append((start, end))
    return merged


@dataclass(frozen=True, slots=True)
class ObservationVisibility:
    observation_id: str
    body_length: int
    raw_intervals: tuple[tuple[int, int], ...]
    raw_covered_chars: int
    raw_span_count: int
    non_raw_span_count: int

    @property
    def complete(self) -> bool:
        return self.body_length > 0 and self.raw_covered_chars >= self.body_length

    @property
    def partial(self) -> bool:
        return 0 < self.raw_covered_chars < self.body_length

    @property
    def coverage(self) -> float | None:
        if self.body_length == 0:
            return None
        return self.raw_covered_chars / self.body_length

    @property
    def summary_only(self) -> bool:
        return self.raw_covered_chars == 0 and self.non_raw_span_count > 0


def visibility_by_observation(
    gold: GoldSet, spans: Sequence[VisibleSpan], question_id: str
) -> dict[str, ObservationVisibility]:
    """Collapse spans per observation and validate offsets against gold bodies.

    Duplicate spans (the same observation shown by several leaves/parents) are
    merged, so one body never counts twice.  Offsets beyond the gold body or a
    ``complete`` flag that contradicts the offsets reject the trace.
    """

    raw_parts: dict[str, list[tuple[int, int]]] = defaultdict(list)
    raw_counts: Counter[str] = Counter()
    non_raw_counts: Counter[str] = Counter()
    for span in spans:
        observation = gold.observations.get(span.observation_id)
        if observation is None:
            raise TraceContractError(
                f"run {question_id} visible span references unknown observation_id {span.observation_id}"
            )
        body_length = len(observation.text)
        if span.end > body_length:
            raise TraceContractError(
                f"run {question_id} span for {span.observation_id} ends at {span.end} beyond body length {body_length}"
            )
        if span.complete is True and (span.start != 0 or span.end != body_length):
            raise TraceContractError(
                f"run {question_id} span for {span.observation_id} claims complete but covers [{span.start}, {span.end}) of {body_length}"
            )
        if span.complete is False and span.start == 0 and span.end == body_length:
            raise TraceContractError(
                f"run {question_id} span for {span.observation_id} claims partial but covers the whole body"
            )
        if span.raw:
            raw_parts[span.observation_id].append((span.start, span.end))
            raw_counts[span.observation_id] += 1
        else:
            non_raw_counts[span.observation_id] += 1
    out: dict[str, ObservationVisibility] = {}
    for oid in set(raw_parts) | set(non_raw_counts):
        merged = merge_intervals(raw_parts.get(oid, ()))
        covered = sum(end - start for start, end in merged)
        out[oid] = ObservationVisibility(
            observation_id=oid,
            body_length=len(gold.observations[oid].text),
            raw_intervals=tuple(merged),
            raw_covered_chars=covered,
            raw_span_count=raw_counts.get(oid, 0),
            non_raw_span_count=non_raw_counts.get(oid, 0),
        )
    return out


# --------------------------------------------------------------------------- time


def _parse_time(value: str | None) -> datetime | None:
    if value is None:
        return None
    try:
        return datetime.fromisoformat(value)
    except (TypeError, ValueError) as exc:
        raise TraceContractError(f"time value {value!r} is not ISO-8601") from exc


def _time_status(observed: datetime, lo: datetime | None, hi: datetime | None) -> str:
    """ok / violation / undetermined.  Never attaches a timezone to naive times."""

    for bound in (lo, hi):
        if bound is None:
            continue
        if (bound.tzinfo is None) != (observed.tzinfo is None):
            return "undetermined"
    if lo is not None and observed < lo:
        return "violation"
    if hi is not None and observed >= hi:
        return "violation"
    return "ok"


# --------------------------------------------------------------------------- per-question


@dataclass(frozen=True, slots=True)
class CaseResult:
    question_id: str
    split: str
    positive: bool
    abstain_expected: bool
    primary_room: str
    case_present: bool
    label: str
    candidate: dict[str, Any]
    visible: dict[str, Any]
    person: dict[str, Any]
    time: dict[str, Any]
    answer: dict[str, Any]
    abstention: dict[str, Any]
    tools: dict[str, Any]

    def as_dict(self) -> dict[str, Any]:
        return {
            "question_id": self.question_id,
            "split": self.split,
            "positive": self.positive,
            "abstain_expected": self.abstain_expected,
            "primary_room": self.primary_room,
            "case_present": self.case_present,
            "label": self.label,
            "candidate": self.candidate,
            "visible": self.visible,
            "person": self.person,
            "time": self.time,
            "answer": self.answer,
            "abstention": self.abstention,
            "tools": self.tools,
        }


def _primary_room(gold: GoldSet, question: GoldQuestion) -> str:
    for group in question.required_evidence_groups:
        for oid in group.alternatives:
            return gold.observations[oid].source_channel_id
    if question.allowed_source_channel_ids:
        return question.allowed_source_channel_ids[0]
    return "unscoped"


def _visible_block(
    gold: GoldSet,
    question: GoldQuestion,
    case: RunCase,
    candidate_eval: QuestionEvaluation,
) -> dict[str, Any]:
    vis = visibility_by_observation(gold, case.visible_spans, case.question_id)
    complete_ids = sorted(oid for oid, v in vis.items() if v.complete)
    partial_ids = sorted(oid for oid, v in vis.items() if v.partial)
    summary_only_ids = sorted(oid for oid, v in vis.items() if v.summary_only)
    duplicate_span_count = sum(max(0, v.raw_span_count - 1) for v in vis.values())
    complete_set = set(complete_ids)
    partial_set = set(partial_ids)
    summary_only_set = set(summary_only_ids)

    group_rows: list[dict[str, Any]] = []
    complete_groups = 0
    partial_groups = 0
    summary_only_groups = 0
    candidate_top = set(candidate_eval.retrieved_observation_ids)
    truncated_candidate_ids: list[str] = []
    for group in question.required_evidence_groups:
        alts = set(group.alternatives)
        complete_hit = sorted(alts & complete_set)
        partial_hit = sorted((alts & partial_set) - complete_set)
        summary_hit = sorted((alts & summary_only_set) - complete_set - partial_set)
        cand_hit = sorted(alts & candidate_top)
        if complete_hit:
            status = "visible_complete"
            complete_groups += 1
        elif partial_hit:
            status = "visible_partial_only"
            partial_groups += 1
        elif summary_hit:
            status = "summary_only"
            summary_only_groups += 1
        elif cand_hit:
            status = "candidate_only"
        else:
            status = "missing"
        if cand_hit and not complete_hit:
            truncated_candidate_ids.extend(oid for oid in cand_hit if oid not in complete_set)
        group_rows.append(
            {
                "group_id": group.group_id,
                "status": status,
                "candidate_hit_ids": cand_hit,
                "visible_complete_ids": complete_hit,
                "visible_partial_ids": partial_hit,
                "summary_only_ids": summary_hit,
            }
        )
    group_count = len(question.required_evidence_groups)
    all_visible_ids = set(vis)
    forbidden_visible = sorted(all_visible_ids & set(question.forbidden_observation_ids))
    out_of_scope_visible = sorted(
        oid
        for oid in all_visible_ids
        if gold.observations[oid].source_channel_id not in question.allowed_source_channel_ids
    )
    return {
        "required_group_count": group_count,
        "visible_complete_group_count": complete_groups,
        "visible_partial_only_group_count": partial_groups,
        "summary_only_group_count": summary_only_groups,
        "visible_evidence_recall": (complete_groups / group_count) if group_count else None,
        "visible_complete_group_recovery": (complete_groups == group_count) if question.positive else None,
        "groups": group_rows,
        "visible_complete_observation_ids": complete_ids,
        "visible_partial_observation_ids": partial_ids,
        "summary_only_observation_ids": summary_only_ids,
        "truncated_candidate_ids": sorted(set(truncated_candidate_ids)),
        "duplicate_raw_span_count": duplicate_span_count,
        "visible_observation_count": len(vis),
        "forbidden_visible_ids": forbidden_visible,
        "forbidden_visible_count": len(forbidden_visible),
        "out_of_scope_visible_ids": out_of_scope_visible,
        "out_of_scope_visible_count": len(out_of_scope_visible),
    }


def _person_block(case: RunCase, review: ReviewVerdict | None) -> dict[str, Any]:
    resolved = tuple(dict.fromkeys(case.resolved_person_ids))
    if case.allowed_person_ids is None:
        status = "no_scope_provided"
        outside: tuple[str, ...] = ()
    else:
        allowed = set(case.allowed_person_ids)
        outside = tuple(pid for pid in resolved if pid not in allowed)
        status = "violation" if outside else "ok"
    attribution = NOT_EVALUATED
    if review is not None and review.person_attribution is not None:
        attribution = review.person_attribution
    return {
        "scope_status": status,
        "scope_provided": case.allowed_person_ids is not None,
        "allowed_person_count": None if case.allowed_person_ids is None else len(set(case.allowed_person_ids)),
        "resolved_person_ids": list(resolved),
        "out_of_scope_person_ids": list(outside),
        "attribution": attribution,
        "attribution_reviewer": review.reviewer if review is not None and attribution != NOT_EVALUATED else None,
    }


def _time_block(
    gold: GoldSet,
    case: RunCase,
    candidate_ids: Sequence[str],
    visible_ids: Sequence[str],
    observation_times: Mapping[str, str] | None,
) -> dict[str, Any]:
    reported = sorted(set(case.time_violation_observation_ids))
    for oid in reported:
        if oid not in gold.observations:
            raise TraceContractError(
                f"run {case.question_id} reports time violation for unknown observation_id {oid}"
            )
    if case.hard_time_bounds is None:
        return {
            "hard_bounds": None,
            "status": "no_hard_bounds",
            "checked_observation_count": 0,
            "candidate_violation_ids": [],
            "visible_violation_ids": [],
            "runtime_reported_violation_ids": reported,
            "undetermined_observation_ids": [],
            "violation_ids": reported,
        }
    lo = _parse_time(case.hard_time_bounds[0])
    hi = _parse_time(case.hard_time_bounds[1])
    if lo is not None and hi is not None and (lo.tzinfo is None) == (hi.tzinfo is None) and hi <= lo:
        raise TraceContractError(f"run {case.question_id} hard_bounds end must be after start")
    candidate_violations: list[str] = []
    visible_violations: list[str] = []
    undetermined: list[str] = []
    checked = 0
    if observation_times:
        for group_name, ids in (("candidate", candidate_ids), ("visible", visible_ids)):
            for oid in dict.fromkeys(ids):
                raw_time = observation_times.get(oid)
                if raw_time is None:
                    continue
                checked += 1
                status = _time_status(_parse_time(raw_time), lo, hi)
                if status == "violation":
                    (candidate_violations if group_name == "candidate" else visible_violations).append(oid)
                elif status == "undetermined":
                    undetermined.append(oid)
    all_violations = sorted(set(reported) | set(candidate_violations) | set(visible_violations))
    if all_violations:
        status = "violation"
    elif undetermined:
        status = "undetermined"
    elif checked == 0 and not observation_times:
        status = "not_checked_no_observation_times"
    else:
        status = "ok"
    return {
        "hard_bounds": {"start": case.hard_time_bounds[0], "end": case.hard_time_bounds[1]},
        "status": status,
        "checked_observation_count": checked,
        "candidate_violation_ids": sorted(set(candidate_violations)),
        "visible_violation_ids": sorted(set(visible_violations)),
        "runtime_reported_violation_ids": reported,
        "undetermined_observation_ids": sorted(set(undetermined)),
        "violation_ids": all_violations,
    }


def _answer_block(
    question: GoldQuestion,
    case: RunCase,
    review: ReviewVerdict | None,
    expected_strings: Sequence[str] | None,
) -> dict[str, Any]:
    string_check: dict[str, Any] = {"status": NOT_EVALUATED, "kind": "string_only"}
    if expected_strings and case.answer_text is not None and case.answer_outcome == "answered":
        matched = [s for s in expected_strings if s and s in case.answer_text]
        string_check = {
            "status": "matched" if matched else "unmatched",
            "kind": "string_only",
            "matched_count": len(matched),
            "expected_count": len(expected_strings),
        }
    support = NOT_EVALUATED
    if review is not None and review.answer_support is not None:
        support = review.answer_support
    return {
        "outcome": case.answer_outcome,
        "answered": case.answer_outcome == "answered",
        "has_text": case.answer_text is not None,
        "string_check": string_check,
        "support": support,
        "support_reviewer": review.reviewer if review is not None and support != NOT_EVALUATED else None,
    }


def _abstention_block(
    question: GoldQuestion,
    case: RunCase,
    candidate_eval: QuestionEvaluation,
    review: ReviewVerdict | None,
) -> dict[str, Any]:
    answer_abstained = case.answer_outcome == "abstained"
    structural: bool | None
    if question.abstain_expected:
        structural = answer_abstained
    else:
        structural = None
    correct: bool | str = NOT_EVALUATED
    if review is not None and review.abstention_correct is not None:
        correct = review.abstention_correct
    return {
        "expected": question.abstain_expected,
        "retrieval_empty": candidate_eval.retrieval_empty,
        "answer_abstained": answer_abstained,
        "answer_outcome": case.answer_outcome,
        "structural_match": structural,
        "abstained_on_positive": bool(question.positive and answer_abstained),
        "answered_on_expected_abstain": bool(question.abstain_expected and case.answer_outcome == "answered"),
        "correct": correct,
        "correct_reviewer": review.reviewer if review is not None and correct != NOT_EVALUATED else None,
    }


def _tools_block(case: RunCase) -> dict[str, Any]:
    outcomes = Counter(tool.outcome for tool in case.tools)
    called = [tool for tool in case.tools if tool.outcome != "not_called"]
    success = sum(1 for tool in called if tool.outcome == "success")
    non_success = {name: count for name, count in outcomes.items() if name not in {"success", "not_called"}}
    return {
        "tool_count": len(case.tools),
        "called_count": len(called),
        "success_count": success,
        "non_success": non_success,
        "all_called_succeeded": (success == len(called)) if called else None,
        "any_unknown_outcome": outcomes.get("unknown_outcome", 0) > 0,
        "names": sorted({tool.name for tool in case.tools}),
    }


def evaluate_case(
    gold: GoldSet,
    question: GoldQuestion,
    candidate_eval: QuestionEvaluation,
    case: RunCase | None,
    *,
    review: ReviewVerdict | None = None,
    observation_times: Mapping[str, str] | None = None,
    expected_answer_strings: Sequence[str] | None = None,
) -> CaseResult:
    present = case is not None
    if case is None:
        case = RunCase(question_id=question.question_id, candidate_observation_ids=(), label="missing")
    candidate = {
        "retrieved_count": len(candidate_eval.retrieved_observation_ids),
        "required_group_count": candidate_eval.required_group_count,
        "recovered_group_count": candidate_eval.recovered_group_count,
        "evidence_recall": candidate_eval.evidence_recall,
        "complete_group_recovery": candidate_eval.complete_group_recovery,
        "forbidden_hit_ids": list(candidate_eval.forbidden_observation_ids),
        "forbidden_hit_count": candidate_eval.forbidden_hit_count,
        "out_of_scope_ids": list(candidate_eval.out_of_scope_observation_ids),
        "out_of_scope_hit_count": candidate_eval.out_of_scope_hit_count,
        "retrieval_empty": candidate_eval.retrieval_empty,
    }
    visible = _visible_block(gold, question, case, candidate_eval)
    time_block = _time_block(
        gold,
        case,
        candidate_eval.retrieved_observation_ids,
        visible["visible_complete_observation_ids"] + visible["visible_partial_observation_ids"],
        observation_times,
    )
    return CaseResult(
        question_id=question.question_id,
        split=question.split,
        positive=question.positive,
        abstain_expected=question.abstain_expected,
        primary_room=_primary_room(gold, question),
        case_present=present,
        label=case.label,
        candidate=candidate,
        visible=visible,
        person=_person_block(case, review),
        time=time_block,
        answer=_answer_block(question, case, review, expected_answer_strings),
        abstention=_abstention_block(question, case, candidate_eval, review),
        tools=_tools_block(case),
    )


# --------------------------------------------------------------------------- report


def _rate(numerator: int, denominator: int) -> float | None:
    return (numerator / denominator) if denominator else None


def _aggregate(results: Sequence[CaseResult]) -> dict[str, Any]:
    n = len(results)
    positives = [r for r in results if r.positive]
    abstains = [r for r in results if r.abstain_expected]
    present = [r for r in results if r.case_present]
    cand_complete = sum(1 for r in positives if r.candidate["complete_group_recovery"])
    vis_complete = sum(1 for r in positives if r.visible["visible_complete_group_recovery"])
    required_groups = sum(r.candidate["required_group_count"] for r in positives)
    cand_groups = sum(r.candidate["recovered_group_count"] for r in positives)
    vis_groups = sum(r.visible["visible_complete_group_count"] for r in positives)
    partial_only_groups = sum(r.visible["visible_partial_only_group_count"] for r in positives)
    summary_only_groups = sum(r.visible["summary_only_group_count"] for r in positives)
    truncated_cases = sum(1 for r in positives if r.visible["truncated_candidate_ids"])
    candidate_only_gap = cand_complete - vis_complete

    forbidden_candidate_cases = sum(1 for r in results if r.candidate["forbidden_hit_count"])
    forbidden_visible_cases = sum(1 for r in results if r.visible["forbidden_visible_count"])
    scope_candidate_cases = sum(1 for r in results if r.candidate["out_of_scope_hit_count"])
    scope_visible_cases = sum(1 for r in results if r.visible["out_of_scope_visible_count"])
    person_violation_cases = sum(1 for r in results if r.person["scope_status"] == "violation")
    person_no_scope_cases = sum(1 for r in results if r.person["scope_status"] == "no_scope_provided")
    time_violation_cases = sum(1 for r in results if r.time["status"] == "violation")
    time_undetermined_cases = sum(1 for r in results if r.time["status"] == "undetermined")
    duplicate_spans = sum(r.visible["duplicate_raw_span_count"] for r in results)

    support_counter = Counter(r.answer["support"] for r in results)
    attribution_counter = Counter(r.person["attribution"] for r in results)
    abstain_correct_counter = Counter(str(r.abstention["correct"]) for r in results)
    answer_outcomes = Counter(r.answer["outcome"] for r in results)
    string_checks = Counter(r.answer["string_check"]["status"] for r in results)

    abstain_structural = sum(1 for r in abstains if r.abstention["structural_match"])
    abstain_retrieval_empty = sum(1 for r in abstains if r.abstention["retrieval_empty"])
    abstained_on_positive = sum(1 for r in positives if r.abstention["abstained_on_positive"])
    answered_on_abstain = sum(1 for r in abstains if r.abstention["answered_on_expected_abstain"])

    tool_cases = [r for r in results if r.tools["called_count"]]
    tool_all_success = sum(1 for r in tool_cases if r.tools["all_called_succeeded"])
    tool_non_success: Counter[str] = Counter()
    for r in results:
        tool_non_success.update(r.tools["non_success"])

    reviewed_semantic = sum(
        1
        for r in results
        if r.answer["support"] != NOT_EVALUATED
        or r.person["attribution"] != NOT_EVALUATED
        or r.abstention["correct"] != NOT_EVALUATED
    )
    return {
        "question_count": n,
        "case_present_count": len(present),
        "positive_question_count": len(positives),
        "abstain_expected_count": len(abstains),
        "candidate": {
            "required_group_count": required_groups,
            "recovered_group_count": cand_groups,
            "evidence_recall": _rate(cand_groups, required_groups),
            "complete_group_recovery_count": cand_complete,
            "complete_group_recovery_rate": _rate(cand_complete, len(positives)),
            "forbidden_case_count": forbidden_candidate_cases,
            "forbidden_case_rate": _rate(forbidden_candidate_cases, n),
            "out_of_scope_case_count": scope_candidate_cases,
            "out_of_scope_case_rate": _rate(scope_candidate_cases, n),
            "positive_empty_retrieval_count": sum(1 for r in positives if r.candidate["retrieval_empty"]),
        },
        "visible": {
            "required_group_count": required_groups,
            "visible_complete_group_count": vis_groups,
            "visible_evidence_recall": _rate(vis_groups, required_groups),
            "visible_complete_group_recovery_count": vis_complete,
            "visible_complete_group_recovery_rate": _rate(vis_complete, len(positives)),
            "candidate_hit_but_not_visible_case_count": candidate_only_gap,
            "partial_only_group_count": partial_only_groups,
            "summary_only_group_count": summary_only_groups,
            "truncated_candidate_case_count": truncated_cases,
            "duplicate_raw_span_count": duplicate_spans,
            "forbidden_visible_case_count": forbidden_visible_cases,
            "forbidden_visible_case_rate": _rate(forbidden_visible_cases, n),
            "out_of_scope_visible_case_count": scope_visible_cases,
            "out_of_scope_visible_case_rate": _rate(scope_visible_cases, n),
        },
        "person": {
            "scope_violation_case_count": person_violation_cases,
            "no_scope_provided_case_count": person_no_scope_cases,
            "attribution": dict(attribution_counter),
        },
        "time": {
            "violation_case_count": time_violation_cases,
            "undetermined_case_count": time_undetermined_cases,
            "status_counts": dict(Counter(r.time["status"] for r in results)),
        },
        "answer": {
            "outcomes": dict(answer_outcomes),
            "string_check": dict(string_checks),
            "support": dict(support_counter),
        },
        "abstention": {
            "expected_count": len(abstains),
            "answer_abstained_on_expected_count": abstain_structural,
            "retrieval_empty_on_expected_count": abstain_retrieval_empty,
            "answered_on_expected_abstain_count": answered_on_abstain,
            "abstained_on_positive_count": abstained_on_positive,
            "correct": dict(abstain_correct_counter),
        },
        "tools": {
            "cases_with_tool_calls": len(tool_cases),
            "cases_all_tools_succeeded": tool_all_success,
            "non_success_outcomes": dict(tool_non_success),
        },
        "human_review": {
            "semantic_reviewed_case_count": reviewed_semantic,
            "semantic_review_coverage": _rate(reviewed_semantic, n),
        },
    }


@dataclass(frozen=True, slots=True)
class GateReport:
    dataset_id: str
    label: str
    k: int
    overall: dict[str, Any]
    by_split: dict[str, dict[str, Any]]
    by_room: dict[str, dict[str, Any]]
    qualification: dict[str, Any]
    cases: tuple[CaseResult, ...]
    limitations: tuple[str, ...]

    def as_dict(self, *, include_cases: bool = True) -> dict[str, Any]:
        out = {
            "schema_version": 1,
            "adapter": "rag_upgrade.quality_gate",
            "dataset_id": self.dataset_id,
            "label": self.label,
            "k": self.k,
            "k_unit": "independent_observations",
            "overall": self.overall,
            "by_split": self.by_split,
            "by_room": self.by_room,
            "qualification": self.qualification,
            "limitations": list(self.limitations),
        }
        if include_cases:
            out["cases"] = [case.as_dict() for case in self.cases]
        return out

    def public_summary(self) -> dict[str, Any]:
        """Aggregates only; no observation IDs, question text or bodies."""

        return self.as_dict(include_cases=False)


LIMITATIONS = (
    "Candidate hit and visible raw evidence are separate; an observation ID in the candidate pool does not count as visible.",
    "Visible evidence requires the whole gold observation body to be covered by raw spans; partial coverage and summary-only coverage are reported separately and never count as complete.",
    "answer.support, person.attribution and abstention.correct come only from human review records; without a reviewer they stay not_evaluated.",
    "answer.string_check is a string containment check, not semantic support.",
    "abstention.structural_match means the answer abstained when gold expects abstention; it is not evidence that abstaining was correct, and an empty retrieval is not an abstention.",
    "Tool success requires outcome=success; error/unconfigured/disabled/needs_input/unknown_outcome are non-success.",
    "gate_decision is always not_decided; qualification eligibility comes from gold review status and cannot be set by this adapter.",
)


def evaluate_run(
    gold: GoldSet | Mapping[str, Any] | str | Path,
    cases: Mapping[str, RunCase] | Sequence[Mapping[str, Any]] | str | Path,
    *,
    k: int = 10,
    reviews: Mapping[str, ReviewVerdict] | Sequence[Mapping[str, Any]] | str | Path | None = None,
    observation_times: Mapping[str, str] | None = None,
    expected_answer_strings: Mapping[str, Sequence[str]] | None = None,
    label: str | None = None,
    require_all_cases: bool = False,
) -> GateReport:
    gold_set = load_gold(gold)
    if not isinstance(cases, Mapping) or (cases and not all(isinstance(v, RunCase) for v in cases.values())):
        cases = load_run_cases(cases)  # type: ignore[arg-type]
    case_map: dict[str, RunCase] = dict(cases)  # type: ignore[arg-type]
    known_questions = {q.question_id for q in gold_set.questions}
    unknown = set(case_map) - known_questions
    if unknown:
        raise TraceContractError("run cases reference unknown question IDs: " + ", ".join(sorted(unknown)))
    missing = known_questions - set(case_map)
    if missing and require_all_cases:
        raise TraceContractError("run cases missing question IDs: " + ", ".join(sorted(missing)))

    review_map: dict[str, ReviewVerdict]
    if reviews is None:
        review_map = {}
    elif isinstance(reviews, Mapping) and all(isinstance(v, ReviewVerdict) for v in reviews.values()):
        review_map = dict(reviews)  # type: ignore[arg-type]
    else:
        review_map = load_reviews(reviews)  # type: ignore[arg-type]
    unknown_reviews = set(review_map) - known_questions
    if unknown_reviews:
        raise ReviewContractError("reviews reference unknown question IDs: " + ", ".join(sorted(unknown_reviews)))

    retrieved = {
        q.question_id: list(case_map[q.question_id].candidate_observation_ids) if q.question_id in case_map else []
        for q in gold_set.questions
    }
    base = evaluate_retrieval(gold_set, retrieved, k=k)
    base_by_q = {qe.question_id: qe for qe in base.question_reports}

    results: list[CaseResult] = []
    labels: set[str] = set()
    for question in gold_set.questions:
        case = case_map.get(question.question_id)
        if case is not None:
            labels.add(case.label)
        results.append(
            evaluate_case(
                gold_set,
                question,
                base_by_q[question.question_id],
                case,
                review=review_map.get(question.question_id),
                observation_times=observation_times,
                expected_answer_strings=(expected_answer_strings or {}).get(question.question_id),
            )
        )
    if label is None:
        label = labels.pop() if len(labels) == 1 else ("mixed" if labels else "missing")

    by_split: dict[str, dict[str, Any]] = {}
    for split in sorted({r.split for r in results}):
        by_split[split] = _aggregate([r for r in results if r.split == split])
    by_room: dict[str, dict[str, Any]] = {}
    for room in sorted({r.primary_room for r in results}):
        by_room[room] = _aggregate([r for r in results if r.primary_room == room])

    reviewed_all = all(
        r.answer["support"] != NOT_EVALUATED or r.abstention["correct"] != NOT_EVALUATED for r in results
    )
    qualification = {
        "gold_status": gold_set.qualification_status,
        "gold_eligible": gold_set.qualification_eligible,
        "semantic_review_complete": bool(results) and reviewed_all,
        "gate_decision": "not_decided",
        "reason": "Structural adapter output; human reviewer decides model quality and gold promotion.",
    }
    return GateReport(
        dataset_id=gold_set.dataset_id,
        label=label,
        k=k,
        overall=_aggregate(results),
        by_split=by_split,
        by_room=by_room,
        qualification=qualification,
        cases=tuple(results),
        limitations=LIMITATIONS,
    )


# --------------------------------------------------------------------------- oracle


def oracle_cases(gold: GoldSet | Mapping[str, Any] | str | Path, *, label: str = "oracle") -> dict[str, RunCase]:
    """Cases with gold evidence injected directly (all required groups fully visible).

    This is the structural upper bound for comparing against a real retrieval
    run.  The oracle never carries an answer, so its semantic fields stay
    ``not_evaluated``; it says nothing about model quality.
    """

    gold_set = load_gold(gold)
    out: dict[str, RunCase] = {}
    for question in gold_set.questions:
        ids: list[str] = []
        spans: list[VisibleSpan] = []
        for group in question.required_evidence_groups:
            for oid in group.alternatives:
                ids.append(oid)
                body_length = len(gold_set.observations[oid].text)
                if body_length:
                    spans.append(VisibleSpan(observation_id=oid, start=0, end=body_length, source="raw", complete=True))
        out[question.question_id] = RunCase(
            question_id=question.question_id,
            candidate_observation_ids=tuple(ids),
            visible_spans=tuple(spans),
            answer_outcome="not_run",
            label=label,
        )
    return out


_COMPARE_KEYS = (
    ("candidate", "complete_group_recovery_rate"),
    ("candidate", "evidence_recall"),
    ("visible", "visible_complete_group_recovery_rate"),
    ("visible", "visible_evidence_recall"),
    ("candidate", "forbidden_case_rate"),
    ("visible", "forbidden_visible_case_rate"),
)


def compare_runs(
    gold: GoldSet | Mapping[str, Any] | str | Path,
    actual: Mapping[str, RunCase] | Sequence[Mapping[str, Any]] | str | Path,
    reference: Mapping[str, RunCase] | Sequence[Mapping[str, Any]] | str | Path | None = None,
    *,
    k: int = 10,
    observation_times: Mapping[str, str] | None = None,
) -> dict[str, Any]:
    """Structural side-by-side of an actual run and a reference (default: oracle)."""

    gold_set = load_gold(gold)
    actual_report = evaluate_run(gold_set, actual, k=k, observation_times=observation_times)
    if reference is None:
        reference_cases: Mapping[str, RunCase] | Sequence[Mapping[str, Any]] | str | Path = oracle_cases(gold_set)
    else:
        reference_cases = reference
    reference_report = evaluate_run(gold_set, reference_cases, k=k, observation_times=observation_times)
    delta: dict[str, float | None] = {}
    for section, key in _COMPARE_KEYS:
        a = actual_report.overall[section][key]
        b = reference_report.overall[section][key]
        delta[f"{section}.{key}"] = (a - b) if (a is not None and b is not None) else None
    return {
        "schema_version": 1,
        "dataset_id": gold_set.dataset_id,
        "k": k,
        "actual": dict(actual_report.overall),
        "reference": dict(reference_report.overall),
        "delta_actual_minus_reference": delta,
        "note": "Structural comparison only (candidate/visible/forbidden). No semantic or model-quality claim.",
    }


# --------------------------------------------------------------------------- binding


def body_digest(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def verify_gold_corpus_binding(
    gold: GoldSet | Mapping[str, Any] | str | Path,
    messages: Iterable[Mapping[str, Any]],
    *,
    strict: bool = True,
) -> dict[str, Any]:
    """Check every gold observation against the current corpus by ID *and* body.

    The gold is message-level, so it survives a different leaf split; what it
    must not survive is a body/room change behind an unchanged ID.  ``messages``
    are ``{observation_id, content|text, source_room_id|source_channel_id}``.
    """

    gold_set = load_gold(gold)
    current: dict[str, tuple[str, str]] = {}
    for message in messages:
        oid = message.get("observation_id")
        if not isinstance(oid, str) or not oid:
            continue
        text = message.get("content", message.get("text"))
        room = message.get("source_room_id", message.get("source_channel_id"))
        current[oid] = (body_digest(text if isinstance(text, str) else ""), str(room or ""))
    missing: list[str] = []
    body_changed: list[str] = []
    room_changed: list[str] = []
    for oid, observation in gold_set.observations.items():
        found = current.get(oid)
        if found is None:
            missing.append(oid)
            continue
        if found[0] != body_digest(observation.text):
            body_changed.append(oid)
        if found[1] != observation.source_channel_id:
            room_changed.append(oid)
    ok = not (missing or body_changed or room_changed)
    report = {
        "schema_version": 1,
        "dataset_id": gold_set.dataset_id,
        "gold_observation_count": len(gold_set.observations),
        "corpus_message_count": len(current),
        "missing_count": len(missing),
        "body_changed_count": len(body_changed),
        "room_changed_count": len(room_changed),
        "missing_ids": sorted(missing),
        "body_changed_ids": sorted(body_changed),
        "room_changed_ids": sorted(room_changed),
        "bound": ok,
    }
    if strict and not ok:
        raise StaleGoldError(
            f"gold {gold_set.dataset_id} is stale against the current corpus: "
            f"missing={len(missing)} body_changed={len(body_changed)} room_changed={len(room_changed)}"
        )
    return report


def check_index_binding(gold: GoldSet | Mapping[str, Any] | str | Path, binding: Mapping[str, Any]) -> dict[str, Any]:
    """Reuse index_binding's fingerprint check on the gold's metadata; never bypass it."""

    gold_set = load_gold(gold)
    payload = {
        "index_binding": gold_set.metadata.get("index_binding"),
        "corpus_binding": gold_set.metadata.get("corpus_binding"),
    }
    try:
        assert_gold_bound_to_index(payload, binding)
    except BindingError as exc:
        return {"bound": False, "reason": str(exc)}
    return {"bound": True, "reason": None}


# --------------------------------------------------------------------------- promotion


def apply_reviews(
    gold_raw: Mapping[str, Any],
    reviews: Mapping[str, ReviewVerdict] | Sequence[Mapping[str, Any]] | str | Path,
    *,
    require_complete: bool = True,
) -> dict[str, Any]:
    """Return a copy of gold with per-question review fields filled from real verdicts.

    Only questions with a reviewer and ``gold_verdict=accept`` become reviewed.
    The gold-level kind becomes ``human_reviewed`` only when *every* question is
    reviewed; with ``require_complete`` (default) an incomplete review set is
    refused instead of silently producing a partially promoted gold.
    """

    load_gold(gold_raw)  # validate the input contract first
    if isinstance(reviews, Mapping) and all(isinstance(v, ReviewVerdict) for v in reviews.values()):
        review_map: dict[str, ReviewVerdict] = dict(reviews)  # type: ignore[arg-type]
    else:
        review_map = load_reviews(reviews)  # type: ignore[arg-type]
    questions_raw = gold_raw.get("questions", gold_raw.get("gold_cases"))
    out_questions: list[dict[str, Any]] = []
    reviewers: set[str] = set()
    unreviewed: list[str] = []
    rejected: list[str] = []
    for item in questions_raw:  # type: ignore[union-attr]
        item = dict(item)
        qid = item["question_id"]
        verdict = review_map.get(qid)
        if verdict is None or verdict.gold_verdict is None:
            unreviewed.append(qid)
        elif verdict.gold_verdict != "accept":
            rejected.append(qid)
            item["review_status"] = "unreviewed"
            item["reviewer"] = None
            item["review_verdict"] = verdict.gold_verdict
        else:
            item["review_status"] = "reviewed"
            item["reviewer"] = verdict.reviewer
            item["review_verdict"] = "accept"
            reviewers.add(verdict.reviewer)
        out_questions.append(item)
    incomplete = unreviewed + rejected
    if incomplete and require_complete:
        raise PromotionRefusedError(
            f"promotion refused: {len(unreviewed)} unreviewed and {len(rejected)} rejected questions remain"
        )
    out = dict(gold_raw)
    out["questions"] = out_questions
    out.pop("gold_cases", None)
    if incomplete:
        out["gold_kind"] = "synthetic" if gold_raw.get("gold_kind") == "synthetic" else gold_raw.get("gold_kind")
        out["review_status"] = "unreviewed"
        out["reviewer"] = None
    else:
        out["gold_kind"] = "human_reviewed"
        out["review_status"] = "reviewed"
        out["reviewer"] = ",".join(sorted(reviewers))
    out["review_summary"] = {
        "reviewed_count": len(out_questions) - len(incomplete),
        "unreviewed_count": len(unreviewed),
        "rejected_count": len(rejected),
    }
    load_gold(out)  # the result must still satisfy the contract (incl. split leakage)
    return out


# --------------------------------------------------------------------------- CLI


def _load_json(path: Path) -> Any:
    return json.loads(path.read_text(encoding="utf-8"))


def _write_private(path: Path, payload: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    try:
        os.chmod(path.parent, 0o700)
    except OSError:
        pass
    path.write_text(json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    try:
        os.chmod(path, 0o600)
    except OSError:
        pass


def main(argv: Sequence[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="Offline quality-gate adapter (structural; no model-quality decision).")
    ap.add_argument("--gold", type=Path, required=True, help="message-level gold JSON (evidence_metrics schema 1)")
    ap.add_argument("--run", type=Path, help="run cases JSON from the runtime trace")
    ap.add_argument("--reviews", type=Path, help="human review verdicts JSON (optional)")
    ap.add_argument("--observation-times", type=Path, help="JSON mapping observation_id -> ISO time (optional)")
    ap.add_argument("--corpus-messages", type=Path, help="messages JSON list to verify gold bodies against (optional)")
    ap.add_argument("--oracle", action="store_true", help="also evaluate gold-injected oracle cases and compare")
    ap.add_argument("--k", type=int, default=10)
    ap.add_argument("--out", type=Path, help="directory for per-question output (private); stdout gets aggregates only")
    args = ap.parse_args(argv)

    gold_set = load_gold(args.gold)
    times = _load_json(args.observation_times) if args.observation_times else None
    if args.corpus_messages:
        binding = verify_gold_corpus_binding(gold_set, _load_json(args.corpus_messages), strict=True)
        print(json.dumps({"corpus_binding": {k: v for k, v in binding.items() if not k.endswith("_ids")}}, ensure_ascii=False))
    outputs: dict[str, Any] = {}
    if args.run:
        report = evaluate_run(gold_set, args.run, k=args.k, reviews=args.reviews, observation_times=times)
        outputs["actual"] = report
        print(json.dumps(report.public_summary(), ensure_ascii=False, sort_keys=True))
    if args.oracle:
        oracle_report = evaluate_run(gold_set, oracle_cases(gold_set), k=args.k, observation_times=times)
        outputs["oracle"] = oracle_report
        if args.run:
            comparison = compare_runs(gold_set, args.run, k=args.k, observation_times=times)
            print(json.dumps({"delta_actual_minus_oracle": comparison["delta_actual_minus_reference"]}, ensure_ascii=False))
        else:
            print(json.dumps(oracle_report.public_summary(), ensure_ascii=False, sort_keys=True))
    if args.out:
        for name, report in outputs.items():
            _write_private(args.out / f"{name}.report.json", report.as_dict(include_cases=True))
            _write_private(args.out / f"{name}.summary.json", report.public_summary())
    return 0


__all__ = [
    "ANSWER_OUTCOMES",
    "CaseResult",
    "GateReport",
    "NOT_EVALUATED",
    "PromotionRefusedError",
    "ReviewContractError",
    "ReviewVerdict",
    "RunCase",
    "StaleGoldError",
    "TOOL_OUTCOMES",
    "ToolOutcome",
    "TraceContractError",
    "VisibleSpan",
    "apply_reviews",
    "check_index_binding",
    "compare_runs",
    "evaluate_case",
    "evaluate_run",
    "load_reviews",
    "load_run_cases",
    "merge_intervals",
    "oracle_cases",
    "verify_gold_corpus_binding",
    "visibility_by_observation",
]


if __name__ == "__main__":
    raise SystemExit(main())
