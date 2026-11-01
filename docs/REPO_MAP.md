# 저장소 지도

## 두 화면

| 화면 | 위치 | 실행 |
|---|---|---|
| 제품 관리 콘솔 | `internal/transport/web/`와 `ui/` | `masamong open` 또는 `masamong connect user@host`, 기본 8787 |
| 빠른 테스트 인스턴스 | 저장소 루트 `start-test.command`(macOS·Linux), `start-test.ps1`(Windows) | 더블클릭 또는 `make try`. 소스 빌드 후 `~/.masamong-try`(Windows는 `%USERPROFILE%\.masamong-try`)로 띄우고 콘솔을 엶. Windows 스크립트는 `-Reset`·`-Check`·`-Stop`을 받음 |
| RAG 테스트 샌드박스 | `experimental/rag-leaf-parent/` | `test-sandbox/start.command`, 기본 8765 |

두 화면은 서로 다른 프로그램입니다. 샌드박스 인덱스는 `test-sandbox/data/`에만 저장하며 제품 DB로 사용하지 않습니다.

## 주요 디렉터리

| 경로 | 역할 |
|---|---|
| `cmd/masamong` | CLI, 런처, SSH 연결과 서비스 명령 |
| `cmd/masamongd` | SQLite, Discord Gateway와 관리 콘솔을 실행하는 데몬. 시작할 때 끊긴 복원·초기화·업데이트를 먼저 마무리하고(못 하면 현지화된 복구 안내 화면을 제공하며 `masamong stop`으로 종료 가능), 오프라인 복구 명령 `--maintenance status\|doctor\|retry\|rollback\|cancel\|abandon-update`과 `--version`을 가짐 |
| `internal/application` | GUI와 CLI가 함께 호출하는 Action 계층. 백업·복원·초기화·프로그램 교체는 `backup.go`·`reset.go`·`selfupdate.go`와 공통 저널·복구 엔진 `maintenance.go`에 있음 |
| `internal/transport/web` | loopback HTTP 서버, 세션, API, `/lite`, Svelte 콘솔 |
| `internal/memory` | 원문, leaf, lexical/vector 검색, 블록과 저장소 |
| `internal/chat` | 플래너, 도구, 기억과 답변 흐름 |
| `internal/connectors/discord` | Discord Gateway와 로컬 simulator |
| `internal/providers` | 모델·날씨·금융 등 외부 공급자 adapter |
| `internal/secrets` | `env:`, `file:`, `keychain:` 참조 처리. `keychain:`은 macOS Keychain, Windows 자격 증명 관리자(`keychain_windows.go`) |
| `internal/fsutil` | 소유자 전용 폴더·파일(Unix 권한, Windows 보호된 ACL), 원자적·내구성 있는 쓰기와 이름 바꾸기 |
| `internal/clock` | 시계와 내장 IANA 시간대 데이터(`time/tzdata`). 두 프로그램이 모두 링크함 |
| `internal/storage` | SQLite 저장소와 백업 (TiDB 코드는 `TiDBEnabled=false`로 비활성) |
| `internal/features` | 개인정보 동의 범위, 운세 천문 계산, 공지 게시판 카탈로그·수집·채점 |
| `internal/lang/packs`, `locales/` | 답변 언어팩과 화면 번역 |
| `assets/brand` | 제품과 배포에 사용하는 최종 브랜드 에셋 |
| `fixtures/` | 합성 테스트 fixture |
| `tools/` | 오프라인 데이터 준비·이행·평가 도구 |
| `docs/` | 현재 제품·운영·개발 공식 문서(먼저 [MAINTAINING.md](MAINTAINING.md)). `docs/archive/`는 Git에서 제외한 로컬 보관소 |

## 설치·실행 스크립트(저장소 루트)

| 파일 | 역할 |
|---|---|
| `install.sh` | macOS·Linux·FreeBSD 설치(소스 빌드, 또는 릴리스 내려받기). `--uninstall` |
| `install.ps1` | Windows 설치(소스 빌드, 릴리스 ZIP, 또는 체크섬을 확인한 내려받기). 사용자 PATH 형식을 보존해 경로를 넣고 `-Uninstall` 때 뺌 |
| `start-test.command` · `start-test.ps1` | 격리된 빠른 테스트 인스턴스(macOS·Linux · Windows) |
| `start-sandbox.command` | RAG 테스트 샌드박스 |
| `.github/workflows/` | `ci`(macOS·Linux·Windows; Windows는 `noweb` 빌드와 설치 스크립트 검사 포함), `release`(아카이브, Linux·Windows 스모크), `soak`, `wiki` |

## 관리 콘솔

- 소스: `internal/transport/web/ui/src/`
- 문구 원본: `internal/transport/web/ui/scripts/strings.py`
- 공개 정적 파일: `internal/transport/web/ui/public/`
- 커밋하는 빌드 결과: `internal/transport/web/ui/dist/`
- 페이지 목록: `internal/transport/web/ui/src/routes.ts`
- 내비게이션: `internal/transport/web/ui/src/lib/nav.ts`
- 공통 부품: `components/LoadState.svelte`(불러오기·오류·다시 시도), `components/AltCommand.svelte`(셸마다 다른 명령 표시), `lib/platform.ts`(시스템별 문구 `tp()`), `lib/scrollable.ts`(스크롤 영역 키보드 접근), `lib/title.svelte.ts`(탭 제목)
- 빌드·테스트 스크립트: `scripts/run-python.mjs`(`python3`·`python`·`py -3` 탐색), `scripts/postbuild.mjs`, `scripts/*.test.mjs`(`npm run test:i18n`)

자료와 색인은 `/blocks`의 기억 블록 흐름으로 통합했습니다. `/sources`와 `/index`는 이전 북마크를 위한 redirect입니다.

## 기억과 검색

| 패키지 | 역할 |
|---|---|
| `memory/corpus` | raw corpus, 카카오 CSV와 Discord 메시지 읽기 |
| `memory/leaves` | 메시지 수·시간 공백 기반 leaf 분할 |
| `memory/lexical` | 언어별 토큰화와 SQLite FTS5 |
| `memory/search` | 후보 결합, 범위 검사, 재순위와 원문 확장 |
| `memory/store` | 원문, leaf, 벡터, 요약, 턴과 비용 장부 |
| `memory/engine` | 가져오기, 색인, 검색과 Discord 증분 처리 |
| `memory/retrieval` | auto 검색 결정과 confidence 계약 |

검색 동작을 바꾸면 Python 샌드박스와 Go 제품을 함께 갱신하고 parity 테스트로 결과를 확인합니다.

## 로컬 전용 경로

다음은 Git에 넣지 않습니다.

- `.private/`, `data/`, `test-sandbox/data/`: 원문, 키와 로컬 인덱스
- `docs/archive/`: 과거 기획·설계·작업지시서 히스토리 보관소
- `output/`: UI 검증 스크린샷
- 개인 에디터·에이전트 설정 파일
- `assets/brand/source/`: 이미지 생성 원본과 제작 기록

원문 본문, 사용자 ID와 비밀값은 공개 문서·로그·fixture에 넣지 않습니다.
