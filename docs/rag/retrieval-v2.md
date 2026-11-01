# 09 — Retrieval v2: R0 v2 candidate retrieval · confidence 기반 Auto · R1 퇴역

현재 제품 검색은 `internal/memory/search`와 `internal/chat`에 구현되어 있다. 이 문서는 샌드박스(`experimental/rag-leaf-parent/`)와 공유 계약(`contracts/sandbox/tool-parity.json`, `internal/memory/retrieval`, `internal/agent/planner_output.go`)이 따르는 후보원·auto 정책·평가 기준을 설명한다. 운영 방법은 [기억 운영](../memory-operations.md), Action 상태는 [catalog.go](../../internal/catalog/catalog.go)를 따른다.

## 한 줄

> 검색 의도를 이해하는 일은 Planner가 하고, 검색 전략은 retrieval이 **후보를 본 뒤** 정한다. person·alias·time은 점수 보너스가 아니라 **scope와 candidate source**다.

## 1. 흐름

```text
User Message
   │
   ▼
Planner (LLM 1회)  → memory_requirement · question_kind · person_targets · sensitive
   │                  · time {kind,from,to} · semantic_query · key_terms      (v2, 선택)
   │                  ※ R0/R2·pool·weight·rerank 여부는 고르지 않는다
   ▼
Scope Resolver      channel/block(view) → time window (per room anchor)      ← ranking보다 먼저
   │
   ▼
Candidate Retrieval (R0 v2, 독립 후보원, 실패 격리)
   │  leaf_lexical · leaf_dense           원문 leaf, q0
   │  summary_lexical · summary_dense     leaf 요약, q0
   │  speaker_lexical · speaker_dense     resolve_persons → 화자 leaf 안에서만
   │  alias_lexical                       인물 라벨을 언급한 leaf
   │  q1_dense · q1_summary_dense         planner semantic_query, 없으면 memory.search.query
   │  q2_lexical                          planner key_terms
   ▼
Candidate Fusion    weighted RRF (중첩: leaf⊕summary 순서는 v1과 동일) + provenance
   │                Candidate{doc_id, score, rank, sources{source→rank}, raw, flags}
   │                이 순서 = **R0 ranking** (`fusion_ids`)
   ▼
Candidate Pool Builder (`guarantee_pool`)
   │  fused head (pool − reserve) + 소스별 top-N 라운드로빈 보장
   │  이 집합 = **R2 candidate pool** (`candidate_pool_ids`)
   │  R0 상위 순서는 바꾸지 않는다
   ▼
Candidate Assessment (rule-based, LLM 없음)
   │  candidate_count · agreement · lexical_dense_overlap · top_margin
   │  target_coverage · source_diversity                → high / medium / low
   ▼
 ┌──────────────┬──────────────────┬────────────────────────┐
 high           medium             low
 R0 순서 그대로   R2 semantic rerank   Recovery 1회 (pool·limit ×1.5, time window ×2)
 (rerank 0회)                         → 재평가 → R2
 └──────────────┴──────────────────┴────────────────────────┘
   │  reranker 없음/예산 미달/호출 실패 → R0 (R1 아님)
   ▼
Relevance floor (rerank_min_score, 모델별) → abstain
   ▼
Dedup / Span merge (finalize_ranked_ids) → Parent expand (±12) → Evidence(+sources)
   ▼
Synthesis (rag_state: not_attempted | candidates | empty_in_allowed_scope | insufficient_authorized_candidates)
```

## 2. 책임

