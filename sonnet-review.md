# Code Review: fix/ci-smoke-lifecycle

Reviewed: `git diff origin/main...HEAD` (single commit `027cae6e` on top of `f4b40eb7`).
Verified with `go build`/`go vet`/`go test ./internal/urnettools/...` (linux, plus cross-compile check for `GOOS=windows` and `GOOS=darwin`), `git show`/`git log -S` for attribution, and manual trace of the docopt usage strings in `provider/main.go`.

## Process note (not a defect in the reviewed commit)

The working tree has an **uncommitted** local modification to `internal/urnettools/discover_unix.go` that is *not* part of `HEAD` and therefore not part of this diff:

```
git diff HEAD -- internal/urnettools/discover_unix.go
```

shows it adds an `os/user.Current()`-based `currentUserName()` and a fallback-to-local-manager path when `systemctl --user -M <user>@ ...` fails for a *non-current* user. This is directly relevant to Finding 1 below — it looks like whoever was iterating on this branch already spotted the same gap I found and started fixing it, but never committed it. Worth checking with the author before merge: is this WIP meant to land in this PR, a separate PR, or was it abandoned? I did not modify it and all findings below are against the committed `HEAD` content only (confirmed via `git show HEAD:...`).

---

## Findings

### MEDIUM — `-M <user>@` CI fragility fixed for discovery but not for the lifecycle mutation paths
**Files:** `internal/urnettools/lifecycle_unix.go:24-27` (`setAutoStart`), `:40-49` (`setAutoUpdateSchedule` off-branch), `:137-144` (`enableTimer`)

The PR's stated rationale for `discoverUserUnits`'s new current-user branch (`discover_unix.go:199-201`) is: *"the -M form goes through machined/loginctl and can fail on CI runners (no cross-user session bus) even when the local user manager is fully functional."* That's a real, verified problem (see the `unix-lifecycle.yml` bootstrap step that has to `loginctl enable-linger` + `daemon-reexec` just to get a session bus at all).

But `setAutoStart`, the `off` branch of `setAutoUpdateSchedule`, and `enableTimer` **still unconditionally use `systemctl --user -M <p.User>@ ...`** for a user unit, with no equivalent "is this the current user, use the local session bus instead" branch. On the exact class of runner this PR fixes discovery for, `urnet-tools auto-start on/off` and `urnet-tools auto-update weekly/monthly/off` (the `enableTimer`/disable calls, not the file write itself) will likely still fail via `-M`.

This isn't a regression — this code was already `-M`-only before this commit — but the PR *demonstrates* the exact failure mode is real and CI-observable, and only patches it in the read path (discovery), not the write path (lifecycle mutation). The `unix-lifecycle.yml` workflow currently masks this because:
- `auto-update weekly` tolerates failure (`|| echo "NOTE: ... treat as informational"`)
- `auto-update off` tolerates failure (`|| true`)
- both "verify" steps check only **file existence**, which `writeTimerCalendar`/`removeTimerFile` guarantee regardless of whether `enableTimer`/`disable` succeeded

So CI stays green, but real end-user invocations of `auto-start`/`auto-update` on a similarly-configured box (e.g., a fresh droplet without an active login session) may still silently or loudly fail via `-M`, which is presumably the whole reason this class of bug matters to a fleet tool. Suggest applying the same current-user branch used in `discoverUserUnits` to `setAutoStart`/`enableTimer`/the `off` branch of `setAutoUpdateSchedule` (a shared helper like `systemctlUserArgs(user string) []string` would avoid the duplication risk that let this drift in the first place).

### LOW — `currentUserName()` env-var-only detection can silently degrade to the old (broken) `-M` path
**File:** `internal/urnettools/discover_unix.go:236-241`

```go
func currentUserName() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return os.Getenv("LOGNAME")
}
```

If neither `USER` nor `LOGNAME` is set (plausible in a stripped/non-interactive shell, cron, or a minimal container `ENTRYPOINT`), `currentUserName()` returns `""`. Then `user == current` is false for every real user in `discoverUserUnits`, and the loop falls back to the exact `-M <user>@` path this PR was written to avoid — silently, with no diagnostic. GitHub Actions' `ubuntu-latest` runners do set `USER`, so `unix-lifecycle.yml` won't catch this, but it's a latent gap. The already-mentioned uncommitted local diff addresses this by trying `os/user.Current()` first, which is more robust (works independent of env-var hygiene) — consider pulling that in before merge rather than shipping the weaker env-var-only version.

### LOW — off-path in `setAutoUpdateSchedule` returns the `disable` command's error even when the (stated-important) file removal succeeded
**File:** `internal/urnettools/lifecycle_unix.go:39-53`

