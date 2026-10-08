package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/urnetwork/connect"
)

func proxyReloadPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "proxy.reload"), nil
}

func proxyLockPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "proxy.lock"), nil
}

// readReloadSeq reads the current sequence number from the trigger file.
// Returns 0 if the file does not exist.
func readReloadSeq(path string) (int, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		// Treat an unparseable trigger as seq 0 rather than failing the watcher.
		tlog("[proxy] warn: reload trigger file unparseable: %v\n", err)
		return 0, nil
	}
	return n, nil
}

// acquireProxyLock creates the lock file at the default path. Returns an error
// if a reload is already in progress (lock already held).
func acquireProxyLock() (func(), error) {
	path, err := proxyLockPath()
	if err != nil {
		return nil, err
	}
	return acquireProxyLockAt(path)
}

// acquireProxyLockWithRetry attempts to acquire the proxy lock with backoff
func acquireProxyLockWithRetry() (func(), error) {
	var release func()
	var err error
	for i := 0; i < 5; i++ {
		release, err = acquireProxyLock()
		if err == nil {
			return release, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, err
}

// acquireProxyLockAt is the path-explicit form, for testing.
func acquireProxyLockAt(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if existing, err := os.ReadFile(path); err == nil {
		if isLockStale(existing) {
			// An empty or unparseable lock younger than the write grace is
			// likely an acquirer that has created the file but not yet
			// written its pid+timestamp (create and write are not atomic).
			// Stealing it would admit two holders at once; falling through
			// to O_EXCL below fails safely instead.
			steal := true
			if _, _, ok := parseLockContent(existing); !ok {
				if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) < proxyLockWriteGrace {
					steal = false
				}
			}
			if steal {
				// Remove only the file we actually judged stale: two racing
				// stealers could otherwise have the slower one delete the
				// faster one's fresh lock. The re-read narrows the window;
				// POSIX has no compare-and-delete, so a microsecond-sized
				// window remains — a fresh lock installed inside it fails
				// our O_EXCL below, which is the safe outcome.
				if current, err := os.ReadFile(path); err == nil && string(current) == string(existing) {
					os.Remove(path)
				}
			}
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("reload already in progress — try again in a moment")
		}
		return nil, err
	}
	nonce := atomic.AddUint64(&proxyLockNonce, 1)
	content := fmt.Sprintf("%d\n%d\n%d\n", os.Getpid(), time.Now().Unix(), nonce)
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return nil, err
	}
	// Release removes the lock ONLY while the file is still ours. A stale
	// steal can replace it with a NEW holder's lock (isLockStale admits a
	// live holder's lock once it ages past the PID-reuse bound), and an
	// unconditional remove would delete THEIR lock and let a third acquirer
	// in while they are still working. The read-compare-remove cannot be
	// atomic on POSIX; the residual window (a steal landing between the
	// read and the remove) is accepted — closing it needs flock, which is
	// not portable to the Windows build.
	var once sync.Once
	return func() {
		once.Do(func() {
			if current, err := os.ReadFile(path); err == nil && string(current) == content {
				os.Remove(path)
			}
		})
	}, nil
}

const proxyLockStaleAge = 5 * time.Minute

// proxyLockNonce disambiguates lock content across acquisitions within one
// process: with second-granularity timestamps two acquisitions by this
// process inside the same second would otherwise carry byte-identical
// content, and a late release of the first would remove the second holder's
// lock.
var proxyLockNonce uint64

// proxyLockWriteGrace covers the non-atomic create-then-write window of a
// lock acquisition: lock content that does not parse and is younger than
// this is assumed mid-write, not stale.
var proxyLockWriteGrace = 5 * time.Second

// processStart approximates this process's start time. A hot swap execs in
// place: the pid survives and deferred cleanup never runs, so a proxy.lock
// written by the previous image carries our own pid with an old timestamp
// and would read as "held by a live holder" until proxyLockMaxAge ages it
// out — blocking every lock consumer in the fresh image for up to an hour.
var processStart = time.Now()

