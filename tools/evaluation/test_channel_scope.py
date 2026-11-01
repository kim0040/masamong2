import unittest
from dataclasses import replace
from channel_scope import ChannelScope,ChannelSpec,Observation,ChannelScopeError

class ChannelScopeTests(unittest.TestCase):
    def setUp(self):
        self.s=ChannelScope()
        self.keys=[]
        for platform,room in [('discord','a'),('kakao','b'),('discord','c')]:
            spec=ChannelSpec('fixture',platform,room,name='same')
            self.s.register_channel(spec);self.keys.append(spec.key)
        self.a,self.b,self.c=self.keys
        self.s.create_view('v','fixture',[self.a],include_outbound_links=True)
        self.s.grant_access(self.a,self.a)
        self.records=[Observation(str(i),'fixture',k.platform,k.source_room_id,'우산') for i,k in enumerate(self.keys)]

    def test_directional_nontransitive_and_grant_independence(self):
        self.s.link_channels(self.a,self.b);self.s.link_channels(self.b,self.c)
        self.s.grant_access(self.c,self.a)
        r=self.s.scoped_search(self.records,'우산',view_id='v',destination=self.a)
        self.assertEqual(r.indexed_count,1)
        self.s.grant_access(self.b,self.a)
        r=self.s.scoped_search(self.records,'우산',view_id='v',destination=self.a)
        self.assertEqual({h.source_room_id for h in r.hits},{'a','b'})
        self.s.create_view('b','fixture',[self.b],include_outbound_links=True)
        self.assertNotIn(self.a,self.s.snapshot('b',self.b).view_channels)

    def test_forged_snapshot_rejected_before_index(self):
        snap=self.s.snapshot('v',self.a)
        forged=replace(snap,view_channels=(self.a,self.b),authorized_channels=(self.a,self.b))
        r=self.s.scoped_search(self.records,'우산',snapshot=forged,policy_supplier=lambda:forged)
        self.assertEqual(r.state,'invalidated');self.assertEqual(r.indexed_count,0);self.assertFalse(r.hits)

    def test_invalid_update_keeps_last_good(self):
        snap=self.s.snapshot('v',self.a)
        with self.assertRaises(ChannelScopeError): self.s.update_view('v','fixture',[])
        self.assertEqual(self.s.snapshot('v',self.a),snap)

    def test_wrong_workspace_and_cache_revocation(self):
        self.s.link_channels(self.a,self.b);self.s.grant_access(self.b,self.a)
        first=self.s.scoped_search(self.records,'우산',view_id='v',destination=self.a)
        cached=self.s.scoped_search(self.records,'우산',view_id='v',destination=self.a)
        self.assertTrue(cached.cache_hit)
        self.s.revoke_access(self.b,self.a)
        stale=self.s.scoped_search(self.records,'우산',snapshot=first.snapshot)
        self.assertEqual(stale.state,'invalidated')
        foreign=Observation('foreign','other','discord','a','우산')
        r=self.s.scoped_search([foreign],'우산',view_id='v',destination=self.a)
        self.assertEqual(r.indexed_count,0)

if __name__=='__main__': unittest.main()
