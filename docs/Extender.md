# Run a provider as an extender

An extender is a provider that also accepts connections on ports the internet can reach, so people in censored countries can reach URnetwork through it. It is the same idea as a Tor bridge: it does not earn, it helps clients get through. The role is **on by default** on the provider's direct slot, and it asks the platform to list the host so clients are sent to it. This page covers what has to be true on the host, how to see whether it is working, how to turn it off, what it will forward, and how to read the log.

## Is it working?

```sh
urnet-tools extender status
```

shows whether the role is on, which ports are up, what each address family is doing and when it tries next, how many clients are connected right now, and how many requests it has served over the last 24 hours, 7 days and its lifetime. The same block appears in `urnet-tools status`, and `urnet-tools top` has an Extender box. The provider also writes one `[health][extender]` line with the same information every few minutes, so you do not have to search the log.

A family reads **ACTIVE** when the platform has listed this host on that address family. Until then the status says why: the platform said no (with its reason), the request failed, or the extender is waiting because TCP 443 is not bound.

"Requests served" counts requests the extender handled for clients. It does not count distinct people; the provider does not record who asked.

## Turn it on or off

```sh
urnet-tools extender disable    # off, and it stays off across restarts
urnet-tools extender enable     # back on
urnet-tools extender about      # plain-language explanation of the ports and how it works (works with the provider stopped)
```

`on` and `off` work too. Both apply to the running provider at once, with no restart and no unit file or drop-in. The setting is stored by the provider's control socket, so it survives restarts and updates.

`URNETWORK_EXTENDER=off` in the environment still works as a startup default for existing installs. The control setting beats it, and an unrecognised value is ignored (the default, on, applies) and logged.

The Linux installer prints one notice and asks nothing: whether TCP 443 is free, or what holds it, and how to turn the extender off.

## Before you start

The platform lists an extender only after it proves it from the outside. It dials back to this host's own address on **TCP 443**, requires a signed answer, and forwards a real request through it to its own API. So the host needs inbound TCP 443 that actually reaches the provider. A host behind NAT with no port forward can start the role, but the platform cannot reach it and will not list it. UDP alone is not enough: with TCP 443 down, the role still listens on UDP, but the status shows the host as not listed.

The role runs from the provider's direct slot, because only that slot's egress is this host's own address. Configured proxies do not prevent it. Turning direct providing off does: the provider logs `[extender] off: direct providing is disabled` and carries on without the role.

The role keeps its own identity in `~/.urnetwork/.provider.extender.key` (mode 0600, created on first use). Its public half names the extender, so the file has to survive restarts. Do not copy it to another box or account: the platform refuses a key that belongs to another network (`key_other_network`) and the log tells you to delete the file so a new one is made. In Docker the state volume at `/root/.urnetwork` must be persistent, which the install docs already require.

The role also keeps `~/.urnetwork/extender_pacing.json` (when each address family may next ask the platform, so a restart or reboot resumes the schedule instead of asking everything at once) and `~/.urnetwork/extender_tally.json` (the request counts).

## What the role listens on

- TCP 443 carries the TLS carrier. **Required** for the platform to list the host.
- UDP 443 carries QUIC. Optional.
- UDP 4053 carries the DNS carrier. Optional. It also answers ordinary DNS queries from anyone who can reach the port, resolved over DoH and rate limited per source and in total, so that from outside it looks like a rate-limited resolver (that is part of its camouflage). It is not limited to the platform's hosts: the allow list below governs forwards, not this. Firewall UDP 4053 like any resolver you expose if you do not want that.
- UDP 53 is a second DNS port, tried by default like the miner does. It needs the same privilege as 443 and often collides with a resolver already on the host. If it cannot be bound the role logs one warning and keeps serving 4053. Set `URNETWORK_EXTENDER_DNS53=off` to skip it. It answers ordinary DNS queries like 4053 does, so the exposure note above applies, and port 53 is probed by scanners far more than 4053.

A port that cannot be bound never stops the provider. If TCP 443 is taken or needs a privilege the process does not have, the role serves the UDP carriers and tries TCP 443 again no faster than every three minutes. It says so plainly, naming what holds the port when the host will tell it:

```text
[extender] CANNOT USE tcp 443: listen tcp :443: bind: address already in use. Port 443 is held by caddy (pid 894). The platform proves an extender by dialing back to it on tcp 443, so while that port is taken this host will NOT be listed and no clients will be sent to it, even though udp 443 and dns 4053 can still listen. Free the port (stop or move whatever holds it). The bind is retried every 3m0s; next attempt at 17:42.
```

