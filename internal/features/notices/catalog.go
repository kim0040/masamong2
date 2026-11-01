// Package notices reads public university notice boards (V1 school_notice and transfer_notice): which
// boards and how to read them is data (catalog.json, overridable per instance), not code. It parses
// list and detail pages with CSS selectors, tags notices with audiences, topics and actions from the
// catalog's vocabulary, and scores them for a person's profile. Fetching is polite and bounded
// (fetch.go): allowed hosts only, public addresses only, robots.txt, a pause between requests.
package notices

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
)

//go:embed catalog.json
var defaultCatalog []byte

// Kinds of boards.
const (
	KindSchool   = "school"
	KindTransfer = "transfer"
)

// Term is one vocabulary entry: an id, how people call it, and the words that mark it in a notice.
type Term struct {
	ID    string   `json:"id"`
	Label string   `json:"label"`
	Terms []string `json:"terms"`
}

// Vocabulary tags notices. DegreeAudiences maps a profile's degree to the audiences it belongs to.
type Vocabulary struct {
	Audiences         []Term              `json:"audiences"`
	Topics            []Term              `json:"topics"`
	Actions           []Term              `json:"actions"`
	DegreeAudiences   map[string][]string `json:"degree_audiences"`
	OpportunityTopics []string            `json:"opportunity_topics"`
}

// TransferFilters decide which links of an admission board are transfer notices.
type TransferFilters struct {
	Announcement  string   `json:"announcement"`
	MixedInclude  string   `json:"mixed_include"`
	Exclude       string   `json:"exclude"`
	TitleTrailing string   `json:"title_trailing"`
	DropQueryKeys []string `json:"drop_query_keys"`

	announcement, mixedInclude, exclude, titleTrailing *regexp.Regexp
}

// DetailFallback finds a detail page's body when its source names no selector that matches.
type DetailFallback struct {
	Containers []string `json:"containers"`
	Drop       []string `json:"drop"`
}

// ListSpec reads a list page. With RowSelector each row is a notice and LinkSelector its link; without
// it every LinkSelector match is a candidate link (admission boards).
type ListSpec struct {
	RowSelector       string   `json:"row_selector,omitempty"`
	LinkSelector      string   `json:"link_selector"`
	IDAttribute       string   `json:"id_attribute,omitempty"`
	IDRegex           string   `json:"id_regex,omitempty"`
	TitleSelector     string   `json:"title_selector,omitempty"`
	TitleAttribute    string   `json:"title_attribute,omitempty"`
	DateSelector      string   `json:"date_selector,omitempty"`
	AuthorSelector    string   `json:"author_selector,omitempty"`
	CategorySelector  string   `json:"category_selector,omitempty"`
	IncludeSelector   string   `json:"include_selector,omitempty"`
	IncludeRegex      string   `json:"include_regex,omitempty"`
	DetailURLTemplate string   `json:"detail_url_template,omitempty"` // {id} (rows) or {value} (links)
	PinnedClasses     []string `json:"pinned_classes,omitempty"`
	HrefRegex         string   `json:"href_regex,omitempty"`
	URLValueSelector  string   `json:"url_value_selector,omitempty"`
	URLValueAttribute string   `json:"url_value_attribute,omitempty"`
	URLValueRegex     string   `json:"url_value_regex,omitempty"`
}

var compiled sync.Map // expression → *regexp.Regexp (validated by Parse)

// re is the compiled form of a catalog expression (nil for "").
func re(expr string) *regexp.Regexp {
	if expr == "" {
		return nil
	}
	if v, ok := compiled.Load(expr); ok {
		return v.(*regexp.Regexp)
	}
	r, err := regexp.Compile(expr)
	if err != nil {
		r = regexp.MustCompile(`$^`) // matches nothing; Parse reports the error
	}
	compiled.Store(expr, r)
	return r
}

// DetailSpec reads a notice page.
type DetailSpec struct {
	TitleSelectors []string `json:"title_selectors,omitempty"`
	BodySelectors  []string `json:"body_selectors,omitempty"`
	DateSelectors  []string `json:"date_selectors,omitempty"`
}

// Source is one board.
type Source struct {
	ID           string              `json:"id"`
	Kind         string              `json:"kind"`
	School       string              `json:"school,omitempty"`
	University   string              `json:"university"`
	Board        string              `json:"board,omitempty"`
	ListURL      string              `json:"list_url,omitempty"`
	OfficialURL  string              `json:"official_url,omitempty"`
	AllowedHosts []string            `json:"allowed_hosts,omitempty"`
	List         ListSpec            `json:"list"`
	Detail       DetailSpec          `json:"detail"`
	Tags         map[string][]string `json:"tags,omitempty"` // department, degree, campus: a board only for them
	MinItems     int                 `json:"min_items,omitempty"`
	MixedBoard   bool                `json:"mixed_board,omitempty"` // an admission board with more than transfer notices
	Manual       bool                `json:"manual,omitempty"`      // not read automatically (robots or terms); link only
	Note         string              `json:"note,omitempty"`
}

