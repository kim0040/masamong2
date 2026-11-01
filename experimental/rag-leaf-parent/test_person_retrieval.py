"""Person / alias candidate retrieval (R0 v2): resolver rules, scope, unique hits, failure isolation."""

from __future__ import annotations

import unittest

import numpy as np

import engine as E
import person_retrieval as PR
from test_engine_retrieval import ROOM_NEW, ROOM_OLD, _FakeClient, _unit, build_index


def _intent(**kw):
    base = {"recent": False, "intro": False, "politics": False, "prefer_missing_over_guess": False,
            "kind": "person_fact", "targets": [], "source": "planner"}
    base.update(kw)
    return base


class ResolverTests(unittest.TestCase):
    def setUp(self) -> None:
        self.index = build_index()
        self.aliases = E._alias_table(self.index.cfg)

    def _resolve(self, targets, **kw):
        return PR.resolve_persons(self.index, targets, self.aliases, token_in_text=E._token_in_text, **kw)

    def test_exact_name(self) -> None:
        res = self._resolve(["을동"])
        self.assertEqual(res.speakers, {"을동"})
        self.assertEqual(res.unresolved, [])

    def test_alias_maps_to_speaker_both_directions(self) -> None:
        # config: 가명 → [김가명]. Speaker label in the index is 가명.
        self.assertEqual(self._resolve(["김가명"]).speakers, {"가명"})
        self.assertEqual(self._resolve(["가명"]).speakers, {"가명"})
        self.assertIn("김가명", self._resolve(["가명"]).labels)

    def test_unknown_name(self) -> None:
        res = self._resolve(["없는사람"])
        self.assertFalse(res.resolved)
        self.assertEqual(res.unresolved, ["없는사람"])

    def test_ambiguous_target_is_dropped_not_merged(self) -> None:
        # Four speakers all end with "수"-like tokens? Build a synthetic ambiguity instead.
        self.index.leaf_ids_by_speaker = {"김민수": ["a"], "이민수": ["b"], "박민수": ["c"], "최민수": ["d"]}
        res = self._resolve(["민수"], max_speakers_per_target=3)
        self.assertEqual(res.ambiguous, ["민수"])
        self.assertEqual(res.speakers, set())
        res2 = self._resolve(["민수"], max_speakers_per_target=4)
        self.assertEqual(len(res2.speakers), 4)  # explicit cap raise, still no merge into one id

    def test_similar_strings_are_not_merged(self) -> None:
        self.index.leaf_ids_by_speaker = {"현민": ["a"], "현민석": ["b"]}
        res = self._resolve(["현민"])
        self.assertEqual(res.speakers, {"현민"})  # 현민석 ⊅ 현민 (boundary rule)


