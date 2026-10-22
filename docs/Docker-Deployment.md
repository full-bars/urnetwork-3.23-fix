# 🐳 Docker Deployment

This page keeps the copy-paste Docker examples from the README in one place. Use either `docker run` or Docker Compose, depending on how you prefer to manage containers.

## 🛠️ Managing Containers with `urnet-docker`

> [!IMPORTANT]
> **Host-Side vs. In-Container Tooling:**
> - **`urnet-docker`** runs **on the Docker host** (outside the containers). It discovers provider containers, reads their in-container JWTs, and dispatches management tasks directly.
> - **`urnet-tools`** runs **inside the container** (accessible via `docker exec -it <container> urnet-tools <command>`).

Install `urnet-docker` once on the host (SHA-256 verified against the release API):

```sh
curl -fSsL https://dl.fullbars.xyz/urnet-docker.sh | sh
# installs /usr/local/bin/urnet-docker (or ~/.local/bin when not root)
# GitHub fallback: curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts/install-urnet-docker.sh | sh
```

The tool is self-updating afterwards:

```sh
urnet-docker update                  # update the tool binary itself
urnet-docker update --unit urfix     # update a provider container in place (no recreate)
urnet-tools self-update              # same, for the process/systemd tool
```

Common host-side commands:

```sh
urnet-docker providers                          # list provider containers
urnet-docker status --unit urfix                # status of one container
urnet-docker direct status --unit urfix         # show direct IP providing state
urnet-docker direct off --unit urfix            # toggle direct IP (proxies only)
urnet-docker usage --unit urfix                 # show aggregate billable vs control traffic
urnet-docker usage graphs --unit urfix          # time-series usage graphs (day/hour/month)
urnet-docker proxy add --unit urfix ~/p.txt     # add proxies from host (or URL)
urnet-docker proxy paste --unit urfix < p.txt   # stream raw proxies from stdin
urnet-docker proxy trim --unit urfix 500        # hold running proxies at cap (A-F worst first)
urnet-docker proxy refresh --unit urfix         # reload proxies without restart
urnet-docker restart --unit urfix               # restart a container
urnet-docker logs --unit urfix 100              # stream logs (RAMLOGS-aware)
```

> [!NOTE]
> `urnet-docker update` with a target flag (such as `--unit`) updates a provider container in place. The container ID stays the same. Plain `urnet-docker update` with no target updates only the host tool binary. Containers can also be updated by pulling a new image and recreating the container (e.g. via Docker Compose or Watchtower).

## 🗄️ Image Registries

Primary image:

```text
ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

Docker Hub mirror:

```text
3cape/urnetwork-3.23-fix:latest
```

Use the Docker Hub mirror if GHCR returns `denied` errors or rate-limiting.

## 🔑 Persistent State & Volume Mounting

The provider maintains identity keys, cryptographic signing states, and cached JWT tokens inside `/root/.urnetwork`.

> [!IMPORTANT]
> **Volume Persistence**:
> A persistent volume mounted at `/root/.urnetwork` is mandatory for production. Without a persistent volume, every container restart creates a NEW provider identity and wipes the cached JWT, forcing re-authentication using the original auth code (which is single-use and will fail).
>
> At startup, the container inspects `/root/.urnetwork`. If it is unmounted or empty, a warning is logged reminding you to attach a persistent volume.

## ⚠️ Migration Notes

> [!IMPORTANT]
> **Key updates in this release:**
> - **Authentication selector (`AUTH=userpass|authcode`)**: New preferred selector. `AUTH=authcode` uses `AUTHCODE` (or trailing argument), while `AUTH=userpass` uses `USER_AUTH` and `PASSWORD`. Existing `BUILD=stable|nightly|jwt` remains fully supported for backward compatibility (see [Legacy Variables](#-legacy-variables)).
> - **vnStat retired**: The embedded vnStat traffic daemon, CGI scripts, and container port 8080 have been removed. Provider containers now operate with zero listening ports by default. For bandwidth and connection telemetry, use built-in Prometheus metrics (`URNETWORK_METRICS`) with the Grafana stack.
> - **Volume mount path**: The state volume must be mounted at `/root/.urnetwork` (for example `-v urnetwork_data:/root/.urnetwork`). If unmounted or empty, startup logs a warning reminding you that a new identity will be generated across container recreations.
> - **Payout wallet reminder**: To get paid on Bittensor SN25, sign one wallet consent from your own computer (`provider wallet sign <coldkey>`). Nothing changes in the container. See the [Bittensor Quick Start](/full-bars/urnetwork-3.23-fix/wiki/Bittensor-Quick-Start).

## 🏃 Quick Start (Docker Run)

The examples below use `urnetwork_data` as the named volume and `urfix` as the container name.

### 🔐 Auth Code Login (`AUTH=authcode`)

```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  --log-driver=json-file \
  --log-opt max-size=10m \
  --log-opt max-file=3 \
  -e AUTH=authcode \
  -e AUTHCODE='YOUR_AUTH_CODE_HERE' \
  -e HOST_HOSTNAME=$(hostname) \
  -v urnetwork_data:/root/.urnetwork \
  -v /path/to/proxy.txt:/app/proxy.txt \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

