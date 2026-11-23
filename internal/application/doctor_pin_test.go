package application_test

import (
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"

	"masamong/internal/config"
)

// The doctor is a list of checks whose items and attention lines the console, the CLI and the tray
// render. This test pins what it reports for a handful of configurations, so splitting it into check
// functions (or adding a check) cannot change an existing line unnoticed. Regenerate the golden file
// only for a deliberate change:
//
//	go test ./internal/application -run TestDoctorReportPinned -update-doctor
var updateDoctor = flag.Bool("update-doctor", false, "rewrite testdata/doctor_pin.golden.json")

// machineBound items depend on the host running the test (memory limit, host tier, heap numbers).
var machineBound = []string{"go_memory_limit", "host_profile"}

func TestDoctorReportPinned(t *testing.T) {
	t.Parallel()
	keys := map[string]string{"MASAMONG_OPENROUTER_KEY": "k", "MASAMONG_TAVILY_KEY": "k", "MASAMONG_FINNHUB_KEY": "k", "MASAMONG_KMA_KEY": "k"}
	scenarios := []struct {
		name  string
		env   map[string]string
		apply func(d *config.Document)
	}{
		{"fresh instance", nil, nil},
		{"web admin off", nil, func(d *config.Document) { d.Admin.Web.Enabled = false }},
		{"discord enabled, no gateway", nil, func(d *config.Document) { d.Discord.Enabled = true }},
		{"bad timezones", nil, func(d *config.Document) { d.I18n.Timezone = "Nowhere/Land"; d.Memory.NaiveTimezone = "Also/Nowhere" }},
		{"features off", nil, func(d *config.Document) {
			d.Features = map[string]bool{"weather": false, "disasters": false, "finance": false, "search": false,
				"polls": false, "fortune": false, "notices": false, "images": false}
		}},
		{"participation shadow", nil, func(d *config.Document) { d.Participation.Enabled, d.Participation.Mode = true, "shadow" }},
		{"retrieval off", nil, func(d *config.Document) { d.Memory.Retrieve = "off" }},
		{"message vectors off", nil, func(d *config.Document) { off := false; d.Memory.MessageVectors = &off }},
		{"vision role set", nil, func(d *config.Document) {
			d.Models["vision"] = config.Model{Provider: "openrouter", Model: "some/vision-model"}
		}},
		{"keys present", keys, nil},
		{"keys present, features off", keys, func(d *config.Document) {
			d.Features = map[string]bool{"weather": false, "disasters": false, "finance": false, "search": false,
				"polls": false, "fortune": false, "notices": false, "images": false}
		}},
		{"keys present, participation conservative", keys, func(d *config.Document) {
			d.Participation.Enabled, d.Participation.Mode = true, "conservative"
		}},
		{"keys present, retrieval off", keys, func(d *config.Document) { d.Memory.Retrieve = "off" }},
		{"keys present, vision set", keys, func(d *config.Document) {
			d.Models["vision"] = config.Model{Provider: "openrouter", Model: "some/vision-model"}
		}},
		{"keys present, vision switch off", keys, func(d *config.Document) {
			d.Models["vision"] = config.Model{Provider: "openrouter", Model: "some/vision-model"}
			d.Chat.Vision = "off"
		}},
		{"keys present, image model", keys, func(d *config.Document) {
			d.Models["image"] = config.Model{Provider: "openrouter", Model: "some/image-model"}
		}},
	}
	got := map[string]any{}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			svc := setupSvcEnv(t, sc.env)
			if sc.apply != nil {
				applyDoc(t, svc, sc.apply)
			}
			res := exec(svc, "runtime.doctor", nil)
			if !res.OK {
				t.Fatalf("doctor: %+v", res.Error)
			}
			raw, err := json.Marshal(map[string]any{"features": res.Output["features"], "attention": res.Output["attention"],
				"title": res.Output["title"], "profile": res.Output["profile"], "single_ready": res.Output["single_ready"],
				"memory_complete": res.Output["memory_complete"], "storage_backend": res.Output["storage_backend"]})
			if err != nil {
				t.Fatal(err)
			}
			var v map[string]any
			_ = json.Unmarshal(raw, &v)
			if f, ok := v["features"].(map[string]any); ok {
				for _, k := range machineBound {
					delete(f, k)
				}
			}
			got[sc.name] = v
		})
	}
	raw, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	const golden = "testdata/doctor_pin.golden.json"
	if *updateDoctor {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v (run with -update-doctor to create it)", err)
	}
	if string(want) != string(raw) {
		t.Errorf("the doctor's report changed:\n%s", firstDifference(string(want), string(raw)))
	}
}

// firstDifference names the first differing line of two texts.
func firstDifference(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var a, b string
		if i < len(w) {
			a = w[i]
		}
		if i < len(g) {
			b = g[i]
		}
		if a != b {
			return "line " + string(rune('0'+i%10)) + "...\n want: " + a + "\n got:  " + b
		}
	}
	return "(no difference)"
}
