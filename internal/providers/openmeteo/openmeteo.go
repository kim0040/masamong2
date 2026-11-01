// Package openmeteo adapts Open-Meteo, a worldwide weather API (current conditions and a daily
// forecast from national weather services' models), and its place-name geocoder. It needs no key for
// non-commercial use; with a key (credential) the customer endpoints are used instead.
//
//	geocoding  {geo}/v1/search?name=&count=1&language=&format=json → {"results":[{name,latitude,…}]}
//	forecast   {api}/v1/forecast?latitude=&longitude=&current=…&daily=…&timezone=auto
//
// Errors come back as HTTP 400 with {"error":true,"reason":"…"}.
package openmeteo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"masamong/internal/providers"
	"masamong/internal/providers/decode"
)

const (
	APIBase           = "https://api.open-meteo.com"
	GeoBase           = "https://geocoding-api.open-meteo.com"
	CustomerAPIBase   = "https://customer-api.open-meteo.com"
	CustomerGeoBase   = "https://customer-geocoding-api.open-meteo.com"
	MaxForecastDays   = 14
	maxPlaceNameRunes = 80
)

type Adapter struct {
	Client  *providers.Client
	APIBase string
	GeoBase string
	Cred    providers.Credential // optional: commercial API key
}

// New builds an adapter. baseURL overrides the forecast host (tests, self-hosted Open-Meteo); the
// geocoder follows it when it is not the public host.
func New(client *providers.Client, baseURL string, cred providers.Credential) *Adapter {
	a := &Adapter{Client: client, APIBase: APIBase, GeoBase: GeoBase, Cred: cred}
	if cred.Present() {
		a.APIBase, a.GeoBase = CustomerAPIBase, CustomerGeoBase
	}
	if b := strings.TrimRight(baseURL, "/"); b != "" && b != APIBase {
		a.APIBase, a.GeoBase = b, b
	}
	return a
}

// Configured is always true: the public endpoints need no key.
func (a *Adapter) Configured() bool { return true }

var (
	ErrNotFound = errors.New("openmeteo: no place matches that name")
	ErrProvider = errors.New("openmeteo: provider error")
)

// Place is a geocoded location.
type Place struct {
	Name        string  `json:"name"`
	FeatureCode string  `json:"feature_code,omitempty"`
	Population  int     `json:"population,omitempty"`
	Admin1      string  `json:"admin1,omitempty"`
	Country     string  `json:"country,omitempty"`
	CountryCode string  `json:"country_code,omitempty"`
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
	Timezone    string  `json:"timezone,omitempty"`
}

func (a *Adapter) withKey(v url.Values) url.Values {
	if a.Cred.Present() {
		v.Set("apikey", a.Cred.Value())
	}
	return v
}

// Geocode asks each bundled geocoder language for the same name. Place names and romanizations vary
// by index, and the largest populated-place match is usually more useful than the first locale hit.
func (a *Adapter) Geocode(ctx context.Context, name, lang string) (Place, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > maxPlaceNameRunes {
		return Place{}, fmt.Errorf("%w: place name must be 1–%d characters", ErrNotFound, maxPlaceNameRunes)
	}
	if lang == "" {
		lang = "en"
	}
	if lang == "zh-Hans" {
		lang = "zh"
	}
	languages := []string{lang, "en", "ko", "ja", "zh", "es"}
	seen := map[string]bool{}
	var matches []Place
	var lastErr error
	for _, candidate := range languages {
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		p, err := a.geocode(ctx, name, candidate)
		if err == nil {
			matches = append(matches, p)
			continue
		}
		if !errors.Is(err, ErrNotFound) {
			lastErr = err
			if len(matches) == 0 {
				return Place{}, err
			}
		}
	}
	if len(matches) > 0 {
		return best(matches), nil
	}
	if lastErr != nil {
		return Place{}, lastErr
	}
	return Place{}, ErrNotFound
}

func (a *Adapter) geocode(ctx context.Context, name, lang string) (Place, error) {
	q := a.withKey(url.Values{"name": {name}, "count": {"5"}, "language": {lang}, "format": {"json"}})
	resp, err := a.Client.Get(ctx, a.GeoBase+"/v1/search?"+q.Encode(), nil)
	if err != nil {
		return Place{}, a.mapErr(err, resp)
	}
	return ParseGeocode(resp.ContentType, resp.Body)
}

