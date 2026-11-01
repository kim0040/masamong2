"""Person / alias as *candidate sources* (R0 v2), replacing R1's person ranking bonus.

- `resolve_persons`: planner `person_targets` → alias labels → speaker labels that exist in the
  index. Uses only the configured alias table and the surname+given-name rule; it never merges
  two different speakers because their strings look alike (blocks.json: "person_aliases on a
  block are search hints, not identity merge"). A target that matches too many distinct
  speakers is *ambiguous* and contributes nothing (lexical/dense still cover it).
- `speaker_fusion`: dense + lexical runs restricted to leaves the resolved speakers spoke in.
- `alias_run`: leaves whose text mentions any label (the person talked *about*).

All runs honour `allowed_ids` (room / block / time scope) before ranking — scope precedes
ranking, per the retrieval v2 design.

Scope contract: `resolve_persons` takes the caller's permission
scope (`allowed` leaf ids). Ambiguity is judged only among speakers who spoke inside that scope;
names from other rooms never enter the result or the trace. `None` = caller gave no scope (whole
index; runtime should always pass one), empty set = nothing allowed. An optional `IdentityTable`
(built from confirmed native account ids) lets one Discord account with several labels count as one
speaker and the same label on different accounts as different speakers. Kakao labels have no
account and stay unlinked. No person ids are minted and platforms are never merged.
"""

from __future__ import annotations

import threading
from dataclasses import dataclass, field
from typing import Any, Callable, Mapping

import numpy as np

from candidates import FusionResult, fuse


@dataclass(frozen=True)
class SpeakerKey:
    """Who spoke, at the granularity the platform actually confirms.

    kind="account": a native account id the platform supplied (Discord). All labels that account
    used are one speaker history. kind="label": no native account (Kakao CSV, or a record without
    one) — the observed label itself, per platform, is the key and stays *unlinked*.
    This is not a person id: keys are never merged across kinds or platforms here."""

    kind: str
    platform: str
    value: str

    def __post_init__(self) -> None:
        if self.kind not in ("account", "label"):
            raise ValueError("speaker key kind must be account|label")

    @property
    def linked(self) -> bool:
        return self.kind == "account"


@dataclass
class IdentityTable:
    """Optional input contract for account-aware resolution (built from the index's own rows).

    labels_by_key: labels each key was observed under; keys_by_label: keys a label maps to;
    leaf_ids_by_key: leaves in which that key spoke. No cross-platform merge, no synthetic ids."""

    labels_by_key: dict[SpeakerKey, set[str]] = field(default_factory=dict)
    keys_by_label: dict[str, set[SpeakerKey]] = field(default_factory=dict)
    leaf_ids_by_key: dict[SpeakerKey, set[str]] = field(default_factory=dict)

    def add(self, key: SpeakerKey, label: str, leaf_id: str | None) -> None:
        self.labels_by_key.setdefault(key, set()).add(label)
        self.keys_by_label.setdefault(label, set()).add(key)
        if leaf_id is not None:
            self.leaf_ids_by_key.setdefault(key, set()).add(leaf_id)


def _platform_of(room: Any) -> str:
    text = str(room or "")
    return text.split(":", 1)[0] if ":" in text else (text or "?")


def speaker_key_for_row(row: Mapping[str, Any]) -> SpeakerKey:
    account = row.get("actor_account")
    platform = _platform_of(row.get("source_room_original") or row.get("source_room_id"))
    if account is not None and str(account).strip():
        return SpeakerKey("account", platform, str(account).strip())
    return SpeakerKey("label", platform, str(row.get("actor_label") or "?"))


def build_identity_table(index: Any) -> IdentityTable:
    """From `index.messages` (actor_label / actor_account) and leaf [start, end) ranges."""

    table = IdentityTable()
    messages = getattr(index, "messages", None) or []
    for leaf in getattr(index, "leaves", None) or []:
        doc_id = str(leaf["doc_id"])
        start, end = int(leaf.get("start") or 0), int(leaf.get("end") or 0)
        if not (0 <= start < end <= len(messages)):
            continue
        for row in messages[start:end]:
            table.add(speaker_key_for_row(row), str(row.get("actor_label") or "?"), doc_id)
    return table


