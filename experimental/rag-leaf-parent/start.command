#!/bin/bash
# 테스트 샌드박스 원터치 실행기 (제품 관리 콘솔 8787 아님).
# experimental/rag-leaf-parent 또는 test-sandbox 에서 더블클릭.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
export SANDBOX_IN_TERMINAL="${SANDBOX_IN_TERMINAL:-}"

if [[ -f "$HERE/app.py" ]]; then
  CODE="$HERE"
  REPO_ROOT="$(cd "$HERE/../.." && pwd)"
else
  REPO_ROOT="$(cd "$HERE/.." && pwd)"
  CODE="$REPO_ROOT/experimental/rag-leaf-parent"
fi

xattr -d com.apple.quarantine "$0" 2>/dev/null || true

if [[ "${SANDBOX_IN_TERMINAL}" != "1" && "${TERM_PROGRAM:-}" != "Apple_Terminal" ]]; then
  osascript <<EOF
tell application "Terminal"
  activate
  do script "export SANDBOX_IN_TERMINAL=1; exec $(printf '%q' "$HERE/start.command")"
end tell
EOF
  exit 0
fi

cd "$CODE" || {
  echo "오류: 테스트 샌드박스 코드가 없다."
  echo "  $CODE"
  read -r -p "Enter 로 닫기…"
  exit 1
}

echo "========================================"
echo " 테스트 샌드박스  (제품 관리 콘솔 아님)"
echo " http://127.0.0.1:8765"
echo "========================================"

if ! command -v python3 >/dev/null; then
  echo "오류: python3 가 없다."
  read -r -p "Enter 로 닫기…"
  exit 1
fi

if [[ ! -f config.json ]]; then
  cp config.example.json config.json
  echo "config.example.json → config.json 복사."
fi

export PYTHONDONTWRITEBYTECODE=1
export RAW_CORPUS_ROOT="${RAW_CORPUS_ROOT:-$REPO_ROOT/.private/raw-corpus}"
export EVAL_SECRETS="${EVAL_SECRETS:-$REPO_ROOT/.private/eval/secrets.env}"

if [[ ! -f "$EVAL_SECRETS" ]]; then
  echo "경고: secrets.env 가 없다. UI는 켜지지만 채팅·임베딩은 실패할 수 있다."
  echo "  $EVAL_SECRETS"
fi

# 데이터셋 선택: test-sandbox/config.<이름>.json 중 인덱스가 있는 것만 보여 준다.
# SANDBOX_DATASET=<이름> 이면 묻지 않는다(default = config.json). 8초 동안 입력이 없으면 기본.
DATASET_NAMES=()
DATASET_FILES=()
for f in "$REPO_ROOT"/test-sandbox/config.*.json; do
  [[ -f "$f" ]] || continue
  name="$(basename "$f" .json)"; name="${name#config.}"
  if python3 - "$f" <<'PY' 2>/dev/null; then
import json, pathlib, sys
cfg = json.load(open(sys.argv[1]))
raise SystemExit(0 if (pathlib.Path(cfg.get("data_dir") or "/nonexistent") / "meta.json").is_file() else 1)
PY
    DATASET_NAMES+=("$name"); DATASET_FILES+=("$f")
  fi
done
CHOSEN="${SANDBOX_DATASET:-}"
if [[ -z "$CHOSEN" && -z "${MASAMONG_SANDBOX_CONFIG:-}" && ${#DATASET_NAMES[@]} -gt 0 ]]; then
  echo "데이터셋 (Enter·8초 = 0):"
  echo "  0) default  (config.json)"
  for i in "${!DATASET_NAMES[@]}"; do echo "  $((i + 1))) ${DATASET_NAMES[$i]}"; done
  read -r -t 8 -p "> " pick || true
  echo
  if [[ "$pick" =~ ^[0-9]+$ && "$pick" -ge 1 && "$pick" -le ${#DATASET_NAMES[@]} ]]; then
    CHOSEN="${DATASET_NAMES[$((pick - 1))]}"
  fi
fi
if [[ -n "$CHOSEN" && "$CHOSEN" != "default" ]]; then
  if [[ -f "$REPO_ROOT/test-sandbox/config.$CHOSEN.json" ]]; then
    export MASAMONG_SANDBOX_CONFIG="$REPO_ROOT/test-sandbox/config.$CHOSEN.json"
  else
    echo "경고: test-sandbox/config.$CHOSEN.json 이 없다. 기본 config.json 을 쓴다."
  fi
fi
CONFIG_FILE="${MASAMONG_SANDBOX_CONFIG:-config.json}"
echo "설정: $CONFIG_FILE"

PORT="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("port") or 8765)' "$CONFIG_FILE")"
HOST="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("host") or "127.0.0.1")' "$CONFIG_FILE")"
case "$HOST" in
  0.0.0.0|::|"") BROWSE_HOST="127.0.0.1" ;;
  *) BROWSE_HOST="$HOST" ;;
esac
URL="http://${BROWSE_HOST}:${PORT}"

open_ui() {
  open "$URL" 2>/dev/null || true
}

if curl -fsS --max-time 1 "${URL}/api/status" >/dev/null 2>&1; then
  echo "이미 실행 중: ${URL}  (데이터셋을 바꾸려면 그 서버를 Ctrl+C로 끄고 다시 실행)"
  open_ui
  echo "이 창은 닫아도 서버는 유지된다."
  read -r -p "Enter 로 닫기…"
  exit 0
fi

if [[ "${SANDBOX_PREPARE:-}" == "1" ]]; then
  echo "SANDBOX_PREPARE=1 → prepare.py"
  python3 prepare.py || {
    echo "인덱스 준비 실패. 서버는 인덱스를 기다린 채로 켠다."
  }
fi

echo "서버 시작…  ${URL}"
echo "종료: 이 창에서 Ctrl+C"

(
  for _ in $(seq 1 60); do
    if curl -fsS --max-time 1 "${URL}/api/status" >/dev/null 2>&1; then
      open_ui
      exit 0
    fi
    sleep 0.25
  done
) &

exec python3 -u app.py