// cleanStaleSelfProxyLock removes a proxy.lock that carries this process's
// own pid but predates this process's start. Run it at startup, before the
// first reload, so an escalated hot restart is not greeted by its own old
// lock.
func cleanStaleSelfProxyLock() {
	path, err := proxyLockPath()
	if err != nil {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	pid, ts, ok := parseLockContent(b)
	if !ok || pid != os.Getpid() {
		return
	}
	// Inclusive bound: a lock written by the previous image in the same
	// integer second as this process's start must be cleaned too, and at
	// cleanup time this image cannot yet hold a lock of its own (it runs
	// before any local consumer).
	if ts <= processStart.Unix() {
		// Compare before removing, like the steal path: another holder
		// could have replaced the file between the read and the remove.
		if current, err := os.ReadFile(path); err != nil || string(current) != string(b) {
			return
		}
		if os.Remove(path) == nil {
			tlog("[proxy] removed a proxy.lock left behind by a previous image with this pid\n")
		}
	}
}

// proxyLockMaxAge is the outer bound: even if the holder appears alive
// (Signal(0) succeeds), a lock older than this is unconditionally stale.
// This prevents a PID-reuse scenario (OS reissues the same PID to an
// unrelated process) from wedging the proxy lock forever. 1 hour is
// generous enough for the largest fleet reloads while bounding worst-case
// staleness under PID reuse.
const proxyLockMaxAge = 1 * time.Hour

// writeReloadTriggerDebounce is the minimum interval between consecutive
// trigger writes. Callers inside fetch/reaper loops may fire hundreds of
// times per cycle; this ensures the watcher only picks up one trigger
// per debounce window instead of spawning overlapping reloads.
var writeReloadTriggerDebounce = 30 * time.Second

// drainPollInterval is how often the drain-completion goroutine rechecks a
// draining proxy's client count. A package var (rather than a literal) so
// tests can shrink it and finish in milliseconds instead of blocking on the
// real interval.
var drainPollInterval = 5 * time.Second

// reloadSlotTimeout bounds how long a new reload waits for an in-flight
// reload to release its slot before skipping. This replaced an unbounded
// r.mu.Lock() wait: a reload that stalls mid-body used to block every future
// trigger silently and forever (observed on a starved box: 13h, see the LA7
// incident) because the file-lock auto-steal never frees the in-process
// mutex. Vars (not consts) so tests can shrink them.
var reloadSlotTimeout = 10 * time.Minute

// reloadSlotPollInterval is the poll cadence while waiting for the slot.
var reloadSlotPollInterval = 250 * time.Millisecond

// reloadHardLimit bounds a single reload's runtime. The reload body checks it
// at phase boundaries and aborts cleanly; RunReloadWatchdog escalates when a
// reload overruns it. A healthy-box reload — even for a many-thousand-proxy
// fleet — completes in seconds to a couple of minutes; overrunning this means
// the box is pathological (thrashing) or the goroutine is stuck.
var reloadHardLimit = 20 * time.Minute

// reloadWatchdogInterval is how often the reload watchdog checks for an
// overrunning reload.
var reloadWatchdogInterval = 30 * time.Second

// reloadWatchdogReFire bounds how often one overrunning episode asks for a
// hot restart. The hotswap decline ledger throttles repeats further; this
// just bounds our asks.
var reloadWatchdogReFire = 5 * time.Minute

// reloadOverdueAction is the reload watchdog's escalation when a reload has
// held the slot past reloadHardLimit. Default: ask for a hot restart — the
// only recovery from a goroutine that is truly stuck — gated on the
// hot_restart control key. Overridable for tests.
var reloadOverdueAction = func() error {
	if !hotRestartEnabled() {
		return fmt.Errorf("hot restart disabled (hot_restart=off)")
	}
	trigger := getHotSwapTrigger()
	if trigger == nil {
		return fmt.Errorf("hot swap trigger not installed")
	}
	return trigger()
}

var lastReloadTriggerTime struct {
	sync.Mutex
	ts      time.Time
	pending bool // true if a suppressed call is waiting for trailing write
}

func writeReloadTrigger(path string) error {
	now := time.Now()
	lastReloadTriggerTime.Lock()
	elapsed := now.Sub(lastReloadTriggerTime.ts)
	if elapsed < writeReloadTriggerDebounce {
		if !lastReloadTriggerTime.pending {
			lastReloadTriggerTime.pending = true
			remaining := writeReloadTriggerDebounce - elapsed
			time.AfterFunc(remaining, func() { doWriteReloadTrigger(path) })
		}
		lastReloadTriggerTime.Unlock()
		return nil
	}
	lastReloadTriggerTime.ts = now
	lastReloadTriggerTime.Unlock()
	return doWriteReloadTrigger(path)
}

var reloadTriggerMutex sync.Mutex

func doWriteReloadTrigger(path string) error {
	reloadTriggerMutex.Lock()
	defer reloadTriggerMutex.Unlock()

	lastReloadTriggerTime.Lock()
	lastReloadTriggerTime.pending = false
	lastReloadTriggerTime.ts = time.Now()
	lastReloadTriggerTime.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	seq, _ := readReloadSeq(path)
	seq++
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(seq)), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// parseLockContent extracts the pid and timestamp from lock content. ok is
// false for empty, truncated or unparseable content — which must NOT be
// treated as stale on sight while an acquirer may still be mid-write (see
// the write-grace guard in acquireProxyLockAt).
func parseLockContent(data []byte) (pid int, ts int64, ok bool) {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 {
		return 0, 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil {
		return 0, 0, false
	}
	ts, err = strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return pid, ts, true
}

func isLockStale(data []byte) bool {
	// Only the first two lines (pid, timestamp) are interpreted; the third
	// is a per-acquisition nonce, and any trailing content must not confuse
	// the parse.
	pid, ts, ok := parseLockContent(data)
	if !ok {
		return true
	}

	// Liveness is the authoritative signal (finding #13): a lock held by a
	// LIVE process is never stale, however old, because a legitimately slow
	// reload (>5min on a large fleet) must not be stolen by a second
	// acquirer. Only treat as stale when the holder is conclusively gone.
	process, err := os.FindProcess(pid)
	if err == nil {
		err = process.Signal(syscall.Signal(0))
	}
	if err == nil {
		// Holder alive and working — stale ONLY if the lock is
		// unreasonably old (PID-reuse bound; proxyLockMaxAge).
		// This prevents a reused PID from wedging the lock forever.
		return time.Since(time.Unix(ts, 0)) > proxyLockMaxAge
	}
	if errors.Is(err, os.ErrProcessDone) ||
		strings.Contains(err.Error(), "no such process") ||
		strings.Contains(err.Error(), "process already finished") {
		// Holder conclusively gone — stale.
		return true
	}
	// Inconclusive (e.g. EPERM: another user's live process we can't
	// signal). Use age as the tiebreaker; very old + unverifiable = stale.
	return time.Since(time.Unix(ts, 0)) > proxyLockStaleAge
}

// ProxyReloader manages hot-reload of proxy goroutines. It is driven by the
// reload watcher goroutine, which polls the trigger file for a changed sequence
// number and calls reload(). reload() is serialized by mu so two reloads never
// overlap.
type ProxyReloader struct {
	mu sync.Mutex // serializes reloads
	// reloadActive/reloadStartedAt track the in-flight reload for
	// RunReloadWatchdog. reloadStartedAt is stored immediately BEFORE the
	// slot is published as active, and both are cleared before r.mu is
	// released, so the watchdog never sees active=true while the slot is
	// free or paired with a previous reload's timestamp.
	reloadActive    atomic.Bool
	reloadStartedAt atomic.Int64 // unix nanos
	// reloadSlotSkipped records that the last reload() could not take the
	// slot; the watcher then leaves the trigger sequence unrecorded so the
	// trigger is retried on a later tick instead of being dropped.
	reloadSlotSkipped atomic.Bool
	// watchdogActionInFlight single-flights the hot-restart request so a
	// slow handoff cannot stack goroutines on every re-fire.
	watchdogActionInFlight atomic.Bool
	cancelMap              map[string]context.CancelFunc
	// runningAuth records the settings each running proxy was launched with.
	// The reloader diffs the desired set by address only, so without this it
	// cannot tell whether a running proxy's credentials still match the
	// config. A re-paste with new credentials for the same host:port must
	// rotate the running proxy, not be a silent no-op (LA7 incident
	// 2026-09-18: 100 proxies pasted with new creds, "added 100" printed,
	// daemon kept dialing the old user).
	// TODO: refactor cancelMap and cancelMapMu into a struct owned by ProxyReloader
	// to avoid storing a *sync.Mutex pointer across function boundaries.
	cancelMapMu *sync.Mutex
	runningAuth map[string]*connect.ProxySettings
	state       *ProxyState
	sourcePath  string // "" = internal config (~/.urnetwork/proxy); else external file
	parentCtx   context.Context
	wg          *sync.WaitGroup

	// spawnProxy starts a proxy goroutine's work (the provideWithProxy closure).
	spawnProxy func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool)

	directDone      chan struct{}                 // closed when direct goroutine exits; nil when not running
	drainingProxies map[string]context.CancelFunc // proxies draining active sessions
	drainMu         sync.Mutex
	// trimShed records addresses currently held out of the running pool by the
	// trim cap (operator or automatic OOM). It outlives a single reload() call
	// so the drain-completion goroutine, which wakes up independently later,
	// can tell "this address was shed by trim" from "some other proxy's drain
	// completed while a cap happens to bind" -- only the former must skip the
	// re-trigger. Guarded by drainMu. Cleared when the address is admitted or
	// launched again.
	trimShed  map[string]bool
	networkID string
}

// proxyLaunches records, per proxy address, the launch generation that
// currently owns the address. A credential rotation relaunches a proxy while
// the cancelled goroutine of the previous launch is still unwinding, so an
// exiting goroutine must not touch shared per-address state (its health
// registration, its cancel-map entry) unless it is still the current launch;
// otherwise it removes the replacement's state.
var proxyLaunches = struct {
	mu      sync.Mutex
	next    uint64
	current map[string]uint64
}{current: map[string]uint64{}}

type proxyLaunchGenKey struct{}

// beginProxyLaunch starts a new launch generation for addr and returns it.
// Callers hold the cancel-map lock so the generation and the cancel-map entry
// change together.
func beginProxyLaunch(addr string) uint64 {
	proxyLaunches.mu.Lock()
	defer proxyLaunches.mu.Unlock()
	proxyLaunches.next++
	proxyLaunches.current[addr] = proxyLaunches.next
	return proxyLaunches.next
}

func withProxyLaunchGen(ctx context.Context, gen uint64) context.Context {
	return context.WithValue(ctx, proxyLaunchGenKey{}, gen)
}

func proxyLaunchIsCurrent(addr string, gen uint64) bool {
	proxyLaunches.mu.Lock()
	defer proxyLaunches.mu.Unlock()
	return proxyLaunches.current[addr] == gen
}

// unregisterProxyIfCurrent removes the health registration for stableID when
// the exiting launch still owns addr, and releases its generation.
func unregisterProxyIfCurrent(addr string, gen uint64, stableID int) {
	proxyLaunches.mu.Lock()
	owns := proxyLaunches.current[addr] == gen
	if owns {
		delete(proxyLaunches.current, addr)
	}
	proxyLaunches.mu.Unlock()
	if owns {
		connect.UnregisterProxy(stableID)
	}
}

// deleteProxyCancelIfCurrent drops addr from cancelMap on behalf of the
// goroutine that owns ctx, but only while that goroutine's launch is still the
// current one. A contexts without a generation (tests, direct callers) keep
// the unconditional delete.
func deleteProxyCancelIfCurrent(mu *sync.Mutex, cancelMap map[string]context.CancelFunc, ctx context.Context, addr string) {
	mu.Lock()
	defer mu.Unlock()
	if gen, ok := ctx.Value(proxyLaunchGenKey{}).(uint64); ok && !proxyLaunchIsCurrent(addr, gen) {
		return
	}
	delete(cancelMap, addr)
}

// runningAuthFor returns the settings the proxy at addr was launched with,
// or ok=false if it is not running / was started before this tracking existed
// (e.g. initial startup loop populates cancelMap but not runningAuth).
func (r *ProxyReloader) runningAuthFor(addr string) (*connect.ProxySettings, bool) {
	r.cancelMapMu.Lock()
	defer r.cancelMapMu.Unlock()
	if r.runningAuth == nil {
		return nil, false
	}
	s, ok := r.runningAuth[addr]
	return s, ok
}

// seedRunningAuth records the settings the startup loop launched each proxy
// with. It must run before the first reload(): reload() treats a running proxy
// with no recorded auth as "unknown, rotate", so an unseeded first pass would
// cancel and relaunch every boot-launched proxy. The same pointers the running
// goroutines use are stored, so later comparisons see exactly what is dialing.
func (r *ProxyReloader) seedRunningAuth(settings []*connect.ProxySettings) {
	r.cancelMapMu.Lock()
	defer r.cancelMapMu.Unlock()
	if r.runningAuth == nil {
		r.runningAuth = make(map[string]*connect.ProxySettings, len(settings))
	}
	for _, s := range settings {
		// Record a COPY, never the pointer handed to the goroutine: the
		// proxy runtime mutates the launched settings (auth write-back),
		// and reload() must compare against the baseline as CONFIGURED,
		// not as dialed. Sharing the pointer made every credentialed
		// proxy look perpetually rotated.
		r.runningAuth[s.Key()] = cloneProxySettings(s)
	}
}

// cloneProxySettings returns a deep copy of s. The rotation baseline
// (runningAuth) and the launched goroutine must never share the same
// pointer: the runtime mutates the launched settings, which would poison
// the comparison on the next reload.
func cloneProxySettings(s *connect.ProxySettings) *connect.ProxySettings {
	if s == nil {
		return nil
	}
	c := *s
	if s.Auth != nil {
		a := *s.Auth
		c.Auth = &a
	}
	return &c
}

// sameAuth reports whether two proxy settings carry identical credentials
// (both nil auth or identical user+password). Address/network are ignored —
// those are the diff key; only the credentials decide whether a running
// proxy needs a rotation.
func sameAuth(a, b *connect.ProxySettings) bool {
	switch {
	case a == nil || b == nil:
		// A nil-vs-set mismatch means "we don't know what's running" — treat
		// that as a rotation-needed (different) rather than risk keeping
		// stale credentials.
		return a == b
	case a.Auth == nil && b.Auth == nil:
		return true
	case a.Auth == nil || b.Auth == nil:
		return false
	default:
		return a.Auth.User == b.Auth.User && a.Auth.Password == b.Auth.Password
	}
}

func (r *ProxyReloader) isDraining(addr string) bool {
	r.drainMu.Lock()
	defer r.drainMu.Unlock()
	_, ok := r.drainingProxies[addr]
	return ok
}

// markTrimShed records addr as currently held out by the trim cap, so the
// drain-completion goroutine can single it out later (see the trimShed field
// doc).
func (r *ProxyReloader) markTrimShed(addr string) {
	r.drainMu.Lock()
	defer r.drainMu.Unlock()
	if r.trimShed == nil {
		r.trimShed = map[string]bool{}
	}
	r.trimShed[addr] = true
}

// isTrimShed reports whether addr is currently held out by the trim cap.
func (r *ProxyReloader) isTrimShed(addr string) bool {
	r.drainMu.Lock()
	defer r.drainMu.Unlock()
	return r.trimShed[addr]
}

// clearTrimShed drops addr's trim-shed mark: it is being admitted or launched
// again, so it is no longer held out.
func (r *ProxyReloader) clearTrimShed(addr string) {
	r.drainMu.Lock()
	defer r.drainMu.Unlock()
	delete(r.trimShed, addr)
}

// drainProxyAfterRemoval takes over a cancelled proxy's shutdown: it waits for
// the last client to leave, then cancels and re-triggers a reload if the proxy
// is desired again by then. The ordinary removal pass and the empty-source path
// both need exactly this, so they share it rather than drift apart.
//
// A drained proxy that is still desired is normally re-added (credential
// rotation, or a source flap that removed and re-added it while it drained).
// A TRIM-SHED proxy also stays in the desired set on purpose (to keep its
// grade/health state), so IT must not re-trigger here: under a binding cap the
// next reload would only hold it again, and each shed proxy that finished
// draining would burn a reload cycle and log a false "re-added while draining"
// line. Checking THIS address's shed mark (not just "some cap happens to bind")
// matters: a non-shed proxy re-added while draining under a binding cap must
// still re-trigger promptly rather than wait for the reconciler's next tick.
// The next natural reload admits a shed proxy when the cap allows.
func (r *ProxyReloader) drainProxyAfterRemoval(addr string, cancel context.CancelFunc, reason string) {
	r.drainMu.Lock()
	r.drainingProxies[addr] = cancel
	r.drainMu.Unlock()

	bw := connect.ProxyBandwidthByKey(addr)
	tlog("[proxy] draining %s (%d active clients)%s\n", proxyKeyDisplay(addr), bw.Clients.Load(), reason)

	go func() {
		defer func() {
			r.drainMu.Lock()
			delete(r.drainingProxies, addr)
			r.drainMu.Unlock()
		}()
		for {
			bw := connect.ProxyBandwidthByKey(addr)
			if bw == nil || bw.Clients.Load() == 0 {
				break
			}
			select {
			case <-r.parentCtx.Done():
				return
			case <-time.After(drainPollInterval):
			}
		}
		tlog("[proxy] drain complete: %s\n", proxyKeyDisplay(addr))
		cancel()

		desired, err := currentDesiredProxyIdentities()
		if err == nil && desired[addr] {
			if r.isTrimShed(addr) {
				tlog("[proxy] drain complete: %s stays within the trim cap; not re-triggering a reload\n", proxyKeyDisplay(addr))
			} else if reloadPath, err := proxyReloadPath(); err == nil {
				if err := writeReloadTrigger(reloadPath); err == nil {
					tlog("[proxy] re-triggered reload for %s (re-added while draining)\n", proxyKeyDisplay(addr))
				}
			}
		}
	}()
}

// reconciliationReloadInterval is how often runReloadReconciler forces a
// reload cycle regardless of whether anything is known to have changed. This
// is a belt-and-suspenders safety net, not the primary reload path — normal
// add/remove/refresh operations already trigger their own immediate reload.
var reconciliationReloadInterval = time.Hour

// runReloadReconciler periodically writes a reload trigger so reload() runs
// on a fixed cadence even if nothing explicitly requested one. Root cause: a
// mass-failure event (e.g. a transient backend outage) can leave a batch of
// still-desired proxies absent from the running set with no future event
// scheduled to bring them back — reload() only ever runs on an explicit
// trigger (add-source, remove-dead, proxy refresh, URL fetch merge, reaper
// change), so if none of those happen to fire afterward, the gap persists
// indefinitely. Confirmed on a live fleet node: ~3300 proxies sat
// recently_offline for ~20+ hours after a single API blip, and all recovered
// instantly the moment an unrelated add-source call forced a reload.
// This is deliberately cheap to run even when nothing is wrong: if running
// already matches desired, reload() logs "+0 added, -0 removed" and returns.
func runReloadReconciler(ctx context.Context) {
	reloadPath, err := proxyReloadPath()
	if err != nil {
		tlog("[proxy] warning: could not determine reload path for reconciler: %v\n", err)
		return
	}

	ticker := time.NewTicker(reconciliationReloadInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := writeReloadTrigger(reloadPath); err != nil {
				tlog("[proxy] warn: reconciliation reload trigger write failed: %v\n", err)
			}
		}
	}
}

