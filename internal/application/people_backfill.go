package application

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/lang"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/episodes"
	"masamong/internal/memory/peoplebackfill"
	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
	"masamong/internal/providers"
	"masamong/internal/providers/openai"
	"masamong/internal/storage"
)

// The paid people backfill (person memory phase 4, contract person-memory-v170 §5): an operator fills
// person memory from a server's past Discord conversations with the person_extract prompt, on the ops
// Anthropic provider only. It is never automatic: no update, restart, index pass or room-memory pass
// starts it, and a restart only goes on polling the batches of a run an operator started.
//
// memory.people.backfill (operator only, no Discord command) takes a mode:
//
//	dry_run  the default: windows, lines, token and cost estimates, whether the cap holds, and a plan
//	         token. No model call; the plan's parameters are kept (kv) so pilot and run need only the token.
//	pilot    confirm=<token>: 60 windows spread over rooms and time, each through Haiku 5.5 and Sonnet 5.5
//	         on the standard API. The answers are stored per window, not applied; the report has counts
//	         only and memory.people.backfill.review shows the review set to the console.
//	run      confirm=<token>, after a finished pilot of that plan: the chosen model through Message
//	         Batches of at most 1,000 windows. Pilot windows apply the chosen model's stored answer.
//	status   the server's latest run (counts only).
//
// Windows are built per room from the stored observations, read in pages (the store has hundreds of
// thousands of rows): episodes.Split segments joined up to 120 merged lines (peoplebackfill). A window
// is applied in one transaction through savePersonClaims, which checks evidence, consent, tombstones and
// the suppress table again; a window whose prompt changed since it was sent goes back to the queue.
// Spend goes to memory_spend kind people_backfill and to the run, settled from the reported usage.

const (
	pbPlanKey      = "people:backfill:plan:" // + guild: the last dry run's parameters and token
	pbLiveSinceKey = "people:live_since:"    // + guild: when live extraction first saved (RFC 3339)

	pbDefaultBudget = 3.0
	pbMaxBudget     = 6.0  // the owner's ceiling: $3 by default, Sonnet allowed up to about $6
	pbPageLines     = 2000 // observations read per page
	pbChunkMax      = 4 * pbPageLines
	pbMaxBatch      = 1000 // windows per Message Batches job (contract §5)
	// pbMaxTokens is the output limit of one window's answer. 1400 (the room-memory extraction's) was too
	// short on live data: a small model's answer for a busy window was cut at it again on every ask, so the
	// pilot could not pass. Only the tokens written are billed; the budget reserves pbWireOut per send.
	pbMaxTokens = 4096
	// pbThinkRoom is the output room the adapter adds on top of pbMaxTokens when the model may think at
	// effort low (openai thinkingRoom, the thinking headroom, a manual thinking budget: all 1024), so
	// pbWireOut is the most output tokens one request can be billed for.
	pbThinkRoom = 1024
	pbWireOut   = pbMaxTokens + pbThinkRoom
	// pbFraming bounds the tokens the Messages format adds around the system and user text.
	pbFraming       = 256
	pbMaxAttempts   = 3 // sends of one window before it is failed
	pbPilotWorkers  = 4
	pbHearsaySample = 30
	// pbHardGap is the segmenter's default hard cut (episodes.Options.HardGap): a page is cut only where
	// Split itself would start a new block, so paging never changes a segment.
	pbHardGap = 45 * time.Minute
)

// pbWindowLines is the merged-line limit of a window (0 = peoplebackfill.MaxWindowLines; tests shorten it).
var pbWindowLines int

// pbModels are the two models the backfill may use, by the names the action takes.
var pbModels = map[string]peoplebackfill.Model{"haiku": peoplebackfill.ModelHaiku, "sonnet": peoplebackfill.ModelSonnet}

var pbPilotModels = []peoplebackfill.Model{peoplebackfill.ModelHaiku, peoplebackfill.ModelSonnet}

var (
	errPBNoAnthropic = errors.New("people backfill: no native Anthropic provider with a key")
	errPBAmbiguous   = errors.New("people backfill: several native Anthropic providers; name one")
)

// pbParams are a plan's parameters: what a dry run was asked for, with the defaults resolved.
type pbParams struct {
	Guild     string   `json:"guild_id"`
	Rooms     []string `json:"rooms"` // room keys, sorted
	Since     string   `json:"since,omitempty"`
	Until     string   `json:"until"`
	UntilFrom string   `json:"until_from"` // input | live_since | plan | now
	BudgetUSD float64  `json:"budget_usd"`
	Token     string   `json:"token,omitempty"`
	CreatedAt string   `json:"created_at,omitempty"`
}

// pbWindow is one window of a plan: ids and counts, never text.
type pbWindow struct {
	ID      string
	Room    string
	Seq     int
	Lines   int // merged lines
	Obs     []string
	First   time.Time
	Last    time.Time
	Skipped bool // never sent: too thin, or covered
	Covered bool // an earlier run of the server completed it with every line it has now
}

// pbPlan is a plan built from the stored observations.
type pbPlan struct {
	Params      pbParams
	Windows     []pbWindow
	Rooms       int // rooms with at least one window
	Lines       int // observations read
	MergedLines int
	Skipped     int // windows too thin to send
	Covered     int // windows an earlier run completed (pbMarkCovered)
	Token       string
}

func init() {
	featureDDL = append(featureDDL, store.PeopleBackfillDDL...)
}

// peopleBackfillAction is memory.people.backfill and memory.people.backfill.review.
func (s *Service) peopleBackfillAction(ctx context.Context, req action.Request) action.Result {
	if req.Invoker != nil {
		// operator only: the console and the CLI; there is no Discord command and no chat tool
		return action.Fail(req, errcode.PermissionDenied, "error.permission", s.msg(req, "error.permission"), nil)
	}
	if s.practice {
		return action.Fail(req, errcode.FeatureDisabled, "people_backfill.unavailable", s.msg(req, "people_backfill.unavailable"), nil)
	}
	db, err := s.adminDB()
	if err != nil {
		return s.adminDBFail(req, err)
	}
	if !db.IsSQLite() {
		return action.Fail(req, errcode.FeatureDisabled, "people_backfill.unavailable", s.msg(req, "people_backfill.unavailable"), nil)
	}
	in := inputMap(req)
	if req.Action == "memory.people.backfill.review" {
		return s.peopleBackfillReview(ctx, req, db, in)
	}
	switch mode := strVal(in, "mode", "dry_run"); mode {
	case "dry_run":
		return s.pbDryRun(ctx, req, db, in)
	case "pilot":
		return s.pbStartPilot(ctx, req, db, in)
	case "run":
		return s.pbStartRun(ctx, req, db, in)
	case "status":
		return s.pbStatus(req, db, in)
	default:
		return action.Fail(req, errcode.InvalidInput, "error.invalid_input", s.msg(req, "error.invalid_input"), map[string]any{"mode": "dry_run|pilot|run|status"})
	}
}

