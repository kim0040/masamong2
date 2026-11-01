package application

import (
	"context"
	"database/sql"
	"sort"
	"strings"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/lang"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/search"
)

// Memory review lets an operator act on one retrieved chunk (leaf): exclude it (it never becomes
// evidence again) or attach a correction (the answer model sees the operator's note next to it).
// The source messages are never edited (RAW_DATA_POLICY): a review is a separate row keyed by leaf.
// People linking writes memory.person_aliases, the same shape the sandbox uses.

var memoryReviewSQLite = `CREATE TABLE IF NOT EXISTS memory_reviews (
	leaf_id TEXT PRIMARY KEY,
	room TEXT NOT NULL,
	kind TEXT NOT NULL,
	note TEXT NOT NULL DEFAULT '',
	excerpt TEXT NOT NULL DEFAULT '',
	at TEXT NOT NULL,
	actor TEXT NOT NULL DEFAULT ''
)`

var memoryReviewTiDB = `CREATE TABLE IF NOT EXISTS memory_reviews (
	leaf_id VARCHAR(64) NOT NULL PRIMARY KEY,
	room VARCHAR(160) NOT NULL,
	kind VARCHAR(16) NOT NULL,
	note TEXT NOT NULL,
	excerpt TEXT NOT NULL,
	at VARCHAR(64) NOT NULL,
	actor VARCHAR(128) NOT NULL DEFAULT ''
)`

type memoryReview struct {
	LeafID  string `json:"leaf_id"`
	Room    string `json:"room"`
	Kind    string `json:"kind"` // exclude | correct
	Note    string `json:"note,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
	At      string `json:"at"`
	Actor   string `json:"actor,omitempty"`
}

func (s *Service) memoryReviewList(kind string) []memoryReview {
	db, err := s.adminDB()
	if err != nil {
		return nil
	}
	q := `SELECT leaf_id, room, kind, note, excerpt, at, actor FROM memory_reviews`
	var args []any
	if kind != "" {
		q += ` WHERE kind=?`
		args = append(args, kind)
	}
	rows, err := db.DB.Query(q+` ORDER BY at DESC`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []memoryReview
	for rows.Next() {
		var r memoryReview
		if rows.Scan(&r.LeafID, &r.Room, &r.Kind, &r.Note, &r.Excerpt, &r.At, &r.Actor) == nil {
			out = append(out, r)
		}
	}
	return out
}

// reviewedSearch runs a memory search with operator exclusions and corrections applied.
func (s *Service) reviewedSearch(ctx context.Context, eng *engine.Engine, req engine.SearchRequest) (search.Result, map[string]memoryReview, error) {
	reviews := map[string]memoryReview{}
	for _, r := range s.memoryReviewList("") {
		reviews[r.LeafID] = r
		if r.Kind == "exclude" {
			req.Exclude = append(req.Exclude, r.LeafID)
		}
	}
	res, err := eng.Search(ctx, req)
	if err != nil || len(reviews) == 0 {
		return res, nil, err
	}
	pack := lang.For(append(append([]string{}, s.Doc().Memory.Languages...), s.Doc().I18n.DefaultLocale)...)
	used := map[string]memoryReview{}
	for i, h := range res.Hits {
		for _, id := range append([]string{h.DocID}, h.MergedLeafIDs...) {
			if r, ok := reviews[id]; ok && r.Kind == "correct" && r.Note != "" {
				res.Hits[i].Excerpt += "\n" + lang.Fill(pack.Chat.CorrectionLine, "note", r.Note)
				used[h.DocID] = r
				break
			}
		}
	}
	return res, used, nil
}

func (s *Service) memoryReviewAction(req action.Request) action.Result {
	db, err := s.adminDB()
	if err != nil {
		return s.evidenceWriteError(req, err)
	}
	in := inputMap(req)
	if req.Action == "memory.reviews" {
		list := s.memoryReviewList(strVal(in, "kind", ""))
		if list == nil {
			list = []memoryReview{}
		}
		return action.OK(req, map[string]any{"reviews": list})
	}
	leaf := strVal(in, "leaf_id", "")
	kind := strVal(in, "kind", "")
	if leaf == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "leaf_id required", nil)
	}
	if kind == "clear" {
		if _, err := db.DB.Exec(`DELETE FROM memory_reviews WHERE leaf_id=?`, leaf); err != nil {
			return s.evidenceWriteError(req, err)
		}
		s.emit("memory", "review", nil)
		return action.OK(req, map[string]any{"leaf_id": leaf, "kind": "clear"})
	}
	if kind != "exclude" && kind != "correct" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "kind must be exclude, correct or clear", nil)
	}
	note := strings.TrimSpace(strVal(in, "note", ""))
	if kind == "correct" && note == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "a correction needs the correct version in note", nil)
	}
	var room, body string
	err = db.WithWriter(func(tx *sql.Tx) error {
		if err := tx.QueryRow(`SELECT room,body FROM memory_leaves WHERE leaf_id=?`, leaf).Scan(&room, &body); err != nil {
			return err
		}
		keys, err := copySources(tx, []string{leaf})
		if err != nil {
			return err
		}
		if err := indexCopy(tx, holderReview, leaf, keys, true); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO memory_reviews(leaf_id,room,kind,note,excerpt,at,actor,evidence_state) VALUES(?,?,?,?,?,?,?,1)
			ON CONFLICT(leaf_id) DO UPDATE SET kind=excluded.kind,note=excluded.note,excerpt=excluded.excerpt,at=excluded.at,actor=excluded.actor,evidence_state=1`, leaf, room, kind, clip(note, 1000), clip(body, 300), s.nowString(), actorName(req))
		return err
	})
	if err != nil {
		return s.evidenceWriteError(req, err)
	}
	s.emit("memory", "review", nil)
	return action.OK(req, map[string]any{"leaf_id": leaf, "kind": kind, "room": room})
}

