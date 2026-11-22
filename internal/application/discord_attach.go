package application

// Discord is the attached gateway, nil until the daemon attaches one. Workers already running (resumed
// jobs, the scheduler) read it while the daemon attaches it, so it is held atomically.
func (s *Service) Discord() DiscordInfo {
	if d := s.discord.Load(); d != nil {
		return *d
	}
	return nil
}

// SetDiscord attaches the gateway; nil detaches it.
func (s *Service) SetDiscord(d DiscordInfo) {
	if d == nil {
		s.discord.Store(nil)
		return
	}
	s.discord.Store(&d)
}
