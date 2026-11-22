// relnotes drafts the release notes of a version from the commits since the previous tag.
//
//	go run ./tools/relnotes --version v1.6.0 [--since v1.5.1] [--dry-run]
//
// It sends the commit subjects and bodies (merges and trailers left out) to the Anthropic Messages API
// with the fixed prompt in prompt.md, which sorts the changes into what Discord users feel and what only
// the operator sees and writes short sentences in every console language. The answer is validated by
// internal/relnotes and written to release-notes/<version>.json, with the GitHub release text beside it
// as release-notes/<version>.md. Read and edit both before tagging; the release workflow publishes them.
//
// The API key comes from the ANTHROPIC_API_KEY environment variable only. Without it (or when the call
// fails) the tool writes a draft that lists the commit subjects as "TODO:" lines under operator, says so,
// and exits 2: the draft does not validate until it is written by hand.
//
//	--since       previous tag (default: the highest tag below --version)
//	--date        release date, YYYY-MM-DD (default: today)
//	--dry-run     write nothing; print the JSON and Markdown (the API is still called when a key is set)
//	--force       replace release-notes/<version>.json when it already exists
//	--render      only rewrite release-notes/<version>.md from the existing .json (no commits, no API call)
//
// Exit status: 0 written, 1 error, 2 a draft was written (or printed) and needs writing.
package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"masamong/internal/relnotes"
)

//go:embed prompt.md
var prompt string

const (
	defaultAPIURL = "https://api.anthropic.com/v1/messages"
	model         = "claude-sonnet-5-5"
	apiVersion    = "2023-06-01"
	maxTokens     = 8192

	maxBodyRunes  = 1200   // one commit body
	maxInputRunes = 120000 // all commit text sent
	draftRunes    = 140    // a commit subject in a draft line
)

type options struct {
	version, since, date  string
	dryRun, force, render bool
}

// runner holds what a run depends on, so tests can point it at a fake server and a scratch repository.
type runner struct {
	root           string
	apiURL         string
	apiKey         string
	client         *http.Client
	stdout, stderr io.Writer
	now            func() time.Time
}

func main() {
	var o options
	flag.StringVar(&o.version, "version", "", "version to write notes for, vMAJOR.MINOR.PATCH (required)")
	flag.StringVar(&o.since, "since", "", "previous tag (default: the highest tag below --version)")
	flag.StringVar(&o.date, "date", "", "release date YYYY-MM-DD (default: today)")
	flag.BoolVar(&o.dryRun, "dry-run", false, "write nothing; print the notes")
	flag.BoolVar(&o.force, "force", false, "replace an existing release-notes/<version>.json")
	flag.BoolVar(&o.render, "render", false, "only rewrite release-notes/<version>.md from the existing .json")
	flag.Parse()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "relnotes: run it inside the repository:", err)
		os.Exit(1)
	}
	r := &runner{
		root:   strings.TrimSpace(string(out)),
		apiURL: defaultAPIURL,
		apiKey: strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")),
		client: &http.Client{Timeout: 5 * time.Minute},
		stdout: os.Stdout, stderr: os.Stderr,
		now: time.Now,
	}
	os.Exit(r.run(context.Background(), o))
}

var versionArg = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)

func (r *runner) run(ctx context.Context, o options) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(r.stderr, "relnotes: "+format+"\n", a...)
		return 1
	}
	if !versionArg.MatchString(o.version) {
		return fail("--version must look like v1.6.0, got %q", o.version)
	}
	if o.since != "" && !versionArg.MatchString(o.since) {
		return fail("--since must look like v1.5.1, got %q", o.since)
	}
	date := o.date
	if date == "" {
		date = r.now().Format("2006-01-02")
	}
	dir := filepath.Join(r.root, "release-notes")
	jsonPath := filepath.Join(dir, o.version+".json")
	mdPath := filepath.Join(dir, o.version+".md")

	prev := o.since
	if prev == "" {
		prev = r.previousTag(o.version)
	}

	if o.render {
		data, err := os.ReadFile(jsonPath)
		if err != nil {
			return fail("%v", err)
		}
		n, err := relnotes.Parse(data)
		if err != nil {
			return fail("%s: %v", jsonPath, err)
		}
		return r.write(o, "", mdPath, nil, relnotes.RenderRelease(n, prev), "")
	}

	if _, err := os.Stat(jsonPath); err == nil && !o.force && !o.dryRun {
		return fail("%s already exists; edit it by hand, or pass --force to replace it", jsonPath)
	}
	commits, err := r.commits(prev)
	if err != nil {
		return fail("%v", err)
	}
	if len(commits) == 0 {
		return fail("no commits since %s", orStart(prev))
	}

	note := relnotes.Note{Version: o.version, Date: date}
	reason := ""
	switch {
	case r.apiKey == "":
		reason = "ANTHROPIC_API_KEY is not set, so nothing was drafted by the model"
	default:
		n, err := r.draftWithModel(ctx, note, prev, commits)
		if err != nil {
			reason = "the model call did not give valid notes: " + err.Error()
		} else {
			note = n
		}
	}
	if reason != "" {
		note = draft(note, commits)
	}
	data, err := relnotes.Marshal(note)
	if err != nil {
		return fail("%v", err)
	}
	code := r.write(o, jsonPath, mdPath, data, relnotes.RenderRelease(note, prev), reason)
	if code == 0 && reason != "" {
		return 2
	}
	return code
}

