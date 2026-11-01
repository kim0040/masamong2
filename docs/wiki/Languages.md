# Languages

[한국어](https://github.com/kim0040/masamong2/wiki/언어-설정) | **English**

Masamong has three separate "languages".

| What | Set by | Affects |
|---|---|---|
| **Reply language** | The room's persona (`persona_ref`) → the server's `guilds.<id>.locale` → `i18n.default_locale` | Prompts, replies, tool descriptions, refusals, Discord card text |
| **Memory language** | `memory.languages` (default `[ko]`) | The search tokenizer, particles, pronouns and stop words, summary prompts |
| **Console language** | Console **Display → Language** | The admin console's text (Korean, English, Japanese, Simplified Chinese and Spanish built in) |

## Changing the reply language

The setup wizard asks which language the bot answers in (the console's language is suggested) and saves it as `i18n.default_locale`; change it later in Settings.

```bash
masamong channels configure --guild_id G --id C --persona_ref friendly_en   # this room in English
```

Built-in personas include `friendly_ko` (Korean, casual), `warm_ko` (Korean, polite), `friendly_en` (English), `friendly_ja` / `warm_ja` (Japanese, casual / polite) and so on, and you can add your own under `personas` ([Configuration reference](https://github.com/kim0040/masamong2/wiki/Configuration-Reference)). The bot's names are in `instance.bot_names`.

A room without a persona uses the default persona of its language: the server's `guilds.<id>.locale`, then `i18n.default_locale`.

| Language | Default persona | Voice |
|---|---|---|
| Korean | `warm_ko` | polite and warm (해요체), still "a friend of the room" |
| Japanese | `warm_ja` | polite and warm (です・ます), no casual speech to strangers |
| English, Spanish, Simplified Chinese | `friendly_<tag>` | one casual register (Spanish with *tú*) |

Otherwise it falls back to `friendly_<tag>`, then `friendly_en`, then `friendly_ko`. The casual presets `friendly_ko` and `friendly_ja` are still there to pick for a room (they appear as casual presets in the persona page). Built-in tone sets cover friendly, polite, brief and cheerful for Korean, English, Japanese, Simplified Chinese and Spanish; built-ins can be hidden (not deleted) from the console and restored later. The persona page follows the console/system language automatically, with a per-admin manual choice that is remembered, and marks which persona is the default.

**Polite or casual by default.** The polite default is the setting `i18n.polite_default` (**Settings → New rooms start polite**): on for an instance created now, and **off for an instance made before the setting existed**, so upgrading never changes the voice of rooms that never chose a persona; they keep answering as before until you turn it on. To keep the casual voice with the setting on, hide `warm_ko` / `warm_ja` (`personas.warm_ko.hidden: true`, or the hide button on the persona page); rooms then fall to `friendly_ko` / `friendly_ja`. A room that picked a persona explicitly is never affected.

**The same voice everywhere.** Morning fortune DMs and zodiac texts speak in the same register as the language's default persona (해요체 in Korean, です・ます in Japanese), and `/summary` posts use bold sub-headings and single-level bullets (no `#` headings or tables), which Discord shows as plain text. A room that picked the casual `friendly_ko` still gets its fortune in 해요체.

## Memory language

For rooms that mix languages (Korean and English, for example), list them all: `memory.languages: [ko, en]`. The search index is rebuilt in memory at start, so changing this needs no re-embedding.

## Adding a language

The bot's language pack, the operator and Discord text, and the console text are translated **separately**.

| What | File |
|---|---|
| Bot prompts and search rules | `internal/lang/packs/<tag>.json` (start from `en.json`) |
| Discord commands, result cards, errors | `locales/<tag>.json` (start from `locales/en.json`) |
| Console (Korean and English first-party; other languages as bundles) | `internal/transport/web/ui/scripts/strings.py` (then run `python3 scripts/strings.py`) |
| Console (other languages) | `locales/ui/<tag>.json` (see `locales/ui/README.md`). The console's language bundles are embedded in the program, so a new language shows up only after a rebuild (`make ui`, then build the programs) |
| KakaoTalk export markers | `internal/memory/corpus/markers/<tag>.json` (only for exports from KakaoTalk in another language) |

Keep in mind:

- Keep **placeholder names and counts** such as `{bot}` and `{name}` exactly.
- Do not translate the keys and types in the planner's JSON example, or tool IDs.
- Do not translate particles, pronouns, stop words or command words literally; choose what fits the language.
- Never send real conversations, user IDs or keys to an outside translator.

Checks:

```bash
cd internal/transport/web/ui && npm run test:i18n && npm run check -- --fail-on-warnings && cd -
make ui
go test ./internal/i18n ./internal/lang ./internal/transport/web
go test ./...
```

The full procedure is in the repository's [docs/tools-and-languages.md](https://github.com/kim0040/masamong2/blob/main/docs/tools-and-languages.md) (Korean).
