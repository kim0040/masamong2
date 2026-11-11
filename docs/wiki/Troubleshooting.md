# Troubleshooting

[한국어](https://github.com/kim0040/masamong2/wiki/문제-해결) | **English**

When something goes wrong, start with these two commands:

```bash
masamong status    # is the background service running?
masamong doctor    # keys, outside providers, Discord gateway, permissions, storage integrity
```

The console's **Home** notifications and **Conversations (`/turns`)** page show how a reply was made, or exactly why a message was skipped.

If the daemon is in maintenance recovery, `masamong status` still reports the process as running (`ready:false` in JSON) and now also includes `maintenance:true` plus a one-line `message`. `masamong runtime status` returns the same maintenance field and message. `masamong doctor` reports maintenance explicitly. `masamong open` opens the recovery page.

## The bot does not answer

Go through this list in order:

| Check | Cause and fix |
|---|---|
| Discord gateway | **Connections → Discord** should say "connected". If not, `masamong doctor` tells the cases apart. During startup, `discord: pending` with `gateway_connecting` means it is waiting for READY/RESUMED. `gateway_retrying` (and a `discord_startup` block with attempt count, last error and next try) means Discord or the network was unreachable: the first connection retries by itself, from 1 second up to 30 seconds apart. A rejected token or missing Message Content Intent (close code 4014) stops retries and shows `discord: error` with `gateway_start_failed`; fix the token or turn on the intent, then restart. A later disconnect is resumed by itself, and reminders, fortunes, notices and alerts carry on without a restart. If it says connected but the bot never answers, open the "Connection check" under **Connections → Discord**: when no heartbeat reply has arrived for about two minutes it shows "Needs a look", and `masamong doctor` raises a `discord: degraded` (`gateway_stale`) warning. If it stays that way for a few minutes, restart the bot. The last-event age is only a hint, and a long gap on a quiet server is normal. The check is read-only and never reconnects by itself (DiscordGo owns reconnecting). |
| Reading messages | Mentions, `!` commands and memory collection need `discord.message_content: true` **and** **Message Content Intent** on in the Developer Portal. (Slash commands work without it.) |
| Room reply mode | Under **Servers & rooms**, the room should be `mention` or `always`. With `command` or `off`, ordinary chat gets no reply. |
| Calling it | In `mention` mode it answers only to a name in `instance.bot_names`, an @mention, or a reply to its own message. |
| Discord permissions | Under **Servers & rooms → Access**, check the required permissions (View Channel, Send Messages, Embed Links, Read Message History). |
| Paused | A "Paused" banner at the top means everything is paused; resume it (`masamong runtime pause --paused false`). |
| Limits or budget | In **Conversations**, a turn result of "limited" or "budget" means a daily limit or the monthly budget was hit; review them under **Costs & limits**. The person who addressed the bot gets a neutral, localized notice that does not disclose the budget or limit. Notices are capped at one per channel per minute and one per person in that channel every five minutes. Rooms where replies are off stay silent. |
| Room queue full | `too many replies waiting in this channel` in the log means a burst in one room ([Concurrency and performance](https://github.com/kim0040/masamong2/wiki/Concurrency-and-Performance)). |
| Model API errors | Look for provider errors (401 Unauthorized, 429 rate limit, …) in the turn details, and check the key under **Connections → Models**. In the console's API a missing or refused key, or a provider that is not connected, is answered as HTTP 424 (not 502, which stays for a provider that really failed to answer). |

## Slash commands (`/`) do not show in Discord

- Check `discord.register_commands: true`.
- Global slash commands can take up to an hour to spread across Discord. For instant testing in one server, put its ID in `discord.guild_ids`.
- Commands of disabled tools are removed from the list on purpose.
- If `instance.expected_bot_id` does not match the connected bot, registration is refused (see the log).

## The bot only says "that's not available right now", or cannot do weather, stocks or search

That tool has no API key, hit its daily limit, or its provider is not answering. The console's **Tools** page shows whether each tool can answer now and why (no key, limit, practice mode, …); **Connections** shows the keys and today's usage. Korean weather still works without a KMA key, via Open-Meteo.

What the bot does then:

- In conversation it says in a word that it is not available right now, without operator details such as keys, settings or error codes, and carries on. It never makes values up.
- When a **command** such as `!시세` or `/places` has no answer, the same message carries on as a conversation instead of ending on an error card. The failed tool is not tried again; the bot may use another (a web search instead of a quote) or just reply. Permission refusals, rooms with replies off, and private commands keep the card.
- `!도움말` (help) and "what can you do?" list only tools that can answer now.

## Replies are slow

In **Conversations**, look at each step's time (planner, tools, memory search, reply):

- **The reply model is slow**: usually the provider's latency. Switch to a faster model (e.g. `deepseek-v4.1-flash`) or check the provider's status.
- **Memory search is slow**: that is the rerank call; `memory.retrieve: hybrid` skips it and is much faster.
- **A model hangs**: every role has a time limit (default 30 seconds for the planner, 90 for the reply); past it, the other chat model takes over. An empty reply also counts as a failure and goes to the other model. For models that think for a long time, raise the limit under **Connections → Models → prices** (`models.<role>.timeout_seconds`, 5–600 s).
- **Only slow when many rooms are busy**: raise `limits.max_concurrent_turns` (check the provider's rate limit first).

## Model costs are higher than expected

- **Costs & limits** shows spend by kind (replies, embeddings, summaries, rerank).
- Most indexing cost is summaries; `memory.summaries: false` runs on embeddings only.
- For `always` rooms, set a cooldown (e.g. 3 s) and a per-person daily limit (e.g. 200) on `chat.respond` ([Tools and limits](https://github.com/kim0040/masamong2/wiki/Tools-and-Limits)).
- `limits.monthly_budget_usd` caps the month's spend; replies pause when it is reached. If a one-time setup cost used this month's budget, reset the count on **Costs & limits**; daily history remains and provider usage or key limits do not change.

## The console does not open

- If `masamong open` does not open a browser, copy the one-time link printed in the terminal into a browser.
- For a remote server, run `masamong connect user@host` on your computer. If `masamong` is not on the server's standard PATH, give its full path with `--remote_command`. A separate SSH key file, opening it with `ssh` alone, and a fix for each error message are in [Admin console › Opening the console of a remote server](https://github.com/kim0040/masamong2/wiki/Admin-Console).
- If the interface does not render, the light page `http://127.0.0.1:8787/lite` still offers status and restart.
- If port 8787 is taken, the service takes a free local port by itself (a warning in the log says so) and `masamong open` finds it; the address is also in `daemon.json` in the data folder. To pin a port, set `admin.web.bind` to another local one (e.g. `127.0.0.1:8790`).
- If the console asks you to sign in again (after "Log out", or when the session ended), its sign-in screen shows the command to run for your system: `masamong open` in bash or zsh, or `& "C:\...\masamong.exe" open` for PowerShell with a `cmd.exe` form beside it. The token itself is the file `admin/web-token` in the data folder (`%APPDATA%\Masamong\admin\web-token` on Windows; `masamong admin token` prints it). The light `/lite` page signs in with a form that sends the token in the request body, not in the address, so it stays out of the browser history and logs.

## On Windows

| Symptom | Cause and fix |
|---|---|
| `masamong` "is not recognized" right after installing | The installer added the folder to your user PATH, but a window that was already open has the old PATH. Open a **new** PowerShell, or run `& "$env:LOCALAPPDATA\Programs\Masamong\masamong.exe" open` (the installer prints this line) |
| `install.ps1` "cannot be loaded because running scripts is disabled" | Run it as `powershell -ExecutionPolicy Bypass -File .\install.ps1` |
| The installer stops with "tray or daemon processes did not exit" | Something still holds `masamong.exe` or `masamongd.exe`. Close the console tab's service (`masamong stop`) and the tray icon, check Task Manager for `masamong`/`masamongd`, then run it again. The installer waits up to 45 seconds first |
| A key set with `$env:MASAMONG_..._KEY` or `setx` is not found | Variables only reach processes **started after** they were set. Run `masamong stop`, then `masamong start` from the window that has the variable, or set it for your user with `[Environment]::SetEnvironmentVariable('NAME', 'value', 'User')` and start the service from a new window. A service started from the tray icon or at login does not see variables set in a terminal; typing the key into the console avoids all of this |
| Reminders or daily limits are off by nine hours | The programs carry their own IANA time zone database, so this should not happen on a current version. `masamong doctor` lists an `i18n.timezone` or memory time zone that cannot be loaded; correct the name. If you run an older version, update |
| The tray icon is still there after `masamong stop` | On current versions `masamong stop` also closes the tray of that data folder. With an older version, quit it from the icon's menu or end `masamong.exe` in Task Manager |
| Where did my key go? | Keys typed in the console go to Windows Credential Manager (an entry `masamong/<account>`; you can see it under Control Panel → Credential Manager → Windows Credentials). If you set `MASAMONG_SECRET_STORE=file`, they go to a file under `%APPDATA%\Masamong\secrets` instead. Existing `keychain:` and `file:` references keep working |
| `--data-root` on another drive or a shared folder | The service restricts the folder to your account, SYSTEM and Administrators when it starts. If setting that fails (for example on a folder that has no Windows permissions, such as some network shares) it prints why and does not start; use a local NTFS folder |
| Windows `doctor` keeps showing a child permission warning | The ACL migration marker is written only after every child is protected. The daemon retries incomplete migrations on each start, and `doctor` keeps the data-folder attention item until the failing child can be protected. A data root that is itself a junction (for example, `mklink /J` to another drive) is supported; reparse points inside it are skipped |
| Pasting a command from the console fails | The console shows the command for PowerShell (`& "C:\...\masamong.exe" ...`) and, when it differs, for `cmd.exe`. Use the form that matches the window you are in |

## The console shows "recovery required" (maintenance mode)

A restore, reset or program update could not be finished. The daemon does not open the database, so the bot, schedulers and memory workers stay off; the daemon itself remains available and serves a **localized, human-readable recovery page** at the usual console address (`127.0.0.1:8787`, or a free local port if that one is taken). `masamong open` and the tray's **Open** command reach this page. `masamong status` reports the daemon as running at the recovery address (`ready:false` in JSON), with `maintenance:true` and a one-line `message`; `masamong runtime status` includes the same fields. `masamong doctor` also explicitly reports maintenance. The page explains what happened and lists only the recovery commands that fit that state; raw technical errors stay in the log. It answers loopback Host names and refuses cross-origin writes.

1. **See what is pending.** `masamong status` shows the daemon is running at the recovery address (`ready:false` in JSON) and includes `maintenance:true` and a one-line `message`. `masamong doctor`, `masamong runtime status` and `masamongd --data-root <folder> --maintenance status` show the operation, phase, completed reset targets and exact `recovery_commands` available for this state. The bot and schedulers remain off.
2. **Stop the maintenance daemon before recovery.** `retry`, `rollback`, `cancel` and `abandon-update` need the data folder to themselves. Run `masamong stop`; it stops the maintenance daemon through its authenticated stop endpoint. The page also prints the matching stop command, including `--data-root` when needed. `status` and `doctor` only read and do not require a stop.
3. **Follow the page's command list.** Run only commands it lists, then start the service with `masamong start`:
   - `retry` resumes an interrupted restore, reset or update.
   - `rollback` restores the previous database and settings, or the old program pair, when rollback is available.
   - `cancel` is for an operation that has not touched live files. An unreadable or old marker is moved aside for inspection only when no recovery files show that data was already moved.
   - `abandon-update` is for a stuck program-update journal after reinstalling Masamong. `masamongd --data-root <folder> --maintenance abandon-update [--yes]` keeps every program file and moves only the journal to `.masamong-update.abandoned-<time>.json`; it deletes nothing. Without `--yes`, type `abandon` when prompted. Old and staged program files remain until that abandoned record is removed.

A reset that has started erasing can only be finished with `retry`; the *Before reset* backup is how to restore the data. If a restore fails during preparation, before moving any live files, Masamong removes its staged copies, records the failure and keeps the bot running on the current data; there is no recovery page to clear. A restore that has committed is still successful if another program holds an old file open: cleanup is retried at startup and hourly, and any remaining files appear in `doctor` until removed. Close a lock holder and retry only when the recovery page lists a command for it.

If the supervisor cannot start a runnable pair of programs after an interrupted update, it stops instead of looping and records the error in `logs/daemon.out`. The previous programs are kept as hidden `.masamong*.exe.old-*` files (`.masamong*.old-*` on macOS and Linux) in the program folder until the new pair is confirmed complete. Reinstalling with `install.sh` or `install.ps1` replaces the programs and never touches the data folder ([Installation](https://github.com/kim0040/masamong2/wiki/Installation)). The commands are listed in [CLI commands](https://github.com/kim0040/masamong2/wiki/CLI-Commands#recovery-commands-masamongd).

## "Already running" or a database lock error

Two daemons are trying to use the same data folder. Check the running daemon's PID with `masamong status`, stop it with `masamong stop`, and start again. After a crash, the file lock is released automatically when the daemon starts again.

## The service restarted by itself, or keeps stopping

A service started from the icon, `masamong open` or `masamong start` runs under a supervisor (`masamongd --supervise`). After an abnormal exit the supervisor restarts it after 1 second and then longer intervals, and the next start records "crash" and the reason (`exit status 2`, `signal: killed`, …) under **Logs → Uptime**. With operator alerts on, a Discord DM arrives too.

After 5 crashes within 10 minutes it assumes the same cause keeps failing and stops restarting. Then:

1. Look at the end of `logs/daemon.out` for `supervisor:` lines and the `panic:` / `fatal error:` lines before them.
2. If it started right after a settings change, roll back with **Settings → Change history** or `masamong config rollback`.
3. Start it again with `masamong start`; the reason it stopped is reported once more as an alert.

## "Conflict" when saving settings

Settings were changed first from another browser tab, the CLI or a Discord admin command. This protects you from overwriting someone else's change. Reload to get the latest settings and make your change again. In the console, a draft you were editing is rebased onto the newest settings with your edits on top. From the command line, `masamong config apply --file F --base_revision N` needs the revision your edit started from (`masamong config show` prints it); without it, apply answers `CONFIG_CONFLICT` "expected N got 0" ([Configuration reference](https://github.com/kim0040/masamong2/wiki/Configuration-Reference)).

## Where the logs are

- **Console**: the **Logs** page streams the live log (SSE) and shows past starts
- **CLI**: `masamong runtime logs`
- **File**: `logs/daemon.log` in the data folder (Windows: `%APPDATA%\Masamong\logs\daemon.log`; follow it with `Get-Content "$env:APPDATA\Masamong\logs\daemon.log" -Tail 50 -Wait`). At 10 MiB it rotates to `daemon.log.1` … `daemon.log.5`, so logs stay within about 60 MiB.
- After a crash (panic), `logs/daemon.out` holds the Go stack trace and the supervisor's restart lines (`supervisor:`).
- As a systemd service: `journalctl --user -u masamong-*`
- Logs never contain raw API keys or database passwords.

## FAQ

**Q. Can I run Masamong completely free?**
The programs, the SQLite database, the console, backup and restore, keyword search and practice mode cost nothing. You pay only the outside providers you connect (models, market data, paid search). Weather from Open-Meteo needs no key, but the free API is non-commercial and requires credit (CC BY 4.0). Stock quotes (Finnhub) and exchange rates (ExchangeRate-API) restrict sharing or redistributing their data, so check their terms before using them in a room many people read ([backlog](https://github.com/kim0040/masamong2/blob/main/docs/BACKLOG.md)).

**Q. Can one computer run several bots?**
Yes. Give each a separate data folder (`masamong --data-root /path/...`) and they run as fully separate instances. Give each console its own port (`admin.web.bind`) too.

**Q. Can I use OpenAI, Gemini, or local vLLM/Ollama models besides OpenRouter?**
Yes. Under **Connections → Add provider**, pick Google Gemini or OpenAI and enter a key; once the connection checks out it recommends that provider's latest model for each role. Any other OpenAI-compatible address (DeepSeek, a local server, …) is added as **Other OpenAI-compatible** with a name and URL. Gemini and OpenAI have no rerank, so keep only that role on OpenRouter or run without rerank. For self-hosted models, set the token prices to 0 to keep the cost ledger right.

**Q. I entered a key and get "connection test failed: timeout".**
The network to the provider is slow or down (a wrong key shows "key rejected" instead). Try **Test connection** again later. Gemini answers a wrong key with 400, which is also shown as "key rejected".

**Q. Web search says "no search service key".**
Add one of Tavily, Linkup, Exa, Serper, Brave, TinyFish or Kakao under Connections and enter its key. With several, they are used in the order set under **Tools → Web search**, falling over to the next on failure.

**Q. What happens when many people write at once?**
Requests from different rooms run in parallel in the background; within one room, replies follow arrival order to keep the conversation consistent. See [Concurrency and performance](https://github.com/kim0040/masamong2/wiki/Concurrency-and-Performance).

**Q. Does it run on a Raspberry Pi or similar small boards?**
Yes. 64-bit OSes use `linux_arm64`, 32-bit Raspberry Pi OS uses `linux_armv7`; `install.sh` picks the right build for the CPU. See [Load testing](https://github.com/kim0040/masamong2/wiki/Load-Testing-and-Long-Running) for memory sizing.
