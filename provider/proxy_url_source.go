package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// proxyURLMaxOverridePath returns ~/.urnetwork/proxy_url_max, a file an
// operator can write to cap the number of URL-sourced proxies at runtime.
func proxyURLMaxOverridePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "proxy_url_max"), nil
}

// resolveProxyURLMax re-reads the cap on every call. startupMax is the value
// from --proxy_url_max at process start. Returns startupMax if the file
// doesn't exist, is empty, or holds an unparseable value.
func resolveProxyURLMax(startupMax int) int {
	path, err := proxyURLMaxOverridePath()
	if err != nil {
		return startupMax
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return startupMax
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return startupMax
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return startupMax
	}
	return n
}

// proxyURLRefreshOverridePath returns ~/.urnetwork/proxy_url_refresh.
func proxyURLRefreshOverridePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "proxy_url_refresh"), nil
}

// resolveProxyURLRefresh re-reads the interval on every call.
func resolveProxyURLRefresh(startupInterval time.Duration) time.Duration {
	path, err := proxyURLRefreshOverridePath()
	if err != nil {
		return startupInterval
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return startupInterval
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return startupInterval
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 10*time.Second {
		return startupInterval
	}
	return d
}

// proxyCleanupScopeOverridePath returns ~/.urnetwork/proxy_dead_cleanup_scope.
func proxyCleanupScopeOverridePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "proxy_dead_cleanup_scope"), nil
}

// resolveProxyCleanupScope re-reads the scope on every call.
func resolveProxyCleanupScope(startupScope string) string {
	path, err := proxyCleanupScopeOverridePath()
	if err != nil {
		return startupScope
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return startupScope
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return startupScope
	}
	return v
}

// proxyCleanupIntervalOverridePath returns ~/.urnetwork/proxy_dead_cleanup_interval.
func proxyCleanupIntervalOverridePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "proxy_dead_cleanup_interval"), nil
}

// resolveProxyCleanupInterval re-reads the interval on every call.
func resolveProxyCleanupInterval(startupInterval time.Duration) time.Duration {
	path, err := proxyCleanupIntervalOverridePath()
	if err != nil {
		return startupInterval
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return startupInterval
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return startupInterval
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < time.Minute {
		return startupInterval
	}
	return d
}

// proxyLoadThresholdOverridePath returns ~/.urnetwork/proxy_load_threshold,
// a file an operator can write to set the per-core load threshold for URL
// fetch gating (e.g. "2.0"). The effective threshold is this value × NumCPU.
func proxyLoadThresholdOverridePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "proxy_load_threshold"), nil
}

// resolveProxyLoadThreshold re-reads the per-core load threshold on every
// call. startupThreshold is the compiled-in default. Returns startupThreshold
// if the override file doesn't exist, is empty, or holds an unparseable value.
func resolveProxyLoadThreshold(startupThreshold float64) float64 {
	path, err := proxyLoadThresholdOverridePath()
	if err != nil {
		return startupThreshold
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return startupThreshold
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return startupThreshold
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n <= 0 {
		return startupThreshold
	}
	return n
}

// getSystemLoad reads /proc/loadavg and returns the 1-minute and 5-minute
// load averages. Returns an error on non-Linux systems or parse failure;
// callers should fail-open (skip gating) when this happens.
func getSystemLoad() (load1, load5 float64, err error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, err
	}
	parts := strings.Fields(string(data))
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("unexpected /proc/loadavg format: %q", string(data))
	}
	load1, err = strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0, 0, err
	}
	load5, err = strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return 0, 0, err
	}
	return load1, load5, nil
}

// proxySelfHealOverridePath returns ~/.urnetwork/proxy_self_heal, a marker
// file for the `urnet-tools self-heal on|off` toggle. Absent means the
// startup default (off unless URNETWORK_SELF_HEAL=1); "on" enables;
// anything else disables. Follows the same pattern as fast_auth.
func proxySelfHealOverridePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "proxy_self_heal"), nil
}

// resolveSelfHealEnabled reads the runtime override file every call.
// startupEnabled is the value from URNETWORK_SELF_HEAL at process start;
// the override file takes precedence when present: "on" enables, any other
// non-empty value disables. Falls back to startupEnabled (default FALSE —
// self-heal is opt-in) when the file is absent or empty.
func resolveSelfHealEnabled(startupEnabled bool) bool {
	path, err := proxySelfHealOverridePath()
	if err != nil {
		return startupEnabled
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return startupEnabled
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return startupEnabled
	}
	return strings.EqualFold(v, "on")
}

