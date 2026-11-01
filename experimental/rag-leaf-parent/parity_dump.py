"""Dump sandbox retrieve() output for the Go retrieve parity test (internal/memory/search/parity_test.go).

No API calls and no stored questions: each query is the start of a sampled leaf's own text and its
query vector is that leaf's stored vector, so both sides search with the same input. Rerank is off
(hybrid, auto) or a deterministic fake reranker (rerank). The dump holds chat text: write it outside
the repository (the scratch folder) and never commit it.

    MASAMONG_SANDBOX_CONFIG=test-sandbox/config.discord-gap30.json PARITY_N=80 \
        python3 experimental/rag-leaf-parent/parity_dump.py <out_dir> discord
    MASAMONG_SANDBOX_DATA=test-sandbox/data-discord-gap30 MASAMONG_SANDBOX_CONFIG=test-sandbox/config.discord-gap30.json \
        MASAMONG_PARITY_RETRIEVE=<out_dir>/retrieve_discord.json go test ./internal/memory/search/ -run Parity -v
"""
import json, os, sys
from parity_numeric import platform_metadata, capture_generation, simulate_dot, packing_variants
repo = os.getcwd()
sys.path.insert(0, os.path.join(repo, "experimental/rag-leaf-parent"))
os.environ.setdefault("EVAL_SECRETS", os.path.join(repo, ".private/eval/secrets.env"))
out_dir, gold_name = sys.argv[1], sys.argv[2]
import numpy as np
import engine as E
capture_generation(E)
# 옛 정렬을 비교할 때만 사용한다. 기본 덤프와 검색은 항상 안정 정렬이다.
if os.environ.get("PARITY_SORT") == "legacy":
    original_argsort = np.argsort
    def legacy_argsort(a, *args, **kwargs):
        kwargs["kind"] = "quicksort"
        return original_argsort(a, *args, **kwargs)
    np.argsort = legacy_argsort
from bot_plan import intent_from_plan

