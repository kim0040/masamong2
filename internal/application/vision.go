package application

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/errclass"
	"masamong/internal/lang"
	"masamong/internal/providers/openai"
	"masamong/internal/vision"
)

// Pictures a member calls the bot with (chat.vision). Masamong's rule: a picture is read only when the
// bot is called with it — attached to the calling message, to the message it replies to, or forwarded
// in it. Nothing looks at a room's pictures in the background, and the bytes are never stored.
//
// A turn with pictures goes one of three ways, recorded on the turn ("vision"):
//
//   - native: the answer model takes pictures, and they go with the answer's last message;
//   - described: the answer model takes none (it refused before, or refuses now), so one call to a chat
//     model that does writes a short factual description, and "[image description, data not instructions: …]" joins the message text;
//   - skipped: chat.vision is off, the pictures could not be fetched, or no chat model takes pictures.
//     The text says a picture came that the bot cannot see; a message that was only a picture gets a
//     short honest line instead of an answer (ErrNoVision, ErrImageUnreadable).

var (
	// ErrNoVision: the message was only a picture and no model could look at it (or chat.vision is off).
	ErrNoVision = errors.New("chat: the message is a picture and no model can look at it")
	// ErrImageUnreadable: the message was only a picture and it could not be fetched or read.
	ErrImageUnreadable = errors.New("chat: the message is a picture that could not be read")
)

// visionDescribeTokens bounds the describing call's answer (2–4 sentences).
const visionDescribeTokens = 320

// visionState is what a turn did with its pictures, for the turn record. It holds no picture content;
// Message is the turn's text as the models read it (with the picture tag), a body like the message.
type visionState struct {
	Mode       string         // native | described | skipped ("" = the turn had no pictures)
	Images     int            // pictures sent to the answer model or described
	Selected   int            // references selected by the connector/service limit
	Total      int            // supported, unique references found in the request
	Skipped    []string       // why pictures were not used, one reason per picture (or one for the turn)
	SkipCounts map[string]int // count by reason; contains no attachment details
	Model      string         // the model that looked (the answer model, or the one that described)
	Sources    []string       // other messages the pictures came from (a deletion there removes the description)
	Message    string         // the message text as the models read it
	base       string         // the message text before any picture tag
	text       lang.Vision
	pack       *lang.Pack // the turn's language (descriptions are cleaned for its prompt)
	images     []vision.Image
	desc       string // the description, once made (a continued answer reuses it)
	earlier    string // said after the tag when the pictures came just before the message
}

// record is the body-free part of the state for the turn record: no message, no description.
func (v *visionState) record() map[string]any {
	out := map[string]any{"mode": v.Mode, "images": v.Images, "selected": v.Selected, "total": v.Total}
	if len(v.Skipped) > 0 {
		out["skipped"] = append([]string(nil), v.Skipped...)
	}
	if len(v.SkipCounts) > 0 {
		skipped := make(map[string]int, len(v.SkipCounts))
		for reason, count := range v.SkipCounts {
			if count > 0 {
				skipped[reason] = count
			}
		}
		if len(skipped) > 0 {
			out["skipped_counts"] = skipped
		}
	}
	if v.Model != "" {
		out["model"] = v.Model
	}
	return out
}

func (v *visionState) skip(reason string, count int) {
	if reason == "" || count <= 0 {
		return
	}
	if v.SkipCounts == nil {
		v.SkipCounts = map[string]int{}
	}
	v.SkipCounts[reason] += count
	for range count {
		v.Skipped = append(v.Skipped, reason)
	}
}

func (v *visionState) tag(t string) {
	v.Message = strings.TrimSpace(v.base + " " + t + " " + v.earlier)
}

type visionCtxKey struct{}

func withVision(ctx context.Context, v *visionState) context.Context {
	return context.WithValue(ctx, visionCtxKey{}, v)
}

func visionFrom(ctx context.Context) *visionState {
	v, _ := ctx.Value(visionCtxKey{}).(*visionState)
	return v
}

// visionKey is the process-wide memory of a model that refused pictures or is cooling down.
func visionKey(r chatRoute) string { return r.provider + "|" + r.model }

