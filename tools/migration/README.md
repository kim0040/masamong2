# 원본 사본 확보 도구

Python의 `PyMySQL`, `python-dotenv`가 필요하다. 현재 환경에 이미 설치된 모듈을 사용했고 패키지를 새로 설치하지 않았다. 제품의 Go/Rust runtime dependency는 아니다.

`export_tidb_snapshot.py`는 explicit env의 TiDB 하나를 읽기 전용 transaction으로 export한다. Discord/LLM/기존 config.py를 import하지 않는다. credentials는 argv에 전달하지 않는다. output은 신규 디렉터리만 허용하고 실패하면 manifest가 incomplete로 남는다. default240초 예산과 세션 scan concurrency1을 사용한다. timeout 뒤 이어붙이지 말고 새 snapshot을 만든다.

```sh
python3 tools/migration/export_tidb_snapshot.py \
  --env .private/legacy-config/.env \
  --expected-db masamong \
  --output .private/tidb-snapshot-NEW-DATE
python3 tools/migration/verify_snapshot.py \
  .private/tidb-snapshot-NEW-DATE \
  --staging .private/tidb-snapshot-NEW-DATE.staging.sqlite3
python3 -m unittest discover -s tools/migration -p 'test_*.py'
```

위 export는 remote 전체 SELECT로 비용과 부하를 발생시킨다. 이번 사본은 이미 검증했으므로 문서를 읽었다는 이유로 다시 실행하지 않는다. 새 snapshot이 필요한 사용자 작업 범위에서만 실행한다.

bundle: `manifest.json`, table별 `.schema.sql`, `.rows.jsonl.gz`, `verification.json`. 행 배열의 열 순서는 manifest의 columns와 같다. BLOB·Decimal·날짜/시간은 `$type` envelope으로 보존한다. JSON SQL 열은 원래 JSON 문자열로 유지한다. source timezone을 임의로 UTC로 변환하지 않는다.

검증 대상은 export 행의 codec round-trip, 원격 같은 snapshot의 COUNT, DDL/행 hash, SQLite source_rows에 저장 후 재조회한 bytes, integrity_check다. 운영 TiDB restore·V2 mapping·실제 모델 품질은 별도 검사다.

known limits: view가 있으면 실패한다. GRANT·계정·global 설정·post-snapshot delta는 수집하지 않는다. SQL 원형으로 restore하는 dump가 아니다. `.private` bundle은 신뢰된 로컬 도구가 생성한 입력 전용이며 임의의 외부 bundle을 여는 importer 보안 경계는 향후02/07에서 별도로 구현한다.
