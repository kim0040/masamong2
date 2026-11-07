package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/chat"
	"masamong/internal/errcode"
	"masamong/internal/features/poll"
	"masamong/internal/providers"
	"masamong/internal/providers/decode"
	"masamong/internal/providers/finnhub"
	"masamong/internal/providers/fx"
	"masamong/internal/providers/kakao"
	"masamong/internal/providers/kma"
	"masamong/internal/providers/openai"
	"masamong/internal/providers/openmeteo"
	"masamong/internal/providers/websearch"
)

// providerFail maps adapter errors to stable codes. Transport failures are retryable; validation and
// auth failures are not. Every branch says what happened rather than folding into "error" (05장).
func (s *Service) providerFail(req action.Request, provider string, err error) action.Result {
	s.noteProvider(provider, err)
	return s.providerResult(req, provider, err)
}

// providerResult is the failed result for err without noting it in the provider's health: for an error
// that is not the provider's (the turn ended, our own time limit).
func (s *Service) providerResult(req action.Request, provider string, err error) action.Result {
	var pe *providers.Error
	var ae *openai.APIError
	args := map[string]any{"provider": provider}
	switch {
	case errors.Is(err, providers.ErrNoCredential):
		return action.Fail(req, errcode.ProviderUnconfigured, "provider.unconfigured", s.msg(req, "provider.unconfigured", "provider", provider), args)
	case errors.Is(err, errQuota):
		args["detail"] = err.Error()
		return action.Fail(req, errcode.BudgetExceeded, "generic.error", err.Error(), args)
	case errors.Is(err, decode.ErrHTMLResponse), errors.Is(err, decode.ErrUnexpected):
		args["detail"] = err.Error()
		return action.Fail(req, errcode.ProviderFormat, "generic.error", "provider returned an unexpected format: "+err.Error(), args)
	case errors.Is(err, websearch.ErrNotPublic):
		return action.Fail(req, errcode.InvalidInput, "generic.error", err.Error(), args)
	case errors.Is(err, decode.ErrKMAService), errors.Is(err, decode.ErrKMATextError), errors.Is(err, fx.ErrProvider), errors.Is(err, openai.ErrAuth), errors.Is(err, websearch.ErrAuth), errors.Is(err, finnhub.ErrForbidden), errors.Is(err, openmeteo.ErrProvider):
		args["detail"] = err.Error()
		return action.Fail(req, errcode.ProviderRejected, "generic.error", err.Error(), args)
	case errors.Is(err, finnhub.ErrRateLimited), errors.Is(err, kakao.ErrQuotaExceeded):
		res := action.Fail(req, errcode.ProviderRateLimited, "generic.error", err.Error(), args)
		res.Error.Retryable = true
		return res
	case errors.As(err, &ae):
		args["type"], args["code"] = ae.Type, ae.Code
		return action.Fail(req, errcode.ProviderRejected, "generic.error", ae.Message, args)
	case errors.As(err, &pe):
		args["kind"], args["status"], args["attempts"], args["url"] = pe.Kind, pe.Status, pe.Attempts, pe.URL
		res := action.Fail(req, errcode.ProviderUnavailable, "generic.error", err.Error(), args)
		res.Error.Retryable = pe.Retryable()
		return res
	}
	args["detail"] = err.Error()
	return action.Fail(req, errcode.ProviderError, "generic.error", err.Error(), args)
}

