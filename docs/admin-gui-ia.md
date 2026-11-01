# 관리 콘솔

관리 콘솔은 `masamongd`에 포함된 Svelte 앱입니다. 일상 관리는 GUI에서 하고 CLI는 자동화와 비상 복구에 사용합니다. 기술 결정은 [ADR-16](adr/ADR-16-gui-default-svelte-console.md)에 있습니다.

## 접속

- 이 컴퓨터: `masamong open`
- 원격 서버: `masamong connect user@host`
- 비상 화면: `http://127.0.0.1:<port>/lite` (5개 콘솔 언어, 로그인은 토큰을 본문으로 보내는 POST 양식)

데몬은 loopback에만 bind합니다. `open`과 `connect`는 1회용 로그인 링크를 만들며 별도 계정, 비밀번호, 초대와 역할은 없습니다.

## 메뉴

왼쪽 메뉴는 14개 항목입니다. 같이 쓰는 화면은 항목 하나에 묶고, 묶인 화면 위에 탭(`SectionTabs`)을 보입니다. 각 화면은 자기 주소를 그대로 가집니다(`lib/nav.ts`의 `sections`).

| 묶음 | 항목 |
|---|---|
| 운영 | 홈, 대화 기록, 작업 |
| 봇 | 서버·방, 도구, 말투·언어, 사람·권한 |
| 기억 | 기억 블록, 기억 탐색, 답변 실험실 |
| 분석 | 통계·비용(통계 `/stats` · 비용·한도 `/usage`) |
| 시스템 | 연결, 로그·진단, 유지보수(서비스·백업 `/maintenance` · 설정 파일·기록 `/settings`) |

화면 및 접속(`/display`)은 메뉴 아래쪽과 상단 막대에 있습니다. 명령 팔레트는 메뉴 밖 화면도 찾습니다.

한 설정은 한 화면이 주인입니다. 설정 파일·기록의 양식은 주인 화면이 있는 항목(Discord·공급자·모델 → 연결, 예산·한도 → 비용·한도, 기억 → 기억 블록, 백업 → 유지보수, 도구·웹 검색 → 도구, 서버·방, 말투, 접근 권한·운영자 알림 → 사람·권한)에서 그 화면으로 가는 버튼을 먼저 보이고, 값은 "여기서 직접 편집"을 눌러야 펼칩니다. 운영자 알림은 운영자 목록 옆(사람·권한)에, 원격 접속 명령은 화면 및 접속에 있습니다.

## 페이지

| 경로 | 기능 |
|---|---|
| `/` | 상태, 비용, 알림, 시작 체크리스트와 최근 활동 |
| `/turns` | 대화 목록과 의도 파악·도구·기억·답변 상세 |
| `/jobs` | 작업 상태, 진행률, 남은 시간, 취소와 재시도 |
| `/rooms` | Discord 서버·방 목록. 방 하나는 채널 작업 공간: 응답·말투·규칙과 저장 전 시험(저장된 설정과 비교), 기억(블록·요약·놓친 메시지 채우기·사건 연표), 도구, 기록, 권한, 다른 방에 설정 복사 |
| `/tools` | 내장·HTTP 도구의 설정, 한도, 시험과 통계. 준비 상태는 서버(`tools.list`의 `ready`·`ready_reason`)가 판단 |
| `/persona` | 말투와 답변 언어(화면·시스템 언어 자동, 관리자별 수동 선택 기억), 기본 제공 말투 숨김·되돌리기, 편집 중 저장 전 시험 |
| `/people` | 사용자별 동의·구독·개인 설정, 철회·삭제, 운영자·차단 목록, DM 대화(허용 대상·하루 한도·오늘 사용량), 운영자 알림 |
| `/blocks` | 기억 블록 가져오기, 준비, 상세와 채널별 연결, 카카오톡 블록의 방 기억 만들기(예상 비용 확인 후 승인), 방 기억 월 한도·하루 경계 설정 |
| `/lab` | 검색·답변 비교와 평가 |
| `/memory` | 기억 검색, 방 기억(방 상태·에피소드, 근거 원문 조회, 에피소드 지우기), 사건 연표, 근거 검토, 정정·제외와 인물 병합 |
| `/stats` | 사용량·지연·비용 통계와 보고서 |
| `/usage` | 예산과 도구·외부 호출 한도, 한도 프리셋(여유·보통·엄격) |
| `/connections` | 모델·단가·키 상태, Discord와 외부 공급자 |
| `/settings` | 전체 설정, YAML, 변경 기록과 스냅샷 |
| `/logs` | 실시간 로그, 진단과 가동 이력 |
| `/maintenance` | 백업·복원, SQLite 상태, 이전 봇 개인 설정 가져오기, 재시작과 자동 시작, 새 버전 확인 |
| `/display` | 테마, 언어, 밀도, 동작, 접속과 튜토리얼 |