// best picks the most likely meant place: populated places (feature codes PPL*) before airports,
// mountains and the like, then the largest population, then the geocoder's order.
func best(rows []Place) Place {
	rank := func(p Place) int {
		if strings.HasPrefix(p.FeatureCode, "PPL") {
			return 0
		}
		return 1
	}
	b := rows[0]
	for _, p := range rows[1:] {
		if rank(p) < rank(b) || (rank(p) == rank(b) && p.Population > b.Population) {
			b = p
		}
	}
	return b
}

// ParseGeocode reads the results and picks the best one; an empty or missing list is ErrNotFound.
func ParseGeocode(contentType string, body []byte) (Place, error) {
	obj, err := decode.Object(contentType, body)
	if err != nil {
		return Place{}, err
	}
	if err := apiError(obj); err != nil {
		return Place{}, err
	}
	var rows []Place
	if raw, ok := obj["results"]; ok {
		if err := json.Unmarshal(raw, &rows); err != nil {
			return Place{}, fmt.Errorf("%w: results: %v", decode.ErrUnexpected, err)
		}
	}
	kept := rows[:0]
	for _, r := range rows {
		if r.Name != "" {
			kept = append(kept, r)
		}
	}
	if len(kept) == 0 {
		return Place{}, ErrNotFound
	}
	p := best(kept)
	if p.Latitude < -90 || p.Latitude > 90 || p.Longitude < -180 || p.Longitude > 180 {
		return Place{}, fmt.Errorf("%w: coordinates out of range", decode.ErrUnexpected)
	}
	return p, nil
}

// Day is one forecast day in the place's local calendar.
type Day struct {
	Date       string   `json:"date"`
	MaxC       *float64 `json:"max_c"`
	MinC       *float64 `json:"min_c"`
	PrecipProb *float64 `json:"precip_prob_pct"`
	Code       int      `json:"code"`
	Condition  string   `json:"condition"`
}

// Weather is current conditions plus daily forecasts.
type Weather struct {
	Timezone   string
	ObservedAt time.Time // local time of the current reading
	TempC      *float64
	FeelsC     *float64
	Humidity   *float64
	PrecipMM   *float64
	WindMS     *float64
	WindDeg    *float64
	Code       int
	Condition  string
	Days       []Day
}

// Forecast returns current conditions and days (1–7) of daily forecast for a coordinate.
func (a *Adapter) Forecast(ctx context.Context, lat, lon float64, days int) (Weather, error) {
	days = min(max(days, 1), MaxForecastDays)
	q := a.withKey(url.Values{
		"latitude":        {strconv.FormatFloat(lat, 'f', 4, 64)},
		"longitude":       {strconv.FormatFloat(lon, 'f', 4, 64)},
		"current":         {"temperature_2m,relative_humidity_2m,apparent_temperature,precipitation,weather_code,wind_speed_10m,wind_direction_10m"},
		"daily":           {"temperature_2m_max,temperature_2m_min,precipitation_probability_max,weather_code"},
		"timezone":        {"auto"},
		"forecast_days":   {strconv.Itoa(days)},
		"wind_speed_unit": {"ms"},
	})
	resp, err := a.Client.Get(ctx, a.APIBase+"/v1/forecast?"+q.Encode(), nil)
	if err != nil {
		return Weather{}, a.mapErr(err, resp)
	}
	return ParseForecast(resp.ContentType, resp.Body)
}

