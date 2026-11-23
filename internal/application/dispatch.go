package application

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
)

// actionHandler runs one catalog Action. Service.execute does the checks every Action shares (catalog
// lookup, deferred and practice-mode refusal, setup, user defaults, gates) and then hands the request
// to the handler registered here.
type actionHandler func(s *Service, ctx context.Context, req action.Request) action.Result

// plainHandler adapts a handler that never waits on a context.
func plainHandler(f func(*Service, action.Request) action.Result) actionHandler {
	return func(s *Service, _ context.Context, req action.Request) action.Result { return f(s, req) }
}

var (
	actionTableOnce sync.Once
	actionTable     map[string]actionHandler
)

// handlerFor returns the handler registered for an Action ID. The table is built on first use and never
// changes afterwards. It is not a package-level initializer, because handlers reach Execute again (a
// memory action runs the chat tools), which Go would report as an initialization cycle.
func handlerFor(id string) (actionHandler, bool) {
	actionTableOnce.Do(func() { actionTable = buildActionTable() })
	h, ok := actionTable[id]
	return h, ok
}

// buildActionTable lists every Action ID that Service.execute can run. An ID listed twice panics, so two
// handlers can never silently share one. catalog_dispatch_test.go checks the table against the catalog.
func buildActionTable() map[string]actionHandler {
	table := map[string]actionHandler{}
	register := func(h actionHandler, ids ...string) {
		for _, id := range ids {
			if _, dup := table[id]; dup {
				panic("application: Action " + id + " is registered twice")
			}
			table[id] = h
		}
	}

	register((*Service).setup, "setup.resume")
	register((*Service).doctor, "runtime.doctor")
	register(plainHandler((*Service).configShow), "config.show")
	register(plainHandler((*Service).configDiff), "config.diff", "config.validate")
	register(plainHandler((*Service).configApply), "config.apply")
	register(plainHandler((*Service).configRollback), "config.rollback")
	register(plainHandler((*Service).jobsAction), "jobs.list", "jobs.get", "jobs.cancel", "jobs.retry", "jobs.watch")
	register(plainHandler((*Service).help), "help.version")
	register(plainHandler((*Service).locales), "locales.list")
	register((*Service).weather, "weather.query")
	register((*Service).disasters, "disasters.query")
	register((*Service).fortuneQuery, "fortune.query")
	register(plainHandler((*Service).fortuneSubscribe), "fortune.subscribe")
	register((*Service).noticesStatus, "notices.status")
	register((*Service).noticesSubscribe, "notices.subscribe")
	register(plainHandler((*Service).transferRecent), "transfer.recent")
	register(plainHandler((*Service).migrationApply), "migration.apply")
	register(plainHandler((*Service).polls), "polls.create", "polls.vote", "polls.close", "polls.result")
	register((*Service).images, "images.generate")
	register((*Service).finance, "finance.quote", "finance.fx")
	register((*Service).search, "search.web")
	register((*Service).webRead, "web.read")
	register((*Service).placesSearch, "places.search")
	register(plainHandler((*Service).activityRanking), "activity.ranking")
	register((*Service).providers, "providers.list", "providers.test", "providers.credentials", "providers.add", "providers.remove",
		"models.list", "models.select", "models.recommend", "models.embed_width")
	register((*Service).discordCheck, "discord.check")
	register(plainHandler((*Service).disasterAlertsStatus), "disasters.alerts")
	register(plainHandler((*Service).channels), "channels.list", "channels.configure", "channels.invite", "channels.leave",
		"channels.attachments.add", "channels.attachments.remove", "channels.ops_log", "channels.view.show", "channels.view.attach", "channels.view.detach")
	register((*Service).memoryBlocks, "memory.blocks.list", "memory.blocks.show", "memory.blocks.prepare", "memory.blocks.update", "memory.blocks.delete")
	register((*Service).evalAction, "eval.run", "eval.runs")
	register(plainHandler((*Service).memoryReviewAction), "memory.review", "memory.reviews")
	register((*Service).roomMemoryAction, "memory.episodes.list", "memory.episodes.show", "memory.episodes.forget", "memory.episodes.extract", "memory.state.show")
	register((*Service).mineAction, "memory.mine.show", "memory.mine.forget")
	register(plainHandler((*Service).peopleAction), "people.list", "people.merge")
	register(plainHandler((*Service).peopleServers), "people.servers")
	register((*Service).importsPreview, "imports.preview")
	register((*Service).memoryActions, "messages.search", "memory.search", "imports.start", "memory.reindex", "chat.respond")
	register((*Service).memoryBackfill, "memory.backfill")
	register((*Service).summariesChannel, "summaries.channel")
	register((*Service).eventsTimeline, "events.timeline")
	register(plainHandler((*Service).personas), "personas.list", "personas.apply")
	register((*Service).agent, "tools.list", "tools.test", "tools.configure", "agent.simulate", "trace.show")
	register(plainHandler((*Service).stats), "stats.query", "reports.create")
	register((*Service).statsExport, "stats.export")
	register((*Service).privacyConsentContext, "privacy.consent")
	register(plainHandler((*Service).preferencesAction), "preferences.set")
	register(plainHandler((*Service).accessPolicy), "access.policy")
	register(plainHandler((*Service).usersList), "users.list")
	register(plainHandler((*Service).remindersPropose), "reminders.propose")
	register(plainHandler((*Service).remindersUpdate), "reminders.update")
	register(plainHandler((*Service).participationMode), "participation.mode")
	register(plainHandler((*Service).conversationLog), "conversation.log")
	register((*Service).roomStyleAction, "room.style.show", "room.style.build", "room.style.preview", "room.style.decide")
	register(plainHandler((*Service).usage), "usage.budgets")
	register(plainHandler((*Service).usagePeriod), "usage.period")
	register(plainHandler((*Service).usageReset), "usage.reset")
	register((*Service).storageActions, "storage.status", "storage.migrate")
	register((*Service).adminActions, "admin.prefs", "admin.prefs.set", "runtime.pause", "runtime.status", "runtime.restart", "runtime.stop", "dashboard.overview",
		"turns.list", "turns.get", "turns.mark", "turns.sample", "turns.marks", "audit.list", "stats.series", "lab.reviews", "lab.review", "config.document", "config.patch",
		"config.snapshots", "config.snapshot", "config.snapshot.delete", "config.snapshot.restore", "config.changes")
	register((*Service).backup, "backup.list", "backup.create", "backup.verify", "backup.restoreDryRun", "backup.restore", "backup.delete")
	register((*Service).schedulesAction, "schedules.list", "schedules.run")
	register((*Service).statsRollupAction, "stats.rollup")
	register(plainHandler((*Service).channelsPermissions), "channels.permissions")
	register(plainHandler((*Service).autostartAction), "runtime.autostart", "runtime.autostart.set")
	register((*Service).alertsAction, "alerts.status", "alerts.test")
	register((*Service).serverLogTest, "serverlog.test")
	register(plainHandler((*Service).feedbackAction), "feedback.summary")
	register(plainHandler((*Service).instanceReset), "instance.reset")
	register((*Service).updatesAction, "updates.status", "updates.check", "updates.apply")
	register(plainHandler((*Service).updatesNotes), "updates.notes")
	register(plainHandler((*Service).updatesSubscribe), "updates.subscribe")
	register(plainHandler((*Service).runtimeHistoryAction), "runtime.history")
	register(plainHandler((*Service).runtimeDrain), "runtime.drain")
	register(plainHandler((*Service).runtimeLogs), "runtime.logs")
	return table
}