// defaultAPIHost is the default target for the API reachability probe.
const defaultAPIHost = "api.bringyour.com"
const defaultAPIPort = 443

// removeDeadProxies removes the given addresses from whichever source they
// came from — a --proxy_file source, the internal config, or the URL
// cache — and triggers a hot-reload. addrsBySource groups addresses by their
// proxy.state Source tag ("file", "internal", or "url"); unrecognized keys
// are ignored. Used by both the interactive `proxy remove-dead` command and
// the automatic scoped cleanup job, so removal logic only lives in one place.
func removeDeadProxies(state *ProxyState, addrsBySource map[string][]string) error {
	release, err := acquireProxyLock()
	if err != nil {
		return fmt.Errorf("could not acquire proxy lock: %w", err)
	}
	defer release()

	if fileAddrs := addrsBySource["file"]; len(fileAddrs) > 0 {
		if state.Source == "" {
			tlog("[proxy] warning: %d proxies tagged source=file but no file source is configured; skipping\n", len(fileAddrs))
		} else if err := removeAddressesFromFile(state.Source, fileAddrs); err != nil {
			return fmt.Errorf("could not update proxy file: %w", err)
		}
	}

	if internalAddrs := addrsBySource["internal"]; len(internalAddrs) > 0 {
		proxyConfig := readProxyConfig()
		removeSet := map[string]bool{}
		for _, a := range internalAddrs {
			removeSet[a] = true
		}
		for proxyAddress := range proxyConfig.Servers {
			addr, _, _ := parseProxyAddress(proxyAddress)
			if removeSet[addr] {
				delete(proxyConfig.Servers, proxyAddress)
			}
		}
		writeProxyConfig(proxyConfig)
	}

	if urlAddrs := addrsBySource["url"]; len(urlAddrs) > 0 {
		urlState, err := readProxyURLState()
		if err != nil {
			return fmt.Errorf("could not read proxy_url.json: %w", err)
		}
		for _, a := range urlAddrs {
			delete(urlState.Cache, a)
		}
		if err := writeProxyURLState(urlState); err != nil {
			return fmt.Errorf("could not write proxy_url.json: %w", err)
		}
	}

	// Release the lock before writing the reload trigger so the running
	// provider's reload() can acquire it. The deferred release is a no-op
	// (sync.Once idempotent guard in acquireProxyLockAt).
	release()
	reloadPath, err := proxyReloadPath()
	if err != nil {
		return fmt.Errorf("could not determine reload path: %w", err)
	}
	return writeReloadTrigger(reloadPath)
}

// evictProxyURLAddress permanently removes address from the URL cache and
// records it in the persisted blacklist in the same write, so a future
// fetch (which is add-only by design) can never silently bring it back,
// even across process restarts. Triggers a hot-reload so the live fleet
// reflects the removal immediately. Used once a URL-sourced proxy has given
// up enough times (proxyURLGiveUpEvictAfterCycles) that retrying it is no
// longer worth an auth-rate-limiter slot.
func evictProxyURLAddress(address string) error {
	release, err := acquireProxyLock()
	if err != nil {
		return fmt.Errorf("could not acquire proxy lock: %w", err)
	}
	defer release()

	state, err := readProxyURLState()
	if err != nil {
		return fmt.Errorf("could not read proxy_url.json: %w", err)
	}

	delete(state.Cache, address)
	if state.Blacklist == nil {
		state.Blacklist = map[string]time.Time{}
	}
	state.Blacklist[address] = time.Now().UTC()

	if err := writeProxyURLState(state); err != nil {
		return fmt.Errorf("could not write proxy_url.json: %w", err)
	}

	// Release the lock before the trigger (deferred release is idempotent).
	release()
	reloadPath, err := proxyReloadPath()
	if err != nil {
		return fmt.Errorf("could not determine reload path: %w", err)
	}
	return writeReloadTrigger(reloadPath)
}

