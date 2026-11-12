# OpenBSD

This page covers running the URnetwork provider and `urnet-tools` on OpenBSD (amd64 and arm64). For FreeBSD, see the [FreeBSD guide](/full-bars/urnetwork-3.23-fix/wiki/FreeBSD). For other platforms, see the [Installation Guide](/full-bars/urnetwork-3.23-fix/wiki/Installation).

> [!IMPORTANT]
> The OpenBSD installer, `rc.d` script and release assets are verified by reading the scripts and by cross-compiling. They have not been run on an OpenBSD host. The installer and the FreeBSD installer are different scripts: do not run one on the other's platform.

## Before you start

OpenBSD's base system has `ftp(1)`, `sha256(1)`, `tar`, `awk` and `rcctl`, so the installer needs no extra package. `curl`, `wget`, `jq` and `python3` are optional. The installer reads the release tag and the SHA-256 digests with `jq`, then `python3`, then an `awk` scanner. It never runs `pkg_add` for you.

The installer needs root for the install tree and the `rc.d` script. It uses `doas` first and `sudo` as a fallback. If neither is configured, add a rule to `/etc/doas.conf` (for example `permit persist youruser`) or run from a root shell.

The provider must not run as root. The installer picks the service account from `SERVICE_USER_OVERRIDE`, `URNETWORK_USER`, an existing `urnetwork` account, `DOAS_USER`, `SUDO_USER`, then the login name, in that order. It stops with `refusing to install a service that runs as root` when the result is `root`.

The installer must reach the GitHub release API. When the metadata cannot be fetched or does not list the asset, it stops with `refusing to install` before it downloads anything.

## Install

Choose an existing non-root service account and replace `urnet` in these commands with its name. Pass it explicitly so an existing `urnetwork` account is not selected instead:

```sh
ftp -o urnet-install.sh https://dl.fullbars.xyz/install-openbsd.sh
doas env SERVICE_USER_OVERRIDE=urnet sh urnet-install.sh install
```

The `install.fullbars.xyz` one-liner also detects OpenBSD and runs the same script. It downloads with `ftp` when `curl`, `fetch` and `wget` are missing:

```sh
ftp -o - https://install.fullbars.xyz | doas env SERVICE_USER_OVERRIDE=urnet sh -s -- install
```

`dl.fullbars.xyz/install-openbsd.sh` serves `scripts/Provider_Install_OpenBSD.sh` from the `main` branch.

The release also ships the same script as `urnet-tools` inside each `urnetwork-provider-<version>-openbsd-<arch>.tar.gz` asset. To install a specific release, pass the tag:

```sh
doas env SERVICE_USER_OVERRIDE=urnet sh urnet-install.sh install v3.23.0-fix.32.7
```

From a root shell, name an existing non-root account:

```sh
env SERVICE_USER_OVERRIDE=urnet sh urnet-install.sh install
```

The installer:

- downloads `urnetwork-provider-<tag>-openbsd-<arch>.tar.gz` and verifies its SHA-256 digest before it extracts anything
- installs the provider to `/usr/local/libexec/urnetwork-provider/bin/urnetwork`, owned by root
- installs `urnet-tools` after verifying its digest
- writes `/etc/rc.d/urnetwork`, runs `rcctl enable urnetwork` on a fresh install, and starts the service
- keeps state in the service account's `~/.urnetwork`

An update leaves the operator's `rcctl enable` or `disable` choice alone.

To build from source instead, use a checkout on an OpenBSD host:

```sh
doas pkg_add go git
go build -o urnetwork ./provider
go build -o urnet-tools-openbsd ./cmd/urnet-tools
```

Check `go version` against `go.mod` first. Put the binaries in a root-owned directory that the service account cannot modify.

## Authenticate

Run authentication as the same non-root account that will run the provider. Get a single-use auth code from <https://ur.io>:

```sh
/usr/local/libexec/urnetwork-provider/bin/urnetwork auth <code>
```

The provider stores its state in that account's home directory under `~/.urnetwork`. Keep this directory private. Replace the example path with the installed binary path if you moved the binary.

## Service commands

