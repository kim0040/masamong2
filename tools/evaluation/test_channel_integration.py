"""Synthetic window -> scoped FTS -> independent evidence integration."""
import unittest

from channel_scope import ChannelScope, ChannelSpec, Observation
from evidence_metrics import evaluate_retrieval
from windows import WindowConfig, build_windows


class ChannelIntegrationTests(unittest.TestCase):
    def setUp(self):
        self.scope = ChannelScope()
        self.a = ChannelSpec('synthetic', 'discord', 'a').key
        self.b = ChannelSpec('synthetic', 'kakao', 'b').key
        self.c = ChannelSpec('synthetic', 'discord', 'c').key
        for key in (self.a, self.b, self.c):
            self.scope.register_channel(ChannelSpec(*key.identity))
        self.scope.create_view('active', 'synthetic', [self.a], include_outbound_links=True)
        self.scope.grant_access(self.a, self.a)
        # Interleaved platform streams; same display name is not an identity link.
        self.raw = [
            dict(observation_id='a1', platform='discord', collection_id='mixed', source_room_id='a', actor_label='같은이름', content='부산 여행 날짜는 토요일'),
            dict(observation_id='b1', platform='kakao', collection_id='mixed', source_room_id='b', actor_label='같은이름', content='부산 여행 장소는 해운대'),
            dict(observation_id='a2', platform='discord', collection_id='mixed', source_room_id='a', actor_label='같은이름', content='부산 여행 준비'),
            dict(observation_id='c1', platform='discord', collection_id='mixed', source_room_id='c', actor_label='같은이름', content='부산 여행 비공개 기록'),
        ]
        self.docs = build_windows(self.raw, WindowConfig(size=2))
        self.records = [Observation(d['doc_id'], 'synthetic', d['platform'], d['source_room_id'], d['body']) for d in self.docs]
        self.members = {d['doc_id']: d['observation_ids'] for d in self.docs}

    def search(self, **kwargs):
        return self.scope.scoped_search(self.records, '부산 여행', view_id='active', destination=self.a, **kwargs)

    def test_link_grant_unlink_and_evidence_completeness(self):
        self.scope.link_channels(self.a, self.b)
        # Link without a grant does not index B.
        initial = self.search()
        self.assertEqual(initial.indexed_count, 1)
        self.assertEqual({h.source_room_id for h in initial.hits}, {'a'})
        self.scope.grant_access(self.b, self.a)
        linked = self.search()
        self.assertEqual({h.source_room_id for h in linked.hits}, {'a', 'b'})
        gold = {
            'schema_version': 1, 'dataset_id': 'integration.synthetic', 'gold_kind': 'synthetic',
            'review_status': 'unreviewed', 'reviewer': None,
            'observations': [dict(observation_id=r['observation_id'], source_provenance_id='source:'+r['observation_id'], source_channel_id=r['platform']+':'+r['source_room_id'], text=r['content']) for r in self.raw],
            'questions': [dict(question_id='q', split='dev', question_split_group='episode', episode_group='episode', allowed_source_channel_ids=['discord:a','kakao:b'], required_evidence_groups=[dict(group_id='date', alternatives=['a1']),dict(group_id='place', alternatives=['b1'])], forbidden_observation_ids=['c1'], abstain_expected=False, review_status='unreviewed', reviewer=None)],
        }
        refs = [ref for hit in linked.hits for ref in self.members[hit.observation_id]]
        report = evaluate_retrieval(gold, {'q': refs}, k=10)
        self.assertEqual(report.complete_group_recovery_count, 1)
        self.assertEqual(report.forbidden_hit_count, 0)
        self.assertFalse(report.qualification_eligible)
        old = self.scope.snapshot('active', self.a)
        self.scope.unlink_channels(self.a, self.b)
        stale = self.scope.scoped_search(self.records, '부산 여행', snapshot=old)
        self.assertEqual(stale.state, 'invalidated')
        self.assertFalse(stale.hits)
        current = self.search()
        self.assertEqual({h.source_room_id for h in current.hits}, {'a'})
        refs = [ref for hit in current.hits for ref in self.members[hit.observation_id]]
        self.assertEqual(evaluate_retrieval(gold, {'q': refs}).complete_group_recovery_count, 0)

    def test_revoke_between_ranking_and_return(self):
        self.scope.link_channels(self.a, self.b)
        self.scope.grant_access(self.b, self.a)
        def revoke():
            self.scope.revoke_access(self.b, self.a)
            return self.scope.snapshot('active', self.a)
        result = self.search(policy_supplier=revoke)
        self.assertEqual(result.state, 'invalidated')
        self.assertFalse(result.hits)


if __name__ == '__main__':
    unittest.main()
