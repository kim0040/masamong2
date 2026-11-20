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
	"masamong/internal/storage"
)

type turnPerson struct {
	ref, label string
	caller     bool
}

// Match requests about the caller, not incidental first-person subjects
// ("I wonder what Minsoo likes"). Exact self mentions also work.
var selfRequest = regexp.MustCompile(`(?i)(?:^|[^\pL\pN])(?:(?:나|저)(?:에\s*(?:대해|관해)|한테\s*어울)|(?:나[는도가를]?|저[는도가를]?|내가|제가|난|전|날|절)\s*(?:(?:뭘|뭐를|무엇을|뭐|어떤\s*걸)\s*)?(?:어떤\s*사람|누구|좋아|싫어|소개|그려|기억)|(?:나의|저의|내|제)\s*(?:취향|성격|별명|소개|특징|근황|취미)|(?:about me|describe me|draw me|remember me|who am i|what do i like|my (?:hobbies|interests|nickname|personality))\b|(?:sobre mí|descríbeme|quién soy|qué me gusta)\b|(?:私|僕|わたし)(?:について|の趣味|はどんな人)|(?:我是谁|我的爱好|介绍我|关于我))`)

type turnNames struct {
	current      map[string]string
	sources      map[string]string
	aliases      map[string][]string
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
		for j := i + 2; j <= len(runes); j++ {
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
	n := turnNames{current: map[string]string{}, sources: map[string]string{}, aliases: map[string][]string{}, denied: denied, tx: tx, guild: guild, rooms: string(raw)}
	if err != nil {
		return n, err
	}
	keys := aliasKeys(text)
	if len(keys) == 0 {
		return n, nil
	}
	rawKeys, _ := json.Marshal(keys)
	rows, err := tx.Query(`SELECT a.person_ref,a.folded FROM person_alias a INDEXED BY idx_person_alias_name`+liveAliasSQL+`
		AND a.folded IN (SELECT value FROM json_each(?)) AND (SELECT cursor FROM person_alias_fill WHERE id=1)=-1`, guild, n.rooms, string(rawKeys))
	if err != nil {
		return n, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref, key string
		if err := rows.Scan(&ref, &key); err != nil {
			return n, err
		}
		if !slices.Contains(n.aliases[key], ref) {
			n.aliases[key] = append(n.aliases[key], ref)
		}
	}
	return n, rows.Err()
}

// Group matches by their position in the request: two accounts matching the
// same word resolve neither, without suppressing a different unambiguous name.
func (n turnNames) match(text string, matches func(string, string) bool) []string {
	type span struct{ lo, hi int }
	at := map[span][]string{}
	text = strings.ToLower(text)
	for label, refs := range n.aliases {
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
			word := text[lo:hi]
			ok := matches(word, label)
			if ok {
				k := span{lo, hi}
				at[k] = uniqueStrings(append(at[k], refs...))
			}
		}
	}
	var keys []span
	for k := range at {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].lo < keys[j].lo || keys[i].lo == keys[j].lo && keys[i].hi > keys[j].hi
	})
	var out []string
	for i := 0; i < len(keys); {
		end, refs := keys[i].hi, at[keys[i]]
		i++
		for i < len(keys) && keys[i].lo < end {
			refs = uniqueStrings(append(refs, at[keys[i]]...))
			end = max(end, keys[i].hi)
			i++
		}
		if len(refs) == 1 {
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
	if selfRequest.MatchString(in.Message) {
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
