package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
		// Socks5-only lines are cached with ProbeOK=false so the background
		// reaper can retry them; they may have had a transient routing issue.
		fetched[i] = append(apiOK, socks5Only...)
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
	for i, url := range urls {
		if fetched[i] == nil {
			continue
		}
		added := mergeProxyURLEntries(state, fetched[i], maxTotal)
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
					}
				}
			}
			tlog("[proxy][url] %s: %d socks5-only entries marked for reaper\n", url, marked)
		}
		tlog("[proxy][url] fetched %s: +%d new proxies\n", url, added)
	}

	if totalAdded == 0 {
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

		release, err := acquireProxyLock()
		if err != nil {
			continue
		}

		state, err := readProxyURLState()
		if err != nil {
			release()
			continue
		}

		changed := false
		for addr, entry := range state.Cache {
			if entry.ProbeOK {
				continue
			}
			// Don't re-probe if recently tested (within one reaper interval)
			if !entry.LastProbe.IsZero() && time.Since(entry.LastProbe) < proxyReaperInterval {
				continue
			}

			result := probeProxy(ctx, addr, apiHost, apiPort)
			entry.LastProbe = time.Now()

			switch result {
			case probeAPIReachable:
				entry.ProbeOK = true
				entry.ProbeFails = 0
				state.Cache[addr] = entry
				changed = true

			case probeSocks5Only:
				entry.ProbeOK = false
				entry.ProbeFails++
				state.Cache[addr] = entry
				if entry.ProbeFails >= proxyAPIMaxFails {
					if state.Blacklist == nil {
						state.Blacklist = map[string]time.Time{}
					}
					state.Blacklist[addr] = time.Now().UTC()
					delete(state.Cache, addr)
					tlog("[proxy][url] reaper: blacklisted %s after %d failed probes\n", addr, entry.ProbeFails)
				}
				changed = true

			case probeDead:
				entry.ProbeFails++
				state.Cache[addr] = entry
				if entry.ProbeFails >= proxyAPIMaxFails {
					if state.Blacklist == nil {
						state.Blacklist = map[string]time.Time{}
					}
					state.Blacklist[addr] = time.Now().UTC()
					delete(state.Cache, addr)
					tlog("[proxy][url] reaper: blacklisted %s (dead, %d fails)\n", addr, entry.ProbeFails)
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

		release()
	}
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
func runProxyURLFetcher(ctx context.Context, urls []string, refreshInterval time.Duration, maxTotal int, apiHost string, apiPort uint16) {
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

	fetchAndMergeProxyURLs(ctx, urls, resolveProxyURLMax(maxTotal), apiHost, apiPort)

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
			fetchAndMergeProxyURLs(ctx, urls, resolveProxyURLMax(maxTotal), apiHost, apiPort)
		}
	}
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

		// Dead/inactive: always remove (existing behavior).
		// These never authed (dead) or haven't been seen in 7+ days
		// (inactive) — no uptime guard needed.
		if e.Health == "dead" || e.Health == "inactive" {
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
// ctx is cancelled. A no-op (returns immediately without starting a ticker)
// when scope is "none" or any other disabling value — automatic cleanup is
// opt-in.
func runProxyURLCleanup(ctx context.Context, scope string, interval time.Duration) {
	activeScope := resolveProxyCleanupScope(scope)
	activeInterval := resolveProxyCleanupInterval(interval)
	if activeScope != "url" && activeScope != "all" {
		return
	}

	runProxyURLCleanupOnce(activeScope)

	ticker := time.NewTicker(activeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Re-check runtime overrides on every tick
			if ns := resolveProxyCleanupScope(scope); ns != activeScope {
				activeScope = ns
				if activeScope != "url" && activeScope != "all" {
					tlog("[proxy][url] cleanup disabled (scope=%s)\n", ns)
					return
				}
				tlog("[proxy][url] cleanup scope changed to %s\n", ns)
			}
			if ni := resolveProxyCleanupInterval(interval); ni != activeInterval {
				ticker.Stop()
				ticker = time.NewTicker(ni)
				activeInterval = ni
				tlog("[proxy][url] cleanup interval changed to %s\n", ni)
			}
			runProxyURLCleanupOnce(activeScope)
		}
	}
}
