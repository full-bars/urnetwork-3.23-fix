# Run a provider as an extender

An extender is a provider that also accepts connections on ports the internet can reach, so people in censored countries can reach URnetwork through it. It is the same idea as a Tor bridge: it does not earn, it helps clients get through. The role is **on by default** on the provider's direct slot, and it asks the platform to list the host so clients are sent to it.

You normally do nothing. This page covers how to see whether it is working, the few things the host must allow, how to turn it off, Docker, and what to do when it is not working.

## Quick start

```sh
urnet-tools extender status     # is it working?
urnet-tools extender setup      # new in this release; inspect and prepare the host
urnet-tools extender about      # a plain-language explanation of the ports and how it works
urnet-tools extender disable    # off, and it stays off across restarts
urnet-tools extender enable     # back on
```

For it to work the host needs:

1. **Inbound TCP 443 that reaches the provider.** This is the one required port. If a web server (caddy, nginx, apache) holds it, the extender cannot be listed until you free or move that service.
2. **Permission to use a port below 1024.** On Linux, a non-root provider can use `cap_net_bind_service` on the binary. The installer and `urnet-tools update` apply it. The new extender setup command checks the binary capability; FreeBSD setup is print-only and untested this release, and regular providing on FreeBSD is unaffected.
3. **A public IPv4 address**, and for IPv6 a public IPv6 address with a working route out.

UDP 443, UDP 4053 and UDP 53 are optional. Without them the host is still listed over TCP. UDP 53 is user-managed: setup does not open it.

## Is it working?

```sh
urnet-tools extender status
```

shows whether the role is on, which ports are up, what each address family is doing and when it tries next, how many clients are connected right now, and how many requests it has served over the last 24 hours, 7 days and its lifetime. The same block appears in `urnet-tools status`, and `urnet-tools top` has an Extender box. The provider also writes one `[health][extender]` line with the same information every few minutes, so you do not have to search the log.

A family reads **ACTIVE** when the platform has listed this host on that address family. Until then the status says why: the platform said no (with its reason), the request failed, or the extender is waiting because TCP 443 is not bound.

"Requests served" counts requests the extender handled, once a request has passed its checks and been handed a stream. It includes the platform's own activation check: when the platform lists the host it forwards one real request through it, so an idle host reads a few above zero (about one per activation per address family). Latency probes are not counted. It does not count distinct people; the provider does not record who asked. Use "clients now" for who is connected.

## How activation works

1. The extender binds TCP 443 first. If that fails, nothing else opens (see below).
2. The provider asks the platform to list this host, once for IPv4 and once for IPv6. Each family is tried separately.
3. The platform dials back to this host on TCP 443, asks for a signed answer, and forwards a real request through it to its own API. It also tries UDP 443 and the DNS port. If those do not answer in its 10 second budget it leaves them out of the listing and still lists the host.
4. The platform returns a signed record that says when the listing expires. Today a listing lasts 24 hours. The provider checks the record against the platform's root keys (read from the platform's `/hello`) and renews it before it expires.
5. Most refused or failed attempts are retried after 10 minutes at first, doubling up to 6 hours, or when the platform says it has room again. If a family has not activated and the platform cannot reach TCP 443, the provider can wait 12 hours before retrying. A family that is refused does not delay the other.

The platform limits how often a host may ask: 6 activations per client, per address family, per hour, counted when the platform starts probing, whether it then succeeds or not. Restarting repeatedly and failing can use that up. A refusal that says "try again in N minutes" is the platform telling the provider to wait, and the provider waits.

### What survives a restart

The role keeps four small files in `~/.urnetwork/` (in Docker, `/root/.urnetwork`, which must be a persistent volume):

