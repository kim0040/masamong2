# ADR-18 0단계 후보 게이트

제품 설정·콘솔에 부분 상주를 노출하지 않는다. `dump-candidates`는 별도 엔진을 만들어 `ResidentRoomsForMeasurement`를 설정하고 정지 상태 저장소를 읽는다. `keep`의 방 삭제 방식은 이 게이트에 사용하지 않는다. 실제 API 호출 없이 합성 데이터를 만들고 검증할 수 있다.

모든 상대 경로는 저장소 루트를 기준으로 해석하며, 데이터 루트·문항·덤프·rerank 기록·비교 결과는 저장소 루트의 `.masamong-scratch/` 아래에 둔다. 실제 본문·방 ID·계정·닉네임은 커밋하거나 콘솔·보고서에 출력하지 않는다. 후보 덤프에도 본문·닉네임은 넣지 않지만 ID가 있으므로 비공개 파일로 취급한다. 새 합성 루트는 기존 DB를 덮어쓰지 않는다. scratch 경로에 있는 기존 심볼릭 링크·정션은 대상이 없거나 외부를 가리켜도 거부한다.

## 합성 실행

PowerShell에서 저장소 루트를 기준으로 실행한다. 아래 방·계정·문항은 모두 합성이다.

```powershell
go run ./tools/evaluation/memory_dims/runner synthetic-root -root .masamong-scratch/c3-gate
go run ./tools/evaluation/memory_dims/runner dump-candidates -synthetic -root .masamong-scratch/c3-gate -gold .masamong-scratch/c3-gate/gold.json -out .masamong-scratch/c3-full-1.jsonl -rerank-mode record -tape .masamong-scratch/c3-rerank.json
go run ./tools/evaluation/memory_dims/runner dump-candidates -synthetic -root .masamong-scratch/c3-gate -gold .masamong-scratch/c3-gate/gold.json -out .masamong-scratch/c3-full-2.jsonl -rerank-mode replay -tape .masamong-scratch/c3-rerank.json
go run ./tools/evaluation/memory_dims/runner compare-candidates -left .masamong-scratch/c3-full-1.jsonl -right .masamong-scratch/c3-full-2.jsonl -out .masamong-scratch/c3-repeat-diff.json
```

10문항 × 기본 `hybrid,rerank,auto`로 30행이다. 반복 비교 결과는 `changed=0 regressed=0 improved=0`이다. 여러 방 scope, 범위 밖에서만 관측한 별칭, 시간 필터, 기록 없는 부정 문항, 존재하지 않는 방, 원래 빈 scope, 기본 auto confidence·recovery를 포함한다. 부정 문항은 시간 범위 안에 해당 기록이 없는 경우이며, 별도 의미 판정 모델을 쓰지 않는다.

부분 집합은 새 요청을 만들 수 있으므로 먼저 두 실행의 요청 합집합을 같은 기록 파일에 수집한다. 같은 질의·문서 순서·topN 요청은 SHA-256 키로 같은 응답을 재사용한다. 응답만 저장하며 원문 요청은 기록하지 않는다. `replay`에서 빠진 요청은 rerank fallback으로 숨기지 않고 명령을 실패시킨다.

`-rerank-mode off`는 외부 rerank 응답 없이 `hybrid`·`auto` 비교를 할 때 쓴다. 이 상태에서 명시 `rerank` 모드를 요청하면 사용할 reranker가 없으므로 `hybrid`로 조용히 대체된다. 실제 rerank 결과를 비교하려면 승인된 `record` 또는 기록된 tape를 쓰는 `replay`를 사용한다.

```powershell
go run ./tools/evaluation/memory_dims/runner dump-candidates -synthetic -root .masamong-scratch/c3-gate -gold .masamong-scratch/c3-gate/gold.json -resident discord:gate:A,discord:gate:B -out .masamong-scratch/c3-ab-prepare.jsonl -rerank-mode record -tape .masamong-scratch/c3-rerank.json
go run ./tools/evaluation/memory_dims/runner dump-candidates -synthetic -root .masamong-scratch/c3-gate -gold .masamong-scratch/c3-gate/gold.json -resident discord:gate:A,discord:gate:B -out .masamong-scratch/c3-ab.jsonl -rerank-mode replay -tape .masamong-scratch/c3-rerank.json
go run ./tools/evaluation/memory_dims/runner compare-candidates -left .masamong-scratch/c3-full-2.jsonl -right .masamong-scratch/c3-ab.jsonl -out .masamong-scratch/c3-partial-diff.json
```

