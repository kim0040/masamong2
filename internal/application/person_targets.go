package application

import (
	"database/sql"
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"masamong/internal/chat"
	"masamong/internal/features/privacy"
	"masamong/internal/lang"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
	"masamong/internal/storage"
)

type turnPerson struct {
	ref, label string
	caller     bool
}

// Match whole requests about the caller with bounded predicates ("날 기억해?",
// "나에 대해 뭐 알아?"), not first-person asides ("나는 기억력이 좋아") or other
// people's names ("내가 민수가 궁금해"). Exact self mentions and replies also work.
const selfEnd = `\s*[ㅋㅎㅠㅜ]*\s*(?:[?？!！.。~…\n]|$)`

const selfRecall = `기억(?:해|해요|하니|하냐|하나|하나요|하지|하죠|하세요|하시나요|하고\s*있어|하고\s*있니|하고\s*있어요|나|나요)`

var selfRequest = regexp.MustCompile(`(?i)(?:(?:^|[^\pL\pN])(?:` + strings.Join([]string{
	`(?:날|나를|저를|절)\s*(?:아직\s*|혹시\s*)?` + selfRecall + selfEnd,
	`(?:나|저)\s*(?:아직\s*|혹시\s*)?` + selfRecall + `\s*[ㅋㅎㅠㅜ]*\s*[?？]`,
	`(?:나|저)에\s*(?:대해서?|관해서?|대한)\s*(?:뭐|뭘|무엇|어떤|아는|알고|알아|알려|기억|말해|얘기|이야기|설명|정리|요약|어떻게|소개)`,
	`(?:내|제|나의|저의)\s*정보`,
	`(?:내가|제가|난|나는|저는|전)\s*누구(?:야|게|냐|니|지|일까|일까요|인가|인가요|예요|에요|죠|였지|였더라|인지\s*알아|인지\s*알아요)?` + selfEnd,
	`(?:나|저|난|전|나는|저는|내가|제가)\s*어떤\s*사람(?:이야|이에요|이예요|인가|인가요|인지|같아|같니|같아요|일까|일까요|이니|이냐|이지|이었어|이었지|인지\s*알아)` + selfEnd,
	`(?:나|저|난|전|나는|저는|내가|제가)\s*(?:뭘|뭐를|무엇을|뭐|어떤\s*걸|어떤\s*것을)\s*(?:좋아|싫어)(?:해|해요|하지|하죠|하니|하냐|하나|하나요|할까|할까요|했지|했더라|하더라|하는지\s*알아)?` + selfEnd,
	`(?:내|제|나의|저의)\s*(?:취향|성격|별명|특징|근황|취미|관심사)(?:이|가|은|는|을|를)?\s*(?:뭐|뭘|뭔지|무엇|어때|어떤|알아|아니|알고|기억|말해|알려|맞혀|맞춰)`,
	`(?:나|저)한테\s*(?:뭐가\s*|어떤\s*게\s*)?어울(?:려|릴까|릴까요|리는\s*(?:거|게|걸|건|것))`,
	`(?:날|나를|저를|절|나|저|내|제)\s*소개\s*(?:좀\s*)?(?:해\s*줘|해\s*주세요|해\s*봐|해\s*줄래|해\s*줄\s*수\s*있어|해)` + selfEnd,
	`(?:날|나를|저를|절|나|저)\s*(?:좀\s*)?그려\s*(?:줘|주세요|봐|줄래|줄\s*수\s*있어)` + selfEnd,
	`(?:what|anything)\s+(?:do|did)\s+you\s+(?:know|remember|recall)\s+about\s+me\b`,
	`tell\s+me\s+about\s+(?:me|myself)\b`,
	`(?:describe|draw)\s+me\b`,
	`(?:do|did)\s+you\s+remember\s+me\b`,
	`who\s+am\s+i\b`,
	`what\s+kind\s+of\s+person\s+am\s+i\b`,
	`what\s+do\s+i\s+(?:like|enjoy|love|hate)\b`,
	`what(?:'s|\s+is|\s+are|\s+was|\s+were)\s+my\s+(?:hobby|hobbies|interests|nickname|personality)\b`,
	`(?:qué\s+sabes|qué\s+recuerdas|háblame)\s+(?:de|sobre)\s+mí`,
	`descríbeme`,
	`quién\s+soy`,
	`qué\s+me\s+gusta`,
	`me\s+recuerdas`,
}, "|") + `)|(?:私|僕|わたし|俺|あたし)(?:について(?:何|なに|知って|覚えて|教えて|どう)|のこと(?:を)?(?:覚えて|知って|どう思)|を覚えて|の趣味(?:は|って)(?:何|なに)|はどんな人|は誰)|我是谁|我是什么样的人|我的爱好是什么|我喜欢什么|介绍一下我|关于我(?:你知道|你记得|的事)|你(?:还)?记得我吗)`)

