# Installation

[한국어](https://github.com/kim0040/masamong2/wiki/설치) | **English**

Installing Masamong needs no administrator rights. The programs go into your user folder, and uninstalling keeps the data folder.

## Before you start

| You need | Where to get it | When |
|---|---|---|
| Git and Go 1.27+ | [git-scm.com](https://git-scm.com), [go.dev/dl](https://go.dev/dl) (macOS `brew install git go`, Windows `winget install Git.Git GoLang.Go`) | To build from source only. Not needed if you install from a release ([Quick install](#quick-install)) |
| A model API key | [OpenRouter](https://openrouter.ai) is the easiest (one key covers chat, memory and pictures); CometAPI, Gemini or any OpenAI-compatible service also work | In the setup wizard. Practice mode needs none |
| A Discord bot token | [Discord Developer Portal](https://discord.com/developers/applications) → New Application → **Bot** → Reset Token, and turn on **Message Content Intent** ([Connecting Discord](https://github.com/kim0040/masamong2/wiki/Connecting-Discord)) | In the setup wizard, or later |
| Optional tool keys | KMA (Korean weather), Kakao (places), a web search service, Finnhub, ExchangeRate-API ([Tools and limits](https://github.com/kim0040/masamong2/wiki/Tools-and-Limits)) | Any time, under **Connections** |

Keys and tokens are typed into the console only. They go into the macOS Keychain, Windows Credential Manager, or an owner-only file on Linux and FreeBSD; never paste them into a chat, an issue or a shell command.

## Quick install

No Git or Go needed. The script downloads the release for your OS and CPU, checks it against the release's `SHA256SUMS` and installs it into your user folder.

macOS · Linux · FreeBSD:

```bash
curl -fsSL https://raw.githubusercontent.com/kim0040/masamong2/main/install.sh | sh
```

Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/kim0040/masamong2/main/install.ps1 | iex
```

- **Options** go after `sh -s --`, for example `curl -fsSL … | sh -s -- --prefix DIR` or `--no-modify-path` (see [Install options](#install-options)). In PowerShell, to pass switches such as `-Prefix`, save the script first (`irm … -OutFile install.ps1`) and run that file.
- **A version**: `curl -fsSL … | MASAMONG_VERSION=v1.0.0 sh`, or in PowerShell `$env:MASAMONG_VERSION = 'v1.0.0'` on the line before. Without it the latest release is used.
- **Shell profile** (macOS, Linux, FreeBSD): if `~/.local/bin` is not on your PATH, the script appends one line to your shell profile (`~/.zshrc`, `~/.bashrc` or `~/.bash_profile`, or `~/.profile`; for fish it only prints the command). `--no-modify-path` skips it.
- **The same terminal does not see the new PATH.** Open a new terminal and run `masamong open`, or use the full path the script prints (for example `~/.local/bin/masamong open`).

## Install with an AI coding agent

Paste this into Claude Code, Codex, Cursor, Gemini CLI or another coding agent. It follows [INSTALL.md](https://github.com/kim0040/masamong2/blob/main/INSTALL.md), which is written for agents: by default it installs the release (no Git or Go needed), shows each command before running it, asks before installing Git or Go (only a source build needs them), and leaves every key to you.

```text
Install Masamong for me. First read https://raw.githubusercontent.com/kim0040/masamong2/main/INSTALL.md in full, then follow it step by step on this computer. Never ask me to paste a Discord token or an API key into this chat; I will type those into the Masamong console myself.
```

From a terminal in one line. On macOS and Linux (bash or zsh): `claude "$(curl -fsSL https://raw.githubusercontent.com/kim0040/masamong2/main/INSTALL.md)"` (Codex: `codex "$(curl -fsSL …)"`).

In PowerShell, `$(curl …)` does not work, so pass the prompt text itself (this also works in bash, zsh and cmd):

```powershell
claude "Install Masamong for me. First read https://raw.githubusercontent.com/kim0040/masamong2/main/INSTALL.md in full, then follow it step by step on this computer. Never ask me to paste a Discord token or an API key into this chat; I will type those into the Masamong console myself."
codex "Install Masamong for me. First read https://raw.githubusercontent.com/kim0040/masamong2/main/INSTALL.md in full, then follow it step by step on this computer. Never ask me to paste a Discord token or an API key into this chat; I will type those into the Masamong console myself."
```

For Gemini CLI, Cursor and other agents, paste the prompt into the agent's chat.

The rest of this page is the same install done by hand.

## Supported platforms

| OS | CPU | Release file | Notes |
|---|---|---|---|
| macOS 12+ | Apple Silicon (arm64), Intel (amd64) | `masamong_darwin_*.tar.gz` | Menu bar icon, optional `.app` |
| Linux | amd64, arm64 | `masamong_linux_amd64/arm64.tar.gz` | Notification area icon (desktop), systemd autostart (server) |
| Linux (32-bit Raspberry Pi) | armv7 | `masamong_linux_armv7.tar.gz` | Raspberry Pi OS 32-bit, armv7l/armv8l |
| Linux | riscv64 | `masamong_linux_riscv64.tar.gz` | |
| Windows 10+ | amd64, arm64 | `masamong_windows_*.zip` | Notification area icon, start at login |
| FreeBSD | amd64, arm64 | `masamong_freebsd_*.tar.gz` | Service and console without a tray icon; autostart via rc.d/cron on servers |

A release contains two programs. Every target except macOS is built without C libraries (CGO); macOS uses CGO for the menu bar icon (built from source without the Xcode command line tools, it simply has no icon).

- `masamong` — the command-line tool and launcher: open the console, start and stop the service, connect to a remote one
- `masamongd` — the bot service (daemon): database, Discord connection, the console

## macOS · Linux · FreeBSD

### From source (Go 1.27+)

```bash
git clone https://github.com/kim0040/masamong2.git
cd masamong2
./install.sh
~/.local/bin/masamong open     # or in a new terminal: masamong open
```

The script appends the PATH line to your shell profile, but the terminal you ran it in does not see it yet. That is why the example uses the full path.

On Linux, distribution Go packages are often too old; install Go from [go.dev/doc/install](https://go.dev/doc/install). On macOS: `brew install go`, plus `xcode-select --install` for the menu bar icon.

### Quick test run (a separate test instance)

To try changes without installing:

```bash
make try
# or double-click start-test.command on macOS
```

This starts a throwaway instance (`~/.masamong-try`) separate from your real one and opens its console. `Ctrl+C` stops it. On Windows use `start-test.ps1` instead ([Windows](#windows)).

### Without Go

Without Go, `install.sh` downloads the release archive for your OS and CPU from GitHub Releases and checks it against `SHA256SUMS` (the [Quick install](#quick-install) one-liner does exactly this). Pick a version with `MASAMONG_VERSION=v1.0.0 ./install.sh`. If you downloaded a release yourself, unpack it and run the `install.sh` inside.

```bash
tar -xzf masamong_linux_arm64.tar.gz
cd masamong_linux_arm64
./install.sh
```

### Install options

| Option | Meaning |
|---|---|
| `--prefix DIR` | Install into `DIR/bin` (default `~/.local`) |
| `--no-modify-path` | Do not add the folder to PATH in your shell profile |
| `--app` | macOS: also build `~/Applications/Masamong.app` (double-click to open) |
| `--uninstall` | Stop the service, turn off autostart, remove the programs (data stays) |

As a macOS app:

```bash
./install.sh --app
open "$HOME/Applications/Masamong.app"
```

## Windows

Installing needs no administrator rights. Everything below is typed in **PowerShell** (the one that comes with Windows 10); Git Bash, WSL and `make` are not needed.

| What | Where |
|---|---|
| Programs | `%LOCALAPPDATA%\Programs\Masamong` (`masamong.exe`, `masamongd.exe`), added to your **user** PATH |
| Data | `%APPDATA%\Masamong` (database, settings, logs, key files). A different folder from the programs; uninstalling keeps it |

`install.ps1` works in three situations:

1. **From the release ZIP, without Go.** Unpack `masamong_windows_amd64.zip` (`arm64` for ARM laptops) and run the script inside the unpacked folder.
2. **From a clone, with Go** (`winget install Git.Git GoLang.Go`). The script builds both programs from source. If your Go is older than the version in `go.mod`, it stops and asks you to update Go.
3. **From a clone, or the script alone, without Go.** With no Go and no programs next to the script, it downloads `masamong_windows_<amd64|arm64>.zip` and `SHA256SUMS` from GitHub Releases, checks the checksum and only then installs. Pick a version with `$env:MASAMONG_VERSION = 'v1.0.0'` before running it.

```powershell
powershell -ExecutionPolicy Bypass -File .\install.ps1
```

The script also runs straight from the web, with no clone and no Go ([Quick install](#quick-install)): `irm https://raw.githubusercontent.com/kim0040/masamong2/main/install.ps1 | iex`. It does not depend on a script-relative folder in that case. To pass switches such as `-Prefix`, save it first (`irm … -OutFile install.ps1`) and run that file.

When it ends, the script prints the next command:

```text
Installed and verified Masamong in C:\Users\you\AppData\Local\Programs\Masamong.

Next:
  Same window: & 'C:\Users\you\AppData\Local\Programs\Masamong\masamong.exe' open
  New terminal: masamong open
```

**Why two forms?** The installer writes the folder into your user PATH, but a window that was already open keeps the PATH it started with, so `masamong` there is "not recognized". Either open a **new** PowerShell and run `masamong open`, or call the program by its full path in the same window (in `cmd.exe`: `"%LOCALAPPDATA%\Programs\Masamong\masamong.exe" open`).

- It proves the destination is writable and stages both new programs beside the installed files **before stopping anything**. It then stops every running service and tray instance by its data folder, swaps the staged files in with rollback if a swap fails, and restarts the previous service if it had been running. A folder it cannot write to fails before any service is stopped; use `-Prefix` to choose a writable folder.
- The script edits the raw user PATH registry value and preserves its type, including `REG_EXPAND_SZ` entries such as `%USERPROFILE%\...`; uninstall removes only Masamong's entry.
- The install folder has a versioned `.masamong-manifest.json`. Uninstall removes only listed, recognized Masamong files; an older install without a valid manifest falls back to the known Masamong filenames. Unrelated files stay, and the folder is removed only if empty. A non-empty folder without a valid manifest is refused unless you pass `-Force` after checking its contents. Drive roots, your profile root, Program Files and Windows roots are refused as install prefixes.
- `-Prefix C:\Apps\Masamong` installs somewhere else.
- `install.ps1 -Uninstall` stops the service, turns off start at login, deletes the programs and **removes its entry from your user PATH**. The data folder is kept.
- `install.ps1 -Uninstall -RemoveKeys` also removes this user's `masamong/*` Credential Manager entries. Without `-RemoveKeys`, saved keys stay.
- Start at login is `masamong autostart on` (a `Run` entry in your user's registry; `masamong autostart off` removes it). The icon sits in the notification area, and `masamong stop` closes it together with the service.
- Keys typed in the console go to **Windows Credential Manager** ([Security and privacy](https://github.com/kim0040/masamong2/wiki/Security-and-Privacy)).

### Quick test run on Windows

From a clone with Go:

```powershell
powershell -ExecutionPolicy Bypass -File .\start-test.ps1
```

It builds the current source into `build\try`, starts a test instance separate from your real one (data in `%USERPROFILE%\.masamong-try`, or the folder in `MASAMONG_TRY_ROOT`), opens its signed-in console and follows the log. `Ctrl+C` stops the test service. Keys are kept as files in that folder instead of Credential Manager (`MASAMONG_SECRET_STORE=file`), so they never mix with your real instance.

| Flag | Meaning |
|---|---|
| `-Reset` | Erase only a test folder carrying the exact ownership marker written by `start-test.ps1`, then start from setup. An unmarked folder named `.masamong-try` is refused too |
| `-Check` | Run `go vet` and `go test` before starting |
| `-Stop` | Only stop the test service |

With `make` installed, `make try` runs the same script (`make try ARGS=-Reset`).

### Start at boot before Windows login

You can create a boot task in **Task Scheduler**, separately from start at login. This is a manual registration procedure; the installer does not register tasks or services. Choose absolute paths for the installed `masamongd.exe`, a data folder and a log folder accessible to the task account. Always pass `--data-root`: the default `%APPDATA%` depends on the account running the task.

1. Open **Create Task** in Task Scheduler. Give each instance a different name and select **Run whether user is logged on or not** on the General tab. Leave **Run with highest privileges** off when the folders do not require administrator access. Registration may ask for the task account's password.
2. Add an **At startup** trigger.
3. Choose **Start a program**. Set the program to the absolute path of `masamongd.exe` and **Start in** to its installation folder. Use these arguments, quoting paths containing spaces:

   ```text
   --supervise --data-root "C:\Masamong Data\always-on" --log-file "C:\Masamong Data\always-on\logs\daemon.log"
   ```

   This runs `masamongd --supervise --data-root <folder> --log-file <file>`. Add `--web` if the console is disabled in configuration, so the normal stop API is available. A task running before login has no tray icon. Manage it from the same account with `masamong --data-root "C:\Masamong Data\always-on" open`.
4. Enable **If the task fails, restart every** (for example, one minute, three attempts). Disable **Stop the task if it runs longer than**. Select **Do not start a new instance** when already running, and adjust power conditions for the machine's operating policy.
5. Verify a manual start and normal stop using this folder before saving the task. To remove it, first run `masamong --data-root "C:\Masamong Data\always-on" stop`, then disable or delete only that task. Task Scheduler's **End** action and forcibly terminating a process do not send a normal stop request.

A normal supervisor close request (`WM_CLOSE`) stops its own child PID and data folder through the local console API, then waits about 60 seconds after that request succeeds (the daemon's shutdown budget plus time to close the database) before forcing the child to exit. If the console or authentication file is unavailable, or instance identity does not match, it logs the reason and kills only its child. In a logged-in user session, a hidden top-level window detects logoff and shutdown. It agrees immediately to `WM_QUERYENDSESSION` and starts cleanup only on confirmed `WM_ENDSESSION`. That path allows at most four seconds to fit within Windows' roughly five-second shutdown limit. Busy replies/jobs or a forced shutdown may prevent cleanup from finishing.

**Shutdown detection in a noninteractive session 0 task is not guaranteed.** The daemon handles SIGTERM, but window-message delivery and Windows termination order still need real-machine verification. Whether the daemon's SIGTERM path works in session 0 is also unverified. Boot, pre-login operation, reboot, actual logoff/shutdown and Task Scheduler's End action have not been validated. This does not add Windows service deployment. Use a separate data folder for a development instance and choose either start at login or a boot task for each folder to avoid duplicate launches.

## On a server (remote operation)

Install on the server, start only the daemon there, and manage it from your own computer over SSH. The console never opens a port on the server.

```bash
# on the server
./install.sh
masamong start
masamong autostart on          # start at login and boot (Linux: a systemd user unit)
loginctl enable-linger $USER   # keep the user service running after you log out (once)
masamong status

# on your computer
masamong connect user@server
```

On servers without systemd, such as FreeBSD, `masamong autostart on` says it is not supported. Add `@reboot $HOME/.local/bin/masamong start` with `crontab -e`, or run `masamong start` from an rc.d script.

## Data folder

| OS | Default path |
|---|---|
| macOS | `~/Library/Application Support/Masamong` |
| Linux · FreeBSD | `${XDG_DATA_HOME:-~/.local/share}/masamong` |
| Windows | `%APPDATA%\Masamong` |

Change it with `--data-root` or the `MASAMONG_DATA_ROOT` environment variable. Use a separate folder for each bot or for experiments.

```bash
masamong --data-root /srv/masamong-friends open
masamong --data-root /tmp/masamong-demo status --output json
```

One data folder is used by one daemon (`daemon.lock`); a second daemon on the same folder is refused.

## Security warnings on first launch

Release files are not code-signed yet, so the OS may warn once. Check that a file matches the published checksum with `SHA256SUMS` (`shasum -a 256 -c SHA256SUMS`; on Windows `Get-FileHash .\masamong_windows_amd64.zip`, compared with its line in `SHA256SUMS`). This detects corruption but does not prove who published the checksum. Both installers (`install.sh` and `install.ps1`) do this check themselves when they download an archive.

- **macOS**: on an "unidentified developer" warning, press **Open Anyway** under **System Settings → Privacy & Security**, or run `xattr -dr com.apple.quarantine <folder>` once on the install folder.
- **Windows**: in SmartScreen's "Windows protected your PC", choose **More info → Run anyway**.

## Automatic recovery

A service started from the icon, `masamong open` or `masamong start` runs under a supervisor. If it exits abnormally it is restarted, and the reason goes into the uptime history and the operator alerts. After more than 5 crashes in 10 minutes it stops restarting ([Troubleshooting](https://github.com/kim0040/masamong2/wiki/Troubleshooting)).

## Updating and uninstalling

- **New version notice**: once a day it checks for a new release and shows it under **Maintenance** and in the notifications (`updates.check: false` turns it off). The check downloads nothing.
- **Update from the console**: if you installed from a release archive, press **Maintenance → Update now**. In order:
  1. Downloads this computer's archive and `SHA256SUMS` and checks the sum.
  2. Checks that **both** new programs (`masamong` and `masamongd`) run here and report the right version.
  3. Takes a backup.
  4. Replaces the two programs as one step recorded in a journal, and restarts the service.

  A download or validation failure leaves the installed pair untouched. Once the journaled swap starts, retry or rollback is settled before the daemon opens its database. Rollback stages and verifies each old program beside the installed one, then replaces its name atomically, so a runnable installed program remains throughout. If recovery cannot finish, the bot stays off and the recovery page shows the available commands. When the update succeeds, the console reloads on the new version.
- **If an update is interrupted** (a crash, a power cut): the next start finishes the swap or puts the old pair back before anything else runs, and a supervisor does the same before it launches the service again. While that cannot be settled, the bot stays off and the daemon serves a localized recovery page with only the commands valid for that state ([Troubleshooting](https://github.com/kim0040/masamong2/wiki/Troubleshooting#the-console-shows-recovery-required-maintenance-mode)). `masamong open` and the tray's Open command reach the page; `doctor` and `masamongd --maintenance status` report maintenance, and `masamong stop` stops the daemon before offline recovery commands are used. On Windows, old program files that a running process still holds are removed after the new pair is confirmed.
- **Update with the installer**: a build from source (a `git describe` version) or a program folder the service cannot write cannot update itself; the console says why. Then `git pull` and run `install.sh` (or `install.ps1`) again. A release install can also be updated by running the [Quick install](#quick-install) command again. On Windows, the installer keeps the old pair until PATH is updated and every previously running data root reports healthy; if any step fails, it restores the old files, manifest and exact PATH value, then restarts those roots with their own `--data-root`.
- **Your data stays**: both ways replace only the programs, never the data folder. The first start of a new version takes another backup. To go back to the previous version, install it again and restore that backup. The newest three backups of each kind (before an update, first start of a version) are kept.
- **Uninstall**: `./install.sh --uninstall` or `install.ps1 -Uninstall`. On Windows, removal is limited to the manifest's recognized Masamong files (or the known-name fallback for older installs); unrelated files remain, and a shared install folder remains unless empty. The data folder is kept, so installing again carries on where you left off. Delete it yourself to remove everything (make a [backup](https://github.com/kim0040/masamong2/wiki/Storage-and-Backup) first).
- **Start over**: to keep the programs but erase the data, use **Maintenance → Reset** in the console ([Storage and backup](https://github.com/kim0040/masamong2/wiki/Storage-and-Backup#reset)).

### When data can be erased

The data folder is where each operating system keeps user data. It is not a cache or temporary folder, so it is outside what these remove by default: OS updates, disk cleanup (macOS storage management, Windows Disk Cleanup and Storage Sense) and general cleaners such as BleachBit. The service keeps nothing long-lived in `/tmp` or `%TEMP%`.

| OS | Data folder |
|---|---|
| macOS | `~/Library/Application Support/Masamong` |
| Windows | `%APPDATA%\Masamong` |
| Linux, BSD | `$XDG_DATA_HOME/masamong` (default `~/.local/share/masamong`) |

It goes only in these cases:
- you **Reset** from the console
- you delete the folder yourself
- you pick it in a cleaner's "app leftovers" list. CleanMyMac, for example, can offer a folder with no matching `.app`; leave it out of the list.

For data that matters, turning on [scheduled backups](https://github.com/kim0040/masamong2/wiki/Storage-and-Backup) is the surest protection.

Next: [Quick start](https://github.com/kim0040/masamong2/wiki/Quick-Start)