index = E.load_index()
# Measurement-only residency; the default/full dump remains unchanged. The Go
# loader reads the same private JSON array via MASAMONG_SANDBOX_ROOMS_FILE.
rooms_file = os.environ.get("MASAMONG_SANDBOX_ROOMS_FILE")
if rooms_file:
    from dataclasses import replace
    from pathlib import Path
    from person_retrieval import IdentityTable, build_identity_table
    scratch = Path(repo, ".masamong-scratch").resolve()
    for path in (Path(rooms_file).resolve(), Path(out_dir).resolve()):
        if scratch != path and scratch not in path.parents:
            raise RuntimeError("부분 parity 파일은 .masamong-scratch/ 아래에 둬야 합니다")
    selected = json.loads(Path(rooms_file).read_text(encoding="utf-8"))
    if not isinstance(selected, list) or not all(isinstance(r, str) for r in selected):
        raise RuntimeError("상주 방 필터는 문자열 JSON 배열이어야 합니다")
    rooms = set(selected)
    global_identity = build_identity_table(index)
    positions, resident_messages = {}, []
    for i, message in enumerate(index.messages):
        if message.get("source_room_id") in rooms:
            positions[i] = len(resident_messages)
            resident_messages.append(message)
    leaf_rows, resident_leaves, leaf_positions = [], [], {}
    for i, leaf in enumerate(index.leaves):
        if leaf.get("source_room_id") not in rooms:
            continue
        start, end = int(leaf["start"]), int(leaf["end"])
        if start not in positions or end - 1 not in positions or positions[end - 1] - positions[start] + 1 != end - start:
            raise RuntimeError("상주 방 leaf 구간이 불정합입니다")
        leaf_positions[i] = len(resident_leaves)
        resident_leaves.append({**leaf, "start": positions[start], "end": positions[end - 1] + 1})
        leaf_rows.append(i)
    kept = {leaf["doc_id"] for leaf in resident_leaves}
    summary_rows = [i for i, summary in enumerate(index.summaries) if summary["doc_id"] in kept]
    summaries = [index.summaries[i] for i in summary_rows]
    leaf_fts = E.ThreadSafeLexicalIndex(str(index.cfg["lexical"]))
    leaf_fts.add_many([(leaf["doc_id"], leaf["body"]) for leaf in resident_leaves])
    summary_fts = None
    if summaries:
        summary_fts = E.ThreadSafeLexicalIndex(str(index.cfg["lexical"]))
        summary_fts.add_many([(s["doc_id"], s["summary"]) for s in summaries])
    times = {key: value for key, value in index.leaf_end_ts.items() if key in kept}
    message_vectors, message_leaf = None, None
    if index.message_vectors is not None and index.message_leaf is not None:
        message_rows = [i for i, leaf_row in enumerate(index.message_leaf) if int(leaf_row) in leaf_positions]
        message_vectors = index.message_vectors[message_rows].copy()
        message_leaf = np.asarray([leaf_positions[int(index.message_leaf[i])] for i in message_rows], dtype=np.int32)
    index = replace(index, messages=resident_messages, leaves=resident_leaves,
                    leaf_vectors=index.leaf_vectors[leaf_rows].copy(), leaf_ids=[l["doc_id"] for l in resident_leaves],
                    leaf_by_id={l["doc_id"]: l for l in resident_leaves}, leaf_fts=leaf_fts,
                    baseline=[], baseline_vectors=np.zeros((0, index.leaf_vectors.shape[1]), dtype=np.float32),
                    baseline_ids=[], baseline_by_id={}, baseline_fts=E.ThreadSafeLexicalIndex(str(index.cfg["lexical"])),
                    summaries=summaries, summary_ids=[s["doc_id"] for s in summaries],
                    summary_by_id={s["doc_id"]: s for s in summaries}, summary_fts=summary_fts,
                    summary_vectors=index.summary_vectors[summary_rows].copy() if index.summary_vectors is not None else None,
                    leaf_end_ts=times, leaves_by_recency=[key for key in index.leaves_by_recency if key in kept],
                    corpus_tmax=max(times.values(), default=0.0), leaf_ids_by_speaker={}, soft_event_runtime_cache={},
                    message_vectors=message_vectors, message_leaf=message_leaf)
    local_identity = build_identity_table(index)
    index._identity_table = IdentityTable(global_identity.labels_by_key, global_identity.keys_by_label, local_identity.leaf_ids_by_key)
    E.populate_block_catalog(index)  # no persisted subset catalog
if os.environ.get("PARITY_DOT"):
    simulate_dot(index, np, os.environ["PARITY_DOT"])
instruction = str(index.cfg.get("embed_query_instruction") or E.DEFAULT_EMBED_QUERY_INSTRUCTION)
model = E.configured_models(index.cfg)["embed"]
import random
random.seed(int(os.environ.get("PARITY_SEED", "7")))
leaf_ids = [d["doc_id"] for d in index.leaves]
id_to_idx = {d: i for i, d in enumerate(index.leaf_ids)}
msgs = index.messages
questions = []
qvec = {}
for doc_id in random.sample(leaf_ids, min(int(os.environ.get("PARITY_N", "60")), len(leaf_ids))):
    leaf = index.leaf_by_id[doc_id]
    rows = [m for m in msgs[int(leaf["start"]):int(leaf["end"])] if (m.get("content") or "").strip()]
    if not rows or doc_id not in id_to_idx:
        continue
    text = " ".join((m.get("content") or "").strip() for m in rows[:2])[:40]
    if text in qvec:
        continue
    qvec[text] = [float(x) for x in index.leaf_vectors[id_to_idx[doc_id]]]
    questions.append({"question_id": doc_id, "query": text, "source_leaf_doc_id": doc_id})
gold = {"questions": questions}
plans = {}
def cached(text):
    return qvec.get(text)
vectors = {}
class FakeClient:
    embed_model = model
    def embed(self, texts, kind="query", instruction=None, **kw):
        out = []
        for t in texts:
            v = cached(t)
            if v is None:
                raise RuntimeError("uncached")
            vectors[t] = v
            out.append(v)
        return out

