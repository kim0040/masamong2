"""Extended gold schema for instruction §6 (builds on evidence_metrics ideas)."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Mapping, Sequence

from . import MEMORY_REQUIREMENTS

ALLOWED_SPLITS = frozenset({"train", "dev", "holdout"})
QUESTION_TYPES = frozenset(
    {
        "person_factual",
        "person_description",
        "episode_recall",
        "temporal",
        "recent_memory",
        "old_memory",
        "multiple_evidence",
        "nickname_alias",
        "typo",
        "kakao_discord_mixed",
        "no_answer",
        "ambiguous_person",
        "same_name_users",
        "optional_memory",
        "tool_plus_memory",
        "tool_only",
        "memory_only",
        "person_fact",
        "person_intro",
        "event",
        "recent",
        "casual",
        "off_domain",
    }
)


class GoldSchemaError(ValueError):
    """Gold mapping violates the upgrade contract."""


@dataclass(frozen=True)
class GoldQuestion:
    question_id: str
    query: str
    split: str
    expected_memory_requirement: str
    expected_abstention: bool
    expected_evidence_ids: tuple[str, ...] = ()
    acceptable_evidence_ids: tuple[str, ...] = ()
    forbidden_evidence_ids: tuple[str, ...] = ()
    expected_person_ids: tuple[str, ...] = ()
    expected_tools: tuple[str, ...] = ()
    snapshot_cutoff: str | None = None
    types: tuple[str, ...] = ()
    review_status: str = "unreviewed"
    question_type: str = ""
    expected_time_scope: str = ""
    reviewed_by: str = ""
    review_verdict: str = ""
    review_note: str = ""

    def positive_ids(self) -> frozenset[str]:
        return frozenset(self.expected_evidence_ids) | frozenset(
            self.acceptable_evidence_ids
        )


@dataclass(frozen=True)
class GoldDataset:
    schema_version: int
    dataset_id: str
    gold_kind: str
    questions: tuple[GoldQuestion, ...]
    review_status: str = "unreviewed"


def parse_gold(raw: Mapping[str, Any]) -> GoldDataset:
    if int(raw.get("schema_version", 0)) < 2:
        raise GoldSchemaError("upgrade gold requires schema_version >= 2")
    dataset_id = str(raw.get("dataset_id") or "").strip()
    if not dataset_id:
        raise GoldSchemaError("dataset_id required")
    gold_kind = str(raw.get("gold_kind") or "")
    if gold_kind not in {"synthetic", "human_reviewed"}:
        raise GoldSchemaError("gold_kind must be synthetic|human_reviewed")
    questions_raw = raw.get("questions")
    if not isinstance(questions_raw, list) or not questions_raw:
        raise GoldSchemaError("questions must be a non-empty list")
    seen: set[str] = set()
    questions: list[GoldQuestion] = []
    for item in questions_raw:
        q = _parse_question(item)
        if q.question_id in seen:
            raise GoldSchemaError(f"duplicate question_id {q.question_id}")
        seen.add(q.question_id)
        questions.append(q)
    return GoldDataset(
        schema_version=int(raw["schema_version"]),
        dataset_id=dataset_id,
        gold_kind=gold_kind,
        review_status=str(raw.get("review_status") or "unreviewed"),
        questions=tuple(questions),
    )


def _parse_question(item: Any) -> GoldQuestion:
    if not isinstance(item, Mapping):
        raise GoldSchemaError("question must be an object")
    qid = str(item.get("question_id") or "").strip()
    query = str(item.get("query") or item.get("question") or "").strip()
    split = str(item.get("split") or "").strip()
    mem = str(item.get("expected_memory_requirement") or "").strip()
    if not qid or not query:
        raise GoldSchemaError("question_id and query required")
    if split not in ALLOWED_SPLITS:
        raise GoldSchemaError(f"bad split {split}")
    if mem not in MEMORY_REQUIREMENTS:
        raise GoldSchemaError(f"bad memory_requirement {mem}")
    abstain = bool(item.get("expected_abstention", False))
    expected = _str_tuple(item.get("expected_evidence_ids"))
    acceptable = _str_tuple(item.get("acceptable_evidence_ids"))
    forbidden = _str_tuple(item.get("forbidden_evidence_ids"))
    if not abstain and not expected and not acceptable:
        raise GoldSchemaError(f"{qid}: non-abstain needs positive evidence ids")
    overlap = (set(expected) | set(acceptable)) & set(forbidden)
    if overlap:
        raise GoldSchemaError(f"{qid}: evidence id both positive and forbidden")
    types = _str_tuple(item.get("types"))
    qtype = str(item.get("question_type") or "").strip()
    if qtype:
        types = tuple(dict.fromkeys([*types, qtype]))
    unknown = set(types) - QUESTION_TYPES
    if unknown:
        raise GoldSchemaError(f"{qid}: unknown types {sorted(unknown)}")
    verdict = str(item.get("review_verdict") or "").strip()
    if verdict and verdict not in {"accept", "reject", "fix", ""}:
        raise GoldSchemaError(f"{qid}: bad review_verdict {verdict}")
    return GoldQuestion(
        question_id=qid,
        query=query,
        split=split,
        expected_memory_requirement=mem,
        expected_abstention=abstain,
        expected_evidence_ids=expected,
        acceptable_evidence_ids=acceptable,
        forbidden_evidence_ids=forbidden,
        expected_person_ids=_str_tuple(item.get("expected_person_ids")),
        expected_tools=_str_tuple(item.get("expected_tools")),
        snapshot_cutoff=(
            str(item["snapshot_cutoff"]) if item.get("snapshot_cutoff") else None
        ),
        types=types,
        review_status=str(item.get("review_status") or "unreviewed"),
        question_type=qtype,
        expected_time_scope=str(item.get("expected_time_scope") or "").strip(),
        reviewed_by=str(item.get("reviewed_by") or "").strip(),
        review_verdict=verdict,
        review_note=str(item.get("review_note") or "").strip(),
    )


def _str_tuple(value: Any) -> tuple[str, ...]:
    if value is None:
        return ()
    if not isinstance(value, Sequence) or isinstance(value, (str, bytes)):
        raise GoldSchemaError("id lists must be arrays")
    out: list[str] = []
    for item in value:
        s = str(item).strip()
        if not s:
            raise GoldSchemaError("empty id in list")
        out.append(s)
    return tuple(out)


def score_retrieval(
    question: GoldQuestion, ranked_ids: Sequence[str], *, k: int
) -> dict[str, Any]:
    """Evidence metrics at cutoff k (observation ids, deduped preserving order)."""

    if k < 1:
        raise ValueError("k must be >= 1")
    seen: list[str] = []
    for oid in ranked_ids:
        if oid not in seen:
            seen.append(oid)
        if len(seen) >= k:
            break
    top = seen[:k]
    top_set = set(top)
    forbidden_hit = sorted(top_set & set(question.forbidden_evidence_ids))
    positives = question.positive_ids()
    if question.expected_abstention:
        recall = 1.0 if not positives else float(bool(positives & top_set))
        # abstain questions: success if no forbidden and ideally empty/no forced memory
        ok = not forbidden_hit
    else:
        hit = bool(positives & top_set)
        recall = 1.0 if hit else 0.0
        ok = hit and not forbidden_hit
    rr = 0.0
    for i, oid in enumerate(top, start=1):
        if oid in positives:
            rr = 1.0 / i
            break
    return {
        "k": k,
        "recall": recall,
        "mrr_contrib": rr,
        "forbidden_hit": forbidden_hit,
        "ok": ok,
        "top": top,
    }
