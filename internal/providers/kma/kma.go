// Package kma adapts 기상청 API Hub (apihub.kma.go.kr). It speaks two wire formats:
// typ02 JSON (public-data envelope or flat items) and typ01 delimited text. Every response is
// decoded through providers/decode and validated field-by-field before a feature can use it.
package kma

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"masamong/internal/providers"
	"masamong/internal/providers/decode"
)

const DefaultBaseURL = "https://apihub.kma.go.kr"

var KST = time.FixedZone("KST", 9*3600)

type Adapter struct {
	Client  *providers.Client
	BaseURL string
	Cred    providers.Credential
	Now     func() time.Time
}

func New(client *providers.Client, baseURL string, cred providers.Credential) *Adapter {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Adapter{Client: client, BaseURL: strings.TrimRight(baseURL, "/"), Cred: cred, Now: time.Now}
}

func (a *Adapter) Configured() bool { return a.Cred.Present() }

// ---------------------------------------------------------------------------
// Grid lookup
// ---------------------------------------------------------------------------

type Grid struct {
	Name string
	NX   int
	NY   int
}

// grids is a curated subset of the KMA 동네예보 grid table. Unknown names are an error, never a silent default.
var grids = map[string]Grid{
	"서울": {"서울", 60, 127}, "seoul": {"서울", 60, 127},
	"부산": {"부산", 98, 76}, "busan": {"부산", 98, 76},
	"대구": {"대구", 89, 90}, "daegu": {"대구", 89, 90},
	"인천": {"인천", 55, 124}, "incheon": {"인천", 55, 124},
	"광주": {"광주", 58, 74}, "gwangju": {"광주", 58, 74},
	"대전": {"대전", 67, 100}, "daejeon": {"대전", 67, 100},
	"울산": {"울산", 102, 84}, "ulsan": {"울산", 102, 84},
	"세종": {"세종", 66, 103}, "sejong": {"세종", 66, 103},
	"수원": {"수원", 60, 121}, "suwon": {"수원", 60, 121},
	"성남": {"성남", 62, 123}, "고양": {"고양", 57, 128}, "용인": {"용인", 64, 119},
	"춘천": {"춘천", 73, 134}, "chuncheon": {"춘천", 73, 134},
	"강릉": {"강릉", 92, 131}, "gangneung": {"강릉", 92, 131},
	"청주": {"청주", 69, 107}, "cheongju": {"청주", 69, 107},
	"전주": {"전주", 63, 89}, "jeonju": {"전주", 63, 89},
	"포항": {"포항", 102, 94}, "pohang": {"포항", 102, 94},
	"창원": {"창원", 90, 77}, "changwon": {"창원", 90, 77},
	"제주": {"제주", 52, 38}, "jeju": {"제주", 52, 38},
	"서귀포": {"서귀포", 52, 33},
}

var ErrUnknownRegion = errors.New("kma: unknown region")

// LookupGrid resolves a region name; suffixes like 시/광역시/특별시 are tolerated.
func LookupGrid(name string) (Grid, error) {
	key := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), " ", ""))
	if g, ok := grids[key]; ok {
		return g, nil
	}
	for _, suffix := range []string{"특별자치도", "특별자치시", "광역시", "특별시", "시", "도"} {
		if strings.HasSuffix(key, suffix) {
			if g, ok := grids[strings.TrimSuffix(key, suffix)]; ok {
				return g, nil
			}
		}
	}
	return Grid{}, fmt.Errorf("%w: %q", ErrUnknownRegion, name)
}

// Regions lists supported region names (Korean display names, deduplicated).
// DefaultRegion is the region used when a weather query names none (KMA covers Korea only).
const DefaultRegion = "서울"