class SpeakerRetrieverTests(unittest.TestCase):
    def test_speaker_run_stays_in_room(self) -> None:
        index = build_index()
        aliases = E._alias_table(index.cfg)
        res = PR.resolve_persons(index, ["을동"], aliases, token_in_text=E._token_in_text)
        allowed = {d for d in index.leaf_ids if index.leaf_by_id[d]["source_room_id"] == ROOM_NEW}
        fused, universe = PR.speaker_fusion(index, res, query="시험", q_vec=_unit(1), allowed=allowed,
                                            limit=50, weights=[1.0, 4.0], lock=index.lock)
        self.assertTrue(universe)
        self.assertTrue(universe <= allowed)
        self.assertTrue(set(fused.ids()) <= allowed)
        for d in fused.ids():
            self.assertIn("을동", index.leaf_by_id[d]["speakers"])

    def test_speaker_only_hit_enters_pool(self) -> None:
        """A leaf that neither lexical nor dense surfaces is still a candidate via the speaker run."""

        index = build_index(with_summaries=False)
        index.cfg["lexical_limit"] = 3
        index.cfg["dense_limit"] = 3
        q = _unit(9)
        trace: dict = {}
        E.retrieve(index, _FakeClient(q), "그 사람 시험 얘기", top_k=3, q_vec=q, ranking="hybrid",
                   trace=trace, source_room_id=ROOM_OLD, intent=_intent(targets=["병수"]))
        self.assertIn("speaker_dense", trace["candidate_sources"])
        # leaves not in lexical/dense top-3 but present through the person sources
        only_speaker = [row for row in trace["candidate_provenance"]
                        if "speaker_dense" in row["sources"]
                        and not ({"leaf_lexical", "leaf_dense"} & set(row["sources"]))]
        self.assertTrue(only_speaker, "speaker retriever must add candidates beyond lexical/dense top-3")
        self.assertTrue(all(row["flags"].get("speaker_match") for row in only_speaker))
        self.assertEqual(trace["persons"]["speakers"], ["병수"])

    def test_alias_run_finds_mentions(self) -> None:
        index = build_index(with_summaries=False)
        aliases = E._alias_table(index.cfg)
        res = PR.resolve_persons(index, ["가명"], aliases, token_in_text=E._token_in_text)
        allowed = {d for d in index.leaf_ids if index.leaf_by_id[d]["source_room_id"] == ROOM_OLD}
        run = PR.alias_run(index, res, allowed=allowed, limit=20, lock=index.lock)
        self.assertTrue(run)
        self.assertTrue({d for d, _ in run} <= allowed)
        self.assertTrue(all("김가명" in index.leaf_by_id[d]["body"] for d, _ in run[:3]))

    def test_disabled_or_no_targets_keeps_r0_identical(self) -> None:
        index = build_index()
        index.cfg["alias_expand_lexical"] = False  # legacy R0 knob; isolate the new retrievers
        q = _unit(2)
        t1: dict = {}
        base = E.retrieve(index, _FakeClient(q), "노트북 추천", top_k=5, q_vec=q, ranking="hybrid", trace=t1,
                          source_room_id=ROOM_OLD, intent=_intent(targets=[]))
        index.cfg["retrieval"] = {"person": {"enabled": False}, "alias": {"enabled": False}}
        t2: dict = {}
        off = E.retrieve(index, _FakeClient(q), "노트북 추천", top_k=5, q_vec=q, ranking="hybrid", trace=t2,
                         source_room_id=ROOM_OLD, intent=_intent(targets=["을동"]))
        self.assertEqual([h["doc_id"] for h in base], [h["doc_id"] for h in off])
        self.assertNotIn("speaker_dense", t2["candidate_sources"])

    def test_retriever_failure_is_isolated(self) -> None:
        index = build_index()
        saved = E.resolve_persons
        E.resolve_persons = lambda *a, **k: (_ for _ in ()).throw(RuntimeError("boom"))
        try:
            q = _unit(2)
            trace: dict = {}
            hits = E.retrieve(index, _FakeClient(q), "을동 뭐래", top_k=3, q_vec=q, ranking="hybrid",
                              trace=trace, source_room_id=ROOM_OLD, intent=_intent(targets=["을동"]))
        finally:
            E.resolve_persons = saved
        self.assertTrue(hits)
        self.assertEqual(trace["person_retriever_error"], "RuntimeError")
        self.assertEqual(trace["retrievers_used"], ["leaf_lexical", "leaf_dense", "summary_lexical", "summary_dense"])

    def test_pool_size_config(self) -> None:
        from retrieval_config import pool_size

        self.assertEqual(pool_size({"retrieve_pool": 100}, top_k=5), 100)
        self.assertEqual(pool_size({"retrieve_pool": 100}, top_k=5, recovery=True), 150)
        self.assertEqual(pool_size({"retrieve_pool": 150, "retrieval": {"candidate_pool": {"max": 160}}},
                                   top_k=5, recovery=True), 160)
        self.assertEqual(pool_size({}, top_k=5), 100)
