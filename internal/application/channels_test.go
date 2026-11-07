package application_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"masamong/internal/config"
)

func TestChannelConfigurePersistsIsolatedPolicy(t *testing.T) {
	svc := setupSvc(t)
	res := exec(svc, "channels.configure", map[string]any{
		"guild_id": "g1", "id": "c1", "respond": "mention", "persona_ref": "neutral_en",
		"logging_level": "errors", "logging_retain": 7, "capture": true, "base_revision": 1,
	})
	if !res.OK {
		t.Fatalf("configure: %+v", res.Error)
	}
	if res.Output["persisted"] != true || res.Output["memory_use"] != "on" {
		t.Fatalf("expected persisted policy with memory on: %#v", res.Output)
	}
	list := exec(svc, "channels.list", nil)
	chans, _ := list.Output["channels"].([]any)
	found := false
	for _, item := range chans {
		m := item.(map[string]any)
		if m["id"] == "c1" && m["guild_id"] == "g1" {
			found = true
			if m["respond"] != "mention" || m["persona_ref"] != "neutral_en" {
				t.Fatalf("policy not merged into list: %#v", m)
			}
			if m["memory_use"] != "on" {
				t.Fatalf("memory_use default is on: %#v", m)
			}
		}
	}
	if !found {
		t.Fatal("configured channel missing from list while offline")
	}
}

func TestChannelRAGAttachmentIsolatedPerChannel(t *testing.T) {
	svc := setupSvc(t)
	staging := filepath.Join(svc.DataRoot, "rag_staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(staging, "notes.md")
	if err := os.WriteFile(src, []byte("# channel local notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	add := exec(svc, "channels.attachments.add", map[string]any{
		"guild_id": "g1", "channel_id": "c1", "filename": "notes.md", "source_path": "notes.md", "base_revision": 1,
	})
	if !add.OK {
		t.Fatalf("add: %+v", add.Error)
	}
	if add.Output["isolated"] != true || add.Output["rag_ready"] != false {
		t.Fatalf("staging contract: %#v", add.Output)
	}
	// Same hash on another channel is a separate attach (file copied under that channel dir).
	add2 := exec(svc, "channels.attachments.add", map[string]any{
		"guild_id": "g1", "channel_id": "c2", "filename": "notes.md", "source_path": "notes.md", "base_revision": svc.Revision(),
	})
	if !add2.OK {
		t.Fatalf("second channel add: %+v", add2.Error)
	}
	pol1 := svc.Doc().Guilds["g1"].Channels["c1"].RAGAttachments
	pol2 := svc.Doc().Guilds["g1"].Channels["c2"].RAGAttachments
	if len(pol1) != 1 || len(pol2) != 1 {
		t.Fatalf("each channel should own its refs: %d %d", len(pol1), len(pol2))
	}
	if pol1[0].StoredName == "" || pol1[0].ContentHash != pol2[0].ContentHash {
		t.Fatalf("hash shared but storage is per channel: %#v %#v", pol1[0], pol2[0])
	}
	path1 := filepath.Join(svc.DataRoot, "rag", "channels", "g1", "c1", pol1[0].StoredName)
	path2 := filepath.Join(svc.DataRoot, "rag", "channels", "g1", "c2", pol2[0].StoredName)
	if _, err := os.Stat(path1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path2); err != nil {
		t.Fatal(err)
	}
	log := exec(svc, "channels.ops_log", map[string]any{"guild_id": "g1", "channel_id": "c1"})
	entries, _ := log.Output["entries"].([]any)
	if len(entries) == 0 {
		t.Fatal("ops log should record attachment_add")
	}
}

func TestChannelViewAttachDetachNoReembed(t *testing.T) {
	svc := setupSvc(t)
	cfg := exec(svc, "channels.configure", map[string]any{
		"guild_id": "g1", "id": "c1", "respond": "command", "base_revision": 1,
	})
	if !cfg.OK {
		t.Fatalf("configure: %+v", cfg.Error)
	}
	// a second channel's history is the block c1 connects to (a channel's own room is always searched)
	if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g1", "id": "c2", "respond": "command"}); !r.OK {
		t.Fatalf("configure c2: %+v", r.Error)
	}
	var blockID string
	for _, b := range jsonShape(exec(svc, "memory.blocks.list", nil).Output)["blocks"].([]any) {
		if row := b.(map[string]any); row["source_room"] == "discord:g1:c2" {
			blockID, _ = row["block_id"].(string)
		}
	}
	if blockID == "" {
		t.Fatal("declared discord block of c2 expected after configure")
	}
	add := exec(svc, "channels.view.attach", map[string]any{
		"guild_id": "g1", "channel_id": "c1", "block_id": blockID, "base_revision": svc.Revision(),
	})
	if !add.OK {
		t.Fatalf("attach: %+v", add.Error)
	}
	if add.Output["reembedded"] != false {
		t.Fatalf("attach must not reembed: %#v", add.Output)
	}
	view, _ := add.Output["memory_view"].(map[string]any)
	if view["revision"] != 1 && view["revision"] != float64(1) {
		// json numbers
		if fmtInt(view["revision"]) != 1 {
			t.Fatalf("revision %#v", view["revision"])
		}
	}
	show := exec(svc, "channels.view.show", map[string]any{"guild_id": "g1", "channel_id": "c1"})
	if !show.OK {
		t.Fatalf("show: %+v", show.Error)
	}
	rm := exec(svc, "channels.view.detach", map[string]any{
		"guild_id": "g1", "channel_id": "c1", "block_id": blockID, "base_revision": svc.Revision(),
	})
	if !rm.OK {
		t.Fatalf("detach: %+v", rm.Error)
	}
	if rm.Output["reembedded"] != false {
		t.Fatal("detach must not reembed")
	}
}

func fmtInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	default:
		return -1
	}
}

func TestPersonaApplyRequiresChannelScope(t *testing.T) {
	svc := setupSvc(t)
	bad := exec(svc, "personas.apply", map[string]any{"id": "friendly_ko", "base_revision": 1})
	if bad.OK {
		t.Fatal("persona apply without channel must fail")
	}
	ok := exec(svc, "personas.apply", map[string]any{
		"id": "friendly_ko", "guild_id": "g9", "channel_id": "c9", "base_revision": 1,
	})
	if !ok.OK || ok.Output["persisted"] != true {
		t.Fatalf("persona apply: %+v %#v", ok.Error, ok.Output)
	}
	if ok.Output["cannot_override_memory_scope"] != true {
		t.Fatal("persona must not claim memory scope override")
	}
}

func TestPersonaApplyToSeveralRoomsIsOneChange(t *testing.T) {
	svc := setupSvc(t)
	before := svc.Revision()
	res := exec(svc, "personas.apply", map[string]any{"id": "neutral_en", "targets": []any{"100000000000000001:200000000000000002", "100000000000000001:200000000000000003"}})
	if !res.OK || fmtInt(res.Output["rooms"]) != 2 {
		t.Fatalf("apply: %+v %#v", res.Error, res.Output)
	}
	if svc.Revision() != before+1 {
		t.Fatalf("two rooms should be one config change: r%d → r%d", before, svc.Revision())
	}
	for _, c := range []string{"200000000000000002", "200000000000000003"} {
		if got := svc.Doc().Guilds["100000000000000001"].Channels[c].PersonaRef; strings.TrimSuffix(got, "_v2") != "neutral_en" {
			t.Fatalf("room %s persona %q", c, got)
		}
	}
	for _, bad := range []map[string]any{
		{"id": "neutral_en", "targets": []any{"not-a-room"}},
		{"id": "nobody", "targets": []any{"100000000000000001:200000000000000002"}},
	} {
		if exec(svc, "personas.apply", bad).OK {
			t.Fatalf("must fail: %v", bad)
		}
	}
}

func TestPersonaListOrderIsStable(t *testing.T) {
	svc := setupSvc(t)
	var first []string
	for i := 0; i < 5; i++ {
		var ids []string
		for _, p := range exec(svc, "personas.list", nil).Output["personas"].([]any) {
			ids = append(ids, p.(map[string]any)["id"].(string))
		}
		if first == nil {
			first = ids
			if len(ids) < 2 || ids[0] != "warm_ko" || ids[1] != "friendly_ko" { // v1 first in the list; v2 follows by language
				t.Fatalf("Korean default persona first, then the casual one: %v", ids)
			}
		} else if strings.Join(ids, ",") != strings.Join(first, ",") {
			t.Fatalf("order changed: %v vs %v", ids, first)
		}
	}
}

