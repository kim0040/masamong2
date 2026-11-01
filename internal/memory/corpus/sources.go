package corpus

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/transform"
)

// Observation is one source record with the provenance storage keeps (raw corpus v1 fields).
type Observation struct {
	ObservationID   string
	Platform        string
	Collection      string
	SourceRoom      string
	NativeMessageID string
	ActorLabel      string
	ActorAccount    string
	Content         string
	ParsedTime      string
	RecordKind      string
	// LegacyID is the ID this row would have been stored under before IDs stopped depending on the
	// file's bytes. An import skips a row whose ObservationID or LegacyID is already stored, so
	// importing a file again after an upgrade does not duplicate what the old release imported.
	LegacyID string
	// IdentityID is a versioned row identity shared by CSV and offline raw imports.
	IdentityID string
}

// Eligible reports whether the observation enters leaves/embeddings/search: human text with a body
// and a parsed time (the sandbox reads only the dated corpus; undated rows are all delete markers).
func (o Observation) Eligible() bool {
	return o.RecordKind == HumanText && strings.TrimSpace(o.Content) != "" && o.ParsedTime != ""
}

// Message converts an eligible observation into an index row. merge maps source rooms to a declared
// logical room (identity when absent). Discord rooms are never merged.
func (o Observation) Message(merge map[string]string) Message {
	logical := o.SourceRoom
	if m, ok := merge[o.SourceRoom]; ok && m != "" {
		logical = m
	}
	label := o.ActorLabel
	if label == "" {
		label = "?"
	}
	return Message{
		ObservationID: o.ObservationID,
		Room:          logical,
		RoomOriginal:  o.SourceRoom,
		ActorLabel:    label,
		ActorAccount:  o.ActorAccount,
		Content:       strings.TrimSpace(o.Content),
		ParsedTime:    o.ParsedTime,
	}
}

// ValidateMerge refuses a logical merge that would join different Discord channels.
func ValidateMerge(merge map[string]string) error {
	for src, dst := range merge {
		if (strings.HasPrefix(src, "discord:") || strings.HasPrefix(dst, "discord:")) && src != dst {
			return fmt.Errorf("discord channels are separate rooms (guild:channel) and cannot be merged: %s → %s", src, dst)
		}
	}
	return nil
}

type rawRecord struct {
	ObservationID   string  `json:"observation_id"`
	Platform        string  `json:"platform"`
	CollectionID    string  `json:"collection_id"`
	SourceRoomID    string  `json:"source_room_id"`
	NativeMessageID *string `json:"native_message_id"`
	Actor           struct {
		NativeAccountID *string `json:"native_account_id"`
		ObservedLabel   *string `json:"observed_label"`
	} `json:"actor"`
	Content *string `json:"content"`
	Time    struct {
		Parsed *string `json:"parsed"`
	} `json:"time"`
	IsBot      *int   `json:"is_bot"`
	RecordKind string `json:"record_kind"`
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ReadRawCorpus streams a masamong-raw-observation/v1 JSONL(.gz) file. rooms==nil reads every room.
// fn returning an error stops the scan.
func ReadRawCorpus(path string, rooms map[string]bool, fn func(Observation) error) error {
	return ReadRawCorpusContext(context.Background(), path, rooms, fn)
}

func ReadRawCorpusContext(ctx context.Context, path string, rooms map[string]bool, fn func(Observation) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil {
		return err
	} else if st.Size() > MaxImportBytes {
		return ErrImportTooLarge
	}
	var r io.Reader = &contextReader{ctx: ctx, r: io.LimitReader(f, MaxImportBytes+1)}
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), MaxRowBytes)
	line := 0
	counts := NewKakaoOccurrences()
	for sc.Scan() {
		line++
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var rec rawRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return fmt.Errorf("raw corpus line %d: %w", line, err)
		}
		if rooms != nil && !rooms[rec.SourceRoomID] {
			continue
		}
		content := deref(rec.Content)
		for _, field := range []string{content, rec.ObservationID, rec.Platform, rec.CollectionID, rec.SourceRoomID,
			deref(rec.NativeMessageID), deref(rec.Actor.NativeAccountID), deref(rec.Actor.ObservedLabel), deref(rec.Time.Parsed)} {
			if len(field) > MaxFieldBytes {
				return ErrFieldTooLarge
			}
		}
		isBot := rec.IsBot != nil && *rec.IsBot == 1
		obs := Observation{
			ObservationID:   rec.ObservationID,
			Platform:        rec.Platform,
			Collection:      rec.CollectionID,
			SourceRoom:      rec.SourceRoomID,
			NativeMessageID: deref(rec.NativeMessageID),
			ActorLabel:      deref(rec.Actor.ObservedLabel),
			ActorAccount:    strings.TrimSpace(deref(rec.Actor.NativeAccountID)),
			Content:         content,
			ParsedTime:      deref(rec.Time.Parsed),
			RecordKind:      Classify(content, isBot, rec.RecordKind),
		}
		if obs.Platform == "kakao" {
			n, err := counts.Next(obs.SourceRoom, obs.ParsedTime, obs.ActorLabel, obs.Content)
			if err != nil {
				return err
			}
			obs.IdentityID = KakaoRowIdentity(obs.SourceRoom, obs.ParsedTime, obs.ActorLabel, obs.Content, n)
		}
		if err := fn(obs); err != nil {
			return err
		}
	}
	return sc.Err()
}

