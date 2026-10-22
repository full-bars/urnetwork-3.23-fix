# ♻️ Hot Restart: Client Identity Reuse

Every proxy slot authenticates with the platform once and receives a `client_id`. Hot restart keeps that signed client JWT on disk and reuses it across restarts, so the provider comes back as the same client instead of a brand new one.

This page covers what is stored, how the toggle resolves, how to read the current state, how to turn reuse off or on, and what makes the platform hand out a new identity anyway.

## Why reuse exists

- The `client_id` is what the platform tracks a connection under. A new id on every restart means a new client row each time, no history, and an auth burst at startup when a box walks a large proxy list.
- Reuse keeps the identity, and the reliability and location data attached to it, stable across restarts and updates.
- A restart with reuse in place does not call `/network/auth-client` at all while the stored token is still valid.

## Who owns the id

`client_id` and `device_id` are minted by the platform, not by the provider. The provider never generates, copies, or transfers an id. It keeps the token the platform issued and presents it again, and when that token has expired it asks the platform to re-sign the same client row by sending the existing `ClientId` to `/network/auth-client`. The server updates the existing `network_client` and `device` rows and returns a JWT carrying the same `client_id` and `device_id`.

## The toggle

Reuse is **on by default** since v3.23.0-fix.26. The setting resolves in this order:

1. Provider control state key `hot_restart`, when set (`on` or `off`).
2. Environment variable `URNETWORK_HOT_RESTART`, where any value other than `0` means on.
3. Default: on.

An unknown stored value falls back to on rather than silently disabling reuse after a bad write.

## Checking the current state

```bash
# The effective override, and whether it is set at all
urnet-tools set hot-restart
# hot_restart: not set for urnetwork.service (using startup default)

# The ids the provider currently holds, read from the store on disk
urnet-tools proxy ids
# PROXY            CLIENT_ID                           NETWORK_ID  AGE
# (direct)         4f2c8d1a9b7e3c5061d2e8f4a7b9c013    9a1b3c7e…   6h12m
# 10.0.0.4:8080    91bd0e5f2a6c48d73b19e0a4c8f72d55    9a1b3c7e…   6h12m
```

`proxy ids` reads `.client_jwts.json` directly, so it works while the provider is stopped. Bearer tokens are never printed, only the assigned ids.

Startup and per-slot logs state what happened:

```
🔥 [hot-restart] loaded 12 stored identities (0 pruned as stale >30d); carrying forward 12
♻️ client_id: 4f2c... (reused)
✨ client_id: 91bd... (new)
```

## Turning it off or on

The `hot-restart` command is a `set` key, not a standalone subcommand. `urnet-tools hot-restart on` fails with `unknown command "hot-restart"`.

```bash
# Reuse off: the next start mints fresh identities
urnet-tools set hot-restart off

# Reuse on again (explicit on is equivalent to the default)
urnet-tools set hot-restart on
```

`off` is stored as a value for this key, it does not clear the override. There is no separate reset to the startup default: turn reuse on explicitly instead.

The override applies to the next time a slot authenticates. Proxies that are already connected keep their identity until they reconnect. If the provider is stopped, the change is queued and applied on the next start.

The environment equivalent, for a systemd user unit:

```bash
systemctl --user edit urnetwork.service
```

```ini
[Service]
Environment=URNETWORK_HOT_RESTART=0
```

## Docker

Pass the variable at container creation. There is no default value inside the image, so leaving it unset keeps reuse on:

```bash
docker run -e URNETWORK_HOT_RESTART=0 ...
```

```bash
# Check what a running container sees
docker exec urfix sh -c 'echo ${URNETWORK_HOT_RESTART:-1}'
```

## What is stored

`$HOME/.urnetwork/.client_jwts.json`, or `$URNETWORK_STATE_DIR/.client_jwts.json` when that override is set. One entry per identity slot:

| Field | Meaning |
| :--- | :--- |
| `by_client_jwt` | The signed client token the platform issued |
| `client_id` | The id carried in that token |
| `network_id` | The network that minted it, used as a credential-swap guard |
| `minted_at` | When the entry was written, used for the 30 day prune |