class FakeRerank:
    def __init__(self, *a, **k):
        self.model = k.get("model", "fake")
    def rerank(self, query, docs, top_n=None):
        def score(d):
            return 0.3 + 0.6 * ((len(d) * 7919) % 1000) / 1000.0
        order = sorted(range(len(docs)), key=lambda i: (-score(docs[i]), i))[: top_n or len(docs)]
        class R: pass
        r = R(); r.indices = order; r.scores = [score(docs[i]) for i in order]; r.model = "voyageai/rerank-2.5-lite"
        r.cost_usd = 0.0; r.latency_ms = 0.0; r.billed = None; r.generation_id = None
        return r
E.RerankClient = FakeRerank
E.load_secrets = lambda *a, **k: {"OPENROUTER_API_KEY": "x"}

leaf_room = {d["doc_id"]: d["source_room_id"] for d in index.leaves}
rows = []
for q in gold["questions"]:
    query = q["query"]
    qv = cached(query)
    if qv is None:
        continue
    vectors[query] = qv
    plan = plans.get(q["question_id"]) or {}
    intent = intent_from_plan(plan.get("parsed")) if plan.get("parsed") else None
    room = leaf_room.get(q.get("source_leaf_doc_id") or "", "")
    rec = {"question_id": q["question_id"], "query": query, "room": room, "intent": intent, "modes": {}}
    for mode, avail in (("hybrid", False), ("auto", False), ("rerank", True)):
        E._rerank_available = (lambda a=avail: a)
        trace = {}
        try:
            hits = E.retrieve(index, FakeClient(), query, top_k=5, mode="leaf_parent", q_vec=np.asarray(qv, dtype=np.float32),
                              ranking=mode, remaining_budget=0.2, trace=trace, source_room_id=room or None, intent=dict(intent) if intent else None)
        except RuntimeError as e:
            rec["modes"][mode] = {"error": str(e)}
            continue
        ac = trace.get("auto_confidence") or {}
        final_scores = dict(zip(trace.get("rerank_order") or [], trace.get("rerank_scores") or [])) if trace.get("applied") == "rerank" else {
            key: value["score"] for key, value in trace.get("parity_scores", {}).items()}
        rec["modes"][mode] = {
            "hits": [{"doc_id": h["doc_id"], "excerpt": h["excerpt"], "status": h["packing_status"], "visible": h["visible_observation_ids"],
                      "merged": h["merged_leaf_ids"], "span": h["span"], "summary": h["summary"], "date": h.get("date", ""),
                      "score": final_scores[h["doc_id"]]} for h in hits],
            "fusion_ids": trace.get("fusion_ids"), "pool_ids": trace.get("candidate_pool_ids"),
            "retrievers": trace.get("retrievers_used"), "sources": trace.get("candidate_sources"),
            "auto": {k: ac.get(k) for k in ("level", "agreement", "top_margin", "candidate_count", "target_coverage")} if ac else None,
            "reason": trace.get("ranking_auto_reason"), "applied": trace.get("applied"), "abstained": bool(trace.get("abstained")),
            "queries": trace.get("queries_used"), "persons": trace.get("persons"), "recovery": bool(trace.get("recovery_performed")),
            "scores": trace.get("parity_scores"), "tree": trace.get("parity_tree"),
            "final_scores": final_scores,
        }
        rec["modes"][mode]["packing_variants"] = packing_variants(E, index, rec["modes"][mode]["hits"], trace["parity_scores"])
    rows.append(rec)
os.makedirs(out_dir, exist_ok=True)
metadata = platform_metadata(np)
metadata["dot_simulation"] = os.environ.get("PARITY_DOT", "blas_float32")
metadata["sort_simulation"] = os.environ.get("PARITY_SORT", "stable")
json.dump({"metadata": metadata, "rows": rows, "vectors": vectors}, open(os.path.join(out_dir, f"retrieve_{gold_name}.json"), "w"), ensure_ascii=False)
print(gold_name, len(rows), "questions", len(vectors), "vectors")