// School is a school people can pick for school notices.
type School struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Catalog is the whole data file.
type Catalog struct {
	SchemaVersion   int             `json:"schema_version"`
	About           string          `json:"about,omitempty"`
	Schools         []School        `json:"schools"`
	Vocabulary      Vocabulary      `json:"vocabulary"`
	TransferFilters TransferFilters `json:"transfer_filters"`
	DetailFallback  DetailFallback  `json:"detail_fallback"`
	Sources         []Source        `json:"sources"`
}

// Load reads the catalog: the file at overridePath when it exists, else the built-in one.
func Load(overridePath string) (*Catalog, string, error) {
	raw, origin := defaultCatalog, "built-in"
	if overridePath != "" {
		if b, err := os.ReadFile(overridePath); err == nil {
			raw, origin = b, overridePath
		} else if !os.IsNotExist(err) {
			return nil, "", err
		}
	}
	c, err := Parse(raw)
	return c, origin, err
}

func compile(field, expr string) (*regexp.Regexp, error) {
	if expr == "" {
		return nil, nil
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	return re, nil
}

// Parse validates a catalog.
func Parse(raw []byte) (*Catalog, error) {
	var c Catalog
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("notice catalog: %w", err)
	}
	if c.SchemaVersion != 1 {
		return nil, fmt.Errorf("notice catalog: schema_version %d is not supported", c.SchemaVersion)
	}
	var err error
	f := &c.TransferFilters
	for _, x := range []struct {
		name string
		expr string
		dst  **regexp.Regexp
	}{{"announcement", f.Announcement, &f.announcement}, {"mixed_include", f.MixedInclude, &f.mixedInclude},
		{"exclude", f.Exclude, &f.exclude}, {"title_trailing", f.TitleTrailing, &f.titleTrailing}} {
		if *x.dst, err = compile("transfer_filters."+x.name, x.expr); err != nil {
			return nil, err
		}
	}
	schools := map[string]bool{}
	for _, s := range c.Schools {
		schools[s.ID] = true
	}
	seen := map[string]bool{}
	for i := range c.Sources {
		s := &c.Sources[i]
		if s.ID == "" || seen[s.ID] {
			return nil, fmt.Errorf("notice catalog: source %d: missing or duplicate id %q", i, s.ID)
		}
		seen[s.ID] = true
		if s.Kind != KindSchool && s.Kind != KindTransfer {
			return nil, fmt.Errorf("notice catalog: %s: kind must be school or transfer", s.ID)
		}
		if s.Kind == KindSchool && !schools[s.School] {
			return nil, fmt.Errorf("notice catalog: %s: school %q is not in schools", s.ID, s.School)
		}
		if s.OfficialURL != "" {
			// shown as a link in the console and on Discord buttons: web addresses only
			if u, err := url.Parse(s.OfficialURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return nil, fmt.Errorf("notice catalog: %s: official_url must be an http(s) address", s.ID)
			}
		}
		if s.Manual {
			continue
		}
		u, err := url.Parse(s.ListURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || !hostAllowed(u.Hostname(), s.AllowedHosts) {
			return nil, fmt.Errorf("notice catalog: %s: list_url must be http(s) on one of allowed_hosts", s.ID)
		}
		if s.List.LinkSelector == "" {
			return nil, fmt.Errorf("notice catalog: %s: list.link_selector required", s.ID)
		}
		if s.List.RowSelector != "" && (s.List.IDAttribute == "" || s.List.IDRegex == "") {
			return nil, fmt.Errorf("notice catalog: %s: rows need id_attribute and id_regex", s.ID)
		}
		l := s.List
		for name, expr := range map[string]string{"id_regex": l.IDRegex, "include_regex": l.IncludeRegex, "href_regex": l.HrefRegex, "url_value_regex": l.URLValueRegex} {
			if _, err := compile(s.ID+".list."+name, expr); err != nil {
				return nil, fmt.Errorf("notice catalog: %w", err)
			}
		}
		if s.MinItems <= 0 {
			s.MinItems = 1
		}
	}
	return &c, nil
}

// Source finds a board by id.
func (c *Catalog) Source(id string) (Source, bool) {
	for _, s := range c.Sources {
		if s.ID == id {
			return s, true
		}
	}
	return Source{}, false
}

// SchoolName is a school's display name ("" when unknown).
func (c *Catalog) SchoolName(id string) string {
	for _, s := range c.Schools {
		if s.ID == id {
			return s.Name
		}
	}
	return ""
}

func hostAllowed(host string, allowed []string) bool {
	host = strings.ToLower(host)
	for _, h := range allowed {
		if strings.ToLower(h) == host {
			return true
		}
	}
	return false
}
