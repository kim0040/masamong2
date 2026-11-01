package vision

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/hostprofile"
)

// pinDecodeSlots installs a profile with n decode slots and rebuilds the channel.
// WHY: the channel is created once, and the unset profile is one slot. A test that
// wants two overlapping decodes has to install that before the first Prepare.
func pinDecodeSlots(t *testing.T, n int) {
	t.Helper()
	prev := hostprofile.Current()
	p := prev
	p.DecodeSlots = n
	hostprofile.Apply(p)
	decodeSlotOnce = sync.Once{}
	decodeSlot = nil
	if cap(ensureDecodeSlot()) != n {
		t.Fatalf("decode slots %d", cap(decodeSlot))
	}
	t.Cleanup(func() {
		hostprofile.Apply(prev)
		decodeSlotOnce = sync.Once{}
		decodeSlot = nil
	})
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSniffRejectsWhatIsNotAPicture(t *testing.T) {
	for name, data := range map[string][]byte{
		"svg":  []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`),
		"bmp":  append([]byte("BM"), make([]byte, 60)...),
		"tiff": append([]byte("II*\x00"), make([]byte, 60)...),
		"html": []byte("<!doctype html><title>x</title>"),
	} {
		if got := Sniff(data); got != "" {
			t.Errorf("%s sniffed as %q", name, got)
		}
		if _, err := Prepare(data); ReasonOf(err) != ReasonFormat {
			t.Errorf("%s: want %s, got %v", name, ReasonFormat, err)
		}
	}
	if Sniff(pngBytes(t, 2, 2)) != "image/png" {
		t.Fatal("png not recognised")
	}
}

func TestPreparePassesSmallPicturesThroughUnchanged(t *testing.T) {
	data := pngBytes(t, 40, 30)
	img, err := Prepare(data)
	if err != nil || img.MIME != "image/png" || !bytes.Equal(img.Data, data) || img.Width != 40 || img.Height != 30 {
		t.Fatalf("small png changed: %v %s", err, img)
	}
}

func TestPrepareShrinksTheLongEdge(t *testing.T) {
	img, err := Prepare(pngBytes(t, 2400, 600))
	if err != nil {
		t.Fatal(err)
	}
	if img.MIME != "image/jpeg" || img.Width != MaxEdge || img.Height != 392 {
		t.Fatalf("want a %dx392 jpeg, got %s", MaxEdge, img)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(img.Data))
	if err != nil || max(cfg.Width, cfg.Height) > MaxEdge {
		t.Fatalf("re-encoded picture is %dx%d (%v)", cfg.Width, cfg.Height, err)
	}
	tall, err := Prepare(pngBytes(t, 300, 3000))
	if err != nil || tall.Height != MaxEdge || tall.Width != 156 {
		t.Fatalf("tall picture: %s %v", tall, err)
	}
}

func TestPrepareSendsTheFirstFrameOfAGIF(t *testing.T) {
	pal := color.Palette{color.Black, color.White}
	anim := &gif.GIF{Image: []*image.Paletted{image.NewPaletted(image.Rect(0, 0, 8, 8), pal), image.NewPaletted(image.Rect(0, 0, 8, 8), pal)}, Delay: []int{10, 10}}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, anim); err != nil {
		t.Fatal(err)
	}
	img, err := Prepare(buf.Bytes())
	if err != nil || img.MIME != "image/jpeg" || img.Width != 8 {
		t.Fatalf("gif: %s %v", img, err)
	}
}

func TestPrepareRefusesAHugeCanvas(t *testing.T) {
	// a PNG header that claims 20000×20000 pixels: refused before anything is decoded
	data := pngBytes(t, 1, 1)
	data[16], data[17], data[18], data[19] = 0, 0, 0x4e, 0x20
	data[20], data[21], data[22], data[23] = 0, 0, 0x4e, 0x20
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	if _, err := Prepare(data); ReasonOf(err) != ReasonPixels {
		t.Fatalf("huge canvas: %v", err)
	}
}

func TestPrepareRefusesAHugeJPEGOn32Bit(t *testing.T) {
	// a JPEG header (SOF0) that claims 50000×50000 pixels: 2.5 billion overflows a 32-bit int (armv7),
	// where width×height used to come out negative and pass the pixel limit
	data := []byte{0xFF, 0xD8, 0xFF, 0xC0, 0x00, 0x11, 0x08, 0xC3, 0x50, 0xC3, 0x50, 0x03,
		0x01, 0x22, 0x00, 0x02, 0x11, 0x01, 0x03, 0x11, 0x01, 0xFF, 0xDA, 0x00, 0x0C}
	if _, err := Prepare(data); ReasonOf(err) != ReasonPixels {
		t.Fatalf("huge jpeg: %v", err)
	}
}

func TestWebPPassesOnlyWithinTheLimits(t *testing.T) {
	vp8x := func(w, h int) []byte {
		b := make([]byte, 40)
		copy(b, "RIFF")
		copy(b[8:], "WEBPVP8X")
		w--
		h--
		b[24], b[25], b[26] = byte(w), byte(w>>8), byte(w>>16)
		b[27], b[28], b[29] = byte(h), byte(h>>8), byte(h>>16)
		return b
	}
	img, err := Prepare(vp8x(800, 600))
	if err != nil || img.MIME != "image/webp" || img.Width != 800 || img.Height != 600 {
		t.Fatalf("small webp: %s %v", img, err)
	}
	if _, err := Prepare(vp8x(4000, 3000)); ReasonOf(err) != ReasonWebPTooBig {
		t.Fatalf("large webp: %v", err)
	}
}

func TestFetchOnlyFromDiscordCDN(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write(pngBytes(t, 4, 4))
	}))
	t.Cleanup(srv.Close)
	f := NewFetcher(nil)
	for _, u := range []string{
		srv.URL + "/a.png", // not Discord, and not https
		"https://evil.example/a.png",
		"https://cdn.discordapp.com.evil.example/a.png",
		"https://user:pw@cdn.discordapp.com/a.png",
		"http://cdn.discordapp.com/a.png",
		"file:///etc/passwd",
	} {
		if _, err := f.Fetch(context.Background(), Ref{URL: u}); ReasonOf(err) != ReasonHost {
			t.Errorf("%s: want %s, got %v", u, ReasonHost, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("a refused address was requested %d times", hits.Load())
	}
	for _, ok := range []string{"https://cdn.discordapp.com/attachments/1/2/a.png", "https://media.discordapp.net/attachments/1/2/a.png?ex=1"} {
		if err := f.Allowed(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	// a test server stands in for the CDN only when the test says so
	local := &Fetcher{Client: f.Client, AllowHTTP: true, AllowHost: func(string) bool { return true }}
	img, err := local.Fetch(context.Background(), Ref{URL: srv.URL + "/a.png", MessageID: "m1"})
	if err != nil || img.MIME != "image/png" || img.MessageID != "m1" {
		t.Fatalf("local fetch: %s %v", img, err)
	}
}

func TestFetchRefusesOversizedDownloads(t *testing.T) {
	big := bytes.Repeat([]byte{0}, MaxBytes+10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(big) }))
	t.Cleanup(srv.Close)
	f := NewFetcher(nil)
	f.AllowHTTP, f.AllowHost = true, func(string) bool { return true }
	if _, err := f.Fetch(context.Background(), Ref{URL: srv.URL}); ReasonOf(err) != ReasonTooLarge {
		t.Fatalf("oversized download: %v", err)
	}
	if _, err := f.Fetch(context.Background(), Ref{URL: srv.URL, Size: MaxBytes + 1}); ReasonOf(err) != ReasonTooLarge {
		t.Fatalf("oversized by its reported size: %v", err)
	}
}

func TestImageType(t *testing.T) {
	for _, c := range []struct {
		ct, name string
		want     bool
	}{{"image/png", "a.png", true}, {"image/jpeg", "", true}, {"image/webp", "", true}, {"image/gif", "", true},
		{"image/svg+xml", "a.svg", false}, {"image/bmp", "", false}, {"video/mp4", "a.mp4", false}, {"", "photo.JPG", true}, {"", "doc.pdf", false}} {
		if got := ImageType(c.ct, c.name); got != c.want {
			t.Errorf("ImageType(%q,%q)=%v", c.ct, c.name, got)
		}
	}
	if strings.Contains(Image{MIME: "image/png", Width: 1, Height: 1}.String(), "http") {
		t.Fatal("log form names an address")
	}
}

// Only one picture is decoded at a time in the process: while the slot is taken, a picture that needs
// decoding waits (and gives up with its context), a picture that is refused or goes as it came does not.
func TestPrepareDecodesOnePictureAtATime(t *testing.T) {
	pinDecodeSlots(t, 1)
	decodeSlot <- struct{}{}
	released := false
	release := func() {
		if !released {
			released = true
			<-decodeSlot
		}
	}
	defer release()

	big := pngBytes(t, 2400, 600) // needs shrinking: decoded
	small := pngBytes(t, 40, 30)  // goes as it came: header checks only
	if img, err := Prepare(small); err != nil || !bytes.Equal(img.Data, small) {
		t.Fatalf("a small picture waited for the decode slot: %v", err)
	}
	if _, err := Prepare([]byte("not a picture")); ReasonOf(err) != ReasonFormat {
		t.Fatalf("a refused picture waited for the decode slot: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := PrepareContext(ctx, big); ReasonOf(err) != ReasonFetch || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a picture past its deadline: %v", err)
	}

	type result struct {
		img Image
		err error
	}
	done := make(chan result, 3)
	for i := 0; i < 3; i++ {
		go func() {
			img, err := Prepare(big)
			done <- result{img, err}
		}()
	}
	select {
	case <-done:
		t.Fatal("a picture was decoded while another held the decode slot")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	for i := 0; i < 3; i++ {
		select {
		case r := <-done:
			if r.err != nil || r.img.Width != MaxEdge {
				t.Fatalf("result %+v %v", r.img, r.err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("pictures never finished after the slot was released")
		}
	}
}

// A picture whose request is already over never starts the decode, whether the slot is free (select picks
// between a free slot and a done context at random, so it is tried until chance could not hide it) or the
// context ends in the instant the slot is taken.
func TestPrepareDoesNotDecodeForACancelledRequest(t *testing.T) {
	pinDecodeSlots(t, 1)
	big := pngBytes(t, 2400, 600)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 40; i++ {
		if img, err := PrepareContext(ctx, big); ReasonOf(err) != ReasonFetch || !errors.Is(err, context.Canceled) {
			t.Fatalf("try %d: a cancelled request was decoded: %+v, %v", i, img, err)
		}
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	afterDecodeSlot = cancel2
	defer func() { afterDecodeSlot = nil }()
	if img, err := PrepareContext(ctx2, big); ReasonOf(err) != ReasonFetch || !errors.Is(err, context.Canceled) {
		t.Fatalf("a request that ended as it took the slot was decoded: %+v, %v", img, err)
	}
	select {
	case decodeSlot <- struct{}{}: // the slot was given back
		<-decodeSlot
	default:
		t.Fatal("the decode slot was not released")
	}
}

// Two decodes overlap when the profile allows two slots, and a third waits.
// One slot keeps them in line, which is what a 512 MB host needs.
func TestPrepareDecodeSlotsFollowTheProfile(t *testing.T) {
	big := pngBytes(t, 64, 1800) // taller than MaxEdge, small enough to decode quickly once released
	for _, n := range []int{1, 2} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			pinDecodeSlots(t, n)
			release := make(chan struct{})
			var closeOnce sync.Once
			closeRelease := func() { closeOnce.Do(func() { close(release) }) }
			t.Cleanup(closeRelease)
			entered := make(chan struct{}, n+1)
			afterDecodeSlot = func() {
				entered <- struct{}{}
				<-release
			}
			t.Cleanup(func() { afterDecodeSlot = nil })

			var wg sync.WaitGroup
			errc := make(chan error, n+1)
			for i := 0; i < n+1; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := Prepare(big)
					errc <- err
				}()
			}
			for i := 0; i < n; i++ {
				select {
				case <-entered:
				case <-time.After(20 * time.Second):
					t.Fatalf("only %d of %d decodes took a slot", i, n)
				}
			}
			select {
			case <-entered:
				t.Fatal("a decode took a slot past the profile cap")
			case <-time.After(200 * time.Millisecond):
			}
			closeRelease()
			wg.Wait()
			close(errc)
			for err := range errc {
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
