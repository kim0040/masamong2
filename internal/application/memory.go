package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/memory/blocks"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/retrieval"
	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
)

func parseUSD(s string, def float64) float64 {
	if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil && v >= 0 {
		return v
	}
	return def
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, value := range in {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

// dbBlocks exposes stored rooms as memory blocks (attachable to channel views).
func (s *Service) dbBlocks() []blocks.Block {
	eng, err := s.MemoryEngine()
	if err != nil {
		return nil
	}
	rooms, err := eng.Store.Rooms()
	if err != nil {
		return nil
	}
	var out []blocks.Block
	for _, r := range rooms {
		status := "imported"
		switch {
		case r.Embedded > 0 && r.Embedded >= r.Leaves-r.Open: // an open tail is embedded once it closes
			status = "ready"
		case r.Embedded > 0:
			status = "indexing"
		}
		label := r.Label
		if label == "" {
			label = blocks.LabelForRoom(r.Room.Room)
		}
		sem := "offset"
		if r.Platform == "kakao" {
			sem = "naive"
		}
		out = append(out, blocks.Block{
			ID: blocks.BlockID(r.Collection, r.Room.Room, eng.Settings.Recipe.Version(), eng.EmbedSpace()), Platform: r.Platform,
			Collection: r.Collection, SourceRoom: r.Room.Room, Label: label, SegmentationVersion: eng.Settings.Recipe.Version(),
			EmbeddingSpace: eng.EmbedSpace(), MessageCount: r.Messages, LeafCount: r.Leaves, Status: status, TimeSemantics: sem,
		})
	}
	return out
}

// channelRooms is the memory scope of a channel: its own room, its threads' rooms (a thread: its parent
// and the parent's other threads), plus attached blocks' rooms. Nothing else — rooms are never merged or
// shared implicitly (RAW_DATA_POLICY); a thread is part of its channel on Discord too.
func (s *Service) channelRooms(guildID, channelID string) []string {
	if guildID == "" {
		return nil
	}
	if s.threadDeleted(channelID) {
		return nil
	}
	rooms := []string{engine.RoomKey(guildID, channelID)}
	// a thread's talk belongs to its parent channel's memory, and the parent's to the thread's
	if parent := s.threadParent(channelID); parent != "" {
		rooms = append(rooms, engine.RoomKey(guildID, parent))
		channelID = parent
	}
	for _, r := range s.threadRooms(guildID, channelID) {
		if r != rooms[0] {
			rooms = append(rooms, r)
		}
	}
	pol := s.channelPolicy(guildID, channelID)
	for _, id := range pol.RAGView.Blocks {
		if b, ok := s.lookupBlock(id); ok && b.SourceRoom != "" {
			rooms = append(rooms, b.SourceRoom)
		}
	}
	return rooms
}

func (s *Service) retrieveMode() retrieval.RankingMode {
	if m, ok := retrieval.Normalize(s.Doc().Memory.Retrieve); ok {
		return m
	}
	return retrieval.Auto
}

func (s *Service) memoryActions(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	eng, err := s.MemoryEngine()
	if err != nil {
		return s.failErr(req, errcode.NotSetup, "memory.not_ready", err, nil)
	}
	switch req.Action {
	case "messages.search":
		q := strVal(in, "query", strVal(in, "q", ""))
		if q == "" {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "query required", nil)
		}
		if len([]rune(q)) > 200 {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "query must be at most 200 characters", nil)
		}
		rooms := strSlice(in, "rooms")
		if g, c := strVal(in, "guild_id", ""), strVal(in, "channel_id", ""); c != "" {
			rooms = append(rooms, s.channelRooms(g, c)...)
		}
		rooms = uniqueStrings(rooms)
		if len(rooms) == 0 {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "scope required: --channel_id (and --guild_id) or --rooms", nil)
		}
		if len(rooms) > 50 {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "at most 50 rooms may be searched", nil)
		}
		topK := min(100, max(1, intVal(in, "top_k", 20)))
		found, err := eng.Store.SearchMessages(rooms, q, topK)
		if err != nil {
			return s.failErr(req, errcode.IO, "error.storage", err, nil)
		}
		byRoom := map[string]blocks.Block{}
		for _, b := range s.dbBlocks() {
			byRoom[b.SourceRoom] = b
		}
		hits := make([]map[string]any, 0, len(found))
		for i, h := range found {
			m := h.Message
			b := byRoom[m.Room]
			hits = append(hits, map[string]any{
				"rank": i + 1, "observation_id": m.ObservationID, "source_room_id": m.Room,
				"source_room_original": m.RoomOriginal, "block_id": b.ID, "platform": b.Platform,
				"label": b.Label, "date": m.ParsedTime, "speakers": []string{m.ActorLabel},
				"excerpt": clip(m.Content, 1200), "lexical_score": h.Score,
			})
		}
		return action.OK(req, map[string]any{"hits": hits, "rag_state": "raw_lexical", "mode": "raw_lexical", "rooms": rooms, "cost_usd": 0})
	case "memory.search":
		q := strVal(in, "query", strVal(in, "q", ""))
		if q == "" {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "query required", nil)
		}
		rooms := strSlice(in, "rooms")
		if g, c := strVal(in, "guild_id", ""), strVal(in, "channel_id", ""); c != "" {
			rooms = append(rooms, s.channelRooms(g, c)...)
		}
		if len(rooms) == 0 {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "scope required: --channel_id (and --guild_id) or --rooms", nil)
		}
		if eng.Index() == nil {
			if _, err := eng.Reload(); err != nil {
				return s.failErr(req, errcode.InvalidInput, "memory.failed", err, nil)
			}
		}
		mode := s.retrieveMode()
		if r := strVal(in, "ranking", ""); r != "" {
			if m, ok := retrieval.Normalize(r); ok {
				mode = m
			}
		}
		it := search.Intent{Kind: "other", Source: "none"}
		if targets := strSlice(in, "targets"); len(targets) > 0 {
			it.Targets = targets
		}
		res, corrected, err := s.reviewedSearch(ctx, eng, engine.SearchRequest{Query: q, Intent: it, Rooms: rooms, Ranking: mode,
			TopK: intVal(in, "top_k", 5), RemainingUSD: parseUSD(s.Doc().Memory.TurnBudgetUSD, 0.05)})
		if err != nil {
			return s.failErr(req, errcode.InvalidInput, "memory.failed", err, nil)
		}
		if corrected == nil {
			corrected = map[string]memoryReview{}
		}
		return action.OK(req, map[string]any{"hits": res.Hits, "rag_state": res.State, "rooms": rooms,
			"ranking": res.Trace["applied"], "auto_reason": res.Trace["ranking_auto_reason"], "corrected": corrected})
	case "imports.start":
		src := strVal(in, "source", "raw")
		path := strVal(in, "path", "")
		if path == "" {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "path required", nil)
		}
		path, err = s.importPath(path)
		if err != nil {
			return s.failErr(req, errcode.PermissionDenied, "import.path_denied", err, nil)
		}
		switch src {
		case "raw":
			var rooms map[string]bool
			if rs := strSlice(in, "rooms"); len(rs) > 0 {
				rooms = map[string]bool{}
				for _, r := range rs {
					rooms[r] = true
				}
			}
			merge := map[string]string{}
			if into := strVal(in, "merge_into", ""); into != "" {
				for r := range rooms {
					merge[r] = into
				}
			}
			res, err := eng.ImportRawContext(ctx, path, rooms, merge)
			if err != nil {
				return s.importFailure(req, err, res)
			}
			return action.OK(req, map[string]any{"source": src, "result": res, "next": "memory reindex", "atomicity": "batch", "resume": "same_file"})
		case "kakao_csv":
			room := strVal(in, "room", "")
			if room == "" {
				return action.Fail(req, errcode.InvalidInput, "generic.error", "room required (e.g. kakao:friends)", nil)
			}
			res, err := eng.ImportKakaoCSVContext(ctx, path, strVal(in, "collection", strings.TrimPrefix(room, "kakao:")), room,
				strVal(in, "label", ""), strVal(in, "timezone", s.Doc().Memory.NaiveTimezone), strVal(in, "export_locale", ""))
			if err != nil {
				return s.importFailure(req, err, res)
			}
			return action.OK(req, map[string]any{"source": src, "result": res, "next": "memory reindex", "atomicity": "batch", "resume": "same_file"})
		}
		return action.Fail(req, errcode.InvalidInput, "generic.error", "source must be raw | kakao_csv", nil)
	case "memory.reindex":
		opts := engine.IndexOptions{Rooms: strSlice(in, "rooms"), DryRun: boolVal(in, "dry_run"), Limit: intVal(in, "limit", 0),
			BudgetUSD: parseUSD(strVal(in, "budget_usd", s.Doc().Memory.IndexBudgetUSD), 1.0)}
		indexCtx := memoryJobContext(ctx, "memory_reindex_")
		defer s.flushMemoryLLMCalls(indexCtx)
		rep, err := eng.IndexRooms(indexCtx, opts, nil)
		if err != nil {
			return s.failErr(req, errcode.InvalidInput, "memory.failed", err, nil)
		}
		out := map[string]any{"report": rep}
		if len(rep.BatchWaiting) > 0 {
			// left out of the pass: their pending work is paid for in a batch still at the provider
			out["note"] = "rooms waiting for their batch at the provider were not indexed (report.batch_waiting_rooms)"
		}
		return action.OK(req, out)
	case "chat.respond":
		ci := ChatInput{GuildID: strVal(in, "guild_id", "0"), ChannelID: strVal(in, "channel_id", ""),
			Message: strVal(in, "message", ""), Speaker: strVal(in, "speaker", "tester"), SpeakerID: strVal(in, "speaker_id", ""), DryRun: true,
			Ranking: strVal(in, "ranking", ""), PersonaRef: strVal(in, "persona_ref", "")}
		if v, ok := in["draft_style"].(string); ok {
			ci.DraftStyle = &v
		}
		if v, ok := in["draft_rules"].(string); ok {
			if len([]rune(v)) > 2000 {
				return action.Fail(req, errcode.InvalidInput, "generic.error", "rules must be at most 2000 characters", nil)
			}
			ci.DraftRules = &v
		}
		out, err := s.ChatTurn(ctx, ci)
		if err != nil {
			return s.failErr(req, errcode.InvalidInput, "memory.failed", err, nil)
		}
		res := map[string]any{"answer": out.Answer, "rag_state": out.RagState, "hits": len(out.Hits),
			"intent": out.Intent, "retrieval": out.Retrieve, "timing_ms": out.Timing, "cost_usd": out.CostUSD, "tools": out.Tools}
		if boolVal(in, "detail") {
			res["evidence"] = hitBrief(out.Hits)
			res["plan"] = out.Plan
			res["assessment"] = out.Assess
		}
		return action.OK(req, res)
	}
	return action.Fail(req, errcode.ActionUnknown, "action.unknown", s.msg(req, "action.unknown"), nil)
}

