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
	mu          sync.Mutex // serializes reloads
	cancelMap map[string]context.CancelFunc
	// TODO: refactor cancelMap and cancelMapMu into a struct owned by ProxyReloader
	// to avoid storing a *sync.Mutex pointer across function boundaries.
	cancelMapMu *sync.Mutex
	state       *ProxyState
	sourcePath  string // "" = internal config (~/.urnetwork/proxy); else external file
	parentCtx   context.Context
	wg          *sync.WaitGroup

	// spawnProxy starts a proxy goroutine's work (the provideWithProxy closure).
	spawnProxy func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool)

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
		fmt.Printf("[proxy] warning: could not determine reload path: %v\n", err)
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
				if err != nil || seq == lastSeq {
					continue
				}
				lastSeq = seq
				r.reload()
			}
		}
	}()
}

// proxyURLGiveUpRetryAfter is the cooldown before a URL-sourced proxy that
// exhausted its auth attempts is automatically reconsidered for restart.
const proxyURLGiveUpRetryAfter = 15 * time.Minute

// proxyGiveUpCooldown tracks, per url-sourced address, the time before which
// reload() must not relaunch it. The address is removed from cancelMap
// immediately on give-up (see requeueURLProxyAfterGiveUp) so reload() can
// eventually re-add it — but "eventually" used to mean "as soon as anything
// else triggers a reload," since reload()'s added/removed diff only looks at
// cancelMap, not at how long ago this specific address gave up. With
// hundreds of proxies independently giving up and the periodic URL refresh
// triggering reloads on its own schedule, some other trigger almost always
// fires well before this address's own 15-minute wait elapses, resurrecting
// it immediately and making the "retry in 15m" log message cosmetic. This
// map is the actual enforcement: reload() skips any address still inside its
// cooldown window regardless of what woke it up.
var (
	proxyGiveUpCooldownMu sync.Mutex
	proxyGiveUpCooldown   = map[string]time.Time{}
)

func setGiveUpCooldown(addr string, until time.Time) {
	proxyGiveUpCooldownMu.Lock()
	proxyGiveUpCooldown[addr] = until
	proxyGiveUpCooldownMu.Unlock()
}

func clearGiveUpCooldown(addr string) {
	proxyGiveUpCooldownMu.Lock()
	delete(proxyGiveUpCooldown, addr)
	proxyGiveUpCooldownMu.Unlock()
}

func isInGiveUpCooldown(addr string) bool {
	proxyGiveUpCooldownMu.Lock()
	until, ok := proxyGiveUpCooldown[addr]
	proxyGiveUpCooldownMu.Unlock()
	return ok && time.Now().Before(until)
}

// requeueURLProxyAfterGiveUp is called when a proxy goroutine has permanently
// given up (exhausted maxAuthFailures) and is about to return for good. By
// default this is terminal: the address stays in cancelMap, so reload() will
// never see it as eligible to restart, even though nothing is running — the
// proxy is dead until a manual 'proxy refresh' or process restart.
//
// For URL-sourced proxies that's too brittle: a large public list naturally
// contains many flaky or dead entries, and requiring manual intervention for
// each one defeats the purpose of pulling from a URL in the first place. For
// those, remove the address from cancelMap now (so reload() will treat it as
// "added" again once its cooldown expires) and schedule a delayed reload
// trigger — staggered restart through the normal jittered backoffPacer path,
// not a thundering herd. Returns true if the proxy was queued for automatic
// retry.
func requeueURLProxyAfterGiveUp(ctx context.Context, addr string, cancelMapMu *sync.Mutex, cancelMap map[string]context.CancelFunc) bool {
	state, err := readProxyState()
	if err != nil || state.Proxies[addr].Source != "url" {
		return false
	}

	cancelMapMu.Lock()
	delete(cancelMap, addr)
	cancelMapMu.Unlock()

	setGiveUpCooldown(addr, time.Now().Add(proxyURLGiveUpRetryAfter))
	go scheduleGiveUpRequeue(ctx, addr, cancelMapMu, cancelMap, proxyURLGiveUpRetryAfter, proxyURLGiveUpRecheckAfter)
	return true
}

// proxyURLGiveUpRecheckAfter is how long after the requeue trigger fires that
// scheduleGiveUpRequeue checks whether the address actually got picked back
// up, retrying the trigger once if not.
const proxyURLGiveUpRecheckAfter = 30 * time.Second

