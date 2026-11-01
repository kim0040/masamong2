package storage

// SQLite and TiDB (MySQL protocol) keep separate DDL. Do not rewrite one dialect
// into the other at runtime. Shared rules: VARCHAR ids, RFC3339 TEXT timestamps,
// ? placeholders, no FTS/vector in this layer.

var sqliteDDL = []string{
	`CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`,
	`CREATE TABLE IF NOT EXISTS config_revisions (
			revision INTEGER PRIMARY KEY,
			status TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			content TEXT NOT NULL,
			base_revision INTEGER,
			created_at TEXT NOT NULL
		)`,
	`CREATE TABLE IF NOT EXISTS jobs (
			id TEXT PRIMARY KEY,
			type TEXT NOT NULL,
			status TEXT NOT NULL,
			idempotency_key TEXT UNIQUE,
			lease_token TEXT,
			lease_expires_at TEXT,
			attempt INTEGER NOT NULL DEFAULT 0,
			checkpoint TEXT,
			payload TEXT,
			result TEXT,
			error_code TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
	`CREATE TABLE IF NOT EXISTS polls (
			id TEXT PRIMARY KEY,
			scope_key TEXT NOT NULL,
			revision INTEGER NOT NULL,
			state TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT ''
		)`,
	`CREATE TABLE IF NOT EXISTS traces (
			id TEXT PRIMARY KEY,
			body TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
	`CREATE TABLE IF NOT EXISTS consents (
			subject TEXT PRIMARY KEY,
			granted INTEGER NOT NULL,
			updated_at TEXT NOT NULL
		)`,
	`CREATE TABLE IF NOT EXISTS channel_ops_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			guild_id TEXT NOT NULL,
			channel_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			summary TEXT NOT NULL,
			revision INTEGER,
			created_at TEXT NOT NULL
		)`,
	`CREATE INDEX IF NOT EXISTS idx_channel_ops_log_scope
			ON channel_ops_log(guild_id, channel_id, id DESC)`,
}

var tidbDDL = []string{
	`CREATE TABLE IF NOT EXISTS schema_migrations (
			version INT NOT NULL PRIMARY KEY,
			applied_at VARCHAR(64) NOT NULL
		)`,
	`CREATE TABLE IF NOT EXISTS config_revisions (
			revision INT NOT NULL PRIMARY KEY,
			status VARCHAR(32) NOT NULL,
			content_hash VARCHAR(64) NOT NULL,
			content MEDIUMTEXT NOT NULL,
			base_revision INT,
			created_at VARCHAR(64) NOT NULL
		)`,
	`CREATE TABLE IF NOT EXISTS jobs (
			id VARCHAR(64) NOT NULL PRIMARY KEY,
			type VARCHAR(64) NOT NULL,
			status VARCHAR(32) NOT NULL,
			idempotency_key VARCHAR(128) UNIQUE,
			lease_token VARCHAR(128),
			lease_expires_at VARCHAR(64),
			attempt INT NOT NULL DEFAULT 0,
			checkpoint MEDIUMTEXT,
			payload MEDIUMTEXT,
			result MEDIUMTEXT,
			error_code VARCHAR(64),
			created_at VARCHAR(64) NOT NULL,
			updated_at VARCHAR(64) NOT NULL
		)`,
	`CREATE TABLE IF NOT EXISTS polls (
			id VARCHAR(64) NOT NULL PRIMARY KEY,
			scope_key VARCHAR(128) NOT NULL,
			revision INT NOT NULL,
			state MEDIUMTEXT NOT NULL,
			created_at VARCHAR(64) NOT NULL DEFAULT ''
		)`,
	`CREATE TABLE IF NOT EXISTS traces (
			id VARCHAR(64) NOT NULL PRIMARY KEY,
			body MEDIUMTEXT NOT NULL,
			created_at VARCHAR(64) NOT NULL
		)`,
	`CREATE TABLE IF NOT EXISTS consents (
			subject VARCHAR(128) NOT NULL PRIMARY KEY,
			granted TINYINT NOT NULL,
			updated_at VARCHAR(64) NOT NULL
		)`,
	`CREATE TABLE IF NOT EXISTS channel_ops_log (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			guild_id VARCHAR(64) NOT NULL,
			channel_id VARCHAR(64) NOT NULL,
			kind VARCHAR(32) NOT NULL,
			summary VARCHAR(512) NOT NULL,
			revision INT,
			created_at VARCHAR(64) NOT NULL,
			KEY idx_channel_ops_log_scope (guild_id, channel_id, id)
		)`,
}
