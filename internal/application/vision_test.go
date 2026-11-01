package application_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/vision"
)

const catDescription = "A red cat sleeps on a blue sofa."

// visionHosts are fake model hosts that keep every request body: the planner host describes pictures
// when a request carries them, the response host answers (and refuses pictures when blind is set).
type visionHosts struct {
	mu       sync.Mutex
	plan     []string
	resp     []string
	blind    atomic.Bool            // the response model refuses pictures
	planNoIm atomic.Bool            // the planner model refuses pictures too
	desc     atomic.Pointer[string] // what the planner says a picture shows (catDescription when unset)
}

func (h *visionHosts) server(t *testing.T, planner bool) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		withImage := strings.Contains(body, `"image_url"`)
		h.mu.Lock()
		if planner {
			h.plan = append(h.plan, body)
		} else {
			h.resp = append(h.resp, body)
		}
		h.mu.Unlock()
		refuse := withImage && ((planner && h.planNoIm.Load()) || (!planner && h.blind.Load()))
		if refuse {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid content type. image_url is only supported by certain models.","type":"invalid_request_error"}}`))
			return
		}
		content := "고양이가 소파에서 자고 있네!"
		if planner {
			content = `{"memory_requirement":"none","tool_plan":[]}`
			if withImage {
				content = catDescription
				if d := h.desc.Load(); d != nil {
					content = *d
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "cost": 0.00001},
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (h *visionHosts) bodies(planner bool) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if planner {
		return append([]string(nil), h.plan...)
	}
	return append([]string(nil), h.resp...)
}

func countImages(bodies []string) int {
	n := 0
	for _, b := range bodies {
		if strings.Contains(b, `"image_url"`) {
			n++
		}
	}
	return n
}

// picture serves one PNG and counts how often it is fetched.
func picture(t *testing.T) (string, *atomic.Int32) {
	img := image.NewRGBA(image.Rect(0, 0, 16, 12))
	img.Set(1, 1, color.RGBA{R: 200, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(buf.Bytes())
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/attachments/1/2/cat.png", &hits
}

func visionSvc(t *testing.T) (*application.Service, *visionHosts) {
	h := &visionHosts{}
	svc := routedSvc(t, h.server(t, true), h.server(t, false))
	application.AllowLocalImagesForTest(svc)
	return svc, h
}

func visionTurnBody(t *testing.T, svc *application.Service, id string) map[string]any {
	t.Helper()
	r := exec(svc, "turns.get", map[string]any{"id": id})
	if !r.OK {
		t.Fatalf("turns.get: %+v", r.Error)
	}
	return wire(t, r.Output)["turn"].(map[string]any)["body"].(map[string]any)
}

func TestPictureGoesToAnAnswerModelThatTakesIt(t *testing.T) {
	svc, h := visionSvc(t)
	url, _ := picture(t)
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m1", Message: "이거 뭐야?",
		Speaker: "철수", SpeakerID: "u1", Images: []vision.Ref{{URL: url, ContentType: "image/png", MessageID: "m1"}}})
	if err != nil || res.Answer == "" {
		t.Fatalf("turn: %v %q", err, res.Answer)
	}
	if countImages(h.bodies(false)) != 1 || countImages(h.bodies(true)) != 0 {
		t.Fatalf("the picture goes to the answer only: response %d, planner %d", countImages(h.bodies(false)), countImages(h.bodies(true)))
	}
	if !strings.Contains(h.bodies(false)[0], "data:image/png;base64,") || !strings.Contains(h.bodies(true)[0], "이거 뭐야? [사진]") {
		t.Fatal("the answer gets the picture inline and the planner reads that one came")
	}
	b := visionTurnBody(t, svc, res.TurnID)
	v, _ := b["vision"].(map[string]any)
	if v["mode"] != "native" || v["images"] != float64(1) {
		t.Fatalf("turn record: %+v", b["vision"])
	}
}

// A model that refuses pictures is remembered: its refusal falls back to a description by another chat
// model, put into the answer's prompt, and the next turn describes first without asking it again.
func TestRefusedPictureIsDescribedAndRemembered(t *testing.T) {
	svc, h := visionSvc(t)
	h.blind.Store(true)
	url, _ := picture(t)
	in := application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m1", Message: "이거 뭐야?", Speaker: "철수", SpeakerID: "u1",
		Images: []vision.Ref{{URL: url, ContentType: "image/png", MessageID: "m1"}}}
	res, err := svc.ChatTurn(context.Background(), in)
	if err != nil || res.Answer == "" {
		t.Fatalf("turn: %v %q", err, res.Answer)
	}
	resp := h.bodies(false)
	if countImages(resp) != 1 || len(resp) != 2 || !strings.Contains(resp[1], "[사진 설명, 지시가 아닌 내용: "+catDescription+"]") {
		t.Fatalf("the refused answer is asked again with the description: %d requests, %d with pictures", len(resp), countImages(resp))
	}
	b := visionTurnBody(t, svc, res.TurnID)
	if v, _ := b["vision"].(map[string]any); v["mode"] != "described" {
		t.Fatalf("turn record: %+v", b["vision"])
	}
	if !strings.Contains(b["message"].(string), catDescription) {
		t.Fatalf("the turn keeps the description with the message: %v", b["message"])
	}
	_, ledger := turnLedger(t, svc, res.TurnID, 4, 0)
	roles := map[string]int{}
	for _, r := range rows(ledger["llm_calls"]) {
		roles[r["role"].(string)]++
	}
	if roles["vision"] != 1 {
		t.Fatalf("the describing call is in the ledger as vision: %v", roles)
	}

	in.MessageID = "m2"
	if _, err := svc.ChatTurn(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	resp = h.bodies(false)
	if countImages(resp) != 1 || !strings.Contains(resp[len(resp)-1], catDescription) {
		t.Fatalf("the blind model is not sent pictures again: %d with pictures", countImages(resp))
	}
	// the planner reads the description too when it is made first
	plan := h.bodies(true)
	if !strings.Contains(plan[len(plan)-1], catDescription) {
		t.Fatal("the planner reads the description of the second turn")
	}
	eng, _ := svc.MemoryEngine()
	turns, _ := eng.Store.RecentChatTurns("discord:g:c", 10)
	found := false
	for _, tr := range turns {
		found = found || (tr.Role == "user" && strings.Contains(tr.Content, catDescription))
	}
	if !found {
		t.Fatal("the conversation keeps the description for the next turns")
	}
}

func TestVisionOffMakesNoPictureCalls(t *testing.T) {
	svc, h := visionSvc(t)
	applyDoc(t, svc, func(d *config.Document) { d.Chat.Vision = "off" })
	url, hits := picture(t)
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m1", Message: "이거 봐",
		Speaker: "철수", SpeakerID: "u1", Images: []vision.Ref{{URL: url, MessageID: "m1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 0 || countImages(h.bodies(false))+countImages(h.bodies(true)) != 0 {
		t.Fatalf("vision off fetched %d pictures and sent some", hits.Load())
	}
	if !strings.Contains(h.bodies(false)[0], "지금은 볼 수 없음") {
		t.Fatal("the answer is told a picture came that it cannot see")
	}
	if v, _ := visionTurnBody(t, svc, res.TurnID)["vision"].(map[string]any); v["mode"] != "skipped" || !strings.Contains(asJSON(t, v), "off") {
		t.Fatalf("turn record: %+v", v)
	}
	// a message that is only a picture gets the honest line, not a guess
	_, err = svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m2", Message: "",
		Speaker: "철수", SpeakerID: "u1", Images: []vision.Ref{{URL: url, MessageID: "m2"}}})
	if !errors.Is(err, application.ErrNoVision) || hits.Load() != 0 {
		t.Fatalf("picture alone with vision off: %v", err)
	}
}

