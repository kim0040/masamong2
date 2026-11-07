// Package lang holds the language packs for everything the bot says to a model or puts into a
// prompt: planner and answer prompts, memory rules, evidence markers, summary prompt, query
// instruction, and the word lists retrieval needs (particles, pronouns, stop words). Code never
// embeds one language's text; it asks the pack for the chat's language.
//
// Packs are packs/<tag>.json (embedded). Korean ("ko") is the evaluated default — its text is the
// exact prompt the test sandbox measured. Adding a language is adding a JSON file with the same keys;
// Load fails on a pack that misses a required key, so a partial translation cannot ship silently.
package lang

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"

	"masamong/internal/i18n"
)

//go:embed packs/*.json
var packsFS embed.FS

// Default is the pack used when nothing more specific is configured.
const Default = "ko"

// Pack is one language.
type Pack struct {
	Tag      string          `json:"-"`
	Name     string          `json:"name"`
	Chat     Chat            `json:"chat"`
	Memory   Memory          `json:"memory"`
	Search   Search          `json:"search"`
	Tools    map[string]Tool `json:"tools"`
	Practice Practice        `json:"practice"`
	Features Features        `json:"features"`
	Time     Time            `json:"time"`
	// Conversation holds the small classifiers that keep the bot in the right conversation of a busy
	// room (docs/plan-memory-conversation.md §5.1–5.2). Their output is JSON; the words of the room are
	// read by the model, never matched by language rules in code.
	Conversation Conversation `json:"conversation"`
	// Thread is what the bot says around a conversation it moves into a thread (§5.3): the offer under a
	// long reply, its buttons, the first message inside, the summary prompt, the recap in the parent.
	Thread Thread `json:"thread"`
	// Shape holds the words of a reply's form: the button that shows the rest of a trimmed reply.
	Shape Shape `json:"shape"`
	// Vision is the text around pictures a member calls the bot with. Optional per pack: what a pack
	// leaves out comes from English (VisionText).
	Vision Vision `json:"vision"`
}

// Vision is how a picture appears in a turn's text, and the prompt that describes pictures for an
// answer model that cannot see them.
type Vision struct {
	Attached       string `json:"attached,omitempty"`        // a picture sent with the message: "[image]"
	Described      string `json:"described,omitempty"`       // {description}: what a vision model saw
	Unseen         string `json:"unseen,omitempty"`          // a picture no model could look at
	Partial        string `json:"partial,omitempty"`         // {read} of {total} pictures were read
	DescribeSystem string `json:"describe_system,omitempty"` // the describing call's system prompt
	Describe       string `json:"describe,omitempty"`        // {n}: how many pictures
	Earlier        string `json:"earlier,omitempty"`         // after the tag: the pictures came just before the message
}

// VisionText is the pack's Vision with English filling what it leaves out.
func (p *Pack) VisionText() Vision {
	v := p.Vision
	en, ok := Get("en")
	if !ok || p.Tag == "en" {
		return v
	}
	fill := func(dst *string, src string) {
		if strings.TrimSpace(*dst) == "" {
			*dst = src
		}
	}
	fill(&v.Attached, en.Vision.Attached)
	fill(&v.Described, en.Vision.Described)
	fill(&v.Unseen, en.Vision.Unseen)
	fill(&v.Partial, en.Vision.Partial)
	fill(&v.DescribeSystem, en.Vision.DescribeSystem)
	fill(&v.Describe, en.Vision.Describe)
	fill(&v.Earlier, en.Vision.Earlier)
	return v
}

