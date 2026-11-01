"""RAG / Memory / Agent upgrade evaluation package (instruction 2026-09-16).

Synthetic fixtures and network-free unit tests by default.
Live OpenRouter/Cohere requires explicit --live and budget; Discord tokens are forbidden.
"""

from __future__ import annotations

__all__ = [
    "MEMORY_REQUIREMENTS",
    "RERANK_MODEL",
]

MEMORY_REQUIREMENTS = frozenset({"none", "optional", "required"})
RERANK_MODEL = "voyageai/rerank-2.5-lite"