func fptr(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

// ---------------------------------------------------------------------------
// Weather: KMA (초단기실황) for Korean regions today, Open-Meteo for everywhere else and forecasts
// ---------------------------------------------------------------------------

// weather answers with where its place came from (region_source): the message, the person's own
// setting, or a default. A defaulted place is an assumption nobody made out loud, so the answer also
// carries assumed_region and says it (principle 7: the previous bot filled Seoul in silently and lost
// people's trust that way).
func (s *Service) weather(ctx context.Context, req action.Request) action.Result {
	src := regionSource(inputMap(req))
	res := s.weatherFor(ctx, req)
	if res.Output != nil {
		res.Output["region_source"] = src
		if src == "default" {
			place, _ := res.Output["region"].(string)
			if place == "" {
				place = kma.DefaultRegion
			}
			res.Output["assumed_region"] = place
		}
	}
	return res
}

// regionSource is where a weather request's place came from: "message" (the caller named it), "user"
// (the person's saved place, applyUserDefaults) or "default" (none given: the tool's configured default
// or the built-in one).
func regionSource(in map[string]any) string {
	if strings.TrimSpace(strVal(in, "region", "")) == "" {
		return "default"
	}
	switch src := strVal(in, "region_source", ""); src {
	case "user", "default":
		return src
	}
	return "message"
}

func (s *Service) weatherFor(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	region := strings.TrimSpace(strVal(in, "region", kma.DefaultRegion)) // override: tools.weather.query.defaults.region
	if region == "" {
		region = kma.DefaultRegion
	}
	day := intVal(in, "day_offset", 0)
	if day < 0 || day >= openmeteo.MaxForecastDays {
		return action.Fail(req, errcode.InvalidInput, "generic.error", fmt.Sprintf("day_offset must be 0–%d", openmeteo.MaxForecastDays-1), nil)
	}
	source := strVal(in, "source", "auto") // auto | kma | global
	g, gridErr := kma.LookupGrid(region)
	if nx, ny := intVal(in, "nx", 0), intVal(in, "ny", 0); nx > 0 && ny > 0 {
		g, gridErr, source = kma.Grid{Name: region, NX: nx, NY: ny}, nil, "kma"
	}
	if fx := strVal(in, "fixture", ""); fx != "" {
		if gridErr != nil {
			g = kma.Grid{Name: region}
		}
		return s.weatherFixture(req, g, fx) // an explicit offline sample, never a live call
	}
	kmaReady := s.Registry().KMA != nil && s.Registry().KMA.Configured()
	if gridErr != nil && (source == "kma" || s.Registry().OpenMeteo == nil) {
		// only KMA could answer, and it does not know this name
		return action.Fail(req, errcode.InvalidInput, "generic.error", gridErr.Error(), map[string]any{"known_regions": kma.Regions()})
	}
	if source == "kma" || (source == "auto" && gridErr == nil && day == 0 && kmaReady) {
		if !kmaReady {
			if s.Registry().OpenMeteo == nil || source == "kma" {
				return action.OK(req, map[string]any{
					"region": g.Name, "status": "unconfigured", "live": false, "source": "none",
					"message": s.msg(req, "provider.unconfigured", "provider", "kma"),
				})
			}
		} else {
			res := s.weatherKMA(ctx, req, g)
			// KMA outages fall through to the worldwide source rather than leaving the room without weather
			if res.OK || source == "kma" || s.Registry().OpenMeteo == nil {
				return res
			}
			out := s.weatherGlobal(ctx, req, region, &g, day)
			if !out.OK {
				return res // both failed: report the primary source's error
			}
			out.Output["fallback_from"] = "kma"
			return out
		}
	}
	if s.Registry().OpenMeteo == nil {
		return action.OK(req, map[string]any{"region": region, "status": "unconfigured", "live": false, "source": "none",
			"message": s.msg(req, "provider.unconfigured", "provider", "open_meteo")})
	}
	if gridErr == nil {
		return s.weatherGlobal(ctx, req, region, &g, day)
	}
	return s.weatherGlobal(ctx, req, region, nil, day)
}

func (s *Service) weatherKMA(ctx context.Context, req action.Request, g kma.Grid) action.Result {
	if err := s.Registry().reserve(s.Registry().kmaID); err != nil {
		return s.providerFail(req, s.Registry().kmaID, err)
	}
	obs, err := s.Registry().KMA.Current(ctx, g)
	if errors.Is(err, decode.ErrKMANoData) {
		return action.OK(req, map[string]any{
			"region": g.Name, "status": "empty", "live": true, "source": "kma",
			"note": "no_observation_for_slot_not_safety", "base_date": obs.BaseDate, "base_time": obs.BaseTime,
		})
	}
	if err != nil {
		return s.providerFail(req, s.Registry().kmaID, err)
	}
	return action.OK(req, map[string]any{
		"region": g.Name, "nx": g.NX, "ny": g.NY, "status": "ok", "live": true, "source": "kma", "day_offset": 0,
		"observed_at": obs.ObservedAt.Format(time.RFC3339), "base_date": obs.BaseDate, "base_time": obs.BaseTime,
		"temp_c": fptr(obs.TempC), "humidity_pct": fptr(obs.Humidity), "wind_ms": fptr(obs.WindSpeed),
		"wind_deg": fptr(obs.WindDeg), "rain_1h_mm": fptr(obs.Rain1h), "precip": obs.Precip,
		"missing_categories": obs.MissingCodes, "envelope": obs.Variant,
		"message": s.msg(req, "weather.ok"),
	})
}

// geocodeCache keeps place lookups for a day: city names do not move, and each lookup is a call. People type
// the place, so it is bounded (geocodeCacheMax) and drops what is older than a day instead of keeping every
// spelling ever asked.
const geocodeCacheMax = 1024

var geocodeCache = newTTLCache[openmeteo.Place](geocodeCacheMax, 24*time.Hour) // "lang|name" → place

// weatherGlobal answers from Open-Meteo. A Korean region from the KMA table (grid) is placed by the
// grid's own coordinates: Open-Meteo's geocoder misses or misplaces many Korean names.
func (s *Service) weatherGlobal(ctx context.Context, req action.Request, region string, grid *kma.Grid, day int) action.Result {
	lang := s.I18n.Resolve(req.Locale)
	if lang == "" {
		lang = s.Doc().I18n.DefaultLocale
	}
	key := lang + "|" + strings.ToLower(region)
	var place openmeteo.Place
	if grid != nil {
		lat, lon := grid.LatLon()
		place = openmeteo.Place{Name: grid.Name, CountryCode: "KR", Latitude: lat, Longitude: lon, Timezone: "Asia/Seoul"}
	} else if cached, _, ok := geocodeCache.get(key, s.Clock.Now()); ok {
		place = cached
	} else {
		if err := s.Registry().reserve(s.Registry().openMeteoID); err != nil {
			return s.providerFail(req, s.Registry().openMeteoID, err)
		}
		p, err := s.Registry().OpenMeteo.Geocode(ctx, region, lang)
		if errors.Is(err, openmeteo.ErrNotFound) {
			return action.Fail(req, errcode.InvalidInput, "weather.place_unknown", s.msg(req, "weather.place_unknown", "region", clip(region, 80)),
				map[string]any{"region": clip(region, 80)})
		}
		if err != nil {
			return s.providerFail(req, s.Registry().openMeteoID, err)
		}
		place = p
		geocodeCache.put(key, p, s.Clock.Now())
	}
	if err := s.Registry().reserve(s.Registry().openMeteoID); err != nil {
		return s.providerFail(req, s.Registry().openMeteoID, err)
	}
	w, err := s.Registry().OpenMeteo.Forecast(ctx, place.Latitude, place.Longitude, day+1)
	if err != nil {
		return s.providerFail(req, s.Registry().openMeteoID, err)
	}
	name := place.Name
	if place.Country != "" && place.CountryCode != "KR" {
		name += ", " + place.Country
	}
	out := map[string]any{
		"region": name, "place": place.Name, "admin1": place.Admin1, "country": place.Country, "country_code": place.CountryCode,
		"latitude": place.Latitude, "longitude": place.Longitude, "timezone": w.Timezone,
		"status": "ok", "live": true, "source": "open_meteo", "day_offset": day,
		"observed_at": w.ObservedAt.Format(time.RFC3339), "temp_c": fptr(w.TempC), "feels_like_c": fptr(w.FeelsC),
		"humidity_pct": fptr(w.Humidity), "wind_ms": fptr(w.WindMS), "wind_deg": fptr(w.WindDeg), "precip_mm": fptr(w.PrecipMM),
		"condition": w.Condition, "precip": openmeteo.Precip(w.Condition), "days": w.Days,
		"message": s.msg(req, "weather.global.ok"),
	}
	if day < len(w.Days) {
		d := w.Days[day]
		out["forecast"] = map[string]any{"date": d.Date, "max_c": fptr(d.MaxC), "min_c": fptr(d.MinC), "precip_prob_pct": fptr(d.PrecipProb), "condition": d.Condition}
	}
	return action.OK(req, out)
}

func (s *Service) weatherFixture(req action.Request, g kma.Grid, fixture string) action.Result {
	base := map[string]any{"region": g.Name, "live": false, "source": "fixture", "message": s.msg(req, "weather.fixture")}
	switch fixture {
	case "stale":
		base["status"], base["observed_at"] = "stale", "2020-01-01T00:00:00Z"
	case "empty":
		base["status"], base["note"] = "empty", "no_observation_not_safety"
	case "error":
		return action.Fail(req, errcode.ProviderUnavailable, "generic.error", "fixture provider error", map[string]any{"region": g.Name, "provider": "fixture"})
	default:
		base["status"], base["temp_c"], base["humidity_pct"], base["precip"] = "ok", 18.0, 55.0, "none"
		base["observed_at"] = s.Clock.Now().Format(time.RFC3339)
	}
	return action.OK(req, base)
}

// ---------------------------------------------------------------------------
// Disasters (earthquake notices + active warnings)
// ---------------------------------------------------------------------------

func (s *Service) disasters(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	if s.Registry().KMA == nil || !s.Registry().KMA.Configured() {
		switch strVal(in, "fixture", "") {
		case "unconfirmed":
			return action.OK(req, map[string]any{"status": "unconfirmed", "live": false, "source": "fixture", "events": []any{}, "note": "latest_unconfirmed_not_no_disaster"})
		case "partial":
			return action.OK(req, map[string]any{"status": "partial", "live": false, "source": "fixture", "events": []any{map[string]any{"id": "eq-1", "kind": "earthquake", "deduped": true}}})
		case "none":
			return action.OK(req, map[string]any{"status": "none", "live": false, "source": "fixture", "events": []any{}, "note": "no_active_alert_not_safety_claim"})
		}
		return action.OK(req, map[string]any{"status": "unconfigured", "live": false, "source": "none", "message": s.msg(req, "provider.unconfigured", "provider", "kma")})
	}
	out := map[string]any{"live": true, "source": "kma"}
	status := "none"
	if err := s.Registry().reserve(s.Registry().kmaID); err != nil {
		return s.providerFail(req, s.Registry().kmaID, err)
	}
	quakes, qerr := s.Registry().KMA.Earthquakes(ctx, intVal(in, "days", 3))
	minDomestic, minOverseas := 2.0, 5.0
	events := []any{}
	for _, q := range quakes {
		if q.NoDomesticImpact {
			continue
		}
		mag := 0.0
		if q.Magnitude != nil {
			mag = *q.Magnitude
		}
		if (q.Domestic && mag < minDomestic) || (!q.Domestic && mag < minOverseas) {
			continue
		}
		events = append(events, map[string]any{
			"kind": "earthquake", "time": q.EventTime.Format(time.RFC3339), "location": q.Location,
			"magnitude": fptr(q.Magnitude), "depth_km": fptr(q.DepthKM), "domestic": q.Domestic, "remark": q.Remark,
		})
	}
	out["earthquakes"] = events
	if qerr != nil {
		out["earthquake_status"] = "unconfirmed"
		out["earthquake_error"] = qerr.Error()
		status = "unconfirmed"
	} else {
		out["earthquake_status"] = "ok"
		if len(events) > 0 {
			status = "active"
		}
	}
	if err := s.Registry().reserve(s.Registry().kmaID); err != nil {
		return s.providerFail(req, s.Registry().kmaID, err)
	}
	warns, werr := s.Registry().KMA.ActiveWarnings(ctx)
	wl := []any{}
	for _, w := range warns {
		wl = append(wl, map[string]any{"region": w.Region, "parent": w.Parent, "kind": w.KindName, "level": w.LevelName, "effective": w.Effective})
	}
	out["warnings"] = wl
	if werr != nil {
		out["warning_status"] = "unconfirmed"
		out["warning_error"] = werr.Error()
		if status == "none" {
			status = "unconfirmed"
		}
	} else {
		out["warning_status"] = "ok"
		if len(wl) > 0 {
			status = "active"
		}
	}
	out["status"] = status
	if status == "none" {
		out["note"] = "no_active_alert_not_safety_claim"
	}
	return action.OK(req, out)
}

// ---------------------------------------------------------------------------
// Finance (Finnhub quote, ExchangeRate-API)
// ---------------------------------------------------------------------------

func (s *Service) finance(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	if req.Action == "finance.fx" {
		if s.Registry().FX == nil {
			return action.OK(req, map[string]any{"status": "unconfigured", "live": false, "message": s.msg(req, "provider.unconfigured", "provider", "exchangerate")})
		}
		if err := s.Registry().reserve(s.Registry().fxID); err != nil {
			return s.providerFail(req, s.Registry().fxID, err)
		}
		base := strVal(in, "base", "USD")
		rates, err := s.Registry().FX.Latest(ctx, base)
		if err != nil {
			return s.providerFail(req, s.Registry().fxID, err)
		}
		want := strSlice(in, "symbols")
		if len(want) == 0 {
			want = []string{"KRW", "JPY", "EUR", "CNY", "GBP"}
		}
		sel := map[string]any{}
		for _, c := range want {
			if v, ok := rates.Rates[strings.ToUpper(c)]; ok {
				sel[strings.ToUpper(c)] = v
			}
		}
		out := map[string]any{"status": "ok", "live": true, "source": "exchangerate-api/" + rates.Source, "base": rates.Base, "rates": sel, "rate_count": len(rates.Rates)}
		if !rates.Updated.IsZero() {
			out["updated_at"] = rates.Updated.Format(time.RFC3339)
		}
		if !rates.NextUpdate.IsZero() {
			out["next_update_at"] = rates.NextUpdate.Format(time.RFC3339)
		}
		return action.OK(req, out)
	}
	symbol := strings.ToUpper(strVal(in, "symbol", ""))
	if symbol == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "symbol required", nil)
	}
	if s.Registry().Finnhub == nil || !s.Registry().Finnhub.Configured() {
		if strVal(in, "fixture", "") == "fx_fail" {
			return action.OK(req, map[string]any{"status": "partial", "live": false, "source": "fixture", "quote": map[string]any{"symbol": symbol, "price": "100.00", "currency": "USD"}, "fx": nil, "note": "quote_without_conversion"})
		}
		if res, ok := s.quoteBySearch(ctx, req, symbol, "no_quote_provider"); ok {
			return res
		}
		return action.OK(req, map[string]any{"status": "unconfigured", "live": false, "symbol": symbol, "message": s.msg(req, "provider.unconfigured", "provider", "finnhub")})
	}
	if err := s.Registry().reserve(s.Registry().finnhubID); err != nil {
		return s.providerFail(req, s.Registry().finnhubID, err)
	}
	q, err := s.Registry().Finnhub.Quote(ctx, symbol)
	if errors.Is(err, finnhub.ErrUnknownSymbol) || errors.Is(err, finnhub.ErrForbidden) {
		// Korean listings (005930.KS), indices (KOSPI) and names ("삼성전자") are outside Finnhub's
		// free plan: the answer comes from a web search, said to be one
		why := "symbol_not_on_plan"
		if errors.Is(err, finnhub.ErrUnknownSymbol) {
			why = "unknown_symbol"
		}
		if res, ok := s.quoteBySearch(ctx, req, symbol, why); ok {
			return res
		}
	}
	if errors.Is(err, finnhub.ErrUnknownSymbol) {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "unknown symbol "+symbol, map[string]any{"symbol": symbol})
	}
	if err != nil {
		return s.providerFail(req, s.Registry().finnhubID, err)
	}
	out := map[string]any{
		"status": "ok", "live": true, "source": "finnhub", "symbol": q.Symbol, "currency": "USD",
		"price": fmt.Sprintf("%.2f", q.Current), "change": fmt.Sprintf("%.2f", q.Change), "change_pct": fmt.Sprintf("%.2f", q.ChangePercent),
		"high": fmt.Sprintf("%.2f", q.High), "low": fmt.Sprintf("%.2f", q.Low), "open": fmt.Sprintf("%.2f", q.Open), "prev_close": fmt.Sprintf("%.2f", q.PrevClose),
	}
	if !q.At.IsZero() {
		out["at"] = q.At.Format(time.RFC3339)
	}
	// Optional KRW conversion; a failed FX call degrades to partial rather than failing the quote (05장 부분 실패).
	if boolVal(in, "krw") && s.Registry().FX != nil {
		if err := s.Registry().reserve(s.Registry().fxID); err == nil {
			if rates, ferr := s.Registry().FX.Latest(ctx, "USD"); ferr == nil {
				if krw, ok := rates.Rates["KRW"]; ok {
					out["price_krw"] = fmt.Sprintf("%.0f", q.Current*krw)
					out["fx_rate_krw"] = krw
				}
			} else {
				out["status"], out["fx"], out["note"] = "partial", nil, "quote_without_conversion"
			}
		}
	}
	return action.OK(req, out)
}

