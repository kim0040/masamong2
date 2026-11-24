[한국어](data-model.md) | **English**

# The data model: SQLite tables and their lifetimes

> **In one line**: everything is in one file, `masamong.db`. The raw tables are the floor; the index, room memory, person memory and turn records point at raw IDs, and deletion and retention clean up along those references.

**In three minutes**

- **One file**: configuration revisions, raw messages, the index, room memory, person memory, turn records, usage and personal features share one SQLite file. The memory tables are not a separate file.
- **Writes go in one line**: there is one writing connection and a separate read pool (WAL). Long writes are split into small batches.
- **Tables are declared in three places**: the core schema (`internal/storage`), the memory tables (`internal/memory/store`) and the feature tables (the feature files of `internal/application`).
- **Raw and derived are kept apart**: `memory_observations` is raw; the other memory tables are derived and can be rebuilt. Derived rows carry evidence IDs.
- **Retention** follows the `retention.*` settings: records with text are kept short, summary rows without text are kept long.
- **Upgrades only add.** Golden files pin the table list and the upgrade from an older shape.

## How the tables relate

<picture><source media="(prefers-color-scheme: dark)" srcset="diagrams/data-model.en-dark.svg"><img alt="The data model at a glance. From the raw tables (memory_rooms, memory_observations) the search index (memory_leaves and others), room memory (channel_episode, channel_state) and person memory (person_claim and others) follow by evidence IDs. The deletion and evidence tables (evidence, privacy_tombstones, redaction_jobs, consent) are told which raw messages room memory and the turn records read. The conversation and record tables (chat_turns, turn_log, llm_calls) and the configuration, operations and personal feature tables stand apart." src="diagrams/data-model.en-light.svg"></picture>

Diagram source: [diagrams/data-model.en.d2](diagrams/data-model.en.d2)

Only the tables each area turns on are drawn; the folded sections below list them all. The lines are logical references. Most are not foreign key constraints but ID columns and the evidence index, and the cleaning up is done by code (the redaction worker, the retention pass).

## In detail

<details>
<summary><b>Where tables are declared, and the opening order</b></summary>

| Group | Declared in | Tables |
|---|---|---|
| Core schema | [`internal/storage/schema.go`](../../internal/storage/schema.go) | `schema_migrations`, `config_revisions`, `jobs`, `polls`, `traces`, `consents`, `channel_ops_log` |
| Memory tables | `store.go`, `episodes.go`, `person_claims.go`, `person_alias.go`, `people_backfill.go`, `privacy.go`, `evidence_refs.go`, `batch_plans.go`, `message_vectors.go` in [`internal/memory/store`](../../internal/memory/store) | see "Memory" below |
| Feature tables | The feature files of [`internal/application`](../../internal/application) (for example `threads.go`, `reminders.go`, `fortune.go`, `notices.go`, `participation.go`, `banter.go`, `cards.go`, `privacy.go`) | the remaining sections |

- The order: `storage.OpenOptions` opens the file and brings the core schema up to date → `store.Open` does the same for the memory tables on the same handle → `migrateFeatures` applies the DDL list the feature files collected (`featureDDL`) in one go.
- **One writing connection, a separate read pool** ([`internal/storage/store.go`](../../internal/storage/store.go) `WithWriterContext`): one write at a time; reads use a pool of read-only connections. The journal is WAL.
- A block of TiDB DDL is still in the code, unused because `TiDBEnabled = false`. New feature tables have SQLite DDL only ([STORAGE.md](../STORAGE.md), Korean).

</details>

<details>
<summary><b>Memory</b>: raw messages, the index, room memory, person memory</summary>

| Table | Contents | Written by | Raw? |
|---|---|---|---|
| `memory_rooms` | The rooms (platform, collection, label, the zone for times without one) | Imports, capture | Raw metadata |
| `memory_observations` | Raw observations: room, original message ID, author label and account, text, time, kind | `Engine.Capture`, imports, the catch-up after downtime | **Raw** |
| `memory_observation_aliases` | Other IDs of the same observation (a KakaoTalk file imported again, imports across formats) | Imports | Raw metadata |
| `message_kept_revisions`, `memory_revision_sequence` | The order of edits (so a late, older edit can be refused) | `Engine.Edit` | Raw metadata |
| `memory_leaves` | Leaves: room, recipe, the observation IDs inside, text, summary, embedding space, leaf vector, summary vector | Indexing (`Engine.IndexRooms`) | Derived |
| `memory_leaf_sync` | The per-room checkpoint of leaf cutting | Indexing | Derived |
| `memory_message_vectors` | Per-message vectors | Indexing | Derived |
| `memory_embed_cache` | Embeddings found by content hash (the query cache included) | Indexing, query embedding | Cache |
| `memory_batch_plans` | The plan and state of a block preparation batch | `Engine.PrepareBatch` | Job state |
| `memory_spend` | The memory-side cost ledger, by kind: indexing, episodes, room state, room style, backfill and so on | Indexing, room memory, the backfill | Ledger |
| `channel_episode`, `episode_item`, `episode_leaf` | L2 episodes, their items (evidence, speakers, state), and the links to leaves | `RunRoomMemory` | Derived |
| `channel_state` | L1 room state (one JSON row per channel, with evidence) | `RunRoomMemory` | Derived |
| `memory_explicit` | The index of facts kept by "remember this" (person and topic → episode item) | Explicit memory | Derived |
| `person_claim` | One line of person memory: subject, speaker, kind, topic, statement, evidence, room, source, state | `savePersonClaims` | Derived |
| `person_memory_suppress` | Hide and forget markers (references and a time only) | Hide, `/forgetme` | Marker |
| `person_alias`, `person_alias_source`, `person_alias_fill` | The per-server name index, its evidence, and the progress of filling it | Indexing after capture | Derived |
| `person_relation` | Reply and mention counts | Relation upkeep | Derived |
| `person_card_item` | The old stored person cards. No longer read or written | (nobody) | Left behind |
| `people_backfill_run`, `people_backfill_window` | The state of backfill runs and windows, the provider's batch numbers | The backfill | Job state |
| `memory_reviews` | The operator's review of leaves (excluded, corrected) | The console's memory review | Operator input |

