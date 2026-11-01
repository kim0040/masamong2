//go:build !noweb

package web

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"masamong/internal/memory/corpus"
)

func TestUploadSupportedLimitAndCancellation(t *testing.T) {
	s, ts := testServer(t)
	c := client()
	csrf := signIn(t, s, ts, c)
	for _, test := range []string{"limit", "cancel"} {
		t.Run(test, func(t *testing.T) {
			var b bytes.Buffer
			mw := multipart.NewWriter(&b)
			f, _ := mw.CreateFormFile("file", "cancel.csv")
			_, _ = f.Write([]byte("Date,User,Message\n"))
			_ = mw.Close()
			r := httptest.NewRequest("POST", ts.URL+"/api/upload", &b)
			r.Header.Set("Content-Type", mw.FormDataContentType())
			r.Header.Set("X-CSRF-Token", csrf)
			u, _ := url.Parse(ts.URL)
			for _, cookie := range c.Jar.Cookies(u) {
				r.AddCookie(cookie)
			}
			if test == "limit" {
				r.ContentLength = corpus.MaxImportBytes + (1 << 20) + 1
			} else {
				ctx, cancel := context.WithCancel(r.Context())
				cancel()
				r = r.WithContext(ctx)
			}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("%d: %s", w.Code, w.Body.String())
			}
			files, err := os.ReadDir(filepath.Join(s.svc.DataRoot, "imports"))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if len(files) != 0 {
				t.Fatal("failed upload retained partial file")
			}
		})
	}
}
