<!-- DRAFT. At tag time: rename this file to releases/<tag>.md, put the tag in the title and today's date in Released, then delete this comment. -->

# NEXT_TAG

**Released:** NEXT_DATE.

Previous release: [v3.23.0-fix.35.9](https://github.com/full-bars/urnetwork-3.23-fix/releases/tag/v3.23.0-fix.35.9).

This release adds Linux ARMv7 builds and OpenBSD service guards. It fixes a handshake bug where one forged id could wedge a peer session. It ports the client side of the network operator's FP2 provider selection. The build is Go 1.27.4.

The provider binary does not run the multi client window. The FP2 port changes nothing for a provider fleet by itself. The fleet gains from the operator's server change. The server now offers a provider for the traffic it carries, even when the provider has no UDP or IPv6. See the Dual-Stack guide.

Update and restart, because the handshake fix changes the binary.

Legend: **[redeploy]** needs the new provider, tool or container image. **[installer]** needs the updated installer or script. **[no redeploy]** covers documentation, CI and tests.

## Quick start

Install on Linux or macOS:

```sh
curl -fSsL https://install.fullbars.xyz | sh -s -- install
```

On FreeBSD, run the installer as a non-root service account with sudo:

```sh
fetch -qo - https://install.fullbars.xyz | sudo sh -s -- install
```

On OpenBSD, choose an existing non-root service account and replace `urnet` with its name:

```sh
ftp -o urnet-install.sh https://dl.fullbars.xyz/install-openbsd.sh
doas env SERVICE_USER_OVERRIDE=urnet sh urnet-install.sh install
```

Install on Windows in PowerShell. Windows Defender can flag the one-line form as a false positive, because the script downloads and unpacks a program. The safer way has two steps. Download the script, read it, then run it from disk:

```powershell
irm https://dl.fullbars.xyz/install-win.ps1 -OutFile "$env:TEMP\install-win.ps1"
powershell -NoProfile -ExecutionPolicy Bypass -File "$env:TEMP\install-win.ps1"
```

The Installation guide lists the Defender labels you may see and what to do about them.

Run the provider in Docker with a persistent volume and a single-use auth code from https://ur.io:

```sh
docker run -d --name urfix --pull always --restart unless-stopped --cap-add NET_ADMIN --cap-add NET_RAW --sysctl net.ipv4.ip_forward=1 -e AUTH=authcode -e AUTHCODE='YOUR_AUTH_CODE_HERE' -v urnetwork_data:/root/.urnetwork ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

This release is the provider line. For miner setup, use the separate miner release and its instructions. This workflow does not publish a miner image.

## What's changed

### Added

- **Linux ARMv7 builds. [redeploy] [installer]** 32-bit ARM devices get provider and tool binaries, release assets and installer detection. The ARM release uses software floating point, so it also starts on hosts without a floating-point unit. ARMv6 is not supported.
- **IPv6 admission and a TCP-only window in the multi client. [no redeploy]** SDK and proxy clients built on this tree keep an IPv4 hard minimum, ask for IPv6-capable exits when the IPv6 share falls short, and hold a limited window of TCP-only providers. The provider binary does not run this code.
- **Peer capability observation. [redeploy]** The client records what each peer completed: the end-to-end handshake, the seal and the receive window it advertised. A weak reference binds the record to the live session, so a stale session cannot replay it.
- **Connect successes and dial failures record the IP version they used. [redeploy]**
- **Structured `[rel]` window log lines. [no redeploy]** The window prints `family_starved` today. The log reference lists the fields.

### Fixed

- **A forged handshake generation id cannot wedge a peer session. [redeploy]** A peer could send a ClientHello with a high id and make the responder drop every later ClientHello that carried a lower id. The responder now takes an id only from a ClientHello. It ignores foreign proofs and responses. It orders ids only while a handshake attempt is in flight.
- **OpenBSD service and installer guards. [redeploy] [installer]** `urnet-tools` tells an absent service from an ambiguous process match before it runs `rcctl`. It keeps manually started providers on the direct stop path. It checks that an `rcctl` action targets the right provider. The installer checks the service user of an existing `rc.d` script. It keeps a working provider when a step fails.
- **The BSD cron log path uses forward slashes on every build host. [no redeploy]**
- **A shutdown during the URL proxy source cycle no longer fails every source. [redeploy]** Before, a cancel marked every remaining source failed, set the resolution status to failed and warned once per source. The cycle now stops and logs one line with the cause. The results of finished sources are kept.

### Changed

- **The provider reports `YYYY.M.D-<code>-<N.M>-meso` to the backend. [redeploy]** Earlier releases ended in `-fix.<N.M>`. The platform reads only the date. 3.23 and meso-miner builds now share the suffix.
- **The build is Go 1.27.4. [redeploy]**
- **The blocklist sync runs the blocker, CFAA, Telegram and security tests against the upstream tables before it opens a PR. [no redeploy]** When they fail, the sync stops and posts the failing tests.

## Known limits

- The blocklist tables stay at the refresh before 2026-11-13. Upstream's tables from that day use a new format. They list a Telegram call reflector that upstream's own test rejects. The sync check blocks them until upstream fixes the table.
- With a fixed window size of one and a single IPv6-only exit, the window does not look for an IPv4 exit until it has room. Upstream behaves the same way.
- This line has no receive-sequence rejection records, so the handshake fix covers the handshake only. I did not verify whether an invalid contract can block lower sequence ids for good here.
- Recent exit memory and the sticky redial of a fixed exit are idle in this line, because it lacks the transport-loss signal they need.
- The OpenBSD runtime was not run on a real OpenBSD host.
