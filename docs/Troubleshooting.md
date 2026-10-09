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
| Proxies marked `NEVER UP` or `DROPPED` | Target proxy failure or connection drop | Run `urnet-tools proxy health` and prune with `urnet-tools proxy remove-dead`. |
| `urnet-tools hotswap` restarts instead of handing off | Unit is `Type=simple`, not `Type=notify` | Expected on any node installed before v3.23.0-fix.31.0. The update still applies via restart. See [HotSwap declines on an existing node](#5-hotswap-declines-on-an-existing-node). |
| A setting from `urnet-tools set` seems not to apply | Change was queued, rejected, or applied only on restart | Check the provider log for `⚙️ [control]` / `❌ [control]`. See [Confirming a settings change](#6-confirming-a-settings-change). |
| `status` shows a running PID but no control socket | Startup failure, or a second provider for the same OS user | The socket is the liveness signal, not the PID. Check the log for a startup error and `urnet-tools providers --all` for a collision. |
| `[proxy][resources] warning` at startup | The memory limits are short for the pool (heap against `GOMEMLIMIT`, or footprint against the RAM the box gives the provider) | Advisory: the provider never changes limits. Lower the pool with `urnet-tools proxy trim <N>` or raise the limit. See the [Log Message Reference](../LOG_REFERENCE.md). |
| `[proxy][resources] low memory headroom`, or kernel OOM kills | The box is short of memory. Connected clients drive memory, so this can happen with no change in the proxy count | Read the decisions with `urnet-tools autopilot log`. Lower the pool with `urnet-tools proxy trim <N>`, or let the provider cap itself after a kill with `urnet-tools set oom-cap on` (default is `shadow`: it logs only). |
| `last restart: unclean` after a plain restart | Expected in 32.8: a plain `systemctl restart` reads `unclean`, the same as a crash | An in-place `urnet-tools update` reads `update`. A real crash also reads `unclean`. Telling a plain restart from a crash is tracked in [#707](https://github.com/full-bars/urnetwork-3.23-fix/issues/707). |
| `last restart: unclean` after an update on a build older than 32.8 | Old builds read a version change as a crash | Fixed in 32.8: an upgrade reads `update`. |
| Update restarts instead of hotswapping, `low_memory` outcome | The box cannot hold two providers at once | Expected on a small box. See [HotSwap](HotSwap.md). |

> [!NOTE]
> **Discovery dedup fix (v3.23.0-fix.31.2):** Previous versions could list duplicate providers when the same binary was registered under multiple paths or when `urnet-tools providers` scanned overlapping install locations. As of v31.2, provider discovery deduplicates entries by `(binary_path, uid)`, so `urnet-tools providers` and `urnet-tools providers --all` no longer show phantom duplicates. If you still see duplicates after upgrading, re-install the provider with `curl -fSsL https://dl.fullbars.xyz/install.sh | sh` to refresh the registration.

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
| 75 | Planned restart requested by the swap-thrash watchdog. It is not a crash. Under systemd with `Restart=on-failure` the unit restarts the provider. In a container the start script restarts it after 5 seconds, and only when `URNETWORK_EXIT75_OK=1` allowed the watchdog to exit at all. See [Swap-thrash watchdog](Configuration.md#swap-thrash-watchdog). |

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
6. **Exit 75** is the thrash watchdog's planned restart, not a fault. The start scripts restart the provider after 5 seconds and leave the JWT alone.
7. **All other non-zero codes** indicate a configuration or environment problem. The fatal message describes the specific issue.

### Fatal messages always write to both logs and stderr

`shmLogFatal` (the function behind every non-zero exit) writes the `FATAL [exit <code>]` line directly to the ramlog file before calling `os.Exit`. This bypasses the normal log pipe, so the message is never lost to a goroutine scheduling race. It also writes to the original stderr so the message appears in `docker logs` regardless of the ramlog setting. You do not lose the error message no matter how you view logs.

## 3. Common OOB (Out-of-Band) Errors

The signaling layer is responsible for contract creation and connection handshaking.

| Error | Cause | Recommended Action |
| :--- | :--- | :--- |
| `oob err = Timeout` | The signaling response took >60s. | Check for network congestion or CPU starvation. |
| `oob err = Invalid` | Authentication token (JWT) or credentials failed. | Verify your `<AUTH-CODE>` or email/pass. |
| `exit could not create contract` | Repeated timeouts prevented contract initialization. | See [Performance Tuning](High-Volume-Performance-Tuning.md). |

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
The update prints `hotswap trigger unavailable (...); falling back to service restart`.

**Cause**: the systemd handoff requires a `Type=notify` unit. The retiring
process aborts whenever systemd started it (`INVOCATION_ID` is set) but
`NOTIFY_SOCKET` is empty, which is exactly what a `Type=simple` unit looks like.
`Provider_Install_Linux.sh` deliberately writes a unit with **no `Type=` line**
(systemd's default, `simple`), because `Type=notify` blocks `systemctl start`
until the provider is ready and can wedge every start when the binary and the
unit come from different releases. Instead, `urnet-tools update` migrates the
unit to `Type=notify` when the binary it installs can signal readiness
(v3.23.0-fix.31.0 or newer). That first update is a normal restart; later ones
hotswap.

> [!WARNING]
> Releases up to and including v3.23.0-fix.32.0 skipped that migration for units
> with no `Type=` line, so those nodes stay on restart-only updates
> indefinitely, and the "update migrates it" wording in the decline message was
> not true for them. Fixed in v3.23.0-fix.32.1.

```bash
# Confirm what the unit actually is
systemctl --user show urnetwork.service -p Type,NotifyAccess   # drop --user for a system unit
```

**Fix**: nothing manual. Run `urnet-tools update` on a build that includes the
migration fix: the first update converts the unit to `Type=notify` and restarts
once, and the updates after that hotswap (see [HotSwap](HotSwap.md#which-update-hotswaps)).
If the update prints `note: ... is Type=simple but its unit file cannot be
migrated automatically`, `Type=` is set by a drop-in you control: change it
there to `Type=notify` and `NotifyAccess=all`, then `systemctl daemon-reload`.
A `hotswap unavailable: the running provider was started before its systemd
unit became Type=notify` message means the unit was converted but the provider
has not restarted since; the update restarts it for you and later ones hotswap.

> [!IMPORTANT]
> The check deliberately fails closed. An earlier revision gated only on the
> version string, so the trigger fired, the internal handoff silently aborted,
> and the "hotswap triggered" path skipped the restart fallback entirely,
> turning every update on a pre-existing node into a permanent no-op. Losing
> zero-downtime is the safe failure; a bricked update is not.

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

---

## 7. Claim and Wallet Messages

These come from `provider claim`, `provider wallet set` and `provide --wallet`. None of them prints a token.

| Message | Meaning and fix |
|---------|-----------------|
| `claim needs the login of a client that served traffic: pass --store-client=<key> ...` | `provider claim` needs a credential. A node holds hundreds of identities and only one that served traffic has a payout. Pass `--store-client=<key>` (the proxy address the identity was minted for, or `direct`), `--provider-jwt=<file>`, or `--legacy-coldkey=<coldkey_ss58>` for an epoch without a provider artifact. |
| `pass only one of --store-client, --provider-jwt and --legacy-coldkey` | Give exactly one credential flag. |
| `no client "<key>" in <path> (N identities) ...` | The store has no identity under that key. Keys are in `~/.urnetwork/.client_jwts.json`. |
| `N identities in <path> share the address "<addr>" ...` | Several proxy accounts at one gateway each hold an identity, so a bare address is ambiguous. Pass the full key as `--store-client=$'<addr>\x1f<user>'`. |
| ``the network jwt has expired; run `provider auth` again, then retry`` | `--legacy-coldkey` sends the network token, and it is past its expiry. Run `provider auth` and retry. |
| `claim: the platform refused: <message>` | The platform answered the claim with an error, for example `No claimable epoch.`. The command exits 1 with no stack trace. |
| `<source> does not name a client (is it the network token?) ...` | The token is the network token. Claim needs a client token. |
| `<source> has expired; let the provider renew it or pass a fresher --provider-jwt` | Start the provider so it renews the identity, or pass a newer token file. |
| `No claimable epoch.` | The platform's own answer: there is nothing to claim for that epoch with that client. Try another identity that served traffic, or another `--epoch`. |
| `subnet wallet not set: provider wallet set sends the unsigned network wallet request ...` | The unsigned request is refused by default, because the platform is moving to a signed wallet consent. Set the wallet in the URnetwork app or web account, or add `--legacy-network-wallet` to send the unsigned request anyway. `provide --wallet` prints this and keeps providing. |
