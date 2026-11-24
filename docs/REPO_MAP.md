# 저장소 지도

디렉터리마다 한 줄. 패키지 층과 경계, 프로세스, 기능별 시작점은 [architecture/system.md](architecture/system.md)에, Action·명령·도구 하나하나의 위치는 [code-map/](code-map/README.md)에 있다.

## 최상위

| 경로 | 역할 |
|---|---|
| `cmd/masamong` | CLI·런처 진입점 |
| `cmd/masamongd` | 데몬 진입점: Discord Gateway, 콘솔 서버, 스케줄러, 기억 워커. 감시 프로세스(`--supervise`)와 오프라인 복구(`--maintenance`)도 여기 있다 |
| `internal/` | 제품 코드(아래 표) |
| `locales/` | Discord·운영 문구와 콘솔 번역(`locales/ui`) |
| `release-notes/` | 버전별 릴리스 노트(Markdown과 JSON). 실행 파일에도 들어간다 |
| `docs/` | 운영·개발 문서. [여기서 시작](README.md) |
| `tools/` | 문서 검사(`doccheck`), 코드 지도 생성(`codemap`), 이동 검사(`movecheck`), 릴리스 노트 초안(`relnotes`), 평가, 부하 테스트 |
| `experimental/rag-leaf-parent` | Python 검색 샌드박스. 제품 DB를 쓰지 않는다 |
| `contracts/` | 샌드박스와 Go가 함께 지키는 도구·블록 계약 |
| `fixtures/` | 합성 테스트 자료 |
| `assets/brand` | 제품과 배포에 쓰는 브랜드 에셋 |
| `packaging/`, `scripts/` | 배포 꾸러미, 릴리스·검증·parity 스크립트 |
| `.github/workflows/` | `ci`, `release`, `soak`, `wiki` |

## `internal/`

| 경로 | 역할 |
|---|---|
| `application` | Action 계층. `Service.Execute`와 기능 대부분 |
| `catalog`, `action` | Action 목록과 요청·결과 봉투 |
| `connectors/discord` | Discord Gateway, 명령, 답 보내기, 스레드, 로컬 시뮬레이터 |
| `transport/web` | loopback HTTP 서버, 세션, API, `/lite`, 내장 콘솔(`ui/`) |
| `cli`, `tui`, `tray`, `launcher`, `present` | 명령 해석과 출력, 트레이 아이콘, 데몬 찾기·시작·자동 시작 |
| `chat` | 답 한 턴: 플래너, 도구 루프, 예산 가드, 답변 프롬프트 |
| `conversation` | 부른 사람의 대화에 속하는 줄 고르기 |
| `memory/corpus`, `leaves`, `lexical`, `search`, `retrieval` | 원문 읽기, leaf 분할, 어휘 색인, 검색, 샌드박스와 공유하는 검색 계약 |
| `memory/store`, `engine`, `blocks` | 기억 표, 색인 적재·수집·색인 실행, 기억 블록 계약 |
| `memory/episodes`, `peoplebackfill` | 방 기억(L1·L2) 추출, 사람 기억 백필의 창과 비용 |
| `tools` | 도구 등록부, 한도, HTTP 사용자 도구 |
| `features` | 운세 달력 계산, 공지 수집, 리마인더 시간 읽기, 투표, 동의 범위, 끼어들기 규칙 |
| `providers` | 외부 API 어댑터: 모델(`openai`), 웹 검색(`websearch`), 날씨·금융·장소, 연습 모드(`practice`) |
| `lang`, `i18n`, `docnames`, `currency` | 언어팩, 메시지 키 해석, 명령 이름 규칙, 통화 이름 읽기 |
| `config`, `jobs` | 설정 문서와 리비전, 작업 상태 기계 |
| `storage` | SQLite 저장소(TiDB 코드는 `TiDBEnabled=false`로 꺼져 있다) |
| `secrets`, `fsutil` | 키 참조(`env:`, `file:`, `keychain:`), 소유자 전용 파일과 내구성 있는 쓰기 |
| `observability`, `logfile`, `crash`, `errclass`, `errcode` | 비밀값 가리기, 회전 로그, 실패 격리와 분류 |
| `relnotes`, `vision` | 내장 릴리스 노트, 붙인 사진 준비 |
| `clock`, `hostprofile`, `memlimit`, `testutil` | 주입하는 시계, 기기 성능 단계, 자동 메모리 한도, 공용 테스트 도우미 |

## 띄우는 프로그램 세 가지

| 화면 | 위치 | 실행 |
|---|---|---|
| 제품 관리 콘솔 | `internal/transport/web/`와 `ui/` | `masamong open` 또는 `masamong connect user@host`, 기본 8787 |
| 빠른 테스트 인스턴스 | 저장소 루트 `start-test.command`(macOS·Linux), `start-test.ps1`(Windows) | 더블클릭 또는 `make try`. `~/.masamong-try`에 띄운다 |
| 검색 테스트 샌드박스 | `experimental/rag-leaf-parent/` | `start-sandbox.command`, 기본 8765 |

샌드박스와 제품은 다른 프로그램이다. 샌드박스 인덱스는 제품 DB로 쓰지 않는다([TEST_SANDBOX.md](TEST_SANDBOX.md)).

## 설치·실행 스크립트(저장소 루트)

| 파일 | 역할 |
|---|---|
| `install.sh` | macOS·Linux·FreeBSD 설치(소스 빌드 또는 릴리스 내려받기). `--uninstall` |
| `install.ps1` | Windows 설치. `-Uninstall` |
| `start-test.command`, `start-test.ps1` | 격리된 빠른 테스트 인스턴스 |
| `start-sandbox.command` | 검색 테스트 샌드박스 |

## 관리 콘솔 파일

- 소스: `internal/transport/web/ui/src/`
- 페이지 목록: `ui/src/routes.ts`, 내비게이션: `ui/src/lib/nav.ts`
- 문구 원본: `ui/scripts/strings.py`(한국어·영어), 다른 언어는 `locales/ui/`
- 커밋하는 빌드 결과: `ui/dist/`(`make ui`로 다시 만든다)

## 로컬 전용 경로

다음은 Git에 넣지 않는다.

- `.private/`, `data/`, `test-sandbox/data/`: 원문, 키, 로컬 인덱스
- `.masamong-scratch/`: 평가 문항과 측정 결과
- `docs/archive/`: 과거 기획·설계 기록
- `output/`: UI 검증 스크린샷
- 개인 에디터·도구 설정 파일
- `assets/brand/source/`: 이미지 원본과 제작 기록

원문 본문, 사용자 ID, 비밀값은 공개 문서·로그·fixture에 넣지 않는다.