// Time is how people say when, for reminders and plans (internal/features/reminders reads it). Forms
// made only of digits (2026-10-03, 10/3, 15:30) are read in code for every language; these are the
// words and patterns around them. Patterns are Go regular expressions matched against lower-cased
// text, with named groups: h, m, half (hours), mo, mon, d (dates), n, unit (in/after), ampm.
type Time struct {
	RelativeDays map[string]int    `json:"relative_days"` // today 0, tomorrow 1, ...
	Weekdays     [7][]string       `json:"weekdays"`      // Sunday first: each day's words
	ThisWeek     []string          `json:"this_week"`     // before a weekday: this week's
	NextWeek     []string          `json:"next_week"`     // before a weekday: next week's
	AM           []string          `json:"am"`            // before or after an hour: morning
	PM           []string          `json:"pm"`            // before or after an hour: afternoon, evening
	DayParts     map[string]string `json:"day_parts"`     // a time of day without a clock: lunch -> "12:00"
	Months       [12][]string      `json:"months"`        // January first: month words for the mon group ([] when unused)
	Hours        []string          `json:"hours"`         // clock patterns (h, optional m or half, optional ampm)
	Dates        []string          `json:"dates"`         // calendar patterns (mo or mon, d)
	DayOfMonth   []string          `json:"day_of_month"`  // a day in this or next month (d)
	After        []string          `json:"after"`         // "in 30 minutes" (n, unit)
	Units        map[string]string `json:"units"`         // unit word -> minute | hour | day | week
	Fillers      []string          `json:"fillers"`       // words around a time that are not the reminder's text
	PlanWords    []string          `json:"plan_words"`    // words of making plans (meet, let's go): a plan candidate
	Question     []string          `json:"question"`      // words that make a sentence a question besides "?"
	Recall       []string          `json:"recall"`        // words of asking the group's memory (when did we, who said)
}

// Conversation prompts: which earlier lines a call points at, whether a message with the bot's name is
// said to it, whether an untagged message continues the conversation with it.
type Conversation struct {
	ReferSystem    string `json:"refer_system"`     // {bot}
	Refer          string `json:"refer"`            // {lines} {speaker} {message}
	NameCallSystem string `json:"name_call_system"` // {bot}
	NameCall       string `json:"name_call"`        // {lines} {speaker} {message} {bot}
	FollowSystem   string `json:"follow_system"`    // {bot}
	Follow         string `json:"follow"`           // {bot_last} {between} {speaker} {message}
	// FollowupCues are optional words that often start a follow-up ("then", "그럼"): one point of the
	// follow-up score, never a decision on their own; a language without them works the same.
	FollowupCues []string `json:"followup_cues,omitempty"`
	// BanterRules are added to a turn that answers a playful mention of the bot (one light line).
	BanterRules string `json:"banter_rules"`
	// BanterEmoji are the reactions a playful mention may get instead of a line.
	BanterEmoji []string `json:"banter_emoji,omitempty"`
}

// Shape is the words around a reply's form.
type Shape struct {
	More string `json:"more"` // button under a trimmed reply
}

// Thread is the thread offer and the bot-made thread's own lines.
type Thread struct {
	Offer         string `json:"offer"`          // under the reply: "getting long — move to a thread?"
	Open          string `json:"open"`           // button
	Stay          string `json:"stay"`           // button
	Stop          string `json:"stop"`           // button inside the thread
	Name          string `json:"name"`           // {speaker} {topic}
	Seed          string `json:"seed"`           // {mention} {summary}
	SeedPlain     string `json:"seed_plain"`     // {mention}: when no summary could be made
	Bye           string `json:"bye"`            // the bot's last line when the thread ends
	NotYours      string `json:"not_yours"`      // someone else pressed the caller's button
	Off           string `json:"off"`            // a thread button pressed in a room whose threads are off
	Recap         string `json:"recap"`          // {summary}: posted in the parent channel
	SummarySystem string `json:"summary_system"` // {bot}
	Summary       string `json:"summary"`        // {lines}: JSON {"summary": "...", "has_plan": bool}
	// StopCues end the thread when a message is exactly one of them ("그만", "마무리"); optional.
	StopCues []string `json:"stop_cues,omitempty"`
}

