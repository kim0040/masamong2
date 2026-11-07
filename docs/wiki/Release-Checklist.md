# Release checklist

[한국어](https://github.com/kim0040/masamong2/wiki/릴리스-체크리스트) | **English**

The steps before handing a new version to people, split into what automated checks can confirm and what only a real environment shows.

## 1. Automated checks

| Check | How | Passes when |
|---|---|---|
| Tests and static analysis | `CI=true go test ./... -count=1`, `go vet ./...` | All pass |
| Console | `make ui`, then `git status` shows no change in `ui/dist` or `locales/ui`; `npm run check -- --fail-on-warnings` | Zero warnings |
| Windows | The `windows` job of `ci` (`go vet`, Go tests, console build, Python suites) on the release commit, and the release workflow's Windows smoke test: it checks the ZIP against `SHA256SUMS` and runs setup, start, `status`, `doctor` (the time zone must load with Go and host zone files hidden) and `stop` | Green |
| Full CI | GitHub **Actions → ci → Run workflow** (concurrency checks, 45-second load, every release target, licenses) | Green |
| Long load test | **Actions → soak → Run workflow** (45 minutes by default) | `-strict` PASS: no goroutine or file leaks, under 1% errors, database integrity ok |
| Search parity | Only for releases that change search: section 2 of the [memory operations document](https://github.com/kim0040/masamong2/blob/main/docs/memory-operations.md) (Korean) | Evidence, excerpts and dates match |

Ordinary pushes to `main` run only the quick checks ([Development guide](https://github.com/kim0040/masamong2/wiki/Development-Guide)). The full checks also run on release tags, but running them by hand before tagging saves re-tagging.

If GitHub Actions cannot run, do the same items locally. Instead of the full CI, run per-package `CI=true go test -count=1 -p 1`, a narrowed `go test -race`, the cross-builds of every release target (`scripts/release.sh`), a build without the web UI (`go build -tags noweb ./cmd/...`) and the license audit (`python3 scripts/license_audit.py`). Run the long load test with the same settings as the `soak` workflow, `GOMAXPROCS=1 go run ./tools/loadtest -seed 20000 -duration 45m -rate 6 -dims 1024 -sample 30s -config-every 60s -strict -report soak.md`, and check the Windows items on a Windows computer. Publishing a release by hand is described in the release section of the [Development guide](https://github.com/kim0040/masamong2/wiki/Development-Guide).

## 2. A week in a real environment

Automated checks use a fake Discord and fake models. Problems that only real Discord, real model keys and real conversation reveal need a few days with a **test bot**. Using the same token as your running bot makes both answer, so always create a separate one.

### Setup

1. Create a test application and bot in the Discord Developer Portal and turn on **Message Content Intent**.
2. Start an isolated test instance with `./start-test.command --reset` (data in `~/.masamong-try`). On Windows: `powershell -ExecutionPolicy Bypass -File .\start-test.ps1 -Reset` (data in `%USERPROFILE%\.masamong-try`).
3. In setup, enter an OpenRouter key and the test bot's token; the console checks the key for real right away.
4. Invite the bot to a test server, approve it under **Servers & rooms → Server approval**, and turn on replies and memory collection in the rooms you will use.
5. Add your Discord ID as an operator under **People & access**, and check that **Operator alerts → Send a test alert** arrives as a DM.
6. Set a small **monthly budget** under settings (e.g. $5).

### Every day (5 minutes)

- Home notifications and operator DMs: anything new?
- **Conversations**: error and no-reply rates. Is every no-reply explained (no mention, limit, pause, …)?
- **Costs**: is the daily cost in the expected range?
- **Logs → Uptime**: any crashes, and why?

### Things to try on purpose

| Test | How | Expected |
|---|---|---|
| Crash | `kill -9 <service PID>` (`pid` in `masamong status`; Windows: `Stop-Process -Id <PID> -Force`) | Back up in 1–2 seconds; "crash" in uptime history; operator DM |
| Network loss | Turn Wi-Fi off for 1–2 minutes | Discord reconnects; **catching up missed messages** fetches what was said meanwhile |
| Rejected key | Save a wrong key on purpose under Connections | "Key rejected" and an operator DM; the bot does not answer |
| Backup | Maintenance: back up → verify → restore preview | Verification passes; the preview compares revisions |
| Reboot | Turn on start at login and restart the computer | After login the service and bot work again |
| Stop | Console **Stop service** | The console switches to the stopped screen and the test window closes |
| Update on Windows | **Maintenance → Update now** (or `install.ps1`) on a Windows machine whose service and tray icon are running, twice in a row | The tray icon and service are replaced without a "file in use" error, both updates succeed, and `masamong doctor` reports the new version |
| Interrupted update or restore | Kill the service in the middle of a restore or update on a scratch instance, then start it again | The next start finishes or undoes it by itself; if not, the recovery page appears and the steps in [Troubleshooting](https://github.com/kim0040/masamong2/wiki/Troubleshooting#the-console-shows-recovery-required-maintenance-mode) settle it |

### Reply quality baseline

Ask 50+ questions people would really ask in the **Answer lab** and mark each reply good or bad. Then press **Run evaluation** once to keep a baseline. For later releases that change search or models, run the same evaluation and compare whether evidence marked good is still found (retention) and whether the bad ones got fixed.

### Pass criteria (example)

- Under 1% of conversations end in an error, and every no-reply is explained
- No crashes, or if there were, automatic recovery and alerts were confirmed
- Every problem in the notification list also arrived as an operator DM
- Daily cost within the budget

## 3. Tag and release

1. Pick the version (`vMAJOR.MINOR.PATCH`). A change that migrates settings or data by itself bumps at least MINOR; one that needs a manual migration bumps MAJOR.
2. `git tag vX.Y.Z && git push origin vX.Y.Z` → the `release` workflow runs the full checks, builds every target, writes `SHA256SUMS`, simulates installs and creates the GitHub Release.
3. Write the release notes: what changed, setting changes, known problems. Releases are not signed yet, so link the first-launch warning help in [Installation](https://github.com/kim0040/masamong2/wiki/Installation).

## 4. Upgrading the real instance, and going back

1. **Back up first**: Maintenance → Backup (or `masamong backup create`) → verify.
2. **Maintenance → Update now** (or run the new version's install script → `masamong restart`) → `masamong doctor`.
3. Check there are no new notifications on Home and that the bot answers in a test room.

The database only migrates forward at start (there is no reverse conversion). To go back, reinstall the previous version and restore the **backup made before upgrading**. Conversation from the meantime is fetched again by Discord **catching up missed messages**, up to the last 7 days.

**Going back to v1.1.x**: restoring the backup is all it takes. To open the current database with the older version instead, empty the conversation context first. Replies from later versions point at the turns they read with links only, and v1.1.x does not follow links, so deleting a message would leave the replies built on it. The steps are in the [maintainer guide §4](https://github.com/kim0040/masamong2/blob/main/docs/MAINTAINING.md).

## 5. Current limits

| Item | Status |
|---|---|
| Code signing and notarization | Not done (on hold by the operator); first-launch warning help instead |
| New version notice, update from the console | Built. Checked against a local fake release server: 0.2.0 → 0.3.0 installed, restarted and backed up (2026-10-02). The first public release is v1.0.0 (2026-11-02); the real in-console update is still to be tried on the next release |
| Human-reviewed evaluation set | The tooling exists (lab verdicts → evaluation run); no reviewed questions yet |
| Installing on real machines | macOS by hand. Windows: `install.ps1` install and uninstall from a clone ran on Windows 10 on 2026-10-05 (including the user PATH being restored exactly), and CI smoke-tests the release ZIP; `install.ps1`'s live download from GitHub Releases and an in-console update on a real Windows machine have not been run. Linux up to CI's install simulation |
| Discord | Running on real servers around the clock since 2026-10-13, which covers the everyday paths. Failure paths such as gateway resume and startup retry are covered only by tests with an injected session. [Backlog](https://github.com/kim0040/masamong2/blob/main/docs/BACKLOG.md) item 1 lists what to check with a test bot |
| Weather, market and search tools | Open-Meteo (weather) and Jina Reader (link reading) checked with real calls. KMA, Finnhub, ExchangeRate-API, Linkup and Kakao (search, places) checked with real keys in the 2026-10-01 usage scenarios (`TestLiveScenarios`, 52 scenarios). On 2026-10-15 Linkup, Exa, Kakao and TinyFish were called with real keys in every search depth and the news topic (scheduled news task included, `TestLiveScheduledTask`). Tavily, Serper and Brave tested against their documented response formats only |
| Gemini and OpenAI | Gemini checked end to end with a real key (recommended models, indexing, search, replies, tool planning, pictures): `MASAMONG_LIVE_GEMINI_KEY_FILE=… go test ./internal/application -run TestLiveGemini`. OpenAI tested against the official format only |
