# Dual-Stack and IPv6 Support

Plain-language answer to "are we dual stack, and what does that mean per proxy?"

## The short version

The provider supports both IPv4 and IPv6. There is exactly one gap: when the
provider dials **through a proxy**, it resolves the target to IPv4 first and
hands the proxy an IPv4 address. So a proxy that could use IPv6 still carries
that session over IPv4.

Nothing needs to be configured for this today, and it does not affect earnings.

## What works today

- **The provider's own platform connection** runs both families. The v33 line
  has family-pinned v4 and v6 transports; both are attempted, and IPv4 stays
  the primary control plane.
- **The proxy data plane** understands both families. The SOCKS5 UDP relay
  handles IPv4 and IPv6 destinations (`socks5_udp.go` accepts both ATYP 1 and
  4).
- **Clients reaching a proxy over IPv6** works when that proxy's endpoint is
  registered/advertised that way; the provider does not block it.
- **The direct identity** is dual stack, with IPv4 primary and IPv6 as a
  per-dial fallback. IPv4 is never demoted.

## The one gap: the provider's egress *through* a proxy

In this line (v3.23.0-fix.33.x), `ProxySettings.NewDialContext` resolves a
hostname target itself, with an **A (IPv4) query only**, and rewrites the
SOCKS5 CONNECT target to an IPv4 literal (`net.go`, `lookupProxyTarget`). The
proxy therefore never chooses the family for that connection.

Consequences:

- A box with 4,000 proxies runs one client per proxy, but every proxied
  platform session is IPv4, regardless of what the proxies support.
- IPv6 still works for the provider's own connection and for clients reaching a
  proxy over v6; it is the provider's egress through the proxy that is pinned.
- This is **not** part of the platform's modern-provider gate, which looks for
  post-quantum encryption and resizable transfer windows (both shipped).

The local resolution was deliberate: it shares a DNS cache across burst dials
and bounds a bad resolver with a short DoH timeout, so thousands of warmup
dials cannot stall behind DNS. The plain-DNS fallback behind DoH rotates its
configured servers one entry per resolver attempt, and that rotation is per
lookup rather than per process, so one lookup's attempts walk consecutive
entries of the paired v4-then-v6 list however many other lookups resolve at
the same time.

## What changes when we flip it

The v2026 engine already does it the other way: `ProxySettings.NewDialContext`
passes the hostname straight to the proxy, so SOCKS5 carries the domain name
and the **proxy's** resolver picks A or AAAA. Its control-family policy
(`control_family.go`) explicitly does not govern that path — the tunnelled data
plane carries both families and each packet keeps its own version.

Enabling the same behavior in this line means:

1. Remove the A-only rewrite (ideally behind a setting such as
   `proxy_target_family=proxy|v4`, defaulting to `v4` during rollout).
2. Accept that the proxy's resolver becomes authoritative: a proxy with broken
   DNS now fails where local IPv4 resolution previously saved it.
3. Add per-proxy evidence if selection should care: SOCKS5 never reports the
   upstream family, so proving a proxy is v6-capable needs a probe through it.
4. Keep family accounting honest: a dial failure through a proxy must never be
   counted as an IPv4 family strike. That accounting bug is what demoted IPv4
   for six hours on a production box. Since rc2, a proxied TLS handshake that
   fails is also never retried on another family: the family of the hop to the
   proxy says nothing about the destination.
5. Canary first — it changes DNS behavior for every proxied dial.

## What you need to do

- **Today:** nothing. No flag, no setting, no fleet change.
- **If you want per-proxy IPv6 now:** it is not in 33.x. It belongs to the
  engine track, with a canary and a way to turn it back off.

## Status

| Line | Provider platform connection | Proxy egress family | Proxy data plane |
|---|---|---|---|
| v3.23.0-fix.33.x | v4 + v6, v4 primary | IPv4 pinned by the provider | v4 and v6 |
| v2026 engine | v4 + v6, v4 primary, demotion ledger | chosen by the proxy | v4 and v6 |

The IPv6 and dual-stack rollout is still in its early stages. IPv4 remains the
primary control plane, and it is never demoted.
