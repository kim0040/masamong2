# Load testing and long-running operation

[한국어](https://github.com/kim0040/masamong2/wiki/부하-테스트와-장기-운영) | **English**

Masamong is a service you leave running for months. This page explains **how** we check it can, **what** the checks found, and **what to prepare** at each size. The Korean page has every historical table in full; this page keeps the latest results and the sizing.

## The load test tool (`tools/loadtest`)

It runs the real service code in one process and puts a fake OpenAI-compatible server (configurable latency and failure rate) where the model providers would be. Nothing leaves the machine (`MASAMONG_OFFLINE=1`), so it costs nothing.

Running at the same time: Discord messages (Poisson arrivals) → memory collection → replies to some; slash and `!` commands, polls and poll buttons; console views (dashboard, conversations, stats, logs); room setting changes; background indexing every minute and scheduled jobs; with `-features`, reminders, chiming in, consents and personal settings.

Measured: latency p50/p95/p99/max and outcome per operation; model calls, peak concurrency and new connections; goroutines, heap, open files, database/WAL/log size and rows per table over time; **growth per 1,000 replies**; and after the load, whether work drains and goroutines and files return to their starting level, plus a database `integrity_check`.

With `-strict` it fails (exit 1) when more than 10 goroutines or open files remain above the starting level, more than 1% of replies error or time out, the database integrity check fails, or work does not drain.

```bash
go run ./tools/loadtest -duration 2m                                  # quick check
go run ./tools/loadtest -duration 30m -rate 6 -dims 4096 -strict      # long soak
go run ./tools/loadtest -duration 5m -rate 30                         # find the throughput limit
go run ./tools/loadtest -duration 5m -seed 100000 -dims 4096          # an instance with a long history
go run ./tools/loadtest -duration 10m -report soak.md                 # write a report (+ soak.json)
```

| Option | Default | Meaning |
|---|---|---|
| `-duration` | `2m` | Load time |
| `-rate` | `10` | Discord messages per second |
| `-reply` | `0.6` | Share of messages answered |
| `-channels`, `-users` | `12`, `60` | Rooms and users |
| `-concurrency` | `8` | `limits.max_concurrent_turns` |
| `-llm-latency`, `-llm-jitter` | `400ms`, `400ms` | Model call latency |
| `-llm-error-rate` | `0.01` | Model call failure rate (502) |
| `-dims` | `256` | Embedding dimensions (production default 1024 since 2026-10-13; 4096 before) |
| `-seed` | `0` | Past messages stored and indexed before the load |
| `-config-every` | `30s` | Setting change interval |
| `-strict` | off | Exit 1 when the long-running checks fail |
| `-features` | on | Personal features too (reminders sent through a fake Discord, chiming in, consents, settings); checks no reminder is sent twice or stuck |
| `-feature-rate` | `1` | Personal feature uses per second |

### In CI

- **Full CI** (weekly, manual, release): a 45-second load test with `-strict`
- **`soak` workflow** (monthly and manual): a 45-minute soak whose duration, rate and dimensions can be changed in Actions; the report is kept as the job summary and an artifact

## Earlier results (2026-10-01, after disaster alerts, places and command-to-chat were added)

Environment: 8-core macOS (Apple Silicon), Go 1.27.1, SQLite. Fake model latency 0.4–0.8 s, 1% failures. 12 rooms, 60 users, personal features on.

| Run | Result |
|---|---|
| **Idle** (fresh install, no Discord, 1 minute) | Service RSS 27 MB + supervisor 13 MB ≈ **40 MB**, CPU about 0%. Programs: `masamongd` 34 MB, `masamong` 26 MB |
| **15-minute soak**, 6 messages/s, 1024 dimensions, `-strict` | **PASS**. 3,258 replies (3.6/s, 56 refused by full room queues), reply p50 4.1 s and p99 12.4 s (fake model latency and queueing included). Message store p99 15.8 ms, poll button p99 5.3 ms, console dashboard p99 40 ms. Goroutines 11 at start → 10 at the end (no leak), heap 19.7 MB after the load, integrity ok. 12,797 model calls over 21 reused connections |
| 100,000 messages + load | **Not finished this time.** With the fake model's 0.4 s latency, indexing 100,000 messages alone would have taken over 30 minutes, so it was stopped; a faster rerun was cut short. The September measurements below are the latest |

On macOS the tool cannot read open files and RSS (-1), so only goroutines and heap were judged; on Linux (the CI `soak` workflow) both are measured.

**Assessment**: 40 MB idle and a heap around 20 MB under load with a small memory fit the reason it was written in Go — small resource use. Memory grows with **the number of stored messages** (the search index lives in memory); everything else (goroutines, connections, caches) returns to its starting level after load. The two long-running leaks found in September (leaked index database connections, and rebuilding the index on every setting change) have not come back. The +0.65 MB/min heap trend in the second half of the soak matches the messages being stored (about 3.5 KB each), so it is not a leak.

### September highlights (2026-09-25 … 09-28)

| Run | Result |
|---|---|
| 20-minute soak, 6 messages/s, 4096 dimensions | PASS after fixes: goroutines back to 8 (were 43 from a leaked index database before the fix), connection reuse 16,072 calls over 26 connections |
| Throughput limit, 40 messages/s for 3 minutes | 4.3 replies/s at concurrency 8, 6.3 at 32; excess refused by room queues, memory and goroutines did not grow — overload does not bring it down |
| 100,000 messages, 4096 dimensions, 5 minutes | PASS. Heap after indexing 157 MB (int8 index), RSS 453 MB; heap under load 355–570 MB; after load 223 MB heap / 654 MB RSS. Message store p95 20 ms, reply p99 2.5 s, index rebuild 1.2–1.8 s |
| 100,000 messages, 1024 dimensions | Heap after indexing 85 MB, RSS 206 MB; RSS after load 476 MB; database 152 MB |
| 10 minutes with personal features | PASS: 2,124 replies, 180 reminders requested → 153 sent, 20 cancelled, 7 pending; no duplicates, none stuck |

## Recommended system

| Size (stored messages) | CPU | Memory | Disk | For example |
|---|---|---|---|---|
| Personal / friends' servers (~10,000) | 1 core | 512 MB | 1 GB | Raspberry Pi 4/5, the smallest cloud VM |
| A few busy servers (~100,000) | 2 cores | 1–1.5 GB (1 GB at 1024 dimensions) | 5 GB | A 2 GB VM |
| A large community (~500,000) | 2–4 cores | 2–4 GB | 20 GB | A 4 GB VM |

- The CPU is mostly idle. Most of a reply's time is waiting for the outside model, so **the provider's speed and rate limits**, not cores, set the throughput.
- On small devices use `models.embed.dimensions: 1024` and `GOMEMLIMIT` (below).
- Disk is mostly memory (about 15–27 MB per 10,000 messages) and backups (times `backup.keep`). Prefer an SSD over an SD card ([Pending decisions](https://github.com/kim0040/masamong2/wiki/Pending-Decisions), item 2).

## Capacity planning

### Memory

The search index is **held in memory** and grows with stored messages. Rebuilds share unchanged immutable int8 rows across the old and new index; new or changed rows allocate new memory. The lexical FTS databases are still rebuilt, and the old ones remain open briefly after a swap, so size the machine from the working set under load plus margin. The default MRL message width is 512: each eligible message adds a 516-byte vector payload (about 0.5 KiB), plus row metadata. Models outside the MRL catalog use the installation's actual leaf width (full width when no explicit width is set) and can use more. The message space contains the actual numeric width; changed leaf widths trigger re-embedding, and messages wait until a leaf width is known. The measurements below used a 1024 message width and do not reflect the new default.

| Stored messages | 4096 dimensions | 1024 dimensions (default since 2026-10-13) |
|---|---|---|
| 10,000 | 512 MB | 512 MB |
| 100,000 | 1.5 GB (measured RSS peak about 0.8 GB) | 1 GB (measured RSS about 0.5 GB) |
| 500,000 | 4 GB (estimate) | 2 GB (estimate) |
| 1,000,000 | 8 GB (estimate) | 4 GB (estimate) |

For long histories or small devices such as a Raspberry Pi:

- **Lower the embedding dimensions**: `models.embed.dimensions: 1024` applies at once at no cost and shrinks memory and disk a lot. Raising it again needs re-embedding everything, so use it with models that support truncation (MRL), like the default.
- **Automatic Go memory limit**: unless `GOMEMLIMIT` is set, `masamongd` detects the process memory ceiling and applies 60% as a soft GC limit (about 307 MiB on 512 MiB machines, 614 MiB on 1 GiB). Linux uses the process cgroup v2/v1 limits, including parent ceilings, then physical RAM; Windows uses `GlobalMemoryStatusEx`; macOS/FreeBSD use `sysctl`. Unknown platforms keep the existing limit. The startup log and Logs → Doctor show the value and source (`go_memory_limit`). An explicit `GOMEMLIMIT` takes precedence. SQLite/FTS allocations outside the Go heap still need the remaining memory.
- **Automatic performance tier**: the same CPU quota and memory ceiling pick small, medium or large. Small (1 core, or under 1.5 GiB) keeps 2 read connections, one picture decode at a time, one fortune generation and a 2-minute gap between pending-message counts. Medium keeps the previous fixed limits, including SQLite's default 2000 KiB page cache and a 1-minute recount gap. Large (4+ cores and at least 6 GiB) raises the read pool (up to 12), the page cache (32 MiB per connection), decode slots (2) and fortune generations (4), and shortens the recount gap to 20 seconds. There is no new setting; `MASAMONG_PERF=small|medium|large` overrides the tier. Details are in the [memory operations document](https://github.com/kim0040/masamong2/blob/main/docs/memory-operations.md).
- **Turn summaries off**: `memory.summaries: false` removes summary vectors; message vectors remain. Search quality may drop a little.

### Disk

Memory grows with **collected messages**: about 27 MB per 10,000 messages at 4096 dimensions, 15 MB at 1024. It is deleted only by block, never by age.

Operational records grow with **replies** and are deleted after their retention period:

| Item | Per 1,000 replies | Kept |
|---|---|---|
| Question embedding cache | about 8 MB (4096) / 2 MB (1024) | 7 days |
| Conversation records | about 1.9 MB | details 30 days, summaries 400 days |
| Diagnostic traces | about 0.8 MB | 30 days |
| Background job records (memory block preparation) | a few KB | finished ones 30 days after their last change; unfinished ones and the newest preparation job of each room always stay |
| Logs | about 0.2 MB | rotated at about 60 MB |

Example: 3,000 messages and 1,000 replies a day level off at about 130 MB of operational records, while memory grows about 8 MB a day (about 3 GB a year; 1.6 GB at 1024 dimensions). Backups need the database size times the number kept (`backup.keep`, default 4).

### Throughput

Throughput ≈ `limits.max_concurrent_turns` ÷ model time per reply. With real providers at 3–8 s of model time per reply, the default 8 gives 1–2.7 replies per second (3,600–9,600 an hour) — plenty for most personal and community bots. For more, check the provider's requests-per-minute limit and raise the value ([Concurrency and performance](https://github.com/kim0040/masamong2/wiki/Concurrency-and-Performance)).

## Known limits

Crashes are restarted by the supervisor and reported to operators ([Pending decisions](https://github.com/kim0040/masamong2/wiki/Pending-Decisions), item 3). Improvements the operator has to decide (int8 on disk, SQLite sync level, backup retention, …) are collected there.

- **Index rebuilds**: new snippets trigger a rebuild. Unchanged leaf/summary/message int8 rows share immutable memory with the previous index. Live message metadata is checked every time; only new or changed vector BLOBs are read, in batches of 500. Erased, edited and renamed messages leave the next rebuilt index. First-load leaf/summary float16 BLOBs are also streamed in batches of 500 leaves. Score accumulation, message projection order and tie-breaking stay the same. FTS and leaf metadata are rebuilt each time, and rooms with new messages re-read their history. Console counts use the read pool, with list sorting and grouping omitted when counting pending messages.
- **Very large single rooms**: new messages that arrive in order re-split only the tail after a checkpoint (2026-10-12). Only out-of-order cases such as imports, edits, deletions or a change of split rules re-read all of the room's messages; with hundreds of thousands in one room, that one run reads that much (about a second on SQLite).
- **SQLite writes one at a time**: concurrent writers take turns; long reads and backups use separate connections and are not blocked (message store p95 20 ms, max 0.45 s at 100,000 messages).
- Tests use a fake model server. Check real providers' latency, limits and failures in production logs and the per-step times in the console's **Conversations**.

## Low-spec memory remeasurement, 2026-10-12

The same i5-6400T ran with `GOMAXPROCS=1`, 1024 dimensions and message vectors enabled by default. The 100k run used a completed synthetic database copy (100,171 observations, 100,016 vectors) with `-seed 0`; the 10k run seeded a new database. Load lasted three minutes with at most 60 seconds of drain, without overlapping tests. A separate cold/hot Reload helper sampled peaks every 5 ms. These tools bypass daemon main, so they measure the index changes without the automatic Go memory limit.

| Size | Heap after cold load | Hot rebuild heap peak | Working set after hot rebuild | Hot rebuild |
|---|---:|---:|---:|---:|
| 10k | 23.6 MiB | 41.6 MiB | 80.0 MiB | 0.574 s |
| 100k | 208.3 MiB | 416.9 MiB | 494.2 MiB | 7.034 s |

Before the change, the 100k rebuild heap peak was 671–688 MiB and post-index working set was 638 MiB. After the change, 100k load sample peaks were 455.8 MiB heap and 563.4 MiB working set; working set after drain was 619.4 MiB. Initial load took 16.925 s and the last rebuild took 6.400 s. The separate copy's first cold load took 32.930 s; cache conditions differ from a warm database just after seeding, so this does not demonstrate faster cold loading. The hot rebuild reused all 100,016 vectors and read no vector BLOBs.

At 10k, post-seed heap/working set were 30.4/131.7 MiB; load sample peaks were 60.0/159.5 MiB. Both load runs completed without errors and passed database integrity checks. The 30k/512MiB case and real model latency need separate validation. Console latency was then cut with a covering index for the per-room counts and a shared count of messages waiting for a vector (the console and the doctor run one at a time, with a one-minute gap after each). On the same 100k copy and load (Windows Defender used one to two cores during the final run, so these are pessimistic), before → after: blocks list p50 27.1 s → 2.1 s, dashboard p50 26.1 s → 8.1 s, chat reply p50 7.9 s → 3.2 s (p95 37.2 s → 9.7 s), capture p95 7.6 s → 0.56 s, peak process RSS 767 → 635 MB. The first blocks list for a space waits for the pending count (about 5–7 s at 100k). FTS/metadata rebuilding remains the next investigation target.
