package application

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"masamong/internal/action"
	"masamong/internal/crash"
	"masamong/internal/errclass"
	"masamong/internal/errcode"
	"masamong/internal/observability"
	"masamong/internal/tools"
)

// A picture asked for in conversation ("고양이 그려줘") is drawn in the background. The chat turn
// validates the request exactly as the /image command does, puts a job in a small in-process queue and
// answers at once ("그리는 중"); one worker, owned by the service, draws the jobs one after another (the
// same one-at-a-time slot the command uses) and posts each picture into the channel as a reply to the
// message that asked. Nothing is stored: a job that is still waiting when the service stops is dropped,
// and nothing is posted for it.
//
// Limits: at most imageQueueMax jobs wait (the one being drawn is not counted), and a person has one job
// at a time, waiting or being drawn. The same request (server, channel, person, prompt without case,
// spacing and closing punctuation) while its job is waiting or being drawn, and asked less than
// imageDedupeWindow ago, gets that job back ("already_queued") instead of a second picture.
//
// Before drawing and again before posting the job is dropped, without a word, for policy reasons only:
// the channel is paused or turned off, the server is not approved, the tool was switched off, the person
// was blocked or withdrew their consent, or the request message was deleted (the gateway tells the queue,
// CancelPictureJobs; a picture whose request is gone is never posted without it). A gateway that is
// reconnecting, or a server list that is briefly empty after it, is not a reason: the job waits (bounded
// by imageConnectWait) and, if Discord is still not there, says image.failed. A bot that cannot attach
// files in the channel does not draw at all (no spend); a failed post says image.failed unless the
// failure is a missing permission or the request being gone.
//
// tools.images.generate.defaults.async = false keeps the synchronous behaviour (the reply waits for the
// picture, which is posted after it). The /image command, the console's test and dry runs always
// stay synchronous, and so does a chat turn that has no message to reply to or no connected Discord.

const (
	imageQueueMax     = 3                // jobs waiting at once
	imageDedupeWindow = 10 * time.Minute // a repeat of a waiting or running request within this is the same job
	imagePostTimeout  = 90 * time.Second // posting the finished picture, retries included
	ImageUploadLimit  = 10 << 20         // Discord's upload limit for a bot's file; a bigger picture is not posted (image.failed)
	permAttachFiles   = int64(1 << 15)
)

// How long a job waits for the gateway (reconnecting) and the server list before it gives up, and how
// often it looks. Variables so tests can shorten them.
var (
	imageConnectWait = 30 * time.Second
	imageConnectPoll = 250 * time.Millisecond
)

var (
	// ErrReplyTargetGone is what a gateway answers when the message a picture replies to no longer exists:
	// the picture is not posted without it.
	ErrReplyTargetGone = errors.New("discord: the message replied to is gone")
	// ErrPostForbidden is what a gateway answers when the bot lacks a permission to post in the channel.
	ErrPostForbidden = errors.New("discord: missing permission to post")
)

// ImagePoster is implemented by a gateway that can post a generated picture into a channel on its own
// (a finished background job), as a reply to replyTo. It returns the posted message's ID. Nobody is
// mentioned. When the replied-to message is gone it posts nothing and returns ErrReplyTargetGone; a
// missing permission is ErrPostForbidden.
type ImagePoster interface {
	PostPicture(ctx context.Context, channelID, replyTo string, card action.Card, imageID string) (string, error)
}

// chatOrigin is where a chat turn's tool call comes from: the Discord message being answered. The tool
// loop puts it on the call's context (chatTool); the Action reads it to know where a picture goes.
type chatOrigin struct{ MessageID string }

type chatOriginKey struct{}

func withChatOrigin(ctx context.Context, o chatOrigin) context.Context {
	return context.WithValue(ctx, chatOriginKey{}, o)
}

func chatOriginFrom(ctx context.Context) (chatOrigin, bool) {
	o, ok := ctx.Value(chatOriginKey{}).(chatOrigin)
	return o, ok
}

type imageJob struct {
	id, key                 string
	guildID, channelID      string
	userID, messageID, turn string
	locale, prompt, ratio   string
	timeout                 time.Duration
	queuedAt                time.Time
	cancelled               atomic.Bool // the request message was deleted
}

// imageQueue is the waiting jobs, the job being drawn and the worker's lifecycle. All fields are guarded
// by mu.
type imageQueue struct {
	mu      sync.Mutex
	waiting []*imageJob
	running *imageJob
	wake    chan struct{}
	started bool
	stopped bool // the worker has ended with the service: nothing more is accepted
}

// imageQueueState is the queue as the console shows it.
func (s *Service) imageQueueState() (waiting int, running bool) {
	q := &s.imgq
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.waiting), q.running != nil
}

