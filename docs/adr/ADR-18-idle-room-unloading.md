# ADR-18: 안 쓰는 방 내려놓기 — 색인 하나를 "상주 방 집합"으로 다시 만든다

- 날짜: 2026-10-13
- 상태: **초안 — 0단계 완료(계약·측정 코드·게이트 도구·실측), C-5 게이트 보고에서 멈춤(2026-10-13)**. C-1 계약은 Fable 최종 검토 SOUND를 받았다. C-2 측정 진단과 C-3 게이트 도구는 다른 모델 검토를 반영했다. C-4 실측은 §13에 있다. 1단계 이후(지시서 D)는 사용자 명시 승인 전에는 시작하지 않는다. 서비스 동작은 아직 바뀌지 않았다(예산 무한, 전체 상주).
- 관련 문서: 다음 묶음 계획 §7(질문 원문·실험 계획 — 정리되어 Git 기록 `ae3b59a3:docs/handoff/2026-10-13-next-batch.md`에만 있음), [기억 운영](../memory-operations.md) §16(저사양 보강, hot 재구축의 행 공유, 실측 피크), [임베딩 폭 측정](../rag/embedding-width-2026-10.md), [남은 일](../BACKLOG.md), [ADR-17](ADR-17-derived-room-memory.md)(형식)
- 작성: Fable 5.1(설계 판단). 실험 수치는 코디네이터가 2026-10-13 Windows에서 잰 것이다(§5). 1차 초안을 Sol(GPT-6.1)이 코드와 대조해 검토했고(판정 REVISE), 이 판은 그 검토를 반영한 것이다(§11).

## 1. 배경

- 기억 색인은 **하나**다. `Engine.idx atomic.Pointer[search.Index]`(`internal/memory/engine/engine.go`)가 가리키는 `search.Index`(`internal/memory/search/index.go`)가 모든 방의 메시지·leaf·벡터·메시지 벡터·어휘 색인(FTS5)·화자·identity 표·시간 정렬·`corpusTMax`를 함께 갖는다.
- 방 범위(`Scope.Rooms`)는 경로마다 다르게 걸린다. dense(`denseTop`, `vectors.go`)와 메시지 벡터는 **범위의 행만 골라 점수를 낸다**. 어휘 검색은 FTS5 조회 뒤 범위로 **거른다**(limit을 전체 leaf 수로 키운 뒤 필터, `retrieve.go`).
- 어휘 점수는 **전체 방 통계**다. `internal/memory/lexical/index.go`는 메모리 안 SQLite FTS5 하나이고 `bm25(docs)`로 점수를 매긴다. 희귀도(IDF)와 평균 길이가 올라온 모든 방 문서 기준이다. 샌드박스(`experimental/rag-leaf-parent/engine.py`)도 FTS5 하나다.
- identity·alias 표는 **올라온 메시지**로 만든다(`person.go` `buildIdentityTable`: leaf 안 메시지의 계정·표시명). `aliasTable`은 같은 계정에서 본 표시명 둘 이상을 묶고, 어휘 질의 확장(`lexicalQueryExpanded`)과 인물 해석(`resolvePersons`)이 이것을 쓴다. 어떤 방을 내려놓으면 그 방에서만 보인 표시명이 표에서 빠진다.
- 시간 창(`narrowRecentOne`, `timescope.go`)은 범위 안 leaf의 최대 시각을 기준으로 잡고, 방이 여럿이면 방별로 잡아 합친다. 전역 `corpusTMax`는 범위가 비었을 때의 대체값일 뿐이다.
- 순위 합치기는 RRF(순위 기반, `fusion.go`)다. 원점수는 추적에만 쓴다. 따라서 bm25 통계가 바뀌면 어휘 run의 순위가 바뀌고, 그 순위가 RRF에 들어간다.
- 재구축(hot)은 변하지 않은 int8 행을 이전 색인과 공유한다. 행은 각각 독립 할당(`qRows [][]int8`)이라 어떤 행을 버리면 그 행만 풀린다(§16). 그러나 메시지 배열(`engine.go` `loadMessages`), 위치 맵, `leafPos`·`leafEndTS`·`byRecency`·identity·화자 맵(`index.go` `Build`·`lazy`)은 빌드마다 새로 만든다. 교체는 `idx.Swap`, 옛 색인의 FTS는 교체마다 별도 타이머로 1분 뒤 닫는다(`closeLater`). 빌더는 `reloadMu` 하나로 직렬화되지만 1분 안의 교체 횟수는 제한하지 않는다.
- FTS는 leaf 하나와 (요약을 켜면) 요약 하나, 세대당 최대 두 개의 메모리 SQLite(modernc, Go 힙 밖 할당)다.
- 실측(§16, 10만 메시지, 메시지 벡터 켬, 1코어): 로드 뒤 heap 208.3MiB, 변경 없는 hot 재구축의 heap 피크 416.9MiB, working set 494.2MiB. **행을 공유해도 피크는 상주의 약 2배, working set은 약 2.4배다.**
- 상주 메모리는 메시지 수에 비례한다(1024 + 메시지 벡터 1024 기준 큰 세트에서 메시지당 약 1.1~1.6KB, 작은 방은 고정비가 커서 더 높다, §5 E4). 질문이 없는 방(가져오기만 한 카톡 기록, 조용해진 채널)도 늘 메모리에 있다.
- 사용자 의도: 몇 년 24시간 운영에서 안정적·지속 가능한 성능과 유지보수. **품질을 깎아 메모리를 얻지 않는다.** 새 사용자 설정을 만들지 않는다. 기기 단계(small·medium·large)는 이미 있다.

## 2. 결정(권고안) 요약

1. **단위: 색인은 하나로 두고 "상주 방 집합"으로 다시 만든다.** 방별 색인으로 나누지 않는다. 방을 넣고 빼는 것은 지금의 hot 재구축이다.
2. **identity·alias 표는 상주와 무관하게 유효 leaf의 전체 방 합집합으로 유지한다.** 같은 recipe·space의 전체 빌드와 같은 입력이다. 살아 있는 원문 전체 DISTINCT는 쓰지 않는다(§3.2).
3. **bm25 통계는 상주 집합 기준이 된다. 이것은 동작 변경이다.** 전체 상주일 때만 지금과 같다. 부분 상주에서는 같은 방 안 어휘 순위가 바뀔 수 있다. E1·E3은 그 변화가 작다는 **탐색 근거**이지 무손실의 증거가 아니다. 받아들일지는 사용자 결정이다(§9 질문 2).
4. **검색은 "필요한 방이 모두 들어 있는 같은 스냅샷"을 받는다.** `EnsureRooms(ctx, rooms)`가 스냅샷을 돌려주고, 돌려준 방을 보호(pin)한다. 검색은 그 스냅샷으로만 끝낸다(§3.6).
5. **정책: 예산을 넘을 때만, 보호되지 않은 방 중 오래 안 쓴 방부터 내려놓는다.** 예산 안이면 지금과 똑같이 동작한다. 예산 비율은 단계 0 측정 뒤 확정한다(초기값 20%, §9 질문 3).
6. **서비스 단위 관리자가 모든 빌드와 세대 수명을 소유한다.** 현재 1·은퇴 1·빌드 1개가 상한이다. 보유 수가 0일 때만 FTS를 닫는다. 설정 교체에도 예외가 없다(§3.6).
7. **첫 질문은 ctx와 대기 상한 안에서 필요한 방을 기다린다.** 실패는 빈 검색 결과로 숨기지 않는다. 살아 있는 턴은 `memory_unavailable`로 제한된 답을 만든다는 계약을 제안한다. 활성화 승인은 C-5에서 받는다(§3.9·§9 질문 4).
8. **단계 0(측정·품질 게이트)을 먼저 한다.** 내려놓기는 게이트를 통과한 뒤에만 켠다(§7).
9. **진단: `runtime.doctor`에 `memory_residency` 행, 콘솔 블록 목록에 상주 상태.** "준비됨(색인됨)"과 "상주함(메모리에 있음)"을 구분한다.
10. C(메시지 벡터 512)는 **2026-10-13 적용 결정**이다. 별도 작업 A·B에서 다룬다. C-4는 적용 뒤 기준으로 잰다(§9 질문 5).

핵심 한계를 먼저 밝힌다. **이 설계는 "안 쓰는 방"만 줄인다.** 활발한 방이 모두 최근에 쓰였거나, 보호 방만으로 예산을 넘으면 절감이 0이다(§6). 활발한 방 자체의 성장은 다른 수단(§10)이 필요하다.

## 3. 질문별 선택지와 권고

### 3.1 단위 — 방별 색인 vs 상주 방 집합 하나

| | (A) 방별 색인 | (B) 상주 집합 하나 (**권고**) |
|---|---|---|
| 올리기·내리기 | 포인터 추가·제거. 그 방만 읽음 | hot 재구축. 변하지 않은 벡터 행은 공유, 새 방만 디스크에서 읽음. 메시지 배열·맵·FTS는 상주 전체를 다시 만듦 |
| 검색 코드 | `search` 패키지 대수정: 여러 Index에 질의 후 합치기, 시간 창·화자·identity 표가 방마다 | **소규모 변경.** 전역 identity 표를 외부에서 넣는 훅 하나(§3.2). `Scope.Rooms` 필터는 그대로 |
| 어휘 통계 | 방별 IDF. 범위가 여러 방이면 bm25 비교 불가(§3.3) | 상주 집합 IDF. 범위의 방이 모두 같은 색인. **부분 상주에서는 순위가 바뀜(동작 변경)** |
| parity | 샌드박스가 전체 하나라 방 범위 문항마다 깨짐. 샌드박스도 방별로 쪼개야 함 | 전체를 올리면 지금과 동일(같은 데이터·설정·시간 참조·삽입 순서로 같은 `Build`를 부름). 부분 집합 parity는 추가 작업(§7) |
| 교체 중 피크 | 그 방 하나 | 상주 ×2(§1 실측) + 은퇴 세대 FTS + 복귀 방. 진입 제어가 필요(§3.6) |
| 코드 위험 | 높음(검색·parity·골든) | 중간(엔진 로드 경로·동시성 계약·정책). 검색 자체는 거의 그대로 |

**권고 B.** 이유: (1) 전체 상주일 때 검색 동작·골든·parity가 지금과 같다. (2) 여러 방 범위(스레드+부모 채널+붙인 블록, `channelRooms`)가 지금도 흔하다. A는 합치기 문제(§3.3)를 새로 만든다. (3) 부분 상주의 동작 변경은 bm25 통계 하나로 한정되고, identity·alias는 전역으로 유지할 수 있다(§3.2).

A를 기각하는 결정적 이유는 메모리가 아니라 **검색 동작의 재정의**다. 방 안의 어휘 순위와 시간 창 계산이 바뀌고, 샌드박스를 같은 구조로 다시 만들어야 parity를 유지할 수 있다.

### 3.2 어휘 통계(IDF)와 identity·alias 표

**두 가지를 나눈다.** 어휘 통계는 상주 집합에 따라 바뀌는 것을 받아들이되, identity·alias는 전역으로 고정한다.

**(1) identity·alias 표 — 전역 유지(권고).**

- 지금: `buildIdentityTable`은 색인의 leaf 안 메시지에서 (플랫폼, 계정, 표시명) 조합을 모은다. `aliasTable`은 그중 `labelsByKey`(계정 → 표시명 집합)만 쓴다. `keysInScope`·`speakersInScope`는 범위 안 leaf로 거르므로 범위의 방만 상주하면 된다.
- 입력·갱신·비용은 아래 계약으로 정한다. 원문 전체 DISTINCT를 쓰던 1차 제안은 이 계약으로 대체한다.
- 대안(기각): "계정의 다른 표시명을 본 방"을 질문 때마다 올린다 — 어느 방인지 알려면 결국 전역 표가 필요하다.