// "그 날 기억해?" asks about a day (날/절 are also nouns), not about the caller.
var selfNounBefore = map[string]bool{"그": true, "이": true, "저": true, "그런": true, "이런": true, "저런": true, "어느": true, "첫": true,
	"마지막": true, "어떤": true, "옛": true, "지난": true, "다음": true, "그다음": true, "좋은": true, "그때": true}

func selfIntent(message string) bool {
	for _, m := range selfRequest.FindAllStringIndex(message, -1) {
		match := strings.TrimLeftFunc(message[m[0]:m[1]], func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		if strings.HasPrefix(match, "날") || strings.HasPrefix(match, "절") {
			before := strings.Fields(message[:m[1]-len(match)])
			if len(before) > 0 && selfNounBefore[before[len(before)-1]] {
				continue
			}
		}
		return true
	}
	return false
}

type turnNames struct {
	current      map[string]string
	sources      map[string]string
	aliases      map[string][]string
	accounts     map[string]string // ref → account, for the name check
	keys         []string
	denied       map[string]bool
	tx           *sql.Tx
	guild, rooms string
}

func (s *Service) deniedPeople(tx *sql.Tx, guild string) (map[string]bool, error) {
	refs := map[string]bool{}
	denied, err := withdrawnTx(tx, privacy.Memory, privacy.Profile)
	if err != nil {
		return nil, err
	}
	for account := range denied {
		refs[personRef(account)] = true
	}
	// The same account lists as BlockedIn; episode subjects only carry refs.
	doc := s.Doc()
	for _, list := range [][]string{doc.Access.BlockedUsers, doc.Guilds[guild].BlockedUsers} {
		for _, id := range list {
			refs[personRef(id)] = true
		}
	}
	return refs, nil
}

const liveAliasSQL = ` JOIN memory_observations o ON o.observation_id=a.last_source
		WHERE a.guild_id=? AND a.room IN (SELECT key FROM json_each(?)) AND o.deleted_at='' AND o.edited_at=''
		AND NOT EXISTS(SELECT 1 FROM privacy_tombstones p WHERE (p.kind='message' AND p.source_id=o.native_message_id) OR (p.kind='account' AND p.source_id=o.actor_account))`

func (n turnNames) label(ref string) string {
	if v, ok := n.current[ref]; ok {
		return v
	}
	var label, source string
	if n.tx.QueryRow(`SELECT a.label,a.last_source FROM person_alias a`+liveAliasSQL+` AND a.person_ref=? ORDER BY a.last_seen DESC,a.label LIMIT 1`, n.guild, n.rooms, ref).Scan(&label, &source) == nil {
		n.sources[ref] = source
	}
	n.current[ref] = label
	return label
}

// Discord display names are at most 32 characters (37 with an old #0000 tag);
// a longer stored label is never matched and never checked.
const maxNameRunes = 40

// Look up bounded starts in the message, not every name in the guild. For
// unusually long/punctuation-heavy prose prefer missing a name to guessing.
func aliasKeys(text string) []string {
	if len(text) > 1024 {
		return nil
	}
	runes := []rune(strings.ToLower(text))
	var keys []string
	for i, r := range runes {
		if unicode.IsSpace(r) || i > 0 && (unicode.IsLetter(runes[i-1]) || unicode.IsDigit(runes[i-1])) {
			continue
		}
		for j := i + 2; j <= min(len(runes), i+maxNameRunes); j++ {
			if !unicode.IsSpace(runes[j-1]) {
				if len(keys) == 1024 {
					return nil
				}
				keys = append(keys, string(runes[i:j]))
			}
		}
	}
	return keys
}

func (s *Service) readPersonNames(tx *sql.Tx, guild string, rooms map[string]bool, text string) (turnNames, error) {
	denied, err := s.deniedPeople(tx, guild)
	raw, _ := json.Marshal(rooms)
	n := turnNames{current: map[string]string{}, sources: map[string]string{}, aliases: map[string][]string{}, accounts: map[string]string{},
		denied: denied, tx: tx, guild: guild, rooms: string(raw)}
	if err != nil {
		return n, err
	}
	n.keys = aliasKeys(text)
	if len(n.keys) == 0 {
		return n, nil
	}
	rawKeys, _ := json.Marshal(n.keys)
	rows, err := tx.Query(`SELECT a.person_ref,a.folded,o.actor_account FROM person_alias a INDEXED BY idx_person_alias_name`+liveAliasSQL+`
		AND a.folded IN (SELECT value FROM json_each(?)) AND (SELECT cursor FROM person_alias_fill WHERE id=1)=-1`, guild, n.rooms, string(rawKeys))
	if err != nil {
		return n, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref, key, account string
		if err := rows.Scan(&ref, &key, &account); err != nil {
			return n, err
		}
		if !slices.Contains(n.aliases[key], ref) {
			n.aliases[key] = append(n.aliases[key], ref)
		}
		n.accounts[ref] = account
	}
	return n, rows.Err()
}

type nameSpan struct{ lo, hi int }

// spans are the words of text (lowercased) that label names: the label plus
// an allowed particle or honorific.
func nameSpans(text, label string, matches func(string, string) bool) []nameSpan {
	var out []nameSpan
	for from := 0; from < len(text); {
		offset := strings.Index(text[from:], label)
		if offset < 0 {
			break
		}
		lo := from + offset
		hi := lo + len(label)
		from = hi
		for lo > 0 {
			r, size := utf8.DecodeLastRuneInString(text[:lo])
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
				break
			}
			lo -= size
		}
		for hi < len(text) {
			r, size := utf8.DecodeRuneInString(text[hi:])
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
				break
			}
			hi += size
		}
		if matches(text[lo:hi], label) {
			out = append(out, nameSpan{lo, hi})
		}
	}
	return out
}

