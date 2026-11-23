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
| `status [--live] [--brief] [--json] [--json-summary] [--summary] [--plain]` | One-screen health summary: version, time-boxed cached update check, proxy counts, UDP relay coverage, IPv6 proxy coverage, extender state, unit health, and resource utilization. `--live` (or `-l`, with `--interval`) refreshes in place until Ctrl-C. `--brief` prints warnings and failures with exit codes 0 for healthy, 1 for warning, 2 for broken and 3 for unreachable. A healthy result prints nothing. `--json` prints the raw snapshot. `--json-summary` prints the summary as a stable schema document (`urnet-tools-status/1`). `--detail` restores the classic full view (`systemctl status` on Linux, styled panel on Windows/macOS, followed by the live and extender blocks). `--plain` uses plain text without ANSI escapes. |
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
| `top [--interval D] [--no-mouse] [--copy] [--plain]` (also `urtop`) | Live full-screen view of one provider: throughput graphs, clients, proxy pool, memory and recent events. Read-only. Press `?` for keys, `q` to quit. Needs an interactive terminal; use `status` in scripts. `--copy` writes the screen to `~/.local/state/urnet-tools/top-<time>.txt` and prints it to stdout on exit; `--plain` turns the box characters into `+ - \|` for pasting. The Events panel shows relative times (`12m ago`), severity icons and collapsed repeats (`x12`). Press `+` or `-` to change the refresh interval down to 100 ms. Every traffic reading goes on one time axis, and the graphs show a fixed time scale of 10s, 1m, 5m or 15m. Press `w` for the next scale and `W` for the previous one, and the footer names the scale on screen. Changing the interval keeps the readings already collected. Longer intervals use one-second slots. The `1h` and `24h` views use binned snapshot history. Press `t` to cycle graph history, `o` for the provider overview, and `a`, `i`, or `v` for alerts, resource gauges, or carriers. Press `]` or `[` (or `PageDown`, `PageUp`, or a digit) to move between pages of graphs. Drag panel dividers to resize them; `K` switches between mouse mode and keyboard mode, and `--no-mouse` starts in keyboard mode. Press `c` to copy the screen to the clipboard while it runs. |
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
| `set [help \| <key> <val> \| <key> off \| <key>] [target]` | Get, set, or clear a runtime provider override over the control socket. `urnet-tools set help` lists every key with its value domain and what it changes. The keys are `node-name`, `fast-auth`, `self-heal`, `proxy-url-max`, `proxy-url-refresh`, `cleanup-scope`, `cleanup-interval`, `hot-restart`, `oom-cap`, `smart-dialer`, `gomemlimit`, `gogc`, `profile`, `ramlogs`, `metrics`, `metrics-listen`, `h3`, `h3-datagram`, `h3-datagram-send`, `tls-hello`, `proxy-ipv6-probe`, `proxy-udp-ipv6`, `proxy-dualstack`, `baseline` and `proxy-audit`. For `tls-hello`, `off` is a stored value, not a clear: it survives restarts and takes precedence over `URNETWORK_TLS_HELLO=on`. A control socket `clear` returns the key to `URNETWORK_TLS_HELLO`. Most apply live on the next tick. `profile`, `ramlogs` and `smart-dialer` need a restart. Queued to `pending_overrides.json` if the provider is stopped. Confirm-gated. |
| `set extender-discovery <off\|on\|feed-only>` | Control the extender directory feed and gossip mesh. Default: `on` when the extender runs. Applies live and persists; `off` is saved explicitly. |
| `set extender-udp443 <on\|off>` | Enable or disable the UDP 443 QUIC carrier. Default: `on`. A change restarts an active extender role. |
| `set extender-udp4053 <on\|off>` | Enable or disable the UDP 4053 DNS carrier. Default: `on`. A change restarts an active extender role. |
| `set extender-udp53 <on\|off>` | Enable or disable the UDP 53 DNS carrier. Default: `on`. A change restarts an active extender role. Port 53 needs bind permission. `URNETWORK_EXTENDER_DNS53=off` also disables it. |
| `set udp-probe <on\|off>` | Control the live proxy UDP background sweep. Default: `on`. A running sweep continues. |
| `set proxy-dualstack <on\|off>` | Emergency switch for the extra IPv6 platform connection of proxies that were measured to carry IPv6. Default: `on`. `off` stops every extra connection and keeps them off. |
| `set proxy-udp-ipv6 <on\|off>` | Relay client UDP to IPv6 destinations through proxies that were measured to carry it. Default: `on`. `off` refuses every IPv6 destination. |
| `set proxy-ipv6-probe <on\|off>` | Whether the background proxy sweep checks proxies for IPv6 capability alongside the UDP check. Default: `on`. `off` stops scheduling new IPv6 capability probes and keeps cached results until they are invalidated. `on` schedules them again for online proxies when due. |
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
| `set <key> off` | Clear a runtime setting and restore its default. Same queueing behavior. For `tls-hello`, `h3`, `baseline`, `oom-cap`, `proxy-ipv6-probe`, `proxy-udp-ipv6` and `proxy-dualstack`, `off` is a stored value and does not clear the override. See each key's description. |
| `history [limit]` | Show the provider's command audit trail from its 1000-entry circular ring. Defaults to the last 50, maximum 100. |
| `dashboard` | Rich terminal status panel: state indicators, active settings, proxy sources, and restart warnings. Aliases `dash`, `panel`. |

