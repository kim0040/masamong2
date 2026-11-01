package discord

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/vision"
)

func att(id, ct, name string) *discordgo.MessageAttachment {
	return &discordgo.MessageAttachment{ID: id, URL: "https://cdn.discordapp.com/attachments/1/" + id + "/" + name, ContentType: ct, Filename: name, Size: 1000}
}

func TestImageRefsFromTheCallAndTheMessageItRepliesTo(t *testing.T) {
	replied := &discordgo.Message{ID: "r1", Attachments: []*discordgo.MessageAttachment{att("a3", "image/jpeg", "photo.jpg")}}
	m := &discordgo.Message{ID: "m1", Content: "<@bot> 이거 뭐야?",
		Attachments:       []*discordgo.MessageAttachment{att("a1", "image/png", "x.png"), att("a2", "application/pdf", "doc.pdf")},
		ReferencedMessage: replied, MessageReference: &discordgo.MessageReference{MessageID: "r1"}}
	refs := imageRefs([]*discordgo.Message{m})
	if len(refs) != 2 {
		t.Fatalf("want the png and the replied-to photo, got %+v", refs)
	}
	if refs[0].MessageID != "m1" || refs[0].ContentType != "image/png" || refs[1].MessageID != "r1" || refs[1].Filename != "photo.jpg" {
		t.Fatalf("refs: %+v", refs)
	}
}

func TestImageRefsSkipNonImagesAndStopAtThree(t *testing.T) {
	m := &discordgo.Message{ID: "m1", Attachments: []*discordgo.MessageAttachment{
		att("v", "video/mp4", "clip.mp4"), att("s", "image/svg+xml", "logo.svg"), att("b", "image/bmp", "old.bmp"),
		att("1", "image/png", "1.png"), att("2", "image/webp", "2.webp"), att("3", "", "3.JPG"), att("4", "image/gif", "4.gif"),
	}}
	refs := imageRefs([]*discordgo.Message{m})
	if len(refs) != 3 {
		t.Fatalf("at most three pictures: %+v", refs)
	}
	for i, want := range []string{"1.png", "2.webp", "3.JPG"} {
		if refs[i].Filename != want {
			t.Fatalf("ref %d: %+v", i, refs[i])
		}
	}
}

func TestImageRefsFromAForward(t *testing.T) {
	m := &discordgo.Message{ID: "m1",
		MessageReference: &discordgo.MessageReference{Type: discordgo.MessageReferenceTypeForward, MessageID: "orig"},
		MessageSnapshots: []discordgo.MessageSnapshot{{Message: &discordgo.Message{Attachments: []*discordgo.MessageAttachment{att("f", "image/png", "f.png")}}}}}
	refs := imageRefs([]*discordgo.Message{m})
	if len(refs) != 1 || refs[0].MessageID != "m1" {
		t.Fatalf("forwarded picture: %+v", refs)
	}
	// the same picture in two held messages is read once
	if refs := imageRefs([]*discordgo.Message{m, m}); len(refs) != 1 {
		t.Fatalf("duplicate picture: %+v", refs)
	}
}

func TestImageSelectionCountsOmissionsAndPrioritizesRequestPictures(t *testing.T) {
	first := &discordgo.Message{ID: "first", Attachments: []*discordgo.MessageAttachment{att("own1", "image/png", "own1.png")}}
	first.ReferencedMessage = &discordgo.Message{ID: "replied", Attachments: []*discordgo.MessageAttachment{att("reply", "image/png", "reply.png")}}
	second := &discordgo.Message{ID: "second", Attachments: []*discordgo.MessageAttachment{att("own2", "image/png", "own2.png")}}
	selection := selectImageRefs([]*discordgo.Message{first, second})
	if got := []string{selection.Refs[0].Filename, selection.Refs[1].Filename, selection.Refs[2].Filename}; strings.Join(got, ",") != "own1.png,own2.png,reply.png" {
		t.Fatalf("selection order = %v", got)
	}

	tooMany := &discordgo.Message{ID: "many", Attachments: []*discordgo.MessageAttachment{
		att("1", "image/png", "1.png"), att("2", "image/png", "2.png"), att("3", "image/png", "3.png"), att("4", "image/png", "4.png"),
	}}
	selection = selectImageRefs([]*discordgo.Message{tooMany})
	if len(selection.Refs) != vision.MaxImages || selection.Total != 4 || selection.Skipped[vision.ReasonOverLimit] != 1 {
		t.Fatalf("selection counts: selected=%d total=%d skipped=%v", len(selection.Refs), selection.Total, selection.Skipped)
	}
}

func TestOnlyCalls(t *testing.T) {
	names := []string{"마사몽"}
	for _, c := range []struct {
		content string
		want    bool
	}{{"", true}, {"<@bot>", true}, {"마사몽", true}, {"<@bot> 이거 뭐야?", false}, {"뭐야 이거", false}} {
		if got := onlyCalls([]*discordgo.Message{{Content: c.content}}, "bot", names); got != c.want {
			t.Errorf("onlyCalls(%q)=%v", c.content, got)
		}
	}
}
