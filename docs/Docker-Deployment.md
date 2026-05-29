# Docker Deployment

This page keeps the copy-paste Docker examples from the README in one place. Use either `docker run` or Docker Compose, depending on how you prefer to manage containers.

## Image Registries

Primary image:

```text
ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

Docker Hub mirror:

```text
3cape/urnetwork-3.23-fix:latest
```

Use the Docker Hub mirror if GHCR returns `denied` errors or rate-limiting.

## Persistent JWT

The JWT is stored inside the container at `/root/.urnetwork/jwt`. Without a persistent volume, every container restart wipes it and forces re-authentication using the original auth code, which is single-use and will fail on the second attempt.

All examples below mount a config volume at `/root/.urnetwork`. With this volume in place, the startup script detects the existing JWT and skips authentication on later starts. Auth codes are only consumed once: on first run or after manually removing the config volume.

## Docker Run - GHCR

The examples below use `urfix` as the container name. Set `NAME` in your shell before running a command so the config and vnStat volume names use the same prefix, for example `NAME=urfix`.

### JWT Auth

```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  --sysctl net.netfilter.nf_conntrack_max=2097152 \
  --sysctl net.netfilter.nf_conntrack_tcp_timeout_established=5400 \
  --log-driver=json-file \
  --log-opt max-size=10m \
  --log-opt max-file=3 \
  -e BUILD=jwt \
  -e ENABLE_VNSTAT=true \
  -e HOST_HOSTNAME=$(hostname) \
  -v ${NAME}_config:/root/.urnetwork \
  -v ${NAME}_vnstat:/var/lib/vnstat \
  -v /path/to/proxy.txt:/app/proxy.txt \
  -p 9001:8080 \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest AUTH_CODE_HERE
```

Replace `AUTH_CODE_HERE` with your token from [ur.io](https://ur.io). Auth codes are single-use; the token is saved to the `${NAME}_config` volume on first run and reused on later starts.

Alternative method:

```bash
-e URNETWORK_AUTH_CODE=YOUR_CODE
```

To label this server in webhook alerts and health logs, add:

```bash
-e URNETWORK_NODE_NAME=your-server-name
```

### Email/Password Auth

```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  --sysctl net.netfilter.nf_conntrack_max=2097152 \
  --sysctl net.netfilter.nf_conntrack_tcp_timeout_established=5400 \
  --log-driver=json-file \
  --log-opt max-size=10m \
  --log-opt max-file=3 \
  -e BUILD=stable \
  -e USER_AUTH=you@example.com \
  -e PASSWORD=yourpassword \
  -e ENABLE_VNSTAT=true \
  -e HOST_HOSTNAME=$(hostname) \
  -v ${NAME}_config:/root/.urnetwork \
  -v ${NAME}_vnstat:/var/lib/vnstat \
  -v /path/to/proxy.txt:/app/proxy.txt \
  -p 9001:8080 \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

## Docker Run - Docker Hub

The commands are identical to GHCR except for the image name.

### JWT Auth

```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  --sysctl net.netfilter.nf_conntrack_max=2097152 \
  --sysctl net.netfilter.nf_conntrack_tcp_timeout_established=5400 \
  --log-driver=json-file \
  --log-opt max-size=10m \
  --log-opt max-file=3 \
  -e BUILD=jwt \
  -e ENABLE_VNSTAT=true \
  -e HOST_HOSTNAME=$(hostname) \
  -v ${NAME}_config:/root/.urnetwork \
  -v ${NAME}_vnstat:/var/lib/vnstat \
  -v /path/to/proxy.txt:/app/proxy.txt \
  -p 9001:8080 \
  3cape/urnetwork-3.23-fix:latest AUTH_CODE_HERE
```

Alternative method:

```bash
-e URNETWORK_AUTH_CODE=YOUR_CODE
```

### Email/Password Auth

```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  --sysctl net.netfilter.nf_conntrack_max=2097152 \
  --sysctl net.netfilter.nf_conntrack_tcp_timeout_established=5400 \
  --log-driver=json-file \
  --log-opt max-size=10m \
  --log-opt max-file=3 \
  -e BUILD=stable \
  -e USER_AUTH=you@example.com \
  -e PASSWORD=yourpassword \
  -e ENABLE_VNSTAT=true \
  -e HOST_HOSTNAME=$(hostname) \
  -v ${NAME}_config:/root/.urnetwork \
  -v ${NAME}_vnstat:/var/lib/vnstat \
  -v /path/to/proxy.txt:/app/proxy.txt \
  -p 9001:8080 \
  3cape/urnetwork-3.23-fix:latest
```

## Docker Compose

For multi-container deployments on a single host, storage isolation is handled by giving each instance a unique name. To run multiple containers, copy your `docker-compose.yml` to a new folder and replace the `urfix` prefix with a unique name in `container_name`, `volumes`, and the top-level `volumes:` section.

### JWT Auth

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
      - net.netfilter.nf_conntrack_max=2097152
      - net.netfilter.nf_conntrack_tcp_timeout_established=5400
    environment:
      - BUILD=jwt
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME}
    volumes:
      - urfix_config:/root/.urnetwork
      - urfix_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9001:8080"
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"

