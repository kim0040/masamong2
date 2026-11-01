package discord

import (
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/application"
)

// staleAckAfter is how long a connected gateway may go without a heartbeat ACK before it needs a look.
// Discord's heartbeat interval is about 41 s and DiscordGo does not expose it, so this is the "about
// three intervals" rule as a fixed two minutes. DiscordGo itself only reconnects after five missed
// intervals, so a state that stays "check" past that means its heartbeat loop is not running.
const staleAckAfter = 2 * time.Minute

// processStart anchors the monotonic event clock: a time.Since reading never jumps with the wall clock.
var processStart = time.Now()

func monoNow() int64 { return int64(time.Since(processStart)) + 1 } // never 0, so 0 can mean "none yet"

// gatewayHealth is the cheap state behind the health snapshot: atomics only, so the event hook adds
// no lock to the hot path.
type gatewayHealth struct {
	lastEvent   atomic.Int64 // monoNow() of the last dispatch event of any type; 0 = none yet
	ready       atomic.Int64
	resumed     atomic.Int64
	disconnects atomic.Int64
}

// onGatewayEvent sees every dispatched event (the SDK's raw-event handler). It only stamps the clock and
// counts session starts; it never looks at the payload.
func (g *Gateway) onGatewayEvent(_ *discordgo.Session, e *discordgo.Event) {
	g.health.lastEvent.Store(monoNow())
	if e == nil {
		return
	}
	switch e.Type {
	case "READY":
		g.health.ready.Add(1)
	case "RESUMED":
		g.health.resumed.Add(1)
	}
}

// heartbeatSample is what the SDK knows about the heartbeat. ok=false means it could not be read (no
// session, or the session is busy opening a connection).
type heartbeatSample struct {
	ack     time.Time // last heartbeat ACK; zero before the first
	latency time.Duration
	ok      bool
}

// readHeartbeat copies the public heartbeat fields without ever blocking: DiscordGo holds the session
// lock for the whole of Open, so while it (re)connects the lock is taken by someone else and this
// returns ok=false instead of stalling the console.
func readHeartbeat(s *discordgo.Session) heartbeatSample {
	if s == nil || !s.TryRLock() {
		return heartbeatSample{}
	}
	defer s.RUnlock()
	// LastHeartbeatSent is written by the SDK's heartbeat loop under its websocket mutex, which is not
	// reachable from here; the value is a plain timestamp and only feeds the latency figure.
	sample := heartbeatSample{ack: s.LastHeartbeatAck, ok: true}
	if sent := s.LastHeartbeatSent; !sent.IsZero() && !s.LastHeartbeatAck.IsZero() {
		sample.latency = s.LastHeartbeatAck.Sub(sent) // negative while a heartbeat is still waiting for its ACK
	}
	return sample
}

// classifyGateway turns the inputs into a state. The heartbeat ACK is the only signal that can say
// "check": event silence is just a quiet server and is reported by the caller as a number.
func classifyGateway(connected bool, hb heartbeatSample, now time.Time) string {
	if !connected || !hb.ok || hb.ack.IsZero() {
		return application.GatewayUnknown
	}
	if now.Sub(hb.ack) > staleAckAfter {
		return application.GatewayCheck
	}
	return application.GatewayOK
}

// GatewayHealth implements application.DiscordHealthReporter. It is read-only: it reads local state and
// the SDK's public fields, makes no network call, and changes nothing.
func (g *Gateway) GatewayHealth() application.GatewayHealth {
	return g.healthAt(readHeartbeat(g.sess), time.Now().UTC(), monoNow())
}

func (g *Gateway) healthAt(hb heartbeatSample, now time.Time, mono int64) application.GatewayHealth {
	connected := g.connected.Load()
	h := application.GatewayHealth{
		State:       classifyGateway(connected, hb, now),
		Connected:   connected,
		LatencyMs:   -1,
		AckAgeSec:   -1,
		EventAgeSec: -1,
		Ready:       g.health.ready.Load(),
		Resumed:     g.health.resumed.Load(),
		Disconnects: g.health.disconnects.Load(),
	}
	if hb.ok && !hb.ack.IsZero() {
		h.AckAgeSec = int64(max(now.Sub(hb.ack), 0) / time.Second)
	}
	if hb.ok && hb.latency > 0 {
		h.LatencyMs = hb.latency.Milliseconds()
	}
	if last := g.health.lastEvent.Load(); last > 0 {
		h.EventAgeSec = int64(max(time.Duration(mono-last), 0) / time.Second)
	}
	return h
}
