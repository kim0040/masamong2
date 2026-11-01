package application

import (
	"bytes"
	"strings"
	"testing"
)

func TestUpdateEntryEnforcesDeclaredAndActualSize(t *testing.T) {
	const max int64 = 4
	var buf bytes.Buffer
	if _, err := writeUpdateEntry("masamong", strings.NewReader("small"), 0, &buf, max); err == nil {
		t.Fatal("entry whose actual bytes exceed the cap was accepted")
	}
	buf.Reset()
	if _, err := writeUpdateEntry("masamong", strings.NewReader("tiny"), ^uint64(0), &buf, max); err == nil {
		t.Fatal("oversized unsigned declaration was accepted")
	}
	buf.Reset()
	n, err := writeUpdateEntry("masamong", strings.NewReader("tiny"), 4, &buf, max)
	if err != nil || n != 4 || buf.String() != "tiny" {
		t.Fatalf("valid entry = %q (%d), %v", buf.String(), n, err)
	}
}
