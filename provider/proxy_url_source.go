package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// resolveEffectiveProxyURLMax is the admit cap fetch cycles actually use:
// the configured ceiling, further limited by the AIMD-discovered target
// once the pool controller has established one. The target only constrains
// admission while self-heal is enabled; toggling self-heal off restores the
// configured ceiling immediately — a target discovered under transient
// pressure is not a durable truth about the box, and an operator who turns
// self-heal off has asked for stock behavior.
func resolveEffectiveProxyURLMax(startupMax int, selfHealEnabled bool) int {
	ceiling := resolveProxyURLMax(startupMax)
	if !resolveSelfHealEnabled(selfHealEnabled) {
		return ceiling
	}
	state, err := readProxyURLState()
	if err != nil || state.TargetPoolSize <= 0 {
		return ceiling
	}
	if ceiling == 0 || state.TargetPoolSize < ceiling {
		return state.TargetPoolSize
	}
	return ceiling
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
	release, err := acquireProxyLockWithRetry()
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

// desiredAddressesForHistoryPruning extends currentDesiredProxyAddresses
// with in-backoff addresses: a self-heal shed deletes the address from the
// URL cache (unlike a give-up, which leaves it in place), so without this a
// shed proxy's history would get pruned before its backoff even elapses.
func desiredAddressesForHistoryPruning() (map[string]bool, error) {
	addrs, err := currentDesiredProxyAddresses()
	if err != nil {
		return nil, err
	}
	for addr := range globalProxyFailureHistory.AddressesInBackoff(time.Now()) {
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
	// grades accumulates the stage-1 result per address so the merge loop
	// can persist score/failed alongside ProbeOK.
	grades := make(map[string]proxyURLGrade)
	// tierCounts breaks down this cycle's grades for the per-source log.
	tierCounts := map[string]int{}
	// countedGrade dedupes the breakdown by ADDRESS across sources: the same
	// address listed in two sources (bare + credentialed, or duplicated)
	// must count once, matching admittedByTier (coderabbit review).
	countedGrade := map[string]bool{}
	probeCfg := resolveProxyTableProbeConfig()
	tlog("[proxy][url] stage-1 table probe config: %s\n", describeProxyTableProbeConfig(probeCfg))
	// Advance the rotation once per FETCH CYCLE (not once per source URL):
	// with N sources, the same address probed from two sources in one cycle
	// must be graded against the same block, and consecutive cycles must
	// walk the table one block at a time (finding M1).
	tableProbePassCounter.Add(1)

	// Probe ONLY addresses not already cached. A cached proxy already has a
	// grade; re-probing it every fetch cycle is redundant because most don't
	// change, and at aggressive refresh (5-15m) it turns the probe itself
	// into a suspicious traffic pattern through the same egress IPs. Quality
	// refresh of cached entries is the reaper's job (its stale sweep
	// re-probes once-good entries and now refreshes the stage-1 grade). The
	// cache snapshot is read once up front; the merge re-reads it under the
	// lock later, so any race just means a proxy gets probed once more.
	cached := cachedProxyAddresses(mustReadProxyURLState())
	// probed is LIVE across sources: an address listed in source A AND
	// source B must be table-probed ONCE per cycle, not once per source —
	// the same address circulating across free lists is the norm, and
	// re-probing it per source defeats the new-only filter's whole purpose
	// (finding MEDIUM #2). The first source to list an address probes it;
	// every later source skips it, and its grade comes from that one pass.
	probed := map[string]bool{}
	skippedCached := 0
	for i, url := range urls {
		lines, err := fetchProxyURLLines(ctx, url)
		if err != nil {
			tlog("[proxy][url] fetch failed for %s: %v (skipping this cycle)\n", url, err)
			continue
		}
		// Free public proxy lists are mostly dead entries. The staged probe
		// checks TCP reachability, SOCKS5 protocol compliance, API CONNECT
		// (stage 0), and — for survivors — a sampled table probe of real
		// destinations (stage 1), before anything ever enters the cache or
		// consumes an auth-rate-limiter slot. Only stage-1-qualified proxies
		// (score >= pass bar) are admitted.
		var probeLines []string
		skippedThisSource := 0
		for _, line := range lines {
			addr, _, _, ok := parseProxyURLLine(line)
			if !ok {
				continue
			}
			if cached[addr] || probed[addr] {
				skippedCached++
				skippedThisSource++
				continue
			}
			probed[addr] = true
			probeLines = append(probeLines, line)
		}
		lineGrades := probeAndGradeProxyURLLines(ctx, probeLines, apiHost, apiPort, probeCfg)
		var qualified, belowBar, socks5Only []string
		for _, line := range lines {
			addr, _, _, ok := parseProxyURLLine(line)
			if !ok {
				continue
			}
			g, ok := lineGrades[addr]
			if !ok {
				continue // probeDead: dropped entirely
			}
			grades[addr] = g
			countThisAddr := !countedGrade[addr]
			countedGrade[addr] = true
			switch {
			case g.Qualified:
				qualified = append(qualified, line)
				if g.Decidable {
					if countThisAddr {
						tierCounts[proxyGradeTier(g.Score)]++
					}
				} else {
					// Kill-switch-disabled or otherwise ungraded but admitted.
					if countThisAddr {
						tierCounts["ungraded"]++
					}
				}
			case g.Socks5Only:
				socks5Only = append(socks5Only, line)
				if countThisAddr {
					tierCounts["socks5-only"]++
				}
			default:
				belowBar = append(belowBar, line)
				if g.Decidable {
					if countThisAddr {
						tierCounts[proxyGradeTier(g.Score)]++
					}
				} else {
					// No verdict this cycle (cancelled/DNS-down): the entry
					// keeps its prior grade and is simply not admitted now.
					if countThisAddr {
						tierCounts["undecidable"]++
					}
				}
			}
		}
		// Qualified lines are cached with ProbeOK=true; below-bar and
		// socks5-only lines with ProbeOK=false so the background reaper can
		// retry them (they may have had a transient routing issue). The
		// auth-time gate re-checks the recorded score, so a below-bar entry
		// that the reaper later revives still cannot spend auth slots.
		fetched[i] = append(append(qualified, belowBar...), socks5Only...)
		// N3 must measure against the NEW lines actually probed, not the
		// fetched count: in the steady state every address is cached, so
		// all three buckets are empty on the healthiest possible cycle, and
		// gating on len(lines) fires a false "all unparseable or dead" on
		// every source (finding MEDIUM #1).
		if len(qualified) == 0 && len(belowBar) == 0 && len(socks5Only) == 0 && len(probeLines) > 0 {
			// The fetch itself succeeded but every NEW line was unparseable
			// or dead — distinct from the fetch-failed case above (N3).
			tlog("[proxy][url] %s: fetched %d lines, %d new, all unparseable or dead\n", url, len(lines), len(probeLines))
		}
		tlog("[proxy][url] probed %s: %d/%d new qualified (%d cached, skipped), %d below-bar, %d socks5-only\n",
			url, len(qualified), len(probeLines), skippedThisSource, len(belowBar), len(socks5Only))
	}
	if skippedCached > 0 {
		// Cached-skip is the main efficiency change of this PR; the
		// operator should see how many addresses were skipped (coderabbit
		// review) — grade refresh is the reaper's job. One AGGREGATE line
		// after the loop: the counter is cumulative across sources, so
		// logging it inside the loop would read as per-source figures
		// (finding LOW).
		tlog("[proxy][url] skipped %d already-cached addresses this cycle (grade refresh is the reaper's job)\n", skippedCached)
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

	// Best-overall admission: pool every candidate across ALL sources, rank
	// by tier (A first), and merge in that order. The cache fills with the
	// highest-tier proxies first; lower tiers fill only remaining slots.
	// This is the A→B→C→D funnel — we add the best, not the first.
	rankAddr := func(addr string) int {
		if g, ok := grades[addr]; ok {
			return rankFromGrade(g)
		}
		return -1
	}
	gradeFor := func(addr string) (proxyURLGrade, bool) {
		g, ok := grades[addr]
		return g, ok
	}
	cands := collectRankedCandidates(fetched, grades)
	admittedByTier := map[string]int{}
	var admittedLines []string
	for _, c := range cands {
		admittedLines = append(admittedLines, c.line)
	}
	// apiOKCount is 0 because gradeFor decides ProbeOK for every candidate
	// (each admitted line has a grade): the pool is ordered by tier RANK,
	// not by Qualified, so the positional fallback (ProbeOK = i < apiOKCount)
	// would be wrong if a candidate ever reached the merge without a grade —
	// a kill-switch-disabled admission (Qualified=true, Decidable=false)
	// ranks last while a decidable F ranks first (coderabbit review).
	added := mergeProxyURLEntries(state, admittedLines, 0, maxTotal, rankAddr, gradeFor)
	totalAdded += added
	// admittedByTier counts what actually entered the cache this cycle, per
	// letter grade. The same address can appear in multiple sources' fetched
	// lists (bare + credentialed, or listed twice), so count each address
	// ONCE — the merge itself dedupes (existing addresses are skipped), and
	// the counters must match the cache, not the pooled line count (Sonnet
	// review finding).
	countedTier := map[string]bool{}
	for _, c := range cands {
		if countedTier[c.address] {
			continue
		}
		if _, exists := state.Cache[c.address]; exists && c.grade.Qualified {
			countedTier[c.address] = true
			if c.grade.Decidable {
				admittedByTier[proxyGradeTier(c.grade.Score)]++
			} else {
				// Kill-switch-disabled or otherwise ungraded but admitted:
				// count into the "ungraded" bucket so the important marker
				// still fires when stage-1 is off — that is exactly when an
				// operator debugging admission most needs the record
				// (finding LOW).
				admittedByTier["ungraded"]++
			}
		}
	}
	if len(admittedByTier) > 0 {
		parts := make([]string, 0, len(admittedByTier))
		for _, tier := range []string{"A", "B", "C", "D", "F", "ungraded"} {
			if n := admittedByTier[tier]; n > 0 {
				parts = append(parts, fmt.Sprintf("%s=%d", tier, n))
			}
		}
		importantLogf("[proxy][url] admitted by tier: %s (cap %d, total cached %d)\n",
			strings.Join(parts, " "), maxTotal, len(state.Cache))
	}

	markedAPI := 0
	markedSocks5 := 0
	// Dedupe by address so a cross-source duplicate is persisted and counted
	// once — the counters reflect distinct cached addresses, not pooled
	// lines (Sonnet review finding).
	seen := map[string]bool{}
	for _, c := range cands {
		if seen[c.address] {
			continue
		}
		seen[c.address] = true
		addr := c.address
		if entry, exists := state.Cache[addr]; exists {
			entry.LastProbe = time.Now()
			// Persist the stage-1 grade alongside ProbeOK so the
			// auth-time gate and fleet grading can consume it.
			//
			// Graded is set ONLY for a genuine stage-1 verdict
			// (g.Decidable). A socks5-only line never reached stage 1,
			// and a pass that could not ask anything (cancelled context,
			// resolver outage) produced no evidence — persisting either
			// as "graded, score 0.0" would convict a proxy the probe
			// never actually tested (findings C1 and C2). Such entries
			// keep their prior grade (or the ungraded state).
			if g, ok := grades[addr]; ok {
				applyProxyGradeToEntry(&entry, g, time.Now())
			}
			if c.grade.Qualified {
				entry.ProbeOK = true
				entry.ProbeFails = 0
				markedAPI++
			} else {
				entry.ProbeOK = false
				markedSocks5++
			}
			state.Cache[addr] = entry
			dirty = true
		}
	}
	if markedSocks5 > 0 || markedAPI > 0 {
		tlog("[proxy][url] %d qualified entries saved, %d below-bar/socks5-only entries marked for reaper\n", markedAPI, markedSocks5)
	}

	// Grade breakdown is printed every cycle that produced any grade, even
	// when nothing was newly added (N1: the old code's early return below
	// skipped it entirely on no-change cycles). Letters are what the probe
	// FOUND this cycle (A>=0.9 .. F<0.6), plus the non-grade buckets.
	if len(tierCounts) > 0 {
		parts := make([]string, 0, len(tierCounts))
		for _, tier := range []string{"A", "B", "C", "D", "F", "ungraded", "undecidable", "socks5-only"} {
			if n := tierCounts[tier]; n > 0 {
				parts = append(parts, fmt.Sprintf("%s=%d", tier, n))
			}
		}
		importantLogf("[proxy][url] probe grade breakdown: %s\n", strings.Join(parts, " "))
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

// capFailedList bounds the persisted Failed list so a wide sample_width
// cannot inflate the hot proxy_url.json file unboundedly (finding L4).
const maxFailedStored = 8

func capFailedList(hosts []string) []string {
	if len(hosts) <= maxFailedStored {
		return hosts
	}
	out := make([]string, maxFailedStored)
	copy(out, hosts[:maxFailedStored])
	return out
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
		// Publish the countdown BEFORE the pass so the first cycle is not
		// "unknown" and the estimate is not systematically late by the
		// pass duration (coderabbit minor + LOW-9).
		setNextGradeRefreshAt(time.Now().Add(proxyReaperInterval))
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
			staleAfter := reaperStaleThreshold(currentPressure())
			if !entry.LastProbe.IsZero() && time.Since(entry.LastProbe) < staleAfter {
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

	// Order the probe loop so the 32/cycle grade-refresh budget lands on the
	// genuinely oldest grades, not a random map-order slice (finding MEDIUM
	// #3). Liveness-only candidates (ProbeOK=false) do not spend refresh
	// budget, so they go first regardless of age. Among the stale once-good
	// candidates, oldest LastProbe first: when a fresh fill of the cache
	// births a herd of entries with synchronized LastProbe, this ordering
	// (plus skipping the re-stamp for budget losers, below) is what breaks
	// the herd — without it the same random 32 of 200 get refreshed every
	// stale window and the rest never desynchronize.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].wasProbeOK != candidates[j].wasProbeOK {
			return !candidates[i].wasProbeOK // liveness-only first, they don't spend budget
		}
		return candidates[i].entry.LastProbe.Before(candidates[j].entry.LastProbe)
	})

	// Probe every candidate outside the lock. Serial probing caps total
	// cycle time but no longer blocks concurrent proxy operations, which
	// was the critical problem — a 5-minute stale-lock age window would
	// let a reload steal the lock mid-cycle and race on proxy_url.json.
	// For once-good entries (stale re-probe), the stage-1 table probe also
	// runs so the QUALITY grade is refreshed, not just liveness — a proxy
	// that degraded (or recovered) gets its letter grade updated on the
	// stale cadence (~6h), giving live proxies a few re-probe chances a
	// day without hammering them every fetch cycle.
	probeCfg := resolveProxyTableProbeConfig()
	type probeResultEntry struct {
		addr       string
		result     probeResult
		wasProbeOK bool
		lastProbe  time.Time
		table      tableProbeResult
		// tableProbed records whether the stage-1 table probe actually ran
		// for this entry (as opposed to being skipped because the 32/cycle
		// refresh budget was exhausted). The apply step uses it to decide
		// whether to re-stamp LastProbe (finding MEDIUM #3).
		tableProbed bool
	}
	// Grade refresh is bounded per cycle: each stale once-good candidate
	// that passes stage 0 would otherwise run a serial table probe of
	// sample_width destinations, and a large cache could stretch the reaper
	// cycle past its own tick (coderabbit review). Liveness probing is
	// unaffected; only the quality refresh is capped. Candidates are sorted
	// above so the budget lands on the OLDEST grades first; budget losers
	// keep their stale LastProbe and are first in line next tick, so the
	// whole once-good cache is re-graded over a few cycles (finding MEDIUM
	// #3).
	const proxyReaperMaxGradeRefresh = 32
	refreshBudget := proxyReaperMaxGradeRefresh
	results := make([]probeResultEntry, 0, len(candidates))
	for _, c := range candidates {
		res := probeProxy(ctx, c.addr, c.entry.User, c.entry.Password, apiHost, apiPort)
		entry := probeResultEntry{
			addr:       c.addr,
			result:     res,
			wasProbeOK: c.wasProbeOK,
			lastProbe:  c.entry.LastProbe,
		}
		// Quality refresh on the stale re-probe of once-good entries: the
		// table probe runs on the same 1-3h stale cadence as liveness (the
		// reaperStaleCalm/Hot window). Upstream re-probes idle exits every
		// 10 minutes, so a 1-3h grade refresh is comfortably conservative —
		// a degraded proxy gets re-ranked a few times a day, never hammered.
		// Credentials are carried through so credentialed entries are
		// re-graded fairly.
		//
		// The refresh is gated on the kill switch, mirroring the fetch-side
		// invariant ("a full skip of the table probe, not just a gate
		// bypass"): with enabled:false the operator has turned stage-1 off
		// because the table probes themselves are the problem (egress abuse
		// detection), and the reaper must not keep running them on the stale
		// cadence (self-review finding).
		if c.wasProbeOK && res == probeAPIReachable && probeCfg.Enabled && refreshBudget > 0 {
			entry.table = probeTableThroughProxy(ctx, c.addr, c.entry.User, c.entry.Password, "", 0, probeCfg)
			entry.tableProbed = true
			refreshBudget--
		}
		results = append(results, entry)
	}

	// Re-acquire the lock and atomically apply all results.
	func() {
		release, err := acquireProxyLockWithRetry()
		if err != nil {
			return
		}
		defer release()

		state, err := readProxyURLState()
		if err != nil {
			return
		}

		changed := false
		// Grade-refresh counters for the per-cycle summary: the per-address
		// important line fires only on a LETTER TIER change, and one summary
		// line is emitted after the loop, so a large cache does not turn the
		// important buffer into a per-proxy stream (coderabbit review).
		refreshedGrades := 0
		tierChanges := 0
		for _, r := range results {
			entry, ok := state.Cache[r.addr]
			if !ok {
				continue // removed by a concurrent writer
			}
			if entry.LastProbe.After(r.lastProbe) {
				continue // updated by a concurrent fetch or another reaper cycle
			}
			if r.wasProbeOK {
				// Stale re-probe of a once-good entry: demote on failure,
				// or refresh timestamp on success.
				switch r.result {
				case probeAPIReachable:
					entry.ProbeOK = true
					entry.ProbeFails = 0
					// Quality refresh: the table probe re-graded the proxy
					// on the stale cadence. Persist the fresh grade so a
					// degraded proxy is re-ranked (and, if it fell below
					// the bar, is no longer spawned by mergeProxyURLCache),
					// and a recovered proxy gets a fair new chance. The
					// Decidable guard keeps C1/C2 semantics: an empty or
					// cancelled pass leaves the prior grade intact.
					if r.table.Decidable {
						oldTier := ""
						oldScore := entry.Score
						wasGraded := entry.Graded // capture BEFORE the write below (HIGH-1)
						if entry.Graded {
							oldTier = proxyGradeTier(entry.Score)
						}
						newTier := proxyGradeTier(r.table.Score)
						entry.Score = r.table.Score
						entry.Graded = true
						entry.Failed = capFailedList(r.table.Failed)
						// Genuine stage-1 refresh — advance the grade clock so
						// the hub dashboard's last_graded reflects this re-grade
						// (mirrors applyProxyGradeToEntry on the fetch path).
						entry.LastGraded = time.Now()
						refreshedGrades++
						if oldTier != newTier {
							tierChanges++
							importantLogf("[proxy][url] reaper: refreshed grade for %s -> %s (%.2f, %d/%d)\n",
								r.addr, newTier, r.table.Score, r.table.OK, r.table.SampleWidth)
							// Per-address delta line (grades.log history), matching
							// the paid-grader convention for URL proxies.
							emitProxyGradeDelta(r.addr, oldTier, newTier, oldScore, r.table.Score, wasGraded)
						}
					}
					// Re-stamp the staleness clock ONLY when the quality
					// refresh actually ran. A budget loser (stage-0 liveness
					// passed but the 32/cycle table-probe budget was
					// exhausted) keeps its stale LastProbe, so it sorts
					// first next tick and gets refreshed then — without
					// this, the once-good herd re-stamps as a block every
					// cycle and never desynchronizes (finding MEDIUM #3).
					if r.tableProbed {
						entry.LastProbe = time.Now()
					}
					state.Cache[r.addr] = entry
					changed = true
				case probeSocks5Only, probeDead:
					entry.LastProbe = time.Now()
					entry.ProbeOK = false
					entry.ProbeFails = 1
					state.Cache[r.addr] = entry
					tlog("[proxy][url] reaper: demoted %s from ProbeOK after stale re-probe\n", r.addr)
					changed = true
				case probeTLSFailed:
					// A once-good proxy that now fails TLS verification has
					// turned hostile (MITM/interceptor). Demote it on the
					// stale re-probe exactly like a dead/socks5-only result —
					// a hostile node must not stay in the pool even though
					// it was admitted earlier (no isLive escape exists on
					// this path by construction).
					entry.LastProbe = time.Now()
					entry.ProbeOK = false
					entry.ProbeFails = 1
					state.Cache[r.addr] = entry
					tlog("[proxy][url] reaper: demoted %s from ProbeOK after stale TLS-verify failure\n", r.addr)
					changed = true
				}
				continue
			}

			entry.LastProbe = time.Now()

			liveHealth := connect.ProxyHealthByAddress()
			isLive := false
			if h, ok := liveHealth[r.addr]; ok && h.Health == "up" {
				isLive = true
			}

			switch r.result {
			case probeAPIReachable:
				entry.ProbeOK = true
				entry.ProbeFails = 0
				state.Cache[r.addr] = entry
				changed = true

			case probeSocks5Only, probeDead:
				if isLive {
					entry.ProbeOK = true
					entry.ProbeFails = 0
					state.Cache[r.addr] = entry
					changed = true
					break
				}

				entry.ProbeOK = false
				entry.ProbeFails++
				state.Cache[r.addr] = entry
				if entry.ProbeFails >= proxyAPIMaxFails {
					if state.Blacklist == nil {
						state.Blacklist = map[string]time.Time{}
					}
					state.Blacklist[r.addr] = time.Now().UTC()
					delete(state.Cache, r.addr)
					reason := "socks5-only"
					if r.result == probeDead {
						reason = "dead"
					}
					tlog("[proxy][url] reaper: blacklisted %s (%s, %d fails)\n", r.addr, reason, entry.ProbeFails)
				}
				changed = true

			case probeTLSFailed:
				// TLS verification through the proxy failed — the proxy
				// intercepts/terminates TLS (MITM) instead of relaying it.
				// This is a hard rejection: a hostile node must not stay in
				// the pool even if it currently relays client traffic, so
				// unlike probeSocks5Only there is NO isLive escape here.
				entry.ProbeOK = false
				entry.ProbeFails++
				state.Cache[r.addr] = entry
				if entry.ProbeFails >= proxyAPIMaxFails {
					if state.Blacklist == nil {
						state.Blacklist = map[string]time.Time{}
					}
					state.Blacklist[r.addr] = time.Now().UTC()
					delete(state.Cache, r.addr)
					tlog("[proxy][url] reaper: blacklisted %s (tls-verify, %d fails)\n", r.addr, entry.ProbeFails)
				}
				changed = true
			}
		}

		if refreshedGrades > 0 {
			importantLogf("[proxy][url] reaper: refreshed grades for %d stale entries (%d tier changes)\n",
				refreshedGrades, tierChanges)
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
// The fetch interval stretches proportionally to system pressure (up to 8x
// at high pressure), so a drowning box adds probe/auth/reload work more
// slowly instead of stopping dead. A near-empty cache is never stretched,
// so a fresh box under load still bootstraps.
func runProxyURLFetcher(ctx context.Context, urls []string, refreshInterval time.Duration, maxTotal int, apiHost string, apiPort uint16, selfHealEnabled bool) {
	if len(urls) == 0 {
		// Publish why the countdown line will stay zero forever: nothing
		// to fetch (LA1 defect 5 — descriptive state, not "fetcher idle").
		setURLFetcherState("none")
		return
	}
	setURLFetcherState("pending")

	// Wait for file-proxy warmup to finish before the first fetch, so URL-
	// sourced proxies never compete for auth rate-limiter slots with the
	// operator-curated file proxies during the initial ramp.
	for !proxyWarmupDone.Load() {
		select {
		case <-ctx.Done():
			// CR #5: cancelled during warmup — publish a distinct
			// "inactive" state so countdownLine() doesn't keep showing
			// the stale "pending" it was set to above.
			setURLFetcherState("inactive")
			return
		case <-time.After(5 * time.Second):
		}
	}

	// Startup cooldown: a process that crash-loops (bad binary, SELinux
	// exec trap, OOM) must NOT re-fetch + re-probe the URL lists on every
	// 5s restart — that is the probe-amplification loop. The first fetch is
	// deferred until the process has been alive probeStartupCooldown, so a
	// crash-looping box can never reach it. A healthy box waits a few
	// seconds, then fetches once as normal.
	select {
	case <-ctx.Done():
		// CR #5: cancelled during the startup cooldown — publish a
		// distinct "inactive" state so countdownLine() doesn't keep
		// showing the stale "pending".
		setURLFetcherState("inactive")
		return
	case <-time.After(probeStartupCooldown):
	}

	// The initial fetch is always allowed (cold-start / starvation escape).
	fetchAndMergeProxyURLs(ctx, urls, resolveEffectiveProxyURLMax(maxTotal, selfHealEnabled), apiHost, apiPort)
	lastFetch := time.Now()

	activeInterval := resolveProxyURLRefresh(refreshInterval)
	// Publish the initial next-fetch estimate WITH the pressure stretch
	// applied, mirroring the loop's own math below (coderabbit minor: the
	// initial estimate was unstretched).
	initialCache := readURLCacheSize()
	initialNext := activeInterval
	if initialCache >= 50 {
		initialNext = time.Duration(float64(activeInterval) * fetchStretch(currentPressure()))
	}
	setNextFetchProbeAt(lastFetch.Add(initialNext))
	ticker := time.NewTicker(activeInterval)
	// A plain `defer ticker.Stop()` binds the ticker *value* at defer time;
	// once the loop below reassigns `ticker` on an interval change, that
	// defer only ever stops the original, now-discarded ticker. Deferring a
	// closure reads the current value of `ticker` when the function
	// actually returns.
	defer func() { ticker.Stop() }()
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

			// Pressure-proportional pacing: under load the effective interval
			// stretches up to 8× so a drowning box adds probe/auth/reload work
			// more slowly instead of stopping dead (old binary gate).
			// currentPressure() is 0 when self-heal is off ⇒ no stretching.
			pressure := currentPressure()
			cacheSize := readURLCacheSize()
			if !shouldFetchNow(time.Since(lastFetch), activeInterval, pressure, cacheSize) {
				tlog("[proxy][url] fetch deferred: pressure=%.2f stretch=%.1fx elapsed=%s cache=%d\n",
					pressure, fetchStretch(pressure), formatDuration(time.Since(lastFetch)), cacheSize)
				continue
			}
			lastFetch = time.Now()
			fetchAndMergeProxyURLs(ctx, urls, resolveEffectiveProxyURLMax(maxTotal, selfHealEnabled), apiHost, apiPort)
			// Publish the next-fetch estimate for the grade summary
			// countdown, mirroring shouldFetchNow's stretch math exactly
			// (reuse cacheSize read above — LOW-10) and anchored to
			// lastFetch, the same reference shouldFetchNow measures against
			// (coderabbit minor: it was anchored to a post-fetch now).
			effectiveNext := activeInterval
			if cacheSize >= 50 {
				effectiveNext = time.Duration(float64(activeInterval) * fetchStretch(currentPressure()))
			}
			setNextFetchProbeAt(lastFetch.Add(effectiveNext))
		}
	}
}

// shouldFetchNow decides whether enough time has passed since the last URL
// fetch, given the pressure-stretched effective interval. The starvation
// floor is kept from the old gate: a near-empty cache (<50) is never
// stretched, so a fresh box under load still bootstraps.
func shouldFetchNow(sinceLast, base time.Duration, pressure float64, cacheSize int) bool {
	// 1s slack absorbs ticker/handler scheduling jitter so a calm box is
	// never deferred a whole interval by landing microseconds short.
	const slack = time.Second
	if cacheSize < 50 {
		return sinceLast >= base-slack
	}
	effective := time.Duration(float64(base) * fetchStretch(pressure))
	return sinceLast >= effective-slack
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

// cleanupTickInterval is the fixed check cadence for runProxyURLCleanup.
// The actual cleanup cadence is the pressure-scaled effective interval
// computed each tick (see cleanupIntervalScale); this just bounds how often
// that effective interval is re-evaluated. Var (not const) so tests can
// shorten it instead of waiting on the real 15-minute cadence.
var cleanupTickInterval = 15 * time.Minute

// runProxyURLCleanup runs runProxyURLCleanupOnce on a pressure-scaled
// interval until ctx is cancelled: cleanup sheds load, so overload is when
// it should run MORE often, not less (6h base shrinks to 1h at pressure
// ≥0.8 — see cleanupIntervalScale). Checked every cleanupTickInterval. When
// scope is "none" or another disabling value the loop still runs so that
// runtime toggles (off→on) work live — it just skips the cleanup call until
// scope becomes active again.
func runProxyURLCleanup(ctx context.Context, scope string, interval time.Duration, selfHealEnabled bool) {
	lastRun := time.Time{} // zero value ⇒ immediate first pass, same gating as before
	ticker := time.NewTicker(cleanupTickInterval)
	defer ticker.Stop()
	for {
		effective := time.Duration(float64(resolveProxyCleanupInterval(interval)) * cleanupIntervalScale(currentPressure()))
		if time.Since(lastRun) >= effective {
			if activeScope := resolveProxyCleanupScope(scope); activeScope == "url" || activeScope == "all" {
				runProxyURLCleanupOnce(activeScope)
			}
			lastRun = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
