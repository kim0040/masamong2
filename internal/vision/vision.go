// Package vision fetches and prepares the pictures a member attaches when they call the bot, so a chat
// model can look at them. Only what the bot was called with is read (the member's own message, the
// message it replies to, a forwarded message): nothing scans the room's images in the background.
//
// Pictures are downloaded only from Discord's CDN, checked by their first bytes (not by the name or the
// type Discord reports), and made small enough for a model request. The bytes live in memory for the
// turn only: nothing here writes them to disk or to the database.
package vision

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"masamong/internal/hostprofile"
	"masamong/internal/providers"
)

// Limits of one turn's pictures.
const (
	// MaxImages is how many pictures one turn reads; the rest are left out.
	MaxImages = 3
	// MaxBytes is the largest download accepted (Discord's own upload limit for most servers is 10 MB).
	MaxBytes = 10 << 20
	// MaxEdge is the longest side sent to a model: larger pictures are scaled down (vision models tile
	// or shrink anything bigger anyway, at the member's cost).
	MaxEdge = 1568
	// MaxSendBytes is the largest picture sent as it came; a larger one is re-encoded as JPEG.
	MaxSendBytes = 1536 << 10
	// maxPixels refuses a picture whose decoded size would take hundreds of megabytes of memory
	// (a small file can claim a huge canvas).
	maxPixels = 16_000_000
	// FetchTimeout bounds one download.
	FetchTimeout = 10 * time.Second
)

// Ref is a picture attached to a Discord message, as the gateway saw it. Nothing is downloaded yet.
type Ref struct {
	URL         string
	ContentType string // what Discord reported; the bytes decide
	Filename    string
	Size        int
	// MessageID is the message the picture is attached to (the member's message, the one it replies
	// to). A deletion of that message removes what the bot wrote about the picture.
	MessageID string
	// AuthorID is who posted the picture ("" when Discord does not say). A picture by someone other than
	// the caller who withdrew memory consent is not read.
	AuthorID string
}

// Image is a prepared picture: bytes a model accepts, and their media type.
type Image struct {
	MIME          string
	Data          []byte
	Width, Height int
	MessageID     string
}

// Skip reasons, recorded on the turn (no picture content, no address).
const (
	ReasonHost        = "host_refused"       // not Discord's CDN
	ReasonTooLarge    = "too_large"          // over MaxBytes
	ReasonFormat      = "unsupported_format" // not PNG, JPEG, WebP or GIF by its bytes
	ReasonFetch       = "fetch_failed"       // the download failed or timed out
	ReasonDecode      = "decode_failed"      // the bytes claim a format but do not decode
	ReasonPixels      = "too_many_pixels"    // the canvas is too large to decode safely
	ReasonWebPTooBig  = "webp_too_large"     // WebP over the size limits: no decoder here to shrink it
	ReasonOff         = "off"                // chat.vision is off
	ReasonNoModel     = "no_vision_model"    // no configured chat model takes pictures
	ReasonOverLimit   = "over_limit"         // more than MaxImages on the turn
	ReasonDescribeErr = "describe_failed"    // the describing call failed for another reason
	ReasonConsent     = "author_withdrew"    // posted by someone else who withdrew memory consent
)

// Error is a picture that was not used, with the reason the turn records.
type Error struct {
	Reason string
	Err    error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return "vision: " + e.Reason + ": " + e.Err.Error()
	}
	return "vision: " + e.Reason
}

func (e *Error) Unwrap() error { return e.Err }

// ReasonOf is the skip reason of err ("fetch_failed" for an error that names none).
func ReasonOf(err error) string {
	var ve *Error
	if errors.As(err, &ve) {
		return ve.Reason
	}
	return ReasonFetch
}

// DiscordHost reports Discord's attachment CDN hosts, the only ones a picture is downloaded from.
func DiscordHost(host string) bool {
	switch strings.ToLower(strings.TrimSuffix(host, ".")) {
	case "cdn.discordapp.com", "media.discordapp.net":
		return true
	}
	return false
}

// Fetcher downloads and prepares pictures.
type Fetcher struct {
	Client *providers.Client
	// AllowHost decides which hosts may serve a picture (nil = DiscordHost). Only https is fetched
	// unless AllowHTTP is set (tests with a local server).
	AllowHost func(host string) bool
	AllowHTTP bool
}

// NewFetcher is a Fetcher for Discord's CDN with the package limits.
func NewFetcher(log *slog.Logger) *Fetcher {
	c := providers.NewClient(FetchTimeout, log)
	c.MaxBody = MaxBytes
	c.MaxRetries = 1
	// Discord's signed attachment links: the expiry and signature are not written to logs or errors
	c.SecretParams = append(c.SecretParams, "ex", "is", "hm")
	return &Fetcher{Client: c}
}