// Features are the prompts of the bot's other features (channel summary, fortune, ...).
type Features struct {
	// Room style (P5-2): the extraction prompt ({lines}), the note template ({median} {p90} {laughs}
	// {laugh_pct} {emoji_pct} {languages} {items}), the header before the note in a reply's rules, and the
	// names of the item kinds (slang, nickname, joke, avoid).
	RoomStyleSystem           string            `json:"room_style_system"`
	RoomStyle                 string            `json:"room_style"`
	RoomStyleNote             string            `json:"room_style_note"`
	RoomStyleHeader           string            `json:"room_style_header"`
	RoomStyleKinds            map[string]string `json:"room_style_kinds"`
	ChannelSummary            string            `json:"channel_summary"`             // {conversation}
	ChannelSummaryIncremental string            `json:"channel_summary_incremental"` // {previous} {conversation}
	ImagePrompt               string            `json:"image_prompt"`                // {request}
	// ImageBlockedWords refuse an image request before any model call (clearly sexual or violent
	// words; every pack's list applies, since a request can mix languages).
	ImageBlockedWords []string `json:"image_blocked_words"`
	// Fortune prompts ({bot} {data} {name} {gender} {sign} {sky} {date}); the system text holds the
	// persona and the Discord-safe formatting rules.
	FortuneSystem      string `json:"fortune_system"`       // {bot}
	FortunePublic      string `json:"fortune_public"`       // {data} {name} (no gender: it is posted in public)
	FortuneToday       string `json:"fortune_today"`        // {data} {name} {gender}
	FortuneDetailDay   string `json:"fortune_detail_day"`   // {data} {name} {gender} {sign}
	FortuneDetailMonth string `json:"fortune_detail_month"` // {data} {name} {gender} {sign}
	FortuneDetailYear  string `json:"fortune_detail_year"`  // {data} {name} {gender} {sign}
	FortuneMorning     string `json:"fortune_morning"`      // {data} {name} {gender} {date} {task} {greeting} (fortune.briefing.*: the send-time slot)
	ZodiacSystem       string `json:"zodiac_system"`        // {bot}
	ZodiacPublic       string `json:"zodiac_public"`        // {sky} {sign}
	ZodiacPrivate      string `json:"zodiac_private"`       // {sky} {sign}
	ZodiacRanking      string `json:"zodiac_ranking"`       // {sky} {date}
	// Participation: the judge that decides, for a debounced candidate only, whether joining the
	// conversation is worth it (a JSON answer), and the extra rules of a reply the bot starts itself.
	ParticipationJudgeSystem string `json:"participation_judge_system"` // {bot}
	ParticipationJudge       string `json:"participation_judge"`        // {conversation} {speaker} {message} {kind} {now}
	// QuietCues ("조용히 해") step the bot back from chiming in for a while when said right after a remark
	// of its own; ChimeOffCues ("끼어들기 꺼") are a person's own opt-out of chiming in. Whole messages only.
	QuietCues    []string `json:"quiet_cues,omitempty"`
	ChimeOffCues []string `json:"chime_off_cues,omitempty"`
	// TurnStopCues ("그만", "stop") cancel the reply the bot is writing for that person when they are a
	// whole message; with nothing in flight they are ordinary talk.
	TurnStopCues []string `json:"turn_stop_cues,omitempty"`
	// ThanksCues, LaughCues and CorrectionCues are words that show how a person took the bot's last reply
	// ("고마워", "ㅋㅋ", "그게 아니라"). A short message holding one is noted on that reply's turn as an
	// implicit signal (application/implicit_feedback.go). They only help choose turns to review: they are
	// never a score or an alert. A language without a list has no such signal.
	ThanksCues     []string `json:"thanks_cues,omitempty"`
	LaughCues      []string `json:"laugh_cues,omitempty"`
	CorrectionCues []string `json:"correction_cues,omitempty"`
	ProactiveRules string   `json:"proactive_rules"`
	// ScheduledTask is the answer rule of a scheduled task's run (application reminders_task.go): it is
	// a run of a task set earlier, done now, scheduling nothing. {name} asked for it, {when} is its time.
	ScheduledTask string `json:"scheduled_task"`
}

// Practice is what the console's offline practice model says (internal/providers/practice).
type Practice struct {
	Answer   string `json:"answer"`    // {evidence}: the best evidence lines
	NoMemory string `json:"no_memory"` // nothing was found
}

