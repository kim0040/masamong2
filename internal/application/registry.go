package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"masamong/internal/config"
	"masamong/internal/providers"
	"masamong/internal/providers/finnhub"
	"masamong/internal/providers/fx"
	"masamong/internal/providers/kakao"
	"masamong/internal/providers/kma"
	"masamong/internal/providers/openai"
	"masamong/internal/providers/openmeteo"
	"masamong/internal/providers/practice"
	"masamong/internal/providers/websearch"
)

// ProviderStatus is what doctor/GUI show for one configured provider. It never contains the secret.
type ProviderStatus struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	CredentialRef string `json:"credential_ref,omitempty"`
	Configured    bool   `json:"configured"`
	Builtin       bool   `json:"builtin,omitempty"` // added by the product, not in config (keyless defaults)
	Disabled      bool   `json:"disabled,omitempty"`
	KeyPresent    bool   `json:"key_present"` // a secret resolved (keyless providers work without one)
	Status        string `json:"status"`      // ready | unconfigured | error
	Error         string `json:"error,omitempty"`
	DailyLimit    int    `json:"daily_limit,omitempty"`
	UsedToday     int    `json:"used_today"`
}

// Registry holds the live adapters built from the current config revision (06장 §9).
type Registry struct {
	Client    *providers.Client
	Quota     *providers.Quota
	KMA       *kma.Adapter
	FX        *fx.Adapter
	Finnhub   *finnhub.Adapter
	OpenMeteo *openmeteo.Adapter
	LLM       map[string]*openai.Adapter
	// Search holds the web search services by provider name; Reader reads a page (web.read).
	Search                 map[string]websearch.Searcher
	Reader                 *websearch.Reader
	byID                   map[string]ProviderStatus
	kmaID, fxID, finnhubID string
	// fetchHosts remembers the hosts whose pages could not be read (search.web, page passages).
	fetchHosts *websearch.HostCache
	// the TinyFish provider whose key reads pages (TinyFish Fetch takes the search key); "" = none
	tinyfishID     string
	tinyfishClient *providers.Client
	tinyfishCred   providers.Credential
	fetchMu        sync.Mutex
	fetchers       map[string]websearch.Fetcher
	// Places is the Kakao Map place search of the first Kakao provider (places.search)
	Places                *kakao.Adapter
	placesID              string
	openMeteoID, readerID string
}

// fetchQuotaKey is the daily counter of pages read (one unit a page, not a call) and the health key of
// the page reader; it is not a provider of config.
const fetchQuotaKey = "fetch"

// tinyfishFetchBase is TinyFish Fetch's address (a test points it at a server).
var tinyfishFetchBase = websearch.FetchBaseURL

// pageFetcher is the service that reads the pages behind search results, as web_search.fetch says:
// TinyFish Fetch when it is chosen (or automatic) and a TinyFish key exists, else Jina Reader; nil for
// "off" or when neither is there. Built on first use, so the built-in reader is in by then.
func (r *Registry) pageFetcher(ws config.WebSearch) websearch.Fetcher {
	kind := ws.FetchKind(r.tinyfishID != "")
	if kind == "off" {
		return nil
	}
	r.fetchMu.Lock()
	defer r.fetchMu.Unlock()
	if r.fetchers == nil {
		r.fetchers = map[string]websearch.Fetcher{}
	}
	if kind == "tinyfish" && r.tinyfishID != "" {
		if f := r.fetchers["tinyfish"]; f != nil {
			return f
		}
		f := websearch.NewTinyFishFetcher(r.tinyfishClient, tinyfishFetchBase, r.tinyfishCred)
		r.fetchers["tinyfish"] = f
		return f
	}
	if r.Reader == nil {
		return nil
	}
	if f := r.fetchers["jina"]; f != nil {
		return f
	}
	f := websearch.NewJinaFetcher(r.Reader)
	r.fetchers["jina"] = f
	return f
}

// reserveN consumes up to n quota units of id and returns how many it got.
func (r *Registry) reserveN(id string, n int) int { return r.Quota.ReserveN(id, n) }