// ---------------------------------------------------------------------------
// Web search (Tavily, Linkup, Exa, Serper, Kakao) and page reading (Jina Reader)
// ---------------------------------------------------------------------------

// recordSpend adds a paid call that is not a model call (a search service that reports its cost) to
// the month's spending.
func (s *Service) recordSpend(kind string, usd float64) {
	if eng, err := s.MemoryEngine(); err == nil {
		_ = eng.Store.AddSpend(kind, usd, 1)
	}
}

// searchOrder is the search services search.web tries, first to last: web_search.order, else every
// search provider in the built-in kind order (websearch.Kinds). Only those with a key are listed.
func (s *Service) searchOrder() []string {
	doc, reg := s.Doc(), s.Registry()
	ids := doc.WebSearch.Order
	if len(ids) == 0 {
		for _, kind := range websearch.Kinds {
			var same []string
			for id, p := range doc.Providers {
				if p.Kind == kind && !p.Disabled {
					same = append(same, id)
				}
			}
			sort.Strings(same)
			ids = append(ids, same...)
		}
	}
	var out []string
	for _, id := range ids {
		if sr, ok := reg.Search[id]; ok && sr.Configured() {
			out = append(out, id)
		}
	}
	return out
}

// placesSearch finds places on Kakao Map ("강남역 맛집", "홍대 카페"): name, category, road address,
// phone and the map page. The previous bot answered such questions from a place search (57 times in
// its logs); a web search only finds articles about places. Input: query, size (1–15, default 5).
func (s *Service) placesSearch(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	query := strings.TrimSpace(strVal(in, "query", ""))
	if query == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "query required", nil)
	}
	r := s.Registry()
	if r.Places == nil || !r.Places.Configured() {
		return action.OK(req, map[string]any{"status": "unconfigured", "live": false, "query": query, "message": s.msg(req, "provider.unconfigured", "provider", "Kakao")})
	}
	if err := r.reserve(r.placesID); err != nil {
		return s.providerFail(req, r.placesID, err)
	}
	found, err := r.Places.Places(ctx, query, intVal(in, "size", 5))
	s.noteProvider(r.placesID, err)
	if err != nil {
		return s.providerFail(req, r.placesID, err)
	}
	searched := query
	if len(found.Places) == 0 {
		// Kakao matches names, categories and areas, not taste: "홍대 분위기 좋은 카페" finds nothing while
		// "홍대 카페" finds plenty. Each other word is tried with the kind of shop (at most two more calls)
		// and a try counts only when Kakao reads an area from it, so "맛있는 홍대 카페" ends on "홍대 카페",
		// not on nationwide "맛있는 카페".
		for _, loose := range looserPlaceQueries(query, 2) {
			more, err := r.Places.Places(ctx, loose, intVal(in, "size", 5))
			if err != nil {
				break
			}
			if len(more.Places) > 0 && more.Region != "" {
				found, searched = more, loose
				break
			}
		}
	}
	list := make([]map[string]any, 0, len(found.Places))
	for _, p := range found.Places {
		item := map[string]any{"name": p.Name, "category": p.Category, "address": p.RoadAddress, "url": p.URL}
		if p.RoadAddress == "" {
			item["address"] = p.Address
		}
		if p.Phone != "" {
			item["phone"] = p.Phone
		}
		list = append(list, item)
	}
	status := "ok"
	switch {
	case len(list) == 0:
		status = "no_results"
	case found.Region == "" && !namesPlace(searched, list):
		// "근처 맛집" with no area: Kakao answers with famous places from anywhere in the country, which
		// are no use to someone asking about their neighbourhood. The answer asks where instead.
		status, list = "no_area", []map[string]any{}
	}
	out := map[string]any{"status": status, "live": true, "source": "kakao_map", "provider": r.placesID, "query": query, "places": list}
	if status == "no_area" {
		out["message"] = s.msg(req, "places.no_area")
	}
	if found.Region != "" {
		out["area"] = found.Region
	}
	if searched != query {
		out["searched_as"] = searched
	}
	return action.OK(req, out)
}