// currentDesiredProxyAddresses returns every address currently desired by
// this provider: the primary source (file or internal config) merged with
// the URL cache. Used wherever "is this address still part of the fleet"
// needs to be independent of live health-registration state — a give-up'd
// proxy's goroutine unregisters immediately on exit, so it would otherwise
// look like it left the fleet for the entire wait window before its next
// requeue, even though it's still desired and will be relaunched.
func currentDesiredProxyAddresses() (map[string]bool, error) {
	state, err := readProxyState()
	if err != nil {
		return nil, fmt.Errorf("could not read proxy.state: %w", err)
	}

	var desired []*connect.ProxySettings
	if state.Source != "" {
		desired, err = readProxySettingsFromFile(state.Source)
		if err != nil {
			return nil, fmt.Errorf("could not read proxy file %s: %w", state.Source, err)
		}
	} else {
		desired = readProxySettings()
	}

	addrs := make(map[string]bool, len(desired))
	for _, s := range desired {
		addrs[s.Address] = true
	}

	urlState, err := readProxyURLState()
	if err != nil {
		return nil, fmt.Errorf("could not read proxy_url.json: %w", err)
	}
	for addr := range urlState.Cache {
		addrs[addr] = true
	}

	return addrs, nil
}

var fetchMu sync.Mutex

// fetchAndMergeProxyURLs fetches every configured source, merges newly
// discovered addresses into the persisted cache (add-only — existing entries
// are never removed here), and triggers a hot-reload if anything new was
// found. A fetch failure for one URL logs a warning and is skipped; it never
// clears already-cached entries from that source.
//
// Only one fetch cycle may run at a time — if an earlier cycle's probing
// phase outlasts the refresh interval, the next tick's call returns
// immediately rather than racing on the same file.
func fetchAndMergeProxyURLs(ctx context.Context, urls []string, maxTotal int, apiHost string, apiPort uint16) {
	if len(urls) == 0 {
		return
	}

	if !fetchMu.TryLock() {
		return
	}
	defer fetchMu.Unlock()

	// Fetching from the network can be slow; do it before taking the lock so
	// we don't hold it across HTTP requests. Only the read-modify-write of
	// proxy_url.json below needs to be serialized against removeDeadProxies.
	fetched := make([][]string, len(urls))
	apiOKCounts := make([]int, len(urls))
	socks5OnlyCounts := make([]int, len(urls))
	for i, url := range urls {
		lines, err := fetchProxyURLLines(ctx, url)
		if err != nil {
			tlog("[proxy][url] fetch failed for %s: %v (skipping this cycle)\n", url, err)
			continue
		}
		// Free public proxy lists are mostly dead entries. The dual-stage probe
		// checks TCP reachability, SOCKS5 protocol compliance, and whether the
		// proxy can route traffic to the URNetwork API — before anything ever
		// enters the cache or consumes an auth-rate-limiter slot.
		apiOK, socks5Only := probeAndFilterProxyURLLines(ctx, lines, apiHost, apiPort)
		tlog("[proxy][url] probed %s: %d/%d api-reachable, %d socks5-only\n", url, len(apiOK), len(lines), len(socks5Only))
		// api-reachable lines are cached with ProbeOK=true; socks5-only lines
		// are cached with ProbeOK=false so the background reaper can retry
		// them (they may have had a transient routing issue).
		fetched[i] = append(apiOK, socks5Only...)
		apiOKCounts[i] = len(apiOK)
		socks5OnlyCounts[i] = len(socks5Only)
	}

	release, err := acquireProxyLock()
	if err != nil {
		tlog("[proxy][url] warning: could not acquire proxy lock: %v\n", err)
		return
	}
	defer release()

	state, err := readProxyURLState()
	if err != nil {
		tlog("[proxy][url] warning: could not read proxy_url.json: %v\n", err)
		state = &ProxyURLState{Cache: map[string]ProxyURLEntry{}}
	}

	totalAdded := 0
	dirty := false
	for i, url := range urls {
		if fetched[i] == nil {
			continue
		}
		added := mergeProxyURLEntries(state, fetched[i], apiOKCounts[i], maxTotal)
		totalAdded += added
		socks5Count := socks5OnlyCounts[i]
		// Mark socks5-only entries for reaper retry
		if socks5Count > 0 {
			marked := 0
			for _, addr := range fetched[i] {
				if entry, ok := state.Cache[addr]; ok {
					if !entry.ProbeOK {
						// entry already has ProbeOK=false from merge, but
						// ensure LastProbe is set so the reaper picks it up
						entry.LastProbe = time.Now()
						state.Cache[addr] = entry
						marked++
						dirty = true
					}
				}
			}
			tlog("[proxy][url] %s: %d socks5-only entries marked for reaper\n", url, marked)
		}
		tlog("[proxy][url] fetched %s: +%d new proxies\n", url, added)
	}

	if totalAdded == 0 && !dirty {
		return
	}
	if err := writeProxyURLState(state); err != nil {
		tlog("[proxy][url] warning: could not write proxy_url.json: %v\n", err)
		return
	}
	if reloadPath, err := proxyReloadPath(); err == nil {
		if err := writeReloadTrigger(reloadPath); err != nil {
			tlog("[proxy][url] warn: reload trigger write failed: %v\n", err)
		}
	}
}