// Allowed checks a picture's address before anything is sent: https, a CDN host, no credentials.
func (f *Fetcher) Allowed(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return &Error{Reason: ReasonHost}
	}
	if u.Scheme != "https" && !(f.AllowHTTP && u.Scheme == "http") {
		return &Error{Reason: ReasonHost}
	}
	allow := f.AllowHost
	if allow == nil {
		allow = DiscordHost
	}
	if !allow(u.Hostname()) {
		return &Error{Reason: ReasonHost}
	}
	return nil
}

// Fetch downloads one picture and prepares it.
func (f *Fetcher) Fetch(ctx context.Context, ref Ref) (Image, error) {
	if err := f.Allowed(ref.URL); err != nil {
		return Image{}, err
	}
	if ref.Size > MaxBytes {
		return Image{}, &Error{Reason: ReasonTooLarge}
	}
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	resp, err := f.Client.Get(ctx, ref.URL, nil)
	if err != nil {
		var pe *providers.Error
		if errors.As(err, &pe) && pe.Kind == "too_large" {
			return Image{}, &Error{Reason: ReasonTooLarge}
		}
		return Image{}, &Error{Reason: ReasonFetch, Err: err}
	}
	img, err := PrepareContext(ctx, resp.Body)
	img.MessageID = ref.MessageID
	return img, err
}

// Sniff names the picture format by its first bytes: image/png, image/jpeg, image/gif, image/webp, or
// "" for anything else (SVG, BMP, TIFF, HEIC, text).
func Sniff(b []byte) string {
	switch {
	case len(b) >= 8 && bytes.Equal(b[:8], []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		return "image/gif"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// Prepare checks the bytes and makes them fit a model request: the long edge at most MaxEdge, at most
// MaxSendBytes. A picture already within both and without metadata goes as it came; one with metadata
// (EXIF GPS, camera) is re-encoded without it, upright by its EXIF orientation. GIF is sent as its first frame (not
// every model takes GIF). WebP cannot be decoded with the standard library, so it goes as it came when
// it is within the limits and is skipped otherwise.
func Prepare(data []byte) (Image, error) { return PrepareContext(context.Background(), data) }

// decodeSlot lets a limited number of pictures through the decode-and-re-encode step at once.
// A 12-16 Mpx photo takes 125-170 MB while it is decoded (the decoded image, the flattened RGBA,
// the rotated copy), and a turn can carry several pictures while several turns run at once: on a
// 512 MB machine two of those is the whole memory, so the small and medium tiers keep one slot.
// A large host (4+ cores and at least 6 GiB) allows two. The channel is created on the first decode,
// not at init: the daemon installs the profile in main, after this package has already been loaded.
// The header checks above it are cheap and stay outside, so a refused picture never waits.
var (
	decodeSlotOnce sync.Once
	decodeSlot     chan struct{}
)

func ensureDecodeSlot() chan struct{} {
	decodeSlotOnce.Do(func() {
		n := hostprofile.Current().DecodeSlots
		// WHY: a zero cap would block every decode forever. Medium, the unset profile, is one.
		if n < 1 {
			n = 1
		}
		decodeSlot = make(chan struct{}, n)
	})
	return decodeSlot
}

// afterDecodeSlot lets a test act between taking the slot and starting the decode; nil outside tests.
var afterDecodeSlot func()

// PrepareContext is Prepare for a caller with a context: waiting for the decode slot ends with it.
func PrepareContext(ctx context.Context, data []byte) (Image, error) {
	mime := Sniff(data)
	if mime == "" {
		return Image{}, &Error{Reason: ReasonFormat}
	}
	if mime == "image/webp" {
		w, h, ok := webpSize(data)
		if !ok {
			return Image{}, &Error{Reason: ReasonDecode}
		}
		if max(w, h) > MaxEdge || len(data) > MaxSendBytes {
			return Image{}, &Error{Reason: ReasonWebPTooBig}
		}
		return Image{MIME: mime, Data: data, Width: w, Height: h}, nil
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		// On 32-bit builds (armv7) the PNG decoder itself refuses a canvas whose pixel buffer would not
		// fit an int; that is the same "too many pixels" as the check below, not broken bytes.
		if strings.Contains(err.Error(), "dimension overflow") {
			return Image{}, &Error{Reason: ReasonPixels, Err: err}
		}
		return Image{}, &Error{Reason: ReasonDecode, Err: err}
	}
	// int64: width×height of a hostile header overflows a 32-bit int and would pass as a negative number
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return Image{}, &Error{Reason: ReasonPixels}
	}
	orientation, meta := metaOf(mime, data) // exif.go: metadata never goes to a model
	if mime != "image/gif" && !meta && max(cfg.Width, cfg.Height) <= MaxEdge && len(data) <= MaxSendBytes {
		return Image{MIME: mime, Data: data, Width: cfg.Width, Height: cfg.Height}, nil
	}
	// a request that is already over must not start a 100+ MB decode: select picks at random when the slot
	// is free and the context is done, so each side is checked on its own
	if err := ctx.Err(); err != nil {
		return Image{}, &Error{Reason: ReasonFetch, Err: err}
	}
	slot := ensureDecodeSlot()
	select {
	case slot <- struct{}{}:
		defer func() { <-slot }()
	case <-ctx.Done():
		return Image{}, &Error{Reason: ReasonFetch, Err: ctx.Err()}
	}
	if afterDecodeSlot != nil {
		afterDecodeSlot()
	}
	if err := ctx.Err(); err != nil { // it ended while it waited and the slot came free in the same instant
		return Image{}, &Error{Reason: ReasonFetch, Err: err}
	}
	var src image.Image
	switch mime {
	case "image/gif":
		src, err = gif.Decode(bytes.NewReader(data)) // the first frame only
	case "image/png":
		src, err = png.Decode(bytes.NewReader(data))
	default:
		src, err = jpeg.Decode(bytes.NewReader(data))
	}
	if err != nil {
		return Image{}, &Error{Reason: ReasonDecode, Err: err}
	}
	flat := orient(flatten(src), orientation)
	if b := flat.Bounds(); max(b.Dx(), b.Dy()) > MaxEdge {
		flat = shrink(flat, MaxEdge)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, flat, &jpeg.Options{Quality: 85}); err != nil {
		return Image{}, &Error{Reason: ReasonDecode, Err: err}
	}
	b := flat.Bounds()
	return Image{MIME: "image/jpeg", Data: buf.Bytes(), Width: b.Dx(), Height: b.Dy()}, nil
}

// flatten draws the picture onto white (JPEG has no transparency) as RGBA starting at 0,0.
func flatten(src image.Image) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Over)
	return dst
}

