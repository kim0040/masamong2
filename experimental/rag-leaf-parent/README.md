# BETA — 테스트 샌드박스 (leaf→parent)

**제품 관리자 GUI가 아니다.** 관리 콘솔은 `internal/transport/web`(8787). 이 폴더는 포트 **8765** 실험 UI다.

이름은 **테스트 샌드박스**다. 예전에 “RAG 플레이그라운드”라고 불렀다.  
역할·경계·도구 표: [docs/TEST_SANDBOX.md](../../docs/TEST_SANDBOX.md) · [docs/REPO_MAP.md](../../docs/REPO_MAP.md).

**상태: 베타 실험.** 여기서 고른 recipe가 자동으로 제품 기본값이 되지는 않는다.
**RAG화 산출은 `test-sandbox/data/`에만 둔다.** 제품 `--data-root`와 섞지 않는다. 원문·벡터·ledger·실 `config.json`은 커밋하지 않는다.

## 무엇을 실험하나

1. leaf로 검색한 뒤 parent로 원문 확장
2. (선택) leaf 요약 hybrid RRF
3. 질문 유형별 재순위 R0/R1/R2 (R2 ≠ R0+R1) · auto v2 (R0 v2 후보 관찰 → finish / R2 / recovery)
4. 제품과 **같은 Action ID·`!` 명령** (`contracts/sandbox/tool-parity.json`)
5. SSE 스트리밍 답변
6. 채널별 새 대화·세션 요약 (`conversations.json`, Discord 인덱스와 별개)
7. 플래너 `memory_requirement`(none/optional/required)로 잡담엔 검색 0회, R2 `rerank_min_score` 하한으로 근거 없으면 abstain (`rag_state`)

제품 고도화 설계는 [`tools/evaluation/rag_upgrade/`](../../tools/evaluation/rag_upgrade/)와 [`docs/rag/`](../../docs/rag/README.md).

## 실행

Finder에서 **이 폴더의 `start.command`** 또는 `../../test-sandbox/start.command`. 저장소 루트 `start-sandbox.command`도 같다. Terminal이 뜨고 브라우저가 `http://127.0.0.1:8765` 를 연다. 기본은 `prepare`를 건너뛴다(`SANDBOX_PREPARE=1` 이면 인덱스 준비).

```bash
cp config.example.json config.json
./start.command
# 또는
python3 prepare.py --force    # → ../../test-sandbox/data/
python3 app.py                # http://127.0.0.1:8765
```

prepare가 돌면 CLI와 샌드박스 상단에 퍼센트·남은 개수·대략 남은 시간이 같이 갱신된다. 인덱스가 없어도 서버는 켜진다.

검색 버전은 UI에서 고른다. 기본 **auto**(v2: R0 후보 풀을 만들고 평가한 뒤 R0로 끝내거나 R2/복구, [설계](../../docs/rag/retrieval-v2.md)). R1은 ablation 전용이라 auto가 고르지 않는다. 고정 R2·auto 모두 리랭크 실패 시 R0. 모델은 `config.json` `models`(llm/planner/summary/embed/rerank)로 교체 — [TEST_SANDBOX](../../docs/TEST_SANDBOX.md#모델-교체-configjson-models). 턴 로그는 데이터 디렉터리 `debug_turns.jsonl` (커밋 금지).

Discord 토큰 없음. 인덱스가 있는 채널만 AI.