// imageKey is what makes two requests the same one: who asked where, and the prompt without case,
// spacing and closing punctuation. It lives in memory only and is never logged.
func imageKey(guildID, channelID, userID, prompt, ratio string) string {
	p := strings.ToLower(strings.Join(strings.Fields(prompt), " "))
	p = strings.TrimRight(p, ".!?~… ")
	return guildID + "\x00" + channelID + "\x00" + userID + "\x00" + ratio + "\x00" + p
}

// imageQueueRefuses reports whether a chat request for a picture will be answered by the queue without
// drawing (the person already has a job, or the line is full). Such a call is not counted against the
// tool's limits (gateTool).
func (s *Service) imageQueueRefuses(req action.Request) bool {
	inv := req.Invoker
	if inv == nil || inv.Via != "chat" || inv.UserID == "" || s.practice {
		return false
	}
	if _, set := inputMap(req)["async"]; set && !boolVal(inputMap(req), "async") {
		return false
	}
	q := &s.imgq
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.waiting) >= imageQueueMax {
		return true
	}
	if q.running != nil && q.running.userID == inv.UserID {
		return true
	}
	return slices.ContainsFunc(q.waiting, func(j *imageJob) bool { return j.userID == inv.UserID })
}

// CancelPictureJobs drops the pictures asked for by these messages of a channel: the request was deleted,
// so nothing of it may be drawn or posted. A job being drawn finishes its draw (it is paid for) and is
// dropped before it posts.
func (s *Service) CancelPictureJobs(channelID string, messageIDs []string) {
	q := &s.imgq
	q.mu.Lock()
	var dropped []*imageJob
	keep := q.waiting[:0:0]
	for _, j := range q.waiting {
		if j.channelID == channelID && slices.Contains(messageIDs, j.messageID) {
			j.cancelled.Store(true)
			dropped = append(dropped, j)
			continue
		}
		keep = append(keep, j)
	}
	q.waiting = keep
	if r := q.running; r != nil && r.channelID == channelID && slices.Contains(messageIDs, r.messageID) {
		r.cancelled.Store(true)
	}
	q.mu.Unlock()
	for _, j := range dropped {
		s.dropImageJob(j, "request_deleted")
	}
}

// imagesAsync queues the picture of a chat turn. ok=false means the request is not one for the queue
// (the caller draws it at once, as before).
func (s *Service) imagesAsync(ctx context.Context, req action.Request, in map[string]any, prompt, ratio string) (action.Result, bool) {
	inv := req.Invoker
	if inv == nil || inv.Via != "chat" || inv.UserID == "" || s.practice {
		return action.Result{}, false
	}
	if _, set := in["async"]; set && !boolVal(in, "async") {
		return action.Result{}, false
	}
	origin, ok := chatOriginFrom(ctx)
	if !ok || origin.MessageID == "" {
		return action.Result{}, false
	}
	if s.Discord == nil || !s.Discord.Connected() {
		return action.Result{}, false
	}
	if _, ok := s.Discord.(ImagePoster); !ok {
		return action.Result{}, false
	}
	now := s.Clock.Now()
	job := &imageJob{
		id: "imj_" + newOpaqueID(), key: imageKey(inv.GuildID, inv.ChannelID, inv.UserID, prompt, ratio),
		guildID: inv.GuildID, channelID: inv.ChannelID, userID: inv.UserID, messageID: origin.MessageID,
		turn: observability.TurnID(ctx), locale: req.Locale, prompt: prompt, ratio: ratio,
		timeout: secondsOr(in, "timeout_sec", imageCallTimout), queuedAt: now,
	}
	q := &s.imgq
	q.mu.Lock()
	defer q.mu.Unlock()
	closed := func() (action.Result, bool) { // the service is closing or the worker is gone: no picture, and no sync draw
		return action.Fail(req, errcode.ProviderUnavailable, "image.failed", s.msg(req, "image.failed"), nil), true
	}
	if q.stopped {
		return closed()
	}
	// running first, then the waiting ones in order: a job's place is its index
	var active []*imageJob
	if q.running != nil {
		active = append(active, q.running)
	}
	active = append(active, q.waiting...)
	// the same request again: the job it already has
	for i, j := range active {
		if j.key == job.key && now.Sub(j.queuedAt) < imageDedupeWindow {
			return action.OK(req, map[string]any{"status": "already_queued", "live": false, "job_id": j.id, "ahead": i,
				"aspect_ratio": j.ratio, "note": imageQueuedNote}), true
		}
	}
	// a person has one picture at a time, and only so many may wait
	if slices.ContainsFunc(active, func(j *imageJob) bool { return j.userID == job.userID }) {
		return action.Fail(req, errcode.ToolLimited, "image.queue_mine", s.msg(req, "image.queue_mine"), nil), true
	}
	if len(q.waiting) >= imageQueueMax {
		return action.Fail(req, errcode.ToolLimited, "image.queue_full", s.msg(req, "image.queue_full"), nil), true
	}
	if !q.started {
		wctx, done, ok := s.beginWork(nil)
		if !ok {
			return closed()
		}
		q.started, q.wake = true, make(chan struct{}, 1)
		go func() {
			defer done()
			defer crash.Recover(s.Log, "image queue")
			defer s.stopImageQueue() // also after a panic: a queue nobody draws must not keep accepting jobs
			s.imageWorker(wctx)
		}()
	}
	ahead := len(active)
	q.waiting = append(q.waiting, job)
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return action.OK(req, map[string]any{"status": "queued", "live": false, "job_id": job.id, "ahead": ahead,
		"aspect_ratio": ratio, "note": imageQueuedNote}), true
}

