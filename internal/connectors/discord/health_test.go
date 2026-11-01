package discord

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/application"
)

func TestClassifyGateway(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	ack := func(age time.Duration) heartbeatSample { return heartbeatSample{ack: now.Add(-age), ok: true} }
	cases := []struct {
		name      string
		connected bool
		hb        heartbeatSample
		want      string
	}{
		{"fresh ack", true, ack(20 * time.Second), application.GatewayOK},
		{"ack just inside the limit", true, ack(staleAckAfter), application.GatewayOK},
		{"stale ack while connected", true, ack(staleAckAfter + time.Second), application.GatewayCheck},
		{"very stale ack", true, ack(time.Hour), application.GatewayCheck},
		{"not connected, stale ack", false, ack(time.Hour), application.GatewayUnknown},
		{"before the first ready", false, heartbeatSample{ok: true}, application.GatewayUnknown},
		{"connected but the SDK gave nothing", true, heartbeatSample{}, application.GatewayUnknown},
		{"connected, no ack recorded yet", true, heartbeatSample{ok: true}, application.GatewayUnknown},
	}
	for _, c := range cases {
		if got := classifyGateway(c.connected, c.hb, now); got != c.want {
			t.Errorf("%s: state %q, want %q", c.name, got, c.want)
		}
	}
}

// A quiet server: the last event is hours old but heartbeats are acknowledged, so the gateway is fine.
// Event silence is reported as a number and never changes the state.
func TestQuietServerStaysOK(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	g := &Gateway{}
	g.connected.Store(true)
	g.health.lastEvent.Store(1) // the very first stamp, long before "now"
	h := g.healthAt(heartbeatSample{ack: now.Add(-15 * time.Second), latency: 80 * time.Millisecond, ok: true}, now, int64(6*time.Hour)+1)
	if h.State != application.GatewayOK || !h.Connected {
		t.Fatalf("a quiet server with fresh ACKs must stay ok: %+v", h)
	}
	if h.EventAgeSec != int64(6*time.Hour/time.Second) || h.AckAgeSec != 15 || h.LatencyMs != 80 {
		t.Fatalf("figures wrong: %+v", h)
	}
}

func TestHealthUnknownFigures(t *testing.T) {
	g := &Gateway{} // no session, nothing received
	h := g.GatewayHealth()
	if h.State != application.GatewayUnknown || h.Connected || h.LatencyMs != -1 || h.AckAgeSec != -1 || h.EventAgeSec != -1 {
		t.Fatalf("a gateway that never connected must report unknown with no figures: %+v", h)
	}
	// a heartbeat still waiting for its ACK gives a negative latency, which is not reported
	now := time.Now().UTC()
	h = g.healthAt(heartbeatSample{ack: now, latency: -time.Second, ok: true}, now, monoNow())
	if h.LatencyMs != -1 {
		t.Fatalf("a negative latency must be reported as unknown: %+v", h)
	}
}

func TestReadHeartbeatNilAndBusySession(t *testing.T) {
	if hb := readHeartbeat(nil); hb.ok {
		t.Fatalf("a nil session has no data: %+v", hb)
	}
	g := &Gateway{sess: nil}
	if h := g.GatewayHealth(); h.State != application.GatewayUnknown {
		t.Fatalf("nil session: %+v", h)
	}
	s := &discordgo.Session{}
	sent := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	s.LastHeartbeatSent = sent
	s.LastHeartbeatAck = sent.Add(55 * time.Millisecond)
	hb := readHeartbeat(s)
	if !hb.ok || !hb.ack.Equal(s.LastHeartbeatAck) || hb.latency != 55*time.Millisecond {
		t.Fatalf("fields not copied: %+v", hb)
	}
	// DiscordGo holds the session lock for the whole of Open: the read must give up, not wait
	s.Lock()
	done := make(chan heartbeatSample, 1)
	go func() { done <- readHeartbeat(s) }()
	select {
	case hb := <-done:
		if hb.ok {
			t.Fatalf("a session busy opening must read as no data: %+v", hb)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readHeartbeat blocked on a locked session")
	}
	s.Unlock()
}

func TestGatewayEventHookCountsSessions(t *testing.T) {
	g := &Gateway{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if g.health.lastEvent.Load() != 0 {
		t.Fatal("no event yet")
	}
	g.onGatewayEvent(nil, &discordgo.Event{Type: "READY"})
	g.onGatewayEvent(nil, &discordgo.Event{Type: "MESSAGE_CREATE"})
	g.onGatewayEvent(nil, &discordgo.Event{Type: "RESUMED"})
	g.onGatewayEvent(nil, nil)
	g.onDisconnect(nil, &discordgo.Disconnect{})
	h := g.GatewayHealth()
	if h.Ready != 1 || h.Resumed != 1 || h.Disconnects != 1 || h.EventAgeSec < 0 {
		t.Fatalf("counters: %+v", h)
	}
}