// canonicalJSON matches Python json.dumps(value, ensure_ascii=False, separators=(",", ":")) for the
// simple values used in observation keys (strings without HTML-sensitive escaping, ints, null).
func canonicalJSON(values ...any) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		switch x := v.(type) {
		case nil:
			b.WriteString("null")
		case int:
			b.WriteString(strconv.Itoa(x))
		case string:
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			_ = enc.Encode(x)
			b.WriteString(strings.TrimRight(buf.String(), "\n"))
		default:
			raw, _ := json.Marshal(x)
			b.Write(raw)
		}
	}
	b.WriteByte(']')
	return b.String()
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// KakaoObservationID reproduces tools/data/prepare_raw.observation for a Kakao CSV row (the hash covers
// the file's bytes, so any edit of the file renumbers every row). It is the ID rows imported before
// KakaoStableObservationID are stored under, and the one the offline raw corpus uses.
func KakaoObservationID(room, fileSHA256 string, ordinal int) string {
	return sha256Hex(canonicalJSON("kakao", room, "kakao_csv", nil, fileSHA256, ordinal))
}

// KakaoStableObservationID identifies a Kakao CSV row by what it says, not by the file it came in:
// the room, the timestamp text, the speaker, the message and how many identical rows preceded
// it in the timestamp group. Editing or truncating a file and importing it again after a failed import
// therefore finds the rows already stored instead of storing them twice.
func KakaoStableObservationID(room, date, user, message string, sameBefore int) string {
	return sha256Hex(canonicalJSON("kakao", room, "kakao_csv_row", date, user, message, sameBefore))
}

// DiscordLiveObservationID is the key for a message captured from the gateway. Storage also
// deduplicates by (platform, native message id), so a message first seen in a raw table import
// and again live is stored once.
func DiscordLiveObservationID(room, messageID string) string {
	return sha256Hex(canonicalJSON("discord", room, "discord_live", messageID))
}

// isoNaive renders a naive time like Python datetime.isoformat() (microseconds only when non-zero).
func isoNaive(t string) (string, bool) {
	parsed, aware, ok := ParseTime(t)
	if !ok || aware {
		return "", false
	}
	if parsed.Nanosecond()/1000 != 0 {
		return parsed.Format("2006-01-02T15:04:05.000000"), true
	}
	return parsed.Format("2006-01-02T15:04:05"), true
}

// ErrKakaoEncoding is returned when the file is neither UTF-8 nor CP949 text.
var ErrKakaoEncoding = errors.New("kakao csv: the file is not UTF-8 or CP949 text")

// ErrKakaoHeader is returned when the CSV is not a KakaoTalk export (Date, User, Message).
var ErrKakaoHeader = errors.New("kakao csv: header must be Date,User,Message")

// ReadKakaoCSV reads a KakaoTalk CSV export for one declared room. Kakao has no account IDs and no
// time zone: actor_account stays empty and times stay naive (the room's zone is declared elsewhere).
func ReadKakaoCSV(path, collection, room string, fn func(Observation) error) (fileSHA string, rows int, err error) {
	return ReadKakaoCSVLocale(path, collection, room, DefaultMarkerLocale, fn)
}

// ReadKakaoCSVLocale reads an export made by a KakaoTalk client in markerLocale (its "photo",
// "deleted message" and join/leave lines are classified with markers/<markerLocale>.json).
func ReadKakaoCSVLocale(path, collection, room, markerLocale string, fn func(Observation) error) (fileSHA string, rows int, err error) {
	return ReadKakaoCSVContext(context.Background(), path, collection, room, markerLocale, fn)
}

const (
	MaxImportBytes int64 = 1 << 30 // 1 GiB: memory is bounded by streaming, so only the disk and the time limit an export
	MaxRowBytes          = 256 << 10
	MaxFieldBytes        = 64 << 10
)

var ErrFieldTooLarge = errors.New("import field exceeds 64 KiB")
var ErrRowTooLarge = errors.New("import row exceeds 256 KiB")
var ErrImportTooLarge = errors.New("import exceeds 1 GiB: split the export into parts (for example by year) and import them one by one")

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// csvRecordReader bounds bytes encoding/csv can accumulate for one logical row, including multiline
// quoted fields. The extra buffer accounts for encoding/csv's bounded read-ahead from adjacent rows.
type csvRecordReader struct {
	r         io.Reader
	remaining int
}

