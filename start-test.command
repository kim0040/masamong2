#!/bin/bash
# 빠른 테스트용 원터치 실행기. Finder에서 더블클릭하거나 터미널에서 ./start-test.command
#
#   지금 소스로 빌드 → 테스트 전용 데이터 폴더로 서비스 시작 → 로그인된 관리 콘솔 열기 → 로그 표시
#   이 창에서 Ctrl+C를 누르거나 창을 닫으면 테스트 서비스가 멈춥니다.
#
#   ./start-test.command            평소 실행 (이전 테스트 데이터·키 유지)
#   ./start-test.command --reset    테스트 데이터를 지우고 처음 설정부터
#   ./start-test.command --check    시작 전에 go vet·go test를 먼저 돌림
#   ./start-test.command --stop     떠 있는 테스트 서비스만 멈춤
#
# 실제 인스턴스(~/Library/Application Support/Masamong)와 데이터·키·포트를 공유하지 않습니다:
#   데이터   $MASAMONG_TRY_ROOT (기본 ~/.masamong-try)
#   키       키체인 대신 데이터 폴더의 secrets/*.key (0600) — 실제 인스턴스 키를 덮어쓰지 않음
#   포트     8787이 쓰이고 있으면 빈 포트를 자동으로 고름
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE" || exit 1
xattr -d com.apple.quarantine "$0" 2>/dev/null || true

ROOT="${MASAMONG_TRY_ROOT:-$HOME/.masamong-try}"
BIN="$HERE/build/try"
CLI="$BIN/masamong"

RESET=0 CHECK=0 STOP_ONLY=0
for a in "$@"; do
  case "$a" in
    --reset) RESET=1 ;;
    --check) CHECK=1 ;;
    --stop) STOP_ONLY=1 ;;
    -h|--help) sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "모르는 옵션: $a (--reset, --check, --stop)"; exit 2 ;;
  esac
done

export MASAMONG_DATA_ROOT="$ROOT"
export MASAMONG_SECRET_STORE=file

pause_exit() {
  # Finder에서 연 창이 바로 닫혀 메시지를 못 읽는 일이 없게
  [ -t 0 ] && { echo; read -r -p "Enter를 누르면 닫습니다… " _; }
  exit "$1"
}

stop_service() { [ -x "$CLI" ] && "$CLI" --data-root "$ROOT" stop >/dev/null 2>&1; }

if [ "$STOP_ONLY" = 1 ]; then
  stop_service && echo "테스트 서비스를 멈췄습니다." || echo "떠 있는 테스트 서비스가 없습니다."
  exit 0
fi

if ! command -v go >/dev/null 2>&1; then
  echo "Go가 필요합니다 (1.27 이상). https://go.dev/dl 에서 설치한 뒤 다시 실행하세요."
  pause_exit 1
fi

echo "▸ 빌드 중… ($(git rev-parse --short HEAD 2>/dev/null || echo '?')$(git diff --quiet 2>/dev/null || echo ', 커밋 안 된 변경 포함'))"
mkdir -p "$BIN"
if ! go build -o "$CLI" ./cmd/masamong || ! go build -o "$BIN/masamongd" ./cmd/masamongd; then
  echo "빌드에 실패했습니다. 위 오류를 확인하세요."
  pause_exit 1
fi

if [ "$CHECK" = 1 ]; then
  echo "▸ 검사 중 (go vet, go test)…"
  if ! go vet ./... || ! MASAMONG_OFFLINE=1 go test ./...; then
    echo "검사가 실패해서 시작하지 않았습니다."
    pause_exit 1
  fi
fi

# 새로 빌드한 프로그램으로 띄우려고 이전 테스트 서비스는 먼저 멈춘다
stop_service

if [ "$RESET" = 1 ] && [ -d "$ROOT" ]; then
  case "$ROOT" in
    "$HOME/Library/Application Support/Masamong"|"$HOME"|"/"|"")
      echo "안전을 위해 $ROOT 는 지우지 않습니다."; pause_exit 1 ;;
  esac
  echo "▸ 테스트 데이터를 지웁니다: $ROOT"
  rm -rf "$ROOT"
fi
mkdir -p "$ROOT"

echo "▸ 테스트 서비스를 시작하고 콘솔을 엽니다…"
if ! "$CLI" --data-root "$ROOT" open --no-tray; then
  echo "시작하지 못했습니다. 로그: $ROOT/logs/daemon.log"
  [ -f "$ROOT/logs/daemon.log" ] && tail -n 30 "$ROOT/logs/daemon.log"
  pause_exit 1
fi

cleanup() {
  trap - INT TERM HUP EXIT
  echo
  echo "▸ 테스트 서비스를 멈춥니다…"
  stop_service
  [ -n "${TAIL_PID:-}" ] && kill "$TAIL_PID" 2>/dev/null
  exit 0
}
trap cleanup INT TERM HUP EXIT

cat <<EOF

────────────────────────────────────────────────────────────
 테스트 서비스가 떠 있습니다.  데이터: $ROOT
 · 브라우저가 안 열렸으면: $CLI --data-root "$ROOT" open
 · 처음이면 콘솔의 첫 설정 → 연결에서 OpenRouter 키·Discord 토큰을 넣으세요.
   (실제 봇과 같은 Discord 토큰을 쓰면 두 봇이 함께 답합니다. 테스트용 봇을 따로 쓰세요.)
 · 키가 하나도 없어도 콘솔의 '연습 모드 켜기'로 오프라인 모델·예시 자료를 써 볼 수 있습니다.
 · 멈추기: 이 창에서 Ctrl+C, 창 닫기, 또는 콘솔의 유지보수 → 서비스 종료
────────────────────────────────────────────────────────────

EOF

LOG="$ROOT/logs/daemon.log"
for _ in 1 2 3 4 5 6 7 8 9 10; do [ -f "$LOG" ] && break; sleep 0.3; done
tail -n 20 -F "$LOG" 2>/dev/null &
TAIL_PID=$!

# 콘솔의 '서비스 종료'·트레이·다른 터미널의 masamong stop으로 멈춰도 이 창이 끝나도록 서비스 프로세스를 지켜본다.
# 서비스는 감시 프로세스(masamongd --supervise) 아래에서 돌므로 그 PID를 본다. 재시작과 비정상 종료 뒤
# 자동 재시작은 감시 프로세스가 그대로 살아 있어서 이어서 보고, 감시 프로세스가 끝나면 서비스도 끝난 것이다.
daemon_pid() {
  "$CLI" --data-root "$ROOT" status --output json 2>/dev/null |
    sed -n -e 's/.*"supervisor_pid":\([0-9][0-9]*\).*/\1/p' -e 't' -e 's/.*"pid":\([0-9][0-9]*\).*/\1/p' | head -n 1
}
PID="$(daemon_pid)"
while :; do
  sleep 1
  if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then continue; fi
  sleep 2 # 다른 곳에서 새로 띄웠으면 그 프로세스를 따라간다
  PID="$(daemon_pid)"
  [ -z "$PID" ] && break
done
trap - INT TERM HUP EXIT
kill "$TAIL_PID" 2>/dev/null
echo
echo "▸ 테스트 서비스가 종료되었습니다. 다시 띄우려면 ./start-test.command"
