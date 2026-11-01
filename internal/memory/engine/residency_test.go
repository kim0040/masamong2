package engine

import (
	"context"
	"testing"
)

func TestReloadSamplesStageBoundaries(t *testing.T) {
	e, _ := messageEngine(t)
	t.Cleanup(e.Close)
	e.Models.EmbedDims = 16
	seed(t, e)
	if _, ok := e.ResidencyMeasurement(); ok {
		t.Fatal("a measurement exists before the first rebuild")
	}
	var got []ReloadMeasurement
	e.OnReloadMeasurement = func(m ReloadMeasurement) { got = append(got, m) }
	if _, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Reload(); err != nil {
		t.Fatal(err)
	}
	m, ok := e.ResidencyMeasurement()
	if !ok || m.Outcome != ReloadPublished || len(got) == 0 {
		t.Fatalf("measurement: %+v ok=%v callbacks=%d", m, ok, len(got))
	}
	stages := make([]string, len(m.Samples))
	for i, s := range m.Samples {
		stages[i] = s.Stage
		if s.HeapAlloc == 0 || s.HeapInuse == 0 {
			t.Fatalf("empty sample: %+v", s)
		}
		if i > 0 && s.Since < m.Samples[i-1].Since {
			t.Fatalf("samples out of order: %+v", m.Samples)
		}
	}
	want := []string{StageRead, StageBuild, StageMessageVectors, StagePublish}
	if len(stages) != len(want) {
		t.Fatalf("stages %v, want %v", stages, want)
	}
	for i := range want {
		if stages[i] != want[i] {
			t.Fatalf("stages %v, want %v", stages, want)
		}
	}
	if m.LastStage() != StagePublish || m.MaxHeapAlloc() == 0 {
		t.Fatalf("summary: last %q max %d", m.LastStage(), m.MaxHeapAlloc())
	}
	// the stored copy is the engine's: changing what a caller got back does not change it
	m.Samples[0].HeapAlloc = 1
	if again, _ := e.ResidencyMeasurement(); again.Samples[0].HeapAlloc == 1 {
		t.Fatal("the measurement was returned by reference")
	}
}

func TestReloadWithoutMessageVectorsHasNoMessageStage(t *testing.T) {
	e, _ := newEngine(t, false)
	t.Cleanup(e.Close)
	e.Models.EmbedDims = 16
	seed(t, e)
	if _, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	m, _ := e.ResidencyMeasurement()
	for _, s := range m.Samples {
		if s.Stage == StageMessageVectors {
			t.Fatalf("a message-vector stage was sampled while they are off: %+v", m.Samples)
		}
	}
	if m.LastStage() != StagePublish {
		t.Fatalf("last stage %q", m.LastStage())
	}
}

func TestRetiredEngineKeepsPreviousMeasurementAndDiscardsRace(t *testing.T) {
	e, _ := newEngine(t, false)
	t.Cleanup(e.Close)
	e.Models.EmbedDims = 16
	seed(t, e)
	if _, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	before, _ := e.ResidencyMeasurement()
	e.retired.Store(true)
	if _, err := e.Reload(); err == nil {
		t.Fatal("a retired engine rebuilt")
	}
	after, _ := e.ResidencyMeasurement()
	if after.StartedAt != before.StartedAt || after.Outcome != ReloadPublished {
		t.Fatalf("a refused rebuild replaced the measurement: %+v", after)
	}
	// replaced while building: the samples reach the build stage but not the publish stage
	e.retired.Store(false)
	r := &reloadRecorder{}
	r.sample(StageRead)
	r.sample(StageBuild)
	e.finishMeasurement(r, ReloadDiscarded)
	m, _ := e.ResidencyMeasurement()
	if m.Outcome != ReloadDiscarded || m.LastStage() != StageBuild {
		t.Fatalf("discarded: %+v", m)
	}
}

func TestInheritCarriesTheLastMeasurement(t *testing.T) {
	old, _ := newEngine(t, false)
	t.Cleanup(old.Close)
	old.Models.EmbedDims = 16
	seed(t, old)
	if _, err := old.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	next := New(old.Store, nil, old.Models, old.Settings)
	t.Cleanup(next.Close)
	if _, ok := next.ResidencyMeasurement(); ok {
		t.Fatal("a new engine starts with a measurement")
	}
	next.Inherit(old)
	if m, ok := next.ResidencyMeasurement(); !ok || m.Outcome != ReloadPublished {
		t.Fatalf("inherit: %+v ok=%v", m, ok)
	}
}

func TestEstimateRoomBytesIsFixedPlusPerMessage(t *testing.T) {
	if EstimateRoomBytesAt(0, 512) != 0 {
		t.Fatal("a room without messages costs nothing")
	}
	small, big := EstimateRoomBytesAt(1000, 512), EstimateRoomBytesAt(11000, 512)
	perMessage := (big - small) / 10000
	if want := int64(EstimateBytesPerMessage) + (512+estimateVectorRowOverhead)*EstimateMessageVectorPermille/1000; perMessage != want {
		t.Fatalf("per message %d, want %d", perMessage, want)
	}
	if small-1000*perMessage != EstimateRoomFixedBytes {
		t.Fatalf("fixed part %d", small-1000*perMessage)
	}
	if EstimateRoomBytesAt(1000, 0) >= small || EstimateRoomBytesAt(1000, 1024) <= small {
		t.Fatal("the message-vector width does not move the estimate")
	}

	e, _ := newEngine(t, false)
	t.Cleanup(e.Close)
	e.Settings.MessageVectors = false
	if e.EstimateRoomBytes(1000) != EstimateRoomBytesAt(1000, 0) {
		t.Fatal("message vectors off still priced a vector")
	}
	e.Settings.MessageVectors = true
	e.Models.EmbedDims = 1024
	e.Models.EmbedTruncatable = true
	if got := e.EstimateRoomBytes(1000); got != EstimateRoomBytesAt(1000, 512) {
		t.Fatalf("a truncatable model uses the 512 width: %d", got)
	}
}