| 층 | 책임 | 하지 않는 것 |
|---|---|---|
| Planner (`bot_plan.PLAN_PROMPT`, `internal/agent.ParsePlannerOutput`) | 의미 구조화: 기억 필요 여부·질문 종류·인물·시점·의미 질의·핵심어 | 검색 알고리즘·pool·weight 선택 |
| Scope (`_scope_allowed_ids`, `retrieval_post.narrow_time_ids`) | 채널/블록/시간 창으로 universe 제한 | 다른 채널 검색 후 LLM 필터 |
| Candidate retrievers (`_generate_candidates`, `person_retrieval.py`) | 독립 후보원 생성, 각자 scope 준수 | 점수 가산 |
| Fusion (`candidates.fuse`) | weighted RRF + provenance | LLM이 weight 생성 |
| Pool builder (`guarantee_pool`) | R2 멤버십: fused head + 저가중 소스 top-N 보장 | R0 순위를 뒤집기 |
| Assessment (`candidates_assess.assess`) | 규칙 기반 confidence | classifier·LLM |
| Auto (`tool_parity.decide_ranking`, Go `AutoPolicy.Decide`) | finish / rerank / recovery 결정 | 검색 전 모드 선택 |
| Recovery | 1회, pool·limit·time window 확대 | 무한 재검색, LLM query 생성 |
| Rerank (`_rank_leaf_ids` rerank 분기) | 의미 재순위 + floor | 후보 생성 |
| Dedup/Expansion (`retrieval_post`) | 인접 leaf 병합, parent 확장 | — |

## 3. R0/R1/R2/Auto 이름 (변경 없음, 역할만 재정의)

| Variant | `ranking` | 역할 |
|---|---|---|
| R0 | `hybrid` | **R0 v2** candidate retrieval / baseline. 후보원 구성은 `retrieval` config로 켜고 끈다 |
| R1 | `heuristic` | legacy heuristic — **ablation 전용**. 코드·테스트 유지. auto의 어떤 경로에서도 선택되지 않는다 |
| R2 | `rerank` | R0 v2 풀 + semantic rerank (모델 config). R1을 포함하지 않는다 |
| Auto | `auto` | R0 v2 → 평가 → finish / R2 / recovery→R2 |

## 4. Person / Alias

- `resolve_persons(targets, alias_table)`: 플래너 `person_targets` → 별칭 라벨(정·역방향) → 인덱스에 실제 있는 화자 라벨. 매칭 규칙은 `_token_in_text`(조사·성+이름·별칭 경계)뿐. **문자열 유사성으로 병합하지 않는다**(`contracts/memory/blocks.json`: person_aliases는 검색 힌트). 한 타깃이 `max_speakers_per_target`(3)보다 많은 화자와 맞으면 *ambiguous* → 그 타깃은 후보원을 만들지 않는다.
- `speaker_fusion`: 그 화자의 leaf 집합(∩ scope) 안에서 dense+lexical. `alias_run`: 라벨을 언급한 leaf. 둘 다 외부 RRF에 weight(`retrieval.person.rrf_weight`, `alias.rrf_weight`)로 합류.
- 기존 `alias_expand_lexical`(라벨을 q0 lexical에 덧붙임)은 R0 v1 baseline 노브로 남겨둔다.

## 5. Time

- 계약 `planner_output.time = {kind, from, to}`, `time_kind = any|recent|today|yesterday|last_week|this_month|before|after|between`. v1 `time_scope`는 유지·상호 유도.
- `narrow_time_ids`: recent는 기존 방별 창(N일/K leaf), 상대 kind는 방별 anchor(최신 leaf) 기준 날짜 경계, 절대 kind는 ISO from/to. 파싱 실패는 `time_window_invalid`로 기록하고 scope를 넓히지 않는다. Recovery는 `time_window_scale`(2.0)로 창을 넓힌다.
- 시간 창 밖 leaf는 후보가 되지 않는다. recency 점수 가산은 R1 안에만 남아 있다.

## 6. Multi-query

q0는 **항상 사용자 원문**. 플래너 `memory.search.query`로 q0를 바꾸지 않는다(gold v3에서 rewrite 대체가 R@1을 깎음). q1은 `semantic_query`, 없거나 원문과 같으면 `memory.search.query`. q2는 `key_terms` lexical. `retrieval.multi_query.max_queries`(3). 공백 canonicalize 후 중복 제거. 임베딩 최대 1회 추가, LLM 추가 호출 0.

