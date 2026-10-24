# urnet-tools (Go) — Provider-Aware Fleet Ops

> Applies to v3.23.0-fix.27.0+ (updated through v3.23.0-fix.34.2). The legacy shell tool (POSIX `Provider_Install_Linux.sh` + Windows `urnet-tools.ps1`) is replaced by a single provider-aware Go binary. Subcommand names and usage are preserved and expanded; what changed is **how the tool decides which provider it operates on**.

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
| `providers` (`list`, `ps`) | List providers: your own OS user's by default, or all providers on the box with `--all` (JWT identities, systemd units, state dirs). |
| `status [target]` | Show detailed status. On Linux, displays live `systemctl status` view; on Windows/macOS, renders styled panel. The live block's pressure row prints the provider's one-sentence pressure summary (the `summary` from `~/.urnetwork/pressure_status`) when the provider writes it; older providers show the bare score. |
| `start [target]` | Start provider service/process. |
| `stop [target]` | Stop provider service/process. |
| `restart [target]` | Restart provider service/process. |
| `reinstall [target]` | Cleanly reinstall provider binary (delegates to latest updater). |
| `uninstall [target]` | Uninstall provider, unit files, and optional state. Confirm-gated. |
| `update [target]` | Update provider to the latest release (or `--tag <version>`). Digest-verified. |
| `hotswap` (`hot-swap`) | Zero-downtime in-process binary reload: hands live service to a verified candidate with no restart. Requires a `Type=notify` unit; see the deep-dive below. |
| `self-update` (`selfupdate`) | Update the tool binary itself without touching running providers. |
| `logs [target] [N]` | Stream provider logs (N lines, default 250). RAMLOGS-aware. The ramlog and file views use a native follower, so they work on Windows (no `tail` needed) and wherever `tail` is missing. It prints the last N lines and then follows. If the log is rotated or replaced it finishes the old file and follows the new one from its start, and a truncated log is followed from its start. |
| `top [target] [--interval D]` (also `urtop`) | Live full-screen view of one provider: throughput graphs, clients, proxy pool, memory and recent events. Read-only. Press `?` for keys, `q` to quit. Needs an interactive terminal; use `status` in scripts. |
| `get [key] [target]` (alias `show`) | Read-only view of the runtime overrides. With no key it lists every active override; pass one key to see its value. Use `set` to change anything. |
| `version` (`--version`, `-v`) | Print stamped binary version and build metadata. |

### Restored Provider & Session Commands (v3.23.0-fix.30.4+)

