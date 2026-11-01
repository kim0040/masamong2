package application

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"masamong/internal/action"
	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/lang"
)

// images.generate (19장 A20, V1 `!이미지`): one picture from a description, by the model behind the
// `image` role (an OpenAI-compatible host with image output, e.g. OpenRouter). Requests with clearly
// sexual or violent words are refused before any call; one picture is made at a time (a second request
// waits up to imageQueueWait); how often people may ask is the tool's limits (tools.images.generate).
// Pictures are kept under <data-root>/images for imageKeepDays so Discord and the console can show them.

// Defaults; tools.images.generate.defaults overrides aspect_ratio, queue_wait_sec and timeout_sec, and
// retention.image_days how long pictures are kept.
const (
	imageQueueWait  = 60 * time.Second
	imagePromptMax  = 1200
	imageDefaultAR  = "1:1"
	imageCallTimout = 120 * time.Second
)

var (
	imageMu     = make(chan struct{}, 1) // the one picture being made
	imageIDRE   = regexp.MustCompile(`^img_[a-f0-9]{16}$`)
	aspectRatio = map[string]bool{"1:1": true, "2:3": true, "3:2": true, "3:4": true, "4:3": true, "4:5": true, "5:4": true, "9:16": true, "16:9": true, "21:9": true}
	blockedOnce sync.Once
	blocked     *regexp.Regexp
	blockedCJK  []string
)

// imageBlocked reports whether a request has a word every pack's image_blocked_words refuses. Latin
// words match whole (so "skill" is not "kill"); other scripts match anywhere.
func imageBlocked(text string) bool {
	blockedOnce.Do(func() {
		var latin []string
		for _, tag := range lang.Available() {
			p, _ := lang.Get(tag)
			for _, w := range p.Features.ImageBlockedWords {
				w = strings.ToLower(strings.TrimSpace(w))
				if w == "" {
					continue
				}
				ascii := true
				for _, r := range w {
					if r > 127 {
						ascii = false
						break
					}
				}
				if ascii {
					latin = append(latin, regexp.QuoteMeta(w))
				} else {
					blockedCJK = append(blockedCJK, w)
				}
			}
		}
		sort.Strings(latin)
		if len(latin) > 0 {
			blocked = regexp.MustCompile(`(^|[^a-z0-9])(` + strings.Join(latin, "|") + `)($|[^a-z0-9])`)
		}
	})
	lower := strings.ToLower(text)
	if blocked != nil && blocked.MatchString(lower) {
		return true
	}
	for _, w := range blockedCJK {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

func (s *Service) imagesDir() string { return filepath.Join(s.DataRoot, "images") }

// ImageFile opens a generated picture by its id (Discord attachment, console preview).
func (s *Service) ImageFile(id string) (io.ReadCloser, string, error) {
	if !imageIDRE.MatchString(id) {
		return nil, "", errors.New("bad image id")
	}
	for ext, mime := range map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".webp": "image/webp", ".gif": "image/gif"} {
		f, err := os.Open(filepath.Join(s.imagesDir(), id+ext))
		if err == nil {
			return f, mime, nil
		}
	}
	return nil, "", os.ErrNotExist
}

// secondsOr reads a duration in seconds from the input (tool defaults fill it), else def.
func secondsOr(in map[string]any, key string, def time.Duration) time.Duration {
	if n := intVal(in, key, 0); n > 0 {
		return time.Duration(min(n, 600)) * time.Second
	}
	return def
}

// pruneImages removes pictures older than retention.image_days (the retention pass).
func (s *Service) pruneImages() int {
	entries, err := os.ReadDir(s.imagesDir())
	if err != nil {
		return 0
	}
	days := s.Doc().Retention.Days()["images"]
	if days < 0 {
		return 0
	}
	cut := s.Clock.Now().AddDate(0, 0, -days)
	n := 0
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && fi.ModTime().Before(cut) && strings.HasPrefix(e.Name(), "img_") {
			if os.Remove(filepath.Join(s.imagesDir(), e.Name())) == nil {
				n++
			}
		}
	}
	return n
}