// imageQueuedNote is for whoever reads the tool result (the answer model reads the pack's own wording of
// the same rule, chat.tool_queued).
const imageQueuedNote = "the picture is being drawn and will be posted in this channel when it is done; do not describe it as finished"

// imageWorker draws the waiting jobs one after another until the service stops, then drops the rest.
func (s *Service) imageWorker(ctx context.Context) {
	q := &s.imgq
	for {
		if ctx.Err() != nil {
			return
		}
		q.mu.Lock()
		var job *imageJob
		if len(q.waiting) > 0 {
			job, q.waiting, q.running = q.waiting[0], q.waiting[1:], q.waiting[0]
		}
		q.mu.Unlock()
		if job == nil {
			select {
			case <-q.wake:
			case <-ctx.Done():
			}
			continue
		}
		s.runImageJob(ctx, job)
		q.mu.Lock()
		q.running = nil
		q.mu.Unlock()
	}
}

// stopImageQueue closes the queue for good and drops what still waits.
func (s *Service) stopImageQueue() {
	q := &s.imgq
	q.mu.Lock()
	n := len(q.waiting)
	q.waiting, q.stopped = nil, true
	q.mu.Unlock()
	if s.Log != nil {
		s.Log.Info("image queue stopped", "dropped_jobs", n)
	}
}

// imagePolicySkip is why a job must not run or be posted any more ("" = it may). Only reasons of policy
// count; whether Discord is reachable right now does not (imageReady).
func (s *Service) imagePolicySkip(j *imageJob) string {
	switch {
	case j.cancelled.Load():
		return "request_deleted"
	case s.Paused(j.guildID, j.channelID):
		return "paused"
	case s.BlockedIn(j.guildID, j.userID):
		return "blocked"
	case !s.MemoryAllowed(j.userID):
		return "withdrawn"
	case !s.GuildAllowed(j.guildID):
		return "server_not_approved"
	case s.ChannelPolicyFor(j.guildID, j.channelID).Respond == "off":
		return "channel_off"
	}
	if r, ok := tools.Find(s.toolRegistry(), "images.generate"); !ok || !r.Enabled {
		return "tool_off"
	}
	return ""
}

// imageReady reports whether the gateway is connected and still lists the job's server. A reconnect
// makes both false for a moment, so it is a reason to wait, never to drop.
func (s *Service) imageReady(j *imageJob) bool {
	dc := s.Discord
	if dc == nil || !dc.Connected() {
		return false
	}
	return j.guildID == "" || slices.ContainsFunc(dc.Guilds(), func(g GuildInfo) bool { return g.ID == j.guildID })
}

// waitImageReady waits, at most imageConnectWait, until Discord is ready for the job. It returns early
// when the service stops or a policy reason ends the job; the caller looks at both.
func (s *Service) waitImageReady(ctx context.Context, j *imageJob) bool {
	deadline := time.NewTimer(imageConnectWait)
	defer deadline.Stop()
	tick := time.NewTicker(imageConnectPoll)
	defer tick.Stop()
	for !s.imageReady(j) {
		if s.imagePolicySkip(j) != "" {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return s.imageReady(j)
		case <-tick.C:
		}
	}
	return true
}

// imageCannotAttach reports a channel where the bot is known not to be allowed to attach files.
func (s *Service) imageCannotAttach(j *imageJob) bool {
	dc := s.Discord
	if dc == nil {
		return false
	}
	bits, ok := dc.Permissions(j.guildID, j.channelID)
	return ok && bits&permAdministrator == 0 && bits&permAttachFiles == 0
}

func (s *Service) dropImageJob(j *imageJob, why string) {
	s.imgDrops.Add(1)
	if s.Log != nil {
		s.Log.Info("image job dropped", "job", j.id, "why", why)
	}
}