// visionUsable reports whether pictures may be sent to the route's model: an OpenAI-compatible
// adapter that has not refused them in this process (the offline practice model takes none).
func (s *Service) visionUsable(doc config.Document, r chatRoute) bool {
	if r.a == nil || r.a.VisionUnsupported(r.model) {
		return false
	}
	if p, ok := doc.Providers[r.provider]; ok && p.Kind == config.KindPractice {
		return false
	}
	return !s.visionRefusedNow(visionKey(r))
}

// visionRefusedNow reports a model that refused pictures in the last openai.VisionRefusalTTL; after it
// the model is asked again (a host can add vision to a model). A config reload forgets every refusal.
func (s *Service) visionRefusedNow(key string) bool {
	v, ok := s.visionRefused.Load(key)
	if !ok {
		return false
	}
	if at, _ := v.(time.Time); sinceWithin(s.Clock.Now(), at, openai.VisionRefusalTTL) {
		return true
	}
	s.visionRefused.CompareAndDelete(key, v)
	return false
}

// visionCooldown is how long the vision role's model is skipped after a call that timed out or failed
// on the wire (timeout, network, 5xx): pictures go straight to the chat models instead of every turn
// waiting out the role's timeout again. It is much shorter than the one-hour refusal memory because an
// outage ends by itself; a config reload and one successful call also end it.
const visionCooldown = 3 * time.Minute

// visionCoolingNow reports a vision-role model that failed in the last visionCooldown.
func (s *Service) visionCoolingNow(r chatRoute) bool {
	key := visionKey(r)
	v, ok := s.visionCooling.Load(key)
	if !ok {
		return false
	}
	if at, _ := v.(time.Time); sinceWithin(s.Clock.Now(), at, visionCooldown) {
		return true
	}
	s.visionCooling.CompareAndDelete(key, v)
	return false
}

// noteVisionCall starts or ends the cool-down after a vision-role description. Only a provider in
// trouble counts (a timeout, a network error, a 5xx); a refusal of pictures has its own memory, and an
// empty or unparsable answer is a model's answer, not an outage.
func (s *Service) noteVisionCall(r chatRoute, err error) {
	key := visionKey(r)
	if err == nil {
		s.visionCooling.Delete(key)
		return
	}
	switch errclass.Of(err) {
	case errclass.Timeout, errclass.Network, errclass.Overloaded:
		s.visionCooling.Store(key, s.Clock.Now())
	}
}

// visionNoKeyTTL is how long an adapter without a key is kept: chatRoutes asks on every turn, and a
// missing keychain item is not cached by the keychain reader, so this spares a lookup per turn while a
// key added later still shows up quickly (a key saved from the console drops it at once).
const visionNoKeyTTL = 20 * time.Second

// visionAdapterTTL bounds how long a kept adapter is trusted: a key replaced under the same reference is
// picked up after it (the keychain read has the same two-minute cache).
const visionAdapterTTL = 2 * time.Minute

// visionAdapterCache keeps the vision role's adapter between turns, so a picture turn does not build
// an HTTP client and read the credential again. It holds one entry (the role has one model).
type visionAdapterCache struct {
	mu  sync.Mutex
	key string
	at  time.Time
	a   *openai.Adapter
}

func (c *visionAdapterCache) drop() {
	c.mu.Lock()
	c.key, c.a = "", nil
	c.mu.Unlock()
}

// visionAdapterKey is what the adapter is built from: the model, the provider's kind, address and API,
// the credential reference, and practice mode. The key's value is not part of it; the TTL covers a
// replaced key, and a config reload (setDoc) drops the entry.
func (s *Service) visionAdapterKey(doc config.Document) string {
	m, _ := modelOf(doc, "vision")
	p := doc.Providers[m.Provider]
	return strings.Join([]string{m.Provider, m.Model, p.Kind, p.BaseURL, p.API, p.CredentialRef, strconv.FormatBool(s.practice)}, "\x00")
}

// visionAdapter is llmAdapterFor(doc, "vision") kept for visionAdapterTTL while nothing it is built
// from changes (visionNoKeyTTL for one without a key). A failed build is not kept; a key saved from the
// console drops the entry (credentialsChanged).
func (s *Service) visionAdapter(doc config.Document) (*openai.Adapter, error) {
	key := s.visionAdapterKey(doc)
	c := &s.visionAd
	c.mu.Lock()
	defer c.mu.Unlock()
	ttl := visionAdapterTTL
	if c.a != nil && !c.a.Configured() {
		ttl = visionNoKeyTTL
	}
	if c.a != nil && c.key == key && sinceWithin(s.Clock.Now(), c.at, ttl) {
		return c.a, nil
	}
	a, err := s.llmAdapterFor(doc, "vision")
	if err != nil {
		c.key, c.a = "", nil
		return a, err
	}
	c.key, c.a, c.at = key, a, s.Clock.Now()
	return a, nil
}

