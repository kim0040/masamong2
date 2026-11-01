package application

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/memory/blocks"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/retrieval"
	"masamong/internal/memory/search"
)

func widthExecute(s *Service, name string, in map[string]any) action.Result {
	raw, _ := json.Marshal(in)
	return s.Execute(context.Background(), action.Request{Action: name, Input: raw, Locale: "ko"})
}

func widthConfig(t *testing.T, s *Service, model string, dims int) {
	t.Helper()
	r := s.mutateConfig(action.Request{}, func(d *config.Document) error {
		m := d.Models["embed"]
		m.Model, m.Dimensions = model, dims
		d.Models["embed"] = m
		return nil
	})
	if !r.OK {
		t.Fatal(r.Error)
	}
}

func widthRoom(t *testing.T, s *Service) blocks.Block {
	t.Helper()
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	q := []string{
		`INSERT INTO memory_rooms(room,collection,platform,updated_at,active_recipe) VALUES('kakao:width-fixture','fixture','kakao','2026-10-01T00:00:00Z',?)`,
		`INSERT INTO memory_observations(observation_id,platform,collection,source_room,room,native_message_id,actor_label,content,parsed_time,record_kind,imported_at) VALUES('width-observation','kakao','fixture','kakao:width-fixture','kakao:width-fixture','width-native','fixture','width fixture search','2026-10-01T00:00:00Z','message','2026-10-01T00:00:00Z')`,
		`INSERT INTO memory_leaves(leaf_id,room,recipe,first_observation_id,observation_ids,n,body,body_hash,speakers,closed,summary,embed_space,leaf_vec,updated_at) VALUES('width-leaf','kakao:width-fixture',?,'width-observation','["width-observation"]',1,'width fixture search','fixture-hash','["fixture"]',1,'',?,?,'2026-10-01T00:00:00Z')`,
	}
	v := make([]float32, 4096)
	v[0] = 1
	args := [][]any{{e.Settings.Recipe.Version()}, nil, {e.Settings.Recipe.Version(), e.EmbedSpace(), search.EncodeFloat16(v)}}
	for i, query := range q {
		if _, err := s.Store.DB.Exec(query, args[i]...); err != nil {
			t.Fatal(err)
		}
	}
	return s.dbBlocks()[0]
}

