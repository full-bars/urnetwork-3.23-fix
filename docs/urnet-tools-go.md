# urnet-tools (Go) — Provider-Aware Fleet Ops

> Applies to v3.23.0-fix.27.0+ (updated through v3.23.0-fix.30.6). The legacy shell tool (POSIX `Provider_Install_Linux.sh` + Windows `urnet-tools.ps1`) is replaced by a single provider-aware Go binary. Subcommand names and usage are preserved and expanded; what changed is **how the tool decides which provider it operates on**.

## Why this exists

The legacy `urnet-tools` resolved its target from a hardcoded path (`$HOME/.local/share/urnetwork-provider`) with zero awareness that other providers exist on the box. On a multi-provider machine it could act on the **wrong provider entirely** — and did (08-08 pool-wipe, 08-09 half-update). The Go rewrite makes the tool's single most important guarantee structural: **it never guesses which provider you mean.**

## The two binaries

| Binary | What it manages |
|---|---|
| `urnet-tools` | Process/systemd providers (`--proxy_file`, internal config, systemd units) |
| `urnet-docker` | Docker-deployed providers (discovers containers, delegates via `docker exec`) |

Both are cross-compiled from one Go source — the shell↔PowerShell drift is gone.

---

## 📋 Complete Command Reference

### Core & Lifecycle Commands

| Command | What it does |
|---|---|
| `providers` (`list`, `ps`) | List all providers on the box with JWT identities, systemd units, and state directories. |
| `status [target]` | Show detailed status. On Linux, displays live `systemctl status` view; on Windows/macOS, renders styled panel. |
| `start [target]` | Start provider service/process. |
| `stop [target]` | Stop provider service/process. |
| `restart [target]` | Restart provider service/process. |
| `hot-restart [target]` | Restart provider unit behind confirm gate (`-f` skips prompt). |
| `reinstall [target]` | Cleanly reinstall provider binary (delegates to latest updater). |
| `uninstall [target]` | Uninstall provider, unit files, and optional state. Confirm-gated. |
| `update [target]` | Update provider to the latest release (or `--tag <version>`). Digest-verified. |
| `self-update` (`selfupdate`) | Update the tool binary itself without touching running providers. |
| `logs [target] [N]` | Stream provider logs (N lines, default 250). RAMLOGS-aware. |
| `version` (`--version`, `-v`) | Print stamped binary version and build metadata. |

### Restored Provider & Session Commands (v3.23.0-fix.30.4+)

| Command | What it does |
|---|---|
| `auth <code> [target] [-f]` | Authenticate provider with an auth code. `-f` forces overwrite of existing JWT. Drops privileges to run as target user when called by root. |
| `choose-network <api> <connect> [target]` | Point provider to custom API and WebSocket signaling endpoints. Use `--reset` to restore default bringyour endpoints. |
| `fast-auth [on\|off\|status] [target]` | Toggle or check `~/.urnetwork/fast_auth` marker to bypass auth rate limiter. Confirm-gated. |
| `set [help \| <key> <val> \| <key> off \| <key>] [target]` | Get, set, or clear runtime provider state overrides (`node-name`, `report-interval`, `proxy-url-max`, `proxy-url-refresh`, `cleanup-scope`, `cleanup-interval`, `fast-auth`). Confirm-gated. |
| `session save <file> [target]` | Export encrypted AES-256-CBC bundle of provider JWT identity and state. Prompts for passphrase. |
| `session load <file> [target] [--allow-different-account]` | Decrypt and load identity bundle into provider. Automatically backs up current state first. Verifies account identity unless bypassed. |
| `self-heal [on\|off\|status] [target]` | Toggle or query resource-pressure self-healing monitor (`~/.urnetwork/proxy_self_heal`). |
| `default [set <target> \| show \| clear]` | Persist, inspect, or clear default provider target for current user in `os.UserConfigDir()/urnet-tools/default`. |

### Proxy Management Commands

