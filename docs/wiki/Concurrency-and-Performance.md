# Concurrency and performance

[한국어](https://github.com/kim0040/masamong2/wiki/동시-처리와-성능) | **English**

Masamong does not handle requests one at a time. Discord messages, slash commands, buttons, console requests and background work (indexing, backups) all run **at the same time**. This page explains what runs how, how much it takes, and how to tune it.

## At a glance

| Situation | Behavior |
|---|---|
| Messages arrive in different rooms at once | Answered **in parallel** |
| Messages arrive one after another in one room | Answered **in arrival order**, one at a time; the second answer knows the first conversation |
| Replies at the same time | At most `limits.max_concurrent_turns` (default **8**); the rest wait their turn |
| A burst in one room | One running plus four waiting; beyond that messages are **skipped quietly** (anti-flood) |
| Slash and `!` commands | Each handled independently, in parallel |
| Many people press poll buttons at once | **Every vote is recorded** |
| Settings change in the console during a conversation | Replies in progress finish without errors; the new settings apply from the next reply at the latest |
| Settings change from the console and Discord at once | Applied in turn; neither overwrites the other |
| Stop or restart | Replies in progress get up to 15 seconds before the database closes |

## The path of one reply

```
Discord message
  → wait for the room's turn (until the earlier reply in that room is done)
  → wait for a free slot (max_concurrent_turns)
  → check limits and budget (including what earlier replies just spent)
  → planner model → tools + memory search (together) → reply model
  → send to Discord, store the conversation record
```

Different tool calls in one reply (such as the weather and a stock quote) and the memory search run **together**, at most 4 at a time. Calls with the same tool name run one at a time in model order, preserving usage and cooldown reservation order. Results also keep model order. Calls that have not started when the turn is cancelled return `cancelled` and give back their call reservations. A panic makes only that call `unavailable`; its ledger records the partial cost and failure class exactly once.

Planner, loop and answer models reserve their call count when they start and add cost when they finish. Optional work is admitted in model or plan order. With a money cap, admission waits for all models in flight to finish so their costs are included; the wait follows cancellation. With the defaults (`chat.tool_mode: loop`, `limits.per_request_budget_usd: 0.03`) the first loop round's tools therefore start after the planner model that started alongside it has finished; the extra time is the difference between the two models' response times, longer if the planner is in a retry backoff (a deliberate, conservative choice). A tool that spends money also finishes before the next call is admitted. This allows no more optional calls than sequential execution.

The 1–4 request caps for external services (Finnhub, KMA, exchange rates, Jina Reader and others) use a process-wide shared governor. Its identity is the provider kind (the canonical provider ID, rather than a config entry's alias), base host and a non-reversible credential hash. Duplicate entries with the same host/key and multiple registries or services share slots. After configuration reload or capacity changes, requests still running on the old registry remain counted. Each HTTP attempt closes its body and releases its slot, including on panic; retry backoff runs outside the slot. An in-flight cap is not a rate limit and does not guarantee requests per second or minute.

Limits and the monthly budget are checked **after** a reply's turn comes, so a burst of requests cannot all slip past the budget together. Waiting counts toward the reply's time limit (180 seconds for a Discord reply, sized for turns that draw a picture), and "typing…" is shown while waiting.

## Measured

Regression tests with a fake model server (a delay per call), all run with `go test -race`:

| Test | What it checks |
|---|---|
| `TestChatTurnsInDifferentChannelsRunInParallel` | 6 rooms at once finish in less than half the sequential time |
| `TestChatTurnsInOneChannelRunOneAtATime` | 5 messages in one room never overlap model calls |
| `TestMaxConcurrentTurnsBoundsModelCalls` | With a limit of 2, model calls never exceed 2 at once |
| `TestChannelBacklogRefusesInsteadOfQueueingForever` | 9 at once in one room → 5 answered, 4 refused |
| `TestChatTurnsWhileConfigChanges` | 6 setting changes during conversation → no races, no lost changes |
| `TestConcurrentPollVotesAreNeverLost`, `TestSimultaneousButtonVotesAllCount` | 12 simultaneous votes → all recorded |

For reference: with 0.3-second model responses, 20 replies sent at once across 4 rooms finished in about 1 second (about 12 seconds one by one).

## Tuning

### `limits.max_concurrent_turns`

How many replies call models at the same time (1–64, default 8).

| Size | Value | Why |
|---|---|---|
| A few personal or friends' servers | 4–8 (default) | Plenty, and it keeps costs from running away |
| Several busy servers | 12–24 | Check the model provider's requests-per-minute limit first |
| Free or cheap model plans | 2–4 | Hitting the provider's limit makes replies slow or fail |

```bash
masamong config show   # check the current value, then change it in the console's settings
```

Changes apply at once; replies already running are not affected.

### Flooding and cost

The concurrency limit only sets "how many at once". **How often** is set with tool limits ([Tools and limits](https://github.com/kim0040/masamong2/wiki/Tools-and-Limits)).

```bash
masamong tools configure --id chat.respond --cooldown_sec 3 --per_user_per_day 200
```

A cooldown like this is recommended for rooms set to `always`.

## Storage

Storage is one SQLite database (TiDB was switched off by the operator on 2026-09-28; see [Storage and backup](https://github.com/kim0040/masamong2/wiki/Storage-and-Backup)). Writes go through one connection in order; they are short, so they are not a bottleneck. Long reads and backups use separate connections.

Most of a reply's time is model calls (seconds) while database work takes milliseconds, so SQLite does not hold replies back. No database connection is held during a model call.

## One data folder = one daemon

A data folder is used by one daemon (`daemon.lock`). You cannot start several daemons on the same folder for more throughput, and you do not need to: one daemon already works in parallel. Run different bots from different data folders.

The `Running` probe checks whether a daemon or tray owns its lock without keeping that lock. On Unix, an owner retries briefly if its lock acquisition overlaps the probe, so checking status cannot make a starting process quit as a duplicate.

## Gateway startup and shutdown

READY and RESUMED callbacks enter the same work-admission guard as replies and commands. When shutdown begins, the gateway closes admission before waiting, so no new handler can join the drain; callbacks already admitted finish before the service closes its database.

## When something is off

- **Slow replies**: in the console's **Conversations**, the per-step times (`timing_ms`) show which step is slow. Usually it is the model provider. If replies often wait for `max_concurrent_turns`, raise it.
- **Some messages get no reply**: if Conversations shows "limited" with `too many replies waiting in this channel`, the room's queue was full. That is the anti-flood working.
- Otherwise see [Troubleshooting](https://github.com/kim0040/masamong2/wiki/Troubleshooting).

Measured throughput, latency, resource use and sizing by scale are in [Load testing and long-running operation](https://github.com/kim0040/masamong2/wiki/Load-Testing-and-Long-Running).
