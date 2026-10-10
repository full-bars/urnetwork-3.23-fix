# 😈 FreeBSD

This page is the complete guide to running the URnetwork provider on FreeBSD (amd64 and arm64): install, the `rc.d` service, updates, the required sysctl tuning, and the platform differences you will run into. For the other platforms and the general installer, see the [Installation Guide](Installation.md).

The provider runs as an `rc.d` service named `urnetwork` under an unprivileged user. There is no hosted one-line installer for FreeBSD: you fetch the installer script and run it, and it downloads the release for your architecture.

## Before you start

The base system has no `jq` and no `python3`. The installer works without them, but they change one thing: they are what lets it verify and install the Go `urnet-tools` binary (see [What the installer does](#what-the-installer-does)). Install `curl`, `sudo` and, recommended, `jq`:

```sh
pkg install -y curl sudo jq
```

Run the installer as the account the provider should run as, with `sudo`, or as root with an explicit account (see [Service user](#service-user)).

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts/Provider_Install_FreeBSD.sh -o urnet-install.sh
sudo sh urnet-install.sh install
```

To install a specific release instead of the latest:

```sh
sudo sh urnet-install.sh install v3.23.0-fix.32.7
```

The installer takes `latest` by default, resolves the tag through the GitHub API, and falls back to `https://dl.fullbars.xyz/latest-version` when the API is rate limited. It supports `amd64` and `arm64`; any other architecture stops the install.

### Service user

The provider must not run as root, and the installer refuses to set that up. It picks the account in this order:

1. `SERVICE_USER_OVERRIDE`, if set.
2. `SUDO_USER`, the account that ran `sudo`.
3. `logname`, the owner of the login session, unless it is `root`.
4. `id -un`, the current user.

If the result is `root` (a genuine root login with none of the above), the install stops before downloading anything:

```text
refusing to install a service that runs as root
```

From a root shell, name the account:

```sh
SERVICE_USER_OVERRIDE=urnet sh urnet-install.sh install
```

The account must already exist. Create one with `pw useradd urnet -m -s /bin/sh` if you want a dedicated user. All paths below come from that user's home directory, read from the passwd database (`getent passwd`, then `pw usershow`), not from `$HOME`, which under `sudo` can still be root's.

### What the installer does

| What | Where |
|---|---|
| Provider binary | `~/.local/share/urnetwork-provider/bin/urnetwork` (service user's home) |
| Installed version tag | `~/.local/share/urnetwork-provider/version` |
| `urnet-tools` | `~/.local/share/urnetwork-provider/bin/urnet-tools` |
| State, including the login token | `~/.urnetwork` (service user's home) |
| `rc.d` script | `/usr/local/etc/rc.d/urnetwork` |
| Service output | `/var/log/urnetwork/stdout.log` |
| PID file | `/var/run/urnetwork.pid` |

The steps, in order:

1. Downloads `urnetwork-provider-<tag>.tar.gz` from `dl.fullbars.xyz`, and from the GitHub release if that fails, and extracts `freebsd/<arch>/provider` from it.
2. Installs the binary by staging it beside the target and renaming it, so an update over a running provider never fails with "Text file busy".
3. Installs `urnet-tools`. It downloads the `urnet-tools-freebsd-<arch>` release asset and installs it only if its SHA-256 matches the digest GitHub publishes for the asset (read with `jq`, or with `python3` when `jq` is absent). `sha256(1)` from the base system does the check, with `openssl` as a fallback. If there is no digest to compare (neither `jq` nor `python3`), the download fails, or the sum does not match, it installs the installer script itself as a shell `urnet-tools` instead. The shell version covers `start`, `stop`, `restart`, `status`, `update`, `version`, `hot-restart`, `self-heal`, `session`, `proxy refresh|remove-dead|summary`, `auth` and `logs`. Install `jq` and re-run `update` to get the full Go tool.
4. Writes the `rc.d` script and, with `sysrc`, sets `urnetwork_enable=YES` and `urnetwork_user=<service user>` only if they are not already set, so an operator's choice survives every update.
5. Starts the service. On a fresh install it always starts; on `update` it restarts only a service that was already running, so a provider you stopped on purpose is not resurrected by an unattended update.
6. Adds the install `bin` directory to the service user's shell profile once (`.profile`, `.zshrc`, `.bash_profile` or `.cshrc`, by the user's login shell).

When the installer is run as an ordinary user, the `rc.d` and `sysrc` steps go through `sudo`; without `sudo` it prints the commands to finish by hand.

## Authenticate

The login token has to land in the service user's `~/.urnetwork`. Get an auth code from <https://ur.io> and run the authentication as the service user:

```sh
su -l urnet -c '$HOME/.local/share/urnetwork-provider/bin/urnetwork auth <code>'
```

If you installed as your own account with `sudo`, that account is the service user and you can run `urnet-tools auth <code>` directly. Restart the service so the provider picks up the token:

```sh
sudo service urnetwork restart
```

Auth codes are single-use, so fetch a new one if a code was already submitted.

## Service commands

`urnet-tools start|stop|restart|status` run the `rc.d` service for you, through `sudo` when you are not root. You can also drive it yourself:

```sh
service urnetwork start
service urnetwork stop
service urnetwork restart
service urnetwork status
```

The script does not use `rc.subr`'s own process tracking, which keys off `procname` and `command` and would report "not running" for this service. It manages a pidfile directly:

- **start** launches the provider through `daemon(8)` and returns. If the pidfile points at a live process it prints `urnetwork is already running as pid <pid>; not starting a second copy.` and does nothing, so a double start cannot create two providers.
- **stop** sends `SIGTERM` to the pid and polls `kill -0` once a second for up to 30 seconds. If the provider is still alive after that it sends `SIGKILL`, then removes the pidfile. The 30 seconds covers the provider's connection drain.
- **status** (`status_cmd`) prints `urnetwork is running as pid <pid>.` and exits 0, or `urnetwork is not running.` and exits 1.

`kill -0` on a process owned by another user fails with `EPERM`, so the installer's own status check runs through `sudo`. Run `service urnetwork status` as root, or through `sudo`, when you are not the service user.

Boot start is controlled by `rc.conf`:

```sh
sysrc urnetwork_enable=YES    # start at boot
sysrc urnetwork_enable=NO     # do not start at boot
sysrc urnetwork_user          # show the service user
sysrc urnetwork_flags="..."   # extra flags passed to `provider provide`
```

`urnet-tools auto-start on|off` sets `urnetwork_enable` for you, and refuses if the `rc.d` script is not installed.

### Logs

```sh
tail -f /var/log/urnetwork/stdout.log
```

`daemon(8)` is started with `-f -t urnetwork -p /var/run/urnetwork.pid -o /var/log/urnetwork/stdout.log -u <user>`:

- **`-o` with no `-m`** sends both stdout and stderr to the file. `-m 0` would discard the provider's output, which is why the script omits it.
- **`-p`** writes the pid of the provider (the child), not of the `daemon` process, so `stop` and `status` act on the provider itself.
- **`-u`** runs the child as the service user and sets its `HOME`, `USER` and `SHELL`, which is why the state directory lands in that user's home.

The log directory is created and handed to the service user on every start.

## Updates and auto-update

```sh
urnet-tools update
```

Update re-runs the install for the latest release (or `urnet-tools update <tag>`), keeps your `rc.conf` choices and shell profile, and restarts the service only if it was running. The installer's update is a plain service restart; HotSwap is a systemd feature.

Turn on a schedule:

```sh
urnet-tools auto-update weekly
```

The interval is `daily`, `weekly` or `monthly`, and `off` removes it. On FreeBSD it is a line in the **invoking user's crontab**, not a systemd timer, and the times are fixed:

| Interval | Cron spec |
|---|---|
| `daily` | `17 3 * * *` (every day, 03:17) |
| `weekly` | `17 3 * * 0` (Sundays, 03:17) |
| `monthly` | `17 3 1 * *` (the 1st, 03:17) |

The line runs `urnet-tools update -f` and appends its output to `~/.urnetwork/auto-update.log` in that user's home. It ends in the marker comment `# urnet-tools auto-update urnetwork`, which is how a later change replaces or removes only its own line and leaves the rest of the crontab alone. Changing the interval never leaves two schedules.

Two things differ from a systemd timer:

- **No catch-up.** `cron` does not run a missed job. A box that was off at 03:17 waits for the next scheduled time.
- **Restarting needs privilege.** The update restarts the service through `sudo service` unless the crontab belongs to root. For unattended updates, schedule it from root's crontab (`sudo urnet-tools auto-update weekly`), or allow the user passwordless `sudo` for `service`, since cron has no terminal to ask for a password.

## Sysctl tuning (required for high connection counts)

Run the tuner as root:

```sh
sudo urnet-tools optimize
```

It sets these keys, in this order:

| Key | Target | Why |
|---|---|---|
| `kern.ipc.maxsockbuf` | `16777216` | Socket buffer ceiling. Must be raised first. |
| `net.inet.tcp.recvspace` | `4194304` | Default TCP receive buffer. |
| `net.inet.tcp.sendspace` | `4194304` | Default TCP send buffer. |
| `kern.maxfiles` | `200000` | System-wide descriptors; one socket per proxied connection. |
| `kern.maxfilesperproc` | `100000` | Per-process descriptors. |

> [!IMPORTANT]
> **Raise `kern.ipc.maxsockbuf` first.** Creating a TCP socket reserves the default send and receive space, and the kernel refuses a reservation above about 0.889 of `kern.ipc.maxsockbuf` (the cluster-to-mbuf ratio, 2048 / (256 + 2048)). Set the TCP buffers to 4 MiB while `maxsockbuf` is still at its default of about 2 MiB, and every new TCP socket fails with `ENOBUFS`. Size `maxsockbuf` to at least about 1.15 times the largest TCP buffer you set; 16 MiB covers the 4 MiB defaults with room to spare. Persisted, this failure survives a reboot.

If you tune by hand, apply them in the same order:

```sh
sysctl kern.ipc.maxsockbuf=16777216
sysctl net.inet.tcp.recvspace=4194304
sysctl net.inet.tcp.sendspace=4194304
sysctl kern.maxfiles=200000
sysctl kern.maxfilesperproc=100000
```

Behavior of `urnet-tools optimize`:

- **Never lowers a value.** Each key is applied as the larger of the current value and the target, so a site that already runs `kern.maxfiles=500000` keeps it.
- **All or nothing.** It reads every key first and aborts before changing anything if one cannot be read. If a live write or the file write fails, it rolls the live values and `/etc/sysctl.conf` back to what they were.
- **Persists in `/etc/sysctl.conf`.** Existing assignments to these keys are replaced in place; new ones go in a block between `# --- urnet-tools optimize (managed block) ---` and `# --- end urnet-tools optimize ---`. The file is rewritten through a temporary file and a rename.

To persist by hand, put `key=value` lines in `/etc/sysctl.conf`:

```sh
cat >> /etc/sysctl.conf <<'EOF'
kern.ipc.maxsockbuf=16777216
net.inet.tcp.recvspace=4194304
net.inet.tcp.sendspace=4194304
kern.maxfiles=200000
kern.maxfilesperproc=100000
EOF
```

> [!WARNING]
> **Never persist a sysctl with `sysrc`.** `sysrc` edits `/etc/rc.conf`, whose entries are shell variable assignments, and a dotted MIB such as `net.inet.tcp.recvspace` is not a valid shell variable name. The line is written, nothing consumes it, and the setting silently reverts at the next boot. `/etc/sysctl.conf` is the file `/etc/rc.d/sysctl` applies at boot.

`optimize` does not set a conntrack-style limit: FreeBSD has no `nf_conntrack`, and `pf` table sizes are set per ruleset, not with a global sysctl.

## Platform notes

- **No `jq` or `python3` in the base system.** Install `jq` for the installer's digest check (see step 3 above).
- **User database fields differ.** The installer reads home and shell with `getent passwd` and falls back to `pw usershow`, whose fields are numbered differently (home is field 9 and shell field 10, against 6 and 7 for `getent`). Use whichever your tooling expects.
- **Swap comes from `swapinfo -k`.** The provider's memory-pressure checks read swap usage by running `swapinfo -k`, because FreeBSD does not expose a swap-used sysctl.
- **ZFS ARC understates available memory.** The host's available memory is estimated from the free, inactive, cache and laundry page counters (`vm.stats.vm.v_*_count`). Memory held by the ZFS ARC is wired and is not counted, so on a ZFS host `top` can show plenty of reclaimable memory while the provider sees less. Cap the ARC with `vfs.zfs.arc_max` in `/boot/loader.conf` if memory headroom matters on a small box.
- **Jails.** Provider discovery uses `procstat(1)` and makes no jail distinction; a provider in a jail is not specially recognized.
- **`daemon(8)` and the pidfile.** The pidfile holds the provider's pid, so `kill -0 $(cat /var/run/urnetwork.pid)` is a valid liveness check from a root shell.

## Release assets

Each release publishes, for both `amd64` and `arm64`:

| Asset | Contents |
|---|---|
| `urnetwork-provider-<tag>.tar.gz` | The universal tarball. The FreeBSD provider is at `freebsd/<arch>/provider`. The installer reads it from here. |
| `urnet-tools-freebsd-<arch>` | The Go `urnet-tools` binary, verified by SHA-256 at install. |
| `urnetwork-provider-<tag>-freebsd-<arch>.tar.gz` | A flat per-platform tarball with `provider` and the installer script as `urnet-tools`. |

If the installer reports `FreeBSD/<arch> binary not found in release tarball`, the release it fetched has no FreeBSD build; install a newer tag.

## Uninstall

There is no hosted uninstall script for FreeBSD. As the service user and root:

```sh
sudo service urnetwork stop
sudo sysrc -x urnetwork_enable urnetwork_user
sudo rm -f /usr/local/etc/rc.d/urnetwork
urnet-tools auto-update off
rm -rf ~/.local/share/urnetwork-provider
sudo rm -rf /var/log/urnetwork
```

`~/.urnetwork` holds the login token and proxy state; delete it too only if you do not intend to reinstall.
