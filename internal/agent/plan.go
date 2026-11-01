// Package agent is the fake planner/DAG/executor (08). No paid providers.
// Invalid plans are rejected before any cost reservation.
package agent

import (
	"fmt"
	"strings"

	"masamong/internal/errcode"
)

const (
	MaxGraphNodes     = 12
	MaxExternalTries  = 6
	MaxPlannerRounds  = 2
	MaxDeepRAGRounds  = 2 // instruction §18/§37 — strategy change, not infinite top_k
	MaxSynthesis      = 1
	MaxConcurrent     = 2
	MaxProactiveSends = 1 // per conversation epoch without new human activity
	DeadlineSeconds   = 45
)

type Node struct {
	ID        string         `json:"id"`
	Need      string         `json:"need"`
	DependsOn []string       `json:"depends_on"`
	Ref       string         `json:"ref"`
	Cost      int            `json:"cost"`
	Status    string         `json:"status"`
	Output    map[string]any `json:"output,omitempty"`
}

type Plan struct {
	Nodes []Node `json:"nodes"`
}

type Execution struct {
	Result   string         `json:"result"` // answer|partial|silent|clarification|proposal|error
	Nodes    []Node         `json:"nodes"`
	Evidence []map[string]any `json:"evidence"`
	TraceID  string         `json:"trace_id"`
	Rejected bool           `json:"rejected,omitempty"`
	Reason   string         `json:"reason,omitempty"`
}

func Validate(p Plan) error {
	if len(p.Nodes) == 0 {
		return fmt.Errorf("%s: empty plan", errcode.PlanRejected)
	}
	if len(p.Nodes) > MaxGraphNodes {
		return fmt.Errorf("%s: graph too large", errcode.PlanRejected)
	}
	ids := map[string]struct{}{}
	for _, n := range p.Nodes {
		if n.ID == "" {
			return fmt.Errorf("%s: node id required", errcode.PlanRejected)
		}
		if _, ok := ids[n.ID]; ok {
			return fmt.Errorf("%s: duplicate node %s", errcode.PlanRejected, n.ID)
		}
		ids[n.ID] = struct{}{}
		if n.Ref != "" && !strings.HasPrefix(n.Ref, "/") && !strings.Contains(n.Ref, "#") {
			return fmt.Errorf("%s: bad ref %s", errcode.PlanRejected, n.Ref)
		}
	}
	for _, n := range p.Nodes {
		for _, d := range n.DependsOn {
			if _, ok := ids[d]; !ok {
				return fmt.Errorf("%s: missing dependency %s", errcode.PlanRejected, d)
			}
		}
	}
	if cyclic(p) {
		return fmt.Errorf("%s: cycle", errcode.PlanRejected)
	}
	return nil
}

func cyclic(p Plan) bool {
	adj := map[string][]string{}
	for _, n := range p.Nodes {
		adj[n.ID] = n.DependsOn
	}
	const unseen, open, done = 0, 1, 2
	state := map[string]int{}
	var visit func(string) bool
	visit = func(id string) bool {
		state[id] = open
		for _, d := range adj[id] {
			switch state[d] {
			case open:
				return true
			case unseen:
				if visit(d) {
					return true
				}
			}
		}
		state[id] = done
		return false
	}
	for _, n := range p.Nodes {
		if state[n.ID] == unseen && visit(n.ID) {
			return true
		}
	}
	return false
}

func Execute(p Plan, fixture string) (Execution, error) {
	if err := Validate(p); err != nil {
		return Execution{Result: "error", Rejected: true, Reason: err.Error()}, err
	}
	nodes := append([]Node(nil), p.Nodes...)
	switch fixture {
	case "timeout":
		for i := range nodes {
			nodes[i].Status = "unknown"
		}
		return Execution{Result: "error", Nodes: nodes, Reason: "timeout_unknown_not_retry", Evidence: nil}, nil
	case "401":
		return Execution{Result: "error", Nodes: degrade(nodes, "degraded"), Reason: "401"}, nil
	case "429":
		return Execution{Result: "error", Nodes: degrade(nodes, "retry_wait"), Reason: "429"}, nil
	case "fx_fail":
		for i := range nodes {
			if nodes[i].Need == "fx" {
				nodes[i].Status = "failed"
				nodes[i].Output = nil
			} else {
				nodes[i].Status = "ok"
				nodes[i].Output = map[string]any{"price": "100.00", "currency": "USD"}
			}
		}
		return Execution{
			Result: "partial",
			Nodes:  nodes,
			Evidence: []map[string]any{{"id": "ev_quote", "kind": "quote", "status": "ok"}},
			Reason: "quote_without_fx",
		}, nil
	default:
		for i := range nodes {
			nodes[i].Status = "ok"
			nodes[i].Output = map[string]any{"fixture": true}
		}
		return Execution{
			Result: "answer",
			Nodes:  nodes,
			Evidence: []map[string]any{{"id": "ev_fake", "kind": "tool", "status": "ok"}},
		}, nil
	}
}

func degrade(nodes []Node, status string) []Node {
	out := append([]Node(nil), nodes...)
	for i := range out {
		out[i].Status = status
	}
	return out
}