| Command | What it does |
|---|---|
| `auth <code> [target] [-f]` | Authenticate provider with an auth code. `-f` forces overwrite of existing JWT. Drops privileges to run as target user when called by root. |
| `direct [on\|off\|status] [target]` | Toggle or report direct/local IP providing state. Taking effect immediately via reload. Available across `provider`, `urnet-tools`, and `urnet-docker`. |
| `show-ip [on\|off\|status] [target]` | Control whether the provider appends its public IP to the dashboard label set by `rename`. Renamed from `ip-detect` in v3.23.0-fix.31.0, which is kept as an alias. This is about what the dashboard shows, not which address the provider serves on; for that see `direct`. |
| `sn-status [--json] [target]` | Query and display Subnet 25 mining & node telemetry (v3.23.0-fix.30.9+): global rank, top-200 tier eligibility, net bandwidth provided, registered coldkey (SS58/Hex), the network wallet consent (apart from any legacy login-proof wallet; with none it says to sign one with `urnet-tools wallet sign`), current subnet epoch blocks, and finalized epoch pool payout share. Available across `urnet-tools`, `urnet-docker`, and `provider`. |
| `usage [graphs\|graph <view>] [target]` | Display traffic & billing accounting: billable relay bytes vs control-plane protocol overhead, with rolling time-series summaries. Available across `urnet-tools` and `urnet-docker`. |
| `choose-network <api> <connect> [target]` | Point provider to custom API and WebSocket signaling endpoints. Use `--reset` to restore default bringyour endpoints. |
| `fast-auth [on\|off\|status] [target]` | Toggle or check `~/.urnetwork/fast_auth` marker to bypass auth rate limiter. Confirm-gated. |
| `set [help \| <key> <val> \| <key> off \| <key>] [target]` | Get, set, or clear a runtime provider override over the control socket. `urnet-tools set help` lists every key with its value domain and what it changes. The keys are `node-name`, `fast-auth`, `self-heal`, `proxy-url-max`, `proxy-url-refresh`, `cleanup-scope`, `cleanup-interval`, `hot-restart`, `oom-cap`, `smart-dialer`, `gomemlimit`, `gogc`, `profile`, `ramlogs`, `metrics`, `metrics-listen`, `h3`, `h3-datagram`, `h3-datagram-send`, `baseline` and `proxy-audit`. Most apply live on the next tick; `profile`, `ramlogs` and `smart-dialer` need a restart. Queued to `pending_overrides.json` if the provider is stopped. Confirm-gated. |
| `rename <name> [target]` | Set the dashboard identity label on the backend. Alias for `set node-name <name>`. Writes `~/.urnetwork/node_name`, re-read on next tick — no restart. Use `off` to clear. Available across `urnet-tools` and `urnet-docker`. |
| `session save <file> [target]` | Export encrypted AES-256-CBC bundle of provider JWT identity and state. Prompts for passphrase. |
| `session load <file> [target] [--allow-different-account]` | Decrypt and load identity bundle into provider. Automatically backs up current state first. Verifies account identity unless bypassed. |
| `self-heal [on\|off\|status] [target]` | Toggle or query resource-pressure self-healing monitor (`~/.urnetwork/proxy_self_heal`). |
| `default [set <target> \| show \| clear]` | Persist, inspect, or clear default provider target for current user in `os.UserConfigDir()/urnet-tools/default`. |

#### OpenRC (Alpine) command parity

On a host where OpenRC is the running init system, the lifecycle commands (`start`, `stop`, `restart`, `auto-start`, `auto-update`, `logs`, `status`, `uninstall`) act on the installer's `urnetwork` service. The command table, the multi-provider selector rules and the root requirement are on the [Alpine Linux (OpenRC)](/full-bars/urnetwork-3.23-fix/wiki/OpenRC#urnet-tools-command-parity) page.

### Wallet Consent Commands

The network wallet consent is one signature from your coldkey that covers every provider client of your network, current and future. Release settlement pays only a consented wallet. The group delegates to the provider binary's own `wallet` subcommands, like `auth` and `choose-network`.

| Command | What it does |
|---|---|
| `wallet status [target]` | Show the stored network consent: the consent coldkey, its epoch interval, and whether it is in effect now, still starts in an epoch, or ended. A legacy login-proof wallet (if any) is listed apart: it is not a consent and settlement does not pay it. Read-only. |
| `wallet challenge <coldkey_ss58> [--wallet-from-epoch N --wallet-through-epoch N] [target]` | Fetch a fresh, verified consent statement to sign. Signs nothing; prints the exact message and the epoch interval it covers. |
| `wallet set <coldkey_ss58> [--message=<text> --signature=0x<hex> \| --coldkey_seed_file=<path>] [target]` | Submit a signed consent. The `--coldkey_seed_file` form reads a seed from disk and is **for the operator's own workstation only, never a fleet node**: on a fleet member the tool refuses unless you pass `--i-am-on-my-own-workstation`. Use `wallet sign` instead when you can. |
| `wallet sign [coldkey_ss58] [target]` | Run `provider wallet sign`: one local page (127.0.0.1 only, 15 minutes, visible countdowns, extendable) where a wallet extension (Talisman, SubWallet, Polkadot{.js}, Nova, or any Polkadot `signRaw` extension) signs the consent statement; without an address you choose the account on the page. The seed never leaves the extension. Refused on a fleet node unless `--i-am-on-my-own-workstation`. See [scripts/wallet-consent/README.md](/full-bars/urnetwork-3.23-fix/blob/main/scripts/wallet-consent/README.md). |

A later consent must start at a later epoch than the one before it. Use `--wallet-from-epoch`/`--wallet-through-epoch` with `wallet challenge` and `wallet set` for an explicit interval; the default is the next epoch through the full 65,536-epoch span.

