package application

import "testing"

type attachGateway struct{ DiscordInfo }

func (attachGateway) Connected() bool { return false }

// The daemon resumes jobs before it starts the gateway. A resumed prepare job lists the known
// channels (blockBySource reads the gateway) while discord.Start attaches it; the plain field this
// used to be was a data race there (-race).
func TestDiscordAttachWhileAResumedJobReadsIt(t *testing.T) {
	s := privacyService(t)
	done := make(chan struct{})
	go func() { defer close(done); s.blockBySource("discord:1:2") }()
	s.SetDiscord(attachGateway{})
	<-done
	if s.Discord() == nil {
		t.Fatal("the attached gateway is not seen")
	}
	s.SetDiscord(nil)
	if s.Discord() != nil {
		t.Fatal("a detached gateway is still seen")
	}
}
