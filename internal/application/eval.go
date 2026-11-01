package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"masamong/internal/action"
	"masamong/internal/crash"
	"masamong/internal/errcode"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/retrieval"
	"masamong/internal/memory/search"
)

// eval.run scores retrieval against the lab's review set. Each judged question is searched again with
// one or more ranking modes (search only: no answer model). A "good" verdict saved the evidence it was
// judged on; the score is how much of that evidence comes back (retention). A "bad" verdict counts as
// fixed when the top result changed. Runs are stored so the lab can compare them over time.

var evalSQLite = `CREATE TABLE IF NOT EXISTS eval_runs (
	id TEXT PRIMARY KEY,
	at TEXT NOT NULL,
	params TEXT NOT NULL,
	summary TEXT NOT NULL,
	items TEXT NOT NULL
)`

var evalTiDB = `CREATE TABLE IF NOT EXISTS eval_runs (
	id VARCHAR(64) NOT NULL PRIMARY KEY,
	at VARCHAR(64) NOT NULL,
	params MEDIUMTEXT NOT NULL,
	summary MEDIUMTEXT NOT NULL,
	items MEDIUMTEXT NOT NULL
)`

type evalCase struct {
	ID       string
	Question string
	Verdict  string
	Rooms    []string
	Docs     []string
}

type evalItem struct {
	ReviewID string              `json:"review_id"`
	Question string              `json:"question"`
	Verdict  string              `json:"verdict"`
	Modes    map[string]evalMode `json:"modes"`
}

type evalMode struct {
	Retention float64  `json:"retention"` // good: share of the judged evidence found again
	Changed   bool     `json:"changed"`   // bad: top result differs from the judged one
	Hits      int      `json:"hits"`
	Top       []string `json:"top"`
	Error     string   `json:"error,omitempty"`
	LatencyMS int64    `json:"latency_ms"`
}

func (s *Service) evalCases(limit int) []evalCase {
	db, err := s.adminDB()
	if err != nil {
		return nil
	}
	rows, err := db.DB.Query(`SELECT id, question, verdict, channel_key, meta FROM lab_reviews WHERE verdict IN ('good','bad') ORDER BY at DESC LIMIT ?`, limit)
	if err != nil {
		return nil
	}
	type candidate struct {
		caseData   evalCase
		channelKey string
	}
	var candidates []candidate
	for rows.Next() {
		var c evalCase
		var ck, meta string
		if rows.Scan(&c.ID, &c.Question, &c.Verdict, &ck, &meta) != nil {
			continue
		}
		var m struct {
			DocIDs []string `json:"doc_ids"`
			Rooms  []string `json:"rooms"`
		}
		_ = json.Unmarshal([]byte(meta), &m)
		c.Docs, c.Rooms = m.DocIDs, m.Rooms
		candidates = append(candidates, candidate{caseData: c, channelKey: ck})
	}
	_ = rows.Close()

	// SQLite uses one connection. Resolve attached channel blocks only after the review query is
	// closed because channelRooms may read the memory tables through that same connection.
	var out []evalCase
	seen := map[string]bool{}
	for _, raw := range candidates {
		c := raw.caseData
		if len(c.Rooms) == 0 && raw.channelKey != "" {
			if g, ch, ok := strings.Cut(raw.channelKey, ":"); ok {
				c.Rooms = s.channelRooms(g, ch)
			}
		}
		key := c.Question + "|" + strings.Join(c.Rooms, ",")
		if len(c.Rooms) == 0 || len(c.Docs) == 0 || seen[key] {
			continue // judged before evidence ids were saved, or a duplicate question
		}
		seen[key] = true
		out = append(out, c)
	}
	return out
}

