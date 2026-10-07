# 📦 Installation Guide

This guide covers the Linux installer, user-level systemd service, post-install commands, and host optimization tools.

## 🚀 Quick Start

The provider is designed to run as a **non-privileged user service** for maximum security and reliability.

> [!IMPORTANT]
> Recommended: run this command as your normal non-root user. If run as root, the installer will guide you through creating a dedicated service user named `urnet`.

Install:

```bash
curl -fSsL https://dl.fullbars.xyz/install.sh | sh
```

Uninstall:

```bash
curl -fSsL https://dl.fullbars.xyz/uninstall.sh | sh
```

### 🔑 Post-Install Authentication

After installation, source your terminal profile so the new commands are available in the terminal you installed from (new terminals, `ssh host urnet-tools ...`, cron and root find them without this step: the installer links `urnet-tools` and `urnetwork` into `~/.local/bin` and `/usr/local/bin` and writes the PATH block to `~/.bashrc`, `~/.profile` and `~/.zshenv`), and authenticate the provider. Then you can load your proxy list:

```bash
source ~/.bashrc
urnetwork auth
urnet-tools proxy add ~/proxies.txt
urnet-tools proxy refresh
```

> [!TIP]
> **Path Formatting**
> You can use either `~/proxies.txt` or `/home/you/proxies.txt`. Both syntaxes work.

> Full proxy-loading walkthrough (including Windows): [Adding Proxies](Adding-Proxies.md).

## 🍎 macOS Installation

The macOS installer is the equivalent of the Linux installer but uses `launchd` instead of `systemd`:

```bash
curl -fSsL https://dl.fullbars.xyz/install-mac.sh | sh
```

Uninstall (manual — macOS uninstall script not yet available):

```bash
# Remove binary and service files
rm -rf ~/.local/share/urnetwork-provider
launchctl unload ~/Library/LaunchAgents/com.urnetwork.provider.plist 2>/dev/null
rm -f ~/Library/LaunchAgents/com.urnetwork.provider.plist
# Remove identity and proxy data (⚠️ deletes all JWTs and proxy state)
rm -rf ~/.urnetwork
# Remove PATH additions from ~/.bashrc / ~/.zshrc
```

### What gets installed

| Component | Location |
|-----------|----------|
| Provider binary | `~/.local/share/urnetwork-provider/bin/urnetwork` |
| launchd plist | `~/Library/LaunchAgents/com.urnetwork.provider.plist` |
| Logs | `~/Library/Logs/com.urnetwork.provider/stdout.log` + `stderr.log` |
| State directory | `~/.urnetwork/` (same as Linux) |

### Post-install commands

All `urnet-tools` commands work identically to Linux:

```bash
# Start/stop
urnet-tools start
urnet-tools stop
urnet-tools restart
urnet-tools status

# Hot-restart toggle
urnet-tools hot-restart on
urnet-tools hot-restart off

# Session save/load
urnet-tools session save backup.urnsession
urnet-tools session load backup.urnsession

# Auth and proxy
urnetwork auth
urnet-tools proxy add ~/proxies.txt
urnet-tools proxy refresh
urnet-tools proxy summary
```

> [!NOTE]
> macOS doesn't support `eco`, `ramlogs`, or `optimize` (those tune Linux kernel parameters). `optimize` in particular has no effect on macOS — it runs Linux-specific `sysctl` keys that don't exist there and prints "done" while actually changing nothing. All other commands work natively.

## 🔐 User-Level Systemd Service
Unlike traditional services that run as root, this build defaults to a **systemd user unit**.

- **Security:** the provider binary does not need root privileges.
- **Isolation:** configuration and JWT tokens are stored in the user's home directory.
- **Linger:** the installer enables `loginctl enable-linger`, so the provider starts automatically on boot and keeps running after logout.
- **Root guard:** if installed as root, the script can create a restricted `urnet` user and add it to the appropriate admin group.

## 🛠️ Post-Install Commands

The installation includes the `urnet-tools` suite for management. Since v3.23.0-fix.27.0 this is the **provider-aware Go binary** — on multi-provider machines, pass a target (`--unit` / `--user` / `--network` / `--network-id` / `--state-dir`) or the tool refuses. See [urnet-tools-go.md](urnet-tools-go.md).

| Command | Description |
| :--- | :--- |
| `urnet-tools status` | Check service health and uptime. |
| `urnet-tools logs` | Stream logs, automatically detecting RAM vs disk logging. |
| `urnet-tools auto on` | Enable Smart Auto. Recommended for most hosts. |
| `urnet-tools optimize` | Full host optimization for many-proxy deployments and high-volume traffic. Add `-f` to skip prompts. |
| `urnet-tools turbo v4` | Enable Turbo V4 mode. |
| `urnet-tools turbo v8` | Enable Turbo V8 mode. |
| `urnet-tools eco on/off` | Toggle Eco mode. |
| `urnet-tools ramlogs on/off` | Toggle RAM-disk logging independently. |
| `urnet-tools update` | Upgrade to the latest version (prompts before restarting the provider). |
| `urnet-tools update -f` | Non-interactive upgrade: stop, update, and restart the provider with no prompts. Use in scripts/automation. |