---

## Pages

`urnet-tools top` groups its graph screens into pages. Page 1 is the dashboard and it is what `top` opens. The strip under the title lists the pages the provider has data for, and the page on screen is marked:

```
[1 Overview] [2 Remote] [3 Billable] [4 Total] [5 Transports]
```

The strip is on by default on every page, page 1 included, so the pages are visible without switching to one. Press `p` to hide it or show it again. A hidden strip gives its row back to the graphs, and the footer keeps the `[/] page` and `p tabs` hints in both states. The choice lasts for the session only. A new `top` starts with the strip on.

Press `]` or `[` to move to the next or previous page, or `PageDown` and `PageUp`. A number key goes straight to that page, and a click on a strip tab does the same. The page stays selected when you switch provider, and it is remembered for the session only.

Page 1 keeps the dashboard's layout, and the rest of the frame is shared, so only the page's own draw function changes when you move to another page. A page with no data, such as Remote before the provider has answered, is left out of the strip unless it is the page you are on, which then shows its own waiting message.

The strip needs the full layout, which is 72 columns by 20 rows. Below that `top` draws the compact single panel, which has no tabs and keeps every row for its graph.

## Graph time scales

The throughput graphs draw one fixed, round time span and fill the plot with it from the left edge to the right edge, whatever the terminal width and the refresh speed. Four scales are available:

| Scale | Span |
|---|---|
| `10s` | 10 seconds |
| `1m` | 1 minute |
| `5m` | 5 minutes |
| `15m` | 15 minutes |

`1m` is the default. Press `w` to step to the next scale and `W` to step back, and both wrap around the table. The footer names the scale (`w zoom 1m`). Clicking the footer hint or the graph title steps to the next scale. The choice is saved in `top.conf` as `graph.window`, and a missing or unknown value falls back to the default, so an older file keeps working.

The graphs keep the readings of the longest scale, one reading per traffic poll, which is 9000 readings at the 100 ms refresh and more than enough at the slower ones. When a scale holds more readings than the plot has columns, each column shows the highest reading in it, so a spike stays visible. When it holds fewer, each reading covers its own share of the columns, so the plot is a set of step bars and no reading is smoothed away.

More columns than readings is the common case at the slower refresh speeds and the shorter scales. A scale whose readings do not reach the left edge yet says so in the title, for example `Billable, 15m (collecting 40s)`, and the data grows in from the right. The title switches to `Billable, 15m` when the readings cover the whole span.

The Remote chart and the direction pages use the same scale. They read the one-second history, which holds about ten minutes, so the 15 minute scale is drawn there as the ten minutes the history carries.

## Peak rates in the Now panel

The Now panel keeps the highest rates `top` has seen since it started, for billable and for total traffic. Three rows appear under `moved` when the panel has the room for them:

```
peak/s    live    1m      5m
bill      1.9M    41.0M   35.7M
total     3.8M    82.0M   71.3M
```

`live` is the highest rate of a single traffic reading, in bytes per second. `1m` and `5m` are the highest of the provider's one and five minute averages. A one or five minute peak is recorded only once its window is full, 60 seconds and 300 seconds after `top` started, so a partial window early after start cannot set it. Until then the cell shows a dash. A peak never comes down.