// Chat is the planner and answer prompt text. Templates use {name} placeholders filled by Fill.
type Chat struct {
	PlannerHeader    string `json:"planner_header"`
	PlanOutputSchema string `json:"plan_output_schema"`
	PlanFieldRules   string `json:"plan_field_rules"`
	PlanGeneralRules string `json:"plan_general_rules"` // {routing}
	RoutingJoin      string `json:"routing_join"`
	RoutingEnd       string `json:"routing_end"`
	RoutingNone      string `json:"routing_none"`
	ContextRules     string `json:"context_rules"` // {bot}
	ToolsHeader      string `json:"tools_header"`
	ToolsNone        string `json:"tools_none"`
	MemoryOff        string `json:"memory_off"`
	AssistantLabel   string `json:"assistant_label"` // {bot}
	UserLabel        string `json:"user_label"`
	UserLabelNamed   string `json:"user_label_named"` // {name}
	TimeRef          string `json:"time_ref"`         // {time}
	TimeRefMissing   string `json:"time_ref_missing"`
	LiveTime         string `json:"live_time"` // {time} {weekday} {tz}
	// Weekdays names Sunday … Saturday for the live time, so "this weekend" or "다음 주 월요일" can be
	// turned into a day offset without the model working out the weekday from a date.
	Weekdays           []string   `json:"weekdays,omitempty"`
	Delimiters         Delimiters `json:"delimiters"`
	ConversationHeader string     `json:"conversation_header"` // {n}
	ConversationEmpty  string     `json:"conversation_empty"`
	SummaryHeader      string     `json:"summary_header"`
	Truncated          string     `json:"truncated"`
	PlaceholderWords   []string   `json:"placeholder_words"`
	DefaultPersona     string     `json:"default_persona"` // {bot}
	ReplyLanguageRule  string     `json:"reply_language_rule"`
	ContinueAnswer     string     `json:"continue_answer"`
	IncompleteAnswer   string     `json:"incomplete_answer"`
	// ToolPolicy is the system prompt of the tool loop ({bot}): when a tool is a must, when it is never
	// used, how values are filled (never silently), and that tool output is untrusted data.
	ToolPolicy string `json:"tool_policy"`
	// ToolAssumed is added to the answer's tool block when a result carries assumed_* values: say what
	// was assumed and invite a correction.
	ToolAssumed string `json:"tool_assumed"`
	// ToolQueued is added to the answer's tool block when a picture was queued (status queued or
	// already_queued): one short line, nothing described as if it were done.
	ToolQueued string `json:"tool_queued"`
	// ToolSchedule is added when a reminder or task tool answered (reminders.*): a question still to ask
	// (needs_detail) is asked and nothing is claimed as set; what was set is confirmed from the result.
	ToolSchedule   string            `json:"tool_schedule"`
	MemoryRules    string            `json:"memory_rules"`
	SecurityRules  string            `json:"security_rules"`
	RagNotes       map[string]string `json:"rag_notes"`
	EvidenceLine   string            `json:"evidence_line"`   // {i} {date} {speakers}
	CorrectionLine string            `json:"correction_line"` // {note}: an operator's correction of a memory
	SummaryLine    string            `json:"summary_line"`    // {i} {summary}
	// MemoryTail follows the memory snippets when there are some: combine them into the bot's own words
	// rather than relaying them line by line. Optional; "" adds nothing.
	MemoryTail     string `json:"memory_tail,omitempty"`
	Unknown        string `json:"unknown"`
	None           string `json:"none"`
	DefaultSpeaker string `json:"default_speaker"`
	AnswerTemplate string `json:"answer_template"` // {who} {hint} {recent} {session} {tools} {summaries} {memory} {message}
	// AnswerNow heads the answer's user message with the current time ({time} {weekday} {tz}), so the
	// answer can place "yesterday" and the conversation's ages. Optional; "" adds nothing.
	AnswerNow string `json:"answer_now,omitempty"`
	// Ages mark how long ago a conversation line was said, for lines older than a few minutes.
	// Optional; an empty form adds nothing.
	Ages Ages `json:"ages,omitempty"`
}

// Ages are the words for a line's age: {n} is the count.
type Ages struct {
	Minutes string `json:"minutes,omitempty"`
	Hours   string `json:"hours,omitempty"`
	Days    string `json:"days,omitempty"`
}

// Delimiters mark data blocks in the planner prompt. User text containing them is neutralized.
type Delimiters struct {
	ConversationStart string `json:"conversation_start"`
	ConversationEnd   string `json:"conversation_end"`
	SummaryStart      string `json:"summary_start"`
	SummaryEnd        string `json:"summary_end"`
	MessageStart      string `json:"message_start"`
	MessageEnd        string `json:"message_end"`
}