A reminder repeats every 30 minutes while the port stays blocked, and when it comes free the role asks the platform to list the host within a few minutes.

Binding below 1024 needs root or `CAP_NET_BIND_SERVICE`. The Docker image runs as root, so the container case needs nothing extra. On a bare-metal install the installer and `urnet-tools update` apply `cap_net_bind_service` to the binary, and the log gives the exact `setcap` command if the provider is not root and lacks it. On Linux, replacing the provider binary can remove the file capability; the installer restores it after a swap and warns if it cannot verify it.

## What the extender will forward

An extender forwards client traffic only to hosts on its allow list (the DNS carrier above is separate). The platform's own hosts are allowed by default: for `api.bringyour.com` that is `bringyour.com` and `*.bringyour.com`. This is required, because the platform's proof forwards a request to its own API host through the extender, and an extender that refuses it can never be listed.

`URNETWORK_EXTENDER_ALLOWED_HOSTS` adds more. It is a comma-separated list of exact hostnames and `*.domain` wildcards, and it is added to the platform's hosts, never in place of them:

```sh
URNETWORK_EXTENDER_ALLOWED_HOSTS='example.com,*.example.org'
```

## Docker

The default run command in [Docker Deployment](/full-bars/urnetwork-3.23-fix/wiki/Docker-Deployment) publishes no ports. To let the extender be reached, publish the carrier ports:

```sh
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  -p 443:443 \
  -p 443:443/udp \
  -p 4053:4053/udp \
  -e AUTH=authcode \
  -e AUTHCODE='YOUR_AUTH_CODE_HERE' \
  -e HOST_HOSTNAME=$(hostname) \
  -v urnetwork_data:/root/.urnetwork \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

No extender variable is needed. To keep the role off in a container, set `-e URNETWORK_EXTENDER=off`.

Publishing the port is only half of it. The port also has to be open on the host's firewall and forwarded by whatever sits in front of the host. On a home router that means a port forward for 443; a host behind carrier-grade NAT cannot receive it at all.

## Reading the log

Lines carry the `[extender]` tag, and the platform's answers carry a 🌉 mark, so `grep 🌉` finds every activation line and `grep '🌉❌'` every failure. For the other lines see the [Log Message Reference](/full-bars/urnetwork-3.23-fix/blob/main/LOG_REFERENCE.md).

When the role starts, with the setting it follows:

```text
[extender] started (startup; setting from default). It listens for censored clients on tcp 443, udp 443 and dns 4053, then asks the platform to list this host.
[extender] ready — serving on tcp, quic and dns; forwards allowed for 2 hosts
[extender] activation started: this host now asks the platform to list it for IPv4 and IPv6. Each family is tried separately; results are logged as they happen.
```

When it is off, it says why once at startup and how to turn it back on:

```text
[extender] off: turned off by control socket (urnet-tools extender off). Turn it back on with: urnet-tools extender on
```

Each activation attempt is logged with its outcome and, when it fails, the platform's reason and when the next try is. The periodic `[health][extender]` line summarises it:

```text
❤️ [health][extender] serving | tcp 443 up, udp 443 up, dns 4053 up | IPv4 ACTIVE 203.0.113.5 | IPv6 not active (failed): no public IPv6 address, next try 17:23 | clients now 2, requests served 24h 120, 7d 800, lifetime 5000
```

The role also exports its state to Prometheus. Turn metrics on with `urnet-tools metrics on` and read the `urnet_extender_*` series: `urnet_extender_enabled`, `urnet_extender_state{state=...}` with one series per state (`off`, `starting`, `serving`, `tcp_unavailable`, `unavailable`, `error`, `closed`), `urnet_extender_carrier_listening{carrier=...}`, `urnet_extender_carrier_listen_error{carrier=...}`, `urnet_extender_requests_total{service=...}`, `urnet_extender_refused_total{stage=...}`, `urnet_extender_tcp_retry_total` and `urnet_extender_connections_active`.

## What the status does and does not tell you

`serving on tcp` means the process bound the port inside itself. **ACTIVE** means the platform proved the host from the outside and listed it. Neither tells you that a particular client is using it; the live client count and the request totals do.

If the role fails to start for a reason other than a bind, such as an unreadable identity file, the provider logs `[extender] FAILED to start: <reason>` and keeps providing without it, retrying every 15 seconds.
