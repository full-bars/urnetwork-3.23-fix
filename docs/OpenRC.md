# 🏔️ Alpine Linux (OpenRC)

This page is the complete guide to running the URnetwork provider on Alpine Linux and other OpenRC distributions: install, authenticate, start, update, boot persistence and the limits of OpenRC supervision. For the other platforms and the general installer, see the [Installation Guide](Installation.md).

The installer detects OpenRC on its own, so there is nothing new to type: the install command is the one every other Linux uses. This page walks through a first install from a root shell, which is how Alpine starts out.

## Before you start

You need a root shell and a regular account for the provider to run as. OpenRC has no per-user service manager, so the provider runs as a system service under a dedicated, unprivileged user. The installer expects that user to exist and uses `urnet` by default.

```sh
adduser -D urnet
apk add --no-cache curl
```

`adduser -D urnet` creates the `urnet` user with a home directory and no password. If the user does not exist when you install, the installer still downloads the files but does **not** install the service, and prints the commands to finish. To run the provider under a different account, set `URNET_OPENRC_USER=<name>` for the install command:

```sh
URNET_OPENRC_USER=provider sh -c 'curl -fSsL https://dl.fullbars.xyz/install.sh | sh'
```

## Install

```sh
curl -fSsL https://dl.fullbars.xyz/install.sh | sh
```

### What the installer does on Alpine, and why