`/sources`와 `/index`는 이전 북마크를 위해 `/blocks`로 이동합니다. `/admins`는 `/display`로 이동합니다.

## 공통 동작

- 라이트·다크·시스템 테마
- 촘촘하게·보통·크게의 세 단계 밀도
- 한국어·영어·일본어·중국어(간체)·스페인어와 `locales/ui/`에서 발견한 추가 언어
- 동작 줄이기와 `prefers-reduced-motion`
- 모바일 햄버거 메뉴, 태블릿 아이콘 사이드바, 데스크톱 전체 사이드바. 좁은 화면에서 메뉴는 모달 대화 상자입니다: 닫혀 있으면 `inert`, 열리면 뒤 화면을 `inert`로 두고 포커스를 현재 항목으로 옮기며 Tab을 메뉴 안에 가두고, `Esc`·배경 클릭으로 닫으면 메뉴 버튼으로 돌아갑니다(`aria-expanded`·`aria-controls`)
- 불러오는 곳은 하나의 패턴을 씁니다. `components/LoadState.svelte`가 스켈레톤 → 3초 뒤 "아직 불러오는 중" → 이유와 **다시 시도**가 있는 오류 카드를 보이고, `lib/resource.svelte.ts`는 데이터가 보이는 중에 새로고침이 실패한 자원을 등록해 `Banners.svelte`가 띠 하나("일부 데이터를 새로 불러오지 못했습니다")로 알립니다. `postJSON`은 던지지 않고 상태 0 `OFFLINE`·`UNKNOWN`으로 돌려주며, 호출하는 쪽은 `finally`에서 `busy`를 풀고 입력을 지킵니다
- 시스템별 문구는 `lib/platform.ts`의 `tp()`가 `키.windows` / `키.darwin`을 골라 씁니다(트레이 아이콘·메뉴 막대, 키가 저장되는 곳, 소스 빌드 업데이트 안내). 세션 `platform`과 Windows의 `signin.open_cmd`·`signin.token_cmd`(PowerShell과 cmd 형태)에서 옵니다
- 페이지 제목은 `lib/title.svelte.ts`의 `useTitle`이 `<화면> · Masamong`으로 맞추고, 화면이 사라지면 되돌립니다
- 언어마다 글꼴 묶음이 따로 있습니다(`global.css`의 `:root:lang(ja)`·`:lang(zh)`·번체). 한국어 글꼴은 한국어(와 영어·스페인어) 묶음에만 둡니다
- 대화 상자: 글자 입력칸에서 Enter는 주 버튼을 실행하고(IME 조합 중 제외), 입력한 내용이 있으면 배경 클릭으로 닫히지 않으며, 저장 안 한 변경 확인은 콘솔 자체 `Confirm`입니다. 설정은 초안을 시작한 리비전(`patchConfig({ base })`)으로 저장해, 그사이 바뀌면 `CONFIG_CONFLICT`와 함께 최신 설정 위에 내 수정을 얹어 다시 맞춥니다
- 명령 팔레트, 알림, 전체 일시 중지와 월 비용 표시
- SSE 상태 갱신, 저장 막대, 확인 대화상자와 토스트
- 연습 모드. 첫 방문 안내는 홈에서만 저절로 뜨고, 다른 화면은 제목 옆 ? 버튼으로 봅니다
- 브랜드 그림 주소는 빌드할 때 파일 내용의 해시를 붙입니다(`virtual:brand`). 그림을 바꾸면 주소가 바뀌므로 서버는 `?v=`가 붙은 그림을 오래 캐시합니다

