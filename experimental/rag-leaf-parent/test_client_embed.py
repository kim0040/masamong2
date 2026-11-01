"""Query embedding instruction is applied only for Qwen3 query vectors."""

from __future__ import annotations

import unittest

from client import (
    DEFAULT_EMBED_QUERY_INSTRUCTION,
    _embed_inputs,
    format_embed_query,
)


class EmbedInstructionTests(unittest.TestCase):
    def test_query_prefix_on_qwen3_only(self) -> None:
        q = "요즘 뭐해"
        out = _embed_inputs(
            [q],
            model="qwen/qwen3-embedding-8b",
            kind="query",
            instruction=None,
        )
        self.assertEqual(out[0], format_embed_query(q, instruction=DEFAULT_EMBED_QUERY_INSTRUCTION))
        self.assertIn("Instruct:", out[0])
        doc = _embed_inputs([q], model="qwen/qwen3-embedding-8b", kind="document", instruction=None)
        self.assertEqual(doc, [q])
        other = _embed_inputs([q], model="openai/text-embedding-3-small", kind="query", instruction=None)
        self.assertEqual(other, [q])

    def test_custom_instruction(self) -> None:
        out = _embed_inputs(
            ["hello"],
            model="Qwen/Qwen3-Embedding-8B",
            kind="query",
            instruction="find chat",
        )
        self.assertTrue(out[0].startswith("Instruct: find chat"))


if __name__ == "__main__":
    unittest.main()