The block shows one number and one unit letter per cell, so three rates fit the narrow panel. The number follows the chosen unit base, so `u` switches it with the rows above. It is left out when the panel has no rows to spare, and nothing about it is stored: a new `top` starts with no peaks.

## Billable and total by direction

Two pages split the traffic figures into the two directions. Page 3 draws billable traffic and page 4 draws total traffic. Each page draws upload above the centre line and download below it:

- upload is the traffic the provider sent away from the client (`tx`).
- download is the traffic it received toward the client (`rx`).

The packet path in `ip.go` and the socket path in `net.go` count them the same way.

Page 3 also draws the billable packet rate as a pink curve beside each byte curve, with the packet curve smaller and drawn under the byte curve so the bytes stay readable. The packet curve scales on its own peak, because a packet rate shares no unit with a byte rate. The corner then shows both figures per direction, bytes first, for example `1.77 KiB/s  42 pkt/s ▲`. The packet figure is muted while that direction carries no packets, and a direction with no packets at all draws no pink curve. The peak labels stay bytes. Page 4 has no packet curve: packets are counted for billable traffic only, and total traffic counts socket reads, which have no packet count.

Both pages use the mirrored chart renderer, so the direction labels, the corner averages and the peak labels work as they do on the Remote page. Each half scales to its own peak, because upload is a fraction of download on most links.

The pages are listed only when the provider reports the split. A provider that predates it keeps the overview and Remote pages and offers no direction page.

## Billable packet data

The live `traffic` reply includes `billable_rx_packets` and `billable_tx_packets`. The snapshot `traffic.packets` block contains `billable_rx` and `billable_tx` rates with `now_pps`, `avg_1m_pps`, `avg_5m_pps` and `history_pps`. Rx counts packets received by the provider and tx counts packets sent by it. These are billable IP packets only. Total socket traffic has no packet count. Counters reset on restart and may decrease when a proxy is removed. Snapshot sampling skips decreases. An older provider omits these fields.

## Transports in the live traffic reply

The provider's `traffic` reply carries a `transports` block beside the byte totals. Each kind has `bytes_tx`, `bytes_rx`, `frames_tx`, `frames_rx` and `up`, where `up` is the number of that kind's connections open now. The counters are cumulative like the byte totals, so a reader takes the difference of two readings to get a window.

The kinds are `h1_websocket`, `h1_plus`, `h3`, `dns` and `dns_pump`. H1 is split into its two carriers, so a plain websocket and the authenticated framed upgrade appear as separate segments. The two H1 kinds are always present, and a provider with H1+ off reports zero for `h1_plus`. A kind the provider never ran is left out instead of sent as zeros.

A provider that predates the block sends no `transports` at all. A reader then hides the transport view, the same way it hides the direction pages.

## The Transports page

Page 5 draws the share of the window's traffic that each transport carried, in the shape of the client app's transport bar. It appears once the provider has answered with a transport block, and it stays out of the strip on a provider that predates the block.

```
last 60s
▌████████████████████████████████████████████████████████████████████████████▐

● H3 60%   ● H1 30%   ● H1+ 9%   ● whodis 1%

unused ○ whodis pump

transport      share                                               rate  conns
H3               60%                                          1.0 MiB/s
H1               30%                                        512.0 KiB/s     20
H1+               9%                                        153.6 KiB/s      5
whodis            1%                                         17.1 KiB/s
whodis pump   unused                                              0 B/s
H1 connections: 25, of them 5 on the H1+ carrier (20%)
```

The window is 60 seconds, the same window the Remote page draws. The page keeps the transport counters from the traffic polls `top` already makes, and differences the newest reading against the newest one a window back. A session shorter than a window uses the oldest reading it has, so the bar is live from the second poll and the caption names the span it covers.

The bar is one row across the content width. Each kind takes a run of cells in proportion to the bytes it carried, by the largest remainder, so the runs add up to exactly the bar width and every kind that carried traffic keeps at least one cell. The first and last cells are half blocks, which rounds the ends. H1 and H1+ are separate carriers and separate segments. The colours are green for H3, light blue for H1, the URnetwork blue for H1+, pink for WhoDis and yellow for its pump. The mono and ASCII themes draw the bar in `#` cells and use `*` and `o` for the markers.

