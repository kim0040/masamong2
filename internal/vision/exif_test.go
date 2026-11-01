package vision

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFetchRefusesARedirectToAnotherHost(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		_, _ = w.Write(pngBytes(t, 4, 4))
	}))
	t.Cleanup(other.Close)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/inside.png", http.StatusFound)
	}))
	t.Cleanup(cdn.Close)
	f := NewFetcher(nil)
	f.AllowHTTP, f.AllowHost = true, func(string) bool { return true }
	_, err := f.Fetch(context.Background(), Ref{URL: cdn.URL + "/a.png?ex=1&is=2&hm=signaturevalue"})
	if ReasonOf(err) != ReasonFetch {
		t.Fatalf("a redirect to another host was followed: %v", err)
	}
	if elsewhere.Load() != 0 {
		t.Fatal("the other host was requested")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("the refusal does not say why: %v", err)
	}
	if strings.Contains(err.Error(), "signaturevalue") {
		t.Fatal("the CDN signature reached an error message")
	}
}

func TestFetcherRedactsTheCDNSignature(t *testing.T) {
	got := NewFetcher(nil).Client.Redact("https://cdn.discordapp.com/attachments/1/2/a.png?ex=aa&is=bb&hm=cc")
	for _, secret := range []string{"=aa", "=bb", "=cc"} {
		if strings.Contains(got, secret) {
			t.Fatalf("%s left in %s", secret, got)
		}
	}
}

// exifJPEG is a w x h JPEG, red on its left half and blue on its right, with an EXIF block that holds
// the orientation and a GPS-looking string.
func exifJPEG(t *testing.T, w, h int, orientation uint16) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{R: 230, A: 255}
			if x >= w/2 {
				c = color.RGBA{B: 230, A: 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	// TIFF, little endian: IFD0 at 8 with one entry (orientation), then a GPS-looking string
	tiff := []byte("II*\x00\x08\x00\x00\x00\x01\x00")
	entry := make([]byte, 12)
	binary.LittleEndian.PutUint16(entry[0:], 0x0112)
	binary.LittleEndian.PutUint16(entry[2:], 3)
	binary.LittleEndian.PutUint32(entry[4:], 1)
	binary.LittleEndian.PutUint16(entry[8:], orientation)
	tiff = append(tiff, entry...)
	tiff = append(tiff, 0, 0, 0, 0)
	tiff = append(tiff, []byte("GPSLatitude 37.5665N")...)
	payload := append([]byte("Exif\x00\x00"), tiff...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(payload)+2))
	seg = append(seg, payload...)
	raw := buf.Bytes()
	out := append([]byte{}, raw[:2]...)
	out = append(out, seg...)
	return append(out, raw[2:]...)
}

func TestPrepareDropsEXIFAndTurnsThePhotoUpright(t *testing.T) {
	// orientation 6: the stored picture is shown turned a quarter clockwise, so 40x20 becomes 20x40 and
	// the red left half ends up on top
	data := exifJPEG(t, 40, 20, 6)
	if o, has := jpegMeta(data); !has || o != 6 {
		t.Fatalf("metadata not found: %d %v", o, has)
	}
	img, err := Prepare(data)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(img.Data, []byte("Exif")) || bytes.Contains(img.Data, []byte("GPSLatitude")) {
		t.Fatal("EXIF reached the model's copy")
	}
	if img.Width != 20 || img.Height != 40 {
		t.Fatalf("not turned: %dx%d", img.Width, img.Height)
	}
	dec, err := jpeg.Decode(bytes.NewReader(img.Data))
	if err != nil {
		t.Fatal(err)
	}
	if r, _, b, _ := dec.At(10, 5).RGBA(); r < b {
		t.Fatalf("top is not the red half: %v", dec.At(10, 5))
	}
	if r, _, b, _ := dec.At(10, 35).RGBA(); b < r {
		t.Fatalf("bottom is not the blue half: %v", dec.At(10, 35))
	}
	// the same photo without the EXIF block goes as it came
	n := int(binary.BigEndian.Uint16(data[4:6]))
	plain := append(append([]byte{}, data[:2]...), data[4+n:]...)
	if _, has := jpegMeta(plain); has {
		t.Fatal("metadata found in a plain JPEG")
	}
	if img, err := Prepare(plain); err != nil || !bytes.Equal(img.Data, plain) {
		t.Fatalf("plain JPEG changed: %v", err)
	}
}

func TestOrientCoversEveryEXIFValue(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 3, 2)) // pixel (x,y) holds R=x+1, G=y+1
	for y := 0; y < 2; y++ {
		for x := 0; x < 3; x++ {
			src.Set(x, y, color.RGBA{R: uint8(x + 1), G: uint8(y + 1), A: 255})
		}
	}
	// where the stored top-left pixel (0,0) is shown
	want := map[int][2]int{1: {0, 0}, 2: {2, 0}, 3: {2, 1}, 4: {0, 1}, 5: {0, 0}, 6: {1, 0}, 7: {1, 2}, 8: {0, 2}}
	for o, at := range want {
		dst := orient(src, o)
		if c := dst.RGBAAt(at[0], at[1]); c.R != 1 || c.G != 1 {
			t.Errorf("orientation %d: (0,0) not at %v", o, at)
		}
		if b := dst.Bounds(); (o >= 5) != (b.Dx() == 2) {
			t.Errorf("orientation %d: size %v", o, b)
		}
	}
}

func TestPrepareRefusesMoreThanSixteenMegapixels(t *testing.T) {
	// 4100×4100 is 16.8 MP: the header alone is enough to refuse it
	data := pngBytes(t, 1, 1)
	binary.BigEndian.PutUint32(data[16:20], 4100)
	binary.BigEndian.PutUint32(data[20:24], 4100)
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	if _, err := Prepare(data); ReasonOf(err) != ReasonPixels {
		t.Fatalf("16.8 MP canvas: %v", err)
	}
}