// reaperProbeTarget holds a single candidate address and a snapshot of its
// probe state, collected under the lock then probed outside it.
type reaperProbeTarget struct {
	addr       string
	entry      ProxyURLEntry
	wasProbeOK bool
}

// runURLProxyReaper iterates the URL cache and re-probes entries whose
// ProbeOK is false (socks5-only from a previous fetch, or entries added
// before the probe fields existed). Entries that fail proxyAPIMaxFails
// consecutive probes are moved to the persistent Blacklist. Runs every
// proxyReaperInterval. Exits when ctx is cancelled.
func runURLProxyReaper(ctx context.Context, apiHost string, apiPort uint16) {
	ticker := time.NewTicker(proxyReaperInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		runURLProxyReaperOnce(ctx, apiHost, apiPort)
	}
}

// runURLProxyReaperOnce performs a single reaper pass: collect candidates,
// probe them, apply results. Split out from runURLProxyReaper so it can be
// exercised directly in tests without waiting on proxyReaperInterval.
//
// Probing happens outside the proxy lock so a large batch of dead entries
// doesn't block concurrent reloads, fetches, or removeDeadProxies calls.
// The lock is held only while collecting candidates and while applying the
// results atomically.
func runURLProxyReaperOnce(ctx context.Context, apiHost string, apiPort uint16) {
	// Collect candidates under the lock, then probe outside it.
	var candidates []reaperProbeTarget
	func() {
		release, err := acquireProxyLock()
		if err != nil {
			return
		}
		defer release()

		state, err := readProxyURLState()
		if err != nil {
			return
		}
		for addr, entry := range state.Cache {
			if !entry.ProbeOK {
				// Unproven or previously-failed proxy: re-probe if due.
				if !entry.LastProbe.IsZero() && time.Since(entry.LastProbe) < proxyReaperInterval {
					continue
				}
				candidates = append(candidates, reaperProbeTarget{addr: addr, entry: entry})
				continue
			}
			// Once-good proxy: re-probe only when stale, so dead-but-cached
			// entries don't accumulate invisibly until the give-up pipeline
			// evicts them. Without this, the reaper has no exit for proxies
			// that passed the initial probe then died later.
			if !entry.LastProbe.IsZero() && time.Since(entry.LastProbe) < proxyReaperStaleThreshold {
				continue
			}
			candidates = append(candidates, reaperProbeTarget{
				addr: addr, entry: entry, wasProbeOK: true,
			})
		}
	}()

	if len(candidates) == 0 {
		return
	}

	// Probe every candidate outside the lock. Serial probing caps total
	// cycle time but no longer blocks concurrent proxy operations, which
	// was the critical problem — a 5-minute stale-lock age window would
	// let a reload steal the lock mid-cycle and race on proxy_url.json.
	type probeResultEntry struct {
		addr       string
		result     probeResult
		wasProbeOK bool
	}
	results := make([]probeResultEntry, 0, len(candidates))
	for _, c := range candidates {
		results = append(results, probeResultEntry{
			addr:       c.addr,
			result:     probeProxy(ctx, c.addr, apiHost, apiPort),
			wasProbeOK: c.wasProbeOK,
		})
	}

	// Re-acquire the lock and atomically apply all results.
	func() {
		release, err := acquireProxyLock()
		if err != nil {
			return
		}
		defer release()

		state, err := readProxyURLState()
		if err != nil {
			return
		}

		changed := false
		for _, r := range results {
			entry, ok := state.Cache[r.addr]
			if !ok {
				continue // removed by a concurrent writer
			}
			if r.wasProbeOK {
				// Stale re-probe of a once-good entry: demote on failure,
				// or refresh timestamp on success.
				switch r.result {
				case probeAPIReachable:
					entry.LastProbe = time.Now()
					entry.ProbeOK = true
					entry.ProbeFails = 0
					state.Cache[r.addr] = entry
					changed = true
				case probeSocks5Only, probeDead:
					entry.LastProbe = time.Now()
					entry.ProbeOK = false
					entry.ProbeFails = 1
					state.Cache[r.addr] = entry
					tlog("[proxy][url] reaper: demoted %s from ProbeOK after stale re-probe\n", r.addr)
					changed = true
				}
				continue
			}

			entry.LastProbe = time.Now()

			switch r.result {
			case probeAPIReachable:
				entry.ProbeOK = true
				entry.ProbeFails = 0
				state.Cache[r.addr] = entry
				changed = true

			case probeSocks5Only:
				entry.ProbeOK = false
				entry.ProbeFails++
				state.Cache[r.addr] = entry
				if entry.ProbeFails >= proxyAPIMaxFails {
					if state.Blacklist == nil {
						state.Blacklist = map[string]time.Time{}
					}
					state.Blacklist[r.addr] = time.Now().UTC()
					delete(state.Cache, r.addr)
					tlog("[proxy][url] reaper: blacklisted %s after %d failed probes\n", r.addr, entry.ProbeFails)
				}
				changed = true

			case probeDead:
				entry.ProbeFails++
				state.Cache[r.addr] = entry
				if entry.ProbeFails >= proxyAPIMaxFails {
					if state.Blacklist == nil {
						state.Blacklist = map[string]time.Time{}
					}
					state.Blacklist[r.addr] = time.Now().UTC()
					delete(state.Cache, r.addr)
					tlog("[proxy][url] reaper: blacklisted %s (dead, %d fails)\n", r.addr, entry.ProbeFails)
				}
				changed = true
			}
		}

		if changed {
			if err := writeProxyURLState(state); err != nil {
				tlog("[proxy][url] reaper: could not write proxy_url.json: %v\n", err)
			}
			if reloadPath, err := proxyReloadPath(); err == nil {
				if err := writeReloadTrigger(reloadPath); err != nil {
					tlog("[proxy][url] warn: reload trigger write failed: %v\n", err)
				}
			}
		}
	}()
}

