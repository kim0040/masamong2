"""Canonical block/view identity — Python side of contracts/memory/blocks.json."""

from __future__ import annotations

import json
import unittest
from pathlib import Path

from memory_blocks import (
    BlockError,
    assert_fingerprint,
    attach_block,
    block_id,
    catalog_from_leaves,
    embedding_space,
    make_view,
    scope_fingerprint,
)

ROOT = Path(__file__).resolve().parents[2]
CATALOG = json.loads((ROOT / "fixtures/memory/blocks/catalog.json").read_text(encoding="utf-8"))
VIEWS = json.loads((ROOT / "fixtures/memory/blocks/views.json").read_text(encoding="utf-8"))


class ContractFixtureTests(unittest.TestCase):
    def test_block_ids_match_fixture(self) -> None:
        space = CATALOG["embedding_space"]
        seg = CATALOG["segmentation_version"]
        for row in CATALOG["blocks"]:
            if row["embedding_space"] != space:
                continue
            got = block_id(row["collection"], row["source_room"], seg, row["embedding_space"])
            self.assertEqual(got, row["block_id"], row["source_room"])

    def test_mismatch_block_id(self) -> None:
        row = next(b for b in CATALOG["blocks"] if b["block_id"] == "ce64e0777720b7e3")
        self.assertEqual(
            block_id(row["collection"], row["source_room"], row["segmentation_version"], row["embedding_space"]),
            row["block_id"],
        )
        self.assertNotEqual(row["embedding_space"], CATALOG["embedding_space"])

    def test_view_fingerprints(self) -> None:
        for view in VIEWS["views"]:
            self.assertEqual(
                scope_fingerprint(view["blocks"], view["revision"]),
                view["scope_fingerprint"],
            )
            assert_fingerprint(view)

    def test_stale_fingerprint_rejected(self) -> None:
        stale = VIEWS["stale_after_revision"]
        with self.assertRaises(BlockError) as ctx:
            assert_fingerprint(stale)
        self.assertEqual(str(ctx.exception), "stale_scope")

    def test_mismatch_space_rejected(self) -> None:
        by_id = {b["block_id"]: b for b in CATALOG["blocks"]}
        spec = VIEWS["reject_mismatch_space"]
        view = make_view(spec["channel_key"], spec["existing_block_ids"], revision=1)
        bound = [by_id[i] for i in spec["existing_block_ids"]]
        with self.assertRaises(BlockError):
            attach_block(view, by_id[spec["add_block_id"]], bound)

    def test_catalog_from_leaves_one_room_one_block(self) -> None:
        space = embedding_space("openrouter", "embed", 8, "l2")
        leaves = [
            {"doc_id": "a", "source_room_id": "discord:1:10"},
            {"doc_id": "b", "source_room_id": "discord:1:10"},
            {"doc_id": "c", "source_room_id": "discord:1:20"},
        ]
        blocks = catalog_from_leaves(
            leaves, collection="discord", segmentation_version="leaf4-o0-lim0-prefix1-fixed", space=space
        )
        self.assertEqual(len(blocks), 2)
        rooms = {b["source_room"] for b in blocks}
        self.assertEqual(rooms, {"discord:1:10", "discord:1:20"})

    def test_room_labels(self) -> None:
        from memory_blocks import display_room_label

        self.assertEqual(display_room_label("kakao:friends-new"), "friends-new")
        self.assertEqual(display_room_label("kakao:club"), "club")
        self.assertEqual(display_room_label("discord:1:123456789"), "#456789")


if __name__ == "__main__":
    unittest.main()