`gate-partial`은 방 C를 scope로 요청하므로 C가 비상주인 실행에서 세 모드 모두 맞음→틀림을 드러낸다. `gate-alias`는 C에서만 관측한 표시명으로 A를 검색하며 부분 상주에서도 유지된다. `{A,B}→{A,B,C}→{A,B}` 복귀는 세 번의 독립 `dump-candidates`로 실행하고 처음·마지막 덤프를 비교한다. 합성 테스트도 이 순서를 검증한다.

## C-4 실데이터 실행

운영 루트가 아닌 **정지 상태 복사본**을 `.masamong-scratch/c4-root/`에 준비한다. 해당 루트의 제품 설정으로 별도 엔진을 만들며 제품 엔진이나 config를 수정하지 않는다. 루트의 저장 공간 적응·마이그레이션은 기존 로더와 같으므로 복사본을 사용한다. 질의·semantic query 벡터는 같은 embed space와 query instruction으로 이미 캐시되어 있어야 한다. 누락되면 외부 임베딩 호출 없이 실패한다. A·B 통합 뒤 메시지 벡터 512를 사용하려면 `-msgvec -msgdims 512`를 붙인다.

문항 JSON은 `questions` 배열이며 기존 `question_id`, `query`, `want_observation_ids`, `expected_abstention`, `room`을 읽는다. 여러 방은 `rooms` 배열, 무제한 scope는 `all:true`, 원래 빈 scope는 `rooms:[]`로 표현한다. 선택적으로 `intent`(targets·time·semantic_query 등)와 RFC3339 `anchor_time`을 넣는다. 문항 본문·실제 ID는 scratch에만 두고 실데이터 문항을 저장소에 추가하지 않는다.

승인된 C-4 공급자 실행에서 `-rerank-mode record`로 전체·부분 요청 합집합의 응답을 먼저 수집한다. 실데이터 `record`만 구성된 rerank 공급자를 호출할 수 있으며, 이 작업의 합성 검증은 가짜 공급자만 사용한다. 이후 아래 명령은 캐시와 응답 재생만 사용한다. `resident-c4.json`에는 실제 방 ID 문자열 배열을 넣어 명령줄에 노출하지 않는다.

```powershell
go run ./tools/evaluation/memory_dims/runner dump-candidates -root .masamong-scratch/c4-root -gold .masamong-scratch/c4-gold.json -msgvec -msgdims 512 -out .masamong-scratch/c4-full.jsonl -rerank-mode replay -tape .masamong-scratch/c4-rerank.json
go run ./tools/evaluation/memory_dims/runner dump-candidates -root .masamong-scratch/c4-root -gold .masamong-scratch/c4-gold.json -msgvec -msgdims 512 -resident-file .masamong-scratch/resident-c4.json -out .masamong-scratch/c4-partial.jsonl -rerank-mode replay -tape .masamong-scratch/c4-rerank.json
go run ./tools/evaluation/memory_dims/runner compare-candidates -left .masamong-scratch/c4-full.jsonl -right .masamong-scratch/c4-partial.jsonl -out .masamong-scratch/c4-diff.json -fail-on-regress
```

덤프는 최초 generate와 recovery 이후의 재순위 전 전체 후보 순위·독립 RRF run 순위/가중치·보장 pool·auto confidence, 최종 hit ID/순위/점수·recovery 여부를 담는다. 비교 JSON은 문항별 맞음→틀림·틀림→맞음, 각 generate 단계의 후보 진입·이탈·순위, pool/run 순위 변동과 전후 결과를 담는다. 콘솔은 문항 ID·개수만 출력하며 `entered`·`exited`는 융합 후보 전체 기준이다. 게이트 통과 = `regressed=0`(또는 사람이 판정한 허용 목록)이다. `-fail-on-regress`는 회귀가 하나라도 있으면 종료 코드 1을 반환한다. `correct`는 원하는 observation이 최종 hit의 leaf 또는 병합 leaf에 들어갔는지이며, 답변 정확도를 자동 판정하지 않는다.

`-resident all`은 필터 없는 빌드, `-resident none`과 빈 resident 파일은 빈 상주 집합이다. 저장소에 없는 상주 방 ID는 실제 ID를 출력하지 않고 개수만 보고 오류 처리한다. 전역 identity는 전체 최종 적격 leaf의 메시지 구간에서만 만들고 상주 leaf 연결은 로컬이다. 최종 전역 테이블은 계정·표시명 수에 비례한다. 측정 빌드는 전역 위치 검증용 본문 없는 메시지 배열과 leaf 메타를 임시로 읽으므로 입력 수와 `identity_prepare_us`를 함께 출력한다. 이 시간은 leaf 메타·메시지 필드 읽기·적격성 선택·전역 테이블 생성이며 전체 reload 피크와는 별도다. 실제 저장소 경쟁·무효화·EnsureRooms·내려놓기 정책은 1단계 범위다.

## 추가 부분 집합 sandbox parity

