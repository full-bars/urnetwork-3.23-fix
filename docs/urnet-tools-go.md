# urnet-tools (Go) — Provider-Aware Fleet Ops

> Applies to v3.23.0-fix.27.0+. The legacy shell tool (POSIX `Provider_Install_Linux.sh` + Windows `urnet-tools.ps1`) is replaced by a single provider-aware Go binary. Subcommand names and usage are unchanged; what changed is **how the tool decides which provider it operates on**.

## Why this exists

The legacy `urnet-tools` resolved its target from a hardcoded path (`$HOME/.local/share/urnetwork-provider`) with zero awareness that other providers exist on the box. On a multi-provider machine it could act on the **wrong provider entirely** — and did (08-08 pool-wipe, 08-09 half-update). The Go rewrite makes the tool's single most important guarantee structural: **it never guesses which provider you mean.**

## The two binaries

| Binary | What it manages |
|---|---|
| `urnet-tools` | Process/systemd providers (`--proxy_file`, internal config, systemd units) |
| `urnet-docker` | Docker-deployed providers (discovers containers, delegates via `docker exec`) |

Both are cross-compiled from one Go source — the shell↔PowerShell drift is gone.

## Provider discovery — not path guessing

`urnet-tools providers` inventories every provider on the box:

```
UNIT                       USER   NETWORK           STATE-DIR
urnetwork-native.service   urnet  tacogonzalez3000  /home/urnet/.urnetwork
urnetwork-beta.service     urnetwork-beta beta-test /home/urnetwork-beta/.urnetwork
```

- Discovery = process scan (`/proc`) + systemd unit enumeration. No hardcoded paths.
- **Identity is JWT-derived**: `network_name` / `network_id` come from decoding each provider's JWT. Paths are only used to locate state, never to decide "which provider."

## Targeting

| Flag | Selects by |
|---|---|
| `--unit <name>` | systemd unit name (system or user) |
| `--user <user>` | the OS user running the provider |
| `--network <name>` | JWT network name (the account) |
| `--network-id <id>` | JWT network ID — for duplicate network names |
| `--state-dir <path>` | explicit state directory |

Rules:
- **Multi-provider box + no target = REFUSAL.** The command errors and prints the inventory. It never picks for you.
- **Single provider + no target = proceed** (after echoing the target).
- **Conflicting selectors** (`--unit x --network y`) = error.
- **`-f` / `--force` only skips the confirm prompt. It never picks a provider.** `-f` alone on a multi-provider box is still refused; `-f --all` or `-f --include a,b` is the explicit everything-bypass.
- `--help` always prints help and never executes anything (the legacy `--help`-executes-clear bug class is gone).

## Destructive ops

`proxy clear`, `proxy remove --all`, `uninstall` print the target + effect, then require a typed `yes` confirmation. `-f`/`--yes` bypasses the prompt for scripts/cron — but the audit trail (the target listing) is always printed to stderr, even under `-f`.

## Safety properties

- **Mandatory digest verification** on `update`: the release tarball is SHA-256 verified against the release API's published digest. A missing digest refuses the update. `--tag` resolves the digest automatically.
- **Private per-update staging dir** (0700 `MkdirTemp`): a local user can't pre-create the stage path and swap the tarball between verify and extract.
- **Atomic binary swap**: `dst.new` + rename — never O_TRUNC on a running executable.
- **Hub install sanity check** reads ELF magic bytes — it never executes the freshly downloaded binary.
- **`--help` never executes**; dry-run (`-n`) prints the plan and does nothing, including for `start`/`stop`.
- Temp JWT scratch files are always removed.

## Migrating scripts

- All 25 legacy subcommands dispatch identically. Single-provider boxes: nothing changes.
- Multi-provider boxes: scripts that previously relied on the tool silently targeting "whatever the calling user owns" must now pass a target. The tool will tell you — it refuses and prints the inventory.

## `optimize` is platform-aware

- **Linux**: socket buffers + FD limit, plus the ephemeral-port pool (`net.ipv4.ip_local_port_range`) and TIME_WAIT recycling (`net.ipv4.tcp_fin_timeout`) — the two knobs that matter at proxy-scale connection churn.
- **Windows**: `netsh` dynamic port pool + `TcpTimedWaitDelay` registry equivalent.

## Getting the tool

Three supported paths (v3.23.0-fix.28+ — the Go tool assets ship with every release from v3.23.0-fix.28 onward; older releases and 32-bit x86 hosts fall back to the legacy shell tool):

| Deployment | How the tool is installed |
|---|---|
| systemd / native provider | `Provider_Install_Linux.sh` now installs the Go `urnet-tools` binary (sha256-verified against the release API). Fresh installs and `update` both hand off to the Go tool — the shell script is only a fallback for releases that predate the Go asset. |
| docker-only providers | Run the standalone host-side installer: `curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts/install-urnet-docker.sh \| sh` — installs `urnet-docker` to `/usr/local/bin` (or `~/.local/bin`), sha256-verified. Same script can install `urnet-tools` with `sh -s -- urnet-tools`. |
| macOS | `Provider_Install_Mac.sh` installs the Go `urnet-tools-darwin-<arch>` binary (sha256-verified via `shasum`), falling back to the legacy wrapper. |

The Go tool is self-updating: `urnet-tools update` refreshes providers **and** its own binary; `urnet-docker update` / `urnet-tools self-update` refresh only the tool. Release assets are named `urnet-tools-<os>-<arch>` and `urnet-docker-<os>-<arch>` (e.g. `urnet-tools-linux-amd64`), attached to every release.

## Migration status

- ✅ Phase 1 (this doc): tool subcommands in Go. Installer (`Provider_Install_Linux.sh`) stays shell.
- ✅ Tool distribution: Go binaries shipped as release assets; installers fetch them digest-verified; tool self-updates via `update`/`self-update`.
- 🔜 Phase 2: retire `urnet-tools.ps1` and the docker shell variant.
- 🔜 Phase 3: installer logic in Go.

Design doc: `URN-TOOLS-GO-DESIGN.md` at the workspace root.
