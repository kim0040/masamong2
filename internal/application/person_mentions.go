package application

import (
	"slices"

	"masamong/internal/lang"
	"masamong/internal/memory/episodes"
	"masamong/internal/memory/search"
	"masamong/internal/storage"
)

// mentionedPeople are the people a run of lines names without writing in it, for the people section of
// an extraction prompt (live extraction, regeneration and the paid backfill): the accounts of <@id>
// mentions, shown with their latest label in the server (link mention), and the exact single-account
// alias matches of this server (link name). It uses the signals a chat turn resolves
// (readPersonNames and turnNames.match, read-only), so an ambiguous name resolves to nobody. The
// bot's own account, withdrawn or blocked people, the authors of the lines and the person who wrote
// the line a name sits in are never listed. Phase 0 targeting is unchanged: this only reads.
func (s *Service) mentionedPeople(db *storage.Store, guild string, msgs []episodes.Message, botRef string) ([]episodes.Person, error) {
	authors := map[string]bool{}
	for _, m := range msgs {
		if !m.Bot && m.Account != "" {
			authors[m.Account] = true
		}
	}
	rooms := map[string]bool{}
	rows, err := db.Reader().Query(`SELECT room FROM memory_rooms WHERE platform='discord' AND room LIKE ?`, "discord:"+guild+":%")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var room string
		if rows.Scan(&room) == nil {
			rooms[room] = true
		}
	}
	rows.Close()
	tx, err := db.Reader().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	doc := s.Doc()
	matches := search.NameMatcher(lang.MergeSearch(append(slices.Clone(doc.Memory.Languages), "ko", "ja"), doc.BotNames()))
	base, err := s.readPersonNames(tx, guild, rooms, "") // labels and consent; no alias lookup
	if err != nil {
		return nil, err
	}
	var out []episodes.Person
	seen := map[string]bool{}
	for _, m := range msgs {
		if m.Bot {
			continue
		}
		for _, sub := range mentionPattern.FindAllStringSubmatch(m.Text, -1) {
			id := sub[1]
			ref := personRef(id)
			if authors[id] || seen[ref] || ref == botRef || base.denied[ref] {
				continue
			}
			if label := base.label(ref); label != "" {
				seen[ref] = true
				out = append(out, episodes.Person{Ref: ref, Label: label, ID: id, Link: episodes.LinkMention})
			}
		}
		text := mentionPattern.ReplaceAllString(m.Text, " ")
		names, err := s.readPersonNames(tx, guild, rooms, text)
		if err != nil {
			return nil, err
		}
		if len(names.aliases) == 0 {
			continue
		}
		for _, ref := range names.match(text, matches) {
			if seen[ref] || names.denied[ref] || ref == botRef || ref == personRef(m.Account) {
				continue
			}
			if label := base.label(ref); label != "" {
				seen[ref] = true
				out = append(out, episodes.Person{Ref: ref, Label: label, Link: episodes.LinkName})
			}
		}
	}
	return out, nil
}

// liveBotRef is the stored reference of the bot's own account, for the people section of a prompt ("" when
// unknown): the connected account, else the account of a stored bot line.
func (s *Service) liveBotRef(lines []roomLine) string {
	if s.Discord() != nil {
		if id, _ := s.Discord().BotUser(); id != "" {
			return personRef(id)
		}
	}
	for _, l := range lines {
		if l.Bot && l.Account != "" {
			return personRef(l.Account)
		}
	}
	return ""
}
