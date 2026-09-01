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
			os.Remove(path)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("reload already in progress — try again in a moment")
		}
		return nil, err
	}
	fmt.Fprintf(f, "%d\n%d\n", os.Getpid(), time.Now().Unix())
	f.Close()
	var once sync.Once
	return func() { once.Do(func() { os.Remove(path) }) }, nil
}

const proxyLockStaleAge = 5 * time.Minute

// writeReloadTriggerDebounce is the minimum interval between consecutive
// trigger writes. Callers inside fetch/reaper loops may fire hundreds of
// times per cycle; this ensures the watcher only picks up one trigger
// per debounce window instead of spawning overlapping reloads.
var writeReloadTriggerDebounce = 30 * time.Second

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

func isLockStale(data []byte) bool {
	lines := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)
	if len(lines) < 2 {
		return true
	}
	pid, err := strconv.Atoi(lines[0])
	if err != nil {
		return true
	}
	ts, err := strconv.ParseInt(lines[1], 10, 64)
	if err != nil {
		return true
	}
	if time.Since(time.Unix(ts, 0)) > proxyLockStaleAge {
		return true
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	err = process.Signal(syscall.Signal(0))
	return err != nil
}

// ProxyReloader manages hot-reload of proxy goroutines. It is driven by the
// reload watcher goroutine, which polls the trigger file for a changed sequence
// number and calls reload(). reload() is serialized by mu so two reloads never
// overlap.
type ProxyReloader struct {
	mu        sync.Mutex // serializes reloads
	cancelMap map[string]context.CancelFunc
	// TODO: refactor cancelMap and cancelMapMu into a struct owned by ProxyReloader
	// to avoid storing a *sync.Mutex pointer across function boundaries.
	cancelMapMu *sync.Mutex
	state       *ProxyState
	sourcePath  string // "" = internal config (~/.urnetwork/proxy); else external file
	parentCtx   context.Context
	wg          *sync.WaitGroup

	// spawnProxy starts a proxy goroutine's work (the provideWithProxy closure).
	spawnProxy func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool)

	drainingProxies map[string]context.CancelFunc // proxies draining active sessions
	drainMu         sync.Mutex
}

func (r *ProxyReloader) isDraining(addr string) bool {
	r.drainMu.Lock()
	defer r.drainMu.Unlock()
	_, ok := r.drainingProxies[addr]
	return ok
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

	go func() {
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
				tlog("🔄 [proxy] reload trigger: seq %d → %d\n", lastSeq, seq)
				lastSeq = seq
				r.reload()
			}
		}
	}()
}