The legend gives the whole percent of each kind that carried traffic. Whole percents add up to exactly 100, and the largest remainder takes the spare point where one is needed. The footer lists every kind the provider runs that carried nothing, with a hollow marker, so a kind that sat out is not read as a zero share. A window with no traffic at all says `idle`, draws the bar as empty cells and lists every kind in the footer.

The table repeats the window per kind, with the payload as a rate over the span, and the live connection gauge. Only the two H1 carriers report a gauge. The line under the table splits the live H1 connections over the two carriers, because one total would hide which carrier carries the traffic.

A counter that went down, for example when a proxy is removed, is skipped instead of read as negative traffic, so that kind shows as unused for the window.

## Remote transfer chart

`urnet-tools top` can draw the client app's mirrored transfer chart under the throughput graphs. Press `r` to show or hide it. The panel needs a graph band tall enough for it and the two throughput graphs, so a short terminal leaves it out and a taller one brings it back, and the key keeps the choice.

Billable traffic is drawn above a dim centre line and total traffic below it. Each side scales to the peak in the window, so the peak reaches the edge of its half, and the peak value is labelled where it happened. The label slides sideways with the peak column and stays inside the plot. The corner shows the average of the newest five samples for each side, and the arrow is bright while that side carries traffic. Curves are smoothed through the samples and drawn with braille dots, with a faint fill under each curve. The mono and ASCII themes draw the same curves with characters. The scale eases over half a second, so a new peak grows into place instead of jumping. The two sides have a scale of their own because total traffic includes the billable part, and one shared height would flatten the billable curve.

The provider reports billable and total bytes, but no separate upload and download counters and no packet counts. The panel therefore draws one curve per side and labels the sides `billable` and `total` instead of `up` and `down`. A provider that does not send the total figures gets `total not reported` in the footer and a billable curve above the line.

## Colour theme

Press `m` and step the `Theme` row, or name a theme on the command line with `--theme <name>`. The row applies the theme to the screen behind the menu as you step, so the screen is the preview. The choice is saved in `~/.config/urnet-tools/top.conf` as `theme` and the next start uses it. `urnet-tools top --theme neon` saves the same way.

The names are `default`, `nord`, `gruvbox`, `dracula`, `solarized-dark`, `tokyo-night`, `high-contrast`, `colorblind`, `neon` and `mono`. `default` follows the terminal's own palette and is the choice on a fresh install. `mono` is what `NO_COLOR` or a dumb terminal selects, and that environment choice outranks a saved one.

`neon` is built from the URnetwork brand palette: green `#87FB67` for traffic, pale lime `#EFF7BB` for warnings, coral `#FF6F7A` for errors and packets, and blue `#007ACC` and `#0099FF` for IPv6 and H1+. It is drawn for a dark terminal. The theme carries its own approximation for a 256 colour terminal and its own base 16 colours, so the accents keep their hue where the terminal cannot show truecolor.

```bash
urnet-tools top --theme neon
```

## Heatmap style

The Proxies panel shows a heatmap when the window has room. The count for the direct connection is separate from the proxy counts. Hover or click a cell to see its addresses, states and UDP check results (`ok`, `no UDP`, `mismatch`, `unknown` or `unchecked`). The direct connection appears as `direct (this host)`. The proxy address contains only the proxy host and port. When available, the popup also shows the cached TCP and UDP exit addresses and their IP families. The popup shows traffic since the previous sample, normally about one second, and lifetime billable and total traffic in each direction. Lifetime values resume from the saved per-proxy counters. A question mark means the value is unavailable. The counters cover accepted TCP and UDP traffic over IPv4 and IPv6, including the direct connection. Proxied UDP to IPv6 destinations is relayed only through proxies whose IPv6 check measured TCP and UDP with matching exit addresses. Other proxies refuse it at once.

Color shows state: up is green, dropped is orange, connecting is violet, dead is bold red and parked is dim. Brighter green, toward aqua, means higher throughput. No throughput shade is orange, violet or red. A bright dot that fades shows recent traffic. The `m` menu item `Dot fade time` (key `proxy.afterglow_seconds`, 1 to 60 seconds) sets how long a dot takes to fade out after its proxy's traffic drops, and the menu says so under the item. At 10 s a dot that carried traffic dims over ten seconds. A proxy that carries steady traffic stays lit at any setting, so the setting only changes dots with bursty traffic. Each dot represents one proxy. Dense cells stack upper and lower dots. When proxies outnumber dot positions, each dot shows the worst state of its group and the popup lists the worst entries first. The mono theme uses block shapes without color. The colorblind theme shows the worst state style for a dense cell. The ASCII theme uses symbols for state and activity. Press `?` for the key.

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