## 6.1 Candidate pool vs R0 ranking

`fusion_ids` / R0 Recall@k 는 weighted RRF 순서. `candidate_pool_ids` / pool recall 은 `guarantee_pool` 이후 R2에 넘길 집합이다. person/alias는 **ranking bonus가 아니라 후보원**이며, fusion weight를 올려 순위를 사는 대신 pool membership을 보장한다.

gold v3 no-window (n=85, 2026-09-17 후속): person 0.3 / alias 0.15 / `per_source_min` 5 / `reserve_max_frac` 0.2. R@1은 multi-query에 가깝게 유지하고 pool recall은 guarantee로 fusion cut 대비 +2문항(rescue 2.4pp). 숫자 목표로 박지 않았고, weight 1.0은 R@1을 다시 0.600으로 끌어내린다.

## 7. Confidence (규칙)

`candidates_assess.level_from_signals` = Go `retrieval.Assess`. 공유 케이스는 계약 `auto_policy.cases`.

```text
count < min_candidates(3)                               → low  (few_candidates)
targets 요청+해결 & coverage < min_target_coverage(0.3) → low  (target_missing)
agreement < low_agreement(0.2)                          → low  (low_agreement)
targets 요청 but 미해결                                  → medium (target_unresolved; 절대 high 아님)
agreement ≥ high_agreement(0.6) & margin ≥ high_margin(0.15) → high
그 외                                                    → medium
```

임계값은 `retrieval.auto` config에 둔다. 초기값은 평가 sweep의 R0@1 정답/오답 분포를 기준으로 잡았으며 코드에 목표치를 박지 않는다.

## 8. Auto 결정표 (계약 `ranking.auto_policy`, kind=cascade_v2)

| level | reranker 가능 | 결정 | reason |
|---|---|---|---|
| high | 무관 | R0 | `r0_confident` |
| medium | 예 | R2 | `rerank_uncertain` |
| medium | 아니오 | R0 | `no_rerank` |
| low | — | recovery 1회 후 재평가 | (`recovery_low_confidence`) |
| low, recovery 후 | 예 | R2 | `recovery_rerank` |
| low, recovery 후 | 아니오 | R0 | `recovery_no_rerank` |
| low, recovery 꺼짐 | 예/아니오 | R2 / R0 | `rerank_low_confidence` / `no_rerank` |
| R2 호출 실패 | — | R0 | `rerank_failed` |

## 9. Config (`config.json` → `retrieval`, 기본값 `retrieval_config.DEFAULTS`)

```json
"retrieval": {
  "person":   {"enabled": true, "limit": 60, "rrf_weight": 0.3, "max_speakers_per_target": 3},
  "alias":    {"enabled": true, "limit": 60, "rrf_weight": 0.15},
  "summaries": {"enabled": true},
  "multi_query": {"enabled": true, "max_queries": 3, "semantic_rrf_weight": 1.0, "key_terms_rrf_weight": 0.5},
  "candidate_pool": {"default": 100, "min": 40, "max": 160, "per_source_min": 5, "reserve_max_frac": 0.2},
  "auto": {"enabled": true, "assess_top_n": 10, "high_agreement": 0.6, "high_margin": 0.15,
           "low_agreement": 0.2, "min_candidates": 3, "min_target_coverage": 0.3},
  "recovery": {"enabled": true, "max_attempts": 1, "pool_scale": 1.5, "limit_scale": 1.5, "time_window_scale": 2.0},
  "rerank": {"enabled": true, "doc_format": "summary_body", "pre_dedup": false}
}
```

기존 flat 키(`retrieve_pool`, `lexical_limit`, `dense_limit`, `rrf_weights`, `summary_rrf_weight`, `rerank_min_score`, `recent_window_*`)는 R0 baseline 노브로 그대로 읽는다.

