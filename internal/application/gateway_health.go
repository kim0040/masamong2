package application

// Gateway health states, as reported to the console and doctor.
const (
	GatewayOK      = "ok"      // connected and the heartbeat is acknowledged on time
	GatewayCheck   = "check"   // connected, but Discord has not acknowledged a heartbeat for too long
	GatewayUnknown = "unknown" // not connected yet, reconnecting, or the SDK gives no data
)

// GatewayHealth is a read-only snapshot of the Discord gateway connection. Connected() alone cannot
// tell a healthy connection from a half-open socket, so this adds what the SDK knows about the
// heartbeat. Ages and latency are -1 when unknown. EventAgeSec is the time since the last event of
// any kind; a quiet server is normal, so it is shown as a hint and never decides State.
type GatewayHealth struct {
	State       string `json:"state"`
	Connected   bool   `json:"connected"`
	LatencyMs   int64  `json:"latency_ms"`
	AckAgeSec   int64  `json:"ack_age_sec"`
	EventAgeSec int64  `json:"event_age_sec"`
	Ready       int64  `json:"ready_count"`
	Resumed     int64  `json:"resumed_count"`
	Disconnects int64  `json:"disconnect_count"`
}

// DiscordHealthReporter is optional so existing and test DiscordInfo implementations stay small.
type DiscordHealthReporter interface {
	GatewayHealth() GatewayHealth
}

// gatewayHealth reads the snapshot from a gateway that reports one.
func (s *Service) gatewayHealth() (GatewayHealth, bool) {
	if s.Discord == nil {
		return GatewayHealth{}, false
	}
	r, ok := s.Discord.(DiscordHealthReporter)
	if !ok {
		return GatewayHealth{}, false
	}
	return r.GatewayHealth(), true
}

// DiscordReconciliationReporter is optional for the same reason. ReconciliationBlocked is how many
// quarantined message-change files wait for an operator (docs/memory-operations.md §17); while it is
// above zero the gateway holds back edits, so a person has to see it somewhere other than the log.
type DiscordReconciliationReporter interface {
	ReconciliationBlocked() int
}

// reconciliationBlocked is the number of recovery files waiting for an operator, 0 when the gateway
// reports none or cannot report.
func (s *Service) reconciliationBlocked() int {
	if s.Discord == nil {
		return 0
	}
	r, ok := s.Discord.(DiscordReconciliationReporter)
	if !ok {
		return 0
	}
	return max(r.ReconciliationBlocked(), 0)
}

// reconciliationAttention is the notification item for the same state as the doctor line. It carries
// only a count: the files may hold private text, so no path, id or content leaves the process.
func (s *Service) reconciliationAttention() []attention {
	n := s.reconciliationBlocked()
	if n == 0 {
		return nil
	}
	return []attention{{"warn", "discord_reconciliation", map[string]any{"n": n}, "/logs?tab=doctor"}}
}
