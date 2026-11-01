package application_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/config"
)

// stepClock is a clock the test moves by hand.
type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *stepClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// visionHostWith serves the vision role from a host that fails in the given way; hits counts its requests.
func visionHostWith(t *testing.T, hits *atomic.Int32, handler http.HandlerFunc) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestVisionProviderCoolsDownAfterAnOutage(t *testing.T) {
	down := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream down","type":"server_error"}}`))
	}
	hang := func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // the server notices a closed connection only once the body is read
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
	}
	empty := func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":0}}`))
	}
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		timeout time.Duration // models.vision.timeout_seconds as the tests may set it (below the validated minimum)
		cools   bool
	}{
		{"5xx", down, 0, true},
		{"timeout", hang, time.Second, true},
		{"empty answer", empty, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, h := visionSvc(t)
			clk := &stepClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
			svc.Clock = clk
			var hits atomic.Int32
			url := visionHostWith(t, &hits, tc.handler)
			applyDoc(t, svc, func(d *config.Document) {
				d.Providers["eyes-host"] = config.Provider{Kind: config.KindOpenAI, BaseURL: url, CredentialRef: "env:FAKE_OR_KEY"}
				d.Models["vision"] = config.Model{Provider: "eyes-host", Model: "eyes/model"}
			})
			if tc.timeout > 0 {
				doc := svc.Doc()
				models := map[string]config.Model{}
				for k, v := range doc.Models {
					models[k] = v
				}
				m := models["vision"]
				m.TimeoutSeconds = int(tc.timeout / time.Second)
				models["vision"] = m
				doc.Models = models
				svc.SetDocForTest(doc)
			}
			pic, _ := picture(t)
			turn := func(id string) {
				t.Helper()
				res, err := svc.ChatTurn(context.Background(), pictureInput(pic, id))
				if err != nil || res.Answer == "" {
					t.Fatalf("turn %s: %v %q", id, err, res.Answer)
				}
				if countImages(h.bodies(false)) == 0 {
					t.Fatalf("turn %s: the picture did not reach the answer model through the fallback path", id)
				}
			}
			turn("m1")
			first := hits.Load()
			if first == 0 {
				t.Fatal("the vision host was tried")
			}
			turn("m2")
			if tc.cools && hits.Load() != first {
				t.Fatalf("during the cool-down the vision host is skipped: %d -> %d requests", first, hits.Load())
			}
			if !tc.cools && hits.Load() == first {
				t.Fatal("an empty answer is no outage: the vision host is asked again")
			}
			if !tc.cools {
				return
			}
			clk.Advance(2 * time.Minute)
			turn("m3")
			if hits.Load() != first {
				t.Fatal("still inside the window two minutes later")
			}
			clk.Advance(2 * time.Minute) // past the three-minute window
			turn("m4")
			if hits.Load() == first {
				t.Fatal("after the window the vision host is tried again")
			}
			second := hits.Load()
			// a config reload ends the cool-down at once (the operator may have fixed the host)
			turn("m5")
			if hits.Load() != second {
				t.Fatal("a second failure starts a new window")
			}
			svc.SetDocForTest(svc.Doc()) // what a reload does (setDoc)
			turn("m6")
			if hits.Load() == second {
				t.Fatal("a config reload ends the cool-down")
			}
		})
	}
}