The `rc.d` service is named `urnetwork`. Use `rcctl`:

```sh
doas rcctl start urnetwork
doas rcctl stop urnetwork
doas rcctl restart urnetwork
rcctl check urnetwork
doas rcctl enable urnetwork
doas rcctl disable urnetwork
```

Extra provider flags go in the service flags: `doas rcctl set urnetwork flags "<flags>"`. The provider output goes to `/var/log/urnetwork/stdout.log`.

The installer script offers the same commands as the other platforms: `install`, `start`, `stop`, `restart`, `status`, `update`, `version`, `hot-restart`, `self-heal`, `session` and `proxy`. Run `sh urnet-install.sh --help` for the full list.

`urnet-tools auto-start on|off` runs `rcctl enable|disable urnetwork`. It needs an installed `rc.d` script and fails with a clear message when there is none.

For a foreground test, run as the service account:

```sh
/usr/local/libexec/urnetwork-provider/bin/urnetwork provide
```

## Updates and auto-update

Run `doas env SERVICE_USER_OVERRIDE=urnet sh urnet-install.sh update` to move to a new release, using the same account as installation. Use `update`, not `install`, to preserve a stopped or disabled service. For the root-owned install, run `doas urnet-tools auto-update daily` to schedule updates in root's crontab. `urnet-tools update` needs a discoverable binary (see Platform notes).

## Sysctl tuning

There are no tested OpenBSD tuning targets in this repository. The FreeBSD sysctl names must not be assumed to work on OpenBSD. Inspect the host's limits with `sysctl` and consult the OpenBSD manual before changing them. `urnet-tools optimize` is not supported and returns an error; tune sysctls manually.

## Platform notes

- **Process discovery:** `urnet-tools` uses `ps` to find running providers by process name. It does not discover stopped services or infer a binary path from process arguments. It checks the standard libexec location for an executable, but that path is not proof of the running image. If the binary is not there, `update` and delegated proxy commands such as `add`, `remove`, `trim` and `paste` cannot resolve the binary. Install it in that location, or run its proxy commands directly. There is no `--binary` override. The libexec binary may differ from the running binary; verify the install before using it.
- **Lifecycle:** Use the installer script or `rcctl` to start, stop and restart the service. `urnet-tools start` and `restart` are not supported on OpenBSD, and `stop` signals the discovered process rather than using `rcctl`. `urnet-tools status` can run `rcctl check`.
- **Memory:** Total physical memory comes from `hw.physmem` when available. Available host memory is not reported. Host memory-pressure and swap checks use the unsupported fallback.
- **Control socket:** Peer credential verification is a no-op on OpenBSD. The control socket relies on its file permissions instead. Keep the service account and its state directory private.
- **Disk diagnostics:** Free space uses the OpenBSD `statfs` interface.
- **UDP:** OpenBSD uses the portable socket reader, not the platform-specific poller.

`urnet-tools` accepts only `/usr/local/libexec/urnetwork-provider/bin/urnetwork`. The file must be executable by other users and not writable by group or others. Binaries in a home directory are ignored, because `urnet-tools` often runs as root.

## Release assets

Each release publishes `urnetwork-provider-<version>-openbsd-amd64.tar.gz` and `-openbsd-arm64.tar.gz`, plus `urnet-tools-openbsd-amd64` and `urnet-tools-openbsd-arm64`. The combined provider tarball also carries an `openbsd/` tree.

## Uninstall

First remove the auto-update entry from root's crontab without deleting provider state. While the provider is running and discoverable:

```sh
doas urnet-tools auto-update off
```

If the provider is already stopped, edit root's crontab with `doas crontab -e` and remove the URnetwork auto-update line. Then stop and disable the service and remove its script and binaries:

```sh
doas rcctl stop urnetwork
doas rcctl disable urnetwork
doas rm /etc/rc.d/urnetwork
doas rm -r /usr/local/libexec/urnetwork-provider
```

`urnet-tools uninstall` also removes the discovered provider's state directory after confirmation, including login and proxy state. The service account's `~/.urnetwork` holds that data; remove it only if you no longer need it.
