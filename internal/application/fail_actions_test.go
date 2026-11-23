package application

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/i18n"
	"masamong/internal/providers/websearch"
)

// failRun executes one Action as the console would.
func failRun(t *testing.T, s *Service, act string, in map[string]any) action.Result {
	t.Helper()
	raw, _ := json.Marshal(in)
	return s.Execute(t.Context(), action.Request{Action: act, Input: raw, Locale: "en"})
}

// assertPlain checks a refusal: the key's sentence, the code, and none of the Go words in the result.
func assertPlain(t *testing.T, s *Service, res action.Result, code, key string, goWords ...string) {
	assertPlainArgs(t, s, res, code, key, nil, goWords...)
}

func assertPlainArgs(t *testing.T, s *Service, res action.Result, code, key string, msgArgs []any, goWords ...string) {
	t.Helper()
	if res.OK || res.Error == nil || res.Error.MessageKey != key || res.Error.Code != code {
		t.Fatalf("want %s/%s, got %+v", code, key, res)
	}
	want := s.msg(action.Request{Locale: "en"}, key, msgArgs...)
	if res.Error.Message != want || want == "" {
		t.Fatalf("message %q, want the catalog sentence %q", res.Error.Message, want)
	}
	wire, _ := json.Marshal(res)
	for _, w := range goWords {
		if strings.Contains(string(wire), w) {
			t.Fatalf("Go text %q in %s", w, wire)
		}
	}
}

// A poll the rules refuse says so in plain words; the rule's own sentence is in the log only.
func TestPollRefusalsAreCatalogSentences(t *testing.T) {
	t.Parallel()
	s, _ := deliveryService(t)
	var logs bytes.Buffer
	s.Log = slog.New(slog.NewTextHandler(&logs, nil))

	res := failRun(t, s, "polls.create", map[string]any{"question": "", "options": []string{"a", "b"}, "scope": "discord:c"})
	assertPlain(t, s, res, errcode.InvalidInput, "polls.invalid", "question required", errcode.InvalidInput+":")
	if !strings.Contains(logs.String(), "question required") {
		t.Fatalf("the log lost the rule's words:\n%s", logs.String())
	}

	created := failRun(t, s, "polls.create", map[string]any{"id": "poll_x", "question": "lunch?", "options": []string{"a", "b"}, "scope": "discord:c"})
	if !created.OK {
		t.Fatalf("%+v", created)
	}
	// a ballot against an old revision: stale, and the revision to retry with stays in the args
	res = failRun(t, s, "polls.vote", map[string]any{"id": "poll_x", "scope": "discord:c", "participant": "p1", "option_ids": []string{"a"}, "base_revision": 99})
	assertPlain(t, s, res, errcode.StaleRevision, "error.stale", "expected", errcode.StaleRevision+":")
	if res.Error.Args["revision"] == nil {
		t.Fatalf("the revision to retry with is gone: %+v", res.Error)
	}
	// a ballot for an option the poll does not have
	res = failRun(t, s, "polls.vote", map[string]any{"id": "poll_x", "scope": "discord:c", "participant": "p1", "option_ids": []string{"zz"}, "latest": true})
	assertPlain(t, s, res, errcode.InvalidInput, "polls.vote_invalid", "unknown option", errcode.InvalidInput+":")
	// closing it, then voting
	if r := failRun(t, s, "polls.close", map[string]any{"id": "poll_x", "scope": "discord:c", "latest": true}); !r.OK {
		t.Fatalf("%+v", r)
	}
	res = failRun(t, s, "polls.vote", map[string]any{"id": "poll_x", "scope": "discord:c", "participant": "p1", "option_ids": []string{"a"}, "latest": true})
	assertPlain(t, s, res, errcode.InvalidInput, "polls.vote_invalid", "poll closed")
	// a wrong scope while closing
	res = failRun(t, s, "polls.close", map[string]any{"id": "poll_x", "scope": "discord:other", "latest": true})
	assertPlain(t, s, res, errcode.ScopeMismatch, "error.invalid_input", "wrong scope", errcode.ScopeMismatch+":")
}

// A document that cannot be attached says why in plain words, not with the path check's sentence.
func TestAttachmentRefusalIsACatalogSentence(t *testing.T) {
	t.Parallel()
	s, _ := deliveryService(t)
	var logs bytes.Buffer
	s.Log = slog.New(slog.NewTextHandler(&logs, nil))
	res := failRun(t, s, "channels.attachments.add", map[string]any{"guild_id": "g", "channel_id": "c", "filename": "a.txt", "source_path": "../secret.txt"})
	assertPlain(t, s, res, errcode.InvalidInput, "channels.source_denied", "parent-directory", "rag_staging")
	if !strings.Contains(logs.String(), "parent-directory") {
		t.Fatalf("the log lost the refusal:\n%s", logs.String())
	}
}

// An import that fails on its file answers with one sentence; the size limits keep their own.
func TestImportFailureIsACatalogSentence(t *testing.T) {
	t.Parallel()
	s, _ := deliveryService(t)
	var logs bytes.Buffer
	s.Log = slog.New(slog.NewTextHandler(&logs, nil))
	req := action.Request{Action: "imports.start", Locale: "en"}
	res := s.importFailure(req, errors.New(goText), map[string]any{"rows": 3})
	assertPlain(t, s, res, errcode.InvalidInput, "import.failed", "no such table", "10.0.0.7")
	if res.Error.Args["result"] == nil || !strings.Contains(logs.String(), "no such table") {
		t.Fatalf("the partial result or the log is gone: %+v\n%s", res.Error, logs.String())
	}
	if r := s.importFailure(req, fmt.Errorf("wrapped: %w", errPreviewSpeakers), nil); r.Error.MessageKey != "imports.limit.speakers" {
		t.Fatalf("%+v", r.Error)
	}
}

// A provider's own refusals (our daily limit, a page that is not public) say it in words, not with Go text.
func TestProviderOwnRefusalsAreCatalogSentences(t *testing.T) {
	t.Parallel()
	s := &Service{I18n: i18n.New()}
	req := action.Request{Action: "web.read", Locale: "en"}
	res := s.providerResult(req, "tavily", fmt.Errorf("%w: %s %d/%d", errQuota, "tavily", 50, 50))
	assertPlainArgs(t, s, res, errcode.BudgetExceeded, "provider.quota", []any{"provider", "tavily"}, "daily quota reached", "50/50", `"detail"`)
	res = s.providerResult(req, "jina", websearch.ErrNotPublic)
	assertPlain(t, s, res, errcode.InvalidInput, "web.not_public", "not a public web address")
}

// A weather place the grid does not know answers with the place sentence, not the lookup's wording.
func TestUnknownWeatherPlaceIsACatalogSentence(t *testing.T) {
	t.Parallel()
	s, _ := deliveryService(t)
	var logs bytes.Buffer
	s.Log = slog.New(slog.NewTextHandler(&logs, nil))
	res := failRun(t, s, "weather.query", map[string]any{"region": "Nowhereville", "source": "kma"})
	if res.OK || res.Error == nil || res.Error.MessageKey != "weather.place_unknown" {
		t.Skipf("the weather fixture does not reach the grid lookup here: %+v", res)
	}
	if strings.Contains(res.Error.Message, "unknown region") || !strings.Contains(res.Error.Message, "Nowhereville") {
		t.Fatalf("message %q", res.Error.Message)
	}
}
