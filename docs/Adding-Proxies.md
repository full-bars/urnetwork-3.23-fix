# Adding Proxies

This guide shows how to load your proxy list into the provider. The core command
is identical on every OS. What differs is the file path, the shell syntax, and a
couple of platform-specific pitfalls.

The command is `urnet-tools proxy add <file>`, where `<file>` points to a text
file with one proxy per line. Each line is `host:port` or `host:port:user:pass`.

## The universal sequence

Install the provider and authenticate first. Then load your proxy file:

```text
urnet-tools proxy add <path-to-your-proxy-file>
urnet-tools proxy refresh
```

`proxy add` merges the file contents (it never replaces what is already there).
`proxy refresh` reloads the file into the running provider.

Verify a moment later with one of:

```text
urnet-tools proxy traffic
urnet-tools proxy health
```

If it has been under 8-12 hours since a provider restart, `proxy refresh` may
refuse to load (a warmup lockout). Add `--force` to bypass it:

```text
urnet-tools proxy refresh --force
```

## Installing the tools

Two CLIs exist. `urnet-tools` runs alongside a provider installed as a process
(systemd / launchd / a Windows service). `urnet-docker` runs on the Docker host,
outside the containers, and delegates into them via `docker exec`.

#### Install `urnet-docker` (Docker users)

The clean one-liner, served from the download domain:

```bash
curl -fSsL https://dl.fullbars.xyz/urnet-docker.sh | sh
```

If that host is unreachable, use the GitHub source as a fallback:

```bash
curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts/install-urnet-docker.sh | sh
```

Both install only the `urnet-docker` CLI on the Docker host. They resolve the
latest release, download the binary for your OS and architecture (amd64 / arm64),
verify its SHA-256 against the release API, and install it to `/usr/local/bin`
(or `~/.local/bin` when not run as root). The tool self-updates afterwards with
`urnet-docker update`. Neither fetches the provider nor a systemd unit.

The installer supports Linux and macOS hosts. On a **Windows** Docker host,
download the matching release asset directly instead:

```powershell
Invoke-WebRequest -Uri "https://github.com/full-bars/urnetwork-3.23-fix/releases/latest/download/urnet-docker-windows-amd64" -OutFile "urnet-docker.exe"
```

#### Install `urnet-tools` (process/systemd users)

Same installer, passing the tool name:

```bash
curl -fSsL https://dl.fullbars.xyz/urnet-docker.sh | sh -s -- urnet-tools
```

GitHub fallback:

```bash
curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts/install-urnet-docker.sh | sh -s -- urnet-tools
```

## Per-OS details

The command is the same everywhere, but the file path and syntax are not.

### Linux

Put the list in your home directory and point at it with a tilde or full path:

```bash
urnet-tools proxy add ~/proxies.txt
urnet-tools proxy refresh
```

Both `~/proxies.txt` and `/home/you/proxies.txt` work.

### macOS

Identical to Linux. `urnet-tools` and the proxy commands behave exactly the same
as on Linux. Only the startup mechanism differs: launchd instead of systemd.

```bash
urnet-tools proxy add ~/proxies.txt
urnet-tools proxy refresh
```

### Windows (PowerShell)

The command is the same, but you must use the correct Windows path and watch out
for two traps.

Use `$env:USERPROFILE` so the command works for whoever runs it, without
needing to know your own username. If you prefer, you can type your real path
instead, for example `C:\Users\yourname\Downloads\proxies.txt`.

```powershell
urnet-tools proxy add "$env:USERPROFILE\Downloads\proxies.txt"
urnet-tools proxy refresh
```

Two Windows-only pitfalls:

- **The hidden `.txt.txt` extension.** Explorer hides extensions by default, so a
  file you think is `proxies.txt` may really be `proxies.txt.txt` on disk. If you
  get `proxy file not found`, check the real on-disk name:

  ```powershell
  dir "$env:USERPROFILE\Downloads\proxies.txt*"
  ```

- **Backslash paths and spaces.** The path must be quoted if it contains spaces.
  `"$env:USERPROFILE\Downloads\proxies.txt"` handles both cases.

### Docker

The provider runs *inside* a container. The container cannot see files on the
Docker host, so the plain `urnet-tools proxy add /host/path/list.txt` command
does not work by itself. You have two options.

#### Option A: `urnet-docker` (recommended)

`urnet-docker` runs on the Docker host and handles the copy for you. It copies
your host file into the container and runs the add there. Point it at your
host-side file directly:

```bash
urnet-docker proxy add --unit urfix ~/proxies.txt
urnet-docker proxy refresh --unit urfix
```

You can pass straight file paths (with automatic `~` home directory expansion),
straight URLs (`urnet-docker proxy add https://example.com/proxies.txt`), or
flag aliases (`--file=` / `--proxy_file=`).

`--unit urfix` selects the container by name. If you have one container, the
name is optional; if you have several, pass the one you want.

#### Option B: `urnet-docker proxy paste` (fastest for pipes/stdin)

You can pipe proxies straight into the container without saving a file to disk:

```bash
cat proxies.txt | urnet-docker proxy paste --unit urfix
```

#### Option C: manual `docker cp` + `docker exec`

Copy the file into the container, then run the add inside it:

```bash
# 1. copy your host file into the container
docker cp ~/proxies.txt urfix:/tmp/proxies.txt

# 2. add it from inside the container
docker exec -it urfix urnet-tools proxy add /tmp/proxies.txt
docker exec -it urfix urnet-tools proxy refresh
```

`urnet-tools proxy add` accepts straight file paths directly (as well as `--file=` and `--proxy_file=`).

All options trigger immediate reload or can be followed by a `refresh` (use `--force` if the warmup
lockout applies). Verify with `urnet-docker proxy traffic --unit urfix` or
`docker exec -it urfix urnet-tools proxy traffic`.

> [!NOTE]
> You can also mount your proxy file straight into the container at creation
> time with a bind mount, for example `-v /path/to/proxy.txt:/app/proxy.txt`.
> That is useful when the list already exists before you start the container.
> For a running container, whether its list is mounted or not, `urnet-docker
> proxy add` (Option A) or `urnet-docker proxy paste` (Option B) is the simplest way to add or replace proxies.

## Tidying up

- To see live traffic: `urnet-tools proxy traffic`.
- To see proxy health: `urnet-tools proxy health`.
- To remove a proxy or clear the list: see the `proxy` subcommand help with
  `urnet-tools proxy --help`.
- `urnet-tools proxy clear` removes all proxies AND any URL proxy sources you have
  configured. If you use URL sources, set them again afterwards.