func TestEmbedWidthLowerKeepsBlockConnections(t *testing.T) {
	for _, path := range []string{"config.patch", "models.embed_width"} {
		t.Run(path, func(t *testing.T) {
			s := privacyService(t)
			widthConfig(t, s, "qwen/qwen3-embedding-8b", 4096)
			var embeds atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/embeddings") {
					_, _ = w.Write([]byte(`{"data":[]}`))
					return
				}
				embeds.Add(1)
				v := make([]float32, 4096)
				v[0] = 1
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": v}}, "usage": map[string]any{"total_tokens": 1}})
			}))
			defer srv.Close()
			t.Setenv("WIDTH_FIXTURE_KEY", "fixture-only")
			res := s.mutateConfig(action.Request{}, func(d *config.Document) error {
				p := d.Providers[d.Models["embed"].Provider]
				p.BaseURL, p.CredentialRef = srv.URL, "env:WIDTH_FIXTURE_KEY"
				d.Providers[d.Models["embed"].Provider] = p
				d.Guilds["width-guild"] = config.GuildSettings{Channels: map[string]config.ChannelPolicy{"width-channel": {}}}
				return nil
			})
			if !res.OK {
				t.Fatal(res.Error)
			}
			old := widthRoom(t, s)
			beforeEngine, err := s.MemoryEngine()
			if err != nil {
				t.Fatal(err)
			}
			messageSpace := beforeEngine.MessageSpace()
			old.PreparedAt, old.CostUSD, old.Hints = "2026-10-01T01:00:00Z", 0.05, map[string]any{"fixture": true}
			if err := s.savePreparedBlock(old); err != nil {
				t.Fatal(err)
			}
			preparedBefore, err := os.ReadFile(s.blockCatalogPath())
			if err != nil {
				t.Fatal(err)
			}
			declared := s.declaredDiscordBlocks()[0]
			if r := widthExecute(s, "channels.view.attach", map[string]any{"guild_id": "width-guild", "channel_id": "width-channel", "block_id": old.ID}); !r.OK {
				t.Fatal(r.Error)
			}
			before := s.Revision()
			in := map[string]any{"dimensions": 1024}
			if path == "config.patch" {
				in = map[string]any{"ops": []any{map[string]any{"path": []string{"models", "embed", "dimensions"}, "value": 1024}}}
			}
			if r := widthExecute(s, path, in); !r.OK {
				t.Fatal(r.Error)
			}
			if s.Revision() <= before {
				t.Fatal("width change did not record a configuration revision")
			}
			for _, b := range []blocks.Block{old, declared} {
				if got, ok := s.lookupBlock(b.ID); !ok || got.SourceRoom != b.SourceRoom {
					t.Error("previous block ID no longer resolves to its room")
				}
			}
			if !slices.Contains(s.channelRooms("width-guild", "width-channel"), old.SourceRoom) {
				t.Error("attached room dropped from channel scope")
			}
			if !slices.Contains(s.channelPolicy("width-guild", "width-channel").RAGView.Blocks, old.ID) {
				t.Error("width change rewrote the attached ID")
			}
			list := widthExecute(s, "memory.blocks.list", nil)
			if !list.OK {
				t.Fatal(list.Error)
			}
			found := false
			for _, b := range list.Output["blocks"].([]map[string]any) {
				if b["source_room"] == old.SourceRoom {
					found = true
					if b["status"] != "ready" || b["prepared_at"] != old.PreparedAt || b["cost_usd"] != old.CostUSD || b["hints"].(map[string]any)["fixture"] != true {
						t.Error("prepared status or preparation record lost after narrowing")
					}
				}
			}
			if !found {
				t.Fatal("prepared block missing from list")
			}
			for _, c := range s.channelDirectory() {
				for _, b := range c["attached"].([]map[string]any) {
					if b["stale"] != false {
						t.Error("channel directory calls an attached block stale")
					}
				}
			}
			e, err := s.MemoryEngine()
			if err != nil {
				t.Fatal(err)
			}
			if e.MessageSpace() != messageSpace {
				t.Fatal("narrowing changed the message-vector space")
			}
			if _, err := e.Reload(); err != nil {
				t.Fatal(err)
			}
			if embeds.Load() != 0 {
				t.Fatal("narrowing called the embedding provider")
			}
			result, err := e.Search(context.Background(), engine.SearchRequest{Query: "width fixture", Rooms: s.channelRooms("width-guild", "width-channel"), Ranking: retrieval.Hybrid, TopK: 5})
			if err != nil || len(result.Hits) == 0 || embeds.Load() != 1 {
				t.Fatalf("search hits=%d embedding calls=%d error=%v", len(result.Hits), embeds.Load(), err)
			}
			var space string
			var bytes int
			if err := s.Store.DB.QueryRow(`SELECT embed_space,length(leaf_vec) FROM memory_leaves WHERE leaf_id='width-leaf'`).Scan(&space, &bytes); err != nil || space != e.EmbedSpace() || bytes != 2048 {
				t.Fatalf("stored width not adapted: bytes=%d error=%v", bytes, err)
			}
			preparedAfter, err := os.ReadFile(s.blockCatalogPath())
			if err != nil || string(preparedAfter) != string(preparedBefore) {
				t.Fatal("width change rewrote preparation records")
			}
			root := s.DataRoot
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(root, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			for _, b := range []blocks.Block{old, declared} {
				if got, ok := s.lookupBlock(b.ID); !ok || got.SourceRoom != b.SourceRoom {
					t.Fatal("width aliases were lost after restart")
				}
			}
			shown := widthExecute(s, "memory.blocks.show", map[string]any{"block_id": old.ID})
			if !shown.OK || shown.Output["block"].(map[string]any)["prepared_at"] != old.PreparedAt {
				t.Fatal("old ID cannot show the retained preparation record")
			}
			current := s.dbBlocks()[0]
			if r := widthExecute(s, "channels.view.detach", map[string]any{"guild_id": "width-guild", "channel_id": "width-channel", "block_id": current.ID}); !r.OK {
				t.Fatal(r.Error)
			}
			if slices.Contains(s.channelRooms("width-guild", "width-channel"), old.SourceRoom) {
				t.Fatal("current block ID cannot detach its previous width's reference")
			}
		})
	}
}

