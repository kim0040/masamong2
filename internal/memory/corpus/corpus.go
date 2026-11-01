// Package corpus holds the raw-observation contract the memory pipeline reads: record-kind
// classification, the masamong-raw-observation/v1 reader, Kakao CSV and Discord live adapters,
// and the per-room message ordering every later stage (leaves, search, evidence) indexes into.
//
// It ports tools/evaluation/corpus.py and experimental/rag-leaf-parent/segment.collect_messages.
// Raw inputs are read-only; bodies are returned to callers and never logged here.
package corpus

import (
	"embed"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Record kinds (docs/rag: only human_text enters windows, embeddings and search).
const (
	HumanText    = "human_text"
	SystemMarker = "system_marker"
	DeleteMarker = "delete_marker"
	MediaMarker  = "media_marker"
	BotRecord    = "bot_record"
	EmptyUnknown = "empty_unknown"
)

// markerSet is one chat-export locale's marker patterns (markers/<locale>.json).
type markerSet struct {
	del, media, system *regexp.Regexp
}

//go:embed markers/*.json
var markerFS embed.FS

var (
	markerOnce sync.Once
	markerSets map[string]*markerSet
)

// DefaultMarkerLocale is the export locale the raw corpus and live messages are classified with
// (the evaluated corpus: KakaoTalk Korean client).
const DefaultMarkerLocale = "ko"

func loadMarkers() {
	markerSets = map[string]*markerSet{}
	entries, _ := markerFS.ReadDir("markers")
	for _, e := range entries {
		raw, err := markerFS.ReadFile("markers/" + e.Name())
		if err != nil {
			panic(err)
		}
		var d struct{ Delete, Media, System []string }
		if err := json.Unmarshal(raw, &d); err != nil {
			panic("corpus: marker file " + e.Name() + ": " + err.Error())
		}
		markerSets[strings.TrimSuffix(e.Name(), ".json")] = &markerSet{
			del: regexp.MustCompile(strings.Join(d.Delete, "|")), media: regexp.MustCompile(strings.Join(d.Media, "|")),
			system: regexp.MustCompile(strings.Join(d.System, "|")),
		}
	}
}

// MarkerLocales lists the chat-export locales with marker files.
func MarkerLocales() []string {
	markerOnce.Do(loadMarkers)
	var out []string
	for k := range markerSets {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func markersFor(locale string) (*markerSet, bool) {
	markerOnce.Do(loadMarkers)
	m, ok := markerSets[locale]
	return m, ok
}

// Classify returns the record kind of one observation (corpus.classify) with the default markers.
func Classify(content string, isBot bool, priorKind string) string {
	m, _ := markersFor(DefaultMarkerLocale)
	return classifyWith(m, content, isBot, priorKind)
}

// ClassifyLocale classifies with one export locale's markers (false when the locale has no file).
func ClassifyLocale(locale, content string, isBot bool, priorKind string) (string, bool) {
	m, ok := markersFor(locale)
	if !ok {
		return "", false
	}
	return classifyWith(m, content, isBot, priorKind), true
}

func classifyWith(m *markerSet, content string, isBot bool, priorKind string) string {
	if priorKind == BotRecord || isBot {
		return BotRecord
	}
	text := strings.TrimSpace(content)
	switch {
	case text == "":
		return EmptyUnknown
	case m.del.MatchString(text):
		return DeleteMarker
	case m.media.MatchString(text):
		return MediaMarker
	case m.system.MatchString(text):
		return SystemMarker
	}
	return HumanText
}

// Message is one semantic-eligible observation as the index sees it (segment.collect_messages row).
// Room is the logical room; RoomOriginal keeps the source room when a logical merge was declared.
type Message struct {
	ObservationID string `json:"observation_id"`
	Room          string `json:"source_room_id"`
	RoomOriginal  string `json:"source_room_original"`
	ActorLabel    string `json:"actor_label"`
	ActorAccount  string `json:"actor_account"` // "" when the platform supplies no account (Kakao)
	Content       string `json:"content"`
	ParsedTime    string `json:"parsed_time"` // ISO-8601 as the source gave it; naive for Kakao
}

// ParseTime reads the ISO-8601 forms Python's datetime.fromisoformat accepts in this corpus.
// aware=false means the source carried no offset (Kakao): the returned time is in UTC only as a
// container and must not be compared with aware times without a declared zone.
func ParseTime(value string) (t time.Time, aware bool, ok bool) {
	text := strings.TrimSpace(value)
	if text == "" {
		return time.Time{}, false, false
	}
	if strings.HasSuffix(text, "Z") {
		text = strings.TrimSuffix(text, "Z") + "+00:00"
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02T15:04-07:00",
	} {
		if parsed, err := time.Parse(layout, text); err == nil {
			return parsed, true, true
		}
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999",
		"2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02",
	} {
		if parsed, err := time.Parse(layout, text); err == nil {
			return parsed, false, true
		}
	}
	return time.Time{}, false, false
}

// Timestamp is the epoch seconds the sandbox uses for ordering and windows. Naive times are read in
// UTC here; the sandbox read them in the host zone. Only differences between times of the same
// room are ever used, so the constant offset cancels (Korea has no DST).
func Timestamp(value string) (float64, bool) {
	t, _, ok := ParseTime(value)
	if !ok {
		return 0, false
	}
	return float64(t.UnixNano()) / 1e9, true
}

// OrderByRoom groups messages by logical room (sorted) and orders each room by
// (parsed_time, source_room_original, observation_id) — segment.order_messages_by_room. Different
// Discord channels are never interleaved into one timeline.
func OrderByRoom(msgs []Message) []Message {
	byRoom := map[string][]Message{}
	var rooms []string
	for _, m := range msgs {
		if _, ok := byRoom[m.Room]; !ok {
			rooms = append(rooms, m.Room)
		}
		byRoom[m.Room] = append(byRoom[m.Room], m)
	}
	sort.Strings(rooms)
	out := make([]Message, 0, len(msgs))
	for _, room := range rooms {
		rows := byRoom[room]
		sort.SliceStable(rows, func(i, j int) bool {
			a, b := rows[i], rows[j]
			if a.ParsedTime != b.ParsedTime {
				return a.ParsedTime < b.ParsedTime
			}
			if a.RoomOriginal != b.RoomOriginal {
				return a.RoomOriginal < b.RoomOriginal
			}
			return a.ObservationID < b.ObservationID
		})
		out = append(out, rows...)
	}
	return out
}

// PlatformOfRoom is the prefix before the first colon ("discord", "kakao").
func PlatformOfRoom(room string) string {
	if i := strings.Index(room, ":"); i >= 0 {
		return room[:i]
	}
	if room == "" {
		return "?"
	}
	return room
}
