// Package decode classifies and parses the response shapes external APIs actually return.
// Content-Type headers are unreliable (KMA API Hub serves JSON as text/plain), so every decoder
// sniffs the body and validates the envelope before a feature sees a single field.
package decode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type Format string

const (
	JSON  Format = "json"
	Text  Format = "text"
	HTML  Format = "html"
	XML   Format = "xml"
	Empty Format = "empty"
)

// Sniff decides the format from the body first and the header second.
func Sniff(contentType string, body []byte) Format {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return Empty
	}
	switch trimmed[0] {
	case '{', '[':
		if json.Valid(trimmed) {
			return JSON
		}
	case '<':
		lower := bytes.ToLower(trimmed[:min(len(trimmed), 64)])
		if bytes.HasPrefix(lower, []byte("<!doctype html")) || bytes.HasPrefix(lower, []byte("<html")) {
			return HTML
		}
		return XML
	}
	ct := strings.ToLower(contentType)
	switch {
	case strings.Contains(ct, "json") && json.Valid(trimmed):
		return JSON
	case strings.Contains(ct, "html"):
		return HTML
	case strings.Contains(ct, "xml"):
		return XML
	}
	return Text
}

// ---------------------------------------------------------------------------
// KMA (기상청 API Hub) — typ02 JSON envelopes
// ---------------------------------------------------------------------------

// KMAEnvelope is the normalized view over the three JSON shapes KMA services return:
//   - openapi:  {"response":{"header":{"resultCode","resultMsg"},"body":{"items":{"item":[...]},"totalCount"}}}
//   - flat:     {"item":[...]} (API Hub V2 without the public-data envelope)
//   - error:    {"result": ..., "message": ...} or an HTML/text error page
type KMAEnvelope struct {
	Variant    string           // openapi | flat
	ResultCode string           // "00" on success; "03" NO_DATA; others are errors
	ResultMsg  string
	Items      []map[string]any // always a list, single objects are wrapped
	TotalCount int
}

var (
	ErrKMANoData    = errors.New("kma: NO_DATA")
	ErrKMAService   = errors.New("kma: service error")
	ErrUnexpected   = errors.New("unexpected response shape")
	ErrHTMLResponse = errors.New("html response where data was expected")
)

// KMAJSON parses either envelope and returns ErrKMANoData for an empty successful result.
func KMAJSON(contentType string, body []byte) (KMAEnvelope, error) {
	switch Sniff(contentType, body) {
	case HTML:
		return KMAEnvelope{}, ErrHTMLResponse
	case JSON:
	default:
		return KMAEnvelope{}, fmt.Errorf("%w: kma json expected, got %s", ErrUnexpected, Sniff(contentType, body))
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return KMAEnvelope{}, fmt.Errorf("%w: %v", ErrUnexpected, err)
	}
	if raw, ok := root["response"]; ok {
		var env struct {
			Header struct {
				ResultCode string `json:"resultCode"`
				ResultMsg  string `json:"resultMsg"`
			} `json:"header"`
			Body struct {
				Items      json.RawMessage `json:"items"`
				TotalCount json.Number     `json:"totalCount"`
			} `json:"body"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			return KMAEnvelope{}, fmt.Errorf("%w: openapi envelope: %v", ErrUnexpected, err)
		}
		out := KMAEnvelope{Variant: "openapi", ResultCode: env.Header.ResultCode, ResultMsg: env.Header.ResultMsg}
		if n, err := env.Body.TotalCount.Int64(); err == nil {
			out.TotalCount = int(n)
		}
		switch env.Header.ResultCode {
		case "00", "":
		case "03":
			return out, ErrKMANoData
		default:
			return out, fmt.Errorf("%w: %s %s", ErrKMAService, env.Header.ResultCode, env.Header.ResultMsg)
		}
		items, err := itemsFrom(env.Body.Items)
		if err != nil {
			return out, err
		}
		out.Items = items
		if len(items) == 0 {
			return out, ErrKMANoData
		}
		return out, nil
	}
	if raw, ok := root["item"]; ok {
		items, err := itemsFrom(raw)
		if err != nil {
			return KMAEnvelope{}, err
		}
		out := KMAEnvelope{Variant: "flat", ResultCode: "00", Items: items, TotalCount: len(items)}
		if len(items) == 0 {
			return out, ErrKMANoData
		}
		return out, nil
	}
	if raw, ok := root["result"]; ok {
		msg := string(root["message"])
		return KMAEnvelope{}, fmt.Errorf("%w: result=%s message=%s", ErrKMAService, bytes.TrimSpace(raw), strings.Trim(msg, `"`))
	}
	return KMAEnvelope{}, fmt.Errorf("%w: no response/item/result key", ErrUnexpected)
}

// itemsFrom accepts {"item":[...]}, {"item":{...}}, [...], {...}, "" and null.
func itemsFrom(raw json.RawMessage) ([]map[string]any, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) || bytes.Equal(raw, []byte(`""`)) {
		return nil, nil
	}
	if raw[0] == '{' {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("%w: items: %v", ErrUnexpected, err)
		}
		if inner, ok := obj["item"]; ok {
			return itemsFrom(inner)
		}
		var one map[string]any
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, fmt.Errorf("%w: item object: %v", ErrUnexpected, err)
		}
		return []map[string]any{one}, nil
	}
	if raw[0] == '[' {
		var list []map[string]any
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("%w: item list: %v", ErrUnexpected, err)
		}
		return list, nil
	}
	return nil, fmt.Errorf("%w: items is %s", ErrUnexpected, string(raw[:min(len(raw), 16)]))
}