// unique checks a span resolved to one indexed account against the captured
// messages: any other account seen in these rooms under a label that names an
// overlapping word (hidden or not yet indexed) makes it ambiguous.
func (n turnNames) unique(text string, lo, hi int, ref string, matches func(string, string) bool) bool {
	account := n.accounts[ref]
	if account == "" {
		return false
	}
	var labels []string
	seen := map[string]bool{}
	for _, key := range n.keys {
		if seen[key] {
			continue
		}
		seen[key] = true
		for _, sp := range nameSpans(text, key, matches) {
			if sp.lo < hi && lo < sp.hi {
				labels = append(labels, key)
				break
			}
		}
	}
	taken, err := store.OtherPersonNamed(n.tx, labels, account, n.rooms)
	return err == nil && !taken
}

// Group matches by their position in the request: two accounts matching the
// same word resolve neither, without suppressing a different unambiguous name.
func (n turnNames) match(text string, matches func(string, string) bool) []string {
	at := map[nameSpan][]string{}
	text = strings.ToLower(text)
	for label, refs := range n.aliases {
		for _, k := range nameSpans(text, label, matches) {
			at[k] = uniqueStrings(append(at[k], refs...))
		}
	}
	var keys []nameSpan
	for k := range at {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].lo < keys[j].lo || keys[i].lo == keys[j].lo && keys[i].hi > keys[j].hi
	})
	var out []string
	for i := 0; i < len(keys); {
		lo, end, refs := keys[i].lo, keys[i].hi, at[keys[i]]
		i++
		for i < len(keys) && keys[i].lo < end {
			refs = uniqueStrings(append(refs, at[keys[i]]...))
			end = max(end, keys[i].hi)
			i++
		}
		if len(refs) == 1 && n.unique(text, lo, end, refs[0], matches) {
			out = append(out, refs[0])
		}
	}
	return out
}