"Derived" means it can be rebuilt from the raw messages. Summaries, embeddings and extraction cost money, though, so no release rebuilds them on its own (see "no reprocessing" in [memory.en.md](memory.en.md)).

</details>

<details>
<summary><b>Deletion and evidence</b>: the tables that follow a deletion to the end</summary>

| Table | Contents |
|---|---|
| `evidence_sources`, `evidence_holders`, `evidence_links` | The evidence index: which record (holder) read which raw message (source). The view `evidence_refs` joins the three |
| `privacy_tombstones` | Tombstones. A deleted message cannot come back through a re-import or a late write |
| `privacy_settled` | The sequence of tombstones whose derived records are already cleaned up. A turn remembers the sequence at its start and checks before it stores whether it read anything deleted since |
| `redaction_jobs` | The durable jobs that erase derived records. Interrupted work carries on at the next start |
| `thread_tombstones` | Deleted threads |
| `consents`, `consent_state`, `consent_audit` | The state of each consent scope and its change history |
| `memory_privacy_migration`, `memory_identity_migration`, `memory_message_vector_migration` | Progress marks of one-time clean-up and migration work |

Holders in the evidence index include conversation context rows, the evidence of a turn record, traces, episode items and room state. The propagation rules are under "How far a delete, an edit or a withdrawal reaches" in [memory.en.md](memory.en.md).

</details>

<details>
<summary><b>Conversation and records</b>: what a turn leaves behind</summary>

| Table | Contents | Holds text? | Retention key |
|---|---|---|---|
| `chat_turns` | Conversation context: the recent lines of each channel (bot and people) | Yes | `context_days` |
| `turn_log` | The turn record: outcome, tools, evidence count, cost, latency, the detail (body) | In the detail, subject to the room's `logging.level` | The row `turn_days`, the detail `turn_detail_days` |
| `llm_calls` | The model call ledger: role, model, tokens, time, cost, failure class | No | `ledger_days` |
| `tool_calls` | The tool call ledger (argument names only) | No | `ledger_days` |
| `delivery_ledger` | What was sent to Discord | No | `ledger_days` |
| `reply_messages` | The link from a reply's message IDs to its turn | No | `turn_detail_days` |
| `turn_feedback` | Reactions to replies and the operator's marks | No | `turn_days` |
| `conversation_log` | Decisions about name calls, follow-ups, threads and banter | No | `turn_days` |
| `participation_log`, `participation_spoke_turns` | Chime-in candidates and decisions, the turns where the bot spoke | Some wording | `turn_detail_days` |
| `banter_turns`, `banter_channel_state` | Banter turns and the channel's state | No | (feature state) |
| `channel_threads`, `thread_parents`, `thread_participants` | Bot threads, their parents and participants | No | `turn_days` after closing |
| `traces` | Diagnostic traces | No | `trace_days` |
| `lab_reviews`, `eval_runs` | Answer lab reviews, evaluation runs | Yes | Managed by the operator |

</details>

<details>
<summary><b>Configuration, operations, usage</b></summary>

| Table | Contents |
|---|---|
| `config_revisions` | The revision history of the configuration document. A change is "replace only if the revision is still the one that was read" |
| `config_snapshots` | Named configuration snapshots |
| `admin_state` | Operating state as keys and values (schedule marks, cursors, cached decisions) |
| `audit_log` | The change log of every state-changing Action (path, outcome, configuration version; only safe fields of the values) |
| `channel_ops_log` | The history of channel setting changes |
| `jobs` | The state machine of long jobs (imports, preparation, reindexing) |
| `tool_usage` | Tool usage counters (per person, per channel, total; by day) |
| `stats_daily`, `stats_days` | Usage statistics: daily totals only, no row per person |
| `activity_counts` | Message counts for the activity ranking |
| `user_prefs` | Per-person preferences (language, region and so on) |
| `update_news_sent` | Where update news was sent, so a restart never sends it twice |
| `schema_migrations` | The schema migration record |