// summariesChannel rebuilds a channel summary. A saved summary is read as it is; a fresh one first
// waits for pending source erasures, because the summary reuses a stable holder ID.
func (s *Service) summariesChannel(ctx context.Context, req action.Request) action.Result {
	if !boolVal(inputMap(req), "saved") {
		// A summary reuses a stable holder ID. Finish older source erasures before rebuilding
		// it, so a delayed job cannot erase fresh text that already reflects that source edit.
		wait, cancel := context.WithTimeout(ctx, 30*time.Second)
		s.DrainRedactions(wait)
		cancel()
		if pending, err := s.redactionPending(); err != nil || pending {
			res := action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
			res.Error.Retryable = true
			return res
		}
	}
	return s.channelSummary(ctx, req)
}

func (s *Service) runtimeHistoryAction(req action.Request) action.Result {
	if _, err := s.adminDB(); err != nil {
		return s.failErr(req, errcode.IO, "error.storage", err, nil)
	}
	return s.runtimeHistory(req)
}

// runtimeDrain is runtime.pause for everything, under a name that reads as the operator's intent.
func (s *Service) runtimeDrain(req action.Request) action.Result {
	in := inputMap(req)
	draining := true
	if _, ok := in["draining"]; ok {
		draining = boolVal(in, "draining")
	} else if _, ok := in["enabled"]; ok {
		draining = boolVal(in, "enabled")
	}
	pauseReq := req
	pauseReq.Action = "runtime.pause"
	pauseReq.Input, _ = json.Marshal(map[string]any{"scope": "all", "paused": draining, "reason": "runtime.drain"})
	res := s.runtimePause(pauseReq)
	res.Action = req.Action
	if res.OK {
		res.Output["draining"] = draining
		if draining {
			res.Output["effect"] = "new chat replies and commands are paused"
		} else {
			res.Output["effect"] = "chat replies and commands are resumed"
		}
	}
	return res
}
