package application

import (
	"database/sql"
	"regexp"
	"slices"
	"sort"
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
var selfRequest = regexp.MustCompile(`(?i)(?:^|[^\pL\pN])(?:(?:나|저)(?:에\s*대해|에\s*관해|는?\s*(?:어떤\s*사람|누구)|를?\s*(?:소개|그려|기억))|(?:난|전)\s*(?:어떤\s*사람|누구)|(?:내|제)(?:\s*(?:취향|성격|별명|소개|특징|근황)|가\s*좋아하)|(?:about me|describe me|draw me|who am i|my (?:hobbies|interests|nickname|personality))\b)`)

type turnNames struct {
	current map[string]string
	aliases map[string][]string
	denied  map[string]bool
}

func readPersonNames(tx *sql.Tx, guild string) (turnNames, error) {
	n := turnNames{map[string]string{}, map[string][]string{}, map[string]bool{}}
	denied, err := withdrawnTx(tx, privacy.Memory, privacy.Profile)
	if err != nil {
		return n, err
	}
	for account := range denied {
		n.denied[personRef(account)] = true
	}
	rows, err := tx.Query(`SELECT person_ref,label FROM person_alias WHERE guild_id=? ORDER BY last_seen DESC,label`, guild)
	if err != nil {
		return n, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref, label string
		if err := rows.Scan(&ref, &label); err != nil {
			return n, err
		}
		if ref == "" || strings.TrimSpace(label) == "" {
			continue
		}
		if n.current[ref] == "" {
			n.current[ref] = label
		}
		key := strings.ToLower(label)
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
			for _, suffix := range []string{"형", "누나", "님", "씨"} {
				if strings.Contains(word, label+suffix) {
					ok = ok || matches(strings.Replace(word, label+suffix, label, 1), label)
				}
			}
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

func (s *Service) targetPeople(db *storage.Store, in ChatInput, planner []string) []turnPerson {
	if in.GuildID == "" || in.GuildID == "0" || in.SpeakerID == "" || !s.MemoryAllowed(in.SpeakerID) {
		return nil
	}
	lines := s.liveLines(in.GuildID, in.ChannelID)
	bot := personRef(s.botAccount(lines))
	tx, err := db.Reader().Begin()
	if err != nil {
		return nil
	}
	defer tx.Rollback()
	names, err := readPersonNames(tx, in.GuildID)
	if err != nil {
		return nil
	}
	names.denied[bot] = true
	caller := personRef(in.SpeakerID)
	ownOnly := s.guildPrivacy(in.GuildID).OwnCardOnly
	var out []turnPerson
	seen := map[string]bool{}
	others := 0
	add := func(ref string) {
		if names.current[ref] == "" || seen[ref] || names.denied[ref] || ref != caller && (ownOnly || others == chat.MaxOtherCards) {
			return
		}
		p := turnPerson{ref, names.current[ref], ref == caller}
		if p.caller {
			out = append([]turnPerson{p}, out...)
		} else {
			out = append(out, p)
			others++
		}
		seen[ref] = true
	}
	for _, m := range mentionPattern.FindAllStringSubmatch(in.Message, -1) {
		add(personRef(m[1]))
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
			add(personRef(account))
			break
		}
	}
	// A Korean display name can be inflected even when the index language is English.
	doc := s.Doc()
	matches := search.NameMatcher(lang.MergeSearch(append(slices.Clone(doc.Memory.Languages), "ko"), doc.BotNames()))
	for _, name := range planner {
		for _, ref := range names.match(name, matches) {
			add(ref)
		}
	}
	for _, ref := range names.match(mentionPattern.ReplaceAllString(in.Message, " "), matches) {
		add(ref)
	}
	if selfRequest.MatchString(in.Message) {
		add(caller)
	}
	return out
}
