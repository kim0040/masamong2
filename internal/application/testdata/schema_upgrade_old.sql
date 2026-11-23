-- An older database, as the first releases of these tables made it: the columns the central add-column
-- list of migrateAdmin gave them later (and the evidence_state columns of migrateEvidence) are not there
-- yet. traces comes from storage.Open itself (without evidence_state). Only synthetic rows. schema_golden_internal_test.go migrates it and pins the result.
CREATE TABLE audit_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		at TEXT NOT NULL,
		action TEXT NOT NULL,
		via TEXT NOT NULL,
		ok INTEGER NOT NULL,
		code TEXT NOT NULL DEFAULT '',
		rev_before INTEGER NOT NULL DEFAULT 0,
		rev_after INTEGER NOT NULL DEFAULT 0,
		summary TEXT NOT NULL DEFAULT ''
	);
CREATE TABLE turn_log (
		id TEXT PRIMARY KEY,
		at TEXT NOT NULL,
		day TEXT NOT NULL,
		hour INTEGER NOT NULL,
		guild_id TEXT NOT NULL,
		channel_id TEXT NOT NULL,
		outcome TEXT NOT NULL,
		tools TEXT NOT NULL DEFAULT '',
		hits INTEGER NOT NULL DEFAULT 0,
		cost_usd REAL NOT NULL DEFAULT 0,
		latency_ms INTEGER NOT NULL DEFAULT 0,
		lab INTEGER NOT NULL DEFAULT 0,
		body TEXT NOT NULL
	);
CREATE TABLE reply_messages (
		message_id TEXT PRIMARY KEY,
		turn_id TEXT NOT NULL,
		guild_id TEXT NOT NULL,
		channel_id TEXT NOT NULL,
		at TEXT NOT NULL
	);
CREATE TABLE participation_log (
		id TEXT PRIMARY KEY,
		guild_id TEXT NOT NULL,
		channel_id TEXT NOT NULL,
		message_id TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		mode TEXT NOT NULL DEFAULT '',
		kind TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL,
		reason TEXT NOT NULL DEFAULT '',
		score REAL NOT NULL DEFAULT 0,
		message TEXT NOT NULL DEFAULT '',
		draft TEXT NOT NULL DEFAULT '',
		features TEXT NOT NULL DEFAULT '{}',
		verdict TEXT NOT NULL DEFAULT '',
		cost_usd REAL NOT NULL DEFAULT 0
	);
CREATE TABLE channel_summary (
		guild_id TEXT NOT NULL,
		channel_id TEXT NOT NULL,
		anchor_observation TEXT NOT NULL,
		anchor_time TEXT NOT NULL,
		summary TEXT NOT NULL,
		messages INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL,
		PRIMARY KEY(guild_id, channel_id)
	);
CREATE TABLE llm_calls (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		turn_id TEXT NOT NULL DEFAULT '',
		job_id TEXT NOT NULL DEFAULT '',
		role TEXT NOT NULL,
		model TEXT NOT NULL DEFAULT '',
		provider TEXT NOT NULL DEFAULT '',
		prompt_tokens INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0,
		cached_tokens INTEGER NOT NULL DEFAULT 0,
		latency_ms INTEGER NOT NULL DEFAULT 0,
		cost_usd REAL NOT NULL DEFAULT 0,
		class TEXT NOT NULL DEFAULT '',
		fallback INTEGER NOT NULL DEFAULT 0,
		finish_reason TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	);
CREATE TABLE tool_calls (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		turn_id TEXT NOT NULL DEFAULT '',
		tool TEXT NOT NULL,
		param_keys TEXT NOT NULL DEFAULT '[]',
		param_sources TEXT NOT NULL DEFAULT '{}',
		guard TEXT NOT NULL DEFAULT 'allowed',
		status TEXT NOT NULL DEFAULT '',
		latency_ms INTEGER NOT NULL DEFAULT 0,
		cost_usd REAL NOT NULL DEFAULT 0,
		result_chars INTEGER NOT NULL DEFAULT 0,
		class TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	);
CREATE TABLE reminders (
			id TEXT PRIMARY KEY,
			owner_id TEXT NOT NULL,
			owner_name TEXT NOT NULL DEFAULT '',
			guild_id TEXT NOT NULL DEFAULT '',
			channel_id TEXT NOT NULL DEFAULT '',
			destination TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL,
			original_text TEXT NOT NULL DEFAULT '',
			timezone TEXT NOT NULL,
			locale TEXT NOT NULL DEFAULT '',
			candidates TEXT NOT NULL DEFAULT '[]',
			precision TEXT NOT NULL DEFAULT '',
			due_at TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			version INTEGER NOT NULL DEFAULT 1,
			attempts INTEGER NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT '',
			delivered_id TEXT NOT NULL DEFAULT '',
			offer_id TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
CREATE TABLE user_prefs (
		user_id TEXT PRIMARY KEY,
		locale TEXT NOT NULL DEFAULT '',
		timezone TEXT NOT NULL DEFAULT '',
		quiet_start TEXT NOT NULL DEFAULT '',
		quiet_end TEXT NOT NULL DEFAULT '',
		weather_region TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL
	);
CREATE TABLE lab_reviews (
		id TEXT PRIMARY KEY,
		at TEXT NOT NULL,
		channel_key TEXT NOT NULL,
		question TEXT NOT NULL,
		answer TEXT NOT NULL,
		verdict TEXT NOT NULL,
		tags TEXT NOT NULL DEFAULT '',
		note TEXT NOT NULL DEFAULT '',
		meta TEXT NOT NULL DEFAULT '{}'
	);
CREATE TABLE memory_reviews (
	leaf_id TEXT PRIMARY KEY,
	room TEXT NOT NULL,
	kind TEXT NOT NULL,
	note TEXT NOT NULL DEFAULT '',
	excerpt TEXT NOT NULL DEFAULT '',
	at TEXT NOT NULL,
	actor TEXT NOT NULL DEFAULT ''
);
CREATE TABLE eval_runs (
	id TEXT PRIMARY KEY,
	at TEXT NOT NULL,
	params TEXT NOT NULL,
	summary TEXT NOT NULL,
	items TEXT NOT NULL
);
INSERT INTO audit_log(at, action, via, ok, summary) VALUES ('2025-01-02T03:04:05Z', 'config.apply', 'api', 1, 'a synthetic change');
INSERT INTO turn_log(id, at, day, hour, guild_id, channel_id, outcome, body) VALUES ('turn-old', '2025-01-02T03:04:05Z', '2025-01-02', 3, 'g1', 'c1', 'replied', '{}');
INSERT INTO reminders(id, owner_id, content, timezone, status, created_at, updated_at) VALUES ('rem-old', 'u1', 'a synthetic reminder', 'UTC', 'pending', '2025-01-02T03:04:05Z', '2025-01-02T03:04:05Z');
INSERT INTO user_prefs(user_id, updated_at) VALUES ('u1', '2025-01-02T03:04:05Z');
INSERT INTO llm_calls(role, created_at) VALUES ('chat', '2025-01-02T03:04:05Z');
