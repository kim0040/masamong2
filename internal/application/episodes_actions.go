package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/memory/episodes"
	"masamong/internal/memory/store"
	"masamong/internal/storage"
)

// Actions over room memory (ADR-17); the write path is episodes.go.
// ---------------------------------------------------------------------------
// actions
// ---------------------------------------------------------------------------

// roomMemoryAction serves memory.episodes.list / show / forget / extract and memory.state.show.
//
// memory.episodes.list {guild_id?, channel_id?, room?, status?, limit?} →
//
//	{"episodes":[{"id","room","guild_id","channel_id","source","kind","title","status","due_at","when_text",
//	  "importance","participants":N,"items":N,"hidden":N,"first_seen","last_seen","expires_at","regen":bool}],
//	 "budget":{"limit_usd","spent_usd"}}
//
// Titles are rendered with current labels (people who left or withdrew show as "?"); no raw message
// text is returned. budget is this month's derived-memory spend (memory_spend kinds episode and state)
// against memory.derived_budget_usd (0 = room memory off).
//
// memory.episodes.show {id} (operators) →
//
//	{"episode":{…as in list, "participants":[{"ref","label"}]},
//	 "items":[{"id","kind","text","state","by":[ref],"evidence":[{"id","author","at","text"}]}]}
//
// text is rendered with current labels; evidence lines are read live from the stored messages and a
// deleted, edited-after or withdrawn message is shown as {"id","gone":true} without its text.
//
// memory.episodes.forget {id} | {guild_id, channel_id} | {room} → {"deleted":N}
//
// memory.state.show {guild_id, channel_id} →
//
//	{"state":{threads,open_questions,mood,banter}|null, "updated_at", "updated_n", "anchor_time"}
//
// The state's texts are rendered with current labels, like the episode items.
//
// memory.episodes.extract {block_id|source_room, dry_run?, budget_usd?, run_now?, limit?} →
//
//	dry_run: {"dry_run":true,"block_id","source","messages","segments","est_input_tokens","est_output_tokens",
//	  "est_cost_usd","price_known","model","budget_usd","derived_budget_usd","derived_spent_usd"}
//	else:    {"dry_run":false,"job":kakaoJob}
func (s *Service) roomMemoryAction(ctx context.Context, req action.Request) action.Result {
	// console only: a person speaking in a channel never reads or changes a room's memory through these
	if req.Invoker != nil && req.Invoker.UserID != "" {
		return action.Fail(req, errcode.PermissionDenied, "generic.error", "room memory is managed in the console", nil)
	}
	db, err := s.adminDB()
	if err != nil || !db.IsSQLite() || s.Store == nil {
		return action.Fail(req, errcode.NotSetup, "generic.error", "room memory needs the SQLite memory store", nil)
	}
	in := inputMap(req)
	switch req.Action {
	case "memory.episodes.list":
		return s.episodesList(req, db, in)
	case "memory.episodes.show":
		return s.episodeShow(req, db, in)
	case "memory.episodes.forget":
		return s.episodesForget(req, db, in)
	case "memory.state.show":
		return s.stateShow(req, db, in)
	case "memory.episodes.extract":
		return s.episodesExtract(ctx, req, db, in)
	}
	return action.Fail(req, errcode.ActionUnknown, "generic.error", req.Action, nil)
}

func (s *Service) episodesList(req action.Request, db *storage.Store, in map[string]any) action.Result {
	where, args := []string{"1=1"}, []any{}
	if room := strVal(in, "room", ""); room != "" {
		where, args = append(where, "e.room=?"), append(args, room)
	}
	if g := strVal(in, "guild_id", ""); g != "" {
		where, args = append(where, "e.guild_id=?"), append(args, g)
	}
	if c := strVal(in, "channel_id", ""); c != "" {
		where, args = append(where, "e.channel_id=?"), append(args, c)
	}
	if st := strVal(in, "status", ""); st != "" {
		where, args = append(where, "e.status=?"), append(args, st)
	}
	limit := intVal(in, "limit", 50)
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	args = append(args, limit)
	rows, err := db.Reader().Query(`SELECT e.id,e.room,e.guild_id,e.channel_id,e.source,e.kind,e.title,e.status,e.due_at,e.when_text,e.importance,e.participants,
		e.first_seen,e.last_seen,e.expires_at,e.regen,
		(SELECT COUNT(*) FROM episode_item i WHERE i.episode_id=e.id), (SELECT COUNT(*) FROM episode_item i WHERE i.episode_id=e.id AND i.state<>'active')
		FROM channel_episode e WHERE `+strings.Join(where, " AND ")+` ORDER BY e.last_seen DESC LIMIT ?`, args...)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	defer rows.Close()
	list := []map[string]any{}
	labelers := map[string]func(string) string{}
	for rows.Next() {
		var id, room, guild, channel, source, kind, title, status, due, when, participants, first, last, expires string
		var importance, regen, items, hidden int
		if err := rows.Scan(&id, &room, &guild, &channel, &source, &kind, &title, &status, &due, &when, &importance, &participants, &first, &last, &expires, &regen, &items, &hidden); err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		var refs []string
		_ = json.Unmarshal([]byte(participants), &refs)
		labelOf := labelers[room]
		if labelOf == nil {
			labelOf = s.refLabels(db, room)
			labelers[room] = labelOf
		}
		list = append(list, map[string]any{"id": id, "room": room, "guild_id": guild, "channel_id": channel, "source": source, "kind": kind, "title": episodes.StoredText(title, labelOf, "?"),
			"status": status, "due_at": due, "when_text": when, "importance": importance, "participants": len(refs), "items": items, "hidden": hidden,
			"first_seen": first, "last_seen": last, "expires_at": expires, "regen": regen == 1})
	}
	return action.OK(req, map[string]any{"episodes": list, "budget": map[string]any{"limit_usd": s.Doc().Memory.DerivedBudget(), "spent_usd": s.derivedSpent()}})
}

