package application

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"masamong/internal/conversation"
	"masamong/internal/memory/store"
)

// Only boundaries live with the answer. Replay still has to obtain its content and
// provenance from the ordinary privacy-gated chat_turns reader.
type deliverySpan struct {
	End int       `json:"end"`
	At  time.Time `json:"at"`
}

func encodeSpans(spans []deliverySpan) string {
	if len(spans) == 0 {
		return ""
	}
	b, _ := json.Marshal(spans)
	return string(b)
}

func visibleDelivery(content, encoded string, cutoff time.Time) string {
	if encoded == "" || cutoff.IsZero() {
		return content
	}
	var spans []deliverySpan
	if json.Unmarshal([]byte(encoded), &spans) != nil {
		return ""
	}
	end := 0
	for _, span := range spans {
		if span.At.After(cutoff) || span.End < end || span.End > len(content) {
			break
		}
		end = span.End
	}
	return content[:end]
}

type replyPart struct {
	id   string
	text string // transient observation, for connectors without explicit byte ends
	deliverySpan
}

func (s *Service) registeredParts(turnID string, live []conversation.Line) []replyPart {
	db, err := s.adminDB()
	if err != nil {
		return nil
	}
	rows, err := db.Reader().Query(`SELECT message_id,part_end,at FROM reply_messages WHERE turn_id=?`, turnID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	observed := make(map[string]conversation.Line)
	for _, l := range live {
		if l.Bot {
			observed[l.ID] = l
		}
	}
	var parts []replyPart
	for rows.Next() {
		var p replyPart
		var registered string
		if rows.Scan(&p.id, &p.End, &registered) != nil {
			return nil
		}
		post := observed[p.id]
		p.text, p.At = post.Text, snowflakeTime(p.id)
		if p.At.IsZero() {
			p.At = post.At
		}
		if p.At.IsZero() {
			p.At, _ = time.Parse(time.RFC3339Nano, registered)
		}
		parts = append(parts, p)
	}
	if rows.Err() != nil {
		return nil
	}
	sort.Slice(parts, func(i, j int) bool {
		if parts[i].At.Equal(parts[j].At) {
			return parts[i].End < parts[j].End
		}
		return parts[i].At.Before(parts[j].At)
	})
	return parts
}

func partSpans(text string, parts []replyPart) []deliverySpan {
	var spans []deliverySpan
	end := 0
	for _, p := range parts {
		if p.End == 0 && p.text != "" {
			if i := strings.Index(text[end:], p.text); i >= 0 {
				p.End = end + i + len(p.text)
			}
		}
		if p.End > end && p.End <= len(text) {
			end = p.End
			spans = append(spans, p.deliverySpan)
		}
	}
	if end < len(text) && len(parts) > 0 {
		// Without an exact split, don't release the remainder before the last post.
		spans = append(spans, deliverySpan{End: len(text), At: parts[len(parts)-1].At})
	}
	return spans
}

func (s *Service) callCutoff(in ChatInput) time.Time {
	if at := snowflakeTime(in.MessageID); !at.IsZero() {
		return at
	}
	for _, l := range s.liveLines(in.GuildID, in.ChannelID) {
		if l.ID == in.MessageID && !l.At.IsZero() {
			return l.At
		}
	}
	return in.started
}

func postedRequestSQL(sqlite bool) string {
	request := `json_extract(t.body,'$.message_id')`
	if !sqlite {
		request = `JSON_UNQUOTE(JSON_EXTRACT(t.body,'$.message_id'))`
	}
	return `COALESCE(r.request_id,` + request + `,'')`
}

// An upgraded conversation may have only commit times in chat_turns. Recover
// its delivery clock on read, without touching old rows or their expiry.
func (s *Service) legacyPostTimes(in ChatInput, requests []string, observed map[string]time.Time) (map[string]time.Time, error) {
	db, err := s.adminDB()
	if err != nil {
		return nil, err
	}
	args := []any{in.GuildID, in.ChannelID}
	for _, id := range requests {
		args = append(args, id)
	}
	request := postedRequestSQL(db.IsSQLite())
	rows, err := db.Reader().Query(`SELECT `+request+`,r.message_id,r.at FROM reply_messages r
		LEFT JOIN turn_log t ON r.request_id IS NULL AND t.id=r.turn_id
		WHERE r.guild_id=? AND r.channel_id=? AND `+request+` IN (`+strings.TrimSuffix(strings.Repeat("?,", len(requests)), ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var req, id, stamp string
		if err := rows.Scan(&req, &id, &stamp); err != nil {
			return nil, err
		}
		at := snowflakeTime(id)
		if at.IsZero() {
			at = observed[id]
		}
		if at.IsZero() {
			at, _ = time.Parse(time.RFC3339Nano, stamp)
		}
		if !at.IsZero() && (out[req].IsZero() || at.Before(out[req])) {
			out[req] = at
		}
	}
	return out, rows.Err()
}

func afterRequest(answer, request time.Time) time.Time {
	if !request.IsZero() && !answer.After(request) {
		return request.Add(time.Millisecond)
	}
	return answer
}

func (s *Service) timeChatTurns(in ChatInput, turns []store.ChatTurn) error {
	var legacy []string
	for _, t := range turns {
		if t.Role == "assistant" && t.SpokenAt == "" && t.NativeID != "" {
			legacy = append(legacy, t.NativeID)
		}
	}
	observed, directlyAnswered := map[string]time.Time{}, map[string]time.Time{}
	var posted map[string]time.Time
	var err error
	if len(legacy) > 0 {
		for _, l := range s.liveLines(in.GuildID, in.ChannelID) {
			observed[l.ID] = l.At
			if l.Bot && l.ReplyTo != "" && (directlyAnswered[l.ReplyTo].IsZero() || l.At.Before(directlyAnswered[l.ReplyTo])) {
				directlyAnswered[l.ReplyTo] = l.At
			}
		}
		posted, err = s.legacyPostTimes(in, legacy, observed)
	}
	requests := map[string]time.Time{}
	for i, t := range turns {
		if t.NativeID == "" {
			continue
		}
		at, _ := time.Parse(time.RFC3339Nano, t.SpokenAt)
		if at.IsZero() {
			at, _ = time.Parse(time.RFC3339Nano, t.CreatedAt)
			delivered := posted[t.NativeID]
			if delivered.IsZero() {
				delivered = directlyAnswered[t.NativeID]
			}
			if t.Role == "assistant" && !delivered.IsZero() {
				at = delivered
			} else if t.Role == "user" {
				switch {
				case !snowflakeTime(t.NativeID).IsZero():
					at = snowflakeTime(t.NativeID)
				case !observed[t.NativeID].IsZero():
					at = observed[t.NativeID]
				case !delivered.IsZero() && !at.Before(delivered):
					at = delivered.Add(-time.Millisecond)
				}
			}
		}
		if t.Role == "user" {
			requests[t.NativeID] = at
		} else {
			at = afterRequest(at, requests[t.NativeID])
		}
		turns[i].SpokenAt = at.UTC().Format(time.RFC3339Nano)
	}
	return err
}