// When the host is back after the window, the vision model describes again and the next turn does not skip it.
func TestVisionCooldownEndsWhenTheHostRecovers(t *testing.T) {
	svc, h := visionSvc(t)
	clk := &stepClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	svc.Clock = clk
	var broken atomic.Bool
	broken.Store(true)
	var hits atomic.Int32
	url := visionHostWith(t, &hits, func(w http.ResponseWriter, r *http.Request) {
		if broken.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"` + catDescription + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	applyDoc(t, svc, func(d *config.Document) {
		d.Providers["eyes-host"] = config.Provider{Kind: config.KindOpenAI, BaseURL: url, CredentialRef: "env:FAKE_OR_KEY"}
		d.Models["vision"] = config.Model{Provider: "eyes-host", Model: "eyes/model"}
	})
	pic, _ := picture(t)
	mode := func(id string) string {
		res, err := svc.ChatTurn(context.Background(), pictureInput(pic, id))
		if err != nil {
			t.Fatal(err)
		}
		v, _ := visionTurnBody(t, svc, res.TurnID)["vision"].(map[string]any)
		return v["mode"].(string)
	}
	if m := mode("m1"); m != "native" {
		t.Fatalf("the outage falls back to the answer model: %s", m)
	}
	broken.Store(false)
	if m := mode("m2"); m != "native" {
		t.Fatalf("inside the window the recovered host is not asked yet: %s", m)
	}
	clk.Advance(4 * time.Minute)
	if m := mode("m3"); m != "described" {
		t.Fatalf("after the window the vision model describes again: %s", m)
	}
	if m := mode("m4"); m != "described" {
		t.Fatalf("a success leaves no cool-down: %s", m)
	}
	if countImages(h.bodies(false)) != 2 {
		t.Fatalf("only the two fallback turns sent the picture to the answer model: %d", countImages(h.bodies(false)))
	}
}

// chatRoutes keeps the vision adapter between turns; a different address, key reference or model builds
// another one, and so does a config reload.
func TestVisionAdapterIsKeptBetweenCalls(t *testing.T) {
	svc, _ := visionSvc(t)
	clk := &stepClock{t: time.Now()}
	svc.Clock = clk
	var hits atomic.Int32
	url := visionHostWith(t, &hits, func(w http.ResponseWriter, r *http.Request) {})
	applyDoc(t, svc, func(d *config.Document) {
		d.Providers["eyes-host"] = config.Provider{Kind: config.KindOpenAI, BaseURL: url, CredentialRef: "env:FAKE_OR_KEY"}
		d.Models["vision"] = config.Model{Provider: "eyes-host", Model: "eyes/model"}
	})
	doc := svc.Doc()
	a1, err := application.VisionAdapterForTest(svc, doc)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := application.VisionAdapterForTest(svc, doc)
	if err != nil || a1 != a2 {
		t.Fatalf("the same configuration reuses the adapter: %p %p %v", a1, a2, err)
	}
	other := func(change func(*config.Provider, *config.Model)) *config.Document {
		d := doc
		providers := map[string]config.Provider{}
		for k, v := range doc.Providers {
			providers[k] = v
		}
		models := map[string]config.Model{}
		for k, v := range doc.Models {
			models[k] = v
		}
		p, m := providers["eyes-host"], models["vision"]
		change(&p, &m)
		providers["eyes-host"], models["vision"] = p, m
		d.Providers, d.Models = providers, models
		return &d
	}
	for name, d := range map[string]*config.Document{
		"base URL":      other(func(p *config.Provider, m *config.Model) { p.BaseURL = url + "/v2" }),
		"key reference": other(func(p *config.Provider, m *config.Model) { p.CredentialRef = "env:ANOTHER_KEY" }),
		"model":         other(func(p *config.Provider, m *config.Model) { m.Model = "eyes/other" }),
	} {
		t.Setenv("ANOTHER_KEY", "k2")
		a, err := application.VisionAdapterForTest(svc, *d)
		if err != nil || a == a1 {
			t.Fatalf("a changed %s builds another adapter: %v", name, err)
		}
		// and the entry now belongs to that configuration: going back rebuilds again
		if back, _ := application.VisionAdapterForTest(svc, doc); back == a1 || back == a {
			t.Fatalf("%s: the entry follows the last configuration", name)
		}
		a1, _ = application.VisionAdapterForTest(svc, doc)
	}
	// a reload drops it (it also forgets what the adapter learned about refused pictures)
	before, _ := application.VisionAdapterForTest(svc, svc.Doc())
	svc.SetDocForTest(svc.Doc()) // what a reload does (setDoc)
	after, err := application.VisionAdapterForTest(svc, svc.Doc())
	if err != nil || after == before {
		t.Fatalf("a config reload builds a new adapter: %v", err)
	}
	// the clock bounds it: a key replaced under the same reference shows up after the TTL
	kept, _ := application.VisionAdapterForTest(svc, svc.Doc())
	if again, _ := application.VisionAdapterForTest(svc, svc.Doc()); again != kept {
		t.Fatal("reused inside the TTL")
	}
	clk.Advance(3 * time.Minute)
	later, _ := application.VisionAdapterForTest(svc, svc.Doc())
	if later == kept {
		t.Fatal("rebuilt after the TTL")
	}
	// a wall clock stepped back does not stretch the TTL
	clk.Advance(-10 * time.Minute)
	if back, _ := application.VisionAdapterForTest(svc, svc.Doc()); back == later {
		t.Fatal("kept across a clock stepped back")
	}
	// a key saved under the same reference (the console's save) drops it
	kept, _ = application.VisionAdapterForTest(svc, svc.Doc())
	if r := exec(svc, "providers.credentials", map[string]any{"id": "eyes-host", "credential_ref": "env:FAKE_OR_KEY"}); !r.OK {
		t.Fatalf("credentials: %+v", r.Error)
	}
	if fresh, _ := application.VisionAdapterForTest(svc, svc.Doc()); fresh == kept {
		t.Fatal("kept after a key was saved")
	}
}