The full flow, including where to run each form and what settlement pays, is in [Bittensor operations](/full-bars/urnetwork-3.23-fix/wiki/Bittensor-Operations). Delegating the consent to a hotkey is covered in [Bittensor hotkey delegation](/full-bars/urnetwork-3.23-fix/wiki/Bittensor-Hotkey-Delegation), and automatic claims in [Bittensor claim daemons](/full-bars/urnetwork-3.23-fix/wiki/Bittensor-Claim-Daemons).

### Proxy Management Commands

| Command | What it does |
|---|---|
| `proxy add <file\|url> [target]` | Merge proxies from text file (`host:port[:user:pass]`) or live URL. Supports straight paths with `~`, URLs (auto-routed to `add-source`), and flags (`--file=`, `--proxy_file=`, `--url=`). |
| `proxy paste [target]` | Stream raw proxies from stdin or pipe without creating host files. Auto-detects formats and URLs. |
| `proxy clear [target]` | Remove all proxies and URL sources. Confirm-gated (`-f` bypasses prompt). |
| `proxy remove [addresses...] [target]` | Remove specific proxies or patterns. Use `--match=<pattern>` for host substring matches, or `--all` for complete wipe. |
| `proxy trim [<N>] [target] [--preview]` | **(New in 30.4)** With `<N>`, set a persistent hard cap of `<N>` running proxies, shedding the worst A-F reachability graded proxies first. `proxy trim off` clears the cap. With no argument it reports the current cap, the configured pool size and how many proxies the cap is holding back. |
| `proxy refresh [target] [--force]` | Reload proxy list into running provider without restarting. `--force` bypasses warmup lockout. |
| `proxy add-source <url> [target]` | Add live URL proxy source. Fetched and probed immediately. |
| `proxy remove-source <url> [target]` | Remove URL proxy source. |
| `proxy ids [target]` | **(New in 31.0)** Show the `client_id` the platform assigned to each proxy, including the `direct` transport. Read from the provider's local client-JWT store; the bearer tokens themselves are never printed. |
| Exclusion via `proxy remove --match=<pattern>` | See `proxy remove` above. `--match=<pattern>` removes matching proxies and persists the pattern so future URL refreshes skip them. There is no `proxy exclude` subcommand. |
| `proxy health [target]` | Display live health state (Up, Down, Dead, Degraded). |
| `proxy traffic [target]` | Display bandwidth, billable traffic, and active NAT sessions per proxy. |
| `proxy remove-dead [target]` | Interactively prune dead and degraded proxies. Honors `--dry-run`. |
| `summary [target]` | Fleet-style summary of proxy counts by source (url, file, internal). Top-level command, not a `proxy` subcommand. |

### Hub Command Family (removed)

> [!WARNING]
> **Removed (v32.0):** The hub service, its dashboard and the whole `hub` command family are gone from `urnet-tools`. For fleet visibility use `urnet-tools metrics on` and the [Monitoring](/full-bars/urnetwork-3.23-fix/wiki/Monitoring) bundle. The leftover `report` command (see "Configuration & Introspection") only matters if you run your own collector.

### System & Performance Tuning

