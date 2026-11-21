package main

import (
	"context"
	"encoding/json"
	"errors"
	"math/bits"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

const (
	udpProbeWorkers = 2
	// At full pressure the grade pipeline already runs at its one-worker
	// floor; defer this optional sweep to preserve capacity for that work.
	udpProbeSkipPressure  = 1.0
	udpProbeRecheckWindow = 24 * time.Hour
)

// An unreadable or empty source leaves ownership unknown, so skip a tick.
func udpPaidDesiredSet(state *ProxyState) (map[string]*connect.ProxySettings, bool) {
	out := make(map[string]*connect.ProxySettings)
	if state.Source != "" {
		settings, rejected, err := readClassifiedProxySource(state.Source)
		if err != nil || (len(settings) == 0 && rejected == 0) {
			return nil, false
		}
		for _, ps := range settings {
			out[ps.Key()] = ps
		}
		return out, true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(home, ".urnetwork", "proxy"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	if err == nil {
		var cfg ProxyConfig
		if json.Unmarshal(b, &cfg) != nil {
			return nil, false
		}
		for address, key := range cfg.Servers {
			ps := internalServerSettings(&cfg, address, key)
			if _, exists := out[ps.Key()]; !exists {
				out[ps.Key()] = ps
			}
		}
	}
	return out, true
}

// urlUDPProbeSettings resolves online URL identities, respecting primary-source
// address ownership. If ownership is uncertain, skip rather than use stale auth.
func urlUDPProbeSettings(online []string, paid map[string]*connect.ProxySettings, url *ProxyURLState) []*connect.ProxySettings {
	primaryAddrs := make(map[string]bool, len(paid))
	for _, settings := range paid {
		primaryAddrs[settings.Address] = true
	}
	byKey := make(map[string]*connect.ProxySettings, len(url.Cache))
	for addr, entry := range url.Cache {
		if primaryAddrs[addr] {
			continue
		}
		settings := &connect.ProxySettings{Network: "tcp", Address: addr}
		if entry.User != "" || entry.Password != "" {
			settings.Auth = &proxy.Auth{User: entry.User, Password: entry.Password}
		}
		byKey[settings.Key()] = settings
	}
	var result []*connect.ProxySettings
	for _, key := range online {
		if settings := byKey[key]; settings != nil {
			result = append(result, settings)
		}
	}
	return result
}

// udpProbeEnabled controls optional background sweeps, not client safety checks.
func udpProbeEnabled(state *controlState) bool {
	value, found := state.get("udp_probe")
	return !found || strings.EqualFold(value, "on")
}

// proxyIPv6ProbeEnabled controls whether IPv6 capability checks run during sweeps.
func proxyIPv6ProbeEnabled(state *controlState) bool {
	if state != nil {
		if value, found := state.get("proxy_ipv6_probe"); found {
			return strings.EqualFold(value, "on")
		}
	}
	if env := strings.TrimSpace(os.Getenv("URNETWORK_PROXY_IPV6_PROBE")); env != "" {
		return strings.EqualFold(env, "on") || env == "1" || strings.EqualFold(env, "true") || strings.EqualFold(env, "yes")
	}
	return true
}

type udpTickRunner func(context.Context, *proxyUDPCheckStore, []*connect.ProxySettings, time.Time, time.Duration, time.Duration, float64, bool)

// A UDP probe tick can take up to its whole interval, so it runs beside the
// grading or reaper pass it belongs to, never in front of it. One tick of each
// kind is in flight at a time: when the previous one is still running at the
// next interval, that interval skips it.
var paidUDPProbeRunning, urlUDPProbeRunning atomic.Bool

func startUDPProbeTick(running *atomic.Bool, ctx context.Context, run func(context.Context)) {
	if !running.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer running.Store(false)
		run(ctx)
	}()
}

func runURLUDPProbeTick(ctx context.Context) {
	runURLUDPProbeTickWith(ctx, runUDPProbeTick)
}

func runURLUDPProbeTickWith(ctx context.Context, run udpTickRunner) {
	if !udpProbeEnabled(globalControlState) || !resolveProxyTableProbeConfig().Enabled || ctx.Err() != nil {
		return
	}
	_, _, _, _, _, online, _ := connect.ProxyHealthSnapshotWithIdentities(trimHeldSnapshot())
	proxyStateMu.Lock()
	state, err := readProxyState()
	if err != nil {
		proxyStateMu.Unlock()
		return
	}
	paid, trusted := udpPaidDesiredSet(state)
	proxyStateMu.Unlock()
	if !trusted {
		return
	}
	release, err := acquireProxyLock()
	if err != nil {
		return
	}
	url, err := readProxyURLState()
	release()
	if err != nil {
		return
	}
	settings := urlUDPProbeSettings(online, paid, url)
	for _, ps := range settings {
		if entry, ok := state.Proxies[ps.Key()]; ok {
			ps.Index = entry.ID
		}
	}
	run(ctx, globalProxyUDPCheck, settings, time.Now(), udpProbeRecheckWindow, proxyReaperInterval, currentPressure(), true)
}

// runPaidUDPProbeTick resolves online identities only through the trusted
// desired set; a missing credential must never turn into an unauthenticated dial.
func runPaidUDPProbeTick(ctx context.Context) {
	runPaidUDPProbeTickWith(ctx, runUDPProbeTick)
}

func runPaidUDPProbeTickWith(ctx context.Context, run udpTickRunner) {
	if !udpProbeEnabled(globalControlState) || !resolveProxyTableProbeConfig().Enabled || ctx.Err() != nil {
		return
	}
	_, _, _, _, _, online, _ := connect.ProxyHealthSnapshotWithIdentities(trimHeldSnapshot())
	proxyStateMu.Lock()
	state, err := readProxyState()
	if err != nil {
		proxyStateMu.Unlock()
		return
	}
	paid, trusted := udpPaidDesiredSet(state)
	proxyStateMu.Unlock()
	if !trusted {
		return
	}
	settings := make([]*connect.ProxySettings, 0, len(online))
	for _, key := range online {
		if ps := paid[key]; ps != nil && ps.Network != "" {
			if entry, ok := state.Proxies[key]; ok {
				ps.Index = entry.ID
			}
			settings = append(settings, ps)
		}
	}
	run(ctx, globalProxyUDPCheck, settings, time.Now(), udpProbeRecheckWindow, proxyReaperInterval, currentPressure(), true)
}

// runUDPProbeTick checks a bounded batch and joins every worker before returning.
// The supplied settings are a snapshot of online identities, not state entries.
func runUDPProbeTick(ctx context.Context, store *proxyUDPCheckStore, online []*connect.ProxySettings, now time.Time, recheckWindow, tick time.Duration, pressure float64, enabled bool) {
	if !enabled || pressure >= udpProbeSkipPressure || ctx.Err() != nil {
		return
	}
	if tick > 0 {
		// A slow batch must not hold the reaper or grader past its own
		// interval. An interrupted check is forgotten by the store.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, tick)
		defer cancel()
	}
	workers := min(udpProbeWorkers, scaledProbeConcurrency(pressure), cap(store.slots))
	if workers <= 0 {
		return
	}
	settingsByKey := make(map[string]*connect.ProxySettings, len(online))
	candidates := make([]udpProbeCandidate, 0, len(online))
	store.mu.Lock()
	store.prune(now)
	for _, settings := range online {
		if settings == nil {
			continue
		}
		key := settings.Key()
		if _, exists := settingsByKey[key]; exists {
			continue
		}
		settingsByKey[key] = settings
		candidate := udpProbeCandidate{key: key, online: true}
		if entry := store.cachedEntryLocked(key); entry != nil {
			if entry.done != nil {
				// An active check will publish a fresh answer itself.
				candidate.lastChecked = now
			} else {
				candidate.lastChecked = entry.at
				candidate.status = entry.result.Status
				candidate.ipv6Due = proxyIPv6ProbeEnabled(globalControlState) && store.ipv6Probe != nil && entry.ipv6DueLocked(now)
			}
		}
		candidates = append(candidates, candidate)
	}
	store.prune(now)
	store.mu.Unlock()
	batch := selectUDPProbeBatch(now, candidates, recheckWindow, tick, rand.New(rand.NewSource(now.UnixNano())))
	workers = min(workers, len(batch))
	if workers == 0 {
		return
	}
	jobs := make(chan udpProbeCandidate)
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for candidate := range jobs {
				if ctx.Err() != nil {
					return
				}
				store.Check(withIPv6Sweep(ctx), settingsByKey[candidate.key], now)
			}
		}()
	}
	for _, candidate := range batch {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		case jobs <- candidate:
		}
	}
	close(jobs)
	wg.Wait()
}

