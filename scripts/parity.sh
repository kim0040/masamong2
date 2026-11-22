#!/bin/sh
# Python sandbox against the Go port on a made-up corpus (make parity). The synthetic sandbox is built, the sandbox
# dumps what it retrieves (the planner-intent cases included: time ranges, people, the recent window) and what its
# full-text index returns, and the Go parity tests must agree on the leaves, the full-text ranking and the retrieval.
# No stored chat, no keys, no network. Needs python3 with numpy and go.
#   PYTHON=python3.12 scripts/parity.sh      KEEP=1 keeps the working folder (it prints where)
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
python=${PYTHON:-python3}
dir=$(mktemp -d)
if [ -z "${KEEP:-}" ]; then trap 'rm -rf "$dir"' EXIT; else echo "parity files: $dir"; fi
cd "$root"
# the sandbox reads a secrets file when it loads; the dump replaces every client with a fake, so this value is never used
echo 'OPENROUTER_API_KEY=parity-synthetic-not-a-key' >"$dir/secrets.env"
"$python" experimental/rag-leaf-parent/parity_synth.py "$dir/syn"
EVAL_SECRETS="$dir/secrets.env" MASAMONG_SANDBOX_DATA="$dir/syn/data" MASAMONG_SANDBOX_CONFIG="$dir/syn/config.json" \
	PARITY_INTENTS="$dir/syn/intents.json" PARITY_N=40 \
	"$python" experimental/rag-leaf-parent/parity_dump.py "$dir/dump" synth
MASAMONG_SANDBOX_DATA="$dir/syn/data" MASAMONG_SANDBOX_CONFIG="$dir/syn/config.json" \
	MASAMONG_PARITY_RETRIEVE="$dir/dump/retrieve_synth.json" MASAMONG_PARITY_DIR="$dir/dump" \
	MASAMONG_SANDBOX_INDEXES="$dir/syn/data=$dir/syn/config.json" \
	go test -count=1 -v -run '^(TestSandboxRetrieveParity|TestSandboxLeafParity|TestSandboxFTS5Parity)$' \
	./internal/memory/search ./internal/memory/leaves ./internal/memory/lexical | tee "$dir/go-test.txt"
# a gated test that skipped would pass without comparing anything
for name in TestSandboxRetrieveParity TestSandboxLeafParity TestSandboxFTS5Parity; do
	grep -q -- "^--- PASS: $name " "$dir/go-test.txt" || { echo "parity: $name did not run and pass" >&2; exit 1; }
done
echo "parity: the sandbox and the Go port agree on the synthetic corpus"
