package application

import "flag"

// The golden files of the E7 guards (exported surface, schema) are rewritten with
//
//	go test ./internal/application -run '<the test>' -update
//
// The flag is declared once, here: the internal and the external test files are one binary, and a
// flag may not be defined twice. External tests read it through UpdateGoldens.
var updateGoldens = flag.Bool("update", false, "rewrite the golden files of the exported surface and the schema")

// UpdateGoldens reports whether the tests were started with -update.
func UpdateGoldens() bool { return *updateGoldens }
