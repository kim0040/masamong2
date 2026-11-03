package application

import (
	"context"
	"strings"
	"testing"
)

func TestMessageVectorDoctorUnknownWidthAndQueryError(t *testing.T) {
	s := privacyService(t)
	widthConfig(t, s, "baai/bge-m3", 4096)
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	status, reason := s.messageVectorDoctor(context.Background(), "ready")
	if status != "pending_index" || !strings.Contains(reason, "waiting:leaf_width_unknown") || strings.Contains(reason, "message_dims") {
		t.Fatalf("unknown width must not claim full dimensions: %s %s", status, reason)
	}
	if err := e.Store.DB.Reader().Close(); err != nil {
		t.Fatal(err)
	}
	status, reason = s.messageVectorDoctor(context.Background(), "ready")
	if status != "error" || reason != "leaf_width_query_failed" {
		t.Fatalf("query error must contain only a status code: %s %s", status, reason)
	}
}