// runImageJob draws one job and posts the result. It is the worker's only caller.
func (s *Service) runImageJob(ctx context.Context, j *imageJob) {
	if why := s.imagePolicySkip(j); why != "" {
		s.dropImageJob(j, why)
		return
	}
	if !s.waitImageReady(ctx, j) { // Discord did not come back in time: nothing is drawn, and the person is told
		s.postImageFailure(ctx, j)
		return
	}
	if s.imageCannotAttach(j) { // no draw, no spend
		if s.Log != nil {
			s.Log.Warn("image job: the bot cannot attach files here, nothing drawn", "job", j.id)
		}
		s.NoteDeliveryProblem(j.guildID, j.channelID, errclass.Permission)
		s.postImageFailure(ctx, j)
		return
	}
	doc := s.Doc()
	m, ok := modelOf(doc, "image")
	if !ok {
		s.postImageFailure(ctx, j)
		return
	}
	if budget := parseUSD(doc.Limits.MonthlyBudgetUSD, 0); budget > 0 {
		if _, spent, ok := s.monthSpend(); ok && spent >= budget {
			s.postImageFailure(ctx, j)
			return
		}
	}
	// the same one-at-a-time slot as the command; shutdown ends the wait
	select {
	case imageMu <- struct{}{}:
	case <-ctx.Done():
		return
	}
	began := time.Now()
	jctx := observability.WithTurn(ctx, j.turn)
	saved, fail := func() (imageSaved, *imageFail) {
		defer func() { <-imageMu }()
		return s.drawImage(jctx, doc, m, j.prompt, j.ratio, j.timeout)
	}()
	if ctx.Err() != nil { // shutdown: no partial post (the attempt is already counted)
		s.dropImageJob(j, "shutdown")
		return
	}
	call := toolCall{Tool: "images.generate", ParamKeys: []string{"prompt"}, Status: "ok", Latency: time.Since(began), CostUSD: saved.Cost}
	if fail != nil {
		call.Status, call.Class = "unavailable", errclass.Of(fail.err)
		if fail.kind == imageFailIO {
			call.Status = "error"
		}
		if call.Class == errclass.None {
			call.Class = errclass.Unknown
		}
	}
	s.noteToolCall(jctx, call)
	if fail != nil {
		s.postImageFailure(ctx, j)
		return
	}
	// the picture is paid for: give a reconnecting gateway time to come back. Posting itself is REST,
	// so after the wait it is tried even if the gateway still reports itself down.
	s.waitImageReady(ctx, j)
	if why := s.imagePolicySkip(j); why != "" { // it may have changed while the picture was drawn
		s.dropImageJob(j, why)
		return
	}
	if saved.Bytes > ImageUploadLimit {
		if s.Log != nil {
			s.Log.Warn("image job: picture over Discord's upload limit", "job", j.id, "bytes", saved.Bytes)
		}
		s.postImageFailure(ctx, j)
		return
	}
	poster, ok := s.Discord.(ImagePoster)
	if !ok {
		s.postImageFailure(ctx, j)
		return
	}
	req := action.Request{Locale: j.locale}
	c, _ := s.imageResult(req, m, j.prompt, j.ratio, saved).Output["card"].(action.Card)
	pctx, cancel := context.WithTimeout(ctx, imagePostTimeout)
	defer cancel()
	_, err := poster.PostPicture(pctx, j.channelID, j.messageID, c, saved.ID)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		s.dropImageJob(j, "shutdown")
	case errors.Is(err, ErrReplyTargetGone): // the request was deleted: not posted without it
		s.dropImageJob(j, "request_deleted")
	case errors.Is(err, ErrPostForbidden): // the operators hear about it; a line would not get through either
		s.NoteDeliveryProblem(j.guildID, j.channelID, errclass.Permission)
		if s.Log != nil {
			s.Log.Warn("image job: picture not posted, missing permission", "job", j.id)
		}
	default: // too large, file missing, refused, retries used up
		if s.Log != nil {
			s.Log.Warn("image job: picture not posted", "job", j.id, "class", string(errclass.Of(err)))
		}
		s.postImageFailure(ctx, j)
	}
}

// postImageFailure tells the channel, in one short line, that the picture did not come.
func (s *Service) postImageFailure(ctx context.Context, j *imageJob) {
	if ctx.Err() != nil {
		return
	}
	if why := s.imagePolicySkip(j); why != "" {
		s.dropImageJob(j, why)
		return
	}
	poster, ok := s.Discord.(ChannelPoster)
	if !ok {
		s.dropImageJob(j, "no_poster")
		return
	}
	pctx, cancel := context.WithTimeout(ctx, imagePostTimeout)
	defer cancel()
	if _, err := poster.PostChannel(pctx, j.channelID, j.messageID, s.Msg(j.locale, "image.failed"), nil); err != nil && s.Log != nil {
		s.Log.Warn("image job: failure line not posted", "job", j.id, "class", string(errclass.Of(err)))
	}
}