func (s *Service) images(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	title := s.msg(req, "image.title")
	prompt := strings.TrimSpace(strVal(in, "prompt", ""))
	if prompt == "" {
		return action.Fail(req, errcode.InvalidInput, "image.empty", s.msg(req, "image.empty"), nil)
	}
	if len([]rune(prompt)) > imagePromptMax {
		prompt = string([]rune(prompt)[:imagePromptMax])
	}
	if imageBlocked(prompt) {
		return action.Fail(req, errcode.InvalidInput, "image.blocked", s.msg(req, "image.blocked"), nil)
	}
	doc := s.Doc()
	m, ok := modelOf(doc, "image")
	if !ok {
		return action.WithCard(action.OK(req, map[string]any{"status": "unconfigured", "live": false}),
			action.Card{Title: title, Text: s.msg(req, "image.unconfigured"), Tone: "warn"})
	}
	ar := strVal(in, "aspect_ratio", imageDefaultAR)
	if !aspectRatio[ar] {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "aspect_ratio must be one of 1:1, 2:3, 3:2, 3:4, 4:3, 4:5, 5:4, 9:16, 16:9, 21:9", nil)
	}
	if budget := parseUSD(doc.Limits.MonthlyBudgetUSD, 0); budget > 0 {
		if _, spent, ok := s.monthSpend(); ok && spent >= budget {
			return action.Fail(req, errcode.BudgetExceeded, "summary.budget", s.msg(req, "summary.budget"), nil)
		}
	}
	if boolVal(in, "dry_run") {
		return action.OK(req, map[string]any{"status": "dry_run", "model": m.Model, "aspect_ratio": ar})
	}
	// asked for in a chat turn: drawn in the background, the turn answers at once (imagequeue.go)
	if res, ok := s.imagesAsync(ctx, req, in, prompt, ar); ok {
		return res
	}
	// one picture at a time: a request waits for the one being made, then gives up without counting
	wait, cancelWait := context.WithTimeout(ctx, secondsOr(in, "queue_wait_sec", imageQueueWait))
	defer cancelWait()
	select {
	case imageMu <- struct{}{}:
		defer func() { <-imageMu }()
	case <-wait.Done():
		return action.Fail(req, errcode.ProviderUnavailable, "image.busy", s.msg(req, "image.busy"), nil)
	}
	saved, fail := s.drawImage(ctx, doc, m, prompt, ar, secondsOr(in, "timeout_sec", imageCallTimout))
	if fail != nil {
		switch fail.kind {
		case imageFailSetup:
			return action.Fail(req, errcode.ProviderUnconfigured, "generic.error", fail.err.Error(), nil)
		case imageFailIO:
			return action.Fail(req, errcode.IO, "generic.error", fail.err.Error(), nil)
		}
		return s.providerFail(req, m.Provider, fail.err)
	}
	return s.imageResult(req, m, prompt, ar, saved)
}

// imageResult is the success result of a picture: the output the chat tool and the command card read.
func (s *Service) imageResult(req action.Request, m config.Model, prompt, ar string, saved imageSaved) action.Result {
	out := map[string]any{"status": "ok", "live": true, "image_id": saved.ID, "mime": saved.MIME, "bytes": saved.Bytes, "model": m.Model,
		"aspect_ratio": ar, "cost_usd": saved.Cost, "elapsed_ms": saved.Elapsed.Milliseconds()}
	// the prompt stays out of the posted card: the picture answers the request, and a planner-written
	// prompt (or a long one) read under it looks like the bot's notes rather than a reply
	return action.WithCard(action.OK(req, out), action.Card{Title: s.msg(req, "image.title"),
		Footer: fmt.Sprintf("%s · %s", m.Model, ar)})
}

// imageSaved is a finished picture: the file is kept under imagesDir.
type imageSaved struct {
	ID, MIME string
	Bytes    int
	Cost     float64
	Elapsed  time.Duration
}

type imageFailKind int

const (
	imageFailSetup    imageFailKind = iota // no adapter for the image role
	imageFailProvider                      // the provider call failed
	imageFailIO                            // the file could not be kept
)

type imageFail struct {
	kind imageFailKind
	err  error
}

// drawImage makes one picture with the image role's model and keeps it. The caller holds imageMu. Spend
// and the provider's health are noted here, a failed attempt included (it is still an attempt), so the
// command and a background job account the same way.
func (s *Service) drawImage(ctx context.Context, doc config.Document, m config.Model, prompt, ar string, timeout time.Duration) (imageSaved, *imageFail) {
	a, err := s.llmAdapter("image")
	if err != nil {
		return imageSaved{}, &imageFail{imageFailSetup, err}
	}
	pack := lang.For(append(append([]string{}, doc.Memory.Languages...), doc.I18n.DefaultLocale)...)
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := a.GenerateImage(callCtx, m.Model, lang.Fill(pack.Features.ImagePrompt, "request", prompt), ar)
	cost := 0.0
	if res.CostUSD != nil {
		cost = *res.CostUSD
	}
	if eng, e2 := s.MemoryEngine(); e2 == nil {
		_ = eng.Store.AddSpend("image", cost, 1) // a failed attempt is still an attempt
	}
	chat.GuardFrom(ctx).Spend(cost)
	s.noteProvider(m.Provider, err)
	if err != nil {
		return imageSaved{Cost: cost}, &imageFail{imageFailProvider, err}
	}
	id := "img_" + newOpaqueID()
	ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif"}[res.MIME]
	if err := os.MkdirAll(s.imagesDir(), 0o700); err != nil {
		return imageSaved{Cost: cost}, &imageFail{imageFailIO, err}
	}
	if err := os.WriteFile(filepath.Join(s.imagesDir(), id+ext), res.Data, 0o600); err != nil {
		return imageSaved{Cost: cost}, &imageFail{imageFailIO, err}
	}
	return imageSaved{ID: id, MIME: res.MIME, Bytes: len(res.Data), Cost: cost, Elapsed: res.Elapsed}, nil
}