// namesPlace reports whether the search names a particular place rather than leaving the area out: a
// word of the query other than the last is in the name ("스타벅스 리저브", "롯데월드") or the address
// ("성수동 카페" when Kakao picked no region but the results are in 성수동) of most results. One shop
// that happens to carry the word ("동네 카페" → a "동네커피" among nationwide picks) is not enough.
func namesPlace(query string, list []map[string]any) bool {
	words := strings.Fields(query)
	if len(words) > 1 {
		words = words[:len(words)-1] // the kind of shop ("맛집", "카페") is in plenty of names
	}
	for _, w := range words {
		if len([]rune(w)) < 2 {
			continue
		}
		hits := 0
		for _, p := range list {
			if strings.Contains(fmt.Sprint(p["name"]), w) || strings.Contains(fmt.Sprint(p["address"]), w) {
				hits++
			}
		}
		if hits*2 > len(list) {
			return true
		}
	}
	return false
}

// looserPlaceQueries pairs each word of a search of three or more words with its last word, the kind of
// shop (Korean puts the head noun last: "홍대 분위기 좋은 카페"), at most n of them; none for shorter ones.
func looserPlaceQueries(q string, n int) []string {
	f := strings.Fields(q)
	if len(f) < 3 {
		return nil
	}
	var out []string
	for _, w := range f[:len(f)-1] {
		if len(out) == n {
			break
		}
		if len([]rune(w)) >= 2 {
			out = append(out, w+" "+f[len(f)-1])
		}
	}
	return out
}