func TestEmbedWidthAction(t *testing.T) {
	for _, dims := range []int{1024, 2048} {
		t.Run("narrow-"+strconv.Itoa(dims), func(t *testing.T) {
			s := privacyService(t)
			widthConfig(t, s, "qwen/qwen3-embedding-8b", 4096)
			widthRoom(t, s)
			r := widthExecute(s, "models.embed_width", map[string]any{"dimensions": dims, "reindex": true})
			if !r.OK || r.Output["dimensions"] != dims || r.Output["previous"] != 4096 || r.Output["adapted"] != true || r.Output["reembed"] != false || r.Output["reindex_jobs"] != nil || s.Doc().Models["embed"].Dimensions != dims {
				t.Fatal("narrowing must apply without confirmation or reindex jobs")
			}
		})
	}
	for _, dims := range []int{0, 1024, 2048} {
		t.Run("widen-"+strconv.Itoa(dims), func(t *testing.T) {
			s := privacyService(t)
			widthConfig(t, s, "qwen/qwen3-embedding-8b", 512)
			widthRoom(t, s)
			before := s.Revision()
			r := widthExecute(s, "models.embed_width", map[string]any{"dimensions": dims, "reindex": true})
			if r.OK || r.Error == nil || r.Error.Code != errcode.InvalidInput || r.Error.MessageKey != EmbedReindexRequiredKey {
				t.Fatal("widening an indexed model must ask for include_embed")
			}
			estimate, ok := r.Error.Args["reindex"].(map[string]any)
			if !ok || estimate["rooms"] != 1 || estimate["leaves"] != 1 || estimate["tokens"].(int64) <= 0 || s.Revision() != before || s.Doc().Models["embed"].Dimensions != 512 {
				t.Fatal("refusal must include the estimate and preserve configuration")
			}
			r = widthExecute(s, "models.embed_width", map[string]any{"dimensions": dims, "include_embed": true})
			if !r.OK || r.Output["dimensions"] != dims || r.Output["previous"] != 512 || r.Output["reembed"] != true || s.Doc().Models["embed"].Dimensions != dims {
				t.Fatal("confirmed widening was not applied")
			}
		})
	}
	t.Run("widen-without-index", func(t *testing.T) {
		s := privacyService(t)
		widthConfig(t, s, "qwen/qwen3-embedding-8b", 512)
		r := widthExecute(s, "models.embed_width", map[string]any{"dimensions": 1024})
		if !r.OK || r.Output["reembed"] != true {
			t.Fatal("widening without an index needs no confirmation")
		}
	})
	t.Run("reindex-jobs", func(t *testing.T) {
		s := privacyService(t)
		t.Setenv("MASAMONG_OPENROUTER_KEY", "")
		widthConfig(t, s, "qwen/qwen3-embedding-8b", 512)
		widthRoom(t, s)
		r := widthExecute(s, "models.embed_width", map[string]any{"dimensions": 1024, "include_embed": true, "reindex": true})
		jobs, ok := r.Output["reindex_jobs"].([]string)
		if !r.OK || !ok || len(jobs) != 1 {
			t.Fatal("confirmed reindex must queue the existing prepare jobs")
		}
	})
}

