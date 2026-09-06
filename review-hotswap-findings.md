# HotSwap Engine — High-Severity Review

Branch `feat/hotswap` @ `bf733e93` (worktree `/home/klets/ur/.worktmp/wt-hotswap`).
All line refs verified against the current working tree. Read-only review; no files
other than this one were modified.

Verification performed:
- `go build ./...` — clean (linux + `GOOS=windows`).
- `go vet ./provider/... ./internal/urnettools/...` — clean on linux; **fails on Windows** (F-13).
- `go test ./provider/ -run 'HotSwap|Hotswap|YieldCoordinator|NotifySystemd' -race -count=1` — 9/9 PASS.
- `git log --stat` on both hotswap commits: **neither touches `docker/scripts/` nor
  `scripts/Provider_Install_Linux.sh`**, which is the root of F-1 and F-2.

---

## Summary of verdict

The IPC protocol, framing, preflight and canary handshake are sound and well tested in
isolation. The problem is not the protocol — it is that **neither of the two shipped
deployment topologies can survive the handoff**, and the PID-1 branch that was written to
solve exactly that never executes in the shipped image. Two CRITICALs (F-1, F-2) mean that
running `urnet-tools hotswap` or letting `urnet-tools update` fire SIGUSR2 on a production
node takes the provider down ~30 seconds later, in both Docker and systemd. A third
CRITICAL (F-3) re-runs `auth` in the candidate for the `auth-provide` entrypoint, which is
a direct violation of the stated identity invariant.

I would not ship this without F-1, F-2, F-3, F-4 and F-5 resolved.

---

## CRITICAL

### F-1 — Standard (non-PID-1) handoff kills the Docker container 30s later

`provider/hotswap.go:416-424`, `docker/scripts/start_stable.sh:213-229`,
`docker/scripts/entrypoint.sh:61`, `Dockerfile:89`

The PID-1 execve branch (`hotswap.go:331`) is gated on `getpidFunc() == 1`. In the shipped
image PID 1 is **not** the provider:

```
Dockerfile:89        ENTRYPOINT ["/app/entrypoint.sh"]        # PID 1 = shell
entrypoint.sh:61     exec /app/start_stable.sh                # still PID 1 = shell
start_stable.sh:219  if "$PROVIDER_BIN" provide; then ...     # provider is a CHILD
```

So `getpid() != 1` and the standard baton branch runs. After the drain the parent calls
`exitFunc(0)` (`hotswap.go:423`). The supervising shell then hits:

```sh
start_stable.sh:220  if [ "$code" -eq 0 ]; then
start_stable.sh:221      if [ -f "$HOME/.urnetwork/update-pending" ]; then ... continue; fi
start_stable.sh:227      log " [INFO] UrNetwork exited cleanly."
start_stable.sh:228      break            # <-- loop exits
```

The `update-pending` marker is written only by the container's own in-container updater
(`scripts/test_docker_update_tarball.sh:699` documents its lifecycle); HotSwap never writes
it. So the loop breaks, `func_start_provider` returns, the entrypoint script exits, PID 1
dies, and Docker tears down the namespace — **killing the freshly-promoted candidate with
it**. Net effect of a "zero-downtime" upgrade: hard container death after ~30s, plus a
restart-policy-dependent cold start.

**Failure scenario:** `docker exec <c> urnet-tools hotswap` on a `BUILD=stable` container.
t=0 candidate spawns, t≈2s READY, TAKEOVER, candidate goes live; t=30s parent `exit(0)`;
t=30s shell breaks out of the supervision loop; t≈30s container exits.

**Fix (pick one):**
- Preferred: make the provider PID 1 (`exec "$PROVIDER_BIN" provide` in the start scripts),
  which is also what makes the F-2 systemd story and the PID-1 execve branch real. Requires
  moving the crash/reauth ladder in `start_stable.sh:230-259` into the provider or an
  init-shim.
- Or: have the retiring parent write `$HOME/.urnetwork/update-pending` (or a dedicated
  `hotswap-in-progress` marker) *before* `exitFunc(0)`, and teach every start script to
  `continue` without re-launching while a candidate is live. This is fragile — the shell
  would need to adopt the orphan rather than spawn a second provider.
- Or: exit with a distinct non-zero code that the scripts treat as "handoff complete, do
  not relaunch, do not exit".

Whichever is chosen, `docker/scripts/*.sh` must be updated in the same PR. Add
`needs-fleet-deploy` to the PR.

---

### F-2 — systemd kills the promoted candidate; MAINPID notify is a guaranteed no-op

`provider/hotswap.go:406-410`, `provider/hotswap_unix.go:136-140`,
`scripts/Provider_Install_Linux.sh:661-672`

The shipped unit is:

```ini
[Service]
Environment="HOST_HOSTNAME=..."
ExecStart=$install_path/bin/urnetwork provide
Restart=no
```

No `Type=notify`, no `NotifyAccess=`. Consequences, all of them fatal:

1. `NOTIFY_SOCKET` is never set, so `notifySystemdMainPID` returns `nil` at
   `hotswap_unix.go:139` without doing anything. The log line at `hotswap.go:409`
   ("systemd updated: MAINPID=%d") **prints on the no-op path** and actively misleads the
   operator into believing the handoff was registered.
