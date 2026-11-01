package application

// ConversationLogForTest lists the decisions of one kind, oldest first.
func (s *Service) ConversationLogForTest(kind string) []string {
	db, err := s.adminDB()
	if err != nil {
		return nil
	}
	rows, err := db.DB.Query(`SELECT decision FROM conversation_log WHERE kind=? ORDER BY created_at, rowid`, kind)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		_ = rows.Scan(&d)
		out = append(out, d)
	}
	return out
}

// ChannelRoomsForTest is a channel's memory scope.
func (s *Service) ChannelRoomsForTest(guildID, channelID string) []string {
	return s.channelRooms(guildID, channelID)
}