// credentialsChanged follows a key saved for a provider. A key saved under the reference already in the
// config changes no revision, so nothing rebuilt the adapters holding the old key: reload them (the
// registry's, and the vision role's kept one with its cool-down and refusal memory).
func (s *Service) credentialsChanged() {
	if err := s.reloadConfig(); err != nil && s.Log != nil {
		s.Log.Warn("config reload after a key was saved failed", "err", err)
	}
	s.visionAd.drop()
	s.visionCooling.Clear()
}

// sinceWithin reports now-at in [0, d): a wall clock stepped back counts as expired, not as d stretched.
func sinceWithin(now, at time.Time, d time.Duration) bool {
	e := now.Sub(at)
	return e >= 0 && e < d
}

func (s *Service) visionFetcher() *vision.Fetcher {
	f := vision.NewFetcher(s.Log)
	if s.visionLocal { // tests: a local server stands in for Discord's CDN
		f.AllowHTTP, f.AllowHost = true, func(host string) bool { return host == "127.0.0.1" || host == "localhost" || vision.DiscordHost(host) }
	}
	return f
}

// prepareVision fetches the turn's pictures and decides how the answer sees them. It returns the
// pictures to send with the answer (native) and puts the right tag into in.Message. A message that was
// only a picture and cannot be seen returns ErrNoVision or ErrImageUnreadable.
func (s *Service) prepareVision(ctx context.Context, in *ChatInput, doc config.Document, llm chatLLM, pack *lang.Pack) ([]chat.Image, float64, error) {
	st := in.vision
	st.text, st.base, st.pack = pack.VisionText(), in.Message, pack
	if in.ImageEarlier {
		st.earlier = st.text.Earlier
	}
	if in.imageOnly {
		st.base = st.text.Attached
	}
	st.Message = st.base
	refs := in.Images
	st.Total = max(in.ImageTotal, len(refs)+sumCounts(in.ImageSkipped))
	for reason, count := range in.ImageSkipped {
		st.skip(reason, count)
	}
	if len(refs) > vision.MaxImages {
		st.Total = max(st.Total, len(refs))
		st.skip(vision.ReasonOverLimit, len(refs)-vision.MaxImages)
		refs = refs[:vision.MaxImages]
	}
	st.Selected = len(refs)
	if !doc.Chat.VisionOn() {
		return nil, 0, s.visionSkipped(in, st, vision.ReasonOff, len(refs))
	}
	f := s.visionFetcher()
	for _, ref := range refs {
		// a picture someone else posted (the message replied to, a forward's original) is not read when
		// its author withdrew consent to being remembered; being blocked does not matter here (blocking
		// only stops the bot answering that person)
		if ref.AuthorID != "" && ref.AuthorID != in.SpeakerID && !s.MemoryAllowed(ref.AuthorID) {
			st.skip(vision.ReasonConsent, 1)
			continue
		}
		img, err := f.Fetch(ctx, ref)
		if err != nil {
			st.skip(vision.ReasonOf(err), 1)
			continue
		}
		st.images = append(st.images, img)
		if img.MessageID != "" && img.MessageID != in.MessageID && !contains(st.Sources, img.MessageID) {
			st.Sources = append(st.Sources, img.MessageID)
		}
	}
	if len(st.images) == 0 {
		reason := vision.ReasonFetch
		if len(st.Skipped) > 0 {
			reason = st.Skipped[len(st.Skipped)-1]
		}
		return nil, 0, s.visionSkipped(in, st, reason, len(refs))
	}
	if in.sourceRefs != nil {
		*in.sourceRefs = append(*in.sourceRefs, st.Sources...)
	}
	// models.vision decides who reads the pictures: when it is set and works, the vision model describes
	// them and the answer model gets text only, even if it takes pictures itself. When it is unset, or it
	// fails or refuses, the turn goes the way it always did: native if the answer model takes pictures,
	// else described by the planner, then the answer model.
	spent := 0.0
	if r, ok := llm.routes["vision"]; ok && s.visionUsable(doc, r) && !s.visionCoolingNow(r) {
		desc, model, cost, err := llm.describeBy(ctx, st, "", "vision")
		spent += cost
		if ctx.Err() == nil {
			s.noteVisionCall(r, err)
		}
		if err == nil {
			st.Mode, st.Images, st.Model = "described", len(st.images), model
			st.tag(lang.Fill(st.text.Described, "description", desc))
			in.Message = st.Message
			return nil, spent, nil
		}
		if ctx.Err() != nil {
			return nil, spent, s.visionSkipped(in, st, describeReason(err), len(refs))
		}
		s.Log.WarnContext(ctx, "chat: the vision model could not describe the pictures; using the chat models", "model", r.model, "class", string(errclass.Of(err)))
	}
	if resp, ok := llm.routes["response"]; ok && s.visionUsable(doc, resp) {
		st.Mode, st.Images, st.Model = "native", len(st.images), resp.model
		st.tag(st.text.Attached)
		if in.imageOnly {
			st.Message = st.base
		}
		in.Message = st.Message
		return chatImages(st.images), spent, nil
	}
	desc, model, cost, err := llm.describe(ctx, st, "")
	spent += cost
	if err != nil {
		return nil, spent, s.visionSkipped(in, st, describeReason(err), len(refs))
	}
	st.Mode, st.Images, st.Model = "described", len(st.images), model
	st.tag(lang.Fill(st.text.Described, "description", desc))
	in.Message = st.Message
	return nil, spent, nil
}