// searchHTTPTimeout is the HTTP limit of the search services' requests: above the longest wait for one
// service (searchDeadline, deep: Linkup's deep search took 41 s), so that limit, not this one, ends a call.
const searchHTTPTimeout = 50 * time.Second

// builtinReaderID names the keyless page reader added when config has none.
const builtinReaderID = "jina"

// addBuiltinReader gives every instance web.read: Jina Reader answers without a key.
func (r *Registry) addBuiltinReader() {
	if r.Reader != nil || r.readerID != "" {
		return
	}
	r.Reader, r.readerID = websearch.NewReader(gatedProvider(r.Client, config.Provider{Kind: config.KindJinaReader}, providers.Credential{}), "", providers.Credential{}), builtinReaderID
	r.byID[builtinReaderID] = ProviderStatus{ID: builtinReaderID, Kind: config.KindJinaReader, Configured: true, Status: "ready", Builtin: true}
}

// builtinOpenMeteoID names the keyless worldwide weather provider added when config has none.
const builtinOpenMeteoID = "openmeteo"

// addBuiltinOpenMeteo gives instances created before open_meteo existed worldwide weather anyway.
func (r *Registry) addBuiltinOpenMeteo() {
	if r.OpenMeteo != nil || r.openMeteoID != "" {
		return
	}
	r.OpenMeteo, r.openMeteoID = openmeteo.New(gatedProvider(r.Client, config.Provider{Kind: config.KindOpenMeteo}, providers.Credential{}), "", providers.Credential{}), builtinOpenMeteoID
	r.byID[builtinOpenMeteoID] = ProviderStatus{ID: builtinOpenMeteoID, Kind: config.KindOpenMeteo, Configured: true, Status: "ready", Builtin: true}
}

// providerGate is how many requests one provider may have in flight from this process (0: no bound
// beyond the turn's own, chat.ToolParallel). Hosts with tight documented limits get few, so the tool
// calls of a turn that run at once (and turns in other rooms) make fewer simultaneous requests:
// this is not a requests-per-second/minute limit. Finnhub's free tier
// (about 60 a minute), the KMA data.go.kr gateway (a per-second limit) and ExchangeRate-API (1,500 a
// month) one; Kakao Local, Open-Meteo (600 a minute) and Linkup (10 a second) two; Jina Reader one
// without a key (20 a minute), four with one (500 a minute). Model hosts are not bounded here: a 429
// is retried with its Retry-After (providers.Client).
func providerGate(kind string, keyed bool) int {
	switch kind {
	case config.KindFinnhub, config.KindKMA, config.KindFX:
		return 1
	case config.KindKakaoSearch, config.KindOpenMeteo, config.KindLinkup:
		return 2
	case config.KindJinaReader:
		if keyed {
			return 4
		}
		return 1
	}
	return 0
}

// gatedProvider uses the adapter's effective base host, and a canonical provider ID (kind), rather
// than a config entry's alias. Duplicate entries and new registries share the process governor.
func gatedProvider(client *providers.Client, p config.Provider, cred providers.Credential) *providers.Client {
	base := strings.TrimRight(p.BaseURL, "/")
	if p.Kind == config.KindFX {
		base = fx.OpenBase
		if cred.Present() {
			base = fx.KeyedBase
		}
	} else if base == "" {
		switch p.Kind {
		case config.KindFinnhub:
			base = finnhub.DefaultBaseURL
		case config.KindKMA:
			base = kma.DefaultBaseURL
		case config.KindJinaReader:
			base = websearch.ReaderBaseURL
		case config.KindOpenMeteo:
			base = openmeteo.APIBase
		default:
			base = websearch.DefaultBaseURL[p.Kind]
		}
	}
	if p.Kind == config.KindOpenMeteo && base == openmeteo.APIBase && cred.Present() {
		base = openmeteo.CustomerAPIBase
	}
	return client.WithProviderGate(p.Kind, base, cred, providerGate(p.Kind, cred.Present()))
}