| 결정 | 계약 | 근거 코드(현재) |
|---|---|---|
| 입력 집합 | 같은 recipe·embedding space·차원으로 **전체 빌드에서 최종 채택한 leaf**의 유효 `[Start, End)` 구간 합집합만 쓴다. 미임베딩·공간 불일치·불정합으로 빠진 leaf는 제외한다(**메타 기준 적격성**. 메타 읽기와 벡터 읽기 사이의 경쟁으로 빠지는 leaf(`engine.go:609` default 분기)는 캐시가 재현하지 않고 무효화 키로 잡는다. C-3 동등성 테스트는 정지 상태 저장소에서 돈다). 열린 꼬리도 실제 leaf로 채택됐을 때만 들어간다 | `internal/memory/store/store.go:1685`, `internal/memory/engine/engine.go:456`, `internal/memory/engine/engine.go:599`, `internal/memory/search/person.go:240` |
| 같은 선택 규칙 | 전체 빌드와 측정용 부분 빌드가 같은 leaf 적격성 선택 함수를 쓴다. 차원 선택도 전체 메타 기준으로 먼저 확정하고 부분 필터는 나중에 한다. 단순 벡터 존재 검사로 대체하지 않는다. 비상주 방은 메시지 순서·관측 ID·작성자 메타와 최종 leaf 적격성만 스트리밍한다. 본문·벡터 행은 캐시에 남기지 않는다 | `internal/memory/engine/engine.go:460`, `internal/memory/engine/engine.go:477`, `internal/memory/engine/engine.go:490`, `internal/memory/engine/engine.go:609` |
| 키·표 | `keyForMessage`의 원래 방 플랫폼·계정 공백 제거·표시명 대체 규칙을 그대로 쓴다. 전역 `labelsByKey`·`keysByLabel`은 방별 캐시의 합집합이다. `leavesByKey`는 상주 leaf만 가지며 범위 필터는 그대로다. 전체 상주에서는 세 표와 `aliasTable`이 지금과 같다 | `internal/memory/search/person.go:218`, `internal/memory/search/person.go:240`, `internal/memory/search/person.go:297`, `internal/memory/search/person.go:457` |
| 무효화 | 캐시 키는 방 revision·active recipe·space·차원·leaf 적격성 세대다. 편집·삭제·철회·방 삭제·recut·임베딩 저장·공간 축소 때 상주와 무관하게 무효화한다. JSON에도 키를 저장한다. 검증되지 않은 재시작 캐시는 쓰지 않는다 | `internal/memory/engine/engine.go:1171`, `internal/memory/engine/engine.go:1198`, `internal/memory/store/store.go:1398`, `internal/memory/store/store.go:1538`, `internal/memory/store/store.go:1601` |
| 반영 시점 | 비상주 방도 다음 identity 빌드에서 갱신하고 **새 스냅샷 발행 전에** 합친다. 검색 획득 시 검증은 **identity 입력에 실제로 영향을 준 키**(계정별 표시명 집합의 변화)로 한정한다. 그 키가 오래됐을 때만 보유를 반환하고 갱신을 기다린다. 메시지 내용의 편집·삭제·철회는 현행대로 hit 방 evidence revision 재검사로 다룬다. 반환 전 전역 키가 바뀐 것은 오류가 아니라 진단 카운트로 남긴다(Fable 최종 검토 P2-2: 모든 방 편집마다 질문을 재구축 뒤로 미루는 더 엄격한 규칙은 별도 동작 변경이라 채택하지 않음). 방 재상주를 기다리지 않는다 | `internal/memory/engine/engine.go:913`, `internal/memory/engine/engine.go:923`, `internal/memory/store/room_state.go:127`. 1단계 계약이다 |
| 경쟁·실패 | 캐시 계산은 저장소의 일관된 읽기에서 한다. 발행 전 키가 달라지면 결과를 버린다. 획득 후 키 검증이 검색의 기준 시점이다. 콜백 지연은 저장소 검증으로 잡는다. 갱신 실패에는 오래된 표를 새 표로 발행하지 않고 명시적 오류를 준다 | `internal/memory/engine/engine.go:913`, `internal/memory/engine/engine.go:1174`, `internal/memory/store/room_state.go:122` |
| 메모리 비용 | 서로 다른 `(방, speakerKey, 표시명)` 조합 P와 방 수 R에 대해 캐시는 O(P+R), 전역 표도 O(P)다. 방 삭제 때 캐시를 지우고, 갱신은 교체한다. 역사별 JSON을 누적하지 않는다. 기존 규모의 **1MiB 미만은 가정**이며 C-2·C-4에서 실제 바이트와 빌드 중 두 벌의 비용을 잰다 | `internal/memory/search/person.go:234`, `internal/memory/search/person.go:252` |

구현 훅 이름은 `search.BuildWithIdentity`·`store.LoadIdentityInputs`·`engine.InvalidateIdentity`로 정한다(새 함수). 기존 `Build`는 그대로 둔다. C-3에서는 전체 상주 동등성 검증 뒤 측정용 빌드에만 훅을 쓴다. 제품의 획득·무효화 경로 연결은 1단계다.

**(2) bm25 통계 — 상주 집합 기준(조건부 권고, 사용자 결정).**

| 선택지 | 내용 | 판단 |
|---|---|---|
| (a) 상주 집합 통계 (**권고, 조건부**) | FTS5 `bm25()` 그대로. 상주 방이 바뀌면 같은 방 안 어휘 점수와 순위가 바뀔 수 있음 | **동작 변경.** E1·E3에서 R@5 합계 변화는 작았으나 문항 단위 +2/−4 같은 교차가 있다(§5). 단계 0 게이트(§7)를 통과해야 켠다 |
| (b) 전체 문서 빈도표 유지 | FTS5 `bm25()`를 못 씀. 직접 IDF·평균 길이 계산 | 기각. 샌드박스 parity 전부 깨짐. 편집·삭제·철회마다 빈도표 갱신 경로가 필요. 바꾸는 범위가 (a)보다 훨씬 큼 |
| (c) FTS는 전부 상주, 벡터만 내려놓음 | 통계가 전체로 고정 | 기각. 벡터는 힙의 약 절반(discord: 메시지 벡터 33MB + leaf 약 8MB / 92.5MB). 절감이 반으로 줄고 재구축·메시지 배열은 그대로 필요 |

(a)에 붙는 조건.

- **동등성 범위.** "검색 동작 불변"은 **전체 상주일 때만** 주장한다. 같은 데이터·설정·시간 참조·삽입 순서로 같은 `Build`를 부르면 같은 색인이 나온다. 미래 구현의 바이트 동일성은 테스트로 확인한다(기존 골든·parity).
- **결정성.** 같은 상주 집합이면 결과가 같아야 한다. 방 순서는 저장소 순(`Store.Rooms`, 이름순)이고 FTS rowid는 삽입 순이므로 동점 순서도 같다. 테스트: {A,B} → {A,B,C} → {A,B}로 되돌린 뒤 범위 A의 결과가 첫 {A,B}와 같다.
- **parity.** 제품의 `TestSandboxRetrieveParity`는 전체를 올리므로 그대로다. 부분 집합 parity는 **추가 테스트**다: 샌드박스 로더와 Go `LoadSandboxIndex`(`parity_test.go`)에 같은 방 필터(환경 변수, 예: `MASAMONG_SANDBOX_ROOMS`)를 넣고 같은 부분 집합으로 덤프·비교한다. 기존 parity 테스트의 본문은 바꾸지 않고, 로더에 필터 인자가 더해진다.

### 3.3 여러 방 검색

지금 채널 범위는 방 하나가 아니다(`channelRooms`: 채널 방 + 부모/스레드 방 + 붙인 블록의 원래 방). 순위 합치기는 RRF라 **각 run 안의 순위**만 쓴다.

- dense: 코사인 점수가 색인 간에 비교 가능하다. 합칠 수 있다.
- lexical(bm25): IDF가 다르면 점수도, 그 점수로 만든 순위도 비교할 수 없다. 방별 순위를 교차로 섞으면 지금의 전체 순위와 다른 결과가 된다(골든·parity 변화).
- 시간 창(`narrowRecent`)은 방별로 잡아 합치므로 색인이 나뉘어도 같지만, 화자·alias 매칭은 하나의 표 위에서 돈다.

**권고: 합치지 않는다.** 범위의 방이 모두 한 색인에 있도록 한다. `EnsureRooms(ctx, scope.Rooms)`가 빠진 방을 한 번의 재구축으로 함께 올리고, 그 스냅샷으로 검색한다(§3.6).

### 3.4 내려놓기 정책

새 사용자 설정은 없다. 모두 자동이다. 운영자 비상 스위치만 환경 변수로 둔다(`MASAMONG_PERF`와 같은 방식, §7 되돌리기).

| 항목 | 권고 | 근거·주의 |
|---|---|---|
| 예산 | `memlimit.AvailableBytes()`(cgroup/물리 가용량, 소프트 한도의 바탕이 되는 같은 수)의 **20%로 시작**. 모르면 단계별 고정값: small 100MiB, medium 400MiB, large 1.6GiB. 단계 0 측정 뒤 확정 | 소프트 한도는 가용량의 60%(512MiB → 약 307MiB). 재구축 피크가 상주의 약 2배(§1)이므로 예산 25%면 피크만 50%가 되어 사진 디코드(512MB 기기에서 125~170MB)와 겹치면 한도에 닿는다. `memlimit.Current().Bytes`는 Go 소프트 한도이고 FTS(Go 힙 밖)는 거기 들어가지 않는다. 512MB 기기 20% ≈ 100MiB ≈ 메시지 약 8만 개(1.2KB 기준) |
| 상주 비용 추정 | 방별 `RoomStats`(메시지·leaf 수)로 `고정비 + 메시지당 상수`(E4로 보정: 기본 1.2KB, 메시지 벡터 512면 1.0KB, 방당 고정비 약 2~3MB). 실제 힙·FTS 크기와 비교해 진단에 둘 다 표시 | 작은 방은 고정비가 지배한다(363건/3.4MB, 3,530건/8.7MB, §5). 정확한 바이트는 필요 없다. 순서와 한도 판단에만 쓴다 |
| 언제 판단 | 재구축이 끝난 직후와 **scheduler 1분 tick**(`schedule.go`)마다. 색인 주기(`IndexIntervalMin`, `memory.go`)와는 별개다. 한 tick에 상주 변경 빌드 최대 1회. 색인 중(`indexMu` TryLock 실패)·빌드 중(`reloadMu` TryLock 실패)·은퇴 세대 FTS가 아직 열려 있으면 건너뜀 | 재구축을 쌓지 않는다. 잠금 순서는 §3.6 |
| 내려놓지 않는 방(보호) | (1) 최근 **7일** 안에 검색 범위에 들어간 방, (2) 최근 **24시간** 안에 메시지가 온 방, (3) 올린 지 **6시간**이 안 된 방(최소 상주), (4) 색인·동기화가 진행 중인 방, (5) 지금 검색이 pin한 방(§3.6) | (1)(2)는 재방문 가능성. (3)은 핑퐁 방지. (2)의 근거는 "파생 기억이 색인을 읽어서"가 아니다(ADR-17 읽기는 DB 직접, §3.7). 메시지가 오는 방은 곧 질문이 올 가능성이 높아서다 |
| 순서 | 예산을 넘으면 보호되지 않은 방 중 **가장 오래 검색되지 않은 방부터**(LRU), 같으면 큰 방부터. 예산의 80% 아래가 될 때까지 | 단순하고 예측 가능하다 |
| 넘는데 내려놓을 방이 없으면 | 아무것도 하지 않는다. 진단에 `over_budget` 경고 | 품질을 깎지 않는다는 원칙. **보호 방만으로 예산을 넘으면 상한은 작동하지 않는다**(사용자 결정 전제, §9) |
| 올리기 | 질문이 요구하면 예산과 무관하게 올린다. prefetch는 예산 안일 때만 | 질문이 기다리는 동안 정책이 끼어들지 않는다 |
| 기록 | 방별 `last_searched_at`·`loaded_at`·identity 요약을 `memory_room_usage` 표에 둔다. 메모리 캐시에 먼저 쓰고 **1분 tick에 바뀐 방만 묶어서** 저장, 종료 때 flush. 재시작 손실 ≤ 1분(7일 규칙에 무의미). 방 삭제(`memory.blocks.delete`) 때 행 삭제, 고아 행은 tick에서 정리. 마지막 메시지 시각은 `idx_memory_obs_room_live`에서 읽는다 | 검색마다 동기 쓰기는 단일 writer와 SD 쓰기 부담. 마이그레이션 때 **기존 방 전부에 `last_searched_at = now`를 심는다**: 업그레이드 직후 7일은 지금과 같이 전부 상주하고, 그 뒤부터 정책이 든다 |
| 신규 방·기록 없는 방 | 첫 질문 때 올린다(첫 질문 대기). 메시지가 오기 시작하면 24시간 규칙으로 보호 | 가져오기만 한 카톡이 바로 내려가 있는 상태가 목표 |
| 시작 | 첫 로드부터 상주 집합만 만든다 | 시작도 빨라진다(cold 로드는 메시지 수 비례, E4) |

