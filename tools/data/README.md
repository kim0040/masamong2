# 원문 준비 도구

`prepare_raw.py`는 신뢰된 로컬 snapshot과 카카오 CSV를 새 private corpus로 변환하는 오프라인 도구다. 네트워크와 모델 API를 사용하지 않으며 기존 임베딩, 요약, window를 가져오지 않는다.

```sh
cp tools/data/raw-sources.example.json tools/data/raw-sources.json
# 로컬 파일 경로, SHA-256, 행 수와 방 ID를 채운다.
python3 tools/data/prepare_raw.py \
  --snapshot .private/source-snapshot \
  --config tools/data/raw-sources.json \
  --output .private/raw-corpus
python3 -m unittest discover -s tools/data -p 'test_*.py'
```

`raw-sources.json`에는 개인 파일 경로와 검증값이 들어가므로 Git에서 제외한다. 추적되는 `raw-sources.example.json`에는 형식만 담는다.

출력은 collection별 `dated.jsonl.gz`, `undated.jsonl.gz`와 manifest다. 서로 다른 source room의 같은 내용은 별도 observation으로 보존한다. 기존 출력에 이어 쓰지 않으며 hash, 행 수, readback 검증을 통과한 완성 결과만 사용한다.
