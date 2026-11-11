# OpenBSD

This page covers the initial OpenBSD build support for the URnetwork provider and `urnet-tools`. For FreeBSD, see the [FreeBSD guide](/full-bars/urnetwork-3.23-fix/wiki/FreeBSD). For other platforms, see the [Installation Guide](/full-bars/urnetwork-3.23-fix/wiki/Installation).

> [!IMPORTANT]
> OpenBSD support is build-only. The binaries and service setup have not been verified on an OpenBSD host. There is no OpenBSD installer or OpenBSD release asset in this tree. The existing BSD installer is specific to FreeBSD; do not run it on OpenBSD.

## Before you start

Use a non-root account for the provider. Install the Go version required by `go.mod` and Git with OpenBSD's package manager, if they are not already installed:

```sh
doas pkg_add go git
```

Check `go version` against `go.mod`. The package version must meet the module's Go requirement. Configure `doas` separately if your account cannot use it.

## Install

There is no automated install yet. Build from a checkout of this branch on an OpenBSD host:

```sh
go build -o provider-openbsd ./provider
go build -o urnet-tools-openbsd ./cmd/urnet-tools
```

These commands leave the binaries in the checkout. Move them to a location your service account can execute, using root-owned directories that the service account cannot modify. This build procedure has not been tested on OpenBSD.

## Authenticate

Run authentication as the same non-root account that will run the provider. Get a single-use auth code from <https://ur.io>:

```sh
./provider-openbsd auth <code>
```

The provider stores its state in that account's home directory under `~/.urnetwork`. Keep this directory private. Replace the example path with the installed binary path if you moved the binary.

## Service commands

For a foreground test, run as the service account:

```sh
./provider-openbsd provide
```

OpenBSD uses `rcctl` and its own `rc.d` service scripts. No OpenBSD service script is provided yet. If you create and test one, use `doas rcctl enable <service>` to enable it and `doas rcctl start <service>` to start it. Use the actual service name in place of `<service>`. Do not use the FreeBSD service script or `sysrc` instructions.

`urnet-tools auto-start` is not supported on OpenBSD. It does not create or configure a service.

## Updates and auto-update

Rebuild and replace the binaries manually. There is no OpenBSD installer or tested update path. `urnet-tools auto-update` scheduling is not supported; it reports an error. A manual cron schedule would need an independently tested update procedure.

## Sysctl tuning

There are no tested OpenBSD tuning targets in this repository. The FreeBSD sysctl names must not be assumed to work on OpenBSD. Inspect the host's limits with `sysctl` and consult the OpenBSD manual before changing them. Do not run `urnet-tools optimize` on OpenBSD: it falls through to the Linux implementation, not an OpenBSD tuner.

## Platform notes

- **Process discovery:** `urnet-tools` uses `ps` to find running providers by process name. It does not discover stopped services or infer a binary path from process arguments. Commands that need that path can be limited.
- **Memory:** Total physical memory comes from `hw.physmem` when available. Available host memory is not reported. Host memory-pressure and swap checks use the unsupported fallback.
- **Control socket:** Peer credential verification is a no-op on OpenBSD. The control socket relies on its file permissions instead. Keep the service account and its state directory private.
- **Disk diagnostics:** Free space uses the OpenBSD `statfs` interface.
- **UDP:** OpenBSD uses the portable socket reader, not the platform-specific poller.

## Release assets

The build support does not add OpenBSD binaries to the release pipeline. Build from source until OpenBSD assets are published.

## Uninstall

There is no OpenBSD uninstaller. Stop any service you created, disable it with `rcctl` and remove its script and manually installed binaries. The service account's `~/.urnetwork` contains login and proxy state. Remove it only if you no longer need that data.
