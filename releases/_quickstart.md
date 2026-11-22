## Quick start

Install on Linux or macOS:

```sh
curl -fSsL https://install.fullbars.xyz | sh -s -- install
```

Install on Windows in PowerShell. Windows Defender can flag the one-line form as a false positive, because the script downloads and unpacks a program. The safer way has two steps. Download the script, read it, then run it from disk:

```powershell
irm https://dl.fullbars.xyz/install-win.ps1 -OutFile "$env:TEMP\install-win.ps1"
powershell -NoProfile -ExecutionPolicy Bypass -File "$env:TEMP\install-win.ps1"
```

The Installation guide lists the Defender labels you may see and what to do about them.

Run the provider in Docker. Use a single-use auth code from https://ur.io and keep the state volume:

```sh
docker run -d \
  --name=urnetwork \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  --log-driver=json-file \
  --log-opt max-size=10m \
  --log-opt max-file=3 \
  -e AUTH=authcode \
  -e AUTHCODE='YOUR_AUTH_CODE_HERE' \
  -e HOST_HOSTNAME=$(hostname) \
  -v urnetwork_data:/root/.urnetwork \
  -v /path/to/proxy.txt:/app/proxy.txt \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

This release is the provider line. For miner setup, use the separate miner release and its instructions. This workflow does not publish a miner image.

On FreeBSD, run the installer as a non-root service account with sudo:

```sh
fetch -qo - https://install.fullbars.xyz | sudo sh -s -- install
```

On OpenBSD, choose an existing non-root service account and replace `urnet` with its name:

```sh
ftp -o urnet-install.sh https://dl.fullbars.xyz/install-openbsd.sh
doas env SERVICE_USER_OVERRIDE=urnet sh urnet-install.sh install
```