```go
case "off":
    if isUserUnit(timer) && p.User != "" {
        if err := exec.Command("systemctl", "--user", "-M", p.User+"@", "disable", "--now", timer).Run(); err != nil {
            // ... "the FILE removal below is the deterministic part" ...
            _ = removeTimerFile(p, timer)
            return err
        }
    } else if err := exec.Command("systemctl", "disable", "--now", timer).Run(); err != nil {
        _ = removeTimerFile(p, timer)
        return err
    }
    return removeTimerFile(p, timer)
```

The comment explains the *intent* correctly (file removal is what actually matters — a disabled-but-present file would keep firing, so remove it regardless), but the code then contradicts that intent by returning the `disable` command's error anyway. A caller/script checking `urnet-tools auto-update off`'s exit code sees failure even though the operation that matters (timer file gone, won't fire again) succeeded. Combined with Finding 1 (this `disable` call is CI-flaky via `-M`), this makes `auto-update off` look broken on exactly the runners this PR is trying to make more reliable. The `unix-lifecycle.yml` workflow only survives this because its `auto-update off` step is `|| true`. Suggest: log the disable failure (`fmt.Fprintf(os.Stderr, ...)`) but don't propagate it as the function's error — return `removeTimerFile(p, timer)`'s result unconditionally, since that's the operation whose success the caller actually cares about.

### LOW — `tool-functional-smoke.yml` teardown wipes a `ur_config` docker volume that this job never creates
**File:** `.github/workflows/tool-functional-smoke.yml:238-243`

```yaml
docker stop urnet-test 2>/dev/null || true
docker rm -f urnet-test 2>/dev/null || true
# The named config volume (ur_config) would otherwise keep the
# provider's JWT/state across runs.
docker volume rm ur_config 2>/dev/null || true
```

The `docker run` that starts `urnet-test` in this same file (line 142-150) has no `-v ur_config:/root/.urnetwork` (or any named-volume mount at all — only a bind-mount of `/tmp/docker-test/proxy.txt`). So `/root/.urnetwork` lives in the container's own writable layer, not in a volume named `ur_config`. `docker volume rm ur_config` here is a no-op (silently swallowed by `2>/dev/null || true`) — it doesn't correspond to anything this job created. The actual state-wipe protection in this job comes entirely from `docker rm -f urnet-test`, which is already sufficient. The line and its comment appear to be copy-pasted from `docker-multi-container.yml`/`docker-functional-soak.yml` (where `ur_config` *is* a real named volume declared under `docker compose`'s `volumes:`), and are misleading here — not a functional bug (nothing leaks), but dead code that will confuse the next person reading the teardown. Either remove it, or if a shared config volume is actually desired for this job too, wire it into the `docker run` mount.

### LOW — `consumeDockerBareTarget` ambiguity between a bare target and a same-named positional argument
**File:** `internal/urnettools/cli_docker.go:294-319`, `:409-416`

`consumeDockerBareTarget` greedily promotes the *first* non-flag positional to the target if it matches any discovered container's unit name — before subcommand-specific parsing gets a chance to interpret it (e.g., as a proxy file path for `add`). If a user runs `urnet-docker proxy add urnet-test` intending `urnet-test` as a *local file* whose name happens to collide with a currently-discovered container's name, the positional is silently consumed as the target instead, leaving `rest2` empty, and the `add` case's `len(rest2) != 1` check then fails with `"proxy add requires exactly one proxy file"` — a correct-but-confusing error that doesn't explain *why* (the user has no obvious reason to suspect their filename collided with a container name). Low likelihood in practice (container/unit names and proxy-list filenames rarely collide), and it fails closed rather than corrupting data, so this is a UX nit rather than a correctness bug. Worth a one-line mention in `usageDocker()` or the error message ("if this looks like a container name, use --unit explicitly") if it's ever hit in the wild.

### NIT — `cmdDockerProxy` re-implements the bare-target consumption inline instead of delegating to `dockerTargetFromArgs`
**File:** `internal/urnettools/cli_docker.go:387-401` vs `:285-292`

`dockerTargetFromArgs` already wraps `parseTargetFlags` + `consumeDockerBareTarget`. `cmdDockerProxy` needs the *lenient* variant (`parseTargetFlagsLenient`) so unknown flags like `--force` pass through to the in-container `urnet-tools`, so it can't reuse `dockerTargetFromArgs` as-is — but the manual `t, rest2, err := parseTargetFlagsLenient(rest); ...; t, rest2 = consumeDockerBareTarget(providers, t, rest2)` sequence duplicates the two-line combination `dockerTargetFromArgs` exists to encapsulate. Not a bug (verified correct via `TestConsumeDockerBareTarget`'s "flag skipped then name consumed" case and by tracing the `refresh --force`/`clear --force` workflow calls), just a place where a `dockerTargetFromArgsLenient(args, providers)` helper would prevent the two call sites from drifting apart the way `providerCandidateHomes`/`Discover()` clearly did in an earlier commit (see below).

---

## Verified correct (no issues found)

- **`attachUnits` before `discoverSystemdUnits` ordering** (`discover.go:450-456`, `discover_unix.go:483-492`): confirmed `attachUnits(procs []Provider)` mutates elements in place via `procs[i]` (not append/copy), so calling it on `all` before `append(all, discoverStopped(all)...)` correctly populates `Unit` fields on the *same* backing array that `unitIn()` later dedups against. This is the right fix for the described F2 regression.
- **F2 regression attribution**: `git show 696d6fa6:internal/urnettools/discover.go` confirms the Windows-lifecycle refactor commit reduced `Discover()` to just `discoverProcesses()`, dropping the `attachUnits`/`discoverSystemdUnits` calls present in `e1acaa26`. This commit's `discoverStopped()` platform-hook restores exactly that, cleanly, via per-platform no-ops for darwin/windows.
- **`--plain` fix**: confirmed necessary — `discoverSystemUnits` and `discoverUserUnits` (both branches) now consistently pass `--plain`, avoiding the bullet-column (`●`) `fields[0]` misparse for failed/loaded-failed units.
- **`--proxy_file=` requirement for `proxy add`**: confirmed against `provider/main.go:779` (`provider proxy add [<key_address>...] [--proxy_file=<proxy_file>] [-f]`) and `proxyAdd()` (`main.go:3713-3731`) — a bare positional would indeed be parsed as `<key_address>` (a raw proxy address), not a file path, so `--proxy_file=` is required, not cosmetic. The in-container shell wrapper (`docker/scripts/urnet-tools.sh:326-329`) forwards `"$@"` verbatim to `provider proxy add`, so the flag passes through correctly.
- **`writeTimerUnitAtomic`**: temp-file + rename is correctly atomic for same-filesystem renames (both `path+".tmp"` and `path` are in the same directory). `TestWriteTimerUnitAtomicNoPartialOnCrash` and `TestWriteTimerCalendarCreatesMissingFile` both pass and exercise the real behavior (no test doubles).
- **`writeTimerCalendar` create path**: template matches the shape described (`[Unit]`/`[Timer]`/`[Install]`) and correctly branches on `os.IsNotExist(err)` vs. other read errors before falling into the "rewrite existing" path.
- **`TestCmdReportWritesOverrideFile` skip guard placement** (`review_fix_test.go:356-363`): correctly placed immediately before the assertion it protects (the no-provider error path), after the unrelated file-content/mode assertions that don't depend on a clean box. No issue.
- **Docker CLI target-flag discovery ordering** (`cli_docker.go:220-236`, `327-352`, `354-374`): `providers := DiscoverDocker()` was correctly hoisted earlier in `cmdDockerStatus`/`cmdDockerRestart`/`cmdDockerLogs` to feed `dockerTargetFromArgs`, with no duplicate `DiscoverDocker()` calls introduced.
- **`upstream_monitor.yml` jq null guard**: the `jq -e '.files'` pre-check before consuming `.total_commits`/`.files[].filename` correctly short-circuits to an empty `$COMPARE` and downstream `2>/dev/null || ...` guards prevent the "Cannot iterate over null" crash.
- **Workflow teardown `if: always()` + scoping**: all five teardown edits (`docker-functional-soak.yml`, `docker-multi-container.yml`, `tool-functional-smoke.yml` ×2, `tool-functional-soak.yml`) are scoped to runner-created state (`$HOME/.urnetwork`, `/tmp/mint`, named containers/volumes the same job created) — no broad `rm -rf` of unrelated paths. SIGTERM-before-SIGKILL (`stop`/`kill -TERM` before `rm -f`/hard cleanup) is consistently applied so the in-container/process provider gets a chance to deregister before state is wiped.
- **Build/vet/test**: `go build`, `go vet`, and `go test ./internal/urnettools/...` all pass on the committed `HEAD` for `linux`, `GOOS=windows`, and `GOOS=darwin` (cross-compile checked for windows/darwin; full test run on linux only, as expected for `//go:build linux`-gated files).

---

## VERDICT: APPROVE

No correctness regressions or CI-breaking issues found in the committed diff; build/vet/test are clean on all three platforms, and the core discovery/atomicity/docopt claims all check out against the actual code they reference. The MEDIUM/LOW findings above (the `-M` fix not extending to lifecycle mutation paths, the env-var-only `currentUserName`, the off-path error semantics, and the dead `ur_config` teardown line) are all pre-existing or low-blast-radius issues that don't regress anything this PR touches and are already tolerated by the CI workflows' own `|| true`/informational patterns — worth follow-up but not blockers. Recommend resolving the uncommitted `discover_unix.go` working-tree diff (fold it in or discard it) before merging, since it appears to be unfinished work on the same problem this PR partially addresses.
