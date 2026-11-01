# Development guide

[한국어](https://github.com/kim0040/masamong2/wiki/개발-가이드) | **English**

## Requirements

- Go 1.27+
- Node.js and npm only when changing the web console (`ui/`); ordinary builds use the committed `ui/dist` bundle
- Optional: Python 3 with NumPy (the `experimental/` RAG sandbox and offline evaluation tools). The console build also needs Python 3 for its string generator; it looks for `python3`, then `python`, then `py -3`, skips the Microsoft Store stub, and says what to install when none works
- Windows: PowerShell is enough. `make` is optional, and every `make` target below has a plain command next to it

```bash
git clone https://github.com/kim0040/masamong2.git
cd masamong2
make build          # build/masamong, build/masamongd
make test           # all unit tests
```

```powershell
# Windows without make
go build -o build\masamong.exe .\cmd\masamong
go build -o build\masamongd.exe .\cmd\masamongd
go test ./...
```

### Quick test instance

On macOS and Linux: double-click `start-test.command` in Finder, or run `./start-test.command` (or `make try`). On Windows: `powershell -ExecutionPolicy Bypass -File .\start-test.ps1` (or `make try`, which runs it). It builds the current source, starts a test instance fully separate from your real one, opens a signed-in console and shows the live log. `Ctrl+C` or closing the window stops it. If it is stopped from the console (Maintenance → Stop service) or `masamong stop` in another terminal, the window ends its log and closes by itself (restarts keep showing).

- Test data lives in `~/.masamong-try` (on Windows `%USERPROFILE%\.masamong-try`; change with `MASAMONG_TRY_ROOT`), and API keys go into that folder's `secrets/` as files instead of the system keychain or Credential Manager (`MASAMONG_SECRET_STORE=file`), so they never mix with your real instance. If port 8787 is busy, a free port is picked.
- `--reset` wipes the test data and starts from setup, `--check` runs `go vet` and `go test` first, `--stop` stops only the test service (e.g. `make try ARGS=--reset`). The Windows script takes the same as PowerShell switches: `-Reset`, `-Check`, `-Stop` (`make try ARGS=-Reset`). It builds into `build\try`, follows `logs\daemon.log` in the window, and `Ctrl+C` stops the test service. `-Reset` deletes only a folder with the exact ownership marker written by `start-test.ps1`; an unmarked `.masamong-try` folder is refused too.
- Without any keys, the console's offline practice mode covers the UI and the main conversation flows. To test Discord, use a separate test bot token so it does not interfere with your real bot.

## Layout

| Path | Role |
|---|---|
| `cmd/masamong`, `cmd/masamongd` | CLI launcher and background service entry points |
| `internal/application` | **The Action layer.** CLI, console and Discord requests all go through `Service.Execute` |
| `internal/catalog` | The Action catalog (CLI subcommands, console routes, side effects) |
| `internal/transport/web` | Loopback HTTP server, sessions, REST API, the light `/lite` page, the Svelte front end (`ui/`) |
| `internal/connectors/discord` | Discord gateway, slash and prefix command parsing, embed rendering |
| `internal/chat` | The conversation pipeline (planner → tools → memory search → reply) |
| `internal/memory/*` | Raw corpus (`corpus`), snippets (`leaves`), keyword search (`lexical`), combined search (`search`), storage (`store`), indexing (`engine`) |
| `internal/tools` | Tool catalog, schema checks, per-user and daily limits |
| `internal/providers/*` | Adapters for models, KMA and Open-Meteo weather, market data, web search and other outside providers |
| `internal/storage` | SQLite storage and backups (TiDB code disabled with `TiDBEnabled=false`) |
| `internal/features/*` | Consent (`privacy`), astronomical fortune calculation (`fortune`), university and transfer notices (`notices`, `catalog.json`) |
| `internal/lang/packs`, `locales/` | Bot prompts and language packs, console and backend translations |
| `internal/launcher`, `internal/tray` | Daemon lifecycle, start at login, tray icon |

### Concurrency rules

Follow these when adding features or Actions ([Concurrency and performance](https://github.com/kim0040/masamong2/wiki/Concurrency-and-Performance)).

- **Settings are immutable snapshots**: read them with `s.Doc()`, `s.Revision()`, `s.Registry()`. The returned values are read-only; **never modify their maps or slices**. When several fields must agree, read once into a local variable.
- **Settings changes are serialized**: change settings only through `mutateConfig` (or `applyCandidate`); every config write goes through an internal mutex.
- **Read-check-write inside one transaction**: a sequence of database reads, checks and writes must run atomically **inside** a `Store.WithWriter` block (e.g. `updatePoll`). Never do slow I/O — outside API or model calls — inside that block.
- **No nested writer**: SQLite has a single writer. Calling `s.Store.DB` again inside a `WithWriter` callback deadlocks; use only the `tx` passed to the callback.
- **Background work finishes cleanly**: Discord handlers register through the gateway work-admission guard. Shutdown closes admission, counts READY and RESUMED handlers along with replies and commands already accepted, and waits for that work before the service closes its database.

## Tests and checks

```bash
go test ./... -count=1            # all unit tests
go test -race ./... -count=1      # race detector
go vet ./...                      # static analysis
python3 scripts/license_audit.py  # open-source license check (Windows: python scripts\license_audit.py)

# console
cd internal/transport/web/ui
npm ci
npm run check -- --fail-on-warnings
npm run build
```

Tests use synthetic fixtures and temporary folders only. Never put real conversation text, personal identifiers or real API tokens in fixtures.

## CI

The `ci` workflow runs in two sizes, so a push gets a quick answer and the heavy checks run weekly.

**Quick** — pushes to `main` and pull requests
- `go vet`, Go tests, a minimal build without the web UI (`noweb`)
- Console checks (type check, bundle build and size budget, committed `ui/dist` and `locales/ui` up to date) only when console code or strings change
- License audit only when dependencies change (`go.mod`, `go.sum`, `package*.json`)
- A **Windows** job (`windows-latest`) on every push and pull request: `go vet`, the Go tests, the console build (`npm ci`, `npm run build`) and the Python retrieval suites, so a change that only breaks on Windows is caught before it merges
- Docs-only pushes (`docs/`, `*.md`) skip CI, and a newer push cancels the previous run

**Full** — every Monday, manual runs (Run workflow) and release tags
- Everything above plus the race detector, a 45-second load test, cross-compiling every release target (macOS, Linux amd64/arm64/armv7/riscv64, Windows amd64/arm64, FreeBSD amd64/arm64) and the license audit

The long load test (`soak`) runs on the 1st of each month; running it by hand before a release is recommended.

**When CI does not run** (Actions turned off in a fork, or you want an answer before pushing) — run the same checks locally: changed packages with `CI=true go test -count=1 -p 1 <packages>`, the race detector narrowed with `go test -race -run <tests>`, the console with `make ui` and `npm run check`, and docs with `make docs`.

## Releases

The steps before a tag (full checks, long load test, a week with a test bot, backup and rollback) are in the [Release checklist](https://github.com/kim0040/masamong2/wiki/Release-Checklist).

Pushing a version tag (`v*`) makes the `release` workflow build every archive on a macOS runner (`scripts/release.sh`), verify them (`scripts/verify-release.sh`), simulate Linux and Windows installs, and create the GitHub Release. The Windows smoke job downloads the Windows ZIP, checks it against `SHA256SUMS`, then sets up, starts, checks `status` and `doctor`, and stops an instance in a scratch folder with Go and the host's time zone files hidden, so a missing embedded time zone database fails the release.

```bash
git tag v0.2.0 && git push origin v0.2.0
```

**Releasing without Actions** — build and check the archives on macOS, then publish them by hand. The Linux and Windows install simulations do not run, so check the Windows items of the [Release checklist](https://github.com/kim0040/masamong2/wiki/Release-Checklist) on a Windows computer. Keep the archive names and `SHA256SUMS` as they are: the in-app update expects them.

```bash
scripts/release.sh v1.0.0 && scripts/verify-release.sh build/release
git tag v1.0.0 && git push origin v1.0.0
gh release create v1.0.0 build/release/* --title "Masamong v1.0.0" --generate-notes
```

The in-app update check and the install scripts download from the repository's public releases. Until the first release is published, the update check shows that it is not available and the install scripts build from source (or install from a release archive you unpacked yourself).

## Changing the console

- Routes and pages: `internal/transport/web/ui/src/routes.ts`; navigation: `src/lib/nav.ts`
- UI strings live in code in `ui/scripts/strings.py` (do not edit the generated `locales/ui/*.json`; the generator writes them with LF line ends on every system, so a rebuild on Windows is not a whole-file diff)
- Catalog JSON is UTF-8. Windows PowerShell 5.1 `Set-Content` needs `-Encoding utf8` to avoid the system code page; prefer the repository's Python catalog generators when available. `go test ./locales` catches catalog text damaged by a wrong code page.
- After changing the UI, run `make ui` (on Windows: `cd internal\transport\web\ui; npm ci; npm run build`) and **commit** the rebuilt bundle (`internal/transport/web/ui/dist`). `npm run build` works on Windows; a second build leaves `git status` clean
- Tests of the console's logic run with `npm run test:i18n` (`node --test scripts/*.test.mjs`): the build scripts, the API client (no endless spinner when the network fails), language lists and fonts, and the pure logic such as per-platform strings (`lib/platform.ts`) and empty-search explanations
- Shared patterns: `components/LoadState.svelte` (skeleton → "still loading" → error card with Retry) for anything that loads, `lib/platform.ts` `tp()` for strings that differ per system (`key.windows` / `key.darwin`), `lib/title.svelte.ts` for the page title ([admin-gui-ia.md](https://github.com/kim0040/masamong2/blob/main/docs/admin-gui-ia.md))
- Check phones down to 320 px, light and dark themes, keyboard access, and reduced motion as well as desktop

## Editing the wiki

The source of this wiki is the repository's `docs/wiki/` folder. When changes reach `main`, the `wiki` workflow publishes them; edits made in the wiki's web editor are overwritten by the next sync, so change the Markdown files in `docs/wiki/` and open a pull request. Links between pages use the full wiki address, like `[text](https://github.com/kim0040/masamong2/wiki/Page-Name)`; the wiki's double-bracket link syntax splits table cells at its `|` and is not a link when viewed in the repository, so it is not used.

### Link and translation check

`make docs` (`go run ./tools/doccheck`) checks two things. `go test ./...` and the `wiki` workflow run the same check, and a failure keeps the wiki from being published.

- **Links**: relative links and links to this repository's files and wiki pages must point at files that exist. `docs/archive/` is a record kept as written and is left out.
- **Translations**: the five READMEs, and each Korean wiki page with its English page, form groups. `docs/translations.json` records a hash of every file. Editing one language alone fails the check; update the others, then record again with `make docs-record`. A new wiki page needs the language switch near the top (`**한국어** | [English](…/wiki/English-Page)`) to be paired.

English pages have English file names (`Installation.md`, `Quick-Start.md`, …; the English home is `English.md`), with a language switch at the top of both versions pointing at each other. When you change one, change the other. The sidebar (`_Sidebar.md`) lists both languages.

## Contributing

Please follow [CONTRIBUTING.md](https://github.com/kim0040/masamong2/blob/main/CONTRIBUTING.md). Report security problems privately through [SECURITY.md](https://github.com/kim0040/masamong2/blob/main/SECURITY.md), not in public issues.