func TestEmbedWidthValidationAndNoop(t *testing.T) {
	s := privacyService(t)
	widthConfig(t, s, "qwen/qwen3-embedding-8b", 4096)
	for _, dims := range []any{-1, 1, 128, 256, 512, 768, 1536, 3072, 4096, 8192, "bad", 1024.5, true, nil} {
		before := s.Revision()
		r := widthExecute(s, "models.embed_width", map[string]any{"dimensions": dims})
		if r.OK || r.Error == nil || r.Error.Code != errcode.InvalidInput || s.Revision() != before {
			t.Fatalf("invalid width accepted or changed revision: %v", dims)
		}
		if dims == 256 || dims == 512 {
			if r.Error.MessageKey != "models.embed_width_invalid" || !strings.Contains(r.Error.Message, "1024") {
				t.Fatal("widths below 1024 need the localized message-vector explanation")
			}
		}
	}
	if r := widthExecute(s, "models.embed_width", nil); r.OK {
		t.Fatal("missing dimensions accepted")
	}
	for _, current := range []int{0, 4096, 1024} {
		widthConfig(t, s, "qwen/qwen3-embedding-8b", current)
		dims := current
		if current == 4096 {
			dims = 0 // legacy full space and native space have the same actual width
		}
		before := s.Revision()
		r := widthExecute(s, "models.embed_width", map[string]any{"dimensions": dims, "reindex": true})
		if !r.OK || r.Output["changed"] != false || r.Output["adapted"] != false || r.Output["reembed"] != false || s.Revision() != before || s.Doc().Models["embed"].Dimensions != current {
			t.Fatal("same effective width must leave configuration unchanged")
		}
	}
	widthConfig(t, s, "qwen/qwen3-embedding-0.6b", 0)
	if r := widthExecute(s, "models.embed_width", map[string]any{"dimensions": 1024}); r.OK {
		t.Fatal("native width must use 0; explicit narrow width must be smaller")
	}
	widthConfig(t, s, "unknown-fixture-model", 0)
	for _, dims := range []int{1024, 2048} {
		r := widthExecute(s, "models.embed_width", map[string]any{"dimensions": dims})
		if r.OK || r.Error == nil || r.Error.MessageKey != "models.embed_width_unsupported" || r.Error.Message == "models.embed_width_unsupported" {
			t.Fatal("unknown models must refuse truncation with a localized stable key")
		}
	}
	if r := widthExecute(s, "models.embed_width", map[string]any{"dimensions": 0}); !r.OK {
		t.Fatal("unknown model's native width must be allowed")
	}
}

func TestEmbedWidthReadMetadata(t *testing.T) {
	s := privacyService(t)
	for _, tc := range []struct {
		model                 string
		dims, effective, full int
		truncatable           bool
		widths                []int
	}{
		{"qwen/qwen3-embedding-8b", 4096, 4096, 4096, true, []int{0, 1024, 2048}},
		{"qwen/qwen3-embedding-8b", 0, 4096, 4096, true, []int{0, 1024, 2048}},
		{"openai/text-embedding-3-large", 1024, 1024, 3072, true, []int{0, 1024, 2048}},
		{"openai/text-embedding-3-large", 4096, 3072, 3072, true, []int{0, 1024, 2048}},
		{"text-embedding-3-small", 0, 1536, 1536, true, []int{0, 1024}},
		{"qwen/qwen3-embedding-0.6b", 0, 1024, 1024, true, []int{0}},
		{"unknown-fixture-model", 0, 0, 0, false, []int{0}},
	} {
		widthConfig(t, s, tc.model, tc.dims)
		r := widthExecute(s, "providers.list", nil)
		m, ok := r.Output["embed_width"].(map[string]any)
		recommended := 0
		if slices.Contains(tc.widths, 1024) {
			recommended = 1024
		}
		if !r.OK || !ok || m["dimensions"] != tc.dims || m["effective"] != tc.effective || m["full_width"] != tc.full || m["truncatable"] != tc.truncatable || m["recommended"] != recommended || !slices.Equal(m["widths"].([]int), tc.widths) {
			t.Fatal("providers.list missing current effective/full width metadata")
		}
		for _, dims := range tc.widths {
			if r := widthExecute(s, "models.embed_width", map[string]any{"dimensions": dims, "include_embed": true}); !r.OK {
				t.Fatalf("advertised width %d rejected for %s", dims, tc.model)
			}
		}
	}
}

