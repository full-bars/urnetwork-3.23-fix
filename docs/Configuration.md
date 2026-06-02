# Configuration Reference

## Environment Variables

| Variable | Default | Description |
| :--- | :--- | :--- |
| `BUILD` | `stable` | Set to `jwt` for auth code login, or `stable` for email/password auth. |
| `USER_AUTH` | - | Your email. Required if `BUILD=stable`. |
| `PASSWORD` | - | Your password. Required if `BUILD=stable`. |
| `URNETWORK_AUTH_CODE` | - | First-run auth code for `BUILD=jwt`. Use this instead of passing the code as a trailing command argument. Ignored once a JWT exists in the volume. |
| `ENABLE_VNSTAT` | `true` | Enables the traffic monitor on port 8080. |
| `ENABLE_IP_CHECKER` | `false` | Diagnostic only. Prints your full public IP to container logs on startup via an external script. Distinct from dashboard identity reporting, which sends only a redacted IP. |
| `TURBO` | - | Set to `v4` or `v8` to enable turbo mode. Prefer this variable for Docker turbo mode. |
| `URNETWORK_RAMLOGS` | `0` | Set to `1` to redirect provider logs to RAM instead of stdout. Cannot be used with Docker `--log-opt`. |
| `URNETWORK_PROFILE` | - | Advanced provider profile: `auto`, `lowmem`, `eco`, `turbo-v4`, or `turbo-v8`. For turbo, prefer `TURBO`. |
| `URNETWORK_ALERT_WEBHOOK` | - | HTTP POST endpoint for outage alerts. Fires on outage start and recovery. |
| `URNETWORK_NODE_NAME` | hostname / redacted IP | Friendly label for dashboard identity and webhook alerts. |
| `HOST_HOSTNAME` | - | Pass the host server name into the container. Use `-e HOST_HOSTNAME=$(hostname)` with `docker run` or `HOST_HOSTNAME=${HOSTNAME}` in Compose. |
| `URNETWORK_HEALTH_INTERVAL` | `5m` | How often to emit a `[health]` heartbeat log line. Includes uptime, RAM stats, and active connection count. Accepts Go duration strings such as `10m` or `1h`. Minimum `1m`. |

## Profile Selection

| Profile | Docker Value | Best For | RAM |
| :--- | :--- | :--- | :--- |
| Auto | `URNETWORK_PROFILE=auto` | Recommended zero-config mode | Any |
| Turbo V8 | `TURBO=v8` or `URNETWORK_PROFILE=turbo-v8` | Maximum throughput, dedicated servers | 16 GiB+ |
| Turbo V4 | `TURBO=v4` or `URNETWORK_PROFILE=turbo-v4` | High throughput, well-provisioned VPS | 4-16 GiB |
| Default | unset | General use | 2-4 GiB |
| Eco | `URNETWORK_PROFILE=eco` | RAM-constrained, full throughput | 1-2 GiB |
| Lowmem | `URNETWORK_PROFILE=lowmem` | Minimum RAM, reduced throughput | < 1 GiB |

See [High-Volume Performance Tuning](High-Volume-Performance-Tuning.md) for the detailed profile behavior and parameter tables.
