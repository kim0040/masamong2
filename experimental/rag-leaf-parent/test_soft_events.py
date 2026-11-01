"""Pure offline tests for deterministic runtime-derived soft events."""

from __future__ import annotations

import copy
import math
import unittest

from soft_events import (
    SoftEventIndex,
    SoftEventRecipe,
    build_soft_events,
    make_event_id,
    project_event_ranks,
    rank_events,
)


def _leaf(
    leaf_id: str,
    room: str = "room-a",
    *,
    minute: int | None = 0,
    chars: int = 4,
) -> dict[str, object]:
    return {
        "doc_id": leaf_id,
        "source_room_id": room,
        "body": "x" * chars,
        "parsed_time": None if minute is None else f"2026-01-01T00:{minute:02d}:00+00:00",
    }


class SoftEventBuildTests(unittest.TestCase):
    def test_room_boundary_never_crosses_and_all_leaves_are_covered(self) -> None:
        leaves = [_leaf("a1", minute=0), _leaf("a2", minute=1), _leaf("b1", "room-b", minute=2)]
        result = build_soft_events(leaves, recipe={"max_leaves": 8, "max_chars": 100})

        self.assertEqual([event.source_room_id for event in result.events], ["room-a", "room-b"])
        self.assertEqual(result.leaf_ids, ("a1", "a2", "b1"))
        self.assertEqual(set(result.leaf_to_event_id), {"a1", "a2", "b1"})
        self.assertEqual([event.boundary_reason for event in result.events], ["room", "end"])
        self.assertTrue(all(len({event.source_room_id}) == 1 for event in result.events))

    def test_known_gap_splits_only_after_minimum(self) -> None:
        leaves = [_leaf("a1", minute=0), _leaf("a2", minute=1), _leaf("a3", minute=40)]
        result = build_soft_events(
            leaves,
            recipe={"gap_minutes": 30, "min_leaves": 2, "max_leaves": 8, "max_chars": 100},
        )
        self.assertEqual([event.leaf_ids for event in result.events], [("a1", "a2"), ("a3",)])
        self.assertEqual([event.boundary_reason for event in result.events], ["gap", "end"])
        self.assertEqual(result.recipe_revision, "soft-event-v1")

    def test_caps_split_without_dropping_or_overlapping_leaves(self) -> None:
        leaves = [_leaf(f"a{i}", minute=i, chars=5) for i in range(5)]
        result = build_soft_events(
            leaves,
            recipe=SoftEventRecipe(gap_seconds=None, max_leaves=2, max_chars=11, min_leaves=1),
        )
        self.assertEqual([event.leaf_ids for event in result.events], [("a0", "a1"), ("a2", "a3"), ("a4",)])
        self.assertEqual([event.boundary_reason for event in result.events], ["max_leaves", "max_leaves", "end"])
        flattened = [leaf_id for event in result.events for leaf_id in event.leaf_ids]
        self.assertEqual(flattened, [f"a{i}" for i in range(5)])
        self.assertEqual(len(flattened), len(set(flattened)))

    def test_single_overlong_leaf_is_retained(self) -> None:
        result = build_soft_events(
            [_leaf("a1", chars=50), _leaf("a2", minute=1, chars=2)],
            recipe={"gap_seconds": None, "max_chars": 10, "max_leaves": 8},
        )
        self.assertEqual([event.leaf_ids for event in result.events], [("a1",), ("a2",)])
        self.assertEqual([event.boundary_reason for event in result.events], ["max_chars", "end"])

    def test_unknown_timestamp_does_not_create_a_gap(self) -> None:
        leaves = [_leaf("a1", minute=0), _leaf("a2", minute=None), _leaf("a3", minute=100)]
        result = build_soft_events(
            leaves,
            recipe={"gap_minutes": 1, "max_leaves": 8, "max_chars": 100},
        )
        self.assertEqual([event.leaf_ids for event in result.events], [("a1", "a2", "a3")])
        self.assertIsNone(result.events[0].start_time)
        self.assertIsNone(result.events[0].end_time)

    def test_event_id_excludes_body_and_includes_recipe_room_membership(self) -> None:
        recipe = SoftEventRecipe(gap_seconds=None, max_leaves=3, max_chars=100, min_leaves=1)
        first = make_event_id(recipe, "room-a", ("a1", "a2"))
        second = make_event_id(recipe, "room-a", ("a1", "a2"))
        changed_body = make_event_id(recipe, "room-a", ("a1", "a2"))
        changed_recipe = make_event_id(
            SoftEventRecipe(gap_seconds=None, max_leaves=4, max_chars=100, min_leaves=1),
            "room-a",
            ("a1", "a2"),
        )
        changed_room = make_event_id(recipe, "room-b", ("a1", "a2"))
        changed_membership = make_event_id(recipe, "room-a", ("a1", "a3"))
        self.assertEqual(first, second)
        self.assertEqual(first, changed_body)
        self.assertNotEqual(first, changed_recipe)
        self.assertNotEqual(first, changed_room)
        self.assertNotEqual(first, changed_membership)

    def test_input_and_result_are_not_mutated(self) -> None:
        leaves = [_leaf("a1"), _leaf("a2", minute=1)]
        original = copy.deepcopy(leaves)
        vectors = {"a1": [1.0, 0.0], "a2": [0.0, 1.0]}
        vector_copy = copy.deepcopy(vectors)
        index = SoftEventIndex.from_leaves(leaves, leaf_vectors=vectors)
        result = build_soft_events(index)
        self.assertEqual(leaves, original)
        self.assertEqual(vectors, vector_copy)
        with self.assertRaises(TypeError):
            result.event_by_id["new"] = result.events[0]  # type: ignore[index]
        leaves[0]["body"] = "changed"
        self.assertNotEqual(result.events[0].char_count, len(leaves[0]["body"]))

    def test_mean_vector_is_l2_normalized(self) -> None:
        leaves = [_leaf("a1"), _leaf("a2", minute=1)]
        result = build_soft_events(
            leaves,
            leaf_vectors={"a1": [1.0, 0.0], "a2": [0.0, 1.0]},
            recipe={"gap_seconds": None, "max_leaves": 8, "max_chars": 100},
        )
        vector = result.events[0].vector
        self.assertIsNotNone(vector)
        assert vector is not None
        self.assertAlmostEqual(math.sqrt(sum(value * value for value in vector)), 1.0)
        self.assertAlmostEqual(vector[0], 1.0 / math.sqrt(2.0))
        self.assertAlmostEqual(vector[1], 1.0 / math.sqrt(2.0))

    def test_summary_is_search_text_only(self) -> None:
        result = build_soft_events(
            [_leaf("a1"), _leaf("a2", minute=1)],
            summaries={"a1": "derived one", "a2": "derived two"},
            recipe={"gap_seconds": None, "max_leaves": 8, "max_chars": 100},
        )
        event = result.events[0]
        self.assertEqual(event.search_text, "derived one\nderived two")
        self.assertFalse(event.is_raw_evidence)
        self.assertEqual(event.recipe_revision, "soft-event-v1")
        self.assertEqual(event.raw_evidence_leaf_ids, ("a1", "a2"))
        self.assertNotIn("body", event.as_dict())