// shrink scales src down so its long edge is edge pixels, averaging every source pixel that falls into
// a destination pixel (a box filter: no aliasing on photos and screenshots, and no extra dependency).
func shrink(src *image.RGBA, edge int) *image.RGBA {
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	dw, dh := edge, edge
	if sw >= sh {
		dh = max(1, sh*edge/sw)
	} else {
		dw = max(1, sw*edge/sh)
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for dy := 0; dy < dh; dy++ {
		y0, y1 := dy*sh/dh, max((dy+1)*sh/dh, dy*sh/dh+1)
		for dx := 0; dx < dw; dx++ {
			x0, x1 := dx*sw/dw, max((dx+1)*sw/dw, dx*sw/dw+1)
			var r, g, bl, a, n uint64
			for y := y0; y < y1; y++ {
				row := src.Pix[y*src.Stride+x0*4 : y*src.Stride+x1*4]
				for i := 0; i < len(row); i += 4 {
					r += uint64(row[i])
					g += uint64(row[i+1])
					bl += uint64(row[i+2])
					a += uint64(row[i+3])
					n++
				}
			}
			o := dy*dst.Stride + dx*4
			dst.Pix[o], dst.Pix[o+1], dst.Pix[o+2], dst.Pix[o+3] = uint8(r/n), uint8(g/n), uint8(bl/n), uint8(a/n)
		}
	}
	return dst
}

// webpSize reads a WebP picture's width and height from its header (VP8, VP8L or VP8X chunk).
func webpSize(b []byte) (w, h int, ok bool) {
	if len(b) < 30 {
		return 0, 0, false
	}
	switch string(b[12:16]) {
	case "VP8 ":
		// frame tag (3 bytes), start code 9d 01 2a, then 14-bit width and height
		if b[23] != 0x9d || b[24] != 0x01 || b[25] != 0x2a {
			return 0, 0, false
		}
		w = int(binary.LittleEndian.Uint16(b[26:28]) & 0x3fff)
		h = int(binary.LittleEndian.Uint16(b[28:30]) & 0x3fff)
	case "VP8L":
		if b[20] != 0x2f {
			return 0, 0, false
		}
		bits := binary.LittleEndian.Uint32(b[21:25])
		w = int(bits&0x3fff) + 1
		h = int((bits>>14)&0x3fff) + 1
	case "VP8X":
		w = int(uint32(b[24])|uint32(b[25])<<8|uint32(b[26])<<16) + 1
		h = int(uint32(b[27])|uint32(b[28])<<8|uint32(b[29])<<16) + 1
	default:
		return 0, 0, false
	}
	return w, h, w > 0 && h > 0
}

// ImageType reports whether an attachment is a picture this package can read, by the type Discord
// reported or, when that is empty, by the file name.
func ImageType(contentType, filename string) bool {
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	switch ct {
	case "image/png", "image/jpeg", "image/jpg", "image/webp", "image/gif":
		return true
	case "":
		name := strings.ToLower(filename)
		for _, ext := range []string{".png", ".jpg", ".jpeg", ".webp", ".gif"} {
			if strings.HasSuffix(name, ext) {
				return true
			}
		}
	}
	return false
}

// String is a body-free description for logs: format and size, never the address.
func (i Image) String() string {
	return fmt.Sprintf("%s %dx%d %dB", i.MIME, i.Width, i.Height, len(i.Data))
}