func TestEmbedWidthAliasesKeepModelAndRecipeIdentity(t *testing.T) {
	s := privacyService(t)
	widthConfig(t, s, "qwen/qwen3-embedding-8b", 4096)
	old := widthRoom(t, s)
	widthConfig(t, s, "qwen/qwen3-embedding-8b", 1024)
	current, ok := s.lookupBlock(old.ID)
	if !ok || current.SourceRoom != old.SourceRoom || current.ID == old.ID {
		t.Fatal("an indexed room without a prepared record lost its old width's ID")
	}
	if !sameBlockPreparation(old, current) {
		t.Fatal("same model's width change lost preparation identity")
	}
	otherRecipe := old
	otherRecipe.SegmentationVersion = "different-fixture-recipe"
	if _, ok := s.lookupBlock(blocks.BlockID(old.Collection, old.SourceRoom, otherRecipe.SegmentationVersion, old.EmbeddingSpace)); ok || sameBlockPreparation(otherRecipe, current) {
		t.Fatal("width aliases must not cross recipes")
	}
	widthConfig(t, s, "openai/text-embedding-3-large", 1024)
	if _, ok := s.lookupBlock(old.ID); ok || sameBlockPreparation(old, s.dbBlocks()[0]) {
		t.Fatal("width aliases must not cross embedding models")
	}
}

func TestEmbedWidthAliasesForLegacyPatchWidths(t *testing.T) {
	s := privacyService(t)
	widthConfig(t, s, "qwen/qwen3-embedding-8b", 1536)
	widthRoom(t, s)
	if r := widthExecute(s, "channels.configure", map[string]any{"guild_id": "width-guild", "channel_id": "width-channel", "capture": false}); !r.OK {
		t.Fatal(r.Error)
	}
	for _, dims := range []int{1536, 768} {
		widthConfig(t, s, "qwen/qwen3-embedding-8b", dims)
		old, declared := s.dbBlocks()[0], s.declaredDiscordBlocks()[0]
		r := widthExecute(s, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []string{"models", "embed", "dimensions"}, "value": 1024}}})
		if !r.OK {
			t.Fatal(r.Error)
		}
		for _, b := range []blocks.Block{old, declared} {
			if got, ok := s.lookupBlock(b.ID); !ok || got.SourceRoom != b.SourceRoom {
				t.Fatal("legacy config.patch width lost a stored or declared block ID")
			}
		}
	}
	root := s.DataRoot
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	current := s.dbBlocks()[0]
	id := blocks.BlockID(current.Collection, current.SourceRoom, current.SegmentationVersion, "qwen/qwen3-embedding-8b:1536:l2")
	if _, ok := s.lookupBlock(id); !ok {
		t.Fatal("fixed legacy width alias did not survive restart")
	}
}

func TestEmbedWidthNativeFromOversizedSettingNeedsReindex(t *testing.T) {
	s := privacyService(t)
	widthConfig(t, s, "qwen/qwen3-embedding-4b", 3000)
	widthRoom(t, s)
	before := s.Revision()
	r := widthExecute(s, "models.embed_width", map[string]any{"dimensions": 0})
	if r.OK || r.Error == nil || r.Error.MessageKey != EmbedReindexRequiredKey || s.Revision() != before {
		t.Fatal("oversized space to native width must require confirmation")
	}
	r = widthExecute(s, "models.embed_width", map[string]any{"dimensions": 0, "include_embed": true})
	if !r.OK || r.Output["reembed"] != true || r.Output["adapted"] != false || s.Doc().Models["embed"].Dimensions != 0 {
		t.Fatal("confirmed native space must require reembedding, not adaptation")
	}
}

