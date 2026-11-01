# Storage and backup

[한국어](https://github.com/kim0040/masamong2/wiki/저장소와-백업) | **English**

## Database: SQLite

Masamong 2 keeps settings, conversation memory, per-turn records, usage statistics and personal features (consents, fortunes, notice subscriptions) in one file, `masamong.db`, in the data folder. There is no separate database server to install or maintain, and it is backed up without stopping (`VACUUM INTO`).

For a personal bot on one machine (a PC, home lab or small server), SQLite is plenty fast and reliable. It was load-tested with 100,000 stored messages and a steady 6 messages a second without falling behind.

- **Separate read and write connections**: writes go through one dedicated connection in order, as SQLite requires. Heavy reads such as loading the search index or making a backup use a **separate read-only pool**, so they never hold up saving live messages (p95 about 20 ms at 100,000 messages).
- **WAL mode**: reads and writes do not block each other. The WAL file is checkpointed when it passes 64 MB.
- **Crashes and power loss**: `config.yaml` is replaced atomically (written to a temporary file, `fsync`, rename). The database relies on SQLite's default sync, so every commit is on disk.
- **Keep it on a local disk**: SQLite is built for local I/O. A database on a network file system (NFS, SMB/CIFS) can deadlock on file locks or get corrupted; use a local SSD or HDD.

## TiDB switched off; one storage engine (2026-09-28)

There used to be an optional TiDB (MySQL-compatible) backend for large distributed setups. To cut the engineering cost of maintaining and testing two engines and focus on stability, the operator decided to run **SQLite only**.

- The code is kept but disabled by the `TiDBEnabled = false` guard in `internal/storage/settings.go`. Every entry point refuses TiDB: `storage.yaml`, the `MASAMONG_DB_BACKEND` environment variable and the `--db-backend` flag.
- The old live storage migration tool (`storage migrate`) is disabled too.
- **Moving an existing TiDB instance**:
  1. Make a backup with the previous version (TiDB backups are logical JSON dumps).
  2. Remove `backend: tidb` from `storage.yaml`.
  3. Start the current version and restore that backup under **Maintenance → Backup**.

The reasoning and what to check before ever turning it back on are in the [storage design document](https://github.com/kim0040/masamong2/blob/main/docs/STORAGE.md) (Korean).

## Personal settings from the previous bot (V1)

Under **Maintenance → Import personal settings from the previous bot**, point to the V1 bot's SQLite file, review with **Preview** what will be imported and what is skipped and why, then import (`masamong migration apply --path V1.db [--confirm true]`).

- Only consent history, fortune birth data and university/transfer notice subscriptions are imported — no conversation text.
- Only people who agreed to the current privacy policy version are imported, and data already in this instance is never overwritten.
- The old database is opened read-only, so it cannot be damaged.

## Backup and restore

### Scheduled backups

```yaml
backup:
  schedule: weekly   # off | daily | weekly
  hour: 4            # hour of day (24-hour, in i18n.timezone)
  weekday: 0         # 0 = Sunday (weekly schedules)
  keep: 4            # how many automatic backups to keep (manual and safety backups are never pruned)
```

### Making and restoring backups

From the console's **Maintenance** page or the CLI:

```bash
# 1. Make a backup now
masamong backup create

# 2. List backups
masamong backup list

# 3. Verify a backup
masamong backup verify --name <backup>

# 4. Restore (the name is repeated to confirm)
masamong backup restore --name <backup> --confirm <backup>

# 5. Delete a backup
masamong backup delete --name <backup>
```

- Backups are stored under `backups/` in the data folder and contain the SQLite database and `config.yaml` (owner-only: `0600` on macOS and Linux, an owner-only ACL on Windows).
- **A backup is one consistent moment.** The database snapshot is taken first, and the settings file and its revision number come from the latest committed settings *inside that snapshot*, so a settings change made while the backup runs cannot leave a database and a `config.yaml` from different moments. Several backups started in the same second each get their own folder; a failed backup removes only its own half-written folder and never touches one that succeeded. Until a backup is complete and verified it sits in a hidden `.backup-staging-*` folder and is not listed. Abandoned staging folders and restore `.old-*`/`.new-*` files are cleaned at startup and hourly when no operation owns them; `doctor` lists anything still left.
- **Verifying** (`backup verify`, and automatically before a restore) checks the file checksums, runs SQLite's integrity check, checks the database against the product's schema, and checks that the settings parse and match the revision in the manifest. A backup that merely has matching checksums but is not a valid database is refused. Old backups made by the TiDB-era version (logical dumps) are checked in an isolated temporary database first.
- **Keys**: raw API keys and the bot token are never in a backup, only their references. After restoring on another computer, enter the keys again in the console.
- **Safety backup**: right before a restore, a snapshot of the current state is made automatically; the restored data is applied when the daemon restarts. If something is wrong, roll back to the safety backup at once.
- **A restore does not lose what was in flight.** The restore is checked completely (checksums, integrity, schema, settings) *before* anything live is touched. Then the current database, its write-ahead log (`-wal`, with commits that were not yet folded into the main file) and its settings are moved aside under names for this operation, the new ones are installed and checked, and only then are the old ones deleted. Each step is written to a journal, so a crash, a power cut or a file Windows will not release at the wrong moment is picked up from where it stopped on the next start, or rolled back to the old files. If the old state cannot be restored either, the service does not open the database and serves a localized recovery page ([Troubleshooting](https://github.com/kim0040/masamong2/wiki/Troubleshooting#the-console-shows-recovery-required-maintenance-mode)).
- **Restore failures and cleanup:** if preparation fails before any live file is moved, Masamong removes staged copies, records the failure and keeps running on the current data. A restore that has committed is still successful if another program holds an old file open; cleanup is retried at startup and hourly, and `doctor` lists any leftovers. `restore-result.json` records a warning for this case.
- **Automatic backups around updates**: right before the console installs a new version (kind *Before update*) and on the first start of a new version however it was installed (kind *New version*). The newest three of each kind are kept. The database only moves forward, so these are what you restore after going back to an older version.
- Backups contain conversation text. Do not share them or upload them to public storage.

### Moving to a new server or computer

1. On the old machine: `masamong backup create`
2. Copy the `backups/<backup>` folder into `backups/` in the new machine's data folder.
3. Install Masamong on the new machine, run `masamong setup`, then `masamong backup restore --name <backup> --confirm <backup>`.
4. Enter the model API keys and the Discord token again under **Connections**.

## Reset

**Maintenance → Reset** starts the instance over, as if it had just been installed. A warning comes first, then you type the confirmation word.

- **Erased**: memory and imported chats, conversation records, people, consents and personal settings, server and room settings, all settings and their history, practice data. This covers the database that is **actually in use**, including a custom file chosen with `sqlite_file` in `storage.yaml` (and `storage.yaml` itself), and the files the instance owns: uploaded imports (`imports/`), prepared-memory staging (`rag_staging/`), the prepared catalog (`memory/`), channel attachments (`rag/`), generated pictures (`images/`), notice data (`notices/`) and any restore marker or result. Files the instance only *points to*, such as a chat export in your Documents folder, are never touched.
- **Kept**: `backups/`, `logs/`, `admin/` and `sessions.json` (the console sign-in), plus daemon and lock metadata needed by the running instance. Saved keys in `secrets/` and Windows Credential Manager are kept unless you tick **Also delete saved API keys and the bot token**. **Back up first** is on by default (kind *Before reset*). To undo, finish the first-run setup, then restore that backup under Maintenance.
- **Keys**: kept unless you tick **Also delete saved API keys and the bot token**. That deletes only the keys saved from the console (key files in `secrets/`, and this instance's Keychain or Credential Manager entries). Keys from environment variables or other files are never touched.
- Like a restore, it runs on the restart: the service clears the files before opening the database and the console opens the first-run setup.
- **If it is interrupted** (a crash, or a file another program holds open, which Windows will not delete): the exact list of what is to be erased and how far it got is kept in a journal. The bot stays off and a localized recovery page lists the valid commands. `masamong status` shows the daemon at the recovery address (`ready:false` in JSON), plus `maintenance:true` and a one-line `message`; `masamong runtime status` includes those maintenance fields too. Run `masamong stop`, then the page's `masamongd --maintenance retry` command and `masamong start` when recovery is complete ([CLI commands](https://github.com/kim0040/masamong2/wiki/CLI-Commands#recovery-commands-masamongd)).
- **Once erasing has started it cannot be undone or cancelled**, only finished. The backup taken first is the way back. A reset marker that is damaged or from an older version is never taken as permission to delete anything; it is set aside for you to look at.
- The data folder itself may sit behind a link (for example macOS's `/var` → `/private/var`, or a moved home folder); only folders *inside* it must be real folders.

## Config snapshots

Save just the `config.yaml` configuration under a name, without touching the database, and go back to it any time:

```bash
# Save the current settings
masamong config snapshot save --name before-experiment --note "before the big prompt experiment"

# List snapshots and their IDs
masamong config snapshots

# Restore one
masamong config snapshot restore --id <snapshot ID>
```
