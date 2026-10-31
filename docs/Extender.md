# Run a provider as an extender

An extender is a provider that also accepts connections on ports the internet can reach and relays what clients ask it for. The role is opt-in and off by default. This page covers what has to be true on the host, the Docker run that publishes the ports, the allow list that decides which destinations the extender will forward to, and the log lines and metrics that show whether the role came up.

> [!NOTE]
> **The extender is a work in progress.** It does not activate yet and is not reliable. If you turn it on, expect errors in the provider log about activation and about carriers not answering; they do not affect the rest of the provider. Treat everything on this page as the intended behaviour, not as a working feature. The provider runs and earns normally with the role off.

## Before you start

The role needs a host where you can open inbound TCP 443. A host behind NAT with no port forward can start the role, but nothing outside can reach it.

The role runs from the provider's direct slot, because only that slot's egress is this host's own address. Configured proxies do not prevent it: the direct slot runs beside them. What does prevent it is turning direct providing off, with `provider direct off` or `DISABLE_DIRECT_IP=1`. In that case the provider logs `[extender] role not started: direct providing is disabled` and carries on without the role.

The role keeps its own identity in `~/.urnetwork/.provider.extender.key` (mode 0600, created on first use, logged once as `[extender] created identity key`). Its public half names the extender, so the file has to survive restarts. In Docker that means the state volume at `/root/.urnetwork` must be persistent, which the install docs already require.

## What the role listens on

The role tries TCP 443 first, then the UDP carriers:

- TCP 443 carries the TLS carrier.
- UDP 443 carries QUIC.
- UDP 4053 carries the DNS carrier.
- UDP 53 is a second DNS port, bound only when `URNETWORK_EXTENDER_DNS53=on`. It needs the same privilege as 443 and usually collides with a resolver already on the host. If it cannot be bound the role keeps serving 4053.

A port that cannot be bound never stops the provider. If TCP 443 is taken or needs a privilege the process does not have, the role serves the UDP carriers and tries TCP 443 again no faster than every three minutes. Binding below 1024 needs root or `CAP_NET_BIND_SERVICE`; the Docker image runs the provider as root, so the container case needs nothing extra. On a bare-metal install the log says what to do:

```text
[extender] tcp 443 unavailable: listen tcp :443: bind: permission denied; retrying no faster than 3m0s; port 443 needs CAP_NET_BIND_SERVICE (setcap cap_net_bind_service=+ep <binary>) or root
```

## Choose what the extender will forward

An extender only forwards to destinations you name. `URNETWORK_EXTENDER_ALLOWED_HOSTS` is a comma-separated list of exact hostnames and `*.domain` wildcards, and an empty list refuses every forward request. Set it before you publish the ports:

```sh
URNETWORK_EXTENDER_ALLOWED_HOSTS='example.com,*.example.org'
```

The provider logs a warning at start when the list is empty:

```text
[extender] no forward hosts configured (URNETWORK_EXTENDER_ALLOWED_HOSTS); every forward request will be refused
```

## Docker

The default run command in [Docker Deployment](/full-bars/urnetwork-3.23-fix/wiki/Docker-Deployment) publishes no ports. To run the role, turn it on and publish the carrier ports:

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
  -e URNETWORK_EXTENDER=on \
  -e URNETWORK_EXTENDER_ALLOWED_HOSTS='example.com,*.example.org' \
  -v urnetwork_data:/root/.urnetwork \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

`URNETWORK_EXTENDER` accepts `on`, `1` or `true`; any other value, including a typo, leaves the role off rather than opening a public port nobody meant to offer.

Publishing the port is only half of it. The port also has to be open on the host's firewall and forwarded by whatever sits in front of the host. On a home router that means a port forward for 443; a host behind carrier-grade NAT cannot receive it at all.

On a bare-metal install, set the same variables in the service environment (for example `Environment="URNETWORK_EXTENDER=on"` in the systemd `override.conf`) and restart the provider.

## What the log shows when the role starts

The role does not activate yet, so a working extender cannot be confirmed from this page. Setting `URNETWORK_EXTENDER=on` starts the listeners (`provider/main.go:4521`), and the lines below are what the role prints when they come up (`provider/extender_role.go:815`). Nothing in the provider registers the host with the network or sends clients to it, so these lines show that the listeners bound, not that clients can use them. For how to read the other lines, see [Log Message Reference](/full-bars/urnetwork-3.23-fix/blob/main/LOG_REFERENCE.md).

The first line that says the role is serving carries the carrier set and the forward policy:

```text
[extender] ready — serving on tcp, quic and dns; forwards allowed for 2 hosts
```

With no allow list the same line ends `forwards are disabled: set URNETWORK_EXTENDER_ALLOWED_HOSTS to the hosts clients may ask for`. After that the role logs only when something changes. When TCP 443 could not be taken and the UDP carriers serve, the state is `tcp_unavailable` and the line is written in the form `[extender] <state> — <note>`, together with the `tcp 443 unavailable` line shown earlier that names the reason:

```text
[extender] tcp_unavailable — listening
```

When TCP 443 is taken later by a retry, the role logs `[extender] serving on tcp, quic and dns` again.

The role also exports its state to Prometheus. Turn metrics on with `urnet-tools metrics on` and read the `urnet_extender_*` series: `urnet_extender_enabled`, `urnet_extender_state{state=...}` with one series per state (`off`, `starting`, `serving`, `tcp_unavailable`, `unavailable`, `error`, `closed`), `urnet_extender_carrier_listening{carrier=...}`, `urnet_extender_carrier_listen_error{carrier=...}`, `urnet_extender_requests_total{service=...}`, `urnet_extender_refused_total{stage=...}`, `urnet_extender_tcp_retry_total` and `urnet_extender_connections_active`.

## What this does not tell you

`serving on tcp` means the process bound the port inside itself. It does not mean the internet can reach the port, and it does not mean any client uses the role. This page does not describe a reachability test, because the role has no working path for clients yet.

Nothing in the provider's own code registers the host with the network or decides whether clients are sent to it. This page documents what the provider binds and serves; whether and how a published address is chosen by clients is decided outside the provider.

If the role fails to start for a reason other than a bind, such as an unreadable identity file, the provider logs `[extender] role disabled: <reason>` and keeps providing without it.
