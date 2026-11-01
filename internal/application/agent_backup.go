package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"masamong/internal/action"
	"masamong/internal/agent"
	"masamong/internal/errcode"
)

func (s *Service) agent(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	switch req.Action {
	case "tools.list":
		return s.toolsList(req)
	case "tools.test":
		return s.toolsTest(ctx, req)
	case "tools.configure":
		return s.toolsConfigure(req)
	case "agent.simulate":
		var p agent.Plan
		if raw, ok := in["plan"]; ok {
			b, _ := json.Marshal(raw)
			_ = json.Unmarshal(b, &p)
		} else if len(req.Input) > 0 {
			_ = json.Unmarshal(req.Input, &p)
		}
		ex, err := agent.Execute(p, strVal(in, "fixture", "ok"))
		ex.TraceID = newID("tr_")
		body, _ := json.Marshal(ex)
		var v any
		_ = json.Unmarshal(body, &v)
		if db, dbErr := s.adminDB(); dbErr == nil {
			if writeErr := db.WithWriterContext(ctx, func(tx *sql.Tx) error {
				keys, err := copySources(tx, v)
				if err != nil {
					return err
				}
				if err := indexCopy(tx, "trace", ex.TraceID, keys, true); err != nil {
					return err
				}
				_, err = tx.Exec(`INSERT INTO traces(id,body,created_at,evidence_state) VALUES(?,?,?,1)`, ex.TraceID, string(body), s.Clock.Now().UTC().Format(time.RFC3339))
				return err
			}); writeErr != nil {
				return s.evidenceWriteError(req, writeErr)
			}
		}
		if err != nil {
			return action.Fail(req, errcode.PlanRejected, "generic.error", err.Error(), map[string]any{"rejected": true, "before_cost": true, "trace_id": ex.TraceID})
		}
		return action.OK(req, map[string]any{"result": ex.Result, "nodes": ex.Nodes, "evidence": ex.Evidence, "trace_id": ex.TraceID, "reason": ex.Reason, "live": false, "source": "fixture"})
	case "trace.show":
		id := strVal(in, "id", req.ResourceID)
		var body string
		if err := s.Store.DB.QueryRowContext(ctx, `SELECT body FROM traces WHERE id=?`, id).Scan(&body); err != nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "trace not found", nil)
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(body), &m)
		return action.OK(req, m)
	}
	return action.Fail(req, errcode.ActionUnknown, "action.unknown", s.msg(req, "action.unknown"), nil)
}
