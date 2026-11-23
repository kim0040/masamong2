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
	"slices"
	"sort"
	"strings"
	"time"

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
	pbMaxBudget     = 100.0
	pbPageLines     = 2000 // observations read per page
	pbChunkMax      = 4 * pbPageLines
	pbMaxBatch      = 1000 // windows per Message Batches job (contract §5)
	pbMaxTokens     = 1400 // output limit of one window (the room-memory extraction's)
	pbMaxAttempts   = 3    // sends of one window before it is failed
	pbPilotWorkers  = 4
	pbHearsaySample = 30
	// pbHardGap is the segmenter's default hard cut (episodes.Options.HardGap): a page is cut only where
	// Split itself would start a new block, so paging never changes a segment.
	pbHardGap = 45 * time.Minute
)

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
	Skipped bool
}

// pbPlan is a plan built from the stored observations.
type pbPlan struct {
	Params      pbParams
	Windows     []pbWindow
	Rooms       int // rooms with at least one window
	Lines       int // observations read
	MergedLines int
	Skipped     int
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
	p := pbParams{Guild: guild, Rooms: rooms, BudgetUSD: parseUSD(strVal(in, "budget_usd", ""), pbDefaultBudget)}
	if p.BudgetUSD <= 0 || p.BudgetUSD > pbMaxBudget {
		return pbParams{}, map[string]any{"field": "budget_usd", "max": pbMaxBudget}, nil
	}
	p.BudgetUSD = math.Round(p.BudgetUSD*100) / 100
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
		r := pbRoomOf(room)
		pack, _ := s.roomVoice(r)
		opt := peoplebackfill.Options{PromptHash: pbPromptVersion(pack),
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
	plan.Token = pbToken(p, plan.Windows)
	plan.Params.Token = plan.Token
	return plan, nil
}

// pbToken is the plan's fingerprint: its parameters and its windows. Any change of either (a deleted
// message at a window's edge, a captured old message, another range or cap) gives another token.
func pbToken(p pbParams, ws []pbWindow) string {
	h := sha256.New()
	fmt.Fprintf(h, "people-backfill/1\x00%s\x00%s\x00%s\x00%s\x00%.2f\n", p.Guild, strings.Join(p.Rooms, ","), p.Since, p.Until, p.BudgetUSD)
	for _, w := range ws {
		fmt.Fprintf(h, "%s %t\n", w.ID, w.Skipped)
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

// pbDryRunOutput is what a dry run reports: counts and money, no text.
func (s *Service) pbDryRunOutput(plan pbPlan) map[string]any {
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
		within[name] = pilotHigh+runHigh <= plan.Params.BudgetUSD
	}
	provider, ready := "", false
	if id, _, err := s.pbAdapter(s.Doc(), ""); err == nil {
		provider, ready = id, true
	}
	return map[string]any{
		"mode": "dry_run", "guild_id": plan.Params.Guild, "rooms": plan.Rooms, "rooms_asked": len(plan.Params.Rooms),
		"since": plan.Params.Since, "until": plan.Params.Until, "until_from": plan.Params.UntilFrom,
		"windows": len(plan.Windows), "sent_windows": len(sendable), "skipped_windows": plan.Skipped, "pilot_windows": len(pilot),
		"lines": plan.Lines, "merged_lines": plan.MergedLines,
		"tokens":     map[string]int{"input": tin, "output": tout},
		"cost_usd":   cost,
		"budget_usd": plan.Params.BudgetUSD, "within_budget": within,
		"provider": provider, "provider_ready": ready,
		"token": plan.Token, "model_calls": 0,
	}
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
	return action.OK(req, s.pbDryRunOutput(plan))
}

// pbConfirmedPlan is the plan a pilot or run confirms: the server's stored plan whose token is confirm,
// built again from the observations. A token that is not the stored one, or a plan that no longer gives
// it, is refused with the current plan's dry-run output (and that plan is stored).
func (s *Service) pbConfirmedPlan(ctx context.Context, req action.Request, db *storage.Store, in map[string]any) (pbPlan, *action.Result) {
	confirm := strVal(in, "confirm", "")
	guild, err := pbGuild(db, in)
	if err != nil {
		res := s.failErr(req, errcode.IO, "error.storage", err, nil)
		return pbPlan{}, &res
	}
	var stored pbParams
	ok, err := s.kvGet(pbPlanKey+guild, &stored)
	if err != nil {
		res := s.failErr(req, errcode.IO, "error.storage", err, nil)
		return pbPlan{}, &res
	}
	if guild == "" || !ok || confirm == "" {
		res := action.Fail(req, errcode.InvalidInput, "people_backfill.confirm_needed", s.msg(req, "people_backfill.confirm_needed"), map[string]any{"field": "confirm"})
		return pbPlan{}, &res
	}
	params := stored
	params.Token, params.CreatedAt = "", ""
	plan, err := s.pbBuildPlan(ctx, db, params)
	if err != nil {
		res := s.failErr(req, errcode.IO, "error.storage", err, nil)
		return pbPlan{}, &res
	}
	if plan.Token != confirm || stored.Token != confirm {
		plan.Params.CreatedAt = s.Clock.Now().UTC().Format(time.RFC3339)
		if err := s.kvSet(pbPlanKey+guild, plan.Params); err != nil {
			res := s.failErr(req, errcode.IO, "error.storage", err, nil)
			return pbPlan{}, &res
		}
		res := action.Fail(req, errcode.StaleRevision, "people_backfill.plan_changed", s.msg(req, "people_backfill.plan_changed"),
			map[string]any{"dry_run": s.pbDryRunOutput(plan)})
		return pbPlan{}, &res
	}
	plan.Params.CreatedAt = stored.CreatedAt
	return plan, nil
}

// ---------------------------------------------------------------------------
// the provider
// ---------------------------------------------------------------------------

// pbAdapter is the backfill's provider: a configured provider whose host family is native Anthropic,
// with a key. want names one; without it there must be exactly one. Never OpenRouter or another family.
func (s *Service) pbAdapter(doc config.Document, want string) (string, *openai.Adapter, error) {
	if s.practice {
		return "", nil, errPBNoAnthropic
	}
	var found []string
	for id, p := range doc.Providers {
		if p.Kind != config.KindOpenAI || p.Disabled || openai.ResolveAPI(p.API, p.BaseURL) != openai.APIAnthropic {
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
	a := newLLMAdapter(client, p, cred)
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

// pbRequest is one window's request: no temperature, top-level ephemeral cache_control.
func pbRequest(model peoplebackfill.Model, bp pbPrompt) openai.ChatRequest {
	return openai.ChatRequest{Model: string(model), MaxTokens: pbMaxTokens, EphemeralCache: true,
		Messages: []openai.Message{{Role: "system", Content: bp.system}, {Role: "user", Content: bp.user}}}
}

// pbSettle prices a result's reported usage: input not read from the cache at the input price (cache
// writes at 1.25 of it), cache reads at a tenth, output at the output price.
func pbSettle(res *openai.ChatResult, model peoplebackfill.Model, mode peoplebackfill.Mode) float64 {
	if res == nil {
		return 0
	}
	in := res.PromptTokens - res.CachedTokens + (res.CacheWriteTokens+3)/4
	v, err := peoplebackfill.Settle(peoplebackfill.Usage{InputTokens: max(0, in), OutputTokens: res.CompletionTokens, CacheReadTokens: res.CachedTokens}, model, mode)
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
	system, user string
	hash         string            // of system and user: an answer applies only to the same prompt
	text         map[string]string // human line id → the text the model read
	author       map[string]string // human line id → its speaker reference
	labels       map[string]string // person reference → the label the prompt showed
	readAt       string
	ok           bool // a human line of at least the meaningful-rune floor is left
}

// pbBuildPrompt reads a window's lines as they are now and builds its prompt. Deleted lines drop out.
func (s *Service) pbBuildPrompt(db *storage.Store, guild string, w store.BackfillWindow) (pbPrompt, error) {
	bp := pbPrompt{text: map[string]string{}, author: map[string]string{}, labels: map[string]string{}}
	bp.readAt = s.Clock.Now().UTC().Format(time.RFC3339Nano)
	lines, err := s.linesByID(db, w.Obs)
	if err != nil {
		return bp, err
	}
	r := pbRoomOf(w.Room)
	pack, botName := s.roomVoice(r)
	var msgs []episodes.Message
	botAccount := ""
	for _, l := range lines {
		m := l.Message
		msgs = append(msgs, m)
		if m.Bot {
			if botAccount == "" && m.Account != "" {
				botAccount = m.Account
			}
			continue
		}
		ref := personRefOf(r, m)
		bp.text[m.ID], bp.author[m.ID] = m.Text, ref
		bp.labels[ref] = m.Author
		if search.MeaningfulRunes(m.Text) >= search.MinMessageRunes {
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
		RefOf: func(m episodes.Message) string { return personRefOf(r, m) }, Mentioned: mentioned, BotRef: botRef})
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

// pbMentioned are the people a window names without writing in it: accounts of <@id> mentions (with
// their latest label in the server) and exact single-account alias matches (link name), the same
// signals a chat turn resolves (readPersonNames and turnNames.match, used read-only here).
// TODO(person memory): live extraction builds the same list; once it has a helper, both should share it.
func (s *Service) pbMentioned(db *storage.Store, guild string, msgs []episodes.Message, botRef string) ([]episodes.Person, error) {
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

// pbClaims turns a window's validated facts into claims for the write gate: one claim per speaker, with
// that speaker's own evidence lines only (the gate refuses evidence of another author), the text each
// line had when the model read it, and the link (self only when the speaker is the subject).
func pbClaims(guild, runID string, facts []episodes.PersonFact, bp pbPrompt) (claims []store.PersonClaim, dropped int) {
	for _, f := range facts {
		for _, speaker := range f.By {
			var ev []string
			lines := map[string]string{}
			for _, id := range f.Ev {
				if bp.author[id] == speaker {
					ev = append(ev, id)
					lines[id] = bp.text[id]
				}
			}
			if len(ev) == 0 {
				dropped++
				continue
			}
			link := f.Link
			switch {
			case speaker == f.About:
				link = store.LinkSelf
			case link == store.LinkSelf || link == "":
				link = store.LinkMention // a participant the speaker named by token
			}
			claims = append(claims, store.PersonClaim{GuildID: guild, SubjectRef: f.About, SpeakerRef: speaker, Kind: f.Kind, Topic: f.Topic,
				Text: f.Text, Evidence: ev, Source: "backfill:" + runID, Link: link, Lines: lines, ReadAt: bp.readAt})
		}
	}
	return claims, dropped
}

// pbApply writes one window's claims and settles it, in one transaction: the claims through the write
// gate, the window's state and counts, and the spend (run and memory_spend).
func (s *Service) pbApply(ctx context.Context, db *storage.Store, runID, guild string, w store.BackfillWindow, facts []episodes.PersonFact, rejected int,
	bp pbPrompt, spent float64, calls int) error {
	now := s.Clock.Now().UTC()
	return db.WithWriterContext(ctx, func(tx *sql.Tx) error {
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
