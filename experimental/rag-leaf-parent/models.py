"""Model roles for the test sandbox — every model is swappable from config.json `models`.

Roles (shared with product config.ModelRoles / contract models.roles):
  response(답변) · planner(도구/의도) · summary(leaf 요약·세션 요약) · embed(임베딩) · rerank(R2)
`llm` is accepted as a legacy alias of `response`.
Precedence per role: config.json models.<role>  >  secrets.env EVAL_<ROLE>_MODEL  >  built-in default.
Guards (the "조건"): OpenRouter `provider/model` 형식, 역할에 맞는 모델 종류(embed/rerank 이름 검사),
그리고 임베딩 모델은 인덱스를 만든 모델과 같아야 한다 (engine.load_index가 거부).
"""

from __future__ import annotations

import re
from typing import Any, Mapping

ROLES = ("response", "planner", "summary", "embed", "rerank")
LEGACY_ROLE_ALIASES = {"llm": "response"}

DEFAULTS: dict[str, str] = {
    "response": "z-ai/glm-5.3-flash",
    "planner": "",   # 빈 값 → response 와 같은 모델
    "summary": "",   # 빈 값 → response 와 같은 모델
    "embed": "qwen/qwen3-embedding-8b",
    "rerank": "voyageai/rerank-2.5-lite",
}

_SECRET_KEYS = {
    "response": "EVAL_LLM_MODEL",
    "planner": "EVAL_PLANNER_MODEL",
    "summary": "EVAL_SUMMARY_MODEL",
    "embed": "EVAL_EMBED_MODEL",
    "rerank": "EVAL_RERANK_MODEL",
}

_MODEL_ID = re.compile(r"^[a-z0-9][a-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._:-]*$")


class ModelConfigError(ValueError):
    pass


def resolve_models(cfg: Mapping[str, Any], secrets: Mapping[str, str] | None = None) -> dict[str, str]:
    """역할별 모델 ID. planner/summary가 비면 response를 따른다. 옛 키 `rerank_model`·`models.llm`도 읽는다."""

    raw_in = cfg.get("models") if isinstance(cfg.get("models"), Mapping) else {}
    raw = {LEGACY_ROLE_ALIASES.get(str(k), str(k)): v for k, v in raw_in.items()}
    secrets = secrets or {}
    out: dict[str, str] = {}
    for role in ROLES:
        value = str(raw.get(role) or "").strip()
        if not value and role == "rerank":
            value = str(cfg.get("rerank_model") or "").strip()
        if not value:
            value = str(secrets.get(_SECRET_KEYS[role]) or "").strip()
        if not value:
            value = DEFAULTS[role]
        out[role] = value
    if not out["planner"]:
        out["planner"] = out["response"]
    if not out["summary"]:
        out["summary"] = out["response"]
    return out


def validate_models(models: Mapping[str, str]) -> None:
    """형식·역할 적합성 검사. 비용 상한은 client/rerank budget이, 인덱스 일치는 load_index가 본다."""

    for role in ROLES:
        value = str(models.get(role) or "").strip()
        if not value:
            raise ModelConfigError(f"models.{role} 이 비어 있다")
        if not _MODEL_ID.match(value):
            raise ModelConfigError(
                f"models.{role}={value!r} 는 OpenRouter 형식(provider/model)이 아니다"
            )
    embed = models["embed"].lower()
    if "embed" not in embed:
        raise ModelConfigError(f"models.embed={models['embed']!r} 는 임베딩 모델로 보이지 않는다 (이름에 embed 필요)")
    rerank = models["rerank"].lower()
    if "rerank" not in rerank:
        raise ModelConfigError(f"models.rerank={models['rerank']!r} 는 재순위 모델로 보이지 않는다 (이름에 rerank 필요)")
    for role in ("response", "planner", "summary"):
        low = models[role].lower()
        if "embed" in low or "rerank" in low:
            raise ModelConfigError(f"models.{role}={models[role]!r} 는 채팅 모델이 아니다")


def public_models(models: Mapping[str, str]) -> dict[str, str]:
    return {role: str(models.get(role) or "") for role in ROLES}