// scheduleGiveUpRequeue waits cooldown, clears it, and fires a reload
// trigger — then waits recheck and fires a second trigger if the address
// still isn't running. Without the recheck, an address whose first trigger
// landed during a transient reload() failure (e.g. the "reload skipped:
// could not read source" path) would sit orphaned forever: its cooldown
// already cleared, with nothing left to ever bring it back short of a
// manual 'proxy refresh' or process restart. Durations are parameters
// (rather than reading the package constants directly) so tests can run this
// on a fast clock instead of waiting on the real 15-minute cooldown.
func scheduleGiveUpRequeue(ctx context.Context, addr string, cancelMapMu *sync.Mutex, cancelMap map[string]context.CancelFunc, cooldown, recheckAfter time.Duration) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(cooldown):
	}
	clearGiveUpCooldown(addr)
	reloadPath, pathErr := proxyReloadPath()
	if pathErr == nil {
		_ = writeReloadTrigger(reloadPath)
	}

	select {
	case <-ctx.Done():
		return
	case <-time.After(recheckAfter):
	}
	cancelMapMu.Lock()
	_, running := cancelMap[addr]
	cancelMapMu.Unlock()
	if !running && pathErr == nil {
		_ = writeReloadTrigger(reloadPath)
	}
}

// reload diffs the proxy source against the currently running set and applies
// the difference: cancels goroutines for removed proxies, starts goroutines for
// added proxies (staggered), and rewrites proxy.state. Untouched proxies are
// never disturbed.
func (r *ProxyReloader) reload() {
	r.mu.Lock()
	defer r.mu.Unlock()

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
			fmt.Printf("[proxy] reload skipped: could not read source: %v\n", err)
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
		fmt.Printf("[proxy][url] warning: could not read proxy_url.json: %v\n", err)
	} else {
		mergeProxyURLCache(desiredSet, sourceOf, urlState)
	}

	// Check emptiness AFTER merging the URL cache — a URL-only deployment
	// (no --proxy_file, no internal proxies) has desired == 0 but a
	// non-empty desiredSet, and must not be treated as a source-read error.
	if len(desiredSet) == 0 {
		fmt.Printf("[proxy] reload skipped: 0 proxies found in source\n")
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
	for addr, s := range desiredSet {
		if !running[addr] && !isInGiveUpCooldown(addr) {
			added = append(added, s)
		}
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

		fmt.Printf("[proxy] draining %s (%d active clients)\n", addr, bw.Clients.Load())

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
			fmt.Printf("[proxy] drain complete: %s\n", proxyAddr)
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
	if len(added) > 0 {
		fmt.Printf("[proxy] reload: adding %d proxies:\n", len(added))
	}
	for i, settings := range added {
		if r.isDraining(settings.Address) {
			fmt.Printf("[proxy] skip add %s: still draining\n", settings.Address)
			continue
		}
		stableID := resolveProxyID(r.state, settings.Address)
		settings.Index = stableID
		tagProxySourceIfUnset(r.state, settings.Address, sourceOf[settings.Address])
		connect.RegisterProxy(stableID, settings.Address)

		var user, password string
		if settings.Auth != nil {
			user = settings.Auth.User
			password = settings.Auth.Password
		}
		fmt.Printf("  proxy[%d] %s (%s/%s)\n", stableID, settings.Address, obfuscateUser(user), obfuscatePassword(password))

		proxyCtx, proxyCancel := context.WithCancel(r.parentCtx)
		r.cancelMapMu.Lock()
		r.cancelMap[settings.Address] = proxyCancel
		r.cancelMapMu.Unlock()

		staggerPos := i
		settingsCopy := settings
		r.wg.Add(1)
		go connect.HandleError(func() {
			defer r.wg.Done()
			defer connect.UnregisterProxy(stableID)
			defer proxyCancel()

			if !backoffPacer(staggerPos, time.Now(), proxyCtx) {
				return
			}
			r.spawnProxy(proxyCtx, settingsCopy, false)
		})
	}

	// Persist the new state snapshot. proxyStateMu prevents the heartbeat
	// goroutine from racing this write and resurrecting removed proxies.
	proxyStateMu.Lock()
	r.state.NextID = currentProxyIDCounter()
	if err := writeProxyState(r.state); err != nil {
		fmt.Printf("[proxy] warning: could not write proxy.state after reload: %v\n", err)
	}
	proxyStateMu.Unlock()

	fmt.Printf("[proxy] reloaded: +%d added, -%d removed\n", len(added), len(removed))
}
