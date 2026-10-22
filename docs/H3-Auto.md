# H3 Auto

> [!NOTE]
> **Status: shipped in v3.23.0-fix.34.0** (it landed in v3.23.0-fix.33.1). Nothing changes unless you set the `h3` control key. This page describes behavior in a shipped build.

`h3 auto` is a value for the existing [`h3`](/full-bars/urnetwork-3.23-fix/wiki/Configuration#-control-socket--runtime-settings) control key. It chooses how many proxies get the H3, H3-over-DNS and DNS-pump fallback policy from the provider's memory limit and its existing memory pressure signal. It does not add a new sensor, a new goroutine or a new metric series.

## What it sizes

Auto sizes the fallback set for proxied identities, the same thing a proxy count or `all` means today. A fallback is tried only while H1 is not connected and stands down once something better is, so the set is cheap in steady state. The direct identity keeps the `direct` policy: its own fallbacks stay on and the beside-H1 auxiliary stays off. Auto never enables the auxiliary that `on` means.

## Enable or turn off

```bash
urnet-tools set h3 auto    # size the proxied fallback set from memory
urnet-tools set h3 direct  # back to the default, direct identity only
urnet-tools set h3 off     # H1 only
```

The setting is live and persisted. Clearing the key hands the decision back to `URNETWORK_H3` and the startup default. There is no environment variable for `auto`.

Entering `auto` starts the target at zero and widens from there. Leaving it resets the controller, so a later re-entry starts conservatively instead of resuming an old count.

## Memory sizing

Capacity comes from the effective memory limit, cgroup quota aware, not from host free RAM.

| Effective memory limit | Proxy slots |
| :--- | :--- |
| below 2 GiB | 0 (direct only for this feature) |
| 2 GiB | 32 |
| 4 GiB | 64 |
| 8 GiB or more | 64 (ceiling) |

The rule is one slot per 64 MiB, with a floor of 2 GiB and a hard ceiling of 64. Capacity is a ceiling, not a target: a pool with fewer proxies simply selects fewer.

## Pressure tiers

The target is evaluated on the existing 30 s pressure sweep. The pressure input is the worst of the memory components: `psi_mem`, memory availability against the effective limit, and heap as a fraction of the soft memory limit. CPU, IO, load and file-descriptor pressure do not size H3. The value is smoothed with the pressure system's asymmetric EWMA, which rises quickly and decays slowly.

| Smoothed memory pressure | Next target |
| :--- | :--- |
| `>= 0.75` | `0`, direct only |
| `>= 0.50` | `min(previous, 16, capacity)`, shed to the 16 tier |
| `0.30` to `< 0.50` | `min(previous, capacity)`, hold |
| `< 0.30` | `min(previous + 4, capacity)`, grow one step |

Growth is at most 4 proxies per sample, about 8 minutes from zero to 64 on a large box. A shed takes effect at once. The hold band between 0.30 and 0.50 means one calm sample cannot re-add the proxies a pressure tier removed.

## What changes and when

A target change re-ranks through the existing sticky ranking. Proxies that remain eligible keep their place; only vacancies are filled, by grade score, then decayed earnings, then key. Only the proxies whose eligibility changed are relaunched, through the existing relaunch path, a few at a time. A proxy carrying live clients is not cut for this and picks up the new set on its next natural restart.

The controller runs from the pressure monitor, which sweeps whether or not self-heal is on, so `h3 auto` does not depend on self-heal. When the key is not `auto`, the path reads the control state once per sweep and returns before reading a sensor.

## What you see

A target move logs one line, and only when the ranked set actually changes:

```text
[h3] auto: 16 -> 20 proxies get the fallback transports (memory pressure 0.12, capacity 64)
```

The first number is the previous target, the second the new one. `capacity` is what the effective memory limit allows, from 0 to 64. A pool smaller than the target does not produce a line per step.

The usual H3 signals still apply: the `[health]` fields (`h3_up`, `h3_tx_share`, `h3_drops`, `h3_conn_fail`), the `[proxy][udp]` lines, and the `urnet_h3_*` and `urnet_transport_*` series in `/metrics`. See [Monitoring](/full-bars/urnetwork-3.23-fix/wiki/Monitoring) and the [log reference](/full-bars/urnetwork-3.23-fix/blob/main/LOG_REFERENCE.md#widening-or-narrowing-the-h3-key-relaunches-proxies).

## Default and compatibility

- Default is off. With no `h3` key, the provider behaves as it does on v33.
- `off`, `direct`, `on`, a positive proxy count and `all` keep their existing meaning.
- `auto` parses on both the provider control socket and the `urnet-tools` client and round-trips as `auto`.
- A change between `direct`, a count, `all` and `auto` never restarts the direct identity, because the direct policy stays `direct`. A change to or from `on` does, as today.
- The feature is independent of self-heal and of the OOM cap. It reacts to the same memory pressure earlier, which is intended: shedding H3 eligibility is cheaper than shedding proxies.

## Canary guidance

The initial constants are conservative beta values, not measured guarantees. Run `h3 auto` on a 2 to 4 GiB canary first and compare it with a box on `direct` over the same window. Watch resident memory, `h3_connect_attempts_total` and the platform speed-test cost, since every added proxy is a redial.

Known risks:

- A misdetected cgroup limit can over- or undersize the target. The pressure loop corrects downward quickly and grows upward slowly.
- If every memory sensor is unavailable, pressure reads zero and the target grows to capacity. The fallbacks are cheap, but this fail-open behavior is deliberate for the beta.
- The 64, 16, 64 MiB and 4 GiB constants need a canary measurement before they are treated as final.

## Related

- [Configuration](/full-bars/urnetwork-3.23-fix/wiki/Configuration#-control-socket--runtime-settings): the `h3` row and the other control keys.
- [Monitoring](/full-bars/urnetwork-3.23-fix/wiki/Monitoring): the H3 metrics and health fields.
- [Log reference](/full-bars/urnetwork-3.23-fix/blob/main/LOG_REFERENCE.md#widening-or-narrowing-the-h3-key-relaunches-proxies): the relaunch and auto lines.