// pruneURLProxyBlacklist removes blacklist entries older than
// proxyBlacklistCooldown, giving previously-dead addresses a chance to
// re-enter via a fresh fetch cycle. Runs every proxyBlacklistPruneInterval.
func pruneURLProxyBlacklist(ctx context.Context) {
	ticker := time.NewTicker(proxyBlacklistPruneInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		release, err := acquireProxyLock()
		if err != nil {
			continue
		}

		state, err := readProxyURLState()
		if err != nil {
			release()
			continue
		}

		cutoff := time.Now().UTC().Add(-proxyBlacklistCooldown)
		pruned := 0
		for addr, when := range state.Blacklist {
			if when.Before(cutoff) {
				delete(state.Blacklist, addr)
				pruned++
			}
		}

		if pruned > 0 {
			tlog("[proxy][url] pruned %d blacklist entries older than %s\n", pruned, proxyBlacklistCooldown)
			if err := writeProxyURLState(state); err != nil {
				tlog("[proxy][url] pruner: could not write proxy_url.json: %v\n", err)
			}
		}

		release()
	}
}

// runProxyURLFetcher periodically fetches configured proxy list URLs and
// merges new entries into the running proxy set. The first fetch runs
// immediately; subsequent fetches run every refreshInterval. Exits when ctx
// is cancelled. A no-op if urls is empty.
//
// Fetches are skipped when the system is under persistent load (both 1m and
// 5m load averages above 2.0 per core), with a starvation escape so a
// chronically loaded box still slowly refreshes its cache.
func runProxyURLFetcher(ctx context.Context, urls []string, refreshInterval time.Duration, maxTotal int, apiHost string, apiPort uint16, selfHealEnabled bool) {
	if len(urls) == 0 {
		return
	}

	// Wait for file-proxy warmup to finish before the first fetch, so URL-
	// sourced proxies never compete for auth rate-limiter slots with the
	// operator-curated file proxies during the initial ramp.
	for !proxyWarmupDone.Load() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}

	// The initial fetch is always allowed (cold-start / starvation escape).
	fetchAndMergeProxyURLs(ctx, urls, resolveProxyURLMax(maxTotal), apiHost, apiPort)

	var consecutiveSkips int

	activeInterval := resolveProxyURLRefresh(refreshInterval)
	ticker := time.NewTicker(activeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Re-check runtime overrides on every tick
			if ni := resolveProxyURLRefresh(refreshInterval); ni != activeInterval {
				ticker.Stop()
				ticker = time.NewTicker(ni)
				activeInterval = ni
				tlog("[proxy][url] refresh interval changed to %s\n", ni)
			}

			// Load-aware gating: skip the fetch cycle when the system is
			// persistently overloaded. This prevents adding more work (probe
			// connections, auth attempts, reload churn) to an already-drowning
			// machine. Applies regardless of whether a cap is configured —
			// an unbounded proxy_url_max (0) is exactly the config where
			// load protection matters most, so it must not be gated on the
			// cap being set. Can be disabled via `urnet-tools self-heal off`.
			if resolveSelfHealEnabled(selfHealEnabled) {
				cacheSize := readURLCacheSize()
				load1, load5, loadErr := getSystemLoad()
				threshold := resolveProxyLoadThreshold(2.0) * float64(runtime.NumCPU())
				if shouldSkipProxyURLFetch(cacheSize, consecutiveSkips, load1, load5, threshold, loadErr) {
					consecutiveSkips++
					tlog("[proxy][url] fetch skipped: load 1m=%.2f 5m=%.2f (threshold=%.2f, ncpu=%d, skip=%d/%d, cache=%d)\n",
						load1, load5, threshold, runtime.NumCPU(), consecutiveSkips, 6, cacheSize)
					continue
				}
			}
			consecutiveSkips = 0
			fetchAndMergeProxyURLs(ctx, urls, resolveProxyURLMax(maxTotal), apiHost, apiPort)
		}
	}
}

