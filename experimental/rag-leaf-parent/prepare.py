#!/usr/bin/env python3
"""소규모 인덱스 준비. 전수 처리 금지."""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

from engine import PlaygroundError, load_config, prepare_index  # noqa: E402


def main() -> int:
    parser = argparse.ArgumentParser(description="테스트 샌드박스 소규모 인덱스 준비 (산출: test-sandbox/data)")
    parser.add_argument("--force", action="store_true", help="체크포인트·인덱스를 버리고 처음부터 다시 만든다")
    parser.add_argument("--config", type=Path, default=None, help="config.json 경로 (기본: 이 폴더 config.json)")
    parser.add_argument("--data-dir", type=Path, default=None, help="인덱스 출력 디렉터리 (기본: config data_dir / test-sandbox/data)")
    args = parser.parse_args()
    if args.config:
        os.environ["MASAMONG_SANDBOX_CONFIG"] = str(Path(args.config).resolve())
    if args.data_dir:
        os.environ["MASAMONG_SANDBOX_DATA"] = str(Path(args.data_dir).resolve())
    cfg = load_config()
    print("=== 테스트 샌드박스 prepare ===")
    print(f"experiment={cfg.get('experiment')} collection={cfg['collection']} rooms={cfg['rooms']}")
    print(
        f"messages≤{cfg.get('message_limit')} leaf={cfg.get('leaf_size')}×{cfg.get('leaf_limit')} "
        f"summary={cfg.get('summary_enabled')} workers={cfg.get('api_workers')} "
        f"(전수 금지) prepare_budget_usd={cfg['prepare_budget_usd']}"
    )
    print("중단 후 재실행 시 완료분(요약·임베딩)은 건너뛴다. 처음부터: --force")
    try:
        result = prepare_index(force=args.force)
    except PlaygroundError as exc:
        print(f"실패: {exc}", file=sys.stderr)
        return 1
    except Exception as exc:  # noqa: BLE001 - surface prep failures cleanly
        print(f"실패: {type(exc).__name__}: {exc}", file=sys.stderr)
        return 1
    meta = result["meta"]
    print(json.dumps({
        "reused": result["reused"],
        "resumed": result.get("resumed", False),
        "message_count": meta.get("message_count"),
        "leaf_count": meta.get("leaf_count"),
        "baseline_count": meta.get("baseline_count"),
        "summary_count": meta.get("summary_count"),
        "prepare_cost_usd": meta.get("prepare_cost_usd"),
        "prepare_usage": meta.get("prepare_usage"),
        "recipe": meta.get("recipe", {}).get("mode"),
    }, ensure_ascii=False, indent=2))
    print("준비 완료. start.command 또는 python3 app.py 로 서버를 켠다.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
