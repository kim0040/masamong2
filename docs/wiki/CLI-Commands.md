# CLI commands

[한국어](https://github.com/kim0040/masamong2/wiki/CLI-명령어) | **English**

One program, `masamong`, manages the service and every feature. Everything the console does can be done from the command line.

`masamong` alone, or `masamong help` (`--help`), lists the common commands and command groups. A mistyped command gets the closest match (`masamong staus` → `masamong status`). With `--output json` or in a pipe it returns the JSON result as before (`help.version`, `ACTION_UNKNOWN`).

## Common options

| Option | Meaning |
|---|---|
| `--data-root DIR` | The data folder to use (picks the instance) |
| `--output json` | JSON output for scripts; automatic in pipes and with `CI` or `NO_COLOR` set |
| `--name value` | Command input, e.g. `--dry_run true`, `--guild_id 123` |
| `--input-file FILE` | For `masamong tools test` only: read its JSON object from a file; `--input-file -` reads it from standard input |

Every command returns the same JSON shape:

```json
{"ok":true,"action":"jobs.list","output":{"jobs":[]},"request_id":"cli"}
{"ok":false,"action":"polls.vote","error":{"code":"INVALID_INPUT","message_key":"generic.error","message":"poll not found","retryable":false},"request_id":"cli"}
```

### JSON input and PowerShell

`--input-file` is only accepted by `masamong tools test`; other commands reject it. It reads one JSON object from a file or standard input. This is the easy way on Windows PowerShell, which can strip the quotes out of inline JSON (an invalid inline value is answered with exactly this hint):

```powershell
# a file: UTF-8 (with or without a byte-order mark)
[IO.File]::WriteAllText("$PWD\query.json", '{"region":"Seoul"}')
masamong tools test --id weather.query --input-file query.json
```

```bash
masamong tools test --id weather.query --input '{"region":"Seoul"}'      # bash, zsh: single quotes are enough
echo '{"region":"Seoul"}' | masamong tools test --id weather.query --input-file -
```

The file or input must hold one JSON object. `--input-file` accepts UTF-8 with or without a BOM and UTF-16 little-endian or big-endian when it has a BOM, including files written by Windows PowerShell 5.1. No encoding workaround is needed.

## Service

| Command | What it does |
|---|---|
| `masamong open` | Start (or reuse) the daemon and open the console in a browser |
| `masamong start` | Start the daemon only (for servers) |
| `masamong status` | Running state (service PID and supervisor PID `supervisor_pid`). In maintenance mode JSON includes `maintenance:true` and a one-line `message`; human output shows that same message |
| `masamong restart` | Restart |
| `masamong stop` | Stop (finishes replies and jobs in progress and waits for the process to end, up to 40 seconds). On Windows it also closes this data folder's tray icon; if the tray cannot close, it warns and still stops the service |
| `masamong connect user@host` | Open a remote server's console through an SSH tunnel |
| `masamong tray` | Menu bar / notification area icon |
| `masamong autostart on` / `off` | Start at login and boot |
| `masamong doctor` | Diagnostics (keys, providers, Discord, permissions, storage, time zones). During Discord startup it reports `pending / gateway_connecting` until READY/RESUMED; while the first connection is being retried it includes the attempt count, last error and next try, and permanent failures such as a rejected token stop retrying. Data-folder warnings appear as `data_folder_warnings` and a `data_folder` attention item. Offline, with a restore, reset or update waiting to finish, it shows the maintenance fields below |
| `masamong admin token` | Print the console sign-in token (the file `admin/web-token` in the data folder) |
| `masamong version` | Version |
| `masamong runtime pause` | Pause replies everywhere (`--paused false` resumes; `--scope guild/channel` for part) |
| `masamong runtime status` | Runtime state; in maintenance mode it includes `maintenance:true` and a one-line `message` in JSON, or shows that message in human output |
| `masamong runtime logs` | Recent log lines |

## Settings

| Command | What it does |
|---|---|
| `config show` · `config document` | Current settings |
| `config diff --file F` · `config validate --file F` | Preview and validate a change |
| `config apply --file F --base_revision N` | Apply (recorded as a revision). `N` is the revision the edit started from: `config show` prints `revision`, `config diff` prints `base_revision` ([Configuration reference](https://github.com/kim0040/masamong2/wiki/Configuration-Reference)) |
| `config rollback --revision N` | Go back |
| `config changes` | Change history |
| `config snapshot save/restore/delete`, `config snapshots` | Config snapshots |

## Rooms

| Command | What it does |
|---|---|
| `channels list` | Servers, rooms and their policies |
| `channels configure --guild_id G --id C --respond mention ...` | Room policy |
| `channels view show/attach/detach --guild_id G --channel_id C --block_id B` | Attach memory blocks to a room |
| `channels permissions --guild_id G --channel_id C` | Check the bot's permissions |
| `channels invite` | Invite link |
| `channels ops-log` | Room setting history |

## Conversation and memory

| Command | What it does |
|---|---|
| `chat --guild_id G --channel_id C --message "..."` | Preview a reply (not stored) |
| `imports preview` · `imports start` | Import conversations (CSV and raw archives up to 1 GiB per file; see [Memory blocks](https://github.com/kim0040/masamong2/wiki/Memory-Blocks#size-limits-and-interruptions)) |
| `memory blocks list/show/prepare/update/delete` | Memory blocks |
| `memory reindex [--dry_run true] [--budget_usd 0.5]` | Indexing |
| `memory search --guild_id G --channel_id C --query "..."` | Memory search (evidence and scores) |
| `memory backfill [--guild_id G --channel_id C]` · `memory backfill --status true` | Fill in Discord messages missed while off (last `memory.backfill_days` days) |
| `summaries --guild_id G --channel_id C [--refresh true] [--saved true]` | Channel summary (folds in only new messages; `--saved` shows the last one without a model call) |
| `events --guild_id G --channel_id C [--query "..."]` | Timeline of events (by date; with a question, only related ones) |
| `messages search --rooms R --query "..."` | Original-text search (free) |
| `memory review` · `memory reviews` | Correct or exclude evidence |
| `people list` · `people merge` | Merge a person's names |
| `turns list` · `turns get --id T` | Conversation records |
| `trace show --id T` | A turn's diagnostic trace |
| `eval run` · `eval runs` | Search quality evaluation |

## Tools

| Command | What it does |
|---|---|
| `tools list` | Tool state and today's usage |
| `tools configure --id ID ...` | On/off, limits, command words, defaults |
| `tools test --id ID [--input JSON \| --input-file F]` | Test run |
| `weather`, `disasters`, `finance quote`, `finance fx`, `search` | Run a tool directly |
| `places search --query "..." [--size 5]` | Look up places (Kakao Map; needs a Kakao key, otherwise it answers that it is not configured) |
| `activity ranking --scope discord:<channel ID> [--period today\|week\|month\|all]` | Top talkers of a channel (message counts only; `--scope` names the channel, the default period is `week`) |
| `read --url https://... [--max_chars 2800]` | Read one public web page as text (link reading; off until turned on under **Tools**, and it sends the address to Jina Reader) |
| `polls create/vote/close/result` | Polls |
| `images --prompt "..."` | Generate a picture (the `image` model role) |
| `fortune` · `transfer-notices` · `notices status` | Fortune, transfer notices, school notices |
| `notices subscribe --op collect` | Collect notice boards now (`--op list`: subscribers; `--op pause/resume/delete --user_id U --kind school`) |

## People and consent

| Command | What it does |
|---|---|
| `users` | Each person's consents, subscriptions, settings, blocks (birth data and notice profiles are not shown) |
| `privacy` · `privacy --user_id U --scope memory --decision withdraw` | View or withdraw consent (only the person can give consent, in Discord) |
| `preferences --user_id U [--timezone Asia/Seoul] [--reset true]` | Personal settings |
| `fortune subscribe --op unsubscribe/delete --user_id U` | Stop a fortune subscription or delete fortune data |
| `access [--operators '["ID"]'] [--blocked_users '["ID"]'] [--guild_admins false]` | Operators, block list, server managers' admin command |
| `reminders --op list [--guild_id G] [--channel_id C] [--user_id U] [--status S]` · `reminders --op cancel --id R` · `reminders --op pause\|resume --id R` | List or cancel reminders, pause or resume a repeating one (people set and change their own in Discord) |
| `participation --guild_id G --channel_id C [--mode off\|shadow\|conservative]` | Chiming-in mode and decision log (`--review ID --verdict good\|bad` to review) |

## Providers, models and costs

| Command | What it does |
|---|---|
| `providers list` · `providers test` | Provider state and connection test |
| `models list` · `models select` · `models embed-width` | Models, embedding width (same model: 1024, 2048, 0 = full width) |
| `usage` | Monthly budget and usage |
| `stats query` · `stats series` · `dashboard` | Statistics |

## Operations

| Command | What it does |
|---|---|
| `jobs list/get/watch/cancel/retry` | Background jobs |
| `backup create/list/verify/restore/delete` | Backups |
| `schedules list` · `schedules run` | Scheduled tasks |
| `storage status` | Storage (SQLite; `storage migrate` was disabled with TiDB) |
| `migration apply --path V1.db [--confirm true]` | Import the previous bot's personal settings (plan, then import) |
| `audit` | Audit log |
| `alerts status` · `alerts test` | Operator alerts (Discord DM): status and a test |
| `serverlog test --guild_id G` | Post a test entry to a server's log channel (`channels list` shows each server's `log`) |
| `updates status` · `updates check` | Update check result, and check now |
| `runtime history` | Uptime history (starts, stops, restarts, crashes and why) |
| `locales` | Languages |

## Recovery commands (`masamongd`)

You normally never run `masamongd` yourself. If a restore, a reset or a program update could not finish, the daemon keeps running without opening the database and serves a localized recovery page instead of the bot ([Troubleshooting](https://github.com/kim0040/masamong2/wiki/Troubleshooting#the-console-shows-recovery-required-maintenance-mode)). `masamong open` and the tray's **Open** command reach that page. It lists only the recovery commands that work for the current state. `status` and `doctor` only read; `retry`, `rollback`, `cancel` and `abandon-update` need the data folder to themselves, so stop the maintenance daemon with `masamong stop` first.

| Command | What it does |
|---|---|
| `masamongd --version` | Print the version as JSON; opens no data folder |
| `masamongd --data-root DIR --maintenance status` (or `doctor`) | Show what is pending without opening the database: `maintenance`, `case`, `operation` (`restore`, `reset` or `update`), `phase`, `completed_targets`, `recovery_commands` (only commands valid for this state), `recovery_requires_stopped_daemon`, and `bot_running` / `schedulers_running` (both false) |
| `masamongd --data-root DIR --maintenance retry` | Resume the interrupted operation where it stopped |
| `masamongd --data-root DIR --maintenance rollback` | Put the previous state back: a restore's database, WAL and settings, or a program update's old pair of programs |
| `masamongd --data-root DIR --maintenance cancel` | Cancel an operation that has not touched live files. An unreadable or old-format marker is moved aside as `maintenance-refused-*.json` only when no recovery files show that data was already moved |
| `masamongd --data-root DIR --maintenance abandon-update [--yes]` | For a stuck program-update journal after reinstalling: keep all program files and move only the journal to `.masamong-update.abandoned-<time>.json`. Without `--yes`, type `abandon` when prompted; the kept old and staged files remain until that record is removed |

A **reset** cannot be rolled back or cancelled once it has started erasing; finish it with `retry`. The backup taken before the reset (kind *Before reset*) is how you get the data back afterwards. Run only the command listed by the recovery page. A restore that fails before any live file is moved clears its staged copies, records the failure and keeps the bot running. A committed restore is successful even if a locked old file cannot be deleted yet; cleanup is retried at startup and hourly, and doctor lists what remains.

```powershell
# Windows: the programs are in %LOCALAPPDATA%\Programs\Masamong, which is on your user PATH
masamong stop
masamongd --data-root "$env:APPDATA\Masamong" --maintenance status
masamongd --data-root "$env:APPDATA\Masamong" --maintenance retry
masamong start
```

## Automation examples

```bash
# Save usage as JSON every day
masamong usage --output json > usage-$(date +%F).json

# Pause and resume replies around maintenance
masamong runtime pause
masamong runtime pause --paused false

# Make sure it is running (in a script)
masamong status --output json | grep -q '"running":true' || masamong start
```

```powershell
# The same in PowerShell
masamong usage --output json | Set-Content -Encoding utf8 "usage-$(Get-Date -Format yyyy-MM-dd).json"

masamong runtime pause
masamong runtime pause --paused false

if (-not (masamong status --output json | ConvertFrom-Json).output.running) { masamong start }
```