func (s *Service) importFailure(req action.Request, err error, result any) action.Result {
	key := ""
	switch {
	case errors.Is(err, corpus.ErrImportTooLarge):
		key = "imports.limit.file"
	case errors.Is(err, corpus.ErrFieldTooLarge):
		key = "imports.limit.field"
	case errors.Is(err, corpus.ErrKakaoGroupTooLarge):
		key = "imports.limit.timestamp_group"
	case errors.Is(err, corpus.ErrRowTooLarge):
		key = "imports.limit.row"
	case errors.Is(err, errPreviewSpeakers):
		key = "imports.limit.speakers"
	}
	detail := map[string]any{}
	if req.Action == "imports.start" {
		detail = map[string]any{"result": result, "atomicity": "batch", "resume": "same_file"}
	}
	if key == "" { // a read or parse failure: the file's own words go to the log, not to the person
		return s.failErr(req, errcode.InvalidInput, "import.failed", err, detail)
	}
	return action.Fail(req, errcode.InvalidInput, key, s.msg(req, key), detail)
}

// memoryStatus is memory.blocks.list's extra section.
func (s *Service) memoryStatus() map[string]any {
	return s.memoryStatusFor(context.Background(), false, false)
}

// memoryStatusFor is memoryStatus with the message-vector section filled further: pending adds the
// waiting count, estimate the cost of turning the setting on (see messageVectorStatus). One engine lookup
// serves the whole section.
func (s *Service) memoryStatusFor(ctx context.Context, pending, estimate bool) map[string]any {
	eng, err := s.MemoryEngine()
	if err != nil {
		return map[string]any{"status": "unavailable", "reason": err.Error()}
	}
	counts, _ := eng.Store.Counts()
	out := map[string]any{"counts": counts, "embed_space": eng.EmbedSpace(), "recipe": eng.Settings.Recipe.Version(),
		"retrieve": s.Doc().Memory.Retrieve, "capture": s.Doc().Memory.Capture}
	if ix := eng.Index(); ix != nil {
		out["loaded"] = map[string]any{"leaves": len(ix.Leaves), "messages": len(ix.Messages), "at": eng.LoadedAt().UTC().Format(time.RFC3339)}
	} else {
		out["loaded"] = nil
	}
	live := -1
	if n, ok := counts["observations"]; ok {
		live = n
	}
	if mv := s.messageVectorStatus(ctx, eng, pending, estimate, live); mv != nil {
		out["message_vectors"] = mv
	}
	return out
}

