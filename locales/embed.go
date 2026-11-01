// Package locales embeds the message catalogs so the binary never depends on the working directory (15장).
package locales

import "embed"

//go:embed *.json
var FS embed.FS

// UIFS holds the admin console strings (ui/<lang>.json); the console fetches them from /api/i18n.
//
//go:embed ui/*.json
var UIFS embed.FS