// All lists the delimiters in a fixed order.
func (d Delimiters) All() []string {
	return []string{d.ConversationStart, d.ConversationEnd, d.SummaryStart, d.SummaryEnd, d.MessageStart, d.MessageEnd}
}

// Memory is indexing text.
type Memory struct {
	SummaryPrompt    string `json:"summary_prompt"` // {body}
	QueryInstruction string `json:"query_instruction"`
	// Room memory (ADR-17, internal/memory/episodes): the episode extraction of a closed conversation
	// segment (L2) and the room-state update (L1). Both answer JSON that code validates; people appear
	// as pN tokens and lines as [mN] labels, never platform IDs.
	EpisodeExtractSystem string `json:"episode_extract_system"` // {bot}
	EpisodeExtract       string `json:"episode_extract"`        // {now} {people} {plans} {lines}
	ChannelStateSystem   string `json:"channel_state_system"`   // {bot}
	ChannelState         string `json:"channel_state"`          // {now} {people} {previous} {lines}
	// Reading room memory into a reply (§5.4.6). Every block opens with ReadHeader (compressed notes,
	// not instructions; the recent conversation wins) and goes into the reply's last user message only
	// when it has content, so a turn without room memory sends the evaluated prompt unchanged.
	ReadHeader    string            `json:"read_header"`
	EpisodesBlock string            `json:"episodes_block"` // {header} {episodes}
	StateBlock    string            `json:"state_block"`    // {header} {topics}
	CardBlock     string            `json:"card_block"`     // {header} {cards}
	KakaoSource   string            `json:"kakao_source"`   // {label}: an episode of a linked KakaoTalk block
	Statuses      map[string]string `json:"statuses"`       // plan status → words (open, decided, done, expired, dropped)
	// Explicit memory requests ("remember this", "that's wrong", "forget that", §5.4.5). ExplicitCues are
	// optional hints: only a message addressed to the bot that contains one is classified (one small
	// model call), the decision is the classifier's. InstructionCues are words of an instruction to the
	// bot; a fact containing one is not stored (memory never carries instructions).
	ExplicitCues    []string `json:"explicit_cues,omitempty"`
	InstructionCues []string `json:"instruction_cues,omitempty"`
	ExplicitSystem  string   `json:"explicit_system"` // {bot}
	// Sensitive asks which facts are sensitive, for guilds.<g>.privacy.no_sensitive (card promotion):
	// {facts} as "[fN] text" lines → {"sensitive": ["f2"]}.
	SensitiveSystem string `json:"sensitive_system"`
	Sensitive       string `json:"sensitive"`
	Explicit        string `json:"explicit"` // {speaker} {message} {last} {items}; the speaker is p1 in the answer
}

// Search is what retrieval needs to know about a language.
type Search struct {
	Particles    string   `json:"particles"`     // suffixes stripped from names (Korean josa)
	Pronouns     []string `json:"pronouns"`      // never person targets
	StopWords    []string `json:"stop_words"`    // no discriminating power in a lexical query
	NgramScripts []string `json:"ngram_scripts"` // scripts indexed as overlapping n-grams: hangul | han | kana
	LatinWords   bool     `json:"latin_words"`   // index Latin-script words whole (else one token per letter)
	Markers      Markers  `json:"markers"`
}

// Markers annotate cuts in packed evidence. Omit* use {n}.
type Markers struct {
	HeadCut    string `json:"head_cut"`
	TailCut    string `json:"tail_cut"`
	OmitBefore string `json:"omit_before"`
	OmitAfter  string `json:"omit_after"`
	OmitMiddle string `json:"omit_middle"`
}

// Tool is a built-in tool's planner text. Description and Route are what the memory planner's menu
// shows (kept byte-identical to the sandbox); the rest describes the tool to the native tool loop
// (docs/plan-memory-conversation.md §5.5.3): purpose, when to use it, when not (and what instead), and
// a hint per parameter. A pack may leave them out; the loop then uses Description.
type Tool struct {
	Description string            `json:"description"`
	Route       string            `json:"route,omitempty"` // one routing hint for planner rule 2
	Purpose     string            `json:"purpose,omitempty"`
	When        string            `json:"when,omitempty"`
	Skip        string            `json:"skip,omitempty"`
	Params      map[string]string `json:"params,omitempty"` // parameter name → how to fill it
}

