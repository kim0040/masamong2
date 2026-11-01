# 원문과 검색 산출물의 수명주기

카카오와 Discord는 유입 방식이 다르지만 원문 저장 이후 같은 분할·색인·검색 계약을 사용합니다.

## 소스별 유입

### 카카오톡 CSV

- 사용자가 내보낸 파일을 관리 콘솔에서 가져옵니다.
- 안정 ID는 방·시각·화자·본문과 같은 시각에 같은 행이 나온 순번으로 만듭니다. 파일 바이트나 행의 인접성에 의존하지 않아, 같은 시각에 다른 메시지 사이에 있는 동일 행도 구분합니다.
- 기존 관찰 ID는 정본으로 유지하고 새 행 정체성의 별칭을 저장합니다. 그래서 업그레이드 전 ID, 편집한 CSV의 재가져오기, CSV와 오프라인 원문 형식 사이의 가져오기를 중복 없이 처리합니다.
- 방·시각 그룹에 서로 다른 메시지가 너무 많아 제한된 출현 횟수 상태를 유지할 수 없으면 `imports.limit.timestamp_group` 안내를 내고 파일을 방과 기간별로 나누도록 합니다.
- 새 파일은 새 원문 관찰로 추가하며 기존 파일이나 원문을 수정하지 않습니다.
- 계정 ID가 없으므로 표시 이름만으로 인물을 자동 병합하지 않습니다.
- timezone이 없는 시각에는 임의의 UTC offset을 만들지 않습니다.

### Discord 과거 원문

- 허용된 history/archive 필드만 bootstrap 입력으로 사용합니다.
- 방 키는 `discord:{guild_id}:{channel_id}`입니다.
- 같은 native message ID의 재수입은 중복 저장하지 않습니다.
- 내용이 달라지면 revision으로 다루고 이전 원문을 덮어쓰지 않습니다.
- 과거 embedding, window와 요약은 가져오지 않습니다.

### Discord 실시간 메시지

Gateway의 create, edit, delete와 bulk-delete 이벤트는 먼저 원문 revision 또는 tombstone을 저장한 뒤 해당 방의 증분 작업을 예약합니다. capture는 모델과 색인 공급자의 상태에 의존하지 않습니다.

## 공통 처리

```text
source adapter
  → 원문 observation/revision commit
  → 해당 방의 tail leaf 재계산
  → lexical index 갱신
  → 활성 embedding space가 있으면 새 leaf만 embedding
  → 연결된 채널에서 새 scope revision 사용
```

| 변경 | 원문 | leaf | embedding |
|---|---|---|---|
| 새 메시지 | insert | 방 tail 갱신 | 새 leaf만 |
| edit | revision 추가 | 포함 leaf 무효화·재생성 | 해당 leaf만 |
| delete | tombstone | 포함 leaf 무효화·재생성 | 해당 leaf만 |
| 별칭 변경 | 변경 없음 | 변경 없음 | 재생성 없음 |
| 채널 연결 변경 | 변경 없음 | 변경 없음 | 재생성 없음 |
| 모델 변경 | 변경 없음 | 유지 | 새 immutable space (이전 벡터는 캐시에 보관) |
| 차원 축소 | 변경 없음 | 유지 | 저장된 벡터를 잘라 새 space로 이동 (호출 없음) |

## 비용과 실패

- 원문 import와 lexical 색인은 provider 호출 없이 완료할 수 있습니다.
- embedding과 요약은 실행 전에 예상 호출 수, 비용과 시간을 보여 줍니다.
- 취소해도 이미 저장된 원문과 완료된 안전한 산출물은 지우지 않습니다.
- 재시도는 저장된 작업 상태와 현재 recipe를 확인한 뒤 수행합니다.
- 새 embedding space는 준비와 검증이 끝나기 전까지 active로 전환하지 않습니다.

## 범위와 삭제

- leaf는 원래 source room 안에서만 만듭니다.
- 여러 블록을 한 채널에 연결해도 원문 방과 provenance는 유지합니다.
- 검색 전후에 현재 연결 revision, 허용 목적, 제외와 tombstone을 확인합니다.
- 연결을 해제한 뒤에는 이전 scope의 결과와 캐시를 재사용하지 않습니다.

운영 절차는 [기억 운영](../memory-operations.md), 입력 제한은 [원문 데이터 정책](../RAW_DATA_POLICY.md)을 따릅니다.