</details>

<details>
<summary><b>Personal features and the rest</b></summary>

| Table | Feature |
|---|---|
| `reminders`, `reminder_offers`, `reminder_context` | Reminders and scheduled tasks, offers, the context of the moment |
| `polls`, `poll_messages` | Polls |
| `fortune_profiles`, `fortune_cache`, `fortune_usage` | Fortune registration, cache, usage |
| `notice_subs`, `notice_items`, `notice_state`, `notice_delivered`, `notice_delivery_outcomes` | School and transfer notice subscriptions and deliveries |
| `channel_summary` | The channel summary, used by the `/summary` command only |
| `channel_style` | Room style notes and their approval state |

</details>

<details>
<summary><b>Retention</b>: what is erased when</summary>

Code: [`internal/application/retention.go`](../../internal/application/retention.go). The list of rules is pinned in the `pruneRules` section of `internal/application/testdata/schema.golden`.

| Key (`retention.`) | Default | What it erases |
|---|---|---|
| `turn_days` | 400 | Summary rows of turn records, decision records, feedback, activity counts, closed threads |
| `turn_detail_days` | 30 | The detail of turn records (message, reply, evidence), reply message links, the wording in participation records |
| `context_days` | (`turn_detail_days`) | The conversation context of rooms that went quiet or that the bot left, finished reminders |
| `pending_days` | (`usage_days`) | Reminder drafts and offers nobody confirmed |
| `trace_days` | 30 | Diagnostic traces |
| `audit_days` | 400 | The change log, channel setting history |
| `usage_days` | 14 | Per-person and per-channel tool usage (daily totals per tool follow `turn_days`) |
| `ledger_days` | 90 | The model call, tool call and delivery ledgers |
| `query_cache_days` | 7 | The query embedding cache |

- A record key set to `-1` is never erased by age. Then set `pending_days` and `context_days` too.
- A room can erase its own text sooner with `logging.retain_days`.
- **Raw messages and memory are never erased by retention.** Only a deletion, a withdrawal or deleting the block removes them.
- With statistics on, the retention pass first finishes the statistics of past days and erases the rows they read only after that day is done.

The policy is under "retention" in the [raw data policy](../RAW_DATA_POLICY.md) (Korean); the keys are explained in the wiki's [configuration reference](../wiki/Configuration-Reference.md).

</details>

<details>
<summary><b>Backup, restore, reset</b></summary>

Code: [`internal/application/backup.go`](../../internal/application/backup.go), [`reset.go`](../../internal/application/reset.go), the shared journal and recovery engine [`maintenance.go`](../../internal/application/maintenance.go).

- **A backup** holds a database snapshot of one moment and the configuration most recently committed inside that snapshot. It is completed and verified in a hidden folder (checksum, SQLite integrity, product schema, configuration parsing) and then published in one step.
- **Automatic backups** are made by the scheduler (`backup.keep`, 4 by default). One is also made at the first start of a new version and just before an in-app update or a reset.
- **Restore, reset and program replacement** use the same journal format. Each step is recorded, so after a crash the next start carries on or rolls back. This finishes before the database opens; if it cannot finish, the bot does not start.
- **A reset** erases the database and imported files and caches, and keeps `backups/`, `logs/` and the console sign-in. Keys are erased only when that option is chosen.
- A deleted message is still in backups made before the deletion. After restoring a backup, deletion requests are processed again.

The procedures are in [STORAGE.md](../STORAGE.md) (Korean) and the wiki's [storage and backup](../wiki/Storage-and-Backup.md).

</details>

<details>
<summary><b>Upgrades</b>: the rules for changing the schema</summary>

- **Only add.** New tables, new columns, new indexes. Avoid a migration that rewrites existing rows.
- **Going back to the previous version must be safe.** Avoid a migration that holds up the start or uses much disk on a small machine.
- **When a default changes, write down what happens on an existing install.** Changing the embedding width or the recipe means embedding again, so the cost and time go into the release notes.
- **Heavy filling runs in the background**, in ranges, and resumes after an interruption (the name index fill, for example).
- The pinning tests: `go test ./internal/application -run 'TestExportedSurface|TestSchema'` compares the table list order, `sqlite_master` and the upgrade from an older shape with golden files. For an intended change, read the diff and rewrite with `-update`.
- A test also checks that the SQLite and TiDB versions of the base schema define the same tables and columns, so the switched-off one does not drift.
- What to watch when going back across particular versions is in the [maintainer guide §4](../MAINTAINING.md) (Korean).

</details>

---

As of v1.7.1 (2026-11-24). Related decisions: [B-2 SQLite only](decisions.en.md#b-2), [D-3 deletion reach](decisions.en.md#d-3), [G-7 one journal](decisions.en.md#g-7), [G-10 statistics stay local](decisions.en.md#g-10).
