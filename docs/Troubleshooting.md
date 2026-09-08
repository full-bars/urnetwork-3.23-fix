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
| `urnet-tools hotswap` restarts instead of handing off | Unit is `Type=simple`, not `Type=notify` | Expected on any node installed before v3.23.0-fix.31.0. The update still applies via restart. See [HotSwap declines on an existing node](#5-hotswap-declines-on-an-existing-node). |
| A setting from `urnet-tools set` seems not to apply | Change was queued, rejected, or applied only on restart | Check the provider log for `⚙️ [control]` / `❌ [control]`. See [Confirming a settings change](#6-confirming-a-settings-change). |
| `status` shows a running PID but no control socket | Startup failure, or a second provider for the same OS user | The socket is the liveness signal, not the PID. Check the log for a startup error and `urnet-tools providers --all` for a collision. |

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

---

## 5. HotSwap Declines on an Existing Node

**Symptom**: `urnet-tools hotswap`, or a `urnet-tools update` on a HotSwap-capable
release, restarts the provider normally instead of handing off with no downtime.

**Cause**: the systemd handoff requires a `Type=notify` unit. The retiring
process aborts whenever systemd started it (`INVOCATION_ID` is set) but
`NOTIFY_SOCKET` is empty, which is exactly what a `Type=simple` unit looks like.
Only `install_systemd_units` in `Provider_Install_Linux.sh` writes
`Type=notify`, and `urnet-tools update` only ever swaps the binary, never the
unit. **Every node installed before v3.23.0-fix.31.0 therefore still runs
`Type=simple`.**

**This is working as intended.** `urnet-tools` checks the unit type, declines
HotSwap, and falls back to a normal restart, so the update still lands with the
usual 20-60s stall.

```bash
# Confirm what the unit actually is
systemctl --user show urnetwork.service -p Type
```

> [!IMPORTANT]
> The check deliberately fails closed. An earlier revision gated only on the
> version string, so the trigger fired, the internal handoff silently aborted,
> and the "hotswap triggered" path skipped the restart fallback entirely,
> turning every update on a pre-existing node into a permanent no-op. Losing
> zero-downtime is the safe failure; a bricked update is not.

**To get zero-downtime updates on a node**: reinstall it, so it picks up the
`Type=notify` unit. A binary update alone will not do it.

---

## 6. Confirming a Settings Change

**Symptom**: `urnet-tools set <key> <value>` returns success, but the provider
does not appear to be using the new value.

The CLI's exit code only says the request was accepted. The provider logs what
it actually did, and that is the authoritative signal:

```bash
urnet-tools logs | grep -E '\[control\]|\[identity\]'
```

| Log line | Meaning |
| :--- | :--- |
| `⚙️ [control] set <key>=<v> (was <old>)` | Applied live. Nothing further needed. |
| `⚙️ [control] applied N queued override(s) ... : <keys>` | The provider was stopped when you ran the command; the change was queued and has just been merged on start. |
| `⚠️ [control] ... persisted but live apply failed ...` | Stored, and will take effect on the next restart, but the immediate part failed. |
| `❌ [control] set <key>=<v> rejected: ...` | Not applied at all. The message says why. |
| `🏷️ [identity] dashboard label changed: ... -> ...` | A `rename` or `show-ip` change reached the backend on a renewal. |

**No `[control]` line at all** means the provider never received the request.
Check that it is running and that its socket is bound:

```bash
urnet-tools status          # reports control socket liveness
urnet-tools providers --all # as root, to rule out a same-user collision
```

> [!NOTE]
> `profile` and `ramlogs` are set through the socket but need the restart that
> `urnet-tools` performs for you, because buffer sizing is fixed at startup and
> ramlogs is a live stdout redirect. Reads (`get`) are deliberately not logged,
> since `urnet-tools status` polls them on every invocation.
