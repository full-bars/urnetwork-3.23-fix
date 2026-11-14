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
- **The proxy data plane** understands both families. The SOCKS5 UDP packet
  format carries IPv4 and IPv6 destinations (`socks5_udp.go` accepts both ATYP 1
  and 4). For a proxied identity, the provider relays IPv4 UDP only. It refuses
  an IPv6 destination with `proxy udp: ipv6 destinations are not relayed`,
  because the relay check proves the relay over IPv4 and most relays have no
  IPv6 exit. The provider logs `client udp refused` for a refused flow.
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

## Providers without UDP or IPv6 are offered for the traffic they carry

On 2026-11-13 the network operator announced a change to provider selection. It takes effect with the operator's next server release and the next app releases. The notice is on the operator's mainnet page, `ur.xyz/mainnet`.

The operator's quality probe loads test sites through every provider about every 24 minutes and judges each provider on its checks of the last 8 hours. Where a site serves HTTP/3, the probe loads it over HTTPS (TCP) and over HTTP/3 (QUIC, which runs over UDP). Where a provider's connections prove IPv6, it loads the site over IPv6 as well. A check passes only when every load passes, and at least 4 in 5 checks must pass. So a provider that carried TCP and IPv4 well but no UDP, or whose IPv6 failed, failed its checks and clients were offered it rarely. On 2026-11-12 about 20,700 providers failed only because of their HTTP/3 loads.

After the change clients are offered these providers for the traffic they carry:

- A provider whose checks failed only over HTTP/3 (UDP) is offered for TCP traffic. Clients never send it UDP.
- A provider whose connections prove IPv6 and whose checks failed only over IPv6 is offered for IPv4 traffic.
- A provider with no failed check over UDP or IPv6 is offered for both, as before.
- A provider that regains UDP or IPv6 is offered for it again within 8 hours, when its failed checks leave the window.

Apps from before these releases keep the old selection, so these providers gain traffic as the apps update. Scoring, weights and payouts do not change. Pool rewards are still split by completed usage.

### What it means for this fleet

- A proxy with no UDP relay fits the first case, because the identity's UDP cannot leave through that proxy. The provider already logs it as `no udp relay ... tcp only` (see the Proxy UDP Relay Check in `LOG_REFERENCE.md`). As I read the notice, such an identity can now earn TCP traffic.
- A host that blocks UDP can carry UDP again. Fix the network and update the provider, because the notice says some older builds do not route UDP.
- There is no setting to change. The provider's status in the apps shows how many of its checks failed only over HTTP/3 or only over IPv6, and what it is offered for.

This line carries the client side of the design as the multi client window: IPv6 admission and a TCP-only window. SDK and proxy clients built on this tree use it. The provider binary does not.

## What you need to do

- **Today:** automatic, and no flag. Since `v3.23.0-fix.35.3` the provider keeps a
  verdict per proxy carrier. After three consecutive IPv6 failures through one
  carrier it refuses IPv6 there for 15 minutes and sends the dial race straight
  to IPv4, then lets one probe through when the cooldown ends. A streak expires
  after ten minutes, so old failures do not add up, and a probe that fails
  re-arms the cooldown. A cancelled dial is not counted, and a carrier that fails
  IPv4 too has its streak reset rather than blamed for IPv6.
- **If you want per-proxy IPv6 earlier than that:** nothing to set. The verdict
  is learned from traffic. An exit that gains IPv6 is not offered it until the
  cooldown ends. The first IPv6 dial through that carrier after the cooldown is
  the probe, so the wait is at most 15 minutes from the last mark or failed
  probe. No timer runs: the probe happens only on a dial that names IPv6.

## Status

| Line | Provider platform connection | Proxy egress family | Proxy data plane |
|---|---|---|---|
| v3.23.0-fix.33.x | v4 + v6, v4 primary | IPv4 pinned by the provider | v4 and v6 |
| v2026 engine | v4 + v6, v4 primary, demotion ledger | chosen by the proxy | v4 and v6 |

The IPv6 and dual-stack rollout is still in its early stages. IPv4 remains the
primary control plane, and it is never demoted.