// reload diffs the proxy source against the currently running set and applies
// the difference: cancels goroutines for removed proxies, starts goroutines for
// added proxies (staggered), and rewrites proxy.state. Untouched proxies are
// never disturbed.
func (r *ProxyReloader) reload() {
	reloadStart := time.Now()

	r.mu.Lock()
	defer r.mu.Unlock()

	lockAcqStart := time.Now()
	lockRelease, err := acquireProxyLock()
	if err != nil {
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
			return
		}
		desired = settings
	} else {
		desired = readProxySettings()
	}

	desiredSet := make(map[string]*connect.ProxySettings, len(desired))
	sourceOf := make(map[string]string, len(desired))
	primarySource := "internal"
	if r.sourcePath != "" {
		primarySource = "file"
	}
	for _, s := range desired {
		desiredSet[s.Address] = s
		sourceOf[s.Address] = primarySource
	}

	urlCacheLoaded := true
	if urlState, err := readProxyURLState(); err != nil {
		tlog("[proxy][url] warning: could not read proxy_url.json: %v\n", err)
		urlCacheLoaded = false
	} else {
		mergeProxyURLCache(desiredSet, sourceOf, urlState)
	}

	// Check emptiness AFTER merging the URL cache — a URL-only deployment
	// (no --proxy_file, no internal proxies) has desired == 0 but a
	// non-empty desiredSet, and must not be treated as a source-read error.
	if len(desiredSet) == 0 {
		tlog("[proxy] reload skipped: 0 proxies found in source\n")
		return
	}

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
		// Disable direct: cancel the goroutine.
		r.cancelMapMu.Lock()
		if cancel, ok := r.cancelMap[directProxyKey]; ok {
			cancel()
			delete(r.cancelMap, directProxyKey)
		}
		r.cancelMapMu.Unlock()
		connect.UnregisterProxy(0)
		tlog("[direct] native [direct] transport stopped (disable)\n")
	} else if !directRunning && directShouldRun {
		// Enable direct: start the goroutine.
		r.wg.Add(1)
		directCtx, directCancel := context.WithCancel(r.parentCtx)
		r.cancelMapMu.Lock()
		r.cancelMap[directProxyKey] = directCancel
		r.cancelMapMu.Unlock()
		go connect.HandleError(func() {
			defer r.wg.Done()
			defer directCancel()
			defer connect.UnregisterProxy(0)
			connect.RegisterProxy(0, "direct")
			r.spawnProxy(directCtx, nil, true, false)
		})
		tlog("[direct] native [direct] transport started (enable)\n")
	}

	tlog("[proxy] reload: running=%d desired=%d lock_wait=%v\n",
		len(running), len(desiredSet), lockWait.Round(time.Millisecond))

	var added []*connect.ProxySettings
	deferredBackoff := 0
	now := time.Now()
	for addr, s := range desiredSet {
		if running[addr] {
			continue
		}
		// Enforce the URL give-up backoff at launch time: an address whose
		// next-eligible time has not yet arrived is skipped, not relaunched.
		// Only URL-sourced proxies ever carry a backoff window, so file and
		// internal proxies (which never call SetBackoffUntil) are always
		// eligible here. Without this, the escalating backoff was defeated
		// because any reload would relaunch every desired-but-not-running
		// proxy immediately.
		if !globalProxyFailureHistory.Eligible(addr, now) {
			deferredBackoff++
			continue
		}
		added = append(added, s)
	}
	var removed []string
	for addr := range running {
		if addr == directProxyKey {
			continue // managed by the direct hot-toggle block above, not the proxy diff
		}
		if _, ok := desiredSet[addr]; !ok {
			removed = append(removed, addr)
		}
	}

	// Operator trim cap (provider proxy trim <N>): hold the running pool at N.
	// Shed the A-F-worst running proxies above N (folded into removed so they are
	// cancelled), and drop the worst-graded not-yet-running additions above the
	// budget so the pool cannot regrow above the cap until it is raised.
	if trimCap, terr := readTrimTarget(); terr == nil && trimCap > 0 {
		traffic := runningProxyTraffic()
		// Read the URL cache here: the urlState read earlier is scoped to its own
		// if/else and is not visible in this hook.
		trimURLState, _ := readProxyURLState()
		gradeFor := buildTrimGradeResolver(r.state, trimURLState)
		// Union count of everything about to be cancelled, so the addition
		// budget is not under-sized by double-counting (review MEDIUM).
		removedSet := make(map[string]bool, len(removed))
		for _, a := range removed {
			removedSet[a] = true
		}
		shedCount := 0
		if len(running) > trimCap {
			rlist := make([]string, 0, len(running))
			for a := range running {
				if !removedSet[a] {
					rlist = append(rlist, a)
				}
			}
			for _, addr := range selectWorstRunningProxies(r.state.Proxies, gradeFor, traffic, rlist, len(running)-trimCap) {
				if _, ok := running[addr]; ok && !removedSet[addr] {
					removed = append(removed, addr)
					removedSet[addr] = true
					shedCount++
					// Do NOT delete from desiredSet: pruning against a trim-mutated
					// set erases grade/health history (review HIGH). Mark a short
					// give-up backoff instead so the launch gate keeps it down and
					// it does not relaunch next cycle.
					globalProxyFailureHistory.SetBackoffUntil(addr, time.Now().Add(shedBackoff))
				}
			}
		}
		budget := trimCap - (len(running) - len(removedSet))
		if budget < 0 {
			budget = 0
		}
		dropped := 0
		if len(added) > budget {
			alist := make([]string, 0, len(added))
			for _, s := range added {
				alist = append(alist, s.Address)
			}
			drop := selectWorstRunningProxies(r.state.Proxies, gradeFor, traffic, alist, len(added)-budget)
			dropSet := make(map[string]bool, len(drop))
			for _, a := range drop {
				dropSet[a] = true
			}
			kept := added[:0]
			for _, s := range added {
				if dropSet[s.Address] {
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
		if shedCount > 0 || dropped > 0 {
			tlog("[proxy][trim] cap=%d: shed %d worst-graded running, held %d additions (pool ~%d)\\n", trimCap, shedCount, dropped, len(running)-shedCount)
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
		delete(r.state.Proxies, addr)

		bw := connect.ProxyBandwidthByAddress(addr)
		if bw == nil || bw.Clients.Load() == 0 {
			cancel()
			continue
		}

		r.drainMu.Lock()
		r.drainingProxies[addr] = cancel
		r.drainMu.Unlock()

		tlog("[proxy] draining %s (%d active clients)\n", addr, bw.Clients.Load())

		go func(cancelFn context.CancelFunc, proxyAddr string) {
			defer func() {
				r.drainMu.Lock()
				delete(r.drainingProxies, proxyAddr)
				r.drainMu.Unlock()
			}()
			for {
				bw := connect.ProxyBandwidthByAddress(proxyAddr)
				if bw == nil || bw.Clients.Load() == 0 {
					break
				}
				select {
				case <-r.parentCtx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			}
			tlog("[proxy] drain complete: %s\n", proxyAddr)
			cancelFn()

			desired, err := currentDesiredProxyAddresses()
			if err == nil && desired[proxyAddr] {
				if reloadPath, err := proxyReloadPath(); err == nil {
					if err := writeReloadTrigger(reloadPath); err == nil {
						tlog("[proxy] re-triggered reload for %s (re-added while draining)\n", proxyAddr)
					}
				}
			}
		}(cancel, addr)
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
	warmupDeferred := 0
	for i, settings := range added {
		if r.isDraining(settings.Address) {
			tlog("[proxy] skip add %s: still draining\n", settings.Address)
			continue
		}
		// Defer URL-sourced proxy launches until file-proxy warmup
		// completes, so operator-curated proxies get an uncontested ramp.
		if sourceOf[settings.Address] == "url" && !proxyWarmupDone.Load() {
			warmupDeferred++
			continue
		}
		stableID := resolveProxyID(r.state, settings.Address)
		settings.Index = stableID
		tagProxySourceIfUnset(r.state, settings.Address, sourceOf[settings.Address])
		connect.RegisterProxy(stableID, settings.Address)

		proxyCtx, proxyCancel := context.WithCancel(r.parentCtx)
		r.cancelMapMu.Lock()
		r.cancelMap[settings.Address] = proxyCancel
		r.cancelMapMu.Unlock()

		staggerPos := i
		settingsCopy := settings
		isURLSourced := sourceOf[settings.Address] == "url"
		reloadStaggerMs := 150
		if isURLSourced {
			reloadStaggerMs = 500
		}
		r.wg.Add(1)
		go connect.HandleError(func() {
			defer r.wg.Done()
			defer connect.UnregisterProxy(stableID)
			defer proxyCancel()

			if !backoffPacer(staggerPos, reloadStaggerMs, time.Now(), proxyCtx) {
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

	deferredTotal := deferredBackoff + warmupDeferred
	reloadDur := time.Since(reloadStart).Round(time.Millisecond)
	if pruned > 0 {
		tlog("[proxy] pruned %d stale proxy.state entries (no longer desired)\n", pruned)
	}
	if deferredTotal > 0 {
		tlog("🔄 [proxy] reloaded: +%d added, -%d removed, %d deferred (backoff=%d warmup=%d) [%s]\n",
			len(added), len(removed), deferredTotal, deferredBackoff, warmupDeferred, reloadDur)
	} else {
		tlog("🔄 [proxy] reloaded: +%d added, -%d removed [%s]\n",
			len(added), len(removed), reloadDur)
	}
}