| Command | What it does |
|---|---|
| `proxy add <file> [target]` | Merge proxies from text file (`host:port[:user:pass]`). |
| `proxy clear [target]` | Remove all proxies and URL sources. Confirm-gated (`-f` bypasses prompt). |
| `proxy remove [addresses...] [target]` | Remove specific proxies or patterns. Use `--match=<pattern>` for host substring matches, or `--all` for complete wipe. |
| `proxy trim <N> [target] [--preview]` | **(New in 30.4)** Set persistent hard cap of `<N>` running proxies. Sheds worst A-F reachability graded proxies first. `proxy trim off` clears the cap. |
| `proxy refresh [target] [--force]` | Reload proxy list into running provider without restarting. `--force` bypasses warmup lockout. |
| `proxy add-source <url> [target]` | Add live URL proxy source. Fetched and probed immediately. |
| `proxy remove-source <url> [target]` | Remove URL proxy source. |
| `proxy exclude [pattern] [--remove] [target]` | Manage persistent proxy exclusion list. |
| `proxy health [target]` | Display live health state (Up, Down, Dead, Degraded). |
| `proxy traffic [target]` | Display bandwidth, billable traffic, and active NAT sessions per proxy. |
| `proxy remove-dead [target]` | Interactively prune dead and degraded proxies. Honors `--dry-run`. |
| `proxy summary [target]` | Fleet-style summary of proxy counts by source (url, file, internal). |

### Hub Command Family (v3.23.0-fix.30.4+)

| Command | What it does |
|---|---|
| `hub init` | Initialize and configure the bandwidth hub service on this machine. Prompts for password (min 8 chars) or reads from stdin. |
| `hub link <url>` | Pair provider with a remote bandwidth hub. Verifies TLS CA or SHA-256 certificate fingerprint (TOFU security). Confirm-gated on identity change. |
| `hub unlink` | Unlink provider from bandwidth hub. |
| `hub test` | Test reachability and TLS certificate chain validation to the configured hub. |
| `hub onboard-cmd` | Generate one-line onboarding command with URL-escaped tokens for remote providers. |
| `hub show-password` | Display current hub admin access password. |
| `hub open-port` | Configure firewall rules (ufw/iptables/firewalld) to open hub listener port. Confirm-gated. |
| `hub update` | Update bandwidth hub binary to latest release. |
| `hub set <url>` | Set raw reporting endpoint URL in `~/.urnetwork/report_url`. |
| `hub off` | Disable hub reporting by clearing `report_url`. |
| `hub install` | Install bandwidth hub binary and systemd service. |

### System & Performance Tuning

| Command | What it does |
|---|---|
| `auto [on\|off]` | Enable or disable Smart Auto hardware profile. |
| `optimize [-f]` | Tune kernel parameters (conntrack, socket buffers, port ranges, BBR). Platform-aware. |
| `eco [on\|off]` | Enable or disable Eco profile (RAM-constrained hosts). |
| `turbo [v4\|v8\|off]` | Enable Turbo V4 or Turbo V8 high-throughput modes. |
| `ramlogs [on\|off]` | Enable or disable RAM-disk logging (`/dev/shm`). |
| `report <url>` | Set live bandwidth hub reporting URL (`report off` disables). |

---

## 🎯 Targeting & Selectors

The tool accepts selectors in both space-separated and equals-separated format (`--flag value` or `--flag=value`):

| Flag | Selects by | Example |
|---|---|---|
| `--unit <name>` or `--unit=<name>` | systemd unit name (system or user) | `--unit=urnetwork-native.service` |
| `--user <user>` or `--user=<user>` | OS user running the provider | `--user=urnet` |
| `--network <name>` or `--network=<name>` | JWT network name (account) | `--network=alpha-fleet` |
| `--network-id <id>` or `--network-id=<id>` | JWT network ID (for identical network names) | `--network-id=net_94f8a...` |
| `--state-dir <path>` or `--state-dir=<path>` | Explicit state directory | `--state-dir=/home/urnet/.urnetwork` |

