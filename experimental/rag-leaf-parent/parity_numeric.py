"""테스트 덤프 전용 숫자 진단. 검색 알고리즘과 제품 trace는 바꾸지 않는다."""
import platform
from itertools import permutations


def platform_metadata(np):
    # show_config의 전체 출력에는 빌드 경로가 있다. 이름/버전만 화이트리스트한다.
    cfg = np.show_config(mode="dicts")
    deps = cfg.get("Build Dependencies", {})
    return {"os": platform.system(), "architecture": platform.machine(),
            "python": platform.python_version(), "numpy": np.__version__,
            "blas": {key: {field: str(deps.get(key, {}).get(field, "unknown"))
                           for field in ("name", "version")} for key in ("blas", "lapack")}}


def capture_generation(engine):
    original = engine._generate_candidates
    original_fuse = engine.fuse
    trees = {}

    def recorded_fuse(runs, **kwargs):
        runs = list(runs)
        result = original_fuse(runs, **kwargs)
        children = kwargs.get("children") or {}
        trees[id(result)] = {"runs": [{"name": name, "ids": ids, "weight": weight,
                                      "child": trees.get(id(children[name])) if name in children else None}
                                     for name, ids, weight in runs],
                             "raw": kwargs.get("raw_scores") or {}}
        return result

    engine.fuse = recorded_fuse

    def generate(*args, **kwargs):
        trees.clear()
        result = original(*args, **kwargs)
        fusion, _, trace, _ = result
        scores = {c.doc_id: {"score": c.score, "raw": dict(c.raw), "sources": dict(c.sources)}
                  for c in fusion.ordered}
        # 중첩 RRF에서 child raw가 사라져도 trace에 있는 원점수는 보존한다.
        for source, values in trace.items():
            if isinstance(values, list) and source.endswith(("_dense", "_lexical")):
                for v in values:
                    if isinstance(v, dict) and v.get("doc_id") in scores:
                        scores[v["doc_id"]]["raw"][source] = v["score"]
        trace["parity_scores"] = scores
        trace["parity_tree"] = trees[id(fusion)]
        return result

    engine._generate_candidates = generate
    original_pack = engine.pack_evidence_set
    def pack(*args, **kwargs):
        engine._parity_pack_call = (args, kwargs)
        return original_pack(*args, **kwargs)
    engine._parity_original_pack = original_pack
    engine.pack_evidence_set = pack


def packing_variants(engine, index, hits, scores):
    """근접 원점수 순열의 포장을 샌드박스로 독립 재계산한다(최대 top-5!).

    Go 결과를 입력으로 쓰지 않는다. 원문/출처/합쳐진 leaf 집합은 고정하고,
    순서에 따른 이미 보인 원문 제거와 예산 배분만 다시 계산한다.
    """
    args, kwargs = engine._parity_pack_call
    messages, segments = args
    by_id = {hit["doc_id"]: hit for hit in hits}
    seg_by_id = {seg["doc_id"]: seg for seg in segments}
    ids = list(by_id)
    pos = {doc_id: i for i, doc_id in enumerate(ids)}
    def near(a, b):
        return abs(a-b) <= 1e-6 * max(abs(a), abs(b))
    def tied(x, y):
        a, b = scores[x], scores[y]
        return near(a["score"], b["score"]) or any(
            source in b["raw"] and near(value, b["raw"][source])
            for source, value in a["raw"].items())
    variants = []
    for order in permutations(ids):
        if list(order) == ids:
            continue
        if any(pos[x] > pos[y] and not tied(x, y)
               for i, x in enumerate(order) for y in order[i+1:]):
            continue
        packed = engine._parity_original_pack(messages, [seg_by_id[doc_id] for doc_id in order], **kwargs)
        rows = []
        for doc_id, expanded in zip(order, packed["docs"]):
            hit = dict(by_id[doc_id])
            leaf = index.leaf_by_id[doc_id]
            hit.update(excerpt=expanded.get("excerpt") or "", status=expanded.get("status") or "insufficient",
                       visible=expanded.get("visible_observation_ids") or [],
                       date=engine._context_only_date(index, expanded, leaf) or engine._leaf_source_date(index, leaf))
            rows.append(hit)
        variants.append(rows)
    return variants


def simulate_dot(index, np, method):
    """OS 재현이 아닌, 내적 합산 정밀도/순서만 바꾸는 오프라인 민감도 실험."""
    class Simulated(np.ndarray):
        def __matmul__(self, q):
            a = np.asarray(self)
            if method == "float64":
                return a.astype(np.float64) @ np.asarray(q, dtype=np.float64)
            if method == "ulp1":
                # 특정 BLAS의 재현이 아니다. 같은 float32 내적의 마지막 비트를
                # 행마다 교대로 ±1 ULP 움직이는 민감도 실험이다.
                sims = a @ q
                direction = np.where(np.arange(sims.size).reshape(sims.shape) % 2, -np.inf, np.inf)
                return np.nextafter(sims, direction.astype(np.float32))
            products = a * q
            if method == "reverse32":
                return np.sum(products[..., ::-1], axis=-1, dtype=np.float32)
            if method.startswith("blocks"):
                block = int(method[6:])
                shape = products.shape[:-1]
                total = np.zeros(shape, dtype=np.float32)
                for start in range(0, products.shape[-1], block):
                    total += np.sum(products[..., start:start + block], axis=-1, dtype=np.float32)
                return total
            raise ValueError("unknown simulation")
    index.leaf_vectors = index.leaf_vectors.view(Simulated)
    if index.summary_vectors is not None:
        index.summary_vectors = index.summary_vectors.view(Simulated)
