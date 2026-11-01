package application_test

import (
	"testing"

	"masamong/internal/config"
)

// A stop word is a whole message in the channel's language or the one it is written in; the same word
// inside a sentence is ordinary talk.
func TestTurnStopCue(t *testing.T) {
	svc := setupSvc(t)
	for _, text := range []string{"그만", " 그만! ", "멈춰.", "취소~", "stop", "Stop.", "Never mind!", "nevermind", "cancel", "para", "やめて！", "ストップ", "停止", "取消。", "停"} {
		if !svc.TurnStopCue("g", "c", text) {
			t.Errorf("%q should stop a reply", text)
		}
	}
	for _, text := range []string{"", "✋", "그만 좀 웃겨", "stop it now", "please do not stop", "그만해줘 제발", "hello"} {
		if svc.TurnStopCue("g", "c", text) {
			t.Errorf("%q is ordinary talk", text)
		}
	}
	// Spanish: by the room's language; elsewhere only a bare "para" is told apart from English
	applyDoc(t, svc, func(d *config.Document) {
		if d.Guilds == nil {
			d.Guilds = map[string]config.GuildSettings{}
		}
		d.Guilds["es"] = config.GuildSettings{Locale: "es"}
	})
	if !svc.TurnStopCue("es", "c", "Para.") || !svc.TurnStopCue("es", "c", "detente") || !svc.TurnStopCue("es", "c", "cancelar!") {
		t.Fatal("Spanish stop words in a Spanish room")
	}
}