| What it does | Why |
|---|---|
| Installs the provider and `urnet-tools` under `/usr/local/lib/urnetwork-provider`, owned by root and readable by everyone. Only the state dir (`/home/urnet/.urnetwork`) lives in the service user's home, because the provider must write it. | The scheduled auto-update runs as root, and `sudo urnet-tools update` is the documented upgrade path. Root-owned files do not defend a path whose ancestor a user can rewrite: a tree under `/home/urnet` could be renamed aside and replaced with the service user's own binary, which root would then execute. Keeping the tree off any user-writable path makes that impossible by construction. |
| Writes the service script `/etc/init.d/urnetwork` and runs `rc-update add urnetwork default`. | The `default` runlevel is what OpenRC starts at boot, so the provider comes back after a reboot with nobody logged in. |
| Runs the provider under `supervise-daemon` as the `urnet` user. | The provider drops root, and `supervise-daemon` restarts it after a crash (see [Restart policy](#restart-policy)). |
| Sends the provider's output to `/var/log/urnetwork.log` (stdout) and `/var/log/urnetwork.err` (stderr), both owned by the service user; `/var/log` remains root-owned. | `supervise-daemon` opens these files after dropping privileges, so the service user must be able to write them. Because the log is a plain file, `urnet-tools logs` can read it even while the service is stopped. |
| Keeps the provider's state, including the login token, in `/home/urnet/.urnetwork`. | The provider reads its credentials from the home directory of the user it runs as. |

The installer does **not** start the service. It finishes by printing the commands for the next two steps.

### Restart policy

`supervise-daemon` restarts the provider when it exits. The delay before a restart starts at 5 seconds and grows by 5 seconds per restart up to a cap of 60 seconds, and the supervisor gives up after 10 restarts within one hour. The limit is deliberate: unlimited respawn would turn a broken binary into a tight crash loop.

## Authenticate

The provider needs an auth code from <https://ur.io>, and the login token it produces has to land in the `urnet` user's home, not root's. A token written to `/root/.urnetwork` is invisible to the service, which would start and then have no credentials.

Run the authentication as the service user:

```sh
su -s /bin/sh urnet -c 'urnetwork auth <code>'
```

The installer prints this exact command when it finishes. When you run it in an interactive terminal it can also offer to do this step for you: answer `y`, enter the code, and the installer runs the authentication as `urnet`. The prompt is read from the terminal device itself, not from standard input, so it works for the one-line `curl ... | sh` form as well. Where there is no terminal at all (a provisioning script, a CI job, a cron entry) the prompt is skipped and the command is printed instead. If the prompt was skipped, or the authentication did not complete, run the command above at any time. Auth codes are single-use, so fetch a new one if a code was already submitted.

## Start and verify

```sh
rc-service urnetwork start
rc-service urnetwork status
```

`status` should report `started`. To see what the provider is doing:

```sh
urnet-tools logs
tail -f /var/log/urnetwork.log
```

`urnet-tools logs` follows the same file. If the service does not stay up, read `/var/log/urnetwork.err` as well as the main log. If you have not authenticated yet, go back to [Authenticate](#authenticate).

A node with no proxy list configured serves traffic directly from the box's own address. The `[profit]` line in the log says so with `mode=direct`; see the [Log Reference](../LOG_REFERENCE.md#-profit-heartbeat-323-fix).

To add proxies, see [Adding Proxies](Adding-Proxies.md).

## Boot persistence

`rc-update add urnetwork default` places a symlink to the service script in `/etc/runlevels/default`, and OpenRC starts everything in that runlevel at boot. Check it with:

```sh
rc-update show default
```

`urnetwork` should be listed. OpenRC has no equivalent of systemd's `enable-linger`, and none is needed: a service in the `default` runlevel starts at boot whether or not anyone logs in.

Stop the provider now (it starts again at the next boot):

```sh
rc-service urnetwork stop
```

Stop it and keep it from starting at boot:

```sh
rc-service urnetwork stop
rc-update del urnetwork default
```

Turn boot start back on later:

```sh
rc-update add urnetwork default
rc-service urnetwork start
```

## Updates and auto-update

Updating is a root action on OpenRC, the same way `systemctl restart` is for a system service under systemd. Restarting an OpenRC service needs root, and so does replacing the root-owned binaries, so run the update from a root shell:

```sh
urnet-tools update
```

To have it run on a schedule, turn on auto-update as root. On OpenRC it is a busybox `crond` entry, not a systemd timer:

```sh
urnet-tools auto-update weekly
```

The interval can be `daily`, `weekly` or `monthly`, and `urnet-tools auto-update off` removes it. The entry is a script at `/etc/periodic/<interval>/urnetwork-update` that runs `urnet-tools update -f`. It only fires while `crond` is running. `urnet-tools` still writes the entry when `crond` is missing, and prints a note; to install and start `crond`, run:

```sh
apk add busybox-openrc
rc-update add crond default
rc-service crond start
```

## HotSwap is not available

There is no zero-downtime HotSwap under OpenRC. HotSwap depends on the old process telling the init system, through `sd_notify`, that the main PID has changed. `supervise-daemon` has no equivalent: it watches the one process it started, so a successor started by the old provider would run outside supervision while `supervise-daemon` launched a second copy of its own.

`urnet-tools update` and `urnet-tools hotswap` therefore decline the handoff and print:

```text
zero-downtime hotswap unavailable: the provider is supervised by OpenRC's supervise-daemon, which has no sd_notify MainPID handoff; this update uses a stop/start service restart
```

`update` then does an ordinary stop, binary swap and start, with a brief gap with no provider process. To restart without updating, use `rc-service urnetwork restart`. Background and measurements for the systemd path are on the [HotSwap](HotSwap.md) page.

## urnet-tools command parity

On a host where OpenRC is the running init system and the installer's `urnetwork` service exists, the lifecycle commands act on that service. The backend is chosen at run time by looking at the running init system, never by operating system. A host where systemd is running keeps the systemd behavior even if OpenRC is installed beside it.

| Command | What it runs on OpenRC |
|---|---|
| `start` | `rc-service urnetwork start` |
| `stop` | `rc-service urnetwork stop` |
| `restart` | `rc-service urnetwork restart`, behind the usual confirm gate (`-f` skips the prompt) |
| `auto-start on` / `off` | `rc-update add urnetwork default` / `rc-update del urnetwork default` |
| `auto-update daily\|weekly\|monthly` / `off` | A busybox `crond` entry in `/etc/periodic/<interval>/urnetwork-update` that runs `urnet-tools update -f`. There is no systemd timer. `off` removes it from every interval. |
| `logs [N]` | Follows the service's log file, `/var/log/urnetwork.log` (the file named by `output_log` in `/etc/init.d/urnetwork`), falling back to the error log. |
| `status` | Prints `rc-service urnetwork status`, then the usual table with the live control-socket view. |
| `uninstall` | When it targets the service's provider: stops the service, runs `rc-update del`, removes `/etc/init.d/urnetwork` and clears the auto-update entry. |

These commands need root, because they change a system service. Run as an ordinary user, a failing command adds a hint to re-run as root.

**Several providers on one box.** `start` is not gated. But a `stop` or `restart` with no selector, on a box where the service runs beside another provider, is refused rather than acted on, because stopping only the service would leave the other provider running while the tool reported success. Name the target with `--user`, `--unit` or `--state-dir` (the selectors in [Targeting & Selectors](urnet-tools-go.md#-targeting--selectors)):

```text
N providers found on this box, specify a target: [alice (pid 4242)]
  urnet-tools stop --user <user>          # a specific provider
  urnet-tools stop --unit urnetwork       # the OpenRC service
  urnet-tools providers             # list what was found
```

`N` is the provider count the tool reports, and the bracketed list names the providers that are not the service's own (here, `alice`'s). `restart` prints the same message with `restart` in the example commands. A selector that matches the service's own provider, such as `--unit urnetwork` or `--user urnet`, goes straight to `rc-service`.

## Watchdog and exit 75

The provider's [swap-thrash watchdog](Configuration.md#swap-thrash-watchdog) restarts it by exiting with status 75 and letting the supervisor start it again. Under OpenRC:

- **There is no sd_notify watchdog.** `supervise-daemon` offers no systemd-style keep-alive, so after 10 minutes without a tick the provider exits with status 75 itself and `supervise-daemon` restarts it. The start cap and the 3 restarts per 24 hours ring are the same as under systemd.
- **OpenRC counts as a supervisor.** The service script exports `URNETWORK_INIT=openrc`, because `supervise-daemon` sets neither `INVOCATION_ID` nor `NOTIFY_SOCKET`. Without that marker the provider could not tell it is supervised and would refuse the watchdog's restart.
- **Declined handoffs are recorded.** When `update` declines HotSwap on a supervised provider, the decline ledger records the reason `openrc`.

## Uninstall

As root:

```sh
curl -fSsL https://dl.fullbars.xyz/uninstall.sh | sh
```

This stops the service, removes `/etc/init.d/urnetwork`, its `default` runlevel entry and any auto-update entry, and deletes the install directory. It also deletes `/home/urnet/.urnetwork`, which holds the login token, so you would need a new auth code to install again. It leaves two things behind: the `urnet` user and the log files. Remove them if you want a clean slate:

```sh
deluser urnet
rm -f /var/log/urnetwork.log /var/log/urnetwork.err
```

## Limitations

- **Updates need root.** Every update restarts the service and replaces root-owned files, so run `urnet-tools update` from a root shell. There is no rootless update path on OpenRC.
- **No zero-downtime HotSwap.** See [HotSwap is not available](#hotswap-is-not-available).
- **Auto-update needs `crond`.** The entry does nothing while `crond` is stopped.