### 3.5 첫 질문 지연

E4(1코어, 메시지 벡터 1024): 복귀하는 방의 cold 읽기는 메시지 1천 개당 약 0.4~0.5초(OS 파일 캐시가 비었을 때), 캐시가 데워지면 약 0.06초. 4만 메시지 방은 17.6초 / 2.6초. 여기에 상주 전체의 메시지 배열·맵·FTS 재구축이 더해진다(10만 변경 없는 hot 재구축 약 7초, §16; 그중 FTS 비중은 단계 0에서 잰다). 지금도 첫 턴은 `EnsureLoaded()`를 기다린다.

| 대응 | 권고 | 비고 |
|---|---|---|
| 미리 올리기(prefetch) | **한다. 단 조건부.** 채널에 메시지가 들어오는 순간(capture) 그 채널 범위(`channelRooms`) 중 **7일 안에 검색된 적이 있고** 상주하지 않는 방이 있으면 백그라운드 로드를 건다. 예산을 넘은 상태면 걸지 않는다. 같은 방은 최소 상주 시간(6시간)마다 한 번만 | 범위에는 붙인 보관 방도 들어간다. 조건 없이 올리면 "보관 방 올림 → 예산 정책이 내림 → 다음 메시지가 또 올림"이 반복된다. 7일 조건으로 한 번도 질문받지 않은 보관 방은 prefetch 대상이 아니고, 질문받은 방은 어차피 7일 보호다 |
| prefetch와 검색 기록 | prefetch는 `last_searched_at`를 찍지 않는다. `loaded_at`만 찍는다 | prefetch가 보호를 연장하면 한 번도 안 쓰는 방이 영원히 남는다 |
| 타이핑 표시 | 턴이 이미 쓰는 표시를 그대로 둔다 | 로드 중에도 같은 표시 |
| 기다리기 | 필요한 방을 원래 ctx와 최대 대기 안에서 기다린다(§3.6). ctx 없는 턴 선행 `EnsureLoaded()`는 1단계에서 없앤다 | 현재 위치: `internal/application/memory.go:1161` |
| 시한 만료·로드 실패 | §3.9의 오류별 정책을 쓴다. 살아 있는 턴은 `memory_unavailable`과 언어팩 안내로 제한된 답을 만든다. 원래 턴 ctx가 끝났으면 답변 생성을 중단한다 | 현재 planner는 기억 오류 뒤 답을 계속 만든다(`internal/chat/runtime.go:270`, `internal/chat/runtime.go:316`). 활성화 승인은 C-5 |

### 3.6 동시성 — `EnsureRooms` 계약, 피크 모델, 빌드 진입 제어

**왜 새 계약이 필요한가.** 지금 `EnsureLoaded`(`engine.go`)는 포인터가 있으면 잠금 없이 돌아오고, `Search`는 포인터를 **다시** 읽은 뒤 외부 임베딩 호출을 거쳐 검색한다. 정책이 방 A를 빼기로 정하고, 검색 쪽이 A가 아직 있는 옛 색인을 확인하고, 그 사이 교체가 일어나고, `Search`가 새 색인을 읽으면 A 없이 검색된다. 포인터는 "이미 잡은 스냅샷의 생존"만 보장하고 "방의 상주"는 보장하지 않는다. 7일 기록은 이 경쟁을 막지 못한다.

**계약.**

```go
// 실재하는 요구 방의 스냅샷과 보유 해제를 돌려준다. 없는 방은 0건이다.
// release까지 스냅샷을 보유하고 실재 요구 방을 pin한다.
func (e *Engine) EnsureRooms(ctx context.Context, rooms []string) (ix *search.Index, release func(), err error)
```

| 항목 | 계약 |
|---|---|
| 정확성의 근거 | `ix`는 `release`까지 유효하다. 같은 recipe·space의 요구 방과 검증된 전역 identity를 갖는다. `Search`는 질의 임베딩(외부 호출, `ix`를 쓰지 않음)을 **먼저** 끝낸 뒤 `EnsureRooms`로 획득하고, 즉시 `defer release()`를 걸어 검색·rerank·근거 재검사까지 같은 `ix`만 쓴다. 순서는 `EmbedQuery → EnsureRooms → Retrieve·재검사`다. 느린 임베딩 응답이 보유를 늘려 은퇴 슬롯을 막지 않게 하려는 것이다(Fable P2-1). 현재 획득/외부 호출/검색 위치는 `internal/memory/engine/engine.go:891`, `internal/memory/engine/engine.go:895`, `internal/memory/engine/engine.go:908` |
| 획득 순서 | 서비스 관리자 `generationMu` 아래서 엔진 활성 여부 확인 → 현재 포인터 읽기 → 요구 방·설정 확인 → **그 Index의 보유 수 증가** → 방 pin 증가를 한 번에 한다. 교체·은퇴·닫기 선정도 같은 잠금이다. 잠금 밖에서 ctx·identity 키를 확인하고 실패하면 전부 반환한다. `idx.Load` 후 따로 보유를 올리는 경로를 금지한다. 교체 지점: `internal/memory/engine/engine.go:676` |
| pin의 역할 | 방 pin은 서비스 관리자에 있고 핑퐁을 막는다. 스냅샷 보유 수는 Index별 공유 수명 레코드에 있고 FTS 생존을 보장한다. `Inherit`로 같은 포인터를 넘겨도 둘 다 같은 레코드를 쓴다. 현재 공유 지점: `internal/memory/engine/engine.go:256`, `internal/memory/engine/engine.go:274` |
| 반환·닫기 | `release`는 `sync.Once`로 중복 호출을 허용한다. 같은 잠금에서 pin·보유 수를 한 번만 내린다. **은퇴했고 보유 수가 0일 때만** 닫을 대상으로 바꾼다. 닫는 동안 은퇴 슬롯을 유지한다. 실제 DB 닫기는 잠금 밖에서 하고 완료 뒤 슬롯을 반환한다. 현재 닫기: `internal/memory/search/index.go:198`, `internal/memory/lexical/index.go:63` |
| 시간 경고 | 5분은 보유 경고·해당 검색 ctx의 취소 요청 시점이다. 강제 닫기가 아니다. 호출이 끝나 `release`될 때까지 다음 빌드는 기다린다. 원문 없이 수·시간만 기록한다. 기존 타이머 대체 위치: `internal/memory/engine/engine.go:307` |
| 느린 경로 | 보유·pin 없이 아래 admission 순서로 등록한다. 알림마다 조건을 다시 검사한다. 자신의 요구 집합을 포함한 빌드가 성공했는데 획득 검증이 경쟁으로 실패한 경우에만 **추가 빌드 1회**를 허용한다. 다른 빌드 알림·설정 은퇴는 빌드 시도 수를 늘리지 않는다. 현재 이중 확인 방식: `internal/memory/engine/engine.go:406` |
| ctx 취소 | 대기자는 원래 `ctx.Err()`로 즉시 돌아온다. 획득/취소가 경합하면 획득한 pin·보유를 반환하고 오류를 준다. 공유 빌드는 서비스 ctx를 쓴다. 한 요청의 취소가 다른 요청의 빌드를 취소하지 않는다. 시작 전 요구는 제거하고 시작한 빌드의 요구 사본은 완료 때 해제한다 |
| 은퇴 | `Retire`는 옛 엔진 신규 획득을 막고 대기자에게 공개 오류 `engine.ErrRetired`를 준다. 대기자 소유 요구·타이머를 제거한다. `Inherit`는 관리자·usage·identity 캐시를 공유한다. 요청은 §3.7 경계에서 다시 등록하므로 옛 `wanted`를 복제하지 않는다. 현재 비공개 오류/은퇴 위치: `internal/memory/engine/engine.go:282`, `internal/memory/engine/engine.go:293` |
| 은퇴 중 빌드 | 발행 전에 엔진 세대를 확인한다. 은퇴면 미발행 Index를 닫고 **`ErrRetired`**를 돌린다. 닫기가 끝나야 빌드 슬롯을 반환한다. 현재 `ix.Close(); return info, nil`은 성공으로 바꾸지 않는다(`internal/memory/engine/engine.go:670`) |
| 어휘 오류 | 새 `lexical.SearchChecked(ctx, q, limit) ([]Hit, error)`를 쓴다. 닫힌 DB·I/O·Scan·rows.Err·잘못된 rowid를 원인 보존 오류로 반환한다. 닫힌 DB는 `ErrIndexClosed`로도 분류한다. 표현식이 비었거나, FTS5가 표현식을 거부했거나(구문 오류, 샌드박스의 `OperationalError` → 0건과 같은 parity 규칙, `lexical/index.go:112`), 정상 조회 0건인 경우는 빈 결과 성공이다. 지금 오류 은폐 위치: `internal/memory/lexical/index.go:122`, `internal/memory/lexical/index.go:131`, `internal/memory/lexical/index.go:137` |

`lexical.Close`는 어휘 잠금 아래 닫힘 상태를 한 번만 설정한다. `SearchChecked`는 같은 잠금에서 그 상태를 검사해 `ErrIndexClosed`를 돌린다. DB에서 뒤늦게 난 오류도 원인 보존으로 반환한다. 오류 문자열로 닫힘을 추측하지 않는다. `SearchChecked` 오류는 `hybrid`·`speakerFusion`·`aliasRun`·q2·`generate`·recovery를 거쳐 `Retrieve`와 `Engine.Search`까지 전파한다. 실패한 lexical run을 dense-only 성공으로 바꾸지 않는다. 근거: `internal/memory/lexical/index.go:63`, `internal/memory/search/retrieve.go:158`, `internal/memory/search/retrieve.go:274`, `internal/memory/search/retrieve.go:391`, `internal/memory/search/person.go:599`, `internal/memory/search/person.go:619`. 이 오류 API 연결도 1단계다. 0단계에서는 기존 성공 경로와 골든을 바꾸지 않는다.

**대기열·합치기·최대 대기.** 아래 수치는 1단계 계약 제안이며 C-5 질문 4의 승인 대상이다.

| 항목 | 결정 | 근거 코드(현재) |
|---|---|---|
| 정규화 | 원래 scope가 비었으면 전체 방 요구다. 그 외에는 중복 제거 → 저장소에 실재하는 방과 교집합 → 저장소 순으로 정렬한다. 원래 scope를 보존해 없는 방 요청이 전체 검색으로 바뀌지 않게 한다 | `internal/memory/store/store.go:311`, `internal/memory/search/retrieve.go:88` |
| 없는·삭제된 방 | 성공 0건이다. 무효 방을 `Resident`·usage·identity에 영구 등록하지 않는다. 실재하나 유효 leaf가 없는 방은 빈 상주 방으로 표시한다. 대기 중 삭제는 요구에서 제거한다. 방 재생성은 새 revision으로 재검증한다 | `internal/memory/store/room_state.go:122`, `internal/memory/engine/engine.go:438` |
| admission 순서 | ctx·엔진·backoff 확인 → **대기자 64명 상한 검사** → 요청 토큰/대기자 등록 → 요구별 보유 수 증가 → 타이머 예약. 모두 작은 상태 잠금에서 처리한다. 거절은 상태·pin·빌드를 바꾸지 않는다. 저장소 조회는 잠금 밖이다 | 기존 직렬 로드 지점: `internal/memory/engine/engine.go:410`. 새 admission 훅으로 대체 |
| 요구 크기·정리 | `wanted`는 살아 있는 대기자의 요구 합집합이다. 크기는 저장소 실재 방 수 이하이고, 요청 메타는 최대 64개다. 취소·성공·실패·은퇴 때 요구 수를 내리고 0이면 지운다. 빌드 시작 시 사본을 떼어 완료/폐기 때 지운다. 실제 데이터 규모에 비례하는 비용은 C-2에서 보고한다 | 실재 방 목록: `internal/memory/store/store.go:311`. 기존 dirty 큐와 요청 요구는 별개 |
| 고정 창 | 배치 최초 등록 시각에서 **2초** 뒤 닫는다. 후속 요청으로 연장하지 않는다. 창 닫힘 뒤 요구 집합을 동결한다. 이미 시작한 빌드에 못 들어간 요청은 다음 FIFO 배치에 넣는다 | `internal/application/revisions.go:45`, `internal/application/revisions.go:48` |
| 최대 대기 | 검색 경계의 최초 진입 t0부터 **min(원래 ctx 시한, t0+30초)**까지다. 정규화·등록·창·세대 슬롯·빌드·재획득을 모두 포함한다. 원래 시한이 없더라도 30초를 적용한다. 초과는 `ErrLoadTimeout`, 원래 ctx 종료는 `ctx.Err()`다. 설정 재시도에도 t0와 시한을 유지한다 | ctx 없는 현행 경로: `internal/application/memory.go:1161`. 30초는 측정 전 제안값이며 실기기 보장값이 아니다 |
| 실패·backoff | 해당 배치 대기자에게 빌드 오류를 돌리고 등록을 모두 정리한다. 자동 반복하지 않는다. 연속 실패 간격은 1→5→30초, 성공 때 초기화한다. backoff 중 새 느린 요청은 `ErrBackoff`와 남은 시간으로 즉시 거절한다. 이미 유효한 스냅샷의 빠른 경로는 허용한다 | 현재 오류 반환: `internal/memory/engine/engine.go:645`, `internal/memory/engine/engine.go:655` |
| 기아 방지 | 닫힌 배치는 후속 요청으로 바뀌지 않는다. 서비스 FIFO로 질문과 필수 갱신을 처리한다. 정책·prefetch는 앞서지 않고 슬롯이 없으면 생략한다. 실패를 무한 재등록하지 않는다. 장기 보유로 슬롯이 막히면 최대 대기로 명시적 실패한다 | 고정 창 선례: `internal/application/revisions.go:45`. 세대 제약은 아래 관리자 계약 |