func (s *Service) episodeShow(req action.Request, db *storage.Store, in map[string]any) action.Result {
	id := strVal(in, "id", strVal(in, "episode_id", ""))
	if id == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "id required", nil)
	}
	var room, guild, channel, source, kind, title, status, due, when, participants, first, last, expires string
	var importance, regen int
	err := db.Reader().QueryRow(`SELECT room,guild_id,channel_id,source,kind,title,status,due_at,when_text,importance,participants,first_seen,last_seen,expires_at,regen
		FROM channel_episode WHERE id=?`, id).Scan(&room, &guild, &channel, &source, &kind, &title, &status, &due, &when, &importance, &participants, &first, &last, &expires, &regen)
	if errors.Is(err, sql.ErrNoRows) {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "no such episode", nil)
	}
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	labelOf := s.refLabels(db, room)
	render := func(text string) string { return episodes.StoredText(text, labelOf, "?") }
	var refs []string
	_ = json.Unmarshal([]byte(participants), &refs)
	people := []map[string]any{}
	for _, ref := range refs {
		people = append(people, map[string]any{"ref": ref, "label": labelOf(ref)})
	}
	rows, err := db.Reader().Query(`SELECT id,kind,text,evidence,by_refs,state FROM episode_item WHERE episode_id=? ORDER BY created_at, rowid`, id)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	type itemRow struct{ id, kind, text, ev, by, state string }
	var items []itemRow
	for rows.Next() {
		var it itemRow
		if rows.Scan(&it.id, &it.kind, &it.text, &it.ev, &it.by, &it.state) == nil {
			items = append(items, it)
		}
	}
	rows.Close()
	outItems := []map[string]any{}
	for _, it := range items {
		var ev, by []string
		_ = json.Unmarshal([]byte(it.ev), &ev)
		_ = json.Unmarshal([]byte(it.by), &by)
		outItems = append(outItems, map[string]any{"id": it.id, "kind": it.kind, "text": render(it.text), "state": it.state, "by": nonEmpty(by),
			"evidence": s.liveEvidence(db, ev)})
	}
	ep := map[string]any{"id": id, "room": room, "guild_id": guild, "channel_id": channel, "source": source, "kind": kind, "title": render(title),
		"status": status, "due_at": due, "when_text": when, "importance": importance, "participants": people, "first_seen": first, "last_seen": last,
		"expires_at": expires, "regen": regen == 1}
	return action.OK(req, map[string]any{"episode": ep, "items": outItems})
}

// liveEvidence reads cited messages now, leaving out the text of any that was deleted or withdrawn.
func (s *Service) liveEvidence(db *storage.Store, ids []string) []map[string]any {
	out := []map[string]any{}
	_ = db.WithWriter(func(tx *sql.Tx) error { // ObservationCurrent takes a transaction
		for _, id := range ids {
			var author, text, at, deleted string
			err := tx.QueryRow(`SELECT actor_label, content, parsed_time, deleted_at FROM memory_observations WHERE observation_id=?`, id).Scan(&author, &text, &at, &deleted)
			current := err == nil && deleted == ""
			if current {
				ok, cerr := store.ObservationCurrent(tx, id)
				current = cerr == nil && ok
			}
			if !current {
				out = append(out, map[string]any{"id": id, "gone": true})
				continue
			}
			out = append(out, map[string]any{"id": id, "author": author, "at": at, "text": text})
		}
		return nil
	})
	return out
}