// visionSkipped records pictures that will not be looked at. A message that was only a picture gets
// the honest line instead of an answer; otherwise the text says a picture came that the bot cannot see.
func (s *Service) visionSkipped(in *ChatInput, st *visionState, reason string, count int) error {
	st.Mode = "skipped"
	if !contains(st.Skipped, reason) {
		st.skip(reason, max(1, count))
	}
	st.images = nil
	if in.imageOnly {
		switch reason {
		case vision.ReasonOff, vision.ReasonNoModel, vision.ReasonDescribeErr:
			return ErrNoVision
		}
		return ErrImageUnreadable
	}
	st.tag(st.text.Unseen)
	in.Message = st.Message
	return nil
}

// notePartialVision hands the connector the omission line when the turn's final outcome read some
// pictures but not all. It runs after the answer, so a native read that fell back to a failed
// description (no picture read at all) is not announced as "M of N".
func (s *Service) notePartialVision(in *ChatInput, st *visionState) {
	if in.OnVisionNotice == nil || st.Images == 0 || st.Images >= st.Total || len(st.Skipped) == 0 {
		return
	}
	line := lang.Fill(st.text.Partial, "read", strconv.Itoa(st.Images), "total", strconv.Itoa(st.Total))
	if strings.TrimSpace(line) != "" {
		in.OnVisionNotice(line)
	}
}

func sumCounts(counts map[string]int) int {
	total := 0
	for _, count := range counts {
		if count > 0 {
			total += count
		}
	}
	return total
}

func describeReason(err error) string {
	if errors.Is(err, errNoVisionModel) {
		return vision.ReasonNoModel
	}
	return vision.ReasonDescribeErr
}

var errNoVisionModel = errors.New("no chat model takes pictures")

func chatImages(imgs []vision.Image) []chat.Image {
	out := make([]chat.Image, len(imgs))
	for i, img := range imgs {
		out[i] = chat.Image{MIME: img.MIME, Data: img.Data}
	}
	return out
}

func imageParts(imgs []chat.Image) []openai.ImagePart {
	if len(imgs) == 0 {
		return nil
	}
	out := make([]openai.ImagePart, len(imgs))
	for i, img := range imgs {
		out[i] = openai.ImagePart{MIME: img.MIME, Data: img.Data}
	}
	return out
}

// describe asks a chat model that takes pictures for a short factual description of the turn's
// pictures: the planner's model (small and cheap), then the answer model. skip names a route ("response")
// that just refused. When models.vision is set, prepareVision asks that model first through describeBy. The call is recorded in the call ledger as "vision" and counted
// like every chat call.
func (c chatLLM) describe(ctx context.Context, st *visionState, skip string) (string, string, float64, error) {
	return c.describeBy(ctx, st, skip, "planner", "response")
}

