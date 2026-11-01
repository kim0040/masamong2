# 저장소: SQLite (TiDB는 비활성)

제품 저장소는 **SQLite 하나**다(`data-root/masamong.db`). 설정·기억·대화 기록·사용량·개인 기능(동의·운세·공지)이 모두 이 파일에 들어간다.

제품 V2 테이블은 구 마사몽 30개 테이블과 **다른 계약**이다. 옛 snapshot을 그대로 붙여 쓰지 않는다. 원문은 RAW_DATA_POLICY대로 새 importer가 읽는다. 구 봇의 **개인 기능 설정**(동의 기록, 운세 등록, 학교·편입 공지 구독)만 `migration.apply`로 읽기 전용으로 가져온다(아래).

## TiDB를 끈 이유 (2026-09-28 운영자 결정)

두 DB를 지원하느라 테스트하고 돌봐야 할 것이 두 배가 됐다. 엔진별 DDL과 upsert, 저장소 이전 마법사, TiDB 전용 잠금·백업 경로, 실서버 CI 작업이 모두 여기에 해당한다. 한 SQLite 파일로 충분히 도는 개인용 봇이라 당분간 SQLite만 쓴다.

- 코드는 지우지 않았다. `internal/storage/settings.go`의 `TiDBEnabled = false`가 막고 있다. 이 상수가 `false`인 동안은 `storage.yaml`, `MASAMONG_DB_BACKEND`, `masamongd -db-backend`, setup 어느 경로로도 TiDB를 열 수 없다(`ErrTiDBDisabled`).
- `storage.migrate` Action은 `disabled_by_operator`로 꺼져 있다. 콘솔의 저장소 카드에는 SQLite 상태만 나오며, 유지보수 복구 페이지에는 별도로 복구 안내와 현재 유효한 명령이 나온다.
- CI의 TiDB 실서버 작업은 사유와 함께 주석 처리했다. `MASAMONG_TIDB_TEST_DSN`이 있어도 live 테스트는 건너뛴다.
- 새로 만든 개인 기능 표(`consent_state`, `fortune_profiles`, `notice_*`, `user_prefs`, `channel_summary` 등)는 SQLite DDL만 있다. 다시 켤 때는 이 표들의 TiDB DDL부터 만들어야 한다.

### TiDB를 쓰던 인스턴스 옮기기

1. 이전 버전으로 백업을 만든다. TiDB 백업은 논리 JSON dump(`dump.json`)다.
2. `storage.yaml`에서 `backend: tidb`를 지운다. 그러면 SQLite가 기본값이 된다.
3. 이 버전을 시작하고 **유지보수 → 백업**에서 그 백업을 복원한다. dump 복원은 SQLite 인스턴스에서도 동작한다.

## 상태 보기

```bash
masamong --data-root DIR storage status   # 엔진, 위치, 표별 행 수
```

콘솔 **유지보수 → 저장소** 카드도 같은 Action을 쓴다. 파일 위치를 옮기거나 되돌릴 때는 백업·복원을 쓴다([저장소와 백업](wiki/저장소와-백업.md)).

## 복원·초기화·백업이 건드리는 파일

- **실제로 쓰는 DB가 기준이다.** `storage.yaml`의 `sqlite_file`로 다른 파일을 골랐다면 초기화는 `masamong.db`가 아니라 그 파일(과 `-wal`·`-shm`·롤백 저널)을 지우고 `storage.yaml`도 지운다. 복원은 그 파일 자리에 설치한다. 초기화는 데이터 폴더 밖이거나 링크 뒤에 있는 DB 경로를 거절하고, 설정이 가리키기만 하는 외부 원본 파일은 지우지 않는다.
- **백업**은 한 시점의 DB 스냅샷과, 그 스냅샷 안에서 가장 최근에 커밋된 설정(리비전 포함)을 함께 담는다. 숨김 폴더에서 완성·검증한 뒤 한 번에 공개하고, 같은 초에 만든 백업도 각자 폴더를 받는다. 검증은 체크섬, SQLite 무결성, 제품 스키마, 설정 파싱을 본다. TiDB 시절의 논리 dump(`dump.json`)는 격리된 빈 SQLite에 가져와 검증한다.
- **복원·초기화·프로그램 교체**는 같은 저널 형식(`maintenance-operation.json`, 설치 폴더의 `.masamong-update.json`)과 복구 엔진을 쓴다. DB를 열기 전에 마무리하고, 못 끝내면 봇을 띄우지 않은 채 현지화된 복구 안내를 제공합니다. `masamong open`과 트레이의 열기로 안내를 볼 수 있고 `masamong stop`으로 데몬을 멈출 수 있습니다. 안내와 `masamongd --maintenance status`는 현재 상태에서 쓸 수 있는 명령만 보여 줍니다. 자세한 단계와 명령은 [저장소와 백업](wiki/저장소와-백업.md), [CLI 명령어](wiki/CLI-명령어.md).
- **초기화**는 현재 DB와 `imports/`, `rag_staging/`, `memory/`, `rag/`, `images/`, `notices/`, `practice/`, 설정·복원 표식을 지운다. `backups/`, `logs/`, `admin/`, `sessions.json`, 데몬·잠금 메타데이터는 유지한다. 키 파일과 현재 인스턴스의 자격 증명 관리자 항목은 별도 삭제 옵션을 켰을 때만 지운다.

## 이전 봇 개인 설정 가져오기 (`migration.apply`)

```bash
masamong --data-root DIR migration apply --path /경로/v1.db                 # 계획만: 가져올 것·건너뛸 것과 이유
masamong --data-root DIR migration apply --path /경로/v1.db --confirm true
```

- 옛 파일은 `mode=ro&immutable=1`로 열고 쓰지 않는다.
- 대화 내용은 가져오지 않는다. 기억은 원문 archive에서 새로 만든다.
- 현재 정책 버전에 동의한 사람의 데이터만 가져온다. 동의 버전은 V1과 같게 유지했다.
- 이 인스턴스에 이미 있는 데이터는 덮어쓰지 않는다.
- 콘솔: **유지보수 → 이전 봇 개인 설정 가져오기** (미리 보기 → 가져오기).

## 아닌 것

- 구 TiDB 사본을 V2로 자동 이전
- 샌드박스 `test-sandbox/data/` numpy 인덱스를 제품 DB에 넣기
- 런타임에 SQLite SQL을 MySQL 문법으로 치환하기
