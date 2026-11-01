"""Thorough unit tests for rag_upgrade package (network-free)."""

from __future__ import annotations

import json
import sys
import threading
import unittest
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from rag_upgrade.ablation import Doc, aggregate_mrr, run_variant  # noqa: E402
from rag_upgrade.deep_search import (  # noqa: E402
    DeepSearchError,
    MAX_ROUNDS,
    next_query_strategy,
    should_abstain,
)
from rag_upgrade.evidence_pool import (  # noqa: E402
    EvidenceError,
    merge_pool,
    synthesizer_input_ok,
)
from rag_upgrade.gold_schema import (  # noqa: E402
    GoldSchemaError,
    parse_gold,
    score_retrieval,
)
from rag_upgrade.leaf_ids import (  # noqa: E402
    LeafContractError,
    build_leaf_from_messages,
    compute_leaf_id,
    parent_expand,
)
from rag_upgrade.observability_events import (  # noqa: E402
    ObservabilityError,
    feature_health,
    make_event,
)
from rag_upgrade.person_memory import PersonMemoryError, PersonMemoryView  # noqa: E402
from rag_upgrade.person_identity import (  # noqa: E402
    IdentityError,
    IdentityLink,
    MemorySpace,
    confirm_link,
    kakao_actor_id,
    search_scope,
)
from rag_upgrade.planner_needs import (  # noqa: E402
    PlannerSchemaError,
    score_planner_vs_gold,
    validate_planner_output,
)
from rag_upgrade.proactive_invariants import (  # noqa: E402
    ConversationEpoch,
    ProactiveCandidate,
    ProactiveInvariantError,
    can_send,
    shadow_record,
)
from rag_upgrade import RERANK_MODEL  # noqa: E402
from rag_upgrade.rerank_client import (  # noqa: E402
    RerankBudgetLedger,
    RerankClient,
    RerankBudgetError,
    RerankError,
    apply_rerank_order,
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
        self.last_request = None

    def __call__(self, request, timeout):
        self.calls += 1
        self.last_request = request
        outcome = self.outcomes.pop(0)
        if isinstance(outcome, BaseException):
            raise outcome
        return FakeResponse(outcome)


SAMPLE_GOLD = {
    "schema_version": 2,
    "dataset_id": "upgrade-synth-v1",
    "gold_kind": "synthetic",
    "review_status": "unreviewed",
    "questions": [
        {
            "question_id": "q-school",
            "query": "민준이가 어디 고등학교 나왔더라?",
            "split": "dev",
            "expected_memory_requirement": "required",
            "expected_abstention": False,
            "expected_evidence_ids": ["obs-school"],
            "forbidden_evidence_ids": ["obs-noise"],
            "expected_person_ids": ["person_dongjun"],
            "types": ["person_factual", "memory_only"],
        },
        {
            "question_id": "q-aapl",
            "query": "전에 내가 사려고 했던 주식 지금 얼마임?",
            "split": "dev",
            "expected_memory_requirement": "optional",
            "expected_abstention": False,
            "expected_evidence_ids": ["obs-aapl"],
            "expected_tools": ["finance.quote", "finance.fx"],
            "types": ["tool_plus_memory", "optional_memory"],
        },
        {
            "question_id": "q-none",
            "query": "오늘 날씨 어때?",
            "split": "dev",
            "expected_memory_requirement": "none",
            "expected_abstention": False,
            "expected_evidence_ids": ["obs-weather-placeholder"],
            "expected_tools": ["weather"],
            "types": ["tool_only"],
        },
        {
            "question_id": "q-abstain",
            "query": "존재하지 않는 사람 민수철 혈액형?",
            "split": "holdout",
            "expected_memory_requirement": "required",
            "expected_abstention": True,
            "forbidden_evidence_ids": ["obs-school"],
            "types": ["no_answer"],
        },
    ],
}


DOCS = [
    Doc("d1", "민준 한빛고 졸업 고등학교", ("obs-school",)),
    Doc("d2", "오늘 점심 메뉴 추천", ("obs-noise",)),
    Doc("d3", "애플 AAPL 주식 사려고 함 190", ("obs-aapl",)),
    Doc("d4", "날씨 맑음", ("obs-weather-placeholder",)),
]


class GoldSchemaTests(unittest.TestCase):
    def test_v3_question_type_aliases(self):
        raw = {
            **SAMPLE_GOLD,
            "questions": [
                {
                    "question_id": "q-v3",
                    "query": "ㅋㅋㅋ",
                    "split": "dev",
                    "question_type": "casual",
                    "expected_memory_requirement": "none",
                    "expected_abstention": True,
                    "types": ["casual"],
                    "review_status": "unreviewed",
                    "review_verdict": "",
                }
            ],
        }
        q = parse_gold(raw).questions[0]
        self.assertEqual(q.question_type, "casual")
        self.assertIn("casual", q.types)

    def test_parse_ok(self):
        ds = parse_gold(SAMPLE_GOLD)
        self.assertEqual(len(ds.questions), 4)
        self.assertEqual(ds.questions[0].expected_memory_requirement, "required")

    def test_rejects_v1(self):
        with self.assertRaises(GoldSchemaError):
            parse_gold({**SAMPLE_GOLD, "schema_version": 1})

    def test_rejects_forbidden_overlap(self):
        bad = json.loads(json.dumps(SAMPLE_GOLD))
        bad["questions"][0]["forbidden_evidence_ids"] = ["obs-school"]
        with self.assertRaises(GoldSchemaError):
            parse_gold(bad)

    def test_score_retrieval(self):
        q = parse_gold(SAMPLE_GOLD).questions[0]
        m = score_retrieval(q, ["obs-noise", "obs-school"], k=2)
        self.assertEqual(m["recall"], 1.0)
        self.assertTrue(m["forbidden_hit"])
        self.assertFalse(m["ok"])


class LeafIdTests(unittest.TestCase):
    def test_stable_id_independent_of_array_offset(self):
        a = compute_leaf_id("discord:1:2", "seg-v1", "r1", "r3")
        b = compute_leaf_id("discord:1:2", "seg-v1", "r1", "r3")
        self.assertEqual(a, b)
        c = compute_leaf_id("discord:1:2", "seg-v2", "r1", "r3")
        self.assertNotEqual(a, c)

    def test_rejects_cross_room_leaf(self):
        with self.assertRaises(LeafContractError):
            build_leaf_from_messages(
                [
                    {
                        "source_room_id": "a",
                        "message_revision_id": "1",
                        "content": "x",
                    },
                    {
                        "source_room_id": "b",
                        "message_revision_id": "2",
                        "content": "y",
                    },
                ],
                segmentation_version="v1",
            )

    def test_parent_expand_same_room_only(self):
        room = [
            {"source_room_id": "r", "message_revision_id": str(i), "content": "c"}
            for i in range(10)
        ]
        out = parent_expand(
            room, leaf_revision_ids=["4", "5"], before=2, after=2
        )
        self.assertEqual(out, ["2", "3", "4", "5", "6", "7"])

    def test_parent_expand_rejects_mixed_room_stream(self):
        with self.assertRaises(LeafContractError):
            parent_expand(
                [
                    {"source_room_id": "a", "message_revision_id": "1"},
                    {"source_room_id": "b", "message_revision_id": "2"},
                ],
                leaf_revision_ids=["1"],
                before=0,
                after=0,
            )


class RerankTests(unittest.TestCase):
    def test_default_model_is_fast(self):
        self.assertEqual(RERANK_MODEL, "voyageai/rerank-2.5-lite")

    def test_rerank_order_and_budget(self):
        transport = FakeTransport(
            [
                {
                    "results": [
                        {"index": 2, "relevance_score": 0.9},
                        {"index": 0, "relevance_score": 0.5},
                        {"index": 1, "relevance_score": 0.1},
                    ],
                    "usage": {"cost": 0.01},
                    "model": "voyageai/rerank-2.5-lite",
                }
            ]
        )
        client = RerankClient(
            {"OPENROUTER_API_KEY": "sk-test"},
            budget_usd=1.0,
            max_request_cost_usd=0.05,
            urlopen=transport,
        )
        docs = ["noise", "other", "민준 한빛고"]
        result = client.rerank("민준 고등학교", docs)
        self.assertEqual(result.model, RERANK_MODEL)
        self.assertEqual(result.indices[0], 2)
        ordered = apply_rerank_order(["a", "b", "c"], result)
        self.assertEqual(ordered[0], "c")
        self.assertEqual(client.spent_usd, 0.01)

    def test_rejects_discord_token_as_key(self):
        with self.assertRaises(RerankBudgetError):
            RerankClient(
                {
                    "OPENROUTER_API_KEY": "discord-bot-token",
                    "DISCORD_TOKEN": "discord-bot-token",
                },
                budget_usd=1.0,
                max_request_cost_usd=0.05,
            )

    def test_unknown_cost_still_ranks(self):
        transport = FakeTransport(
            [
                {"results": [{"index": 0, "relevance_score": 1}]},
                {"results": [{"index": 0, "relevance_score": 0.5}]},
            ]
        )
        client = RerankClient(
            {"OPENROUTER_API_KEY": "sk"},
            budget_usd=1.0,
            max_request_cost_usd=0.05,
            urlopen=transport,
        )
        result = client.rerank("q", ["d"])
        self.assertEqual(result.indices, [0])
        self.assertEqual(result.cost_usd, 0.05)
        self.assertEqual(client.unknown_cost_attempts, 1)
        self.assertEqual(client.unknown_cost_usd, 0.05)
        self.assertEqual(client.reserved_usd, 0.0)
        result2 = client.rerank("q", ["d"])
        self.assertEqual(result2.indices, [0])
        self.assertEqual(client.unknown_cost_attempts, 2)
        self.assertEqual(result2.cost_usd, 0.05)

    def test_shared_ledger_caps_multiple_clients_before_transport(self):
        ledger = RerankBudgetLedger(0.10)
        first_transport = FakeTransport(
            [{"results": [{"index": 0, "relevance_score": 1}], "usage": {"cost": 0.05}}]
        )
        second_transport = FakeTransport(
            [{"results": [{"index": 0, "relevance_score": 1}], "usage": {"cost": 0.05}}]
        )
        blocked_transport = FakeTransport([])
        first = RerankClient(
            {"OPENROUTER_API_KEY": "sk"},
            budget_usd=0.10,
            max_request_cost_usd=0.05,
            urlopen=first_transport,
            ledger=ledger,
        )
        second = RerankClient(
            {"OPENROUTER_API_KEY": "sk"},
            budget_usd=0.10,
            max_request_cost_usd=0.05,
            urlopen=second_transport,
            ledger=ledger,
        )
        blocked = RerankClient(
            {"OPENROUTER_API_KEY": "sk"},
            budget_usd=0.10,
            max_request_cost_usd=0.05,
            urlopen=blocked_transport,
            ledger=ledger,
        )

        first.rerank("q", ["d"])
        second.rerank("q", ["d"])
        with self.assertRaises(RerankBudgetError):
            blocked.rerank("q", ["d"])
        self.assertEqual(first_transport.calls, 1)
        self.assertEqual(second_transport.calls, 1)
        self.assertEqual(blocked_transport.calls, 0)
        self.assertEqual(ledger.spent_usd, 0.10)
        self.assertEqual(ledger.reserved_usd, 0.0)

    def test_shared_ledger_reserves_atomically_in_parallel(self):
        ledger = RerankBudgetLedger(0.05)
        transport = FakeTransport(
            [{"results": [{"index": 0, "relevance_score": 1}], "usage": {"cost": 0.05}}]
        )
        clients = [
            RerankClient(
                {"OPENROUTER_API_KEY": "sk"},
                budget_usd=0.05,
                max_request_cost_usd=0.05,
                urlopen=transport,
                ledger=ledger,
            )
            for _ in range(2)
        ]
        barrier = threading.Barrier(2)

        def run(client):
            barrier.wait(timeout=2)
            try:
                client.rerank("q", ["d"])
                return "ok"
            except RerankBudgetError:
                return "budget"

        with ThreadPoolExecutor(max_workers=2) as pool:
            outcomes = list(pool.map(run, clients))
        self.assertEqual(sorted(outcomes), ["budget", "ok"])
        self.assertEqual(transport.calls, 1)
        self.assertEqual(ledger.spent_usd, 0.05)
        self.assertEqual(ledger.reserved_usd, 0.0)

    def test_ledger_waits_for_other_thread_then_uses_settled_cost(self):
        # 0.03 예약 두 개는 0.05를 넘지만, 먼저 것이 0.01로 정산되면 다음 예약은 기다렸다가 통과한다.
        ledger = RerankBudgetLedger(0.05)
        first = ledger.reserve(0.03)
        got = {}

        def second():
            got["r"] = ledger.reserve(0.03)

        worker = threading.Thread(target=second)
        worker.start()
        worker.join(0.2)
        self.assertTrue(worker.is_alive())
        ledger.settle(first, 0.01)
        worker.join(2)
        self.assertIn("r", got)
        self.assertAlmostEqual(ledger.reserved_usd, 0.03)

    def test_ledger_same_thread_shortfall_fails_immediately(self):
        ledger = RerankBudgetLedger(0.05)
        ledger.reserve(0.03)
        with self.assertRaises(RerankBudgetError):
            ledger.reserve(0.03)

    def test_missing_cost_uses_generation_settlement(self):
        transport = FakeTransport(
            [
                {
                    "id": "generation-test",
                    "results": [{"index": 0, "relevance_score": 1}],
                },
                {"data": {"total_cost": 0.02}},
            ]
        )
        ledger = RerankBudgetLedger(0.10)
        client = RerankClient(
            {"OPENROUTER_API_KEY": "sk"},
            budget_usd=0.10,
            max_request_cost_usd=0.05,
            urlopen=transport,
            sleep=lambda _: None,
            ledger=ledger,
        )
        result = client.rerank("q", ["d"])
        self.assertEqual(result.cost_usd, 0.02)
        self.assertEqual(result.billed["cost_basis"], "generation")
        self.assertEqual(ledger.verified_cost_usd, 0.02)
        self.assertEqual(ledger.unknown_cost_attempts, 0)
        self.assertEqual(transport.calls, 2)

    def test_missing_cost_without_generation_is_conservative(self):
        transport = FakeTransport(
            [{"results": [{"index": 0, "relevance_score": 1}]}]
        )
        ledger = RerankBudgetLedger(0.05)
        client = RerankClient(
            {"OPENROUTER_API_KEY": "sk"},
            budget_usd=0.05,
            max_request_cost_usd=0.05,
            urlopen=transport,
            ledger=ledger,
        )
        result = client.rerank("q", ["d"])
        self.assertEqual(result.cost_usd, 0.05)
        self.assertEqual(result.billed["cost_basis"], "reservation")
        self.assertEqual(ledger.unknown_cost_usd, 0.05)
        self.assertEqual(ledger.estimated_cost_usd, 0.05)
        with self.assertRaises(RerankBudgetError):
            client.rerank("q", ["d"])
        self.assertEqual(transport.calls, 1)

    def test_transport_and_invalid_response_consume_reservation(self):
        transport = FakeTransport([OSError("disconnected"), "not-json"])
        ledger = RerankBudgetLedger(0.20)
        client = RerankClient(
            {"OPENROUTER_API_KEY": "sk"},
            budget_usd=0.20,
            max_request_cost_usd=0.05,
            urlopen=transport,
            ledger=ledger,
        )
        with self.assertRaises(RerankError):
            client.rerank("q", ["d"])
        self.assertEqual(ledger.unknown_cost_usd, 0.05)
        with self.assertRaises(RerankError):
            client.rerank("q", ["d"])
        self.assertEqual(ledger.unknown_cost_usd, 0.10)
        self.assertEqual(ledger.reserved_usd, 0.0)
        self.assertEqual(transport.calls, 2)

    def test_cost_overrun_is_recorded_and_blocks_followup(self):
        transport = FakeTransport(
            [
                {
                    "results": [{"index": 0, "relevance_score": 1}],
                    "usage": {"cost": 0.07},
                }
            ]
        )
        ledger = RerankBudgetLedger(0.20)
        client = RerankClient(
            {"OPENROUTER_API_KEY": "sk"},
            budget_usd=0.20,
            max_request_cost_usd=0.05,
            urlopen=transport,
            ledger=ledger,
        )
        with self.assertRaises(RerankBudgetError):
            client.rerank("q", ["d"])
        self.assertEqual(ledger.spent_usd, 0.07)
        self.assertTrue(ledger.overrun)
        with self.assertRaises(RerankBudgetError):
            client.rerank("q", ["d"])
        self.assertEqual(transport.calls, 1)


class AblationTests(unittest.TestCase):
    def test_r0_r1_r2_synthetic(self):
        gold = parse_gold(SAMPLE_GOLD)
        q = gold.questions[0]
        transport = FakeTransport(
            [
                {
                    "results": [
                        {"index": 2, "relevance_score": 0.99},
                        {"index": 0, "relevance_score": 0.2},
                        {"index": 1, "relevance_score": 0.1},
                        {"index": 3, "relevance_score": 0.05},
                    ],
                    "usage": {"cost": 0.001},
                }
            ]
        )
        client = RerankClient(
            {"OPENROUTER_API_KEY": "sk"},
            budget_usd=1.0,
            max_request_cost_usd=0.05,
            urlopen=transport,
        )
        r0 = run_variant(variant="R0", query=q.query, docs=DOCS, question=q)
        r1 = run_variant(variant="R1", query=q.query, docs=DOCS, question=q)
        r2 = run_variant(
            variant="R2", query=q.query, docs=DOCS, question=q, rerank_client=client
        )
        self.assertIn("metrics", r0)
        self.assertEqual(r2["ranked_docs"][0], "d3")  # forced by fake scores order
        # Actually index 2 is d3 in ranked hybrid order — verify R2 used rerank
        self.assertEqual(transport.calls, 1)
        self.assertGreaterEqual(r1["metrics"]["recall@5"]["recall"], 0.0)
        mrr = aggregate_mrr([r0, r1, r2], k=5)
        self.assertGreaterEqual(mrr, 0.0)


class PlannerTests(unittest.TestCase):
    def test_apple_needs(self):
        plan = validate_planner_output(
            {
                "goal": "AAPL 가격과 과거 투자 기억",
                "memory_requirement": "optional",
                "needs": [
                    {"id": "quote", "type": "market_quote", "required": True, "entity": "AAPL"},
                    {"id": "fx", "capability": "finance.fx", "required": True},
                    {
                        "id": "memory",
                        "type": "person_memory",
                        "required": False,
                        "topic": "AAPL",
                    },
                ],
            }
        )
        score = score_planner_vs_gold(
            plan,
            expected_memory_requirement="optional",
            expected_tools=["finance.quote", "finance.fx"],
        )
        self.assertTrue(score["memory_requirement_ok"])
        self.assertGreaterEqual(score["required_tool_recall"], 0.5)

    def test_rejects_required_memory_none(self):
        with self.assertRaises(PlannerSchemaError):
            validate_planner_output(
                {
                    "goal": "x",
                    "memory_requirement": "none",
                    "needs": [
                        {"id": "m", "type": "person_memory", "required": True},
                    ],
                }
            )


class EvidenceTests(unittest.TestCase):
    def test_merge_and_synthesizer(self):
        pool = merge_pool(
            [
                {
                    "id": "ev1",
                    "kind": "market_quote",
                    "status": "ok",
                    "data": {"symbol": "AAPL", "price": 240},
                },
                {
                    "id": "ev2",
                    "kind": "memory_claim",
                    "status": "ok",
                    "subject_person_id": "p1",
                    "evidence_ids": ["obs-aapl"],
                    "data": {"claim": "wanted AAPL"},
                },
            ]
        )
        self.assertEqual(len(pool), 2)
        self.assertTrue(synthesizer_input_ok(pool))
        with self.assertRaises(EvidenceError):
            merge_pool(
                [
                    {
                        "id": "bad",
                        "kind": "memory_claim",
                        "status": "ok",
                        "evidence_ids": [],
                    }
                ]
            )


class PersonMemoryTests(unittest.TestCase):
    def test_coalesce_many_messages_one_materialize(self):
        view = PersonMemoryView("person_42")
        for _ in range(30):
            view.mark_dirty("education.high_school")
        self.assertEqual(view.coalesce_dirty_ids(), ["education.high_school"])
        view.materialize(
            "education.high_school",
            claim_ids=["claim-1"],
            at="2026-09-16T00:00:00Z",
        )
        self.assertFalse(view.facets["education.high_school"].dirty)
        self.assertEqual(view.facets["education.high_school"].revision, 1)
        with self.assertRaises(PersonMemoryError):
            view.materialize(
                "education.high_school", claim_ids=["claim-2"], at="t"
            )


class PersonTests(unittest.TestCase):
    def test_memory_space_intersection(self):
        space = MemorySpace(
            "ms1",
            "discord:1:2",
            sources=["discord:1:2", "kakao:friends-old", "kakao:friends-new"],
        )
        scoped = search_scope(
            person_id="p1",
            memory_space=space,
            allowed_sources={"kakao:friends-old", "discord:1:2"},
        )
        self.assertEqual(scoped, {"kakao:friends-old", "discord:1:2"})
        self.assertTrue(kakao_actor_id("friends-old", "민준").startswith("kakao:"))
        link = confirm_link(
            IdentityLink(person_id="p1", account_id="discord:9", status="candidate")
        )
        self.assertEqual(link.status, "confirmed")
        with self.assertRaises(IdentityError):
            search_scope(
                person_id=None,
                memory_space=space,
                allowed_sources={"other"},
            )


class DeepSearchTests(unittest.TestCase):
    def test_two_rounds_then_abstain(self):
        self.assertEqual(MAX_ROUNDS, 2)
        r1 = next_query_strategy(1, base_query="민준 고등학교")
        r2 = next_query_strategy(
            2,
            base_query="민준 고등학교",
            aliases=["정민준"],
            predicates=["졸업", "한빛고"],
        )
        self.assertFalse(r1["alias_expansion"])
        self.assertTrue(r2["alias_expansion"])
        self.assertFalse(r2["top_k_only_increase"])
        self.assertTrue(should_abstain(2, False))
        with self.assertRaises(DeepSearchError):
            next_query_strategy(3, base_query="x")


class ProactiveTests(unittest.TestCase):
    def test_invariants(self):
        c = ProactiveCandidate(
            "c1",
            conversation_version="v1",
            topic_version="t1",
            human_anchor_message_ids=("h1",),
        )
        epoch = ConversationEpoch("v1", "h1")
        can_send(c, epoch)
        epoch.proactive_sent_ids.append("c1")
        epoch.human_activity_since_proactive = False
        with self.assertRaises(ProactiveInvariantError):
            can_send(
                ProactiveCandidate(
                    "c2", "v1", "t1", human_anchor_message_ids=("h1",)
                ),
                epoch,
            )
        shadow = shadow_record(c, would_participate=True, predicted_response="hi")
        self.assertEqual(shadow["mode"], "shadow")
        with self.assertRaises(ProactiveInvariantError):
            can_send(
                ProactiveCandidate(
                    "x", "v1", "t1", human_anchor_message_ids=(), source_message_is_bot=True
                ),
                epoch,
            )


class ObservabilityTests(unittest.TestCase):
    def test_allowlist_strips_secrets(self):
        ev = make_event(
            event_type="rag.retrieve.completed",
            component="memory",
            status="ok",
            trace_id="t",
            request_id="r",
            duration_ms=12,
            fields={
                "candidate_count": 40,
                "prompt": "SECRET",
                "api_key": "x",
                "cost": 0.01,
            },
        )
        self.assertNotIn("prompt", ev["fields"])
        self.assertNotIn("api_key", ev["fields"])
        self.assertEqual(ev["fields"]["candidate_count"], 40)
        health = feature_health(
            {
                "discord_ingestion": "healthy",
                "dense_rag": "degraded",
                "proactive": "blocked",
            }
        )
        self.assertEqual(health["proactive"], "blocked")
        with self.assertRaises(ObservabilityError):
            feature_health({"ready": "healthy"})


if __name__ == "__main__":
    unittest.main()
