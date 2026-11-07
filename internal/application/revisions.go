package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"masamong/internal/memory/store"
	"masamong/internal/storage"
)

// Edits and deletions of captured Discord messages. The author's latest version is what the bot may
// remember: an edit replaces the stored text, a deletion removes it (engine.Edit / engine.Delete). The
// console's turn history drops the text of a deleted message the bot answered, and the rooms are
// resynced shortly after so search stops showing the old text.

// resyncDelay gathers a burst of edits or deletions (a bulk delete, a user tidying up) into one resync.
var resyncDelay = 2 * time.Second

type resyncQueue struct {
	mu    sync.Mutex
	rooms map[string]bool
	timer *time.Timer
}

var resyncQueues sync.Map // *Service → *resyncQueue

// scheduleResync resyncs the rooms after resyncDelay (engine.OnRevised).
func (s *Service) scheduleResync(rooms []string) {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.closing {
		return
	}
	v, _ := resyncQueues.LoadOrStore(s, &resyncQueue{rooms: map[string]bool{}})
	q := v.(*resyncQueue)
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, r := range rooms {
		q.rooms[r] = true
	}
	if q.timer != nil {
		return
	}
	q.timer = time.AfterFunc(resyncDelay, func() {
		q.mu.Lock()
		rooms := make([]string, 0, len(q.rooms))
		for r := range q.rooms {
			rooms = append(rooms, r)
		}
		q.rooms, q.timer = map[string]bool{}, nil
		q.mu.Unlock()
		s.ownedGo("memory resync", func(_ context.Context) {
			eng, err := s.MemoryEngine()
			if err != nil {
				return
			}
			if err := eng.Resync(rooms); err != nil {
				s.Log.Warn("memory: resync after an edit failed", "rooms", len(rooms), "err", err)
				s.scheduleResync(rooms)
			}
		})
	})
}

// SettledMark is the number a caller takes before it reads what a turn will come with (ChatInput.ReadFrom).
func (s *Service) SettledMark() int64 { return store.SettledMark(s.Store) }

// MessageEdited applies an author's edit of a Discord message the bot may have stored.
func (s *Service) MessageEdited(guildID, channelID, messageID, content string, at time.Time) error {
	ctx, done, ok := s.beginWork(context.Background())
	if !ok {
		return nil
	}
	defer done()
	if s.Store == nil || messageID == "" {
		return nil
	}
	eng, err := s.MemoryEngine()
	if err != nil {
		return err
	}
	if at.IsZero() {
		at = s.Clock.Now()
	}
	if s.Store.IsSQLite() {
		// older than an edit that kept the words: those words stand, and what is built on them
		newer, err := store.KeptRevisionNewer(ctx, s.Store.Reader(), messageID, at)
		if err != nil || newer {
			return err
		}
	}
	s.liveEdit(guildID, channelID, messageID, content)
	s.dropReminderContextMessages([]string{messageID}) // a reminder's copy would keep the old words
	if err := s.queueSourceChange(guildID, channelID, []string{messageID}, "edit", content, at); err != nil {
		s.Log.Warn("memory: edit intent not committed", "err", err)
		return err
	}
	changed, err := eng.EditContext(ctx, messageID, content, at)
	if err != nil {
		s.Log.Warn("memory: edit not applied", "err", err)
		return err
	}
	if changed {
		s.emit("memory", "edited", map[string]any{"channel": channelID})
	}
	if s.practice {
		s.drainRedactions(context.Background())
	}
	return nil
}

// MessageKeptItsWords takes an edit that left the words as they were and the files too (only the connector can say
// that): what was read of the message is still what the message says, so nothing built on it changes and only the
// stored revision moves. It reports false, changing nothing, unless the words are shown the same as the copy the bot
// holds, and the caller then applies the edit in full. So does a change of the message not finished yet: that one
// is decided by its own job, and these words may be the ones it replaces.
func (s *Service) MessageKeptItsWords(guildID, channelID, messageID, content string, at time.Time) (bool, error) {
	_, done, ok := s.beginWork(context.Background())
	if !ok {
		return false, nil
	}
	defer done()
	content = strings.TrimSpace(content) // as the copies hold it (connectors' bodyOf)
	if s.Store == nil || s.practice || messageID == "" || content == "" {
		return false, nil
	}
	db, err := s.adminDB()
	if err != nil || !db.IsSQLite() {
		return false, err
	}
	if at.IsZero() {
		at = s.Clock.Now()
	}
	// the window keeps a clipped copy: a longer message cannot be compared with it
	windowSame := utf8.RuneCountInString(content) <= liveTextRunes && s.liveHasWords(guildID, channelID, messageID, content)
	kept := false
	err = db.WithWriter(func(tx *sql.Tx) error {
		if s.Store.HasPendingEvents("source-change") {
			return nil
		}
		waiting, err := sourcesInJobsAfter(tx, 0)
		if err != nil || waiting[messageID] {
			return err
		}
		kept, err = store.KeepMessageWords(tx, messageID, content, at.UTC().Format(time.RFC3339Nano), windowSame)
		return err
	})
	return kept, err
}

