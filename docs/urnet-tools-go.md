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
| `status [--live] [--brief] [--json] [--json-summary] [--summary] [--plain]` | One-screen health summary: version, time-boxed cached update check, proxy counts, UDP relay coverage, extender state, unit health, and resource utilization. `--live` (or `-l`, with `--interval`) refreshes in place until Ctrl-C. `--brief` prints warnings and failures with exit codes 0 for healthy, 1 for warning, 2 for broken and 3 for unreachable. A healthy result prints nothing. `--json` prints the raw snapshot. `--json-summary` prints the summary as a stable schema document (`urnet-tools-status/1`). `--detail` restores the classic full view (`systemctl status` on Linux, styled panel on Windows/macOS, followed by the live and extender blocks). `--plain` uses plain text without ANSI escapes. |
| `diag` | Capture a running provider's profiles and summary. See [Diagnostics](/full-bars/urnetwork-3.23-fix/wiki/Diagnostics). |
| `start` | Start the provider's systemd unit. Target a specific provider with target flags. |
| `stop` | Stop the provider's systemd unit. Target a specific provider with target flags. |
| `restart` | Restart the provider's systemd unit. Confirm-gated (`-f`/`--force` skips prompt). |
| `reinstall [target]` | Cleanly reinstall provider binary (delegates to latest updater). |
| `uninstall [target]` | Uninstall provider, unit files, and optional state. Confirm-gated. |
| `update [--all] [--include <list>] [--exclude <list>] [--tag <tag>] [--digest <sha256>] [--url <url>]` | Update provider(s) to the latest release, check digest, swap binary, and restart unit. |
| `hotswap` (`hot-swap`) | Zero-downtime in-process binary reload: hands live service to a verified candidate with no restart. Requires a `Type=notify` unit; see the deep-dive below. |
| `self-update` (`selfupdate`) | Update the tool binary itself without touching running providers. |
| `logs [N]` | Stream provider logs (N lines, default 250). RAMLOGS-aware. The ramlog and file views use a native follower, so they work on Windows (no `tail` needed) and wherever `tail` is missing. It prints the last N lines and then follows. If the log is rotated or replaced it finishes the old file and follows the new one from its start, and a truncated log is followed from its start. |
| `top [--interval D] [--no-mouse] [--copy] [--plain]` (also `urtop`) | Live full-screen view of one provider: throughput graphs, clients, proxy pool, memory and recent events. Read-only. Press `?` for keys, `q` to quit. Needs an interactive terminal, use `status` in scripts. `--copy` writes the screen to `~/.local/state/urnet-tools/top-<time>.txt` and prints it to stdout on exit, and `--plain` turns the box characters into `+ - \|` for pasting. The Events panel shows relative times (`12m ago`), severity icons and collapsed repeats (`x12`). Press `+` or `-` to change the refresh interval down to 100 ms. At one second or faster, each live traffic reading adds a graph column. The title shows the duration of the visible columns, and changing the interval keeps earlier columns. Longer intervals use one-second slots. The `1h` and `24h` views use binned snapshot history. Press `t` to cycle graph history, `s` to switch between split (download above center, upload mirrored below) and combined views, `o` for the provider overview, and `a`, `i`, or `v` for alerts, resource gauges, or carriers. Drag panel dividers to resize them, and `M` toggles mouse reporting. Peak and valley rate labels mark visible extrema. |
| `extender [status|events|about|on|off|enable|disable|setup]` | Manage the extender role: `status` shows ports and relayed traffic; `events` reads recent gossip events (`--since`, `--kind`, `--json`); `about` explains ports and requirements; `setup` configures low-port bind privilege and firewall rules; `on`/`off` controls the role live without restart. |
| `get [key] [target]` (alias `show`) | Read-only view of the runtime overrides. With no key it lists every active override; pass one key to see its value. Use `set` to change anything. |
| `version` (`--version`, `-v`) | Print stamped binary version and build metadata. |

### Restored Provider & Session Commands (v3.23.0-fix.30.4+)

