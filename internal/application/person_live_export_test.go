package application

// PeopleCalls is how many model calls a room-memory pass made for one-line statements made to the bot.
func (r RoomMemoryReport) PeopleCalls() int { return r.peopleCalls }

// PeopleClaims is how many person claims live extraction saved in a room-memory pass.
func (r RoomMemoryReport) PeopleClaims() int { return r.peopleClaims }
