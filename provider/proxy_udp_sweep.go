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

// udpPaidDesiredSet never logs source lines: they may contain credentials.
// Any unreadable or malformed source leaves ownership unknown, so skip a tick.
func udpPaidDesiredSet(state *ProxyState) (map[string]*connect.ProxySettings, bool) {
	out := make(map[string]*connect.ProxySettings)
	if state.Source != "" {
		b, err := os.ReadFile(state.Source)
		if err != nil {
			return nil, false
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			address, user, password := parseProxyAddress(line)
			if user == "" || password == "" {
				return nil, false
			}
			ps := &connect.ProxySettings{Network: "tcp", Address: address, Auth: &proxy.Auth{User: user, Password: password}}
			out[ps.Key()] = ps
		}
		if len(out) == 0 {
			return nil, false
		}
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

type udpTickRunner func(context.Context, *proxyUDPCheckStore, []*connect.ProxySettings, time.Time, time.Duration, time.Duration, float64, bool)

func runURLUDPProbeTick(ctx context.Context) {
	runURLUDPProbeTickWith(ctx, runUDPProbeTick)
}

func runURLUDPProbeTickWith(ctx context.Context, run udpTickRunner) {
	if !resolveProxyTableProbeConfig().Enabled || ctx.Err() != nil {
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
	run(ctx, globalProxyUDPCheck, urlUDPProbeSettings(online, paid, url), time.Now(), udpProbeRecheckWindow, proxyReaperInterval, currentPressure(), true)
}

// runPaidUDPProbeTick resolves online identities only through the trusted
// desired set; a missing credential must never turn into an unauthenticated dial.
func runPaidUDPProbeTick(ctx context.Context) {
	runPaidUDPProbeTickWith(ctx, runUDPProbeTick)
}

func runPaidUDPProbeTickWith(ctx context.Context, run udpTickRunner) {
	if !resolveProxyTableProbeConfig().Enabled || ctx.Err() != nil {
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
		if ps := paid[key]; ps != nil {
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
	workers := min(udpProbeWorkers, scaledProbeConcurrency(pressure), cap(store.slots))
	if workers <= 0 {
		return
	}
	settingsByKey := make(map[string]*connect.ProxySettings, len(online))
	candidates := make([]udpProbeCandidate, 0, len(online))
	store.mu.Lock()
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
		if entry := store.entries[key]; entry != nil {
			if entry.done != nil {
				// An active check will publish a fresh answer itself.
				candidate.lastChecked = now
			} else {
				candidate.lastChecked = entry.at
				candidate.status = entry.result.Status
			}
		}
		candidates = append(candidates, candidate)
	}
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
				store.Check(ctx, settingsByKey[candidate.key], time.Now())
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
	changed     bool
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
		if candidate.lastChecked.IsZero() || !now.Before(candidate.lastChecked.Add(proxyUDPTTL(candidate.status))) {
			due = append(due, candidate)
		}
	}
	if len(due) == 0 {
		return nil
	}

	sort.Slice(due, func(i, j int) bool {
		a, b := due[i], due[j]
		if a.lastChecked.IsZero() != b.lastChecked.IsZero() {
			return a.lastChecked.IsZero()
		}
		if a.changed != b.changed {
			return a.changed
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