The store keeps up to 10 timestamped snapshots taken before an overwrite, so a corrupted or truncated file can be inspected. Snapshots of identical content are at most one every 5 minutes, which stops a crash loop from cycling through every backup slot. A file that fails to parse is reported and the provider starts fresh instead of refusing to run.

Entries whose `minted_at` is older than 30 days are pruned at load time.

## What keeps an id and what mints a new one

| Change | Result |
| :--- | :--- |
| Provider restart, same state dir, same proxy target | Same `client_id` (reused) |
| Stored token expired, server row still present | Renewed with the same `client_id` and `device_id` |
| New proxy address | New slot, new `client_id` |
| Different proxy credentials on the same address | New slot on the sn fork, which keys by address and auth user; same slot on 3.23-fix and meso-miner, which key by address alone |
| Different account or network (`~/.urnetwork/jwt` replaced) | Fresh mint, logged as `network_id mismatch` |
| State dir wiped, new `HOME`, container recreated without its volume | Fresh mint |
| `minted_at` older than 30 days | Entry pruned, fresh mint on return |
| Reuse turned off | Fresh mint on every start |

Identities are per slot, never shared. Two proxies on one box do not present one id, and one id is not meant to be copied to another host.

## Forcing a fresh identity

There is one store per provider, and no per-slot delete command. Two ways to get a new id:

```bash
# Fresh ids for exactly one restart, then reuse again
urnet-tools set hot-restart off
urnet-tools restart -y
urnet-tools set hot-restart on
```

```bash
# Reset the whole store, then start
urnet-tools stop
mv ~/.urnetwork/.client_jwts.json ~/.urnetwork/.client_jwts.json.bak
urnet-tools start
```

Setting `URNETWORK_HOT_RESTART=0` in front of `urnet-tools restart` does nothing: the unit and the container start from their own environment, not from the caller's shell.

Both routes replace the stored identity for every slot that connects. Turning reuse back on afterwards reuses whatever the last start wrote, so a single restart with reuse off moves the whole provider to a new identity set.

## Interaction with the rest of the tool

- **Proxy audit.** Parking a proxy that grades as junk requires reuse. With hot restart off the audit still runs, but it observes and logs `would-park` instead of parking, because a relaunch would otherwise mint a new identity each time.
- **HotSwap.** A handover runs the replacement against the same state dir, so the incoming process presents the same identities as the outgoing one. The brief overlap where both processes hold connections is documented on the [HotSwap](HotSwap.md) page.
- **Session save and load.** `urnet-tools session save` bundles `.client_jwts.json` together with the account JWT and the proxy state, so loading that bundle on another box carries the same client ids with it. The backend keeps one resident per client, so two hosts running the same bundle displace each other rather than doubling throughput. It is a way to move a provider, not to clone one.

## Troubleshooting

**Ids churn on every restart.** Check `urnet-tools set hot-restart` first, then `URNETWORK_HOT_RESTART` in the unit environment or the container spec. A container recreated without a persistent state volume also churns, because the store is gone each time.

**`🔥 [hot-restart] network_id mismatch` in the log.** The account JWT now carries a different `network_id` than the stored entry, usually after switching accounts or networks. The old entries cannot be reused and are replaced on the next auth.

**`client limit exceeded`.** The platform refused a new client because the account is at its concurrent client limit. Churn makes this more likely: every fresh mint adds a row. Reuse reduces it.

**A slot comes back with a new id even though reuse is on.** Either the entry aged past the 30 day prune, the token could not be renewed, or the state dir changed. The log line for the slot says which path was taken.

**Store reported as corrupt.** The provider logs `⚠️ [jwt-store] corrupt ... starting fresh` and mints new identities. The previous bytes are kept in a snapshot beside the store for inspection.

## Verifying reuse after a restart

```bash
ids() { urnet-tools proxy ids 2>/dev/null | awk '$1 != "PROXY" && NF >= 2 {print $1" "$2}' | sort; }
ids > /tmp/before.txt
urnet-tools restart -y
ids > /tmp/after.txt
diff /tmp/before.txt /tmp/after.txt
```

An empty diff, plus `♻️ client_id: ... (reused)` lines in the startup log, means every slot kept its identity. A `✨ ... (new)` line for a slot you did not change is the one worth investigating.