Generate an authentication code at [ur.io](https://ur.io). Auth codes are single-use: on first run, the code is exchanged for a JWT token and saved to `/root/.urnetwork/jwt` on the mounted volume. On future starts or updates, the token is reused automatically after local validation.

Trailing argument syntax is also supported:
```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  -e AUTH=authcode \
  -v urnetwork_data:/root/.urnetwork \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest YOUR_AUTH_CODE_HERE
```

To label this server in webhook alerts and health logs, add:
```bash
-e URNETWORK_NODE_NAME=your-server-name
```

### ✉️ Email/Password Login (`AUTH=userpass`)

```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  --log-driver=json-file \
  --log-opt max-size=10m \
  --log-opt max-file=3 \
  -e AUTH=userpass \
  -e USER_AUTH='you@example.com' \
  -e PASSWORD='yourpassword' \
  -e HOST_HOSTNAME=$(hostname) \
  -v urnetwork_data:/root/.urnetwork \
  -v /path/to/proxy.txt:/app/proxy.txt \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

### Docker Hub Mirror

To pull from Docker Hub instead of GHCR, substitute `3cape/urnetwork-3.23-fix:latest` for the image name.

## 🐙 Docker Compose

### 🔐 Auth Code Login (`AUTH=authcode`)

```yaml
services:
  urnetwork:
    image: ghcr.io/full-bars/urnetwork-3.23-fix:latest
    container_name: urfix
    restart: unless-stopped
    pull_policy: always
    cap_add:
      - NET_ADMIN
      - NET_RAW
    sysctls:
      - net.ipv4.ip_forward=1
    environment:
      - AUTH=authcode
      - AUTHCODE=YOUR_AUTH_CODE_HERE
      - HOST_HOSTNAME=${HOSTNAME}
    volumes:
      - urnetwork_data:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"

volumes:
  urnetwork_data:
```

### ✉️ Email/Password Login (`AUTH=userpass`)

```yaml
services:
  urnetwork:
    image: ghcr.io/full-bars/urnetwork-3.23-fix:latest
    container_name: urfix
    restart: unless-stopped
    pull_policy: always
    cap_add:
      - NET_ADMIN
      - NET_RAW
    sysctls:
      - net.ipv4.ip_forward=1
    environment:
      - AUTH=userpass
      - USER_AUTH=you@example.com
      - PASSWORD=yourpassword
      - HOST_HOSTNAME=${HOSTNAME}
    volumes:
      - urnetwork_data:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"

volumes:
  urnetwork_data:
```


## 💾 RAM Logging

Setting `URNETWORK_RAMLOGS=1` redirects provider logs to `/dev/shm/urnetwork.log` inside the container, a RAM-backed filesystem, instead of stdout. This keeps log I/O entirely off disk.

> [!TIP]
> **Live Monitoring (RAMLOGS)**
> If you have `URNETWORK_RAMLOGS=1` enabled, your logs are stored in high-speed memory instead of standard output. Use this command to live-tail them:
> ```bash
> docker exec -it <container_name> tail -f /dev/shm/urnetwork.log
> ```

> [!NOTE]
> `URNETWORK_RAMLOGS=1` and Docker `--log-opt` are mutually exclusive. When RAM logging is active, nothing is written to stdout, so Docker's log driver has nothing to capture. Remove `--log-driver` and `--log-opt` if you enable this.

Example Docker Run:

```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  -e URNETWORK_RAMLOGS=1 \
  -e URNETWORK_NODE_NAME=urfix \
  -e AUTH=authcode \
  -e AUTHCODE='YOUR_AUTH_CODE' \
  -e HOST_HOSTNAME=$(hostname) \
  -e PROXY_URL='https://example.com/your-proxy-list.txt' \
  -v urnetwork_data:/root/.urnetwork \
  -v /path/to/proxy.txt:/app/proxy.txt \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

View logs live:

```bash
docker exec -it urfix tail -f /dev/shm/urnetwork.log
```

RAM logs are capped at 1MB with automatic rotation and are lost when the container restarts.

## 📊 Proxy Summary

View a single-pane fleet overview showing proxy counts by source (file vs URL), health breakdown, and URL cache status:

```sh
docker exec -it <container> provider proxy summary
```

## ♻️ Hot-Restart

Client JWTs are reused across restarts by default (no env var needed since v3.23.0-fix.26). The write path has been un-gated since v25.15, so JWTs were already being saved on every auth cycle: now the read path follows suit. What is stored, how to inspect it, and what mints a new identity: [Hot-Restart.md](/full-bars/urnetwork-3.23-fix/wiki/Hot-Restart).

**Disabling:**
```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  -e AUTH=authcode \
  -e AUTHCODE='YOUR_AUTH_CODE' \
  -e URNETWORK_HOT_RESTART=0 \
  -v urnetwork_data:/root/.urnetwork \
  -v /path/to/proxy.txt:/app/proxy.txt \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

**Status check:**
```bash
docker exec urfix sh -c 'echo ${URNETWORK_HOT_RESTART:-1}'
```

> [!NOTE]
> To opt out, set `-e URNETWORK_HOT_RESTART=0` at container creation. If you don't set anything, hot-restart is active by default.

## 💾 Session Save/Load

Export a provider's full identity state (client JWTs, account JWT, signing keys, proxy lists) as an encrypted, portable bundle for cross-machine transfer.

**Save** (two steps: save inside container, then copy out):
```bash
docker exec -it urfix urnet-tools session save /root/.urnetwork/nyc.urnsession
docker cp urfix:/root/.urnetwork/nyc.urnsession .
```

**Load** (two steps: copy in, then load inside container):
```bash
docker cp atlanta.urnsession urfix:/root/.urnetwork/
docker exec -it urfix urnet-tools session load /root/.urnetwork/atlanta.urnsession
```

The load will prompt for the passphrase, check the network_id against the current account, backup existing files, stage the new session, and ask "Restart now? (Y/n)". If yes, it kills the provider process: the container's start script crash loop picks it up with the new identity.

> [!IMPORTANT]
> Save and load require an interactive TTY (`docker exec -it`, not just `docker exec`). The script will fail with a clear error if `-it` is omitted.

## 🩺 Viewing Proxy Health

You can view the full list of never-up and dropped proxies, as well as a live event log of proxy state transitions. These files persist on the config volume and survive container restarts, even if RAM logging is active.

- Persistent (always): `docker exec -it <container> proxy-health`
- Live-tail RAMLOGS on: `docker exec -it <container> sh -c "tail -f /dev/shm/urnetwork.log | grep -E '\[health\]\[proxies\]|\[pulse\]'"`
- Live-tail RAMLOGS off: `docker logs -f <container> 2>&1 | grep -E '\[health\]\[proxies\]|\[pulse\]'`

## 🏎️ Auto-Tune Performance

The Auto-Tune feature (`URNETWORK_PROFILE=auto`) automatically optimizes the provider based on server hardware.

It detects total system RAM, assigns buffer sizes, enables Eco mode on very low-RAM machines, and benchmarks disk speed on startup. If storage is too slow, it can automatically enable RAM logging to prevent disk I/O from bottlenecking the network stack.

Example Docker Run:

```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  -e AUTH=authcode \
  -e AUTHCODE='YOUR_AUTH_CODE' \
  -e URNETWORK_PROFILE=auto \
  -e URNETWORK_NODE_NAME=urfix \
  -e HOST_HOSTNAME=$(hostname) \
  -e PROXY_URL='https://example.com/your-proxy-list.txt' \
  -v urnetwork_data:/root/.urnetwork \
  -v /path/to/proxy.txt:/app/proxy.txt \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

> [!NOTE]
> Because Auto-Tune may enable RAM logging if it detects a slow disk, omit `--log-driver` and `--log-opt` from this command to avoid conflicts.

## 🔄 Automatic Updates

[Watchtower](https://containrrr.dev/watchtower/) can automatically pull new image versions and restart your container when an update is published. Add it to your `docker-compose.yml` alongside the `urnetwork` service:

```yaml
  watchtower:
    image: containrrr/watchtower
    container_name: watchtower
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    command: --cleanup --interval 3600 urfix
```

> [!IMPORTANT]
> The `urfix_config` volume is required when using Watchtower. Without it, Watchtower will pull a new image, recreate the container, and the auth code will be consumed again, which fails because auth codes are single-use. With the volume mounted, the existing JWT is reused.
> 
> **JWT Smart Refresh**: As of `v3.23.0-fix.17`, the container includes "smart refresh" logic. If the JWT stored in your volume expires, the provider will automatically detect this, delete the stale file, and attempt to re-authenticate using the `USER_AUTH` and `PASSWORD` environment variables if provided. This ensures your nodes stay online even if a JWT is revoked or corrupted during an update.

> [!NOTE]
> **Pelican Panel deployments**: When the image runs under a [Pelican panel](#-pelican-panel) with `PELICAN=yes`, the self-update scripts are disabled. The panel manages updates by re-pulling the published image. Runtime fetches are blocked to prevent silently replacing the audited fork binary mid-flight.

## ⏳ Idle Update

As of `v3.23.0-fix.26.5`, `urnet-tools idle-update` lets you apply a pending provider update without interrupting active client sessions: it waits for a quiet traffic window before swapping the binary, instead of updating immediately and cutting off whatever's in flight:

```sh
docker exec -it <container> urnet-tools idle-update
```

It polls `billable_rate` every 10s and waits until traffic stays at or below a threshold for a sustained window, then double-checks with 1s polling for 10s before actually applying the update - so a brief lull doesn't trigger an update while traffic is still fluctuating.

| Flag | Default | Meaning |
|---|---|---|
| `--threshold <bytes/sec>` | `5120` (5 KiB/s) | Traffic at or below this is considered "quiet". |
| `--window <seconds>` | `300` (5 min) | How long traffic must stay quiet before updating. |

```sh
# Wait for a 10-minute quiet window under 10 KiB/s
docker exec -it <container> urnet-tools idle-update --window 600 --threshold 10240

# Skip waiting entirely and update immediately
docker exec -it <container> urnet-tools idle-update --window 0
```

> [!NOTE]
> If `billable_rate` isn't available yet (provider predates this feature, or hasn't written its first sample), `idle-update` treats the node as **not idle** rather than assuming it's safe to update - it fails closed.

## 📊 Traffic Monitoring (vnStat Retired)

> [!NOTE]
> **vnStat Retirement**:
> The embedded vnStat traffic daemon, CGI scripts, and container port 8080 have been retired and removed from the Docker image as of `v3.23.0-fix`.
>
> Production containers run with **zero listening ports** by default. All p2p tunnels and relay connections dial outbound.
>
> For backward compatibility, the `ENABLE_VNSTAT` variable remains accepted so existing compose files do not error, but it logs a deprecation warning and does not start any HTTP server. For traffic monitoring, time-series graphs, and fleet dashboards, use the Prometheus `/metrics` endpoint and Grafana stack documented below.

### Multi-Container Isolation Rule: Config Volumes (`/root/.urnetwork`)

When running more than one provider container on the same host, you must keep their identity volumes isolated:

> [!CAUTION]
> **Isolate Provider Config Volumes (`/root/.urnetwork`):** Never share `/root/.urnetwork` between multiple running containers. The provider maintains process-local SQLite databases (`proxy.state`, `.client_jwts.json`, `lifetime`) and identity keys inside this directory. Sharing it across containers leads to lock contention and state corruption. Each container requires its own config volume (`ur_config_1`, `ur_config_2`).

#### Multi-Container Compose Snippet:

```yaml
services:
  node-1:
    image: ghcr.io/full-bars/urnetwork-3.23-fix:latest
    container_name: urfix-1
    environment:
      - AUTH=authcode
      - AUTHCODE=YOUR_AUTH_CODE_1
    volumes:
      - ur_config_1:/root/.urnetwork    # ISOLATED identity & state volume
      - ./proxy-1.txt:/app/proxy.txt

  node-2:
    image: ghcr.io/full-bars/urnetwork-3.23-fix:latest
    container_name: urfix-2
    environment:
      - AUTH=authcode
      - AUTHCODE=YOUR_AUTH_CODE_2
    volumes:
      - ur_config_2:/root/.urnetwork    # ISOLATED identity & state volume
      - ./proxy-2.txt:/app/proxy.txt

volumes:
  ur_config_1:
  ur_config_2:
```

---

### 📊 Prometheus + Grafana monitoring bundle

The repository ships a ready-made Prometheus + Grafana stack in the `monitoring/` directory. It scrapes each provider's built-in `/metrics` endpoint and draws one fleet dashboard. It is a separate stack from the provider compose file, so it can run on the same host or on any machine that can reach the providers.

Set it up once, then start it:

```bash
cd monitoring
./setup.sh                            # create .env and an empty target list
./setup.sh 100.64.0.10:9100=node-1    # add a provider (repeat for more)
docker compose up -d
```

What comes up:

- **Prometheus** on `127.0.0.1:9090` (set `PROMETHEUS_PORT` to change it). It reads its targets from `monitoring/prometheus/targets/providers.yml` and picks up edits within a minute. The scrape interval is 30 seconds.
- **Grafana** on port `3000` (set `GRAFANA_PORT`, and bind it with `GRAFANA_BIND`). The admin password is the random one `setup.sh` wrote to `monitoring/.env`.

Each target is the host address and the port that reaches the provider. On bare metal, `urnet-tools metrics status` prints that address. Inside a container it prints the wildcard instead, because the provider listens on every interface of its own network namespace, so use the host address and the host-published port. Turn metrics on inside a container with `docker exec urnetwork urnet-tools metrics on`, or set `URNETWORK_METRICS=:9100` to listen on every interface at start.

> [!TIP]
> For several provider containers on one host, publish each on its own host port (for example `-p 100.64.0.10:9101:9100`) and add that host port as the target. Each container binds its own internal `9100`, so the port only moves up to `9103` when several processes share one network namespace, as on bare metal. Never expose the metrics endpoint on a public address: it lists every proxy address and its traffic.

See [Monitoring](/full-bars/urnetwork-3.23-fix/wiki/Monitoring) for the full setup, the metric list and the alert rules. The [Configuration](/full-bars/urnetwork-3.23-fix/wiki/Configuration#-monitoring--telemetry) reference lists the telemetry variables.

## ⚙️ Environment Variables

| Variable | Requirement | Description |
|---|---|---|
| `AUTH` | Optional | Authentication mode: `authcode` (auth code from ur.io) or `userpass` (email/password). Defaults to `userpass` when credentials are provided, or `authcode` when an auth code is present. |
| `AUTHCODE` | Required for `AUTH=authcode` | Single-use authentication code from https://ur.io. Exchanged on first start and stored to `/root/.urnetwork/jwt` on the persistent volume. |
| `USER_AUTH` | Required for `AUTH=userpass` | Account email or username. |
| `PASSWORD` | Required for `AUTH=userpass` | Account password. |
| `HOST_HOSTNAME` | Optional | Host server name passed into the container for dashboard identification. |
| `URNETWORK_NODE_NAME` | Optional | Custom label for webhook alerts and dashboard identification. |
| `PROXY_URL` | Optional | Live proxy list URL, fetched and merged on interval (see [Proxy URL Sources](/full-bars/urnetwork-3.23-fix/wiki/Proxy-URL-Sources)). |
| `URNETWORK_RAMLOGS` | Optional | Set to `1` to redirect logs to in-memory `/dev/shm/urnetwork.log`. |
| `URNETWORK_PROFILE` | Optional | Hardware profile (`auto`, `lowmem`, `eco`, `turbo-v4`, `turbo-v8`). |
| `TURBO` | Optional | Shorthand for turbo mode (`v4` or `v8`). |
| `URNETWORK_METRICS` | Optional | Prometheus listen address (e.g. `:9100`). |
| `ALL_OPERATORS` | Optional | Set to `1` to run `provide --all-operators`: one child provider per operator in the published list. The single `USER_AUTH`/`PASSWORD` login is skipped because each operator signs in on its own. See [Multi-Operator](/full-bars/urnetwork-3.23-fix/wiki/Multi-Operator). |
| `AUTO_REGISTER` | Optional | With `ALL_OPERATORS=1`: add `--auto-register`, signing in to each operator with the hotkey. Requires `HOTKEY_SEED_FILE`. |
| `HOTKEY_SEED_FILE` | Optional | With `ALL_OPERATORS=1`: path to the 32-byte hotkey seed used by `--auto-register`. Mount the file read-only. |
| `ALLOW_CLIENT_REGISTRATION` | Optional | `auto`, `Y` or `N`. `Y` always adds `--allow-client-registration`; `N` never does; `auto` adds it only when `/root/.urnetwork` has neither `.provider.key` nor an `operators/` directory (a fresh identity). |
| `MAX_MEMORY` | Optional | Passed as `--max-memory=<mem>`, for example `2gib`. With `ALL_OPERATORS=1` the supervisor splits it across the operator children. |
| `PROVIDER_ARGS` | Optional | Extra `provide` arguments appended verbatim and split on whitespace, for v33 flags without a dedicated variable (for example `--operators-url=... --operators-refresh=30m`). |

### 🧩 Multi-Operator Flags

The v33 operator flags can be set from the container environment. `docker/scripts/provider_env.sh` translates them into `provider provide` arguments; with none of the variables set the command line is exactly `provide`, so existing containers are unchanged.

| Environment variable | Flag added to `provider provide` |
|---|---|
| `ALL_OPERATORS=1` | `--all-operators` |
| `AUTO_REGISTER=1` | `--auto-register` (only with `ALL_OPERATORS=1`; needs `HOTKEY_SEED_FILE`) |
| `HOTKEY_SEED_FILE=/path` | `--hotkey_seed_file=/path` (only with `ALL_OPERATORS=1`) |
| `ALLOW_CLIENT_REGISTRATION=Y` | `--allow-client-registration` |
| `ALLOW_CLIENT_REGISTRATION=N` | nothing |
| `ALLOW_CLIENT_REGISTRATION=auto` | `--allow-client-registration` only when the state directory has no `.provider.key` and no `operators/` directory |
| `MAX_MEMORY=2gib` | `--max-memory=2gib` |
| `PROVIDER_ARGS="--operators-url=..."` | the arguments, split on whitespace, appended last |

`1`, `true`, `yes` and `on` are accepted for the two boolean variables, case-insensitively; `0`, `false`, `no` and `off` turn them off. `AUTO_REGISTER` and `HOTKEY_SEED_FILE` are refused when `ALL_OPERATORS` is off, because those flags exist only in the `provide --all-operators` usage. `PROVIDER_ARGS` applies to `provide` only and is split on whitespace, so an individual argument that contains spaces cannot be passed this way.

Leave `AUTH` unset with `ALL_OPERATORS=1`. `AUTH=userpass` still makes the entrypoint require `USER_AUTH` and `PASSWORD`, and those are not used by the operator supervisor, which signs in per operator.

Example:

```bash
docker run -d \
  --name=urnetwork-multi \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  -e ALL_OPERATORS=1 \
  -e AUTO_REGISTER=1 \
  -e HOTKEY_SEED_FILE=/run/secrets/hotkey.seed \
  -e ALLOW_CLIENT_REGISTRATION=auto \
  -e MAX_MEMORY=4gib \
  -v urnetwork_multi:/root/.urnetwork \
  -v /etc/urnetwork/hotkey.seed:/run/secrets/hotkey.seed:ro \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

The first run with a fresh volume adds `--allow-client-registration` because `ALLOW_CLIENT_REGISTRATION=auto` sees no `.provider.key` and no `operators/` directory. Later runs do not add it. To add a new operator to an existing `operators/` tree, set `ALLOW_CLIENT_REGISTRATION=Y`.

## 🧩 Shared Entrypoint Layout

**Shipped in v3.23.0-fix.34.0** (it landed in v3.23.0-fix.33.1). The v33.1 batch consolidates the three Docker start scripts into one shared implementation. It is a script layout change only: no environment variable, image port, binary name or startup command changes.

| File | Role |
| :--- | :--- |
| `docker/scripts/entrypoint.sh` | The image entrypoint. Resolves `BUILD` and `AUTH`, validates the selected auth mode, checks or removes a stale stored JWT, and execs one wrapper. |
| `docker/scripts/start_stable.sh` | Thin wrapper. Sets `PROVIDER_MODE=stable` and `PROVIDER_FLAVOR=stable`, sources `provider_common.sh`, calls `main`. |
| `docker/scripts/start_jwt.sh` | Thin wrapper for the auth-code path. Keeps the `AUTHCODE` to `URNETWORK_AUTH_CODE` alias, sets `PROVIDER_MODE=jwt`, sources `provider_common.sh`, calls `main`. |
| `docker/scripts/start_nightly.sh` | Thin wrapper for the nightly flavor. Sets `PROVIDER_MODE=nightly` and `PROVIDER_FLAVOR=nightly`, sources `provider_common.sh`, calls `main`. |
| `docker/scripts/provider_common.sh` | The shared bootstrap: directory and architecture checks, proxy setup, custom network selection, identity reporting, the IP checker, the vnStat retirement warning, and the login and supervision paths. |
| `docker/scripts/provider_env.sh` | The v33 operator environment mapper. Its output is appended to `provide` exactly as before. |
| `docker/scripts/jwt_verify.sh` | Local JWT `exp` and `nbf` validation, sourced before a stored token is reused. |
| `docker/scripts/update_verify.sh` | Release selection and digest verification, sourced by the nightly path and `urnet-tools`. |

### Compatibility

- `BUILD` and `AUTH` keep their precedence. An explicit `BUILD` wins and logs the conflict warning when `AUTH` disagrees; `AUTH=userpass` maps to `stable`, `AUTH=authcode` maps to `jwt`, and with neither set the entrypoint chooses `jwt` when an auth code or positional argument is present, otherwise `stable`.
- `AUTHCODE` and `URNETWORK_AUTH_CODE` remain interchangeable aliases.
- `USER_AUTH` and `PASSWORD` are still required for `AUTH=userpass` or the stable login path, and are still used for self-healing in auth-code mode.
- `TURBO`, `URNETWORK_PROFILE`, `PELICAN`, `ENABLE_VNSTAT` and the other documented variables keep their existing handling.
- `ALL_OPERATORS`, `AUTO_REGISTER`, `HOTKEY_SEED_FILE`, `ALLOW_CLIENT_REGISTRATION`, `MAX_MEMORY` and `PROVIDER_ARGS` still map through `provider_env.sh`; with all of them unset the command line is exactly `provide`.
- The exit 75 planned-restart loop and the exit 78 auth loop keep their existing behavior.

The consolidation was checked with the Docker script suites and an old-versus-new offline equivalence matrix covering `BUILD` and `AUTH` conflicts, credentials, the auth-code alias, a positional JWT, `TURBO`, `PELICAN` and both restart loops. Live Pelican and arm64 execution were not part of that check.

## 🏛️ Legacy Variables

The following variables are preserved for full backward compatibility:

| Variable | Status | Description |
|---|---|---|
| `BUILD` | Deprecated | Legacy auth selector: `stable` (maps to `AUTH=userpass`), `jwt` (maps to `AUTH=authcode`), or `nightly`. Explicit `BUILD` takes precedence over `AUTH`. When `BUILD` is set without `AUTH`, a deprecation warning is logged advising use of `AUTH=userpass\|authcode`. |
| `URNETWORK_AUTH_CODE` | Alias | Earlier variable name for `AUTHCODE`. Both `AUTHCODE` and `URNETWORK_AUTH_CODE` are accepted interchangeably. |
| `ENABLE_VNSTAT` | Retired | vnStat and the port 8080 HTTP server have been removed. If set to `true`, a warning is logged advising migration to Prometheus metrics (`URNETWORK_METRICS`). The container continues running safely with zero listening ports. |
| `ENABLE_IP_CHECKER` | Diagnostic | Opt-in public IP diagnostic script. Defaults to `false`. |

## 🐦 Pelican Panel

As of `v3.23.0-fix.30.8`, the provider image is importable into the [Pelican game-server panel](https://github.com/pelican-dev/panel) as a one-click egg. The egg ships with the audit-preferred defaults pinned: vnStat off, IP checker off, and runtime self-update disabled.

### Importing the egg

1. Download the egg JSON from the repo: `pelican/egg-urnetwork-323fix.json`
2. In Pelican admin, go to **Nests**, select or create a nest, and use **Import Egg** to upload the JSON.
3. The egg pulls `ghcr.io/full-bars/urnetwork-3.23-fix:latest` (multi-arch amd64/arm64).

### Configuration variables

| Variable | Editable | Description |
|---|---|---|
| `BUILD` | Yes | `stable`, `nightly`, or `jwt` |
| `USER_AUTH` | Yes | Email/phone for password-based auth. Required for `stable`/`nightly`, ignored for `jwt` |
| `PASSWORD` | Admin-only | Password for password-based auth. Required for `stable`/`nightly` |
| `AUTHCODE` | Admin-only | One-time auth code from [ur.io](https://ur.io). Required for `jwt` |
| `PELICAN` | Hidden | Always `yes`: set by the egg, not editable |
| `ENABLE_VNSTAT` | Hidden | Always `false` in the egg |
| `ENABLE_IP_CHECKER` | Hidden | Always `false` in the egg |

### Authentication modes

- **`BUILD=jwt`**: Uses `AUTHCODE` (one-time auth code from [ur.io](https://ur.io)). `pelican_panel.sh` calls `auth-provide "$AUTHCODE" -f` once; it does not retry on failure.
- **`BUILD=stable`** or **`BUILD=nightly`**: Uses `USER_AUTH` + `PASSWORD` for password-based authentication, with retry-on-failure and automatic JWT re-auth after 3 crashes.

> [!NOTE]
> Under Pelican, `BUILD=nightly` currently behaves identically to `BUILD=stable`: the panel always routes through `pelican_panel.sh`, which runs the stable provider binary regardless of `BUILD`. The nightly-vs-stable binary split only applies outside Pelican mode. Don't rely on `BUILD=nightly` to get nightly binary behavior when `PELICAN=yes`.

### Resource requirements

The provider needs raw packet access on the node running the egg:

- Container capability `NET_ADMIN` (and typically `NET_RAW`)
- IP forwarding enabled on the host
- Outbound UDP allowed, for WebRTC P2P transport

No inbound ports need to be forwarded: the provider dials out.

### Update behavior

Under Pelican (`PELICAN=yes`), the self-update checks in `start_nightly.sh` and `urnet-tools update` are **disabled**. The published image is the single source of truth; to update, re-pull the image via the panel (**Settings → Reinstall**) or `docker pull`.

### Testing

The egg ships with automated CI checks (`docker/scripts/test_pelican_gates.sh` for egg JSON structure, variable contracts, and PELICAN-gate behavior; `docker/scripts/test_pelican_smoke.sh` for a full boot smoke test against a fake provider binary). For a real-panel import walkthrough and log output to expect, see `pelican/README.md`.

## Letting the Thrash Watchdog Restart the Container's Provider

The swap-thrash watchdog ([details](/full-bars/urnetwork-3.23-fix/wiki/Configuration#swap-thrash-watchdog)) restarts the provider by exiting with status 75. Inside a container the watchdog counts the container as a supervisor, the same as systemd and OpenRC, so the provider restarts with no extra switch: self-heal is the one switch.

```yaml
services:
  provider:
    image: ghcr.io/full-bars/urnetwork-3.23-fix:latest
    restart: unless-stopped
    environment:
      URNETWORK_SELF_HEAL: "1"
    volumes:
      - ur_config_1:/root/.urnetwork   # the thrash cap and its 24h ceiling live here
```

The start scripts (`start_stable.sh`, `start_nightly.sh`, `start_jwt.sh` and the provider loop of the Pelican egg) treat exit 75 as a planned restart: they wait 5 seconds, restart the provider and leave the JWT alone. Without a writable, persistent `~/.urnetwork` the watchdog logs `persist-failed` and never exits. On runtimes that do not create `/.dockerenv` (Podman, Kubernetes) also set `URNETWORK_CONTAINER=1`.
