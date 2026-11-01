"""Model roles are config-driven and guarded (format, role fit, index binding)."""

from __future__ import annotations

import unittest

from models import DEFAULTS, ModelConfigError, resolve_models, validate_models


class ResolveTests(unittest.TestCase):
    def test_defaults_and_role_inheritance(self) -> None:
        m = resolve_models({})
        self.assertEqual(m["response"], DEFAULTS["response"])
        self.assertEqual(m["planner"], m["response"])   # empty → follows llm
        self.assertEqual(m["summary"], m["response"])
        self.assertEqual(m["rerank"], DEFAULTS["rerank"])

    def test_config_beats_secrets_beats_default(self) -> None:
        m = resolve_models({"models": {"response": "openai/gpt-x"}}, {"EVAL_LLM_MODEL": "z/other", "EVAL_EMBED_MODEL": "openai/text-embedding-3-small"})
        self.assertEqual(m["response"], "openai/gpt-x")
        self.assertEqual(m["embed"], "openai/text-embedding-3-small")

    def test_legacy_llm_alias(self) -> None:
        self.assertEqual(resolve_models({"models": {"llm": "a/legacy"}})["response"], "a/legacy")

    def test_roles_match_contract(self) -> None:
        import json
        from tool_parity import _repo_root

        spec = json.loads((_repo_root() / "contracts" / "sandbox" / "tool-parity.json").read_text(encoding="utf-8"))
        from models import ROLES

        self.assertEqual(list(ROLES), spec["models"]["roles"])

    def test_legacy_rerank_model_key(self) -> None:
        self.assertEqual(resolve_models({"rerank_model": "cohere/rerank-v3.5"})["rerank"], "cohere/rerank-v3.5")

    def test_planner_can_differ_from_llm(self) -> None:
        m = resolve_models({"models": {"response": "a/big", "planner": "b/fast"}})
        self.assertEqual((m["response"], m["planner"], m["summary"]), ("a/big", "b/fast", "a/big"))


class ValidateTests(unittest.TestCase):
    def test_ok(self) -> None:
        validate_models(resolve_models({"models": {"rerank": "cohere/rerank-4-pro", "embed": "voyageai/voyage-embed-3"}}))

    def test_bad_format(self) -> None:
        with self.assertRaises(ModelConfigError):
            validate_models(resolve_models({"models": {"response": "gpt-4"}}))

    def test_role_mismatch(self) -> None:
        with self.assertRaises(ModelConfigError):
            validate_models(resolve_models({"models": {"embed": "z-ai/glm-5.3-flash"}}))
        with self.assertRaises(ModelConfigError):
            validate_models(resolve_models({"models": {"rerank": "qwen/qwen3-embedding-8b"}}))
        with self.assertRaises(ModelConfigError):
            validate_models(resolve_models({"models": {"planner": "voyageai/rerank-2.5-lite"}}))


class EngineIntegrationTests(unittest.TestCase):
    def test_configured_models_raises_playground_error(self) -> None:
        import engine as E

        with self.assertRaises(E.PlaygroundError):
            E.configured_models({"models": {"rerank": "not-a-model"}})
        m = E.configured_models({"models": {"rerank": "cohere/rerank-4-pro"}})
        self.assertEqual(m["rerank"], "cohere/rerank-4-pro")

    def test_lab_patch_rejects_bad_models(self) -> None:
        import lab

        saved = (lab.load_config, lab.save_config)
        lab.load_config = lambda: {"ranking": "auto"}
        lab.save_config = lambda cfg: None
        try:
            with self.assertRaises(lab.PlaygroundError):
                lab.patch_config({"models": {"embed": "z-ai/glm-5.3-flash"}})
            out = lab.patch_config({"models": {"response": "openai/gpt-x", "rerank": "cohere/rerank-4-pro"}})
            self.assertEqual(out["models"]["rerank"], "cohere/rerank-4-pro")
        finally:
            lab.load_config, lab.save_config = saved


if __name__ == "__main__":
    unittest.main()