| Command | What it does |
|---|---|
| `auth <code> [target] [-f]` | Authenticate provider with an auth code. `-f` forces overwrite of existing JWT. Drops privileges to run as target user when called by root. |
| `direct [on\|off\|status] [target]` | Toggle or report direct/local IP providing state. Taking effect immediately via reload. Available across `provider` and `urnet-tools`. `urnet-docker` forwards it to the image's shell `urnet-tools`, which does not implement it; in Docker run `docker exec <container> provider direct <state>`. |
| `show-ip [on\|off\|status] [target]` | Control whether the provider appends its public IP to the dashboard label set by `rename`. Renamed from `ip-detect` in v3.23.0-fix.31.0, which is kept as an alias. This is about what the dashboard shows, not which address the provider serves on; for that see `direct`. |
| `sn-status [--json] [target]` | Query and display Subnet 25 mining & node telemetry (v3.23.0-fix.30.9+): global rank, top-200 tier eligibility, net bandwidth provided, registered coldkey (SS58/Hex), the network wallet consent (apart from any legacy login-proof wallet; with none it says to sign one with `urnet-tools wallet sign`), current subnet epoch blocks, and finalized epoch pool payout share. Available across `urnet-tools` and `provider`. `urnet-docker` forwards it to the image's shell `urnet-tools`, which does not implement it; in Docker run `docker exec <container> provider sn-status`. |
| `usage [graphs\|graph <view>] [target]` | Display traffic & billing accounting: billable relay bytes vs control-plane protocol overhead, with rolling time-series summaries. Available with `urnet-tools`. `urnet-docker` forwards it to the image's shell `urnet-tools`, which does not implement it. |
| `choose-network <api> <connect> [target]` | Point provider to custom API and WebSocket signaling endpoints. Use `--reset` to restore default bringyour endpoints. |
| `fast-auth [on\|off\|status] [target]` | Toggle or check `~/.urnetwork/fast_auth` marker to bypass auth rate limiter. Confirm-gated. |
| `set [help \| <key> <val> \| <key> off \| <key>] [target]` | Get, set, or clear a runtime provider override over the control socket. `urnet-tools set help` lists every key with its value domain and what it changes. The keys are `node-name`, `fast-auth`, `self-heal`, `proxy-url-max`, `proxy-url-refresh`, `cleanup-scope`, `cleanup-interval`, `hot-restart`, `oom-cap`, `smart-dialer`, `gomemlimit`, `gogc`, `profile`, `ramlogs`, `metrics`, `metrics-listen`, `h3`, `h3-datagram`, `h3-datagram-send`, `tls-hello`, `baseline` and `proxy-audit`. For `tls-hello`, `off` is a stored value, not a clear: it survives restarts and takes precedence over `URNETWORK_TLS_HELLO=on`. A control socket `clear` returns the key to `URNETWORK_TLS_HELLO`. Most apply live on the next tick; `profile`, `ramlogs` and `smart-dialer` need a restart. Queued to `pending_overrides.json` if the provider is stopped. Confirm-gated. |
| `set extender-discovery <off\|on\|feed-only>` | Control the extender directory feed and gossip mesh. Default: `on` when the extender runs. Applies live and persists; `off` is saved explicitly. |
| `set extender-udp443 <on\|off>` | Enable or disable the UDP 443 QUIC carrier. Default: `on`. A change restarts an active extender role. |
| `set extender-udp4053 <on\|off>` | Enable or disable the UDP 4053 DNS carrier. Default: `on`. A change restarts an active extender role. |
| `set extender-udp53 <on\|off>` | Enable or disable the UDP 53 DNS carrier. Default: `on`. A change restarts an active extender role. Port 53 needs bind permission. `URNETWORK_EXTENDER_DNS53=off` also disables it. |
| `set udp-probe <on\|off>` | Control the live proxy UDP background sweep. Default: `on`. A running sweep continues. |
| `set pprof [on\|off\|<ip:port>]` | Turn the loopback pprof diagnostics on or off, or move them to another loopback address. The default is on at `127.0.0.1:6060`. Applies live, no restart. See [Troubleshooting](/full-bars/urnetwork-3.23-fix/wiki/Troubleshooting#inspecting-a-stuck-or-heavy-provider). |
| `rename <name> [target]` | Set the dashboard identity label on the backend. Alias for `set node-name <name>`. The setting persists in provider state and takes precedence over the legacy node name file. Use `off` to clear. Available with `urnet-tools`. `urnet-docker rename` does not work: the image's shell `urnet-tools` does not implement it. For container instructions, see [Node-Identity](/full-bars/urnetwork-3.23-fix/wiki/Node-Identity). |
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
| `wallet set <coldkey_ss58> [--message=<text> --signature=0x<hex> \| --coldkey_seed_file=<path>] [target]` | Submit a signed consent. The `--coldkey_seed_file` form reads a seed from disk and is **for the operator's own workstation only, never a fleet node**: on a fleet member the tool prints a loud warning to stderr and carries on. It never asks and never refuses, with or without a terminal. Use `wallet sign` instead when you can. |
| `wallet sign [coldkey_ss58] [target]` | Run `provider wallet sign`: one local page (127.0.0.1 only, 15 minutes, visible countdowns, extendable) where a wallet extension (Talisman, SubWallet, Polkadot{.js}, Nova, or any Polkadot `signRaw` extension) signs the consent statement; without an address you choose the account on the page. The seed never leaves the extension. No flag skips any gate here: the seed never leaves the extension, so it runs on a fleet node too. See [scripts/wallet-consent/README.md](/full-bars/urnetwork-3.23-fix/blob/main/scripts/wallet-consent/README.md). |

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
| `set <key> off` | Clear a runtime setting and restore its default. Same queueing behavior. For `tls-hello`, `h3`, `baseline` and `oom-cap`, `off` is a stored value and does not clear the override. See each key's description. |
| `history [limit]` | Show the provider's command audit trail from its 1000-entry circular ring. Defaults to the last 50, maximum 100. |
| `dashboard` | Rich terminal status panel: state indicators, active settings, proxy sources, and restart warnings. Aliases `dash`, `panel`. |

---

## Heatmap style

The Proxies panel shows a heatmap when the window has room. The count for the direct connection is separate from the proxy counts. Hover or click a cell to see its addresses, states and UDP check results (`ok`, `no UDP`, `mismatch`, `unknown` or `unchecked`). The direct connection appears as `direct (this host)`. The address contains only the proxy host and port. The popup shows traffic since the previous sample, normally about one second, and lifetime billable and total traffic in each direction. Lifetime values resume from the saved per-proxy counters. A question mark means the value is unavailable. The counters cover accepted TCP and UDP traffic over IPv4 and IPv6, including the direct connection. Proxied UDP to IPv6 destinations is refused.

Color shows state: up is green, dropped is orange, connecting is violet, dead is bold red and parked is dim. Brighter green, toward aqua, means higher throughput. No throughput shade is orange, violet or red. A bright dot that fades shows recent traffic. Proxy cells refresh at the selected interval, capped at one second when requests complete in time. The `m` menu item `Dot fade time` (key `proxy.afterglow_seconds`, 1 to 60 seconds) sets how long a dot takes to fade out after its proxy's traffic drops, and the menu says so under the item. At 10 s a dot that carried traffic dims over ten seconds. A proxy that carries steady traffic stays lit at any setting, so the setting only changes dots with bursty traffic. The proxy popup shows how long clients have been attached, or idle when none are attached. It also shows traffic in the last second. Each dot represents one proxy. Dense cells stack upper and lower dots. When proxies outnumber dot positions, each dot shows the worst state of its group and the popup lists the worst entries first. The mono theme uses block shapes without color. The colorblind theme shows the worst state style for a dense cell. The ASCII theme uses symbols for state and activity. Press `?` for the key.

The `m` menu includes a 'Heatmap style' row to cycle through six styles:

- `Glow` – the current behavior (state-colored dots, afterglow fade, grouped worst state)
- `Aurora` – scrolling wave across contiguous index ranges, colored by aggregated throughput
- `Sonar` – angular bearings: each proxy appears on a circle, distance by throughput, sweep animation
- `Locality` – Hilbert curve: contiguous index along a stable curve for locality perception
- `Blocks` – rectangular grid: per‑index group summary bars with health‑bucket colors
- `Timeline` – per‑index bucket history while the panel is visible, scrolling with time

The chosen style is saved in `~`/.config/urnet-tools/top.conf` (`proxy.heatmap_style`). All styles degrade gracefully:

- `mono` – block shapes only (no color, symbols for state)
- `colorblind` – blocks plus bold/dim for the worst state
- `ASCII` – symbols for state (`. : + x -`) and activity (`.` to `█`)

If the panel is too small for a style (less than the minimum grid height), it falls back to the classic state‑bar display exactly as today.

The style name appears in the legend row when the panel is wide enough; in narrow panels the help overlay (`?`) shows all six names.

## Top screen glossary

Hovering over any element on the `top` screen (or tapping on a touch screen) opens an explainer tooltip. Below is the glossary of components, metrics, states and controls:

### Sessions and clients
- **Client Sessions (`now:sessions`)**: Active client sessions connected through this node. Shows post-quantum encrypted (pqe) and classical (cl) split.
- **PQE Sessions (`pqe`)**: Post-Quantum Encrypted client sessions. Uses hybrid ML-KEM and Kyber key exchange algorithms.
- **Classical Sessions (`cl`)**: Client sessions using classical cryptographic algorithms. Includes traditional key exchange and unencrypted fallback sessions.
- **Clients (`clients`)**: Count of distinct client endpoints currently routing traffic. Each client may maintain one or more concurrent transport sessions.
- **Memory Pressure (`now:pressure`)**: Host memory and scheduler pressure score from 0.00 to 1.00. High pressure above 0.70 triggers autopilot protective trims.
- **Billable Bandwidth (`now:billable`)**: Current rate of client data that earns network rewards. Excludes internal management overhead, proxy checks, and health probes.
- **Total Bandwidth (`now:total`)**: Total current network transfer rate across all node interfaces. Includes billable client traffic, health checks, gossip, and retransmits.
- **1-Minute Average (`now:avg1m`)**: Rolling 1-minute moving average of billable network transfer rate.
- **5-Minute Average (`now:avg5m`)**: Rolling 5-minute moving average of billable network transfer rate.
- **1-Minute Total Average (`now:total_avg1m`)**: Rolling 1-minute moving average of total network transfer rate.
- **5-Minute Total Average (`now:total_avg5m`)**: Rolling 5-minute moving average of total network transfer rate.
- **Cumulative Billed (`now:billed`)**: Total billable client volume transferred since provider startup.
- **Cumulative Moved (`now:moved`)**: Total raw byte volume transferred across all paths since startup.
- **Restart Pending (`now:restart_pending`)**: A configuration update or binary hotswap is scheduled. The provider will restart when active client sessions drain.

### Proxy activity and states
- **Clients Attached For (`age`)**: Continuous time clients have been attached to this proxy. Resets to idle when no clients have been connected for 10 seconds. Idle means no active clients right now, not a broken proxy.
- **Up State (`up`)**: Proxy is authenticated, passing health checks, and serving traffic. Actively accepting incoming connections from the client pool.
- **Dropped State (`dropped`)**: Proxy experienced transient network errors or packet loss. Temporarily degraded while health checks verify connectivity.
- **Connecting State (`connecting`)**: Proxy is establishing connection and authenticating credentials. Transitions to up once the upstream handshake succeeds.
- **Dead State (`dead`)**: Proxy failed health verification or authentication repeatedly. Excluded from routing until the next periodic retry cycle.
- **Parked State (`parked`)**: Proxy is temporarily idle under autopilot capacity controls. Conserves host memory while staying ready for fast unparking.

### UDP coverage states
- **UDP OK (`udp:ok`)**: Proxy passed UDP ASSOCIATE test with verified relay delivery. Outbound UDP packets match the proxy TCP public IP address.
- **No UDP (`udp:no_relay`)**: Proxy refused UDP ASSOCIATE or relay endpoint did not answer. Only TCP client traffic is routed through this proxy.
- **UDP Mismatch (`udp:mismatch`)**: UDP relay succeeded but emerged from a different public IP than TCP. Flagged for routing transparency and protocol consistency.
- **UDP Unknown (`udp:unknown`)**: UDP relay test answered but TCP exit address could not be verified. Egress parity could not be determined in this check cycle.
- **Native UDP (`udp:native`)**: Direct network interface handles UDP without proxy encapsulation. Routes packets through host default gateway with native performance.
- **UDP Unchecked (`udp:unchecked`)**: Proxy is online but background probe has not completed yet. UDP sweeps run periodically in background to verify support.
- **UDP Coverage (`udp:coverage`)**: Percentage of online proxies verified for UDP relay capability. Shows checked ratio and total online pool coverage.

### Resources and runtime internals
- **Heap Memory (`res:heap`)**: Go runtime heap memory in use against the configured memory cap. Tracked by autopilot to prevent process out-of-memory termination.
- **File Descriptors (`res:fds`)**: Open file descriptors against host operating system process limit. Each active proxy connection and client socket consumes a descriptor.
- **Resident Memory (RSS) (`res:rss`)**: Physical RAM occupied by the provider process in host memory. Includes Go runtime heap, execution stacks, and internal buffers.
- **Swap Usage (`swap`)**: Host swap space utilized by provider process or operating system. Non-zero swap indicates host physical memory limits are saturated.
- **Goroutines (`gor`)**: Active Go goroutines executing inside the provider runtime. Press g to open the detailed goroutine stack inspection panel.
- **Heap Objects (`internals:objects`)**: Total allocated live heap objects managed by the Go runtime.
- **Stack Memory (`internals:stacks`)**: Memory allocated for goroutine execution stacks.
- **GC Goal (`internals:goal`)**: Target heap size for the next garbage collection cycle.
- **Allocation Rate (`internals:alloc`)**: Byte rate of new memory allocations per second by the provider.
- **GC Cycles (`internals:gc`)**: Garbage collection frequency per minute and 99th percentile pause time.
- **GC CPU Fraction (`internals:gccpu`)**: Fraction of total available CPU time consumed by garbage collection.
- **Scheduler Latency (`internals:sched`)**: 99th percentile time runnable goroutines wait for available OS threads.
- **HTTP/1.1 Transport (`h1`)**: Standard TCP-based HTTP connection transport fallback. Used when UDP, QUIC, or HTTP/3 paths are blocked by network firewalls.
- **HTTP/3 Transport (`h3`)**: High-performance QUIC transport running over UDP datagrams. Provides multiplexed streams and zero round-trip connection restarts.
- **H3 Outbound Share (`internals:h3all`)**: Cumulative outbound bytes transferred over HTTP/3 versus total payload.

### Extender and gossip mesh
- **Extender Role (`extender:state`)**: Status of the local extender relay role and registration. Extenders announce their availability to clients through the gossip mesh.
- **Gossip Mesh (`extender:gossip`)**: Gossip carries the directory of working extenders, not client traffic. Nodes exchange membership announcements to maintain network topology.
- **Gossip Peers (`gossip:peers`)**: Number of other nodes linked to this extender in the gossip mesh. Peers exchange signed extender directory updates across the network.
- **Directory Feeds (`gossip:feeds`)**: Number of clients holding an open subscription to this directory feed. Feeds 0 means no client is subscribed right now, not that nobody uses it.
- **Gossip Messages (`gossip:msgs`)**: Number of valid directory messages received from peers since start. Gossip carries the directory of working extenders, not client traffic.
- **Extender Clients (`extender:clients`)**: Number of client connections currently routing through this extender.
- **Extender Carriers (`carriers`)**: Transport carriers listening for client traffic on this extender. Shows bytes transferred, active connections, and refusal diagnostics.
- **Extender Served History (`extender:served`)**: Unique clients served by this extender over 24 hours, 7 days, and lifetime.
- **Extender Port Bindings (`extender:ports`)**: Network port listeners bound for incoming extender client traffic.

### Heatmap styles
- **Glow Heatmap (`glow`)**: Default half-block proxy grid with afterglow decay. Brightens cells based on real-time throughput intensity.
- **Aurora Heatmap (`aurora`)**: Flowing color wave visualizer showing relative proxy bandwidth. Smoothly transitions hues across active and recovering proxies.
- **Sonar Heatmap (`sonar`)**: Periodic radial scan highlighting background health probe sweeps. Visualizes check completion order across configured proxies.
- **Locality Heatmap (`locality`)**: Groups proxies into subnet blocks and network provider clusters. Displays diversity and concentration across egress routes.
- **Blocks Heatmap (`blocks`)**: Discrete block cells showing high-contrast proxy state characters. Avoids half-block packing for maximum readability on small fonts.
- **Timeline Heatmap (`timeline`)**: Chronological activity bars tracking client session distribution. Highlights recently active proxies against idle pool capacity.

### Legend and layout
- **Direct Path (`legend:direct`)**: Local host network interface (proxy slot 0, drawn as a star). Routes client traffic directly through host egress without a proxy.
- **Up Count (`legend:up`)**: Count of healthy, verified proxies ready for client traffic.
- **Dropped Count (`legend:dropped`)**: Count of degraded proxies recovering from dropped connections.
- **Connecting Count (`legend:connecting`)**: Count of proxies actively establishing upstream connections.
- **Dead Count (`legend:dead`)**: Count of unreachable proxies removed from active rotation.
- **Parked Count (`legend:parked`)**: Count of idle proxies parked by autopilot to conserve host RAM.
- **Heatmap Style Selector (`legend:style`)**: Current proxy heatmap visualization style. Change via the m settings menu to customize visual appearance.
- **Throughput Graph (`box:graph`)**: Real-time bandwidth graph showing transfer history. Press s to split upload and download, and press w to toggle vertical zoom.
- **Graph Mode & Scale (`title:graph`)**: Graph title showing active history window and vertical peak scale. Click or press w to toggle zoom, and press t to cycle time intervals.
- **Proxy Pool Panel (`box:proxies`)**: Visual heatmap and status distribution of configured proxies. Hover individual dots for address, state, and traffic details.
- **Proxies Header (`title:proxies`)**: Proxy pool management panel header. Displays real-time cell matrix and background UDP probe telemetry.
- **Events Panel (`box:events`)**: Chronological log of recent node events, transitions, and errors. Scroll with mouse wheel or Up and Down arrow keys.
- **Events Header (`title:events`)**: Provider diagnostic event log header.
- **Now Panel (`box:now`)**: Real-time summary of bandwidth rates, client sessions, and pressure. Displays active performance state and operational diagnostics.
- **Now Header (`title:now`)**: Current node telemetry and status overview.
- **Resources Panel (`box:resources`)**: Operating system and Go runtime resource utilization. Monitors memory heap, file descriptors, and physical resident memory.
- **Resources Header (`title:resources`)**: Process resource consumption metrics and limits.
- **Extender Panel (`box:extender`)**: Extender relay status, port listeners, carriers, and gossip mesh. Shown when the provider operates in relay extender mode.
- **Extender Header (`title:extender`)**: Extender service and mesh directory status.
- **Internals Panel (`box:internals`)**: Go runtime internals: memory allocation, GC pacing, and QUIC/H3 stats. Press g to view active goroutine stack groups.
- **Internals Header (`title:internals`)**: Go runtime engine performance metrics.
- **Side Column Divider (`side`)**: Vertical boundary between main charts and sidebar panels. Click and drag to resize width, or double-click to reset default.
- **Proxies Divider (`left:proxies`)**: Horizontal boundary between throughput graph and proxy pool. Click and drag to adjust height, or double-click to reset default.
- **Events Divider (`left:events`)**: Horizontal boundary between proxy pool and events log. Click and drag to adjust height, or double-click to reset default.
- **Resources Divider (`side:resources`)**: Horizontal boundary between now and resources panels. Click and drag to adjust height, or double-click to reset default.
- **Extender Divider (`side:extender`)**: Horizontal boundary between resources and extender panels. Click and drag to adjust height, or double-click to reset default.
- **Internals Divider (`side:internals`)**: Horizontal boundary between extender and internals panels. Click and drag to adjust height, or double-click to reset default.
- **Provider Selector (`tab:provider`)**: Current provider instance, software version, and uptime duration. Click or press Tab to cycle between running providers on this host.

### Footer navigation shortcuts
- **Quit Shortcut (`footer:quit`)**: Exit urnet-tools top and restore the terminal display. Keyboard shortcut: q or Ctrl+C.
- **Provider Switch (`footer:provider`)**: Cycle display to the next running provider found on this host. Keyboard shortcut: Tab.
- **Help Overlay (`footer:help`)**: Open overlay listing all navigation keys and interactive commands. Keyboard shortcut: ? (question mark).
- **Settings Menu (`footer:menu`)**: Open menu to configure heatmap styles, themes, and unit formatting. Keyboard shortcut: m.
- **Graph Zoom (`footer:zoom`)**: Toggle vertical graph scaling between auto-range and peak zoom. Keyboard shortcut: w.
- **Goroutines Panel (`footer:goroutines`)**: Open live stack view grouping all active runtime goroutines. Keyboard shortcut: g.
- **Refresh Interval (`footer:interval`)**: Adjust dashboard polling and redraw frequency from 100ms to 10s. Click - half to poll faster, and click + half to poll slower.
- **Mouse Reporting (`footer:mouse`)**: Toggle terminal mouse event capture and click interactions. Keyboard shortcut: M (Shift+m).

## Docker top

`urnet-docker top [container]` opens the dashboard in the selected running container. It needs an interactive terminal and an image that includes the Go dashboard. The image keeps the existing shell tool for other Docker commands. Update and recreate an older image if the dashboard executable is missing.

The dashboard uses the writable `/root/.urnetwork` mount for its settings when that mount is present. Use a named volume or a bind mount and reuse it when recreating the container. An anonymous volume is not automatically reused. Without a writable state mount, settings remain in the container and can be lost on recreation. Terminal colors follow `TERM` and `COLORTERM` when they are set on the host. Container network interfaces describe the container namespace, not necessarily the host network.

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
12. **Every provider unit is discovered, not just `urnetwork.service`.** The listings also include `urnetwork@*.service` template instances and other `urnetwork-*.service` units whose `ExecStart` runs a provider binary, so one box can run more than one provider. When more than one provider shares an identity directory, the tool says so once; it never deletes or disables anything.
13. **Update verification does not fail on an unreadable image.** A provider binary with a file capability (`cap_net_bind_service`) is non-dumpable, so `/proc/<pid>/exe` is unreadable. The verifier then confirms the restart through the process start time after the binary swap, the installed binary's version stamp, the control socket version, and a changed pid. A restart that truly did not land still fails.

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

`urnet-tools status` and `urnet-tools top` show the actual trim-parked count separately from the active pool. Parked proxies do not count as down. Sessions on proxies still draining remain in the client count. Older providers do not report a parked count.

`urnet-tools status` carries the same information as a `trim` row on the live block whenever a cap is in force, so a capped box is visible at a glance instead of looking like a small one.

- **A-F Grade Ranking:** Sheds worst-graded proxies first using the provider's website-reachability probe scores (`dead` → `never-graded` → `F` → `D` → `C` → `B` → `A`).
- **Traffic Tiebreaker:** Proxies with active billable bandwidth are shed last within their grade tier, preserving active earning connections.
- **Persistence:** Stored at `~/.urnetwork/proxy_trim`, surviving provider restarts and reloads.
- **AIMD Integration:** Clamps the AIMD pool controller `TargetPoolSize` so automated pressure management works within the hard cap.
- **Receipts and preview (32.8):** every change logs `[proxy][trim] received` and `applied` lines. `--preview` is computed by the running provider, and the CLI says so when it cannot reach one. Shed proxies keep their state, and the cap is honored before launching at startup. See [Proxy Management](/full-bars/urnetwork-3.23-fix/wiki/Proxy-Management).

### UDP proxy coverage

`urnet-tools top` and `urnet-tools status` report coverage for online proxies. Online means up or degraded. The counts are:

- `ok`: the relay works and its exit address matches TCP.
- `no UDP` (`no-udp` in narrow `top` views): the proxy refuses UDP ASSOCIATE or the relay does not answer.
- `mismatch` (`diff` in narrow `top` views): the relay works but its exit address differs from TCP.
- `unknown` (`unk` in narrow `top` views): the relay works but its TCP exit address is unknown.
- `unchecked`: the result is missing, in flight or expired.

The counts sum to the online total. Checked is the sum of all counts except unchecked. `ok/checked` is the percentage of checked proxies that are ok. `ok/online` is the percentage of online proxies that are ok. A zero denominator shows `n/a`. Compact `top` panels (up to 3 proxies) show the counts on one row and omit the percentage line unless expanded. When a provider omits the field, the UDP row is omitted.

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

`rename` uses the control socket, or queues the change if the provider is
stopped. `show-ip` uses a marker file instead. The provider reports the
resulting dashboard label when it changes at renewal:

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