// An unconfigured room uses the persona of its language: the server locale, then the instance
// default. A Korean server never hands an English room a Korean persona, and the reverse.
func TestDefaultPersonaFollowsTheRoomLanguage(t *testing.T) {
	svc := setupSvc(t)
	// the instance default is en (config.Default): an unconfigured room is English
	if got := svc.ChannelPolicyFor("g", "c").PersonaRef; strings.TrimSuffix(got, "_v2") != "friendly_en" {
		t.Fatalf("instance default persona %q", got)
	}
	applyDoc(t, svc, func(d *config.Document) { d.Guilds = map[string]config.GuildSettings{"g": {Locale: "ko"}} })
	if got := svc.ChannelPolicyFor("g", "c").PersonaRef; strings.TrimSuffix(got, "_v2") != "warm_ko" {
		t.Fatalf("Korean server persona %q", got)
	}
	// a stored room without a persona follows the server language too, and an unrelated edit does not pin one
	if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g", "id": "c", "respond": "mention"}); !r.OK {
		t.Fatalf("%+v", r.Error)
	}
	if got := svc.Doc().Guilds["g"].Channels["c"].PersonaRef; strings.TrimSuffix(got, "_v2") != "" {
		t.Fatalf("an unrelated edit must not pin a persona: %q", got)
	}
	if got := svc.ChannelPolicyFor("g", "c").PersonaRef; strings.TrimSuffix(got, "_v2") != "warm_ko" {
		t.Fatalf("stored room persona %q", got)
	}
	// an explicit choice is kept
	if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g", "id": "c", "persona_ref": "neutral_en"}); !r.OK {
		t.Fatalf("%+v", r.Error)
	}
	if got := svc.ChannelPolicyFor("g", "c").PersonaRef; strings.TrimSuffix(got, "_v2") != "neutral_en" {
		t.Fatalf("explicit persona %q", got)
	}
	// a room that chose the casual Korean persona keeps it: the new default only reaches rooms with no choice
	if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g", "id": "c", "persona_ref": "friendly_ko"}); !r.OK {
		t.Fatalf("%+v", r.Error)
	}
	if got := svc.ChannelPolicyFor("g", "c").PersonaRef; strings.TrimSuffix(got, "_v2") != "friendly_ko" {
		t.Fatalf("explicit casual persona %q", got)
	}
	// a language without a friendly persona falls back to English
	applyDoc(t, svc, func(d *config.Document) { d.Guilds = map[string]config.GuildSettings{"g": {Locale: "fr"}} })
	if got := svc.ChannelPolicyFor("g", "c").PersonaRef; strings.TrimSuffix(got, "_v2") != "friendly_en" {
		t.Fatalf("unknown language persona %q", got)
	}
}

// Hiding a built-in keeps it out of the default choice and marks it in the list, so the console can
// restore it later.
func TestHiddenPersonaIsSkippedAndListed(t *testing.T) {
	svc := setupSvc(t)
	applyDoc(t, svc, func(d *config.Document) {
		d.I18n.DefaultLocale = "ko"
		d.Personas = map[string]config.Persona{"warm_ko": {Hidden: true}}
	})
	// an operator who wants the casual voice hides the polite default: rooms fall to friendly_ko
	if got := svc.ChannelPolicyFor("g", "c").PersonaRef; strings.TrimSuffix(got, "_v2") != "friendly_ko" {
		t.Fatalf("a hidden default must fall back: %q", got)
	}
	applyDoc(t, svc, func(d *config.Document) {
		d.Personas = map[string]config.Persona{"warm_ko": {Hidden: true}, "friendly_ko": {Hidden: true}}
	})
	if got := svc.ChannelPolicyFor("g", "c").PersonaRef; strings.TrimSuffix(got, "_v2") != "friendly_en" {
		t.Fatalf("both Korean defaults hidden: %q", got)
	}
	found := false
	for _, p := range exec(svc, "personas.list", nil).Output["personas"].([]any) {
		m := p.(map[string]any)
		if m["id"] == "warm_ko" {
			found = true
			if m["hidden"] != true {
				t.Fatalf("hidden flag: %#v", m)
			}
		}
	}
	if !found {
		t.Fatal("a hidden persona stays in the list so the console can restore it")
	}
}

// The console hides and restores a built-in through config.patch: the flag persists and unsetting
// the override brings the built-in back.
func TestPersonaHidePatchRoundTrips(t *testing.T) {
	svc := setupSvc(t)
	r := exec(svc, "config.patch", map[string]any{"base_revision": svc.Revision(), "ops": []any{
		map[string]any{"path": []any{"personas", "friendly_ko", "hidden"}, "value": true},
	}})
	if !r.OK {
		t.Fatalf("patch: %+v", r.Error)
	}
	if !svc.Doc().Personas["friendly_ko"].Hidden {
		t.Fatal("hidden flag did not persist")
	}
	r = exec(svc, "config.patch", map[string]any{"base_revision": svc.Revision(), "ops": []any{
		map[string]any{"path": []any{"personas", "friendly_ko"}, "op": "unset"},
	}})
	if !r.OK {
		t.Fatalf("unset: %+v", r.Error)
	}
	if _, ok := svc.Doc().Personas["friendly_ko"]; ok {
		t.Fatal("restore must remove the override")
	}
}

