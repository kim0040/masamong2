# Connecting Discord

[한국어](https://github.com/kim0040/masamong2/wiki/Discord-연결) | **English**

## 1. Create the bot application

1. In the [Discord Developer Portal](https://discord.com/developers/applications), press **New Application**.
2. On the **Bot** tab, press **Reset Token** to get the bot token. It is shown only once, so copy it right away.
3. On the same tab, under **Privileged Gateway Intents**, turn on **Message Content Intent**. The bot needs it to notice its name, to run `!` commands and to remember conversation.

## 2. Give the token to the console

Paste the token under **Connections → Discord** (or the wizard's Discord step) and switch Discord on. You do not need the application ID: when the token is saved, the console asks Discord itself (`GET /applications/@me`, `masamong discord check`), fills in the bot's name and ID (`instance.expected_bot_id`), and shows whether **Message Content Intent** is on in the portal. If the settings read messages but the portal has the intent off, Discord refuses the connection (close code 4014), so the console shows a red notice with a link to the portal. A rejected token gets a hint to **Reset Token**. After saving, restart the service and it connects to the Discord gateway. The token is kept in the macOS Keychain or Windows Credential Manager, or in an owner-only key file in the data folder on Linux and FreeBSD (a file is permission-protected, not encrypted); the config file only holds a reference such as `keychain:`, `file:` or `env:` ([Security and privacy](https://github.com/kim0040/masamong2/wiki/Security-and-Privacy)).

**If the first connection fails**: while the gateway is starting, `masamong doctor` shows `discord: pending` (`gateway_connecting`) until READY or RESUMED. When Discord or the network cannot be reached (a flaky network, Discord down), the service keeps trying by itself, 1 second apart at first and up to 30 seconds apart, until it connects. You do not need to restart it. `doctor` shows `gateway_retrying` with the attempt count, last error and time of the next try. A rejected token or a missing Message Content Intent (close code 4014) is different: Discord's answer will not change, so retries stop and `doctor` shows `discord: error` (`gateway_start_failed`); fix the token or turn on the intent under **Connections** and in the Developer Portal, then restart. A connection that drops later is resumed by itself, and reminders, fortunes, notices and alerts carry on after it without a restart.

To use an environment variable in `config.yaml`:

```yaml
discord:
  enabled: true
  token_ref: env:MASAMONG_DISCORD_TOKEN   # an environment variable, or the keychain:/file: reference the console made
  command_prefix: "!"
  register_commands: true                 # register slash commands at start
  message_content: true                   # needs Message Content Intent in the Developer Portal
  guild_ids: []                           # empty: global registration; server IDs: registered there at once
instance:
  expected_bot_id: ""                     # when set, commands are not registered for a different bot's token
```

## 3. Invite the bot

Add the bot with the console's **invite link** (Connections; Servers & rooms → room → Access; the wizard's Discord step). The link asks only for the permissions the bot uses, never Administrator. A link made from a room page has that server preselected, which helps when re-granting permissions.

| Permission | | Why |
|---|---|---|
| View Channel | required | See the channel and receive messages |
| Send Messages | required | Replies and notices |
| Embed Links | required | Tool result cards |
| Read Message History | required | Replies and surrounding context |
| Attach Files | recommended | Posting generated pictures |
| Create Polls | recommended | `/poll` |
| Send Messages in Threads | recommended | Replying inside threads |

Without a required permission the bot cannot answer in that room; without a recommended one only that feature stops. If roles or channel settings take a required permission away, a warning appears on the home page, and **Servers & rooms → room → Access** lists what is missing (`masamong channels permissions --guild_id <server ID> --channel_id <channel ID>`).

### Server approval

So that nobody can put the bot into an unknown server behind the operator's back, newly invited servers **wait for approval** (`access.server_approval`, on by default).

- In a waiting server the bot stores nothing, watches nothing and answers nothing. Slash commands get a "waiting for approval" notice; `!` commands are ignored silently. Withdrawing privacy consent and commands from bot operators (`access.operators`) still work.
- **Approve** or **Decline and leave** (the bot leaves; settings and memory stay) from the "Server approval" card in **Servers & rooms** or the home warning. **Revoke** puts an approved server back to waiting.
- Servers with a configured room count as approved. Servers the bot was in before this feature are approved automatically on first connection, recorded in the change history as "automatic".
- With the switch off, the bot works in every server it is invited to.

## 4. Per-room settings

Set each room under **Servers & rooms** (or `masamong channels configure`):

| Setting | Values | What it does |
|---|---|---|
| Reply mode `respond` | `mention` · `always` · `command` · `off` | When the bot replies |
| Collect `capture` | on / off | Store the room's conversation as memory (needs `memory.capture` on) |
| Use memory `memory_use` | `on` · `off` | Search memory when replying |
| Persona `persona_ref` | `warm_ko`, `friendly_ko`, `friendly_en`, … | Tone and **default reply language**. A room with none uses its language's default (`warm_ko` and `warm_ja` are polite; see [Languages](https://github.com/kim0040/masamong2/wiki/Languages)) |
| Extra rules `rules` | text | Additional instructions for this room only |
| Memory blocks `memory_view.blocks` | block IDs | Past-conversation blocks this room may search |
| Tools `tools` | per tool on/off | Override a tool for this room |

```bash
masamong channels configure --guild_id 123456789 --id 987654321 --respond mention --capture true --persona_ref friendly_en
```

> [!IMPORTANT]
> **Room isolation:** a room only sees its own live conversation and the memory blocks attached to it. Conversations from other rooms, servers or DMs are never mixed in.

## 5. Commands

### Slash commands

| Command | What it does |
|---|---|
| `/weather` | Current weather and forecast (KMA in Korea, Open-Meteo elsewhere) |
| `/alerts` | Weather warnings and earthquakes (automatic posts are switched in the console) |
| `/places` | Restaurants, cafes, shops (Kakao Map; needs a Kakao REST key) |
| `/ranking` | Top talkers in this room: today, this week, this month, all time |
| `/quote` | Stock quotes (needs a Finnhub key) |
| `/fx` | Exchange rates |
| `/search` | Web search (needs a key for Tavily, Linkup, Exa, Serper, Brave, TinyFish or Kakao) |
| `/poll` | A Discord poll in this room |
| `/summary` | Three-line summary and decisions of the recent conversation (rooms that collect conversation) |
| `/image` | Draw a picture from a description (needs an `image` model) |
| `/fortune`, `/fortune-register`, `/zodiac` | Today's fortune, register your birth date and time, zodiac fortunes (needs consent; the room gets a three-line summary, the detail goes to you privately) |
| `/notices`, `/notices-register`, `/transfer` | University notices, register for notices, latest transfer-admission notices (subscribing needs consent) |
| `/remind`, `/reminders` | Reminders in plain language (confirmed with a button), list and cancel |
| `/privacy` | See, give or withdraw consent for memory, fortunes, notices and chiming in |
| `/mysettings` | Your weather place, time zone, quiet hours, language |
| `/admin` | Run the bot in this room: pause and resume, reply mode, collecting, chiming in, blocking people in this server (visible to server managers only) |
| `/status` | Bot status (visible only to you) |
| `/help` | Commands available right now: registered slash commands whose tools are ready, your language's prefix commands only when Message Content Intent and a command prefix are enabled, and the "Rate this reply" menu item |

Turning a tool off removes its slash command from Discord's menu at once.

### Prefix commands (`!`, need Message Content Intent)

The prefix words follow the bot's language. In Korean: `!날씨 서울`, `!특보`, `!시세 AAPL`, `!환율 USD`, `!검색 마사몽`, `!투표 lunch | gimbap, ramen`, `!요약`, `!알림 tomorrow 3pm meeting`, `!알림목록`, `!운세`, `!운세등록 1995-05-15 14:30 남 서울`, `!이번달운세`, `!올해운세`, `!구독 08:00`, `!공지`, `!편입`, `!개인정보`, `!내설정`, `!관리`, `!도움말`. The words live in the `locales/` language files, not in code, and can be renamed per tool (`tools.<id>.aliases`).

The bot sends a refusal notice only when the person addressed it (by mention, reply or bot name). Notices use neutral wording and do not reveal budget details; at most one is sent per channel per minute, and at most one to the same person in that channel every five minutes. If a prefix or slash command times out, its final localized error card is still sent when the Discord connection is available.

### Just talking to it

In a `mention` room, call it by name — "Masamong, what's the weather in Busan tomorrow?", "what was the cafe we said we'd go to?" — and the planner model works out what is being asked, uses the right tools and memory, and answers like a friend. The names it answers to are set in `instance.bot_names`.

**Split messages get one answer.** Sending "Masamong" and then "what's the weather tomorrow?" no longer buries the question.
- A message that only calls the bot ("Masamong", "@Masamong", "hey Masamong~") is not answered at once. The bot waits up to 10 seconds for the same person's next message in that channel, which needs no mention.
- A message with content waits 1.5 seconds for more fragments, then gets one answer. Rooms that answer every message (`always`) work the same way.
- Other people's messages are never mixed in, and no request is held longer than 20 seconds.

### Talking in DMs

Send the bot a DM and it answers without a mention or its name. Tools such as weather, search, reminders and fortunes work as they do in a server, and split messages get one answer as above.
- **Who can:** by default operators and members of a server the bot serves (an approved server). The console can switch this to operators only or off. Someone the bot will not talk to gets one short notice a day. `!` and slash commands keep working in DMs even when DM conversation is off.
- **Limits:** by default 30 a day per person and 100 a day for all DMs together. Change them, or remove them, in the console. Hitting a limit gets one notice a day per kind; counts reset at midnight in `i18n.timezone`. The chat reply limits and the monthly budget still apply.
- **Kept apart:** a DM never searches server or room memory, and nothing said in a DM goes into server memory. The bot keeps only the last few exchanges as the DM's own context.
- Thread offers, emoji reactions and chiming in are not used in DMs. Settings: [`dm` in the Configuration Reference](https://github.com/kim0040/masamong2/wiki/Configuration-Reference#dm).

### Server managers' `!관리` (admin) command

Server owners and members with "Manage Server" (`access.guild_admins`, allowed by default), and bot operators, can use it. Results are visible only to the caller, and every change is recorded in the console's change history with **who, which server, and via Discord**.

| Command | What it does |
|---|---|
| `!관리` | This room's state (running, reply mode, memory, collecting, chiming in, blocked people in this server) |
| `!관리 정지` / `!관리 재개` | Pause / resume the bot in this room |
| `!관리 응답 호출` | Reply mode: always, when called, commands, off |
| `!관리 기억 끄기` | Collecting conversation on or off |
| `!관리 먼저말걸기 기록만` | Chiming in ("먼저 말 걸기"): off, log only, speak. Cannot allow more than the instance setting. `끼어들기`, `능동참여` and `참여` still work in its place |
| `!관리 차단 @name` / `!관리 해제 @name` | Ignore that person's messages, commands and buttons in this server only. Bots, yourself and bot operators cannot be blocked |

## 6. Polls

Polls made with `/poll` or in conversation ("make a poll for lunch") use Discord's native components. Even when many people press the buttons at once, every vote is counted under a single transaction lock.

## 7. How replies behave

- While a reply is being written, Discord shows "typing…"; replies longer than Discord's 2,000-character limit are split at paragraph boundaries.
- The bot never pings people, `@everyone` or `@here`.
- When a turn is refused for a limit, budget, pause or busy room, the bot sends a **neutral, localized notice** only if the person addressed it; the notice does not disclose a budget or limit. Notices are capped at one per channel per minute and one per person in that channel every five minutes. A room with replies turned off, and a blocked person, get nothing. The reason is in the console's **Conversations** page either way.
- When a command fails validation (a bad date, a missing field), the card says so in a plain sentence in their language. A command that times out still gets a localized error card when Discord is reachable; permitted operators and server managers also see its error code. Raw technical errors and paths stay in the log.
- Replies and card posts stop as soon as the service is told to stop, so a long reply is not half-sent after a stop.
- A command whose tool has no answer (no key, nothing found) carries on as a normal conversation instead of ending on an error card ([Troubleshooting](https://github.com/kim0040/masamong2/wiki/Troubleshooting)).
- Result cards are trimmed to Discord's embed limits (title 256, body 4,096, 25 fields, 6,000 in total).
- **Feedback on replies does not clutter the chat.** The bot puts no emoji on its own replies.
  - It quietly counts only the reactions people add **themselves** to its replies. 👍 ❤️ 😂 🔥 and the like count as good, 👎 ❌ 😕 and the like as bad, and anything else is ignored.
  - Long-press or right-click a reply and choose **Apps → Rate this reply** to leave good or bad. After bad you can pick a reason (optional). Only you see that window.
  - The results show as 👍/👎 on the conversation in the console's **Conversations**. Reading reactions (Guild Message Reactions) is not a privileged intent, so there is nothing to turn on in the developer portal.
- Personal fortunes and notice briefings go by DM or a message only you can see. If DMs are blocked, nothing is posted in the room; the failure is logged.

## 8. Chiming in

The bot can step in with something useful on its own. It is off by default and needs both the global switch (`participation`) and the room's **Chiming in** option.

- **Log only (shadow)**: the bot decides whether it would speak and writes the sentence to the log only, so you can check its judgment in the console first.
- **Speak (conservative)**: it speaks, in one or two short sentences, only when strict checks pass.
- **What it looks for**: a factual question nobody answered for a while (45 seconds by default), and plans with a clear date and time. For plans it shows a "Shall I set a reminder?" card and schedules a reminder only for whoever presses the button.
- **Filtered before any model call**: messages calling the bot directly, private back-and-forth between two people, blocked people, people who withdrew consent (`/privacy`), the cooldown (180 seconds) and frequency limits (3 per hour, 10 per day).
- If someone answers first or the conversation moves on while it waits, it stays quiet.

## 9. Edits, deletions and catching up

- When someone **edits** a message, memory is updated; when it is **deleted**, it leaves the memory index at once and is excluded from search even before the next indexing run.
- Conversation that happened while the bot was offline is filled in on reconnect, **up to the last 7 days** (`memory.backfill_days`). The bot does not reply to those late messages.
- Messages from people who withdrew memory consent (`/privacy`) are not collected, and what was already stored is deleted. Blocked people only stop getting replies; their messages are still collected to keep the room's context.