var (
	loadOnce sync.Once
	packs    map[string]*Pack
	loadErr  error
)

func load() {
	packs, loadErr = loadPacks(packsFS)
}

func loadPacks(fsys fs.FS) (map[string]*Pack, error) {
	loaded := map[string]*Pack{}
	err := fs.WalkDir(fsys, "packs", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
			return err
		}
		raw, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		filename := strings.TrimSuffix(strings.TrimPrefix(path, "packs/"), ".json")
		tag := Normalize(filename)
		if tag == "" {
			return fmt.Errorf("invalid pack filename %q", path)
		}
		if _, exists := loaded[tag]; exists {
			return fmt.Errorf("duplicate normalized pack %q from %q", tag, path)
		}
		p, err := Parse(tag, raw)
		if err != nil {
			return err
		}
		loaded[tag] = p
		return nil
	})
	return loaded, err
}

// Parse decodes and validates one pack.
func Parse(tag string, raw []byte) (*Pack, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var p Pack
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("lang %s: %w", tag, err)
	}
	p.Tag = tag
	if missing := p.missing(); len(missing) > 0 {
		return nil, fmt.Errorf("lang %s: missing %s", tag, strings.Join(missing, ", "))
	}
	if err := validateTranslation(&p); err != nil {
		return nil, fmt.Errorf("lang %s: %w", tag, err)
	}
	return &p, nil
}

