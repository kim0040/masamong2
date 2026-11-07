// Package catalog is the product Action registry (17장).
// Agent-facing IDs shared with the test sandbox: contracts/sandbox/tool-parity.json
// Capability/agent registry is separate: backup and secret Actions stay agent_exposure=none.
package catalog

const (
	Executable = "executable"
	Deferred   = "deferred"
)

// Reason used for RAG/12 items. GUI must not render a success execute control for these.
const RAGDeferred = "rag_dependency_deferred"

// ImplementationDeferred marks a catalog surface whose product contract exists but whose live
// provider or durable domain implementation does not. Such Actions must fail before a fixture can
// be mistaken for a real success.
const ImplementationDeferred = "implementation_deferred"

// OperatorDisabled marks an Action the operator switched off on purpose. storage.migrate moved an
// instance between SQLite and TiDB; with the TiDB backend disabled (storage.TiDBEnabled) it has
// nothing left to do, so it is off with the backend rather than half-working.
const OperatorDisabled = "disabled_by_operator"

type Descriptor struct {
	ID             string
	CLI            string
	GUIRoute       string // a page served by internal/transport/web (parity test: web.TestCatalogGUIRoutesAreServed)
	Availability   string
	DeferredReason string
	Effects        string
	AgentExposure  string
	Permission     string
}