| File | What it holds |
|---|---|
| `.provider.extender.key` | The extender's identity (mode 0600, created on first use). Its public half names the extender, so the file must survive restarts. Never copy it to another box or account: the platform refuses a key that belongs to another network (`key_other_network`) and the log tells you to delete the file so a new one is made. |
| `extender_pacing.json` | When each address family may next ask the platform, and whether its listing is valid. A restart resumes the schedule instead of asking everything at once. |
| `extender_directory.json` | The platform's signed records, so a restart can recognise that a listing is still valid. |
| `extender_tally.json` | The request counts for 24 hours, 7 days and lifetime. |

After a restart a family whose listing is still valid shows ACTIVE straight away. The provider then asks the platform once more at a random moment in the next 30 minutes, so a fleet that restarts together does not ask together. A family with no valid listing asks as soon as it is allowed.

## Turn it on or off

```sh
urnet-tools extender disable
urnet-tools extender enable
```

`on` and `off` work too. Both apply to the running provider at once, with no restart and no unit file or drop-in. The setting is stored by the provider's control socket, so it survives restarts and updates.

`URNETWORK_EXTENDER=off` in the environment still works as a startup default. The control setting beats it, and an unrecognised value is ignored (the default, on, applies) and logged. In Docker, where `urnet-tools extender` is not available, use the environment variable and restart the container.

The role runs from the provider's direct slot, because only that slot's egress is this host's own address. Configured proxies do not prevent it, and a proxy can never be an extender: the platform would be dialing the proxy's address, where no part of this provider listens. Turning direct providing off (`urnet-tools direct off`) does stop it: the provider logs `[extender] off: direct providing is disabled` and carries on without the role.

## Ports

| Port | Role | Notes |
|---|---|---|
| TCP 443 | TLS carrier | **Required.** The platform proves the host over this port. |
| UDP 443 | QUIC carrier | Optional. |
| UDP 4053 | DNS carrier | Optional. See the note on DNS below. |
| UDP 53 | Second DNS port | Optional and user-managed. Setup does not open it. Tried by default; skipped if something else holds it. `URNETWORK_EXTENDER_DNS53=off` skips it. |

TCP 443 gates everything. The extender binds it first, and **while it fails no other port is opened**: the platform could not list the host anyway, so nothing could use them. It retries every three minutes and opens the rest once it has the port. It says so plainly, naming what holds the port when the host will tell it:

```text
🌉❌ [extender] CANNOT USE tcp 443: listen tcp :443: bind: address already in use. Port 443 is held by caddy (pid 894). The platform proves an extender by dialing back to it on tcp 443, so while that port is taken this host will NOT be listed and no clients will be sent to it, so every other extender port stays closed until tcp 443 can be bound. Free the port (stop or move whatever holds it). The bind is retried every 3m0s; next attempt at 17:42.
```

A reminder repeats every 30 minutes while the port stays blocked. When it comes free, the log says `tcp 443 is bound again` and the platform is asked within a few minutes. (A listing the platform made earlier stays valid until it expires.)

**About the DNS ports.** The DNS carrier also answers ordinary DNS questions from anyone who can reach the port, resolved over DoH and rate limited per source and in total, so that from outside it looks like a small rate-limited resolver. That is part of its camouflage; it is upstream's design. It is not limited to the platform's hosts. Port 53 is probed by scanners much more than 4053. Firewall UDP 4053 and 53 like any resolver you do not want to expose, or turn the role off. Without them the host is still listed over TCP.

On Linux, binding below 1024 needs root or `CAP_NET_BIND_SERVICE`. The Docker image runs as root. On a bare-metal Linux install the installer and `urnet-tools update` apply `cap_net_bind_service` to the binary, and the log gives the exact `setcap` command if the provider is not root and lacks it. Setup preserves the binary's existing file capabilities when it adds this bind capability. Replacing the provider binary can remove the file capability; the installer restores it after a swap and warns if it cannot verify it.

## Prepare the host with `urnet-tools extender setup`

```sh
urnet-tools extender setup
urnet-tools extender setup --dry-run
urnet-tools extender setup --yes
```

