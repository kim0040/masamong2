#!/bin/bash
# Embedding-width sweep on copies of evaluation data roots (real chat history: never commit outputs).
#
#   DIMS="4096 1024" MSGVEC=0 tools/evaluation/memory_dims/run_sweep.sh
#   DIMS="4096 1024" MSGVEC=1 tools/evaluation/memory_dims/run_sweep.sh   # message vectors on
#
# Env: SRC   data roots made by `runner setup` (default .masamong-scratch/eval-20261011/roots/<set>)
#      OUT   output folder (default .masamong-scratch/eval-dims), runs land in $OUT/runs
#      GOLD1 single-message gold dir (default .masamong-scratch/eval-20261011/gold, <set>.json)
#      GOLD2 span gold dir (default .masamong-scratch/eval-20261012/gold, <set>.span.json)
#      SETS  (default "discord kakao-a kakao-b"), MODES (default hybrid,rerank)
#      EVAL_KEYFILE OpenRouter key file (default .private/test-keys/openrouter)
# Sets run one after another: two sets at once make the reranker answer 429.
# A lower width is applied by the product (adaptSpace truncation, no API call). MSGVEC=1 builds
# message vectors once on the widest copy (one paid index pass per set, about $0.01 and 10 min on a
# low-spec box) and copies that root for the narrower widths: message vectors live in their own
# space (model:min(width,1024):l2), so widths >= 1024 share them; below 1024 they are cut too.
set -u
cd "$(dirname "$0")/../../.."
T=tools/evaluation/memory_dims
SRC=${SRC:-.masamong-scratch/eval-20261011/roots}
OUT=${OUT:-.masamong-scratch/eval-dims}
GOLD1=${GOLD1:-.masamong-scratch/eval-20261011/gold}
GOLD2=${GOLD2:-.masamong-scratch/eval-20261012/gold}
SETS=${SETS:-discord kakao-a kakao-b}
MODES=${MODES:-hybrid,rerank}
MSGVEC=${MSGVEC:-0}
DIMS=${DIMS:-4096 2048 1024 512 256}
mkdir -p "$OUT/roots" "$OUT/runs"
EXE=""; case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) EXE=.exe;; esac  # Windows Git Bash
RUN="$OUT/runner$EXE"
go build -o "$RUN" ./$T/runner || exit 1
mv_flag=""; tag=""
if [ "$MSGVEC" = 1 ]; then mv_flag="-msgvec"; tag=".mv"; fi
widest=$(echo $DIMS | tr ' ' '\n' | sort -rn | head -1)
for set in $SETS; do
  base="$SRC/$set"
  if [ "$MSGVEC" = 1 ]; then
    base="$OUT/roots/$set-mvbase"
    if [ ! -d "$base" ]; then
      cp -R "$SRC/$set" "$base"
      echo "== $(date +%H:%M:%S) $set: build message vectors at width $widest"
      "$RUN" index -root "$base" -dims "$widest" -msgvec -budget 0.10 | cut -c1-400
    fi
  fi
  for dims in $DIMS; do
    R="$OUT/roots/$set-$dims$tag"
    [ -d "$R" ] || cp -R "$base" "$R"
    for g in single span; do
      if [ $g = single ]; then GF=$GOLD1/$set.json; else GF=$GOLD2/$set.span.json; fi
      o="$OUT/runs/$set.$g.d$dims$tag.jsonl"
      [ -s "$o" ] && continue
      echo "== $(date +%H:%M:%S) $set $g d$dims$tag"
      "$RUN" search -root "$R" -gold "$GF" -out "$o" -modes "$MODES" -workers 4 -dims "$dims" $mv_flag 2>&1 | grep -E "dims=|index leaves|wrote|patch|die|error" | head -5
    done
  done
done
echo "== done $(date +%H:%M:%S); summary: (cd $OUT && python3 $PWD/$T/agg.py)  # Windows: python"