// An adapter without a key is kept only briefly: a key added under the reference shows up within
// seconds (at once when it is saved from the console).
func TestVisionAdapterWithoutAKeyIsKeptBriefly(t *testing.T) {
	svc, _ := visionSvc(t)
	clk := &stepClock{t: time.Now()}
	svc.Clock = clk
	t.Setenv("MASAMONG_TEST_NO_SUCH_KEY", "")
	applyDoc(t, svc, func(d *config.Document) {
		d.Providers["eyes-host"] = config.Provider{Kind: config.KindOpenAI, BaseURL: "http://127.0.0.1:9", CredentialRef: "env:MASAMONG_TEST_NO_SUCH_KEY"}
		d.Models["vision"] = config.Model{Provider: "eyes-host", Model: "eyes/model"}
	})
	a1, _ := application.VisionAdapterForTest(svc, svc.Doc())
	if a1 != nil && a1.Configured() {
		t.Fatal("the key is not set")
	}
	t.Setenv("MASAMONG_TEST_NO_SUCH_KEY", "k")
	if again, _ := application.VisionAdapterForTest(svc, svc.Doc()); again != a1 {
		t.Fatal("a keyless adapter is kept for a few seconds (no lookup per turn)")
	}
	clk.Advance(21 * time.Second)
	a2, err := application.VisionAdapterForTest(svc, svc.Doc())
	if err != nil || a2 == nil || !a2.Configured() {
		t.Fatalf("the key added later is used soon: %v", err)
	}
}

// A key saved under the reference already in the config changes no revision; the provider's adapters
// are rebuilt anyway, so a connection test right after the save uses the new key.
func TestKeySavedUnderTheSameReferenceRebuildsTheAdapters(t *testing.T) {
	svc, _ := visionSvc(t)
	t.Setenv("MASAMONG_TEST_ROTATED_KEY", "old")
	applyDoc(t, svc, func(d *config.Document) {
		d.Providers["eyes-host"] = config.Provider{Kind: config.KindOpenAI, BaseURL: "http://127.0.0.1:9", CredentialRef: "env:MASAMONG_TEST_ROTATED_KEY"}
	})
	before := svc.Registry().LLM["eyes-host"]
	if before == nil {
		t.Fatal("no adapter for the provider")
	}
	t.Setenv("MASAMONG_TEST_ROTATED_KEY", "new")
	if r := exec(svc, "providers.credentials", map[string]any{"id": "eyes-host", "credential_ref": "env:MASAMONG_TEST_ROTATED_KEY"}); !r.OK {
		t.Fatalf("credentials: %+v", r.Error)
	}
	if after := svc.Registry().LLM["eyes-host"]; after == nil || after == before {
		t.Fatal("the adapter holding the old key was kept")
	}
}
