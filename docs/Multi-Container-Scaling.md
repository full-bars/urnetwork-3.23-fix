# 🐳 Multi-Container Scaling

You can run multiple independent nodes in a single `docker-compose.yml` file. This is the most efficient way to scale on a single host because all nodes can share a single authentication session.

Use the 3-node example if you want to understand the pattern. Use the 5-node or 10-node templates if you want a larger stack ready to copy.

> [!NOTE]
> The templates below publish no ports and mount no traffic-monitor volume. vnStat, the CGI stats page and container port 8080 were retired; `ENABLE_VNSTAT` is now accepted and ignored with a warning, and `ENABLE_IP_CHECKER` remains the only optional startup diagnostic. For bandwidth and connection telemetry use the Prometheus `/metrics` endpoint (`urnet-tools metrics on`) with the Grafana stack.

## ⚙️ How It Works

By sharing a single config volume, `ur_config`, you only need one auth code to authenticate the whole stack.

1. Node 1 starts, detects the empty volume, and uses `URNETWORK_AUTH_CODE` to get a JWT.
2. The other nodes wait through `depends_on` and Node 1's healthcheck until the shared JWT exists.
3. Each node registers its own distinct client identity with the backend and reports to your dashboard with its own label.

The healthcheck avoids a cold-start race where dependent nodes would otherwise launch before the JWT is written and crash-loop until it appears.

## 📋 Choose a Template

| Stack Size | Best For |
| :--- | :--- |
| 3 nodes | Small deployments and learning the pattern. |
| 5 nodes | Moderate single-host scaling. |
| 10 nodes | Larger hosts where you want a ready fleet file. |

Each service needs a unique service name and container name. All nodes share `ur_config` for the JWT. The provider needs no inbound peer ports, so the templates publish none. Each container still runs its metrics listener on 9100 to 9103 by default, and no template publishes those ports.

## 🚶 3-Node Walkthrough

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
    environment:
      - BUILD=jwt
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-1
      - URNETWORK_AUTH_CODE=YOUR_AUTH_CODE # Only needed on Node 1
    volumes:
      - ur_config:/root/.urnetwork  # SHARED volume for JWT
      - ./proxy.txt:/app/proxy.txt
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
    depends_on:
      node-1:
        condition: service_healthy
    environment:
      - BUILD=jwt
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-2
    volumes:
      - ur_config:/root/.urnetwork  # SHARED volume
      - ./proxy.txt:/app/proxy.txt
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
    depends_on:
      node-1:
        condition: service_healthy
    environment:
      - BUILD=jwt
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-3
    volumes:
      - ur_config:/root/.urnetwork  # SHARED volume
      - ./proxy.txt:/app/proxy.txt
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"

volumes:
  ur_config:      # Shared authentication session
```

## 🚀 Ready Templates

For larger stacks, these templates use YAML anchors to keep the files shorter while still being ready to copy.

Set `URNETWORK_AUTH_CODE` only on `node-1`. The other nodes wait for `node-1` to write the shared JWT, then reuse it.

### 🖐️ 5 Nodes

```yaml
x-urnetwork-common: &urnetwork-common
  image: ghcr.io/full-bars/urnetwork-3.23-fix:latest
  restart: unless-stopped
  pull_policy: always
  cap_add: [NET_ADMIN, NET_RAW]
  sysctls:
    - net.ipv4.ip_forward=1
  logging:
    driver: json-file
    options:
      max-size: "10m"
      max-file: "3"

x-dependent-node: &dependent-node
  <<: *urnetwork-common
  depends_on:
    node-1:
      condition: service_healthy