### Targeting Rules
1. **Multi-provider box + no target = REFUSAL.** The tool errors and displays an inventory table of available providers. It never guesses.
2. **Single provider + no target = AUTO-SELECT.** Proceeds after echoing the selected target.
3. **Persisted Default Provider:** If configured via `urnet-tools default set <target>`, the tool uses this target when no flag is passed, printing a visible notice to stderr.
4. **Explicit flags and `--all` override default:** `--unit`, `--user`, `--network`, etc., take precedence over persisted defaults.
5. **Conflicting selectors** (e.g. `--unit foo --network bar` pointing to different instances) = ERROR.
6. **`-f` / `--force` only skips confirmation prompts:** It **never** selects a provider. To target all providers with force, use `-f --all`.
7. **`--help` always prints help** and never executes actions.

---

## 🔧 Deep-Dive: Key Restored & New Features

### 1. Persistent Proxy Trim (`proxy trim <N>`)

`urnet-tools proxy trim <count>` sets a persistent hard cap on the number of running proxies:

```bash
# Preview what proxies would be shed without making changes
urnet-tools proxy trim 500 --preview

# Set running proxy cap to 500
urnet-tools proxy trim 500

# Remove the cap
urnet-tools proxy trim off
```

- **A-F Grade Ranking:** Sheds worst-graded proxies first using the provider's website-reachability probe scores (`dead` → `never-graded` → `F` → `D` → `C` → `B` → `A`).
- **Traffic Tiebreaker:** Proxies with active billable bandwidth are shed last within their grade tier, preserving active earning connections.
- **Persistence:** Stored at `~/.urnetwork/proxy_trim`, surviving provider restarts and reloads.
- **AIMD Integration:** Clamps the AIMD pool controller `TargetPoolSize` so automated pressure management works within the hard cap.

### 2. Session Save and Load

Securely backup, migrate, or clone provider identities:

```bash
# Save encrypted identity bundle (AES-256-CBC, prompts for password)
urnet-tools session save /path/to/backup.urnsession

# Load identity bundle (automatically backs up existing state directory first)
urnet-tools session load /path/to/backup.urnsession

# Load onto a host with a different account identifier
urnet-tools session load /path/to/backup.urnsession --allow-different-account
```

- **Pre-Load Safety Backup:** Automatically creates a timestamped copy of `~/.urnetwork/` (e.g. `~/.urnetwork.bak.1724288000`) before modifying live files.
- **Permission Hardening:** Unpacks files with `0700` directory permissions and `0600` file permissions, automatically chowning them to the unit owner when run with elevated privileges.

### 3. Hub Setup and Verification

Full lifecycle management for centralized bandwidth reporting:

```bash
# Initialize hub service on the server
urnet-tools hub init

# Link a provider to the hub with TOFU TLS verification
urnet-tools hub link https://hub.example.com:8080

# Verify certificate chain and reachability
urnet-tools hub test

# Open firewall ports for hub traffic
urnet-tools hub open-port
```

- **TLS Pinning:** Verifies TLS against `hub_ca.pem` or pinned SHA-256 public key fingerprints.
- **Identity Safety:** Prompts with typed confirmation if linking would overwrite existing provider hub registration.

### 4. Persisted Default Provider

Avoid passing `--unit` or `--network` on every invocation:

```bash
# Set default provider by unit name
urnet-tools default set --unit urnetwork.service

# View current default
urnet-tools default show

# Clear default
urnet-tools default clear
```

---

## 🔒 Safety & Security Guarantees

- **Mandatory Digest Verification:** `update` verifies downloads against release API SHA-256 checksums.
- **Isolated Staging:** Temporary files created in private `0700` directories.
- **Atomic Binary Replacement:** New executables staged as temporary files and renamed into place, preventing truncation of running binaries.
- **Privilege Separation:** Delegated commands automatically drop root privileges to the provider's UID/GID.

---

## 📦 Getting the Tool

Install via the download domain:

```bash
# Docker host tool (urnet-docker)
curl -fSsL https://dl.fullbars.xyz/urnet-docker.sh | sh

# Process/systemd tool (urnet-tools)
curl -fSsL https://dl.fullbars.xyz/urnet-docker.sh | sh -s -- urnet-tools
```

Fallback to GitHub raw sources if the download domain is unavailable:

```bash
curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts/install-urnet-docker.sh | sh
```
