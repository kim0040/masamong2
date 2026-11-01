"""Compat shim: the client moved to rerank_client.py (vendor-neutral). Import from there."""

from .rerank_client import *  # noqa: F401,F403
from .rerank_client import CohereRerankClient, RerankClient, RerankError  # noqa: F401