// StartWatcher launches the background goroutine that polls the reload trigger
// file every 2 seconds and triggers reload() when its sequence number changes.
func (r *ProxyReloader) StartWatcher(ctx context.Context) {
	reloadPath, err := proxyReloadPath()
	if err != nil {
		tlog("[proxy] warning: could not determine reload path: %v\n", err)
		return
	}

	lastSeq, _ := readReloadSeq(reloadPath)

	go connect.HandleError(func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				seq, err := readReloadSeq(reloadPath)
				if err != nil {
					tlog("[proxy] warn: reload trigger read failed: %v\n", err)
					continue
				}
				if seq == lastSeq {
					continue
				}
				tlog("🔄 [proxy] Reload requested (trigger #%d, was #%d)\n", seq, lastSeq)
				r.reload()
				// Record the sequence only after the reload. A reload that
				// could not take the slot is retried on a later tick
				// instead of dropping its trigger; every other outcome
				// (ran, skipped for a source error, aborted) is a decision,
				// not a lost notification.
				if !r.reloadSlotSkipped.Load() {
					lastSeq = seq
				}
			}
		}
	})
}

// acquireReloadSlot takes r.mu, waiting up to reloadSlotTimeout for an
// in-flight reload to finish. It returns false when the slot never freed (or
// the parent context cancelled), so the caller can skip instead of blocking
// forever.
func (r *ProxyReloader) acquireReloadSlot() bool {
	deadline := time.Now().Add(reloadSlotTimeout)
	// One reused timer: time.After in the loop would schedule a fresh timer
	// on every poll tick.
	poll := time.NewTimer(reloadSlotPollInterval)
	defer poll.Stop()
	for {
		if r.mu.TryLock() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		// Shutdown must never wait out the slot timeout: the parent context
		// cancels on SIGTERM and on hotswap drain, and a reload that cannot
		// take the slot has nothing to do anyway.
		if r.parentCtx != nil {
			select {
			case <-r.parentCtx.Done():
				return false
			case <-poll.C:
			}
			poll.Reset(reloadSlotPollInterval)
		} else {
			time.Sleep(reloadSlotPollInterval)
		}
	}
}