// MessagesDeleted applies the deletion of Discord messages: stored text, chat context and the text
// the console's turn history kept of them.
func (s *Service) MessagesDeleted(guildID, channelID string, messageIDs []string) error {
	ctx, done, ok := s.beginWork(context.Background())
	if !ok {
		return nil
	}
	defer done()
	if s.Store == nil || len(messageIDs) == 0 {
		return nil
	}
	s.liveDelete(guildID, channelID, messageIDs)
	s.dropReminderContextMessages(messageIDs)
	s.partForget(guildID, channelID, messageIDs)
	s.pollMessagesDeleted(messageIDs)
	if err := s.queueSourceChange(guildID, channelID, messageIDs, "delete", "", s.Clock.Now()); err != nil {
		s.Log.Warn("memory: deletion intent not committed", "err", err)
		return err
	}
	eng, err := s.MemoryEngine()
	if err != nil {
		return err
	}
	n, err := eng.DeleteContext(ctx, messageIDs, s.Clock.Now())
	if err != nil {
		s.Log.Warn("memory: deletion not applied", "err", err)
		return err
	}
	if n > 0 {
		s.emit("memory", "deleted", map[string]any{"channel": channelID, "n": n})
	}
	if s.practice {
		s.drainRedactions(context.Background())
	}
	return nil
}

// queueSourceChange makes the erasure of every copy of these messages durable. It runs before the source
// is changed or deleted, because the legacy copies that kept only text are found by the source's text.
// The event path pays for two indexed lookups and one insert; the worker (evidence_index.go) does the
// rest without holding any lock a chat reply needs.
func (s *Service) queueSourceChange(guildID, channelID string, messageIDs []string, operation, content string, at time.Time) error {
	ctx, done, ok := s.beginWork(context.Background())
	if !ok {
		return errors.New("service closed")
	}
	defer done()
	return s.queueSourceChangeContext(ctx, guildID, channelID, messageIDs, operation, content, at)
}

func (s *Service) queueSourceChangeContext(ctx context.Context, guildID, channelID string, messageIDs []string, operation, content string, at time.Time) error {
	j := redactionJob{Guild: guildID, Channel: channelID, Messages: messageIDs, Operation: operation, Content: content, At: at.UTC().Format(time.RFC3339Nano)}
	id, err := s.Store.EnqueueEvent("source-change", j)
	if err != nil {
		return err
	}
	err = s.Store.ApplyEvent(ctx, "source-change", id, func() error {
		return s.commitSourceChange(ctx, guildID, channelID, messageIDs, operation, content, at)
	})
	if storage.IsBusy(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		s.startEventWorker()
		return err
	}
	if err != nil {
		// A rejected transaction preserves source text and tombstones together. Keep the intent for
		// recovery on restart rather than silently discard it or spin against a permanent SQL error.
		return errors.Join(err, s.Store.SuspendEvent("source-change", id))
	}
	return nil
}

func (s *Service) commitSourceChange(ctx context.Context, guildID, channelID string, messageIDs []string, operation, content string, at time.Time) error {
	db, err := s.adminDB()
	if err != nil {
		return err
	}
	j := redactionJob{Guild: guildID, Channel: channelID, Messages: messageIDs, Operation: operation, Content: content, At: at.UTC().Format(time.RFC3339Nano)}
	err = db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		raw, _ := json.Marshal(messageIDs)
		rows, err := tx.Query(`SELECT observation_id,content FROM memory_observations WHERE platform='discord' AND native_message_id IN (SELECT value FROM json_each(?))`, string(raw))
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, text string
			if err := rows.Scan(&id, &text); err != nil {
				rows.Close()
				return err
			}
			j.Obs = append(j.Obs, id)
			if len([]rune(text)) >= prefixRunes {
				j.Texts = append(j.Texts, clip(text, 80))
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		obsJSON, _ := json.Marshal(j.Obs)
		rows, err = tx.Query(`SELECT DISTINCT l.leaf_id FROM memory_leaves l,json_each(l.observation_ids) x WHERE x.value IN (SELECT value FROM json_each(?))`, string(obsJSON))
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			j.Leaves = append(j.Leaves, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		return s.enqueueRedactionTx(tx, j)
	})
	if err != nil {
		return err
	}
	s.kickRedaction()
	return s.applyPracticePrivacy(func(p *Service) error {
		if operation == "edit" && len(messageIDs) > 0 {
			return p.MessageEdited(guildID, channelID, messageIDs[0], content, at)
		}
		return p.MessagesDeleted(guildID, channelID, messageIDs)
	})
}