@dataclass
class PersonResolution:
    targets: list[str]
    labels: set[str] = field(default_factory=set)
    speakers: set[str] = field(default_factory=set)  # matched speaker labels (unambiguous, in scope)
    per_target: dict[str, list[str]] = field(default_factory=dict)
    ambiguous: list[str] = field(default_factory=list)
    unresolved: list[str] = field(default_factory=list)
    scope_mode: str = "none"  # none (caller gave no scope → whole index) | set | empty
    scope_leaves_n: int | None = None
    scope_speakers_n: int = 0
    speaker_keys: set[SpeakerKey] = field(default_factory=set)  # only when an IdentityTable was given
    per_target_keys: dict[str, list[SpeakerKey]] = field(default_factory=dict)
    identity_used: bool = False

    @property
    def resolved(self) -> bool:
        return bool(self.speakers)

    def to_trace(self) -> dict[str, Any]:
        """Labels only. Native account ids never enter the trace."""

        out: dict[str, Any] = {
            "targets": list(self.targets),
            "labels": sorted(self.labels)[:32],
            "speakers": sorted(self.speakers)[:16],
            "ambiguous": list(self.ambiguous),
            "unresolved": list(self.unresolved),
            "scope": {"mode": self.scope_mode, "leaves_n": self.scope_leaves_n, "speakers_n": self.scope_speakers_n},
        }
        if self.identity_used:
            out["speaker_keys"] = {
                "account_linked_n": sum(1 for k in self.speaker_keys if k.linked),
                "label_only_n": sum(1 for k in self.speaker_keys if not k.linked),
                "per_target_n": {t: len(ks) for t, ks in self.per_target_keys.items()},
            }
        return out


def speaker_leaf_index(index: Any) -> dict[str, list[str]]:
    """speaker label → leaf ids (index order). Built once per loaded index."""

    cached = getattr(index, "leaf_ids_by_speaker", None)
    if cached:
        return cached
    table: dict[str, list[str]] = {}
    for leaf in index.leaves:
        doc_id = str(leaf["doc_id"])
        for sp in leaf.get("speakers") or []:
            table.setdefault(str(sp), []).append(doc_id)
    try:
        index.leaf_ids_by_speaker = table
    except AttributeError:  # frozen fakes in tests
        pass
    return table


def speakers_in_scope(index: Any, allowed: set[str] | None) -> dict[str, list[str]]:
    """speaker label → leaf ids restricted to `allowed`. None = no scope given (whole index);
    an empty set = nothing allowed → no speakers."""

    table = speaker_leaf_index(index)
    if allowed is None:
        return table
    if not allowed:
        return {}
    out: dict[str, list[str]] = {}
    for sp, ids in table.items():
        kept = [d for d in ids if d in allowed]
        if kept:
            out[sp] = kept
    return out


def _labels_for(targets: list[str], aliases: dict[str, set[str]]) -> set[str]:
    labels: set[str] = set()
    for key in targets:
        labels.add(key)
        labels |= set(aliases.get(key) or ())
        # reverse lookup: target is an alias value of some canonical key
        for canon, vals in aliases.items():
            if key in vals:
                labels.add(canon)
                labels |= set(vals)
    return {x for x in labels if x}


def _keys_in_scope(identity: IdentityTable, label: str, allowed: set[str] | None) -> set[SpeakerKey]:
    keys = identity.keys_by_label.get(label) or set()
    if allowed is None:
        return {k for k in keys if identity.leaf_ids_by_key.get(k)}
    return {k for k in keys if identity.leaf_ids_by_key.get(k, set()) & allowed}