2. Even with `NOTIFY_SOCKET` present, `Type=simple` ignores `MAINPID=` entirely — only
   `Type=notify`/`forking` honour it — and `NotifyAccess` defaults to `main`, so a child
   could not notify regardless.
3. When the parent (the unit's MainPID) exits at t=30s, systemd considers the service
   stopped and, under the default `KillMode=control-group`, SIGTERMs then SIGKILLs every
   remaining process in the cgroup — including the candidate. With `Restart=no` nothing
   comes back.

**Failure scenario:** `urnet-tools hotswap` on any node installed by
`Provider_Install_Linux.sh`. Candidate goes live, parent exits at t=30s, systemd kills the
candidate at t≈30s, provider stays down until an operator runs `systemctl start`.

**Fix:** the unit must become `Type=notify` + `NotifyAccess=all` + `Restart=on-failure`,
*and* `hotswap.go:406` must treat "NOTIFY_SOCKET unset" as a hard precondition failure that
aborts the handoff rather than a silent success. Concretely, change
`notifySystemdMainPID` to return a distinguishable `errNoNotifySocket`, and in
`runHotSwapParentHandoff` refuse to enter the drain when the socket is absent — fall back
to telling the caller to use a normal restart. A parent that cannot hand the baton to the
service manager must not exit.

Note this is not fixable by the Go code alone; `Provider_Install_Linux.sh` must ship the
new unit and `needs-fleet-deploy` applies.

---

### F-3 — Candidate re-runs `auth()` and overwrites `~/.urnetwork/jwt` under `auth-provide`

`provider/hotswap_unix.go:66` (`exec.Command(exe, args...)` with `os.Args[1:]`),
`provider/main.go:1064-1066`, `provider/main.go:1217`,
`docker/scripts/pelican_panel.sh:159`

The candidate is spawned with the parent's full argv. The hotswap candidate check lives
inside `provide()` at `main.go:2680`. But the dispatcher at `main.go:1064` is:

```go
} else if authProvide, _ := opts.Bool("auth-provide"); authProvide {
    auth(opts)      // <-- runs FIRST, in the candidate, before provide()
    provide(opts)
}
```

`auth()` performs a live login and writes the account JWT at `main.go:1217`
(`atomicWriteFile(jwtPath, []byte(byJwt), 0700)`). `auth()` does not consult
`EnvHotSwap`. So for the `BUILD=jwt` / Pelican entrypoint —

```sh
pelican_panel.sh:159   if "$PROVIDER_BIN" auth-provide "$AUTHCODE" -f; then
```

— the candidate re-authenticates with a (typically single-use) auth code and clobbers the
live parent's account JWT before it ever reaches the handshake. This is the exact invariant
the design claims to hold ("runHotSwapChildPreflight only READS the JWT"): true, but
irrelevant, because `auth()` runs before preflight.

Two branches, both bad:
- With `-f` (the shipped form): re-auth proceeds unattended. Auth-code reuse fails →
  `shmLogFatal`/`os.Exit` → candidate dies before READY → parent sees EOF at
  `hotswap.go:310`, aborts, and the operator gets "Candidate failed pre-flight or
  disconnected" with no hint that the *account JWT may already have been overwritten*.
- Without `-f`: `spawnHotSwapCandidate` never sets `cmd.Stdin` (`hotswap_unix.go:66-71`),
  so stdin is `/dev/null`, the `[yN]` prompt at `main.go:1089-1095` reads EOF, and auth is
  silently skipped. Benign only by accident.

**Fix:** two independent guards.
1. In the dispatcher, skip `auth()` when `os.Getenv(EnvHotSwap) == "1"`:
   ```go
   } else if authProvide, _ := opts.Bool("auth-provide"); authProvide {
       if os.Getenv(EnvHotSwap) != "1" {
           auth(opts)
       }
       provide(opts)
   }
   ```
   Same treatment for any other argv form that mutates identity state.
2. In `spawnHotSwapCandidate`, do not blindly forward argv. Rewrite the candidate's
   command to the pure `provide` form (drop `auth-provide`, drop `<auth_code>`,
   `--user_auth`, `--password`) so no future dispatcher change can reintroduce this.
   Belt and braces: set `cmd.Stdin = nil` explicitly and document why.

Add a test that spawns a candidate with `auth-provide` argv and asserts the JWT file mtime
is unchanged.

---

### F-4 — Update rollback truncates the live binary in place; can race `execInPlace`

`internal/urnettools/update.go:679`, `internal/urnettools/update.go:947-980`,
`provider/hotswap.go:365`

Rollback is `copyFile(backup, p.Binary)`, and `copyFile` opens the destination with
`os.O_CREATE|os.O_WRONLY|os.O_TRUNC` (`update.go:957`) — an **in-place truncate-and-rewrite
of the canonical binary path**, unlike `installBinary` which correctly does temp+fsync+
rename (`update.go:859-908`).

The verification loop is bounded at ~30s (`update.go:645`), but the worst-case handoff is
longer: `HotSwapPreflightTimeout` alone is 20s (`hotswap.go:57`), plus candidate process
start, plus (standard branch) `HotSwapAckTimeout` 15s = 35s, plus the candidate's own auth
backoff which can be minutes. So rollback routinely fires *while the handoff is still in
flight*.

**Failure scenario (PID 1 path):** t=30s the updater begins truncating `p.Binary`;
t=30.1s the parent calls `execInPlaceFunc(exe, ...)` (`hotswap.go:365`) against that same
path, mid-write. `execve` on a partially-written image returns `ENOEXEC`/`EIO` (or, worse,
succeeds on a torn but structurally valid file). The parent has *already* yielded its
coordinator session (`hotswap.go:344`) and terminally shut down its retention writer
(`hotswap.go:347`, see F-6). PID 1 survives as a process but is a dead provider with no
transport, no telemetry, and no recovery path — the container looks healthy to Docker and
serves nothing.

Secondary: `O_TRUNC` on a path that is the running image of the candidate returns `ETXTBSY`,
so rollback fails and the code prints `warning: rollback copy failed` while still returning
the "binary rolled back; live provider PID %d was never killed and remains active" error at
`update.go:685`. That message is false in at least three of these paths.

**Fix:**
1. Route rollback through `installBinary(backup, p.Binary, p.User)` so it is atomic
   temp+rename, never an in-place truncate. This alone removes the torn-image class.
2. Do not roll back on a timeout that is shorter than the handoff's own worst case. Either
   raise the verification budget above `HotSwapPreflightTimeout + HotSwapAckTimeout +
   margin`, or better: have the provider write a `~/.urnetwork/hotswap-state` file
   (`spawning`/`ready`/`live`/`failed`) that the updater polls, so rollback only fires on
   an observed terminal failure rather than a wall-clock guess.
3. Correct the rollback message to reflect what actually happened (rolled back vs. failed
   to roll back vs. handoff still in flight).

---

## HIGH

### F-5 — The ACK/listener block runs once per proxy goroutine: data race, duplicate ACKs, N signal listeners, unbounded closer leak

`provider/main.go:3250-3261`, called from `provideWithProxy` (`main.go:2812`), which is
launched once per proxy at `main.go:3476` and `main.go:3552`, and **again on every reload**
via `ProxyReloader.spawnProxy` (`proxy_reload.go:218`, wired at `main.go:3566`, invoked at
`proxy_reload.go:430` and `proxy_reload.go:671`).

```go
main.go:3251  RegisterCoordinatorCloser(func() { platformTransport.Close() })
main.go:3256  if isHotSwapCandidate && hotSwapIPC != nil {
main.go:3257      _ = runHotSwapChildAck(hotSwapIPC)
main.go:3258      hotSwapIPC = nil
main.go:3260      startHotSwapSignalListener(ctx, cancel, opts)
main.go:3261  }
```

`hotSwapIPC` is a `provide()` local captured by N concurrent goroutines with **no
synchronisation**. Four distinct defects:

1. **Data race.** Concurrent read/write of `hotSwapIPC` across goroutines. `go test -race`
   does not catch it today only because no test exercises `provideWithProxy`.
2. **Duplicate ACKs.** Two proxies can both observe non-nil and both write an ACK frame to
   fd 3. The parent reads one; the rest sit unread in the socket buffer.
3. **N signal listeners.** Each call to `startHotSwapSignalListener` registers a fresh
   channel via `signal.Notify` (`hotswap_unix.go:118-119`). Go delivers SIGUSR2 to *every*
   registered channel, so one `urnet-tools hotswap` fans out into N concurrent
   `runHotSwapParentHandoff` calls. `hotSwapLock.TryLock()` (`hotswap.go:269`) keeps them
   from interleaving, but you get N-1 spurious "already in progress" log lines per trigger
   and N leaked goroutines.
4. **Unbounded coordinator-closer leak (the worst of the four).** `coordinatorClosers` is
   append-only (`hotswap.go:94`); the only removal is `ClearCoordinatorClosers`, which is
   test-only. Every proxy relaunch — URL refresh, degraded reaper, give-up backoff,
   `proxy.reload` — appends another closer holding a strong reference to a
   `platformTransport` that is already dead. On a 500-proxy node with hourly URL refresh
   this grows without bound for the process lifetime, pins every historical transport
   against GC, and makes `yieldCoordinatorSession` (`hotswap.go:105`) call `Close()` on
   thousands of stale objects during the one moment that must be fast.

**Failure scenario:** node with 200 URL-sourced proxies, 24h uptime, hourly refresh with
30% churn → ~1400 retained closers and transports. Operator triggers a hotswap; the parent
spends the handoff window closing dead transports while 1400 SIGUSR2 handler goroutines
contend on `hotSwapLock`.

**Fix:**
- Move the ACK + listener arming out of `provideWithProxy` entirely. It is a
  process-lifecycle event, not a per-proxy one. Arm it once in `provide()` behind a
  `sync.Once` fed by a channel that the first successfully-connected transport closes.
- Make `RegisterCoordinatorCloser` return a deregistration handle and have
  `provideWithProxy` `defer unregister()` — or key closers by `identityKey` in a map so a
  relaunch replaces rather than appends.
- Guard `hotSwapIPC` with `sync.Once` regardless.

---

### F-6 — Yield/flush happen before operations that can fail, leaving an unrecoverable half-dead parent

`provider/hotswap.go:344-368` (PID 1) and `provider/hotswap.go:374-385` (standard)

Both branches perform irreversible teardown *before* the step that can fail:

PID 1: `yieldCoordinatorSession()` (344) → `flushRetentionEvents()` (347) →
`lifetimeStore.Flush()` (348) → `execInPlaceFunc(...)` (365). If execve returns an error the
code logs "Live provider retained" (366) and returns. It is not retained in any meaningful
sense: the coordinator transport is closed with nothing to reopen it, and
`flushRetentionEvents` is **terminal** — `proxy_health_log.go:396-399` sets
`retentionEventClosed = true` and closes the channel permanently, after which
`appendRetentionEvent` silently drops every subsequent event
(`proxy_health_log.go:370-375`). The process keeps running with no transport and no
retention telemetry, and nothing signals the operator.

Standard: `yieldCoordinatorSession()` (374) runs *before* the TAKEOVER write (377). If the
write fails the code kills the candidate and returns an error (383-384) — but the parent's
coordinator session is already gone.

`lifetimeStore.Flush()` is idempotent and fine (`lifetime_metrics.go:136-145`); both flushes
are synchronous, so the stated ordering concern (flush-before-execve) is **correct** —
`flushRetentionEvents` blocks on `<-retentionEventDone` (`proxy_health_log.go:401`).

**Fix:**
- Move `yieldCoordinatorSession()` to *after* the last fallible step in each branch. In the
  PID-1 branch that means yielding immediately before `execInPlaceFunc` and accepting that
  execve either succeeds (yield is moot) or fails (nothing was yielded). In the standard
  branch, send TAKEOVER first, then yield.
- Make `flushRetentionEvents` reversible, or add a `drainRetentionEvents()` that flushes
  without closing, and only call the terminal form once the exit is guaranteed.
- If `execInPlace` fails, the correct action is not "return an error and keep limping" — it
  is to re-establish the coordinator or deliberately exit non-zero so the supervisor
  restarts. Silently degraded is the worst option.

---

### F-7 — Socketpair fds lack `SOCK_CLOEXEC`; the parent's end leaks into the candidate

`provider/hotswap_unix.go:58`

```go
fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
```

Neither descriptor gets `SOCK_CLOEXEC`. `os/exec` only arranges 0/1/2 and `ExtraFiles`; it
relies on close-on-exec for everything else. So `fds[0]` (the parent's end, `parentFile`) is
**inherited by the candidate** at its original descriptor number, and by any other process
the provider forks while the socketpair is open.

Consequences:
- The socketpair never reaches EOF from the child's perspective when the parent closes its
  copy, because the child holds a duplicate. Any future EOF-based liveness detection on
  this channel is silently broken.
- The successor process carries a stray descriptor forever; across repeated hotswaps this
  accumulates (each generation inherits its predecessor's leaked ends).
- Without `SOCK_CLOEXEC` this is also racy against concurrent `fork`+`exec` from other
  goroutines (the classic reason Go's own code always passes `SOCK_CLOEXEC`).

`fds[1]` also needs it — `dup2` clears `FD_CLOEXEC` on the new descriptor, so `ExtraFiles`
still works correctly with `SOCK_CLOEXEC` set.

**Fix:**
```go
fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
```

Related and worth noting: fd 3 in the candidate is *not* leaked into a post-execve image on
the PID-1 path, because the PID-1 parent is never a candidate and `session.Close()` runs at
`hotswap.go:341` before the exec. That part is correct.

---

### F-8 — `session.Wait()` on the canary has no timeout; a wedged canary hangs the handoff forever

`provider/hotswap.go:340`

```go
_ = writeHotswapMessage(session.Writer, ...CANARY_DONE...)
_ = session.Wait()          // unbounded
session.Close()
```

`Wait()` blocks indefinitely on `childCmd.Wait()` (`hotswap_unix.go:48-52`). The canary is
supposed to `exitFunc(0)` on CANARY_DONE (`hotswap.go:250`), but it can fail to: a full
stdout pipe (the canary shares the parent's stdout via `hotswap_unix.go:70`, and a stalled
Docker log collector blocks writes), a goroutine wedged in `connect.HandleError`, or a
`SIGSTOP`. The handoff then blocks forever inside the signal-handler goroutine while holding
`hotSwapLock`, so every subsequent `urnet-tools hotswap` returns "already in progress" and
the operator has no way to clear it short of restarting.

Also: the CANARY_DONE write error is discarded (`_ =`). If the canary already died, the
write fails with EPIPE and the code proceeds to `Wait()` anyway — harmless here, but the
discarded error is the same pattern that hides real failures at `hotswap.go:335`.

**Fix:** wrap the wait:
```go
done := make(chan error, 1)
go func() { done <- session.Wait() }()
select {
case <-done:
case <-time.After(HotSwapCanaryExitTimeout): // e.g. 5s
    tlog("⚠️ [hotswap] canary did not exit in time; killing\n")
    session.Kill()
}
```
and log the CANARY_DONE write error instead of discarding it.

---

### F-9 — Candidate mints/persists client identities into a store with cross-process lost updates and a shared temp filename

`provider/main.go:4199-4206`, `provider/main.go:3219-3234`,
`provider/client_jwt_store.go:200-206`, `provider/client_jwt_store.go:245-261`,
`provider/client_jwt_store.go:68-72`

The invariant "the candidate does not mint new identities" does not hold on the standard
branch. After TAKEOVER the candidate runs the full `provideAuth` path, which on any
reuse miss (network_id mismatch `main.go:4049`, expired stored JWT with failed renewal
`main.go:4070`, unparseable client_id `main.go:4096`) falls through to a fresh
`AuthNetworkClient` mint and then `globalClientJWTStore.Put` at `main.go:4199`. It also
unconditionally writes the client key seed (`main.go:4221`) and provider TLS cert/key
(`main.go:4230`).

Meanwhile the parent is still live for up to `HotSwapDrainTimeout` (30s) and its own
`runProxyJWTWatcher`/renewal paths also call `Put`. The store's `mu` is a **process-local**
mutex; there is no file lock. And:

- `loadLocked` reads the file exactly once per process (`client_jwt_store.go:69-72`,
  guarded by `s.loaded`). Every `Put` then serialises that process's **entire stale
  in-memory map**. So a parent renewal for proxy A written at t=10s is silently erased by a
  candidate `Put` for proxy B at t=12s, which rewrites the whole map from its t=0 snapshot.
- `flushLocked` writes to `s.path + ".tmp"` — a **fixed, shared** temp name
  (`client_jwt_store.go:257`). Two live processes writing it concurrently interleave, and
  whichever renames second promotes a possibly-torn file to `.client_jwts.json`.

**Failure scenario:** 50-proxy node, hotswap triggered. Parent's JWT watcher renews proxy
`1.2.3.4:8080` at t=8s. Candidate mints a fresh identity for `5.6.7.8:8080` at t=11s and
`Put`s its stale map. The renewal for `1.2.3.4:8080` is lost; on the next restart that
proxy mints fresh and loses its server-side reliability reputation. Worse case, the two
`.tmp` writes interleave and the store is corrupt, which trips the
`⚠️ [jwt-store] corrupt ... starting fresh` path (`client_jwt_store.go:86`) and remints
**every** identity on the node.

**Fix:**
- Use an unpredictable temp name (`os.CreateTemp(dir, ".client_jwts-*.tmp")`) in
  `flushLocked` — this is the same fix already applied to `installBinary`
  (`update.go:857-859`) and should be mirrored here.
- Take an `flock(LOCK_EX)` on the store file around load+mutate+flush so the two live
  processes serialise, and re-read under the lock instead of trusting the `loaded` cache.
- Explicitly reconcile the design intent: either the candidate must not write identity
  material until the parent has fully exited (gate `Put`/`writeProviderClientKeySeed`/
  `writeProviderTlsCertAndKey` on a "parent gone" signal), or the store must be made safe
  for two writers. Right now it is neither.

---

### F-10 — `HotSwapAckTimeout` cannot cover candidate bring-up, and a candidate that dies post-TAKEOVER still lets the parent exit

`provider/hotswap.go:377-424`, `provider/main.go:3256`

The ACK is sent from `main.go:3256`, i.e. after `platformTransport` construction, which is
after the full auth path including retry backoff (`main.go:2988-3129`, delays up to 30s per
attempt). `HotSwapAckTimeout` is 15s (`hotswap.go:60`). On any node whose first auth attempt
does not succeed immediately, the parent will time out.

This ordering is *defensible* — waiting for "actually live" is stronger than waiting for
"pre-flight OK", and arming the child's SIGUSR2 listener only after ACK correctly prevents
cascade storms. The problem is that timing out is treated as a warning, not a failure:

```go
hotswap.go:401  case <-time.After(HotSwapAckTimeout):
hotswap.go:402      tlog("⚠️ ... ACK timed out ... Proceeding with drain.\n")
```

and then the parent unconditionally exits 30s later (`hotswap.go:413-424`). There is no
post-TAKEOVER liveness check at all: if the candidate's auth fails permanently and its
`provideWithProxy` returns at `main.go:3194`, or the candidate crashes, the parent still
exits on schedule. Result is a total outage with an `⚠️`-level log line as the only signal.

Compounding: the drain is a bare `time.Sleep(HotSwapDrainTimeout)` (`hotswap.go:418`)
despite the comment claiming "Wait for active streams to wind down". Nothing is inspected;
every in-flight client stream is severed at exactly 30s regardless of state.

**Fix:**
- Raise `HotSwapAckTimeout` to cover realistic bring-up (60-90s), and make ACK **mandatory**:
  on timeout or on candidate exit, `session.Kill()`, re-establish the coordinator, and abort
  the handoff with an error rather than draining.
- Monitor `session.Wait()` concurrently during the drain; if the candidate exits before the
  parent does, cancel the drain and recover.
- Make the drain actually drain: poll live stream/contract counts (the same counters
  `liveContractsAcquired` uses, `main.go:2468`) and exit early when they hit zero, capped at
  `HotSwapDrainTimeout`.

---

## MEDIUM

### F-11 — `getHotSwapChildIPC` trusts fd 3 exists; a stray `URNETWORK_HOTSWAP=1` bricks the container

`provider/hotswap_unix.go:107-114`

```go
if os.Getenv(EnvHotSwap) != "1" { return nil, false }
ipcFile := os.NewFile(uintptr(3), "hotswap-child-ipc")
return ipcFile, true
```

No validation that fd 3 is open or is a socket. An operator who sets
`URNETWORK_HOTSWAP=1` in their Docker env — plausible given the neighbouring
`URNETWORK_HOT_RESTART` documented in CLAUDE.md, and the two names are one character apart —
gets: `applyStagedSession` silently skipped (`main.go:2647`), then the handshake writes to a
bogus fd, fails, and `os.Exit(2)` (`main.go:2686`). Under `Restart=no` / the shell
supervisor's crash ladder this is a crash loop with a confusing
`❌ [hotswap] Candidate pre-flight failed` message that never mentions the env var.

**Fix:** stat fd 3 and verify it is a unix socket before claiming candidate mode; if it is
not, log loudly (`URNETWORK_HOTSWAP=1 is set but no IPC descriptor was inherited — this
variable is set automatically by the provider and must not be set manually`) and return
`false` so the process starts normally. Also consider renaming the env var to something
less confusable, e.g. `URNETWORK_HOTSWAP_CANDIDATE`.

### F-12 — `triggerHotSwap` error is swallowed; verification gate is partly tautological

`internal/urnettools/update.go:613-619`, `internal/urnettools/update.go:656`

```go
if err := triggerHotSwap(p); err == nil {
```
The error is discarded. On Windows this is intended (always errors, falls through to
`restartForUpdate`), but a real failure — process vanished between `Discover()` and the
signal, EPERM — is silently indistinguishable, and the operator sees a normal restart with
no explanation.

On the gate at 656: `p.Version` is the *pre-update* version captured by `Discover()` before
the swap, and `cfg.Tag` is the target. `hotSwapTriggered && p.Version != cfg.Tag` is
therefore a loop-invariant constant that is true for every real update, which disables the
`rp.PID != oldPID` requirement entirely for the hotswap case. The verification then rests
solely on `providerVersionFromBuildinfo(procExe) == cfg.Tag`.

That happens to be sound on Linux — `installBinary` renames a new inode into place
(`update.go:905`), so the old running process's `/proc/<pid>/exe` points at a deleted inode
and `!rp.BinaryDeleted` correctly fails until the handoff completes. But the soundness is
accidental and undocumented; it depends entirely on `installBinary` never writing in place.
`rp.PID == 1` in the same disjunction is dead weight.

**Fix:** log the `triggerHotSwap` error at 615 (`fmt.Printf("hotswap trigger unavailable
(%v); falling back to restart\n", err)`). Replace the tautological version comparison with
an explicit comment that `!rp.BinaryDeleted` is the load-bearing check, or drop the
`p.Version != cfg.Tag` clause and rely on `!rp.BinaryDeleted && procVersion == cfg.Tag`
alone. Drop `rp.PID == 1`.

### F-13 — `provider/hotswap_test.go` has no build tag; breaks Windows vet/test

`provider/hotswap_test.go:1-15`

```
$ GOOS=windows go vet ./provider/...
vet: provider/hotswap_test.go:102:22: undefined: syscall.Socketpair
```

The file references `syscall.Socketpair`, `net.ListenUnixgram`, `notifySystemdMainPID`, and
the `childCmd`/`parentFd` fields that only exist on the Unix `HotswapParentSession`
(`hotswap_unix.go:24-29`; the Windows struct at `hotswap_windows.go:16-19` has neither).
Non-test builds are clean, so this does not break the release binary, but any
`GOOS=windows go vet ./...` in CI or a pre-commit hook fails.

**Fix:** add `//go:build !windows` to `provider/hotswap_test.go`, and split the two
platform-neutral tests (`TestHotSwapMessageFraming`, `TestYieldCoordinatorSession`) into a
tag-free `hotswap_common_test.go` so they still run on Windows.

### F-14 — `TestHotSwapPreflightPermissionHealing` fails when run as root

`provider/hotswap_test.go:491-525`

The test chmods the JWT to `0000` (504) and asserts preflight heals it to `0600` (522). As
root, `os.ReadFile` on a `0000` file **succeeds** (CAP_DAC_OVERRIDE), so the
`os.IsPermission` branch at `hotswap.go:151` never runs, no chmod happens, and the final
assertion sees `0000` and fails. Docker-based CI and any root test run will fail here. It
passes locally only because the reviewer is unprivileged.

**Fix:** `if os.Geteuid() == 0 { t.Skip("permission healing is unobservable as root") }`.

### F-15 — `flushRetentionEvents` is terminal but called on a fallible path

See F-6 for the full mechanism; calling it out separately because the fix is local and
independent: `provider/hotswap.go:347` permanently disables retention telemetry for the
process, and `hotswap.go:365` can fail afterwards. Split the API into `drainRetentionEvents()`
(flush, keep the writer alive) and the existing terminal `flushRetentionEvents()`; the
PID-1 branch should use the former until execve is committed.

### F-16 — JWT permissions fight between the healer and the writer

`provider/hotswap.go:152-153` heals the account JWT to `0600`;
`provider/main.go:2383` and `provider/main.go:1217` both write it with `atomicWriteFile(...,
0700)`. So every refresh or re-auth puts the file back to `0700` (an execute bit on a
credential file), and the healer only fires on an actual permission error. The two should
agree; `0600` is the correct mode for both. Low blast radius, but it means the "self-healing"
claim is undone by the next refresh.

---

## LOW / NIT

### F-17 — `execInPlace` uses `==` instead of `errors.Is` for ETXTBSY
`provider/hotswap_unix.go:164`. `err == syscall.ETXTBSY` happens to work because
`syscall.Exec` returns a bare `syscall.Errno`, but it silently stops matching if the error is
ever wrapped. Use `errors.Is(err, syscall.ETXTBSY)`.

On the sizing question: 3 attempts × 150ms = 450ms. `ETXTBSY` from `execve` requires a writer
to hold the file open for write; since `installBinary` uses rename (never opening the live
path for write), the realistic source is the F-4 rollback path — which can hold it open far
longer than 450ms. The retry budget is not the real fix; F-4 is. If kept, make it
exponential (150/300/600ms) and log the final failure at CRIT, not just `tlog`.

### F-18 — DNS/dual-stack retry window is short but the structure is the bigger issue
`provider/hotswap.go:183-205`. Total worst case is bounded by `3 × (4s + 4s) + 250ms + 500ms
= ~24.75s`, which already exceeds `HotSwapPreflightTimeout` (20s) — so a genuinely slow
network produces a *preflight timeout* rather than the intended dial error, and the operator
sees the wrong diagnosis. The backoff itself (750ms of sleep) is fine; the dial timeouts are
what blow the budget. Either drop `dialer.Timeout` to 2s or raise
`HotSwapPreflightTimeout` above the worst-case dial budget. The tcp→tcp4 fallback logic
itself is correct and worth keeping.

### F-19 — Preflight chmod/read TOCTOU is real but low-value
`provider/hotswap.go:152-155` and `hotswap_unix.go:77` both chmod-by-path then use-by-path.
An attacker able to swap `~/.urnetwork/jwt` or the provider binary between the two calls
already owns the provider's home directory. Noting for completeness; `installBinary`'s
fd-based approach (`update.go:882-898`) is the pattern to copy if this is ever hardened.

### F-20 — Canary pollutes logs with a phantom startup
The canary runs `main.go:2651-2674` before the handshake, so every hotswap emits a duplicate
`❤️ [startup] provider version=...`, a `critLog("STARTUP: ...")` from a PID that immediately
vanishes, and a `🔑 [jwt] expires in ...` line — into the same stdout and the same crit-log
file as the live provider (`hotswap_unix.go:70-71`). Monitoring that alerts on STARTUP lines
will false-positive on every hotswap. Suppress these when `EnvHotSwap == "1"`, or move the
candidate check above them.

### F-21 — `cmdHotswap` validates argument count too late
`internal/urnettools/hotswap.go:19-24`. The `!p.Running` check (19) precedes the
`len(rest) > 0` check (22), so `urnet-tools hotswap bogus` against a stopped provider reports
"is not running" instead of the actual usage error. Swap the two blocks.

### F-22 — `exe = os.Args[0]` fallback can be relative
`provider/hotswap.go:280-283`. If `os.Executable()` fails, `os.Args[0]` may be a bare name or
relative path, which `exec.Command`/`execve` will resolve against the current working
directory rather than `$PATH`. Better to fail the handoff than to exec an ambiguous path.

(For the record: `os.Executable()` itself is safe here — Go's `executable_procfs.go` strips
the `" (deleted)"` suffix, so a post-rename binary swap still yields the correct path. I
checked this specifically because it would have been a CRITICAL if not handled.)

### F-23 — `cleanEnv` strip of `EnvHotSwap` is dead code
`provider/hotswap.go:353-358` filters `URNETWORK_HOTSWAP=` out of the parent's environment,
but the PID-1 parent is never a candidate and never has it set. Harmless and cheap
insurance; the test at `hotswap_test.go:370-375` asserts on it. Keep, but the comment should
say it is defensive rather than implying the variable is present.

---

## Test coverage gaps

Confirmed by reading `provider/hotswap_test.go` in full (527 lines, 9 tests, all passing
under `-race` as non-root).

**What is covered well:** message framing round-trip; preflight JWT-missing / expired /
valid; child handshake success and failure; canary CANARY_DONE → exit; systemd notify
payload; PID-1 execve success and preflight-failure-preserves-process.

**On the test-direction question you raised:** the goroutine at `hotswap_test.go:396-408`
*does* match the real flow. It writes READY into `childFile` (child→parent, read by the
parent's `session.Reader` which wraps `parentFile`) and then reads CANARY_DONE from
`childReader` (parent→child, written by the parent to `session.Writer`). Directions are
correct.

**Real gaps, roughly in priority order:**

1. **The entire standard (PID != 1) branch is untested** — `hotswap.go:372-426`. No test for
   TAKEOVER→ACK, none for the ACK timeout at 401, none for the systemd-notify call at 406,
   none for the drain goroutine. This is the branch that actually runs on every shipped
   deployment (F-1, F-2), and it has zero coverage.
2. **No test for candidate death after TAKEOVER** (F-10) — the case where the parent exits
   into a total outage.
3. **`spawnHotSwapCandidate` is never exercised.** Both PID-1 tests stub it with
   `childCmd: nil`, which makes `session.Wait()` (`hotswap_unix.go:48-52`) and
   `session.Kill()` (`hotswap_unix.go:39-45`) no-ops. So
   `TestHotSwapParentPID1PreflightFailurePreservesProcess` asserts execve was not called but
   **does not verify the candidate is killed** — exactly as you suspected. Add a fake
   `childCmd` (e.g. `exec.Command("sleep", "60")`) and assert the process is reaped.
4. **No ETXTBSY retry test** (`hotswap_unix.go:160-172`). Easy: inject a fake exec func that
   returns `syscall.ETXTBSY` twice then `nil`, assert 3 calls and ~300ms elapsed.
5. **No concurrent-SIGUSR2 test.** `hotSwapLock.TryLock()` (`hotswap.go:269`) is the only
   thing standing between F-5's N listeners and N concurrent handoffs; it deserves a direct
   test.
6. **No test for the DNS/tcp4 fallback** (`hotswap.go:193-199`). Bind an IPv6-only listener,
   or inject a dialer, and assert the fallback path fires.
7. **No test that `provide()` skips `applyStagedSession` under `EnvHotSwap=1`**
   (`main.go:2647`) — one of the two identity invariants the feature claims.
8. **No test for the `auth-provide` argv problem** (F-3). This is the single highest-value
   test to add: spawn a candidate with `auth-provide` argv and assert the JWT is untouched.
9. **`TestHotSwapParentPID1ExecveSuccess` goroutine can call `t.Errorf` after the test
   returns** (`hotswap_test.go:406`) — nothing synchronises the goroutine's exit with the
   test body. Calling `t.Errorf` on a finished test panics. Add a done channel.
10. **`internal/urnettools/hotswap_test.go`** (49 lines) covers only invalid PID, a
    not-running target, `--help`, and cobra registration. Nothing covers `triggerHotSwap`
    actually delivering SIGUSR2 to a live process, and nothing covers the
    `updateProvider` hotswap branch or its rollback (F-4).

---

## Windows (secondary) — verified

Build tags are correct on the non-test files: `hotswap_unix.go:1` (`//go:build !windows`),
`hotswap_windows.go:1` (`//go:build windows`), and the same pairing in
`internal/urnettools/`. `GOOS=windows go build ./...` is clean. The stubs return honest
errors rather than silently no-op'ing, which is the right call — except
`startHotSwapSignalListener` (`hotswap_windows.go:35-37`) and `notifySystemdMainPID`
(`hotswap_windows.go:39-41`), which are silent no-ops; that is fine for the latter and
correct-by-necessity for the former.

The one real Windows defect is F-13 (test file missing its build tag).

Note for whenever the named-pipe adapter lands: `triggerHotSwap` on Windows
(`internal/urnettools/hotswap_windows.go:71-73`) returning an error means
`updateProvider` falls through to `restartForUpdate` (`update.go:621-629`), which is the
correct degradation. That path is worth keeping when the adapter is implemented, as a
fallback.

---

## Docker PID 1 (secondary) — verified, with one correction

The specific ordering you asked about is **correct**: `childCmd.Wait()` → `Close()` →
`yieldCoordinatorSession()` → `flushRetentionEvents()` → `lifetimeStore.Flush()` →
`execInPlace()`. Both flushes are synchronous (`proxy_health_log.go:401` blocks on
`retentionEventDone`; `lifetime_metrics.go:136-145` is a direct locked write), so nothing is
lost to the process-image replacement.

Two things to correct in the design narrative:

- **stdout/stderr after execve are safe.** The PID-1 branch never closes 0/1/2, and execve
  preserves open descriptors that lack `FD_CLOEXEC`. The canary's `cmd.Stdout = os.Stdout`
  (`hotswap_unix.go:70`) creates a dup that dies with the canary; PID 1's originals are
  untouched. No issue here.
- **The branch is unreachable in production** (F-1). PID 1 in the shipped image is
  `/app/entrypoint.sh` → `/app/start_stable.sh`. Until the start scripts `exec` the provider,
  every line of `hotswap.go:331-370` is dead code on real containers, and the *standard*
  branch — the one with no test coverage — is what actually runs.

---

## Suggested order of work

1. F-1 + F-2 together (deployment topology). Nothing else matters until a handoff can
   survive in at least one shipped environment. Both need script/unit changes, so they
   belong in one PR with `needs-fleet-deploy`.
2. F-3 (candidate re-running `auth`) — small, self-contained, and it is a live credential
   bug today.
3. F-4 (atomic rollback + rollback timing).
4. F-5 (move ACK/listener/closer registration out of the per-proxy path).
5. F-6 + F-15 (ordering of irreversible teardown).
6. F-7, F-8, F-9, F-10.
7. Tests: gap #8 (auth-provide), #1 (standard branch), #3 (real childCmd), #5 (concurrent
   SIGUSR2) — in that order.
8. F-11 through F-23 as cleanup.