// shouldSkipProxyURLFetch decides whether a URL fetch cycle should be
// skipped due to sustained system load. threshold is the absolute load
// value (already scaled by NumCPU) that load1/load5 are compared against;
// the 5-minute average only needs to clear 75% of it, so a spike that's
// already cooling on the longer window doesn't extend the gate.
//
// Starvation escape: never gate when the cache is nearly empty (< 50
// entries) or after 6 consecutive skips (~6h at the default refresh
// interval), so a box that hovers at the threshold forever still slowly
// refreshes instead of never fetching again.
func shouldSkipProxyURLFetch(cacheSize, consecutiveSkips int, load1, load5, threshold float64, loadErr error) bool {
	if cacheSize < 50 || consecutiveSkips >= 6 {
		return false
	}
	if loadErr != nil {
		return false
	}
	return load1 >= threshold && load5 >= threshold*0.75
}

// readURLCacheSize reads the current URL proxy cache size from proxy_url.json.
// Returns 0 on error (fetch will not be gated).
func readURLCacheSize() int {
	release, err := acquireProxyLock()
	if err != nil {
		return 0
	}
	defer release()
	state, err := readProxyURLState()
	if err != nil {
		return 0
	}
	return len(state.Cache)
}

// runProxyURLCleanupOnce removes dead/inactive/degraded proxies whose source
// matches scope ("url" removes only url-sourced proxies; "all" removes any
// source; any other value, including "none", removes nothing and returns 0).
// When ProxyURLState.DegradedCleanupThreshold is set, URL-sourced proxies
// offline longer than that threshold are also removed. The threshold is
// read from proxy_url.json every cycle, so changes take effect without a
// restart. Returns the number of proxies removed.
func runProxyURLCleanupOnce(scope string) (removed int) {
	if scope != "url" && scope != "all" {
		return 0
	}

	state, err := readProxyState()
	if err != nil {
		tlog("[proxy][cleanup] warning: could not read proxy.state: %v\n", err)
		return 0
	}

	// For degraded cleanup, require the provider to have been running
	// long enough to avoid killing proxies that just haven't authed yet
	// during this startup cycle.
	uptime := time.Since(state.StartedAt)
	const minUptime = 65 * time.Minute

	// Read degraded threshold from proxy_url.json (may be empty = disabled).
	var degradedThreshold time.Duration
	if urlState, err := readProxyURLState(); err == nil && urlState.DegradedCleanupThreshold != "" {
		if d, err := time.ParseDuration(urlState.DegradedCleanupThreshold); err == nil && d > 0 {
			degradedThreshold = d
		}
	}

	addrsBySource := map[string][]string{}
	for addr, e := range state.Proxies {
		// Skip untagged entries (pre-source-tagging)
		if e.Source == "" {
			continue
		}
		if scope == "url" && e.Source != "url" {
			continue
		}

		// Dead: apply the same uptime guard as degraded to avoid evicting
		// proxies that simply haven't authed yet during warmup. The health
		// system reports "dead" before the first auth completes, so without
		// this guard the startup cleanup pass would mass-evict warming URL
		// proxies. Inactive (7+ days unseen) is always safe to remove.
		if e.Health == "dead" {
			if uptime > minUptime {
				addrsBySource[e.Source] = append(addrsBySource[e.Source], addr)
				removed++
			}
			continue
		}
		if e.Health == "inactive" {
			addrsBySource[e.Source] = append(addrsBySource[e.Source], addr)
			removed++
			continue
		}

		// Degraded: only remove if:
		//   1. A threshold is configured
		//   2. The provider has been running long enough (>65 min)
		//   3. DownSince is set AND the proxy has been down past the threshold
		// This prevents killing proxies that are still warming up or whose
		// DownSince is stale from a prior provider session.
		if degradedThreshold > 0 && uptime > minUptime &&
			(e.Health == "recently_offline" || e.Health == "offline" || e.Health == "long_offline") {
			if e.DownSince != "" {
				if ds, err := time.Parse(time.RFC3339, e.DownSince); err == nil && time.Since(ds) >= degradedThreshold {
					addrsBySource[e.Source] = append(addrsBySource[e.Source], addr)
					removed++
					continue
				}
			}
		}
	}

	if removed == 0 {
		return 0
	}

	if err := removeDeadProxies(state, addrsBySource); err != nil {
		tlog("[proxy][cleanup] warning: %v\n", err)
		return 0
	}
	tlog("[proxy][cleanup] automatically removed %d dead/inactive/degraded proxies (scope=%s, degraded_threshold=%s)\n", removed, scope, degradedThreshold)
	return removed
}