// walkEvidence visits retrieval records in both turn hits and memory.search tool data.
func walkEvidence(v any, fn func(map[string]any, []string)) {
	switch x := v.(type) {
	case map[string]any:
		var ids []string
		for _, k := range []string{"visible_observation_ids", "observation_ids", "leaf_observation_ids", "observation_id"} {
			switch a := x[k].(type) {
			case string:
				ids = append(ids, a)
			case []any:
				for _, id := range a {
					if s, ok := id.(string); ok {
						ids = append(ids, s)
					}
				}
			}
		}
		if len(ids) > 0 || x["doc_id"] != nil && x["excerpt"] != nil {
			fn(x, uniqueStrings(ids))
			return
		}
		for _, child := range x {
			walkEvidence(child, fn)
		}
	case []any:
		for _, child := range x {
			walkEvidence(child, fn)
		}
	}
}

func (s *Service) referenceEvidence(body map[string]any) {
	walkEvidence(body, func(hit map[string]any, ids []string) {
		if len(ids) == 0 {
			var raw string
			if s.Store.Reader().QueryRow(`SELECT observation_ids FROM memory_leaves WHERE leaf_id=?`, hit["doc_id"]).Scan(&raw) == nil {
				_ = json.Unmarshal([]byte(raw), &ids)
				hit["observation_ids"] = ids
			}
		}
		delete(hit, "excerpt")
		delete(hit, "summary")
		delete(hit, "speakers")
		// Keep one-way speaker references too, for consent withdrawal after a source was removed.
		var refs, native []string
		for _, id := range ids {
			var account string
			var messageID *string
			if s.Store.Reader().QueryRow(`SELECT actor_account, native_message_id FROM memory_observations WHERE observation_id=?`, id).Scan(&account, &messageID) == nil {
				if ref := personRef(account); ref != "" {
					refs = append(refs, ref)
				}
				if messageID != nil {
					native = append(native, *messageID)
				}
			}
		}
		hit["speaker_refs"], hit["message_ids"] = uniqueStrings(refs), uniqueStrings(native)
	})
}

func (s *Service) resolveEvidence(body map[string]any) {
	walkEvidence(body, func(hit map[string]any, ids []string) {
		var lines, speakers []string
		if len(ids) == 0 {
			var raw string
			if s.Store.Reader().QueryRow(`SELECT observation_ids FROM memory_leaves WHERE leaf_id=?`, hit["doc_id"]).Scan(&raw) == nil {
				_ = json.Unmarshal([]byte(raw), &ids)
			}
		}
		// Visible IDs describe the packed excerpt; fall back to all IDs for older records.
		if v, ok := hit["visible_observation_ids"].([]any); ok && len(v) > 0 {
			ids = nil
			for _, id := range v {
				if str, ok := id.(string); ok {
					ids = append(ids, str)
				}
			}
		}
		for _, id := range ids {
			var content, speaker, account string
			if s.Store.Reader().QueryRow(`SELECT content, actor_label, actor_account FROM memory_observations WHERE observation_id=? AND deleted_at=''`, id).Scan(&content, &speaker, &account) != nil || !s.MemoryAllowed(account) {
				continue
			}
			lines = append(lines, speaker+": "+content)
			speakers = append(speakers, speaker)
		}
		hit["excerpt"], hit["speakers"] = clip(strings.Join(lines, "\n"), 1200), uniqueStrings(speakers)
	})
}

func evidenceTextMatches(v any, texts []string) bool {
	switch x := v.(type) {
	case string:
		for _, text := range texts {
			if strings.Contains(x, strings.TrimSuffix(text, "…")) {
				return true
			}
		}
	case []any:
		for _, c := range x {
			if evidenceTextMatches(c, texts) {
				return true
			}
		}
	case map[string]any:
		for _, c := range x {
			if evidenceTextMatches(c, texts) {
				return true
			}
		}
	}
	return false
}

func evidenceMatches(v any, keys map[string]bool) bool {
	switch x := v.(type) {
	case string:
		return keys[x]
	case []any:
		for _, c := range x {
			if evidenceMatches(c, keys) {
				return true
			}
		}
	case map[string]any:
		for _, c := range x {
			if evidenceMatches(c, keys) {
				return true
			}
		}
	}
	return false
}