def resolve_persons(
    index: Any,
    targets: list[str],
    aliases: dict[str, set[str]],
    *,
    token_in_text: Callable[..., bool],
    max_speakers_per_target: int = 3,
    allowed: set[str] | None = None,
    identity: IdentityTable | None = None,
) -> PersonResolution:
    """planner targets → speakers *inside `allowed`*.

    `allowed` is the caller's permission scope (room/block leaf ids). None means the caller gave
    no scope and the whole index is searched (runtime callers should always pass one); an empty
    set resolves nothing. Speakers outside the scope never count toward ambiguity and never
    appear in the result or trace. With an `identity` table, ambiguity is counted per SpeakerKey
    (native account when confirmed, otherwise per-platform label) instead of per label."""

    res = PersonResolution(targets=list(targets))
    res.scope_mode = "none" if allowed is None else ("set" if allowed else "empty")
    res.scope_leaves_n = None if allowed is None else len(allowed)
    res.identity_used = identity is not None
    if not targets:
        return res
    in_scope = speakers_in_scope(index, allowed)
    res.scope_speakers_n = len(in_scope)
    res.labels = _labels_for(targets, aliases)
    if not in_scope:
        res.unresolved = list(targets)
        for t in targets:
            res.per_target[t] = []
        return res
    speakers_all = list(in_scope.keys())
    for target in targets:
        labels_t = _labels_for([target], aliases)
        toks = {t for t in labels_t | {target} if 2 <= len(t) <= 12}
        matched: list[str] = []
        for sp in speakers_all:
            if sp in labels_t or any(token_in_text(sp, tok, labels=labels_t) for tok in toks):
                matched.append(sp)
        if identity is not None:
            keys: set[SpeakerKey] = set()
            for sp in matched:
                keys |= _keys_in_scope(identity, sp, allowed)
            # labels of matched keys that are themselves in scope (same account, other label)
            key_labels = {lab for k in keys for lab in identity.labels_by_key.get(k, ()) if lab in in_scope}
            matched = sorted(set(matched) | key_labels, key=speakers_all.index)
            res.per_target_keys[target] = sorted(keys, key=lambda k: (k.kind, k.platform, k.value))
            distinct = len(keys)
        else:
            distinct = len(matched)
        res.per_target[target] = matched
        if not matched:
            res.unresolved.append(target)
        elif distinct > max_speakers_per_target:
            res.ambiguous.append(target)
        else:
            res.speakers.update(matched)
            if identity is not None:
                res.speaker_keys.update(res.per_target_keys[target])
    return res


def speaker_universe(
    index: Any,
    res: PersonResolution,
    allowed: set[str] | None,
    identity: IdentityTable | None = None,
) -> set[str]:
    out: set[str] = set()
    if identity is not None and res.identity_used and res.speaker_keys:
        for key in res.speaker_keys:
            out.update(identity.leaf_ids_by_key.get(key) or ())
    else:
        table = speaker_leaf_index(index)
        for sp in res.speakers:
            out.update(table.get(sp) or ())
    if allowed is not None:
        out &= allowed
    return out


def speaker_fusion(
    index: Any,
    res: PersonResolution,
    *,
    query: str,
    q_vec: np.ndarray,
    allowed: set[str] | None,
    limit: int,
    weights: list[float],
    lock: threading.Lock,
    identity: IdentityTable | None = None,
) -> tuple[FusionResult, set[str]]:
    """Dense + lexical over the speaker's own leaves (scope, not bonus)."""

    universe = speaker_universe(index, res, allowed, identity)
    if not universe:
        return fuse([("speaker_lexical", [], weights[0]), ("speaker_dense", [], weights[1])]), universe
    id_to_idx = {doc_id: i for i, doc_id in enumerate(index.leaf_ids)}
    idx = [id_to_idx[d] for d in universe if d in id_to_idx]
    sims = index.leaf_vectors[idx] @ q_vec
    order = np.argsort(-sims)[:limit]
    dense = [(index.leaf_ids[idx[i]], float(sims[i])) for i in order]
    with lock:
        lex_raw = index.leaf_fts.search(query, limit=max(limit, len(index.leaf_ids)))
    lexical = [(d, s) for d, s in lex_raw if d in universe][:limit]
    fused = fuse(
        [("speaker_lexical", [d for d, _ in lexical], weights[0]), ("speaker_dense", [d for d, _ in dense], weights[1])],
        raw_scores={"speaker_lexical": dict(lexical), "speaker_dense": dict(dense)},
    )
    return fused, universe


def alias_run(
    index: Any,
    res: PersonResolution,
    *,
    allowed: set[str] | None,
    limit: int,
    lock: threading.Lock,
) -> list[tuple[str, float]]:
    """Leaves that *mention* the person (any label) — lexical only."""

    labels = [x for x in sorted(res.labels, key=len, reverse=True) if len(x) >= 2]
    if not labels:
        return []
    with lock:
        raw = index.leaf_fts.search(" ".join(labels[:8]), limit=max(limit, len(index.leaf_ids)) if allowed is not None else limit)
    out = [(d, float(s)) for d, s in raw if allowed is None or d in allowed]
    return out[:limit]