// quoteBySearch answers a price the quote service cannot give from a web search (the previous bot
// settled on the same for Korean markets). The result says it is a search, not a live quote, so the
// answer names its sources and does not present a snippet as the current price. It counts toward
// search.web's own limits; without a search service it reports false.
func (s *Service) quoteBySearch(ctx context.Context, req action.Request, symbol, why string) (action.Result, bool) {
	if len(s.searchOrder()) == 0 {
		return action.Result{}, false
	}
	name := strings.TrimSuffix(strings.TrimSuffix(symbol, ".KS"), ".KQ")
	query := name + " stock price"
	if strings.HasPrefix(strings.ToLower(req.Locale), "ko") || strings.HasPrefix(strings.ToLower(s.Doc().I18n.DefaultLocale), "ko") || name != symbol {
		query = name + " 주가"
	}
	raw, _ := json.Marshal(map[string]any{"query": query, "size": 5, "fresh": "day"})
	res := s.Execute(ctx, action.Request{Action: "search.web", Input: raw, Locale: req.Locale, RequestID: req.RequestID, Invoker: req.Invoker})
	if !res.OK {
		return action.Result{}, false
	}
	out := map[string]any{"status": "ok", "live": false, "source": "search", "symbol": symbol, "note": "quote_from_search", "reason": why, "query": query}
	for k, v := range res.Output {
		if k == "results" || k == "answer" || k == "provider" {
			out[k] = v
		}
	}
	return action.OK(req, out), true
}

