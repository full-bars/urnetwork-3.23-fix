# Installer Matrix CI

`installer-matrix.yml` runs **the shipped Linux installer**
(`scripts/Provider_Install_Linux.sh`) on real distro userlands. It exists
because nothing else does: the other workflows run pre-built binaries or a
single Ubuntu runner, so a distro-specific install break reached users first.

## When it runs

| Trigger | Scope |
| --- | --- |
| Push of a release tag | all four cells |
| Pull request touching the installer | all four cells |
| `workflow_dispatch` | all four cells |

The path filter is deliberately narrow: the installer scripts, the harness, and
the workflow itself. A build-only change does not need this tier.

## The cells

| Image | Why this one |
| --- | --- |
| `debian:12-slim` | the older apt/glibc generation, wide VPS base |
| `debian:13-slim` | current apt/glibc, cgroup v2 |
| `almalinux:9` | RHEL family: `dnf`, SELinux enforcing, systemd 252 |
| `alpine:latest` | musl, busybox, `apk`, OpenRC rather than systemd |

Each cell installs `curl`, `unzip`, `tar`, `gzip`, `jq` and `gawk`, fetches the
run's payload, then runs `.github/scripts/installer-lifecycle.sh`.

## What it asserts

The harness serves a local fake release (`scripts/ci/fake-release-server`, which
advertises real sha256 digests) and runs the real installer against it:

1. install exits 0 and creates the install tree
2. `.version` records the tag
3. the installed binary reports that tag itself
4. a second install is idempotent (same version, no binary pile-up)
5. a tag switch installs the second build and the recorded version advances
6. uninstall removes the tree, the unit/init script and the shell rc block

The order of the two middle passes is seeded. `SHUFFLE_SEED` is printed and
recorded, so a failing order is reproducible:

```sh
SHUFFLE_SEED=<seed> sh .github/scripts/installer-lifecycle.sh
```

`URNET_NO_AUTOINSTALL=1` flips the harness to the refusal path instead: the
installer must exit non-zero, name the missing dependency, and leave no partial
install tree.

## Running a cell by hand

```sh
# from a checkout, with the payload built into ./out
mkdir -p /tmp/p && cp out/provider out/provider2 out/urnet-tools out/urnet-tools2 \
  out/release-server out/installer.sh .github/scripts/installer-lifecycle.sh /tmp/p
chmod +x /tmp/p/*

docker run --rm --init -v /tmp/p:/payload:ro \
  -e BIN_DIR=/payload -e INSTALLER=/payload/installer.sh \
  -e TAG=v0.0.0-local-1 -e TAG2=v0.0.0-local-2 \
  debian:12-slim sh /payload/installer-lifecycle.sh
```

## Release gating

`release.yml`'s `create-release` job carries a step named **Gate on the
installer matrix**. It finds the `Installer matrix` run for the same commit,
waits for it (bounded at 60 minutes), and fails the release when any job whose
name starts with `installer` concluded `failure` or `cancelled`.

The gate reads **installer job conclusions**, not this workflow's overall
conclusion, so nothing outside the installer tier can block a release.

`RELEASE_GATE_MODE` in that step is the switch:

- `report` (current) — the matrix is advisory; the step exits 0.
- `block` — the gate is enforced.

Flip it to `block` only after two clean **tag** runs. A green
`workflow_dispatch` does not prove the tag path.
