package application

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
)

// Operator marks: the console's operator tells, per turn, whether the reply was right (맞음), wrong
// (틀림) or built on out-of-date information (낡은 정보). The point is to measure how often search answers
// are right, which nothing measured before. A mark is one row of turn_feedback with source "operator"
// and a fixed user_hash, so there is at most one per turn and it never mixes with what people rated
// (feedback.go) or with the implicit signals (implicit_feedback.go): every sum and chip of ratings
// leaves it out and the console shows it apart. It stores no text and no reason, only the turn id,
// the verdict and when. Nothing reads a mark back into the model's behaviour.
//
// A mark goes with the turn records' retention (retention.turn_days, like the ratings). It can only be
// made while the turn still shows what was said and answered: once the message is deleted, the person
// withdrew their memory consent, the channel keeps no text, or the detail aged out (retention.turn_detail),
// there is nothing left to judge (markBlock). A mark already made stays and still counts in the totals.
const (
	operatorSource = "operator"
	// operatorRef is the user_hash of every operator mark: the operator is not one of the people who rate
	// replies, and forgetPerson looks rows up by a person's hash, so a mark is never taken for theirs.
	operatorRef = "operator"
)

// The verdict is kept in the rating column; -2 is "stale", apart from -1 "wrong". Rating readers that
// do not know operator rows (anything counting rating<0 as bad) must skip the source, as every one here does.
var markRatings = map[string]int{"right": 1, "wrong": -1, "stale": -2}

func markName(rating int) string {
	for name, r := range markRatings {
		if r == rating {
			return name
		}
	}
	return ""
}

// searchTool reports whether a tool id in a turn's tools list is a web search (the replies this
// measurement is about): the same group the answer prompt gives the conclusion-first note (chat.searchPrefixes).
func searchTool(id string) bool {
	for _, p := range []string{"search.", "web.", "places."} {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	return false
}

// markBlock says why a turn cannot be marked, "" when it can: "expired" (the turn's detail aged out),
// "deleted" (its message was deleted or its speaker withdrew), "no_text" (the channel's logging level
// keeps no text) or "no_answer" (it has no reply to judge).
func markBlock(body map[string]any) string {
	switch {
	case len(body) == 0:
		return "expired"
	case body["message_deleted"] == true:
		return "deleted"
	case body["body_free"] != nil:
		return "no_text"
	}
	for _, k := range []string{"answer", "answer_preview"} {
		if v, _ := body[k].(string); strings.TrimSpace(v) != "" {
			return ""
		}
	}
	return "no_answer"
}

// turnMarks maps the turns to the operator's mark on them ("right", "wrong" or "stale"); unmarked turns are absent.
func (s *Service) turnMarks(ids []string) map[string]string {
	out := map[string]string{}
	db, err := s.adminDB()
	if err != nil || len(ids) == 0 {
		return out
	}
	args := []any{operatorSource}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.Reader().Query(`SELECT turn_id, rating FROM turn_feedback WHERE source=? AND turn_id IN (?`+strings.Repeat(",?", len(ids)-1)+`)`, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var rating int
		if rows.Scan(&id, &rating) == nil {
			out[id] = markName(rating)
		}
	}
	return out
}

// turnMarkAction: turns.mark (set or clear the operator's mark on one turn).
func (s *Service) turnMarkAction(req action.Request) action.Result {
	db, err := s.adminDB()
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	in := inputMap(req)
	id, verdict := strVal(in, "id", ""), strVal(in, "verdict", "")
	rating, ok := markRatings[verdict]
	if id == "" || (!ok && verdict != "clear") {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "id and verdict (right, wrong, stale or clear) required", nil)
	}
	var raw string
	if err := db.Reader().QueryRow(`SELECT body FROM turn_log WHERE id=?`, id).Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "turn not found", map[string]any{"reason": "gone"})
		}
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	if verdict == "clear" { // taking a mark back is always allowed
		if _, err := db.DB.Exec(`DELETE FROM turn_feedback WHERE turn_id=? AND user_hash=? AND source=?`, id, operatorRef, operatorSource); err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		s.emit("turn", id, map[string]any{"mark": ""})
		return action.OK(req, map[string]any{"id": id, "mark": ""})
	}
	body := map[string]any{}
	_ = json.Unmarshal([]byte(raw), &body)
	if why := markBlock(body); why != "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "turn cannot be marked: "+why, map[string]any{"reason": why})
	}
	now := s.Clock.Now().UTC().Format(time.RFC3339)
	q := `INSERT INTO turn_feedback(turn_id, user_hash, source, rating, reason, at) VALUES(?,?,?,?,'',?)
		ON CONFLICT(turn_id, user_hash, source) DO UPDATE SET rating=excluded.rating, at=excluded.at`
	if !db.IsSQLite() {
		q = `INSERT INTO turn_feedback(turn_id, user_hash, source, rating, reason, at) VALUES(?,?,?,?,'',?)
			ON DUPLICATE KEY UPDATE rating=VALUES(rating), at=VALUES(at)`
	}
	if _, err := db.DB.Exec(q, id, operatorRef, operatorSource, rating, now); err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	s.emit("turn", id, map[string]any{"mark": verdict})
	return action.OK(req, map[string]any{"id": id, "mark": verdict})
}

