package application

import (
	"context"
	"sort"
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/retrieval"
	"masamong/internal/memory/search"
)

// events.timeline (19장 A12): what happened when, in a channel's memory scope (its own room plus the
// blocks connected to it) or in chosen rooms. Without a question it lists the conversation pieces
// (leaves) by day with their summaries; with one it searches memory and puts what it found in date
// order ("when did we go camping?"). It never calls a model beyond the search's own query embedding.

type timelineDay struct {
	Date    string           `json:"date"`
	Entries []map[string]any `json:"entries"`
}

func (s *Service) eventsTimeline(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	rooms := strSlice(in, "rooms")
	if g, c := strVal(in, "guild_id", ""), strVal(in, "channel_id", ""); c != "" {
		rooms = append(rooms, s.channelRooms(g, c)...)
	}
	rooms = uniqueStrings(rooms)
	if len(rooms) == 0 {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "scope required: channel_id (and guild_id) or rooms", nil)
	}
	eng, err := s.MemoryEngine()
	if err != nil {
		return action.Fail(req, errcode.NotSetup, "generic.error", err.Error(), nil)
	}
	labels := map[string]string{}
	for _, b := range s.dbBlocks() {
		labels[b.SourceRoom] = b.Label
	}
	limit := min(200, max(1, intVal(in, "limit", 60)))
	var entries []map[string]any
	mode := "browse"
	if q := strings.TrimSpace(strVal(in, "query", "")); q != "" {
		mode = "search"
		entries, err = s.timelineSearch(ctx, eng, q, rooms, limit)
		if err != nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", err.Error(), nil)
		}
	} else {
		from, to := strVal(in, "from", ""), strVal(in, "to", "")
		if to != "" {
			// "to" is a day: include it
			if t, err := time.Parse("2006-01-02", to); err == nil {
				to = t.AddDate(0, 0, 1).Format("2006-01-02")
			}
		}
		list, err := eng.Store.Timeline(rooms, from, to, limit)
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		for _, e := range list {
			entries = append(entries, timelineEntry(e.LeafID, e.Room, e.Time, e.Summary, e.Excerpt, e.Speakers))
		}
	}
	for _, e := range entries {
		e["label"] = labels[e["room"].(string)]
	}
	days := []timelineDay{}
	for _, e := range entries {
		d := dayOf(e["time"].(string))
		if len(days) == 0 || days[len(days)-1].Date != d {
			days = append(days, timelineDay{Date: d})
		}
		days[len(days)-1].Entries = append(days[len(days)-1].Entries, e)
	}
	return action.OK(req, map[string]any{"mode": mode, "rooms": rooms, "days": days, "count": len(entries)})
}

func timelineEntry(id, room, at, summary, excerpt string, speakers []string) map[string]any {
	if speakers == nil {
		speakers = []string{}
	}
	return map[string]any{"doc_id": id, "room": room, "time": at, "summary": summary, "excerpt": clip(excerpt, 600), "speakers": speakers}
}

// dayOf is the date part of a stored time ("2026-09-01T..." or a naive "2026-09-01 ...").
func dayOf(t string) string {
	if len(t) >= 10 {
		return t[:10]
	}
	return t
}

// timelineSearch finds what a question is about and orders it newest first. Without a loaded index
// (no embedding model) it falls back to the exact message search, one entry per matching message.
func (s *Service) timelineSearch(ctx context.Context, eng *engine.Engine, q string, rooms []string, limit int) ([]map[string]any, error) {
	var out []map[string]any
	if eng.Index() != nil || eng.EnsureLoaded() == nil {
		res, _, err := s.reviewedSearch(ctx, eng, engine.SearchRequest{Query: q, Intent: search.Intent{Kind: "other", Source: "none"}, Rooms: rooms,
			Ranking: retrieval.Hybrid, TopK: min(limit, 30), RemainingUSD: parseUSD(s.Doc().Memory.TurnBudgetUSD, 0.05)})
		if err == nil {
			for _, h := range res.Hits {
				out = append(out, timelineEntry(h.DocID, h.Room, h.Date, h.Summary, h.Excerpt, h.Speakers))
			}
			sortNewest(out)
			return out, nil
		}
	}
	hits, err := eng.Store.SearchMessages(rooms, q, limit)
	if err != nil {
		return nil, err
	}
	for _, h := range hits {
		m := h.Message
		out = append(out, timelineEntry(m.ObservationID, m.Room, m.ParsedTime, "", m.Content, []string{m.ActorLabel}))
	}
	sortNewest(out)
	return out, nil
}

func sortNewest(entries []map[string]any) {
	sort.SliceStable(entries, func(i, j int) bool { return entries[i]["time"].(string) > entries[j]["time"].(string) })
}
