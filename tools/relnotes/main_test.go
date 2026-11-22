package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/relnotes"
)

// goodAnswer is notes the way the model should return them.
const goodAnswer = `{"user":{"ko":["수정: /환율의 기본 통화를 바로잡았습니다."],"en":["Fixed: the default currency of /환율."],"es":["Corregido: la moneda predeterminada de /환율."],"ja":["修正: /환율の既定の通貨を直しました。"],"zh-Hans":["修复：/환율的默认货币。"]},` +
	`"operator":{"ko":["새 설정 chat.transcript_chars가 있습니다."],"en":["New setting chat.transcript_chars."],"es":["Nuevo ajuste chat.transcript_chars."],"ja":["新しい設定 chat.transcript_chars があります。"],"zh-Hans":["新设置 chat.transcript_chars。"]}}`

type fakeAPI struct {
	mu       sync.Mutex
	requests []fakeRequest
	answers  []string // text of each answer in turn; the last one repeats
	status   int
}

type fakeRequest struct {
	Header http.Header
	Path   string
	Method string
	Body   apiRequest
}

func (f *fakeAPI) serve(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		raw, _ := io.ReadAll(req.Body)
		var body apiRequest
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		f.requests = append(f.requests, fakeRequest{req.Header.Clone(), req.URL.Path, req.Method, body})
		if f.status != 0 && f.status != http.StatusOK {
			w.WriteHeader(f.status)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down sk-test-secret"}}`)
			return
		}
		i := len(f.requests) - 1
		if i >= len(f.answers) {
			i = len(f.answers) - 1
		}
		out, _ := json.Marshal(map[string]any{
			"content":     []map[string]any{{"type": "text", "text": f.answers[i]}},
			"stop_reason": "end_turn",
		})
		_, _ = w.Write(out)
	}))
}

// scratchRepo makes a repository with two tagged releases, a merge, trailers and a docs commit.
func scratchRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	commit := func(msg string) {
		t.Helper()
		f := filepath.Join(root, "f.txt")
		old, _ := os.ReadFile(f)
		if err := os.WriteFile(f, append(old, []byte(msg+"\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", "-A")
		run("commit", "-q", "-m", msg)
	}
	run("init", "-q", "-b", "main")
	commit("old: before the first release")
	run("tag", "v1.0.0")
	commit("old: before the second release")
	run("tag", "v1.1.0")
	run("tag", "v1.1.1-rc1") // a pre-release tag below v1.1.1 is still a tag
	commit("fix: default currency of /환율\n\nThe base currency was swapped.\n\nCo-authored-by: Someone <someone@example.com>\nSigned-off-by: Someone <someone@example.com>")
	run("checkout", "-q", "-b", "side")
	commit("feat: chat.transcript_chars setting\n\nLength of the history, default 5000.")
	run("checkout", "-q", "main")
	run("merge", "-q", "--no-ff", "-m", "Merge branch 'side' into main", "side")
	commit("docs: touch up the wiki")
	return root
}

func newRunner(t *testing.T, root, apiURL, key string) (*runner, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return &runner{
		root: root, apiURL: apiURL, apiKey: key, client: &http.Client{Timeout: 10 * time.Second},
		stdout: &out, stderr: &errb,
		now: func() time.Time { return time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC) },
	}, &out, &errb
}

