"""raw-corpus 로더와 record_kind 분류기.

.private/raw-corpus/의 masamong-raw-observation/v1 관찰을 읽는다.
원본은 읽기만 하며 수정하지 않는다. 본문은 호출자에게만 반환하고 로그에 쓰지 않는다.
"""
from __future__ import annotations

import gzip
import json
import os
import re
import unicodedata
from dataclasses import dataclass, field
from pathlib import Path
from typing import Iterator

CORPUS_ROOT = Path(os.environ.get("RAW_CORPUS_ROOT", ".private/raw-corpus"))

def collection_names(root: Path | None = None) -> tuple[str, ...]:
    """Return collections that are present in the configured private corpus."""

    base = Path(root or CORPUS_ROOT)
    if not base.is_dir():
        return ()
    return tuple(
        sorted(
            entry.name
            for entry in base.iterdir()
            if entry.is_dir()
            and ((entry / "dated.jsonl.gz").is_file() or (entry / "undated.jsonl.gz").is_file())
        )
    )

# --- record_kind 분류 -------------------------------------------------------
# 표지는 원문에 그대로 보존하되 turn/window/embedding/claim의 의미 입력에서 제외한다.

_DELETE_PATTERNS = [
    r"^메시지가 삭제되었습니다\.?$",
    r"^삭제된 메시지입니다\.?$",
    r"^삭제된 메시지 입니다\.?$",
    r"^이 메시지는 삭제되었습니다\.?$",
]

_MEDIA_PATTERNS = [
    r"^사진$", r"^사진 \d+장$", r"^동영상$", r"^음성메시지$", r"^이모티콘$",
    r"^샵검색: .*$", r"^파일: .*$", r"^지도: .*$", r"^연락처: .*$",
    r"^\[첨부 \d+개\]$", r"^선물하기: .*$", r"^송금$", r"^캘린더: .*$",
    r"^투표: .*$", r"^라이브톡.*$", r"^보이스룸.*$",
]

_SYSTEM_PATTERNS = [
    r"님이 들어왔습니다\.?$",
    r"님이 나갔습니다\.?$",
    r"님을 내보냈습니다\.?$",
    r"님이 .*님을 초대했습니다\.?$",
    r"님이 .*님과 .*님을 초대했습니다\.?$",
    r"님이 방장이 되었습니다\.?$",
    r"^채팅방 관리자가 .*$",
    r"님이 채팅방 이름을 변경하였습니다\.?$",
    r"^저장한 날짜 : .*$",
    r"^.*님과 카카오톡 대화$",
    r"운영정책을 위반한 메시지로 신고 접수되어 가려진 메시지입니다\.?$",
]

_DELETE_RE = re.compile("|".join(_DELETE_PATTERNS))
_MEDIA_RE = re.compile("|".join(_MEDIA_PATTERNS))
_SYSTEM_RE = re.compile("|".join(_SYSTEM_PATTERNS))

HUMAN_TEXT = "human_text"
SYSTEM_MARKER = "system_marker"
DELETE_MARKER = "delete_marker"
MEDIA_MARKER = "media_marker"
BOT_RECORD = "bot_record"
EMPTY_UNKNOWN = "empty_unknown"

#: 의미 파이프라인(window/embedding/claim)의 기본 입력으로 허용하는 kind
SEMANTIC_ELIGIBLE = frozenset({HUMAN_TEXT})


def classify(content: str | None, *, is_bot: int | None, prior_kind: str | None) -> tuple[str, str]:
    """(record_kind, 분류 근거)를 반환한다."""
    if prior_kind == "bot_record" or is_bot == 1:
        return BOT_RECORD, "source_bot_flag"
    text = (content or "").strip()
    if not text:
        return EMPTY_UNKNOWN, "empty_content"
    if _DELETE_RE.search(text):
        return DELETE_MARKER, "delete_pattern"
    if _MEDIA_RE.search(text):
        return MEDIA_MARKER, "media_pattern"
    if _SYSTEM_RE.search(text):
        return SYSTEM_MARKER, "system_pattern"
    return HUMAN_TEXT, "default_human_text"


# --- 정규화 -----------------------------------------------------------------

_WS_RE = re.compile(r"\s+")


def normalize(text: str) -> str:
    """검색용 정규화. 원문을 덮어쓰지 않고 파생 필드로만 쓴다.

    NFKC + 공백 정리만 한다. 조사 분리나 형태소 분석은 tokenizer 계층의 책임이다.
    """
    return _WS_RE.sub(" ", unicodedata.normalize("NFKC", text)).strip()


# --- 관찰 -------------------------------------------------------------------


@dataclass(slots=True)
class Observation:
    observation_id: str
    platform: str
    collection_id: str
    source_room_id: str
    native_message_id: str | None
    actor_label: str | None
    actor_account: str | None
    content: str
    parsed_time: str | None
    record_kind: str
    kind_reason: str
    ordinal: int
    is_bot: int | None
    # ``ordinal`` is the position in the merged JSONL stream.  These fields
    # preserve the adapter's original source provenance for window/cache
    # fingerprints; they are optional for synthetic observations.
    source_ordinal: str | int | None = None
    source_fingerprint: str | None = None
    source_file: str | None = None
    source_kind: str | None = None

    @property
    def semantic_eligible(self) -> bool:
        return self.record_kind in SEMANTIC_ELIGIBLE


def _to_observation(raw: dict, ordinal: int) -> Observation:
    actor = raw.get("actor") or {}
    time = raw.get("time") or {}
    source = raw.get("source") or {}
    kind, reason = classify(
        raw.get("content"),
        is_bot=raw.get("is_bot"),
        prior_kind=raw.get("record_kind"),
    )
    return Observation(
        observation_id=raw["observation_id"],
        platform=raw["platform"],
        collection_id=raw["collection_id"],
        source_room_id=raw["source_room_id"],
        native_message_id=raw.get("native_message_id"),
        actor_label=actor.get("observed_label"),
        actor_account=actor.get("native_account_id"),
        content=raw.get("content") or "",
        parsed_time=time.get("parsed"),
        record_kind=kind,
        kind_reason=reason,
        ordinal=ordinal,
        is_bot=raw.get("is_bot"),
        source_ordinal=source.get("record_ordinal"),
        source_fingerprint=source.get("fingerprint"),
        source_file=source.get("file"),
        source_kind=source.get("kind"),
    )


def iter_observations(
    collection: str,
    *,
    kind: str = "dated",
    rooms: set[str] | None = None,
    limit: int | None = None,
) -> Iterator[Observation]:
    """한 collection의 관찰을 파일 순서대로 흘려보낸다."""
    path = CORPUS_ROOT / collection / f"{kind}.jsonl.gz"
    if not path.exists():
        raise FileNotFoundError(f"raw corpus 없음: {path}")
    emitted = 0
    with gzip.open(path, "rt", encoding="utf-8") as handle:
        for ordinal, line in enumerate(handle):
            raw = json.loads(line)
            if rooms is not None and raw.get("source_room_id") not in rooms:
                continue
            yield _to_observation(raw, ordinal)
            emitted += 1
            if limit is not None and emitted >= limit:
                return


@dataclass
class KindStats:
    total: int = 0
    by_kind: dict[str, int] = field(default_factory=dict)
    by_reason: dict[str, int] = field(default_factory=dict)

    def add(self, obs: Observation) -> None:
        self.total += 1
        self.by_kind[obs.record_kind] = self.by_kind.get(obs.record_kind, 0) + 1
        self.by_reason[obs.kind_reason] = self.by_reason.get(obs.kind_reason, 0) + 1
