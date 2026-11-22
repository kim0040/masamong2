# Statistics

[한국어](https://github.com/kim0040/masamong2/wiki/통계) | **English**

Masamong keeps daily totals of what this install does, in its own database, so you can see how it is used and what it costs. **Nothing is sent anywhere by the program.** If you want to report a problem or share how your install runs, you can build a small file from those totals and decide yourself whether to send it.

## What is collected, and where it stays

Totals only, one set per day (the day follows the instance's time zone):

| Area | What is counted |
|---|---|
| Conversations | Turns by outcome (answered, paused, limited, …), reply time, cost |
| Model calls | Calls, tokens, cost, errors, fallbacks, by role, model and provider family |
| Tools | Calls by tool, result status, error class and guard verdict |
| Replies | Parts sent, retries, replies that could not be completed |
| Feedback | Ratings and implicit reactions by kind |
| Chiming in and conversation | Decisions by mode and state, name calls, follow-ups, threads |
| Reminders and jobs | By status, how late a reminder went out, job results |
| Activity | Messages, active rooms and active people per day (only people whose messages are collected) |
| Actions | Console, command line and Discord actions by name, where they came from and the result |

The totals hold **no message text, no names, no Discord IDs, no keys and no addresses**. A name you chose yourself (a custom model, a custom HTTP tool) stays on this install and shows in the console; it never goes into an exported file. The busiest hours of the day are kept for the console only.

Totals are made from the records the bot already keeps (turns, the model call and tool ledgers, feedback, reminders, jobs) and are not pruned by age. A day that has been finished is not rewritten afterwards, so a total does not change when the records behind it are pruned. A person's withdrawal removes their records, not the totals: a total counts events and holds nothing that points to a person, and because finished days are never recalculated, no number shifts afterwards. Action counts are written in batches, so a crash can lose the last few minutes of them.

See them in the console under **Stats & costs › Stats**.

## Turning collection off

Set `stats.enabled: false` in `config.yaml`, or switch **Collect statistics** off in the console under **Settings** (section *Statistics*). The bot then collects nothing new. What was collected earlier stays and can still be exported, and the export card tells you collection is off. The setting needs no restart.

`ledger_days`, `pending_days` and `context_days` under `retention` are separate: they decide how long the records the totals are made from, and member content such as unconfirmed reminders, are kept. See the [Configuration reference](https://github.com/kim0040/masamong2/wiki/Configuration-Reference).

## What an export contains

The export is one JSON file of **weekly** totals. It is built only from the daily totals and works when collection is off.

- `schema`, `generated_at` (a week, such as `2026-W47`, not a date).
- `install`: a random install id, the program version, operating system, CPU architecture, the machine size tier (`small`, `medium`, `large`) and the week this install was first seen.
- `period`: the weeks covered. Weeks follow ISO numbering in UTC and are whole; a week that has not finished (it counts as finished two days after its last day) is left out.
- `settings`: how the install is set up, as switches (on or off), choices from fixed lists, and counts as ranges (`1`, `2-5`, `6-20`, `21-100`, `100+`): for example whether memory capture is on, the retrieval mode, the chime-in mode, the backup schedule, how many servers, personas and providers are configured, and which families of providers are used.
- `weekly` and `totals`: one row per metric and week (and over the whole period). Each row has a metric name, dimensions, a count, and for some metrics sums (tokens, cost, time) and a reply-time histogram.

How the privacy rules work:

- **No daily or hourly rows, and no dates anywhere.** Weeks only.
- **Activity is rounded.** Messages to one significant digit (1,456 becomes 1,000); active rooms and people as ranges.
- **No weather or disaster alert rows.**
- **Small counts are folded.** In a week, a row about what members do (tools, ratings, reminders, chiming in, and actions that came from Discord) with fewer than 5 events is added to an `other` row instead of being listed by name. The model's own tool calls are counted in the tool rows only, not in the action rows. Model errors, time, cost and jobs are shown as counted.
- **Every name comes from a fixed list.** A model that is not on the built-in list is shown as its vendor's `other` or as `custom` (a fine-tune or a proxy id can carry a company name); a tool of your own is `custom_http`; a provider is shown as its family (`openrouter`, `openai`, `gemini`, `anthropic`, `compatible`, or `custom`); anything else is `other`.
- **Settings carry no names.** No server or room names or IDs, persona names or text, host names, URLs, keys or time zone.

It does **not** contain message text, names, Discord IDs, server or room names, your own model, provider or tool names, addresses, keys or persona text.

Folding small counts removes the single dated act; it does not make one very active person anonymous. **Read the whole file before you share it.**

### The install id

The id is random and is made the first time you export. It lets two files from the same install be recognised as such, and nothing else. A file you attach to a **public issue is public**, and the id links every file that carries it. To break that link, make a new id (the console's **New install ID** button, or `--reset-id` on the command line). The statistics themselves are not changed.

## How to send a file

1. In the console, open **Stats & costs › Stats** and find **Export statistics**.
2. Choose the period and press **Build the file**. The complete file is shown on the page, exactly as it will be saved.
3. Read it. Press **Download the file** to save it.
4. Send it yourself, for example by attaching it to an issue (remember that this is public) or by sending it privately to whoever asked for it. Nothing is sent for you.

On the command line:

```
masamong stats export --from 2026-09-01 --to 2026-11-15 --file stats.json
```

`--from` and `--to` are dates and are rounded to whole weeks (the default is the last 12 finished weeks); `--file` is where to write the file; without it the file is printed in the command's JSON result. `--reset-id` makes a new install id first.