func buildRegistry(doc config.Document, log *slog.Logger, now func() time.Time) *Registry {
	client := providers.NewClient(15*time.Second, log)
	limits := map[string]int{}
	for id, p := range doc.Providers {
		if p.DailyLimit > 0 {
			limits[id] = p.DailyLimit
		}
	}
	limits[fetchQuotaKey] = doc.WebSearch.FetchDailyPages()
	// the search services get their own copy of the client with the longer limit (it shares the
	// connections); everything else keeps the 15 s one
	searchClient := *client
	searchHTTP := *client.HTTP
	searchHTTP.Timeout = searchHTTPTimeout
	searchClient.HTTP = &searchHTTP
	r := &Registry{Client: client, Quota: providers.NewQuota(limits), fetchHosts: websearch.NewHostCache(), LLM: map[string]*openai.Adapter{}, Search: map[string]websearch.Searcher{}, byID: map[string]ProviderStatus{}}
	r.Quota.Now = now
	for id, p := range doc.Providers {
		st := ProviderStatus{ID: id, Kind: p.Kind, CredentialRef: p.CredentialRef, DailyLimit: p.DailyLimit}
		if p.Disabled {
			st.Status, st.Disabled = "disabled", true
			r.byID[id] = st
			switch p.Kind {
			case config.KindOpenMeteo:
				r.openMeteoID = id // an explicit, switched-off entry also keeps the built-in one away
			case config.KindJinaReader:
				r.readerID = id
			}
			continue
		}
		cred, err := providers.Resolve(p.CredentialRef)
		if err != nil {
			st.Status, st.Error = "error", err.Error()
			r.byID[id] = st
			continue
		}
		st.Configured = cred.Present()
		st.KeyPresent = cred.Present()
		st.Status = "unconfigured"
		if st.Configured {
			st.Status = "ready"
		}
		gated := gatedProvider(client, p, cred)
		switch p.Kind {
		case config.KindKMA:
			a := kma.New(gated, p.BaseURL, cred)
			a.Now = now
			r.KMA, r.kmaID = a, id
		case config.KindFX:
			r.FX, r.fxID = fx.New(gated, cred), id
			st.Configured, st.Status = true, "ready" // the open endpoint works without a key (daily data)
		case config.KindFinnhub:
			r.Finnhub, r.finnhubID = finnhub.New(gated, p.BaseURL, cred), id
		case config.KindKakaoSearch, config.KindTavily, config.KindLinkup, config.KindExa, config.KindSerper, config.KindBrave, config.KindTinyFish:
			if sr, err := websearch.New(p.Kind, gatedProvider(&searchClient, p, cred), p.BaseURL, cred); err == nil {
				r.Search[id] = sr
			}
			if p.Kind == config.KindTinyFish && cred.Present() && (r.tinyfishID == "" || id < r.tinyfishID) {
				r.tinyfishID, r.tinyfishClient, r.tinyfishCred = id, client, cred
			}
			// the same Kakao REST key also searches Kakao Map (places.search)
			if p.Kind == config.KindKakaoSearch && (r.Places == nil || !r.Places.Configured()) {
				r.Places, r.placesID = kakao.New(gated, p.BaseURL, cred), id
			}
		case config.KindJinaReader:
			r.Reader, r.readerID = websearch.NewReader(gated, p.BaseURL, cred), id
			st.Configured, st.Status = true, "ready" // keyless; a key raises the rate limit
		case config.KindOpenMeteo:
			r.OpenMeteo, r.openMeteoID = openmeteo.New(gated, p.BaseURL, cred), id
			st.Configured, st.Status = true, "ready" // keyless; a key switches to the commercial endpoints
		case config.KindOpenAI:
			r.LLM[id] = newLLMAdapter(client, p, cred)
		case config.KindFake:
			st.Configured, st.Status = true, "ready"
		case config.KindPractice:
			r.LLM[id] = practiceAdapter(p.BaseURL, doc.I18n.DefaultLocale, 15*time.Second, log)
			st.Configured, st.Status = true, "ready"
		}
		r.byID[id] = st
	}
	return r
}