// describeBy is describe with the routes to try, in order (the vision role alone, or the chat models).
func (c chatLLM) describeBy(ctx context.Context, st *visionState, skip string, roles ...string) (string, string, float64, error) {
	if st.desc != "" {
		return st.desc, st.Model, 0, nil
	}
	doc := c.s.Doc()
	msgs := []chat.Msg{{Role: "system", Content: st.text.DescribeSystem},
		{Role: "user", Content: lang.Fill(st.text.Describe, "n", strconv.Itoa(len(st.images))), Images: chatImages(st.images)}}
	cost := 0.0
	lastErr := errNoVisionModel
	seen := map[string]bool{}
	for _, role := range roles {
		r, ok := c.routes[role]
		if !ok || role == skip || seen[visionKey(r)] || !c.s.visionUsable(doc, r) {
			continue
		}
		seen[visionKey(r)] = true
		out, err := c.ask(withLedgerRole(ctx, "vision"), r, "vision", msgs, visionDescribeTokens, 0, false)
		cost += out.CostUSD
		if err == nil {
			st.desc = flatDescription(st.pack, out.Text)
			return st.desc, r.model, cost, nil
		}
		if ctx.Err() != nil {
			return "", "", cost, err
		}
		if !errors.Is(err, openai.ErrVisionUnsupported) {
			lastErr = err
		}
	}
	return "", "", cost, lastErr
}

// flatDescription keeps a description on one line and short: it sits inside the member's message. It is
// data like any text the bot did not write (a picture can hold "ignore your rules" or a chat-template
// token, and the describing model may copy it): control characters and special tokens dropped, the
// prompt's delimiters and block tags neutralized, no brackets to close the picture tag.
func flatDescription(pack *lang.Pack, s string) string {
	s = chat.UntrustedText(pack, s)
	s = strings.Join(strings.Fields(strings.NewReplacer("[", "(", "]", ")").Replace(s)), " ")
	return clip(s, 700)
}

func hasImages(msgs []chat.Msg) bool {
	for _, m := range msgs {
		if len(m.Images) > 0 {
			return true
		}
	}
	return false
}

// chatWithImages asks the role's model with the turn's pictures. When it cannot look at them (it
// refuses pictures, or the call fails for another reason), the pictures are described by another chat
// model and the request goes again as text, with the description in the member's message.
func (c chatLLM) chatWithImages(ctx context.Context, role string, msgs []chat.Msg, maxTokens int, temperature float64) (chat.Completion, error) {
	st := visionFrom(ctx)
	r := c.routes[role]
	spent := 0.0
	if st == nil || st.desc == "" {
		if c.s.visionUsable(c.s.Doc(), r) {
			out, err := c.ask(ctx, r, role, msgs, maxTokens, temperature, false)
			if err == nil || ctx.Err() != nil || st == nil {
				return out, err
			}
			spent = out.CostUSD
			c.s.Log.WarnContext(ctx, "chat: the answer model could not look at the pictures; describing them instead", "model", r.model, "class", string(errclass.Of(err)))
		}
	}
	text := make([]chat.Msg, len(msgs))
	copy(text, msgs)
	last := -1
	for i := range text {
		if len(text[i].Images) > 0 {
			text[i].Images, last = nil, i
		}
	}
	var tag string
	if st == nil {
		tag = lang.For("en").VisionText().Unseen
	} else {
		desc, model, cost, err := c.describe(ctx, st, role)
		spent += cost
		if err != nil {
			st.Mode, st.Images, st.Model = "skipped", 0, ""
			st.skip(describeReason(err), len(st.images))
			tag = st.text.Unseen
		} else {
			st.Mode, st.Model = "described", model
			tag = lang.Fill(st.text.Described, "description", desc)
		}
		st.tag(tag)
	}
	if last >= 0 {
		text[last].Content = strings.TrimRight(text[last].Content, "\n") + "\n" + tag
	}
	out, err := c.ChatDetailed(ctx, role, text, maxTokens, temperature)
	out.CostUSD += spent
	return out, err
}

// visionRecord adds the turn's picture handling to its record.
func visionRecord(body map[string]any, st *visionState) {
	if st != nil && st.Mode != "" {
		body["vision"] = st.record()
	}
}