## 🪟 Windows Installation

Install via PowerShell (no admin required):

```powershell
powershell -c "irm https://dl.fullbars.xyz/install-win.ps1 | iex"
```

If Windows Defender blocks that one-liner, download the script, review it, and run it from disk in two steps instead (both lines in PowerShell):

```powershell
irm https://dl.fullbars.xyz/install-win.ps1 -OutFile "$env:TEMP\install-win.ps1"
powershell -NoProfile -ExecutionPolicy Bypass -File "$env:TEMP\install-win.ps1"
```

Uninstall via PowerShell (no admin required):

```powershell
powershell -c "irm https://dl.fullbars.xyz/uninstall-win.ps1 | iex"
```

### What gets installed

| Component | Location |
|-----------|----------|
| Provider binary | `%LOCALAPPDATA%\urnetwork\provider\windows\<arch>\urnetwork.exe` |
| Management tool | `urnet-tools` (Go binary, v3.23.0-fix.27.0+) |
| State directory | `%USERPROFILE%\.urnetwork\` |
| Startup (optional) | `%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup\urnetwork.lnk` |
| PATH | User PATH updated to include `%LOCALAPPDATA%\urnetwork\provider\windows\<arch>\` |

### Post-install commands

```powershell
# Authenticate
urnetwork auth

# Start in foreground
urnetwork provide

# Start in background
urnet-tools start

# Manage proxies
urnet-tools proxy add "$env:USERPROFILE\Downloads\proxies.txt"
urnet-tools proxy refresh
urnet-tools proxy summary

# View logs
urnet-tools logs

# Hot-restart toggle
urnet-tools hot-restart on
urnet-tools hot-restart off

# Session save/load
urnet-tools session save C:\Users\You\backup.urnsession
urnet-tools session load C:\Users\You\backup.urnsession

# Update
urnet-tools update
```

> See [Adding Proxies](Adding-Proxies.md) for per-OS proxy-loading instructions and the Windows `.txt.txt` extension trap.

### 📦 Tarball Install (Alternative)

Starting with v3.23.0-fix.31.2, the Windows release tarball includes `urnet-tools` alongside the provider binary. If you prefer a manual or offline install:

1. Download the release tarball from [GitHub Releases](https://github.com/full-bars/urnetwork-3.23-fix/releases).
2. Extract the archive to your desired location (e.g. `%LOCALAPPDATA%\urnetwork`).
3. Open **PowerShell** and run the included install script:

```powershell
.\Provider_Install_Win32.ps1
```

This registers the PATH entry and optional startup shortcut — the same result as the CDN installer, but sourced entirely from the tarball. No internet access is required at install time. The script detects `amd64`/`arm64` automatically and places the correct binaries.

> [!TIP]
> The tarball method is useful for air-gapped machines or when you want to pin a specific release version rather than always pulling `latest`.

## 📊 System Auditor & Host Optimization

When the provider starts, it logs a **System Auditor** report that checks kernel limits and disk I/O performance:

```text
[audit] Conntrack Max: 262144 (Suboptimal! Target: 2097152)
[audit] Hint: Container detected suboptimal host limits. Run 'urnet-tools optimize' on the HOST to fix.
```

> [!WARNING]
> The provider cannot modify host-level kernel settings from inside a container. Run `urnet-tools optimize` on the host machine when deploying many proxies, or whenever you see `Suboptimal!` warnings.

For Docker-only users who do not want the systemd provider service, run the installer on the host to install the tools:

```bash
curl -fSsL https://dl.fullbars.xyz/install.sh | sh
```

Then optimize the host:

```bash
urnet-tools optimize -f
```

`optimize` re-executes itself under `sudo` with its own resolved binary path when it needs root, so you don't have to type `sudo /path/to/urnet-tools` (and it does NOT work as bare `sudo urnet-tools` — the binary lives on a per-user path, not root's PATH).

The `-f` flag skips interactive prompts. This applies:

- Conntrack max: `262144` -> `2097152`
- Conntrack timeout: `432000s` -> `5400s`
- TCP established timeout: 5 days -> 1 hour
- BBR congestion control and Fair Queuing
- Auto-install of `zram` and `conntrack-tools`
- Boot persistence for kernel modules

After optimization, your Docker container should restart and report:

```text
[audit] Conntrack Max: 2097152 (Optimal!)
```

> [!NOTE]
> If you only run Docker and do not intend to use the systemd provider service, the installer still offers just the tools. Choose `n` when prompted to enable the systemd service.
