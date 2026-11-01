"""Synthetic, network-free tests for the evaluation OpenRouter client."""
from __future__ import annotations

import json
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))

from openrouter import (  # noqa: E402
    BudgetConfigurationError,
    BudgetExceededError,
    BudgetReconciliationError,
    OpenRouter,
    ProviderRequestError,
    ProviderResponseError,
)


class FakeResponse:
    def __init__(self, body: dict):
        self._body = body

    def __enter__(self):
        return self

    def __exit__(self, *args):
        return False

    def read(self):
        return json.dumps(self._body).encode()


class FakeTransport:
    def __init__(self, outcomes):
        self.outcomes = list(outcomes)
        self.calls = 0

    def __call__(self, request, timeout):
        self.calls += 1
        outcome = self.outcomes.pop(0)
        if isinstance(outcome, BaseException):
            raise outcome
        return FakeResponse(outcome)


def usage(cost: float = 0.1) -> dict:
    return {"cost": cost, "prompt_tokens": 2, "completion_tokens": 1}


def embedding_body(data, *, cost: float = 0.1, include_usage: bool = True) -> dict:
    body = {"data": data}
    if include_usage:
        body["usage"] = usage(cost)
    return body


def row(index: int, vector) -> dict:
    return {"index": index, "embedding": vector}


class OpenRouterTests(unittest.TestCase):
    def client(self, transport, *, budget=1.0, max_request=0.1):
        return OpenRouter(
            {"OPENROUTER_API_KEY": "synthetic"},
            budget_usd=budget,
            max_request_cost_usd=max_request,
            urlopen=transport,
            sleep=lambda _: None,
        )

    def test_exact_budget_reservation_is_rejected_before_transport(self):
        transport = FakeTransport([])
        client = self.client(transport, budget=1.0, max_request=1.0)

        with self.assertRaises(BudgetExceededError):
            client.embed(["one"])

        self.assertEqual(transport.calls, 0)
        self.assertEqual(client.usage.attempts, 0)

    def test_missing_max_request_cost_blocks_paid_call(self):
        transport = FakeTransport([])
        client = OpenRouter(
            {"OPENROUTER_API_KEY": "synthetic"},
            budget_usd=1.0,
            urlopen=transport,
            sleep=lambda _: None,
        )

        with self.assertRaises(BudgetConfigurationError):
            client.embed(["one"])

        self.assertEqual(transport.calls, 0)

    def test_missing_usage_cost_locks_subsequent_call(self):
        transport = FakeTransport(
            [embedding_body([row(0, [1.0, 0.0])], include_usage=False),
             embedding_body([row(0, [1.0, 0.0])])]
        )
        client = self.client(transport)

        with self.assertRaises(BudgetReconciliationError):
            client.embed(["one"])
        with self.assertRaises(BudgetReconciliationError):
            client.embed(["two"])

        self.assertEqual(transport.calls, 1)
        self.assertEqual(client.usage.cost_usd, 0.0)
        self.assertEqual(client.usage.unknown_cost_attempts, 1)
        self.assertEqual(client.budget_state, "reconciliation_required")

    def test_embedding_response_count_index_dimension_and_finite_nonzero(self):
        cases = [
            (embedding_body([row(0, [1.0, 0.0])]), ["a", "b"], "count"),
            (embedding_body([row(0, [1.0, 0.0]), row(2, [0.0, 1.0])]), ["a", "b"], "index"),
            (embedding_body([row(0, [1.0, 0.0]), row(1, [0.0, 1.0, 0.0])]), ["a", "b"], "dimension"),
            (embedding_body([row(0, [float("nan"), 0.0])]), ["a"], "non-finite"),
            (embedding_body([row(0, [0.0, 0.0])]), ["a"], "zero"),
        ]
        for body, texts, label in cases:
            with self.subTest(label=label):
                client = self.client(FakeTransport([body]))
                with self.assertRaises(ProviderResponseError):
                    client.embed(texts)

    def test_retry_attempts_are_bounded_and_each_attempt_is_reserved(self):
        failures = [TimeoutError("synthetic timeout") for _ in range(3)]
        transport = FakeTransport(failures)
        client = self.client(transport, budget=1.0, max_request=0.2)

        with self.assertRaises(ProviderRequestError):
            client._post("/synthetic", {"x": "y"}, attempts=3)

        self.assertEqual(transport.calls, 3)
        self.assertEqual(client.usage.attempts, 3)
        self.assertEqual(client.usage.retries, 2)
        self.assertAlmostEqual(client.usage.reserved_cost_usd, 0.6)
        self.assertEqual(client.usage.unknown_cost_attempts, 3)
        self.assertEqual(client.budget_state, "reconciliation_required")


if __name__ == "__main__":
    unittest.main()