func readNote(t *testing.T, root, version string) relnotes.Note {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "release-notes", version+".json"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := relnotes.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// The whole path: commits since the previous tag go to the API as the prompt asks, the answer is
// validated, and the JSON and the release Markdown are written.
func TestGenerateWritesValidatedNotes(t *testing.T) {
	root := scratchRepo(t)
	api := &fakeAPI{answers: []string{"```json\n" + goodAnswer + "\n```"}}
	srv := api.serve(t)
	defer srv.Close()
	r, stdout, stderr := newRunner(t, root, srv.URL+"/v1/messages", "sk-test-secret")

	if code := r.run(context.Background(), options{version: "v1.2.0"}); code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if len(api.requests) != 1 {
		t.Fatalf("%d API calls, want 1", len(api.requests))
	}
	req := api.requests[0]
	if req.Method != "POST" || req.Path != "/v1/messages" {
		t.Errorf("%s %s", req.Method, req.Path)
	}
	if req.Header.Get("x-api-key") != "sk-test-secret" || req.Header.Get("anthropic-version") != "2023-06-01" || req.Header.Get("content-type") != "application/json" {
		t.Errorf("headers %v", req.Header)
	}
	if req.Body.Model != "claude-sonnet-5-5" || req.Body.System != prompt || len(req.Body.Messages) != 1 || req.Body.Messages[0].Role != "user" {
		t.Errorf("request body %+v", req.Body)
	}
	if !strings.Contains(req.Body.System, "Return one JSON object and nothing else") {
		t.Error("the fixed prompt (tools/relnotes/prompt.md) is not what is sent")
	}
	in := req.Body.Messages[0].Content
	for _, want := range []string{"New version: v1.2.0", "Previous version: v1.1.0", "fix: default currency of /환율", "The base currency was swapped.", "feat: chat.transcript_chars setting", "docs: touch up the wiki"} {
		if !strings.Contains(in, want) {
			t.Errorf("commit text lacks %q:\n%s", want, in)
		}
	}
	for _, bad := range []string{"Merge branch", "Co-authored-by", "Signed-off-by", "someone@example.com", "before the second release", "before the first release"} {
		if strings.Contains(in, bad) {
			t.Errorf("commit text must not hold %q:\n%s", bad, in)
		}
	}

	n := readNote(t, root, "v1.2.0")
	if n.Version != "v1.2.0" || n.Date != "2026-12-01" {
		t.Errorf("version and date come from the tool, not the model: %s %s", n.Version, n.Date)
	}
	if got := n.UserItems("ja"); len(got) != 1 || !strings.Contains(got[0], "修正") {
		t.Errorf("ja user items %v", got)
	}
	md, err := os.ReadFile(filepath.Join(root, "release-notes", "v1.2.0.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(md) != relnotes.RenderRelease(n, "v1.1.0") {
		t.Errorf("the Markdown is not the rendering of the JSON:\n%s", md)
	}
	if !strings.Contains(string(md), "compare/v1.1.0...v1.2.0") {
		t.Errorf("Markdown lacks the changelog link:\n%s", md)
	}
	if strings.Contains(stdout.String()+stderr.String(), "sk-test-secret") {
		t.Error("the key must never be printed")
	}
}

func TestSinceFlagAndPreReleaseTags(t *testing.T) {
	root := scratchRepo(t)
	api := &fakeAPI{answers: []string{goodAnswer}}
	srv := api.serve(t)
	defer srv.Close()
	r, _, stderr := newRunner(t, root, srv.URL, "k")
	// the pre-release tag v1.1.1-rc1 is not "the previous release"
	if got := r.previousTag("v1.1.1"); got != "v1.1.0" {
		t.Errorf("previousTag(v1.1.1) = %q", got)
	}
	if got := r.previousTag("v1.0.0"); got != "" {
		t.Errorf("previousTag(v1.0.0) = %q", got)
	}
	if code := r.run(context.Background(), options{version: "v1.2.0", since: "v1.0.0"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if in := api.requests[0].Body.Messages[0].Content; !strings.Contains(in, "before the second release") || !strings.Contains(in, "Previous version: v1.0.0") {
		t.Errorf("--since v1.0.0 must include the commits after v1.0.0:\n%s", in)
	}
	r2, _, stderr2 := newRunner(t, root, srv.URL, "k")
	if code := r2.run(context.Background(), options{version: "v1.3.0", since: "v9.9.9", force: true}); code != 1 || !strings.Contains(stderr2.String(), "does not exist") {
		t.Errorf("an unknown --since tag is an error: %d %s", code, stderr2)
	}
}

// A first answer that breaks the rules gets one more try with the validation error.
func TestInvalidAnswerIsRetriedOnce(t *testing.T) {
	root := scratchRepo(t)
	bad := `{"user":{"ko":["수정: 고침 https://example.com"],"en":["Fixed: x https://example.com"]},"operator":{"ko":[],"en":[]}}`
	api := &fakeAPI{answers: []string{bad, goodAnswer}}
	srv := api.serve(t)
	defer srv.Close()
	r, _, stderr := newRunner(t, root, srv.URL, "k")
	if code := r.run(context.Background(), options{version: "v1.2.0"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if len(api.requests) != 2 {
		t.Fatalf("%d API calls, want 2", len(api.requests))
	}
	second := api.requests[1].Body.Messages
	if len(second) != 3 || second[1].Role != "assistant" || !strings.Contains(second[2].Content, "only https://github.com/kim0040/masamong2 is allowed") {
		t.Errorf("the retry must carry the answer and the validation error: %+v", second)
	}
	_ = readNote(t, root, "v1.2.0")
}

func TestInvalidTwiceWritesDraftAndExits2(t *testing.T) {
	root := scratchRepo(t)
	api := &fakeAPI{answers: []string{`not json at all`}}
	srv := api.serve(t)
	defer srv.Close()
	r, _, stderr := newRunner(t, root, srv.URL, "k")
	if code := r.run(context.Background(), options{version: "v1.2.0"}); code != 2 {
		t.Fatalf("exit %d, want 2: %s", code, stderr)
	}
	if len(api.requests) != 2 {
		t.Errorf("%d API calls, want 2", len(api.requests))
	}
	assertDraft(t, root, stderr.String())
}

func TestAPIErrorWritesDraftWithoutLeakingTheKey(t *testing.T) {
	root := scratchRepo(t)
	api := &fakeAPI{status: http.StatusTooManyRequests, answers: []string{""}}
	srv := api.serve(t)
	defer srv.Close()
	r, stdout, stderr := newRunner(t, root, srv.URL, "sk-test-secret")
	if code := r.run(context.Background(), options{version: "v1.2.0"}); code != 2 {
		t.Fatalf("exit %d, want 2: %s", code, stderr)
	}
	if !strings.Contains(stderr.String(), "429") || !strings.Contains(stderr.String(), "rate_limit_error") {
		t.Errorf("the API error must be reported: %s", stderr)
	}
	if strings.Contains(stdout.String()+stderr.String(), "sk-test-secret") {
		t.Errorf("the key leaked:\n%s\n%s", stdout, stderr)
	}
	assertDraft(t, root, stderr.String())
}

// Without a key the model is not called; the commit subjects are written as a draft and the exit is non-zero.
func TestNoKeyWritesDraftAndExits2(t *testing.T) {
	root := scratchRepo(t)
	api := &fakeAPI{answers: []string{goodAnswer}}
	srv := api.serve(t)
	defer srv.Close()
	r, _, stderr := newRunner(t, root, srv.URL, "")
	if code := r.run(context.Background(), options{version: "v1.2.0"}); code != 2 {
		t.Fatalf("exit %d, want 2: %s", code, stderr)
	}
	if len(api.requests) != 0 {
		t.Error("no key means no API call")
	}
	if !strings.Contains(stderr.String(), "ANTHROPIC_API_KEY is not set") {
		t.Errorf("stderr: %s", stderr)
	}
	assertDraft(t, root, stderr.String())
}

func assertDraft(t *testing.T, root, stderr string) {
	t.Helper()
	if !strings.Contains(stderr, "a draft was written, not release notes") || !strings.Contains(stderr, "TODO") {
		t.Errorf("the message must say the draft needs writing: %s", stderr)
	}
	data, err := os.ReadFile(filepath.Join(root, "release-notes", "v1.2.0.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := relnotes.Parse(data); err == nil {
		t.Error("a draft must not validate until it is written by hand")
	}
	var n relnotes.Note
	if err := json.Unmarshal(data, &n); err != nil {
		t.Fatal(err)
	}
	if len(n.User["en"]) != 0 || len(n.User["ko"]) != 0 {
		t.Errorf("a draft has nothing under user: %v", n.User)
	}
	want := []string{"TODO: fix: default currency of /환율", "TODO: feat: chat.transcript_chars setting", "TODO: docs: touch up the wiki"}
	sort.Strings(want)
	for _, lang := range []string{"en", "ko"} {
		got := append([]string(nil), n.Operator[lang]...)
		sort.Strings(got) // commits made within one second have no fixed order
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("operator.%s = %v", lang, n.Operator[lang])
		}
	}
	for _, bad := range []string{"Merge branch", "Co-authored-by"} {
		if strings.Contains(string(data), bad) {
			t.Errorf("the draft holds %q", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "release-notes", "v1.2.0.md")); err != nil {
		t.Error("the draft Markdown is written too")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	root := scratchRepo(t)
	api := &fakeAPI{answers: []string{goodAnswer}}
	srv := api.serve(t)
	defer srv.Close()
	r, stdout, stderr := newRunner(t, root, srv.URL, "k")
	if code := r.run(context.Background(), options{version: "v1.2.0", dryRun: true}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "release-notes")); err == nil {
		t.Error("--dry-run must not create release-notes/")
	}
	for _, want := range []string{"--- v1.2.0.json", `"version": "v1.2.0"`, "--- v1.2.0.md", "## What's new"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	// without a key a dry run still prints the draft and exits 2
	r2, out2, _ := newRunner(t, root, srv.URL, "")
	if code := r2.run(context.Background(), options{version: "v1.2.0", dryRun: true}); code != 2 || !strings.Contains(out2.String(), "TODO: ") {
		t.Errorf("dry run without a key: exit %d, stdout %s", code, out2)
	}
	if _, err := os.Stat(filepath.Join(root, "release-notes")); err == nil {
		t.Error("--dry-run must not create release-notes/")
	}
}

// Hand-edited notes are never replaced by accident.
func TestExistingNotesNeedForce(t *testing.T) {
	root := scratchRepo(t)
	api := &fakeAPI{answers: []string{goodAnswer}}
	srv := api.serve(t)
	defer srv.Close()
	dir := filepath.Join(root, "release-notes")
	_ = os.MkdirAll(dir, 0o755)
	mine := filepath.Join(dir, "v1.2.0.json")
	_ = os.WriteFile(mine, []byte("hand written"), 0o644)

	r, _, stderr := newRunner(t, root, srv.URL, "k")
	if code := r.run(context.Background(), options{version: "v1.2.0"}); code != 1 || !strings.Contains(stderr.String(), "already exists") {
		t.Errorf("exit %d: %s", code, stderr)
	}
	if b, _ := os.ReadFile(mine); string(b) != "hand written" {
		t.Error("the file was touched")
	}
	if len(api.requests) != 0 {
		t.Error("no API call (no cost) when the file exists")
	}
	if code := r.run(context.Background(), options{version: "v1.2.0", force: true}); code != 0 {
		t.Fatalf("--force exit %d", code)
	}
	_ = readNote(t, root, "v1.2.0")
}

func TestRenderOnlyRewritesMarkdown(t *testing.T) {
	root := scratchRepo(t)
	n := relnotes.Note{Version: "v1.1.0", Date: "2026-11-04",
		User:     map[string][]string{"ko": {"가"}, "en": {"A"}},
		Operator: map[string][]string{"ko": {}, "en": {}}}
	data, _ := relnotes.Marshal(n)
	_ = os.MkdirAll(filepath.Join(root, "release-notes"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "release-notes", "v1.1.0.json"), data, 0o644)

	r, _, stderr := newRunner(t, root, "http://127.0.0.1:1", "") // no key, no server: --render needs neither
	if code := r.run(context.Background(), options{version: "v1.1.0", render: true, since: "v1.0.0"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	md, _ := os.ReadFile(filepath.Join(root, "release-notes", "v1.1.0.md"))
	if string(md) != relnotes.RenderRelease(n, "v1.0.0") {
		t.Errorf("md:\n%s", md)
	}
	// an invalid JSON is not rendered
	_ = os.WriteFile(filepath.Join(root, "release-notes", "v1.1.0.json"), []byte(`{"version":"v1.1.0"}`), 0o644)
	if code := r.run(context.Background(), options{version: "v1.1.0", render: true}); code != 1 {
		t.Errorf("an invalid note must fail --render, exit %d", code)
	}
}

func TestBadArguments(t *testing.T) {
	r, _, stderr := newRunner(t, t.TempDir(), "http://127.0.0.1:1", "k")
	for _, o := range []options{{}, {version: "1.2.0"}, {version: "v1.2"}, {version: "v1.2.0", since: "last"}} {
		stderr.Reset()
		if code := r.run(context.Background(), o); code != 1 || stderr.Len() == 0 {
			t.Errorf("%+v: exit %d, stderr %q", o, code, stderr)
		}
	}
}

func TestCleanBody(t *testing.T) {
	body := "First line.\n\nSecond.\n\nCo-authored-by: A <a@example.com>\nsigned-off-by: B\n🤖 Generated with a tool\n"
	if got := cleanBody(body); got != "First line.\n\nSecond." {
		t.Errorf("cleanBody = %q", got)
	}
	long := strings.Repeat("あ", maxBodyRunes+50)
	if got := cleanBody(long); len([]rune(got)) != maxBodyRunes {
		t.Errorf("a long body is cut to %d characters, got %d", maxBodyRunes, len([]rune(got)))
	}
}

func TestCommitTextDropsBodiesBeforeSubjects(t *testing.T) {
	var commits []commit
	for i := 0; i < 400; i++ {
		commits = append(commits, commit{Subject: "subject " + strings.Repeat("s", 20), Body: strings.Repeat("b", maxBodyRunes)})
	}
	got := commitText("v2.0.0", "v1.0.0", commits)
	if strings.Contains(got, "bbbb") || !strings.Contains(got, "[400] subject") {
		t.Error("when the commit text is too long, bodies go first and every subject stays")
	}
}