// RunReloadWatchdog watches for a reload holding the slot past
// reloadHardLimit and escalates: a loud record (ramlog + events.log) and a
// hot-restart request through reloadOverdueAction (gated on hot_restart).
// This is the backstop for the case the reload body cannot handle itself —
// a goroutine stuck inside a call that never returns. Without it, one stuck
// reload stalls every future trigger and every proxy-pool maintenance path
// for as long as the process lives.
func (r *ProxyReloader) RunReloadWatchdog(ctx context.Context) {
	ticker := time.NewTicker(reloadWatchdogInterval)
	defer ticker.Stop()
	var lastFired time.Time
	var episodeStart int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !r.reloadActive.Load() {
			episodeStart = 0
			continue
		}
		started := r.reloadStartedAt.Load()
		if started <= 0 {
			continue
		}
		held := time.Since(time.Unix(0, started))
		if held < reloadHardLimit {
			continue
		}
		// Fire once per episode, then at most once per reloadWatchdogReFire
		// while the same reload stays overdue. The hotswap decline ledger
		// throttles repeats further.
		if episodeStart == started && time.Since(lastFired) < reloadWatchdogReFire {
			continue
		}
		episodeStart = started
		lastFired = time.Now()
		// tlog is a cheap in-memory write and goes out immediately; the
		// fsyncing critLog record is written inside the action goroutine so
		// a thrashing disk or a busy critLogMu cannot stall this ticker
		// loop — the very escalation it exists to fire.
		tlog("🚨 [proxy] reload watchdog: reload overdue %v — requesting hot restart\n", held.Round(time.Second))
		// Single-flight: a hot-swap handoff can take minutes, and stacking a
		// fresh goroutine on every re-fire would leak them if it ever hung.
		if r.watchdogActionInFlight.CompareAndSwap(false, true) {
			go connect.HandleError(func() {
				defer r.watchdogActionInFlight.Store(false)
				critLog("%s", fmt.Sprintf("[proxy] reload watchdog: reload overdue %v (hard limit %v) — requesting hot restart",
					held.Round(time.Second), reloadHardLimit))
				if err := reloadOverdueAction(); err != nil {
					critLog("%s", fmt.Sprintf("[proxy] reload watchdog: hot restart unavailable: %v — operator action needed (urnet-tools restart)", err))
					tlog("🚨 [proxy] reload watchdog: hot restart unavailable: %v — operator action needed (urnet-tools restart)\n", err)
				} else {
					critLog("%s", "[proxy] reload watchdog: hot restart requested")
					tlog("🚨 [proxy] reload watchdog: hot restart requested\n")
				}
			})
		} else {
			tlog("🚨 [proxy] reload watchdog: a hot-restart request is already in flight\n")
		}
	}
}

