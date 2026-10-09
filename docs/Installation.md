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

> [!NOTE]
> **Alpine Linux (OpenRC)?** The command above is the same, but the installer takes a different path and the service is a system service, not a user service. Follow [Alpine Linux (OpenRC)](#-alpine-linux-openrc) instead of the systemd notes on this page.

## 🏔️ Alpine Linux (OpenRC)

Alpine and other OpenRC distributions are supported. The installer detects OpenRC on its own, so there is nothing new to type: the install command is the one every other Linux uses. This section walks through a first install from a root shell, which is how Alpine starts out.

### Before you start

You need a root shell and a regular account for the provider to run as. OpenRC has no per-user service manager, so the provider runs as a system service under a dedicated, unprivileged user. The installer expects that user to exist and uses `urnet` by default.

```sh
adduser -D urnet
apk add --no-cache curl
```

`adduser -D urnet` creates the `urnet` user with a home directory and no password. If the user does not exist when you install, the installer still downloads the files but does **not** install the service, and prints the commands to finish. To run the provider under a different account, set `URNET_OPENRC_USER=<name>` for the install command.

### Install

```sh
curl -fSsL https://dl.fullbars.xyz/install.sh | sh
```

### What the installer does on Alpine, and why

| What it does | Why |
|---|---|
| Installs the provider and `urnet-tools` under `/usr/local/lib/urnetwork-provider`, owned by root and readable by everyone. Only the state dir (`/home/urnet/.urnetwork`) lives in the service user's home, because the provider must write it. | The scheduled auto-update runs as root, and `sudo urnet-tools update` is the documented upgrade path. Root-owned files do not defend a path whose ANCESTOR a user can rewrite — a tree under `/home/urnet` could be renamed aside and replaced with the service user's own binary, which root would then execute. Keeping the tree off any user-writable path makes that impossible by construction. |
| Writes the service script `/etc/init.d/urnetwork` and runs `rc-update add urnetwork default`. | The `default` runlevel is what OpenRC starts at boot, so the provider comes back after a reboot with nobody logged in. |
| Runs the provider under `supervise-daemon` as the `urnet` user. | The provider drops root, and `supervise-daemon` restarts it after a crash with a five-second delay, increasing by five seconds per restart up to 60 seconds, with a limit of 10 restarts per hour. |
| Sends the provider's output to `/var/log/urnetwork.log` (stdout) and `/var/log/urnetwork.err` (stderr), both owned by the service user; `/var/log` remains root-owned. | `supervise-daemon` opens these files after dropping privileges, so the service user must be able to write them. Because the log is a plain file, `urnet-tools logs` can read it even while the service is stopped. |
| Keeps the provider's state, including the login token, in `/home/urnet/.urnetwork`. | The provider reads its credentials from the home directory of the user it runs as. |

The installer does not start the service. It finishes by printing the commands for the next two steps.

> [!NOTE]
> OpenRC has no equivalent of systemd's `enable-linger`, and none is needed: a service in the `default` runlevel starts at boot whether or not anyone logs in.

### Authenticate

The provider needs an auth code from <https://ur.io>, and the login token it produces has to land in the `urnet` user's home, not root's. A token written to `/root/.urnetwork` is invisible to the service, which would start and then have no credentials.

Run the authentication as the service user:

```sh
su -s /bin/sh urnet -c 'urnetwork auth <code>'
```

The installer prints this exact command when it finishes. When you run it in an interactive terminal it can also offer to do this step for you: answer `y`, enter the code, and the installer runs the authentication as `urnet`. The prompt is read from the terminal device itself, not from standard input, so it works for the one-line `curl ... | sh` form as well — the script arriving on standard input does not take the terminal away. Where there is no terminal at all (a provisioning script, a CI job, a cron entry) the prompt is skipped and the command is printed instead. If the prompt was skipped, or the authentication did not complete, run the command above at any time. Auth codes are single-use, so fetch a new one if a code was already submitted.

### Start and verify

```sh
rc-service urnetwork start
rc-service urnetwork status
```

`status` should report `started`. To see what the provider is doing:

```sh
urnet-tools logs
tail -f /var/log/urnetwork.log
```

`urnet-tools logs` follows the same file. If the service does not stay up, read `/var/log/urnetwork.err` as well as the main log. If you have not authenticated yet, go back to [Authenticate](#authenticate).

A node with no proxy list configured serves traffic directly from the box's own address. The `[profit]` line in the log says so with `mode=direct`; see the [Log Reference](../LOG_REFERENCE.md#-profit-heartbeat-323-fix).

To add proxies, see [Adding Proxies](Adding-Proxies.md).

### Stop, disable, and uninstall

Stop the provider now (it starts again at the next boot):

```sh
rc-service urnetwork stop
```

Stop it and keep it from starting at boot:

```sh
rc-service urnetwork stop
rc-update del urnetwork default
```

Turn boot start back on later:

```sh
rc-update add urnetwork default
rc-service urnetwork start
```

Uninstall, as root:

```sh
curl -fSsL https://dl.fullbars.xyz/uninstall.sh | sh
```

This stops the service, removes `/etc/init.d/urnetwork`, its `default` runlevel entry and any auto-update entry, and deletes the install directory. It also deletes `/home/urnet/.urnetwork`, which holds the login token, so you would need a new auth code to install again. It leaves two things behind: the `urnet` user and the log files. Remove them if you want a clean slate:

```sh
deluser urnet
rm -f /var/log/urnetwork.log /var/log/urnetwork.err
```

### Updates and auto-update

Updating is a root action on OpenRC, the same way `systemctl restart` is for a system service under systemd. Restarting an OpenRC service needs root, and so does replacing the root-owned binaries, so run the update from a root shell:

```sh
urnet-tools update
```

To have it run on a schedule, turn on auto-update as root. On OpenRC it is a busybox `crond` entry, not a systemd timer:

```sh
urnet-tools auto-update weekly
```

The interval can be `daily`, `weekly` or `monthly`, and `urnet-tools auto-update off` removes it. The entry only fires while `crond` is running. If `urnet-tools` reports that `crond` is not installed or not started, run:

```sh
apk add busybox-openrc
rc-update add crond default
rc-service crond start
```

### Limitations

- **Updates need root.** Every update restarts the service and replaces root-owned files, so run `urnet-tools update` from a root shell. There is no rootless update path on OpenRC.
- **No zero-downtime hotswap.** `supervise-daemon` cannot hand its supervised process over to a new one, so `urnet-tools update` stops the provider and starts it again, with a brief gap in service. See [HotSwap on OpenRC](HotSwap.md#not-available-under-openrc-alpine).
- **Several providers on one box.** `urnet-tools stop` and `restart` ask you to name the target when other providers run beside the service. See [OpenRC command parity](urnet-tools-go.md#openrc-alpine-command-parity).

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
irm https://dl.fullbars.xyz/install-win.ps1 | iex
```

Windows Defender may flag this one-liner. See the note below.

> [!NOTE]
> Windows Defender may flag the Windows install one-liner, and it may flag the downloaded
> binaries. What we see are machine-learning heuristics (the `!ml` suffix), not signatures;
> for the binaries we publish they are false positives. Recent release pages record the scan
> results for the published binaries. If Defender blocks the one-liner, download the script,
> review it, and run it from disk instead. If Defender quarantines an extracted binary,
> allow it from Windows Security > Virus & threat protection > Protection history. Both lines
> go in PowerShell:
>
> ```powershell
> irm https://dl.fullbars.xyz/install-win.ps1 -OutFile "$env:TEMP\install-win.ps1"
> powershell -NoProfile -ExecutionPolicy Bypass -File "$env:TEMP\install-win.ps1"
> ```

<details>
<summary>The detections you may see, and what each one means</summary>

- `Trojan:Script/Wacatac.B!ml`, `Trojan:Script/Wacatac.C!ml`: Defender's machine-learning label for the PowerShell installer script fetching and extracting a remote payload.
- `Trojan:Win32/Wacatac.B!ml`, `Trojan:Win32/Wacatac.C!ml`: Defender's machine-learning label for files whose shape looks like a packed trojan. Our Go binaries are stripped, statically linked and unsigned, which reads as a packed payload. The B and C variants are different model generations, so one binary can be flagged under more than one name. `Trojan:Win32/Execution.A!ml` is another label from the same family on some builds.
- `Trojan:Win32/Commando.A!ml`: fires on the download-and-run command line itself (the `irm ... | iex` one-liner), not on the installed files. Fetching a remote script and piping it into execution reads as a trojan-downloader pattern to the model.
- `Trojan:Win32/Bearfoos.A!ml`: a behavioural label for scheduled-task activity. The installer registers Task Scheduler tasks — a weekly update task (on `latest` installs) and, if you accept auto-start, a logon task so the provider starts at login — and a behavioural model cannot tell that apart from persistence malware.

</details>

Uninstall via PowerShell (no admin required):

```powershell
irm https://dl.fullbars.xyz/uninstall-win.ps1 | iex
```

### What gets installed

| Component | Location |
|-----------|----------|
| Provider binary | `%LOCALAPPDATA%\urnetwork\provider\windows\<arch>\urnetwork.exe` |
| Management tool | `urnet-tools` (Go binary, v3.23.0-fix.27.0+) |
| State directory | `%USERPROFILE%\.urnetwork\` |
| Startup (optional) | Task Scheduler logon task `urnetwork-autostart` |
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

### `urnet-tools` on Windows

`urnet-tools` runs natively on Windows. It needs no `tail`, no `sudo` and no WSL: the log views use a built-in follower that survives log rotation, `urnet-tools start` creates `%USERPROFILE%\.urnetwork` before it launches the provider, and the commands that would need `sudo` on Linux are skipped. Rotating, clearing or updating a log no longer fails with "being used by another process", because the follower opens it with delete sharing.

### 📦 Tarball Install (Alternative)

Starting with v3.23.0-fix.31.2, the Windows release tarball includes `urnet-tools` alongside the provider binary. If you prefer a manual or offline install:

1. Download the release tarball from [GitHub Releases](https://github.com/full-bars/urnetwork-3.23-fix/releases).
2. Extract the archive to your desired location (e.g. `%LOCALAPPDATA%\urnetwork`).
3. Open **PowerShell** and run the included install script:

```powershell
.\Provider_Install_Win32.ps1
```

This registers the PATH entry and optional Task Scheduler logon task — the same result as the CDN installer, but sourced entirely from the tarball. No internet access is required at install time. The script detects `amd64`/`arm64` automatically and places the correct binaries.

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