func (r *csvRecordReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, ErrRowTooLarge
	}
	if len(p) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.r.Read(p)
	r.remaining -= n
	return n, err
}

// ReadKakaoCSVContext hashes/validates encoding in one bounded pass, then parses a second pass.
// Observation IDs continue to hash the original bytes, including a BOM and CRLF.
func ReadKakaoCSVContext(ctx context.Context, path, collection, room, markerLocale string, fn func(Observation) error) (fileSHA string, rows int, err error) {
	markers, ok := markersFor(markerLocale)
	if !ok {
		return "", 0, fmt.Errorf("no KakaoTalk marker file for export locale %q (have %v)", markerLocale, MarkerLocales())
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	hash := sha256.New()
	buf := make([]byte, 64<<10)
	var tail []byte
	validUTF8 := true
	var size int64
	cr := &contextReader{ctx: ctx, r: f}
	for {
		n, err := cr.Read(buf)
		size += int64(n)
		if size > MaxImportBytes {
			return "", 0, ErrImportTooLarge
		}
		_, _ = hash.Write(buf[:n])
		if validUTF8 {
			chunk := append(tail, buf[:n]...)
			i := 0
			for i < len(chunk) && utf8.FullRune(chunk[i:]) {
				r, width := utf8.DecodeRune(chunk[i:])
				if r == utf8.RuneError && width == 1 {
					validUTF8 = false
					break
				}
				i += width
			}
			tail = append(tail[:0], chunk[i:]...)
		}
		if err == io.EOF {
			if len(tail) > 0 {
				validUTF8 = false
			}
			break
		}
		if err != nil {
			return "", 0, err
		}
	}
	fileSHA = hex.EncodeToString(hash.Sum(nil))
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fileSHA, 0, err
	}
	body := bufio.NewReader(cr)
	if bom, _ := body.Peek(3); bytes.Equal(bom, []byte("\xef\xbb\xbf")) {
		_, _ = body.Discard(3)
	}
	var decoded io.Reader = body
	if !validUTF8 {
		decoded = transform.NewReader(body, korean.EUCKR.NewDecoder())
	}
	bounded := &csvRecordReader{r: decoded, remaining: MaxRowBytes + 4096}
	rd := csv.NewReader(bounded)
	rd.FieldsPerRecord = -1
	// Python's csv module (tools/data/prepare_raw) accepts a stray quote inside a message; so do we,
	// or one odd message would reject the whole export.
	rd.LazyQuotes = true
	header, err := rd.Read()
	if err != nil {
		return fileSHA, 0, err
	}
	if len(header) != 3 || header[0] != "Date" || header[1] != "User" || header[2] != "Message" {
		return fileSHA, 0, ErrKakaoHeader
	}
	ordinal := 0
	counts := NewKakaoOccurrences()
	for {
		if err := ctx.Err(); err != nil {
			return fileSHA, ordinal, err
		}
		bounded.remaining = MaxRowBytes + 4096
		rec, err := rd.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fileSHA, ordinal, err
		}
		ordinal++
		if len(rec) != 3 {
			return fileSHA, ordinal, fmt.Errorf("kakao csv row %d: expected 3 fields", ordinal)
		}
		rowSize := 0
		for _, field := range rec {
			if len(field) > MaxFieldBytes {
				return fileSHA, ordinal, ErrFieldTooLarge
			}
			if !utf8.ValidString(field) || !validUTF8 && strings.ContainsRune(field, utf8.RuneError) {
				return fileSHA, ordinal, ErrKakaoEncoding
			}
			rowSize += len(field)
		}
		if rowSize > MaxRowBytes {
			return fileSHA, ordinal, ErrRowTooLarge
		}
		date, user, msg := rec[0], rec[1], rec[2]
		parsed, _ := isoNaive(date)
		n, err := counts.Next(room, parsed, user, msg)
		if err != nil {
			return fileSHA, ordinal, err
		}
		obs := Observation{
			ObservationID: KakaoStableObservationID(room, date, user, msg, n),
			LegacyID:      KakaoObservationID(room, fileSHA, ordinal),
			IdentityID:    KakaoRowIdentity(room, parsed, user, msg, n),
			Platform:      "kakao",
			Collection:    collection,
			SourceRoom:    room,
			ActorLabel:    user,
			Content:       msg,
			ParsedTime:    parsed,
			RecordKind:    classifyWith(markers, msg, false, ""),
		}
		if err := fn(obs); err != nil {
			return fileSHA, ordinal, err
		}
	}
	return fileSHA, ordinal, nil
}