func (s *Service) targetPeople(db *storage.Store, in ChatInput, _ []string) []turnPerson {
	tx, err := db.Reader().Begin()
	if err != nil {
		return nil
	}
	defer tx.Rollback()
	rooms := s.personRooms(tx, in.GuildID, in.ChannelID, s.guildPrivacy(in.GuildID).CardChannelOnly)
	people, _ := s.resolvePeople(tx, in, rooms)
	return people
}

func (s *Service) resolvePeople(tx *sql.Tx, in ChatInput, rooms map[string]bool) ([]turnPerson, turnNames) {
	if len(rooms) == 0 || in.GuildID == "" || in.GuildID == "0" || in.SpeakerID == "" || s.BlockedIn(in.GuildID, in.SpeakerID) || !s.MemoryAllowed(in.SpeakerID) {
		return nil, turnNames{}
	}
	text := mentionPattern.ReplaceAllString(in.Message, " ")
	names, err := s.readPersonNames(tx, in.GuildID, rooms, text)
	if err != nil {
		return nil, names
	}
	lines := s.liveLines(in.GuildID, in.ChannelID)
	bot := personRef(s.botAccount(lines))
	names.denied[bot] = true
	caller := personRef(in.SpeakerID)
	ownOnly := s.guildPrivacy(in.GuildID).OwnCardOnly
	var out []turnPerson
	seen := map[string]bool{}
	add := func(ref string, explicit bool) {
		if ref == "" || seen[ref] || names.denied[ref] || ref != caller && ownOnly || ref == caller && !explicit || names.label(ref) == "" {
			return
		}
		p := turnPerson{ref, names.current[ref], ref == caller}
		if p.caller {
			out = append([]turnPerson{p}, out...)
		} else {
			out = append(out, p)
		}
		seen[ref] = true
	}
	for _, m := range mentionPattern.FindAllStringSubmatch(in.Message, -1) {
		add(personRef(m[1]), true)
	}
	for _, l := range lines {
		if l.ID == in.MessageID && l.ReplyTo != "" && !l.ReplyToBot {
			account := l.ReplyAuthorID
			if account == "" {
				account = authorOfLine(lines, l.ReplyTo)
			}
			if account == "" {
				_ = tx.QueryRow(`SELECT actor_account FROM memory_observations WHERE platform='discord' AND room=?
					AND native_message_id=? AND deleted_at='' AND record_kind=?`, roomOf(in.GuildID, in.ChannelID), l.ReplyTo, corpus.HumanText).Scan(&account)
			}
			add(personRef(account), true)
			break
		}
	}
	// A Korean display name can be inflected even when the index language is English.
	// Any valid planner target already has one of these exact signals. A guess
	// cannot add an account, change the order, or displace an explicit target.
	if len(names.aliases) > 0 {
		doc := s.Doc()
		matches := search.NameMatcher(lang.MergeSearch(append(slices.Clone(doc.Memory.Languages), "ko", "ja"), doc.BotNames()))
		for _, ref := range names.match(text, matches) {
			add(ref, false)
		}
	}
	if selfIntent(in.Message) {
		add(caller, true)
	}
	limit := chat.MaxOtherCards
	if len(out) > 0 && out[0].caller {
		limit++
	}
	return out[:min(len(out), limit)], names
}

// Two explicitly addressed accounts may have the same current display name.
// Keep their headers and subject tokens distinct without exposing account IDs.
func distinguishNames(people []turnPerson, labels map[string]string) {
	count, seen := map[string]int{}, map[string]int{}
	for _, p := range people {
		count[strings.ToLower(labels[p.ref])]++
	}
	for _, p := range people {
		k := strings.ToLower(labels[p.ref])
		if k != "" && count[k] > 1 {
			seen[k]++
			labels[p.ref] += " (" + strconv.Itoa(seen[k]) + ")"
		}
	}
}