// reload diffs the proxy source against the currently running set and applies
// the difference: cancels goroutines for removed proxies, starts goroutines for
// added proxies (staggered), and rewrites proxy.state. Untouched proxies are
// never disturbed.
func (r *ProxyReloader) reload() {
	reloadStart := time.Now()

	// Important lines are printed as they happen, but their events.log copy is
	// an open+write+fsync, so it is held back and written after r.mu is
	// released: this defer is registered BEFORE the Unlock defer, so it runs
	// after it. A slow or failing disk then stalls only this goroutine's log
	// tail, not every other reload-path caller waiting on r.mu.
	var pendingCrit []func()
	// One serialised writer for the whole post-unlock batch: the queued
	// warnings and the pending writes go out together, in reload order, so a
	// second reload cannot slip its cap change in before this one's.
	defer func() {
		writes := pendingCrit
		drainDeferredCritFn(func(lines []string) {
			for _, line := range lines {
				critLog("%s", line)
			}
			for _, write := range writes {
				write()
			}
		})
	}()
	// critLog appends its own newline, so the format must NOT carry one or every
	// line leaves a blank line behind it in events.log. tlog needs its newline,
	// so the immediate path adds it back.
	logImportant := func(format string, args ...any) {
		tlog(format+"\n", args...)
		pendingCrit = append(pendingCrit, func() { critLog("%s", fmt.Sprintf(format, args...)) })
	}

	r.reloadSlotSkipped.Store(false)
	if !r.acquireReloadSlot() {
		if r.parentCtx != nil && r.parentCtx.Err() != nil {
			// Shutdown, not contention: no retry is wanted and no alarm is
			// due.
			tlog("[proxy] reload cancelled: shutting down\n")
			return
		}
		r.reloadSlotSkipped.Store(true)
		held := "unknown"
		if started := r.reloadStartedAt.Load(); started > 0 {
			held = time.Since(time.Unix(0, started)).Round(time.Second).String()
		}
		tlog("[proxy] reload skipped: a previous reload has held the slot for %s (slot wait limit %v) — RunReloadWatchdog escalates if it is stuck\n", held, reloadSlotTimeout)
		return
	}
	defer r.mu.Unlock()
	// Publish the timestamp BEFORE the active flag: whenever the watchdog
	// sees active=true, startedAt already belongs to this reload. Clear both
	// before the slot is released (defers run LIFO) so a stale startedAt
	// can never survive into the next window.
	r.reloadStartedAt.Store(time.Now().UnixNano())
	r.reloadActive.Store(true)
	defer func() {
		r.reloadActive.Store(false)
		r.reloadStartedAt.Store(0)
	}()
	slotAcquiredAt := time.Now()

	// overdue reports when this reload has run past reloadHardLimit so the
	// caller can abort at the next phase boundary; the watchdog is the hard
	// backstop for a goroutine that never reaches another check. ("start"
	// is effectively a zero-limit test hook; "planning" guards the mutation
	// section before any trim, removal or launch is staged.)
	overdue := func(phase string) bool {
		if time.Since(slotAcquiredAt) < reloadHardLimit {
			return false
		}
		// Mark the trigger un-consumed: an aborted reload did none of its
		// work, and without this the change would wait for the next trigger
		// (up to the hourly reconciler) instead of being retried on a later
		// watch tick.
		r.reloadSlotSkipped.Store(true)
		tlog("🚨 [proxy] reload aborted at %s after %v (hard limit %v): state left as-is, retried on a later tick\n",
			phase, time.Since(slotAcquiredAt).Round(time.Second), reloadHardLimit)
		return true
	}
	if overdue("start") {
		return
	}

	lockAcqStart := time.Now()
	lockRelease, err := acquireProxyLockWithRetry()
	if err != nil {
		// Transient cross-process contention (a CLI or reaper holding the
		// file lock): mark the trigger un-consumed so the watcher retries
		// it on a later tick instead of dropping the change.
		r.reloadSlotSkipped.Store(true)
		tlog("[proxy] reload skipped: %v (waited %v)\n", err, time.Since(reloadStart).Round(time.Millisecond))
		return
	}
	defer lockRelease()
	lockWait := time.Since(lockAcqStart)

	proxyStateMu.Lock()
	if newState, err := readProxyState(); err == nil {
		r.state = newState
	}
	proxyStateMu.Unlock()

	// Load desired set from the source. On a read error in Workflow A, SKIP the
	// reload entirely — proceeding would diff against zero proxies and cancel the
	// entire running fleet over a transient file error.
	var desired []*connect.ProxySettings
	if r.sourcePath != "" {
		settings, err := readProxySettingsFromFile(r.sourcePath)
		if err != nil {
			tlog("[proxy] reload skipped: could not read source: %v\n", err)
			// Record the failure before returning. Without this the resolution
			// stays pending, so the status line reads "starting: resolving
			// proxies" instead of a source failure, and the snapshot's startup
			// reason later reads as stuck rather than as an unreadable source.
			// The running proxies are deliberately left alone.
			setProxyResolutionStatus(proxyResolutionFailed, fmt.Sprintf("could not read %s: %v", r.sourcePath, err))
			return
		}
		desired = settings
	} else {
		desired = readProxySettings()
	}

	// desiredSet is keyed by proxy IDENTITY (ProxySettings.Key(): address,
	// or address+user for a shared-gateway proxy where the same host:port
	// serves multiple accounts — Decodo's model), not bare address. Keying
	// by address alone let two different accounts at the same gateway
	// address collide: Go's randomized map iteration order over the raw
	// config picked a different "winner" on every reload, which looked
	// like a credential change and triggered a perpetual rotate-forever
	// loop against the real backend (incident 2026-09-23). Every map and
	// set derived from desiredSet below (running, cancelMap, runningAuth,
	// drainingProxies, rotatedSet, removedSet, the launch-generation
	// tracker) must stay keyed the same way for the same reason.
	desiredSet := make(map[string]*connect.ProxySettings, len(desired))
	sourceOf := make(map[string]string, len(desired))
	primarySource := "internal"
	if r.sourcePath != "" {
		primarySource = "file"
	}
	for _, s := range desired {
		desiredSet[s.Key()] = s
		sourceOf[s.Key()] = primarySource
	}

	urlCacheLoaded := true
	anySourceConfigured := r.sourcePath != ""
	if urlState, err := readProxyURLState(); err != nil {
		tlog("[proxy][url] warning: could not read proxy_url.json: %v\n", err)
		urlCacheLoaded = false
	} else {
		if len(urlState.Sources) > 0 {
			anySourceConfigured = true
		}
		mergeProxyURLCache(desiredSet, sourceOf, urlState)
	}

	// Migrate any legacy (pre-identity, bare-address-keyed) entries in the
	// persisted stores to their new identity key, for every identity the
	// current desired set actually claims. MUST run before anything below
	// reads or prunes r.state.Proxies against desiredSet (the final prune
	// pass, and resolveProxyID/tagProxySourceIfUnset in the add loop) —
	// otherwise a not-yet-adopted legacy entry looks like "no longer
	// desired" under the new identity-keyed desiredSet and gets silently
	// deleted instead of adopted. Idempotent: safe every cycle.
	desiredValues := make([]*connect.ProxySettings, 0, len(desiredSet))
	for _, s := range desiredSet {
		desiredValues = append(desiredValues, s)
	}
	adoptLegacyProxyState(r.state, desiredValues)
	if globalClientJWTStore != nil {
		globalClientJWTStore.AdoptLegacy(desiredValues)
	}
	if globalProxyEarningsStore != nil {
		globalProxyEarningsStore.adoptLegacy(desiredValues)
	}
	globalProxySlowRetryState.adoptLegacy(desiredValues)

	// Lock ordering: r.mu (held by caller) is always acquired before r.cancelMapMu.
	// provide()'s initial startup loop writes the cancel map before StartWatcher is called,
	// so it is exempt from this ordering — no concurrent reload() can run at that point.
	// Snapshot the currently running set from the cancel map.
	r.cancelMapMu.Lock()
	running := make(map[string]bool, len(r.cancelMap))
	for addr := range r.cancelMap {
		running[addr] = true
	}
	r.cancelMapMu.Unlock()

	// Hot-toggle the native [direct] transport based on the runtime
	// toggle (~/.urnetwork/direct). `provider direct off|on` writes the file
	// and triggers a reload; this block applies the change to the running set.
	directRunning := running[directProxyKey]
	// Same precedence as startup: toggle file wins, then env var.
	directShouldRun := true
	if directOn, fileExists := readDirectOverride(); fileExists {
		directShouldRun = directOn
	} else {
		directShouldRun = os.Getenv("DISABLE_DIRECT_IP") != "1"
	}
	if directRunning && !directShouldRun {
		// Disable direct: cancel the goroutine and wait for it to exit
		// with a bounded timeout so we don't wedge the reload path.
		r.cancelMapMu.Lock()
		if cancel, ok := r.cancelMap[directProxyKey]; ok {
			cancel()
		}
		r.cancelMapMu.Unlock()
		// Snapshot directDone before the select to avoid reading a stale field
		// if another reload replaces it between now and the goroutine exit.
		done := r.directDone
		if done != nil {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				tlog("[direct] warn: goroutine exit timed out after 10s, proceeding\n")
			}
		}
		connect.UnregisterProxy(0)
		tlog("[direct] native [direct] transport stopped (disable)\n")
	} else if !directRunning && directShouldRun {
		// Enable direct: start the goroutine.
		r.wg.Add(1)
		done := make(chan struct{}) // local — goroutine closes this, not the field
		r.directDone = done
		directCtx, directCancel := context.WithCancel(r.parentCtx)
		r.cancelMapMu.Lock()
		r.cancelMap[directProxyKey] = directCancel
		r.cancelMapMu.Unlock()
		go connect.HandleError(func() {
			defer close(done) // first defer = runs last (LIFO); closes LOCAL, not field
			defer r.wg.Done()
			defer directCancel()
			// Delete cancelMap AFTER UnregisterProxy so stale-unregister
			// can't nuke a fresh direct's registration.
			defer func() {
				r.cancelMapMu.Lock()
				delete(r.cancelMap, directProxyKey)
				r.cancelMapMu.Unlock()
			}()
			defer connect.UnregisterProxy(0)
			connect.RegisterProxy(0, "direct", "direct")
			r.spawnProxy(directCtx, nil, true, false)
		})
		tlog("[direct] native [direct] transport started (enable)\n")
	}

	// Check emptiness AFTER merging the URL cache — a URL-only deployment
	// (no --proxy_file, no internal proxies) has desired == 0 but a
	// non-empty desiredSet, and must not be treated as a source-read error.
	if len(desiredSet) == 0 {
		// A node that deliberately provides via the direct transport with no
		// proxy source configured is a valid, completed zero-proxy config — it
		// settles, it is not degraded. Only reserve "empty" for the case where
		// a proxy source WAS configured but returned no usable proxies.
		if anySourceConfigured {
			tlog("[proxy] reload skipped: 0 proxies found in source\n")
			setProxyResolutionStatus(proxyResolutionEmpty, "source returned no usable proxies")
		} else if !urlCacheLoaded {
			// proxy_url.json exists but could not be read (a missing file reads as
			// empty, not as an error), so whether URL sources are configured is
			// unknown. Settling as direct-only would report a healthy node whose
			// sources were never resolved.
			tlog("[proxy] reload skipped: proxy_url.json unreadable, URL sources unknown\n")
			setProxyResolutionStatus(proxyResolutionFailed, "could not read proxy_url.json")
		} else if !directShouldRun {
			// Direct transport is turned off and no proxy source is configured,
			// so nothing is being provided. It is a valid config but not a
			// completed zero-proxy setup that is actively serving, so it reads
			// as degraded rather than as a healthy direct-only node.
			tlog("[proxy] reload skipped: direct transport disabled and no proxy source\n")
			setProxyResolutionStatus(proxyResolutionNoSource, "direct transport disabled and no proxy source configured")
		} else {
			tlog("[proxy] reload: 0 proxies; direct-only (no proxy source configured) — settled\n")
			setProxyResolutionStatus(proxyResolutionZeroValid, "direct-only providing; no proxy source configured")
		}
		// A source that was configured and has now gone empty is NOT a reason to
		// keep dialling the proxies it used to supply. The full diff below is
		// skipped because there is nothing to add, so the removal half has to
		// happen here: without it the old proxies keep running and the stale
		// positive configured count makes the status line and the startup
		// phase ignore the resolution just recorded.
		//
		// The unreadable proxy_url.json case is exempt: there the sources are
		// UNKNOWN rather than empty, so a transient read error must not cancel
		// a working fleet.
		// A source that was configured and has now gone empty is NOT a reason to
		// keep dialling the proxies it used to supply. The full diff below is
		// skipped because there is nothing to add, so the removal half has to
		// happen here: without it the old proxies keep running and the stale
		// positive configured count makes the status line and the startup
		// phase ignore the resolution just recorded.
		//
		// An unreadable proxy_url.json does not make the desired set empty, it
		// makes it UNKNOWN, so the proxies whose desired state is unknown —
		// URL-sourced, or with no recorded source — are left running, exactly
		// as the removal pass below leaves them. A blanket skip instead would
		// keep a file proxy the operator just deleted dialling for as long as
		// the cache stayed unreadable.
		stopped, draining := 0, 0
		for addr := range running {
			if addr == directProxyKey {
				continue // managed by the direct hot-toggle block above
			}
			if r.isDraining(addr) {
				continue
			}
			if !urlCacheLoaded {
				if e, ok := r.state.Proxies[addr]; !ok || e.Source == "" || e.Source == "url" {
					continue
				}
			}
			r.cancelMapMu.Lock()
			cancel, ok := r.cancelMap[addr]
			if ok {
				delete(r.cancelMap, addr)
			}
			r.cancelMapMu.Unlock()
			if !ok {
				continue
			}
			delete(r.state.Proxies, addr)
			r.cancelMapMu.Lock()
			delete(r.runningAuth, addr)
			r.cancelMapMu.Unlock()

			// Drain rather than hard-cancel, exactly as the removal pass below
			// does: a proxy with live clients must not have them cut
			// mid-session. No clients stops now; otherwise the cancel waits in
			// a drain goroutine until the last one leaves.
			bw := connect.ProxyBandwidthByKey(addr)
			if bw == nil || bw.Clients.Load() == 0 {
				cancel()
				stopped++
				continue
			}
			r.drainProxyAfterRemoval(addr, cancel, ": source went empty")
			draining++
		}
		if stopped > 0 || draining > 0 {
			tlog("[proxy] reload: source empty, stopped %d running prox(ies), draining %d with active clients\n", stopped, draining)
		}
		// Reconcile and persist, not just the running set. A dead or offline
		// proxy's goroutine has already exited, so it was never in `running`
		// and the loop above never saw it — without this its state entry would
		// survive in proxy.state forever.
		pruned := 0
		for addr := range r.state.Proxies {
			if _, ok := desiredSet[addr]; ok {
				continue
			}
			if !urlCacheLoaded {
				if e := r.state.Proxies[addr]; e.Source == "" || e.Source == "url" {
					continue
				}
			}
			delete(r.state.Proxies, addr)
			pruned++
		}
		if pruned > 0 {
			tlog("[proxy] pruned %d stale proxy.state entries (source empty)\n", pruned)
		}
		proxyStateMu.Lock()
		if diskState, err := readProxyState(); err == nil {
			for addr, entry := range r.state.Proxies {
				if diskEntry, ok := diskState.Proxies[addr]; ok {
					entry.Health = diskEntry.Health
					entry.DownSince = diskEntry.DownSince
					entry.AuthFailures = diskEntry.AuthFailures
					r.state.Proxies[addr] = entry
				}
			}
		}
		r.state.NextID = currentProxyIDCounter()
		if err := writeProxyState(r.state); err != nil {
			tlog("[proxy] warning: could not write proxy.state after reload: %v\n", err)
		}
		proxyStateMu.Unlock()

		// The configured count reflects the desired set, which is empty. Only
		// when the URL cache was readable: an unreadable one means the URL
		// sources are UNKNOWN, not empty, so the count they contributed stays.
		// This must still be set when nothing was running, or a node that never
		// had proxies would keep a stale count.
		if urlCacheLoaded {
			setConfiguredProxyCount(0)
		}
		return
	}

	// The mid-reload counters fold into the "Proxy list reloaded" summary at
	// the end of this function (readability contract); kept as variables so
	// the plan/apply split is unchanged. The direct transport lives in the
	// running map but is not a proxy, so the count excludes it, like the
	// trim receipt's. The trim section below reuses this count instead of
	// re-walking the map.
	reloadRunning := 0
	for addr := range running {
		if addr != directProxyKey {
			reloadRunning++
		}
	}
	reloadDesired := len(desiredSet)
	reloadLockWait := lockWait.Round(time.Millisecond)

	var added []*connect.ProxySettings
	deferredBackoff := 0
	now := time.Now()
	var rotated []string // addresses whose credentials changed while running
	for addr, s := range desiredSet {
		if running[addr] {
			// Credential rotation: the desired settings carry different
			// auth than the proxy currently running. The config changed
			// (re-paste with new credentials) but the address is already
			// in the cancel map, so the plain diff would silently keep
			// the old credentials. Rotate by cancelling the running
			// proxy (via the removed set) and relaunching it in the same
			// pass with the new auth.
			if previous, ok := r.runningAuthFor(addr); !ok || !sameAuth(previous, s) {
				// Add it to BOTH sets: the removal pass cancels the running
				// goroutine (old credentials) and the add pass relaunches it
				// with the new auth — one reload, minimal gap.
				rotated = append(rotated, addr)
				added = append(added, cloneProxySettings(s))
				tlog("[proxy] rotating credentials for %s\n", proxyKeyDisplay(addr))
				continue
			}
			continue
		}
		// Enforce the URL give-up backoff at launch time: an address whose
		// next-eligible time has not yet arrived is skipped, not relaunched.
		// URL-sourced proxies carry a give-up backoff, and proxy audit
		// puts one on a paid or file proxy it parks. Other file and internal
		// proxies never get one and are always eligible here. Without this,
		// the backoff was defeated because any reload would relaunch every
		// desired-but-not-running proxy immediately.
		if !globalProxyFailureHistory.Eligible(addr, now) {
			deferredBackoff++
			continue
		}
		added = append(added, s)
	}
	var removed []string
	// The rotated set is appended here: its addresses were already placed in
	// `added` above (so they relaunch with the new auth in this same pass),
	// and folding them into `removed` is what cancels the OLD goroutine.
	rotatedSet := make(map[string]bool, len(rotated))
	for _, addr := range rotated {
		rotatedSet[addr] = true
		removed = append(removed, addr)
	}
	for addr := range running {
		if addr == directProxyKey {
			continue // managed by the direct hot-toggle block above, not the proxy diff
		}
		if _, ok := desiredSet[addr]; !ok {
			// When proxy_url.json could not be read, desiredSet never got the
			// URL cache merged in (see the urlCacheLoaded gate above), so EVERY
			// running URL proxy looks absent here and this loop cancels the
			// entire live URL pool off one transient read error. The state
			// prune further down is gated on urlCacheLoaded for exactly this
			// reason; the cancel has to be too. A proxy is only "no longer
			// desired" when we actually know what the desired set is.
			//
			// Only URL-sourced proxies depend on that cache. A file or internal
			// proxy is "no longer desired" because the operator removed it from a
			// source this reload DID read, so an unreadable proxy_url.json must
			// not hold it up: with the gate on every address, `proxy remove` was
			// silently never applied for as long as the file stayed unreadable. An
			// entry with no recorded source is treated as URL-sourced (the
			// conservative reading for state written before sources were recorded).
			if !urlCacheLoaded {
				if e, ok := r.state.Proxies[addr]; !ok || e.Source == "" || e.Source == "url" {
					continue
				}
			}
			removed = append(removed, addr)
		}
	}

	// Addresses shed by the trim cap below. Their state entry (ID, health,
	// downtime, grade) must survive the removal loop: the shed is a capacity
	// decision, not a verdict on the proxy, and dropping the entry would make a
	// later relaunch allocate a new ID and rank the proxy as ungraded.
	trimShedSet := map[string]bool{}

	// Abort BEFORE the fleet-mutation section if this reload has run too
	// long: everything above is reads, idempotent store adoption and the
	// planning itself, so the running fleet is untouched — no trim, removal
	// or launch is staged, and no audit or events.log record can describe
	// work that will not happen. (The direct hot-toggle above is idempotent
	// and safe to have run.)
	if overdue("planning") {
		return
	}

	// Operator trim cap (provider proxy trim <N>): hold the running pool at N.
	// Shed the A-F-worst running proxies above N (folded into removed so they are
	// cancelled), and drop the worst-graded not-yet-running additions above the
	// budget so the pool cannot regrow above the cap until it is raised.
	trimCapNow, trimSource, trimErr := effectiveTrimCapSource()
	// The direct transport is in the running map but is never trimmed, so the
	// counts reported below exclude it, like the cap does.
	runningProxies := reloadRunning
	autoNote := ""
	switch trimSource {
	case trimCapOOM:
		autoNote = " (automatic OOM cap)"
	case trimCapThrash:
		autoNote = " (automatic thrash cap: memory was thrashing on the last run)"
	}
	trimChanged := false
	if trimErr == nil {
		// Acknowledge a new or cleared operator cap once, so the log shows the
		// command was received before (and regardless of) what it sheds.
		var prevCap int
		var prevSource string
		if prevCap, prevSource, trimChanged = noteTrimCap(trimCapNow, trimSource); trimChanged {
			if trimCapNow > 0 {
				prev := "none"
				if prevCap > 0 {
					prev = strconv.Itoa(prevCap)
				}
				logImportant("[proxy][trim] received: limiting this provider to %d running proxies (was %s); %d running now, %d desired, applying%s (cap=%d was=%s running=%d desired=%d)", trimCapNow, prev, runningProxies, len(desiredSet), autoNote, trimCapNow, prev, runningProxies, len(desiredSet))
			} else {
				// The cap that just cleared may have been the automatic OOM cap
				// relaxing to zero, not an operator command: attribute the
				// ledger entry to whichever source actually bound before, so an
				// OOM-driven clear is not mislabeled "operator".
				clearedMode := prevSource
				if clearedMode == "" {
					clearedMode = trimCapOperator
				}
				logImportant("[proxy][trim] received: cap cleared (was %d); pool may regrow toward %d desired", prevCap, len(desiredSet))
				pendingCrit = append(pendingCrit, func() {
					ledgerRecord(ledgerEntry{Actor: "trim", Action: "cleared", From: prevCap, To: 0, Mode: clearedMode,
						Reason: fmt.Sprintf("pool may regrow toward %d desired", len(desiredSet))})
				})
			}
		}
	}
	if trimCap := trimCapNow; trimErr == nil && trimCap > 0 {
		traffic := runningProxyEarnings()
		// Read the URL cache here: the urlState read earlier is scoped to its own
		// if/else and is not visible in this hook.
		trimURLState, _ := readProxyURLState()
		gradeFor := buildTrimGradeResolver(r.state, trimURLState)
		// Union count of everything about to be cancelled, so the addition
		// budget is not under-sized by double-counting.
		removedSet := make(map[string]bool, len(removed))
		for _, a := range removed {
			removedSet[a] = true
		}
		shedCount := 0
		// runningNonDirect excludes direct (managed by the hot-toggle block
		// above, not the trim logic). Including it would cause direct to be
		// shed as the worst-graded proxy on every reload when the cap binds,
		// creating a restart flap. The count was computed once above as
		// reloadRunning; this loop only builds the shed list.
		runningNonDirect := reloadRunning
		rlist := make([]string, 0, len(running))
		for a := range running {
			if a == directProxyKey {
				continue
			}
			if !removedSet[a] {
				rlist = append(rlist, a)
			}
		}
		if runningNonDirect > trimCap {
			// trimCap applies to non-direct proxies only.
			for _, addr := range selectWorstRunningProxies(r.state.Proxies, gradeFor, traffic, rlist, runningNonDirect-trimCap) {
				if _, ok := running[addr]; ok && !removedSet[addr] {
					removed = append(removed, addr)
					removedSet[addr] = true
					trimShedSet[addr] = true
					r.markTrimShed(addr)
					shedCount++
					// Do NOT delete from desiredSet: pruning against a trim-mutated
					// set erases grade/health history. Mark a short
					// give-up backoff instead so the launch gate keeps it down and
					// it does not relaunch next cycle.
					applyShedBackoff(addr, time.Now())
				}
			}
		}
		budget := trimCap - (runningNonDirect - len(removedSet))
		if budget < 0 {
			budget = 0
		}
		dropped := 0
		if len(added) > budget {
			alist := make([]string, 0, len(added))
			for _, s := range added {
				alist = append(alist, s.Key())
			}
			drop := selectWorstRunningProxies(r.state.Proxies, gradeFor, traffic, alist, len(added)-budget)
			dropSet := make(map[string]bool, len(drop))
			for _, a := range drop {
				dropSet[a] = true
			}
			kept := added[:0]
			for _, s := range added {
				if dropSet[s.Key()] {
					// Deferred, not undesired: leave it in desiredSet so the
					// prune pass keeps this proxy's grade/health history.
					// It re-enters the budget next cycle.
					dropped++
					continue
				}
				kept = append(kept, s)
			}
			added = kept
		}
		// The durable line is for a change: a new cap, or running proxies shed.
		// Held additions alone repeat on every reload for as long as desired
		// exceeds the cap (startup-held proxies carry no backoff and re-enter
		// the budget each cycle), so they stay on the RAM log, or the important
		// buffer and the fsynced events.log fill with a line that carries no news.
		if shedCount > 0 || trimChanged {
			logImportant("[proxy][trim] applied: the running cap is now %d — removed %d lowest-graded running proxies, holding %d additions so the pool stays under the cap. (cap=%d shed=%d held=%d pool~%d)", trimCap, shedCount, dropped, trimCap, shedCount, dropped, runningNonDirect-shedCount)
		} else if dropped > 0 {
			tlog("[proxy][trim] applied: the running cap is now %d — nothing to remove; holding %d additions so the pool stays under the cap. (cap=%d shed=0 held=%d pool~%d)\n", trimCap, dropped, trimCap, dropped, runningNonDirect)
		}
		if trimChanged {
			pendingCrit = append(pendingCrit, func() {
				ledgerRecord(ledgerEntry{Actor: "trim", Action: "applied", From: runningProxies, To: trimCap, Mode: trimSource,
					Reason: fmt.Sprintf("shed %d worst-graded running, held %d additions", shedCount, dropped)})
			})
		}
	}

	// Remove proxies: cancel immediately if idle, or drain gracefully if active.
	for _, addr := range removed {
		if r.isDraining(addr) {
			continue
		}
		r.cancelMapMu.Lock()
		cancel, ok := r.cancelMap[addr]
		if ok {
			delete(r.cancelMap, addr)
		}
		r.cancelMapMu.Unlock()
		if !ok {
			continue
		}
		// Keep the state entry of a rotated proxy: it is relaunched in this same
		// pass, and dropping it would make the relaunch allocate a new ID and
		// lose its persisted health, downtime and grading history.
		if !rotatedSet[addr] && !trimShedSet[addr] {
			delete(r.state.Proxies, addr)
		}
		// The goroutine for this address has now been cancelled; drop its
		// recorded auth so a credential change is picked up on relaunch.
		r.cancelMapMu.Lock()
		delete(r.runningAuth, addr)
		r.cancelMapMu.Unlock()

		bw := connect.ProxyBandwidthByKey(addr)
		// A rotated proxy is never drained: its old credentials are being
		// replaced (usually because they are dead or revoked), the launch pass
		// skips addresses that are still draining, and the drain loop has no
		// deadline. Draining would keep the old credentials serving until the
		// last client leaves, i.e. the rotation would not take effect.
		if rotatedSet[addr] || bw == nil || bw.Clients.Load() == 0 {
			cancel()
			continue
		}

		r.drainProxyAfterRemoval(addr, cancel, "")
	}

	// Note: if all running proxies enter draining state and none are added, the
	// WaitGroup in provide() stays non-zero until all drains complete and their
	// goroutines exit. The process remains alive to avoid interrupting active
	// sessions. This is intentional — draining proxies keep serving traffic
	// until the last session finishes.

	// Start added proxies. Each goroutine staggers its own startup using the
	// same jittered backoffPacer as the initial startup path (main.go), so a
	// large batch added at once (e.g. hundreds of proxies merged in from a
	// URL source) ramps up exactly as slowly as it would on a fresh start,
	// instead of bursting the auth API. Skip any still draining from a
	// previous removal.
	// Note: reload() deliberately does NOT enumerate each added proxy. On a
	// fleet of thousands of (often churning) proxies that per-proxy dump was a
	// dominant ramlog flooder, flushing high-value lines out of the small
	// in-RAM buffer within seconds. The "[proxy] reloaded: +N -M" summary
	// below carries the operator-relevant signal.
	// Prioritize added proxies by warmth (cached client JWTs) so warm proxies ramp up
	// at 25ms intervals while cold proxies use standard backoff.
	addedSchedules, _, _, _ := prioritizeAndScheduleProxies(added, sourceOf, r.networkID)
	warmupDeferred := 0
	// Count URL-sourced proxies that will actually be launched, inside the
	// loop so a draining proxy (skipped before launch) is not counted as
	// scheduled. urlLaunchLine subtracts warmupDeferred, so warmup-deferred
	// entries stay in the total.
	urlAdded := 0
	for _, sched := range addedSchedules {
		settings := sched.Settings
		// key is this proxy's identity (address, or address+user for a
		// shared-gateway proxy) — every per-proxy map below is keyed by
		// this, never by settings.Address alone. See reload()'s desiredSet
		// comment for why.
		key := settings.Key()
		if r.isDraining(key) {
			tlog("[proxy] skip add %s: still draining\n", proxyKeyDisplay(key))
			continue
		}
		// This address is being admitted or launched again (the cap raised
		// enough to let it back in, or it was never trim-shed): it is no
		// longer held out, so the next drain of it (if any) must re-trigger
		// normally instead of being mistaken for a still-standing shed.
		r.clearTrimShed(key)
		if sourceOf[key] == "url" {
			urlAdded++
		}
		// Defer unproven URL-sourced proxy launches until file-proxy warmup
		// completes, so operator-curated proxies get an uncontested ramp.
		// Promoted URL proxies (earnings >= 64 MiB) are known earners and
		// should launch with the file list rather than be deferred.
		isPromoted := proxyEarningsScore(key, time.Now()) >= earningsPromotionBytes
		if sourceOf[key] == "url" && !isPromoted && !proxyWarmupDone.Load() {
			warmupDeferred++
			continue
		}
		stableID := resolveProxyID(r.state, key)
		settings.Index = stableID
		tagProxySourceIfUnset(r.state, key, sourceOf[key])
		proxyCtx, proxyCancel := context.WithCancel(r.parentCtx)
		r.cancelMapMu.Lock()
		r.cancelMap[key] = proxyCancel
		launchGen := beginProxyLaunch(key)
		proxyCtx = withProxyLaunchGen(proxyCtx, launchGen)
		// Record the settings this proxy launched with, so a later reload can
		// see when its credentials changed and rotate it (see the rotation
		// branch in reload()).
		if r.runningAuth == nil {
			r.runningAuth = make(map[string]*connect.ProxySettings)
		}
		r.runningAuth[key] = cloneProxySettings(settings)
		r.cancelMapMu.Unlock()
		// Register AFTER the generation bump: a superseded goroutine that is
		// still unwinding sees it is no longer current and leaves this
		// registration alone. Registering first left a window in which the
		// old goroutine still looked current and unregistered the entry we
		// had just created. address stays the literal dial target for
		// operator display; key is the real identity two accounts sharing
		// a gateway address must never share.
		connect.RegisterProxy(stableID, settings.Address, key)

		settingsCopy := settings
		isURLSourced := sourceOf[key] == "url"
		baseDelay := sched.Delay
		staggerDuration := sched.Stagger
		r.wg.Add(1)
		go connect.HandleError(func() {
			defer r.wg.Done()
			defer unregisterProxyIfCurrent(key, launchGen, stableID)
			defer proxyCancel()

			if !backoffPacerWithDelay(baseDelay, staggerDuration, proxyCtx) {
				return
			}
			r.spawnProxy(proxyCtx, settingsCopy, false, isURLSourced)
		})
	}

	// Reconcile state.Proxies against the full desired set (not just the
	// running-but-undesired diff handled above). A dead/offline proxy's
	// goroutine has already exited by the time it's removed from the source
	// (e.g. `proxy remove-dead`), so it was never in `running` and never
	// went through the removal loop above — without this pass its state
	// entry would never be pruned, accumulating forever. This is safe for
	// URL proxies in give-up backoff: mergeProxyURLCache keeps them in
	// desiredSet for the duration of their backoff window (only eviction/
	// blacklist removes them from the cache), so they are never pruned here.
	//
	// Gated on urlCacheLoaded: if proxy_url.json failed to read this cycle,
	// desiredSet is missing every URL-sourced address (transient I/O error,
	// not "no URL sources configured" — that case returns an empty cache
	// with no error). Pruning against an incomplete desiredSet would wipe
	// ID/health history for every still-desired URL proxy, including ones
	// mid give-up-backoff. Skip the pass entirely until the cache is
	// readable again; the next reload will catch up.
	pruned := 0
	if urlCacheLoaded {
		for addr := range r.state.Proxies {
			if _, ok := desiredSet[addr]; !ok {
				delete(r.state.Proxies, addr)
				pruned++
			}
		}
	} else {
		tlog("[proxy] skipping state prune this cycle: proxy_url.json unavailable\n")
	}

	// Persist the new state snapshot. proxyStateMu prevents the heartbeat
	// goroutine from racing this write and resurrecting removed proxies.
	//
	// This runs under r.mu by design: the slot must not free until the
	// post-reload state is on disk, and an fsync cannot be made cancellable
	// portably. If the filesystem stalls here past reloadHardLimit (a
	// thrashing disk), the reload watchdog is the bound — it escalates to a
	// hot restart, the only way to shed a goroutine stuck in a syscall.
	// Skipping the write instead would drop the prune and ID bookkeeping
	// this pass just computed.
	proxyStateMu.Lock()
	if diskState, err := readProxyState(); err == nil {
		for addr, entry := range r.state.Proxies {
			if diskEntry, ok := diskState.Proxies[addr]; ok {
				entry.Health = diskEntry.Health
				entry.DownSince = diskEntry.DownSince
				entry.AuthFailures = diskEntry.AuthFailures
				r.state.Proxies[addr] = entry
			}
		}
	}
	r.state.NextID = currentProxyIDCounter()
	if err := writeProxyState(r.state); err != nil {
		tlog("[proxy] warning: could not write proxy.state after reload: %v\n", err)
	}
	proxyStateMu.Unlock()

	// Update systemd status counters: the configured count reflects
	// the full desired set (file/internal + URL cache), and resolution
	// is OK since we found proxies. These are operator-facing only.
	setConfiguredProxyCount(trimmedConfiguredCount(len(desiredSet)))
	setProxyResolutionOK()

	deferredTotal := deferredBackoff + warmupDeferred
	reloadDur := time.Since(reloadStart).Round(time.Millisecond)
	// Say where the additions came from, and announce URL-sourced launches on
	// their own line: a bare "+N added" said neither. The summary keeps its
	// "reloaded: +N added" prefix for anything that matches on it.
	fromSources := reloadSourceBreakdown(added, sourceOf)
	if line := urlLaunchLine(urlAdded, warmupDeferred); line != "" {
		logImportant("%s", line)
	}
	if pruned > 0 {
		tlog("[proxy] pruned %d stale proxy.state entries (no longer desired)\n", pruned)
	}
	// One operator-readable sentence, with the machine form kept verbatim in
	// the trailing paren — the docs, the CHANGELOG and several tests match on
	// the "reloaded: +N added" prefix.
	reloadTail := fmt.Sprintf("reloaded: +%d added%s, -%d removed", len(added), fromSources, len(removed))
	if deferredTotal > 0 {
		reloadTail += fmt.Sprintf(", %d deferred (backoff=%d warmup=%d)", deferredTotal, deferredBackoff, warmupDeferred)
	}
	reloadTail += fmt.Sprintf(" [%s] running=%d desired=%d lock_wait=%v", reloadDur, reloadRunning, reloadDesired, reloadLockWait)
	switch {
	case len(added) == 0 && len(removed) == 0 && deferredTotal == 0:
		tlog("🔄 [proxy] Proxy list reloaded: nothing changed, took %s — %d running, %d desired. (%s)\n",
			reloadDur, reloadRunning, reloadDesired, reloadTail)
	case deferredTotal > 0:
		tlog("🔄 [proxy] Proxy list reloaded: %d added%s, %d removed, %d held back (%d backing off, %d still warming up), took %s — %d running, %d desired. (%s)\n",
			len(added), fromSources, len(removed), deferredTotal, deferredBackoff, warmupDeferred,
			reloadDur, reloadRunning, reloadDesired, reloadTail)
	default:
		tlog("🔄 [proxy] Proxy list reloaded: %d added%s, %d removed, took %s — %d running, %d desired. (%s)\n",
			len(added), fromSources, len(removed), reloadDur, reloadRunning, reloadDesired, reloadTail)
	}
}