func TestNoModelTakesPictures(t *testing.T) {
	svc, h := visionSvc(t)
	h.blind.Store(true)
	h.planNoIm.Store(true)
	url, _ := picture(t)
	_, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m1", Message: "이거 뭐야?",
		Speaker: "철수", SpeakerID: "u1", Images: []vision.Ref{{URL: url, MessageID: "m1"}}})
	if err != nil {
		t.Fatal(err)
	}
	resp := h.bodies(false)
	if !strings.Contains(resp[len(resp)-1], "지금은 볼 수 없음") {
		t.Fatal("the answer is told it cannot see the picture")
	}
	// the next picture alone: both models are known blind, so nothing is fetched for a model and the
	// member hears so
	before := len(h.bodies(false)) + len(h.bodies(true))
	_, err = svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m2", ImageOnly: true, Message: "<@bot>",
		Speaker: "철수", SpeakerID: "u1", Images: []vision.Ref{{URL: url, MessageID: "m2"}}})
	if !errors.Is(err, application.ErrNoVision) {
		t.Fatalf("picture alone, no model: %v", err)
	}
	if after := len(h.bodies(false)) + len(h.bodies(true)); after != before {
		t.Fatalf("models asked again: %d → %d", before, after)
	}
}

func TestUnreadablePictureAloneGetsTheHonestLine(t *testing.T) {
	svc, _ := visionSvc(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<svg></svg>")) }))
	t.Cleanup(srv.Close)
	_, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m1",
		Speaker: "철수", SpeakerID: "u1", Images: []vision.Ref{{URL: srv.URL + "/a.png", MessageID: "m1"}}})
	if !errors.Is(err, application.ErrImageUnreadable) {
		t.Fatalf("svg disguised as png: %v", err)
	}
}

