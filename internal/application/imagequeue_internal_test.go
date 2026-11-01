package application

import (
	"testing"

	"masamong/internal/chat"
	"masamong/internal/lang"
)

// How a picture is drawn is the operator's setting: the model is never offered it as a parameter.
func TestPictureQueueSettingIsNotOfferedToTheModel(t *testing.T) {
	s, _ := deliveryService(t)
	for _, tool := range s.chatToolset(chat.Voice{Pack: lang.For("en")}, "g", "c").Enabled {
		if tool.ID != "images.generate" {
			continue
		}
		names := map[string]bool{}
		for _, p := range tool.Params {
			names[p.Name] = true
		}
		if !names["prompt"] || !names["aspect_ratio"] || names["async"] {
			t.Fatalf("params offered to the model: %v", names)
		}
		return
	}
	t.Fatal("images.generate is not offered in chat")
}

func TestPictureRequestsMeanTheSameThingWhenOnlyCaseSpacingAndPunctuationDiffer(t *testing.T) {
	a := imageKey("g", "c", "u", "Hamster in a space suit", "1:1")
	if b := imageKey("g", "c", "u", "  hamster   in a SPACE suit!! ", "1:1"); a != b {
		t.Fatalf("%q != %q", a, b)
	}
	for name, other := range map[string]string{
		"another person":  imageKey("g", "c", "v", "Hamster in a space suit", "1:1"),
		"another channel": imageKey("g", "d", "u", "Hamster in a space suit", "1:1"),
		"another server":  imageKey("h", "c", "u", "Hamster in a space suit", "1:1"),
		"another prompt":  imageKey("g", "c", "u", "Hamster on the moon", "1:1"),
		"another ratio":   imageKey("g", "c", "u", "Hamster in a space suit", "16:9"),
	} {
		if other == a {
			t.Fatalf("%s must be another request", name)
		}
	}
}