## 기억 블록 흐름

CSV 가져오기는 미리보기 → 이름·메모 → 예상 시간·비용·예산 → 백그라운드 준비 → 채널 연결 순서입니다. 진행 중에는 처리량, 진행률과 남은 시간을 표시합니다. 취소된 작업은 블록 상세와 작업 화면에서 재시도할 수 있습니다.

## 키와 비용

키 입력은 `/api/secret`으로만 받습니다. 시스템 저장소를 우선합니다(macOS Keychain, Windows 자격 증명 관리자). 없으면(Linux·FreeBSD) 데이터 폴더의 `secrets/` 파일로 저장하며, 파일은 POSIX에서 `0600`, Windows에서 소유자 전용 ACL이지만 **암호화되지는 않습니다**. `MASAMONG_SECRET_STORE=file`이면 어디서나 파일입니다. 설정에는 참조만 남기고, 화면의 문구도 이 사실 그대로입니다("안전하게 암호화" 같은 말을 쓰지 않음). `keychain:` 참조 입력은 서버가 지원한다고 알릴 때만 받습니다. 키가 없거나 거절되었거나 공급자가 연결되지 않은 경우 `POST /api/action`은 424를 돌려줍니다(`statusFor`: 429 속도 제한, 500 `IO`, 404 `JOB_NOT_FOUND`, 409 `LEASE_MISMATCH`·멱등성 충돌, 422 `BACKUP_VERIFY_FAILED`). 502는 공급자가 실제로 답하지 못했을 때만 씁니다. `GET /api/session`은 `platform`(Windows에서는 `signin.open_cmd`·`signin.token_cmd`도)을, `channels.list`는 `discord_token`(저장된 봇 토큰이 있는지)을 돌려주며, Discord 단계는 이 값으로 토큰 없이 완료되지 않게 합니다. 연결 화면은 키 존재 여부, 모델 단가와 키 없이 쓸 수 있는 공급자를 구분합니다.

공급자 응답에 비용이 없으면 토큰 수와 단가로 계산합니다. 블록 준비 전에 예상 비용과 이전 처리 속도에 기반한 예상 시간을 보여 줍니다. 일일 호출량은 DB에 저장되어 재시작과 설정 변경 뒤에도 유지됩니다.

## 개발과 검증

첫 로드 예산은 gzip 150 KiB이고(`scripts/postbuild.mjs`가 넘으면 빌드를 실패시킴) 지금은 60.9 KiB입니다. `/api/i18n`은 언어별로 한 번 만들어 gzip으로 압축하고 `ETag`와 `Cache-Control: no-cache`를 달아 두 번째부터는 `304`로 답합니다.

- 페이지 등록: `internal/transport/web/ui/src/routes.ts`
- 사이드바: `internal/transport/web/ui/src/lib/nav.ts`
- 문구 원본: `internal/transport/web/ui/scripts/strings.py`
- 빌드 결과: `internal/transport/web/ui/dist/`
- 빌드는 Windows에서도 됩니다(`cd internal\transport\web\ui; npm ci; npm run build`). 문구 생성기는 `scripts/run-python.mjs`가 찾은 `python3`·`python`·`py -3`으로 돌고 LF로 씁니다

```bash
make ui
go test ./internal/transport/web ./internal/application ./internal/jobs
```

GUI 변경은 데스크톱과 320px 모바일, 라이트·다크, 키보드, 동작 줄이기, 오류 복구와 실제 Action 흐름을 브라우저에서 확인합니다.