## 10. Trace (sandbox/eval/debug 전용 — 사용자 응답에 노출하지 않음)

`intent`(source·time·semantic_query·key_terms·memory_search_query) · `queries_used` · `persons` · `time_kind`/`recent_window_*` · `retrievers_used` · `candidate_sources` · `candidate_runs` · `candidate_provenance` · `candidate_union_n` · `fusion_ids` · `candidate_pool_ids` · `candidate_pool_n` · `pool_guaranteed_ids` / `pool_guaranteed_n` / `pool_guaranteed_by_source` · `auto_confidence` · `ranking_auto_reason` · `auto_decision` · `recovery`(before/after/limits) · `recovery_performed` · `rerank_performed` · `rerank_model`/`rerank_cost_usd`/`rerank_latency_ms`/`rerank_doc_format` · `abstained`/`abstain_reason` · hits[].`sources`.

## 11. Fallbacks / 실패 격리

| 실패 | 결과 |
|---|---|
| person resolver / speaker·alias retriever 예외 | `person_retriever_error`, lexical+dense(+summary) 유지 |
| semantic_query 임베딩 실패 | `multi_query_error`, q0만 |
| reranker 키 없음 / 예산 < $0.02 | R0 (`no_rerank`) |
| reranker 호출 실패·잘못된 응답 | R0 (`rerank_failed`), 오류 본문은 trace에 남기지 않음 |
| 시간 창 파싱 실패 | `time_window_invalid`, scope 유지 |
| 모든 rerank 점수 < floor | abstain → `insufficient_authorized_candidates` |

## 12. Evaluation

- `tools/evaluation/rag_upgrade/run_retrieval_v2_eval.py`: **런타임 `retrieve()`를 그대로 호출**한다 (구 `run_sample_dataset_ablation.py`는 leaf-only hybrid + body-only rerank라 런타임과 달랐다). 측정: R@1/5/10·MRR, **pool recall = R2-eligible `candidate_pool_ids`**, fusion_pool_recall(guarantee 전 fused cut), guarantee_rescue_rate, union recall, retriever별 recall, unique contribution / pool unique, auto: rerank/recovery 호출률·불필요 rerank율(R0@1인데 R2)·R0 miss 복구 수·route accuracy·latency p50/p95·비용, 유형별 분해, abstention 정답률.
- 플래너 의도는 `--plans` 캐시(문항당 planner 1회)로 런타임과 같게 쓴다. `--query-source original`(기본, q0=원문) | `planner`(q0를 memory.search.query로 **교체하는 ablation**).
- `sweep_retrieval_v2.py`: r0 / guarantee / auto / r2doc (`r2on-*` live 포함). 기존 태그 디렉터리는 덮지 않는다. 과거 합성 세트에서는 R2 `summary_body` R@1 0.835, Auto 0.824였지만 사람 검수 gold 전에는 품질 승인 근거로 사용하지 않는다.
- gold v3는 synthetic·unreviewed. "최근" 문항은 시점과 무관하게 만들어져 **runtime time-window 평가에 편향**이 있다(07). 시간 창을 끄지 않는다. 시점 gold는 `build_temporal_gold.py` 초안 + 사람 검수. **승격 판단은 사람 검수 gold가 있어야 한다.**

## 13. 하지 않은 것 / 다음

- Planner에 R0/R2 선택·weight 생성 위임 — 하지 않음(원칙).
- learned fusion / classifier confidence — 규칙만.
- Recovery 2회 이상, 다중 reranker — 비용 대비 근거 없음.
- 사람 검수 gold(person_intro·person_fact·recent·alias·event) — 미착수. 07 로그 "다음 권장 작업".
- 검색 본체는 `internal/memory/search`, 색인·저장은 `internal/memory/engine`·`internal/memory/store`에 있다. 사람 검수 gold와 운영 품질 승인은 자동 평가와 별도로 확인한다.
