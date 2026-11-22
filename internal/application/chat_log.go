package application

import (
	"database/sql"
	"encoding/json"
	"time"
	"unicode/utf8"

	"masamong/internal/chat"
	"masamong/internal/memory/blocks"
	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
)

// hitBrief is the evidence a turn used, as the console shows it.
func hitBrief(hits []search.Hit) []map[string]any {
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		m := map[string]any{"rank": h.Rank, "doc_id": h.DocID, "room": h.Room, "date": h.Date, "speakers": h.Speakers, "excerpt": clip(h.Excerpt, 1200),
			"summary": h.Summary, "span": h.Span, "sources": h.Sources, "dense_cos": h.DenseCos}
		m["observation_ids"] = h.ObservationIDs
		m["visible_observation_ids"] = h.VisibleObservationIDs
		m["leaf_observation_ids"] = h.LeafObservationIDs
		if h.RerankScore != nil {
			m["rerank_score"] = *h.RerankScore
		}
		// Evidence is packed without repeats: when this hit's own messages were already shown by an
		// earlier hit, its excerpt holds only their surroundings. Say where they are, so the date (the
		// hit's own messages) and the text do not look like they disagree.
		if n := shownEarlier(hits, h); n > 0 {
			m["leaf_shown_in"] = n
		}
		out = append(out, m)
	}
	return out
}

// shownEarlier is the rank of the first earlier hit whose excerpt shows h's own messages, when h's
// excerpt shows none of them (0 otherwise).
func shownEarlier(hits []search.Hit, h search.Hit) int {
	own := make(map[string]bool, len(h.LeafObservationIDs))
	for _, id := range h.LeafObservationIDs {
		own[id] = true
	}
	for _, id := range h.VisibleObservationIDs {
		if own[id] {
			return 0
		}
	}
	for _, e := range hits {
		if e.Rank >= h.Rank {
			break
		}
		for _, id := range e.VisibleObservationIDs {
			if own[id] {
				return e.Rank
			}
		}
	}
	return 0
}

// lookedUp lists the messages a reply retrieved (its hits and tool results): the reply is built on them as
// much as on the conversation, so deleting one takes the reply out of the chat context. Messages, not
// leaves: deleting a message tombstones the leaf that held it, the leaf's ID comes back when the room is
// cut again, and a later reply on the new leaf has read nothing deleted.
func lookedUp(res chat.Result) []string {
	raw, _ := json.Marshal(map[string]any{"hits": hitBrief(res.Hits), "tools": res.Tools})
	var body any
	_ = json.Unmarshal(raw, &body)
	var ids []string
	walkEvidence(body, func(_ map[string]any, found []string) { ids = append(ids, found...) })
	return ids
}

// recordPassageRunes is how much of a page's text a turn record keeps: the page belongs to a third party and
// a deep search reads several, so the record would otherwise grow by 10–20 thousand characters a turn and
// show them in the console. The model saw the whole passage; the record only needs to show what was read.
const recordPassageRunes = 200

// recordTools returns the turn's tool results as the record keeps them: every search result's passage cut
// to recordPassageRunes, with passage_runes giving its full length. Results without passages (every
// search that read no pages) come back untouched, and res.Tools itself is never changed.
func recordTools(results []chat.ToolResult) []chat.ToolResult {
	var out []chat.ToolResult
	for i, r := range results {
		if r.Tool != "search.web" {
			continue
		}
		clipped, changed := clipPassages(r.Data["results"])
		if !changed {
			continue
		}
		if out == nil {
			out = append([]chat.ToolResult(nil), results...)
		}
		data := make(map[string]any, len(r.Data))
		for k, v := range r.Data {
			data[k] = v
		}
		data["results"] = clipped
		out[i].Data = data
	}
	if out == nil {
		return results
	}
	return out
}

func clipPassages(list any) (any, bool) {
	clipOne := func(item map[string]any) (map[string]any, bool) {
		text, ok := item["passage"].(string)
		if !ok || utf8.RuneCountInString(text) <= recordPassageRunes {
			return item, false
		}
		cp := make(map[string]any, len(item)+1)
		for k, v := range item {
			cp[k] = v
		}
		cp["passage"] = clip(text, recordPassageRunes)
		cp["passage_runes"] = utf8.RuneCountInString(text)
		return cp, true
	}
	switch items := list.(type) {
	case []any:
		var out []any
		for i, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			if cp, changed := clipOne(m); changed {
				if out == nil {
					out = append([]any(nil), items...)
				}
				out[i] = cp
			}
		}
		return out, out != nil
	case []map[string]any:
		var out []map[string]any
		for i, m := range items {
			if cp, changed := clipOne(m); changed {
				if out == nil {
					out = append([]map[string]any(nil), items...)
				}
				out[i] = cp
			}
		}
		return out, out != nil
	}
	return nil, false
}

// logTurn writes a body-free turn record to the traces table (operator diagnostics).
func (s *Service) logTurn(in ChatInput, res chat.Result) {
	body := map[string]any{
		"kind": "chat_turn", "channel": blocks.ChannelKey(in.GuildID, in.ChannelID), "rag_state": res.RagState,
		"turn_id": in.turnID,
		"hits":    len(res.Hits), "memory_state": res.Assess.MemoryState, "intent_source": res.Intent.Source,
		"retrieval": res.Retrieve, "timing_ms": res.Timing, "cost_usd": res.CostUSD, "tools": len(res.Tools),
	}
	addTurnSpans(body, res)
	if res.ToolLoop != nil {
		body[toolLoopKey(res.ToolLoop)] = res.ToolLoop.Brief() // calls and reasons, no arguments
	}
	if res.AnswerRetry != "" {
		body["answer_retry"] = res.AnswerRetry
	}
	if res.AnswerLoop {
		body["answer_loop"] = true
	}
	id := newID("tr_")
	raw, _ := json.Marshal(body)
	db, err := s.adminDB()
	if err != nil {
		return
	}
	var sources []string
	if in.sourceRefs != nil {
		sources = *in.sourceRefs
	}
	refsRaw, _ := json.Marshal(map[string]any{"hits": hitBrief(res.Hits), "tools": res.Tools})
	var refs any
	_ = json.Unmarshal(refsRaw, &refs)
	err = db.WithWriter(func(tx *sql.Tx) error {
		keys, err := copySources(tx, refs, sources, []string{in.MessageID, personRef(in.SpeakerID)})
		if err != nil {
			return err
		}
		blocked, err := store.SourcesBlockedAsOf(tx, keys, in.readAsOf)
		if err != nil {
			return err
		}
		if blocked {
			delete(body, "retrieval")
		}
		raw, _ = json.Marshal(body)
		if _, err := tx.Exec(`INSERT INTO traces(id,body,created_at,evidence_state) VALUES(?,?,?,1)`, id, string(raw), s.Clock.Now().UTC().Format(time.RFC3339)); err != nil {
			return err
		}
		// The turn already indexes every source read by this reply. A trace points to that turn
		// instead of copying its candidate/context list; redaction follows the turn -> trace link.
		if in.turnID != "" {
			// Register the turn's sources in this transaction too: the trace is written just before
			// recordTurn, and a concurrent deletion must follow the link during that interval.
			if err := indexTurnRefs(tx, in.turnID, keys, in.MessageID); err != nil {
				return err
			}
			keys = uniqueStrings([]string{in.turnID, in.MessageID, personRef(in.SpeakerID)})
		}
		return indexCopy(tx, "trace", id, keys, false)
	})
	if err != nil {
		s.warnRecordFailed("trace write failed", err)
	}
}
