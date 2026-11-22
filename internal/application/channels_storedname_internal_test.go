package application

import (
	"strings"
	"testing"

	"masamong/internal/config"
)

// The attachment delete joins stored_name to the channel's folder, so right before the delete only the
// generated form passes, the same form config parsing accepts.
func TestAttachmentDeleteAcceptsOnlyTheGeneratedStoredName(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	for name, want := range map[string]bool{
		hash: true, hash + ".md": true, hash + ".tar.gz": true,
		"../" + hash: false, hash + "/../x": false, hash + `\..\x`: false, "..": false, ".": false,
		"notes.md": false, strings.Repeat("AB", 32): false, "": false, hash + ".md\x00": false,
	} {
		if got := generatedStoredName.MatchString(name); got != want {
			t.Errorf("%q: %v, want %v", name, got, want)
		}
	}
	// a document that names such a file is refused when parsed, so the delete path sees it only if the
	// two checks drift apart
	doc := config.Default("gate", t.TempDir())
	doc.Guilds = map[string]config.GuildSettings{"g1": {Channels: map[string]config.ChannelPolicy{"c1": {
		RAGAttachments: []config.RAGAttachmentRef{{ID: "att_1", ContentHash: "sha256:" + hash, Filename: "n.md", StoredName: "../" + hash}},
	}}}}
	raw, err := config.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.Parse(raw); err == nil {
		t.Fatal("config parsing accepted a stored_name the delete path refuses")
	}
}
