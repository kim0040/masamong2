package application

// PeopleCalls is how many model calls a room-memory pass made for one-line statements made to the bot.
func (r RoomMemoryReport) PeopleCalls() int { return r.peopleCalls }

// PeopleClaims is how many person claims live extraction saved in a room-memory pass.
func (r RoomMemoryReport) PeopleClaims() int { return r.peopleClaims }

// PeopleSkipped is how many people facts of the answers a room-memory pass skipped under the live rules
// (one speaker's own lines; a self statement names only its subject).
func (r RoomMemoryReport) PeopleSkipped() int { return r.peopleSkipped }

// PeopleFailed is how many answers of a room-memory pass lost their people list (cut after a whole
// episodes list): the episodes were kept and the people were not asked for again.
func (r RoomMemoryReport) PeopleFailed() int { return r.peopleFailed }
