#!/usr/bin/env bash
# Test-name guard for the split of internal/application (E7): after each step, no test may have
# disappeared from internal/application and the packages below it. A test that moved to a new package
# keeps its name, so the union of names stays.
#
#   scripts/testnames.sh            fail when a name in the golden file is gone (new tests are fine)
#   scripts/testnames.sh -update    write the golden file (after a test was removed on purpose)
#
# Names come from two sources, joined: `go test -list` (what this machine builds) and the source of
# every *_test.go file whatever its build tags, so a Windows-only test that moves is seen on a Mac.
set -euo pipefail
cd "$(dirname "$0")/.."

golden=internal/application/testdata/testnames.golden
mode=check
case "${1:-}" in
  "") ;;
  -update) mode=update ;;
  *) echo "usage: scripts/testnames.sh [-update]" >&2; exit 2 ;;
esac

listed=$(go test -list '.' ./internal/application/...)
declared=$(find internal/application -name '*_test.go' -print0 |
  xargs -0 grep -hE '^func (Test|Benchmark|Fuzz|Example)([^a-z][A-Za-z0-9_]*)?\(' || true)

current=$(
  {
    printf '%s\n' "$listed" | grep -E '^(Test|Benchmark|Fuzz|Example)([^a-z][A-Za-z0-9_]*)?$' || true
    printf '%s\n' "$declared" | sed -E 's/^func ([A-Za-z0-9_]*)\(.*/\1/'
  } | grep -vx 'TestMain' | LC_ALL=C sort -u
)

if [ "$mode" = update ]; then
  printf '%s\n' "$current" > "$golden"
  echo "testnames: wrote $(wc -l < "$golden" | tr -d ' ') names to $golden"
  exit 0
fi

[ -f "$golden" ] || { echo "testnames: $golden is missing; run scripts/testnames.sh -update" >&2; exit 1; }
missing=$(LC_ALL=C comm -23 "$golden" <(printf '%s\n' "$current"))
added=$(LC_ALL=C comm -13 "$golden" <(printf '%s\n' "$current") | wc -l | tr -d ' ')
if [ -n "$missing" ]; then
  echo "testnames: these tests are gone from internal/application/...:" >&2
  printf '  %s\n' $missing >&2
  echo "A move keeps the name. If a test was removed on purpose, run scripts/testnames.sh -update and say so in the commit." >&2
  exit 1
fi
echo "testnames: no test lost ($(wc -l < "$golden" | tr -d ' ') pinned, $added new since the golden file)"
