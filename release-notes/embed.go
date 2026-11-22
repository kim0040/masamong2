// Package releasenotes embeds the release notes (vX.Y.Z.json) so every build knows the notes of every
// version up to its own without a network. internal/relnotes reads, validates and renders them; the
// Markdown files next to the JSON are what the release workflow publishes and are not embedded.
package releasenotes

import "embed"

//go:embed *.json
var FS embed.FS
