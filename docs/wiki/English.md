# Masamong 2 Wiki (English)

[한국어](https://github.com/kim0040/masamong2/wiki/Home) | **English**

![Masamong](https://github.com/kim0040/masamong2/raw/main/assets/brand/deploy/readme-banner-en.png)

Localized product guides: [English](https://github.com/kim0040/masamong2/blob/main/README.md) · [한국어](https://github.com/kim0040/masamong2/blob/main/README.ko.md) · [日本語](https://github.com/kim0040/masamong2/blob/main/README.ja.md) · [简体中文](https://github.com/kim0040/masamong2/blob/main/README.zh-Hans.md) · [Español](https://github.com/kim0040/masamong2/blob/main/README.es.md) · [screenshot gallery](https://github.com/kim0040/masamong2/blob/main/docs/images/readme/README.md)

**Masamong 2** is a personal AI bot for Discord that remembers your rooms' conversations (live Discord messages and imported KakaoTalk CSV exports) and talks with people like a friend. It comes with a **web console** that opens only on your own computer, where you manage each room's rules, memory blocks, AI models, budget, backups and logs.

Version `1.0.0` · License: MIT · Repository: [kim0040/masamong2](https://github.com/kim0040/masamong2)

## What it does

| Area | Description |
|---|---|
| Chat and personas | Reply mode per room (every message / when mentioned / commands only / off), a persona and language rules per room; Korean, English, Japanese, Simplified Chinese and Spanish built in |
| Memory and search | Stores live conversation and indexes KakaoTalk CSV; each room searches **only the memory blocks attached to it** |
| Tools | Weather, earthquake and weather-warning posts, stock quotes, exchange rates, web search, places (Kakao Map), link reading, polls, pictures, channel summaries, top talkers — in conversation or with slash (`/`) and prefix (`!`) commands. When a tool has no answer the bot carries on talking instead of showing an error. Custom HTTP tools can be added in settings |
| Personal helpers | Reminders ("remind me tomorrow at 3"), a morning fortune DM, school notice subscriptions. Each needs its own consent; withdraw, delete and block in **People & access** |
| Concurrency | Rooms are answered **in parallel**; inside one room replies keep **arrival order** |
| Cost control | Monthly budget, daily limits per tool and per person, a cost ledger from token prices |
| Operations | Streaming logs, diagnostics (`doctor`), backup and one-click restore, migration from the previous bot, start at login, practice mode |

## Getting started

1. [Installation](https://github.com/kim0040/masamong2/wiki/Installation) — install on your OS
2. [Quick start](https://github.com/kim0040/masamong2/wiki/Quick-Start) — from the console to the first reply in ten minutes
3. [Connecting Discord](https://github.com/kim0040/masamong2/wiki/Connecting-Discord) — create the bot, permissions, invite it
4. [Memory blocks](https://github.com/kim0040/masamong2/wiki/Memory-Blocks) — index KakaoTalk exports and attach them to rooms

## Running it

- [Admin console](https://github.com/kim0040/masamong2/wiki/Admin-Console) — pages, and remote access over SSH
- [Tools and limits](https://github.com/kim0040/masamong2/wiki/Tools-and-Limits) — built-in tools, limits, custom HTTP tools, outside service terms
- [Configuration reference](https://github.com/kim0040/masamong2/wiki/Configuration-Reference) — every `config.yaml` option
- [Concurrency and performance](https://github.com/kim0040/masamong2/wiki/Concurrency-and-Performance) — reply limits, per-room order, tuning by size
- [Load testing and long-running operation](https://github.com/kim0040/masamong2/wiki/Load-Testing-and-Long-Running) — measured results, sizing, retention
- [Storage and backup](https://github.com/kim0040/masamong2/wiki/Storage-and-Backup) — SQLite, backup and restore, why it is built this way
- [CLI commands](https://github.com/kim0040/masamong2/wiki/CLI-Commands) — every command and the JSON output for scripts
- [Languages](https://github.com/kim0040/masamong2/wiki/Languages) — reply and memory language packs, adding a language
- [Security and privacy](https://github.com/kim0040/masamong2/wiki/Security-and-Privacy) — keys, loopback binding, conversation data
- [Troubleshooting](https://github.com/kim0040/masamong2/wiki/Troubleshooting) — when the bot does not answer, and FAQ

## Development

- [Development guide](https://github.com/kim0040/masamong2/wiki/Development-Guide) — build, tests, CI, layout, contributing
- [Pending decisions](https://github.com/kim0040/masamong2/wiki/Pending-Decisions) · [Release checklist](https://github.com/kim0040/masamong2/wiki/Release-Checklist)

## Note

Masamong is a personal project by a university student, provided as is, with no warranty or support. Outside API costs and the handling of conversation records are the installer's responsibility. See the [disclaimer](https://github.com/kim0040/masamong2/blob/main/README.md#disclaimer) in the README.

> [!NOTE]
> This wiki is published from the repository's `docs/wiki/` folder. To change a page, edit the file there and open a pull request; edits made in the wiki web editor are overwritten by the next sync.
