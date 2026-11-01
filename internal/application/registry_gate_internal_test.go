package application

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/config"
	"masamong/internal/providers"
	"masamong/internal/providers/openmeteo"
	"masamong/internal/providers/websearch"
)

// The hosts with tight documented limits get few requests in flight from the process; the shared
// client (model hosts) is not bounded.
func TestStrictProvidersAreGated(t *testing.T) {
	doc := config.Document{Providers: map[string]config.Provider{"fh": {Kind: config.KindFinnhub}, "kma": {Kind: config.KindKMA}}}
	r := buildRegistry(doc, nil, time.Now)
	fh, _ := registryGateState(r.Finnhub.Client)
	km, _ := registryGateState(r.KMA.Client)
	if fh != 1 || km != 1 {
		t.Fatal("finnhub and kma must have one request in flight")
	}
	if r.Client.Gate != nil {
		t.Fatal("the shared client must stay unbounded")
	}
	for _, c := range []struct {
		kind  string
		keyed bool
		want  int
	}{{config.KindFX, false, 1}, {config.KindKakaoSearch, true, 2}, {config.KindOpenMeteo, false, 2}, {config.KindLinkup, true, 2},
		{config.KindJinaReader, false, 1}, {config.KindJinaReader, true, 4}, {config.KindOpenAI, true, 0}, {config.KindTavily, true, 0}} {
		if got := providerGate(c.kind, c.keyed); got != c.want {
			t.Errorf("%s keyed=%v: %d, want %d", c.kind, c.keyed, got, c.want)
		}
	}
}

func registryGateState(c *providers.Client) (int, int) {
	if g, ok := any(c).(interface{ GateState() (int, int) }); ok {
		return g.GateState()
	}
	return cap(c.Gate), len(c.Gate) // lets the regression tests execute against 574cc1d too
}

type registryGateTransport func(*http.Request) (*http.Response, error)

func (f registryGateTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func gateResponse() *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"results":[]}`))}
}

func TestProviderGateSurvivesRegistryReload(t *testing.T) {
	s, _ := deliveryService(t)
	doc := s.Doc()
	doc.Providers = map[string]config.Provider{"fh": {Kind: config.KindFinnhub, BaseURL: "https://reload-gate.test"}}
	s.setDoc(doc, s.Revision()+1)
	old := s.Registry().Finnhub.Client
	old.InProcess, old.MaxRetries = true, 0
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	old.HTTP.Transport = registryGateTransport(func(*http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return gateResponse(), nil
	})
	go func() { _, err := old.Get(context.Background(), "https://reload-gate.test", nil); done <- err }()
	<-entered
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	s.setDoc(doc, s.Revision()+1)
	next := s.Registry().Finnhub.Client
	next.InProcess, next.MaxRetries = true, 0
	var reached atomic.Bool
	next.HTTP.Transport = registryGateTransport(func(*http.Request) (*http.Response, error) {
		reached.Store(true)
		return gateResponse(), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, err := next.Get(ctx, "https://reload-gate.test", nil)
	if err == nil || reached.Load() {
		t.Error("new registry ignored the old registry's held slot")
	}
	if cap, active := registryGateState(next); cap != 1 || active != 1 {
		t.Errorf("gate capacity=%d active=%d", cap, active)
	}
}

func TestProviderGateSharedByDuplicateEntries(t *testing.T) {
	t.Setenv("MASAMONG_P2_DUPLICATE_TEST_KEY", "synthetic-test-credential")
	doc := config.Document{Providers: map[string]config.Provider{
		"first":  {Kind: config.KindLinkup, BaseURL: "http://127.0.0.1:19181/v1", CredentialRef: "env:MASAMONG_P2_DUPLICATE_TEST_KEY"},
		"second": {Kind: config.KindLinkup, BaseURL: "http://127.0.0.1:19181/other", CredentialRef: "env:MASAMONG_P2_DUPLICATE_TEST_KEY"},
	}}
	r := buildRegistry(doc, nil, time.Now)
	// The loopback address is intercepted by this transport; no socket or external API is used.
	entered, release, done := make(chan struct{}, 4), make(chan struct{}), make(chan error, 4)
	var active, most atomic.Int32
	r.Client.HTTP.Transport = registryGateTransport(func(*http.Request) (*http.Response, error) {
		n := active.Add(1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		entered <- struct{}{}
		<-release
		active.Add(-1)
		return gateResponse(), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, id := range []string{"first", "second"} {
		go func() { _, err := r.Search[id].Search(ctx, websearch.Query{Text: "q"}); done <- err }()
	}
	<-entered
	<-entered
	for _, id := range []string{"first", "second"} {
		go func() {
			wait, stop := context.WithTimeout(ctx, 60*time.Millisecond)
			defer stop()
			_, err := r.Search[id].Search(wait, websearch.Query{Text: "q"})
			done <- err
		}()
	}
	// Waiters must time out while the two original calls hold the capacity.
	for range 2 {
		select {
		case <-done:
		case <-entered:
			t.Error("duplicate entry exceeded the shared capacity")
			cancel()
			close(release)
			for range 4 {
				<-done
			}
			return
		}
	}
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	if most.Load() > 2 {
		t.Errorf("max requests in flight = %d", most.Load())
	}
}

func TestProviderGateUsesEffectiveOpenMeteoHost(t *testing.T) {
	t.Setenv("MASAMONG_P2_METEO_TEST_KEY", "synthetic-test-credential")
	provider := config.Provider{Kind: config.KindOpenMeteo, CredentialRef: "env:MASAMONG_P2_METEO_TEST_KEY"}
	first := buildRegistry(config.Document{Providers: map[string]config.Provider{"first": provider}}, nil, time.Now).OpenMeteo
	base := openmeteo.APIBase + "/"
	provider.BaseURL = base
	second := buildRegistry(config.Document{Providers: map[string]config.Provider{"second": provider}}, nil, time.Now).OpenMeteo
	if first.APIBase != second.APIBase {
		t.Fatal("adapters disagree on the effective host")
	}
	first.Client.InProcess = true
	first.Client.HTTP.Transport = registryGateTransport(func(*http.Request) (*http.Response, error) {
		if limit, active := registryGateState(second.Client); limit != 2 || active != 1 {
			t.Errorf("equivalent adapter hosts split the governor: capacity=%d active=%d", limit, active)
		}
		return gateResponse(), nil
	})
	if _, err := first.Client.Get(context.Background(), first.APIBase, nil); err != nil {
		t.Fatal(err)
	}
}
