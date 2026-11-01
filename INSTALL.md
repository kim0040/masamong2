# masamong

> Masamong is a self-hosted AI friend bot for Discord: it remembers what was said in each room, answers like a friend, and looks up weather, stocks, exchange rates, places and the news. It runs on the user's own computer or server with their own API keys and is set up in a web console bound to `127.0.0.1`. This file is written for AI coding agents (Claude Code, Codex, Cursor, Gemini CLI and others) so they can install it for a person. People can follow it too; the README and the [Installation wiki](https://github.com/kim0040/masamong2/wiki/Installation) have the same steps with more explanation.

I want you to install Masamong for me on this computer. Do the steps below yourself, one at a time, and show me each command before you run it. Stop and ask me only where a step says **ASK**. Talk to me in the language I write in.

OBJECTIVE: Masamong is installed from source, its service is running, and I have the console open in my browser with the setup wizard in front of me (or finished).

DONE WHEN: `masamong status --output json` prints `"running":true`, and I have told you the console opened (or, on a remote server, that `masamong connect` opened it on my computer).

## Rules (read these first)

- **Never ask me to paste a Discord bot token or an API key into this chat.** Never put one in a file, an environment variable, a command line or shell history. I type keys into the Masamong console myself; it stores them in the macOS Keychain, Windows Credential Manager, or an owner-only file on Linux and FreeBSD.
- No administrator rights are needed. Do not use `sudo` or an elevated shell for Masamong itself. If Git or Go is missing, **ASK** before installing it with a package manager.
- Do not open firewall ports or change the bind address. The console only listens on `127.0.0.1`; a remote server is reached through SSH (`masamong connect`).
- The sign-in link the console prints works once and for 2 minutes. Show it only to me; do not paste it anywhere else or fetch it yourself.
- Do not edit files inside the cloned repository. Do not run `install.sh --uninstall`, a reset from the console, or anything that deletes a data folder.
- If a step fails, show me the exact error and check the [Troubleshooting wiki](https://github.com/kim0040/masamong2/wiki/Troubleshooting) before you try something else. Do not work around a failure by changing Masamong's code.

## TODO

- [ ] 1. Find out the OS, CPU and whether this is a desktop or a headless server
- [ ] 2. Check Git and Go 1.27 or newer
- [ ] 3. Clone the repository
- [ ] 4. Build and install with the install script
- [ ] 5. Start the service and check it
- [ ] 6. Open the console
- [ ] 7. Walk me through the setup wizard (model key, Discord bot)
- [ ] 8. Optional: start at login or boot

## Steps

### 1. Look at this computer

Find the OS (macOS, Windows, Linux, FreeBSD), the CPU (`uname -m`, or `$env:PROCESSOR_ARCHITECTURE` on Windows) and whether there is a desktop with a browser. If you are connected to a remote machine over SSH or there is no display (`$DISPLAY` and `$WAYLAND_DISPLAY` empty on Linux), treat it as a **headless server** and follow the server notes in steps 6 and 8.

Supported: macOS 12+ (arm64, amd64), Linux (amd64, arm64, armv7 such as a 32-bit Raspberry Pi, riscv64), Windows 10+ (amd64, arm64), FreeBSD (amd64, arm64).

### 2. Check Git and Go

```bash
git --version
go version          # needs go1.27 or newer
```

If either is missing or Go is older than 1.27, **ASK** me, then install it:

- macOS: `brew install git go` (or the installer from https://go.dev/dl). For the menu bar icon, also `xcode-select --install` (optional; without it Masamong builds without the icon).
- Windows (PowerShell): `winget install Git.Git GoLang.Go`, then open a **new** PowerShell window so the new PATH is seen.
- Linux: distribution Go packages are often too old. Install Go from https://go.dev/doc/install and check `go version` again. Git comes from the distribution (`apt install git`, `dnf install git` and so on; this needs `sudo`, so **ASK**).

### 3. Clone

Clone into the current folder unless I name another one:

```bash
git clone https://github.com/kim0040/masamong2.git
cd masamong2
```

If a `masamong2` folder already exists and is a clone of this repository, run `git pull` in it instead.

### 4. Build and install

macOS, Linux, FreeBSD:

```bash
./install.sh          # macOS: ./install.sh --app also builds ~/Applications/Masamong.app (optional)
```

Windows (PowerShell, in the `masamong2` folder):

```powershell
powershell -ExecutionPolicy Bypass -File .\install.ps1
```

The script builds `masamong` and `masamongd` and installs them in the user's own folder (`~/.local/bin` on macOS, Linux and FreeBSD, `%LOCALAPPDATA%\Programs\Masamong` on Windows) and adds that folder to the user PATH. It prints the install path on its last lines.

A shell that was already open may not see the new PATH. In that case use the full path the script printed (for example `~/.local/bin/masamong`, or `& "$env:LOCALAPPDATA\Programs\Masamong\masamong.exe"` on Windows) for the rest of the steps.

Check it:

```bash
masamong version
```

### 5. Start the service and check it

```bash
masamong start
masamong status --output json      # expect "running":true
masamong doctor                    # diagnostics; before setup, missing keys and Discord are expected
```

`masamong start` only starts the service; it does not open a browser. If it does not reach `"running":true`, show me the output of `masamong status` and the last lines of the log file named in `masamong start --output json` (`log_file`).

### 6. Open the console

On a **desktop**:

```bash
masamong open
```

This opens the console in the default browser with a one-time sign-in link. There is no password or account. If no browser opens, run `masamong open --no-browser` and give me the link it prints (valid for 2 minutes).

On a **headless server**: do not try to open a browser there. Tell me to install Masamong on my own computer too (the same steps), then run this **on my computer**:

```bash
masamong connect user@server-address
```

It opens the server's console through an SSH tunnel. No port needs to be opened on the server.

### 7. Walk me through the setup wizard

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

If I only want to look around first, tell me about **Start practice mode** on the first screen: sample data, no keys, no cost.

### 8. Optional: start automatically

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

- where Masamong was installed and where its data folder is (`masamong status --output json` shows `data_root`),
- how to open the console again (`masamong open`, or `masamong connect user@server`),
- how to update later: `cd masamong2 && git pull && ./install.sh` (Windows: `git pull` then `install.ps1`); a running service restarts on the new version,
- that every outside API is billed to my own keys, and where to set the monthly budget (console → **Usage**),
- that I should tell the people in a room before the bot remembers their conversation.

EXECUTE NOW: Start with step 1 and work through the TODO list in order, showing me each command before you run it.
