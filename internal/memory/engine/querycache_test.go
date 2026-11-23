package engine

import (
	"context"
	"testing"
)

// A kept vector is returned without building anything: the "did not finish" result a flight carries
// is created with the flight, not on every lookup of a cached or shared query.
func TestQueryVectorAllocatesNothingOnAKeptVector(t *testing.T) {
	e := &Engine{}
	key := "space|query"
	want := []float32{1, 2, 3}
	e.queries.remember(key, want)
	n := testing.AllocsPerRun(200, func() {
		v, err := e.queryVector(context.Background(), key, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(v) != len(want) {
			t.Fatalf("v=%v, want %v", v, want)
		}
	})
	if n != 0 {
		t.Fatalf("a kept vector's lookup allocates: %v allocs/run", n)
	}
}