func (s *Service) evalAction(ctx context.Context, req action.Request) action.Result {
	db, err := s.adminDB()
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	in := inputMap(req)
	if req.Action == "eval.runs" {
		rows, err := db.DB.Query(`SELECT id, at, params, summary FROM eval_runs ORDER BY at DESC LIMIT 50`)
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		defer rows.Close()
		list := []map[string]any{}
		for rows.Next() {
			var id, at, params, summary string
			if rows.Scan(&id, &at, &params, &summary) != nil {
				continue
			}
			var p, sm any
			_ = json.Unmarshal([]byte(params), &p)
			_ = json.Unmarshal([]byte(summary), &sm)
			list = append(list, map[string]any{"id": id, "at": at, "params": p, "summary": sm})
		}
		out := map[string]any{"runs": list}
		if id := strVal(in, "id", ""); id != "" {
			var items string
			if db.DB.QueryRow(`SELECT items FROM eval_runs WHERE id=?`, id).Scan(&items) == nil {
				var it any
				_ = json.Unmarshal([]byte(items), &it)
				out["items"] = it
			}
		}
		return action.OK(req, out)
	}
	modes := strSlice(in, "modes")
	if len(modes) == 0 {
		modes = []string{string(s.retrieveMode())}
	}
	for i, m := range modes {
		rm, ok := retrieval.Normalize(m)
		if !ok {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "unknown ranking mode "+m, nil)
		}
		modes[i] = string(rm)
	}
	topK := min(max(intVal(in, "top_k", 5), 1), 20)
	cases := s.evalCases(min(max(intVal(in, "limit", 100), 1), 300))
	rerank := false
	for _, m := range modes {
		rerank = rerank || m == string(retrieval.Rerank) || m == string(retrieval.Auto)
	}
	estimate := 0.0
	if rerank {
		var usd float64
		var calls int
		_ = db.DB.QueryRow(`SELECT COALESCE(SUM(usd),0), COALESCE(SUM(calls),0) FROM memory_spend WHERE kind='rerank'`).Scan(&usd, &calls)
		if calls > 0 {
			estimate = usd / float64(calls) * float64(len(cases))
		}
	}
	if boolVal(in, "dry_run") {
		return action.OK(req, map[string]any{"dry_run": true, "cases": len(cases), "modes": modes, "estimate_usd": estimate, "may_cost": rerank})
	}
	if len(cases) == 0 {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "no judged questions with saved evidence yet: judge answers in the lab first", nil)
	}
	eng, err := s.MemoryEngine()
	if err != nil {
		return action.Fail(req, errcode.NotSetup, "generic.error", err.Error(), nil)
	}
	if eng.Index() == nil {
		if _, err := eng.Reload(); err != nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", err.Error(), nil)
		}
	}
	started := s.Clock.Now()
	items := make([]evalItem, len(cases))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, c := range cases {
		wg.Add(1)
		go func(i int, c evalCase) {
			defer wg.Done()
			defer crash.Recover(s.Log, "memory eval case")
			sem <- struct{}{}
			defer func() { <-sem }()
			item := evalItem{ReviewID: c.ID, Question: clip(c.Question, 200), Verdict: c.Verdict, Modes: map[string]evalMode{}}
			for _, m := range modes {
				t0 := time.Now()
				rm, _ := retrieval.Normalize(m)
				res, _, err := s.reviewedSearch(ctx, eng, engine.SearchRequest{Query: c.Question, Intent: search.Intent{Kind: "other", Source: "none"},
					Rooms: c.Rooms, Ranking: rm, TopK: topK, RemainingUSD: parseUSD(s.Doc().Memory.TurnBudgetUSD, 0.05)})
				em := evalMode{LatencyMS: time.Since(t0).Milliseconds(), Top: []string{}}
				if err != nil {
					em.Error = err.Error()
				} else {
					got := map[string]bool{}
					for _, h := range res.Hits {
						got[h.DocID] = true
						for _, mid := range h.MergedLeafIDs {
							got[mid] = true
						}
						em.Top = append(em.Top, h.DocID)
					}
					em.Hits = len(res.Hits)
					found := 0
					for _, d := range c.Docs {
						if got[d] {
							found++
						}
					}
					em.Retention = float64(found) / float64(len(c.Docs))
					em.Changed = len(em.Top) == 0 || em.Top[0] != c.Docs[0]
				}
				item.Modes[m] = em
			}
			items[i] = item
		}(i, c)
	}
	wg.Wait()
	summary := map[string]any{}
	for _, m := range modes {
		var good, bad, fixed, errs, empty int
		var ret float64
		var lat int64
		for _, it := range items {
			em := it.Modes[m]
			lat += em.LatencyMS
			if em.Error != "" {
				errs++
				continue
			}
			if em.Hits == 0 {
				empty++
			}
			if it.Verdict == "good" {
				good++
				ret += em.Retention
			} else {
				bad++
				if em.Changed {
					fixed++
				}
			}
		}
		sm := map[string]any{"good": good, "bad": bad, "fixed": fixed, "errors": errs, "empty": empty, "avg_latency_ms": lat / int64(max(1, len(items)))}
		if good > 0 {
			sm["retention"] = ret / float64(good)
		}
		if bad > 0 {
			sm["fixed_rate"] = float64(fixed) / float64(bad)
		}
		summary[m] = sm
	}
	params := map[string]any{"modes": modes, "top_k": topK, "cases": len(cases), "embed_space": eng.EmbedSpace(), "recipe": eng.Settings.Recipe.Version(),
		"revision": s.Revision(), "duration_ms": s.Clock.Now().Sub(started).Milliseconds()}
	id := newID("eval_")
	pj, _ := json.Marshal(params)
	sj, _ := json.Marshal(summary)
	ij, _ := json.Marshal(items)
	var values any
	_ = json.Unmarshal(ij, &values)
	err = db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		keys, err := copySources(tx, values)
		if err != nil {
			return err
		}
		if err := indexCopy(tx, holderEval, id, keys, true); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO eval_runs(id,at,params,summary,items,evidence_state) VALUES(?,?,?,?,?,1)`, id, s.nowString(), string(pj), string(sj), string(ij))
		return err
	})
	if err != nil {
		return s.evidenceWriteError(req, err)
	}
	s.emit("eval", id, nil)
	return action.OK(req, map[string]any{"id": id, "params": params, "summary": summary, "items": items})
}