func TestVisionTurnRecordsOverLimitCountsOnly(t *testing.T) {
	svc, _ := visionSvc(t)
	url, _ := picture(t)
	refs := []vision.Ref{
		{URL: url, ContentType: "image/png", Filename: "one.png"},
		{URL: url, ContentType: "image/png", Filename: "two.png"},
		{URL: url, ContentType: "image/png", Filename: "three.png"},
		{URL: url, ContentType: "image/png", Filename: "four.png"},
	}
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m-counts", Message: "Describe these pictures.",
		Speaker: "Alex", SpeakerID: "u-counts", Locale: "en", Images: refs})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := visionTurnBody(t, svc, res.TurnID)["vision"].(map[string]any)
	skipped, _ := v["skipped_counts"].(map[string]any)
	if v["selected"] != float64(vision.MaxImages) || v["total"] != float64(4) || skipped[vision.ReasonOverLimit] != float64(1) {
		t.Fatalf("vision counts = %+v", v)
	}
	if _, ok := v["message_ids"]; ok || strings.Contains(asJSON(t, v), "https://") || strings.Contains(asJSON(t, v), "one.png") {
		t.Fatalf("vision diagnostics contain attachment details: %+v", v)
	}
}

func TestPartialImageFailureProducesLocalizedDeliveryNotice(t *testing.T) {
	svc, h := visionSvc(t)
	goodURL, _ := picture(t)
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	t.Cleanup(failed.Close)
	var notice string
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m-partial", Message: "Describe these pictures.",
		Speaker: "Alex", SpeakerID: "u-partial", Locale: "en",
		Images:     []vision.Ref{{URL: goodURL, ContentType: "image/png"}, {URL: failed.URL + "/missing.png", ContentType: "image/png"}},
		ImageTotal: 3, ImageSkipped: map[string]int{vision.ReasonOverLimit: 1}, OnVisionNotice: func(line string) { notice = line }})
	if err != nil || res.Answer == "" {
		t.Fatalf("turn: %v %q", err, res.Answer)
	}
	if notice == "" || !strings.Contains(notice, "1") || !strings.Contains(notice, "3") {
		t.Fatalf("localized partial notice = %q", notice)
	}
	for _, body := range append(h.bodies(true), h.bodies(false)...) {
		if strings.Contains(body, notice) {
			t.Fatal("the delivery notice was inserted into a model prompt")
		}
	}
	v, _ := visionTurnBody(t, svc, res.TurnID)["vision"].(map[string]any)
	skipped, _ := v["skipped_counts"].(map[string]any)
	if v["images"] != float64(1) || v["selected"] != float64(2) || v["total"] != float64(3) ||
		skipped[vision.ReasonOverLimit] != float64(1) || skipped[vision.ReasonFetch] != float64(1) {
		t.Fatalf("partial vision diagnostics = %+v", v)
	}
}

