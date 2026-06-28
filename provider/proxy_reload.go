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

// writeReloadTrigger increments the sequence number in the trigger file.
// Called by the proxy refresh subcommand after confirmation.
func writeReloadTrigger(path string) error {
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

// acquireProxyLock creates the lock file at the default path. Returns an error
// if a reload is already in progress (lock already held).
func acquireProxyLock() (func(), error) {
	path, err := proxyLockPath()
	if err != nil {
		return nil, err
	}
	return acquireProxyLockAt(path)
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
	return func() { os.Remove(path) }, nil
}

const proxyLockStaleAge = 5 * time.Minute

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
	r.mu.Lock()
	defer r.mu.Unlock()

	lockRelease, err := acquireProxyLock()
	if err != nil {
		tlog("[proxy] reload skipped: %v\n", err)
		return
	}
	defer lockRelease()

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

	if urlState, err := readProxyURLState(); err != nil {
		tlog("[proxy][url] warning: could not read proxy_url.json: %v\n", err)
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
		if _, ok := desiredSet[addr]; !ok {
			removed = append(removed, addr)
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

	// Persist the new state snapshot. proxyStateMu prevents the heartbeat
	// goroutine from racing this write and resurrecting removed proxies.
	proxyStateMu.Lock()
	r.state.NextID = currentProxyIDCounter()
	if err := writeProxyState(r.state); err != nil {
		tlog("[proxy] warning: could not write proxy.state after reload: %v\n", err)
	}
	proxyStateMu.Unlock()

	deferredTotal := deferredBackoff + warmupDeferred
	if deferredTotal > 0 {
		tlog("[proxy] reloaded: +%d added, -%d removed, %d deferred (backoff=%d warmup=%d)\n",
			len(added), len(removed), deferredTotal, deferredBackoff, warmupDeferred)
	} else {
		tlog("[proxy] reloaded: +%d added, -%d removed\n", len(added), len(removed))
	}
}