type udpProbeCandidate struct {
	key         string
	online      bool
	status      proxyUDPStatus
	lastChecked time.Time
	ipv6Due     bool
}

// selectUDPProbeBatch selects due online candidates without modifying the input.
// Invalid intervals return no batch. A nil rng disables jitter. Jitter is a
// whole number of candidates, up to floor(20% of the unjittered batch size).
func selectUDPProbeBatch(now time.Time, candidates []udpProbeCandidate, recheckWindow, tick time.Duration, rng *rand.Rand) []udpProbeCandidate {
	if recheckWindow <= 0 || tick <= 0 {
		return nil
	}
	var due []udpProbeCandidate
	online := 0
	for _, candidate := range candidates {
		if !candidate.online {
			continue
		}
		online++
		refreshWindow := proxyUDPTTL(candidate.status) / 2
		if candidate.ipv6Due || candidate.lastChecked.IsZero() || !now.Before(candidate.lastChecked.Add(refreshWindow)) {
			due = append(due, candidate)
			if !candidate.lastChecked.IsZero() && refreshWindow < recheckWindow {
				recheckWindow = refreshWindow
			}
		}
	}
	if len(due) == 0 {
		return nil
	}

	// an ipv6 retry goes first: it runs on its own interval, which is far
	// shorter than the udp recheck cycle, so leaving it in lastChecked order
	// would hold it for most of a cycle. The batch size and the tick pacing are
	// untouched; only the order inside the budget changes.
	sort.Slice(due, func(i, j int) bool {
		a, b := due[i], due[j]
		// an identity that was never checked goes first: IPv6 retries can hold
		// every slot of a small batch, and it leaves this group after one check,
		// so it cannot starve them in turn
		if a.lastChecked.IsZero() != b.lastChecked.IsZero() {
			return a.lastChecked.IsZero()
		}
		if a.ipv6Due != b.ipv6Due {
			return a.ipv6Due
		}
		if !a.lastChecked.Equal(b.lastChecked) {
			return a.lastChecked.Before(b.lastChecked)
		}
		return a.key < b.key
	})

	// ceil(online * tick / recheckWindow), without overflow or float rounding.
	hi, lo := bits.Mul64(uint64(online), uint64(tick))
	// At twice the online count, even maximum negative jitter covers all due.
	if hi >= uint64(recheckWindow) {
		return due
	}
	quotient, remainder := bits.Div64(hi, lo, uint64(recheckWindow))
	if quotient >= 2*uint64(online) {
		return due
	}
	if remainder != 0 {
		quotient++
	}
	// Counts this large cannot be represented as an int or allocated in a
	// candidate slice. The available due candidates are the practical limit.
	if quotient > uint64(^uint(0)>>1) {
		return due
	}
	batch := int(quotient)
	if rng != nil {
		span := batch / 5
		if span > 0 {
			delta := rng.Intn(2*span+1) - span
			if delta > 0 && delta > len(due)-batch {
				batch = len(due)
			} else {
				batch += delta
			}
		}
	}
	if batch < 1 {
		batch = 1
	}
	if batch > len(due) {
		batch = len(due)
	}
	return due[:batch]
}
