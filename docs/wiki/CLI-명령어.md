# CLI 명령어

**한국어** | [English](https://github.com/kim0040/masamong2/wiki/CLI-Commands)

`masamong` 하나로 서비스 관리와 모든 기능을 다룹니다. 콘솔에서 하는 모든 일은 명령줄로도 할 수 있습니다.

터미널에서 `masamong`만 치거나 `masamong help`(`--help`)를 치면 자주 쓰는 명령과 명령 그룹 목록이 나옵니다. 없는 명령을 치면 가장 비슷한 명령을 알려 줍니다(`masamong staus` → `masamong status`). `--output json`이나 파이프에서는 예전처럼 JSON 결과(`help.version`, `ACTION_UNKNOWN`)를 돌려줍니다.

## 공통 옵션

| 옵션 | 뜻 |
|---|---|
| `--data-root DIR` | 사용할 데이터 폴더 (인스턴스 선택) |
| `--output json` | 자동화용 JSON 출력. 파이프·`CI`·`NO_COLOR` 환경에서는 자동으로 JSON |
| `--이름 값` | 명령 입력. `--dry_run true`, `--guild_id 123`처럼 씀 |
| `--input-file FILE` | `masamong tools test` 전용: JSON 객체를 파일에서 읽음. `--input-file -`는 표준 입력에서 읽음 |

모든 명령은 같은 JSON 구조를 돌려줍니다.

```json
{"ok":true,"action":"jobs.list","output":{"jobs":[]},"request_id":"cli"}
{"ok":false,"action":"polls.vote","error":{"code":"INVALID_INPUT","message_key":"generic.error","message":"poll not found","retryable":false},"request_id":"cli"}
```

### JSON 입력과 PowerShell

`--input-file`은 `masamong tools test`에서만 쓸 수 있으며, 다른 명령에서는 거절합니다. JSON 객체 하나를 파일이나 표준 입력에서 읽습니다. Windows PowerShell은 인라인 JSON에서 따옴표를 벗겨 버릴 수 있어서, 이 방법이 가장 쉽습니다(잘못된 인라인 값에는 바로 이 안내가 돌아옵니다).

```powershell
# 파일: BOM이 있거나 없어도 되는 UTF-8
[IO.File]::WriteAllText("$PWD\query.json", '{"region":"Seoul"}')
masamong tools test --id weather.query --input-file query.json
```

```bash
masamong tools test --id weather.query --input '{"region":"Seoul"}'      # bash·zsh는 작은따옴표면 충분
echo '{"region":"Seoul"}' | masamong tools test --id weather.query --input-file -
```

파일이나 입력에는 JSON 객체 하나가 들어 있어야 합니다. `--input-file`은 BOM 유무와 관계없이 UTF-8을 읽고, BOM이 있는 UTF-16 리틀엔디안·빅엔디안도 읽습니다. Windows PowerShell 5.1이 쓰는 파일도 별도 인코딩 작업 없이 사용할 수 있습니다.

## 서비스 관리

| 명령 | 하는 일 |
|---|---|
| `masamong open` | 데몬을 시작(또는 재사용)하고 브라우저로 콘솔 열기 |
| `masamong start` | 데몬만 시작 (서버용) |
| `masamong status` | 실행 상태 (서비스 PID와 감시 프로세스 PID `supervisor_pid`). 유지보수 모드의 JSON에는 `maintenance:true`와 한 줄 `message`가 들어가고, 일반 출력도 같은 문구를 보여 줌 |
| `masamong restart` | 재시작 |
| `masamong stop` | 중지 (진행 중인 답변·작업을 마무리하고 프로세스가 끝날 때까지 기다림, 최대 40초). Windows에서는 이 데이터 폴더의 트레이 아이콘도 닫으며, 트레이를 닫지 못해도 경고를 남기고 서비스는 중지함 |
| `masamong connect user@host` | 원격 서버의 콘솔을 SSH 터널로 열기 |
| `masamong tray` | 메뉴 막대·알림 영역 아이콘 |
| `masamong autostart on` / `off` | 로그인·부팅 시 자동 시작 |
| `masamong doctor` | 진단 (키, 공급자, Discord, 권한, 저장소, 시간대). Discord 시작 중에는 READY/RESUMED 전까지 `pending / gateway_connecting`을 표시합니다. 첫 연결을 재시도 중이면 시도 횟수·마지막 오류·다음 시각을 보여 주고, 거절된 토큰 같은 영구 오류는 재시도하지 않습니다. 데이터 폴더 문제는 `data_folder_warnings`와 `data_folder` 확인 필요 항목에 나옵니다. 복원·초기화·업데이트가 끝나지 못한 상태에서 오프라인으로 실행하면 아래의 복구 필드를 보여 줍니다 |
| `masamong admin token` | 콘솔 로그인 토큰 출력 (데이터 폴더의 `admin/web-token` 파일) |
| `masamong version` | 버전 |
| `masamong runtime pause` | 모든 채널 답변 일시 중지 (`--paused false`로 재개, `--scope guild/channel`로 일부만) |
| `masamong runtime status` | 실행 상태. 유지보수 모드의 JSON에는 `maintenance:true`와 한 줄 `message`가 들어가고, 일반 출력도 같은 문구를 보여 줌 |
| `masamong runtime logs` | 최근 로그 |

## 설정

| 명령 | 하는 일 |
|---|---|
| `config show` · `config document` | 현재 설정 |
| `config diff --file F` · `config validate --file F` | 바뀔 내용 확인·검증 |
| `config apply --file F --base_revision N` | 적용 (리비전 기록). `N`은 고치기 시작한 시점의 리비전입니다. `config show`가 `revision`, `config diff`가 `base_revision`을 출력합니다([설정 레퍼런스](https://github.com/kim0040/masamong2/wiki/설정-레퍼런스)) |
| `config rollback --revision N` | 되돌리기 |
| `config changes` | 변경 기록 |
| `config snapshot save/restore/delete`, `config snapshots` | 설정 스냅샷 |

## 채널

| 명령 | 하는 일 |
|---|---|
| `channels list` | 서버·채널과 정책 |
| `channels configure --guild_id G --id C --respond mention ...` | 채널 정책 |
| `channels view show/attach/detach --guild_id G --channel_id C --block_id B` | 채널에 기억 블록 연결 |
| `channels permissions --guild_id G --channel_id C` | 봇 권한 확인 |
| `channels invite` | 초대 링크 |
| `channels ops-log` | 채널 설정 변경 기록 |

## 대화와 기억

| 명령 | 하는 일 |
|---|---|
| `chat --guild_id G --channel_id C --message "..."` | 답변 미리보기 (저장하지 않음) |
| `imports preview` · `imports start` | 대화 가져오기 (CSV·원문 아카이브, 파일당 최대 1 GiB. [기억 블록](https://github.com/kim0040/masamong2/wiki/기억-블록#크기-제한과-중단) 참고) |
| `memory blocks list/show/prepare/update/delete` | 기억 블록 |
| `memory reindex [--dry_run true] [--budget_usd 0.5]` | 색인 |
| `memory search --guild_id G --channel_id C --query "..."` | 기억 검색 (근거·점수) |
| `memory backfill [--guild_id G --channel_id C]` · `memory backfill --status true` | 꺼져 있던 동안의 Discord 메시지 채우기 (최근 `memory.backfill_days`일) |
| `summaries --guild_id G --channel_id C [--refresh true] [--saved true]` | 채널 요약 (새 메시지만 이어서 반영, `--saved`는 모델 호출 없이 마지막 요약만) |
| `events --guild_id G --channel_id C [--query "..."]` | 사건 연표 (날짜별, 질문하면 관련된 것만) |
| `messages search --rooms R --query "..."` | 원문 검색 (무료) |
| `memory review` · `memory reviews` | 근거 정정·제외 |
| `people list` · `people merge` | 인물 이름 병합 |
| `turns list` · `turns get --id T` | 대화 기록 |
| `trace show --id T` | 턴 진단 기록 |
| `eval run` · `eval runs` | 검색 품질 평가 |

## 도구

| 명령 | 하는 일 |
|---|---|
| `tools list` | 도구 상태·오늘 사용량 |
| `tools configure --id ID ...` | 켜기/끄기, 한도, 별칭, 기본값 |
| `tools test --id ID [--input JSON \| --input-file F]` | 시험 실행 |
| `weather`, `disasters`, `finance quote`, `finance fx`, `search` | 도구 직접 실행 |
| `places search --query "..." [--size 5]` | 장소 찾기 (카카오맵. 카카오 키가 필요하며 없으면 설정되지 않았다고 답함) |
| `activity ranking --scope discord:<채널 ID> [--period today\|week\|month\|all]` | 채널의 수다왕 (메시지 수만 셈. `--scope`가 채널, 기본 기간은 `week`) |
| `read --url https://... [--max_chars 2800]` | 공개 웹 페이지 하나를 글로 읽기 (링크 읽기. **도구**에서 켜기 전까지 꺼져 있고, 주소를 Jina Reader로 보냄) |
| `polls create/vote/close/result` | 투표 |
| `images --prompt "..."` | 이미지 생성 (연결의 `image` 모델 역할) |
| `fortune` · `transfer-notices` · `notices status` | 운세·편입 공지·학교 공지 조회 |
| `notices subscribe --op collect` | 공지 게시판 지금 수집 (`--op list`: 구독자, `--op pause/resume/delete --user_id U --kind school`) |

## 사람과 동의

| 명령 | 하는 일 |
|---|---|
| `users` | 사용자별 동의·구독·개인 설정·차단 (생년월일·공지 프로필은 보이지 않음) |
| `privacy` · `privacy --user_id U --scope memory --decision withdraw` | 동의 기록 보기·철회 (동의는 본인만 Discord에서) |
| `preferences --user_id U [--timezone Asia/Seoul] [--reset true]` | 개인 설정 |
| `fortune subscribe --op unsubscribe/delete --user_id U` | 운세 구독 끄기·운세 정보 삭제 |
| `access [--operators '["ID"]'] [--blocked_users '["ID"]'] [--guild_admins false]` | 운영자·차단 목록, 서버 관리자 관리 명령 |
| `reminders --op list [--guild_id G] [--channel_id C] [--user_id U] [--status S]` · `reminders --op cancel --id R` · `reminders --op pause\|resume --id R` | 알림 목록·취소, 반복 알림 일시 정지·재개 (예약·변경은 본인이 Discord에서) |
| `participation --guild_id G --channel_id C [--mode off\|shadow\|conservative]` | 먼저 말 걸기 방식·판단 기록 (`--review ID --verdict good\|bad`로 검토) |

## 공급자·모델·비용

| 명령 | 하는 일 |
|---|---|
| `providers list` · `providers test` | 공급자 상태·연결 시험 |
| `models list` · `models select` · `models embed-width` | 모델, 임베딩 폭(같은 모델에서 1024·2048·0=전체 폭) |
| `usage` | 월 예산·사용량 |
| `stats query` · `stats series` · `dashboard` | 통계 |

## 운영

| 명령 | 하는 일 |
|---|---|
| `jobs list/get/watch/cancel/retry` | 배경 작업 |
| `backup create/list/verify/restore/delete` | 백업 |
| `schedules list` · `schedules run` | 예약 작업 |
| `storage status` | 저장소 (SQLite. `storage migrate`는 TiDB를 끄면서 비활성) |
| `migration apply --path V1.db [--confirm true]` | 이전 봇 개인 설정 가져오기 (계획 → 가져오기) |
| `audit` | 변경 감사 기록 |
| `alerts status` · `alerts test` | 운영자 알림(Discord DM) 상태와 테스트 발송 |
| `serverlog test --guild_id G` | 서버 로그 채널에 시험 글 올리기(서버마다 `log`는 `channels list`에 표시) |
| `updates status` · `updates check` | 새 버전 확인 결과와 지금 확인 |
| `runtime history` | 가동 이력 (시작·종료·재시작·비정상 종료와 그 이유) |
| `locales` | 언어 목록 |

## 복구 명령 (`masamongd`)

`masamongd`를 직접 실행할 일은 보통 없습니다. 복원·초기화·프로그램 업데이트가 끝나지 못하면 데몬은 DB를 열지 않은 채 봇 대신 현지화된 복구 안내 화면을 띄웁니다([문제 해결](https://github.com/kim0040/masamong2/wiki/문제-해결#콘솔에-복구-필요가-뜰-때-유지보수-모드)). `masamong open`과 트레이의 **열기**로 이 화면을 볼 수 있고, 화면에는 지금 가능한 복구 명령만 나옵니다. `status`와 `doctor`는 읽기만 합니다. `retry`·`rollback`·`cancel`·`abandon-update`는 데이터 폴더를 혼자 써야 하므로 먼저 `masamong stop`으로 유지보수 데몬을 멈추세요.

| 명령 | 하는 일 |
|---|---|
| `masamongd --version` | 버전을 JSON으로 출력. 데이터 폴더를 열지 않음 |
| `masamongd --data-root DIR --maintenance status` (또는 `doctor`) | DB를 열지 않고 걸려 있는 작업을 보여 줌: `maintenance`, `case`, `operation`(`restore`·`reset`·`update`), `phase`, `completed_targets`, 현재 상태에서 쓸 수 있는 `recovery_commands`, `recovery_requires_stopped_daemon`, `bot_running`·`schedulers_running`(둘 다 false) |
| `masamongd --data-root DIR --maintenance retry` | 끊긴 작업을 멈춘 자리에서 이어서 진행 |
| `masamongd --data-root DIR --maintenance rollback` | 이전 상태로 되돌림: 복원이면 DB·WAL·설정, 프로그램 업데이트면 이전 프로그램 한 쌍 |
| `masamongd --data-root DIR --maintenance cancel` | 실제 파일을 건드리지 않은 작업을 취소. 데이터가 이미 옮겨졌다는 복구 파일이 없을 때만 읽을 수 없거나 옛 형식인 표식을 `maintenance-refused-*.json`으로 옮김 |
| `masamongd --data-root DIR --maintenance abandon-update [--yes]` | 재설치 뒤 자동 복구할 수 없는 프로그램 업데이트 저널에 씀. 프로그램 파일은 그대로 두고 저널만 `.masamong-update.abandoned-<시각>.json`으로 옮김. `--yes`가 없으면 `abandon`을 입력해야 하며, 옛 파일과 스테이징 파일은 이 기록을 지울 때까지 남음 |

**초기화**는 지우기를 시작한 뒤에는 되돌리거나 취소할 수 없습니다. 화면에 나온 `retry`로 끝까지 진행하세요. 초기화 뒤 데이터를 되살리는 방법은 직전에 만든 백업(종류 *초기화 전*)입니다. 복원이 실제 파일을 옮기기 전에 실패하면 스테이징 사본을 치우고 실패를 기록한 뒤 현재 데이터로 봇을 계속 실행합니다. 커밋된 복원은 잠긴 옛 파일을 아직 지우지 못해도 성공이며, 시작 때와 매시간 정리를 다시 시도하고 `doctor`에 남은 파일을 표시합니다.

```powershell
# Windows: 프로그램은 %LOCALAPPDATA%\Programs\Masamong에 있고 사용자 PATH에 들어 있습니다
masamong stop
masamongd --data-root "$env:APPDATA\Masamong" --maintenance status
masamongd --data-root "$env:APPDATA\Masamong" --maintenance retry
masamong start
```

## 자동화 예시

```bash
# 매일 사용량을 JSON으로 저장
masamong usage --output json > usage-$(date +%F).json

# 점검 전후로 답변 멈추기/재개
masamong runtime pause
masamong runtime pause --paused false

# 실행 중인지 확인 (스크립트)
masamong status --output json | grep -q '"running":true' || masamong start
```

```powershell
# PowerShell에서는
masamong usage --output json | Set-Content -Encoding utf8 "usage-$(Get-Date -Format yyyy-MM-dd).json"

masamong runtime pause
masamong runtime pause --paused false

if (-not (masamong status --output json | ConvertFrom-Json).output.running) { masamong start }
```
