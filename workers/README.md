# Cloudflare Workers

Source for the Cloudflare Workers backing `*.fullbars.xyz`. Each subdirectory is a
`wrangler`-deployable worker with its own `wrangler.jsonc` (routes) and `src/index.js`.

| Worker | Routes | Purpose |
|---|---|---|
| `dl` | `dl.fullbars.xyz/install.sh`, `install-mac.sh`, `install-win.ps1`, `uninstall.sh`, `uninstall-win.ps1` + `install.fullbars.xyz/*` | Proxies install/uninstall scripts from `raw.githubusercontent.com`, logs to `dl-log`. On `install.fullbars.xyz` serves the smart installer dispatcher (OS auto-detect) or browser landing page. |
| `dl-fullbars` | `dl.fullbars.xyz/latest-version`, `dl.fullbars.xyz/releases/download/*` | Mirrors GitHub release tag + assets (primary download CDN). |
| `geo` | `geo.fullbars.xyz/*` | Returns request geo info (IP, city, region, country, ASN, colo, RTT). |
| `provider-redirect` | `provider.fullbars.xyz/*` | 301 to `https://github.com/full-bars/urnetwork-3.23-fix`, preserving path/query. |

## `dl` worker

Handles two hostnames:

- **`dl.fullbars.xyz`** — proxies the install/uninstall scripts:
  - `/install.sh` -> `scripts/Provider_Install_Linux.sh`
  - `/install-mac.sh` -> `scripts/Provider_Install_Mac.sh`
  - `/install-win.ps1` -> `scripts/Provider_Install_Win32.ps1`
  - `/uninstall.sh` -> `scripts/Provider_Uninstall_Linux.sh`
  - `/uninstall-win.ps1` -> `scripts/Provider_Uninstall_Win32.ps1`
  - Logs each request to `dl-log.fullbars.xyz` (via the `dl-log` Cloudflare Tunnel).

- **`install.fullbars.xyz`** — smart installer entry point:
  - `GET /` with `Accept: text/html` -> browser landing page
  - `GET /` via curl -> POSIX dispatcher that detects OS (`uname -s`) and pipes to
    the right installer:
    - Linux -> `dl.fullbars.xyz/install.sh`
    - Darwin -> `dl.fullbars.xyz/install-mac.sh`
    - MINGW/MSYS/CYGWIN -> auto-launches the PowerShell installer via `powershell.exe`
  - Aliases (`/install.sh`, `/install-mac.sh`, `/install-win.ps1`, `/win.ps1`)
    -> 302 to the `dl.fullbars.xyz` equivalents.

### Usage

```bash
# Linux / macOS (always latest)
curl -fsSL install.fullbars.xyz | sh

# Pin a version
curl -fsSL install.fullbars.xyz | sh -s -- --version=v3.23.0-fix.26.4

# Windows (PowerShell)
irm https://install.fullbars.xyz/win.ps1 | iex
```

## DNS

`dl.fullbars.xyz`, `install.fullbars.xyz`, and `provider.fullbars.xyz` use proxied `A`
records to the TEST-NET placeholder `192.0.2.1`. The orange-cloud proxy runs the Worker
before any origin is contacted, so the placeholder IP never actually serves.

## Deploy

Workers use the classic `addEventListener('fetch', ...)` service-worker format (matching
the live deployments). Deploy with `wrangler deploy` from each worker directory, or via the
Cloudflare API. Update a script via:

```http
PUT /accounts/{account_id}/workers/scripts/{script_name}
Content-Type: application/javascript   # raw body, not JSON
```
