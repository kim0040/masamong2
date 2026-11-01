# 기억 검색과 RAG

제품 구현은 `internal/memory/*`와 `internal/chat`에 있습니다. `experimental/rag-leaf-parent`는 검색 변경을 비교하는 테스트 샌드박스이며 제품 데이터나 제품 런타임이 아닙니다.

## 현재 구조

```text
카카오 CSV / Discord 원문
  → 원문 관찰과 revision 저장
  → 방 안에서 leaf 분할
  → lexical + 선택한 embedding space
  → 허용된 방·블록 범위에서 후보 생성
  → weighted RRF와 선택적 재순위
  → 원문 범위 확장과 권한 재검사
  → 답변
```

## 문서

| 문서 | 내용 |
|---|---|
| [retrieval-v2.md](retrieval-v2.md) | 후보원, 결합, auto 결정, 시간·인물 범위와 fallback |
| [source-lifecycle.md](source-lifecycle.md) | 카카오 배치와 Discord bootstrap·실시간 증분 처리 |
| [memory-blocks.md](memory-blocks.md) | 기억 블록과 채널 연결 계약 |
| [evaluation.md](evaluation.md) | gold, index binding, holdout과 평가 도구 |
| [embedding-width-2026-10.md](embedding-width-2026-10.md) | 임베딩 폭 측정과 결정(기본 1024), 메시지 벡터 512 측정 |
| [../memory-operations.md](../memory-operations.md) | 운영자가 블록을 만들고 준비·연결·복구하는 절차 |

## 불변 원칙

1. 입력은 허용된 카카오 CSV와 Discord 원문 관찰만 사용합니다. 과거 window, embedding, claim, summary, chunk와 vector store는 입력이나 fallback으로 사용하지 않습니다.
2. 서로 다른 Discord 채널, 카카오 방, 계정과 표시 이름을 자동으로 합치지 않습니다.
3. 채널 연결은 검색 범위만 정하며 원문 이동, 인물 병합 또는 역방향 권한을 만들지 않습니다.
4. 요약과 별칭은 검색 신호입니다. 답변 근거는 허용 범위 안의 원문입니다.
5. capture는 embedding 성공과 분리합니다. 모델이 없어도 원문과 lexical 경로는 유지합니다.
6. 검색 결과를 답변에 넣기 직전에 scope, revision, 제외와 tombstone을 다시 확인합니다.
7. 검색 recipe를 바꿀 때는 샌드박스 비교, Go 반영과 parity 테스트를 함께 수행합니다.
8. 합성·미검수 gold는 사람 검수 품질을 증명하지 않습니다.

개인정보와 원문 취급은 [원문 데이터 정책](../RAW_DATA_POLICY.md)을 따릅니다.
