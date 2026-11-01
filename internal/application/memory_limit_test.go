package application_test

import (
	"math"
	"runtime/debug"
	"strings"
	"testing"

	"masamong/internal/hostprofile"
)

func TestDoctorReportsProcessMemoryLimit(t *testing.T) {
	svc := practiceSvc(t)
	res := exec(svc, "runtime.doctor", nil)
	if !res.OK {
		t.Fatalf("doctor failed: %+v", res.Error)
	}
	features := res.Output["features"].(map[string]any)
	limit, ok := features["go_memory_limit"].(map[string]any)
	if !ok {
		t.Fatal("doctor omitted process memory limit")
	}
	want := debug.SetMemoryLimit(-1)
	if want == math.MaxInt64 {
		want = 0
	}
	if limit["bytes"] != want {
		t.Fatalf("limit bytes %v want %d", limit["bytes"], want)
	}
	if source, ok := limit["source"].(string); !ok || source == "" {
		t.Fatal("missing detection source")
	}
	if _, ok := limit["automatic"].(bool); !ok {
		t.Fatal("missing automatic flag")
	}
	if reason, ok := limit["reason"].(string); !ok || reason == "" {
		t.Fatal("missing generic GUI doctor detail")
	}
}

func TestDoctorReportsHostProfile(t *testing.T) {
	prev := hostprofile.Current()
	t.Cleanup(func() { hostprofile.Apply(prev) })
	hostprofile.Apply(hostprofile.DetectFrom(8, 8<<30, "large"))
	svc := practiceSvc(t)
	res := exec(svc, "runtime.doctor", nil)
	if !res.OK {
		t.Fatalf("doctor failed: %+v", res.Error)
	}
	features := res.Output["features"].(map[string]any)
	item, ok := features["host_profile"].(map[string]any)
	if !ok {
		t.Fatal("doctor omitted host profile")
	}
	if item["status"] != "ready" {
		t.Fatalf("status %v", item["status"])
	}
	reason, _ := item["reason"].(string)
	if reason != "host_profile:large, 8 CPU, 8192 MiB, env" {
		t.Fatalf("reason %q", reason)
	}
	text, _ := item["reason_text"].(string)
	if text == "" || strings.Contains(text, "host_profile") {
		t.Fatalf("reason text %q", text)
	}
}
