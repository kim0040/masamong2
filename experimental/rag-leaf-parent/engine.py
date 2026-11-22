"""Sandbox engine facade: re-exports the three halves so `import engine as E` keeps working.

The code lives in `engine_index` (config, paths, ledger, prepare_index, load_index), `engine_retrieve`
(intent, candidates, fusion, rerank, retrieve) and `engine_turn` (prompt, sessions, chat, chat_stream).
Reading a name here gives the owner module's current value. Assigning to a name here does NOT reach the
owner: a test that fakes `RerankClient`, `load_secrets` or `retrieve` patches the module that uses it
(`engine_retrieve`, `engine_turn`), not this one. The data-dir paths and the prepare state are read live
through `__getattr__` because `_bind_data_dir()` rebinds them in `engine_index`.
"""

from __future__ import annotations

from engine_index import (  # noqa: F401
    _repo_root,
    ROOT,
    EVAL,
    HERE,
    _bind_data_dir,
    SUMMARY_PROMPT,
    ThreadSafeLexicalIndex,
    PlaygroundError,
    load_config,
    _maybe_rebind_data_from_env,
    _LEDGER_LOCK,
    load_ledger,
    save_ledger,
    apply_client_usage,
    remaining_session_budget,
    _elapsed_ms,
    _compact_timing,
    _secrets,
    configured_models,
    make_client,
    _room_observations,
    index_fingerprint,
    _atomic_write_json,
    _load_vecs,
    _report_prepare,
    _note_prepare_cost,
    prepare_status_payload,
    _load_ckpt_state,
    _save_ckpt_state,
    _clear_checkpoint,
    _normalize_summary_text,
    _summarize_leaves,
    _embed_texts,
    _message_fingerprint_for,
    _MESSAGE_CKPT_FILES,
    _align_message_checkpoint,
    prepare_message_vectors,
    _settle_prepare,
    _prepare_messages_only,
    prepare_index,
    _parse_ts,
    IndexBundle,
    _configured_embed_model,
    load_index,
    _truncate_rows,
    apply_vector_dims,
    populate_block_catalog,
)
from engine_retrieve import (  # noqa: F401
    weighted_rrf,
    _PARTICLE_CHARS,
    _strip_name_particles,
    _token_in_text,
    DEFAULT_IDENTITY_TERMS,
    _RECENT_RE,
    _INTRO_RE,
    _POLITICS_RE,
    _identity_re,
    _source_date,
    _leaf_source_date,
    _context_only_date,
    _query_intent,
    _neutral_intent,
    resolve_intent,
    DEFAULT_ALIAS_IGNORE,
    _PRONOUNS,
    _clean_planner_targets,
    _person_identity_mode,
    _identity_for,
    _observed_alias_table,
    _alias_table_for,
    _alias_table,
    _query_targets,
    _target_labels,
    _intent_targets,
    _lexical_query_expanded,
    _SoftEventRuntime,
    _rerank_available,
    fit_query_vector,
    _embed_instruction,
    _late_settle,
    prefetch_query_vector,
    query_vector,
    _q1_text,
    _hit_provenance,
    _dense_hits,
    _lexical_hits,
    _hybrid_fusion,
    _hybrid_rank,
    _person_signal,
    _recency_bonus,
    _rerank_leaves,
    RERANK_DOC_FORMATS,
    _rerank_document,
    _pre_rerank_dedup,
    _rank_leaf_ids,
    _apply_hybrid_floor,
    _scope_allowed_ids,
    _canon_query,
    _soft_event_recipe,
    _soft_event_leaf_times,
    _soft_event_search_text,
    _soft_event_runtime,
    _soft_event_candidates,
    _generate_candidates,
    retrieve,
)
from engine_turn import (  # noqa: F401
    ANSWER_SYSTEM,
    TOOL_BLOCK_MAX,
    _json_len,
    _fit_tail,
    _fill_shares,
    _tool_block,
    _build_chat_messages,
    MEMORY_TAIL,
    _indexed_rooms,
    SERVER_RUN_ID,
    _log_context,
    _config_snap,
    _prompt_stats,
    _history_stats,
    _request_time_reference,
    _planner_time_reference,
    _prepare_bot_turn,
    _session_keep,
    _session_char_limit,
    _load_session,
    _persist_session,
    chat,
    chat_stream,
    status_payload,
)

