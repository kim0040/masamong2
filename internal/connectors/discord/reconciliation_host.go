package discord

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"runtime/debug"
	"time"

	"masamong/internal/memory/store"
)

// The gateway side of message reconciliation: where the queue lives and how one change is applied. The
// queue itself is messageReconciler (reconciliation.go).

// reconciler is the gateway's queue of deferred message changes, bound to this gateway on first use.
func (g *Gateway) reconciler() *messageReconciler {
	return g.recon.bind(g)
}

func (g *Gateway) logger() *slog.Logger {
	return g.log
}

// applyMessageChange applies an edit or delete that just arrived, queueing it when the store cannot take
// it now.
func (g *Gateway) applyMessageChange(event messageReconciliation) {
	g.reconciler().applyMessageChange(event)
}

func (g *Gateway) reconciliationRoot() (string, error) {
	if g.svc == nil || g.svc.DataRoot == "" {
		return "", errors.New("discord: message reconciliation has no data root")
	}
	return g.svc.DataRoot, nil
}

func (g *Gateway) applyMessageEvent(event messageReconciliation) (err error) {
	defer func() {
		if recover() != nil {
			// Panic values may contain private text. Keep only the category and call stack, and
			// return through the normal retry/queue path for both live and deferred changes.
			err = errors.New("discord: reconciliation apply panicked")
			g.log.Error(err.Error(), "stack", string(debug.Stack()))
		}
	}()
	if event.Operation == "edit" && len(event.MessageIDs) == 1 {
		current, err := g.reconciliationEditCurrent(event)
		if err != nil || !current {
			return err
		}
	}
	if g.messageEventApply != nil {
		return g.messageEventApply(event)
	}
	if g.svc == nil {
		return errors.New("discord: message change has no application service")
	}
	switch event.Operation {
	case "edit":
		if len(event.MessageIDs) != 1 {
			return errors.New("discord: edit reconciliation must name one message")
		}
		if event.SameFiles {
			kept, err := g.svc.MessageKeptItsWords(event.GuildID, event.ChannelID, event.MessageIDs[0], event.Content, event.At)
			if err != nil || kept {
				return err
			}
		}
		return g.svc.MessageEdited(event.GuildID, event.ChannelID, event.MessageIDs[0], event.Content, event.At)
	case "delete":
		return g.svc.MessagesDeleted(event.GuildID, event.ChannelID, event.MessageIDs)
	default:
		return errors.New("discord: unknown message reconciliation operation")
	}
}

// The committed revision, not removal of a queue file, decides whether an edit may reach the live
// window. This also covers an old legacy record migrated after a newer live edit succeeded.
func (g *Gateway) reconciliationEditCurrent(event messageReconciliation) (bool, error) {
	if g.svc == nil || g.svc.Store == nil {
		return true, nil
	}
	reader := g.svc.Store.Reader()
	ctx, cancel := context.WithTimeout(g.reconciler().reconciliationContext(), 5*time.Second)
	defer cancel()
	// an edit that kept the words leaves no tombstone, and is a revision all the same
	if newer, err := store.KeptRevisionNewer(ctx, reader, event.MessageIDs[0], event.At); err != nil || newer {
		return false, err
	}
	var raw string
	err := reader.QueryRowContext(ctx, `SELECT at FROM privacy_tombstones WHERE kind='message' AND source_id=?`, event.MessageIDs[0]).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	latest, err := time.Parse(time.RFC3339Nano, raw)
	if err == nil && !event.At.After(latest) {
		// An equal intent already has durable application replay. Replaying it here must not
		// overwrite live text with a different payload that happens to have the same timestamp.
		return false, nil
	}
	// Match the store's revision policy: an unparseable timestamp is not a permanent retry
	// error. Still check deletion and the observation revision before touching live effects.
	var edited, deleted string
	err = reader.QueryRowContext(ctx, `SELECT edited_at,deleted_at FROM memory_observations WHERE platform='discord' AND native_message_id=?`, event.MessageIDs[0]).Scan(&edited, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil || deleted != "" {
		return false, err
	}
	if edited != "" {
		applied, err := time.Parse(time.RFC3339Nano, edited)
		if err == nil && !event.At.After(applied) {
			return false, nil
		}
	}
	return true, nil
}

func (g *Gateway) applyQueuedMessageEvent(item messageReconciliation) error {
	if _, ok := g.beginWork(); !ok {
		return context.Canceled
	}
	defer g.inflight.Done()
	return g.applyMessageEvent(item)
}

func (g *Gateway) startReconciliationDrain(ctx context.Context) {
	g.workMu.Lock()
	defer g.workMu.Unlock()
	if g.draining {
		return
	}
	r := g.reconciler()
	r.ctx, r.cancel = context.WithCancel(ctx)
	r.done = make(chan struct{})
	go r.runReconciliationDrain(r.ctx)
}

// ReconciliationBlocked is the number of quarantined recovery files that wait for an operator. It
// implements application.DiscordReconciliationReporter so the doctor and the console's notifications
// can show what used to be only an error log line. The count is kept by the recount that runs on every
// drain pass, so this is a cheap atomic read.
func (g *Gateway) ReconciliationBlocked() int {
	return int(g.recon.blocked.Load())
}
