package application

import (
	"database/sql"
	"encoding/json"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

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

// A request about the caller is a whole sentence from the language packs'
// memory.self_requests ("날 기억해?", "who am I"), not a clause inside other
// words ("내 정보통신 과제", "Tú me recuerdas a mi hermana"). Only the last
// sentence counts, so a quoted or reported request followed by more words
// ("你还记得我吗？他问。") is not one. Exact self mentions and replies also work.
type selfTable struct {
	phrases map[string]bool // letters and digits → counts only as a question
	trims   []string        // longest first
}

var selfRequests = sync.OnceValue(func() selfTable {
	t := selfTable{phrases: map[string]bool{}}
	seen := map[string]bool{}
	for _, tag := range lang.Available() {
		p, _ := lang.Get(tag)
		for _, w := range p.Memory.SelfTrim {
			if w = selfLetters(w); w != "" && !seen[w] {
				seen[w] = true
				t.trims = append(t.trims, w)
			}
		}
	}
	sort.Slice(t.trims, func(i, j int) bool { return len(t.trims[i]) > len(t.trims[j]) })
	for _, tag := range lang.Available() {
		p, _ := lang.Get(tag)
		for _, entry := range p.Memory.SelfRequests {
			phrases, _ := lang.Expand(entry) // validated when the pack loads
			for _, phrase := range phrases {
				phrase, question := strings.CutSuffix(phrase, "?")
				key := t.key(phrase)
				if key == "" {
					continue
				}
				if q, ok := t.phrases[key]; ok {
					question = question && q // an entry without "?" wins
				}
				t.phrases[key] = question
			}
		}
	}
	return t
})

// selfLetters keeps the letters and digits of s, lower case, with Latin
// accents folded ("¿Quién soy?" → "quiensoy").
func selfLetters(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 0x80 && unicode.Is(unicode.Latin, r) {
			if d := []rune(norm.NFD.String(string(r))); len(d) > 1 && d[0] < 0x80 {
				r = d[0]
			}
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Mc, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (t selfTable) key(s string) string {
	k := selfLetters(s)
	for trimmed := true; trimmed; {
		trimmed = false
		for _, w := range t.trims {
			if rest, ok := strings.CutPrefix(k, w); ok {
				k, trimmed = rest, true
			}
			if rest, ok := strings.CutSuffix(k, w); ok {
				k, trimmed = rest, true
			}
		}
	}
	return k
}

const sentenceEnds = ".!?。！？…．｡؟\n"

// A whole request is short; a longer last sentence is not one.
const maxSelfSentence = 512

// selfIntent reports whether the message's last sentence asks about its
// speaker. The bot's own mention and the names it is called by (with a call
// ending, withoutBotNames) are not part of the request.
func selfIntent(message, bot string, names []string) bool {
	if bot != "" {
		message = strings.NewReplacer("<@"+bot+">", " ", "<@!"+bot+">", " ").Replace(message)
	}
	var sentences []string
	var questions []bool
	start, ended := 0, true
	for i, r := range message {
		if !strings.ContainsRune(sentenceEnds, r) {
			if ended {
				start, ended = i, false
			}
			continue
		}
		if !ended {
			sentences, questions, ended = append(sentences, message[start:i]), append(questions, false), true
		}
		if len(questions) > 0 && (r == '?' || r == '？' || r == '؟') {
			questions[len(questions)-1] = true
		}
	}
	if !ended {
		sentences, questions = append(sentences, message[start:]), append(questions, false)
	}
	t := selfRequests()
	for i := len(sentences) - 1; i >= 0; i-- {
		if len(sentences[i]) > maxSelfSentence {
			return false
		}
		key := t.key(withoutBotNames(sentences[i], names))
		if key == "" || slices.ContainsFunc(names, func(n string) bool { return t.key(n) == key }) {
			continue // laughter, an emoji or the bot's name after the request
		}
		question, ok := t.phrases[key]
		return ok && (!question || questions[i])
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
	botID := s.botAccount(lines)
	bot := personRef(botID)
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
	if selfIntent(in.Message, botID, s.Doc().BotNames()) {
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