`urnet-tools extender setup` is new in this release. On supported platforms it checks whether TCP 443 is occupied, whether the provider binary can bind it, and whether the host firewall allows the required and optional ports. It prints the exact commands and asks before changing anything. Only explicit `--yes` (`-y`) or `--force` approval skips that confirmation; non-interactive setup without either flag does not apply changes. FreeBSD setup is untested this release and print-only: it prints the `sysctl` command and its undo, and makes no changes. Regular providing on FreeBSD is unaffected.

1. **Port 443 occupancy.** Checks whether another process holds TCP 443 and stops if it does. If the holder is hidden from an unprivileged user, setup checks again after elevation.
2. **Low-port bind privilege.** On Linux, checks the provider binary's capability and the process capability bounding set, then plans the missing grant. On FreeBSD, setup prints temporary `reservedhigh=0` and undo commands for the operator. This removes privileged-port protection for all users. The temporary change does not edit `/etc/sysctl.conf`; for a non-root provider, status also requires the setting persisted there. Manage any persistent change and its undo yourself.
3. **Host firewall rules.** On supported platforms, inspects the active local firewall for TCP 443, UDP 443 and UDP 4053 and plans missing rules. UDP 53 is optional and user-managed. On FreeBSD, apply and undo pf or ipfw rules yourself using your ruleset's procedure.
4. **Confirmation gate.** If elevation is needed, interactive setup first asks to re-run under sudo. The elevated run rechecks the host, prints its current plan and asks `Apply these changes? [y/N]:` before applying it. Only explicit `--yes` (`-y`) or `--force` approval permits non-interactive changes. Without either flag, non-interactive setup does not apply changes. `--dry-run` (`-n`) prints the plan without applying it. When a manual elevated rerun is needed, setup prints the command with the original options.
5. **Bind verification.** On Linux, after capability checks, runs `<provider-binary> --extender-bind-probe` as the provider's user (`sudo -n -u <user>`) when TCP 443 is free to confirm a live low-port bind. When the running provider already holds TCP 443 or the probe cannot run without a password prompt, `setup` states plainly that the live TCP 443 bind was not tested.

Docker containers are not modified by `extender setup`; publish the ports on `docker run` or use `--network host` as shown below, and allow the ports in the host firewall.

## The firewall

Opening a port in the provider does not open it on the host. If the host has a firewall, allow what you want:

Use whichever matches your host. Only TCP 443 is required; the UDP lines are optional.

```sh
# ufw (Ubuntu, Debian)
sudo ufw allow 443/tcp
sudo ufw allow 443/udp
sudo ufw allow 4053/udp

# firewalld (RHEL, Alma, Rocky, Fedora)
# Setup adds missing runtime and permanent rules separately. It does not reload.
sudo firewall-cmd --zone=public --add-port=443/tcp
sudo firewall-cmd --permanent --zone=public --add-port=443/tcp
sudo firewall-cmd --zone=public --add-port=443/udp
sudo firewall-cmd --permanent --zone=public --add-port=443/udp
sudo firewall-cmd --zone=public --add-port=4053/udp
sudo firewall-cmd --permanent --zone=public --add-port=4053/udp

# iptables (plain)
sudo iptables -I INPUT -p tcp --dport 443 -j ACCEPT
sudo iptables -I INPUT -p udp --dport 443 -j ACCEPT
sudo iptables -I INPUT -p udp --dport 4053 -j ACCEPT
sudo ip6tables -I INPUT -p tcp --dport 443 -j ACCEPT   # for IPv6, repeat the rules with ip6tables

# nftables (adjust the table and chain names to your ruleset)
sudo nft add rule inet filter input tcp dport 443 accept
sudo nft add rule inet filter input udp dport '{ 443, 4053 }' accept
```

`iptables` and `nft` rules added this way are lost at reboot. Save them with your distribution's tool (`sudo netfilter-persistent save` on Debian and Ubuntu, `sudo service iptables save` on RHEL-style hosts, or write them into `/etc/nftables.conf`). ufw and firewalld (with `--permanent`) already persist. If you also have IPv6 enabled, make sure the rule covers it: ufw and firewalld cover both families by default, plain `iptables` does not. Other firewalls (a Windows host, a cloud provider's panel) need the equivalent inbound TCP 443 rule.

