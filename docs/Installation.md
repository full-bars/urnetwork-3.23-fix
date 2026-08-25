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

After installation, you must source your terminal profile so the new commands are available, and authenticate the provider. Then you can load your proxy list:

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
> macOS doesn't support `eco` or `ramlogs` commands (those tune Linux kernel parameters). All other commands work natively.

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

Uninstall via PowerShell (no admin required):

```powershell
powershell -c "irm https://dl.fullbars.xyz/uninstall-win.ps1 | iex"
```

### What gets installed

| Component | Location |
|-----------|----------|
| Provider binary | `%LOCALAPPDATA%\urnetwork\provider\windows\<arch>\urnetwork.exe` |
| Management tool | `urnet-tools` (Go binary, v3.23.0-fix.27.0+; the legacy `urnet-tools.ps1` wrapper is deprecated) + `urnetwork-updater.ps1` |
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
sudo urnet-tools optimize -f
```

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
