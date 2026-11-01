#!/bin/bash
# 저장소 루트에서 Finder 더블클릭용. 실제 서버는 experimental/rag-leaf-parent.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
xattr -d com.apple.quarantine "$0" 2>/dev/null || true
xattr -d com.apple.quarantine "$HERE/experimental/rag-leaf-parent/start.command" 2>/dev/null || true
exec "$HERE/experimental/rag-leaf-parent/start.command"