### Cloud providers block inbound ports by default

Most clouds put a second firewall in front of your VM, and it blocks inbound traffic until you allow it. Opening the port inside the VM is not enough; you must also allow inbound **TCP 443** (and optionally UDP 443 and UDP 4053) there:

- **Oracle Cloud (OCI):** the subnet's security list or the instance's network security group. Oracle's Ubuntu images also ship with their own `iptables` rules that reject inbound ports, so you usually need the host rule as well.
- **Google Cloud (GCP):** a VPC firewall rule allowing the ports to the instance (by network tag or service account). Ingress is denied by default.
- **AWS:** the instance's security group (and the subnet's network ACL if you changed the default).
- **Azure:** the network security group on the NIC or subnet.
- **Other hosts:** look for a "firewall", "security group" or "network rules" panel in the provider's dashboard.

Allow IPv6 separately if the provider's rules are per address family (GCP and AWS rules are; add an IPv6 source range `::/0` alongside IPv4 `0.0.0.0/0`). If activation works for IPv4 but not IPv6, or the log says the UDP ports did not answer, this is the first place to look.

A router in front of a home or office host needs the ports forwarded to it. A host behind carrier-grade NAT cannot receive inbound connections at all and cannot be an extender.

You can tell what the platform could reach from the activation warning. For example, `the platform left out udp 443 (quic)` means that carrier failed the platform's probe even though the family activated. Check the named port in the host firewall and any cloud security group. The health line and status continue to show it as `bound, left out by the platform (probe failed)`. The next renewal retries it; a ✅ line says when the platform accepts it.

## IPv6

IPv6 activates separately. It needs a **public IPv6 address and a working IPv6 route out**. A host with an IPv6 address but no default route cannot reach the platform over IPv6, and the attempt times out after about a minute:

```text
🌉❌ [extender] IPv6 activation try 1 failed after 60.2s: the platform did not answer in time
```

Check on the host: `ip -6 route show default` should print a route, and `curl -6 https://api.bringyour.com/hello` should answer. If either fails, fix the host's IPv6 networking (your provider's gateway setting); the extender cannot work around it. IPv4 is unaffected.

## What the extender will forward

An extender forwards client traffic only to hosts on its allow list (the DNS carrier above is separate). The platform's own hosts are allowed by default: for `api.bringyour.com` that is `bringyour.com` and `*.bringyour.com`. This is required, because the platform's proof forwards a request to its own API host through the extender, and an extender that refuses it can never be listed.

`URNETWORK_EXTENDER_ALLOWED_HOSTS` adds more. It is a comma-separated list of exact hostnames and `*.domain` wildcards, and it is added to the platform's hosts, never in place of them:

```sh
URNETWORK_EXTENDER_ALLOWED_HOSTS='example.com,*.example.org'
```

## Docker

The default run command in [Docker Deployment](/full-bars/urnetwork-3.23-fix/wiki/Docker-Deployment) publishes no ports, so the extender could not be reached. There are three ways to run it. Tested with Docker 26 on Debian 13.

**1. Host networking (simplest, IPv4 and IPv6).** The container uses the host's addresses and ports directly. Both families activated on the first try.

```sh
docker run -d --name=urfix --pull=always --restart=unless-stopped \
  --network host --cap-add=NET_ADMIN --cap-add=NET_RAW \
  -e AUTH=authcode -e AUTHCODE='YOUR_AUTH_CODE_HERE' -e HOST_HOSTNAME=$(hostname) \
  -v urnetwork_data:/root/.urnetwork \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

**2. Published ports on the default bridge (IPv4 only).** The default Docker bridge has no IPv6, so only IPv4 activates.

```sh
docker run -d --name=urfix --pull=always --restart=unless-stopped \
  --cap-add=NET_ADMIN --cap-add=NET_RAW --sysctl net.ipv4.ip_forward=1 \
  -p 443:443 -p 443:443/udp -p 4053:4053/udp \
  -e AUTH=authcode -e AUTHCODE='YOUR_AUTH_CODE_HERE' -e HOST_HOSTNAME=$(hostname) \
  -v urnetwork_data:/root/.urnetwork \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

