"""retrieval.message_dense: parity with the Go implementation through the shared fixture
(internal/memory/search/testdata/message_dense), the config defaults, the engine integration (off is
byte-identical, on adds one run, recovery scales the run) and the optional artifacts. No network."""

from __future__ import annotations

import copy
import json
import tempfile
import importlib.util
import unittest
from pathlib import Path
from unittest import mock

import numpy as np

import engine as E
import message_vectors as MV
from candidates import Candidate, FusionResult, fuse, guarantee_pool, source_family
from retrieval_config import DEFAULTS, retrieval_config
from test_engine_retrieval import ROOM_OLD, _FakeClient, _unit, build_index

FIXTURE = (
    Path(__file__).resolve().parents[2]
    / "internal" / "memory" / "search" / "testdata" / "message_dense" / "fixture.json"
)
QUANTIZE_FIXTURE = FIXTURE.with_name("quantize_fixture.json")


def _norm(v: np.ndarray) -> np.ndarray:
    v = np.asarray(v, dtype=np.float32)
    return v / (np.linalg.norm(v, axis=-1, keepdims=True) + 1e-9)


class MessageDimensionTests(unittest.TestCase):
    def test_detached_import_and_explicit_flag_do_not_need_hosts_json(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            # Import the actual module from a detached sandbox directory, with no product catalogue.
            path = Path(tmp) / "sandbox" / "rag" / "message_vectors.py"
            path.parent.mkdir(parents=True)
            path.write_text(Path(MV.__file__).read_text(encoding="utf-8"), encoding="utf-8")
            spec = importlib.util.spec_from_file_location("detached_message_vectors", path)
            detached = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(detached)  # importing succeeds before classification is needed
            missing = Path(tmp) / "internal/providers/openai/hosts.json"
            self.assertFalse(missing.exists())
            with self.assertRaisesRegex(RuntimeError, "MRL 모델 판정용 카탈로그.*FileNotFoundError") as caught:
                detached.embed_full_width("unknown-model")
            self.assertIn(str(missing), str(caught.exception))
            self.assertIn("embed_truncatable", str(caught.exception))
            np.save(Path(tmp) / "leaf_vecs.npy", np.ones((1, 768), dtype=np.float16))
            messages = [{"observation_id": "m1", "actor_label": "x", "content": "synthetic message"}]
            leaves = [{"doc_id": "L0", "observation_ids": ["m1"]}]
            with mock.patch.object(E, "DATA", Path(tmp)), mock.patch.object(E, "embed_full_width", detached.embed_full_width):
                for flag, want in [(False, 768), (True, 512)]:
                    fp, _, _ = E._message_fingerprint_for(
                        {"embed_truncatable": flag, "embed_dimension": 1024}, "unknown-model", messages, leaves
                    )
                    self.assertEqual(fp["dims"], want)
                with self.assertRaises(RuntimeError):
                    E._message_fingerprint_for({}, "unknown-model", messages, leaves)

    def test_width_rules_and_measurement_override(self) -> None:
        self.assertEqual(MV.MAX_MESSAGE_DIMS, 512)
        self.assertEqual(MV.MAX_MEASURED_MESSAGE_DIMS, 1024)
        self.assertEqual(MV.message_dims(256, embed_truncatable=True), 256)
        self.assertEqual(MV.message_dims(512, embed_truncatable=True), 512)
        self.assertEqual(MV.message_dims(4096, embed_truncatable=True), 512)
        self.assertEqual(MV.message_dims(0, embed_truncatable=True), 512)
        self.assertEqual(MV.message_dims(4096), 4096)  # non-MRL follows actual leaf width
        self.assertEqual(MV.message_dims(4096, override=1024), 1024)
        self.assertEqual(MV.message_dims(4096, embed_truncatable=True, override=2048), 512)

    def test_non_mrl_vectors_are_not_cut(self) -> None:
        full = np.arange(1, 9, dtype=np.float32).reshape(1, 8)
        fitted = MV.fit_message_vectors(full, MV.message_dims(8, embed_truncatable=False))
        self.assertEqual(fitted.shape, (1, 8))
        self.assertTrue(np.allclose(np.linalg.norm(fitted, axis=1), 1.0))

    def test_mrl_catalogue_matches_go_shared_table(self) -> None:
        path = Path(__file__).resolve().parents[2] / "internal/providers/openai/testdata/embed_widths_parity.json"
        for row in json.loads(path.read_text(encoding="utf-8")):
            with self.subTest(model=row["model"]):
                self.assertEqual(MV.reference_name(row["model"]), row["reference"])
                self.assertEqual(MV.embed_full_width(row["model"]), row["width"])


class FixtureParityTests(unittest.TestCase):
    """The same answers the Go test (TestMessageDenseFixture) asserts."""

    @classmethod
    def setUpClass(cls) -> None:
        cls.fx = json.loads(FIXTURE.read_text(encoding="utf-8"))

    def test_quantisation_matches_the_go_bytes(self) -> None:
        """Half-way values, the clip and a zero row quantise to the same int8 values and the same float32 scale
        bytes as Go's search.EncodeQuantizedRow (TestQuantizeFixture)."""

        qfx = json.loads(QUANTIZE_FIXTURE.read_text(encoding="utf-8"))
        for row in qfx["rows"]:
            with self.subTest(row=row["note"]):
                q, scale = MV.quantize_rows(np.array([row["values"]], dtype=np.float32))
                self.assertEqual(q[0].tolist(), row["q"])
                self.assertEqual(scale.astype("<f4").tobytes().hex(), row["scale_hex"])
                self.assertEqual(q[0].tobytes().hex() + scale.astype("<f4").tobytes().hex(), row["blob_hex"])

    def test_an_older_float16_artifact_is_scored_as_the_quantised_rows(self) -> None:
        """The loader quantises a float16 file like Go quantises the same stored row (float16 -> float32 ->
        int8 + scale), so both score q * scale, not the unquantised floats."""

        qfx = json.loads(QUANTIZE_FIXTURE.read_text(encoding="utf-8"))
        rows = qfx["rows"]
        f16 = np.stack([np.frombuffer(bytes.fromhex(r["float16_hex"]), dtype="<f2") for r in rows])
        fp = {"version": 2, "marker": "legacy"}
        leaf = list(range(len(rows)))
        with tempfile.TemporaryDirectory() as tmp:
            d = Path(tmp)
            np.save(d / MV.MESSAGE_VECS_NAME, f16)
            np.save(d / MV.MESSAGE_LEAF_NAME, np.asarray(leaf, dtype=np.int32))
            (d / MV.MESSAGE_META_NAME).write_text(json.dumps({"fingerprint": fp}), encoding="utf-8")
            got, _ = MV.load_message_vectors(d, fingerprint=fp, leaf_rows=leaf)
        for i, r in enumerate(rows):
            want_scale = np.frombuffer(bytes.fromhex(r["scale_hex"]), dtype="<f4")[0]
            want = np.array(r["q"], dtype=np.float32) * want_scale
            with self.subTest(row=r["note"]):
                self.assertEqual(got[i].dtype, np.float32)
                self.assertTrue(np.array_equal(got[i], want), (got[i].tolist(), want.tolist()))

    def test_eligibility_and_meaningful_runes(self) -> None:
        for row in self.fx["eligibility"]:
            text = row.get("text", "") + "".join(chr(int(h, 16)) for h in row.get("codepoints", []))
            with self.subTest(text=text, note=row.get("note")):
                self.assertEqual(MV.meaningful_runes(text), row["meaningful_runes"])
                self.assertEqual(MV.message_eligible(text), row["eligible"])

    def test_projection_onto_every_leaf_in_canonical_order(self) -> None:
        pr = self.fx["projection"]
        names = [leaf["name"] for leaf in pr["leaves"]]
        vectors = pr["observation_vectors"]
        pairs = MV.project_message_rows([leaf["observations"] for leaf in pr["leaves"]], vectors.__contains__)
        self.assertEqual([[oid, names[li]] for oid, li in pairs], pr["expected_pairs"])
        # one row per (message, leaf) pair, built from one vector per observation
        rows = _norm(np.array([vectors[oid] for oid, _ in pairs], dtype=np.float32))
        q = _norm(np.array(pr["query"], dtype=np.float32))
        for case in pr["cases"]:
            with self.subTest(case=case["name"]):
                allowed = None if case["allowed"] is None else set(case["allowed"])
                want = case["expected"]
                ids, scores, scored = MV.message_dense_run(
                    rows, [li for _, li in pairs], names, q, allowed_ids=allowed, limit=case["limit"]
                )
                self.assertEqual(ids, want["message_dense_ids"])
                self.assertEqual(scored, want["messages_scored"])
                self.assertEqual(set(scores), set(want["message_dense_scores"]))
                for k, v in want["message_dense_scores"].items():
                    self.assertAlmostEqual(scores[k], v, delta=1e-4)

    def test_select_rows_follows_the_same_projection(self) -> None:
        pr = self.fx["projection"]
        messages = [
            {"observation_id": oid, "actor_label": "x", "content": "가나다라마바사"}
            for oid in sorted(pr["observation_vectors"])
        ] + [{"observation_id": "o9", "actor_label": "x", "content": "ㅋㅋ"}]
        leaves = [{"observation_ids": leaf["observations"]} for leaf in pr["leaves"]]
        observations, pairs = MV.select_message_rows(messages, leaves)
        names = [leaf["name"] for leaf in pr["leaves"]]
        self.assertEqual([[oid, names[li]] for oid, li in pairs], pr["expected_pairs"])
        self.assertEqual(len({oid for oid, _ in observations}), len(observations))  # embedded once each
        self.assertEqual([oid for oid, _ in observations], ["o3", "o1", "o2", "o4", "o5"])  # first appearance

    def test_source_family(self) -> None:
        for name, want in self.fx["source_family"].items():
            with self.subTest(source=name):
                self.assertEqual(source_family(name), want)

    def test_message_run_and_fusion(self) -> None:
        fx = self.fx
        names = fx["leaves"]
        leaf_vecs = _norm(np.array(fx["leaf_vectors"], dtype=np.float32))
        msg_vecs = _norm(np.array([m["vec"] for m in fx["message_vectors"]], dtype=np.float32))
        leaf_of = [names.index(m["leaf"]) for m in fx["message_vectors"]]
        q = _norm(np.array(fx["query"], dtype=np.float32))
        for case in fx["cases"]:
            with self.subTest(case=case["name"]):
                allowed = None if case["allowed"] is None else set(case["allowed"])
                want = case["expected"]
                ids, scores, scored = MV.message_dense_run(
                    msg_vecs, leaf_of, names, q, allowed_ids=allowed, limit=case["limit"]
                )
                self.assertEqual(ids, want["message_dense_ids"])
                self.assertEqual(scored, want["messages_scored"])
                self.assertEqual(set(scores), set(want["message_dense_scores"]))
                for k, v in want["message_dense_scores"].items():
                    self.assertAlmostEqual(scores[k], v, delta=1e-4)
                base = [d for d, _ in E._dense_hits(leaf_vecs, names, q, allowed_ids=allowed, limit=len(names))]
                self.assertEqual(base, want["leaf_dense_ids"])
                fused = fuse([("leaf_dense", base, 1.0), ("message_dense", ids, case["rrf_weight"])])
                self.assertEqual(fused.ids(), want["fused_ids"])
                for cand in fused.ordered:
                    self.assertAlmostEqual(cand.score, want["fused_scores"][cand.doc_id], delta=1e-7)

    def test_guarantee_pool_claims_the_message_run_last(self) -> None:
        for case in self.fx["guarantee_pool"]:
            with self.subTest(case=case["name"]):
                fusion = FusionResult(
                    ordered=[Candidate(doc_id=i) for i in case["ordered"]], weights={}, runs=case["runs"]
                )
                _ids, extras, by_source = guarantee_pool(
                    fusion, pool=case["pool"], per_source_min=case["per_source_min"],
                    reserve_max_frac=case["reserve_max_frac"],
                )
                self.assertEqual(extras, case["expected"]["guaranteed"])
                self.assertEqual(by_source, case["expected"]["by_source"])


class ConfigTests(unittest.TestCase):
    def test_defaults_are_off(self) -> None:
        self.assertEqual(DEFAULTS["message_dense"], {"enabled": False, "limit": 200, "rrf_weight": 0.1})
        self.assertEqual(MV.MESSAGE_RRF_WEIGHT, DEFAULTS["message_dense"]["rrf_weight"])
        self.assertFalse(retrieval_config({})["message_dense"]["enabled"])
        merged = retrieval_config({"retrieval": {"message_dense": {"enabled": True}}})["message_dense"]
        self.assertEqual((merged["enabled"], merged["limit"], merged["rrf_weight"]), (True, 200, 0.1))


def _with_messages(index: E.IndexBundle, *, enabled: bool, limit: int = 200) -> E.IndexBundle:
    """Attach one synthetic vector per message of each leaf (deterministic, 6 dims)."""

    rows, leaf_rows = [], []
    for li, leaf in enumerate(index.leaves):
        for k, _oid in enumerate(leaf["observation_ids"]):
            rows.append(_norm(np.random.default_rng(1000 + li * 17 + k).standard_normal(6).astype(np.float32)))
            leaf_rows.append(li)
    index.message_vectors = np.array(rows, dtype=np.float32)
    index.message_leaf = np.array(leaf_rows, dtype=np.int64)
    index.cfg["retrieval"] = {"message_dense": {"enabled": enabled, "limit": limit}}
    return index


def _run(index: E.IndexBundle, **kw):
    q = _unit(1)
    return E._generate_candidates(
        index, _FakeClient(q), "점심 뭐 먹을까", q_vec=q, intent={}, scope=kw.pop("scope", None),
        use_summary=True, pool=40, lex_lim=200, den_lim=150, **kw,
    )


def _dump(fusion: FusionResult, trace: dict) -> str:
    return json.dumps(
        {"ids": fusion.ids(), "prov": [c.to_trace() for c in fusion.ordered], "runs": fusion.runs, "trace": trace},
        ensure_ascii=False, sort_keys=True, default=str,
    )


class EngineIntegrationTests(unittest.TestCase):
    def test_off_is_identical_with_or_without_vectors(self) -> None:
        plain = _run(build_index(seed=3))
        for retrieval in ({"message_dense": {"enabled": False}}, {}):
            index = _with_messages(build_index(seed=3), enabled=False)
            index.cfg["retrieval"] = retrieval
            fusion, _a, trace, _m = _run(index)
            self.assertNotIn("message_dense", trace)
            self.assertNotIn("message_dense", trace["retrievers_used"])
            # elapsed-time fields are not part of the comparison
            self.assertEqual(_dump(fusion, trace), _dump(plain[0], plain[2]))

    def test_enabled_without_vectors_is_identical(self) -> None:
        plain = _run(build_index(seed=3))
        index = build_index(seed=3)
        index.cfg["retrieval"] = {"message_dense": {"enabled": True}}
        fusion, _a, trace, _m = _run(index)
        self.assertNotIn("message_dense", trace)
        self.assertEqual(_dump(fusion, trace), _dump(plain[0], plain[2]))

    def test_enabled_adds_one_run_after_the_fusion(self) -> None:
        base_fusion, _a, _t, _m = _run(build_index(seed=3))
        index = _with_messages(build_index(seed=3), enabled=True)
        fusion, _a, trace, _m = _run(index)
        md = trace["message_dense"]
        self.assertEqual(md["dims"], 6)
        self.assertGreater(md["leaves"], 0)
        self.assertEqual(md["messages_scored"], len(index.message_vectors))
        self.assertIn("message_dense", trace["retrievers_used"])
        self.assertIn("message_dense", fusion.runs)
        for name, ids in base_fusion.runs.items():  # every earlier retriever's run is kept as it was
            self.assertEqual(fusion.runs[name], ids)
        cand = next(c for c in fusion.ordered if "message_dense" in c.sources)
        self.assertIsNotNone(cand.raw.get("message_dense"))
        self.assertIn("dense", cand.families)

    def test_scope_is_respected(self) -> None:
        index = _with_messages(build_index(seed=3), enabled=True)
        old = {d["doc_id"] for d in index.leaves if d["source_room_id"] == ROOM_OLD}
        fusion, _a, _t, _m = _run(index, scope=old)
        self.assertTrue(set(fusion.runs["message_dense"]) <= old)
        self.assertTrue(set(fusion.ids()) <= old)

    def test_recovery_scales_the_run_like_dense(self) -> None:
        index = _with_messages(build_index(seed=3), enabled=True, limit=4)
        self.assertEqual(_run(index)[2]["message_dense"]["leaves"], 4)
        self.assertEqual(_run(index, limit_scale=1.5)[2]["message_dense"]["leaves"], 6)
        # the recovery attempt of retrieve() passes recovery.limit_scale on
        self.assertIn("limit_scale=scale,", Path(E.__file__).read_text(encoding="utf-8"))

    def test_message_run_weight_moves_the_order(self) -> None:
        heavy = _with_messages(build_index(seed=3), enabled=True)
        heavy.cfg["retrieval"]["message_dense"]["rrf_weight"] = 50.0
        light = _with_messages(build_index(seed=3), enabled=True)
        light.cfg["retrieval"]["message_dense"]["rrf_weight"] = 0.01
        fusion = _run(heavy)[0]
        self.assertEqual(fusion.ids()[0], fusion.runs["message_dense"][0])
        self.assertNotEqual(fusion.ids(), _run(light)[0].ids())


class _Client:
    embed_model = "test-embed"


class _CostClient:
    """A client whose spend is whatever the fake embedding adds to it."""

    embed_model = "test-embed"

    def __init__(self) -> None:
        self.usage = mock.Mock(cost_usd=0.0)
        self.usage.summary.return_value = "calls=1"


def _embedder(calls: list[int], dims: int = 12, spy=None):  # noqa: ANN001
    def fake_embed(client, texts, *, name, final_path, resume=True, batch_size=32):  # noqa: ANN001
        calls.append(len(texts))
        if spy is not None:
            spy(client, texts)
        return np.random.default_rng(len(texts)).standard_normal((len(texts), dims)).astype(np.float32)

    return fake_embed


class ArtifactTests(unittest.TestCase):
    def _env(self, tmp: str):
        d = Path(tmp)
        (d / "ckpt").mkdir()
        return d, [
            mock.patch.object(E, "DATA", d),
            mock.patch.object(E, "CKPT_DIR", d / "ckpt"),
            mock.patch.object(E, "META_PATH", d / "meta.json"),
            mock.patch.object(E, "LEDGER_PATH", d / "ledger.json"),
            mock.patch.object(E, "MESSAGES_PATH", d / "messages.json"),
            mock.patch.object(E, "_report_prepare"),
        ]

    def test_select_rows_skips_short_messages_and_projects_onto_every_leaf(self) -> None:
        messages = [
            {"observation_id": "a", "actor_label": "x", "content": "ㅋㅋㅋㅋㅋㅋㅋ"},
            {"observation_id": "b", "actor_label": "y", "content": "  내일 몇 시에 만나?  "},
            {"observation_id": "c", "actor_label": "x", "content": "ok"},
        ]
        leaves = [{"observation_ids": ["a", "b"]}, {"observation_ids": ["c", "b"]}]
        observations, pairs = MV.select_message_rows(messages, leaves)
        self.assertEqual(observations, [("b", "y: 내일 몇 시에 만나?")])  # embedded once
        self.assertEqual(pairs, [("b", 0), ("b", 1)])  # a message in two leaves is a row for each

    def test_row_order_is_leaf_order_then_message_order_not_id_order(self) -> None:
        messages = [{"observation_id": oid, "actor_label": "x", "content": "가나다라마바사"} for oid in ("a1", "a2", "a3")]
        leaves = [{"observation_ids": ["a3", "a1"]}, {"observation_ids": ["a2"]}]
        observations, pairs = MV.select_message_rows(messages, leaves)
        self.assertEqual(pairs, [("a3", 0), ("a1", 0), ("a2", 1)])
        self.assertEqual([oid for oid, _ in observations], ["a3", "a1", "a2"])

    def test_meaningful_characters_are_letters_and_numbers_only(self) -> None:
        for text, runes in (
            ("\U0001FAE9" * 6, 0),  # assigned after Python's Unicode 15, a symbol in Go's: never a letter
            ("\u0378" * 6, 0),  # unassigned
            ("\ue000" * 6, 0),  # private use
            ("\u3131\u3134\u318e", 0),  # compatibility jamo
            ("\u00b9\u00b2\u2167", 3),  # superscript digits and a roman numeral are numbers
            ("e\u0301" * 3, 3),  # combining marks do not count
            ("\u1100\u1161\u11a8", 3),  # conjoining jamo are letters
        ):
            with self.subTest(text=ascii(text)):
                self.assertEqual(MV.meaningful_runes(text), runes)

    def _index(self):
        index = build_index(seed=1)
        cfg = copy.deepcopy(index.cfg)
        cfg["embed_dimension"] = 8
        cfg["embed_truncatable"] = True
        return index, cfg

    def test_load_ignores_missing_or_mismatched_files(self) -> None:
        fp = MV.message_fingerprint(
            embed_model="m", dims=4, observations=[("a", "t")], pairs=[("a", 0), ("a", 2)], leaf_ids=["l0", "l1", "l2"]
        )
        with tempfile.TemporaryDirectory() as tmp:
            d = Path(tmp)
            self.assertIsNone(MV.load_message_vectors(d, fingerprint=fp, leaf_rows=[0, 2]))
            MV.write_message_vectors(d, np.ones((2, 4), dtype=np.float32), [0, 2], meta={"fingerprint": fp})
            vecs, leaf = MV.load_message_vectors(d, fingerprint=fp, leaf_rows=[0, 2])
            self.assertEqual((vecs.shape, leaf.tolist()), ((2, 4), [0, 2]))
            self.assertIsNone(MV.load_message_vectors(d, fingerprint=fp, leaf_rows=[0, 1]))  # other projection
            self.assertIsNone(MV.load_message_vectors(d, fingerprint={**fp, "dims": 8}, leaf_rows=[0, 2]))
            (d / MV.MESSAGE_META_NAME).unlink()
            self.assertIsNone(MV.load_message_vectors(d, fingerprint=fp, leaf_rows=[0, 2]))  # no vouching metadata

    def test_prepare_embeds_once_and_reuses(self) -> None:
        index, cfg = self._index()
        calls: list[int] = []
        with tempfile.TemporaryDirectory() as tmp:
            d, patches = self._env(tmp)
            with mock.patch.object(E, "_embed_texts", _embedder(calls)):
                for p in patches:
                    p.start()
                try:
                    first = E.prepare_message_vectors(cfg, index.messages, index.leaves, _Client())
                    second = E.prepare_message_vectors(cfg, index.messages, index.leaves, _Client())
                    fp, _obs, pairs = E._message_fingerprint_for(cfg, "test-embed", index.messages, index.leaves)
                    vecs, leaf = MV.load_message_vectors(d, fingerprint=fp, leaf_rows=[li for _, li in pairs])
                finally:
                    mock.patch.stopall()
            self.assertEqual(first, {"message_vectors": len(vecs), "message_rows": len(vecs), "reused": False})
            self.assertEqual(second, {"message_vectors": len(vecs), "message_rows": len(vecs), "reused": True})
            self.assertEqual(len(calls), 1)
            self.assertEqual(vecs.shape[1], 8)  # cut to embed_dimension
            self.assertTrue(np.allclose(np.linalg.norm(vecs, axis=1), 1.0, atol=1e-2))  # re-normalised (int8 rows)
            self.assertEqual(len(leaf), len(vecs))

    def test_prepare_keeps_full_width_when_model_is_not_catalogued_mrl(self) -> None:
        messages = [{"observation_id": "m1", "actor_label": "x", "content": "가나다라마바사"}]
        leaves = [{"doc_id": "L0", "observation_ids": ["m1"]}]
        cfg = {"embed_dimension": 0, "embed_truncatable": False}
        calls: list[int] = []
        with tempfile.TemporaryDirectory() as tmp:
            d, patches = self._env(tmp)
            np.save(d / "leaf_vecs.npy", np.ones((1, 12), dtype=np.float16))
            with mock.patch.object(E, "_embed_texts", _embedder(calls, dims=12)):
                for p in patches:
                    p.start()
                try:
                    E.prepare_message_vectors(cfg, messages, leaves, _Client())
                    fp, _observations, pairs = E._message_fingerprint_for(cfg, "test-embed", messages, leaves)
                    vecs, _leaf = MV.load_message_vectors(d, fingerprint=fp, leaf_rows=[li for _, li in pairs])
                finally:
                    mock.patch.stopall()
        self.assertEqual(calls, [1])
        self.assertEqual(fp["dims"], 12)  # actual leaf width identifies the space
        self.assertEqual(vecs.shape, (1, 12))

    def test_missing_mrl_flag_uses_catalogue_and_explicit_flag_wins(self) -> None:
        messages = [{"observation_id": "m1", "actor_label": "x", "content": "synthetic message"}]
        leaves = [{"doc_id": "L0", "observation_ids": ["m1"]}]
        model = "  Provider/QWEN3-Embedding-8B:free  "
        with tempfile.TemporaryDirectory() as tmp, mock.patch.object(E, "DATA", Path(tmp)):
            np.save(Path(tmp) / "leaf_vecs.npy", np.ones((1, 1536), dtype=np.float16))
            for leaf_dims, want in [(0, 512), (256, 256), (512, 512), (1024, 512), (4096, 512)]:
                fp, _, _ = E._message_fingerprint_for({"embed_dimension": leaf_dims}, model, messages, leaves)
                self.assertEqual(fp["dims"], want)
            fp, _, _ = E._message_fingerprint_for({"embed_truncatable": False}, model, messages, leaves)
            self.assertEqual(fp["dims"], 1536)
            fp, _, _ = E._message_fingerprint_for({}, "unknown-model", messages, leaves)
            self.assertEqual(fp["dims"], 1536)
            fp, _, _ = E._message_fingerprint_for({"embed_truncatable": True}, "unknown-model", messages, leaves)
            self.assertEqual(fp["dims"], 512)

    def test_non_mrl_leaf_width_growth_and_shrink_rebuild_artifacts(self) -> None:
        messages = [{"observation_id": "m1", "actor_label": "x", "content": "synthetic message"}]
        leaves = [{"doc_id": "L0", "observation_ids": ["m1"]}]
        calls: list[int] = []
        with tempfile.TemporaryDirectory() as tmp:
            d, patches = self._env(tmp)
            for p in patches:
                p.start()
            try:
                for dims in [12, 24, 8]:
                    np.save(d / "leaf_vecs.npy", np.ones((1, dims), dtype=np.float16))
                    with mock.patch.object(E, "_embed_texts", _embedder(calls, dims=dims)):
                        first = E.prepare_message_vectors({}, messages, leaves, _Client())
                        second = E.prepare_message_vectors({}, messages, leaves, _Client())
                    self.assertFalse(first["reused"])
                    self.assertTrue(second["reused"])
                    fp, _, pairs = E._message_fingerprint_for({}, "test-embed", messages, leaves)
                    vecs, _ = MV.load_message_vectors(d, fingerprint=fp, leaf_rows=[li for _, li in pairs])
                    self.assertEqual(fp["dims"], dims)
                    self.assertEqual(vecs.shape, (1, dims))
            finally:
                mock.patch.stopall()
        self.assertEqual(calls, [1, 1, 1])

    def test_non_mrl_explicit_leaf_widths_produce_message_candidates(self) -> None:
        messages = [{"observation_id": "m1", "actor_label": "x", "content": "synthetic message"}]
        leaves = [{"doc_id": "L0", "observation_ids": ["m1"]}]
        for dims in [256, 1024, 4096]:
            with self.subTest(dims=dims), tempfile.TemporaryDirectory() as tmp:
                d, patches = self._env(tmp)
                np.save(d / "leaf_vecs.npy", np.ones((1, dims), dtype=np.float16))
                for p in patches:
                    p.start()
                try:
                    cfg = {"embed_dimension": dims}
                    with mock.patch.object(E, "_embed_texts", _embedder([], dims=6144)):
                        E.prepare_message_vectors(cfg, messages, leaves, _Client())
                    fp, _, pairs = E._message_fingerprint_for(cfg, "test-embed", messages, leaves)
                    vecs, leaf = MV.load_message_vectors(d, fingerprint=fp, leaf_rows=[li for _, li in pairs])
                    self.assertEqual(fp["dims"], dims)
                    self.assertEqual(vecs.shape, (1, dims))
                    ids, _, scored = MV.message_dense_run(vecs, leaf, ["L0"], np.ones(dims, dtype=np.float32), allowed_ids=None, limit=5)
                    self.assertEqual((ids, scored), (["L0"], 1))
                finally:
                    mock.patch.stopall()

    def test_non_mrl_shorter_leaf_width_wins_over_explicit_setting(self) -> None:
        messages = [{"observation_id": "m1", "actor_label": "x", "content": "synthetic message"}]
        leaves = [{"doc_id": "L0", "observation_ids": ["m1"]}]
        with tempfile.TemporaryDirectory() as tmp:
            d, patches = self._env(tmp)
            np.save(d / "leaf_vecs.npy", np.ones((1, 768), dtype=np.float16))
            for p in patches:
                p.start()
            try:
                cfg = {"embed_dimension": 1024, "embed_truncatable": False}
                with mock.patch.object(E, "_embed_texts", _embedder([], dims=768)):
                    E.prepare_message_vectors(cfg, messages, leaves, _Client())
                fp, _, pairs = E._message_fingerprint_for(cfg, "test-embed", messages, leaves)
                vecs, leaf = MV.load_message_vectors(d, fingerprint=fp, leaf_rows=[li for _, li in pairs])
                self.assertEqual(fp["dims"], 768)
                ids, _, scored = MV.message_dense_run(vecs, leaf, ["L0"], np.ones(768, dtype=np.float32), allowed_ids=None, limit=5)
                self.assertEqual((ids, scored), (["L0"], 1))
            finally:
                mock.patch.stopall()

    def test_explicit_flags_prepare_without_catalogue(self) -> None:
        messages = [{"observation_id": "m1", "actor_label": "x", "content": "synthetic message"}]
        leaves = [{"doc_id": "L0", "observation_ids": ["m1"]}]
        for flag, want in [(False, 768), (True, 512)]:
            with self.subTest(flag=flag), tempfile.TemporaryDirectory() as tmp:
                d, patches = self._env(tmp)
                np.save(d / "leaf_vecs.npy", np.ones((1, 768), dtype=np.float16))
                for p in patches:
                    p.start()
                try:
                    cfg = {"embed_truncatable": flag, "embed_dimension": 1024}
                    with mock.patch.object(E, "embed_full_width", side_effect=RuntimeError("missing hosts.json")) as classify, mock.patch.object(E, "_embed_texts", _embedder([], dims=768)):
                        report = E.prepare_message_vectors(cfg, messages, leaves, _Client())
                        fp, _, pairs = E._message_fingerprint_for(cfg, "test-embed", messages, leaves)
                        classify.assert_not_called()
                    vecs, _ = MV.load_message_vectors(d, fingerprint=fp, leaf_rows=[li for _, li in pairs])
                    self.assertEqual(report["message_vectors"], 1)
                    self.assertEqual(vecs.shape, (1, want))
                finally:
                    mock.patch.stopall()

    def test_non_mrl_waits_without_leaf_vectors(self) -> None:
        messages = [{"observation_id": "m1", "actor_label": "x", "content": "synthetic message"}]
        leaves = [{"doc_id": "L0", "observation_ids": ["m1"]}]
        with tempfile.TemporaryDirectory() as tmp, mock.patch.object(E, "DATA", Path(tmp)), mock.patch.object(E, "_embed_texts") as embed:
            report = E.prepare_message_vectors({}, messages, leaves, _Client())
            self.assertEqual(report["message_vectors"], 0)
            embed.assert_not_called()
            self.assertFalse((Path(tmp) / MV.MESSAGE_META_NAME).exists())

    def test_quantize_rows_matches_the_product_quantisation(self) -> None:
        v = np.array([[0.6, -0.8, 0.0, 0.0123], [0.0, 0.0, 0.0, 0.0], [-0.2, 0.2, 0.5, -0.5]], dtype=np.float32)
        q, scale = MV.quantize_rows(v)
        self.assertEqual(q.dtype, np.int8)
        self.assertEqual(scale.dtype, np.float32)
        # scale = largest |component| / 127; that component maps to +-127; rounding is half away from zero; zero row stays zero
        self.assertTrue(np.allclose(scale, [0.8 / 127, 0.0, 0.5 / 127]))
        self.assertEqual(q[0].tolist(), [95, -127, 0, 2])
        self.assertEqual(q[1].tolist(), [0, 0, 0, 0])
        self.assertEqual(q[2].tolist(), [-51, 51, 127, -127])
        back = q.astype(np.float32) * scale[:, None]
        self.assertLess(float(np.abs(back - v).max()), 0.005)

    def test_artifact_is_int8_with_scales_and_an_older_float16_file_still_loads(self) -> None:
        rng = np.random.default_rng(3)
        vecs = rng.normal(size=(5, 16)).astype(np.float32)
        vecs /= np.linalg.norm(vecs, axis=1, keepdims=True)
        fp = {"version": 2, "marker": "x"}
        leaf = [0, 0, 1, 1, 2]
        with tempfile.TemporaryDirectory() as tmp:
            d = Path(tmp)
            MV.write_message_vectors(d, vecs, leaf, meta={"fingerprint": fp})
            self.assertEqual(np.load(d / MV.MESSAGE_VECS_NAME).dtype, np.int8)
            self.assertEqual(np.load(d / MV.MESSAGE_SCALE_NAME).shape, (5,))
            got, got_leaf = MV.load_message_vectors(d, fingerprint=fp, leaf_rows=leaf)
            self.assertEqual(got.dtype, np.float32)
            self.assertGreater(float(np.min(np.sum(got * vecs, axis=1))), 0.999)
            self.assertEqual(got_leaf.tolist(), leaf)
            # an artifact written before the int8 format: float16 file, no scale file
            (d / MV.MESSAGE_SCALE_NAME).unlink()
            np.save(d / MV.MESSAGE_VECS_NAME, vecs.astype(np.float16))
            old, _ = MV.load_message_vectors(d, fingerprint=fp, leaf_rows=leaf)
            q16, s16 = MV.quantize_rows(vecs.astype(np.float16).astype(np.float32))
            self.assertTrue(np.array_equal(old, q16.astype(np.float32) * s16[:, None]))  # quantised like Go does
            self.assertTrue(np.allclose(old, vecs, atol=1e-2))
            # int8 rows without their scales are ignored, not fatal
            np.save(d / MV.MESSAGE_VECS_NAME, MV.quantize_rows(vecs)[0])
            self.assertIsNone(MV.load_message_vectors(d, fingerprint=fp, leaf_rows=leaf))

    def test_overlap_embeds_once_per_message_and_writes_a_row_per_leaf(self) -> None:
        messages = [{"observation_id": f"m{i}", "actor_label": "x", "content": f"가나다라마바사 {i}"} for i in range(6)]
        leaves = [
            {"doc_id": "L0", "observation_ids": ["m0", "m1", "m2", "m3"]},
            {"doc_id": "L1", "observation_ids": ["m2", "m3", "m4", "m5"]},
        ]
        calls: list[int] = []
        with tempfile.TemporaryDirectory() as tmp:
            d, patches = self._env(tmp)
            with mock.patch.object(E, "_embed_texts", _embedder(calls)):
                for p in patches:
                    p.start()
                try:
                    report = E.prepare_message_vectors({"embed_dimension": 8, "embed_truncatable": True}, messages, leaves, _Client())
                    leaf = np.load(d / MV.MESSAGE_LEAF_NAME)
                    vecs = np.load(d / MV.MESSAGE_VECS_NAME)
                finally:
                    mock.patch.stopall()
        self.assertEqual(calls, [6])  # six messages embedded, not eight (message, leaf) rows
        self.assertEqual((report["message_vectors"], report["message_rows"]), (6, 8))
        self.assertEqual(leaf.tolist(), [0, 0, 0, 0, 1, 1, 1, 1])
        self.assertTrue(np.allclose(vecs[2], vecs[4]) and np.allclose(vecs[3], vecs[5]))  # m2, m3 in both leaves

    def test_boundary_change_with_the_same_leaf_count_is_not_reused_or_loaded(self) -> None:
        messages = [{"observation_id": oid, "actor_label": "x", "content": "가나다라마바사"} for oid in ("A", "B", "C")]
        before = [{"doc_id": "L0", "observation_ids": ["A", "B"]}, {"doc_id": "L1", "observation_ids": ["C"]}]
        after = [{"doc_id": "L0", "observation_ids": ["A"]}, {"doc_id": "L1", "observation_ids": ["B", "C"]}]
        calls: list[int] = []
        cfg = {"embed_dimension": 8, "embed_truncatable": True}
        with tempfile.TemporaryDirectory() as tmp:
            d, patches = self._env(tmp)
            with mock.patch.object(E, "_embed_texts", _embedder(calls)):
                for p in patches:
                    p.start()
                try:
                    E.prepare_message_vectors(cfg, messages, before, _Client())
                    self.assertEqual(np.load(d / MV.MESSAGE_LEAF_NAME).tolist(), [0, 0, 1])
                    fp_after, _o, pairs_after = E._message_fingerprint_for(cfg, "test-embed", messages, after)
                    # the old artifacts are not accepted for the new boundaries
                    self.assertIsNone(
                        MV.load_message_vectors(d, fingerprint=fp_after, leaf_rows=[li for _, li in pairs_after])
                    )
                    again = E.prepare_message_vectors(cfg, messages, after, _Client())
                finally:
                    mock.patch.stopall()
            self.assertFalse(again["reused"])
            self.assertEqual(len(calls), 2)
            self.assertEqual(np.load(d / MV.MESSAGE_LEAF_NAME).tolist(), [0, 1, 1])  # the new projection

    def test_model_or_dimension_change_is_not_reused(self) -> None:
        index, cfg = self._index()
        calls: list[int] = []

        class Other:
            embed_model = "other-embed"

        with tempfile.TemporaryDirectory() as tmp:
            d, patches = self._env(tmp)
            with mock.patch.object(E, "_embed_texts", _embedder(calls)):
                for p in patches:
                    p.start()
                try:
                    E.prepare_message_vectors(cfg, index.messages, index.leaves, _Client())
                    other_model = E.prepare_message_vectors(cfg, index.messages, index.leaves, Other())
                    wider = E.prepare_message_vectors(
                        {**cfg, "embed_dimension": 6}, index.messages, index.leaves, Other()
                    )
                    fp_old, _o, pairs = E._message_fingerprint_for(cfg, "test-embed", index.messages, index.leaves)
                    stale = MV.load_message_vectors(d, fingerprint=fp_old, leaf_rows=[li for _, li in pairs])
                finally:
                    mock.patch.stopall()
        self.assertFalse(other_model["reused"])
        self.assertFalse(wider["reused"])
        self.assertEqual(len(calls), 3)
        self.assertIsNone(stale)  # the artifacts now describe another model and dimensionality

    def test_interrupted_run_resumes_only_under_the_same_fingerprint(self) -> None:
        messages = [{"observation_id": oid, "actor_label": "x", "content": "가나다라마바사"} for oid in ("A", "B", "C")]
        before = [{"doc_id": "L0", "observation_ids": ["A", "B"]}, {"doc_id": "L1", "observation_ids": ["C"]}]
        after = [{"doc_id": "L0", "observation_ids": ["A"]}, {"doc_id": "L1", "observation_ids": ["B", "C"]}]
        cfg = {"embed_dimension": 8, "embed_truncatable": True}
        seen: list[bool] = []
        with tempfile.TemporaryDirectory() as tmp:
            d, patches = self._env(tmp)
            ckpt = d / "ckpt"

            def spy(client, texts):  # noqa: ANN001
                seen.append((ckpt / "message_vecs.partial.npy").exists())

            with mock.patch.object(E, "_embed_texts", _embedder([], spy=spy)):
                for p in patches:
                    p.start()
                try:
                    # an earlier run was interrupted: its partial rows and fingerprint are on disk
                    fp_before, _o, _p = E._message_fingerprint_for(cfg, "test-embed", messages, before)
                    E._align_message_checkpoint(fp_before)
                    np.save(ckpt / "message_vecs.partial.npy", np.ones((2, 8), dtype=np.float32))
                    (ckpt / "message_embed.json").write_text("{}", encoding="utf-8")
                    # the same index: the partial rows are still there when embedding starts (resume)
                    E.prepare_message_vectors(cfg, messages, before, _Client())
                    self.assertEqual(seen, [True])
                    # interrupted again, then the leaf boundaries changed: nothing of it may be resumed
                    (d / MV.MESSAGE_VECS_NAME).unlink()
                    np.save(ckpt / "message_vecs.partial.npy", np.ones((2, 8), dtype=np.float32))
                    (ckpt / "message_embed.json").write_text("{}", encoding="utf-8")
                    E.prepare_message_vectors(cfg, messages, after, _Client())
                    self.assertEqual(seen, [True, False])
                    # a checkpoint of another model is dropped too
                    E._align_message_checkpoint(fp_before)
                    np.save(ckpt / "message_vecs.partial.npy", np.ones((2, 8), dtype=np.float32))
                    (d / MV.MESSAGE_VECS_NAME).unlink()
                    class Other:
                        embed_model = "other-embed"

                    E.prepare_message_vectors(cfg, messages, before, Other())
                    self.assertEqual(seen, [True, False, False])
                finally:
                    mock.patch.stopall()

    def _message_only_env(self, tmp: str):
        index, cfg = self._index()
        cfg["prepare_budget_usd"] = 1.0
        d, patches = self._env(tmp)
        for p in patches:
            p.start()
        (d / "messages.json").write_text(json.dumps(index.messages, ensure_ascii=False), encoding="utf-8")
        meta = {"prepare_cost_usd": 0.5, "prepare_usage": "first run", "index_fingerprint": "x"}
        (d / "meta.json").write_text(json.dumps(meta), encoding="utf-8")
        E.save_ledger(2.0)
        (d / "ckpt" / "leftover.json").write_text("{}", encoding="utf-8")
        return index, cfg, d, meta

    def test_message_only_prepare_settles_the_ledger_once(self) -> None:
        client = _CostClient()

        def spend(c, texts):  # noqa: ANN001
            c.usage.cost_usd += 0.0123

        with tempfile.TemporaryDirectory() as tmp:
            index, cfg, d, meta = self._message_only_env(tmp)
            try:
                with mock.patch.object(E, "make_client", return_value=client), \
                        mock.patch.object(E, "_embed_texts", _embedder([], spy=spend)):
                    E._prepare_messages_only(cfg, meta, index.leaves)
                ledger = json.loads((d / "ledger.json").read_text(encoding="utf-8"))
                saved = json.loads((d / "meta.json").read_text(encoding="utf-8"))
                self.assertAlmostEqual(ledger["spent_usd"], 2.0123, places=6)  # once, not twice
                self.assertAlmostEqual(saved["prepare_cost_usd"], 0.5123, places=6)  # on top of the first run
                self.assertEqual(saved["prepare_usage"], "first run")  # the first run's usage is not overwritten
                self.assertGreater(saved["message_vectors"], 0)
                self.assertFalse((d / "ckpt" / "leftover.json").exists())  # checkpoint cleared
                # a second pass has nothing to embed and spends nothing: the ledger does not move
                client.usage.cost_usd = 0.0
                with mock.patch.object(E, "make_client", return_value=client):
                    E._prepare_messages_only(cfg, saved, index.leaves)
                self.assertAlmostEqual(
                    json.loads((d / "ledger.json").read_text(encoding="utf-8"))["spent_usd"], 2.0123, places=6
                )
            finally:
                mock.patch.stopall()

    def test_message_only_prepare_books_the_spend_when_it_fails_and_keeps_the_checkpoint(self) -> None:
        client = _CostClient()

        def spend_then_fail(c, texts):  # noqa: ANN001
            c.usage.cost_usd += 0.004
            raise RuntimeError("budget")

        with tempfile.TemporaryDirectory() as tmp:
            index, cfg, d, meta = self._message_only_env(tmp)
            try:
                with mock.patch.object(E, "make_client", return_value=client), \
                        mock.patch.object(E, "_embed_texts", _embedder([], spy=spend_then_fail)):
                    with self.assertRaises(RuntimeError):
                        E._prepare_messages_only(cfg, meta, index.leaves)
                ledger = json.loads((d / "ledger.json").read_text(encoding="utf-8"))
                self.assertAlmostEqual(ledger["spent_usd"], 2.004, places=6)  # exactly once
                self.assertTrue((d / "ckpt" / "leftover.json").exists())  # a failed pass keeps its checkpoint
            finally:
                mock.patch.stopall()


if __name__ == "__main__":
    unittest.main()
