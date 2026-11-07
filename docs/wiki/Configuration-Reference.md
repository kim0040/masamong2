# Configuration reference

[한국어](https://github.com/kim0040/masamong2/wiki/설정-레퍼런스) | **English**

Settings live in one file, `config.yaml`, in the data folder, and every change is recorded in the database as a revision. Usually you change them in the console's **Settings** page or with commands (`channels configure`, `tools configure`, …). After editing the file yourself, apply it with `masamong config apply --file config.yaml --base_revision N` (validated → recorded as a revision → applied without a restart).

`--base_revision` is the revision your edit started from. Apply refuses to overwrite a newer one, so a change made meanwhile in the console, from another terminal or by a Discord admin command is never lost. Get the number from the same command you started with: `config show` prints `revision`, and `config diff` and `config validate` print `base_revision`. Without it, apply compares against revision 0 and answers `CONFIG_CONFLICT` ("expected 1 got 0" on a fresh instance); that is the cue to read the current revision and run it again.

```bash
masamong config show                      # current settings; "revision" is the number you pass below
masamong config diff --file new.yaml      # what would change (also prints base_revision)
masamong config validate --file new.yaml
masamong config apply --file new.yaml --base_revision 3
masamong config rollback --revision 12
masamong config changes                   # change history
```

In a script, take the number straight from the JSON:

```bash
rev=$(masamong config show --output json | python3 -c "import sys,json; print(json.load(sys.stdin)['output']['revision'])")
masamong config apply --file new.yaml --base_revision "$rev"
```

```powershell
$rev = (masamong config show --output json | ConvertFrom-Json).output.revision
masamong config apply --file new.yaml --base_revision $rev
```

> **Keys never go into the settings as values.** Use only references: `env:NAME`, `file:path`, `keychain:account` ([Security and privacy](https://github.com/kim0040/masamong2/wiki/Security-and-Privacy)).

## instance

| Key | Default | Meaning |
|---|---|---|
| `id` | chosen at setup | Instance name |
| `data_dir` | the data folder | Filled in automatically |
| `bot_names` | `[마사몽, masamong]` | Names people call the bot by; the first is used in prompts |
| `expected_bot_id` | `""` | Refuse to register commands when connected with a token for a different bot (protects against token mix-ups). The console fills it from Discord after saving the token (`masamong discord check --apply`) |

## admin

| Key | Default | Meaning |
|---|---|---|
| `web.enabled` | `true` | Turn the console on |
| `web.bind` | `127.0.0.1:8787` | Console address. **Loopback only**; use `masamong connect` or `web.hosts` below for remote access |
| `web.hosts` | `[]` | Names the console also answers to besides `127.0.0.1`/`localhost` (up to 8, no port or path): names a proxy on this computer forwards, usually the tailnet name for `tailscale serve`. The console still listens on loopback only and refuses unlisted names. Added in one click on Display & access |

## discord

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Connect to Discord |
| `token_ref` | `env:MASAMONG_DISCORD_TOKEN` | Bot token reference |
| `command_prefix` | `!` | Prefix for `!` commands |
| `register_commands` | `true` | Register slash commands automatically |
| `message_content` | `false` | Read ordinary messages (also turn on Message Content Intent in the portal) |
| `guild_ids` | `[]` | Servers to register slash commands in; empty means global |

## i18n

| Key | Default | Meaning |
|---|---|---|
| `default_locale` | `en` | Reply language when servers and rooms set none |
| `fallback_locale` | `en` | When a translation is missing |
| `bundled_locales` | `[en, es, ja, ko, zh-Hans]` | Languages to load |
| `timezone` | `Asia/Seoul` | The day boundary for limits, statistics and backup times. It must be an IANA name such as `Asia/Seoul`. The programs carry their own time zone database, so names work on Windows without Go or a system copy. An unknown name is not silently treated as UTC: `masamong doctor` and Home's attention list show it. If a room's stored `naive_timezone` is invalid, indexing uses the instance time zone and reports the room in its warnings; an invalid zone stored for a person or reminder is logged before its fallback is used |
| `polite_default` | `true` for new instances | Rooms without a persona start in the polite-but-warm voice (`warm_ko`, `warm_ja`) instead of the casual friend (`friendly_ko`, `friendly_ja`). An instance created before this key existed has none and **keeps the casual voice** until you turn it on: **Settings → New rooms start polite** or `polite_default: true` |

## providers

Outside providers, keyed by provider ID:

| Field | Meaning |
|---|---|
| `kind` | `openai_compat` (models), `kma`, `open_meteo`, `finnhub`, `exchangerate`, web search `tavily`, `linkup`, `exa`, `serper`, `brave`, `tinyfish`, `kakao_search`, link reading `jina_reader` |
| `base_url` | API address (any OpenAI-compatible host: OpenRouter, OpenAI, Gemini, your own server, …). It must be HTTPS. Plain HTTP is accepted only for `localhost` and loopback IP addresses (`127.0.0.0/8`, `::1`), for a model server on the same computer. Addresses with a username or password and look-alike hosts such as `localhost.example.com` are refused. A redirect to another scheme, host or port is not followed |
| `api` | `openai_compat` only: host kind `openrouter`, `openai`, `gemini`, `anthropic` (the Messages API, at any address), `compatible`. Empty means it is detected from `base_url`; set it when the address cannot tell (a test server, or another company's Anthropic-format endpoint) |
| `credential_ref` | Key reference |
| `daily_limit` | Calls per day (kept across restarts) |
| `disabled` | Turn off |
| `byok_only` | OpenRouter only, optional (unset = normal routing). Maps a model family (the part of a model name before `/`) to the one upstream provider its chat requests may use, with no fallback: `{anthropic: anthropic}` sends `anthropic/*` only to Anthropic. With your own Anthropic key added in OpenRouter (BYOK, set to never use shared capacity) those models then run only on your key; when it fails, the role's fallback model answers instead of OpenRouter credits. Spending counts what the upstream provider billed too. The console's OpenRouter card has a switch for Claude |

Default providers: `openrouter` (models), `kma` (Korea Meteorological Administration), `openmeteo` (weather without a key), `finnhub` (stocks), `exchangerate` (FX), `kakao`, `tavily`, `linkup`, `exa`, `serper`, `brave`, `tinyfish` (web search). Instances created before this list add them under **Connections → Add provider**. Link reading (`jina`, Jina Reader) works without a key even when not listed; add it as a provider to use a key.

After an upgrade, a committed provider or HTTP tool address that the stricter URL policy rejects does not stop the daemon. Masamong disables that provider or tool, logs the rejected setting and shows an attention item in `doctor`. Correct the address before enabling it again. New settings remain subject to the strict URL check and are rejected until valid.

The adapter handles what differs between host kinds:

| | OpenRouter | OpenAI | Gemini | CometAPI | NanoGPT |
|---|---|---|---|---|---|
| Reasoning control | `reasoning: {effort}` | `reasoning_effort` | `reasoning_effort` | `reasoning_effort` | `reasoning_effort` |
| Output length | `max_tokens` | `max_completion_tokens` | `max_tokens` | `max_tokens` | `max_tokens` |
| Key check | `GET /key` | `GET /models` | `GET /models` | `GET /models` | A 1-token request to a role model (its model list opens without a key) |
| Rerank | yes | no | no | no | yes (up to 1,024 documents a call, more in parts; its raw scores are mapped to 0–1) |
| Images | Chat API (modalities) | `/images/generations` | native `generateContent` | Chat API, a Markdown data URI in the text | `/images/generations` (switches and remembers when the chat API refuses) |
| Cost | in the response | official price list | official price list | reference price | the model list's prices (`?detailed=true`) |

A reasoning level a model does not take is changed before the request, to the nearest level it does take: the levels come from the model list (OpenRouter's `reasoning.supported_efforts`, NanoGPT's `reasoning_efforts`) or, where the list does not say, from the host's docs. GPT-6 Astra and GPT-6.1 Sol on OpenAI, and Gemini 3.x/4, have no `none`, so `none` is sent as their lowest level. On OpenAI's own host, GPT-6 Astra, GPT-6.1 Sol and GPT-6.1 Terra take tools only through the Responses API, which Masamong does not use yet: they answer without tools.

A used-up balance or spending limit (OpenAI `credit_balance_exhausted` and the spend limits, NanoGPT `*_usd_limit`, OpenGateway `budget_exceeded`, an OpenRouter 403 with `limit_source`) and a model the host cannot route (`model_not_found`, also with 503) are not retried: the fallback model answers at once. A rate limit is still retried after its wait. An OpenRouter 403 from a content filter goes to the fallback without marking the key as refused.

When **Fill roles** recommends OpenRouter models, it recognizes catalog entries with image output as image models and keeps them out of chat roles.

CometAPI and NanoGPT can be picked directly under **Connections → Add provider** (`providers add --host cometapi`) and have their own recommended-model rules (CometAPI's embedding is `qwen3-embedding-8b`, which the memory recipe was measured with). Models from providers without a price list use a **reference price**: the same model name (after the last `/`, without a `:variant`) is looked up in OpenRouter's public list (fetched once a day without a key), and image models are priced by image output (`image_output`). CometAPI charges about 80% of list price, so the reference price keeps the budget on the safe side. A model with no price anywhere raises a "model with unknown price" notice on Home, because the monthly budget cannot cap its cost.

**Anthropic (Messages API).** `api: anthropic` speaks Anthropic's native Messages API at any address: Anthropic's own (`https://api.anthropic.com/v1`) or another company's Anthropic-format endpoint (`https://<host>/anthropic`; `/v1` is added when missing, and a pasted `…/v1/messages` is trimmed). Requests go to `{base}/v1/messages` with `x-api-key` and `anthropic-version`. Hosts other than `api.anthropic.com` also get the key as `Authorization: Bearer`, because some of them read only that header. Effort is sent as `thinking` plus `output_config.effort` from the model's capabilities. They are read when the model list is refreshed and when you test the provider: the test shows its result first, then asks `GET /v1/models/{id}` once for each role model the list does not cover. A chat call never asks first. A model nothing is known about gets adaptive thinking; if the host refuses that, thinking with a token budget is tried, and only if that is refused too, no thinking field. A field the host refuses is dropped and remembered for every call to that provider for an hour. It is forgotten sooner when the model list says something new about the model, or when you edit the provider, save its key or test it:

| Effort | Model that can turn thinking off (Haiku 5.5) | Thinks between tool calls only (Sonnet 5.5) | Always thinks (Opus 5.5) | Budgeted thinking (Haiku 4.5) |
|---|---|---|---|---|
| `none`, `minimal` | `thinking: disabled` | `thinking: between_tools` | no `thinking`, effort `low` | `thinking: disabled` |
| `low` … `max` | `thinking: adaptive` + `output_config.effort` | the same | the same | `thinking: enabled` with a token budget (1024–16384) |

Thinking gets extra output room on top of the call's limit (`low` 1024 up to `max` 16384 tokens). Signed thinking blocks go back unchanged within a tool loop. Once an Anthropic-format model has answered in the loop, the tool list and the system prompt stay as they were too: a tool closed during the turn stays on the list and answers "not available now" if called. When a fallback model or host takes over, the thinking blocks another model or host wrote are left out. `temperature` goes only to the Claude models listed as taking it (Claude 4.6 and earlier, in `hosts.json`), on calls without thinking. Every other model on an Anthropic-format host, another company's included, runs at its default temperature. A model with budgeted thinking (Haiku 4.5) that takes over a tool loop from another model or host answers that round without thinking: it refuses a round whose last answer has no thinking block. A reached spend limit or an empty credit balance counts as quota: the role's fallback answers. Prompt caching is not sent yet. The cost counts cache reads at 0.1× and cache writes at 1.25× the input price if a host reports them. A host without an Anthropic-format model list still passes the connection test ("list unavailable"): type the model ID in **Change**, and the key is proven by a one-token answer once a role uses the host. A refusal (`stop_reason: refusal`) goes to the role's fallback.

When a model rejects an option (e.g. Gemini 3.8 Flash rejects `reasoning_effort: minimal`, OpenAI reasoning models reject `temperature`), the request is sent once more without it and that shape is remembered for the model. When the planner or reply model fails (model missing, overloaded, timeout, ran out of output while thinking), the other one gets the same request once; there is no setting for this. Official prices are in `internal/providers/openai/hosts.json` with the date they were checked; prices entered by the operator always come first.

## models

A model per role. Roles: `response`, `planner`, `summary`, `embed`, `rerank`, `image` (the image tool; empty makes image generation answer "not configured"), `vision` (optional: the model that reads pictures members send). When `vision` is set, it describes every picture and the response model gets the description as text, even if the response model takes pictures itself; if the vision model fails or refuses, the picture goes the usual way. When it is empty: the response model gets the picture directly if it takes pictures, otherwise the planner model, then the response model, describe it. `image` and `vision` may be empty; the role table has a **Clear** button for them.

| Field | Meaning |
|---|---|
| `provider` | ID in `providers` |
| `model` | Model name |
| `dimensions` | Embedding dimensions (`embed` only). Default `1024`; `0` = the model's full width |
| `price_in_per_mtok`, `price_out_per_mtok` | USD per million tokens, used when the provider reports no cost |
| `timeout_seconds` | Time limit for one call. 0 = default (`planner` 30, `response` 90), otherwise 5–600. Past it, or on an empty reply, the other chat model takes over. Console: **Connections → Models → prices** |
| `effort` | The chat roles (`response`, `planner`, `summary`, `vision`). How long a reasoning model thinks before it answers: `none`, `minimal`, `low`, `medium`, `high`, `xhigh` or `max`. Empty = `low`. Precedence is below the table. Console: **Connections → Models → prices** |
| `fallbacks` | The chat roles. Up to 3 models (`provider`, `model`, optional `price_in_per_mtok`, `price_out_per_mtok`, `timeout_seconds`, `effort`) asked in order when the role's model fails. A fallback's `effort` replaces the role's on that model; empty keeps the role's. Console: **Connections → Models → Fallbacks** |

**Thinking effort, in order of precedence:**

1. A call of the role asks for the role's `effort` (empty: `low`). A fallback with its own `effort` asks for that instead.
2. Calls the code makes thinking-free on purpose ask for `none` while the role's `effort` is empty: the name-call, reference and follow-up checks of the planner, the one-line memory summary and summary-role write-ups, and fortunes. They stop at a short output limit that thinking would use up. Once the role's `effort` is set, these calls think too, at most at `low`, and get extra output room for the thinking. The summary role's own `effort` is the level of its job and applies as set. A fallback without its own `effort` follows the same rules for the role it answers.
3. The level is then fitted to the host and model. A model whose list says it does not reason gets no field. A level the list says the model lacks becomes the nearest level below (a clamp is logged once). `xhigh` and `max` become `high` on Gemini and on hosts that refuse them (learned after one refusal). OpenAI and OpenRouter take `xhigh`, and `max` becomes `xhigh` there.

What the console shows comes from the model lists: **Thinks** or **No thinking** when the list says (Anthropic's capabilities, OpenRouter's `supported_parameters`, NanoGPT's detailed list, Gemini's native list), **Not known** otherwise (OpenAI's list, CometAPI). Levels a model does not take are greyed out. A model that thinks with a fixed budget (Claude Haiku 4.5) has no effort field, but `low` and up are budgets of their own, so every level can be picked.

On Anthropic, `low` now means adaptive thinking at effort `low`. The model may think a little on easy questions, so answers can use somewhat more output tokens and time than before, when Claude did not think at all. Its thinking room is the smallest, 1024 tokens. Set `none` to turn thinking off.

With a provider that has no rerank (OpenAI, Gemini) in `rerank`, or a rerank provider without a key, memory search answers with the fused ranking and no rerank (the console marks it in the role table). Each role can use a different provider — for example chat and embeddings on Gemini, only rerank on OpenRouter. The role table's **Change** button lists the chosen provider's models (pick another provider in the same dialog) with badges for picture input and tools, context, and input/output/cached prices per million tokens; search, sort by price or context, and for `vision` only models that take pictures are shown when the list says (a name-based list, like OpenAI's or Gemini's, does not, so all are shown with a note). Models whose list says "no tools" are marked for `response` and `planner`; where a list has no prices the console says "No price info".

**Fill with recommended models** on a provider's card under **Connections** picks the newest stable model for each role from that provider's list and changes only the roles you check (the picking rules are in `hosts.json` too; for OpenRouter there is a rule for each of the default roles). When none of the models the provider returned fits a rule, there is nothing to fill: the answer succeeds with an empty `picks` list and the dialog shows only the budget alternatives below (use one of them, or pick by hand). Only when there are no budget alternatives either does it fail with a reason ("No returned models match the supported recommendation rules."); pick the models by hand then. The same dialog also lists up to three **budget alternatives** per role (chat roles need a chat model with at least 32k context; `response`/`planner` need tool support unless the list does not say; `vision` needs picture input, which OpenAI's and Gemini's name-only lists do not say, so for those `hosts.json` names the models known to take pictures; `embed` an embedding model; `image` a picture-writing model), ranked by (3×input + output)/4 per million tokens, with models of the families the picking rules name for that role first and one model per family (the cheapest of its newest generation, that is, of its newest major version), so the top three are three families where the list has them. Prices are read as the bot is billed: the provider's list, then the published price table, then the public reference list (the source is shown); models with no known price, variants such as `:free` and `:batch`, `…-latest` aliases and preview or experimental models are left out. **Use this** sets that one role (the embedding model is not offered while memory is indexed: change it in the role table, then re-index from **Memory blocks**). If memory is already indexed, embedding is left out by default; checking it shows how much would be re-indexed and the estimated cost (`include_embed`, `reindex`; see the repository's `docs/memory-operations.md`).

Changing the `embed` **model** means re-embedding memory (the console shows the estimated cost). The previous model's vectors are kept in a cache, so switching back costs nothing.

The default `dimensions` is **1024** (since 2026-10-13). Measured on three real chat histories, it searched as well as 4096 with a reranker, used 21–27% less memory, and was just as fast and cheap (embedding prices are per token). Only models known to be trained for cutting (Matryoshka) are cut: the list with each model's full width is `embed_widths` in `hosts.json` (qwen3-embedding 8b/4b/0.6b, OpenAI text-embedding-3 large/small, gemini-embedding-001, gemini-embedding-2). Any other model always keeps its full width. Choosing a listed embedding model whose full width is above 1024 sets 1024; **Fill with recommended models** proposes the full width instead when the result would have no reranker (without one, 1024 found 5 of 120 conversation-flow questions fewer), except on a small host (hostprofile `small`), which keeps 1024. A `dimensions` value you wrote yourself is never changed automatically.

**Lowering** `dimensions` (e.g. 4096 → 1024, from the console's **Connections** or `masamong models embed-width`) keeps the front part of each stored vector in place at the next load, at no cost; prepared memory blocks stay ready and stay attached to their channels, and vector memory and disk shrink by the same ratio. The console offers 1024 and 2048 below the full width: going under 1024 would also change the message-vector space and re-embed every message. **Raising** it again means re-embedding everything (the console shows the estimate first). Only the chosen width is stored; to widen later, memory is embedded again from the original messages.

## limits

| Key | Default | Meaning |
|---|---|---|
| `profile` | `lite` | Profile name |
| `monthly_budget_usd` | `"10.00"` | This month's model spend cap; replies stop past it. The Usage page or `masamong usage reset` can restart this month's count from now without changing daily history or provider usage and key limits. Months are counted in UTC (in Korea a new month starts at 09:00 on the 1st) |
| `per_request_budget_usd` | `"0.03"` | **Budget per reply.** Once a reply has spent this, its remaining tools and memory search are skipped; the reply itself (and the other chat model standing in when one fails) still runs. A step that alone costs more (a picture) is not refused, only what comes after it |
| `max_external_attempts` | `6` (1–20) | **Outside calls per reply** (model calls, tools, memory search). Past it, the remaining tools and memory search are skipped as above. The skipped steps show in the turn record |
| `budget_alert_percent` | `80` | When the console starts warning about the budget |
| `max_concurrent_turns` | `8` (1–64) | **Replies running at the same time.** [Concurrency and performance](https://github.com/kim0040/masamong2/wiki/Concurrency-and-Performance) |

Model calls reserve the call count at their start and add cost at completion. With a per-reply money cap, optional work waits for models in flight to finish before its limits are checked; the wait follows cancellation. Money-spending tools also finish before the next admission. Different tools run together (at most four); calls of the same tool run in model order. Provider in-flight caps are shared across configuration reloads by provider kind, base host and credential hash; they are not requests-per-second or requests-per-minute limits.

## memory

| Key | Default | Meaning |
|---|---|---|
| `capture` | `false` | Store room messages as memory (the room's `capture` must be on too) |
| `embed` | `true` | Background indexing |
| `retrieve` | `auto` | `off` · `auto` · `hybrid` · `rerank` ([Memory blocks](https://github.com/kim0040/masamong2/wiki/Memory-Blocks)) |
| `summaries` | `true` | Snippet summaries (most of the cost) |
| `message_vectors` | `true` (on when unset) | **Message-level search.** Also embeds each eligible message of a closed snippet on its own (at least 6 letters or numbers; laughter, emoji and punctuation do not count) and adds the closest messages' snippets as one more search source, so a one-line fact inside a long snippet is found. MRL models store up to 512 dimensions per message, about 0.5 KB of int8 vector payload plus memory; models outside the MRL catalog use the installation's actual leaf width (full width when no explicit width is set) and may use more. The message space contains the actual numeric width; changed leaf widths trigger re-embedding, and messages wait until a leaf width is known. Vectors are not duplicated in the embedding cache; only messages with at least 6 letters or numbers are embedded, roughly a third to 60% of a real history. On by default after measurement (R@5 on the no-answer questions 65% to 75%, flow questions unchanged, under one cent of indexing cost for 30,000 to 40,000 messages). Set `false` to turn it off; while off, search is unchanged. Nothing is embedded at the moment you update. A width change is rebuilt on the next index pass, block prepare or `memory reindex`, within `index_budget_usd`; while rebuilding, search has fewer `message_dense` candidates, like message vectors were partly off (the doctor shows `pending_messages:N` and the width). Before turning it back on, the console estimates messages, tokens, cost, time and vector payload size ([Memory blocks](https://github.com/kim0040/masamong2/wiki/Memory-Blocks)) |
| `naive_timezone` | `Asia/Seoul` | Time zone for times without one (KakaoTalk) |
| `index_budget_usd` | `"1.00"` | Cost cap of one background indexing run |
| `turn_budget_usd` | `"0.05"` | Rerank headroom for one conversation turn |
| `index_interval_min` | `10` | Background indexing interval (minutes) |
| `index_workers` | `8` | Embedding and summary requests one indexing pass sends at once (1–256). Lower it if the embedding host often answers 429; raise it to finish a large first index sooner |
| `languages` | `[ko]` | Memory languages (tokenizer, particles, summary prompts) |
| `agent_prompt` | `""` | Extra instructions for every room |
| `leaf_split` | `time_gap` | Snippet split: `time_gap` (8 messages / 30 minutes) or `fixed` (8) |
| `person_aliases` | `{}` | Link one person's different names (set by people) |
| `backfill_days` | `0` (= 7) | On reconnect, how many days back to fill in messages missed while off. `-1` is off, at most 30 |
| `backfill_max_messages` | `0` (= 4000) | Most messages read per room in one fill |
| `derived_budget_usd` | `"3.00"` | Monthly cost cap of background room memory (episodes and room state). `"0"` stops it |
| `day_boundary` | `"05:00"` | Where one day's talk ends for room memory (`HH:MM`, `i18n.timezone`): late-night chat stays one day |
| `episodes` | `on` | `on` · `off`. Replies get up to 3 related room episodes (about 1,200 characters), each with a few of its lines read again as they stand now |
| `state` | `on` | `on` · `off`. Replies get the room's live topics linked to the speaker's conversation (about 500 characters) |
| `cards` | `off` | `on` · `off`. Person cards: a fact someone said about themselves on 2 or more days, in 2 or more conversations (or asked to have remembered), kept per server. Replies get the speaker's card and up to 2 others'. Members see theirs with `/mymemory` and delete it with `/forgetme` |

## chat

| Key | Default | Meaning |
|---|---|---|
| `tool_mode` | `loop` | How a reply picks its tools (`loop` is the default since it came out more accurate on the live regression sets of 2026-10-11). `planner`: the memory planner's plan names them. `shadow`: the planner still picks, and the native tool loop runs beside it only to record what it would have called (`tool_shadow` in the turn record). `loop`: the tool loop picks with native tool calling (at most 3 rounds, every call through the same checks and limits) and the planner only decides the memory search. A model that takes no tools falls back to `planner` for that reply. Also on the Tools page |
| `loop_final` | `tools` | When the tool loop ends without asking its model once more. `tools`: in a short message (six words or fewer), when the round's calls are all of tools whose one result is the answer (`weather.query`, `places.search`) and all found what they looked for. A failure, an empty or partial result, or a quote answered by a web search still asks. `search`: also when `search.web` found at least 3 results with text, unless the model searched with `depth: deep` or the message has a link. `off`: always asks. A request whose next call depends on a result ("check the weather, then find somewhere to go") can end at its first tool; set `off` if those are common |
| `vision` | `auto` | Image understanding. `auto`: reads only the pictures sent when someone calls the bot (including those on the message they reply to or forward, at most 3 per reply). A picture posted just before a separate call (within 2 minutes), or sent alone within 45 seconds after a call that had none, counts as part of that call. An answer model that takes pictures sees them; otherwise another chat model describes them in 2–4 sentences inside the message. `off`: pictures are not read. No other pictures in the room are looked at, and pictures are not stored. Also on the Tools page |

## dm

```yaml
dm:
  allow: members
  per_user_per_day: 30
  per_day: 100
```

| Key | Default | Meaning |
|---|---|---|
| `allow` | `members` | Who may chat in DMs. `members`: operators or members of a server the bot currently belongs to and is allowed to serve. `operators`: operators only. `off`: DM conversation disabled. Globally blocked people receive no replies |
| `per_user_per_day` | `30` | Daily DM conversation turns per person. `0`: default, `-1`: unlimited, otherwise `1`–`10000` |
| `per_day` | `100` | Daily DM conversation turns across the instance. `0`: default, `-1`: unlimited, otherwise `1`–`100000` |

DM conversation needs no mention; split messages are merged into one turn. `!` and slash commands still work when DM conversation is disabled; an unanswered `!` tool command continues as conversation when DM chat is allowed. DM quotas apply alongside `tools.chat.respond` limits, the monthly budget and the concurrency cap; refused requests consume no quota. Counters and refusal notices (once per person per day for access and each quota kind) survive restarts and reset at midnight in `i18n.timezone`.

Membership uses Discord state then REST, with no additional privileged intents (REST positive cache 10 minutes, negative cache 1 minute). DMs use the instance default persona and the global tool settings of an unconfigured room; like a server room, a reply follows the language of the message. Recent exchanges are stored only as short-term DM context; DMs do not search server or room memory, capture or index memory blocks, analyze room style, offer threads, react with emoji, open follow-up windows or initiate participation. Retention and personal memory deletion or consent withdrawal also apply to DM context.

## guilds

Server ID → server settings; channel ID → room policy.

```yaml
guilds:
  "123456789012345678":
    locale: en
    channels:
      "234567890123456789":
        respond: mention          # always | mention | command | off
        persona_ref: friendly_en
        rules: "Never mention sources"
        capture: true
        memory_use: "on"
        memory_view:
          blocks: [kakao:friends]
        tools: {search.web: false}
```

### Room conversation settings

Each room's `channels.<id>` entry also holds how a conversation with the bot goes there (all on the Room page, **Behavior → Conversation**). Unset keys take the first value listed. A thread inherits its parent room's settings, memory rooms, pause and capture unless it has its own entry; a private thread is a room of its own.

| Key | Values | Meaning |
|---|---|---|
| `name_call` | `addressed` · `always` · `off` | A message that only says the bot's name (no mention, no reply to it). `addressed`: a small classifier tells "said to the bot" from "about the bot" and answers only the first (unsure means no). `always`: every message with the name is a call. `off`: the name alone never calls |
| `attribution` | `on` · `structural` · `off` | Which recent lines a reply sees. `on`: the caller's own conversation plus lines a classifier finds the call pointing at. `structural`: the caller's conversation only. `off`: every recent line |
| `conversation.follow` | `off` · `reply_only` · `window` | **Experimental.** Carrying on without a tag. `reply_only`: replies to the bot's message. `window`: a scored window after each answer. Decisions are recorded even while `off` |
| `conversation.follow_window_sec` | `0` (= 90, at most 600) | How long the window stays open; a question from the bot adds 60 |
| `conversation.follow_max_untagged` | `0` (= 12) | Untagged answers in a row before a tag is needed again |
| `conversation.thread` | `offer` · `auto_task` · `off` | A button under the reply offers a thread for a long one-on-one exchange. Anyone in the channel may press it, and whoever presses it joins the conversation (only the person asked can press **Keep it here**). `auto_task` (experimental) also opens one by itself for a task that used 3 or more tools. `off` turns thread continuing off in that room: new offers and automatic opening stop, a button shown before the switch opens nothing (only the person who pressed is told), and in a thread the bot already opened it no longer answers without a tag but follows the room's ordinary reply rules. Threads people made themselves are unaffected. There is no server-wide default, so it is set per room. When offers keep going unaccepted the room rests: after two ignored or declined offers in a row with no thread opened, 6 hours; each further ignored offer doubles it (12 h, 24 h …) up to 7 days; one opened thread starts the count over. The rest is read back from the conversation log (`conversation_log`), so it survives a restart, and the room's activity log in the console shows "Resting: recent offers were ignored" (`skip_backoff`) once per conversation |
| `conversation.thread_follow` | `participants` · `all` | Who the bot answers without a tag in a thread it opened: the person asked, whoever pressed the thread button and anyone who called the bot there, or everyone with `all` (lines aimed at someone else are left alone) |
| `conversation.thread_idle_close_min` | `0` (= 30) | Minutes of quiet before the bot ends its part in its thread |
| `conversation.thread_recap` | `plan` · `always` · `off` | A short recap in the parent room when a thread ends: only when a plan or decision came out of it, always, or never |
| `conversation.reply_shape` | `bubbles` · `trim` · `plain` | `bubbles`: two or three short paragraphs go out as separate messages a moment apart, and a reply far longer than a chat line shows its first part with a "more" button. `trim`: only the "more" button. `plain`: as written |
| `conversation.banter` | `off` · `shadow` · `on` | **Experimental.** When someone talks about the bot in fun, one light line or an emoji back. Never for a real complaint; at most 2 lines and 4 emoji an hour; a 👎 stops lines for 2 hours. `shadow` only records what it would have done |
| `logging.level` | `ops` · `errors` · `off` | How much of a turn the console keeps: `ops` the whole turn, `errors` the words of failed turns only, `off` none (ids, timing, tools and costs stay). Remarks only about the bot follow the same level, with the name-call decision (`about_bot`, `unsure`, …) beside them |

Every decision of these features (name calls, follow-ups, thread offers, banter) goes to a body-free log (`conversation_log`, kept for `retention.turn_days`) that the Conversation card summarizes, to tune the defaults from real use.

## tools

Per-tool settings; see [Tools and limits](https://github.com/kim0040/masamong2/wiki/Tools-and-Limits).

```yaml
tools:
  chat.respond:
    limits: {per_user_per_day: 200, cooldown_sec: 3}
  weather.query:
    defaults: {region: Busan}
    aliases: [날씨, w]
  search.web:
    enabled: false
```

## features

Feature group switches: `weather`, `disasters`, `finance`, `search`, `polls`, `images`, `fortune`, `notices`, `summaries`. `false` turns off every tool in the group.

`disasters` (KMA) and `notices` (Korean universities) only cover Korea. A new instance whose wizard language is not Korean and whose browser time zone is not `Asia/Seoul` starts with both `false`, and on `UTC` if the browser sent no time zone. Existing instances keep their settings.

## disaster_alerts

Posts earthquakes and warnings in rooms without being asked. Needs a KMA key and a Discord connection.

```yaml
disaster_alerts:
  enabled: true                 # master switch (on by default)
  earthquake_min_domestic: 4.0  # magnitude threshold in Korea (0–10)
  earthquake_min_overseas: 7.0  # magnitude threshold abroad
  warnings: true                # weather warnings (land warnings nationwide)
  warning_regions: [부산, 서울] # advisories only for regions whose names contain these
  marine: false                 # marine warnings
guilds:
  "<server ID>":
    channels:
      "<channel ID>":
        disaster_alerts: "off"  # off in this room only (empty = on)
```

## personas

Your own personas, in addition to the built-in ones (`friendly_ko`, `friendly_en`, `warm_ko`, `warm_ja`, …). A room without a persona uses the default of its language (the server's `guilds.<id>.locale`, then `i18n.default_locale`): with `i18n.polite_default` on, `warm_ko` for Korean and `warm_ja` for Japanese, otherwise `friendly_<locale>`. `warm_ko` and `warm_ja` are polite (해요체, です・ます); `friendly_ko` and `friendly_ja` are the casual presets and stay pickable. A built-in can be hidden from the console's lists without deleting it. Hiding `warm_ko` is also how to keep a Korean room on the casual voice:

```yaml
personas:
  calm_en:
    locale: en
    label: Calm
    style: "Answer briefly and calmly, casually."
  friendly_en:
    hidden: true   # hide a built-in; remove the entry to restore it
```

## backup

| Key | Default | Meaning |
|---|---|---|
| `schedule` | `weekly` | `off` · `daily` · `weekly` |
| `hour` | `4` | Hour of day (`i18n.timezone`) |
| `weekday` | `0` | Day for weekly backups (0 = Sunday) |
| `keep` | `4` | Automatic backups to keep |

## retention

Over months, the operational records each conversation leaves (turn details, diagnostic traces, usage counters, the question embedding cache) keep growing the database. Once a day the **old usage cleanup (`usage_gc`)** task deletes records older than the periods below, a little at a time. Units are days; `0` means the default, `-1` keeps forever. **Memory (collected messages, snippets, summaries) is never deleted by age.**

| Key | Default | What |
|---|---|---|
| `turn_days` | `400` | Turn rows (the summary used by statistics and reports) |
| `turn_detail_days` | `30` | Turn details (message, reply, evidence); after that only the summary row stays. Each channel's conversation context (the recent turns the bot reads to carry on) is also removed after this |
| `trace_days` | `30` | Diagnostic traces |
| `audit_days` | `400` | Change history and room setting history |
| `usage_days` | `14` | Per-user and per-room tool counters (limits need only today's). Daily totals per tool are kept for `turn_days` |
| `image_days` | `14` | Files made by the image tool (`<data-root>/images/`) |
| `query_cache_days` | `7` | Question embedding cache (stored at the configured dimensions: about 8 KB per question at 4096, 2 KB at 1024; the same question rarely comes again). Snippet and summary vectors kept from a previous model are not deleted, because they make switching back free |

```yaml
retention:
  turn_detail_days: 14
  trace_days: 7
```

**Jobs → Scheduled tasks** in the console shows the last run and how many rows it deleted, and runs it now (`masamong schedules run --id usage_gc`). The first run is 5 minutes after the daemon starts, then every 24 hours.

## participation

The chiming-in master switch and limits. Each room turns it on separately with `guilds.<g>.channels.<c>.participation` (`off`, `collect`, `shadow`, `conservative`; `collect` records candidate moments and what followed without any model call, and keeps no message text); the stricter of the two wins. The numbers are starting guesses to tune from the shadow log.

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Master switch |
| `mode` | `off` | The most a room may use |
| `wait_sec` | `0` (= 45) | How long a question must go unanswered before judging |
| `cooldown_sec` | `0` (= 180, min 120) | Before speaking again in a room |
| `per_hour` · `per_day` | `0` (= 3 · 10) | Times it speaks per room |
| `judge_per_hour` | `0` (= 12) | Judgments (model calls) per room |
| `quiet_start` · `quiet_end` | none | Hours not to chime in (`i18n.timezone`) |
| `min_score` | `0` (= 0.7) | Judgment score threshold (a ranking score, not a probability) |
| `shadow_drafts` | on | In shadow mode, also write and log what it would say (one more model call per judgment) |

## access

Discord admin commands (`/admin`, `!관리`), people to ignore, and the servers the bot works in. Also changed under **People & access** and **Servers & rooms → Server approval** in the console.

| Key | Default | Meaning |
|---|---|---|
| `operators` | `[]` | Discord user IDs that may use admin commands in any server, including servers waiting for approval |
| `guild_admins` | `true` | Whether server owners and members with Manage Server may use admin commands in their own server |
| `blocked_users` | `[]` | User IDs whose messages, commands and buttons the bot ignores in every server. Their messages are still stored as context (they can stop that by withdrawing memory consent) |
| `server_approval` | `true` | When on, the bot works only in approved servers (`servers`) and servers with configured rooms; newly invited servers store and answer nothing until approved |
| `servers` | `[]` | Discord server IDs approved by the operator. Servers the bot was in before this feature are added once on first connection |

Per-server blocks are stored in `guilds.<server ID>.blocked_users`. That server's managers add them with `!관리 차단 @name`, and they are lifted under **Servers & rooms** in the console.

**Stricter memory per server** (`guilds.<server ID>.privacy`, all `false` by default; the bot is for friends' servers, where remembering freely is the point):

| Key | Default | What it does |
|---|---|---|
| `own_card_only` | `false` | A reply uses only the caller's person card, not those of the others in the conversation |
| `no_hearsay` | `false` | What one person said about another stays out of the room memory a reply sees (episode items, episode titles that name such a person, and live topics) |
| `no_sensitive` | `false` | A classifier checks facts before they become card items, and "remember this" refuses sensitive ones (health, romance, money, religion, politics, family or legal trouble). The check is paid from `memory.derived_budget_usd` and cached per fact. Episodes and live topics are not classified |
| `card_channel_only` | `false` | A card item is used only in the channel (and its threads) where all of its evidence was said |

Whatever these say, servers never share people, deleting a message or withdrawing consent removes what was derived from it, and logs hold no message text or IDs. Anyone can also withdraw **Notes about me** (`/privacy withdraw profile`): their cards are deleted, none are made again, room style notes quoting them stop being used, and their lines stay out of the room style analysis. Facts they saved with "remember this" are conversation memory and stay.

**Server log** (`guilds.<server ID>.log`): what happened in a server goes to its log channel, as the previous Masamong posted to a channel named `logs`. Set it under **Servers & rooms → Server log**, where **Send a test entry** checks that it arrives (CLI `serverlog test --guild_id G`).

| Key | Default | What it does |
|---|---|---|
| `channel` | unset (off) | The log channel's ID, or `auto` for the server's text channel named `logs` when there is one. Unset or `off` = no log: it is opt-in, so a server's existing `#logs` channel never starts receiving posts unasked |
| `deletes` | `true` | A deleted message: who wrote it, in which channel and when. A bot's messages and the log channel's own are left out, and many deleted at once become one entry. The author comes from the gateway's cache of recent messages (100 per channel) or from the stored messages; otherwise it says unknown |
| `deleted_text` | `true` | Also post what the deleted message said and its attached file names (when Masamong saw it recently or stored it). It is still removed from Masamong's memory; the text stays only as a post in the log channel. `false` posts who, where and when only |
| `problems` | `true` | A reply that failed, a tool such as search that failed in a reply because a service let it down (the answer went out without it), a reminder, scheduled task or disaster alert that could not be posted, and a channel where the bot lacks permission to post. The same problem in the same channel is posted once per 15 minutes |

A server gets at most 20 entries per 10 minutes; the next entry says how many were left out. Nothing is posted while replies are paused everywhere, or for a server waiting for approval. If Discord refuses the log channel, the server's log rests for 30 minutes.

## alerts

When something goes wrong, operators (`access.operators`) get a Discord DM. Switch and test it under **Maintenance → Operator alerts**.

| Key | Default | Meaning |
|---|---|---|
| `discord` | `true` | Alert operators by DM. Nothing is sent without operators |
| `min_severity` | `warn` | Which problems to send: `warn` (warnings and up) or `danger` (serious only) |

- Sent: items **new** in the console's notification list (Home, notifications), e.g. a failed scheduled backup, a rejected key, the budget at 80% or used up, failed jobs, the bot losing permissions, servers waiting for approval, an automatic restart after a crash
- Not sent: things operators did themselves (pause all, a pending restart), Discord disconnects (no DM can go out then), informational items
- Checked every 5 minutes; only new items are sent, grouped in one message. The same problem is not sent again, nor one that disappeared and came back within an hour; a serious problem lasting more than a day is sent once more.
- Messages contain only the problem and the instance name, never conversation text.
- If an operator blocks DMs from server members, delivery fails and the console shows why.

## updates

| Key | Default | Meaning |
|---|---|---|
| `check` | `true` | Ask GitHub once a day whether there is a new release. Only the program name and version are sent; nothing is installed automatically. Development builds (`-dev`) do not check. A source build (version from `git describe`) checks too, but updates only by installing again |

A new version shows under **Maintenance** and in the notifications (info).

## web_search

| Key | Default | Meaning |
|---|---|---|
| `order` | (none) | The order the web search tool uses search services in (provider names). Empty uses the services with keys in the order Tavily → Linkup → Exa → Serper → Brave → TinyFish → Kakao |

When a service errors or finds nothing, the next one is tried. Results record which service found them (`provider`) and which were passed over (`passed_over`). Reordering with the arrows under **Tools → Web search** in the console is easier.

## storage.yaml (separate file)

Storage settings needed before the database opens. Only SQLite is used now (`backend: sqlite`, or no file); `backend: tidb` is refused. See [Storage and backup](https://github.com/kim0040/masamong2/wiki/Storage-and-Backup).

## Environment variables

| Variable | Meaning |
|---|---|
| `MASAMONG_DATA_ROOT` | Default data folder |
| `MASAMONG_TRY_ROOT` | The separate test data folder for `make try` / `start-test.command` (default `~/.masamong-try`) |
| `MASAMONG_OPENROUTER_KEY`, `MASAMONG_GEMINI_KEY`, `MASAMONG_OPENAI_KEY`, `MASAMONG_COMETAPI_KEY`, `MASAMONG_NANOGPT_KEY` | Model keys (default references of providers added in the console) |
| `MASAMONG_DISCORD_TOKEN` | Discord bot token (default reference) |
| `MASAMONG_KMA_KEY`, `MASAMONG_FINNHUB_KEY`, `MASAMONG_EXCHANGERATE_KEY` | Weather and market data keys |
| `MASAMONG_TAVILY_KEY`, `MASAMONG_LINKUP_KEY`, `MASAMONG_EXA_KEY`, `MASAMONG_SERPER_KEY`, `MASAMONG_BRAVE_KEY`, `MASAMONG_TINYFISH_KEY`, `MASAMONG_KAKAO_KEY`, `MASAMONG_JINA_KEY` | Web search and link reading keys (Jina works without one) |
| `MASAMONG_DB_BACKEND` | Storage (`sqlite` only; TiDB is disabled) |
| `MASAMONG_NO_BROWSER` | `open` does not open a browser |
| `NO_COLOR`, `CI` | Command output as JSON |