func (r *Registry) Statuses() []ProviderStatus {
	used := r.Quota.Snapshot()
	out := make([]ProviderStatus, 0, len(r.byID))
	for _, st := range r.byID {
		st.UsedToday = used[st.ID]
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *Registry) Status(id string) (ProviderStatus, bool) {
	st, ok := r.byID[id]
	st.UsedToday = r.Quota.Snapshot()[id]
	return st, ok
}

var errQuota = errors.New("daily quota reached")

// reserve consumes one quota unit or returns errQuota. Features call it before every live request.
func (r *Registry) reserve(id string) error {
	if id == "" {
		return nil
	}
	used, limit, ok := r.Quota.Reserve(id)
	if !ok {
		return fmt.Errorf("%w: %s %d/%d", errQuota, id, used, limit)
	}
	return nil
}

// Probe makes the cheapest real request for provider id so 'providers test' proves the credential works.
func (r *Registry) Probe(ctx context.Context, id string) (map[string]any, error) {
	st, ok := r.byID[id]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", id)
	}
	if err := r.reserve(id); err != nil {
		return nil, err
	}
	start := time.Now()
	out := map[string]any{"id": id, "kind": st.Kind}
	var err error
	switch st.Kind {
	case config.KindKMA:
		var w []kma.Warning
		w, err = r.KMA.ActiveWarnings(ctx)
		out["active_warnings"] = len(w)
	case config.KindFX:
		var rates fx.Rates
		rates, err = r.FX.Latest(ctx, "USD")
		out["rates"] = len(rates.Rates)
		out["source"] = rates.Source
	case config.KindFinnhub:
		var q finnhub.Quote
		q, err = r.Finnhub.Quote(ctx, "AAPL")
		out["sample_symbol"] = q.Symbol
	case config.KindKakaoSearch, config.KindTavily, config.KindLinkup, config.KindExa, config.KindSerper, config.KindBrave, config.KindTinyFish:
		var res websearch.Response
		res, err = r.Search[id].Search(ctx, websearch.Query{Text: "weather", Max: 1})
		out["documents"] = len(res.Results)
	case config.KindJinaReader:
		var p websearch.Page
		p, err = r.Reader.Read(ctx, "https://example.com", 200)
		out["sample_page"] = p.Title
	case config.KindOpenAI, config.KindPractice:
		// OpenRouter lists models without a key, so its key is checked on its own first; elsewhere the
		// list itself needs the key and one request proves both
		a := r.LLM[id]
		if a.API == openai.APIOpenRouter {
			err = a.CheckKey(ctx)
		}
		if err == nil {
			var ms []openai.Model
			ms, err = a.ListModels(ctx)
			out["models"] = len(ms)
			if errors.Is(err, openai.ErrNoModelList) {
				// an Anthropic-format host without a model list: not a failure, the operator types the model
				// ID; the key is proven by the first answer instead
				out["model_list"], out["key_checked"] = "unavailable", false
				err = nil
			}
		}
	case config.KindOpenMeteo:
		var p openmeteo.Place
		p, err = r.OpenMeteo.Geocode(ctx, "Seoul", "en")
		out["sample_place"] = p.Name
	case config.KindFake:
		out["fake"] = true
	}
	out["latency_ms"] = time.Since(start).Milliseconds()
	return out, err
}

// newLLMAdapter is the adapter for an openai_compat provider: its host family from config or its
// address, and OpenRouter's app attribution headers only where OpenRouter reads them.
func newLLMAdapter(client *providers.Client, p config.Provider, cred providers.Credential) *openai.Adapter {
	a := openai.New(client, p.BaseURL, cred)
	a.API = openai.ResolveAPI(p.API, p.BaseURL)
	if a.API == openai.APIOpenRouter {
		a.Headers = map[string]string{"HTTP-Referer": "https://github.com/kim0040/masamong2", "X-Title": "masamong"}
		a.ByokOnly = p.ByokOnly
	}
	return a
}

// practiceAdapter is an OpenAI-compatible adapter whose requests never leave the process.
func practiceAdapter(baseURL, locale string, timeout time.Duration, log *slog.Logger) *openai.Adapter {
	client := providers.NewClient(timeout, log)
	client.HTTP.Transport = practice.Transport{Locale: locale}
	client.InProcess = true
	client.MaxRetries = 0
	if baseURL == "" {
		baseURL = practice.BaseURL
	}
	return openai.New(client, baseURL, providers.Offline())
}