// peopleAction lists speakers across stored rooms and links the names that are one person.
func (s *Service) peopleAction(req action.Request) action.Result {
	in := inputMap(req)
	if req.Action == "people.merge" {
		name := strings.TrimSpace(strVal(in, "name", ""))
		if name == "" {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "name required", nil)
		}
		var aliases []string
		seen := map[string]bool{name: true}
		for _, a := range strSlice(in, "aliases") {
			if a = strings.TrimSpace(a); a != "" && !seen[a] {
				seen[a] = true
				aliases = append(aliases, a)
			}
		}
		return s.mutateConfig(req, func(d *config.Document) error {
			if d.Memory.PersonAliases == nil {
				d.Memory.PersonAliases = map[string][]string{}
			}
			if len(aliases) == 0 || boolVal(in, "remove") {
				delete(d.Memory.PersonAliases, name)
			} else {
				d.Memory.PersonAliases[name] = aliases
			}
			if len(d.Memory.PersonAliases) == 0 {
				d.Memory.PersonAliases = nil
			}
			return nil
		})
	}
	// people.list
	type speaker struct {
		Name     string `json:"name"`
		Platform string `json:"platform"`
		Rooms    int    `json:"rooms"`
		Messages int    `json:"messages"`
		Accounts int    `json:"accounts"`
	}
	list := []speaker{}
	rows, err := s.Store.Reader().Query(`SELECT actor_label, platform, COUNT(DISTINCT room), COUNT(*), COUNT(DISTINCT actor_account)
		FROM memory_observations WHERE record_kind='human_text' AND deleted_at='' GROUP BY actor_label, platform ORDER BY COUNT(*) DESC LIMIT 500`)
	if err == nil {
		for rows.Next() {
			var sp speaker
			if rows.Scan(&sp.Name, &sp.Platform, &sp.Rooms, &sp.Messages, &sp.Accounts) == nil {
				list = append(list, sp)
			}
		}
		rows.Close()
	}
	people := []map[string]any{}
	names := make([]string, 0, len(s.Doc().Memory.PersonAliases))
	for k := range s.Doc().Memory.PersonAliases {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		people = append(people, map[string]any{"name": k, "aliases": s.Doc().Memory.PersonAliases[k]})
	}
	return action.OK(req, map[string]any{"speakers": list, "people": people})
}