func All() []Descriptor {
	exec := func(id, cli, route, effects string) Descriptor {
		return Descriptor{ID: id, CLI: cli, GUIRoute: route, Availability: Executable, Effects: effects, AgentExposure: "none", Permission: id}
	}
	disabled := func(id, cli, route string) Descriptor {
		return Descriptor{ID: id, CLI: cli, GUIRoute: route, Availability: Deferred, DeferredReason: OperatorDisabled, Effects: "none", AgentExposure: "none", Permission: id}
	}
	return []Descriptor{
		exec("setup.resume", "setup", "/setup", "internal_write"),
		exec("config.show", "config show", "/settings", "read"),
		exec("config.diff", "config diff", "/settings", "read"),
		exec("config.validate", "config validate", "/settings", "read"),
		exec("config.apply", "config apply", "/settings", "internal_write"),
		exec("config.rollback", "config rollback", "/settings", "internal_write"),
		exec("providers.list", "providers list", "/connections", "read"),
		exec("providers.test", "providers test", "/connections", "read"),
		exec("providers.credentials", "providers credentials", "/connections", "internal_write"),
		exec("models.list", "models list", "/connections", "read"),
		exec("models.select", "models select", "/connections", "internal_write"),
		exec("models.embed_width", "models embed-width", "/connections", "internal_write"),
		exec("providers.add", "providers add", "/connections", "internal_write"),
		exec("providers.remove", "providers remove", "/connections", "internal_write"),
		exec("models.recommend", "models recommend", "/connections", "internal_write"),
		exec("channels.list", "channels list", "/rooms", "read"),
		exec("channels.configure", "channels configure", "/rooms", "internal_write"),
		exec("channels.attachments.add", "channels attachments add", "/rooms", "internal_write"),
		exec("channels.attachments.remove", "channels attachments remove", "/rooms", "internal_write"),
		exec("channels.ops_log", "channels ops-log", "/rooms", "read"),
		exec("channels.view.show", "channels view show", "/rooms", "read"),
		exec("channels.view.attach", "channels view attach", "/rooms", "internal_write"),
		exec("channels.view.detach", "channels view detach", "/rooms", "internal_write"),
		exec("memory.blocks.list", "memory blocks list", "/blocks", "read"),
		exec("memory.blocks.show", "memory blocks show", "/blocks", "read"),
		exec("memory.blocks.prepare", "memory blocks prepare", "/blocks", "external_write"),
		exec("memory.blocks.update", "memory blocks update", "/blocks", "internal_write"),
		exec("memory.blocks.delete", "memory blocks delete", "/blocks", "internal_write"),
		exec("personas.list", "personas list", "/persona", "read"),
		exec("personas.apply", "personas apply", "/persona", "internal_write"),
		exec("imports.start", "imports start", "/blocks", "internal_write"),
		exec("messages.search", "messages search", "/memory", "read"),
		exec("people.merge", "people merge", "/memory", "internal_write"),
		exec("people.list", "people list", "/memory", "read"),
		exec("people.servers", "people servers", "/people", "read"),
		exec("memory.search", "memory search", "/memory", "read"),
		exec("memory.review", "memory review", "/memory", "internal_write"),
		exec("memory.reviews", "memory reviews", "/memory", "read"),
		exec("memory.episodes.list", "memory episodes list", "/memory", "read"),
		exec("memory.episodes.show", "memory episodes show", "/memory", "read"),
		exec("memory.episodes.forget", "memory episodes forget", "/memory", "internal_write"),
		exec("memory.mine.show", "memory mine show", "/memory", "read"),
		exec("memory.mine.forget", "memory mine forget", "/memory", "internal_write"),
		exec("memory.episodes.extract", "memory episodes extract", "/blocks", "external_write"),
		exec("memory.state.show", "memory state show", "/memory", "read"),
		exec("events.timeline", "events", "/memory", "read"),
		exec("memory.reindex", "memory reindex", "/blocks", "external_write"),
		exec("memory.backfill", "memory backfill", "/rooms", "internal_write"),
		exec("chat.respond", "chat", "/lab", "external_read"),
		exec("tools.list", "tools list", "/tools", "read"),
		exec("tools.test", "tools test", "/tools", "external_read"),
		exec("tools.configure", "tools configure", "/tools", "internal_write"),
		exec("agent.simulate", "agent simulate", "/lab", "read"),
		exec("trace.show", "trace show", "/logs", "read"),
		exec("weather.query", "weather", "/tools", "external_read"),
		exec("disasters.query", "disasters", "/tools", "external_read"),
		exec("finance.quote", "finance quote", "/tools", "external_read"),
		exec("finance.fx", "finance fx", "/tools", "external_read"),
		exec("search.web", "search", "/tools", "external_read"),
		exec("web.read", "read", "/tools", "external_read"),
		exec("images.generate", "images", "/tools", "external_write"),
		exec("fortune.query", "fortune", "/tools", "external_read"),
		exec("fortune.subscribe", "fortune subscribe", "/people", "internal_write"),
		exec("notices.status", "notices status", "/tools", "read"),
		exec("notices.subscribe", "notices subscribe", "/people", "internal_write"),
		exec("transfer.recent", "transfer-notices", "/tools", "read"),
		exec("summaries.channel", "summaries", "/rooms", "external_read"),
		exec("polls.create", "polls create", "/tools", "internal_write"),
		exec("polls.vote", "polls vote", "/tools", "internal_write"),
		exec("polls.close", "polls close", "/tools", "internal_write"),
		exec("polls.result", "polls result", "/tools", "read"),
		exec("stats.query", "stats query", "/stats", "read"),
		exec("reports.create", "reports create", "/stats", "read"),
		exec("participation.mode", "participation", "/rooms", "internal_write"),
		exec("conversation.log", "conversation log", "/rooms", "read"),
		exec("room.style.show", "room style show", "/rooms", "read"),
		exec("room.style.build", "room style build", "/rooms", "external_write"),
		exec("room.style.preview", "room style preview", "/rooms", "external_read"),
		exec("room.style.decide", "room style decide", "/rooms", "internal_write"),
		exec("reminders.propose", "reminders propose", "/rooms", "internal_write"),
		exec("reminders.update", "reminders", "/rooms", "internal_write"),
		exec("preferences.set", "preferences", "/people", "internal_write"),
		exec("privacy.consent", "privacy", "/people", "internal_write"),
		exec("access.policy", "access", "/people", "internal_write"),
		exec("users.list", "users", "/people", "read"),
		exec("jobs.list", "jobs list", "/jobs", "read"),
		exec("jobs.watch", "jobs watch", "/jobs", "read"),
		exec("jobs.get", "jobs get", "/jobs", "read"),
		exec("jobs.cancel", "jobs cancel", "/jobs", "internal_write"),
		exec("jobs.retry", "jobs retry", "/jobs", "internal_write"),
		exec("usage.budgets", "usage", "/usage", "read"),
		exec("usage.period", "usage period", "/usage", "read"),
		exec("usage.reset", "usage reset", "/usage", "internal_write"),
		exec("runtime.doctor", "doctor", "/logs", "read"),
		exec("eval.run", "eval run", "/lab", "external_read"),
		exec("eval.runs", "eval runs", "/lab", "read"),
		exec("backup.list", "backup list", "/maintenance", "read"),
		exec("backup.create", "backup create", "/maintenance", "internal_write"),
		exec("backup.verify", "backup verify", "/maintenance", "read"),
		exec("backup.restoreDryRun", "backup restore-preview", "/maintenance", "read"),
		exec("backup.restore", "backup restore", "/maintenance", "internal_write"),
		exec("backup.delete", "backup delete", "/maintenance", "internal_write"),
		exec("schedules.list", "schedules list", "/jobs", "read"),
		exec("schedules.run", "schedules run", "/jobs", "internal_write"),
		exec("storage.status", "storage status", "/maintenance", "read"),
		disabled("storage.migrate", "storage migrate", "/maintenance"), // with the TiDB backend (OperatorDisabled)
		exec("admin.prefs", "prefs show", "/", "read"),
		exec("admin.prefs.set", "prefs set", "/", "internal_write"),
		exec("runtime.status", "runtime status", "/", "read"),
		exec("runtime.pause", "runtime pause", "/", "internal_write"),
		exec("runtime.restart", "runtime restart", "/maintenance", "internal_write"),
		exec("runtime.stop", "runtime stop", "/maintenance", "internal_write"),
		exec("runtime.history", "runtime history", "/logs", "read"),
		exec("runtime.autostart", "autostart status", "/maintenance", "read"),
		exec("runtime.autostart.set", "autostart set", "/maintenance", "internal_write"),
		exec("alerts.status", "alerts status", "/maintenance", "read"),
		exec("alerts.test", "alerts test", "/maintenance", "external_write"),
		exec("serverlog.test", "serverlog test", "/rooms", "external_write"),
		exec("updates.status", "updates status", "/maintenance", "read"),
		exec("updates.check", "updates check", "/maintenance", "read"),
		exec("updates.apply", "updates apply", "/maintenance", "internal_write"),
		exec("instance.reset", "reset", "/maintenance", "internal_write"),
		exec("feedback.summary", "feedback summary", "/turns", "read"),
		exec("config.changes", "config changes", "/settings", "read"),
		exec("config.snapshots", "config snapshots", "/settings", "read"),
		exec("config.snapshot", "config snapshot save", "/settings", "internal_write"),
		exec("config.snapshot.restore", "config snapshot restore", "/settings", "internal_write"),
		exec("config.snapshot.delete", "config snapshot delete", "/settings", "internal_write"),
		exec("imports.preview", "imports preview", "/blocks", "read"),
		exec("dashboard.overview", "dashboard", "/", "read"),
		exec("turns.list", "turns list", "/turns", "read"),
		exec("turns.get", "turns get", "/turns", "read"),
		exec("turns.mark", "turns mark", "/turns", "internal_write"),
		exec("turns.sample", "turns sample", "/turns", "read"),
		exec("turns.marks", "turns marks", "/turns", "read"),
		exec("audit.list", "audit", "/settings", "read"),
		exec("stats.series", "stats series", "/stats", "read"),
		exec("lab.reviews", "lab reviews", "/lab", "read"),
		exec("lab.review", "lab review", "/lab", "internal_write"),
		exec("config.document", "config document", "/settings", "read"),
		exec("config.patch", "config patch", "/settings", "internal_write"),
		exec("migration.apply", "migration apply", "/maintenance", "internal_write"),
		exec("runtime.drain", "runtime drain", "/maintenance", "internal_write"),
		exec("locales.list", "locales", "/persona", "read"),
		exec("help.version", "help", "/", "read"),
		exec("runtime.logs", "runtime logs", "/logs", "read"),
		exec("channels.invite", "channels invite", "/connections", "read"),
		exec("discord.check", "discord check", "/connections", "internal_write"),
		exec("disasters.alerts", "disasters alerts", "/tools", "read"),
		exec("places.search", "places search", "/tools", "external_read"),
		exec("activity.ranking", "activity ranking", "/tools", "read"),
		exec("channels.permissions", "channels permissions", "/rooms", "read"),
		exec("channels.leave", "channels leave", "/rooms", "external_write"),
	}
}

func Lookup(id string) (Descriptor, bool) {
	for _, d := range All() {
		if d.ID == id {
			return d, true
		}
	}
	return Descriptor{}, false
}

func IsExecutable(id string) bool {
	d, ok := Lookup(id)
	return ok && d.Availability == Executable
}