// messageVectorStatus is the message-vector section of the memory status (memory.message_vectors): how
// many message vectors are stored for the current space and, with pending, how many messages of closed
// leaves still wait for one. With estimate it also previews what turning the setting on would cost, which
// the console shows before the operator confirms (it works while the setting is still off). While the
// setting is off and no estimate is asked for it returns nil and the status stays as it was.
func (s *Service) messageVectorStatus(ctx context.Context, eng *engine.Engine, pending, estimate bool, live int) map[string]any {
	on := s.Doc().Memory.MessageVectorsOn()
	if !on && !estimate {
		return nil
	}
	space := eng.MessageSpace()
	dims := eng.MessageVectorDims()
	if ix := eng.Index(); space != "" && ix != nil && ix.MessageSpace == space && ix.MsgVecs.Dims > 0 {
		dims = ix.MsgVecs.Dims
	}
	out := map[string]any{"enabled": on, "space": space, "dimensions": dims}
	_, inSpace, countErr := eng.Store.MessageVectorCounts(space)
	if countErr == nil {
		out["stored"] = inSpace
	}
	if ix := eng.Index(); ix != nil && on {
		out["loaded"] = ix.MsgVecs.Rows()
	}
	if pending && on && countErr == nil {
		if n, ok := s.pendingMessages(ctx, eng, space, inSpace, live); ok {
			out["pending"] = n
		}
	}
	if estimate {
		if est, err := eng.EstimateMessageVectors(nil); err == nil {
			out["estimate"] = est
		}
	}
	return out
}