// Every language has a friendly persona for the default, and the tone sets are complete.
func TestBuiltinPersonaSetsCoverEachLanguage(t *testing.T) {
	all := config.BuiltinPersonas()
	for id, p := range all {
		if p.Locale == "" || p.Style == "" || !p.Builtin {
			t.Fatalf("%s: locale, style and builtin are required: %#v", id, p)
		}
	}
	for _, tag := range []string{"ko", "en", "ja", "es", "zh_hans"} {
		for _, tone := range []string{"friendly", "polite", "brief", "cheer"} {
			if _, ok := all[tone+"_"+tag]; !ok {
				t.Fatalf("%s_%s missing", tone, tag)
			}
		}
	}
	if got := config.DefaultPersonaCandidates("ko-KR", true)[0]; strings.TrimSuffix(got, "_v2") != "warm_ko" {
		t.Fatalf("region variants normalize: %q", got)
	}
	// Korean and Japanese start polite (a bot talking down to strangers is rude); English, Spanish
	// and Chinese have no formality split a new server would feel, so they keep the friendly voice
	for tag, want := range map[string]string{"ko": "warm_ko", "ja": "warm_ja", "en": "friendly_en", "es": "friendly_es", "zh-Hans": "friendly_zh_hans"} {
		if got := config.DefaultPersonaCandidates(tag, true)[0]; strings.TrimSuffix(got, "_v2") != want {
			t.Fatalf("default persona of %s = %q, want %q", tag, got, want)
		}
	}
	// the casual Korean friend stays a named preset under its old ID, and the polite default is not it
	if _, ok := all["friendly_ko"]; !ok || !strings.Contains(all["friendly_ko"].Style, "반말") {
		t.Fatalf("friendly_ko must stay the 반말 preset: %#v", all["friendly_ko"])
	}
	if st := all["warm_ko"].Style; !strings.Contains(st, "해요체") || strings.Contains(st, "반말로") {
		t.Fatalf("warm_ko must be polite 해요체: %q", st)
	}
	if st := all["warm_ja"].Style; !strings.Contains(st, "です・ます") || strings.Contains(st, "タメ口で") {
		t.Fatalf("warm_ja must be polite: %q", st)
	}
	if got := config.DefaultPersonaCandidates("zh-Hans", true)[0]; strings.TrimSuffix(got, "_v2") != "friendly_zh_hans" {
		t.Fatalf("zh-Hans persona id: %q", got)
	}
}

// An instance made before i18n.polite_default existed has no such key and keeps the casual default
// it was running with; turning it on moves unconfigured Korean rooms to the polite voice.
func TestPoliteDefaultIsOptInForExistingInstances(t *testing.T) {
	svc := setupSvc(t)
	applyDoc(t, svc, func(d *config.Document) {
		d.I18n.PoliteDefault = false
		d.Guilds = map[string]config.GuildSettings{"g": {Locale: "ko"}}
	})
	if got := svc.ChannelPolicyFor("g", "c").PersonaRef; strings.TrimSuffix(got, "_v2") != "friendly_ko" {
		t.Fatalf("an upgraded instance changed voice: %q", got)
	}
	applyDoc(t, svc, func(d *config.Document) { d.I18n.PoliteDefault = true })
	if got := svc.ChannelPolicyFor("g", "c").PersonaRef; strings.TrimSuffix(got, "_v2") != "warm_ko" {
		t.Fatalf("polite default on: %q", got)
	}
	if !config.Default("x", t.TempDir()).I18n.PoliteDefault {
		t.Fatal("new instances start polite")
	}
}

// A direct message and a channel without an entry of its own read the defaults with every choice filled
// in, as a saved entry does: an empty reply_shape used to mean "not bubbles", so their replies were
// never split into bubbles.
func TestChannelsWithoutAnEntryReplyInBubbles(t *testing.T) {
	svc := setupSvc(t)
	if res := exec(svc, "channels.configure", map[string]any{"guild_id": "g1", "id": "c1", "respond": "mention"}); !res.OK {
		t.Fatalf("configure: %+v", res.Error)
	}
	for name, pol := range map[string]config.ChannelPolicy{
		"direct message":                 svc.ChannelPolicyFor("", "dm"),
		"unlisted server":                svc.ChannelPolicyFor("g-unknown", "c-unknown"),
		"unlisted channel, known server": svc.ChannelPolicyFor("g1", "c2"),
	} {
		if pol.Conversation.ReplyShape != "bubbles" {
			t.Errorf("%s: reply_shape = %q, want bubbles", name, pol.Conversation.ReplyShape)
		}
	}
}
