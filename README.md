# ⛓ UrNetwork 3.23-fix

[![CodeRabbit Pull Request Reviews](https://img.shields.io/coderabbit/prs/github/full-bars/urnetwork-3.23-fix?utm_source=oss&utm_medium=github&utm_campaign=full-bars%2Furnetwork-3.23-fix&labelColor=171717&color=FF570A&link=https%3A%2F%2Fcoderabbit.ai&label=CodeRabbit+Reviews)](https://coderabbit.ai)
[![CI](https://github.com/full-bars/urnetwork-3.23-fix/actions/workflows/build.yml/badge.svg)](https://github.com/full-bars/urnetwork-3.23-fix/actions)
![Go Version](https://img.shields.io/github/go-mod/go-version/full-bars/urnetwork-3.23-fix?labelColor=171717&color=FF570A)
![Release](https://img.shields.io/github/v/release/full-bars/urnetwork-3.23-fix?labelColor=171717&color=FF570A)
![Language](https://img.shields.io/github/languages/top/full-bars/urnetwork-3.23-fix?labelColor=171717&color=FF570A)
![Activity](https://img.shields.io/github/commit-activity/m/full-bars/urnetwork-3.23-fix?labelColor=171717&color=FF570A)

A fork of the **UrNetwork Connect** provider, tuned for people who run many proxies at high throughput. It adds clear logs, automatic tuning, a single management tool (`urnet-tools`), and Bittensor Subnet 25 support. It is the production line of this project.

## 👋 New here? Do this

1. **Install.** Pick your system in [Quick Start](#-quick-start) below. On Linux the one-liner is `curl -fSsL https://dl.fullbars.xyz/install.sh | sh`.
2. **Log in.** Run `urnetwork auth` and paste your auth code from the UrNetwork app when asked.
3. **Add proxies (optional).** `urnet-tools proxy add ~/proxies.txt`, then `urnet-tools proxy refresh`. See [Adding Proxies](docs/Adding-Proxies.md).
4. **Check that it works.** Run `urnet-tools status`. It shows whether the provider is running. Run `urnet-tools logs` to watch it start. For a live screen, run `urnet-tools top`.
5. **Get paid on Bittensor (optional).** Sign one wallet message, once, from your own computer. Your nodes do not change. See the [Bittensor Quick Start](docs/Bittensor-Quick-Start.md).
6. **Update.** Run `urnet-tools update`. It asks before it changes anything. Docker users run `urnet-docker update` on the host.

If something looks wrong, start with [Troubleshooting](docs/Troubleshooting.md). If you would rather follow a guide, pick one from the next section.

## 🗺 Start Here

| If you want to... | Go here |
| :--- | :--- |
| **Get paid on Bittensor SN25 (do I need to do anything?)** | [Bittensor Quick Start](docs/Bittensor-Quick-Start.md) |
| **Start here — pick your skill level** | [🐣 Beginner](docs/guides/beginner.md) · [🧭 Intermediate](docs/guides/intermediate.md) · [🚀 Advanced](docs/guides/advanced.md) |
| Install on a Linux host as a user-level service | [Installation Guide](docs/Installation.md) |
| Install on FreeBSD as an rc.d service | [FreeBSD Guide](docs/FreeBSD.md) |
| Run one Docker container | [Docker Deployment](docs/Docker-Deployment.md) |
| Run multiple containers on one host | [Multi-Container Scaling](docs/Multi-Container-Scaling.md) |
| Choose profiles, turbo mode, or host tuning | [Performance Tuning](docs/High-Volume-Performance-Tuning.md) |
| Understand environment variables | [Configuration Reference](docs/Configuration.md) |
| Interpret provider logs | [Log Message Reference](LOG_REFERENCE.md) |
| Track traffic usage (billable vs control overhead) | [Docker Deployment](docs/Docker-Deployment.md) · [CLI Reference](docs/urnet-tools-go.md) |
| Load a proxy file into the provider (per-OS) | [Adding Proxies](docs/Adding-Proxies.md) |
| Feed the provider a live proxy list URL | [Proxy URL Sources](docs/Proxy-URL-Sources.md) |
| Understand IPv4/IPv6 support and what changes per proxy | [Dual-Stack](docs/Dual-Stack.md) |
| Import into a Pelican game-server panel | [Pelican Panel](docs/Docker-Deployment.md#-pelican-panel) · [Egg README](pelican/README.md) |

---

## ₿ Bittensor Subnet 25

Most operators need to do one thing: sign a single wallet message, once, from their own computer. Your nodes do not change. Everything else here (hotkeys, claim daemons, fleets, multiple operators) is optional or advanced, and skipping it costs you nothing. Nothing here runs unless you start it.

- [Bittensor Quick Start](docs/Bittensor-Quick-Start.md): start here. Check where you stand, sign once, and see which commands you can ignore.
- [Bittensor operations](docs/Bittensor-Operations.md): the overview, from the two tiers and wallet consent through claims and the FAQ.
- [Bittensor head tier](docs/Bittensor-Head-Tier.md): the optional, advanced tier. Most operators should skip it.
- [Bittensor glossary](docs/Bittensor-Glossary.md): plain meanings of the words.
- [Bittensor claim daemons](docs/Bittensor-Claim-Daemons.md): automatic epoch claims, config key by key.
- [Bittensor fleet](docs/Bittensor-Fleet.md): the head-tier lifecycle from one manifest.
- [Bittensor hotkey delegation](docs/Bittensor-Hotkey-Delegation.md): one coldkey signature pays the miner's network on every operator.
- [Multi-operator providers](docs/Multi-Operator.md): one provider per operator from one supervisor.

---

## ⚡ Quick Start

### Install

**🐧 Linux (systemd)**

```sh
curl -fSsL https://dl.fullbars.xyz/install.sh | sh
```

**🐡 FreeBSD (rc.d)**

```sh
fetch -qo urnet-install.sh https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts/Provider_Install_FreeBSD.sh && \
  sudo sh urnet-install.sh install
```

The FreeBSD installer downloads the provider and installs an `rc.d` service. See [docs/FreeBSD.md](docs/FreeBSD.md).

**🍎 macOS (launchd)**

```sh
curl -fSsL https://dl.fullbars.xyz/install-mac.sh | sh
```


**🐋 Docker**

```sh
docker pull ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

**🐋 Docker Wrapper (Optional but Recommended -- For docker deployments, install this to control and interact with your container)**

```sh
curl -fSsL https://dl.fullbars.xyz/urnet-docker.sh | sh
```

**🪟 Windows (PowerShell)**

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

### After you install

After installation, authenticate and start providing:

```bash
# Linux / macOS / FreeBSD: one Go binary on every platform
urnetwork auth
urnet-tools proxy add ~/proxies.txt
urnet-tools proxy refresh
urnet-tools auto on
```
On Windows, run the same commands in PowerShell but use a Windows path, e.g.
`urnet-tools proxy add "$env:USERPROFILE\Downloads\proxies.txt"`. Full per-OS
walkthrough, including the `.txt.txt` extension trap: [Adding Proxies](docs/Adding-Proxies.md).

> [!NOTE]
> Since v3.23.0-fix.27.0, `urnet-tools` is a provider-aware Go binary (the legacy POSIX shell + PowerShell variants are retired). It discovers every provider on the box and **refuses to act on an ambiguous target** — on multi-provider machines, pass `--unit` / `--user` / `--network` / `--network-id` / `--state-dir`. See [docs/urnet-tools-go.md](docs/urnet-tools-go.md).
>
> Docker-only deployments: the provider runs in a container, but the management tool (`urnet-docker`) runs **on the docker host, outside the container**. Install it with the one-liner above (use `curl -fSsL https://dl.fullbars.xyz/urnet-docker.sh | sh -s -- urnet-tools` for the systemd variant; GitHub fallback: `curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts/install-urnet-docker.sh | sh` (use `| sh -s -- urnet-tools` for the systemd variant)). The tool self-updates afterward (`urnet-docker update`).

### Uninstall

**🐧 Linux**

```sh
curl -fSsL https://dl.fullbars.xyz/uninstall.sh | sh
```

**🍎 macOS**

Manual — see [docs/Installation.md](docs/Installation.md).

**🐡 FreeBSD**

Manual — see [FreeBSD Guide](docs/FreeBSD.md#uninstall).

**🪟 Windows (PowerShell)**

```powershell
irm https://dl.fullbars.xyz/uninstall-win.ps1 | iex
```

**🐋 Docker**

```sh
docker rm -f <container> && docker rmi ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

**🐋 Docker (management wrapper)**

```sh
rm /usr/local/bin/urnet-docker   # root install
rm ~/.local/bin/urnet-docker     # non-root install
```

### 🐋 Docker (Production-Ready)

Recommended for real deployments: includes auto-tuning, in-memory logs, and persistent config with zero listening ports:

```bash
docker run -d \
  --name=urnetwork-provider \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  -e AUTH=authcode \
  -e AUTHCODE='YOUR_AUTH_CODE_HERE' \
  -e URNETWORK_PROFILE=auto \
  -e URNETWORK_RAMLOGS=1 \
  -e HOST_HOSTNAME=$(hostname) \
  -e PROXY_URL='https://example.com/your-proxy-list.txt' \
  -v urnetwork_data:/root/.urnetwork \
  -v /path/to/proxy.txt:/app/proxy.txt \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

**Key env vars:**
- `AUTH=authcode` - Authentication mode: `authcode` (with `AUTHCODE`) or `userpass` (with `USER_AUTH` and `PASSWORD`).
- `AUTHCODE` - One-time auth code from https://ur.io (single-use on first run; saved to volume).
- `URNETWORK_PROFILE=auto` - Auto-tunes based on available RAM (balanced, lowmem, etc.).
- `URNETWORK_RAMLOGS=1` - In-memory logging for fast diagnostics (view with `urnet-docker logs`).
- `PROXY_URL` - Optional live proxy list URL (comma-separated for multiple), additive with the mounted `proxy.txt`. See [Proxy URL Sources](docs/Proxy-URL-Sources.md).
- `UR_API_URL` / `UR_CONNECT_URL` - Point at a custom API + connect backend instead of `bringyour.com`. Must be set together; saved to the `~/.urnetwork` volume so it survives restarts. See [Configuration Reference](docs/Configuration.md).

> [!TIP]
> Providers do not require any listening ports. Outbound connections handle all traffic (vnStat has been retired; use Prometheus metrics via `URNETWORK_METRICS` for telemetry).

See [Docker Deployment](docs/Docker-Deployment.md) for Docker Compose, email/password auth, Watchtower, multi-container, and advanced options.

> [!NOTE]
> **Docker Management**: Use `urnet-docker` directly on the host (no `docker exec` wrapping needed):
> - `urnet-docker status`
> - `urnet-docker direct status` (or `urnet-docker direct off` for proxy-only mode)
> - `urnet-docker usage` (or `urnet-docker usage graphs` for time-series history)
> - `urnet-docker proxy add ~/proxies.txt`
> - `cat proxies.txt | urnet-docker proxy paste`
> - `urnet-docker proxy traffic`

---

## 🆚 What this fork changes vs upstream

| | Upstream | This fork |
| :--- | :--- | :--- |
| Control-plane dial visibility | One `Debug(2)` line per backend dial (`[net][s]select`) | The same line promoted to INFO, plus dial time and a count of suppressed repeats (`dur=…ms`, `(N suppressed)`) |
| Contract sizing | Fixed 1 MiB initial, 4-step ramp | Initial size follows the memory tier (128 KiB on small boxes, 256 KiB mid-range, 2 MiB on large ones) with a faster 3-step ramp |
| Proxy startup | All at once | Jittered stagger with live `[pace]` warmup, plus a shared adaptive rate limiter that bounds aggregate auth load on the API |
| Proxy changes | Restart required | Hot-reload via trigger file, zero downtime, with full added-proxy listing |
| Dead proxy handling | Retry forever (15 min loop, no ceiling) | 24 h daily retry, 14-day drop (file) or 65 min cleanup (URL), persisted state |
| Proxy source | Static file only | File and/or live URL feed, with scoped auto-cleanup |
| Error noise | Log-level throttle (suppresses repeated lines) | Shared auth rate limiter reduces the error source itself — fewer API calls hit the failure path |
| Proxy health grading | None | A–F reachability grade per proxy with continuous re-probing (`proxy health`, `proxy trim`) |
| Fleet visibility & accounting | None | Built-in CLI accounting (`usage`, `proxy traffic`), persistent byte splits, and Prometheus metrics (`urnet-tools metrics on`) |
| Performance profiles | None | Auto / Turbo V4 / Turbo V8 / Eco / Lowmem — memory, window, and GC tuned per profile |
| Crash diagnostics | Journal-only, logs lost on restart | Optional RAM logs (`URNETWORK_RAMLOGS=1`) plus a disk-based critical event log and panic hooks |
| Custom API/connect backend | One-off `--api_url`/`--connect_url` flags only, re-passed on every invocation | `choose_network` persists the URLs to disk; flags still override per-call |
| Runtime settings | Edit systemd drop-ins by hand, then restart | Live control socket (`~/.urnetwork/provider.sock`); changes apply without a restart and are queued in `pending_overrides.json` when the provider is stopped |
| Binary upgrade | Stop, swap, start (20–60 s of downtime) | HotSwap handoff to a verified candidate, with automatic rollback: no gap where no provider process is running, though proxy connections still ramp back over about 30 s (requires a `Type=notify` unit, which `urnet-tools update` sets up; see [docs/HotSwap.md](docs/HotSwap.md)) |
| Node identity on the dashboard | Hostname only | `rename` sets the display label and `show-ip` appends the public IP, both without a restart |
| Multi-provider boxes | One provider per host, no targeting | One provider per OS user, with `providers` / `providers --all` inventory and cross-user `sudo` self-elevation |
| Session migration | None | `session save` / `session load` exports identity + proxy state as an encrypted bundle for cross-machine transfer |
| Subnet 25 telemetry | None | `sn-status` command with STSubnet operations guide, wallet registration, and head-fleet tiering docs |
| Subnet 25 claims | Network token only | `provider claim` takes an explicit client credential (`--store-client`, `--provider-jwt` or `--legacy-coldkey`), and a failed renewal no longer replaces a client identity |
| Bittensor command families | None | The v33 port adds `provider fleet` (manifest, register, publish, bind, sign, status, revoke), `provider wallet hotkey` (challenge, set, status), `provider claim-daemon` and `provider claim-daemon-v2`, `provider operators`, and `provider provide --all-operators`; see docs/Bittensor-Operations.md |
| Memory-thrash recovery | None | Swap-thrash watchdog with a supervised exit-75 restart (systemd, OpenRC and containers all restart it; self-heal is the one switch) |
| Windows operations | Not provided | `urnet-tools` runs natively on Windows: no `tail`, `sudo` or WSL, and its log follower survives rotation |

---

## 🔧 Common Operations

| Command | Use this when... |
| :--- | :--- |
| `urnetwork auth` | You need to log in or refresh your identity manually |
| `urnet-tools direct [on\|off\|status]` | You want to check or toggle native direct IP providing (`off` = proxy-only stealth) |
| `urnet-tools usage [graphs]` | You want to view persistent traffic accounting (billable relay vs control plane split) |
| `urnet-tools proxy add <path\|url>` | You want to add proxies from a file (straight path or `--file=`) or URL source |
| `urnet-tools proxy paste < file` | You want to stream raw proxies from stdin or a pipe into the provider |
| `urnet-tools proxy trim <count>` | You want to enforce a hard cap, shedding worst A-F reachability graded proxies first |
| `urnet-tools proxy traffic` | You want to see active clients, bandwidth, and **Max Age** per proxy |
| `urnet-tools proxy health` | You need to see which proxies are `DEAD` vs `DEGRADED` vs `UP` |
| `urnet-tools logs` | You want to stream the current RAMLOGS buffer |
| `urnet-tools optimize` | You just added many proxies and need to tune kernel `ulimits` |
| `urnet-tools summary` | You want a single-pane fleet overview -- sources, health, URL cache status |
| `urnet-tools proxy refresh` | You updated your proxy list and want the node to reload live |
| `urnet-tools set hot-restart on/off` | Toggle client JWT reuse across restarts (on by default; there is no standalone `hot-restart` command). See [docs/Hot-Restart.md](docs/Hot-Restart.md) |
| `urnet-tools set [<key> [<value>]]` | Show or change a runtime tuning override live, without editing a drop-in or restarting |
| `urnet-tools hotswap` | Swap to an updated binary without a gap where no process is running (needs a `Type=notify` unit; otherwise `update` falls back to a restart). Proxy connections still ramp back over about 30 s. Procedure and measured costs: [docs/HotSwap.md](docs/HotSwap.md) |
| `urnet-tools config [--json]` | Show every provider setting with the source it came from, so you can see which writer won |
| `urnet-tools history [limit]` | Read the provider's command audit trail, now including lifecycle events (start, hotswap, shutdown) alongside `set`/`clear` changes |
| `urnet-tools top` (or `urtop`) | You want a live full-screen view of one provider: throughput, clients, proxies, memory and recent events |
| `urnet-tools dashboard` | Terminal status panel: state, active settings, proxy sources, restart warnings |
| `urnet-tools metrics on/off` | Toggle the Prometheus `/metrics` endpoint live, no restart |
| `urnet-tools profile [name]` | Show or set the memory and GC tuning profile |
| `urnet-tools proxy ids` | Show the `client_id` the platform assigned to each proxy, including `direct` |
| `urnet-tools rename <name>` | Set the dashboard display label without touching the hostname |
| `urnet-tools show-ip [on\|off\|status]` | Control whether the public IP is appended to that dashboard label (was `ip-detect`) |
| `urnet-tools providers [--all]` | List the providers on this box; `--all` (as root) covers every OS user |
| `urnet-tools session save <file>` | Export identity+proxy state as encrypted bundle (cross-machine transfer) |
| `urnet-tools session load <file>` | Import identity+proxy state, then restart |
| `urnetwork choose_network <api_url> <connect_url>` | You run your own API/connect backend and want the provider to default to it |
| `urnetwork choose_network --reset` | You want to clear a saved custom network and revert to the main network |
| `urnetwork choose_network --show` | You want to see which network is in effect (the saved one or the main network); it warns when the saved URLs are unencrypted, and the "jwt issued by the previous network" warning appears only when the effective API URL actually changes |
| `urnetwork auth --operator=<domain>` | You want to write one listed operator's network token (auth code, user/password, or `--hotkey_seed_file`) |
| `urnetwork provide --all-operators --auto-register --hotkey_seed_file=<path>` | You want one provider per listed operator, signing in with the hotkey for every operator that has no token yet |
| `urnetwork operators` | You want the published operator list with each operator's credential state |
| `urnetwork snclaim <args>` | You want the low-level settlement-vault tool (v1 submit/status/withdraw, v2 status/claim/check); `urnetwork snclaim --help` prints its usage |

> [!TIP]
> `~/proxies.txt` and `/home/user/proxies.txt` are both valid straight path formats across all tools.

---

## 📡 Telemetry & Fleet Visibility

UrNetwork Connect provides rich standalone metrics directly via `urnet-tools usage` and `urnet-docker usage` (billable vs control plane accounting with hour/day/month historical graphs).

Prometheus metrics are built in. `urnet-tools metrics on` turns them on without a
restart and prints the address to scrape: loopback plus the machine's Tailscale
address by default, or any address you choose with `urnet-tools metrics listen`.
The `monitoring/` bundle runs Prometheus and Grafana with a ready-made fleet
dashboard. See [Monitoring](docs/Monitoring.md).

> [!NOTE]
> **Fleet Dashboard (Removed):** The multi-node aggregation hub was removed in v32.0. For fleet-wide visibility, use Prometheus metrics (`urnet-tools metrics on`) with the [Monitoring bundle](docs/Monitoring.md), or Grafana for custom dashboards.

---

## 💡 Recommended Defaults

- Use the **Linux installer** (systemd) or the **FreeBSD installer** (rc.d) for a host-managed service
- Use **Docker** if you prefer containers — both are fully documented
- Leave profile on **`auto`** unless you have a specific reason to override
- Mount `/root/.urnetwork` as a persistent volume in Docker deployments
- Run `urnet-tools optimize` after adding a large proxy list, or when the System Auditor flags kernel limits

---

## 📚 Documentation

**In-repo:**

- [Installation](docs/Installation.md)
- [Docker Deployment](docs/Docker-Deployment.md)
- [Multi-Container Scaling](docs/Multi-Container-Scaling.md)
- [Configuration Reference](docs/Configuration.md)
- [Node Identity & Dashboard Label](docs/Node-Identity.md)
- [Proxy Management & Hot-Reload](docs/Proxy-Management.md)
- [High-Volume Performance Tuning](docs/High-Volume-Performance-Tuning.md)
- [Monitoring](docs/Monitoring.md)
- [Project Structure](docs/Project-Structure.md)
- [Log Message Reference](LOG_REFERENCE.md)
- [Go urnet-tools Reference](docs/urnet-tools-go.md)
- [Bittensor Quick Start](docs/Bittensor-Quick-Start.md) · [Bittensor Operations](docs/Bittensor-Operations.md) · [Head Tier](docs/Bittensor-Head-Tier.md) · [Glossary](docs/Bittensor-Glossary.md)
- [Bittensor Claim Daemons](docs/Bittensor-Claim-Daemons.md) · [Bittensor Fleet](docs/Bittensor-Fleet.md) · [Bittensor Hotkey Delegation](docs/Bittensor-Hotkey-Delegation.md) · [Multi-Operator](docs/Multi-Operator.md)
- [Changelog](CHANGELOG.md)

**Wiki:**

- [Online GitHub Wiki](https://github.com/full-bars/urnetwork-3.23-fix/wiki)
- [CI and Release Process](https://github.com/full-bars/urnetwork-3.23-fix/wiki/CI-and-Release-Process)
- [Bittensor Quick Start](https://github.com/full-bars/urnetwork-3.23-fix/wiki/Bittensor-Quick-Start) · [Bittensor Operations](https://github.com/full-bars/urnetwork-3.23-fix/wiki/Bittensor-Operations) · [Head Tier](https://github.com/full-bars/urnetwork-3.23-fix/wiki/Bittensor-Head-Tier) · [Glossary](https://github.com/full-bars/urnetwork-3.23-fix/wiki/Bittensor-Glossary)
- [Bittensor Claim Daemons](https://github.com/full-bars/urnetwork-3.23-fix/wiki/Bittensor-Claim-Daemons) · [Bittensor Fleet](https://github.com/full-bars/urnetwork-3.23-fix/wiki/Bittensor-Fleet) · [Bittensor Hotkey Delegation](https://github.com/full-bars/urnetwork-3.23-fix/wiki/Bittensor-Hotkey-Delegation) · [Multi-Operator](https://github.com/full-bars/urnetwork-3.23-fix/wiki/Multi-Operator)

---

## 🏗 Build Info

- **Base engine:** UrNetwork v3.23, with the v2026 engine changes ported in (v34)
- **Language:** Go 1.27.3, built on Alpine
- **Platforms:** Linux (systemd, and Alpine/OpenRC with a native boot-start service), FreeBSD (amd64 and arm64), macOS and Windows. FreeBSD setup: [docs/FreeBSD.md](docs/FreeBSD.md). Alpine setup: [docs/OpenRC.md](docs/OpenRC.md)
- **Images:** Multi-arch `linux/amd64` + `linux/arm64` via GitHub Actions → GHCR. Each release also attaches `darwin/amd64` + `darwin/arm64`, `windows/amd64` + `windows/arm64` and `freebsd/amd64` + `freebsd/arm64` binaries.
- **Bridge-friendly:** runs on standard Docker bridge networks, no `--network host` required

---

> [!WARNING]
> This is a private, custom modification for professional provider use. Not affiliated with the official UrNetwork project.