// ParseForecast validates the current block and the daily arrays (which must be the same length).
func ParseForecast(contentType string, body []byte) (Weather, error) {
	obj, err := decode.Object(contentType, body)
	if err != nil {
		return Weather{}, err
	}
	if err := apiError(obj); err != nil {
		return Weather{}, err
	}
	var w Weather
	w.Timezone = decode.Str(obj["timezone"])
	var cur map[string]json.RawMessage
	if err := json.Unmarshal(obj["current"], &cur); err != nil || cur == nil {
		return Weather{}, fmt.Errorf("%w: current missing", decode.ErrUnexpected)
	}
	loc := time.UTC
	if w.Timezone != "" {
		if l, err := time.LoadLocation(w.Timezone); err == nil {
			loc = l
		} else if offset, ok := decode.Number(obj["utc_offset_seconds"]); ok {
			// a zone name this build does not know: the offset the API returns is still right for the
			// reading, so the forecast is not lost over a name
			loc = time.FixedZone(w.Timezone, int(offset))
		}
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04", decode.Str(cur["time"]), loc); err == nil {
		w.ObservedAt = t
	}
	num := func(raw json.RawMessage) *float64 {
		if n, ok := decode.Number(raw); ok {
			return &n
		}
		return nil
	}
	w.TempC, w.FeelsC, w.Humidity = num(cur["temperature_2m"]), num(cur["apparent_temperature"]), num(cur["relative_humidity_2m"])
	w.PrecipMM, w.WindMS, w.WindDeg = num(cur["precipitation"]), num(cur["wind_speed_10m"]), num(cur["wind_direction_10m"])
	if w.TempC == nil {
		return Weather{}, fmt.Errorf("%w: no current temperature", decode.ErrUnexpected)
	}
	if c := num(cur["weather_code"]); c != nil {
		w.Code = int(*c)
	}
	w.Condition = Condition(w.Code)
	var daily struct {
		Time []string   `json:"time"`
		Max  []*float64 `json:"temperature_2m_max"`
		Min  []*float64 `json:"temperature_2m_min"`
		Prob []*float64 `json:"precipitation_probability_max"`
		Code []*float64 `json:"weather_code"`
	}
	if raw, ok := obj["daily"]; ok {
		if err := json.Unmarshal(raw, &daily); err != nil {
			return Weather{}, fmt.Errorf("%w: daily: %v", decode.ErrUnexpected, err)
		}
		n := len(daily.Time)
		if len(daily.Max) != n || len(daily.Min) != n || len(daily.Prob) != n || len(daily.Code) != n {
			return Weather{}, fmt.Errorf("%w: daily arrays differ in length", decode.ErrUnexpected)
		}
		for i := 0; i < n; i++ {
			d := Day{Date: daily.Time[i], MaxC: daily.Max[i], MinC: daily.Min[i], PrecipProb: daily.Prob[i]}
			if daily.Code[i] != nil {
				d.Code = int(*daily.Code[i])
			}
			d.Condition = Condition(d.Code)
			w.Days = append(w.Days, d)
		}
	}
	return w, nil
}

func apiError(obj map[string]json.RawMessage) error {
	if string(obj["error"]) == "true" {
		return fmt.Errorf("%w: %s", ErrProvider, decode.Str(obj["reason"]))
	}
	return nil
}

func (a *Adapter) mapErr(err error, resp *providers.Response) error {
	var pe *providers.Error
	if errors.As(err, &pe) && resp != nil && len(resp.Body) > 0 {
		if obj, oerr := decode.Object(resp.ContentType, resp.Body); oerr == nil {
			if e := apiError(obj); e != nil {
				return e
			}
		}
	}
	return err
}

// Condition names a WMO weather interpretation code (the codes Open-Meteo returns). The names are
// stable keys; surfaces translate them.
func Condition(code int) string {
	switch {
	case code == 0:
		return "clear"
	case code == 1:
		return "mainly_clear"
	case code == 2:
		return "partly_cloudy"
	case code == 3:
		return "overcast"
	case code == 45 || code == 48:
		return "fog"
	case code >= 51 && code <= 57:
		return "drizzle"
	case code == 66 || code == 67:
		return "freezing_rain"
	case code >= 61 && code <= 65:
		return "rain"
	case code >= 71 && code <= 77:
		return "snow"
	case code >= 80 && code <= 82:
		return "showers"
	case code == 85 || code == 86:
		return "snow_showers"
	case code >= 95:
		return "thunderstorm"
	}
	return "unknown"
}

// Precip maps a condition to the precipitation vocabulary the KMA path already uses.
func Precip(condition string) string {
	switch condition {
	case "drizzle":
		return "drizzle"
	case "rain", "showers", "thunderstorm", "freezing_rain":
		return "rain"
	case "snow", "snow_showers":
		return "snow"
	}
	return "none"
}