func (s *Service) episodesForget(req action.Request, db *storage.Store, in map[string]any) action.Result {
	id := strVal(in, "id", strVal(in, "episode_id", ""))
	room := strVal(in, "room", "")
	if g, c := strVal(in, "guild_id", ""), strVal(in, "channel_id", ""); room == "" && g != "" && c != "" {
		room = roomOf(g, c)
	}
	if id == "" && room == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "id, room or guild_id+channel_id required", nil)
	}
	n := 0
	err := db.WithWriter(func(tx *sql.Tx) error {
		if id != "" {
			var exists int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM channel_episode WHERE id=?`, id).Scan(&exists); err != nil {
				return err
			}
			n = exists
			return deleteEpisodeTx(tx, id)
		}
		if err := tx.QueryRow(`SELECT COUNT(*) FROM channel_episode WHERE room=?`, room).Scan(&n); err != nil {
			return err
		}
		return store.DeleteRoomMemory(tx, room)
	})
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	if room != "" {
		_ = s.kvSet(cursorKey(room), episodeCursor{BlockStart: s.Clock.Now().UTC().Format("2006-01-02T15:04:05+00:00")}) // not made again from what was forgotten
	}
	s.emit("memory", "episodes", nil)
	return action.OK(req, map[string]any{"deleted": n})
}

func (s *Service) stateShow(req action.Request, db *storage.Store, in map[string]any) action.Result {
	g, c := strVal(in, "guild_id", ""), strVal(in, "channel_id", "")
	if g == "" || c == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "guild_id and channel_id required", nil)
	}
	row := s.loadState(db, g, c)
	if !row.exists {
		return action.OK(req, map[string]any{"state": nil})
	}
	row.body.Age(s.Clock.Now()) // a quiet room's topics cool and drop without an update
	row.body = consoleState(renderState(row.body, s.refLabels(db, roomOf(g, c))))
	return action.OK(req, map[string]any{"state": row.body, "updated_at": row.updatedAt, "updated_n": row.updatedN, "anchor_time": row.anchorAt})
}

func (s *Service) episodesExtract(ctx context.Context, req action.Request, db *storage.Store, in map[string]any) action.Result {
	block, ok := s.resolveBlockRoom(in)
	if !ok || block.SourceRoom == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "block_id or known source_room required", nil)
	}
	if block.Platform == "discord" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "Discord rooms are extracted in the background; this is for imported (Kakao) blocks", nil)
	}
	r := memRoom{channel: block.SourceRoom, room: block.SourceRoom, source: "kakao:" + block.ID}
	budget := parseUSD(strVal(in, "budget_usd", ""), 1.0)
	if budget <= 0 || budget > 1000 {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "budget_usd must be more than 0 and at most 1000", nil)
	}
	if boolVal(in, "dry_run") {
		est, err := s.kakaoEstimate(db, r)
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		est["dry_run"], est["block_id"], est["source"], est["budget_usd"] = true, block.ID, r.source, budget
		est["derived_budget_usd"], est["derived_spent_usd"] = s.Doc().Memory.DerivedBudget(), s.derivedSpent()
		return action.OK(req, est)
	}
	if s.practice {
		return action.Fail(req, errcode.FeatureDisabled, "generic.error", "not in practice mode", nil)
	}
	job := kakaoJob{Block: block.ID, Room: r.room, BudgetUSD: budget, Started: s.Clock.Now().UTC().Format(time.RFC3339)}
	var prev kakaoJob
	if ok, _ := s.kvGet(kakaoJobKey(r.room), &prev); ok && prev.Block == block.ID && !prev.Done {
		job = prev
		job.BudgetUSD = budget
		job.Stopped = ""
	}
	if err := s.kvSet(kakaoJobKey(r.room), job); err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	rooms := s.kakaoJobs()
	if !containsString(rooms, r.room) {
		_ = s.kvSet(kakaoJobsKey, append(rooms, r.room))
	}
	if boolVal(in, "run_now") {
		limit := intVal(in, "limit", kakaoSegments)
		if !s.derivedAllows(spendEpisode) {
			job.Stopped = "budget"
		} else if _, err := s.stepKakao(ctx, db, &job, s.Clock.Now(), max(1, limit)); err != nil && job.Stopped != "budget" {
			return action.Fail(req, errcode.ProviderError, "generic.error", err.Error(), map[string]any{"job": job})
		}
	}
	s.emit("memory", "episodes", nil)
	return action.OK(req, map[string]any{"dry_run": false, "job": job})
}

// consoleState is what the console shows of a state: no person references and no message IDs, which only
// the model's prompt needs.
func consoleState(st episodes.State) episodes.State {
	for i := range st.Threads {
		st.Threads[i].Who, st.Threads[i].Last, st.Threads[i].Ev = nil, "", nil
	}
	for i := range st.OpenQuestions {
		st.OpenQuestions[i].Who, st.OpenQuestions[i].Ev = nil, nil
	}
	for _, n := range []**episodes.Note{&st.Mood, &st.Banter} {
		if *n != nil {
			c := **n
			c.Ev = nil
			*n = &c
		}
	}
	return st
}

// renderState shows a state with people by their current names instead of stored references.
func renderState(st episodes.State, labelOf func(string) string) episodes.State {
	render := func(text string) string { return episodes.StoredText(text, labelOf, "?") }
	out := st
	out.Threads = append([]episodes.Thread(nil), st.Threads...)
	for i := range out.Threads {
		out.Threads[i].Label, out.Threads[i].Gist = render(out.Threads[i].Label), render(out.Threads[i].Gist)
	}
	out.OpenQuestions = append([]episodes.Question(nil), st.OpenQuestions...)
	for i := range out.OpenQuestions {
		out.OpenQuestions[i].Q = render(out.OpenQuestions[i].Q)
	}
	for _, n := range []**episodes.Note{&out.Mood, &out.Banter} {
		if *n != nil {
			c := **n
			c.Text = render(c.Text)
			*n = &c
		}
	}
	return out
}
