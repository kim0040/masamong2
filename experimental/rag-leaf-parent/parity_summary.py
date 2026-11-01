"""원문을 출력하지 않는 parity 숫자 요약: python parity_summary.py dump numeric.json."""
import json
import sys


def summarize(dump_path, numeric_path):
    with open(dump_path, encoding="utf-8") as f:
        dump = json.load(f)
    with open(numeric_path, encoding="utf-8") as f:
        rows = json.load(f)
    # 메타데이터도 명시한 필드만 출력한다. 임의의 덤프 문자열은 출력하지 않는다.
    meta = dump.get("metadata", {})
    safe = {key: meta.get(key) for key in ("os", "architecture", "python", "numpy", "blas")}
    print(json.dumps(safe))
    fusion_diff = hits_diff = 0
    max_dense_error = max_go_gap = max_np_gap = 0.0
    print("question mode rank item dense_go dense_np bm25_go bm25_np rrf_go rrf_np final_go final_np")
    for row in rows:
        fusion_diff += row["fusion_go"] != row["fusion_np"]
        hits_diff += row["hits_go"] != row["hits_np"]
        for item, score in row["scores_go"].items():
            expected = row["scores_np"].get(item, {})
            a, b = score["raw"].get("leaf_dense"), expected.get("raw", {}).get("leaf_dense")
            if a is not None and b is not None:
                max_dense_error = max(max_dense_error, abs(a - b))
        pos = {item: i for i, item in enumerate(row["fusion_np"])}
        for i, item in enumerate(row["fusion_go"]):
            if pos.get(item) == i:
                continue
            key = str(item)
            a, b = row["scores_go"][key], row["scores_np"].get(key, {})
            if not b:
                continue
            # 출력은 문항 번호/항목 번호/숫자와 고정 모드 이름만이다.
            values = [a["raw"].get("leaf_dense", 0), b["raw"].get("leaf_dense", 0),
                      a["raw"].get("leaf_lexical", 0), b["raw"].get("leaf_lexical", 0),
                      a["score"], b["score"], row["final_go"].get(key, a["score"]),
                      row["final_np"].get(key, b["score"])]
            print(row["question"], row["mode"], i + 1, item, *(format(v, ".17g") for v in values))
            for other in row["fusion_go"][i+1:]:
                if item in pos and other in pos and pos[item] > pos[other]:
                    c, d = row["scores_go"][str(other)], row["scores_np"][str(other)]
                    if "leaf_dense" in a["raw"] and "leaf_dense" in c["raw"]:
                        max_go_gap = max(max_go_gap, abs(a["raw"]["leaf_dense"] - c["raw"]["leaf_dense"]))
                        max_np_gap = max(max_np_gap, abs(b["raw"]["leaf_dense"] - d["raw"]["leaf_dense"]))
    print(json.dumps({"runs": len(rows), "fusion_raw_diff": fusion_diff, "hits_raw_diff": hits_diff,
                      "proof_failures": sum(not row["proof"] for row in rows),
                      "max_dense_error": max_dense_error, "max_go_inverted_gap": max_go_gap,
                      "max_np_inverted_gap": max_np_gap}))


if __name__ == "__main__":
    summarize(*sys.argv[1:])