// searchDeadline is how long search.web waits for one service, by depth, before it moves to the next
// one (the whole call, retries included). Measured live on 2026-11-07, one request each:
//
//	quick/standard   Exa 0.4–1.8 s, Linkup fast and standard 0.7–1.6 s, Kakao under 0.12 s, TinyFish
//	                 0.2–2.5 s without a date filter (once 4.5 s, cold), but 6.5–8.2 s in 9 of 11 calls
//	                 with one (recency_minutes or after_date)
//	deep             Exa deep-lite 1.5–1.9 s; Linkup deep 6.9, 9.0, 9.3, 25.8 and 41.3 s (and a tool loop
//	                 that took 66 s)
//
// Before this limit a service that was slow held the turn: the 15 s HTTP limit (registry.go) cut each
// attempt and the client retried it twice, so one hung service cost 3 × 15 s plus the waits (46 s in the
// test of this file). quick and standard sit above the slowest date-filtered TinyFish search (8.2 s)
// with a little room, so a service that is merely slow still answers; deep is the HTTP limit itself, so
// a deep search that has not answered by then is handed on rather than waited for.
var searchDeadline = map[string]time.Duration{
	websearch.DepthQuick:    8 * time.Second,
	websearch.DepthStandard: 10 * time.Second,
	websearch.DepthDeep:     15 * time.Second,
}

// searchTimeout is searchDeadline for a depth ("" is standard).
func searchTimeout(depth string) time.Duration {
	if d, ok := searchDeadline[depth]; ok {
		return d
	}
	return searchDeadline[websearch.DepthStandard]
}

// search asks the search services in order; one that fails or finds nothing hands over to the next.
// The answer says which service found the pages (source/provider) and which were passed over.
// Input: query, size (1–10), fresh (day|week|month), depth (quick|standard|deep: speed against
// coverage, each service in its own words), topic (news: news articles where the service has a news
// search), provider (only that one).
func (s *Service) search(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	query := strings.TrimSpace(strVal(in, "query", ""))
	if query == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "query required", nil)
	}
	order := s.searchOrder()
	if want := strVal(in, "provider", ""); want != "" {
		if !slices.Contains(order, want) {
			return action.Fail(req, errcode.ProviderUnconfigured, "provider.unconfigured", s.msg(req, "provider.unconfigured", "provider", want), map[string]any{"provider": want, "usable": order})
		}
		order = []string{want}
	}
	if len(order) == 0 {
		return action.OK(req, map[string]any{"status": "unconfigured", "live": false, "query": query, "results": []any{},
			"message": s.msg(req, "search.unconfigured")})
	}
	depth, topic := strings.ToLower(strVal(in, "depth", "")), strings.ToLower(strVal(in, "topic", ""))
	if depth != "" && depth != websearch.DepthQuick && depth != websearch.DepthStandard && depth != websearch.DepthDeep {
		depth = "" // a word the services do not take: the standard search
	}
	if topic != websearch.TopicNews {
		topic = ""
	}
	size := 5
	if depth == websearch.DepthDeep {
		size = 8 // a deeper search is asked to look wider
	}
	fresh := strings.ToLower(strings.TrimSpace(strVal(in, "fresh", "")))
	switch fresh {
	case "day", "week", "month":
	case "hour", "hourly", "1h", "today", "24h", "1d", "now", "recent", "latest":
		fresh = "day" // within the day: the narrowest window the services take ("hour": Tavily answers 400 to it)
	default:
		fresh = "" // a word the schema does not offer and that names no span of days
	}
	q := websearch.Query{Text: query, Max: intVal(in, "size", size), Fresh: fresh, Depth: depth, Topic: topic}
	if topic == websearch.TopicNews && len(order) > 1 {
		// news: the services with a news search of their own first (dated articles), the rest after
		reg := s.Registry()
		later := func(id string) int {
			if websearch.NewsSearch(reg.Search[id].Kind()) {
				return 0
			}
			return 1
		}
		slices.SortStableFunc(order, func(a, b string) int { return later(a) - later(b) })
	}
	var tried []any
	var lastID string
	var lastErr error
	lastIsProviders := true // lastErr is the service's own answer (not a limit of ours or the end of the turn)
	for _, id := range order {
		if err := ctx.Err(); err != nil {
			// the turn has ended: the services left are not asked, and none of them is to blame
			lastID, lastErr, lastIsProviders = id, err, false
			break
		}
		if err := s.Registry().reserve(id); err != nil {
			tried = append(tried, map[string]any{"provider": id, "reason": "daily_limit"})
			lastID, lastErr = id, err
			continue
		}
		sr := s.Registry().Search[id]
		callCtx, cancel := context.WithTimeout(ctx, searchTimeout(depth))
		res, err := sr.Search(callCtx, q)
		ours := callCtx.Err() != nil         // our limit for the depth ran out or the turn ended: not the service's error
		timedOut := ours && ctx.Err() == nil // this service's limit, not the turn's
		cancel()
		if err == nil || !ours {
			s.noteProvider(id, err) // only what the service itself answered goes into its health
		}
		if err != nil {
			reason := "error"
			if timedOut {
				reason = "timeout"
			}
			tried = append(tried, map[string]any{"provider": id, "reason": reason, "detail": clip(err.Error(), 200)})
			lastID, lastErr, lastIsProviders = id, err, !ours
			continue
		}
		if res.CostUSD != nil {
			s.recordSpend("search", *res.CostUSD)
			chat.GuardFrom(ctx).Spend(*res.CostUSD)
			chat.GuardFrom(ctx).Paid(*res.CostUSD) // the turn's cost counts the fee too (Guard.Paid)
		}
		if len(res.Results) == 0 && len(tried)+1 < len(order) {
			tried = append(tried, map[string]any{"provider": id, "reason": "empty"})
			continue
		}
		items := make([]any, 0, len(res.Results))
		for _, r := range res.Results {
			items = append(items, map[string]any{"title": r.Title, "snippet": r.Snippet, "url": r.URL, "datetime": r.Published})
		}
		out := map[string]any{"status": "ok", "live": true, "source": sr.Kind(), "provider": id, "query": query, "results": items, "paid_provider_call": true}
		if depth != "" {
			out["depth"] = depth
		}
		if topic != "" {
			out["topic"] = topic
		}
		if len(items) == 0 {
			out["status"] = "empty"
		}
		if res.Answer != "" {
			out["answer"] = res.Answer
		}
		if len(tried) > 0 {
			out["passed_over"] = tried
		}
		return action.OK(req, out)
	}
	if !lastIsProviders {
		return s.providerResult(req, lastID, lastErr)
	}
	return s.providerFail(req, lastID, lastErr)
}

