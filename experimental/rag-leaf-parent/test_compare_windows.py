"""Network-free tests for the bounded window comparison runner."""

from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace

HERE = Path(__file__).resolve().parent
if str(HERE) not in sys.path:
    sys.path.insert(0, str(HERE))

import compare_windows as cw  # noqa: E402


def _messages(count: int = 32) -> list[dict]:
    return [
        {
            "observation_id": f"obs-{index}",
            "source_room_id": "room-a" if index < count // 2 else "room-b",
            "sample_block": "early" if index < count // 2 else "late",
            "actor_label": "alice" if index % 2 else "bob",
            "actor_account": f"account-{index % 2}",
            "content": f"메시지 {index}",
            "parsed_time": f"2026-09-16T00:{index:02d}:00",
        }
        for index in range(count)
    ]


class CompareWindowsTests(unittest.TestCase):
    def test_gold_groups_are_bounded_before_chunking(self):
        messages = _messages(32)
        groups = cw._gold_groups(messages)
        self.assertTrue(groups)
        self.assertTrue(all(len(group) == 11 for group in groups))
        for group in groups:
            keys = {cw._message_block_key(messages[index], index) for index in group}
            self.assertEqual(len(keys), 1)
        selected = cw._evenly_spaced(groups, 2)
        self.assertEqual(len(selected), 2)
        # The gold groups are source ranges, not fixed20/fixed8 doc IDs.
        self.assertEqual(selected[0], groups[0])
        self.assertEqual(selected[-1], groups[-1])

    def test_rrf_respects_dense_four_weight(self):
        ranked = cw._rrf_rank(["lex-first", "dense-first"], ["dense-first", "lex-first"])
        self.assertEqual(ranked[:2], ["dense-first", "lex-first"])

    def test_provider_is_passed_only_when_signature_supports_it(self):
        seen = {}

        class AcceptsProvider:
            def chat(self, messages, *, max_tokens, temperature, reasoning, provider):
                seen["provider"] = provider
                return "ok"

        policy = {"order": ["DeepInfra"], "allow_fallbacks": True}
        result = cw._call_chat(
            AcceptsProvider(),
            [{"role": "user", "content": "q"}],
            max_tokens=1,
            temperature=0,
            reasoning="low",
            provider=policy,
        )
        self.assertEqual(result, "ok")
        self.assertEqual(seen["provider"], policy)

        class NoProvider:
            def chat(self, messages, *, max_tokens, temperature, reasoning):
                return "should not be called"

        with self.assertRaises(cw.ExperimentError):
            cw._call_chat(
                NoProvider(),
                [{"role": "user", "content": "q"}],
                max_tokens=1,
                temperature=0,
                reasoning="low",
                provider=policy,
            )

    def test_cache_key_changes_for_exact_payload(self):
        with tempfile.TemporaryDirectory() as directory:
            out = Path(directory)
            ledger = cw.UsageLedger(out, 1.0)
            calls = cw.CachedCalls(
                out,
                runtime="offline",
                client=None,
                ledger=ledger,
                provider_policy={"order": ["DeepInfra"], "allow_fallbacks": True},
            )
            first = calls._key("chat", "model-a", {"prompt": "one"})
            second = calls._key("chat", "model-a", {"prompt": "two"})
            third = calls._key("chat", "model-b", {"prompt": "one"})
            self.assertNotEqual(first, second)
            self.assertNotEqual(first, third)

    def test_normalize_messages_rejects_missing_or_duplicate_identity(self):
        with self.assertRaises(cw.ExperimentError):
            cw._normalize_messages([{"content": "방이 없다"}])
        with self.assertRaises(cw.ExperimentError):
            cw._normalize_messages(
                [
                    {"observation_id": "same", "source_room_id": "room-a", "content": "첫째"},
                    {"observation_id": "same", "source_room_id": "room-a", "content": "둘째"},
                ]
            )

    def test_pending_request_marker_blocks_restart_and_clears_after_checkpoint(self):
        with tempfile.TemporaryDirectory() as directory:
            out = Path(directory)
            ledger = cw.UsageLedger(out, 1.0)
            ledger.begin_request()
            marker = json.loads((out / "usage.json").read_text(encoding="utf-8"))
            self.assertIs(marker["pending_request"], True)
            with self.assertRaises(cw.ExperimentError):
                cw.UsageLedger(out, 1.0)

            ledger.checkpoint()
            ledger.clear_request()
            cleared = json.loads((out / "usage.json").read_text(encoding="utf-8"))
            self.assertIs(cleared["pending_request"], False)
            cw.UsageLedger(out, 1.0)

    def test_cached_call_marks_pending_before_invoke(self):
        with tempfile.TemporaryDirectory() as directory:
            out = Path(directory)
            ledger = cw.UsageLedger(out, 1.0)
            calls = cw.CachedCalls(
                out,
                runtime="live",
                client=object(),
                ledger=ledger,
                provider_policy={"order": ["DeepInfra"], "allow_fallbacks": True},
            )
            seen = []

            def invoke():
                seen.append(json.loads((out / "usage.json").read_text(encoding="utf-8"))["pending_request"])
                return "ok"

            result, cached = calls._cached("chat", "model-a", {"prompt": "one"}, invoke)
            self.assertEqual((result, cached), ("ok", False))
            self.assertEqual(seen, [True])
            final = json.loads((out / "usage.json").read_text(encoding="utf-8"))
            self.assertIs(final["pending_request"], False)

    def test_live_run_rejects_synthetic_metadata_and_offline_allows_context_repack(self):
        base = {
            "input_sha256": "input-sha",
            "recipe_revision": cw.RECIPE_REVISION,
            "runtime": "synthetic",
            "models": {},
            "provider_routing": {"mode": "synthetic_local"},
            "code_hashes": {"compare_windows.py": "code"},
            "prompt_revisions": dict(cw.PROMPT_REVISIONS),
            "segmentation_params": {
                "target_chars": 300,
                "max_chars": 1200,
                "max_messages": 40,
                "idle_seconds": 300.0,
            },
            "context_chars": 1600,
        }
        with self.assertRaises(cw.ExperimentError):
            cw._validate_existing_meta(
                base,
                input_sha="input-sha",
                recipe_revision=cw.RECIPE_REVISION,
                runtime="live",
                models={"llm": "live-model", "embedding": "live-embed"},
                routing={"mode": "live", "policy": {"order": ["DeepInfra"], "allow_fallbacks": True}},
                code_hashes={"compare_windows.py": "code"},
                prompt_revisions=cw.PROMPT_REVISIONS,
                segmentation_params=base["segmentation_params"],
                context_chars=1600,
            )
        cw._validate_existing_meta(
            base,
            input_sha="input-sha",
            recipe_revision=cw.RECIPE_REVISION,
            runtime="offline",
            models={},
            routing={"mode": "offline"},
            code_hashes={"compare_windows.py": "code"},
            prompt_revisions=cw.PROMPT_REVISIONS,
            segmentation_params=base["segmentation_params"],
            context_chars=4000,
        )

    def test_default_run_is_network_free_and_reports_visible_evidence(self):
        messages = _messages(32)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            input_path = root / "input.json"
            out = root / "out"
            input_path.write_text(json.dumps(messages, ensure_ascii=False), encoding="utf-8")
            args = cw.parser().parse_args(
                [
                    "--input",
                    str(input_path),
                    "--out",
                    str(out),
                    "--questions",
                    "2",
                    "--context-chars",
                    "4000",
                ]
            )
            report = cw.run(args)
            self.assertEqual(report["status"], "ok")
            self.assertEqual(report["cost_usd"], 0.0)
            self.assertEqual(report["attempts"], 0)
            self.assertEqual(set(report["variants"]), {spec["name"] for spec in cw.VARIANTS})
            for variant in report["variants"].values():
                self.assertEqual(variant["summary_used_for_evidence"], False)
                self.assertEqual(variant["evidence_all_visible"]["n"], 2)
            contexts = json.loads((out / "contexts.json").read_text(encoding="utf-8"))
            self.assertTrue(contexts["variants"]["adaptive+parent"]["questions"][0]["text"])

    def test_gold_validator_rejects_outside_group_without_fallback(self):
        value = {"question": "q", "answer": "a", "evidence_indices": [9]}
        accepted, reason = cw._validate_gold(value, {1, 2})
        self.assertIsNone(accepted)
        self.assertEqual(reason, "evidence_index_outside_group")


if __name__ == "__main__":
    unittest.main()