**피크 모델.** 상주 집합 크기를 R, 복귀 방 크기를 N, 한 세대의 FTS 크기를 F라 하면,

| 구성 | 크기 | 근거 |
|---|---|---|
| 상주 벡터 행(공유, 복사 없음) | R의 약 절반 | §16 행 공유 |
| 새 세대의 메시지 배열·위치 맵·`leafPos`·`leafEndTS`·`byRecency`·identity·화자 맵 | R의 나머지 상당분, **빌드마다 새로 할당** | `loadMessages`, `Build`, `lazy` |
| 복귀 방의 새 행 + 읽기 버퍼 | N + 읽기 버퍼 | cold 읽기 |
| 새 세대 FTS | F(leaf + 요약, Go 힙 밖) | `Build` |
| 은퇴 세대 FTS | 최대 1세대의 F | 서비스 관리자 슬롯. 닫기 중에도 점유 |
| 진행 중 검색이 쥔 옛 행 | 내려놓은 방의 행(검색이 끝날 때까지) | 포인터 생존 |
| GC 여유 | heap의 GOGC 비율 | |
| SQLite 페이지 캐시·OS | working set − heap | §16: 494 − 417 MiB |

실측 환산(§16, 변경 없는 hot 재구축, 세대 1개): heap 피크 ≈ 2.0R, working set ≈ 2.4R. 여기에 복귀 방 N과 은퇴 세대의 옛 배열·맵·행·FTS가 더해질 수 있다. FTS만 더해 피크를 보장하지 않는다. 현재 1·은퇴 1·빌드 1개 제한은 **개수 상한**이며 방 크기나 RSS의 바이트 상한이 아니다. C-4에서 공유 행을 중복 제외하고 세 상태별 힙·FTS·RSS를 잰다. **FTS 크기 F는 아직 재지 않았다**(단계 0). "FTS 약 1초/4%"(§16)는 초기 cold CPU 프로파일의 시간 비중이고 메모리 비중이나 복귀 때 벽시계로 쓸 수 없다.

**빌드 진입 제어(설계에 넣는다).**

| 규칙 | 내용 |
|---|---|
| 소유자 | `Service.memState()`가 새 `engine.GenerationManager`를 **서비스당 한 번** 만든다. 모든 `engine.New`에 같은 관리자를 준다. 독립 평가 엔진은 자기 서비스의 관리자 하나를 쓴다. 새 엔진이 관리자나 보유 수를 새로 만들지 않는다. 생성 근거: `internal/application/memory.go:87`, `internal/application/memory.go:150` |
| 세대 상한 | 현재 슬롯 1·은퇴 슬롯 1·빌드 슬롯 1. 은퇴 슬롯이 비고 빌드 슬롯을 얻어야 새 빌드를 시작한다. 발행 시 현재를 은퇴 슬롯으로 옮긴다. 보유 수 0이면 즉시 닫기 절차를 시작한다. 미발행 결과는 빌드 슬롯 안에서 닫는다. 같은 Index 포인터는 한 세대로 센다 |
| 모든 진입점 | 초기 로드·`Reload`·`EnsureRooms`·`Resync`·`IndexRooms` 후 재구축·설정 교체·정책·prefetch가 같은 관리자 큐를 지난다. 엔진별 `reloadMu`만으로 입장하지 않는다. 현행 지점: `internal/memory/engine/engine.go:420`, `internal/memory/engine/engine.go:1215`, `internal/memory/engine/indexing.go:344` |
| 설정 교체 | 서비스는 최신 설정 세대 하나만 대상으로 보관한다. `Inherit`는 호환 포인터를 복사하지 않고 같은 수명 레코드에 연결한다. 비호환이면 기존 현재 슬롯은 다음 발행까지 전임 스냅샷으로 유지하며 새 설정 검색에는 주지 않는다. 반복 설정 교체는 미입장 작업을 최신 세대로 합친다. 별도 은퇴·빌드 슬롯을 만들지 않는다. 현행 교체 지점: `internal/application/memory.go:283` |
| 진행 중 옛 빌드 | 은퇴/설정 변경 때 서비스 소유 빌드 ctx에 취소를 요청한다. 취소 불가능한 읽기는 끝날 때까지 기다린다. 완료 후 설정 세대가 다르면 결과를 닫고 슬롯을 반환한다. 그전에는 최신 엔진 빌드도 시작하지 않는다. 현행 미발행 폐기 지점: `internal/memory/engine/engine.go:670` |
| 질문·갱신 우선 | 질문과 revision/identity 갱신은 고정 창 뒤 FIFO 배치다. 설정 변경은 대기자의 시한을 연장하지 않는다. 정책은 대기/빌드 요구 집합과 pin 방을 빼지 않는다. 내려놓기 후보는 발행 직전 다시 확인하고 새 요구가 생겼으면 그 정책 변경을 버린다. 정책·prefetch는 슬롯 없으면 생략한다. tick당 정책 빌드 최대 1회 |
| 종료 | `Service.Close`는 서비스 ctx 취소 → 대기자 깨우기 → 공유 빌드 완료/폐기와 검색 `release` 대기 → FTS 닫기 → 관리자 폐기 순이다. 현재 슬롯은 신규 획득만 막고 제자리에 둔다. 종료 때문에 두 번째 은퇴 슬롯을 만들지 않는다. 이 대기 중 수명·엔진·관리자 잠금을 잡지 않는다. drain에는 상한(서비스 ctx 취소 뒤 N초, 1단계에서 정한다)을 둔다. 평소에는 사용 중 FTS를 닫지 않고, **종료 때 상한을 넘은 경우에만** 닫는다. 그때 남은 검색은 `SearchChecked`의 `ErrIndexClosed`로 명시적 오류가 된다(Fable P2-3: 보유 누수가 정상 정지를 막지 않게). 현행 종료 지점: `internal/application/service.go:297`, `internal/application/service.go:314`, `internal/application/service.go:323` |

근거: 기존 포인터 공유는 `internal/memory/engine/engine.go:272`이고 엔진별 잠금은 `internal/memory/engine/engine.go:83`이다. 서비스 관리자로 이 둘을 묶어 설정 교체 예외를 없앤다. 관리자 구현·연결은 1단계이며 0단계 측정 코드는 이 스케줄링을 켜지 않는다.

**잠금 순서와 진전 조건.** 관리자의 `generationMu`가 포인터·세대 보유·방 pin·요구·슬롯을 함께 보호한다. 그 아래에서 다른 잠금, 저장소 읽기, 외부 호출, FTS 닫기, 알림 대기를 하지 않는다. 필요한 상태 사본과 알림 채널만 얻고 바로 푼다. 기존 엔진 잠금 순서 `indexMu → reloadMu`는 유지한다(`internal/memory/engine/engine.go:323`, `internal/memory/engine/indexing.go:195`). 서비스 쪽 전체 순서는 `lifeMu → memoryState.mu → generationMu`다(`MemoryEngine()`이 앞의 둘을 쥔 채 `Retire`를 부른다, `internal/application/memory.go:150`). `generationMu` 아래에서 `lifeMu`·`memoryState.mu`·엔진 잠금을 잡지 않는다.

빌더는 잠금 없이 서비스 슬롯을 예약한 뒤 **`reloadMu`만** 얻어 빌드한다(현행 `Reload`와 같다, `internal/memory/engine/engine.go:420`). `indexMu`는 `IndexRooms`·`Resync`의 DB 작업(긴 임베딩 백필 포함) 전용이다. **슬롯을 예약한 채 `indexMu`를 기다리는 경로는 금지한다.** 그렇게 하면 색인 패스 내내 빌드 슬롯이 묶여 모든 `EnsureRooms` 느린 경로가 30초 만료로 실패한다(Fable 최종 검토 P1). 엔진 잠금을 푼 뒤 `generationMu`로 발행한다. `IndexRooms`·`Resync`는 DB 작업을 마치고 `indexMu`를 푼 뒤 빌드 큐에 등록한다. 현재 `IndexRooms`가 잠금 안에서 `Reload`를 부르는 곳(`internal/memory/engine/indexing.go:344`)도 이 순서로 분리한다. 정책은 TryLock 실패면 건너뛴다. 종료는 `lifeMu`·`memoryState.mu`를 놓고 drain한다. 서비스 종료/슬롯 대기를 하는 동안 엔진 잠금을 잡는 경로는 금지한다. 요청 측 저장소 정규화·검증은 요청 ctx, 빌더의 저장소 읽기는 서비스 빌드 ctx를 받는 경로로 옮긴다. ctx 없이 긴 저장소 읽기를 기다리는 요청 경로도 남기지 않는다.

| 상황 | 동작 | 근거·논증 |
|---|---|---|
| 획득 직전 교체·닫기 | 포인터 읽기·보유 증가·은퇴 선정이 같은 `generationMu`에서 직렬화된다 | `internal/memory/engine/engine.go:676`. 획득이 먼저면 보유>0, 교체가 먼저면 새 포인터만 획득한다 |
| 5분 넘는 검색 | 취소를 요청하되 `release`까지 FTS를 유지한다 | `internal/memory/engine/engine.go:895`, `internal/memory/search/index.go:198`. 시간은 보유 수를 대신하지 않는다 |
| 등록 전 ctx 취소·65번째 대기자 | 등록·요구·pin 없이 오류를 돌린다 | admission 표. 거절이 빌드 입력에 영향을 주지 않는다 |
| 대기 중 취소·삭제 | 요청 토큰별 요구 수를 내리고 알림을 정리한다 | 요구는 살아 있는 요청에만 속한다. 빌드 사본은 완료 때 제거한다 |
| 획득 뒤 오류·취소·panic | 획득 직후 걸어 둔 `defer release()`가 pin·보유를 반환한다 | `internal/memory/engine/engine.go:886`. 중복 반환은 Once가 막는다. 종료도 검색 종료 확인 뒤 닫는다 |
| 세대 슬롯 대기와 release | 슬롯 대기는 잠금 없는 select다. release는 작은 잠금만 쓰고 빌더 잠금을 기다리지 않는다 | `internal/memory/engine/engine.go:83`. release가 슬롯을 비우는 데 역방향 의존성이 없다 |
| 증분/정책/종료 경합 | DB 작업의 엔진 잠금을 푼 뒤 입장하고 drain도 잠금 밖이다 | `internal/memory/engine/indexing.go:195`, `internal/memory/engine/indexing.go:344`, `internal/application/service.go:314`. `indexMu → reloadMu` 역전과 슬롯 교착을 차단한다 |
| 계속 새 질문이 옴 | 최초 2초 창을 고정하고 닫힌 배치를 FIFO로 실행한다 | `internal/application/revisions.go:45`. 후속 입력이 앞 배치 시각·요구를 밀지 않는다 |
| 보유 누수·응답하지 않는 외부 호출 | 5분 경고·취소 요청, 신규 대기자 최대 30초 후 명시적 오류. FTS 강제 종료는 없다 | 생존 안전성을 지킨다. 협력적 취소/실제 release가 없으면 빌드 진전은 보장할 수 없다. 누수는 진단·합성 경쟁 테스트로 잡는다 |

