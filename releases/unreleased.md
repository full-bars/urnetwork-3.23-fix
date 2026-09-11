### **Unreleased (targeting v3.23.0-fix.31.7)**

> [!NOTE]
> The whole v31 surface is still unreleased in the sense that matters.
> v3.23.0-fix.31.0 through v3.23.0-fix.31.6 are tags cut to exercise what
> will eventually ship as v31, not deployments. The fleet runs v30.x, so no
> v31 change has reached production yet. See
> [releases/v3.23.0-fix.31.0.md](v3.23.0-fix.31.0.md) for the full v31
> feature set and `CHANGELOG.md` for the per-tag history.

This file covers what has landed on `main` since the v3.23.0-fix.31.6 tag,
plus one pending pull request. Everything here is additional to the v31
feature set in the notes linked above, not a replacement for it.

#### Added
- **Version stamp for `-trimpath` builds**: every release binary is built
  with `-trimpath`, which strips `-ldflags` from Go buildinfo, so
  `main.Version` cannot be read back out of a stopped release binary.
  `provider/main.go` now also embeds the version as a plain program-data
  string literal (`VersionStamp`, set via
  `-ldflags "-X main.VersionStamp=URNET_VERSION_STAMP=$VERSION"`).
  `providerVersionFromStamp` in `internal/urnettools/provider.go` scans the
  binary's raw bytes for the `URNET_VERSION_STAMP=` marker and reads the
  version back without executing the file, so it works on a stopped
  provider, a cross-architecture binary, and any `-trimpath` release build.
- **Control-socket version command**: a running provider now answers a
  `version` control-socket request with its own build version
  (`provider/control_socket.go`), reporting `dev` if built without
  `-ldflags`. `internal/urnettools/discover_unix.go` asks a running
  provider's socket first when resolving its version, since that is the
  only source answering "what is this process" rather than inferring it
  from the filesystem; it falls back to `/proc/<pid>/exe` and then to
  on-disk buildinfo for providers too old to know the command or whose
  socket never bound.

- **Auto-update timer fix**: the weekly `urnetwork-update.timer` has never
  completed an update. Its `ExecStart` is a bare `urnet-tools update` with
  no `-y`, systemd hands the oneshot unit `/dev/null` on stdin, and the
  version-choice confirmation was gated only on `!force`, so every run
  exited 1 on the non-interactive stdin read. Not a v31 regression — it
  predates v3.23.0-fix.30.9, and `scripts/Provider_Install_Linux.sh` still
  writes that same `ExecStart`. The fix introduces `unattendedUpdate()`,
  a single decision shared by both confirmation gates and the target
  pickers. Gate semantics:

  | Condition | Behavior |
  |-----------|----------|
  | `-f`/`--force` | Skip prompts |
  | `INVOCATION_ID` set (systemd) | Skip prompts |
  | stdin is `/dev/null` (cron) | Skip prompts |
  | Non-interactive but not `/dev/null` | Refuse (SSH w/o pty, pipes) |
  | Terminal, no `-f` | Prompt as before |

  Piped answers (`echo y | urnet-tools update`) are **not** read —
  `confirmStdinRead` is shared with other destructive commands and keeps
  refusing non-terminal input. The install script should pass `-y` for
  explicitness. `cmdSelfUpdate` had the identical bug and is fixed too.
  An update lock (flock on `*.update.lock`) now serializes concurrent
  updates of the same binary.

> [!IMPORTANT]
> **Nodes on v3.23.0-fix.30.9 or v3.23.0-fix.31.6 will report a false
> "failed to update" even when the upgrade succeeds.** On those builds,
> resolving a running provider's version under systemd falls through to
> reading `-ldflags` out of buildinfo, which is stripped by the `-trimpath`
> release build, so the read comes back empty. `urnet-tools update`'s
> post-update verification cannot then confirm the new binary took over,
> and reports the update as failed. Measured on a test node: the restart
> had taken effect, the process id moved and the unit was active, while the
> run printed "1 of 1 provider(s) failed to update". The same blind spot
> also defeats the "already on `<tag>`" skip check, so a node can reinstall
> the same release repeatedly. This is fixed by the version-stamp and
> control-socket changes above, but those builds predate the fix. Until a
> node is updated past this point, verify the outcome with
> `urnet-tools version` rather than trusting the update command's exit
> status.
