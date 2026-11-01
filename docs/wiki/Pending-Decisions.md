# Pending decisions

[한국어](https://github.com/kim0040/masamong2/wiki/보류-중인-결정) | **English**

Items found in the September 2026 stability and capacity review that were **not built because the operator has to decide**. Each trades one feature or kind of data for another, so the defaults were left as they are. Once decided, this page is updated and the item built.

What was fixed right away in that review is in [Load testing](https://github.com/kim0040/masamong2/wiki/Load-Testing-and-Long-Running) and [Storage and backup](https://github.com/kim0040/masamong2/wiki/Storage-and-Backup).

| # | Item | Gain | Cost | Recommendation |
|---|---|---|---|---|
| 1 | Store vectors on disk as int8 too | About −37% memory disk | Original precision lost for good | Only when disk is tight |
| 2 | One step lower SQLite sync | Faster writes on slow disks | The last few seconds on power loss | Only on SD cards and HDDs |
| 3 | ~~Automatic restart on Windows and Linux desktops~~ | — | — | **Built** (2026-09-30, a supervisor on every OS) |
| 4 | ~~A fallback store for TiDB outages~~ | — | — | **Not applicable** (TiDB disabled 2026-09-28) |
| 5 | Backup count and compression | Less backup disk | Fewer points to restore to | Adjust the count first |
| 6 | ~~Incremental snippet splitting for very large single rooms~~ | — | — | **Done** (2026-10-12, only the tail after a checkpoint is re-split) |
| 7 | ~~Filling in Discord messages missed while off~~ | — | — | **Built** (2026-09-28, automatic, last 7 days) |

---

## 1. Storing vectors on disk as int8

**Now**: snippet and summary vectors are stored on disk as float16 (8 KB per vector at 4096 dimensions, two per snippet). The in-memory search index already holds them as int8 (4 KB per vector).

**If changed**: vectors on disk become int8 too (one scale per row).

- Memory disk drops from about 27 MB to about 17 MB per 10,000 messages, and backups by the same ratio.
- Search already runs on int8, so results are practically the same (score difference about 0.001, 97.8% identical top 10 on synthetic data).
- Loading the index gets a little faster (no float16 → int8 conversion).

**Costs**:

- The float16 originals are gone. It cannot be undone without re-embedding everything (paid).
- Lowering `models.embed.dimensions` later (Matryoshka truncation) would cut from int8, which may lose a little more quality.
- Existing databases need a one-time conversion (background job, no cost).

## 2. SQLite sync one step lower (`synchronous=NORMAL`)

**Now**: SQLite's default (FULL) syncs the WAL to disk on every commit — one fsync per stored message.

**If changed**: NORMAL, the common choice with WAL.

- Much lower write latency on disks with slow fsync, like SD cards (Raspberry Pi) and HDDs. Little difference on SSDs.
- The database does not get corrupted (NORMAL is corruption-safe in WAL mode).

**Cost**: on power loss or a forced OS shutdown, **the last few commits (usually under a second)** can be lost. A process crash alone loses nothing.

**To decide**: a setting (`storage.sqlite_sync: full | normal`), or a new default.

## 3. Automatic restart on Windows and Linux desktops

> **Built (2026-09-30)**, as option (a):
> - The icon, the menu bar, `masamong open` and `masamong start` run the service as `masamongd --supervise`. The supervisor runs the real service as a child and restarts it after a non-zero exit (runtime fatal errors, out of memory, being killed) after 1, 2, 4 … up to 60 seconds. After 10 healthy minutes the interval resets.
> - After 5 abnormal exits within 10 minutes it stops (the same cause keeps failing). The reason goes into `logs/crash.json` and reaches the uptime history and operator alerts on the next start.
> - A normal stop (`masamong stop`, the console or icon) ends the supervisor too. A restart is relaunched by the supervisor on the same address.
> - macOS is the same. Before, launchd restarted only the menu bar app, not the service. The Linux server systemd unit runs the service directly (`Restart=on-failure`) and is unchanged.
>
> Below is the review from before the decision.

**At the time**:

| Autostart | After a crash |
|---|---|
| Linux server (systemd) | Restarts after 5 seconds |
| macOS (launchd) | Restarts (added in this review, except when you turned it off) |
| Windows (registry run key) | **Stays off** |
| Linux desktop (XDG autostart) | **Stays off** |

Panics are now caught and logged, so this is rare, but exits that cannot be caught — out of memory, Go runtime fatal errors — remain. The tray app only showed "off" and did not restart it.

**Options**:

- (a) A small supervisor in front of the daemon that restarts it after a non-zero exit with growing intervals. The same on every OS.
- (b) The tray app restarts it when it notices a crash. Does not help when running without the tray.
- (c) On Windows, register with the Task Scheduler's "restart on failure". Different per OS.

**Recommended**: (a).

## 4. A fallback store for TiDB outages

> **Not applicable (2026-09-28)**: the operator switched the TiDB backend off ([Storage and backup](https://github.com/kim0040/masamong2/wiki/Storage-and-Backup)). This cannot happen while only SQLite is used. Revisit this if TiDB is ever turned back on.

**At the time**: while TiDB was unreachable, messages from Discord failed to save and were only logged; that conversation never reached memory. Replies failed too (silently). SQLite is not affected.

**Option**: collect messages that failed to save in a local file (or a small SQLite) in the data folder, and move them over in order when the connection returns. Imports skip duplicates, so moving something twice is safe.

**To consider**:

- A size cap for the fallback store (long outages).
- Ordering with new messages arriving during the move — small, since messages are sorted by their time, not by when they were saved.
- Showing "N messages waiting" in the console.

**Recommended**: build it together with any move to TiDB.

## 5. Backup count and compression

**Now**: automatic backups run weekly and keep 4 (`backup.keep: 4`), so the disk needs about 5× the database (the database plus 4 backups). When space runs short the backup is skipped with a reason (added in this review).

**Options**:

- Keeping 2 makes it about 3×. Only a setting change.
- Compressing backups (gzip/zstd). Text compresses well but vectors, most of the size, do not; expect **only 10–20%**, at the cost of slower backup and restore.
- Item 1 (int8 storage) would shrink backups by about 37% too.

**Recommended**: adjust `backup.keep` first if needed. Compression gains little and is low priority.

## 6. Incremental snippet splitting for very large single rooms

> **Done (2026-10-12)**: the low-spec work added a checkpoint-based incremental path. The details are in section 16, "leaf sync for large rooms", of the [memory operations document](https://github.com/kim0040/masamong2/blob/main/docs/memory-operations.md) (Korean).

**Now**: the last completed split is saved as a checkpoint, and new messages that arrive in order after it re-split **only the tail**. The whole room is split again only for the first sync, messages with old times that arrive late (imports, catching up), edits and deletions, a change of split rules or time zone, and an interrupted or uncertain checkpoint. Tests check that the result matches a full re-split.

## 7. Filling in Discord messages missed while off

> **Built (2026-09-28)**: the operator chose "automatic, last 7 days".
> - Once connected, after 5 seconds, each room with collection (`capture`) on is read from the last stored message onward. Rooms that never stored anything after collection was turned on are skipped (`never_captured`), so nothing from before collection began is pulled in.
> - The period is `memory.backfill_days` (0 = 7 days, -1 = off, at most 30); each room reads at most `memory.backfill_max_messages` (0 = 4000) at once.
> - The bot does not reply to filled-in messages, and messages from people who withdrew memory consent are not stored (the same rules as live collection). Edits and deletions reach memory through live events.
> - Console: the room's **Memory → Catch up missed messages** shows the last result and fills now. CLI: `memory backfill`.
>
> Below is the review from before the decision.

**At the time**: only messages received live were stored. Messages posted while the service was off, or while the Discord connection was being re-established, were never fetched and were lost from memory for good. There was no code to fetch past messages.

**Option**: once connected (ready), read each collecting room from the last stored message ID through the Discord API. Imports skip duplicates by message ID, so overlapping reads are safe. These are late messages with past times, so the room's snippets are re-split (related to item 6).

**To consider**:

- Collection scope: follow the same room policy as live collection (`capture`), and cap the period (e.g. 7 days). It must match the Discord raw allowlist policy.
- Discord rate limits: many rooms or a long gap need several reads.
- No replies to filled-in messages (collect only).

**Recommended**: build it once the scope and period cap are decided.
