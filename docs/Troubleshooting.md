# Troubleshooting Guide

## 🚨 Incident Quick Diagnosis Matrix

> [!NOTE]
> On Docker deployments, run these commands inside the container via `docker exec -it <container> urnet-tools <command>` or directly from the host using `urnet-docker <command> --unit <name>`.

| Symptom / Error | Probable Cause | Action |
| :--- | :--- | :--- |
| `[t]auth error` / `[contract]oob err (N suppressed)` | Backend outage or signaling failure | Run `urnet-tools status`; monitor self-healing status with `urnet-tools self-heal status`. |
| Container exits with code `78` | JWT expired, invalid, or unpersisted | Ensure `/root/.urnetwork` volume is mounted; check `USER_AUTH`/`PASSWORD` in env or re-authenticate via `urnetwork auth` (auth codes are single-use). |
| Memory ballooning / OOM kills | High proxy count without memory profile | Set `URNETWORK_PROFILE=auto` or `eco`; enable `URNETWORK_SELF_HEAL=1`. |
| Exit code `52` on `proxy refresh` | 8-hour warmup threshold not met | Run `urnet-tools proxy refresh --force` to bypass warmup gate. |
| Disk space exhaustion | Unrotated logs filling `/var/log` or root | Enable `URNETWORK_RAMLOGS=1` or set `--log-opt max-size=10m --log-opt max-file=3`. |
| Proxies marked `DEAD` or `DEGRADED` | Target proxy failure or connection drop | Run `urnet-tools proxy health` and prune with `urnet-tools proxy remove-dead`. |

---

## 1. Exit Codes

Every time the provider binary exits with a non-zero code, it prints a `FATAL [exit <code>]: ...` line to both stderr (visible in `docker logs`) and the ramlog file (`/dev/shm/urnetwork.log`, visible via `logs`). The message describes the failure and explains what happened.

### auth

| Code | Meaning |
|------|---------|
| 10 | Home directory not found: the binary cannot determine where to store the JWT. |
| 11 | Login request failed: a network error prevented reaching the API. |
| 12 | API rejected the credentials: the username/password combination is wrong. |
| 13 | Verification required: the account has not completed setup via the app or web. |
| 14 | Auth code request failed: a network error prevented reaching the API. |
| 15 | Auth code rejected: the code is expired, already used, or invalid. Auth codes are single-use; mount `/root/.urnetwork` as a persistent volume if restarting. |
| 16 | Could not create `~/.urnetwork` directory for JWT storage. |

### provide (provider runtime)

| Code | Meaning |
|------|---------|
| 20 | The proxy file specified with `--proxy_file` cannot be read. Check the path and file permissions. |
| 21 | The proxy file is empty or contains no valid `ip:port:user:pass` lines. |
| 78 | The JWT is expired or invalid. The startup script intercepts this code, deletes the stale JWT, and re-authenticates automatically. |

### logs

| Code | Meaning |
|------|---------|
| 40 | Ramlog file not found at `/dev/shm/urnetwork.log`. Is `URNETWORK_RAMLOGS=1` set? |

### proxy refresh

| Code | Meaning |
|------|---------|
| 50 | Could not read `proxy.state`: the provider may not have started yet. |
| 51 | Provider is not currently running. |
| 52 | Provider has not reached the 8-hour warmup threshold. Use `--force` to override. |
| 53 | Could not acquire the proxy lock: another operation is in progress. |
| 54 | Could not read the proxy source file. |
| 55 | Could not determine the reload trigger path. |
| 56 | Could not write the reload trigger. |

### proxy remove-dead

| Code | Meaning |
|------|---------|
| 60 | Provider is not currently running. |
| 61 | Provider has not reached the 65-minute dead-confirmation threshold. |
| 62 | Could not update the proxy source file. |
| 63 | Could not acquire the proxy lock. |
| 64 | Could not write the reload trigger. |

---

## 2. Container Troubleshooting

If your container exits unexpectedly:

1. **Check the exit code**: `docker inspect <name> --format '{{.State.ExitCode}}'`
2. **Look up the code** in the tables above for the likely cause.
3. **Read the ramlogs**: `docker exec <name> logs`
4. **Exit 0** means a clean shutdown (SIGTERM or manual stop).
5. **Exit 78** means the JWT expired (the script attempted automatic re-authentication). Verify `USER_AUTH`/`PASSWORD` or `URNETWORK_AUTH_CODE` are set correctly.
6. **All other non-zero codes** indicate a configuration or environment problem. The fatal message describes the specific issue.

### Fatal messages always write to both logs and stderr

`shmLogFatal` (the function behind every non-zero exit) writes the `FATAL [exit <code>]` line directly to the ramlog file before calling `os.Exit`. This bypasses the normal log pipe, so the message is never lost to a goroutine scheduling race. It also writes to the original stderr so the message appears in `docker logs` regardless of the ramlog setting. You do not lose the error message no matter how you view logs.

## 3. Common OOB (Out-of-Band) Errors

The signaling layer is responsible for contract creation and connection handshaking.

| Error | Cause | Recommended Action |
| :--- | :--- | :--- |
| `oob err = Timeout` | The signaling response took >60s. | Check for network congestion or CPU starvation. |
| `oob err = Invalid` | Authentication token (JWT) or credentials failed. | Verify your `<AUTH-CODE>` or email/pass. |
| `exit could not create contract` | Repeated timeouts prevented contract initialization. | See [Performance Tuning](High-Volume-Performance-Tuning). |

## 4. Resource Exhaustion

### Disk Space (Log Ballooning)

UrNetwork logs are notoriously "chatty." Without management, they can grow to several gigabytes in hours.

- **Symptoms**: "No space left on device" errors, system instability.
- **Fix**: Use Docker log rotation flags (`--log-opt max-size=10m --log-opt max-file=3`) or enable ramlogs (`URNETWORK_RAMLOGS=1`) to redirect output to `/dev/shm`.

### CPU Starvation

In high-volume environments, a pegged CPU can delay the processing of OOB signaling packets.

- **Symptoms**: Frequent `Timeout` errors despite a stable network.
- **Fix**: If using `--cpus`, ensure the limit is high enough to handle the signaling overhead of your proxy list.