// webRead reads one public web page as text (Jina Reader). Input: url, max_chars (500–20000; the
// default fits the answer prompt's tool block, internal/chat BuildAnswerMessages).
func (s *Service) webRead(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	target, err := websearch.PublicURL(strVal(in, "url", ""))
	if err != nil {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "url: a public http(s) web address is required", nil)
	}
	reg := s.Registry()
	if reg.Reader == nil {
		return action.OK(req, map[string]any{"status": "unconfigured", "live": false, "url": target, "message": s.msg(req, "provider.unconfigured", "provider", "jina")})
	}
	if err := reg.reserve(reg.readerID); err != nil {
		return s.providerFail(req, reg.readerID, err)
	}
	page, err := reg.Reader.Read(ctx, target, min(max(intVal(in, "max_chars", 2800), 500), 20000))
	if err != nil {
		return s.providerFail(req, reg.readerID, err)
	}
	s.noteProvider(reg.readerID, nil)
	return action.OK(req, map[string]any{"status": "ok", "live": true, "source": "jina_reader", "provider": reg.readerID, "url": page.URL,
		"title": page.Title, "content": page.Content, "truncated": page.Truncated})
}

// ---------------------------------------------------------------------------
// Fortune / notices / images — no live provider; every output says so.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Polls (persisted domain state)
// ---------------------------------------------------------------------------

func pollCode(err error) string {
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, errcode.StaleRevision):
		return errcode.StaleRevision
	case strings.HasPrefix(msg, errcode.ScopeMismatch):
		return errcode.ScopeMismatch
	}
	return errcode.InvalidInput
}

func (s *Service) polls(req action.Request) action.Result {
	in := inputMap(req)
	switch req.Action {
	case "polls.create":
		opts := []poll.Option{}
		for _, id := range strSlice(in, "option_ids") {
			opts = append(opts, poll.Option{ID: id, Label: id})
		}
		if len(opts) == 0 {
			if raw, ok := in["options"].([]any); ok {
				for _, x := range raw {
					switch v := x.(type) {
					case map[string]any:
						opts = append(opts, poll.Option{ID: fmt.Sprint(v["id"]), Label: fmt.Sprint(v["label"])})
					case string:
						opts = append(opts, poll.Option{ID: v, Label: v})
					}
				}
			}
		}
		if len(opts) == 0 {
			// GUI/CLI convenience: "options_text" is one option per line, ids are derived.
			for i, line := range strings.Split(strVal(in, "options_text", ""), "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					opts = append(opts, poll.Option{ID: fmt.Sprintf("o%d", i+1), Label: line})
				}
			}
		}
		if len(opts) == 0 {
			opts = []poll.Option{{ID: "o1", Label: s.msg(req, "poll.yes")}, {ID: "o2", Label: s.msg(req, "poll.no")}}
		}
		hours := intVal(in, "duration_hours", 24)
		if hours < 1 {
			hours = 24
		}
		if hours > 768 {
			hours = 768
		}
		closes := s.Clock.Now().Add(time.Duration(hours) * time.Hour)
		maxSel := intVal(in, "max_select", 1)
		if boolVal(in, "allow_multiselect") {
			maxSel = len(opts)
		}
		id := strVal(in, "id", newID("poll_"))
		p, err := poll.Create(poll.CreateCmd{
			ID: id, ScopeKey: strVal(in, "scope", "default"), Question: strVal(in, "question", ""),
			Options: opts, MaxSelect: maxSel, ClosesAt: &closes,
		})
		if err != nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", err.Error(), nil)
		}
		if err := s.savePoll(p); err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		out := pollOut(p)
		out["duration_hours"] = hours
		answers := make([]any, 0, len(p.Options))
		for _, o := range p.Options {
			answers = append(answers, map[string]any{"poll_media": map[string]any{"text": o.Label}})
		}
		out["poll"] = map[string]any{
			"question":          map[string]any{"text": p.Question},
			"answers":           answers,
			"duration":          hours,
			"allow_multiselect": p.MaxSelect > 1,
			"layout_type":       1,
		}
		out["message"] = s.msg(req, "poll.created")
		res := action.OK(req, out)
		res.Revision = p.Revision
		return res
	case "polls.vote":
		ids := strSlice(in, "option_ids")
		if len(ids) == 0 {
			for _, part := range strings.Split(strVal(in, "option_ids_text", ""), ",") {
				if part = strings.TrimSpace(part); part != "" {
					ids = append(ids, part)
				}
			}
		}
		p, err := s.updatePoll(strVal(in, "id", req.ResourceID), func(p poll.Poll) (poll.Poll, error) {
			return poll.Vote(p, poll.VoteCmd{ScopeKey: strVal(in, "scope", p.ScopeKey), ExpectedRev: pollBase(in, req, p), ParticipantKey: strVal(in, "participant", ""), OptionIDs: ids, Now: s.Clock.Now()})
		})
		if errors.Is(err, errPollNotFound) {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "poll not found", nil)
		}
		if err != nil {
			return action.Fail(req, pollCode(err), "generic.error", err.Error(), map[string]any{"revision": p.Revision})
		}
		res := action.OK(req, pollOut(p))
		res.Revision = p.Revision
		return res
	case "polls.close":
		p, err := s.updatePoll(strVal(in, "id", req.ResourceID), func(p poll.Poll) (poll.Poll, error) {
			return poll.Close(p, poll.CloseCmd{ScopeKey: strVal(in, "scope", p.ScopeKey), ExpectedRev: pollBase(in, req, p), Now: s.Clock.Now()})
		})
		if errors.Is(err, errPollNotFound) {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "poll not found", nil)
		}
		if err != nil {
			return action.Fail(req, pollCode(err), "generic.error", err.Error(), map[string]any{"revision": p.Revision})
		}
		res := action.OK(req, pollOut(p))
		res.Revision = p.Revision
		return res
	case "polls.result":
		id := strVal(in, "id", req.ResourceID)
		if id == "" {
			list, err := s.listPolls()
			if err != nil {
				return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
			}
			return action.OK(req, map[string]any{"polls": list})
		}
		p, err := s.loadPoll(id)
		if err != nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "poll not found", nil)
		}
		r := poll.Tally(p)
		out := pollOut(p)
		out["unique_voters"], out["total_selections"] = r.UniqueVoters, r.TotalSelections
		out["by_option"], out["ties"] = tallyOut(r), r.TieOptionIDs
		return action.OK(req, out)
	}
	return action.Fail(req, errcode.ActionUnknown, "action.unknown", s.msg(req, "action.unknown"), nil)
}