func Regions() []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range grids {
		if !seen[g.Name] {
			seen[g.Name] = true
			out = append(out, g.Name)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// 초단기실황 (current observation) — typ02 JSON
// ---------------------------------------------------------------------------

type Observation struct {
	Grid         Grid
	BaseDate     string // YYYYMMDD
	BaseTime     string // HHMM
	ObservedAt   time.Time
	TempC        *float64
	Humidity     *float64
	WindSpeed    *float64
	WindDeg      *float64
	Rain1h       *float64
	Precip       string // none|rain|rain_snow|snow|drizzle|drizzle_snow|snow_flurry|unknown
	Variant      string // envelope variant seen
	MissingCodes []string
}

// BaseTime returns the observation slot published by the time now. KMA publishes hourly at ~:10;
// we keep a 45-minute margin like the legacy bot so a request never asks for an unpublished slot.
func BaseTime(now time.Time) (date, hhmm string, at time.Time) {
	t := now.In(KST)
	if t.Minute() < 45 {
		t = t.Add(-1 * time.Hour)
	}
	t = t.Truncate(time.Hour)
	return t.Format("20060102"), t.Format("1500"), t
}

var ErrNoCredential = providers.ErrNoCredential

func (a *Adapter) Current(ctx context.Context, g Grid) (Observation, error) {
	if !a.Configured() {
		return Observation{}, ErrNoCredential
	}
	date, hhmm, at := BaseTime(a.Now())
	q := url.Values{}
	q.Set("pageNo", "1")
	q.Set("numOfRows", "20")
	q.Set("dataType", "JSON")
	q.Set("base_date", date)
	q.Set("base_time", hhmm)
	q.Set("nx", strconv.Itoa(g.NX))
	q.Set("ny", strconv.Itoa(g.NY))
	q.Set("authKey", a.Cred.Value())
	resp, err := a.Client.Get(ctx, a.BaseURL+"/api/typ02/openApi/VilageFcstInfoService_2.0/getUltraSrtNcst?"+q.Encode(), nil)
	if err != nil {
		return Observation{}, err
	}
	obs, err := ParseCurrent(resp.ContentType, resp.Body)
	if err != nil {
		return Observation{}, err
	}
	obs.Grid = g
	if obs.BaseDate == "" {
		obs.BaseDate, obs.BaseTime = date, hhmm
	}
	obs.ObservedAt = at
	if t, err := time.ParseInLocation("200601021504", obs.BaseDate+obs.BaseTime, KST); err == nil {
		obs.ObservedAt = t
	}
	return obs, nil
}

var requiredCategories = []string{"T1H", "REH", "WSD", "PTY"}

// ParseCurrent validates a getUltraSrtNcst payload. It returns decode.ErrKMANoData for an empty slot,
// and an error naming the missing category when the payload is structurally valid but incomplete.
func ParseCurrent(contentType string, body []byte) (Observation, error) {
	env, err := decode.KMAJSON(contentType, body)
	if err != nil {
		return Observation{}, err
	}
	obs := Observation{Variant: env.Variant, Precip: "unknown"}
	seen := map[string]bool{}
	for _, it := range env.Items {
		cat := decode.FieldStr(it, "category")
		if cat == "" {
			continue
		}
		if obs.BaseDate == "" {
			obs.BaseDate = decode.FieldStr(it, "baseDate")
			obs.BaseTime = decode.FieldStr(it, "baseTime")
		}
		val, ok := decode.FieldNum(it, "obsrValue")
		seen[cat] = true
		switch cat {
		case "T1H":
			if ok && val > -60 && val < 60 {
				obs.TempC = &val
			}
		case "REH":
			if ok && val >= 0 && val <= 100 {
				obs.Humidity = &val
			}
		case "WSD":
			if ok && val >= 0 && val < 100 {
				obs.WindSpeed = &val
			}
		case "VEC":
			if ok && val >= 0 && val <= 360 {
				obs.WindDeg = &val
			}
		case "RN1":
			if ok && val >= 0 {
				obs.Rain1h = &val
			}
		case "PTY":
			if ok {
				obs.Precip = precipName(int(val))
			}
		}
	}
	for _, c := range requiredCategories {
		if !seen[c] {
			obs.MissingCodes = append(obs.MissingCodes, c)
		}
	}
	if obs.TempC == nil && obs.Humidity == nil && obs.WindSpeed == nil {
		return obs, fmt.Errorf("%w: no usable observation categories (missing %v)", decode.ErrUnexpected, obs.MissingCodes)
	}
	return obs, nil
}

func precipName(code int) string {
	switch code {
	case 0:
		return "none"
	case 1:
		return "rain"
	case 2:
		return "rain_snow"
	case 3:
		return "snow"
	case 5:
		return "drizzle"
	case 6:
		return "drizzle_snow"
	case 7:
		return "snow_flurry"
	}
	return "unknown"
}

// ---------------------------------------------------------------------------
// 지진통보 (earthquake notices) — typ02 JSON
// ---------------------------------------------------------------------------

type Earthquake struct {
	NoticeTime       string    // tmFc raw
	EventTime        time.Time // tmEqk parsed (KST)
	Location         string
	Lat, Lon         *float64
	Magnitude        *float64
	DepthKM          *float64
	Remark           string
	Domestic         bool
	Sequence         string
	NoDomesticImpact bool
}

func (a *Adapter) Earthquakes(ctx context.Context, days int) ([]Earthquake, error) {
	if !a.Configured() {
		return nil, ErrNoCredential
	}
	if days < 1 || days > 3 {
		days = 3 // API allows at most 3 days
	}
	now := a.Now().In(KST)
	q := url.Values{}
	q.Set("pageNo", "1")
	q.Set("numOfRows", "100")
	q.Set("dataType", "JSON")
	q.Set("fromTmFc", now.AddDate(0, 0, -(days-1)).Format("20060102"))
	q.Set("toTmFc", now.Format("20060102"))
	q.Set("authKey", a.Cred.Value())
	resp, err := a.Client.Get(ctx, a.BaseURL+"/api/typ02/openApi/EqkInfoService/getEqkMsg?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	return ParseEarthquakes(resp.ContentType, resp.Body)
}

// ParseEarthquakes validates getEqkMsg items. An empty list (NO_DATA) is returned as (nil, nil):
// "no notices" is a valid answer and distinct from a transport error.
func ParseEarthquakes(contentType string, body []byte) ([]Earthquake, error) {
	env, err := decode.KMAJSON(contentType, body)
	if errors.Is(err, decode.ErrKMANoData) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Earthquake
	for _, it := range env.Items {
		e := Earthquake{
			NoticeTime: decode.FieldStr(it, "tmFc"),
			Location:   strings.Join(strings.Fields(decode.FieldStr(it, "loc")), " "),
			Remark:     decode.FieldStr(it, "rem"),
			Sequence:   decode.FieldStr(it, "tmSeq"),
		}
		raw := decode.FieldStr(it, "tmEqk")
		if len(raw) == 14 {
			e.EventTime, _ = time.ParseInLocation("20060102150405", raw, KST)
		} else if len(raw) == 12 {
			e.EventTime, _ = time.ParseInLocation("200601021504", raw, KST)
		}
		if v, ok := decode.FieldNum(it, "lat"); ok {
			e.Lat = &v
		}
		if v, ok := decode.FieldNum(it, "lon"); ok {
			e.Lon = &v
		}
		if v, ok := decode.FieldNum(it, "mt"); ok && v >= 0 && v < 10 {
			e.Magnitude = &v
		}
		if v, ok := decode.FieldNum(it, "dep"); ok {
			e.DepthKM = &v
		}
		e.NoDomesticImpact = strings.Contains(e.Remark, "국내영향없음")
		e.Domestic = isDomestic(e)
		if e.Location == "" && e.Magnitude == nil {
			continue // not a notice row
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].EventTime.After(out[j].EventTime) })
	return out, nil
}

var overseasTokens = []string{"일본", "중국", "대만", "타이완", "러시아", "필리핀", "인도네시아", "몽골", "베트남", "미국", "알래스카", "칠레", "멕시코", "페루", "뉴질랜드", "파푸아", "터키", "튀르키예", "네팔", "이란", "쿠릴", "캄차카", "괌", "통가", "피지", "솔로몬", "바누아투"}
var domesticTokens = []string{"서울", "부산", "대구", "인천", "광주", "대전", "울산", "세종", "경기", "강원", "충북", "충남", "전북", "전남", "경북", "경남", "제주", "해역", "북한", "황해", "동해", "남해", "서해"}

type band struct{ latMin, latMax, lonMin, lonMax float64 }

var koreaBands = []band{{32, 34, 124, 128.5}, {34, 36, 124, 130}, {36, 39, 123.5, 132}, {39, 43, 124, 131}}

func isDomestic(e Earthquake) bool {
	for _, t := range overseasTokens {
		if strings.Contains(e.Location, t) {
			return false
		}
	}
	for _, t := range domesticTokens {
		if strings.Contains(e.Location, t) {
			return true
		}
	}
	if e.Lat == nil || e.Lon == nil {
		return true // classification failure leans toward the lower (domestic) threshold
	}
	for _, b := range koreaBands {
		if *e.Lat >= b.latMin && *e.Lat <= b.latMax && *e.Lon >= b.lonMin && *e.Lon <= b.lonMax {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 특보 현황 (active warnings) — typ01 text
// ---------------------------------------------------------------------------

type Warning struct {
	RegionCode string
	Region     string
	ParentCode string
	Parent     string
	Kind       string // W R C D O N V T S Y H F K
	KindName   string
	Level      string // 1 예비 2 주의보 3 경보
	LevelName  string
	Command    string // CMD
	Announced  string // TM_FC
	Effective  string // TM_EF
}

var warningColumns = []string{"REG_UP", "REG_UP_KO", "REG_ID", "REG_KO", "TM_FC", "TM_EF", "WRN", "LVL", "CMD"}

var kindNames = map[string]string{"W": "강풍", "R": "호우", "C": "한파", "D": "건조", "O": "폭풍해일", "N": "지진해일", "V": "풍랑", "T": "태풍", "S": "대설", "Y": "황사", "H": "폭염", "F": "안개", "K": "열대야"}
var levelNames = map[string]string{"1": "예비", "2": "주의보", "3": "경보"}

func (a *Adapter) ActiveWarnings(ctx context.Context) ([]Warning, error) {
	if !a.Configured() {
		return nil, ErrNoCredential
	}
	q := url.Values{}
	q.Set("fe", "e")
	q.Set("tm", "")
	q.Set("disp", "1")
	q.Set("help", "0")
	q.Set("authKey", a.Cred.Value())
	resp, err := a.Client.Get(ctx, a.BaseURL+"/api/typ01/url/wrn_now_data.php?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	return ParseWarnings(resp.Body)
}

// ParseWarnings turns the typ01 table into typed rows. No rows means "no active warnings"; a text
// error body or HTML page is an error, never an empty success. With disp=1 API Hub answers a JSON
// array whose WRN/LVL are Korean names padded with spaces (checked live 2026-09-24); the typ01 text
// form (codes, CSV or whitespace) is still accepted.
func ParseWarnings(body []byte) ([]Warning, error) {
	if decode.Sniff("", body) == decode.JSON {
		return parseWarningsJSON(body)
	}
	t, err := decode.KMAText(body, warningColumns)
	if err != nil {
		return nil, err
	}
	var out []Warning
	for _, r := range t.Rows {
		kind := r["WRN"]
		if kind == "" {
			continue
		}
		w := Warning{
			RegionCode: r["REG_ID"], Region: r["REG_KO"], ParentCode: r["REG_UP"], Parent: r["REG_UP_KO"],
			Kind: kind, KindName: kindNames[kind], Level: r["LVL"], LevelName: levelNames[r["LVL"]],
			Command: r["CMD"], Announced: r["TM_FC"], Effective: r["TM_EF"],
		}
		if w.KindName == "" {
			w.KindName = kind
		}
		if w.LevelName == "" {
			w.LevelName = w.Level
		}
		out = append(out, w)
	}
	return out, nil
}

var kindCodes = func() map[string]string {
	m := make(map[string]string, len(kindNames))
	for code, name := range kindNames {
		m[name] = code
	}
	return m
}()

// levelCodes maps the JSON level names; 주의/경보 are how disp=1 spells 주의보/경보.
var levelCodes = map[string]string{"예비": "1", "주의": "2", "주의보": "2", "경보": "3"}

func parseWarningsJSON(body []byte) ([]Warning, error) {
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		// {"result":401,"message":...} and other objects are error bodies, never "no warnings".
		return nil, fmt.Errorf("%w: warnings: %s", decode.ErrUnexpected, strings.TrimSpace(string(body[:min(len(body), 120)])))
	}
	out := make([]Warning, 0, len(rows))
	for _, r := range rows {
		f := func(k string) string { return decode.FieldStr(r, k) }
		name := f("WRN")
		if name == "" {
			continue
		}
		w := Warning{
			RegionCode: f("REG_ID"), Region: f("REG_KO"), ParentCode: f("REG_UP"), Parent: f("REG_UP_KO"),
			Kind: name, KindName: name, Level: f("LVL"), LevelName: f("LVL"),
			Command: f("CMD"), Announced: f("TM_FC"), Effective: f("TM_EF"),
		}
		if code, ok := kindCodes[name]; ok {
			w.Kind = code
		} else if n, ok := kindNames[name]; ok { // already a code
			w.KindName = n
		}
		if code, ok := levelCodes[w.Level]; ok {
			w.Level, w.LevelName = code, levelNames[code]
		} else if n, ok := levelNames[w.Level]; ok {
			w.LevelName = n
		}
		out = append(out, w)
	}
	return out, nil
}

// LatLon converts the grid point back to WGS84 degrees with KMA's Lambert conformal conic projection
// (동네예보 격자: 5 km, standard parallels 30°/60°, origin 38°N 126°E at grid 43,136). Other sources
// (the worldwide forecast) use it for Korean regions, whose names their geocoders resolve poorly.
func (g Grid) LatLon() (lat, lon float64) {
	const (
		re    = 6371.00877 / 5.0
		slat1 = 30.0 * math.Pi / 180
		slat2 = 60.0 * math.Pi / 180
		olon  = 126.0 * math.Pi / 180
		olat  = 38.0 * math.Pi / 180
		xo    = 43.0
		yo    = 136.0
	)
	sn := math.Log(math.Cos(slat1)/math.Cos(slat2)) / math.Log(math.Tan(math.Pi*0.25+slat2*0.5)/math.Tan(math.Pi*0.25+slat1*0.5))
	sf := math.Pow(math.Tan(math.Pi*0.25+slat1*0.5), sn) * math.Cos(slat1) / sn
	ro := re * sf / math.Pow(math.Tan(math.Pi*0.25+olat*0.5), sn)
	xn := float64(g.NX) - xo
	yn := ro - float64(g.NY) + yo
	ra := math.Sqrt(xn*xn + yn*yn)
	if sn < 0 {
		ra = -ra
	}
	alat := 2*math.Atan(math.Pow(re*sf/ra, 1/sn)) - math.Pi*0.5
	theta := 0.0
	switch {
	case math.Abs(xn) <= 0:
	case math.Abs(yn) <= 0:
		theta = math.Pi * 0.5
		if xn < 0 {
			theta = -theta
		}
	default:
		theta = math.Atan2(xn, yn)
	}
	alon := theta/sn + olon
	return alat * 180 / math.Pi, alon * 180 / math.Pi
}