**3. Published ports with IPv6 enabled on the bridge.** Turn on IPv6 in Docker's daemon config (`/etc/docker/daemon.json`), restart Docker, and run the same command as in (2). The container gets a private IPv6 address that Docker translates, and the platform could still reach it: both families activated on the first try.

```json
{
  "ipv6": true,
  "fixed-cidr-v6": "fd00:d0c:1::/64",
  "ip6tables": true,
  "experimental": true
}
```

**Compose.** The following example publishes ports. Changing Docker's default bridge settings in (3) does not configure the separate network that Compose creates. For IPv6, enable IPv6 on the Compose network, configure routing and port publishing, and verify reachability from outside. The example below does not configure IPv6:

```yaml
services:
  urnetwork:
    image: ghcr.io/full-bars/urnetwork-3.23-fix:latest
    restart: unless-stopped
    cap_add: [NET_ADMIN, NET_RAW]
    sysctls:
      net.ipv4.ip_forward: "1"
    ports:
      - "443:443"
      - "443:443/udp"
      - "4053:4053/udp"
    environment:
      AUTH: authcode
      AUTHCODE: "YOUR_AUTH_CODE_HERE"
      HOST_HOSTNAME: my-host
    volumes:
      - urnetwork_data:/root/.urnetwork
volumes:
  urnetwork_data:
```

With host networking, drop `ports` and `sysctls` and add `network_mode: host`:

```yaml
services:
  urnetwork:
    image: ghcr.io/full-bars/urnetwork-3.23-fix:latest
    restart: unless-stopped
    network_mode: host
    cap_add: [NET_ADMIN, NET_RAW]
    environment:
      AUTH: authcode
      AUTHCODE: "YOUR_AUTH_CODE_HERE"
      HOST_HOSTNAME: my-host
    volumes:
      - urnetwork_data:/root/.urnetwork
volumes:
  urnetwork_data:
```

The published-port examples do not publish UDP 53. If you want it and the host port is free, add `-p 53:53/udp` to `docker run` or `"53:53/udp"` to the Compose ports list. Manage the host firewall yourself. Host networking uses the host's ports directly.

### Known limitations

- The Docker RAM log follower may replay retained lines after the log is trimmed.
- A hot-swap candidate can race the old provider while it still holds a listening socket. This release does not fix that race.

In every case:

- The auth code is single-use. Keep `/root/.urnetwork` on a persistent volume: after the first start the saved login is used (`-e AUTH=authcode` with no code works), and the volume also keeps the extender identity and listing.
- No extender variable is needed. To keep the role off in a container, set `-e URNETWORK_EXTENDER=off`.
- `urnet-tools extender` is not available inside the container. Read the state with `docker logs <container> | grep 🌉` (the `[health][extender]` line repeats every few minutes) and control the role with the environment variable.
- The port also has to be open on the host's firewall and forwarded by whatever sits in front of the host.

## Reading the log

Every extender line starts with a 🌉. A line that reports an outcome adds a mark: ✅ worked, ⛔ the platform refused, ❌ failed, ⚠️ a warning. `grep 🌉` finds all of them and `grep '🌉❌'` every failure. For the full list see the [Log Message Reference](/full-bars/urnetwork-3.23-fix/blob/main/LOG_REFERENCE.md).

When the role starts:

```text
🌉✅ [extender] started (startup; setting from default). It listens for censored clients on tcp 443 (required), then udp 443 and dns 4053, then asks the platform to list this host.
🌉✅ [extender] ready — serving on tcp, quic and dns; forwards allowed for 2 hosts
🌉✅ [extender] activation started: this host now asks the platform to list it for IPv4 and IPv6. Each family is tried separately; results are logged as they happen.
🌉✅ [extender] IPv4 activation try 1 worked in 10.2s
🌉✅ [extender] IPv4 is now activated on try 1. The platform can reach this host over IPv4.
```

