# Pelican egg — UrNetwork (full-bars 3.23-fix)

Pelican panel support for the hardened provider image.

## Import

1. Panel admin → **Nests → Create** → import `pelican/egg-urnetwork-323fix.json`.
2. The egg pulls `ghcr.io/full-bars/urnetwork-3.23-fix:latest` (multi-arch amd64/arm64).
3. Assign to a node, create a server, fill the admin-only variables.

## Variables

| Variable | Panel role | Values |
|---|---|---|
| `BUILD` | user-editable | `stable`, `nightly`, `jwt` |
| `USER_AUTH` | user-editable | account email/phone (stable + nightly) |
| `PASSWORD` | admin-only | account password (stable + nightly) |
| `AUTHCODE` | admin-only | auth code from ur.io (`BUILD=jwt`) |
| `PELICAN` | hidden, fixed `yes` | routes the entrypoint into panel mode |
| `ENABLE_VNSTAT` | hidden, fixed `false` | see security note below |
| `ENABLE_IP_CHECKER` | hidden, fixed `false` | diagnostic only |

Auth modes:

- `BUILD=stable` / `BUILD=nightly`: provider authenticates with `USER_AUTH` +
  `PASSWORD`; the panel script retries authentication and supervises crashes
  (3 crashes ⇒ wipe session and re-auth).
- `BUILD=jwt`: single-shot `auth-provide "$AUTHCODE" -f`. Generate the code at
  https://ur.io and paste it into the `Auth code` variable.

## Updates

Runtime self-update is DISABLED under Pelican (`start_nightly.sh` skips its
update check; `urnet-tools update` refuses). The published image is the single
source of truth: to update a server, re-pull the image from the panel
(**Settings → Reinstall** or docker-level `pull`). This prevents the container
from silently replacing the audited fork binary with whatever the release API
serves mid-flight.

## Security notes

- vnStat's web UI (port 8080) has no authentication and exposes traffic
  counters. It ships OFF and cannot be enabled via the panel — build a custom
  image with `ENABLE_VNSTAT=true` if you accept that.
- State lives under `$HOME/.urnetwork` inside the server's data volume.
  All scripts resolve state there, so no path is hardwired to /root.
- Credentials are admin-only in the egg definition; users of the server
  cannot read them back from the panel UI.

## Resource / capability requirements

The provider needs raw packet access. On the node running this egg:

- container needs `NET_ADMIN` (and typically `NET_RAW`);
- IP forwarding enabled on the host;
- outbound UDP for WebRTC P2P transport.

Ports do NOT need to be publicly forwarded — the provider dials out.

## Smoke-testing an import

After creating the server, press Start. A healthy boot prints, in order:

```
>>> UrNetwork >>> Running Pelican Panel mode...
[INFO] Public IP detected: <ip>
[INFO] Credentials found
[INFO] Authentication successful - JWT written
```

and the done marker is the word `Provider` emitted by the binary once it
starts serving. If auth fails, the log says which credential was missing.
