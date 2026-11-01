# RAG 평가

평가는 검색 후보, 모델에 실제로 보인 근거, 답변 정확성, 보류 판단을 분리해 측정한다. 로컬 원문과 평가 질문은 `.private/` 또는 `.masamong-scratch/`에 두고 Git에 넣지 않는다.

## 검토 세트

- 질문, 기대 근거 ID, 금지 근거 ID, 보류 여부와 검색 범위를 기록한다.
- 합성 질문은 `synthetic`, 사람이 확인하지 않은 항목은 `unreviewed`로 표시한다.
- 개발에 사용한 문항은 같은 설정을 판정하는 holdout으로 재사용하지 않는다.
- 여러 방과 플랫폼을 평가하되 실제 운영 채널 이름과 대화 본문을 보고서에 쓰지 않는다.

## index 바인딩

1. leaves, vectors, recipe로 index fingerprint를 만든다.
2. 평가 세트의 fingerprint가 현재 index와 다르면 채점을 거부한다.
3. 분할 방식만 비교할 때는 원문 관찰 ID와 방 범위가 같은 corpus binding을 별도로 확인한다.
4. 기대 근거 ID가 현재 index에 없으면 다른 leaf로 임의 치환하지 않는다.

## 지표

- Recall@k, MRR, 후보 pool recall
- 모델 입력에 실제로 포함된 visible evidence
- 답변의 의미상 support와 인물 귀속
- 시간 범위 위반과 scope 누출
- 답이 없는 질문의 보류 정확도
- p50/p95 지연 시간과 공급자 비용

fallback이 발생한 문항은 표시하고 설정 비교에서 같은 기준으로 제외하거나 포함한다. LLM judge는 보조 신호로만 사용하고, 승격 전 실패 사례를 사람이 확인한다.

## 실행

공용 평가 코드는 `tools/evaluation/rag_upgrade/`에 있다. 입력과 출력 경로는 명령행에서 명시한다.

```bash
python3 tools/evaluation/rag_upgrade/run_retrieval_v2_eval.py \
  --gold <reviewed-gold.json> --plans <plans.json> \
  --variants R0,R2 --embed-cache <cache.json> --out <output-dir>

python3 tools/evaluation/rag_upgrade/summarize_generalization.py \
  --runs <run-dir> --variant R2 --drop-fallback
```

새 recipe는 같은 문항에 대해 baseline과 짝지어 비교한다. 데이터셋, 검수 상태, 설정, 품질, 비용, 지연 시간, 회귀 사례와 승격 결정을 함께 남긴다.
