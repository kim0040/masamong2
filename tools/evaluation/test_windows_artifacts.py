import contextlib
import io
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
import numpy as np
from artifacts import *
from windows import WindowConfig, build_windows
import run_retrieval_pilot as pilot
import analyze_hybrid as hybrid


def rows(n=25):
    return [dict(observation_id=f'm{i}',platform='kakao',collection_id='fixture',source_room_id='a',content=f'부산 여행 {i}',actor_label='A') for i in range(n)]


class WindowsArtifactsTests(unittest.TestCase):
    def test_overlap_tail_interleaved_and_provenance(self):
        docs=build_windows(rows(),WindowConfig(size=20,overlap=4))
        self.assertEqual([d['n'] for d in docs],[20,9])
        self.assertEqual(len(set(docs[0]['observation_ids'])&set(docs[1]['observation_ids'])),4)
        mixed=rows(3)+[dict(r,platform='discord',observation_id='d'+r['observation_id']) for r in rows(3)]
        mixed=[mixed[i] for i in (0,3,1,4,2,5)]
        out=build_windows(mixed,WindowConfig(size=2))
        self.assertEqual(sum(d['n'] for d in out),6)
        self.assertEqual(len(out),4)
        self.assertEqual(len({d['doc_id'] for d in out}),4)
        self.assertEqual(len(build_windows(rows(20),WindowConfig(size=20,overlap=4))),1)

    def test_invalid_scope_and_config_rejected(self):
        for config in (WindowConfig(size=0),WindowConfig(size=20,overlap=20)):
            with self.assertRaises(ValueError): build_windows(rows(),config)
        with self.assertRaises(ValueError): build_windows([dict(rows(1)[0],source_room_id='')])
        with patch.dict('os.environ',{},clear=True):
            with self.assertRaises(ValueError): pilot.configured_rooms()

    def test_cache_rejects_same_length_text_order_model_and_corruption(self):
        with tempfile.TemporaryDirectory() as root:
            path=Path(root)/'v.npy'; recipe={'size':20}; provider='fixture'; model='m'
            fp=cache_input_fingerprint(['a','b'],provider=provider,model=model,recipe=recipe,kind='docs')
            save_array_cache(path,np.eye(2,dtype=np.float32),input_fingerprint=fp,provider=provider,model=model,recipe=recipe)
            self.assertEqual(load_array_cache(path,input_fingerprint=fp).shape,(2,2))
            for texts in (['c','d'],['b','a']):
                other=cache_input_fingerprint(texts,provider=provider,model=model,recipe=recipe,kind='docs')
                with self.assertRaises(ArtifactValidationError): load_array_cache(path,input_fingerprint=other)
            with self.assertRaises(ArtifactValidationError): load_array_cache(path,model='changed')
            path.write_bytes(b'truncated')
            with self.assertRaises(ArtifactValidationError): load_array_cache(path,input_fingerprint=fp)
            sidecar_path(path).unlink()
            with self.assertRaises(ArtifactValidationError): load_array_cache(path)

    def test_baseline_main_and_offline_analyzer_replay(self):
        class Fake:
            base='synthetic';llm_model='llm';embed_model='embed'
            class Usage:
                errors={}
                def summary(self): return 'synthetic calls only'
            usage=Usage()
            def chat(self,*a,**k): return json.dumps({'literal':'부산 여행','paraphrase':'여행 장소'})
            def embed(self,texts): return [[1.,0.] for _ in texts]
        docs=build_windows(rows(4),WindowConfig(size=2))
        with tempfile.TemporaryDirectory() as folder:
            root=Path(folder)
            with patch.object(pilot,'OUT',root),patch.object(pilot,'N_QUESTIONS',2),patch.object(pilot,'N_WINDOWS',2),patch.object(pilot,'WINDOW_SIZE',2),patch.object(pilot,'TAG','w2o0'),patch.object(pilot,'configured_rooms',return_value={'a'}),patch.object(pilot,'build_windows',return_value=docs),patch.object(pilot,'OpenRouter',return_value=Fake()),contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(pilot.main(),0)
            with patch.object(hybrid,'OUT',root),patch.object(hybrid,'TAG','w2o0'),patch.object(hybrid,'build_windows',return_value=docs) as builder,patch.dict('os.environ',{'WINDOW_SIZE':'50'},clear=True),contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(hybrid.main(),0)
                self.assertEqual(builder.call_args.args[3].size,2)
            with patch.object(hybrid,'OUT',root),patch.object(hybrid,'TAG','w2o0'),patch.object(hybrid,'build_windows',return_value=build_windows(rows(5),WindowConfig(size=2))):
                with self.assertRaises(ArtifactValidationError): hybrid.main()

if __name__=='__main__': unittest.main()