type spendResetSnapshot struct {
	Month    string             `json:"month"`
	At       string             `json:"at"`
	ByKind   map[string]float64 `json:"by_kind"`
	Total    float64            `json:"total"`
	Day      string             `json:"day"`
	DayTotal float64            `json:"day_total"`
}

const spendResetKey = "spend_reset"

func (s *Service) readSpendReset() (spendResetSnapshot, bool) {
	var reset spendResetSnapshot
	ok, err := s.kvGet(spendResetKey, &reset)
	if err != nil || !ok || reset.Month == "" || reset.At == "" || reset.ByKind == nil {
		return spendResetSnapshot{}, false
	}
	return reset, true
}

func (s *Service) activeSpendReset(now time.Time) (spendResetSnapshot, bool) {
	reset, ok := s.readSpendReset()
	if !ok || reset.Month != now.UTC().Format("2006-01") {
		return spendResetSnapshot{}, false
	}
	return reset, true
}

func spendResetInfo(reset spendResetSnapshot) map[string]any {
	return map[string]any{"at": reset.At, "before_usd": reset.Total}
}

func (s *Service) spendAfterResetDay(day string, spent float64) float64 {
	reset, ok := s.readSpendReset()
	if ok && reset.Day == day {
		spent -= reset.DayTotal
		if spent < 0 {
			return 0
		}
	}
	return spent
}

