package application

import "time"

// ThreadBackedOffForTest reports whether the channel rests from thread offers at the service's clock.
func (s *Service) ThreadBackedOffForTest(guildID, channelID string) bool {
	return s.threadBackedOff(guildID, channelID, s.Clock.Now())
}

// LogThreadOfferForTest writes an "offer" row of the decision log at the given time.
func (s *Service) LogThreadOfferForTest(guildID, channelID string, at time.Time) {
	db, err := s.adminDB()
	if err != nil {
		return
	}
	_, _ = db.DB.Exec(`INSERT INTO conversation_log(id,guild_id,channel_id,created_at,kind,decision) VALUES(?,?,?,?,?,?)`,
		newID("cv_"), guildID, channelID, at.UTC().Format(time.RFC3339Nano), convThread, "offer")
}