volumes:
  urfix_config:
  urfix_vnstat:
```

On first start, provide your auth code using one of these methods:

1. Trailing argument: `docker compose run --rm urnetwork AUTH_CODE_HERE`
2. Environment variable: add `URNETWORK_AUTH_CODE=AUTH_CODE_HERE` to the `environment:` section, then run `docker compose up -d`

After the JWT is saved to the volume, subsequent starts need no auth code:

```bash
docker compose up -d
```

### Email/Password Auth

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
      - net.netfilter.nf_conntrack_max=2097152
      - net.netfilter.nf_conntrack_tcp_timeout_established=5400
    environment:
      - BUILD=stable
      - USER_AUTH=you@example.com
      - PASSWORD=yourpassword
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME}
    volumes:
      - urfix_config:/root/.urnetwork
      - urfix_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9001:8080"
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"

volumes:
  urfix_config:
  urfix_vnstat:
```

## Outage Alerting

Set `URNETWORK_ALERT_WEBHOOK` to receive a push notification when the provider loses contact with the URnetwork backend and when it recovers. The provider posts a JSON payload:

```json
{
  "event": "outage_start",
  "node": "my-server-name (docker)",
  "timestamp": "2026-05-27T23:48:34Z",
  "message": "Backend unreachable - provider holding existing connections but not accepting new ones."
}
```

`event` is either `outage_start` or `outage_clear`. The provider logs `[outage]` state transitions to stdout regardless of whether a webhook URL is set.

Examples:

```text
URNETWORK_ALERT_WEBHOOK=https://discord.com/api/webhooks/YOUR_ID/YOUR_TOKEN
URNETWORK_ALERT_WEBHOOK=https://hooks.slack.com/services/T.../B.../...
URNETWORK_ALERT_WEBHOOK=https://ntfy.sh/your-topic
```

Example Docker Run:

```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  --sysctl net.netfilter.nf_conntrack_max=2097152 \
  --sysctl net.netfilter.nf_conntrack_tcp_timeout_established=5400 \
  --log-driver=json-file \
  --log-opt max-size=10m \
  --log-opt max-file=3 \
  -e URNETWORK_ALERT_WEBHOOK=https://discord.com/api/webhooks/YOUR_ID/YOUR_TOKEN \
  -e URNETWORK_NODE_NAME=${NAME} \
  -e HOST_HOSTNAME=$(hostname) \
  -v ${NAME}_config:/root/.urnetwork \
  -p 9001:8080 \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest YOUR_AUTH_CODE
```

Startup log:

```text
[outage] watcher active node=my-server-name (docker) webhook=configured
```

## RAM Logging

Setting `URNETWORK_RAMLOGS=1` redirects provider logs to `/dev/shm/urnetwork.log` inside the container, a RAM-backed filesystem, instead of stdout. This keeps log I/O entirely off disk.

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
  --sysctl net.netfilter.nf_conntrack_max=2097152 \
  --sysctl net.netfilter.nf_conntrack_tcp_timeout_established=5400 \
  -e URNETWORK_RAMLOGS=1 \
  -e URNETWORK_NODE_NAME=${NAME} \
  -e BUILD=jwt \
  -e ENABLE_VNSTAT=true \
  -e HOST_HOSTNAME=$(hostname) \
  -v ${NAME}_config:/root/.urnetwork \
  -v ${NAME}_vnstat:/var/lib/vnstat \
  -v /path/to/proxy.txt:/app/proxy.txt \
  -p 9001:8080 \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest YOUR_AUTH_CODE
```

View logs live:

```bash
docker exec -it urfix tail -f /dev/shm/urnetwork.log
```

RAM logs are capped at 1MB with automatic rotation and are lost when the container restarts.

## Auto-Tune Performance

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
  --sysctl net.netfilter.nf_conntrack_max=2097152 \
  --sysctl net.netfilter.nf_conntrack_tcp_timeout_established=5400 \
  -e BUILD=jwt \
  -e URNETWORK_PROFILE=auto \
  -e URNETWORK_NODE_NAME=${NAME} \
  -e ENABLE_VNSTAT=true \
  -e HOST_HOSTNAME=$(hostname) \
  -v ${NAME}_config:/root/.urnetwork \
  -v ${NAME}_vnstat:/var/lib/vnstat \
  -v /path/to/proxy.txt:/app/proxy.txt \
  -p 9001:8080 \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest YOUR_AUTH_CODE
```

> [!NOTE]
> Because Auto-Tune may enable RAM logging if it detects a slow disk, omit `--log-driver` and `--log-opt` from this command to avoid conflicts.

## Automatic Updates

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
> The `${NAME}_config` volume is required when using Watchtower. Without it, Watchtower will pull a new image, recreate the container, and the auth code will be consumed again, which fails because auth codes are single-use. With the volume mounted, the existing JWT is reused.

For multiple containers, give each deployment a different container name, config volume, vnStat volume, and host port.