// ---------------------------------------------------------------------------
// the plan
// ---------------------------------------------------------------------------

// pbGuild is the server the request names, or the only server with stored Discord rooms.
func pbGuild(db *storage.Store, in map[string]any) (string, error) {
	if g := strVal(in, "guild_id", ""); g != "" {
		return g, nil
	}
	rows, err := db.Reader().Query(`SELECT room FROM memory_rooms WHERE platform='discord' ORDER BY room`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	guilds := map[string]bool{}
	for rows.Next() {
		var room string
		if err := rows.Scan(&room); err != nil {
			return "", err
		}
		if parts := strings.SplitN(room, ":", 3); len(parts) == 3 && parts[1] != "0" && parts[1] != "" {
			guilds[parts[1]] = true
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(guilds) == 1 {
		for g := range guilds {
			return g, nil
		}
	}
	return "", nil
}

// pbRooms are the server's stored Discord rooms, narrowed to want (room keys or channel ids) when given.
func pbRooms(db *storage.Store, guild string, want []string) ([]string, []string, error) {
	rows, err := db.Reader().Query(`SELECT room FROM memory_rooms WHERE platform='discord' AND room LIKE ? ORDER BY room`, "discord:"+guild+":%")
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var all []string
	for rows.Next() {
		var room string
		if err := rows.Scan(&room); err != nil {
			return nil, nil, err
		}
		if strings.HasPrefix(room, "discord:"+guild+":") {
			all = append(all, room)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(want) == 0 {
		return all, nil, nil
	}
	var out, unknown []string
	for _, w := range want {
		w = strings.TrimSpace(w)
		room := w
		if !strings.HasPrefix(w, "discord:") {
			room = "discord:" + guild + ":" + w
		}
		if slices.Contains(all, room) {
			out = append(out, room)
		} else if w != "" {
			unknown = append(unknown, w)
		}
	}
	sort.Strings(out)
	return slices.Compact(out), unknown, nil
}

// pbTime reads an RFC 3339 time or a date (the start of that day in the instance's zone).
func (s *Service) pbTime(v string) (time.Time, bool) {
	if v == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t.UTC(), true
	}
	if t, err := time.ParseInLocation("2006-01-02", v, s.instanceZone()); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

// pbResolve turns a request into a plan's parameters. The end of the range is, in order: the request's
// until, the time live extraction first saved person claims for the server (kv), the until of the
// server's stored plan (so a repeated dry run plans the same range), else now.
func (s *Service) pbResolve(db *storage.Store, in map[string]any) (pbParams, map[string]any, error) {
	guild, err := pbGuild(db, in)
	if err != nil {
		return pbParams{}, nil, err
	}
	if guild == "" || guild == "0" {
		return pbParams{}, map[string]any{"field": "guild_id"}, nil
	}
	rooms, unknown, err := pbRooms(db, guild, strSlice(in, "rooms"))
	if err != nil {
		return pbParams{}, nil, err
	}
	if len(unknown) > 0 {
		return pbParams{}, map[string]any{"field": "rooms", "unknown": len(unknown)}, nil
	}
	budget, ok := pbBudgetIn(in)
	if !ok {
		return pbParams{}, map[string]any{"field": "budget_usd", "min_exclusive": 0, "max": pbMaxBudget}, nil
	}
	p := pbParams{Guild: guild, Rooms: rooms, BudgetUSD: budget}
	if v := strVal(in, "since", ""); v != "" {
		t, ok := s.pbTime(v)
		if !ok {
			return pbParams{}, map[string]any{"field": "since"}, nil
		}
		p.Since = t.Format(time.RFC3339)
	}
	if v := strVal(in, "until", ""); v != "" {
		t, ok := s.pbTime(v)
		if !ok {
			return pbParams{}, map[string]any{"field": "until"}, nil
		}
		p.Until, p.UntilFrom = t.Format(time.RFC3339), "input"
	}
	if p.Until == "" {
		var live string
		if ok, _ := s.kvGet(pbLiveSinceKey+guild, &live); ok {
			if t, ok := s.pbTime(live); ok {
				p.Until, p.UntilFrom = t.Format(time.RFC3339), "live_since"
			}
		}
	}
	if p.Until == "" {
		var prev pbParams
		if ok, _ := s.kvGet(pbPlanKey+guild, &prev); ok && prev.Guild == guild && prev.Until != "" {
			p.Until, p.UntilFrom = prev.Until, "plan"
		}
	}
	if p.Until == "" {
		p.Until, p.UntilFrom = s.Clock.Now().UTC().Format(time.RFC3339), "now"
	}
	return p, nil, nil
}

// pbBudgetIn reads budget_usd, the server's cap: absent (or an empty string) is the default; anything
// else must be a number above 0 and at most pbMaxBudget, kept to the cent below (never rounded up). A
// malformed or out-of-range value is refused, never replaced by the default.
func pbBudgetIn(in map[string]any) (float64, bool) {
	v, present := in["budget_usd"]
	if !present || v == nil {
		return pbDefaultBudget, true
	}
	if str, ok := v.(string); ok && strings.TrimSpace(str) == "" {
		return pbDefaultBudget, true
	}
	raw := strVal(in, "budget_usd", "")
	f, err := strconv.ParseFloat(raw, 64)
	if raw == "" || err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 || f > pbMaxBudget {
		return 0, false
	}
	f = math.Floor(f*100+1e-6) / 100
	return f, f > 0
}

// pbPromptVersion identifies a pack's person_extract prompt: part of every window id, so an answer to
// an older prompt never applies to a newer one.
func pbPromptVersion(pack *lang.Pack) string {
	sum := sha256.Sum256([]byte(pack.Memory.PersonExtractSystem + "\x00" + pack.Memory.PersonExtract))
	return hex.EncodeToString(sum[:8])
}

func pbRoomOf(room string) memRoom {
	parts := strings.SplitN(room, ":", 3)
	if len(parts) != 3 {
		return memRoom{room: room, source: "discord"}
	}
	return memRoom{guild: parts[1], channel: parts[2], room: room, source: "discord"}
}

// pbBuildPlan builds the plan's windows, room by room, page by page.
func (s *Service) pbBuildPlan(ctx context.Context, db *storage.Store, p pbParams) (pbPlan, error) {
	plan := pbPlan{Params: p}
	since, _ := s.pbTime(p.Since)
	until, _ := s.pbTime(p.Until)
	doc := s.Doc()
	for _, room := range p.Rooms {
		if gone, err := s.pbThreadDeleted(db.Reader(), room); err != nil {
			return plan, err
		} else if gone {
			continue // a deleted thread keeps its stored lines until they are redacted; none is planned
		}
		r := pbRoomOf(room)
		pack, _ := s.roomVoice(r)
		opt := peoplebackfill.Options{PromptHash: pbPromptVersion(pack), MaxLines: pbWindowLines,
			Split: episodes.Options{Location: s.instanceZone(), DayBoundary: doc.Memory.Boundary(), Tokenizer: tokenizerFor(pack)}}
		had := false
		err := s.pbPageRoom(ctx, db, r, since, until, func(lines []peoplebackfill.Line) {
			plan.Lines += len(lines)
			for _, w := range peoplebackfill.BuildWindows(room, lines, opt) {
				pw := pbWindow{ID: w.ID, Room: room, Seq: len(plan.Windows), Lines: len(w.Lines), First: w.First, Last: w.Last, Skipped: w.Skipped}
				for _, ml := range w.Lines {
					pw.Obs = append(pw.Obs, ml.IDs...)
				}
				plan.MergedLines += pw.Lines
				if pw.Skipped {
					plan.Skipped++
				}
				plan.Windows = append(plan.Windows, pw)
				had = true
			}
		})
		if err != nil {
			return plan, err
		}
		if had {
			plan.Rooms++
		}
	}
	if err := pbMarkCovered(db, &plan); err != nil {
		return plan, err
	}
	plan.Token = pbToken(p, plan.Windows)
	plan.Params.Token = plan.Token
	return plan, nil
}

// pbMarkCovered marks the plan's windows that an earlier run of the server completed (applied or
// refused), when that window held every line the plan's window holds now: they are never sent again, by
// a pilot or a batch, whatever plan they come back in. A window with a line it did not have (a message
// captured late) is new history and is sent.
func pbMarkCovered(db *storage.Store, plan *pbPlan) error {
	ids := make([]string, 0, len(plan.Windows))
	for _, w := range plan.Windows {
		if !w.Skipped {
			ids = append(ids, w.ID)
		}
	}
	done, err := store.BackfillCompleted(db.Reader(), plan.Params.Guild, ids)
	if err != nil {
		return err
	}
	for i := range plan.Windows {
		w := &plan.Windows[i]
		had, ok := done[w.ID]
		if !ok || w.Skipped {
			continue
		}
		covered := true
		for _, id := range w.Obs {
			covered = covered && slices.Contains(had, id)
		}
		if covered {
			w.Skipped, w.Covered = true, true
			plan.Covered++
		}
	}
	return nil
}

// pbToken is the plan's fingerprint: its parameters and its windows with the lines each holds. Any
// change of either (a deleted message, a captured old message, another range or cap) gives another token.
func pbToken(p pbParams, ws []pbWindow) string {
	h := sha256.New()
	fmt.Fprintf(h, "people-backfill/1\x00%s\x00%s\x00%s\x00%s\x00%.2f\n", p.Guild, strings.Join(p.Rooms, ","), p.Since, p.Until, p.BudgetUSD)
	for _, w := range ws {
		fmt.Fprintf(h, "%s %t %d %s\n", w.ID, w.Skipped, w.Lines, strings.Join(w.Obs, ","))
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}

// pbPageRoom reads a room's current human and bot lines in [since, until) in pages, oldest first, and
// hands them to emit in chunks cut where the segmenter cuts a hard block, so no segment crosses two
// chunks. A chunk with no such cut is handed over whole at pbChunkMax lines (the same place every time).
func (s *Service) pbPageRoom(ctx context.Context, db *storage.Store, r memRoom, since, until time.Time, emit func([]peoplebackfill.Line)) error {
	naive := s.naiveZone()
	loc, boundary := s.instanceZone(), s.Doc().Memory.Boundary()
	var buf []peoplebackfill.Line
	cut := func(final bool) {
		if len(buf) == 0 {
			return
		}
		if final {
			emit(buf)
			buf = nil
			return
		}
		at := 0
		for i := len(buf) - 1; i > 0; i-- {
			a, b := buf[i-1].At, buf[i].At
			if b.Sub(a) >= pbHardGap || episodes.DayKey(a, boundary, loc) != episodes.DayKey(b, boundary, loc) {
				at = i
				break
			}
		}
		if at == 0 {
			if len(buf) < pbChunkMax {
				return
			}
			at = len(buf)
		}
		emit(buf[:at])
		buf = append([]peoplebackfill.Line(nil), buf[at:]...)
	}
	lastParsed, lastID := "", ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := db.Reader().QueryContext(ctx, `SELECT observation_id, actor_label, actor_account, content, parsed_time, record_kind FROM memory_observations
			WHERE room=? AND source_room=room AND deleted_at='' AND record_kind IN ('`+corpus.HumanText+`','`+corpus.BotRecord+`')
			AND (parsed_time>? OR (parsed_time=? AND observation_id>?)) ORDER BY parsed_time, observation_id LIMIT ?`,
			r.room, lastParsed, lastParsed, lastID, pbPageLines)
		if err != nil {
			return err
		}
		n := 0
		for rows.Next() {
			var id, label, account, content, parsed, kind string
			if err := rows.Scan(&id, &label, &account, &content, &parsed, &kind); err != nil {
				rows.Close()
				return err
			}
			n++
			lastParsed, lastID = parsed, id
			at, ok := s.obsTime(parsed, naive)
			if !ok || at.Before(since) || (!until.IsZero() && !at.Before(until)) {
				continue
			}
			l := peoplebackfill.Line{ID: id, At: at, Bot: kind == corpus.BotRecord, Text: strings.TrimSpace(content)}
			if !l.Bot {
				l.Ref = personRefOf(r, episodes.Message{Account: account, Author: label})
			}
			buf = append(buf, l)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if n < pbPageLines {
			break
		}
		sort.SliceStable(buf, func(i, j int) bool { return buf[i].At.Before(buf[j].At) })
		if len(buf) >= pbPageLines {
			cut(false)
		}
	}
	sort.SliceStable(buf, func(i, j int) bool { return buf[i].At.Before(buf[j].At) })
	cut(true)
	return nil
}

// pbEstimate is the low/high USD of sending windows on model in mode.
func pbEstimate(ws []pbWindow, model peoplebackfill.Model, mode peoplebackfill.Mode) (low, high float64) {
	for _, w := range ws {
		if w.Skipped {
			continue
		}
		l, h := pbWindowCost(w.Lines, model, mode)
		low, high = low+l, high+h
	}
	return low, high
}

func pbWindowCost(lines int, model peoplebackfill.Model, mode peoplebackfill.Mode) (low, high float64) {
	l, h, err := peoplebackfill.EstimateCost([]peoplebackfill.Window{{Lines: make([]peoplebackfill.MergedLine, lines)}}, model, mode)
	if err != nil {
		return 0, 0
	}
	return l, h
}

// pbPilotPick is the plan's pilot: a deterministic spread over rooms and time.
func pbPilotPick(ws []pbWindow) map[string]bool {
	in := make([]peoplebackfill.Window, 0, len(ws))
	for _, w := range ws {
		in = append(in, peoplebackfill.Window{ID: w.ID, Room: w.Room, First: w.First, Last: w.Last, Skipped: w.Skipped})
	}
	out := map[string]bool{}
	for _, w := range peoplebackfill.SelectPilot(in, peoplebackfill.PilotSize) {
		out[w.ID] = true
	}
	return out
}

func usd(v float64) float64 { return math.Round(v*1e6) / 1e6 }

// pbDryRunOutput is what a dry run reports: counts and money, no text. spent is what the server's
// backfill runs spent so far: the cap is a total across them.
func (s *Service) pbDryRunOutput(plan pbPlan, spent float64) map[string]any {
	var sendable, pilot, rest []pbWindow
	picks := pbPilotPick(plan.Windows)
	for _, w := range plan.Windows {
		if w.Skipped {
			continue
		}
		sendable = append(sendable, w)
		if picks[w.ID] {
			pilot = append(pilot, w)
		} else {
			rest = append(rest, w)
		}
	}
	tin, tout := 0, 0
	for _, w := range sendable {
		i, o := peoplebackfill.EstimateTokens([]peoplebackfill.Window{{Lines: make([]peoplebackfill.MergedLine, w.Lines)}})
		tin, tout = tin+i, tout+o
	}
	pilotLow, pilotHigh := 0.0, 0.0
	for _, m := range pbPilotModels {
		l, h := pbEstimate(pilot, m, peoplebackfill.ModeStandard)
		pilotLow, pilotHigh = pilotLow+l, pilotHigh+h
	}
	cost := map[string]any{"pilot": map[string]float64{"low": usd(pilotLow), "high": usd(pilotHigh)}}
	within := map[string]bool{}
	for name, m := range pbModels {
		per := map[string]any{}
		for _, mode := range []peoplebackfill.Mode{peoplebackfill.ModeBatch, peoplebackfill.ModeStandard} {
			l, h := pbEstimate(sendable, m, mode)
			per[string(mode)] = map[string]float64{"low": usd(l), "high": usd(h)}
		}
		_, runHigh := pbEstimate(rest, m, peoplebackfill.ModeBatch)
		per["run_after_pilot"] = map[string]float64{"high": usd(runHigh)}
		cost[name] = per
		within[name] = spent+pilotHigh+runHigh <= plan.Params.BudgetUSD
	}
	provider, ready := "", false
	if id, _, err := s.pbAdapter(s.Doc(), ""); err == nil {
		provider, ready = id, true
	}
	out := map[string]any{
		"mode": "dry_run", "guild_id": plan.Params.Guild, "rooms": plan.Rooms, "rooms_asked": len(plan.Params.Rooms),
		"since": plan.Params.Since, "until": plan.Params.Until, "until_from": plan.Params.UntilFrom,
		"windows": len(plan.Windows), "sent_windows": len(sendable), "skipped_windows": plan.Skipped, "covered_windows": plan.Covered, "pilot_windows": len(pilot),
		"lines": plan.Lines, "merged_lines": plan.MergedLines,
		"tokens":     map[string]int{"input": tin, "output": tout},
		"cost_usd":   cost,
		"budget_usd": plan.Params.BudgetUSD, "spent_usd": usd(spent), "left_usd": usd(max(0, plan.Params.BudgetUSD-spent)), "within_budget": within,
		"provider": provider, "provider_ready": ready,
		"token": plan.Token, "model_calls": 0,
	}
	if s.guildPrivacy(plan.Params.Guild).NoSensitive {
		// the server keeps person memory only after the sensitive-topic check, which the backfill does not
		// run: its claims are stored pending and are not read until something classifies them
		out["warnings"] = []string{"no_sensitive_pending"}
	}
	return out
}

// pbDryRun is mode=dry_run: no model call. It keeps the plan's parameters for pilot and run.
func (s *Service) pbDryRun(ctx context.Context, req action.Request, db *storage.Store, in map[string]any) action.Result {
	p, bad, err := s.pbResolve(db, in)
	if err != nil {
		return s.failErr(req, errcode.IO, "error.storage", err, nil)
	}
	if bad != nil {
		return action.Fail(req, errcode.InvalidInput, "error.invalid_input", s.msg(req, "error.invalid_input"), bad)
	}
	plan, err := s.pbBuildPlan(ctx, db, p)
	if err != nil {
		return s.failErr(req, errcode.IO, "error.storage", err, nil)
	}
	plan.Params.CreatedAt = s.Clock.Now().UTC().Format(time.RFC3339)
	if err := s.kvSet(pbPlanKey+p.Guild, plan.Params); err != nil {
		return s.failErr(req, errcode.IO, "error.storage", err, nil)
	}
	spent, err := pbGuildSpent(db.Reader(), p.Guild)
	if err != nil {
		return s.failErr(req, errcode.IO, "error.storage", err, nil)
	}
	return action.OK(req, s.pbDryRunOutput(plan, spent))
}

// pbConfirmedPlan is the plan a pilot or run confirms: the server's stored plan whose token is confirm,
// built again from the observations. A token that is not the stored one, or a plan that no longer gives
// it, is refused with the current plan's dry-run output (and that plan is stored).
func (s *Service) pbConfirmedPlan(ctx context.Context, req action.Request, db *storage.Store, in map[string]any) (pbPlan, *action.Result) {
	plan, _, fail := s.pbCheckPlan(ctx, req, db, in)
	return plan, fail
}

// pbCheckPlan is pbConfirmedPlan that also hands back the plan as it is now when it changed (changed:
// the plan built again, with its new token stored, and the plan_changed failure).
func (s *Service) pbCheckPlan(ctx context.Context, req action.Request, db *storage.Store, in map[string]any) (pbPlan, bool, *action.Result) {
	confirm := strVal(in, "confirm", "")
	guild, err := pbGuild(db, in)
	if err != nil {
		res := s.failErr(req, errcode.IO, "error.storage", err, nil)
		return pbPlan{}, false, &res
	}
	var stored pbParams
	ok, err := s.kvGet(pbPlanKey+guild, &stored)
	if err != nil {
		res := s.failErr(req, errcode.IO, "error.storage", err, nil)
		return pbPlan{}, false, &res
	}
	if guild == "" || !ok || confirm == "" {
		res := action.Fail(req, errcode.InvalidInput, "people_backfill.confirm_needed", s.msg(req, "people_backfill.confirm_needed"), map[string]any{"field": "confirm"})
		return pbPlan{}, false, &res
	}
	params := stored
	params.Token, params.CreatedAt = "", ""
	plan, err := s.pbBuildPlan(ctx, db, params)
	if err != nil {
		res := s.failErr(req, errcode.IO, "error.storage", err, nil)
		return pbPlan{}, false, &res
	}
	if plan.Token != confirm || stored.Token != confirm {
		plan.Params.CreatedAt = s.Clock.Now().UTC().Format(time.RFC3339)
		if err := s.kvSet(pbPlanKey+guild, plan.Params); err != nil {
			res := s.failErr(req, errcode.IO, "error.storage", err, nil)
			return pbPlan{}, false, &res
		}
		spent, _ := pbGuildSpent(db.Reader(), guild)
		res := action.Fail(req, errcode.StaleRevision, "people_backfill.plan_changed", s.msg(req, "people_backfill.plan_changed"),
			map[string]any{"dry_run": s.pbDryRunOutput(plan, spent)})
		return plan, true, &res
	}
	plan.Params.CreatedAt = stored.CreatedAt
	return plan, false, nil
}

// ---------------------------------------------------------------------------
// the provider
// ---------------------------------------------------------------------------

// pbTestOrigin is the one non-Anthropic origin (scheme://host:port) the backfill may reach: set by tests
// only, for their fake host. Nothing in config or input can set it.
var pbTestOrigin string

// pbNativeAnthropic reports whether a provider's base URL is Anthropic's own API: HTTPS to the host
// api.anthropic.com (default port, no user info). The configured wire format (api: anthropic) is not
// enough: a relay or reseller speaking the Messages format on another host is refused.
func pbNativeAnthropic(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.User != nil {
		return false
	}
	if pbTestOrigin != "" && strings.EqualFold(u.Scheme+"://"+u.Host, pbTestOrigin) {
		return true
	}
	return u.Scheme == "https" && strings.EqualFold(u.Hostname(), "api.anthropic.com") && (u.Port() == "" || u.Port() == "443")
}

// pbAdapter is the backfill's provider: a configured provider whose destination is native Anthropic
// (pbNativeAnthropic, checked on the base URL the adapter will call) in the Anthropic wire format, with a
// key. want names one; without it there must be exactly one. Never OpenRouter, a relay or another family.
func (s *Service) pbAdapter(doc config.Document, want string) (string, *openai.Adapter, error) {
	if s.practice {
		return "", nil, errPBNoAnthropic
	}
	var found []string
	for id, p := range doc.Providers {
		if p.Kind != config.KindOpenAI || p.Disabled || openai.ResolveAPI(p.API, p.BaseURL) != openai.APIAnthropic || !pbNativeAnthropic(p.BaseURL) {
			continue
		}
		if want != "" && id != want {
			continue
		}
		found = append(found, id)
	}
	sort.Strings(found)
	switch {
	case len(found) == 0:
		return "", nil, errPBNoAnthropic
	case len(found) > 1:
		return "", nil, errPBAmbiguous
	}
	id := found[0]
	p := doc.Providers[id]
	cred, err := s.credential(p.CredentialRef)
	if err != nil || !cred.Present() {
		return id, nil, errPBNoAnthropic
	}
	client := providers.NewClient(120*time.Second, s.Log)
	client.MaxBody = 64 << 20
	client.MaxRetries = 0 // a paid call is never sent again by the client: every send has its own reservation
	client.HTTP.Transport = pbSendTracker{base: client.HTTP.Transport}
	a := newLLMAdapter(client, p, cred)
	if a.API != openai.APIAnthropic || !pbNativeAnthropic(a.BaseURL) {
		return id, nil, errPBNoAnthropic // the resolved destination, checked again
	}
	a.Price, a.CachedPrice, a.ContextPrice = s.pricer(id), s.cachedPricer(id), s.contextPricer(id)
	a.Gate = s.llmGate(id)
	s.shareCaps(a, id, p)
	return id, a, nil
}

// pbProviderFail is the refusal for a missing or ambiguous provider.
func (s *Service) pbProviderFail(req action.Request, err error) action.Result {
	if errors.Is(err, errPBAmbiguous) {
		return action.Fail(req, errcode.InvalidInput, "people_backfill.provider_ambiguous", s.msg(req, "people_backfill.provider_ambiguous"), map[string]any{"field": "provider"})
	}
	return action.Fail(req, errcode.ProviderUnconfigured, "people_backfill.no_anthropic", s.msg(req, "people_backfill.no_anthropic"), nil)
}

// pbRequest is one window's request: no temperature, no cache_control (every window is unique, so a
// cache breakpoint would only add the cache-write surcharge), thinking at effort low with its room, and
// sent once (Once: an answer cut at the limit is not asked again; each send has its own reservation).
func pbRequest(model peoplebackfill.Model, bp pbPrompt) openai.ChatRequest {
	return openai.ChatRequest{Model: string(model), MaxTokens: pbMaxTokens, Effort: "low", Once: true,
		Messages: []openai.Message{{Role: "system", Content: bp.system}, {Role: "user", Content: bp.user}}}
}

// pbSettle prices a result's reported usage: input not read from the cache at the input price (cache
// writes at 1.25 of it), cache reads at a tenth, output at the output price.
func pbSettle(res *openai.ChatResult, model peoplebackfill.Model, mode peoplebackfill.Mode) float64 {
	if res == nil {
		return 0
	}
	in := res.PromptTokens - res.CachedTokens + (res.CacheWriteTokens+3)/4
	v, err := peoplebackfill.Settle(peoplebackfill.Usage{InputTokens: max(0, in), OutputTokens: res.CompletionTokens, CacheReadTokens: res.CachedTokens,
		PromptTokens: res.PromptTokens}, model, mode)
	if err != nil {
		return 0
	}
	return v
}

// ---------------------------------------------------------------------------
// one window's prompt
// ---------------------------------------------------------------------------

// pbPrompt is a window's person_extract prompt, built from the current lines.
type pbPrompt struct {
	p            episodes.Prompt
	room         string // the window's room: a thread deleted since the build sends nothing
	system, user string
	hash         string              // of system and user: an answer applies only to the same prompt
	text         map[string]string   // human line id → the text the model read
	author       map[string]string   // human line id → its speaker reference
	labels       map[string]string   // person reference → the label the prompt showed
	lines        map[string]string   // every line the prompt carries (the bot's too) → its stored text
	parts        map[string][]string // a prompt line (its first observation) → the observations merged into it
	readAt       string
	ok           bool            // a human line of at least the meaningful-rune floor is left
	gone         map[string]bool // the server's deleted threads before the build read any label (pbGoneThreads)
}

// pbBuildPrompt reads a window's lines as they are now and builds its prompt. Only lines that may leave
// the process go in (pbSendable): deleted lines, lines with a deletion or edit pending, and the lines of
// a person who withdrew or is blocked drop out.
func (s *Service) pbBuildPrompt(db *storage.Store, guild string, w store.BackfillWindow) (pbPrompt, error) {
	bp := pbPrompt{text: map[string]string{}, author: map[string]string{}, labels: map[string]string{}, lines: map[string]string{},
		parts: map[string][]string{}}
	bp.readAt, bp.room = s.Clock.Now().UTC().Format(time.RFC3339Nano), w.Room
	r := pbRoomOf(w.Room)
	// before any label is read: a thread deleted after this is caught before the send (pbPromptCurrent)
	gone, err := s.pbGoneThreads(db.Reader(), guild)
	if err != nil {
		return bp, err
	}
	bp.gone = gone
	lines, err := s.linesByID(db, w.Obs)
	if err == nil {
		lines, err = s.pbSendable(db, guild, r, lines)
	}
	if err != nil {
		return bp, err
	}
	pack, botName := s.roomVoice(r)
	// Consecutive lines of one speaker are one line of the prompt, the unit the plan counted
	// (peoplebackfill.BuildWindows): each keeps its own clip, and the merged line counts for the
	// meaningful-rune floor as a whole, as in the plan. A claim cites every observation of a line it cites.
	var msgs []episodes.Message
	var speakers []string
	merged := map[string]string{} // prompt line → its stored texts joined, for the floor
	botAccount, longest := "", pbLineRunes
	for _, l := range lines {
		m := l.Message
		bp.lines[m.ID] = m.Text
		speaker := ""
		if m.Bot {
			if botAccount == "" && m.Account != "" {
				botAccount = m.Account
			}
		} else {
			speaker = personRefOf(r, m)
			bp.text[m.ID], bp.author[m.ID] = m.Text, speaker
			bp.labels[speaker] = m.Author
		}
		if n := len(msgs); n > 0 && speakers[n-1] == speaker && msgs[n-1].Bot == m.Bot {
			last := &msgs[n-1]
			last.Text += "\n" + pbClip(m.Text)
			bp.parts[last.ID] = append(bp.parts[last.ID], m.ID)
			merged[last.ID] += "\n" + m.Text
			longest = max(longest, utf8.RuneCountInString(last.Text))
			continue
		}
		bp.parts[m.ID], merged[m.ID] = []string{m.ID}, m.Text
		m.Text = pbClip(m.Text)
		msgs, speakers = append(msgs, m), append(speakers, speaker)
	}
	for _, m := range msgs {
		if !m.Bot && search.MeaningfulRunes(merged[m.ID]) >= search.MinMessageRunes {
			bp.ok = true
		}
	}
	if !bp.ok {
		return bp, nil
	}
	if botAccount == "" && s.Discord() != nil {
		botAccount, _ = s.Discord().BotUser()
	}
	botRef := ""
	if botAccount != "" {
		botRef = personRef(botAccount)
	}
	mentioned, err := s.pbMentioned(db, guild, msgs, botRef)
	if err != nil {
		return bp, err
	}
	for _, m := range mentioned {
		if _, ok := bp.labels[m.Ref]; !ok {
			bp.labels[m.Ref] = m.Label
		}
	}
	p, ok := episodes.BuildPersonExtract(episodes.Input{Lines: msgs, BotName: botName, Now: msgs[len(msgs)-1].At, Location: s.instanceZone(),
		RefOf: func(m episodes.Message) string { return personRefOf(r, m) }, Mentioned: mentioned, BotRef: botRef, MaxRunes: longest})
	if !ok {
		bp.ok = false
		return bp, nil
	}
	bp.p = p
	bp.system = lang.Fill(pack.Memory.PersonExtractSystem, "bot", botName)
	bp.user = p.FillPersonExtract(pack.Memory.PersonExtract)
	sum := sha256.Sum256([]byte(bp.system + "\x00" + bp.user))
	bp.hash = hex.EncodeToString(sum[:16])
	return bp, nil
}

// pbLineRunes is the clip of one message in the prompt (the episode prompt's default per line).
const pbLineRunes = 400

func pbClip(text string) string {
	if utf8.RuneCountInString(text) <= pbLineRunes {
		return text
	}
	return string([]rune(text)[:pbLineRunes]) + "…"
}

// pbPartsOf are the observations of the prompt line id stands for (id itself when it is not a line's first).
func (bp pbPrompt) pbPartsOf(id string) []string {
	if parts, ok := bp.parts[id]; ok {
		return parts
	}
	return []string{id}
}

// pbSendable keeps the lines that may be sent to the provider: the room is not a deleted thread, the
// stored line is current (not deleted, no deletion or edit pending, its account not tombstoned) and a
// human author has not withdrawn memory or profile consent, is not blocked, and has no person tombstone.
// One read transaction.
func (s *Service) pbSendable(db *storage.Store, guild string, r memRoom, lines []roomLine) ([]roomLine, error) {
	if len(lines) == 0 {
		return nil, nil
	}
	tx, err := db.Reader().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if gone, err := s.pbThreadDeleted(tx, r.room); err != nil || gone {
		return nil, err
	}
	denied, err := s.deniedPeople(tx, guild)
	if err != nil {
		return nil, err
	}
	out := lines[:0:0]
	for _, l := range lines {
		if ok, err := store.ObservationCurrent(tx, l.ID); err != nil || !ok {
			if err != nil {
				return nil, err
			}
			continue
		}
		if !l.Bot {
			ref := personRefOf(r, l.Message)
			if denied[ref] {
				continue
			}
			if blocked, err := store.SourceBlocked(tx, ref); err != nil || blocked {
				if err != nil {
					return nil, err
				}
				continue
			}
		}
		out = append(out, l)
	}
	return out, nil
}

// pbThreadDeleted reports whether a room is a thread that was deleted: its deletion is pending (the
// tombstone not written yet) or its tombstone (thread_tombstones, or the closed row's reason) is stored.
// A thread's deletion leaves its observations in place until they are redacted, so the backfill checks
// the room itself, when it plans, builds and sends. q is the caller's transaction or the reader.
func (s *Service) pbThreadDeleted(q interface {
	QueryRow(string, ...any) *sql.Row
}, room string) (bool, error) {
	r := pbRoomOf(room)
	if r.channel == "" {
		return false, nil
	}
	t := &s.threads
	t.mu.Lock()
	pending := t.deleted[r.channel]
	t.mu.Unlock()
	if pending {
		return true, nil
	}
	var gone bool
	err := q.QueryRow(`SELECT EXISTS(SELECT 1 FROM thread_tombstones WHERE thread_id=?1)
		OR EXISTS(SELECT 1 FROM channel_threads WHERE thread_id=?1 AND close_reason='deleted')`, r.channel).Scan(&gone)
	return gone, err
}

// pbGoneThreads are the threads of a server's memory rooms that were deleted: the deletion pending in
// memory, its tombstone, or the closed row's reason (the rooms mentionedPeople reads no label from).
func (s *Service) pbGoneThreads(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, guild string) (map[string]bool, error) {
	prefix := "discord:" + guild + ":"
	rows, err := q.Query(`SELECT substr(room, ?2), substr(room, ?2) IN (SELECT thread_id FROM thread_tombstones)
		OR substr(room, ?2) IN (SELECT thread_id FROM channel_threads WHERE close_reason='deleted')
		FROM memory_rooms WHERE platform='discord' AND room LIKE ?1`, prefix+"%", len(prefix)+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	gone, stored := map[string]bool{}, map[string]bool{}
	for rows.Next() {
		var id string
		var tomb bool
		if err := rows.Scan(&id, &tomb); err != nil {
			return nil, err
		}
		stored[id] = tomb
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	t := &s.threads // as pbThreadDeleted: a pending deletion is in memory whether the table was loaded or not
	t.mu.Lock()
	for id, tomb := range stored {
		if tomb || t.deleted[id] {
			gone[id] = true
		}
	}
	t.mu.Unlock()
	return gone, nil
}

// pbPromptCurrent checks again, in the caller's transaction and just before a send, everything a prompt
// carries: its room is not a deleted thread, every line is still stored with the text it had, current, and
// no person the prompt shows (author or mentioned) has withdrawn, been blocked or tombstoned since it was
// built. False means build it again: nothing of it may leave.
func (s *Service) pbPromptCurrent(tx *sql.Tx, guild string, bp pbPrompt) (bool, error) {
	if gone, err := s.pbThreadDeleted(tx, bp.room); err != nil || gone {
		return false, err
	}
	// a thread of the server deleted since the build: a label the prompt shows for a mentioned person may
	// be one only that thread gave (mentionedPeople leaves those out), so the prompt is built again
	gone, err := s.pbGoneThreads(tx, guild)
	if err != nil {
		return false, err
	}
	for id := range gone {
		if !bp.gone[id] {
			return false, nil
		}
	}
	denied, err := s.deniedPeople(tx, guild)
	if err != nil {
		return false, err
	}
	for ref := range bp.labels {
		if denied[ref] {
			return false, nil
		}
		if blocked, err := store.SourceBlocked(tx, ref); err != nil || blocked {
			return false, err
		}
	}
	for id, text := range bp.lines {
		var content, deleted string
		err := tx.QueryRow(`SELECT content,deleted_at FROM memory_observations WHERE observation_id=?`, id).Scan(&content, &deleted)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if deleted != "" || strings.TrimSpace(content) != text {
			return false, nil
		}
		if ok, err := store.ObservationCurrent(tx, id); err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// pbMentioned are the people a window names without writing in it: accounts of <@id> mentions (with
// their latest label in the server) and exact single-account alias matches (link name), the same
// signals a chat turn resolves (readPersonNames and turnNames.match, used read-only here).
// TODO(person memory): live extraction builds the same list; once it has a helper, both should share it.
func (s *Service) pbMentioned(db *storage.Store, guild string, msgs []episodes.Message, botRef string) ([]episodes.Person, error) {
	return s.mentionedPeople(db, guild, msgs, botRef) // one helper for live extraction and the backfill
}

// pbAttribute ties a validated fact to the lines it cites, or drops it. Its evidence is every observation
// of the prompt lines it cites. Those must all be one speaker's: a fact cited from two people's lines (a
// question of one and the answer of another, or what one said and another acknowledged) cannot be
// attributed to either, so it is dropped rather than split into a self claim with part of its provenance.
// The link comes from the cited lines too: self when the speaker is the subject, mention when a cited line
// names the subject by <@id>, otherwise name (the subject was found by a name: reviewed in the pilot).
// It is idempotent: a stored pilot fact goes through it again when it is applied.
func pbAttribute(f episodes.PersonFact, bp pbPrompt) (episodes.PersonFact, bool) {
	var ev []string
	authors := map[string]bool{}
	for _, id := range f.Ev {
		for _, part := range bp.pbPartsOf(id) {
			if !slices.Contains(ev, part) {
				ev = append(ev, part)
			}
			authors[bp.author[part]] = true
		}
	}
	if len(ev) == 0 || len(authors) != 1 || len(f.By) != 1 || !authors[f.By[0]] {
		return f, false
	}
	f.Ev = ev
	switch {
	case f.By[0] == f.About:
		f.Link = episodes.LinkSelf
	case f.Link == episodes.LinkReply:
	case pbNamesByID(bp, ev, f.About):
		f.Link = episodes.LinkMention
	default:
		f.Link = episodes.LinkName
	}
	return f, true
}

// pbNamesByID reports whether one of the lines names the person by an account mention (<@id>).
func pbNamesByID(bp pbPrompt, ids []string, ref string) bool {
	for _, id := range ids {
		for _, sub := range mentionPattern.FindAllStringSubmatch(bp.text[id], -1) {
			if personRef(sub[1]) == ref {
				return true
			}
		}
	}
	return false
}

// pbFacts parses a window's answer into attributed facts; dropped facts count as rejected.
func pbFacts(bp pbPrompt, content string) ([]episodes.PersonFact, int) {
	facts, rejected := bp.p.ParsePeople(content, episodes.PeopleOptions{})
	out := facts[:0:0]
	for _, f := range facts {
		if g, ok := pbAttribute(f, bp); ok {
			out = append(out, g)
		} else {
			rejected++
		}
	}
	return out, rejected
}

// pbClaims turns a window's facts into claims for the write gate: one speaker's claim per attributable
// fact (pbAttribute), with the text each evidence line had when the model read it.
func pbClaims(guild, runID string, facts []episodes.PersonFact, bp pbPrompt) (claims []store.PersonClaim, dropped int) {
	for _, f := range facts {
		f, ok := pbAttribute(f, bp)
		if !ok {
			dropped++
			continue
		}
		lines := map[string]string{}
		for _, id := range f.Ev {
			lines[id] = bp.text[id]
		}
		claims = append(claims, store.PersonClaim{GuildID: guild, SubjectRef: f.About, SpeakerRef: f.By[0], Kind: f.Kind, Topic: f.Topic,
			Text: f.Text, Evidence: f.Ev, Source: "backfill:" + runID, Link: f.Link, Lines: lines, ReadAt: bp.readAt})
	}
	return claims, dropped
}

// pbApply writes one window's claims and settles it, in one transaction: the claims through the write
// gate, the window's state and counts, and the spend (run and memory_spend). The same transaction checks
// the whole window again (pbPromptCurrent): a line changed since bp was built (cited or only context),
// or a person withdrew, sends the window back to the queue instead, charged once. readAt is when the
// lines were read for the prompt the model answered; an edit after it refuses the claims citing it.
func (s *Service) pbApply(ctx context.Context, db *storage.Store, runID, guild string, w store.BackfillWindow, facts []episodes.PersonFact, rejected int,
	bp pbPrompt, readAt string, spent float64, calls int) error {
	now := s.Clock.Now().UTC()
	return db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		current, err := s.pbPromptCurrent(tx, guild, bp)
		if err != nil {
			return err
		}
		if !current {
			w.State, w.Batch, w.Result = store.WindowQueued, "", nil
			if w.Attempts >= pbMaxAttempts {
				w.State = store.WindowFailed
			}
			w.CostUSD += spent
			w.UpdatedAt = now.Format(time.RFC3339)
			return pbSettleTx(tx, runID, w, spent, calls, now)
		}
		if readAt != "" {
			bp.readAt = readAt
		}
		claims, dropped := pbClaims(guild, runID, facts, bp)
		saved, err := s.savePersonClaims(ctx, tx, claims)
		if err != nil {
			return err
		}
		w.State, w.Batch, w.Result = store.WindowApplied, "", nil
		w.Claims += saved
		w.Rejected += rejected + dropped + (len(claims) - saved)
		w.CostUSD += spent
		w.UpdatedAt = now.Format(time.RFC3339)
		return pbSettleTx(tx, runID, w, spent, calls, now)
	})
}

// pbSettleTx saves a window and adds its spend, in the caller's transaction.
func pbSettleTx(tx *sql.Tx, runID string, w store.BackfillWindow, spent float64, calls int, now time.Time) error {
	if err := store.SaveBackfillWindow(tx, w); err != nil {
		return err
	}
	if spent == 0 && calls == 0 {
		return nil
	}
	if err := store.AddBackfillRunSpend(tx, runID, spent, now.Format(time.RFC3339)); err != nil {
		return err
	}
	return store.AddBackfillSpend(tx, now.Format("2006-01-02"), spent, calls)
}

// pbSettleWindow moves a window to state with its spend, in one transaction (no claims).
func (s *Service) pbSettleWindow(ctx context.Context, db *storage.Store, runID string, w store.BackfillWindow, state string, spent float64, calls int) error {
	now := s.Clock.Now().UTC()
	w.State, w.UpdatedAt, w.CostUSD = state, now.Format(time.RFC3339), w.CostUSD+spent
	if state != store.WindowSubmitted {
		w.Batch = ""
	}
	return db.WithWriterContext(ctx, func(tx *sql.Tx) error { return pbSettleTx(tx, runID, w, spent, calls, now) })
}

// pbRequeue sends a window back to the queue, or fails it after pbMaxAttempts sends.
func (s *Service) pbRequeue(ctx context.Context, db *storage.Store, runID string, w store.BackfillWindow, spent float64, calls int) error {
	state := store.WindowQueued
	if w.Attempts >= pbMaxAttempts {
		state = store.WindowFailed
	}
	return s.pbSettleWindow(ctx, db, runID, w, state, spent, calls)
}

// pbGuildSpent is the server's backfill total against its cap: what its runs spent, pilots included,
// plus the reserved maximum of every pilot send whose intent was stored and never settled (a crash cut it
// between the intent and the answer, so the host may have run and billed it). Such a send counts until it
// is charged (pbSettleIntentsTx): a new plan is never admitted against money that may already be billed.
func pbGuildSpent(q *sql.DB, guild string) (float64, error) {
	spent, err := store.BackfillGuildSpent(q, guild)
	if err != nil {
		return 0, err
	}
	ws, err := store.BackfillAnswered(q, guild)
	if err != nil {
		return 0, err
	}
	for _, w := range ws {
		for _, a := range pbAnswers(w.Result) {
			if a.Intent > 0 {
				spent += a.Intent
			}
		}
	}
	return spent, nil
}

// pbRunJSON is a run's status: counts only.
func pbRunJSON(db *storage.Store, run store.BackfillRun) map[string]any {
	counts, _ := store.CountBackfillWindows(db.Reader(), run.ID)
	states := map[string]int{}
	for _, st := range []string{store.WindowQueued, store.WindowSubmitted, store.WindowApplied, store.WindowFailed, store.WindowSkipped, store.WindowRefused} {
		states[st] = counts.States[st]
	}
	out := map[string]any{"id": run.ID, "guild_id": run.GuildID, "state": run.State, "model": run.Model, "provider": run.Provider,
		"budget_usd": run.BudgetUSD, "spent_usd": usd(run.SpentUSD), "stopped_by": run.StoppedBy, "pilot_done": run.PilotDone,
		"windows": states, "windows_total": run.Windows, "claims": counts.Claims, "rejected": counts.Rejected, "token": run.Token,
		"created_at": run.CreatedAt, "updated_at": run.UpdatedAt}
	var params pbParams
	if json.Unmarshal(run.Params, &params) == nil {
		out["since"], out["until"], out["rooms"] = params.Since, params.Until, len(params.Rooms)
	}
	if len(run.Pilot) > 0 {
		var rep map[string]any
		if json.Unmarshal(run.Pilot, &rep) == nil {
			out["pilot"] = rep
		}
	}
	if spent, err := pbGuildSpent(db.Reader(), run.GuildID); err == nil {
		out["guild_spent_usd"] = usd(spent) // every run of the server, against the cap
	}
	var pending int // claims stored pending (a no_sensitive server): not read until classified
	if db.Reader().QueryRow(`SELECT COUNT(*) FROM person_claim WHERE guild_id=? AND source=? AND state=?`, run.GuildID, "backfill:"+run.ID,
		store.PersonPending).Scan(&pending) == nil {
		out["claims_pending"] = pending
	}
	inflight := 0
	for _, b := range pbDecodeBatches(run.Batches) {
		if b.inFlight() {
			inflight++
		}
	}
	out["batches_in_flight"] = inflight
	return out
}

// pbStatus is mode=status: the server's latest run, or the one run_id names.
func (s *Service) pbStatus(req action.Request, db *storage.Store, in map[string]any) action.Result {
	run, ok, err := s.pbFindRun(db, in)
	if err != nil {
		return s.failErr(req, errcode.IO, "error.storage", err, nil)
	}
	out := map[string]any{"mode": "status"}
	if ok {
		out["run"] = pbRunJSON(db, run)
		out["running"] = s.pbActive(run.ID)
	}
	return action.OK(req, out)
}

// pbFindRun is the run run_id names, else the latest run of the request's server.
func (s *Service) pbFindRun(db *storage.Store, in map[string]any) (store.BackfillRun, bool, error) {
	if id := strVal(in, "run_id", ""); id != "" {
		return store.LoadBackfillRun(db.Reader(), id)
	}
	guild, err := pbGuild(db, in)
	if err != nil || guild == "" {
		return store.BackfillRun{}, false, err
	}
	runs, err := store.BackfillRuns(db.Reader(), guild)
	if err != nil || len(runs) == 0 {
		return store.BackfillRun{}, false, err
	}
	return runs[0], true, nil
}
