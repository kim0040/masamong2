# RAG 파일럿 평가 하네스

원문 corpus에 붙는 오프라인 프로토타입이다. 제품 importer·운영 채널 연결·실제 답변 품질 구현과 구분한다. **관리 콘솔도, 8765 테스트 샌드박스 UI도 아니다.** 위치는 [저장소 지도](../../docs/REPO_MAP.md), 평가 기준은 [RAG 평가](../../docs/rag/evaluation.md), 원문 제한은 [원문 정책](../../docs/RAW_DATA_POLICY.md)을 따른다.

## 기본 원칙

- Kakao/Discord와 source room은 독립이다. collection은 권한이나 인물 연결이 아니다.
- `channel_scope.py`의 명시적 view와 source→destination/purpose grant로 함께 검색할 채널을 선택한다. 역방향·전이 연결을 추론하지 않는다. 해제/철회는 이전 scope와 검색 캐시를 무효화한다.
- view로 연결해도 `windows.py`는 원래 채널 안에서만 문맥을 만든다. 각 window는 원문 observation ID와 source provenance를 보존한다.
- 현재 실제 채널 mapping은 설정하지 않았다. `fixtures/evaluation/`은 synthetic이다.
- 원문과 기존 `.private/eval/` 산출물은 보존한다. 새 실행은 격리된 `EVAL_OUTPUT_DIR`를 사용한다. 실제 본문·질문·벡터·개인 ID가 담긴 산출물을 Git에 넣거나 stdout에 출력하지 않는다.

## API 호출 없는 검증

```bash
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tools/evaluation -p 'test_*.py'
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tools/evaluation/rag_upgrade -p 'test_*.py'
PYTHONDONTWRITEBYTECODE=1 python3 tools/evaluation/verify_raw_windows.py --size 20 --overlap 0
PYTHONDONTWRITEBYTECODE=1 python3 tools/evaluation/verify_raw_windows.py --size 20 --overlap 4
```

첫 두 명령은 synthetic/mock tests다 (`rag_upgrade`는 2026-09-16 고도화: Cohere adapter·gold v2·leaf ID·planner/proactive 계약). 뒤 두 명령은 로컬 `.private/raw-corpus/`의 날짜 있는 관찰을 읽어 의미 입력 후보의 누락·중복·채널 혼합을 검사하고 집계만 출력한다. 원문·캐시를 수정하거나 외부 모델을 호출하지 않는다. 이 검사는 의미 분류기의 정확도나 답변 품질을 입증하지 않는다.

RAG 설계·평가·실험 기록: [docs/rag/](../../docs/rag/README.md). live Cohere smoke(합성 문서만): `python3 tools/evaluation/rag_upgrade/smoke_rerank_live.py` (OPENROUTER만, Discord 토큰 금지).

샘플 실인덱스 ablation은 **index-bound gold만** 허용한다 (임시 probe/stale remap 금지):

```bash
python3 tools/evaluation/rag_upgrade/regen_bound_gold.py --questions 20 --budget-usd 1.5
PYTHONUNBUFFERED=1 python3 -u tools/evaluation/rag_upgrade/run_sample_dataset_ablation.py --budget-usd 1.0
```

규칙: [docs/rag/evaluation.md](../../docs/rag/evaluation.md) §3. 결과는 `.masamong-scratch/rag-upgrade-ablation-20260916/` (git 제외).


## 구성

| 파일 | 역할 |
|---|---|
| `corpus.py` | raw 로더와 후보 분류, 원래 출처 ordinal 보존 |
| `windows.py` | 명시적 크기/overlap, 방별 buffer, 말단 구간, 원문 refs |
| `artifacts.py` | 내용·순서·모델·recipe fingerprint, checksum/shape/finite cache 검증 |
| `channel_scope.py` | revisioned view/link/grant와 검색 전 scope 제한·반환 전 재검사 |
| `evidence_metrics.py` | observation ID 기반 다중 근거 회수·금지 근거·split 검사 |
| `verify_raw_windows.py` | 실제 corpus 전체의 window membership 검사 |
| `run_retrieval_pilot.py` | 합성 질문의 단일 정답 window 검색 탐색 |
| `analyze_hybrid.py` | 검증된 artifact를 사용하는 offline 가중치 비교 |
| `run_person_pilot.py` | 표시명 존재 진단. 인물 답변 정확도가 아님 |
| `run_identity_impact.py` | 계정 window·별칭 확장 진단. 자동 identity 해결이 아님 |
| `analyze_real_queries.py` | 기존 로그 질문의 범주 분류. 검색 필요 여부와 구분 |
| `run_korean_fts.py`, `lexical.py` | 한국어 lexical probe와 SQLite FTS |
| `openrouter.py` | 명시적 요청별 비용 예약과 응답 유효성 검사 |

## 평가 의미

`evidence_metrics`의 k는 중복 제거한 observation 수다. top-k window 평가와 혼동하지 않는다. 같은 observation이 중첩 window에 반복되어도 한 번만 센다. 같은 화자·같은 이름·같은 본문만으로 정답이 되지 않는다. 여러 근거가 필요한 문항은 모든 필수 근거 그룹의 회수를 따로 검사한다.

Synthetic 및 미검수 gold는 제품 qualification의 근거가 될 수 없다. 빈 검색 결과는 답변기가 적절히 답변을 보류했다는 증거가 아니다. Wrong-person attribution, 답변 support, temporal correctness와 B3 효용은 별도 사람 검수 및 실제 모델 평가로 남긴다.

## 모델 호출과 캐시

실제 모델 호출은 명시된 source·목적·모델·예산 안에서만 실행한다. 캐시 개수나 파일명 TAG만으로 재사용하지 않는다. sidecar 없는 과거 벡터는 역사적 실험 산출물이며 새 결과에 자동 승격하지 않는다.

`OpenRouter`의 `budget_usd`는 인스턴스별 로컬 예산이다. 호출별 보수적인 `max_request_cost_usd` 또는 `EVAL_MAX_REQUEST_COST_USD`가 없으면 유료 호출이 차단된다. 추정은 실제 요청 크기와 확인한 가격을 근거로 설정해야 한다. 미확인 과금은 0으로 처리하지 않으며 재조정 전 새 작업을 차단한다. 이 제한은 여러 프로세스·재시작의 누적 ledger나 provider 계정의 실제 과금 상한을 보장하지 않는다.

현재 파일럿에서 관찰한 모델 비용·reasoning·라우팅 동작을 다른 시점/모델/provider의 보장으로 사용하지 않는다. `effort=low`의 품질 무손실을 확인한 것은 아니다.