기존 전체 parity 본문은 그대로다. 별도 부분 집합 덤프는 Python `parity_dump.py`와 Go 로더에 동일한 `MASAMONG_SANDBOX_ROOMS_FILE`(scratch 문자열 JSON 배열)을 넘긴다. 양쪽 모두 메시지 위치·leaf/summary 벡터·FTS를 부분 집합으로 재구축하고 identity의 표시명·계정 표는 전체 입력으로 유지한다. Python은 기존 sandbox 환경(numpy 등)을 사용한다. 설정 경로는 절대 경로로 지정한다.

```powershell
$env:MASAMONG_SANDBOX_CONFIG=(Resolve-Path .masamong-scratch/c4-sandbox/config.json).Path
$env:MASAMONG_SANDBOX_DATA=(Resolve-Path .masamong-scratch/c4-sandbox/data).Path
$env:MASAMONG_SANDBOX_ROOMS_FILE=(Resolve-Path .masamong-scratch/resident-c4.json).Path
python experimental/rag-leaf-parent/parity_dump.py .masamong-scratch/c4-parity partial
$env:MASAMONG_PARITY_RETRIEVE=(Resolve-Path .masamong-scratch/c4-parity/retrieve_partial.json).Path
$env:CI='true'
go test -count=1 -p 1 ./internal/memory/search -run '^TestSandboxPartialRetrieveParity$'
```

부분 비교 뒤 환경 변수를 해제하고 기존 전체 parity를 실행한다. 실제 sandbox 실행은 C-4 담당자가 수행한다. 합성 테스트는 같은 Go 부분 로더·전역 identity·전체 집합 필터를 검증한다.

## C-4 실측 도구 (상주 메모리, 스크래치 복사본 전용)

`measure-reload`와 `measure-rooms`는 `dump-candidates`처럼 정지 상태 사본을 독립 엔진으로 열고 공급자를 부르지 않는다. 방 ID·본문·이름은 출력하지 않고 숫자만 낸다(방은 메시지 수 순위로 부른다). 작은 기기 흉내는 프로세스 환경이다: `GOMAXPROCS=1`과 `GOMEMLIMIT`(1GB 기기 `700MiB`, 512MB 기기 `350MiB`). 결과 JSON은 `-out`(스크래치 아래)에 쓴다.

```powershell
# cold 로드 → 첫 검색(지연 표 생성) → 변경 없는 hot 재구축 1회. 힙·런타임·working set, 단계 경계 표본, 폴링 피크, EstimatedBytes, FTS 페이지 바이트
$env:GOMAXPROCS='1'; $env:GOMEMLIMIT='700MiB'
go run ./tools/evaluation/memory_dims/runner measure-reload -root .masamong-scratch/<root> -msgvec -out .masamong-scratch/<out>.json
# 구성별 할당(느림): hot 재구축의 할당·남은 힙을 pprof로 저장, -settle은 교체된 색인의 FTS가 닫히는 1분을 기다려 다시 잰다
go run ./tools/evaluation/memory_dims/runner measure-reload -root ... -msgvec -prof .masamong-scratch/<dir> -settle -out ...
# 상주 집합 {나머지}에서 {나머지 + 한 방}으로 다시 빌드하는 벽시계·피크. -rank는 대상 방의 메시지 수 순위(1 = 가장 큼), -rest-ranks는 이미 올라온 방의 순위(비우면 나머지 전부)
go run ./tools/evaluation/memory_dims/runner measure-rooms -root ... -msgvec -rank 3 -rest-ranks 1 -reps 3 -out ...
```

- 힙 피크는 5ms 폴링의 최댓값(`runtime/metrics`), working set은 운영체제 값(Windows `GetProcessMemoryInfo`, 그 밖에는 `/proc/self/status`)이다. 폴링이 놓친 순간은 더 높을 수 있으므로 하한이다.
- 같은 프로세스에서 여러 번 재구축하면 1분 안에 교체된 색인의 FTS가 겹쳐 남는다(`closeLater`). 그래서 `measure-rooms`의 첫 반복이 깨끗한 값이고, 뒤 반복은 연속 교체 때의 값이다.
- `dump-candidates -resident-seq-file`은 `[[A,B],[A,B,C],[A,B]]`처럼 상주 집합 배열을 한 프로세스에서 차례로 적용한다(앞 단계의 행을 이어 쓰는 제품 경로). 단계별 결과는 `<out>.stepN.jsonl`이다.
- `-max-spend <USD>`는 `-rerank-mode record`의 공급자 비용 상한이다. 상한에 닿으면 기록을 멈추고 실패로 드러내며, 그때까지 받은 응답은 tape에 남는다. 실행 끝 줄의 `rerank_spent_usd`가 이번 실행의 비용이다.
