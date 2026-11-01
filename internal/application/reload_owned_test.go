package application

import (
	"os"
	"strings"
	"testing"
)

// A goroutine that reloads the lexical index must be registered with the service (ownedGo), or a reload
// racing Close reopens an index after engine.Close and keeps a file handle that blocks resetting or
// deleting the data folder on Windows. The race itself cannot be forced, so this guards the structure:
// no unowned goroutine may call Reload.
func TestIndexReloadGoroutinesAreOwnedByTheService(t *testing.T) {
	for _, file := range []string{"memory_blocks.go", "memory.go", "revisions.go", "memory_review.go", "channels.go"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if strings.Contains(line, "Reload()") && strings.Contains(line, "crash.Go(") {
				t.Errorf("%s:%d reloads the index in an unowned goroutine; use s.ownedGo", file, i+1)
			}
		}
	}
}
