# rag_upgrade — evaluation package

Instruction-driven RAG/Memory/Agent upgrade harness (2026-09-16).

- Default: **network-free** unit tests
- Live Cohere via OpenRouter only with explicit budget; **never use Discord tokens**
- Does not modify private corpora or product WO defaults

```bash
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tools/evaluation/rag_upgrade -p 'test_*.py'
```

## Retrieval v2 evaluation (2026-09-17)

`run_retrieval_v2_eval.py` calls the **runtime** `engine.retrieve()` (same code path as the
sandbox bot: R0 v2 candidate sources, time window, auto v2 cascade, floor, span merge) and
separates candidate-generation metrics from ranking metrics:

```bash
# planner intent cache (one planner call per gold question, cached)
python3 tools/evaluation/rag_upgrade/run_retrieval_v2_eval.py --variants R0 --plans .masamong-scratch/retrieval-v2/plans.v3.json --plan --out .masamong-scratch/retrieval-v2/plan-build
# named sweeps (r0 = no reranker cost; auto / r2doc call the reranker)
python3 tools/evaluation/rag_upgrade/sweep_retrieval_v2.py r0 guarantee auto r2doc
```

Outputs: `summary.json` (overall + `by_type`: R@k, MRR, pool/union recall, per-retriever
recall, unique contribution, auto route metrics, latency, cost) and `per_question_metrics.json`.
`--config-override` merges JSON over `config.json` (e.g. disable a retriever); `--query-source
original` (default, q0 = user text) or `planner` (replace q0 — ablation). `--embed-cache` memoises
query embeddings (key: model + kind + instruction + text); the q1 (semantic_query) embedding is
memoised per index in memory, so it is paid once per question, not once per variant. `--q-workers N`
(default 8) scores questions in parallel with identical results; `--q-workers 1` is sequential. `pool_recall` is the R2-eligible
pool after `guarantee_pool`, not the fused cut.

`run_sample_dataset_ablation.py` is the legacy R0/R1/R2 harness (leaf-only hybrid, body-only
rerank input) and remains available for controlled regression comparisons.

Docs: `docs/rag/` (design: `retrieval-v2.md`, evaluation rules: `evaluation.md`)

## 2026-09-23 일반화 감사 도구

| 파일 | 용도 |
|---|---|
| `build_holdout_gold.py` | 메시지 단위 기준·방/시간 층화·원문 베끼기 필터로 holdout gold 생성 (다른 collection 사실로 "기억에 없음" 문항) |
| `summarize_generalization.py` | 도메인 × 설정 × 버전 표, 짝지은 bootstrap CI, holdout 매크로·최악 세트, dev−holdout 차이 |
| `replay_stream.py` | 실제 타임라인을 `incremental_leaves`로 재생 (배치=실시간, 삭제·미래 노출, 재수입 중복, 분할 규칙별 지연). API 호출 없음 |
| `answer_level_check.py` | 런타임 경로로 답변 생성 후 judge 판정 (정답·부분·오답·모른다 × 근거 충분성) |

평가 계약과 실행 예시는 [docs/rag/evaluation.md](../../../docs/rag/evaluation.md)에 있다.