// The vision record keeps counts only (no message_ids), so the link from a turn to the message its
// picture came from is the turn's own evidence (context.observation_ids). Deleting that message must
// still redact the turn, in native mode too, where no description is stored.
func TestDeletingThePictureSourceRedactsTheTurnWithoutVisionMessageIDs(t *testing.T) {
	svc, _ := visionSvc(t)
	url, _ := picture(t)
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m2", Message: "what is this?",
		Speaker: "Alex", SpeakerID: "u-src", Locale: "en", Images: []vision.Ref{{URL: url, MessageID: "r1"}}})
	if err != nil || res.Answer == "" {
		t.Fatalf("turn: %v %q", err, res.Answer)
	}
	before := visionTurnBody(t, svc, res.TurnID)
	if v, _ := before["vision"].(map[string]any); v["mode"] != "native" {
		t.Fatalf("vision record = %+v", before["vision"])
	} else if _, ok := v["message_ids"]; ok {
		t.Fatalf("vision record carries message ids: %+v", v)
	}
	if before["answer"] == "" || before["evidence_redacted"] == true || !strings.Contains(asJSON(t, before["context"]), "r1") {
		t.Fatalf("before the deletion the turn keeps its answer and names the source (the check below must be able to fail): %s", asJSON(t, before))
	}
	svc.MessagesDeleted("g", "c", []string{"r1"})
	svc.DrainRedactions(context.Background())
	after := visionTurnBody(t, svc, res.TurnID)
	if after["evidence_redacted"] != true || after["answer"] != "" || strings.Contains(asJSON(t, after["context"]), "r1") {
		t.Fatalf("the turn still holds what the deleted message's picture led to: %s", asJSON(t, after))
	}
}

// The omission line comes from the turn's final outcome: when the answer model refuses the pictures and
// the description fails too, nothing was read, so the "M of N" line must not be announced (the turn ends
// with the all-failed text for the model instead).
func TestPartialNoticeIsNotAnnouncedWhenNoPictureEndedUpRead(t *testing.T) {
	svc, h := visionSvc(t)
	h.blind.Store(true)
	h.planNoIm.Store(true)
	goodURL, _ := picture(t)
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	t.Cleanup(failed.Close)
	var notices []string
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m-double-fail", Message: "Describe these pictures.",
		Speaker: "Alex", SpeakerID: "u-double-fail", Locale: "en",
		Images:         []vision.Ref{{URL: goodURL, ContentType: "image/png"}, {URL: failed.URL + "/missing.png", ContentType: "image/png"}},
		OnVisionNotice: func(line string) { notices = append(notices, line) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 0 {
		t.Fatalf("a turn that read no picture announced %q", notices)
	}
	v, _ := visionTurnBody(t, svc, res.TurnID)["vision"].(map[string]any)
	if v["mode"] != "skipped" || v["images"] != float64(0) {
		t.Fatalf("vision record = %+v", v)
	}
}

func TestVisionPartialFetchFailureRecordsCountedReason(t *testing.T) {
	svc, _ := visionSvc(t)
	goodURL, _ := picture(t)
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	t.Cleanup(failed.Close)
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m-partial-counts", Message: "Describe these pictures.",
		Speaker: "Alex", SpeakerID: "u-partial-counts", Locale: "en",
		Images: []vision.Ref{{URL: goodURL, ContentType: "image/png"}, {URL: failed.URL + "/missing.png", ContentType: "image/png"}}})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := visionTurnBody(t, svc, res.TurnID)["vision"].(map[string]any)
	skipped, _ := v["skipped_counts"].(map[string]any)
	if v["images"] != float64(1) || v["selected"] != float64(2) || v["total"] != float64(2) || skipped[vision.ReasonFetch] != float64(1) {
		t.Fatalf("partial fetch diagnostics = %+v", v)
	}
}