func TestBlockLookupRejectsLabelsBeforeReadingStorage(t *testing.T) {
	s := &Service{}
	for _, id := range []string{"", "fixture label", "kakao:fixture", "0123456789abcde", "0123456789abcdef0", "0123456789abcdeg", "0123456789abcde한"} {
		if _, ok := s.lookupBlock(id); ok {
			t.Fatal("invalid ID must miss without storage")
		}
	}
}

func TestEmbedWidthAliasesUseFixedCandidatesAndInvalidate(t *testing.T) {
	s := privacyService(t)
	widthConfig(t, s, "qwen/qwen3-embedding-8b", 1024)
	b := widthRoom(t, s)
	// Lookup no longer depends on the config revision table, even on a miss.
	if _, err := s.Store.DB.Exec(`ALTER TABLE config_revisions RENAME TO width_fixture_revisions`); err != nil {
		t.Fatal(err)
	}
	for _, dims := range []int{0, 256, 512, 768, 1024, 1536, 2048, 2560, 3072, 4096} {
		id := blocks.BlockID(b.Collection, b.SourceRoom, b.SegmentationVersion, "qwen/qwen3-embedding-8b:"+strconv.Itoa(dims)+":l2")
		if got, ok := s.lookupBlock(id); !ok || got.SourceRoom != b.SourceRoom {
			t.Fatalf("fixed width alias %d did not resolve", dims)
		}
	}
	unsupported := blocks.BlockID(b.Collection, b.SourceRoom, b.SegmentationVersion, "qwen/qwen3-embedding-8b:1234:l2")
	if _, ok := s.lookupBlock(unsupported); ok {
		t.Fatal("arbitrary historic widths must not resolve")
	}
	if _, err := s.Store.DB.Exec(`UPDATE memory_rooms SET label='updated fixture' WHERE room=?`, b.SourceRoom); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.lookupBlock(b.ID); !ok || got.Label != "updated fixture" {
		t.Fatal("cached aliases must return fresh block metadata")
	}
	if _, err := s.Store.DB.Exec(`ALTER TABLE width_fixture_revisions RENAME TO config_revisions`); err != nil {
		t.Fatal(err)
	}
	widthConfig(t, s, "qwen/qwen3-embedding-8b", 1234)
	if _, ok := s.lookupBlock(unsupported); !ok {
		t.Fatal("current custom width must resolve after invalidation")
	}
	widthConfig(t, s, "qwen/qwen3-embedding-4b", 1024)
	if _, ok := s.lookupBlock(b.ID); ok {
		t.Fatal("model change must invalidate aliases")
	}
	current := s.dbBlocks()[0]
	native := blocks.BlockID(current.Collection, current.SourceRoom, current.SegmentationVersion, "qwen/qwen3-embedding-4b:2560:l2")
	if _, ok := s.lookupBlock(native); !ok {
		t.Fatal("new model native alias did not resolve")
	}
	if _, err := s.Store.DB.Exec(`DELETE FROM memory_rooms WHERE room=?`, current.SourceRoom); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.lookupBlock(native); ok {
		t.Fatal("deleted room must invalidate aliases")
	}
}