// monthSpend is provider spend this month from memory_spend, adjusted by an optional reset snapshot.
func (s *Service) monthSpend() (map[string]float64, float64, bool) {
	eng, err := s.MemoryEngine()
	if err != nil {
		return nil, 0, false
	}
	now := s.Clock.Now().UTC()
	by, total, err := eng.Store.SpendSince(now.Format("2006-01") + "-01") // spend days are UTC (store.AddSpend)
	if err != nil {
		return nil, 0, false
	}
	// the paid people backfill has its own cap per server (people_backfill_run): it is listed by kind but
	// is not part of the total the monthly budget gates chat and features with
	reset, ok := s.activeSpendReset(now)
	if !ok {
		return by, total - by[store.SpendPeopleBackfill], true
	}
	effective := make(map[string]float64, len(by))
	total = 0
	for kind, raw := range by {
		spent := raw - reset.ByKind[kind]
		if spent < 0 {
			spent = 0
		}
		effective[kind] = spent
		if kind != store.SpendPeopleBackfill {
			total += spent
		}
	}
	return effective, total, true
}

func (s *Service) usageReset(req action.Request) action.Result {
	in := map[string]any{}
	if len(req.Input) > 0 {
		if err := json.Unmarshal(req.Input, &in); err != nil || in == nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "input must be an object", nil)
		}
	}
	if boolVal(in, "undo") {
		reset, _ := s.readSpendReset()
		if err := s.kvDelete(spendResetKey); err != nil {
			return s.failErr(req, errcode.IO, "error.storage", err, nil)
		}
		spent := "unknown"
		if _, total, ok := s.monthSpend(); ok {
			spent = fmt.Sprintf("%.4f", total)
		}
		return action.OK(req, map[string]any{
			"spent_usd":  spent,
			"before_usd": fmt.Sprintf("%.4f", reset.Total),
			"at":         reset.At,
		})
	}

	eng, err := s.MemoryEngine()
	if err != nil {
		return s.failErr(req, errcode.IO, "memory.not_ready", err, nil)
	}
	now := s.Clock.Now().UTC()
	month := now.Format("2006-01")
	day := now.Format("2006-01-02")
	// month and today come from one read, so the two baselines can never straddle a spend written between them
	by, total, dayTotal := map[string]float64{}, 0.0, 0.0
	rows, err := eng.Store.DB.Reader().Query(`SELECT day, kind, usd FROM memory_spend WHERE day>=?`, month+"-01")
	if err != nil {
		return s.failErr(req, errcode.IO, "error.storage", err, nil)
	}
	for rows.Next() {
		var d, kind string
		var usd float64
		if err := rows.Scan(&d, &kind, &usd); err != nil {
			rows.Close()
			return s.failErr(req, errcode.IO, "error.storage", err, nil)
		}
		by[kind] += usd
		total += usd
		if d == day {
			dayTotal += usd
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return s.failErr(req, errcode.IO, "error.storage", err, nil)
	}
	reset := spendResetSnapshot{Month: month, At: now.Format(time.RFC3339), ByKind: by, Total: total, Day: day, DayTotal: dayTotal}
	if err := s.kvSet(spendResetKey, reset); err != nil {
		return s.failErr(req, errcode.IO, "error.storage", err, nil)
	}
	spent := "0.0000"
	if _, total, ok := s.monthSpend(); ok {
		spent = fmt.Sprintf("%.4f", total)
	}
	return action.OK(req, map[string]any{
		"spent_usd":  spent,
		"before_usd": fmt.Sprintf("%.4f", reset.Total),
		"at":         reset.At,
	})
}
