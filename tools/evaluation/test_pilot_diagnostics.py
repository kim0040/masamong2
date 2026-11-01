import contextlib,io,tempfile,unittest
from pathlib import Path
from unittest.mock import patch
import run_person_pilot as person
import run_identity_impact as identity
from analyze_real_queries import validate_labels
from windows import WindowConfig,build_windows

class DiagnosticsTests(unittest.TestCase):
    def test_explicit_channel_required_before_provider(self):
        for module in (person,identity):
            with patch.object(module,'ROOM',''),patch.object(module,'OpenRouter') as provider:
                with self.assertRaises(ValueError): module.main()
                provider.assert_not_called()
    def test_partial_duplicate_classification_rejected(self):
        for rows in ([{'i':1,'c':'person'}],[{'i':1,'c':'person'},{'i':1,'c':'episode'}]):
            with self.assertRaises(ValueError): validate_labels(rows,2)
        validate_labels([{'i':1,'c':'person'},{'i':2,'c':'episode'}],2)
    def test_person_and_identity_entrypoints_with_fake_provider(self):
        class Fake:
            base='synthetic';llm_model='llm';embed_model='embed'
            class Usage:
                def summary(self):return 'mock'
            usage=Usage()
            def embed(self,texts):return [[1.,0.] for _ in texts]
        raw=[dict(observation_id=f'm{i}',platform='discord',collection_id='discord',source_room_id='a',content='부산 여행',actor_label='합성',actor_account='fixture-account') for i in range(4)]
        docs=build_windows(raw,WindowConfig(size=20))
        with tempfile.TemporaryDirectory() as tmp:
            for module in (person,identity):
                with patch.object(module,'ROOM','a'),patch.object(module,'OUT',Path(tmp)/module.__name__),patch.object(module,'OpenRouter',return_value=Fake()),contextlib.redirect_stdout(io.StringIO()):
                    if module is person:
                        with patch.object(person,'MIN_MESSAGES',1),patch.object(person,'build_windows',return_value=docs):self.assertEqual(person.main(),0)
                    else:
                        with patch.object(identity,'MIN_ACCOUNT_MSGS',1),patch.object(identity,'bounded_windows',return_value=docs):self.assertEqual(identity.main(),0)

if __name__=='__main__': unittest.main()
