# Tools and limits

[한국어](https://github.com/kim0040/masamong2/wiki/도구와-한도) | **English**

Masamong's tools (weather, stocks and FX, web search, polls, memory search and more) can each be turned on or off, overridden per room, limited precisely, and extended with **custom HTTP tools without code**. Whichever way a tool is called — conversation, `!` prefix, `/` slash command, CLI or console — it passes through the same gateway: a tool switched off is blocked everywhere, and limits apply the same way everywhere.

## Built-in tools

| Tool ID | In chat | Prefix / slash command | Credentials / API key |
|---|---|---|---|
| `memory.search` | automatic | — | None (embeddings use the model key) |
| `weather.query` | yes | `!날씨`, `/weather` | Korea: KMA `MASAMONG_KMA_KEY` (falls back to Open-Meteo without it); elsewhere: no key |
| `disasters.query` | yes | `!특보`, `/alerts` | Korea only. KMA key. The automatic **disaster alerts** that post without being asked are described below |
| `places.search` | yes | `/places` | Korea only. Kakao REST key (the same one as Kakao search). Finds restaurants, cafes and shops on Kakao Map with name, address, phone and map link. "places near me" with no area makes it ask where |
| `activity.ranking` (top talkers) | yes | `/ranking` | None. Who sent the most messages in this room (today, this week, this month, all time). Counts only; no message text is stored for it |
| `finance.quote` | yes | `!시세`, `/quote` | Finnhub `MASAMONG_FINNHUB_KEY`. Korean stocks and indexes the free plan lacks are answered from a web search, which the reply says |
| `finance.fx` | yes | `!환율`, `/fx` | Optional (`MASAMONG_EXCHANGERATE_KEY`) |
| `search.web` | yes | `!검색`, `/search` | At least one search service: Tavily, Linkup, Exa (AI search), Serper (Google), Brave (independent index), TinyFish (free up to 12,000 a day), Kakao (Daum). Used in order, falling over on failure; a service that has not answered in 8 s (quick), 10 s (standard) or 15 s (deep) is passed over, so a slow one does not hold the turn. The model picks a depth (quick for one fact, deep only when asked for a news roundup or a comparison; light questions stay standard) and the news topic; each service maps them to its own options, and for news the services with a news search go first. Different from `memory.search`, which searches the room's past |
| `web.read` (read a link) | yes (**opt-in**) | — | Optional (Jina Reader, 20 a minute without a key). Reads the page behind a link in the message for the answer. The link goes to an outside service, so it is off by default and turning it on shows what leaves. Internal addresses (localhost, private IPs) are never read |
| `polls.create` | yes | `!투표`, `/poll` | None (Discord's built-in polls) |
| `help.version` | yes | `!도움말`, `/help` | None ("what can you do?"). Lists only what is live: tools that can answer now (no key or no model means left out) and slash commands that are actually registered |
| `chat.respond` | — | — | **The reply engine itself.** A limit here caps the number of replies per day |
| `reminders.propose`, `reminders.update` | yes | `!알림`, `/remind`, `!알림목록`, `/reminders` | None. "Remind me about the meeting tomorrow at 3pm" works in plain conversation. When the time is ambiguous, a confirmation button (DM or the room) settles it before scheduling; nothing is sent twice. Can repeat (see below) |
| `summaries.channel` | yes | `!요약`, `/summary` | None (uses the summary model). Summarizes stored conversation incrementally. An edit, deletion or withdrawn memory consent in the channel drops the saved summary; the next request makes a new one |
| `images.generate` | yes | `!이미지`, `/image` | Needs the `image` model role. One at a time; default daily limits 10 per person, 30 per room, 50 overall; files kept for `retention.image_days` (14 days). **A picture asked for in chat is drawn in the background**: the bot answers at once that it is drawing, and posts the picture as a reply to the request when it is done (a short line if it fails). At most 3 pictures wait, a person has one at a time, and the same request from the same person is not drawn twice but waits for the first. A picture is dropped without a word if the channel is paused, the requester was blocked or withdrew their memory consent, or the requester deleted the request message (a picture whose request is gone is never posted); a short reconnect of the bot is waited out (up to 30 seconds), a channel where the bot cannot attach files is not drawn for at all, and waiting pictures are dropped when the bot stops. Turn off "Draw in the background when asked in chat" under Tools → Image generation → Settings (default parameters; `tools.images.generate.defaults.async: false`) and the answer waits for the picture instead. The `/image` command always waits. **Content control:** a request is first checked against a list of clearly sexual or violent words in the language packs (`image_blocked_words`, every language's list applies). That list is only a **fast reject**: a request that avoids those words is not caught by it, so it is not a safety filter and not a measure of coverage. The real control is the **image provider's own content filter**, so choose an image model and provider whose safety filter is on |
| `fortune.query`, `fortune.subscribe` | — | `!운세`, `/fortune`, `!운세등록`, `!이번달운세`, `!올해운세`, `!구독`, `/zodiac` | Uses the model key. **Needs fortune consent.** Public rooms get a three-line summary; the detailed reading goes privately (DM or a message only you see). The morning briefing is prepared ahead and sent at each person's chosen time |
| `notices.status`, `notices.subscribe`, `transfer.recent` | — | `!공지`, `/notices`, `!공지등록`, `!편입`, `/transfer` | Korea only (Korean universities). None (reads public university pages). **Subscribing needs consent** (just listing recent notices does not). A daily DM briefing of new notices |

### Repeating reminders and conversation lines

- **Repeating**: "every day at 8am remind me to take my pill", "every Monday and Wednesday 9am standup", "on the 31st of each month", "every 6 hours". The rules are daily, weekly on chosen weekdays, monthly on a day (a shorter month uses its last day) and every N hours (1–48), each with an optional end (`until` date or a number of times). The same confirmation as a one-time reminder applies, and the card states the repetition ("Every day at 08:00"). In Discord `/remind` has an optional `repeat` choice (daily, weekly, monthly; the weekday or day comes from the time you give).
- **Times**: computed on the wall clock of the reminder's own time zone, so 08:00 stays 08:00 across a daylight-saving change. Each occurrence is sent at most once: the next one is scheduled in the same step that takes the current one, so a stop in the middle can neither send it twice nor end the series (an occurrence cut off mid-send is marked unknown and the series goes on). After downtime (or when `grace_min` is longer than the interval) only the newest occurrence still inside `grace_min` is sent; older ones are skipped, never sent in a burst. The last occurrence of a series is retried up to three times when Discord refuses it, like a one-time reminder. DMs still wait for quiet hours to end.
- **Managing**: cancelling, pausing and resuming apply to the whole series: `/reminders` (Cancel / Pause / Resume buttons) for the person, **Rooms → room → Reminders** and **People → person** in the console for the operator. Changing the time of a series keeps its rule and asks for confirmation again.
- **Limits** (`tools.reminders.propose.defaults`): `max_repeating` (10 repeating reminders per person), `max_repeating_room` (30 per channel), in addition to `max_active` (20), `max_days` (366), `grace_min` (30).
- **Conversation lines**: when a reminder is asked in chat with something vague ("remind me about that"), up to 3 recent lines (each at most 220 characters, 700 in all, author name and text) are kept with it and shown under a short header when it fires (for a repeating reminder, with its first message only; the rows are deleted after it). They come only from the caller's own conversation (the lines the bot attributes to it, plus what the caller said in the last 10 minutes), only in a channel where **memory capture is on and memory is used**, and never from someone who withdrew memory consent. If capture or memory is turned off in the channel before the reminder fires, the lines are dropped unsent. The lines are deleted when their message is deleted or edited, when their author withdraws memory consent, when the reminder is over, and after 30 days at the latest. A model cannot supply lines itself; commands and the console cannot attach any.

### Disaster alerts (automatic earthquake and warning posts)

Like the previous bot, it posts earthquakes and weather warnings **on its own**. It covers Korea only and needs a KMA key and a Discord connection. A new instance set up outside Korea starts with `disasters` and `notices` off (see [Configuration Reference](https://github.com/kim0040/masamong2/wiki/Configuration-Reference#features)).

- **Earthquakes**: checked every minute. By default magnitude 4.0+ in Korea and 7.0+ abroad, and "no impact in Korea" notices are not posted. Quakes within 72 hours and 150 km count as one event; the first post is edited to update it.
- **Warnings**: checked every 5 minutes. Land warnings nationwide, advisories only for chosen regions, marine warnings only when switched on; lifted warnings are announced too.
- When first turned on, that moment is the starting point; old news is not posted in a burst.
- **Switching**: in the console under **Tools → Disaster alerts → Operations** (all, warnings, marine, magnitude thresholds, advisory regions); per room with the "Disaster alerts" switch on the room's **Replies** tab. The status (`disasters.alerts`) shows why it is idle (off, no KMA key, Discord not connected, no room to post in).

Personal features (fortune, notices) are tuned with `tools.<id>.defaults` (e.g. the daily fortune limit `daily_limit`, how early the morning briefing is prepared `lead_min`, how often notices are collected `collect_every_hours`, the per-site pause `host_interval_ms`). The boards to collect are data, not code: copy the default catalog (`internal/features/notices/catalog.json`) to `<data-root>/notices/catalog.json` and add or change boards there.

A tool without its API key does not make values up: the bot says in a word that it is not available and carries on (a command carries on as conversation; see [Troubleshooting](https://github.com/kim0040/masamong2/wiki/Troubleshooting)). `!도움말` (help) lists only tools that can answer now. Each tool's state is in `masamong tools list` or the console's **Tools** page. The server decides readiness once (`ready`, `ready_reason` in `tools.list`): a service key is missing (`no_key`), the model role the tool needs (`needs_model`, `model_key_missing`; image generation uses the `image` role), memory indexing (`needs_reindex`, `embed_model_changed`, `no_messages`), opt-in tools (`opt_in`), and tools unavailable in practice mode (`practice_unavailable`). A missing key for an optional tool is shown only on the Tools page, not as a home notification.

The **limit presets** on **Stats & costs › Costs & limits** fill every tool's per-person and per-room daily limits and cooldown at once (relaxed 50 · 300 · 0 s, normal 20 · 120 · 3 s, strict 8 · 40 · 10 s; fewer for image generation, more for chat replies). Overall daily limits (e.g. 50 images) are left as they are, and nothing applies until you save.

### Outside service terms

The outside APIs the tools call follow each company's own terms, separate from Masamong's MIT license (official terms checked on 2026-10-01).

| Service | Tool | Good to know |
|---|---|---|
| Finnhub | Stock quotes | Personal use. Sharing the data with others needs Finnhub's written approval |
| ExchangeRate-API | Exchange rates | No redistribution of the data; your own use only |
| Open-Meteo | Weather abroad, Korean fallback | The free API is non-commercial only. Data is CC BY 4.0, so weather cards show `Open-Meteo.com · CC BY 4.0` |
| KMA | Korean weather, warnings, earthquakes | Credit shown at the bottom of the card |
| Kakao | Places, search | Kakao Developers terms and daily quota |

If Open-Meteo returns a time-zone name this build does not know, forecasts use its `utc_offset_seconds` value (UTC when that value is absent).

Posting stock prices or exchange rates in a room many people read may count as "sharing". If the room is not just you, check the terms and turn the tool off if needed. Open items are in the repository's [backlog](https://github.com/kim0040/masamong2/blob/main/docs/BACKLOG.md).

## Testing and defaults in the console

The **Test** tab and **Settings → Default parameters** on each tool's page show a form of the inputs that tool takes (the list comes from the server's tool definitions in `internal/tools`). Switch to **Raw JSON** when you need it; **Insert example** fills in an example in the tool's format.

## Turning tools on and off

```bash
# Turn a tool off everywhere
masamong tools configure --id search.web --enabled off

# Not from conversation, only as the !투표 (poll) command
masamong tools configure --id polls.create --chat off

# Not as a command, only from conversation
masamong tools configure --id weather.query --commands off

# Command words and default parameters
masamong tools configure --id weather.query --aliases "날씨,w,weather"
masamong tools configure --id weather.query --defaults '{"region":"Busan"}'

# Back to the built-in defaults
masamong tools configure --id search.web --enabled default

# Off in one Discord room only
masamong tools configure --id search.web --guild_id <server ID> --channel_id <channel ID> --enabled off
```

Turning off a feature group (`features.search: false` and so on) turns off every tool in it.

## Usage limits

```bash
# Web search: 300 a day overall, 20 per person, 10-second cooldown
masamong tools configure --id search.web --per_day 300 --per_user_per_day 20 --cooldown_sec 10

# Chat replies: 200 per person a day, 3-second cooldown
masamong tools configure --id chat.respond --per_user_per_day 200 --cooldown_sec 3
```

| Field | Meaning (0 = unlimited) |
|---|---|
| `per_day` | Calls per day for the whole instance (CLI and background included) |
| `per_user_per_day` | Calls per day for one Discord user |
| `per_channel_per_day` | Calls per day in one Discord room |
| `cooldown_sec` | Minimum seconds between one user's calls |

A "day" starts at midnight in the configured time zone (`i18n.timezone`). Counters are stored in SQLite, so they survive restarts and reloads. User IDs are stored only as one-way hashes.

When a tool's limit is reached, the bot says so politely in the user's language. When the reply engine's own limit (`chat.respond`), the monthly budget, a pause or a busy room stops a reply, the person who wrote gets one short notice in their language, at most once per person and room in five minutes, so a busy room is not filled with notices. A room with replies off stays silent.

### Instance-wide resources and budget

| Key | Meaning |
|---|---|
| `limits.monthly_budget_usd` | Replies pause when this month's effective model spend passes it (default $10.00). Use **Costs & limits** or `masamong usage reset` to start the count from now; undo is available this month. Daily history and provider usage or key limits stay unchanged, and the count resets automatically next month |
| `limits.max_concurrent_turns` | Replies generated at the same time (default 8) — [Concurrency and performance](https://github.com/kim0040/masamong2/wiki/Concurrency-and-Performance) |
| `providers.<id>.daily_limit` | Requests per day to one outside provider |
| `memory.turn_budget_usd` | Memory search / rerank budget for one turn |
| `memory.index_budget_usd` | Budget for one background indexing run |

> [!TIP]
> In rooms set to `always`, set `cooldown_sec: 3` and `per_user_per_day: 200` on `chat.respond` to stop someone spamming the bot.

## Custom HTTP tools without code

Any HTTPS REST API that returns JSON can become a tool with a few lines of YAML, no code or rebuild. The planner model reads the description and parameters and calls it when it fits; with `aliases` it also works as a `!` command.

```yaml
tools:
  lotto.latest:                        # tool ID: lowercase.lowercase
    description: "Lottery winning numbers. parameters.round (draw number, omit if unknown)"
    route: "questions about lottery numbers or results go to lotto.latest"  # optional routing hint for the planner
    aliases: [lotto]                   # command word (!lotto)
    params:
      - {name: round, required: false}
    http:
      url: "https://api.example.com/lotto?round={round}&key={credential}"
      credential_ref: env:LOTTO_API_KEY
      headers: {Accept: application/json}
      timeout_sec: 10
      max_chars: 3000                  # most characters of the response passed to the model
      pick: [draw_no, numbers, date]   # optional: keep only these JSON keys
    limits: {per_day: 200, cooldown_sec: 5}
```

Built-in guardrails:
- The URL is checked as a parsed address when the config loads: HTTPS only, except plain HTTP for `localhost` and loopback addresses (`127.0.0.0/8`, `::1`) for local debugging. An address with a username or password, a look-alike host such as `localhost.example.com`, or a placeholder in the host part is refused.
- Parameters cannot go into the host part of the URL, so the model cannot redirect the request to another server (SSRF). Redirects are not followed.
- Parameter values are URL-encoded, and template variables (`{variable}`) that were not declared are rejected when the config loads.
- The credential (`credential_ref`) never appears in logs or in error messages shown in chat.
- **A successful response is scrubbed too.** If the API echoes the credential back (in a value, a key or an array, raw, URL-encoded, base64, or JSON/Unicode-escaped), it is removed before the text goes to the model, the chat or the history. This catches the credential itself in those forms, not data the remote service has changed in another way, so for an API that returns authentication or debug fields, keep only the fields you need with `pick`.
- Credentials shorter than 8 characters are not scrubbed because replacing such a short string could corrupt ordinary response text; the operator gets a warning in the log. Use a longer API key.

Check a custom tool before using it from the console's **Tools → Test** tab or the CLI (`masamong tools test --id lotto.latest --input '{"round":1}'`; in PowerShell, which can strip the quotes, use `--input-file query.json`, see [CLI commands](https://github.com/kim0040/masamong2/wiki/CLI-Commands#json-input-and-powershell)). Test runs count toward the limits too.