// runProxyURLCleanup runs runProxyURLCleanupOnce on a fixed interval until
// ctx is cancelled. When scope is "none" or another disabling value the
// ticker still runs so that runtime toggles (off→on) work live — the
// loop just skips the cleanup call until scope becomes active again.
func runProxyURLCleanup(ctx context.Context, scope string, interval time.Duration, selfHealEnabled bool) {
	activeScope := resolveProxyCleanupScope(scope)
	activeInterval := resolveProxyCleanupInterval(interval)

	ticker := time.NewTicker(activeInterval)
	defer ticker.Stop()

	// Run once immediately if scope is active. Gated by self-heal too — an
	// operator who set self-heal off before starting the provider must not
	// have that first pass run anyway.
	if resolveSelfHealEnabled(selfHealEnabled) && (activeScope == "url" || activeScope == "all") {
		runProxyURLCleanupOnce(activeScope)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Re-check runtime overrides on every tick
			if ns := resolveProxyCleanupScope(scope); ns != activeScope {
				activeScope = ns
				if activeScope == "url" || activeScope == "all" {
					tlog("[proxy][url] cleanup scope changed to %s\n", ns)
				} else {
					tlog("[proxy][url] cleanup disabled (scope=%s)\n", ns)
				}
			}
			if ni := resolveProxyCleanupInterval(interval); ni != activeInterval {
				ticker.Stop()
				ticker = time.NewTicker(ni)
				activeInterval = ni
				tlog("[proxy][url] cleanup interval changed to %s\n", ni)
			}
			if resolveSelfHealEnabled(selfHealEnabled) && (activeScope == "url" || activeScope == "all") {
				runProxyURLCleanupOnce(activeScope)
			}
		}
	}
}