### 3.7 읽기 경로 분류(`eng.Index()`를 읽는 곳)

| 경로 | 지금 | 소비 방식 | 권고안 |
|---|---|---|---|
| 턴 검색(`memory_review.go` `reviewedSearch` → `engine.Search`) | 받은 엔진으로 한 번 검색 | 검색 | 아래 서비스 경계에서 엔진 재획득. `Search` 안의 `EnsureRooms(req.Rooms)`가 같은 스냅샷을 보장 |
| `memory.search` 액션(`memory.go` ~430) | nil이면 `Reload` 뒤 검색 | 검색 | 선행 전체 `Reload` 제거. 서비스 경계로 검색 |
| 타임라인(`timeline.go` `timelineSearch`) | 로드 시도/검색 오류면 원문 검색 대체. 검색 성공 0건은 그대로 | 검색 | ctx 없는 `EnsureLoaded` 제거. 같은 서비스 경계 사용. 검색 성공 0건을 오류로 바꾸지 않고, 기존 원문 대체는 유지 |
| 평가(`eval.go` ~185) | nil이면 `Reload`, 문항 4개 병렬 검색 | 검색(여러 방) | 선행 전체 `Reload` 제거. 문항별 서비스 경계 사용. 실패는 문항 오류이며 0건 성공으로 세지 않음 |
| 상태(`memory.go` ~577·608) | 로드된 leaf·메시지 수 | 진단 | 상주/내려놓음 수와 "준비됨(색인됨)"을 따로 표시 |
| 설정 교체(`memory.go` ~285) | `Inherit` 뒤 백그라운드 `Reload` | 수명 | 상주 집합으로 `Reload`. `Inherit`가 상주 상태를 넘김 |
| 근거 라벨 수정(`memory_blocks.go` ~715) | 라벨이 바뀌면 `Reload` | 수명 | 상주 집합으로 `Reload`. 내려놓은 방의 라벨은 다음 로드 때 반영 |
| 엔진 내부(`Search`·`dropRevised`·`Resync`·`IndexRooms`) | `IndexRooms`는 바뀐 것이 있을 때만 `Reload` | 수명 | `Reload`는 상주 집합만 만든다. **비상주 방의 dirty 큐·revision·recipe 변경·증분 색인(`indexing.go`)은 그대로 돈다.** 색인됨 ≠ 상주함 |
| 평가 runner(`tools/evaluation/.../main.go` ~182·316) | 자체 전체 색인 | 측정 도구 | 영향 없음. 선택: 상주 집합 옵션을 측정용으로 추가 |
| ADR-17 방 기억 읽기(`roommemory_read.go`, `episodes.go`) | **DB 직접 읽기**(`memory_observations`, `memory_rooms`) | 색인 안 씀 | 영향 없음. "파생 기억이 색인을 읽는다"는 1차 초안의 근거는 틀렸다 |
| 선제 응답(`participation.go` → `ChatTurn`) | 턴과 같은 경로 | 검색 | 턴과 같이 `Search`로 보장 |
| 검토(`memory_review.go` ~139) | DB 직접(`memory_leaves`) | 색인 안 씀 | 영향 없음 |

**엔진 교체 재시도는 `Service.reviewedSearch` 한 곳에서 맡는다.** 1단계에서 엔진 인자를 제거하고 호출마다 `MemoryEngine()`을 얻는다. `errors.Is(err, engine.ErrRetired)`일 때만 새 엔진을 **한 번** 더 얻어 요청한다. 두 번째도 은퇴하면 오류를 반환한다. 원래 ctx·scope·t0·최대 대기를 그대로 쓰고, 요청별 예산·도구 횟수 예약은 재시도 때 다시 하지 않는다. 다른 오류의 자동 재시도는 없다. 근거: `internal/application/memory_review.go:79`, `internal/application/memory_review.go:87`, `internal/application/memory.go:905`.

`ChatTurn`의 ctx 없는 선행 `EnsureLoaded`를 제거한다(`internal/application/memory.go:1160`). `gatedMemory`는 고정 엔진 대신 서비스를 가진다(`internal/application/memory.go:899`). planner와 도구 루프의 `memory.search`, 평가(`internal/application/eval.go:205`), 타임라인(`internal/application/timeline.go:103`)이 모두 이 경계를 쓴다. 타임라인의 원문 대체는 재시도 이후 기존 규칙으로만 실행한다. 0단계에는 이 호출 경로를 변경하지 않는다.

### 3.8 진단

0단계는 §7 C-2의 숫자 측정만 추가한다. 예산은 무한, 상주는 전체다. 대기/내려놓기 이벤트를 만들지 않는다. 아래 운영 상태·이벤트는 후속 단계용이다. FTS 크기는 DB 페이지 바이트와 프로세스 할당을 구분한다. `page_count × page_size`는 DB 크기 지표이며 SQLite 할당/RSS 전체가 아니다. C-4에서 heap·working set과 함께 보고한다.

- `runtime.doctor` 행 `memory_residency`: 상태(`ready` / `over_budget` / `loading`), 세부(상주 방 수·메시지 수·추정 MiB·실제 힙·FTS 추정, 색인은 됐지만 내려놓은 방 수·추정 MiB, 예산 MiB와 출처, 열려 있는 은퇴 세대 수, 24시간 동안 질문이 로드를 기다린 횟수와 최대 대기 시간, prefetch 횟수, 마지막 변경 시각). 방 이름은 넣지 않는다(개인정보 규칙: 숫자만).
- 콘솔 블록 목록(`memory.blocks.list`)에 방별 상태 열: 상주 / 내려놓음(색인됨) / 불러오는 중 / 색인 대기. 문구는 `strings.py`(ko·en) + `locales/ui`. 로그 → 진단에 "기억 상주 방".
- `memory` 이벤트 `residency_changed`로 열린 콘솔이 목록을 다시 읽는다. Discord `/status`는 상주/전체 방 수 한 줄.

### 3.9 실패 시 답변 계약(1단계 제안, C-5 승인 대상)

현재 planner는 기억 검색 오류를 `memory_unavailable`로 표시한 뒤 답변 모델을 호출한다(`internal/chat/runtime.go:270`, `internal/chat/runtime.go:308`, `internal/chat/runtime.go:316`). 아래는 이를 기준으로 닫은 설계다. 0단계는 현행 동작·문구 그대로다. 제한된 답변 안내 추가와 대기 상한 적용은 사용자 승인 뒤 한다.

| 상황 | 검색 반환 | 턴 답변 정책 | 근거·구현 경계 |
|---|---|---|---|
| `ErrBusy` | 등록 없이 즉시 오류 | 원래 ctx가 살아 있으면 `memory_unavailable`·빈 기억 근거와 언어팩 안내로 제한된 답을 만든다. 같은 턴의 추가 기억 로드는 막는다 | §3.6 admission. `internal/chat/runtime.go:270`의 오류 처리와 `internal/application/memory.go:905`의 검색 경계 |
| 빌드·identity·FTS 오류 | 원인 보존 오류. 실패 배치 정리 | 같은 정책. 오류를 "기록 없음"이라고 설명하거나 일부 후보를 성공으로 쓰지 않는다 | `internal/memory/engine/engine.go:645`, `internal/memory/lexical/index.go:122` |
| `ErrBackoff` | 즉시 오류와 retry-after 집계 | 같은 정책. 턴 안에서 sleep·재등록하지 않는다. 다음 턴은 backoff 종료 뒤 다시 시도할 수 있다 | §3.6 실패/backoff. `internal/application/memory_review.go:87` |
| 로드 대기 30초 만료 | `ErrLoadTimeout` | 원래 ctx가 살아 있으면 같은 제한된 답. 로드만 취소하고 공유 빌드는 계속한다 | §3.6 최대 대기. `internal/application/memory.go:1161`의 선행 대기 대체 |
| 원래 턴 시한 만료·사용자 취소 | `context.DeadlineExceeded`/`context.Canceled` | 답변 생성·추가 도구 호출을 중단한다. 새 ctx로 답변을 이어가지 않는다. 취소된 전송에 오류 안내를 억지로 보내지 않는다 | `internal/chat/runtime.go:316`, `internal/chat/toolloop.go:405` |
| 엔진 두 번 은퇴·획득 검증 재시도 소진 | 명시적 오류 | 원래 ctx가 살아 있으면 같은 제한된 답. 무한 재시도 없음 | §3.6·§3.7. 현재 비공개 은퇴 오류: `internal/memory/engine/engine.go:293` |
| 정상 검색 0건 | 오류 없음 | 현행 빈 결과 정책. `memory_unavailable`과 구분한다 | `internal/chat/runtime.go:274` |

제한된 답은 이번 검색의 기억 사실·근거를 주장하지 않고 현재 대화로만 답한다. 안내는 기존 언어팩 경로에 둔다. planner와 도구 루프가 같은 턴 실패 표시를 쓰며 도구 재호출을 막는다(`internal/chat/toolloop.go:424`). 평가·직접 `memory.search`는 오류를 그대로 보고한다. 타임라인은 §3.7의 기존 원문 대체를 쓴다. 사용자가 C-5 질문 4에서 오류 안내만 선택하면 살아 있는 턴도 답변 모델 호출을 생략한다. 지금은 그 대안을 구현하거나 기본값으로 적용하지 않는다.

## 4. 대안과 기각 이유

| 대안 | 기각 이유 |
|---|---|
| 방별 색인(A) | §3.1. 검색·parity·골든을 다시 정의해야 한다. 메모리 이득은 B와 같다 |
| 전체 빈도표로 bm25 직접 계산 | §3.2(b). FTS5를 버리면 샌드박스 parity가 사라진다. 바꾸는 범위가 크다 |
| 벡터만 내려놓고 FTS·본문은 상주 | §3.2(c). 절감이 약 절반뿐 |
| 벡터를 파일로 두고 mmap | 범위 검색은 범위의 행만 점수를 내지만(`denseTop`), 그 행들은 행렬 전체에 흩어져 있어 페이지 단위로는 사실상 무작위 읽기다. SD 카드에서 내려간 페이지의 재읽기가 매 질의에 생기고, OS가 상주를 정하므로 예측도 안 된다. FTS·메시지 배열·맵은 어차피 mmap으로 못 덜어낸다 |
| 오래된 방을 디스크에서 바로 검색(SQLite FTS 영구 사본 + 벡터 스캔) | 별도 검색 경로가 생겨 품질·parity가 둘로 갈린다. 영구 FTS 사본의 무효화·삭제 경로가 필요하다(§16이 미룬 이유와 같다) |
| 사용자 설정으로 방마다 "항상 상주" 지정 | lowspec 원칙(새 설정 없음)에 어긋난다. 자동 규칙(7일·24시간·6시간)으로 같은 효과 |
| identity 표도 상주 집합 기준 | §3.2(1). 범위 밖 방에서만 본 표시명이 사라져 인물 해석이 결정적으로 바뀐다. 전역 표는 캐시로 유지하고 실제 비용을 C-2·C-4에서 잰다 |

## 5. 실험 결과 (코디네이터, 2026-10-13, Windows)

조건: 제품 Go, leaf 1024 + 메시지 벡터 1024(현재 기본), 문항 세트마다 답 있는 40 + 없는 10, 단일·흐름형. R0=재순위 없음(hybrid), R2=Voyage rerank-2.5-lite. 방 범위 검색(`Scope.Rooms`=문항의 방). 원본은 `.masamong-scratch/eval-msgdims/`(커밋하지 않음).

**E1 방별 색인.** discord 루트 15방 → 문항 있는 4방 각각 그 방만 남긴 사본(제품의 `memory.blocks.delete`로 다른 방 삭제). 어휘 IDF·평균 길이·`corpusTMax`가 그 방만으로 계산된다.

| discord 100문항, R@5 | 전체 색인 | 방별 색인 | 문항 단위 |
|---|---|---|---|
| R0 단일 | 25/40 | 25/40 | +0 −0 (R@1 23→24) |
| R0 흐름형 | 26/40 | 26/40 | +0 −0 (R@1 18→19) |
| R2 단일 | 32/40 | 30/40 | +2 −4 |
| R2 흐름형 | 37/40 | 36/40 | +0 −1 |