func (p *Pack) missing() []string {
	c := p.Chat
	req := map[string]string{
		"chat.planner_header": c.PlannerHeader, "chat.plan_output_schema": c.PlanOutputSchema, "chat.plan_field_rules": c.PlanFieldRules,
		"chat.plan_general_rules": c.PlanGeneralRules, "chat.routing_none": c.RoutingNone, "chat.context_rules": c.ContextRules,
		"chat.tools_header": c.ToolsHeader, "chat.tools_none": c.ToolsNone, "chat.memory_off": c.MemoryOff,
		"chat.assistant_label": c.AssistantLabel, "chat.user_label": c.UserLabel, "chat.user_label_named": c.UserLabelNamed,
		"chat.time_ref": c.TimeRef, "chat.time_ref_missing": c.TimeRefMissing, "chat.live_time": c.LiveTime,
		"chat.conversation_header": c.ConversationHeader, "chat.conversation_empty": c.ConversationEmpty, "chat.summary_header": c.SummaryHeader,
		"chat.truncated": c.Truncated, "chat.default_persona": c.DefaultPersona, "chat.reply_language_rule": c.ReplyLanguageRule,
		"chat.continue_answer": c.ContinueAnswer, "chat.incomplete_answer": c.IncompleteAnswer, "chat.memory_rules": c.MemoryRules,
		"chat.security_rules": c.SecurityRules, "chat.evidence_line": c.EvidenceLine, "chat.correction_line": c.CorrectionLine, "chat.summary_line": c.SummaryLine,
		"chat.none": c.None, "chat.default_speaker": c.DefaultSpeaker, "chat.answer_template": c.AnswerTemplate,
		"chat.tool_policy": c.ToolPolicy, "chat.tool_assumed": c.ToolAssumed, "chat.tool_queued": c.ToolQueued, "chat.tool_schedule": c.ToolSchedule,
		"memory.summary_prompt": p.Memory.SummaryPrompt, "memory.query_instruction": p.Memory.QueryInstruction,
		"memory.episode_extract_system": p.Memory.EpisodeExtractSystem, "memory.episode_extract": p.Memory.EpisodeExtract,
		"memory.channel_state_system": p.Memory.ChannelStateSystem, "memory.channel_state": p.Memory.ChannelState,
		"memory.read_header": p.Memory.ReadHeader, "memory.episodes_block": p.Memory.EpisodesBlock, "memory.state_block": p.Memory.StateBlock,
		"memory.card_block": p.Memory.CardBlock, "memory.kakao_source": p.Memory.KakaoSource,
		"memory.explicit_system": p.Memory.ExplicitSystem, "memory.explicit": p.Memory.Explicit,
		"memory.sensitive_system": p.Memory.SensitiveSystem, "memory.sensitive": p.Memory.Sensitive,
		"features.channel_summary": p.Features.ChannelSummary, "features.channel_summary_incremental": p.Features.ChannelSummaryIncremental,
		"features.image_prompt":   p.Features.ImagePrompt,
		"features.fortune_system": p.Features.FortuneSystem, "features.fortune_public": p.Features.FortunePublic,
		"features.fortune_today": p.Features.FortuneToday, "features.fortune_detail_day": p.Features.FortuneDetailDay,
		"features.fortune_detail_month": p.Features.FortuneDetailMonth, "features.fortune_detail_year": p.Features.FortuneDetailYear,
		"features.fortune_morning": p.Features.FortuneMorning, "features.zodiac_system": p.Features.ZodiacSystem,
		"features.zodiac_public": p.Features.ZodiacPublic, "features.zodiac_private": p.Features.ZodiacPrivate,
		"features.zodiac_ranking":    p.Features.ZodiacRanking,
		"features.room_style_system": p.Features.RoomStyleSystem, "features.room_style": p.Features.RoomStyle,
		"features.room_style_note": p.Features.RoomStyleNote, "features.room_style_header": p.Features.RoomStyleHeader,
		"features.participation_judge_system": p.Features.ParticipationJudgeSystem, "features.participation_judge": p.Features.ParticipationJudge,
		"features.proactive_rules": p.Features.ProactiveRules, "features.scheduled_task": p.Features.ScheduledTask,
		"search.markers.head_cut": p.Search.Markers.HeadCut, "search.markers.tail_cut": p.Search.Markers.TailCut,
		"search.markers.omit_before": p.Search.Markers.OmitBefore, "search.markers.omit_after": p.Search.Markers.OmitAfter,
		"search.markers.omit_middle": p.Search.Markers.OmitMiddle,
		"practice.answer":            p.Practice.Answer, "practice.no_memory": p.Practice.NoMemory,
		"conversation.refer_system": p.Conversation.ReferSystem, "conversation.refer": p.Conversation.Refer,
		"conversation.name_call_system": p.Conversation.NameCallSystem, "conversation.name_call": p.Conversation.NameCall,
		"conversation.banter_rules":  p.Conversation.BanterRules,
		"conversation.follow_system": p.Conversation.FollowSystem, "conversation.follow": p.Conversation.Follow,
		"thread.offer": p.Thread.Offer, "thread.open": p.Thread.Open, "thread.stay": p.Thread.Stay, "thread.stop": p.Thread.Stop,
		"thread.name": p.Thread.Name, "thread.seed": p.Thread.Seed, "thread.seed_plain": p.Thread.SeedPlain, "thread.bye": p.Thread.Bye,
		"thread.not_yours": p.Thread.NotYours, "thread.off": p.Thread.Off, "thread.recap": p.Thread.Recap, "thread.summary_system": p.Thread.SummarySystem,
		"thread.summary": p.Thread.Summary, "shape.more": p.Shape.More,
	}
	for k, v := range map[string]int{"time.relative_days": len(p.Time.RelativeDays), "time.hours": len(p.Time.Hours),
		"time.after": len(p.Time.After), "time.units": len(p.Time.Units), "time.am": len(p.Time.AM), "time.pm": len(p.Time.PM),
		"time.plan_words": len(p.Time.PlanWords), "time.question": len(p.Time.Question)} {
		if v == 0 {
			req[k] = ""
		}
	}
	for i, w := range p.Time.Weekdays {
		if len(w) == 0 {
			req[fmt.Sprintf("time.weekdays[%d]", i)] = ""
		}
	}
	for i, d := range c.Delimiters.All() {
		req[fmt.Sprintf("chat.delimiters[%d]", i)] = d
	}
	for _, k := range []string{"not_attempted", "empty_in_allowed_scope", "insufficient_authorized_candidates", "memory_search_disabled", "memory_unavailable"} {
		req["chat.rag_notes."+k] = c.RagNotes[k]
	}
	var out []string
	for k, v := range req {
		if strings.TrimSpace(v) == "" {
			out = append(out, k)
		}
	}
	if !strings.Contains(c.PlanGeneralRules, "{routing}") {
		out = append(out, "chat.plan_general_rules{routing}")
	}
	if !strings.Contains(p.Memory.SummaryPrompt, "{body}") {
		out = append(out, "memory.summary_prompt{body}")
	}
	for key, tpl := range map[string]string{"memory.episode_extract": p.Memory.EpisodeExtract, "memory.channel_state": p.Memory.ChannelState} {
		if tpl != "" && !strings.Contains(tpl, "{lines}") {
			out = append(out, key+"{lines}")
		}
	}
	sort.Strings(out)
	return out
}