When it is off, it says why once at startup and how to turn it back on:

```text
🌉 [extender] off: turned off by control socket (urnet-tools extender off). Turn it back on with: urnet-tools extender on
```

The periodic `[health][extender]` line summarises everything:

```text
🌉❤️ [health][extender] serving | tcp 443 bound, udp 443 bound, dns 4053 bound | IPv4 ACTIVE 203.0.113.5; tcp 443 bound, accepted by the platform; udp 443 bound, left out by the platform (probe failed); dns 4053 bound, left out by the platform (probe failed) | IPv6 not active (failed): no public IPv6 address, next try 17:23; tcp 443 bound, platform status unknown; udp 443 bound, platform status unknown; dns 4053 bound, platform status unknown | clients now 2, requests served 24h 120, 7d 800, lifetime 5000
```

The role also exports its state to Prometheus. Turn metrics on with `urnet-tools metrics on` and read the `urnet_extender_*` series: `urnet_extender_enabled`, `urnet_extender_state{state=...}` with one series per state (`off`, `starting`, `serving`, `tcp_unavailable`, `unavailable`, `error`, `closed`), `urnet_extender_carrier_listening{carrier=...}`, `urnet_extender_carrier_listen_error{carrier=...}`, `urnet_extender_requests_total{service=...}`, `urnet_extender_refused_total{stage=...}`, `urnet_extender_tcp_retry_total` and `urnet_extender_connections_active`.

`serving on tcp` means the process bound the port inside itself. **ACTIVE** means the platform proved the host from the outside and listed it. Neither tells you that a particular client is using it; the live client count and the request totals do.

## When it is not working

| What you see | What it means | What to do |
|---|---|---|
| `CANNOT USE tcp 443 … address already in use` | Something else holds TCP 443 (often caddy or nginx). | Stop or move it. The extender retries every 3 minutes by itself. |
| `CANNOT USE tcp 443 … permission denied` | The provider may not bind below 1024. | `sudo setcap cap_net_bind_service=+ep <provider binary>`, or run as root. |
| IPv4 ACTIVE, `bound, left out by the platform (probe failed)` | The named carrier was left out after its probe failed. | Check that port in the host firewall and any cloud security group. The next renewal retries it. |
| `dns 53 unavailable … address already in use; keeping 4053` | Another resolver holds UDP 53. | Nothing to do, 4053 serves. `URNETWORK_EXTENDER_DNS53=off` silences it. |
| IPv6 `failed … did not answer in time` | No IPv6 route out, or inbound IPv6 blocked. | `ip -6 route show default` and `curl -6 https://api.bringyour.com/hello` on the host. |
| `the platform says this extender key belongs to ANOTHER network` | The identity file was copied from another box or account. | Stop the provider, delete `~/.urnetwork/.provider.extender.key`, start it again. |
| A family stays `waiting` | TCP 443 is not bound, the platform asked it to wait, or direct providing is off. | Read the status line; it says which. |
| `off: direct providing is disabled` | `urnet-tools direct off` is set. | `urnet-tools direct on`. |
| `FAILED to start: <reason>` | The role failed for a reason other than a port. | The provider keeps providing and retries every 15 seconds; fix the reason shown. |

## FAQ

**Does it earn?** No. It is a public service like a Tor bridge. It does not change what the provider earns.

**Can a proxy be an extender?** No. The platform dials the address it sees; for a proxy that is the proxy's server, where nothing of this provider listens. Only the host the provider runs on can be an extender.

**Can I run more than one on a box?** Only one process can hold TCP 443 on an address, so one extender per address.

**Does restarting cost anything?** Each family asks the platform once more after a restart, spread over the next 30 minutes. That counts toward the platform's 6 per hour per family. A still-valid listing keeps showing ACTIVE meanwhile.
