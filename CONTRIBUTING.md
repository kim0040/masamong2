# Masamong에 기여하기

Masamong에 관심을 가져 주셔서 감사합니다. Maintainer는 **김현민 (Hyunmin Kim)**이며 연락처는 [mini0227kim@gmail.com](mailto:mini0227kim@gmail.com)입니다.

## 작업 전

1. 이슈 또는 변경 목적을 한 문장으로 정리합니다.
2. `git status`, 최근 commit, 현재 diff를 확인합니다.
3. 진행 중인 파일과 추적되지 않은 파일을 보존합니다. `reset`, `restore`, `clean`, 강제 push로 다른 작업을 지우지 않습니다.
4. 관리자 GUI와 테스트 샌드박스를 혼동하지 않도록 [저장소 지도](docs/REPO_MAP.md)를 확인합니다.

## 개발 환경

- Go 1.27 이상
- 관리 GUI 변경 시 Node.js와 npm. GUI 빌드는 문구 생성기 때문에 Python 3도 씁니다(`python3`, `python`, `py -3` 순으로 찾고 Microsoft Store 스텁은 건너뜁니다)
- 선택 사항: Python 3과 NumPy는 `experimental/` 및 평가 도구용
- 선택 사항: PyMySQL은 TiDB snapshot 도구용
- macOS, Linux, Windows 모두 지원 대상입니다. Windows는 PowerShell이면 충분하고 `make`는 선택입니다. 격리된 시험 인스턴스는 `start-test.command`(macOS·Linux) 또는 `start-test.ps1`(Windows)로 띄웁니다([개발 가이드](https://github.com/kim0040/masamong2/wiki/개발-가이드))

```bash
git clone https://github.com/kim0040/masamong2.git
cd masamong2
go test ./...
```

## 코드와 문서 규칙

- 제품 기능은 `internal/application`의 Action을 통해 구현하고 관리 GUI 흐름까지 연결합니다.
- GUI 원본은 `internal/transport/web/ui`, 생성 결과는 `internal/transport/web/ui/dist`입니다.
- GUI 문구는 `ui/scripts/strings.py`에서 생성합니다. 생성된 locale JSON만 직접 수정하지 않습니다.
- 카탈로그 JSON은 UTF-8로 저장합니다. Windows PowerShell 5.1에서는 `Set-Content -Encoding utf8`을 지정해야 시스템 코드 페이지로 한글·일본어 등이 깨지지 않습니다. 가능하면 저장소의 Python 카탈로그 생성 스크립트를 사용하세요. `go test ./locales`가 잘못된 코드 페이지로 망가진 문구를 검사합니다.
- 새 화면 언어는 `locales/ui/README.md`, 봇 언어는 `docs/tools-and-languages.md`의 절차를 따릅니다.
- 검색 동작을 바꾸면 Python 샌드박스와 Go 제품 구현, parity 테스트를 함께 확인합니다.
- catalog에서 Deferred인 Action을 성공 응답이나 fixture로 완료된 것처럼 표시하지 않습니다.
- 문서는 현재 코드와 실제 검증 결과를 기준으로 쓰고, 과거 계획은 역사적 문서라고 표시합니다.
- 위키와 README는 한국어가 기준이고 영어(README는 다섯 언어)를 함께 고칩니다. 고친 뒤 `go run ./tools/doccheck -write`로 번역 상태를 기록하고 `go run ./tools/doccheck`가 통과해야 합니다.
- 설치·실행·문제 해결 문서는 PowerShell 예시를 bash 옆에 함께 둡니다(`export` 대신 `$env:NAME = '…'` 등). 키는 "암호화"라고 쓰지 않습니다. macOS Keychain, Windows 자격 증명 관리자, Linux의 소유자 전용 파일이라고 사실대로 적습니다.
- Windows에서 동작이 다른 코드(권한, 파일 잠금, 경로 구분자, 종료 순서)는 Windows에서도 도는 테스트를 함께 둡니다. 단위 테스트는 Unix 권한 비트 대신 `fsutil.VerifyPrivateFile` 같은 도우미로 확인합니다.

## 데이터와 비밀값

다음 항목은 commit, issue, pull request, 테스트 fixture에 넣지 않습니다.

- `.private/`, `data/`, `test-sandbox/data/`
- 실제 메신저 대화, 사용자 ID, 운영 DB와 백업
- Discord token, API key, 비밀번호, session cookie, CSRF 값
- 로컬 `sessions.json`, Playwright scratch, 임시 로그

원문은 수정하지 않고 읽기 전용 입력으로 취급합니다. 테스트는 합성 fixture와 별도 임시 data root를 사용합니다.

## 의존성과 라이선스

- 유료 라이선스가 필요한 라이브러리, 폰트, 아이콘, 데이터셋을 추가하지 않습니다.
- 외부 API는 선택 사항이어야 하며 필요한 키, 비용 가능성, 호출 범위를 문서화합니다.
- 의존성을 바꾸면 `python3 scripts/license_audit.py`를 실행하고 [라이선스 감사 문서](docs/license-audit.md)를 갱신합니다.
- 저작권 고지 또는 별도 라이선스가 있는 자산을 추가하면 `THIRD_PARTY_NOTICES.md`와 배포물 고지도 함께 갱신합니다.

## 필수 검사

변경 범위에 맞는 검사를 먼저 실행하세요. 아래 전체 gate는 GitHub Actions `ci` workflow가 pull request마다 실행합니다. CI가 돌지 않은 변경(예: fork에서 Actions가 꺼져 있을 때)은 로컬 결과(명령과 출력)를 pull request에 적습니다. 저사양 컴퓨터에서는 바뀐 패키지만 `CI=true go test -count=1 -p 1 <패키지>`로 순서대로 돌립니다([유지보수 가이드](docs/MAINTAINING.md) §1·§5).

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
python3 scripts/license_audit.py        # Windows에서는 python scripts\license_audit.py
go run ./tools/doccheck                 # 링크와 번역 상태

cd internal/transport/web/ui
npm ci
npm run check
npm run build
```

`ci` workflow에는 Windows 작업이 있어 pull request마다 Windows에서 `go vet`, `go test`, GUI 빌드, Python 검사를 돌립니다.

GUI를 수정했다면 `ui/dist`를 source와 함께 commit하고 실제 브라우저에서 desktop, mobile, dark mode, keyboard focus, reduced motion을 확인합니다. 외부 API나 Discord 호출을 실행했다면 비용과 실제 side effect를 pull request에 기록합니다.

## Pull request에 적을 내용

- 무엇을 왜 바꿨는지
- 사용자에게 보이는 동작
- 데이터 migration 또는 되돌리기 방법
- 실행한 테스트와 결과
- 실행하지 못한 실제 환경 테스트
- 외부 API 호출, 비용, 운영 side effect 여부
- 새 의존성의 라이선스와 출처

버그와 기능 요청은 재현 가능한 최소 정보만 공유하세요. 보안 문제는 공개 issue 대신 [SECURITY.md](SECURITY.md)를 따릅니다.