func all() map[string]*Pack {
	loadOnce.Do(load)
	if loadErr != nil {
		panic("lang: embedded pack invalid: " + loadErr.Error()) // build error, caught by tests
	}
	return packs
}

// Available lists the embedded pack tags.
func Available() []string {
	var out []string
	for t := range all() {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Normalize follows i18n's catalog/pack tag contract, preserving zh-Hans as a
// filename while reducing ordinary region variants to their primary language.
func Normalize(tag string) string {
	if strings.TrimSpace(tag) == "" {
		return "" // Get("") must not select English implicitly
	}
	return i18n.Normalize(tag)
}

// Get returns the pack for tag, or false when no pack exists for that language.
func Get(tag string) (*Pack, bool) {
	p, ok := all()[Normalize(tag)]
	return p, ok
}

// For returns the first available pack among tags, then English, then Default.
func For(tags ...string) *Pack {
	for _, t := range tags {
		if t == "" {
			continue
		}
		if p, ok := Get(t); ok {
			return p
		}
	}
	if p, ok := Get("en"); ok {
		return p
	}
	return all()[Default]
}

// MergeSearch combines the search rules of several languages (a room can mix languages): word lists
// and scripts are unioned, markers come from the first language. extraPronouns (the bot's own names)
// are added to the pronoun list.
func MergeSearch(tags []string, extraPronouns []string) Search {
	var out Search
	seenP, seenW, seenS := map[string]bool{}, map[string]bool{}, map[string]bool{}
	particles := map[rune]bool{}
	first := true
	for _, t := range tags {
		p, ok := Get(t)
		if !ok {
			continue
		}
		s := p.Search
		if first {
			out.Markers = s.Markers
			first = false
		}
		for _, r := range s.Particles {
			if !particles[r] {
				particles[r] = true
				out.Particles += string(r)
			}
		}
		for _, w := range s.Pronouns {
			if !seenP[w] {
				seenP[w] = true
				out.Pronouns = append(out.Pronouns, w)
			}
		}
		for _, w := range s.StopWords {
			if !seenW[w] {
				seenW[w] = true
				out.StopWords = append(out.StopWords, w)
			}
		}
		for _, sc := range s.NgramScripts {
			if !seenS[sc] {
				seenS[sc] = true
				out.NgramScripts = append(out.NgramScripts, sc)
			}
		}
		out.LatinWords = out.LatinWords || s.LatinWords
	}
	if first {
		return MergeSearch([]string{Default}, extraPronouns)
	}
	for _, w := range extraPronouns {
		if w = strings.TrimSpace(w); w != "" && !seenP[w] {
			seenP[w] = true
			out.Pronouns = append(out.Pronouns, w)
		}
	}
	return out
}

// Fill replaces {name} placeholders named in kv (name, value pairs) in one pass over the template.
// Values are never re-scanned, so user text containing "{memory}" stays literal, and braces that are
// not a listed placeholder (JSON examples in prompts) are kept.
func Fill(tpl string, kv ...string) string {
	if !strings.Contains(tpl, "{") || len(kv) < 2 {
		return tpl
	}
	vals := make(map[string]string, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		vals[kv[i]] = kv[i+1]
	}
	var b strings.Builder
	b.Grow(len(tpl))
	for i := 0; i < len(tpl); {
		if tpl[i] == '{' {
			if j := strings.IndexByte(tpl[i+1:], '}'); j >= 0 {
				if v, ok := vals[tpl[i+1:i+1+j]]; ok {
					b.WriteString(v)
					i += j + 2
					continue
				}
			}
		}
		b.WriteByte(tpl[i])
		i++
	}
	return b.String()
}
