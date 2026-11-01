# masamong

> Masamong is a self-hosted AI friend bot for Discord: it remembers what was said in each room, answers like a friend, and looks up weather, stocks, exchange rates, places and the news. It runs on the user's own computer or server with their own API keys and is set up in a web console bound to `127.0.0.1`. This file is written for AI coding agents (Claude Code, Codex, Cursor, Gemini CLI and others) so they can install it for a person. People can follow it too; the README and the [Installation wiki](https://github.com/kim0040/masamong2/wiki/Installation) have the same steps with more explanation.

I want you to install Masamong for me on this computer. Do the steps below yourself, one at a time, and show me each command before you run it. Stop and ask me only where a step says **ASK**. Talk to me in the language I write in.

OBJECTIVE: Masamong is installed, its service is running, and I have the console open in my browser with the setup wizard in front of me (or finished).

DONE WHEN: `masamong status --output json` prints `"running":true`, and I have told you the console opened (or, on a remote server, that `masamong connect` opened it on my computer).

## Rules (read these first)

- **Never ask me to paste a Discord bot token or an API key into this chat.** Never put one in a file, an environment variable, a command line or shell history. I type keys into the Masamong console myself; it stores them in the macOS Keychain, Windows Credential Manager, or an owner-only file on Linux and FreeBSD.
- If I paste a key or token into the chat anyway, say so, do not use or store it, and tell me to revoke it (Discord Developer Portal → Bot → Reset Token, or the key's own dashboard).
- Do not run `masamong admin token`, and do not open or print `admin/web-token`, `secrets/`, `sessions.json` or `.env` files in the data folder. They hold the long-lived console token or keys.
- No administrator rights are needed. Do not use `sudo` or an elevated shell for Masamong itself. If Git or Go is missing (only the source build needs them), **ASK** before installing it with a package manager.
- Do not open firewall ports or change the bind address. The console only listens on `127.0.0.1`; a remote server is reached through SSH (`masamong connect`).
- The sign-in link the console prints works once and for 2 minutes. Show it only to me; do not paste it anywhere else or fetch it yourself.
- Do not edit files inside a cloned repository. Do not run `install.sh --uninstall`, a reset from the console, or anything that deletes a data folder.
- If your sandbox blocks the network or writes outside the workspace, stop and ask me to approve the exact command, or to run it myself. Do not turn the sandbox off on your own. The installers need the network, the user's program folder (`~/.local/bin` or `%LOCALAPPDATA%\Programs\Masamong`) and, on macOS and Linux, my shell profile.
- If a step fails, show me the exact error and check the [Troubleshooting wiki](https://github.com/kim0040/masamong2/wiki/Troubleshooting) before you try something else. Do not work around a failure by changing Masamong's code.

## TODO

- [ ] 1. Find out the OS, CPU and whether this is a desktop or a headless server
- [ ] 2. Choose how to install: release download (default) or build from source
- [ ] 3. Install
- [ ] 4. Start the service and check it
- [ ] 5. Open the console
- [ ] 6. Walk me through the setup wizard (model key, Discord bot)
- [ ] 7. Optional: start at login or boot

## Steps

### 1. Look at this computer

Find the OS (macOS, Windows, Linux, FreeBSD), the CPU (`uname -m`, or `$env:PROCESSOR_ARCHITECTURE` on Windows) and whether there is a desktop with a browser. If you are connected to a remote machine over SSH or there is no display (`$DISPLAY` and `$WAYLAND_DISPLAY` empty on Linux), treat it as a **headless server** and follow the server notes in steps 5 and 7.

Supported: macOS 12+ (arm64, amd64), Linux (amd64, arm64, armv7 such as a 32-bit Raspberry Pi, riscv64), Windows 10+ (amd64, arm64), FreeBSD (amd64, arm64).

### 2. Choose how to install

- **A. Release download (default).** No Git or Go needed. The installer downloads the archive for this OS and CPU from the latest GitHub Release and checks it against the release's `SHA256SUMS`. The console can update this install later by itself.
- **B. Build from source (Git and Go 1.27 or newer).** Only if I say I want to change the code, or if A fails. A source build is updated with `git pull` and the install script, not from the console.

Use A unless I say otherwise. Check Git and Go only for B.

### 3A. Install from the release

macOS, Linux, FreeBSD:

```bash
curl -fsSL https://raw.githubusercontent.com/kim0040/masamong2/main/install.sh | sh
```

Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/kim0040/masamong2/main/install.ps1 | iex
```

- To pin a version, set `MASAMONG_VERSION=v1.0.0`: `curl -fsSL … | MASAMONG_VERSION=v1.0.0 sh`, and on Windows `$env:MASAMONG_VERSION = 'v1.0.0'` on the line before. Without it the latest release is used.
- To pass options to the shell script, add them after `sh -s --`, for example `… | sh -s -- --prefix DIR`. `--no-modify-path` skips the shell-profile edit below.
- If I ask to read the script first, download it (`curl -fsSL … -o install.sh`) and show it to me before running it.

Where it installs and what it changes:

- `masamong` and `masamongd` go in the user's own folder: `~/.local/bin` on macOS, Linux and FreeBSD, `%LOCALAPPDATA%\Programs\Masamong` on Windows. Data stays in a separate folder and is never touched.
- On macOS, Linux and FreeBSD, if that folder is not on PATH, `install.sh` appends one line to my shell profile (`~/.zshrc`, `~/.bashrc`, `~/.bash_profile` or `~/.profile`; for fish it only prints a hint). **Tell me this before you run it**, tell me which file it changed afterwards, and use `--no-modify-path` if I decline. On Windows it adds the folder to the user PATH.
- The script prints `installed <folder>/masamong …` (Windows: `Installed and verified Masamong in <folder>`) and a "Next:" block.

A shell that was already open does not see the new PATH, and macOS does not have `~/.local/bin` on PATH by default. Use the full path the script printed for the rest of the steps (for example `~/.local/bin/masamong`, or `& "$env:LOCALAPPDATA\Programs\Masamong\masamong.exe"` on Windows), or ask me to open a new terminal.

Check it:

```bash
~/.local/bin/masamong version      # or just: masamong version
```

### 3B. Build from source

Check Git and Go:

```bash
git --version
go version          # needs go1.27 or newer
```

If either is missing or Go is older than 1.27, **ASK** me, then install it:

- macOS: `brew install git go` (or the installer from https://go.dev/dl). For the menu bar icon, also `xcode-select --install` (optional; without it Masamong builds without the icon).
- Windows (PowerShell). winget may show a Windows permission (UAC) prompt, so tell me before you run it:

  ```powershell
  winget install --id Git.Git -e --accept-source-agreements --accept-package-agreements
  winget install --id GoLang.Go -e --accept-source-agreements --accept-package-agreements
  $env:Path = [Environment]::GetEnvironmentVariable('Path','Machine') + ';' + [Environment]::GetEnvironmentVariable('Path','User')   # use the new PATH in this shell
  ```

- Linux: distribution Go packages are often too old. Install Go from https://go.dev/doc/install and check `go version` again. Git comes from the distribution (`apt install git`, `dnf install git` and so on; this needs `sudo`, so **ASK**).

Clone into a folder of its own, never inside another project: use `~/masamong2` (Windows: `$HOME\masamong2`) unless I name another one. If the current folder is already a Git repository, do not clone into it. To match the latest release, add `--branch v1.0.0 --depth 1` to the clone command.

```bash
git clone https://github.com/kim0040/masamong2.git "$HOME/masamong2"
cd "$HOME/masamong2"
```

On Windows (PowerShell) use `"$HOME\masamong2"` in both lines.

If that folder already exists and is a clone of this repository, run `git pull` in it instead.

Build and install. The first build downloads Go modules and can take several minutes: run it with a long timeout or in the background, and tell me it is working.

macOS, Linux, FreeBSD:

```bash
./install.sh          # macOS: ./install.sh --app also builds ~/Applications/Masamong.app (optional)
```

Windows (PowerShell, in the `masamong2` folder):

```powershell
powershell -ExecutionPolicy Bypass -File .\install.ps1
```

The script builds `masamong` and `masamongd` and installs them in the same places as 3A, with the same PATH and shell-profile notes. Check it with `masamong version` (or the full path).

### 4. Start the service and check it

```bash
masamong start
masamong status --output json      # expect "running":true
masamong doctor                    # diagnostics; before setup, missing keys and Discord are expected
```

`masamong start` only starts the service; it does not open a browser. If it does not reach `"running":true`, show me the output of `masamong status` and the last lines of the log file named in `masamong start --output json` (`log_file`).

### 5. Open the console

On a **desktop**, ask me to run this in my own terminal (in Claude Code I can type `! masamong open`):

```bash
masamong open
```

It opens the console in the default browser with a one-time sign-in link. There is no password or account. Without a terminal, `masamong open` prints JSON that includes the sign-in URL, which would then sit in this chat's tool output, so run it yourself only if I agree. If no browser opens, run `masamong open --no-browser` (only if I agree) and give me the link it prints (valid for 2 minutes).

On a **headless server**: do not try to open a browser there. If I have not installed Masamong on my own computer, install it there first (step 3A, run on my computer). Then give me this command to run **on my computer**:

```bash
masamong connect user@server-address
```

Do not run `masamong connect` through your own shell tool. It stays in the foreground until I press Ctrl+C, and `ssh` may ask me for a password or passphrase, so your call would hang. Give me the command and let me run it in my terminal (in Claude Code: `! masamong connect user@server-address`). It starts the service on the server if needed and opens the console through an SSH tunnel. No port needs to be opened on the server.

### 6. Walk me through the setup wizard

The wizard asks for these. I type every key myself in the browser; you only explain.

1. **Data folder**: the default is fine. Every setting and memory lives there (macOS `~/Library/Application Support/Masamong`, Linux `~/.local/share/masamong`, Windows `%APPDATA%\Masamong`).
2. **Model key**: an API key from OpenRouter (easiest: one key covers chat, memory indexing and pictures), or CometAPI, Gemini, or another OpenAI-compatible service. Recommended models are picked automatically. Suggest setting a monthly budget in the console.
3. **Discord bot**: explain these steps in the [Discord Developer Portal](https://discord.com/developers/applications):
   1. **New Application**, give it a name.
   2. **Bot** tab → **Reset Token** → copy the token (shown only once) and paste it **into the console**, not here.
   3. Same tab, **Privileged Gateway Intents** → turn on **Message Content Intent** (needed for `!` commands, noticing its name, and memory).
   4. Back in the console, use the **invite link** it shows to add the bot to a server. It asks only for the permissions the bot uses, never Administrator. New servers wait for approval in **Servers & rooms**.
4. **First room**: the channel it answers in and its persona.
5. **Test chat**: a reply in the console without posting to Discord.

If I only want to look around first, tell me about **Try practice mode** on the first screen: sample data, no keys, no cost.

### 7. Optional: start automatically

**ASK** me first.

```bash
masamong autostart on
```

On a Linux server, also keep the user service running after logout (once, needs no root on most systems):

```bash
loginctl enable-linger "$USER"
```

## When you are done

Tell me, in short:

- where Masamong was installed and where its data folder is (`masamong status --output json` shows `data_root`), and whether you changed my shell profile (which file),
- how to open the console again (`masamong open`, or `masamong connect user@server`),
- how to update later:
  - release install (A): console → **Maintenance** → **Update now**, or run the step 3A command again; a running service restarts on the new version,
  - source build (B): `cd ~/masamong2 && git pull && ./install.sh` (Windows: `git pull`, then `install.ps1`); a running service restarts on the new version,
- that every outside API is billed to my own keys, and where to set the monthly budget (console → **Costs & limits**),
- that I should tell the people in a room before the bot remembers their conversation.

EXECUTE NOW: Start with step 1 and work through the TODO list in order, showing me each command before you run it.
