package application

import (
	"slices"
	"testing"
)

// The memory, message-vector and residency stages are each their own doctor step, so a slow one is
// named in the slow-request log (memory_ms, message_vectors_ms, residency_ms) instead of hiding
// inside the step before it.
func TestDoctorLapsTheMemoryStagesSeparately(t *testing.T) {
	var names []string
	for _, c := range doctorChecks {
		names = append(names, c.name)
	}
	i := slices.Index(names, "memory")
	if i < 0 || i+3 >= len(names) || names[i+1] != "message_vectors" || names[i+2] != "residency" || names[i+3] != "participation" {
		t.Fatalf("memory, message_vectors and residency are not separate doctor steps: %v", names)
	}
}
