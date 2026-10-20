# Node Identity & Dashboard Label

Every URNetwork provider reports a **dashboard identity label** when it authenticates with the backend. This label appears in your Client Manager as a single human-readable string:

```
Name @ first.x.x.last [Version]
```

For example: `us-east-1 @ 66.x.x.96 [v3.23.0-fix.30.9]`

The label is built by `providerDescription()` in `provider/main.go` and is sent at **every mint and renewal** — so changes take effect within one auth cycle (~once per day), no restart required.

## How the Label Is Assembled

The provider resolves each component in priority order:

### 1. Node Name (the `Name` part)

| Priority | Source | Description |
|----------|--------|-------------|
| 1 | `URNETWORK_NODE_NAME` env var | Explicit override — set this for full control |
| 2 | `HOST_HOSTNAME` env var | Docker: pass the host's actual hostname with `-e HOST_HOSTNAME=$(hostname)` |
| 3 | OS hostname | `os.Hostname()` — what the container or machine reports |
| 4 | Auto-detect | If the name looks like a 12-char Docker container ID, it's replaced with `provider` |

If no name is set and the hostname is a container ID (e.g. `a1b2c3d4e5f6`), the label shows only the redacted IP or just `provider`.

### 2. Public IP (the `@ first.x.x.last` part)

| Priority | Source | Description |
|----------|--------|-------------|
| 1 | `URNETWORK_PUBLIC_IP` env var | Explicit override — takes priority over all other methods |
| 2 | `~/.urnetwork/disable_ip_autodetect` file | If this file exists, IP detection is skipped (see below) |
| 3 | Auto-fetch via `ip.me` | The provider fetches its public IPv4 with a 5s timeout |

The IP is **always redacted** to only show the first and last octets (`first.x.x.last`). The full IP is never sent to the backend or displayed in the dashboard.

### 3. Version (the `[Version]` part)

Automatically appended from Go build info.

## Controlling Identity

### Set a custom name

#### Native (systemd/binary)
```sh
# Via urnet-tools — no restart, takes effect on next renewal tick
urnet-tools rename us-east-1

# Or via systemd drop-in for persistence across restarts
echo 'Environment="URNETWORK_NODE_NAME=us-east-1"' >> /etc/systemd/system/urnetwork.service.d/override.conf

# Docker
docker run -e URNETWORK_NODE_NAME="us-east-1" ...
```

#### Docker
```sh
# Sets the dashboard identity label on the targeted container
urnet-docker rename us-east-1 --unit my-provider

# Or directly inside the container (does not affect the container's hostname)
docker exec -it <container> urnet-tools rename us-east-1

# Clear the override (reverts to hostname)
urnet-docker rename off --unit my-provider
```

> **Note:** The `rename` command is a convenience alias for `urnet-tools set node-name <name>`. Both write to `~/.urnetwork/node_name`, which the provider re-reads on its next renewal tick. No restart is needed.

### Disable IP autodetection (native/systemd only)
Docker startup scripts already set `URNETWORK_PUBLIC_IP` at container startup. For native/systemd installs where the binary auto-fetches its own IP, you can disable this:

```sh
# Disable (provider won't try to fetch public IP)
urnet-tools ip-detect off

# Re-enable (provider will fetch from ip.me at next renewal)
urnet-tools ip-detect on

# Check current status
urnet-tools ip-detect status
```

This creates/removes `~/.urnetwork/disable_ip_autodetect` — a marker file the provider checks on every renewal tick, so no restart is needed.

### Override with a static IP
```sh
# Useful behind a NAT where ip.me returns the router's IP, not yours
# Use TEST-NET range (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24) for examples
export URNETWORK_PUBLIC_IP=192.0.2.5
```

## App Version Reported to the Platform

The dashboard label above shows the build's own version, such as `[v3.23.0-fix.34.2]`. The provider also sends a separate **`app_version`** in its connect auth (and the `X-UR-AppVersion` header), and that is what the platform records against the node. Since v3.23.0-fix.34.2 it is written in the platform's own format instead of the git tag:

```
YYYY.M.D-<code>-fix.<N.M>
2026.10.20-1076955500-fix.34.1
```

| Part | Meaning |
|------|---------|
| `YYYY.M.D` | UTC commit date of the tagged commit, no zero padding. The platform's provider rollout gauge keeps only this part. |
| `<code>` | The house build code, `(seconds since 2023-05-23T00:00:00Z) x 10`, taken at the tagged commit's time. The same formula gives upstream's own tag codes. Stored by the platform, not read. |
| `fix.<N.M>` | This line's release number. Keeps the node identifiable as a 3.23-fix provider. |

A version that does not start with a real calendar date (every release before 34.2 sent the raw tag) is filed under `other` by the platform. The string is 64 characters or fewer, which is the limit the platform stores.

It is set at build time with `-X main.AuthVersion=...`, computed from the release tag by `scripts/compute-app-version.sh <tag>`. A build without the flag, such as a local `make` build, a main-branch Docker image, or a test mint, reports its plain version instead. Everything else, including logs, metrics, hotswap and the update checks, keeps using the git tag.

> [!NOTE]
> This is how a node is recorded and counted by release. It is not what makes a node count as a modern provider: the platform's modern provider gate measures post-quantum encryption and resizable transfer windows on the wire and does not read the version.

## Privacy

The provider is designed to give you flexibility in how you identify your nodes without compromising privacy:

- **Redacted IP**: Only `first.x.x.last` is ever reported — the full IP is never sent to the backend or shown in the dashboard.
- **Custom names**: Use `urnet-tools rename <name>` to label nodes however you want — no IP exposure at all.
- **Hostname passthrough**: Docker users can pass the host's real hostname via `HOST_HOSTNAME` for fleet-wide consistency.
- **Opt out entirely**: Run `urnet-tools ip-detect off` to report only the node name without any IP.

Besides the redacted IP, you can also have it report either the server's hostname or something entirely custom that you choose — so there are multiple ways to identify your own nodes without resorting to a full IP address. The provider is designed to give users that flexibility without compromising privacy.

The `ENABLE_IP_CHECKER` env var is a **separate** diagnostic that logs your full unredacted IP to container logs at startup — it is not related to dashboard identity reporting and is off by default.

## How Docker Handles It

The Docker startup scripts (`start_stable.sh`, `pelican_panel.sh`) set `URNETWORK_PUBLIC_IP` on container startup via `curl -s --max-time 5 ip.me -4`. This env var takes priority in the provider, so Docker containers don't rely on the binary's autodetect. If you run the provider binary directly (systemd, bare metal), the binary fetches its own IP.