func orStart(prev string) string {
	if prev == "" {
		return "the start"
	}
	return prev
}

// write stores (or, with --dry-run, prints) the JSON and Markdown. jsonPath and data are empty for --render.
func (r *runner) write(o options, jsonPath, mdPath string, data []byte, md, reason string) int {
	if o.dryRun {
		if data != nil {
			fmt.Fprintf(r.stdout, "--- %s.json\n%s\n", o.version, data)
		}
		fmt.Fprintf(r.stdout, "--- %s.md\n%s", o.version, md)
	} else {
		if err := os.MkdirAll(filepath.Dir(mdPath), 0o755); err != nil {
			fmt.Fprintln(r.stderr, "relnotes:", err)
			return 1
		}
		if jsonPath != "" {
			if err := os.WriteFile(jsonPath, data, 0o644); err != nil {
				fmt.Fprintln(r.stderr, "relnotes:", err)
				return 1
			}
			fmt.Fprintln(r.stdout, "wrote", rel(r.root, jsonPath))
		}
		if err := os.WriteFile(mdPath, []byte(md), 0o644); err != nil {
			fmt.Fprintln(r.stderr, "relnotes:", err)
			return 1
		}
		fmt.Fprintln(r.stdout, "wrote", rel(r.root, mdPath))
	}
	if reason != "" {
		fmt.Fprintf(r.stderr, "relnotes: a draft was written, not release notes: %s.\n"+
			"  The draft lists the commit subjects as TODO lines under operator. Replace them with short sentences\n"+
			"  (user: what people feel in Discord; operator: install, console, config, internals; ko and en required),\n"+
			"  then run go test ./internal/relnotes and go run ./tools/relnotes --version %s --render --force.\n", reason, o.version)
	}
	return 0
}

func rel(root, p string) string {
	if s, err := filepath.Rel(root, p); err == nil {
		return filepath.ToSlash(s)
	}
	return p
}

// draft fills a note with the commit subjects: nothing under user, a TODO line per subject under operator
// (ko and en alike), so it does not validate until someone writes it.
func draft(n relnotes.Note, commits []commit) relnotes.Note {
	var items []string
	seen := map[string]bool{}
	for _, c := range commits {
		s := truncate(c.Subject, draftRunes)
		if seen[s] {
			continue
		}
		seen[s] = true
		items = append(items, "TODO: "+s)
	}
	n.User = map[string][]string{"ko": {}, "en": {}}
	n.Operator = map[string][]string{"ko": append([]string(nil), items...), "en": items}
	return n
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// ---- commits ----

type commit struct{ Subject, Body string }

func (r *runner) git(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", r.root}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// previousTag is the highest release tag below version ("" when there is none). Pre-release tags
// (v1.6.0-rc1) are skipped: the notes of v1.6.0 cover everything since the last release.
func (r *runner) previousTag(version string) string {
	out, err := r.git("tag", "--list", "v*")
	if err != nil {
		return ""
	}
	best := ""
	for _, t := range strings.Fields(out) {
		v, ok := relnotes.ParseVersion(t)
		if !ok || v.After || v.Pre != "" || relnotes.Compare(t, version) >= 0 {
			continue
		}
		if best == "" || relnotes.Compare(t, best) > 0 {
			best = t
		}
	}
	return best
}

// commits lists the commits after prev up to HEAD, oldest first, without merges and trailers.
func (r *runner) commits(prev string) ([]commit, error) {
	rng := "HEAD"
	if prev != "" {
		if _, err := r.git("rev-parse", "--verify", "--quiet", prev+"^{commit}"); err != nil {
			return nil, fmt.Errorf("tag %s does not exist; pass --since with an existing tag", prev)
		}
		rng = prev + "..HEAD"
	}
	out, err := r.git("log", "--no-merges", "--reverse", "--format=%s%x1f%b%x1e", rng)
	if err != nil {
		return nil, err
	}
	var list []commit
	for _, rec := range strings.Split(out, "\x1e") {
		subject, body, _ := strings.Cut(rec, "\x1f")
		subject = strings.TrimSpace(subject)
		if subject == "" {
			continue
		}
		list = append(list, commit{Subject: subject, Body: cleanBody(body)})
	}
	return list, nil
}

var trailerRE = regexp.MustCompile(`(?i)^(co-authored-by|signed-off-by|reviewed-by|acked-by|tested-by|reported-by|suggested-by|helped-by|change-id|cc)\s*:`)

// cleanBody drops trailer lines (Co-authored-by: …) and attribution footers from a commit body.
func cleanBody(body string) string {
	var keep []string
	for _, l := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(l)
		if trailerRE.MatchString(t) || strings.HasPrefix(t, "🤖") || strings.HasPrefix(strings.ToLower(t), "generated with") {
			continue
		}
		keep = append(keep, strings.TrimRight(l, " \t"))
	}
	return truncate(strings.TrimSpace(strings.Join(keep, "\n")), maxBodyRunes)
}

// commitText is the user message: version, previous version and the commits, oldest first. Bodies go
// first when the whole is too long for the model; subjects are kept to the end.
func commitText(version, prev string, commits []commit) string {
	build := func(withBodies bool) string {
		var b strings.Builder
		fmt.Fprintf(&b, "New version: %s\nPrevious version: %s\nCommits since then (%d, oldest first):\n", version, orStart(prev), len(commits))
		for i, c := range commits {
			fmt.Fprintf(&b, "\n[%d] %s\n", i+1, c.Subject)
			if withBodies && c.Body != "" {
				b.WriteString(c.Body + "\n")
			}
		}
		return b.String()
	}
	if s := build(true); utf8.RuneCountInString(s) <= maxInputRunes {
		return s
	}
	return truncate(build(false), maxInputRunes)
}

// ---- the model call ----

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type apiRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system"`
	Messages  []message `json:"messages"`
}

type apiResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// draftWithModel asks for the notes, validates them, and asks once more with the validation error when
// they do not pass.
func (r *runner) draftWithModel(ctx context.Context, base relnotes.Note, prev string, commits []commit) (relnotes.Note, error) {
	msgs := []message{{Role: "user", Content: commitText(base.Version, prev, commits)}}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		text, err := r.call(ctx, msgs)
		if err != nil {
			return relnotes.Note{}, err
		}
		n, err := parseAnswer(base, text)
		if err == nil {
			return n, nil
		}
		lastErr = err
		msgs = append(msgs,
			message{Role: "assistant", Content: text},
			message{Role: "user", Content: "That did not pass validation: " + err.Error() + "\nReturn the corrected JSON object only."})
	}
	return relnotes.Note{}, lastErr
}

var fenceRE = regexp.MustCompile("(?s)^\\s*```[a-zA-Z]*\\s*(.*?)\\s*```\\s*$")

// parseAnswer reads the model's JSON (a code fence or text around it is forgiven), fills in the version
// and date, and validates the result.
func parseAnswer(base relnotes.Note, text string) (relnotes.Note, error) {
	if m := fenceRE.FindStringSubmatch(text); m != nil {
		text = m[1]
	}
	i, j := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if i < 0 || j < i {
		return relnotes.Note{}, errors.New("the answer holds no JSON object")
	}
	var got struct {
		User     map[string][]string `json:"user"`
		Operator map[string][]string `json:"operator"`
	}
	dec := json.NewDecoder(strings.NewReader(text[i : j+1]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		return relnotes.Note{}, fmt.Errorf("the answer is not the expected JSON: %v", err)
	}
	n := base
	n.User, n.Operator = got.User, got.Operator
	if err := n.Validate(); err != nil {
		return relnotes.Note{}, err
	}
	return n, nil
}

func (r *runner) call(ctx context.Context, msgs []message) (string, error) {
	body, err := json.Marshal(apiRequest{Model: model, MaxTokens: maxTokens, System: prompt, Messages: msgs})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.apiURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", r.apiKey)
	req.Header.Set("anthropic-version", apiVersion)
	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("the API request failed: %v", scrub(err.Error(), r.apiKey))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("reading the API answer: %v", err)
	}
	var ar apiResponse
	_ = json.Unmarshal(raw, &ar)
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		if ar.Error != nil {
			msg = ar.Error.Type + ": " + ar.Error.Message
		}
		return "", fmt.Errorf("the API answered %d: %s", resp.StatusCode, truncate(scrub(msg, r.apiKey), 300))
	}
	if ar.StopReason == "max_tokens" {
		return "", errors.New("the answer was cut off (max_tokens); shorten the commit range with --since")
	}
	var text strings.Builder
	for _, c := range ar.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	if strings.TrimSpace(text.String()) == "" {
		return "", errors.New("the API answer has no text")
	}
	return text.String(), nil
}

func scrub(s, key string) string {
	if key != "" {
		s = strings.ReplaceAll(s, key, "[key]")
	}
	return s
}