// ---------------------------------------------------------------------------
// KMA typ01 — delimited text tables
// ---------------------------------------------------------------------------

// Table is a parsed typ01 text response. Header names are upper-cased with trailing dashes removed.
type Table struct {
	Header []string
	Rows   []map[string]string
	Raw    int // data lines seen, including rows dropped for column mismatch
}

var ErrKMATextError = errors.New("kma text: error body")

// KMAText parses the '#START7777 … #7777END' text format. Comment lines start with '#'; the header
// is the comment line that names the columns; rows are CSV when they contain commas, else whitespace.
// expected supplies column names when the service omits the header line.
func KMAText(body []byte, expected []string) (Table, error) {
	text := string(body)
	switch Sniff("", body) {
	case HTML:
		return Table{}, ErrHTMLResponse
	case JSON:
		return Table{}, fmt.Errorf("%w: json body where typ01 text was expected: %s", ErrUnexpected, firstLine(text))
	case Empty:
		return Table{}, fmt.Errorf("%w: empty typ01 body", ErrUnexpected)
	}
	// A successful typ01 response is always framed by #START7777/#7777END; anything else is an error page.
	if !strings.Contains(text, "#START7777") {
		return Table{}, fmt.Errorf("%w: %s", ErrKMATextError, firstLine(text))
	}
	t := Table{}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line == "#START7777" || line == "#7777END" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			candidate := strings.TrimSpace(strings.TrimLeft(line, "#"))
			cells := splitCells(candidate)
			if looksLikeHeader(cells, expected) {
				t.Header = normalizeHeader(cells)
			}
			continue
		}
		cells := splitCells(line)
		t.Raw++
		keys := t.Header
		if len(keys) == 0 {
			keys = expected
		}
		if len(keys) == 0 || len(cells) < min(len(keys), 2) {
			continue
		}
		row := map[string]string{}
		for i, k := range keys {
			if i < len(cells) {
				row[k] = strings.TrimSpace(cells[i])
			}
		}
		t.Rows = append(t.Rows, row)
	}
	return t, nil
}

func splitCells(s string) []string {
	if strings.Contains(s, ",") {
		parts := strings.Split(s, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		return parts
	}
	return strings.Fields(s)
}

func looksLikeHeader(cells, expected []string) bool {
	if len(cells) < 2 {
		return false
	}
	upper := normalizeHeader(cells)
	hits := 0
	for _, e := range expected {
		for _, c := range upper {
			if c == strings.ToUpper(e) {
				hits++
				break
			}
		}
	}
	if len(expected) > 0 {
		return hits >= 2
	}
	// No expectation: a header is all-caps identifiers.
	for _, c := range upper {
		if c == "" || strings.ContainsAny(c, "0123456789") {
			return false
		}
	}
	return true
}

func normalizeHeader(cells []string) []string {
	out := make([]string, len(cells))
	for i, c := range cells {
		out[i] = strings.ToUpper(strings.TrimRight(strings.TrimSpace(c), "-"))
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

// ---------------------------------------------------------------------------
// Generic JSON helpers with strict shape checks
// ---------------------------------------------------------------------------

// Object decodes a JSON object and rejects other top-level shapes.
func Object(contentType string, body []byte) (map[string]json.RawMessage, error) {
	switch Sniff(contentType, body) {
	case HTML:
		return nil, ErrHTMLResponse
	case JSON:
	case Empty:
		return nil, fmt.Errorf("%w: empty body", ErrUnexpected)
	default:
		return nil, fmt.Errorf("%w: json object expected", ErrUnexpected)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(body), &m); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnexpected, err)
	}
	return m, nil
}

// Number reads a JSON number or numeric string; APIs disagree on which they send.
func Number(raw json.RawMessage) (float64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" || s == "-" {
			return 0, false
		}
		var v float64
		if _, err := fmt.Sscanf(s, "%g", &v); err == nil {
			return v, true
		}
	}
	return 0, false
}

// Str reads a string, or stringifies a number/bool. Objects and arrays yield "".
func Str(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	if raw[0] == '{' || raw[0] == '[' {
		return ""
	}
	return string(raw)
}

// FieldStr reads a string-ish field from a generic item map.
func FieldStr(item map[string]any, key string) string {
	v, ok := item[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case bool:
		return fmt.Sprint(t)
	}
	return ""
}

// FieldNum reads a numeric field; "-", "" and KMA sentinel values count as missing.
func FieldNum(item map[string]any, key string) (float64, bool) {
	s := FieldStr(item, key)
	if s == "" || s == "-" || s == "-999" || s == "-99.0" || s == "-999.0" {
		return 0, false
	}
	var v float64
	if _, err := fmt.Sscanf(s, "%g", &v); err != nil {
		return 0, false
	}
	return v, true
}
