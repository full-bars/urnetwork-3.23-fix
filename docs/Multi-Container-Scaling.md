# Multi-Container Scaling

You can run multiple independent nodes in a single `docker-compose.yml` file. This is the most efficient way to scale on a single host because all nodes can share a single authentication session.

## How It Works

By sharing a single config volume, `ur_config`, you only need one auth code to authenticate the whole stack.

1. Node 1 starts, detects the empty volume, and uses `URNETWORK_AUTH_CODE` to get a JWT.
2. Node 2 and Node 3 wait through `depends_on` and Node 1's healthcheck until the shared JWT exists.
3. Each node registers its own distinct client identity with the backend and reports to your dashboard with its own label.

The healthcheck avoids a cold-start race where Nodes 2 and 3 would otherwise launch before the JWT is written and crash-loop until it appears.

## Create `docker-compose.yml`

Each service needs a unique service name, container name, host port, and vnStat volume.

```yaml
services:
  # Node 1: Handles the initial authentication for the whole stack
  node-1:
    image: ghcr.io/full-bars/urnetwork-3.23-fix:latest
    container_name: urfix-1
    restart: unless-stopped
    pull_policy: always
    cap_add: [NET_ADMIN, NET_RAW]
    sysctls:
      - net.ipv4.ip_forward=1
      - net.netfilter.nf_conntrack_max=2097152
      - net.netfilter.nf_conntrack_tcp_timeout_established=5400
    environment:
      - BUILD=jwt
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME:-unknown}
      - URNETWORK_NODE_NAME=urfix-1
      - URNETWORK_AUTH_CODE=YOUR_AUTH_CODE # Only needed on Node 1
    volumes:
      - ur_config:/root/.urnetwork  # SHARED volume for JWT
      - urfix-1_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9001:8080"
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"

    # Reports healthy once the shared JWT is written, gating the other nodes' start
    healthcheck:
      test: ["CMD-SHELL", "[ -s /root/.urnetwork/jwt ]"]
      interval: 5s
      timeout: 3s
      retries: 30
      start_period: 10s

  # Node 2: Uses the JWT created by Node 1
  node-2:
    image: ghcr.io/full-bars/urnetwork-3.23-fix:latest
    container_name: urfix-2
    restart: unless-stopped
    pull_policy: always
    cap_add: [NET_ADMIN, NET_RAW]
    sysctls:
      - net.ipv4.ip_forward=1
      - net.netfilter.nf_conntrack_max=2097152
      - net.netfilter.nf_conntrack_tcp_timeout_established=5400
    depends_on:
      node-1:
        condition: service_healthy
    environment:
      - BUILD=jwt
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME:-unknown}
      - URNETWORK_NODE_NAME=urfix-2
    volumes:
      - ur_config:/root/.urnetwork  # SHARED volume
      - urfix-2_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9002:8080"
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"

  # Node 3: Uses the JWT created by Node 1
  node-3:
    image: ghcr.io/full-bars/urnetwork-3.23-fix:latest
    container_name: urfix-3
    restart: unless-stopped
    pull_policy: always
    cap_add: [NET_ADMIN, NET_RAW]
    sysctls:
      - net.ipv4.ip_forward=1
      - net.netfilter.nf_conntrack_max=2097152
      - net.netfilter.nf_conntrack_tcp_timeout_established=5400
    depends_on:
      node-1:
        condition: service_healthy
    environment:
      - BUILD=jwt
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME:-unknown}
      - URNETWORK_NODE_NAME=urfix-3
    volumes:
      - ur_config:/root/.urnetwork  # SHARED volume
      - urfix-3_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9003:8080"
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"

volumes:
  ur_config:      # Shared authentication session
  urfix-1_vnstat: # Unique traffic stats per node
  urfix-2_vnstat:
  urfix-3_vnstat:
```

## Start the Stack

```bash
docker compose up -d
```

## Verify

Check logs:

```bash
docker logs urfix-1
```

Check your Client Manager. You should see three nodes identified by your chosen names and a redacted public IP for privacy, for example:

```text
urfix-1 @ 69.x.x.96 [v3.23.0-fix.15]
```