const (
	sampleDefault = 30
	sampleMax     = 100
	// sampleScan bounds how many candidate turns one draw looks at before giving up on filling the sample
	sampleScan = 3000
	// samplePool bounds how many candidate turn ids one draw reads (newest first)
	samplePool = 20000
)

// turnSampleAction: turns.sample (a random draw of search turns that can still be marked and have not
// been yet, so one pass through the sample measures new turns). Input: n (default 30, at most 100),
// days (only turns of the last N days, default all), lab (false by default, as in turns.list),
// include_marked (also turns that already have a mark).
func (s *Service) turnSampleAction(req action.Request) action.Result {
	db, err := s.adminDB()
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	in := inputMap(req)
	n := min(max(intVal(in, "n", sampleDefault), 1), sampleMax)
	where := []string{"tools<>''", "outcome='answered'", "body<>'{}'"}
	var args []any
	switch strVal(in, "lab", "false") {
	case "only":
		where = append(where, "lab=1")
	case "all":
	default:
		where = append(where, "lab=0")
	}
	if d := intVal(in, "days", 0); d > 0 {
		where, args = append(where, "at>=?"), append(args, s.Clock.Now().AddDate(0, 0, -d).UTC().Format(time.RFC3339))
	}
	if !boolVal(in, "include_marked") {
		where, args = append(where, "id NOT IN (SELECT turn_id FROM turn_feedback WHERE source=?)"), append(args, operatorSource)
	}
	rows, err := db.Reader().Query(`SELECT id, tools FROM turn_log WHERE `+strings.Join(where, " AND ")+` ORDER BY at DESC LIMIT `+fmt.Sprint(samplePool), args...)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	var pool []string
	for rows.Next() {
		var id, tools string
		if rows.Scan(&id, &tools) != nil {
			continue
		}
		for _, tl := range strings.Split(tools, ",") {
			if searchTool(tl) {
				pool = append(pool, id)
				break
			}
		}
	}
	rows.Close()
	rand.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	list := []TurnRecord{}
	for i, id := range pool {
		if len(list) >= n || i >= sampleScan {
			break
		}
		row := db.Reader().QueryRow(`SELECT id, at, guild_id, channel_id, outcome, tools, hits, cost_usd, latency_ms, lab, body FROM turn_log WHERE id=?`, id)
		t, err := scanTurn(row.Scan, false)
		if err != nil || t.MarkBlocked != "" { // markBlock is read from the full body in scanTurn
			continue
		}
		list = append(list, t)
	}
	s.decorateTurns(list)
	return action.OK(req, map[string]any{"turns": list, "pool": len(pool)})
}

// turnMarksAction: turns.marks (the operator's marks over the last N days, by verdict and by tool).
// A turn that used two tools counts once for each; the total counts every marked turn once.
func (s *Service) turnMarksAction(req action.Request) action.Result {
	db, err := s.adminDB()
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	since := s.Clock.Now().AddDate(0, 0, -max(intVal(inputMap(req), "days", 30), 1)).UTC().Format(time.RFC3339)
	rows, err := db.Reader().Query(`SELECT f.rating, t.tools FROM turn_feedback f JOIN turn_log t ON t.id=f.turn_id WHERE f.source=? AND f.at>=?`, operatorSource, since)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	defer rows.Close()
	total := map[string]int{"right": 0, "wrong": 0, "stale": 0}
	byTool := map[string]map[string]int{}
	for rows.Next() {
		var rating int
		var tools string
		if rows.Scan(&rating, &tools) != nil {
			continue
		}
		verdict := markName(rating)
		if verdict == "" {
			continue
		}
		total[verdict]++
		if tools == "" {
			continue
		}
		seen := map[string]bool{}
		for _, tl := range strings.Split(tools, ",") {
			if tl == "" || seen[tl] {
				continue
			}
			seen[tl] = true
			if byTool[tl] == nil {
				byTool[tl] = map[string]int{"right": 0, "wrong": 0, "stale": 0}
			}
			byTool[tl][verdict]++
		}
	}
	list := make([]map[string]any, 0, len(byTool))
	for tl, c := range byTool {
		list = append(list, map[string]any{"tool": tl, "right": c["right"], "wrong": c["wrong"], "stale": c["stale"]})
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		na, nb := a["right"].(int)+a["wrong"].(int)+a["stale"].(int), b["right"].(int)+b["wrong"].(int)+b["stale"].(int)
		if na != nb {
			return na > nb
		}
		return a["tool"].(string) < b["tool"].(string)
	})
	return action.OK(req, map[string]any{"total": total, "tools": list, "since": since})
}