func TestEmbedWidthDirectionIsCheckedByTheServer(t *testing.T) {
	changed := func(t *testing.T, r action.Result, s *Service, before int, dims int) {
		t.Helper()
		if r.OK || r.Error == nil || r.Error.Code != errcode.ConfigConflict || r.Error.MessageKey != "models.embed_width_direction_changed" || r.Error.Message == "models.embed_width_direction_changed" {
			t.Fatalf("a direction that no longer holds must be a localized config conflict: %+v", r)
		}
		if s.Revision() != before || s.Doc().Models["embed"].Dimensions != dims {
			t.Fatal("a refused direction must change nothing")
		}
	}
	t.Run("raise-approved-but-it-would-lower", func(t *testing.T) {
		s := privacyService(t)
		widthConfig(t, s, "qwen/qwen3-embedding-8b", 4096) // the stored width is the full one: 2048 now cuts
		widthRoom(t, s)
		before := s.Revision()
		changed(t, widthExecute(s, "models.embed_width", map[string]any{"dimensions": 2048, "direction": "raise", "include_embed": true}), s, before, 4096)
	})
	t.Run("lower-approved-but-it-would-raise", func(t *testing.T) {
		s := privacyService(t)
		widthConfig(t, s, "qwen/qwen3-embedding-8b", 1024)
		widthRoom(t, s)
		before := s.Revision()
		changed(t, widthExecute(s, "models.embed_width", map[string]any{"dimensions": 2048, "direction": "lower"}), s, before, 1024)
		changed(t, widthExecute(s, "models.embed_width", map[string]any{"dimensions": 2048, "direction": "lower", "include_embed": true}), s, before, 1024)
	})
	t.Run("matching-direction-works-as-before", func(t *testing.T) {
		s := privacyService(t)
		widthConfig(t, s, "qwen/qwen3-embedding-8b", 4096)
		r := widthExecute(s, "models.embed_width", map[string]any{"dimensions": 1024, "direction": "lower"})
		if !r.OK || r.Output["adapted"] != true || s.Doc().Models["embed"].Dimensions != 1024 {
			t.Fatal("an approved cut must apply")
		}
		widthRoom(t, s)
		r = widthExecute(s, "models.embed_width", map[string]any{"dimensions": 2048, "direction": "raise"})
		if r.OK || r.Error == nil || r.Error.MessageKey != EmbedReindexRequiredKey {
			t.Fatal("an approved raise on an indexed model still asks for include_embed")
		}
		r = widthExecute(s, "models.embed_width", map[string]any{"dimensions": 2048, "direction": "raise", "include_embed": true})
		if !r.OK || r.Output["reembed"] != true || s.Doc().Models["embed"].Dimensions != 2048 {
			t.Fatal("an approved, confirmed raise must apply")
		}
	})
	t.Run("same-width-with-a-direction-is-still-unchanged", func(t *testing.T) {
		s := privacyService(t)
		widthConfig(t, s, "qwen/qwen3-embedding-8b", 1024)
		before := s.Revision()
		for _, dir := range []string{"lower", "raise"} {
			r := widthExecute(s, "models.embed_width", map[string]any{"dimensions": 1024, "direction": dir})
			if !r.OK || r.Output["changed"] != false || s.Revision() != before {
				t.Fatalf("an already-reached width stays a plain no-op (%s)", dir)
			}
		}
	})
	t.Run("oversized-setting-back-to-full-is-a-raise", func(t *testing.T) {
		s := privacyService(t)
		widthConfig(t, s, "qwen/qwen3-embedding-8b", 6000)
		widthRoom(t, s)
		before := s.Revision()
		changed(t, widthExecute(s, "models.embed_width", map[string]any{"dimensions": 0, "direction": "lower"}), s, before, 6000)
	})
	t.Run("omitted-direction-keeps-the-old-behavior", func(t *testing.T) {
		s := privacyService(t)
		widthConfig(t, s, "qwen/qwen3-embedding-8b", 4096)
		if r := widthExecute(s, "models.embed_width", map[string]any{"dimensions": 1024}); !r.OK {
			t.Fatal("a caller without direction (the CLI) must keep working")
		}
	})
	t.Run("unknown-direction-is-invalid", func(t *testing.T) {
		s := privacyService(t)
		widthConfig(t, s, "qwen/qwen3-embedding-8b", 4096)
		before := s.Revision()
		r := widthExecute(s, "models.embed_width", map[string]any{"dimensions": 1024, "direction": "up"})
		if r.OK || r.Error == nil || r.Error.Code != errcode.InvalidInput || s.Revision() != before {
			t.Fatal("only lower or raise are directions")
		}
	})
}
