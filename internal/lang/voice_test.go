package lang

import (
	"regexp"
	"strings"
	"testing"
)

// voiceWords are, per language, the phrases the voice rules are written with. A pack is checked for
// what its prompts tell the model, not for a whole sentence, so rewording keeps the tests green.
var voiceWords = map[string]struct {
	askRoom      string   // the thin-memory note sends the question back to the room
	bluff        []string // the old instructions to bluff through a gap
	ownSituation string   // the rule against inventing the bot's own circumstances
	several      string   // the rule for several matching memories
}{
	"ko":      {"방에 물어봐라", []string{"아는 척 대충", "농담으로 받아쳐도", "조금 틀려도 괜찮다"}, "네 사정", "여럿이면"},
	"en":      {"ask the room", []string{"wing it", "let it slide", "a little wrong is okay"}, "your own circumstances", "several memories"},
	"ja":      {"部屋に聞いて", []string{"適当に流す", "軽く流したり", "少し違っても大丈夫"}, "自分の事情", "複数"},
	"zh-Hans": {"房间里", []string{"随口带过", "一带而过", "稍微不对也没关系"}, "你自己的情况", "多条记忆"},
	"es":      {"la sala", []string{"improvísalo", "dejarlo pasar", "no pasa nada"}, "tus propias circunstancias", "varios recuerdos"},
}

// A thin-memory note must not tell the model to bluff: it says the same as the empty-memory note
// (not clearly remembered, ask the room, invent neither who did what nor the bot's own situation),
// and the memory rules carry the two rules that keep the model honest when memory has gaps or
// several answers.
func TestThinMemoryNoteAndRulesDoNotLetTheModelBluff(t *testing.T) {
	for _, tag := range Available() {
		w, ok := voiceWords[tag]
		if !ok {
			t.Fatalf("%s: add the voice phrases of this language to voiceWords", tag)
		}
		p, _ := Get(tag)
		thin, empty, rules := p.Chat.RagNotes["insufficient_authorized_candidates"], p.Chat.RagNotes["empty_in_allowed_scope"], p.Chat.MemoryRules
		if !strings.Contains(thin, w.askRoom) {
			t.Errorf("%s: the thin-memory note must send the question to the room: %q", tag, thin)
		}
		for _, note := range []string{thin, empty} {
			if !strings.Contains(note, w.ownSituation) {
				t.Errorf("%s: a memory note must forbid inventing the bot's circumstances: %q", tag, note)
			}
		}
		for _, bad := range w.bluff {
			for name, text := range map[string]string{"thin-memory note": thin, "memory rules": rules} {
				if strings.Contains(text, bad) {
					t.Errorf("%s: the %s still invites bluffing (%q)", tag, name, bad)
				}
			}
		}
		for name, want := range map[string]string{"own circumstances": w.ownSituation, "several memories": w.several} {
			if !strings.Contains(rules, want) {
				t.Errorf("%s: memory rules lack the %s rule (%q)", tag, name, want)
			}
		}
	}
}

var atxHeading = regexp.MustCompile(`(?m)^#{1,6} `)

// Discord shows "##" as plain text in many places, so no prompt asks for it: summaries use bold
// subheadings and single bullets, the way the fortune prompts do.
func TestSummaryPromptsAskForDiscordSafeFormat(t *testing.T) {
	for _, tag := range Available() {
		p, _ := Get(tag)
		for name, prompt := range map[string]string{
			"channel_summary":             p.Features.ChannelSummary,
			"channel_summary_incremental": p.Features.ChannelSummaryIncremental,
			"fortune_system":              p.Features.FortuneSystem,
			"zodiac_system":               p.Features.ZodiacSystem,
		} {
			if atxHeading.MatchString(prompt) {
				t.Errorf("%s %s demands a # heading Discord does not render", tag, name)
			}
		}
		for name, prompt := range map[string]string{"channel_summary": p.Features.ChannelSummary, "channel_summary_incremental": p.Features.ChannelSummaryIncremental} {
			if got := strings.Count(prompt, "\n**"); got != 3 {
				t.Errorf("%s %s has %d bold subheadings, want 3", tag, name, got)
			}
		}
	}
}

// Korean and Japanese have a formality split a new server notices, so their default persona, the
// fortune prompts and the fallback persona text all ask for the same polite register. The other
// languages keep one friendly register everywhere and must not start in a casual-for-strangers voice.
func TestFortuneAndDefaultPersonaShareTheChatRegister(t *testing.T) {
	polite := map[string]struct{ persona, fortune string }{
		"ko": {"해요체", "해요체"},
		"ja": {"です・ます", "です・ます"},
	}
	for _, tag := range Available() {
		p, _ := Get(tag)
		if want, ok := polite[tag]; ok {
			if !strings.Contains(p.Chat.DefaultPersona, want.persona) {
				t.Errorf("%s: the default persona must be polite: %q", tag, p.Chat.DefaultPersona)
			}
			for name, prompt := range map[string]string{"fortune_system": p.Features.FortuneSystem, "zodiac_system": p.Features.ZodiacSystem} {
				if !strings.Contains(prompt, want.fortune) {
					t.Errorf("%s %s must ask for the same polite register as the default persona", tag, name)
				}
			}
			continue
		}
		for _, bad := range []string{"반말로", "タメ口で"} {
			if strings.Contains(p.Chat.DefaultPersona, bad) {
				t.Errorf("%s: default persona %q", tag, p.Chat.DefaultPersona)
			}
		}
	}
	ko, _ := Get("ko")
	if strings.Contains(ko.Chat.DefaultPersona, "반말로") {
		t.Errorf("the Korean default persona must not be 반말: %q", ko.Chat.DefaultPersona)
	}
	// the briefing's task and greeting example follow its send time (catalog fortune.briefing.*, whose
	// Korean examples are 해요체: internal/application TestBriefingGreetingsArePolite)
	if !strings.Contains(ko.Features.FortuneMorning, "{task}") || !strings.Contains(ko.Features.FortuneMorning, "GREETING: {greeting}") {
		t.Errorf("the briefing takes its task and greeting from the catalog: %q", ko.Features.FortuneMorning)
	}
}

// Proactive participation has one name in Korean, "먼저 말 걸기", in the judge prompt too.
func TestParticipationJudgeUsesTheConsentName(t *testing.T) {
	ko, _ := Get("ko")
	if !strings.Contains(ko.Features.ParticipationJudgeSystem, "먼저 말 걸기") || strings.Contains(ko.Features.ParticipationJudgeSystem, "능동") {
		t.Fatalf("judge prompt: %q", ko.Features.ParticipationJudgeSystem)
	}
}

func TestChatVoiceFollowsTheMessageLanguageAndNamesUnconfiguredTools(t *testing.T) {
	for _, tag := range Available() {
		p, _ := Get(tag)
		if strings.TrimSpace(p.Chat.ReplyLanguageRule) == "" || strings.TrimSpace(p.Chat.ContinueAnswer) == "" || strings.TrimSpace(p.Chat.IncompleteAnswer) == "" {
			t.Errorf("%s: missing reply-language or truncation guidance", tag)
		}
		if !strings.Contains(p.Chat.MemoryRules, "unconfigured") {
			t.Errorf("%s: unconfigured tools need a clear user-facing response", tag)
		}
	}
}