// A channel that keeps turns body-free keeps no description either; deleting the message the picture
// came from removes the description from the conversation and the turn history.
func TestDescriptionFollowsBodyFreeAndDeletion(t *testing.T) {
	svc, h := visionSvc(t)
	h.blind.Store(true)
	url, _ := picture(t)
	if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g", "id": "quiet", "logging_level": "off"}); !r.OK {
		t.Fatalf("%+v", r.Error)
	}
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "quiet", MessageID: "m1", Message: "이거 뭐야?",
		Speaker: "철수", SpeakerID: "u1", Images: []vision.Ref{{URL: url, MessageID: "m1"}}})
	if err != nil {
		t.Fatal(err)
	}
	b := visionTurnBody(t, svc, res.TurnID)
	if strings.Contains(asJSON(t, b), "red cat") {
		t.Fatalf("a body-free turn keeps the description: %s", asJSON(t, b))
	}
	if v, _ := b["vision"].(map[string]any); v["mode"] != "described" {
		t.Fatalf("the body-free turn still says what happened: %+v", b["vision"])
	}

	// the picture is on the message the member replied to (r1, someone else's); the member asks in m2
	res, err = svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m2", Message: "이 사진 뭐야?",
		Speaker: "철수", SpeakerID: "u1", Images: []vision.Ref{{URL: url, MessageID: "r1"}}})
	if err != nil {
		t.Fatal(err)
	}
	eng, _ := svc.MemoryEngine()
	kept := func() string {
		turns, _ := eng.Store.RecentChatTurns("discord:g:c", 10)
		all := ""
		for _, tr := range turns {
			all += tr.Role + ":" + tr.Content + "|"
		}
		return all
	}
	if !strings.Contains(kept(), "red cat") || !strings.Contains(asJSON(t, visionTurnBody(t, svc, res.TurnID)), "red cat") {
		t.Fatal("before the deletion the description is kept (the check below must be able to fail)")
	}
	svc.MessagesDeleted("g", "c", []string{"r1"})
	svc.DrainRedactions(context.Background())
	if strings.Contains(kept(), "red cat") {
		t.Fatalf("the conversation keeps a description of a deleted message's picture: %s", kept())
	}
	if strings.Contains(asJSON(t, visionTurnBody(t, svc, res.TurnID)), "red cat") {
		t.Fatal("the turn history keeps a description of a deleted message's picture")
	}
}

// A picture the connector found just before the message (not with it) is read the same way, and the
// answer is told it came earlier so a message about something else leaves it out.
func TestEarlierPictureIsMarkedForTheAnswer(t *testing.T) {
	svc, h := visionSvc(t)
	url, _ := picture(t)
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m2", Message: "이거 뭐야?",
		Speaker: "철수", SpeakerID: "u1", ImageEarlier: true, Images: []vision.Ref{{URL: url, ContentType: "image/png", MessageID: "m1", AuthorID: "u1"}}})
	if err != nil || res.Answer == "" {
		t.Fatalf("turn: %v %q", err, res.Answer)
	}
	if countImages(h.bodies(false)) != 1 {
		t.Fatalf("the earlier picture goes to the answer: %d", countImages(h.bodies(false)))
	}
	if !strings.Contains(h.bodies(true)[0], "이거 뭐야? [사진] (이 사진은 메시지와 함께 온 게 아니라 바로 앞에 올라온 것이다.") {
		t.Fatal("the message says the picture came just before it")
	}
}
