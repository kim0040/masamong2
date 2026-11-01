package application

import (
	"sync"
	"time"

	"masamong/internal/errclass"
)

// Reply delivery: what happened when a reply was posted to Discord. The connector reports one
// DeliveryReport per reply (how many parts, how many reached the channel, how many extra sends it
// took, why it stopped), and the service keeps a row per delivery next to the turn it belongs to, so a
// reply that was split, retried or lost can be told apart from one that went out cleanly. A reply the
// bot is not allowed to post also reaches the operators once per channel as an alert.
//
// Stored: the turn id, server and channel IDs, counts, the failure class (a short machine value) and
// whether the reply reference was dropped. No message text and no Discord user id. Rows go after
// deliveryKeepDays.

// DeliveryReport is the outcome of posting one reply. It carries no message bodies.
type DeliveryReport struct {
	Parts            int            // parts the reply was split into
	Sent             int            // parts that reached the channel
	Retries          int            // sends beyond the first attempt of each part (backoff retries and the no-reference retry)
	Class            errclass.Class // why delivery stopped; errclass.None when every part was sent
	ReferenceDropped bool           // the message replied to was gone, so the reply went out without the reference
	MessageIDs       []string       // Discord IDs of the parts that were sent
}

const (
	deliveryKeepDays   = 90
	deliveryAlertEvery = 6 * time.Hour
)

var deliverySQLite = []string{
	`CREATE TABLE IF NOT EXISTS delivery_ledger (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		turn_id TEXT NOT NULL,
		guild_id TEXT NOT NULL,
		channel_id TEXT NOT NULL,
		parts INTEGER NOT NULL,
		sent INTEGER NOT NULL,
		retries INTEGER NOT NULL,
		class TEXT NOT NULL DEFAULT '',
		reference_dropped INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_delivery_ledger_turn ON delivery_ledger(turn_id)`,
}

var deliveryTiDB = []string{
	`CREATE TABLE IF NOT EXISTS delivery_ledger (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		turn_id VARCHAR(64) NOT NULL,
		guild_id VARCHAR(64) NOT NULL,
		channel_id VARCHAR(64) NOT NULL,
		parts INT NOT NULL,
		sent INT NOT NULL,
		retries INT NOT NULL,
		class VARCHAR(32) NOT NULL DEFAULT '',
		reference_dropped TINYINT NOT NULL DEFAULT 0,
		created_at VARCHAR(64) NOT NULL,
		KEY idx_delivery_ledger_turn (turn_id)
	)`,
}

func init() {
	adminSQLite = append(adminSQLite, deliverySQLite...)
	adminTiDB = append(adminTiDB, deliveryTiDB...)
}

// NoteDelivery records how a reply was posted. A reply without a turn is not recorded.
func (s *Service) NoteDelivery(turnID, guildID, channelID string, rep DeliveryReport) {
	if turnID == "" || rep.Parts == 0 {
		return
	}
	db, err := s.adminDB()
	if err != nil {
		return
	}
	dropped := 0
	if rep.ReferenceDropped {
		dropped = 1
	}
	_, err = db.DB.Exec(`INSERT INTO delivery_ledger(turn_id, guild_id, channel_id, parts, sent, retries, class, reference_dropped, created_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, turnID, guildID, channelID, rep.Parts, rep.Sent, rep.Retries, string(rep.Class), dropped,
		s.Clock.Now().UTC().Format(time.RFC3339))
	if err != nil && s.Log != nil {
		s.Log.Warn("delivery ledger write failed", "err", err)
	}
}

var deliveryAlertMu sync.Mutex

// NoteDeliveryProblem tells the operators about a delivery failure they can fix. Today that is a missing
// Discord permission in a channel; it is queued as an alert at most once per channel per
// deliveryAlertEvery. Other classes are only in the ledger.
func (s *Service) NoteDeliveryProblem(guildID, channelID string, class errclass.Class) {
	if class != errclass.Permission || s.Store == nil || s.practice || channelID == "" {
		return
	}
	s.LogProblem(guildID, channelID, logPermission, "", class)
	deliveryAlertMu.Lock()
	defer deliveryAlertMu.Unlock()
	now := s.Clock.Now().UTC()
	last := map[string]string{}
	_, _ = s.kvGet("delivery:alerted", &last)
	if t, err := time.Parse(time.RFC3339, last[channelID]); err == nil && now.Sub(t) < deliveryAlertEvery {
		return
	}
	for ch, at := range last { // forget what has expired, so the map stays small
		if t, err := time.Parse(time.RFC3339, at); err != nil || now.Sub(t) >= deliveryAlertEvery {
			delete(last, ch)
		}
	}
	last[channelID] = now.Format(time.RFC3339)
	_ = s.kvSet("delivery:alerted", last)
	s.queueAlert(alertDeliveryPerm, map[string]any{"channel_id": channelID, "guild_id": guildID})
}

// deliveryPermissionProblems counts channels with a fresh permission failure (doctor).
func (s *Service) deliveryPermissionProblems() int {
	if s.Store == nil {
		return 0
	}
	last := map[string]string{}
	if ok, _ := s.kvGet("delivery:alerted", &last); !ok {
		return 0
	}
	n := 0
	for _, at := range last {
		if t, err := time.Parse(time.RFC3339, at); err == nil && s.Clock.Now().UTC().Sub(t) < deliveryAlertEvery {
			n++
		}
	}
	return n
}