재순위 잡음: 같은 전체 색인을 두 번 돌리면 R2 단일이 +2 −2로 바뀐다(합계 같음). 기록 없는 질문의 근거 비움은 모두 같다(R2 8/10).

**E3 반대 방향(통계가 섞일 때).** 카카오 B 루트에 카카오 A 원문을 가져와 2방 루트(17.8만 메시지, leaf 24,486)를 만들고 각 세트 문항을 돌림.

| R@5 | 자기 루트 | 합친 루트 |
|---|---|---|
| kakao-a R0 단일 / 흐름형 | 20 / 22 | 21 (+2 −1) / 21 (+1 −2) |
| kakao-a R2 단일 / 흐름형 | 28 / 27 | 28 / 27 |
| kakao-b R0·R2 네 칸 | 25·20·33·20 | 모두 같음(+0 −0) |

**E4 메모리·cold 로드.** GOMAXPROCS=1, 새 프로세스마다 한 번(`runner load`). 두 번째 실행은 OS 파일 캐시가 데워진 상태.

| 루트 | 방 | 메시지 | 메시지 벡터 | 힙 | 로드(첫 / 두 번째) |
|---|---|---|---|---|---|
| discord 방4 | 1 | 363 | 217 | 3.4MB | 0.45 / 0.02s |
| discord 방3 | 1 | 3,530 | 2,350 | 8.7MB | 3.46 / 0.20s |
| discord 방2 | 1 | 11,007 | 5,758 | 19.4MB | 7.55 / 0.61s |
| discord 방1 | 1 | 40,792 | 24,126 | 65.9MB | 17.61 / 2.58s |
| discord 상위3 | 3 | 55,329 | 32,234 | 90.7MB | 18.63 / 3.69s |
| discord 상위5 | 5 | 55,930 | 32,656 | 91.7MB | 3.37 / 3.46s |
| discord 전체 | 15 | 56,328 | 32,927 | 92.5MB | 15.87 / 3.23s |
| kakao-a | 1 | 75,439 | 40,592 | 105.1MB | 9.42 / 3.50s |
| kakao-b | 1 | 102,132 | 33,801 | 111.0MB | 10.61 / 4.00s |
| 합친 2방 | 2 | 177,571 | 74,393 | 213.0MB | 8.69 / 9.60s |

한계: 첫 실행은 디스크 캐시가 비어 있고(복사 직후) 방 삭제 직후 사본은 첫 열기에 정리 작업이 섞였을 수 있다. 측정 장비는 저사양이 아니다(1코어 제한만). 다른 작업(테스트·워커)이 같은 시간에 돌았다. Go heap만 쟀고 RSS·SD cold p95는 재지 않았다. 방 수가 아니라 메시지 수가 힙·로드 시간을 정한다. discord 방 크기는 매우 치우쳐 있다(40,792 / 11,007 / 3,530 / 363 / 238 / … / 4).

**C 참고(같은 날):** 메시지 벡터 1024 → 512는 R@5 네 칸 모두 3문항 이내(최대 −1), 힙 −15~19%. 권고만 했고 기본값은 그대로다.

**읽는 법(수정).** E1·E3은 **탐색 근거**다. 보여 주는 것은 "어휘 통계가 방 하나 기준이든 두 세트가 섞인 기준이든 R@5 합계의 변화가 작다"까지다. 무손실의 증거는 아니다: (1) E1 R2 단일은 32→30이고 문항 단위 +2/−4로, 합계가 비슷해도 특정 문항의 회귀가 다른 문항의 개선에 가려진다. (2) 같은 색인 반복의 +2/−2는 합계 차이 0이라 "합계 ±2 허용"의 통계적 근거가 못 된다. (3) E3 R0의 +2/−1, +1/−2는 재순위가 없는 **결정적** 후보 변화다. 그래서 (a)를 "조건부 권고"로 두고, 단계 0 게이트(§7)에서 재순위 전 후보·순위를 직접 비교한다. "방별 IDF가 오히려 나을 수도"라는 가설은 R@1 +1로 약하게만 보이고 결론 지을 수 없다.

## 6. 메모리 절감 예상

메시지당 상주 비용은 E4의 큰 세트에서 1.09~1.64KB(메시지 벡터 비율에 따라). 작은 방은 고정비가 지배한다(363건 3.4MB ≈ 9.4KB/건, 3,530건 8.7MB ≈ 2.5KB/건). 아래는 계획서 §7과 같은 1.2KB 기준이며 **오차는 ±30%**로 본다(본문 길이·벡터 비율·요약·지연 캐시가 세트마다 다르다). 메시지 벡터 512(C)를 함께 적용하면 모든 칸이 약 15~19% 줄어든다. 예산은 512MB 기기 20%(≈100MiB), 2GB 기기 20%(≈400MiB) 기준이다.

| 시나리오 | 기기·예산 | 보호 방 | 지금 상주 | 권고안 상주 | 설명 |
|---|---|---|---|---|---|
| 활발한 방 3개, 3년, 모두 최근 질문 있음(≈100만 메시지) | 2GB·400MiB | 3방 전부 | ≈1.2GB | **≈1.2GB** | 보호 방만으로 예산 초과. 내려놓을 방이 없다. `over_budget`. C 적용 시 ≈1.0GB |
| 활발한 방 3개 1년(≈33만) + 카톡 보관 2개(17.8만, 한 채널에 붙임, 그 채널은 1주 이상 질문 없음) | 2GB·400MiB | 3방 | ≈0.6GB | **≈0.4GB** | 카톡 2방(E4 실측 213MB)이 내려간다. 남은 ≈396MB도 예산 근처라 `over_budget` 경고가 날 수 있다 |
| discord 세트 구조(15방, 상위 1방이 72%), 상위 1방만 최근 질문 | 512MB·100MiB | 1방 | 92.5MB | **92.5MB** | 예산 안이라 **내려놓기가 일어나지 않는다.** 지금과 같다 |
| 512MB 기기, 방 5개 각 5만 메시지, 질문은 1방에만 | 512MB·100MiB | 1방 | ≈300MB(소프트 한도 307MiB와 충돌) | **≈60MB** | 80% 목표(80MiB)까지 LRU로 내린다. 다른 방 질문 시 그 방이 올라와 ≈120MB(예산 초과 → 다음 tick에 보호 안 된 방부터 다시 내림. 두 방 모두 7일 보호면 그대로) |

결론: 권고안은 상주 메모리를 **예산 안으로 묶으려 시도**한다. 예산 안이면 아무것도 바꾸지 않고, 보호 방(7일 검색·24시간 메시지·6시간 최소 상주)만으로 예산을 넘으면 아무것도 줄이지 못한다. 그래서 몇 년 운영에서 활발한 방 자체의 성장에는 별도 수단이 필요하다(§10).

## 7. 구현 범위·기간·위험·도입 순서·되돌리기·테스트

**바뀌는 곳(예상).**

| 영역 | 파일 | 내용 |
|---|---|---|
| 엔진 | `internal/memory/engine/engine.go` | `reload()`에 상주 집합 필터와 identity 힌트. `EnsureRooms(ctx, rooms)`(§3.6 계약). `Search`가 `EnsureRooms` 사용. `closeLater`를 보유 수 기반으로. `Inherit`/`Retire`에 상주 상태·대기자 처리 |
| 엔진 | `internal/memory/engine/residency.go`·`generation.go`(새) | 서비스 `GenerationManager`·Index 단위 보유. 예산·방별 추정·보호·LRU·tick·고정 창 합치기·usage·identity 캐시. 측정과 1단계 구현을 아래 표로 구분 |
| 저장소 | `internal/memory/store` | `memory_room_usage(room, last_searched_at, loaded_at, identity_json)` 표(마이그레이션 1개, 기존 방에 `now` 심기), 방별 (계정, 표시명) 조회, 방별 마지막 메시지 시각 조회(기존 인덱스) |
| 검색·어휘 | `internal/memory/search`·`internal/memory/lexical` | identity 입력 훅·Checked 오류 전파. `Index.Resident` 집합, `Index.EstimatedBytes()`·FTS 크기 보고. `LoadSandboxIndex`에 방 필터 인자(parity 로더). 측정 외 API/오류 변경은 1단계 |
| 애플리케이션 | `internal/application/memory.go`·`memory_review.go`·`service.go` | 서비스 관리자 소유·종료 drain. ctx 없는 선행 전체 로드 제거. `reviewedSearch`에 단일 엔진 교체 재시도. scheduler tick에 정책·usage flush. prefetch는 3단계 |
| 애플리케이션 | `timeline.go`, `memory_blocks.go`, `eval.go` | §3.7 분류대로. 검색 경로는 `Search`가 보장하므로 변경 작음. 진단 표시 추가 |
| 진단·콘솔 | `doctor.go`, `strings.py`, `locales/ui`, `ui/dist` | `memory_residency` 행, 블록 목록 상태 열, 이벤트 |
| 평가 도구 | `tools/evaluation/.../runner` | 단계 0 게이트용: 방 남기기, 재순위 전 후보 덤프와 비교, 짝 비교 |
| 문서 | `memory-operations.md` 새 절, 이 ADR 상태 | |

**단계적 도입(게이트 포함).**

| 작업 | 허용 범위 | 산출물·통과 조건 |
|---|---|---|
| C-1 계약 | 이 ADR만 수정한다. 코드 구현 없음 | §12 여섯 결정. 코디네이터와 Fable 최종 검토. 작성자와 다른 모델이 검토한다 |
| C-2 측정 전용 | **동작 변화 0·예산 무한·내려놓기 없음**. `Resident`는 항상 전체. `Index.EstimatedBytes()`·방별 비용·FTS 크기·재구축 피크·`memory_residency`·블록 예상 메모리 열만 추가한다 | 전체 상주 검색·parity·골든 바이트 그대로. 콘솔 ko/en/ja/es/zh-Hans·360px 검증. 준비됨과 상주함을 구분 |
| C-3 게이트 도구 | runner의 독립 측정 빌드만 상주 방 필터를 받는다. 제품 config·콘솔 설정에는 노출하지 않는다. 원문·방·벡터 삭제로 흉내 내지 않는다. 측정 훅 `Settings.ResidentRoomsForMeasurement`·`search.BuildWithIdentity`는 명시적 도구 호출에서만 쓴다 | 재순위 전 후보·순위 덤프/비교, 아래 문항, 전체 identity 표 동등성, 부분 집합 parity. 기록 없는 방 요청과 빈 scope도 검증 |
| C-4 실측 | A·B의 메시지 벡터 512 전환 뒤 Windows에서 GOMAXPROCS=1·GOMEMLIMIT로 1GB/512MB를 흉내 낸다. 실제 서비스 정책은 켜지 않는다 | 3세트+합친 루트의 상주 heap·hot 피크·working set·FTS·방 크기별 복귀 지연·빌드별 재할당 내역. 실기기 미확인을 명시 |
| C-5 보고·정지 | 집계 수치·문항 ID로 사용자에게 보고한다. 이 단계에서 멈춘다 | 후보/문항별 회귀·피크·예산 권고·§9 질문 2·3·4·6·후속 기간/위험. **사용자 승인 전 1단계 금지** |

0단계에는 `EnsureRooms` 연결, 세대 관리자 입장 제어, 참조 수 종료 전환, lexical 오류 API 전환, 선행 로드 제거, 실패 안내·대기 상한 변경, usage 마이그레이션, prefetch·내려놓기를 넣지 않는다. 이들의 설계만 닫는다. 0단계 진단은 숫자를 관측할 뿐 스케줄링·검색 후보·오류 답변을 바꾸지 않는다. 근거 지점은 `internal/memory/engine/engine.go:61`(기존 측정용 설정), `internal/memory/search/index.go:126`(빌드), `tools/evaluation/memory_dims/runner/main.go:169`(로드 측정)이다.

