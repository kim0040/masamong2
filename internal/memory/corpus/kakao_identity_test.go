package corpus

import (
	"errors"
	"strconv"
	"testing"
)

func TestKakaoOccurrenceStateIsBoundedAndResetsAtTheNextTimestamp(t *testing.T) {
	c := NewKakaoOccurrences()
	for i := 0; i < 100000; i++ {
		if n, err := c.Next("room", "first", "Alice", strconv.Itoa(i)); err != nil || n != 0 {
			t.Fatalf("row %d n=%d err=%v", i, n, err)
		}
	}
	if _, err := c.Next("room", "first", "Bob", "one more"); !errors.Is(err, ErrKakaoGroupTooLarge) {
		t.Fatalf("unbounded timestamp group: %v", err)
	}
	if n, err := c.Next("room", "second", "Bob", "one more"); err != nil || n != 0 {
		t.Fatalf("group did not expire: n=%d err=%v", n, err)
	}
}
