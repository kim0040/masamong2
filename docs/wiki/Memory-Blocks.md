# Memory blocks

[한국어](https://github.com/kim0040/masamong2/wiki/기억-블록) | **English**

Masamong remembers past conversations and brings them back naturally — "we said we'd go there that time", "that movie we talked about last month". Memory is kept in separate **memory blocks**, and you decide which blocks each room may use.

## Where memory comes from

| Source | How it gets in |
|---|---|
| Live Discord conversation | Collected in rooms where both the global switch (`memory.capture: true`) and the room's `capture: true` are on |
| KakaoTalk chats | Upload a KakaoTalk "export chat" CSV in the console and index it |
| Discord raw archive | Index an archive of original messages with `imports start --source raw` |

Only messages written by people, with a known time, are stored. Uploaded files are read, never changed, and importing the same file twice stores nothing twice (each message has a unique observation ID).

## Importing a KakaoTalk CSV (console)

Choose a CSV under **Memory blocks (`/blocks`)** and indexing goes through these steps:

1. **Check and preview** — format, room name, time zone, date range, message count, duplicates. UTF-8 (with or without a byte-order mark) and CP949 (re-saved from Excel) are read, with Windows (CRLF) line ends and quoted multi-line messages, and rows with broken quotes are repaired. The file is read as a stream, not loaded whole, and the preview stops when you close it.
2. **Name the block** — a recognizable name and a note
3. **Estimate** — expected time, model cost, and the most you allow it to spend
4. **Index in the background** — split into snippets (leaves) → embedding vectors → summaries (if on). Watch progress, time left and cost; cancel any time or retry only what failed. A restart of the service continues where it stopped.
5. **Attach to rooms** — on `/blocks?view=channels`, connect the block to the Discord rooms that may use it.

### Size limits and interruptions

Import is bounded so a huge or odd file cannot exhaust memory.

- **Limits**: a file may be at most **1 GiB**, one message field at most **64 KiB** and one logical row at most **256 KiB**. A file over a limit is refused with a message that names it (a console upload over the limit is refused and its partial file removed). Split a larger export by date and import the parts.
- **Retrying an import**: a Kakao row's stable ID uses its room, timestamp, speaker, text and occurrence number among identical rows at that timestamp. Identical messages are kept as separate rows, even when another message falls between them. Unchanged rows are recognized after edits to an export; stored aliases also recognize rows imported before this ID scheme and make CSV and offline-raw imports idempotent. If one timestamp group contains too many distinct messages to track safely, the import asks you to split the export by room and date range.
- **Batches**: messages are written in batches of up to 256 messages or about 2 MiB. Each batch is all-or-nothing: if an import fails or is cancelled, the batch in progress is rolled back and the batches already written stay.
- **Retrying**: importing the **same, unchanged file** again continues safely. Every message has a stable ID, so what was already stored is skipped and only the rest is added. The result reports how many were committed and how many skipped (`atomicity: batch`, `resume: same_file`). A file that changed in between is a different file; check the preview.
- **Cancelling** an import or a preview takes effect promptly, even while the database is busy with live messages.

## From the command line

```bash
# 1. Import a KakaoTalk CSV
masamong imports start --source kakao_csv --path friends.csv --room kakao:friends --label "Friends" --timezone Asia/Seoul

# 2. See the cost and time first (dry run)
masamong memory blocks prepare --source_room kakao:friends --dry_run true

# 3. Start indexing in the background with a budget
masamong memory blocks prepare --source_room kakao:friends --budget_usd 0.50

# 4. List blocks and their IDs
masamong memory blocks list

# 5. Attach a block to a Discord room
masamong channels view attach --guild_id <server ID> --channel_id <channel ID> --block_id <block ID>
```

## How indexing works

- **Snippets (leaves)**: conversation is grouped per room and split every **8 messages or 30 minutes of silence**.
- **Embeddings and summaries**: each snippet gets a vector for meaning search, plus a summary and its vector when summaries are on. Most of the token cost is summaries; to save, set `memory.summaries: false` and run on embeddings alone.
- **Incremental**: snippets already indexed are reused; only new or changed ones are processed.
- **Open snippets**: in live rooms the newest snippet stays open until it has 8 messages or 30 quiet minutes; until then it is not embedded, and those recent messages go straight into the prompt as recent context.
- **Periodic indexing**: every `memory.index_interval_min` (10 minutes by default) the daemon indexes rooms with new messages, spending at most `memory.index_budget_usd` ($1.00 by default) per run.
- **Recovery**: a room interrupted by a provider error, slow storage or the budget continues in the next run. The queue is stored, so nothing is lost across restarts.
- **No downtime**: when index settings change, the old index keeps answering until the new one is built in the background, then it is swapped in.

## Search strategy (`memory.retrieve`)

| Mode | Behavior |
|---|---|
| `auto` (default) | Keyword (lexical) and vector (semantic) hybrid search; the rerank model is called only when scores are unclear |
| `hybrid` | Hybrid search only, no rerank (fast and cheap) |
| `rerank` | Always rerank every candidate set (most accurate) |
| `off` | Memory search off |

> [!TIP]
> If the rerank API does not respond, the hybrid result is used; if memory search fails altogether, the bot still answers from the immediate context. Conversation never stops because of memory.

## Checking and correcting memory

```bash
# What memory a room would find, with scores
masamong memory search --guild_id <server ID> --channel_id <channel ID> --query "where did we decide to go camping"

# Plain keyword search over the original messages (no model calls, no cost)
masamong messages search --rooms kakao:friends --query "tent" --top_k 20

# Try a reply without posting to the room
masamong chat --guild_id <server ID> --channel_id <channel ID> --message "who's been the busiest lately?"
```

In the console's **Memory (`/memory`)** page you can read the source text behind every memory, **correct** a wrong one or **exclude** it from search, and **merge** one person's different names (KakaoTalk name, Discord account) by hand.

## Persona and reply style

Masamong is a friend to talk to, not a report generator.

- It brings memories up the way people do — "you said … that time", "I think it was …" — not "according to the conversation records".
- When a memory is vague or missing, it does not make things up; it lets it go or answers with a light touch.
- **Who did what** ("who said they'd bring the tent?") is said only when it is in memory. Otherwise it names no one — pointing at the asker or at itself is a guess too — says it does not remember, and asks the room whether anyone does.
- It never invents sensitive personal matters (romance, health, money, legal trouble), and passes on tool data such as weather and prices exactly.

## Room isolation and privacy

- Each Discord room sees **only its own conversation and the memory blocks attached to it**.
- Conversations from other servers, rooms or DMs are never merged in or shown.
- People are never merged just because their display names match; merging always needs an operator's confirmation.
- More in [Security and privacy](https://github.com/kim0040/masamong2/wiki/Security-and-Privacy).
