package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

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
func fetchAndMergeProxyURLs(ctx context.Context, urls []string, maxTotal int) {
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
	for i, url := range urls {
		lines, err := fetchProxyURLLines(ctx, url)
		if err != nil {
			tlog("[proxy][url] fetch failed for %s: %v (skipping this cycle)\n", url, err)
			continue
		}
		// Free public proxy lists are mostly dead entries. Probing here, before
		// anything is ever merged into the cache, means a dead entry never gets
		// an auth attempt (or a slot from the shared auth rate limiter) in the
		// first place — instead of relying on 10 auth retries per dead proxy to
		// find that out the expensive way.
		reachable := filterReachableProxyURLLines(ctx, lines)
		tlog("[proxy][url] probed %s: %d/%d reachable\n", url, len(reachable), len(lines))
		fetched[i] = reachable
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
		_ = writeReloadTrigger(reloadPath)
	}
}

// runProxyURLFetcher periodically fetches configured proxy list URLs and
// merges new entries into the running proxy set. The first fetch runs
// immediately; subsequent fetches run every refreshInterval. Exits when ctx
// is cancelled. A no-op if urls is empty.
func runProxyURLFetcher(ctx context.Context, urls []string, refreshInterval time.Duration, maxTotal int) {
	if len(urls) == 0 {
		return
	}

	fetchAndMergeProxyURLs(ctx, urls, maxTotal)

	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fetchAndMergeProxyURLs(ctx, urls, maxTotal)
		}
	}
}

// runProxyURLCleanupOnce removes dead/inactive proxies whose source matches
// scope ("url" removes only url-sourced proxies; "all" removes any source;
// any other value, including "none", removes nothing and returns 0).
// Untagged entries (Source == "", from before this feature shipped) are
// never touched automatically. Returns the number of proxies removed.
func runProxyURLCleanupOnce(scope string) (removed int) {
	if scope != "url" && scope != "all" {
		return 0
	}

	state, err := readProxyState()
	if err != nil {
		tlog("[proxy][cleanup] warning: could not read proxy.state: %v\n", err)
		return 0
	}

	addrsBySource := map[string][]string{}
	for addr, e := range state.Proxies {
		if e.Health != "dead" && e.Health != "inactive" {
			continue
		}
		if e.Source == "" {
			continue
		}
		if scope == "url" && e.Source != "url" {
			continue
		}
		addrsBySource[e.Source] = append(addrsBySource[e.Source], addr)
		removed++
	}

	if removed == 0 {
		return 0
	}

	if err := removeDeadProxies(state, addrsBySource); err != nil {
		tlog("[proxy][cleanup] warning: %v\n", err)
		return 0
	}
	tlog("[proxy][cleanup] automatically removed %d dead/inactive proxies (scope=%s)\n", removed, scope)
	return removed
}

// runProxyURLCleanup runs runProxyURLCleanupOnce on a fixed interval until
// ctx is cancelled. A no-op (returns immediately without starting a ticker)
// when scope is "none" or any other disabling value — automatic cleanup is
// opt-in.
func runProxyURLCleanup(ctx context.Context, scope string, interval time.Duration) {
	if scope != "url" && scope != "all" {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runProxyURLCleanupOnce(scope)
		}
	}
}
