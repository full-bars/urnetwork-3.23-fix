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
| `PROXY_URL` | user-editable | comma-separated URLs to proxy lists |
| `PROXY_FILE` | user-editable | path to mounted proxy file (e.g. `/app/proxies.txt`) |
| `PROXY_URL_REFRESH` | user-editable | re-fetch interval (default `1h`) |
| `PROXY_URL_MAX` | user-editable | max proxies from URLs (default `500`) |
| `PELICAN` | hidden, fixed `yes` | routes the entrypoint into panel mode |
| `ENABLE_VNSTAT` | hidden, fixed `false` | see security note below |
| `ENABLE_IP_CHECKER` | hidden, fixed `false` | diagnostic only |

Auth modes:

- `BUILD=stable` / `BUILD=nightly`: provider authenticates with `USER_AUTH` +
  `PASSWORD`; the panel script retries authentication and supervises crashes
  (3 crashes ⇒ wipe session and re-auth).
- `BUILD=jwt`: single-shot `auth-provide "$AUTHCODE" -f`. Generate the code at
  https://ur.io and paste it into the `Auth code` variable.

## Proxies

The provider needs a source of SOCKS5 proxies to route traffic through. There are two ways to add proxies in a Pelican deployment:

### URL-sourced proxies (recommended)

1. In the panel, go to **Startup** → edit `PROXY_URL`
2. Paste one or more comma-separated URLs pointing to proxy lists:
   ```
   https://example.com/proxies.txt, https://another.io/socks5.txt
   ```
3. The provider fetches the lists at startup and auto-refreshes every `PROXY_URL_REFRESH` (default 1h)
4. To change URLs: edit the var in the panel → Pelican restarts the container

The list format is one proxy per line: `host:port:user:pass` (credentials required for file-based sources).

### File-based proxies

1. Mount your proxy list as a volume: `-v /path/to/proxies.txt:/app/proxies.txt`
2. Set `PROXY_FILE` to the container path (e.g. `/app/proxies.txt`)
3. The provider reads the file at startup

To update file-based proxies: edit the file on the host, then restart the server from the panel.

### Hot-reload

- URL-sourced proxies auto-refresh on the configured interval — no restart needed
- File-based proxies require a container restart to pick up changes
- At runtime: `docker exec <container> urnet-tools proxy refresh --force` forces an immediate URL re-fetch

## Updates

Runtime self-update is DISABLED under Pelican (`start_nightly.sh` skips its
update check; `urnet-tools update` refuses). The published image is the single
source of truth: to update a server, re-pull the image from the panel
(**Settings → Reinstall** or docker-level `pull`). This prevents the container
from silently replacing the audited fork binary with whatever the release API
serves mid-flight.

## Security notes

- vnStat ships OFF by default: its port-8080 web UI exposes traffic counters
  publicly, which is why the default is off. It was hardened a long time ago,
  so this is a conservative default, not an open vulnerability — enable it if
  you want the counters and accept the exposure.
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

```console
>>> UrNetwork >>> Running Pelican Panel mode...
[INFO] Public IP detected: <ip>
[INFO] Credentials found
[INFO] Authentication successful - JWT written
```

and the done marker is the word `Provider` emitted by the binary once it
starts serving. If auth fails, the log says which credential was missing.