# Names the old single-file engine imported from the shared packages; callers still reach them as `engine.X`.
from corpus import iter_observations  # noqa: F401
from lexical import (  # noqa: F401
    LexicalIndex,
    VARIANTS,
)
from client import (  # noqa: F401
    DEFAULT_EMBED_QUERY_INSTRUCTION,
    ClientError,
    PlayClient,
    StreamError,
    load_secrets,
)
from retrieval_post import (  # noqa: F401
    TimeReference as RetrievalTimeReference,
    narrow_time_ids,
    resolve_time_scope,
    RERANK_MIN_SCORE,
    finalize_ranked_ids,
    merged_span,
    narrow_recent_ids,
    recency_anchor_ts,
    scaled_person,
    search_limits,
    resolve_rerank_min_score,
)
from segment import (  # noqa: F401
    collect_messages,
    expand_segment,
    slice_segments_per_room,
)
from rag_upgrade.playground_debug import (  # noqa: F401
    RankingError,
    append_debug_error,
    append_debug_turn,
    new_debug_id,
    normalize_ranking,
    recent_debug_turns,
)
from rag_upgrade.rerank_client import (  # noqa: F401
    RerankClient,
    RerankError,
)
from models import (  # noqa: F401
    ModelConfigError,
    resolve_models,
    validate_models,
)
from bot_plan import (  # noqa: F401
    intent_from_plan,
    memory_query,
    needs_local_rag,
    parse_plan,
)
from bot_runtime import (  # noqa: F401
    channel_id_from_room,
    compose_system_prompt,
    ensure_runtime,
    get_channel,
    public_channel_list,
    respond_allows,
    tool_policy,
)
from planner_context import (  # noqa: F401
    PLANNER_MAX_TOKENS,
    PlannerHistory,
    TimeReference as PlannerTimeReference,
    plan_turn,
)
from evidence_packing import (  # noqa: F401
    make_time_filter,
    pack_evidence_set,
)
from bot_tools import (  # noqa: F401
    execute_named_tool,
    is_success,
)
from memory_blocks import (  # noqa: F401
    catalog_from_leaves,
    leaf_ids_for_blocks,
    platform_of_room,
    segmentation_version_from_meta,
    space_from_index,
    write_catalog,
    _label_for_room,
)
from candidates import (  # noqa: F401
    FusionResult,
    fuse,
    guarantee_pool,
    source_family,
)
from candidates_assess import assess  # noqa: F401
from person_retrieval import (  # noqa: F401
    alias_run,
    build_identity_table,
    resolve_persons,
    speaker_fusion,
)
from retrieval_config import (  # noqa: F401
    pool_size,
    retrieval_config,
)
from message_vectors import (  # noqa: F401
    MESSAGE_LEAF_NAME,
    MESSAGE_META_NAME,
    MESSAGE_RRF_WEIGHT,
    MESSAGE_RUN_NAME,
    MESSAGE_SCALE_NAME,
    MESSAGE_VECS_NAME,
    fit_message_vectors,
    embed_full_width,
    load_message_vectors,
    message_dense_run,
    message_dims,
    message_fingerprint,
    select_message_rows,
    write_message_vectors,
)
from query_embed import (  # noqa: F401
    QueryEmbedder,
    query_embed_config,
)
from turn_diagnosis import build_diagnosis  # noqa: F401
from soft_events import (  # noqa: F401
    SoftEventRecipe,
    build_soft_events,
    project_event_ranks,
)
from sandbox_paths import (  # noqa: F401
    load_local_config,
    local_config_path,
    resolve_sandbox_data_dir,
)
from tool_parity import (  # noqa: F401
    canonicalize_tool,
    decide_ranking,
    fallback_ranking,
    parse_bang_command,
    ranking_policy,
)
from conversation import (  # noqa: F401
    DEFAULT_CHAR_LIMIT,
    DEFAULT_KEEP,
    SUMMARIZE_PROMPT,
    append_turn,
    compact,
    get_conversation,
    history_for_llm,
    needs_compact,
    put_conversation,
)
from prepare_progress import (  # noqa: F401
    idle_payload,
    mark_phase_start,
    phase_started,
    print_cli,
    read_progress,
    reset_embed_counts,
    set_embed_count,
    snapshot,
    write_progress,
)

import engine_index  # noqa: E402
import engine_retrieve  # noqa: E402

_LIVE = {
    "DATA": engine_index,
    "MESSAGES_PATH": engine_index,
    "LEAVES_PATH": engine_index,
    "BASELINE_PATH": engine_index,
    "LEAF_VECS_PATH": engine_index,
    "BASELINE_VECS_PATH": engine_index,
    "SUMMARIES_PATH": engine_index,
    "SUMMARY_VECS_PATH": engine_index,
    "META_PATH": engine_index,
    "LEDGER_PATH": engine_index,
    "CKPT_DIR": engine_index,
    "CKPT_STATE_PATH": engine_index,
    "PROGRESS_PATH": engine_index,
    "_prepare_summary_on": engine_index,
    "_prepare_cost_usd": engine_index,
    "LEXICAL_LIMIT": engine_retrieve,
    "DENSE_LIMIT": engine_retrieve,
}


def __getattr__(name: str):
    """The paths and prepare state change when the data dir is rebound; always read the owner."""
    owner = _LIVE.get(name)
    if owner is None:
        raise AttributeError(f"module 'engine' has no attribute {name!r}")
    return getattr(owner, name)