| 단계 | 내용 | 완료 조건 |
|---|---|---|
| 0. 측정·품질 게이트 | (a) 상주 추정·`memory_residency` 진단·FTS 크기 측정만 넣는다(예산 무한, 내려놓지 않음). (b) 평가 도구에 **재순위 전 후보·순위 비교**를 넣는다. (c) 게이트 문항: 같은 요청/응답을 고정한 짝 비교(또는 반복한 짝 비교), 여러 방 scope 문항, 서로 다른 부분 집합과 출입 순서({A,B}→{A,B,C}→{A,B}), 범위 밖 방에서만 본 계정 별칭 문항, 시간 필터 문항, 부정 문항(기록 없음), 기본 auto 모드의 confidence·recovery 경로. (d) identity 전역 표의 전체 상주 동등성 테스트 | 전체 상주 parity·골든 통과(동등성). 부분 상주 게이트에서 재순위 전 후보 집합 변화와 R@5 문항 단위 회귀를 사용자에게 보고. **무손실 판정은 parity 통과와 별개로 사용자가 한다** |
| 1. 계약 구현 | 사용자 승인 범위의 `EnsureRooms`·Index 보유 수·서비스 세대 관리자·호출자·오류 정책·identity 무효화·usage 표. 좁힌 `-race`·합성 경쟁 검증 | C-1 타 모델 검토 통과, C-2~C-4 검증·측정 완료, C-5 보고 후 **사용자 명시 승인**. 이후 엔진 단위 검증·Fable 최종 검토 |
| 2. 내려놓기 켬 | 예산·보호 규칙 적용. 예산 안인 설치는 동작 변화 없음 | 실기기(라즈베리파이·SD·512MB) 피크·첫 질문 지연 측정. 실기기 접근은 별도 완료 조건 |
| 3. prefetch | capture 경로 연결. 진단의 "질문이 로드를 기다린 횟수"로 효과 확인 | 핑퐁 없음(방당 6시간에 1회 이하) |
| 별도 A·B(결정됨) | 메시지 벡터 512 적용과 MRL 확인 | 2026-10-13 사용자 결정. C-4보다 먼저 검증하며 이 ADR 문서 작업에서는 구현하지 않음 |

**기간(워커 기준, 재추정).** 계약 확정 0.5~1일(이 ADR + 타 모델 검토) → 단계 0 도구·게이트 2~3일 → 구현 3~5일 → 정적/합성 경쟁 검증 1~2일 → 실기기 피크·지연 측정(실기기 확보 뒤, 1~2일) → 사용자 활성화. 벽시계 **약 2~3주**, 실기기 접근은 별도 조건. 1차 초안의 "약 1주"는 동시성 계약·읽기 경로·부분 parity·실기기가 모두 미리 풀린 경우의 값이었다.

**위험.**

| 위험 | 완화 |
|---|---|
| 부분 상주에서 어휘 순위·재순위 후보가 바뀌어 특정 문항이 회귀 | 단계 0 게이트에서 재순위 전 후보를 비교하고 문항 단위 회귀를 보고. 사용자 승인 없이는 켜지 않음(§9 질문 2) |
| 재구축 피크가 소프트 한도에 닿음(small) | §3.6 구성별 모델과 서비스 세대 상한. 옛 배열·맵·행도 포함한다. 단계 0에서 FTS·RSS 실측 뒤 예산 확정 |
| `EnsureRooms` 대기자 누수·교착 | 잠금 순서 고정(§3.6), ctx 취소 경로 테스트, `-race` 합성 경쟁 테스트 |
| 올리기·내리기 핑퐁 | 7일·24시간·6시간 최소 상주 + prefetch 조건(7일 검색 기록, 예산 안) + 80% 히스테리시스. 진단으로 감시 |
| 첫 질문 지연이 실기기에서 길다(SD cold 읽기) | 조건부 prefetch. 실기기 측정 뒤 시한 만료 대체 동작 여부를 정한다(§9 질문 4) |
| `Index()`를 전체 색인으로 가정한 읽기 경로 | §3.7 분류. 검색 경로는 `Search`가 보장, 진단은 상주/준비 구분. 테스트로 고정 |
| 비상주 방의 증분 색인·dirty 큐 누락 | `IndexRooms`·dirty 큐·revision은 상주와 무관하게 돈다(§3.7). 테스트로 고정 |
| `last_searched_at` 쓰기 부담 | 캐시 + 1분 배치 + 종료 flush |

**되돌리기.** 원문·벡터·leaf는 바뀌지 않는다. **운영 메타 표(`memory_room_usage`) 하나가 추가**되며 추가 전용이고 구 바이너리는 읽지 않는다. 운영자 비상 스위치 `MASAMONG_MEMORY_RESIDENT=all`(환경 변수, `MASAMONG_PERF`와 같은 방식, 로그에 값은 남기지 않음)이 내려놓기를 끄고 전체 상주로 돌린다. 이전 바이너리로 돌아가면 그냥 전부 올린다. **한계:** `all`도 구 바이너리도 "전부 올리기"라 전체 로드 피크(≈2R + F)를 만든다. 전체 집합이 기기를 넘는 설치에서는 둘 다 안전한 복구가 아니다. 내려놓기는 완화 수단이지 복구 수단이 아니며, 복구는 예산 안으로 돌아올 수 있을 때만 가능하다. 이 점을 사용자 결정에 포함한다(§9 질문 1).

**테스트·parity.**

- 엔진 단위(1단계, 가짜 저장소): 예산 안이면 전체 상주(지금과 동일). 넘으면 LRU 순으로 내려감. 7일·24시간·6시간·색인 중·pin 방은 남음. `EnsureRooms`가 빠진 방만 읽고 상주 행을 공유함(`LastReused`). 동시 요청은 빌드를 공유한다. ctx 취소·오류·중복 release·은퇴에서 pin/보유/요구 누수가 없다. `Inherit`는 같은 관리자·수명 레코드·기록·identity 캐시를 쓴다. 옛 요청은 정리 후 서비스 경계에서 재등록한다. 호환·비호환 반복 설정 교체에도 은퇴 1·빌드 1 상한을 지킨다. 보유>0이면 5분 뒤에도 FTS는 열려 있고, 0일 때 닫힌다. 없는/삭제 방 성공이 영구 가짜 방을 남기지 않는다.
- 결정성: {A,B} → {A,B,C} → {A,B}에서 범위 A의 결과가 같다.
- identity(C-3): 전체 상주에서 기존 `labelsByKey`·`keysByLabel`·`leavesByKey`·`aliasTable`과 같다. 부분 상주에서도 전역 alias가 같다. 열린 꼬리·미임베딩·불정합 leaf·플랫폼/계정 정규화·벡터 적격성을 검사한다. 비상주 방 삭제/철회·recipe/space/임베딩 변경 무효화 연결은 1단계에서 검증한다.
- 애플리케이션: 턴 하나가 상주하지 않는 방을 올린 뒤 기억을 쓴다. capture가 조건부 prefetch를 건다(7일 기록 없는 보관 방은 걸지 않음). `memory.search`·타임라인·평가·근거 라벨이 상주하지 않는 방에서도 옳다. 비상주 방의 증분 색인이 돈다.
- 동시성(1단계): 좁힌 `-race`로 보유 증가 직전 교체·닫기, 검색 중 내리기·반복 설정 교체·종료, 취소/획득 경쟁을 검사한다. 65번째 거절의 무변경, 고정 창/최대 대기/FIFO, 실패 후 요구 정리/backoff, 비상주 identity 삭제와 콜백 지연도 합성 검증한다.
- parity: `TestSandboxRetrieveParity`는 전체를 올리므로 본문 변경 없음. 로더(`LoadSandboxIndex`)와 샌드박스에 같은 방 필터 인자를 넣고 부분 집합 덤프 하나를 **추가로** 비교한다.
- 품질 게이트: 단계 0 표(§7 도입)대로. 합계 ±2가 아니라 **재순위 전 후보·순위 비교와 문항 단위 회귀 보고**가 기준이다.
- 성능: `runner load`로 상주 집합 로드 시간·힙·FTS 크기·RSS, 교체 피크(은퇴 세대 포함)를 small 단계(GOMAXPROCS=1)와 실기기에서 잰다.

## 8. 계속 지키는 것

- 품질을 깎아 메모리를 얻지 않는다. 기억을 쓸 때는 요구 방이 든 동일 스냅샷으로 검색한다. 로드 실패를 빈 기억 성공으로 숨기지 않고 §3.9 정책을 쓴다. 예산을 넘어도 내려놓을 방이 없으면 그대로 둔다.
- identity·alias는 전역이다. 부분 상주가 인물 해석을 바꾸지 않는다.
- 새 사용자 설정은 없다. 운영자 환경 변수 하나만 둔다.
- 전체 상주일 때 검색 코드·골든·parity 결과는 지금과 같다. 부분 상주의 어휘 순위 변화는 사용자 승인 뒤에만 켠다.
- 방 이름·대화 본문은 진단·로그에 내지 않는다. 숫자만.

## 9. 사용자 결정 — 0단계 진행, 나머지는 C-5에서

2026-10-13 사용자는 **질문 1의 0단계 진행과 질문 5의 메시지 벡터 512 적용을 결정했다**. 1단계 이후 구현·내려놓기 활성화 승인은 아직 없다. 질문 **2·3·4·6은 C-5 게이트 보고에서 다시 묻는다**. 지금 추가 승인을 요청하지 않는다.

| 번호 | 상태 | 결정 또는 C-5에서 물을 내용·권고 |
|---|---|---|
| 1. 진행 범위 | 결정됨 | 0단계(계약·측정 전용 진단·게이트 도구·실측·보고)만 진행한다. C-5에서 멈추고 후속 승인을 받는다. 비상 all/구 바이너리도 전체 로드 피크 때문에 모든 설치의 안전한 복구를 보장하지 않는다 |
| 2. 부분 상주 어휘 순위 | C-5 재질문 | identity·alias 전역 유지 조건으로 bm25/재순위 후보 변화 허용 여부를 정한다. 후보 변화·문항별 회귀를 보고 최종 승인한다. 전체 parity 통과만으로 허용하지 않는다 |
| 3. 예산 | C-5 재질문 | 20% 시작을 잠정 권고한다. C-4의 heap·FTS·working set 피크로 20/25%를 비교한다. 보호 방만으로 예산 초과면 상한은 작동하지 않는다. small은 따로 정하기 전까지 측정만 한다 |
| 4. 첫 질문 대기·실패 답변 | C-5 재질문 | §3.6의 30초 최대 대기와 §3.9 오류별 제한된 답을 제안한다. `ErrBusy`·빌드 오류·backoff·로드 대기 만료에 적용한다. 원래 턴 ctx 종료는 답변 중단이다. (a) 살아 있는 턴도 답변 모델을 호출하지 않고 오류 안내만 / (b) 언어팩 안내와 제한된 답 중 고른다. 현행 planner 기준으로 (b)를 권고하며 측정 지연 뒤 상한을 확정한다. 30초는 검색 호출 하나의 상한이다. planner가 한 턴에 여러 번 검색하면 턴 전체의 상한은 턴 시한이다 |
| 5. 메시지 벡터 512 | 결정됨 | 적용한다. A·B에서 구현·MRL/업그레이드/parity를 확인한다. 다음 일반 색인에서 공간이 달라 메시지 벡터를 다시 만든다. C-4는 적용 뒤 기준이다. 이 ADR의 §5 기존 실험 수치는 바꾸지 않는다 |
| 6. 보호 기간 | C-5 재질문 | 검색 7일·메시지 24시간·최소 상주 6시간을 잠정 권고한다. 절감·첫 질문 지연·핑퐁 측정을 보고 정한다 |

## 10. 한계와 다음 후보 (이 ADR 범위 밖)

- 활발한 방의 성장은 B로 멈추지 않는다. 다음 후보는 **방 안 기간별 내려놓기**다: 오래된 기간의 메시지 벡터만 내려놓고 leaf 벡터·FTS는 남긴다(메시지 벡터가 힙의 약 1/3~1/2). "오래된 일에 대한 질문" 품질을 재는 문항 세트가 먼저 필요하다. 별도 ADR로 다룬다.
- 실기기(라즈베리파이·SD 카드·512MB) 측정은 아직 없다([남은 일](../BACKLOG.md)의 저사양 실기기 확인). 이 ADR의 로드 시간은 x86 1코어 흉내이고 Go heap만 쟀다.
- 빌드마다 새로 만드는 메시지 배열·맵(§1)을 세대 간에 공유하면 피크 배수 2.0을 낮출 수 있다. 이 ADR의 범위 밖이며 단계 0 측정이 그 가치를 보여 줄 것이다.

## 11. 검토 반영 기록 (Sol, 2026-10-13, 판정 REVISE)