| Command | What it does |
|---|---|
| `auto [on\|off]` | Enable or disable Smart Auto hardware profile. |
| `optimize [-f]` | Tune kernel parameters (conntrack, socket buffers, port ranges, BBR). Platform-aware. Self-elevates to root when needed (apply live + persist atomically, or roll back). |
| `eco [on\|off]` | Enable or disable Eco profile (RAM-constrained hosts). |
| `smart-dialer [status\|on\|off]` | **(New in 32.8)** Show or set the measured-cost transport preference. Persisted, off by default; a change takes effect at the provider's next start, because the probe loop that measures connect costs is created per identity when it starts. `urnet-tools smart-dialer on` says a restart is needed. `status` says which way it is set and how to change it. See [Configuration](/full-bars/urnetwork-3.23-fix/wiki/Configuration#-control-socket--runtime-settings). |
| `set oom-cap [on\|off\|shadow]` | **(New in 32.8)** Show or set the OOM-aware start cap kill switch. `shadow` (default) decides and logs and enforces nothing, `on` enforces, `off` disables it and forgets a standing cap. Any source saying `off` wins. Live and persisted. |
| `set h3 [off\|direct\|on\|auto\|N\|all]` | Show or set which identities may use the transports beyond H1 (H3, H3 over DNS, the DNS pump), live and persisted. `direct` (default): the direct identity runs them as automatic fallbacks. `on` also runs H3 beside H1; a number or `all` widens the fallbacks to the best N or every proxy; `auto` sizes the fallback set from memory pressure, from none up to 64. Widening relaunches the proxies it adds and each redial costs the platform's speed tests. See [Configuration](/full-bars/urnetwork-3.23-fix/wiki/Configuration) and [H3 Auto](/full-bars/urnetwork-3.23-fix/wiki/H3-Auto). |
| `set h3-datagram [on\|off]` | Offer QUIC DATAGRAM (RFC 9221) on the H3 connection, so a server that accepts can send its small frames as datagrams instead of on the reliable stream. On by default, as upstream. |
| `set h3-datagram-send [on\|off]` | Also send small frames as datagrams on a connection where the server accepted them. On by default, as upstream; read per message, and used only while H1 is up. |
| `set h1-plus [on\|off]` | Show or set the H1+ carrier upgrade on new H1 dials, live and persisted. On by default. See [Configuration](/full-bars/urnetwork-3.23-fix/wiki/Configuration). |
| `set ip-version [dual\|ipv4\|ipv6\|legacy]` | **(New)** Show or set which IP families the **direct** identity uses to reach the platform, live and persisted, no restart. `dual` (default): an IPv4 connection and an IPv6 connection plus a fallback if neither works. `ipv4` / `ipv6`: that family only, no fallback. `legacy`: the behavior before dual stack, one connection where the OS picks the family. `ip` is a short alias. Proxied identities are never affected. See [Configuration](/full-bars/urnetwork-3.23-fix/wiki/Configuration) and [Monitoring](/full-bars/urnetwork-3.23-fix/wiki/Monitoring#metrics-and-log-lines-for-dual-stack-platform-transports). |
| `autopilot log [limit]` | **(New in 32.8)** Show the capacity decisions the provider recorded (OOM-aware start cap and trim results) as a timeline: UTC time, actor, action, the change, the mode and the reason. Shadow decisions show as `[shadow]`. Also prints the current `oom-cap` value. Default 20 entries, at most 200. A provider that predates the `ledger` command answers with an explanatory error.
| `baseline show [-n N] [--json]` | **(New in 32.8)** Show the newest rows of this box's own behaviour record: UTC time, kind, version, proxies up against desired, RSS, host memory available, swap, and the file's first and last timestamps and size. Default 20 rows, at most 200. Reads `~/.urnetwork/baseline.jsonl` directly, so it works on a box whose provider is stopped. |
| `baseline mark <label>` | **(New in 32.8)** Annotate the timeline, for example just before an upgrade. Goes through the control socket, so the provider stays the only writer to the file. Prints the timestamp recorded. A label is required: an unlabelled mark is a boundary `compare` cannot use. Works while the recorder is off, and never deletes the file. |
| `baseline compare [--from A] [--to B] [--skip-ramp 20m] [--json]` | **(New in 32.8)** Compare the box's behaviour across an upgrade. With no arguments it splits at the most recent start whose version differs from the previous one. `A` and `B` are a mark label, a timestamp prefix, or (for `A`) the default. See the rules below, because they decide what the numbers mean. |
| `enforcement [summary\|history [N]]` | Show the durable record of what the governors enforced: releases of silent clients, connect budget cuts, attempt caps, memory holds and crash-loop starts. `summary` (the default) counts events per kind and mode for today, the last 7 and 30 UTC days, and since the log began; `history [N]` lists the newest events (default 20, at most 500). Mode `shadow` means the governor only noted what it would have done. The files live in the provider's state dir and survive restarts and reboots. |

#### What `baseline compare` actually computes

The billable rate is the **lifetime byte counter's delta divided by the time delta**, never an average of the instantaneous rates. Instantaneous readings are bursty: a box showing 4.6 KiB/s now and 56 KiB/s a minute later is ordinary, and averaging those would report a figure that never happened.

- A counter that went **down** was reset (a fresh lifetime store, or a wiped state directory). That interval is skipped rather than reported.
- Rows with no lifetime total are skipped, since the store may not have been running for the whole segment.
- The first `--skip-ramp` (default 20 minutes) after every start mark is excluded from **both** sides: a pool that just restarted under-earns while it ramps, and those minutes would otherwise make the comparison a story about the ramp.
- A segment with **fewer than 4 samples** prints `insufficient data` and no percentages, because a segment that short may sit entirely inside one burst.
- **Capacity and availability-source changes are warned about.** A box trimmed from 1170 to 500 is not a regression, and a container reporting host memory is not comparable to a systemd box. The row for `desired` and `trim cap` is always printed so the two sides can be read side by side.

Rows: billable KiB/s, proxies up (mean), clients (mean), RSS (mean and max), host available MiB (min), swap used MiB (max), PSI full avg60 (mean and max), desired, trim cap, restarts, OOM kills. Every row shows before, after and the change. |
| `turbo [v4\|v8\|off]` | Enable Turbo V4 or Turbo V8 high-throughput modes. |
| `ramlogs [on\|off]` | Enable or disable RAM-disk logging (`/dev/shm`). |
| `profile [name]` | **(New in 31.0)** Show or set the memory and GC tuning profile (`auto`, `turbo-v4`, `turbo-v8`, `eco`, `lowmem`). With no argument, prints the current profile and what each one is for. |
| `metrics [status\|on\|off\|listen <ip:port\|auto>]` | Show where the Prometheus `/metrics` endpoint listens and the address to scrape, turn it on or off, or choose its listen address. Live, no restart, and persisted. See [Monitoring](/full-bars/urnetwork-3.23-fix/wiki/Monitoring). |


---

### Configuration & Introspection (v3.23.0-fix.31.0+)

| Command | What it does |
|---|---|
| `config [--json]` | Show every provider setting with the source it came from (`socket`, `env`, `pending`, `legacy`, `default`). The provider is the single source of truth; this is what it actually believes. |
| `set <key> <value>` | Set a runtime setting over the control socket. Prints `⚠ <key> requires a restart to take effect` when the provider reports the key has no live effect. Queued to `pending_overrides.json` when the provider is down. |
| `set <key>` | Show one setting's current value. Reads are not logged at the provider, because `status` polls them on every invocation. |
| `set <key> off` | Clear a runtime setting and restore its default. Same queueing behavior. |
| `history [limit]` | Show the provider's command audit trail from its 1000-entry circular ring. Defaults to the last 50, maximum 100. |
| `dashboard` | Rich terminal status panel: state indicators, active settings, proxy sources, and restart warnings. Aliases `dash`, `panel`. |

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
1. **One provider per OS user** is the supported deployment model. Your provider, your user: an unprivileged `urnet-tools` command with no explicit target resolves to the provider owned by your own OS account.
2. **Unprivileged, no target, exactly one provider for your user = AUTO-SELECT.** The tool acts on it with minimal commentary. It deliberately does not enumerate other users' providers on every command.
3. **Unprivileged, no target, more than one provider for your user = REFUSAL** with a short inventory of *your* providers (you broke the one-per-user contract), pointing at `urnet-tools providers`, `default set --network <name>`, or `--unit <unit>`. The tool never guesses.
4. **Unprivileged, no provider for your user = error** saying so, pointing at `urnet-tools providers --all` (as root) to see other users' providers.
5. **`providers` shows your providers by default; `providers --all` (root) shows every provider on the box** — the single place multi-user inventory is visible.
6. **Explicit target always wins** — `--unit`, `--user`, `--network`, `--network-id`, `--state-dir` resolve exactly, never narrowed.
7. **Root, no target, more than one provider total = REFUSAL** with the full inventory. Root must name a target or pass `--all`.
8. **Persisted Default Provider:** `urnet-tools default set <target>` pins an implicit target for future no-flag commands, printing a visible notice to stderr. It only fills the "no target" gap.
9. **Conflicting selectors** (e.g. `--unit foo --network bar` pointing to different instances) = ERROR.
10. **`-f` / `--force` (or `-y` / `--yes`) only skips confirmation prompts:** It **never** selects a provider. To target all providers with force, use `-f --all`.
11. **`--help` always prints help** and never executes actions.

---

## 🔧 Deep-Dive: Key Restored & New Features

### 1. Persistent Proxy Trim (`proxy trim <N>`)

`urnet-tools proxy trim <count>` sets a persistent hard cap on the number of running proxies:

```bash
# Report the cap in force, the configured pool size and what the cap holds back
urnet-tools proxy trim

# Preview what proxies would be shed without making changes
urnet-tools proxy trim 500 --preview

# Set running proxy cap to 500
urnet-tools proxy trim 500

# Remove the cap
urnet-tools proxy trim off
```

`urnet-tools status` carries the same information as a `trim` row on the live block whenever a cap is in force, so a capped box is visible at a glance instead of looking like a small one.

- **A-F Grade Ranking:** Sheds worst-graded proxies first using the provider's website-reachability probe scores (`dead` → `never-graded` → `F` → `D` → `C` → `B` → `A`).
- **Traffic Tiebreaker:** Proxies with active billable bandwidth are shed last within their grade tier, preserving active earning connections.
- **Persistence:** Stored at `~/.urnetwork/proxy_trim`, surviving provider restarts and reloads.
- **AIMD Integration:** Clamps the AIMD pool controller `TargetPoolSize` so automated pressure management works within the hard cap.
- **Receipts and preview (32.8):** every change logs `[proxy][trim] received` and `applied` lines. `--preview` is computed by the running provider, and the CLI says so when it cannot reach one. Shed proxies keep their state, and the cap is honored before launching at startup. See [Proxy Management](/full-bars/urnetwork-3.23-fix/wiki/Proxy-Management).

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

### 3. Persisted Default Provider

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

### 4. Control Socket, Queued Overrides, and Confirming a Change (v3.23.0-fix.31.0+)

Runtime settings no longer go through hand-edited systemd drop-ins. The provider
owns a Unix domain socket at `~/.urnetwork/provider.sock` (owner-only, `0600`)
and is the single writer of `~/.urnetwork/provider_state.json`.

```bash
# Change a setting on a running provider. No restart.
urnet-tools set proxy-url-refresh 5m

# Read one back
urnet-tools set proxy-url-refresh

# Clear it
urnet-tools set proxy-url-refresh off
```

**When the provider is stopped**, the change is written to
`~/.urnetwork/pending_overrides.json` instead (flock-guarded, so the CLI and the
installer's shell helpers cannot lose each other's writes) and merged atomically
on the next start.

**Confirming the change registered.** The CLI's exit code only tells you the
request was accepted. The provider logs the change itself, which is the
authoritative confirmation:

```text
⚙️ [control] set proxy-url-refresh=5m (was unset)
⚙️ [control] cleared proxy-url-refresh (was 5m)
⚙️ [control] applied 2 queued override(s) from pending_overrides.json: profile=v8, cleared gogc
```

Rejected changes log too, so a setting that did not take explains itself:

```text
❌ [control] set profile=v9 rejected: unknown control key "profile" value
⚠️ [control] set gomemlimit=2GiB (was 1GiB) persisted but live apply failed, takes effect on restart: ...
```

`rename` and `show-ip` do not go through the socket. Both take effect at the
next renewal, and the provider reports the resulting dashboard label when it
changes:

```text
🏷️ [identity] dashboard label changed: nyc-1 [...] -> nyc-2 [...]
```

Reads are deliberately not logged: `urnet-tools status` polls the socket on
every invocation, so logging them would bury the writes that matter.

> [!TIP]
> `urnet-tools status` also reports whether the control socket is actually
> bound. A running PID with no reachable socket means a startup failure or a
> same-user collision, not a healthy provider.

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