services:
  node-1:
    <<: *urnetwork-common
    container_name: urfix-1
    environment:
      - BUILD=jwt
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-1
      - URNETWORK_AUTH_CODE=YOUR_AUTH_CODE
    volumes:
      - ur_config:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt
    healthcheck:
      test: ["CMD-SHELL", "[ -s /root/.urnetwork/jwt ]"]
      interval: 5s
      timeout: 3s
      retries: 30
      start_period: 10s

  node-2:
    <<: *dependent-node
    container_name: urfix-2
    environment:
      - BUILD=jwt
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-2
    volumes:
      - ur_config:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

  node-3:
    <<: *dependent-node
    container_name: urfix-3
    environment:
      - BUILD=jwt
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-3
    volumes:
      - ur_config:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

  node-4:
    <<: *dependent-node
    container_name: urfix-4
    environment:
      - BUILD=jwt
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-4
    volumes:
      - ur_config:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

  node-5:
    <<: *dependent-node
    container_name: urfix-5
    environment:
      - BUILD=jwt
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-5
    volumes:
      - ur_config:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

volumes:
  ur_config:
```

### 🔟 10 Nodes

```yaml
x-urnetwork-common: &urnetwork-common
  image: ghcr.io/full-bars/urnetwork-3.23-fix:latest
  restart: unless-stopped
  pull_policy: always
  cap_add: [NET_ADMIN, NET_RAW]
  sysctls:
    - net.ipv4.ip_forward=1
  logging:
    driver: json-file
    options:
      max-size: "10m"
      max-file: "3"

x-dependent-node: &dependent-node
  <<: *urnetwork-common
  depends_on:
    node-1:
      condition: service_healthy

services:
  node-1:
    <<: *urnetwork-common
    container_name: urfix-1
    environment:
      - BUILD=jwt
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-1
      - URNETWORK_AUTH_CODE=YOUR_AUTH_CODE
    volumes:
      - ur_config:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt
    healthcheck:
      test: ["CMD-SHELL", "[ -s /root/.urnetwork/jwt ]"]
      interval: 5s
      timeout: 3s
      retries: 30
      start_period: 10s

  node-2:
    <<: *dependent-node
    container_name: urfix-2
    environment:
      - BUILD=jwt
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-2
    volumes:
      - ur_config:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

  node-3:
    <<: *dependent-node
    container_name: urfix-3
    environment:
      - BUILD=jwt
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-3
    volumes:
      - ur_config:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

  node-4:
    <<: *dependent-node
    container_name: urfix-4
    environment:
      - BUILD=jwt
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-4
    volumes:
      - ur_config:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

  node-5:
    <<: *dependent-node
    container_name: urfix-5
    environment:
      - BUILD=jwt
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-5
    volumes:
      - ur_config:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

  node-6:
    <<: *dependent-node
    container_name: urfix-6
    environment:
      - BUILD=jwt
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-6
    volumes:
      - ur_config:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

  node-7:
    <<: *dependent-node
    container_name: urfix-7
    environment:
      - BUILD=jwt
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-7
    volumes:
      - ur_config:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

  node-8:
    <<: *dependent-node
    container_name: urfix-8
    environment:
      - BUILD=jwt
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-8
    volumes:
      - ur_config:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

  node-9:
    <<: *dependent-node
    container_name: urfix-9
    environment:
      - BUILD=jwt
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-9
    volumes:
      - ur_config:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

  node-10:
    <<: *dependent-node
    container_name: urfix-10
    environment:
      - BUILD=jwt
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-10
    volumes:
      - ur_config:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

volumes:
  ur_config:
```

## ▶️ Start the Stack

From the folder containing your chosen `docker-compose.yml`:

```bash
docker compose up -d
```

## ✅ Verify

Check Node 1 logs to confirm first-time authentication:

```bash
docker logs urfix-1
```

List running containers:

```bash
docker compose ps
```

Check your Client Manager. You should see one entry per node, identified by the names in `URNETWORK_NODE_NAME` and a redacted public IP for privacy, for example:

```text
urfix-1 @ 69.x.x.96 [v3.23.0-fix.15]
urfix-2 @ 69.x.x.96 [v3.23.0-fix.15]
```
