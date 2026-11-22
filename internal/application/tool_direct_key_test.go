package application_test

import (
	"encoding/json"
	"strings"
	"testing"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/tools"
)

func directKeyTool(header string) tools.Settings {
	return tools.Settings{Description: "d", Params: []tools.Param{{Name: "q"}},
		HTTP: &tools.HTTP{URL: "https://api.example.com/v1?q={q}", Headers: map[string]string{"X-API-Key": header}, CredentialRef: "env:OPS_KEY"}}
}

// Saving a key written into an operator tool is refused in the person's language, naming the tool and the
// field and never the value.
func TestDirectKeySaveErrorIsLocalized(t *testing.T) {
	svc := setupSvc(t)
	direct := "synthetic" + "-direct-value"
	d := svc.Doc()
	d.Tools = map[string]tools.Settings{"ops.lookup": directKeyTool(direct)}
	raw, _ := config.Marshal(d)
	for _, loc := range []string{"en", "ko"} {
		for _, act := range []string{"config.apply", "config.validate"} {
			in, _ := json.Marshal(map[string]any{"yaml": string(raw)})
			r := svc.Execute(t.Context(), action.Request{Action: act, Input: in, Locale: loc, BaseRevision: svc.Revision()})
			if r.OK || r.Error == nil || r.Error.MessageKey != "tool.direct_key" {
				t.Fatalf("%s/%s: %+v", act, loc, r.Error)
			}
			msg := r.Error.Message
			if !strings.Contains(msg, "ops.lookup") || !strings.Contains(msg, "headers.X-API-Key") || !strings.Contains(msg, "credential_ref") || strings.Contains(msg, direct) {
				t.Fatalf("%s/%s: %q", act, loc, msg)
			}
			if loc == "ko" && !strings.Contains(msg, "저장하지 않았어요") {
				t.Fatalf("not in Korean: %q", msg)
			}
		}
	}
}

// A configuration an earlier release accepted still loads; the doctor lists the tools that keep a key in
// it (ids only) so the operator moves them to credential_ref.
func TestDoctorNamesToolsWithDirectKeys(t *testing.T) {
	svc := setupSvc(t)
	direct := "synthetic" + "-direct-value"
	d := svc.Doc()
	d.Tools = map[string]tools.Settings{"ops.lookup": directKeyTool(direct), "ops.clean": directKeyTool("{credential}")}
	svc.SetDocForTest(d)
	res := out(t, svc, "runtime.doctor", nil)
	var found map[string]any
	for _, raw := range res["attention"].([]any) {
		if a := raw.(map[string]any); a["item"] == "tool_keys" {
			found = a
		}
	}
	if found == nil {
		t.Fatalf("no attention item: %v", res["attention"])
	}
	text, _ := found["reason_text"].(string)
	if found["reason"] != "tool_direct_key:ops.lookup" || !strings.Contains(text, "ops.lookup") || strings.Contains(text, "ops.clean") || strings.Contains(text, direct) {
		t.Fatalf("attention: %v", found)
	}
	if f := res["features"].(map[string]any)["tool_keys"].(map[string]any); f["status"] != "degraded" {
		t.Fatalf("feature: %v", f)
	}
	// nothing to report once the key is a reference
	d.Tools = map[string]tools.Settings{"ops.lookup": directKeyTool("{credential}")}
	svc.SetDocForTest(d)
	res = out(t, svc, "runtime.doctor", nil)
	if _, listed := res["features"].(map[string]any)["tool_keys"]; listed {
		t.Fatalf("a clean configuration lists no tool: %v", res["features"])
	}
}
