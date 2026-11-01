# 명령행 프로그램

제품은 같은 Application Action을 GUI와 CLI에서 사용합니다. 일반 사용자는 `masamong open`으로 관리 GUI를 여는 흐름을 권장합니다.

| 바이너리 | 역할 |
|---|---|
| `masamong` | 사용자 CLI와 런처. Action 명령, daemon lifecycle, 브라우저 열기, tray, autostart, SSH 연결을 담당 |
| `masamongd` | 장기 실행 daemon. Discord, job, 저장소와 `internal/transport/web` 관리 콘솔을 소유 |

## 빌드

```bash
go build ./cmd/masamong
go build ./cmd/masamongd
# 또는 루트에서
make build
```

GUI 정적 파일은 `internal/transport/web/ui/dist`에 포함되어 있어 일반 Go 빌드에 Node.js가 필요하지 않습니다.

## lifecycle

```bash
masamong open
masamong start
masamong status
masamong restart
masamong stop
masamong tray
masamong autostart on
masamong autostart
masamong autostart off
```

- `open`은 실행 중인 인스턴스를 재사용하고 1회용 로그인 링크로 기본 브라우저를 엽니다.
- `start`는 브라우저 없이 daemon을 시작하므로 headless server에 적합합니다.
- 정상 `restart`에서는 살아 있는 GUI session을 권한 `0600` 임시 handoff 파일로 넘기고 다음 시작에서 읽은 즉시 삭제합니다.
- `--output json`은 자동화용입니다. pipe, `CI`, `NO_COLOR`에서도 JSON이 기본입니다.

## data root

```bash
masamong --data-root /tmp/masamong-test open
MASAMONG_DATA_ROOT=/tmp/masamong-test masamong status --output json
```

설정, DB, 백업, 로그, credential 참조, 관리자 session은 data root 단위로 격리됩니다. 테스트 인스턴스와 실제 인스턴스에 같은 경로를 쓰지 마세요.

## Action 명령

카탈로그의 CLI 문자열은 Action ID와 연결됩니다.

```bash
masamong help version
masamong config show
masamong backup list
masamong memory search --rooms kakao:friends --query 텐트
```

flag는 Action 입력 이름을 사용하고, 쉼표로 구분한 문자열 입력은 Action에서 목록으로 읽습니다. 결과 스키마를 보존하려면 `--output json`을 사용하세요. 객체 편집, 비용, 외부 발송, 삭제, 복원 작업은 GUI에서 영향과 확인 단계를 보는 흐름을 권장합니다.

## 원격 관리

```bash
masamong connect user@server
masamong connect ssh://user@server:2222
```

SSH로 원격 daemon의 loopback 관리 주소를 찾고 로컬 터널과 브라우저를 엽니다. 원격 콘솔 포트를 공인 주소에 노출하지 않습니다. 고급 SSH 설정은 `~/.ssh/config`의 Host 별칭을 사용할 수 있습니다.

## daemon 직접 실행

설치기와 `masamong start`가 일반적인 진입점입니다. 개발·진단에서만 직접 실행합니다.

```bash
masamongd --data-root /tmp/masamong-test --web
```

TiDB 백엔드 옵션(`--db-backend tidb`)은 코드에 남아 있지만 꺼져 있어 실행하면 거절됩니다(`internal/storage/settings.go`의 `TiDBEnabled = false`, [저장소 문서](../docs/STORAGE.md)).

secret 값 자체를 명령행이나 로그에 남기지 말고 `env:` 또는 data root의 권한 제한 파일 참조를 사용하세요.

Python 테스트 샌드박스(`experimental/rag-leaf-parent`, 포트 8765)는 이 패키지와 제품 data root를 사용하지 않습니다. 디렉터리 구분은 [저장소 지도](../docs/REPO_MAP.md)를 참조하세요.