## Mouse and keyboard mode

`urnet-tools top` runs the terminal in one of two modes. In mouse mode the terminal reports clicks, hover and the wheel to top, so a click on a footer hint, a page tab or a divider works. In keyboard mode that reporting is off, so the terminal keeps the mouse and its own text selection works with an ordinary drag.

Press `K` to switch between the two. The footer hint names the mode the key would switch to, and the mode that is on is written at the right end of the same row. A one-line message at the bottom says what changed:

```
Keyboard Mode: select text with your terminal. K for Mouse Mode
```

`--no-mouse` starts top in keyboard mode. A terminal that cannot report the mouse, such as one with `TERM=dumb` or no `TERM` at all, also starts there, and `K` says why it stays. The old `M` key still switches the mode, but it is not listed in the footer, the help overlay or this page.

Copying the screen with `c` works in either mode. The copy does not touch the terminal's mouse setting.

## Copying the screen

Press `c` to copy the screen that is on display. The copy is the same plain text that `--copy` writes when top exits: styles dropped, trailing spaces trimmed, box characters kept (and turned into `+ - |` with `--plain`). A one-line message at the bottom says what happened:

```
copied 40 lines with wl-copy
sent 40 lines to the terminal clipboard (OSC 52)
no clipboard here (xclip: exit status 1); saved 40 lines to ~/.local/state/urnet-tools/top-20260101-120000-1.txt
```

Three methods are tried in this order:

1. **A local clipboard tool**, when the session is local (no `SSH_CONNECTION` and no `SSH_TTY`). The tool is `wl-copy` when `WAYLAND_DISPLAY` is set, `xclip -selection clipboard` or `xsel --clipboard --input` when `DISPLAY` is set, `clip.exe` on WSL, and `pbcopy` on macOS. The text goes on the tool's standard input, and a tool that is missing or fails passes to the next one.
2. **OSC 52**, the escape sequence that asks the terminal to set the clipboard. It is the only way over SSH, and it is used when no local tool worked. It is wrapped for tmux (`$TMUX`) and for GNU screen (`$STY`). Some terminals need this feature enabled in their settings, and no terminal reports whether the clipboard took the text, so the message says `sent` rather than `copied`.
3. **The `--copy` file**, `top-<time>.txt` under `~/.local/state/urnet-tools` (or `$XDG_STATE_HOME/urnet-tools`). The message names the path.

The terminal payload stops at 64 KiB of text, cut between lines, because many terminals refuse a larger sequence. A screen is far smaller than that.

A copy is a snapshot. It does not change the frame, the selected page, or the terminal's mouse mode.

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

### IPv6 proxy coverage

`urnet-tools top` and `urnet-tools status` also report IPv6 coverage for online proxies. Online means up or degraded.

- `IPv6 proxies: <Y> yes, <N> no, <U> unknown[, <NP> not probed]`: yes means the proxy carries IPv6 with a matching exit address, no means no IPv6 capability was confirmed, unknown means the check has no confirmed result, and not probed means the proxy was not measured yet.
- `IPv6 UDP probes: <Y> yes[, <N> no][, <U> unknown]`: the cached results of the IPv6 UDP check. Yes means the relay works with a matching exit address, no means the relay refused or did not answer. The row reports measurements only. Whether a relay is actually used also depends on the `proxy_udp_ipv6` key.

In `top` both counts sit on one line, as `IPv6 proxies: ... (udp: ...)`. A terminal narrower than 60 columns abbreviates the line to `ipv6:`, `unk` and `unprobed`, and counts drop off the end when the line runs out of room. In `status` they are rows labelled `IPv6` and `IPv6 UDP`, printed below the UDP row.

The top proxy popup shows, per proxy, `TCP exit (IPv4) <ip>` or `TCP exit (IPv6) <ip>` and `UDP exit (IPv4) <ip>` or `UDP exit (IPv6) <ip>` for a known exit address, and `IPv6 exit: <status> [(<ip>)]`, where the status is `yes`, `no` or `unknown`.

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