func (s *Service) savePoll(p poll.Poll) error {
	raw, err := poll.Marshal(p)
	if err != nil {
		return err
	}
	err = s.Store.WithWriter(func(tx *sql.Tx) error {
		return s.Store.UpsertPoll(tx, p.ID, p.ScopeKey, p.Revision, string(raw), s.Clock.Now().UTC().Format(time.RFC3339))
	})
	if err == nil {
		// Counts only: ballots never leave the domain, and watchers re-fetch the tally themselves.
		s.emit("poll", p.ID, map[string]any{"status": string(p.Status), "revision": p.Revision})
	}
	return err
}

var errPollNotFound = errors.New("poll not found")

// pollBase is the revision a vote or close is checked against. A change made from a view of the poll
// (the console) sends the revision it saw. A Discord button press does not depend on what the voter
// saw, so it sends latest=true and is applied to the current revision inside the same transaction.
func pollBase(in map[string]any, req action.Request, cur poll.Poll) int {
	if boolVal(in, "latest") {
		return cur.Revision
	}
	return intVal(in, "base_revision", req.BaseRevision)
}

// updatePoll reads, changes and stores one poll in a single writer transaction, so the revision check
// in fn and the write cannot interleave with another vote: two people voting at once both count, or
// the later one gets STALE_REVISION, never a silently lost ballot. On a domain error the returned poll
// is the stored one (its revision tells the caller what to retry with).
func (s *Service) updatePoll(id string, fn func(poll.Poll) (poll.Poll, error)) (poll.Poll, error) {
	var out poll.Poll
	err := s.Store.WithWriter(func(tx *sql.Tx) error {
		q := `SELECT state FROM polls WHERE id=?`
		if !s.Store.IsSQLite() {
			q += ` FOR UPDATE` // other daemons sharing the TiDB database
		}
		var raw string
		if err := tx.QueryRow(q, id).Scan(&raw); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errPollNotFound
			}
			return err
		}
		cur, err := poll.Unmarshal([]byte(raw))
		if err != nil {
			return err
		}
		out = cur
		next, err := fn(cur)
		if err != nil {
			return err
		}
		state, err := poll.Marshal(next)
		if err != nil {
			return err
		}
		if err := s.Store.UpsertPoll(tx, next.ID, next.ScopeKey, next.Revision, string(state), s.Clock.Now().UTC().Format(time.RFC3339)); err != nil {
			return err
		}
		out = next
		return nil
	})
	if err == nil {
		s.emit("poll", out.ID, map[string]any{"status": string(out.Status), "revision": out.Revision})
	}
	return out, err
}

func (s *Service) loadPoll(id string) (poll.Poll, error) {
	var raw string
	if err := s.Store.DB.QueryRow(`SELECT state FROM polls WHERE id=?`, id).Scan(&raw); err != nil {
		return poll.Poll{}, err
	}
	return poll.Unmarshal([]byte(raw))
}

func (s *Service) listPolls() ([]map[string]any, error) {
	rows, err := s.Store.DB.Query(s.Store.ListPollsQuery())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		p, err := poll.Unmarshal([]byte(raw))
		if err != nil {
			continue
		}
		r := poll.Tally(p)
		m := pollOut(p)
		m["unique_voters"], m["total_selections"], m["by_option"] = r.UniqueVoters, r.TotalSelections, tallyOut(r)
		out = append(out, m)
	}
	return out, rows.Err()
}

// pollOut is the public shape of a poll. Individual ballots (participant → choices) stay private;
// only counts leave the domain (03장 privacy, NR-01 handoff).
func pollOut(p poll.Poll) map[string]any {
	opts := make([]any, 0, len(p.Options))
	for _, o := range p.Options {
		opts = append(opts, map[string]any{"id": o.ID, "label": o.Label})
	}
	m := map[string]any{
		"id": p.ID, "scope": p.ScopeKey, "question": p.Question, "options": opts, "max_select": p.MaxSelect,
		"status": string(p.Status), "revision": p.Revision, "voters": len(p.Votes),
		"allow_multiselect": p.MaxSelect > 1, "layout_type": 1,
	}
	if p.ClosesAt != nil {
		m["closes_at"] = p.ClosesAt.Format(time.RFC3339)
	}
	return m
}

func tallyOut(r poll.Result) []any {
	out := make([]any, 0, len(r.ByOption))
	for _, t := range r.ByOption {
		out = append(out, map[string]any{"option_id": t.OptionID, "label": t.Label, "votes": t.Votes, "ratio": t.Ratio})
	}
	return out
}