| 항목 | 반영 절 |
|---|---|
| P1-1 identity·alias 전역, 검색 불변 범위 한정, bm25 동작 변경 명시 | §1, §2 2·3, §3.2, §4, §8, §9 질문 2 |
| P1-2 `EnsureRooms` 스냅샷·pin 계약, ctx·대기열·공유 빌드·실패·없는 방·Inherit/Retire·은퇴 중 빌드 | §3.6 계약 표. "7일 기록으로 경쟁이 풀린다" 삭제 |
| P1-3 피크 모델·세대 누적·진입 제어·잠금 순서·§16 실측 인용 | §1, §3.6 피크 모델·진입 제어·잠금 순서, §6 |
| P2-1 E1·E3 탐색 근거, 게이트 항목, 시간 창 사실 | §1, §5 읽는 법, §7 단계 0 |
| P2-2 읽기 경로 분류, ADR-17 근거 수정, 준비됨/상주함 구분 | §3.7, §3.4 보호 근거, §3.8 |
| P2-3 prefetch 조건·쿨다운, 기록 쓰기 배치, 1분 tick, 신규 방, 예산 한계, memlimit 구분 | §3.4, §3.5 |
| P2-4 FTS 4% 한정, §6 예산·보호 방 열, discord 92.5MB 유지, 비용 모델 오차 | §3.6 피크 모델, §6 |
| P2-5 기간 재추정, 승인 조건, 질문 재정리, 비상 env 한계 | §7 기간·되돌리기, §9 |
| P3 범위 필터 문구, mmap 기각 근거, FTS 두 DB, parity 로더, 스키마 문구 | §1, §3.2 parity, §4, §7 되돌리기 |
| 반영하지 않은 것 | 없음. 단, 피크 모델의 FTS 크기 F와 RSS는 측정값이 없어 단계 0으로 미뤘다(§3.6·§7) |

## 12. 0단계 계약 결정 (C-1, 2026-10-13, Fable 최종 검토 SOUND)

2차 검토의 여섯 쟁점을 아래 결정으로 닫았다. 1·2차 검토에서 이미 닫힌 방향·실험 해석은 다시 열지 않는다. 아래는 **설계 완료이며 구현/검증 완료 주장이 아니다**. 코디네이터와 Fable(최종)이 검토했고 판정은 SOUND다. Fable이 남긴 1단계 계약 수정(빌더는 reloadMu만, 임베딩 뒤 획득, identity 검증 범위, 종료 drain 상한, 어휘 구문 오류, 잠금 순서)은 §3.2·§3.6·§9에 반영했다.

| 쟁점 | 확정한 설계 | 본문·근거 코드 |
|---|---|---|
| P1-1 스냅샷 수명 | 같은 잠금에서 포인터 읽기→Index 보유 증가→방 pin. 은퇴하고 보유 0일 때만 닫는다. release는 Once. 5분은 경고/취소 요청. lexical DB 오류를 SearchChecked부터 검색 반환까지 전파한다 | §3.6. `internal/memory/engine/engine.go:676`, `internal/memory/search/index.go:198`, `internal/memory/lexical/index.go:122` |
| P1-2 전역 identity | 전체 빌드의 최종 유효 leaf 구간 합집합과 동일 키를 쓴다. 비상주 revision/leaf 적격성 변경도 캐시 무효화·발행 전 반영·획득/반환 시 검증한다. 비용 O(P+R) 측정 | §3.2. `internal/memory/search/person.go:218`, `internal/memory/search/person.go:240`, `internal/memory/engine/engine.go:599`, `internal/memory/store/store.go:1685` |
| P1-3 세대 소유권 | Service.memState가 관리자 하나를 만들고 모든 엔진이 공유한다. 현재 1·은퇴 1·빌드 1. 반복 설정 교체도 예외 없이 최신 대상을 합치고 옛 빌드 폐기/닫기 뒤 슬롯 반환 | §3.6. `internal/application/memory.go:87`, `internal/application/memory.go:283`, `internal/memory/engine/engine.go:252` |
| P2-1 대기열·합치기 | 64명 검사 뒤 등록. 실재 방 교집합·요구 수 정리. 최초 고정 2초·FIFO. 최초 검색 진입+30초와 원래 ctx 중 빠른 시한. 포함된 빌드 뒤 검증 경쟁만 추가 1회. 실패/backoff는 요구를 남기지 않음 | §3.6. `internal/application/revisions.go:45`, `internal/memory/store/store.go:311`, `internal/memory/engine/engine.go:410` |
| P2-2 호출자 | ChatTurn 선행 EnsureLoaded 제거. Service.reviewedSearch 한 곳에서 공개 ErrRetired에만 새 엔진 1회 재시도. ctx·시한·scope·예산 예약 유지. 평가·타임라인·두 답변 경로가 공유 | §3.7. `internal/application/memory.go:1161`, `internal/application/memory_review.go:79`, `internal/application/eval.go:205`, `internal/application/timeline.go:103` |
| P2-3 실패 답변 | ErrBusy·빌드/FTS/identity 오류·backoff·로드 대기 만료는 살아 있는 턴에서 memory_unavailable+언어팩 안내+제한된 답 제안. 원래 턴 취소/시한 종료는 답변 중단. C-5에서 활성화 정책 승인 | §3.9·§9 Q4. `internal/chat/runtime.go:270`, `internal/chat/runtime.go:316`, `internal/chat/toolloop.go:405` |

§7의 0단계 범위와 1단계 게이트가 이 설계의 적용 경계다. C-1 수정안 검토에서 두 번 연속 REVISE면 사용자에게 보고하고 멈춘다. 1단계 이후는 C-5 명시 승인 전 시작하지 않는다. 추정 기간(§7, 약 2~3주)과 실기기 별도 조건은 유지한다.


## 13. 0단계 실측 결과 (C-4, 2026-10-13, Windows 흉내)

- 측정 장비: i5-6400T 4코어, Windows. 작은 기기는 `GOMAXPROCS=1`과 `GOMEMLIMIT`(1GB 기기 700MiB, 512MB 기기 350MiB)로만 흉내 냈다. 실기기, SD 카드 첫 읽기, 요약을 켠 방은 재지 않았다.
- 측정 방법: 반복 3회의 중앙값이다. Defender 등이 배경 CPU를 약 15% 썼다. 시간 값은 A/B 비교용이다.
- 측정 데이터:
  - 메시지 벡터는 512(`A`) 적용 뒤 기준이다.
  - 평가 세트 셋(5.6만·7.5만·10.2만 메시지)과, 이 셋을 저장된 임베딩 그대로 합친 루트(23.4만 메시지, leaf 3.3만, 메시지 벡터 10.7만)를 썼다.
- 실데이터 숫자 원본은 저장소 밖에만 있다. 도구는 `tools/evaluation/memory_dims/runner`의 `measure-reload`, `measure-rooms`, `dump-candidates -resident-seq-file`이다.

| 루트(메시지) | cold 로드 | 상주 heap | hot 재구축 heap 피크 | hot 피크 working set | 피크/상주(heap) |
|---|---|---|---|---|---|
| 5.6만 | 2.6초 | 73MiB | 153MiB | 210MiB | 2.03 |
| 7.5만 | 3.3초 | 83MiB | 177MiB | 229MiB | 2.07 |
| 10.2만 | 3.5초 | 92MiB | 209MiB | 262MiB | 2.21 |
| 23.4만 | 9.4초 | 247MiB | 568MiB(350MiB 한도에서 405MiB, 재구축 7→11초) | 661MiB | 2.27 |

- **피크 모델(§3.6).**
  - heap 피크 ≈ 2.0~2.3R로 가정이 맞았다. working set은 이 장비에서 ≈ 1.5~1.6R로 가정(2.4R)보다 낮았다.
  - 23.4만 루트의 피크 568MiB는 정상 250MiB, 새 세대 94MiB, 임시 할당·GC 여유 약 214MiB로 나뉜다.
  - 새 세대에서 줄일 수 있는 후보가 있다.
    - 방이 안 바뀌면 메시지 배열을 재사용한다: 약 26MiB
    - leaf 메타·본문을 다시 읽지 않는다: 남는 중복 약 40MiB, 일시 할당 129MiB
    - 메시지 벡터 맵: 약 12MiB
  - 재구축 직후 working set은 약 1분 동안 높게 남는다. 은퇴 세대의 FTS를 늦게 닫고 스캐빈저가 늦게 돌기 때문이다.
- **FTS 크기 F.**
  - leaf DB 페이지 바이트는 세트당 3.1~3.4MiB, 합본 9.4MiB다. 평가 루트에는 요약이 없다.
  - Go 밖 private 메모리는 루트 크기와 상관없이 약 55~58MiB다. 런타임·DLL·SQLite 기본 비용이다. 피크 모델에 F를 따로 더할 만큼 크지 않다.
- **방 올리기 시간(첫 질문 지연 근사).**
  - 지연은 더하는 방의 크기가 아니라 **다시 빌드하는 상주 메시지 총량**에 비례한다. 이 장비 1코어에서 약 40µs/메시지다.
  - 상주가 5.6만 이하면 2~3.5초, 23.4만이면 8~10초다(363개짜리 방을 더해도 같다).
- **추정치(C-2).**
  - `EstimatedBytes`/상주 heap은 0.95~0.96이다.
  - `EstimateRoomBytes`는 작은 방이 많을 때 +35%까지 크게 나온다. 방 고정비 3MiB가 과하기 때문이다. 보정은 1단계 전에 한다. 방 고정비는 약 1MiB, 메시지당 비벡터 바이트는 800~900B가 적당하다. 메시지 벡터 몫은 실제 벡터 수로 계산한다.
- **게이트(C-3 도구, 실데이터).**
  - 대상: 질문 300개(답 있음 240), 부분 상주 구성 8개, hybrid·auto 모드. Discord 세트는 rerank 모드도 봤다.
  - **필요한 방이 모두 상주한 문항**에서 정답이 맞음→틀림으로 바뀐 것은 0건이고, 틀림→맞음은 1건이다. 답 없음 문항의 변화도 0이다.
  - 다만 BM25 문서 통계가 바뀌므로 어휘 run의 순서는 거의 모든 행에서 바뀐다. 후보 집합은 4~40% 행에서 바뀌고, 최종 5개 집합은 hybrid에서 2~11행 바뀐다. rerank·auto에서는 0~2행이다.
  - {A,B}→{A,B,C}→{A,B}를 한 프로세스에서 돌린 결과는 같은 집합을 독립으로 빌드한 결과와 같았다(변경 0).
  - 범위 밖 문항은 이 도구로 재지 않았다. 1단계 `EnsureRooms`가 방을 먼저 올리기 때문이다.
- **parity.**
  - 전체와 상위 2방 부분 상주는 `fusion_diff 0`, `hits_diff 0`, `pool_diff 0`이다.
  - 3방 부분 상주 하나는 처음에 `hits_diff 0`이었지만 `pool_diff 3`이 났다(1문항 × 3모드).
    - 원인은 dense 동점이 아니라 어휘 정규화였다. Go `x/text`의 NFKC는 UAX #15 stream-safe 규칙에 따라 비시작 문자 30개 뒤에 U+034F를 넣는다. 그래서 호환 모음이 길게 이어진 질문(ㅠㅠ…)이 Go에서만 FTS phrase 둘로 갈라졌다.
    - 입력에 없던 U+034F를 정규화 뒤에 지우도록 고쳤다(`internal/memory/lexical/ngram.go`). 다시 돌린 부분·전체 parity는 모두 `pool_diff 0`이다. 두 경우는 아직 Python과 다를 수 있다. 입력에 CGJ가 원래 있던 경우와, 결합 등급이 서로 다른 표시가 30개 넘게 이어지는 경우다.
    - 같은 실행에서 fusion 54/55위가 바뀐 것은 이미 허용된 dense float32 동점(`fusion_epsilon`)이다.
- **기기별 결론.**
  - 512MB 기기(350MiB 한도)에는 단일 세트(10만 메시지 이하)만 들어가고 23.4만은 들어가지 않는다.
  - 1GB 기기(700MiB 한도)에는 23.4만이 빠듯하게 들어간다(working set 650~800MiB).
- **예산 비율(§9 Q3).**
  - 20%가 맞다. 512MB 기기에서 피크 2.2배와 Go 밖 60MiB를 더하면 약 285MiB로 soft limit(약 307MiB) 안이다.
  - 25%는 약 342MiB로 넘는다.
