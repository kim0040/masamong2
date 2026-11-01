package discord

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"masamong/internal/action"
	"masamong/internal/application"
)

var _ application.ImagePoster = (*Gateway)(nil)

// bodyRecorder is a fake Discord that keeps what it was sent and answers from a script.
type bodyRecorder struct {
	mu     sync.Mutex
	bodies []string
	types  []string
	script []int // status per POST; the last one repeats
}

func (b *bodyRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(req.Body)
	b.mu.Lock()
	n := len(b.bodies)
	b.bodies = append(b.bodies, string(raw))
	b.types = append(b.types, req.Header.Get("Content-Type"))
	status := b.script[min(n, len(b.script)-1)]
	b.mu.Unlock()
	body := `{"id":"posted"}`
	if status >= 400 {
		body = `{"message":"temporary","code":0}`
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func pictureGateway(t *testing.T, script ...int) (*Gateway, *bodyRecorder, string) {
	t.Helper()
	g, _ := gatewayWithFakeDiscord(t)
	rec := &bodyRecorder{script: script}
	g.sess.Client = &http.Client{Transport: rec}
	g.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := filepath.Join(g.svc.DataRoot, "images")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	id := "img_0123456789abcdef"
	if err := os.WriteFile(filepath.Join(dir, id+".png"), []byte("\x89PNG picture-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	return g, rec, id
}

// A finished picture goes out as a reply to the request, with the file attached, nobody mentioned, and a
// retry sends the whole file again under the same nonce (the reader is rewound, not left at its end).
func TestPostPictureRepliesWithTheFileAndRetriesWholeUnderOneNonce(t *testing.T) {
	fastReplyBackoff(t)
	g, rec, id := pictureGateway(t, http.StatusInternalServerError, http.StatusOK)
	msgID, err := g.PostPicture(context.Background(), "c1", "m1", action.Card{Title: "Picture", Text: "a cat"}, id)
	if err != nil || msgID != "posted" {
		t.Fatalf("PostPicture = %q, %v", msgID, err)
	}
	if len(rec.bodies) != 2 {
		t.Fatalf("%d attempts, want 2", len(rec.bodies))
	}
	nonces := map[string]bool{}
	for i, body := range rec.bodies {
		if !strings.HasPrefix(rec.types[i], "multipart/form-data") {
			t.Fatalf("attempt %d is %q, not multipart", i, rec.types[i])
		}
		if !strings.Contains(body, "picture-bytes") {
			t.Fatalf("attempt %d lost the file", i)
		}
		for _, want := range []string{`"message_reference"`, `"message_id":"m1"`, `"enforce_nonce":true`, `"parse":[]`, "attachment://" + id + ".png"} {
			if !strings.Contains(body, want) {
				t.Fatalf("attempt %d lacks %s:\n%s", i, want, body)
			}
		}
		start := strings.Index(body, `"nonce":"`)
		if start < 0 {
			t.Fatalf("attempt %d has no nonce", i)
		}
		rest := body[start+len(`"nonce":"`):]
		nonces[rest[:strings.Index(rest, `"`)]] = true
	}
	if len(nonces) != 1 {
		t.Fatalf("a retry must keep its nonce: %v", nonces)
	}
}

func TestPostPictureWithoutTheFileIsAnError(t *testing.T) {
	g, rec, _ := pictureGateway(t, http.StatusOK)
	if _, err := g.PostPicture(context.Background(), "c1", "m1", action.Card{Title: "Picture"}, "img_ffffffffffffffff"); err == nil {
		t.Fatal("a missing picture file was reported as posted")
	}
	if len(rec.bodies) != 0 {
		t.Fatal("something was posted without the picture")
	}
}

// The request message may be gone by the time the picture is done (its author deleted it): what it
// asked for is not posted, not even without the reference, and nothing else is sent.
func TestPostPictureIsNotPostedWhenTheRequestMessageIsGone(t *testing.T) {
	fastReplyBackoff(t)
	g, _, id := pictureGateway(t, http.StatusBadRequest, http.StatusOK)
	rec := &bodyRecorder{script: []int{http.StatusBadRequest, http.StatusOK}}
	g.sess.Client = &http.Client{Transport: gone{rec}}
	_, err := g.PostPicture(context.Background(), "c1", "m-deleted", action.Card{Title: "Picture"}, id)
	if !errors.Is(err, application.ErrReplyTargetGone) {
		t.Fatalf("err = %v, want ErrReplyTargetGone", err)
	}
	if len(rec.bodies) != 1 || !strings.Contains(rec.bodies[0], `"fail_if_not_exists":true`) {
		t.Fatalf("one reply that Discord must refuse when the message is gone, and no plain post after it:\n%v", rec.bodies)
	}
}

func TestPostPictureReportsAMissingPermission(t *testing.T) {
	fastReplyBackoff(t)
	g, _, id := pictureGateway(t, http.StatusForbidden)
	rec := &bodyRecorder{script: []int{http.StatusForbidden}}
	g.sess.Client = &http.Client{Transport: forbidden{rec}}
	if _, err := g.PostPicture(context.Background(), "c1", "m1", action.Card{Title: "Picture"}, id); !errors.Is(err, application.ErrPostForbidden) {
		t.Fatalf("err = %v, want ErrPostForbidden", err)
	}
}

type forbidden struct{ *bodyRecorder }

func (f forbidden) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := f.bodyRecorder.RoundTrip(req)
	if err == nil {
		resp.Body = io.NopCloser(bytes.NewReader([]byte(`{"message":"Missing Permissions","code":50013}`)))
	}
	return resp, err
}

// gone answers a reply whose reference is missing with Discord's "unknown message_reference" refusal.
type gone struct{ *bodyRecorder }

func (g gone) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := g.bodyRecorder.RoundTrip(req)
	if err == nil && resp.StatusCode == http.StatusBadRequest {
		resp.Body = io.NopCloser(bytes.NewReader([]byte(`{"message":"Invalid Form Body","code":50035,"errors":{"message_reference":{"_errors":[{"code":"MESSAGE_REFERENCE_UNKNOWN_MESSAGE","message":"Unknown message"}]}}}`)))
	}
	return resp, err
}