class SoftEventProjectionTests(unittest.TestCase):
    def setUp(self) -> None:
        leaves = [_leaf(f"a{i}", minute=i) for i in range(4)]
        self.result = build_soft_events(
            leaves,
            recipe={"gap_seconds": None, "max_leaves": 2, "max_chars": 100},
        )

    def test_projection_preserves_event_then_leaf_order_and_deduplicates(self) -> None:
        event_ids = self.result.event_ids
        ranked = [event_ids[1], event_ids[1], event_ids[0], "unknown"]
        self.assertEqual(project_event_ranks(self.result, ranked), ("a2", "a3", "a0", "a1"))
        self.assertEqual(project_event_ranks(ranked, self.result, top_k=3), ("a2", "a3", "a0"))

    def test_score_mapping_and_rank_helper_use_deterministic_ties(self) -> None:
        event_ids = self.result.event_ids
        scores = {event_ids[0]: 0.5, event_ids[1]: 0.5}
        ranked = rank_events(self.result, scores)
        self.assertEqual(ranked, tuple(sorted(event_ids)))
        self.assertEqual(project_event_ranks(scores, self.result), tuple(leaf for event_id in sorted(event_ids) for leaf in self.result.event_by_id[event_id].leaf_ids))


if __name__ == "__main__":
    unittest.main()
