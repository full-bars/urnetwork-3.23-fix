package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"

	"github.com/urnetwork/connect"
)

// parkProxies cancels each address in toPark that is still eligible at the
// moment of the cancel, and returns the addresses it actually parked.
//
// It follows reapProxies: the candidate list came from a snapshot that is
// stale by now (the proxy may have picked up sessions or been relaunched), so
// stillEligible is re-checked under the SAME lock as the cancel and delete so
// nothing can change between the check and the act.
//
// The cancel-map entry MUST be deleted. A paid proxy's goroutine does not
// remove its own entry on exit, so cancelling without deleting would leave the
// address looking "running" forever and reload would never relaunch it.
// CancelFunc is idempotent, so a second canceller (reaper, trim, drain) is
// harmless.
//
// onParked, when set, runs for each parked address while the lock is still
// held. The caller puts the backoff there: a reload that runs between the
// cancel and the backoff would see the address not running and eligible, and
// relaunch what was just parked. It must not take proxyStateMu or the
// cross-process proxy lock, which stay outside the cancel lock.
func parkProxies(toPark []string, cancelMap map[string]context.CancelFunc, cancelMu *sync.Mutex, stillEligible func(addr string) bool, onParked func(addr string)) []string {
	var parked []string
	for _, addr := range toPark {
		// One function per address so the lock is released by defer: a panic in
		// a callback must not leave reload and the reaper blocked on it.
		func() {
			cancelMu.Lock()
			defer cancelMu.Unlock()
			if !stillEligible(addr) {
				return
			}
			cancel, ok := cancelMap[addr]
			if !ok {
				return
			}
			cancel()
			delete(cancelMap, addr)
			parked = append(parked, addr)
			if onParked != nil {
				onParked(addr)
			}
		}()
	}
	return parked
}

// proxyAuditEnv is everything proxy audit reads or does in the live process,
// behind function fields so runOnce can be tested with fakes and no registry.
type proxyAuditEnv struct {
	now func() time.Time
	// readPaid returns the tracked paid/file proxies from proxy.state, plus a
	// credential fingerprint per address (see proxyAuditCredFingerprint). ok is
	// false when the paid set cannot be trusted (source file unreadable or
	// empty mid-edit): proxy audit then does nothing this tick.
	readPaid func() (entries map[string]ProxyEntry, creds map[string]string, ok bool)
	// health is the LIVE registry. Running must come from here, never from
	// proxy.state Health, which goes stale after a park.
	health func() map[string]connect.ProxyHealthStatus
	// clients is the active client count; known is false when the bandwidth
	// registry has no entry (unknown is not idle).
	clients        func(addr string) (n int64, known bool)
	earningsScore  func(addr string, now time.Time) float64
	earnedRecently func(addr string) bool
	trackerWarm    func() bool
	// act is true when parks should be executed (self-heal on and hot restart
	// on). False means observe: log would-park, touch nothing, and give back
	// anything already parked.
	act func() bool
	// notActingReason says why act is false (one of proxy auditReason values);
	// optional, empty when unknown.
	notActingReason func() string
	markParked      func(addrs []string)
	rand            func() float64
	// failureHistory carries the relaunch backoff the reload path enforces.
	failureHistory *proxyFailureHistory
	cancelMap      map[string]context.CancelFunc
	cancelMu       *sync.Mutex
	log            func(format string, args ...any)
}

// proxyAuditor drives proxyAuditTick against the live process. Not safe for
// concurrent runOnce calls; the ticker loop is its only caller.
type proxyAuditor struct {
	cfg proxyAuditConfig
	st  *proxyAuditState
	env proxyAuditEnv

	// mu serializes runOnce with control-socket audit actions (on, off,
	// release). The ticker loop is the scheduled caller, but the control
	// actions mutate the same state (parks, announced, paused), so they
	// must not run concurrently with a tick.
	mu sync.Mutex

	// announced is the set of proxies already logged as would-park, so observe
	// mode says it once per newly eligible proxy rather than every tick.
	announced map[string]bool
	// degraded remembers the last thin/distrusted note so it logs on change.
	degraded string
	// pausedSince is when the paid proxy list last became untrustworthy; zero
	// while it is trusted. Proxy audit does nothing while paused.
	pausedSince time.Time
}

func newProxyAuditor(cfg proxyAuditConfig, env proxyAuditEnv) *proxyAuditor {
	return &proxyAuditor{cfg: cfg, st: newProxyAuditState(), env: env, announced: map[string]bool{}}
}

// runOnce performs one audit tick, serialized against control actions.
func (g *proxyAuditor) runOnce() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.runOnceLocked()
}

// runOnceLocked is the tick body; callers must hold g.mu.
func (g *proxyAuditor) runOnceLocked() {
	now := g.env.now()
	entries, creds, ok := g.env.readPaid()
	act := g.env.act()
	if !act {
		// Observe mode, or hot restart off: hand back everything we took. This
		// comes before the paid-set check so switching self-heal off is honoured
		// even while the paid list is unreadable.
		if released := g.st.releaseAll(); len(released) > 0 {
			for _, addr := range released {
				g.releaseBackoff(addr)
			}
			g.env.log("[proxy][audit] released %d parked proxies (audit is not acting)\n", len(released))
		}
	}
	if !ok {
		// The paid list is what proves a proxy is paid-owned, so without it the
		// audit cannot tell what it may park. Do nothing, but say so.
		if g.pausedSince.IsZero() {
			g.pausedSince = now
			g.env.log("[proxy][audit] paused: the paid proxy list is unreadable or empty, so proxy audit cannot tell which proxies it may park; parking nothing until it can\n")
		}
		g.st.keepTime(now)
		g.publish(now, act, proxyAuditResult{})
		return
	}
	if !g.pausedSince.IsZero() {
		g.env.log("[proxy][audit] resumed after %s\n", now.Sub(g.pausedSince).Round(time.Second))
		g.pausedSince = time.Time{}
	}

	health := g.env.health()
	addrs := make([]string, 0, len(entries))
	for addr := range entries {
		addrs = append(addrs, addr)
	}
	sort.Strings(addrs)

	proxies := make([]proxyAuditProxy, 0, len(addrs))
	for _, addr := range addrs {
		h, has := health[addr]
		n, known := g.env.clients(addr)
		proxies = append(proxies, proxyAuditProxy{
			Addr:           addr,
			Entry:          entries[addr],
			Running:        has && h.Health == "up",
			BandwidthKnown: known,
			Idle:           known && n == 0,
			EarningsScore:  g.env.earningsScore(addr, now),
			EarnedRecently: g.env.earnedRecently(addr),
			Cred:           creds[addr],
		})
	}

	res := proxyAuditTick(g.cfg, g.st, proxyAuditInput{
		Now:         now,
		Proxies:     proxies,
		Act:         act,
		TrackerWarm: g.env.trackerWarm(),
	})

	for _, addr := range res.Release {
		g.releaseBackoff(addr)
		if _, listed := entries[addr]; listed {
			g.env.log("[proxy][audit] restored %s\n", addr)
		} else {
			g.env.log("[proxy][audit] released %s (no longer in the paid proxy list)\n", addr)
		}
	}

	note := ""
	switch {
	case res.Distrusted:
		note = "distrusted"
	case res.Thin:
		note = "thin"
	}
	if note != g.degraded {
		g.degraded = note
		switch note {
		case "distrusted":
			g.env.log("[proxy][audit] correlated failure: a large share of fresh grades came back bad at once; treating this pass as a box-level problem and parking nothing\n")
		case "thin":
			g.env.log("[proxy][audit] too few proxies could be evaluated from this box; parking nothing\n")
		}
	}

	if !act {
		g.observe(res, proxies)
	} else {
		g.announced = map[string]bool{}
		g.remarkLost(entries, proxies)
		g.park(now, res, proxies)
	}
	g.publish(now, act, res)
}

func (g *proxyAuditor) observe(res proxyAuditResult, proxies []proxyAuditProxy) {
	score := map[string]float64{}
	for _, p := range proxies {
		score[p.Addr] = p.Entry.Score
	}
	current := map[string]bool{}
	for _, p := range res.Park {
		current[p.Addr] = true
		if !g.announced[p.Addr] {
			g.env.log("[proxy][audit] would-park %s score=%.2f\n", p.Addr, score[p.Addr])
		}
	}
	g.announced = current
}

func (g *proxyAuditor) park(now time.Time, res proxyAuditResult, proxies []proxyAuditProxy) {
	if len(res.Park) == 0 {
		return
	}
	score := map[string]float64{}
	for _, p := range proxies {
		score[p.Addr] = p.Entry.Score
	}
	toPark := make([]string, 0, len(res.Park))
	for _, p := range res.Park {
		toPark = append(toPark, p.Addr)
	}
	until := map[string]time.Time{}
	parked := parkProxies(toPark, g.env.cancelMap, g.env.cancelMu, g.stillEligible, func(addr string) {
		u := g.st.commitPark(g.cfg, addr, now, g.env.rand())
		g.env.failureHistory.ExtendBackoffUntil(addr, u)
		until[addr] = u
	})
	for _, addr := range parked {
		g.env.log("[proxy][audit] parked %s score=%.2f until=%s\n", addr, score[addr], until[addr].Format(time.RFC3339))
	}
	if len(parked) > 0 {
		g.env.markParked(parked)
	}
}

// remarkLost re-asserts the parked mark on parks from earlier ticks whose
// proxy.state Health no longer says parked. The mark is best effort (the proxy
// lock can be busy) and the heartbeat writes a still-registered proxy back as
// up until its old goroutine unregisters, so a single mark is not enough. It
// reads the snapshot the tick already took and only writes when something is
// wrong, since every mark takes the cross-process proxy lock.
func (g *proxyAuditor) remarkLost(entries map[string]ProxyEntry, proxies []proxyAuditProxy) {
	running := make(map[string]bool, len(proxies))
	for _, p := range proxies {
		running[p.Addr] = p.Running
	}
	var lost []string
	for _, addr := range g.st.parkedAddrs() {
		// A running proxy is not parked, whatever the record says: something
		// relaunched it, and marking it parked would make proxy.state undercount
		// what is running. (Right after a park the old goroutine can still read
		// as up for a moment; the next tick re-marks it.)
		if running[addr] {
			continue
		}
		if e, ok := entries[addr]; ok && e.Health != proxyHealthParked {
			lost = append(lost, addr)
		}
	}
	if len(lost) > 0 {
		g.env.markParked(lost)
	}
}

// releaseBackoff gives an address back by clearing the backoff proxy audit set
// for it, and nothing else. A longer backoff someone else placed on the address
// (trim shed, URL give-up) stays.
func (g *proxyAuditor) releaseBackoff(addr string) {
	if until, ok := g.st.takeOwnBackoff(addr); ok {
		g.env.failureHistory.ReleaseBackoff(addr, until)
	}
}

// stillEligible re-checks, at the moment of the cancel and under the cancel
// lock, the vetoes that could have changed since the snapshot: the proxy must
// still be up, its bandwidth known and idle, and it must not have earned.
func (g *proxyAuditor) stillEligible(addr string) bool {
	if h, ok := g.env.health()[addr]; !ok || h.Health != "up" {
		return false
	}
	if n, known := g.env.clients(addr); !known || n != 0 {
		return false
	}
	return !g.env.earnedRecently(addr)
}

// proxyAuditTickInterval is how often proxy audit evaluates. Grades only change
// every few hours, so this mostly costs a cheap snapshot; it stays short so a
// self-heal toggle is honoured within minutes.
const proxyAuditTickInterval = 5 * time.Minute

// proxyAuditActEnabled reports whether proxy audit should execute parks: the
// proxy audit switch (live via `urnet-tools proxy audit on|off`, default
// off) AND hot restart. With hot restart off every relaunch mints a fresh
// client identity, so parking would churn identities instead of resting a
// proxy. When false proxy audit only observes.
func proxyAuditActEnabled(startupProxyAudit bool) bool {
	return resolveProxyAuditEnabled(startupProxyAudit) && hotRestartEnabled()
}

// The two reasons proxy audit can be observing only. They are machine-stable
// strings carried on the control socket; `urnet-tools` turns them into advice.
const (
	auditReasonAuditOff      = "audit-off"
	auditReasonHotRestartOff = "hot-restart-off"
)

// proxyAuditNotActingReason says why proxyAuditActEnabled is false, or "" when the
// audit acts. The fixes differ (proxy audit on, or hot-restart on), so the
// status has to name the one that applies.
func proxyAuditNotActingReason(startupProxyAudit bool) string {
	switch {
	case !resolveProxyAuditEnabled(startupProxyAudit):
		return auditReasonAuditOff
	case !hotRestartEnabled():
		return auditReasonHotRestartOff
	}
	return ""
}

// proxyAuditTrackerWarm is true once the in-memory earn tracker has watched at
// least two earn windows. Right after a restart it knows nothing, so "no
// recent earnings" would be meaningless.
func proxyAuditTrackerWarm(start, now time.Time) bool {
	return now.Sub(start) >= 2*paidEarnWindow
}

// liveReadPaid returns the tracked paid/file proxies from proxy.state. ok is
// false when the paid set cannot be trusted (state unreadable, or a source
// file that is unreadable or empty mid-edit), so the caller does nothing.
func liveReadPaid() (map[string]ProxyEntry, map[string]string, bool) {
	proxyStateMu.Lock()
	defer proxyStateMu.Unlock()
	state, err := readProxyState()
	if err != nil {
		return nil, nil, false
	}
	paid, fileOK := paidDesiredSet(state)
	if !fileOK {
		return nil, nil, false
	}
	out := make(map[string]ProxyEntry, len(paid))
	creds := make(map[string]string, len(paid))
	for addr, entry := range state.Proxies {
		if s, ok := paid[addr]; ok {
			out[addr] = entry
			creds[addr] = proxyAuditCredFingerprint(s.Auth)
		}
	}
	return out, creds, true
}

// proxyAuditCredFingerprint is an opaque, stable stand-in for a proxy's
// credentials, taken from the same desired set the grader validates its
// results against. An address is a proxy's identity, so re-pasting the same
// host:port with new credentials (the LA7 incident, 2026-09-18) is a rotation
// proxy audit must notice: what it learned about the old credentials says
// nothing about the new ones. Near-identical endpoints (same host, another
// port, or another credential) are different addresses and never share state.
//
// Length-prefixing keeps ("ab","c") and ("a","bc") apart, and the hash keeps
// the secret out of every log line and status output. No credentials, or
// empty ones, fingerprint as "".
func proxyAuditCredFingerprint(a *proxy.Auth) string {
	if a == nil || (a.User == "" && a.Password == "") {
		return ""
	}
	sum := sha256.Sum256([]byte(strconv.Itoa(len(a.User)) + ":" + a.User + ":" + a.Password))
	return hex.EncodeToString(sum[:8])
}

// markProxiesParked sets Health to proxyHealthParked on the named tracked
// entries in proxy.state, leaving grade fields alone. The heartbeat writer only
// updates entries present in the live registry, so without a mark a parked
// proxy would read "up" forever and be counted as running and ranked shed-last
// by trim. The value is deliberately NOT "inactive": that means "unseen 7+
// days, safe to remove", and dead-proxy cleanup and `proxy remove-dead` both
// collect it and edit the operator's proxy file. A parked proxy is resting,
// not dead, and must survive both. Takes the cross-process lock and
// proxyStateMu, like the heartbeat writer.
func markProxiesParked(addrs []string) {
	if len(addrs) == 0 {
		return
	}
	release, err := acquireProxyLockWithRetry()
	if err != nil {
		tlog("[proxy][audit] warn: could not mark parked proxies: %v\n", err)
		return
	}
	defer release()
	proxyStateMu.Lock()
	defer proxyStateMu.Unlock()
	state, err := readProxyState()
	if err != nil {
		tlog("[proxy][audit] warn: could not read proxy.state to mark parked proxies: %v\n", err)
		return
	}
	for _, addr := range addrs {
		if entry, ok := state.Proxies[addr]; ok {
			entry.Health = proxyHealthParked
			state.Proxies[addr] = entry
		}
	}
	if err := writeProxyState(state); err != nil {
		tlog("[proxy][audit] warn: state write failed: %v\n", err)
	}
}

// newLiveProxyAuditor wires a proxyAuditor to the real registries. start is
// the process start, used both as the memory boundary (grades stamped earlier
// are not evidence) and for the tracker warm-up.
func newLiveProxyAuditor(cancelMap map[string]context.CancelFunc, cancelMu *sync.Mutex, startupProxyAudit bool, start time.Time) *proxyAuditor {
	g := newProxyAuditor(defaultProxyAuditConfig(), proxyAuditEnv{
		now:      time.Now,
		readPaid: liveReadPaid,
		health:   connect.ProxyHealthByAddress,
		clients: func(addr string) (int64, bool) {
			bw := connect.ProxyBandwidthByAddress(addr)
			if bw == nil {
				return 0, false
			}
			return bw.Clients.Load(), true
		},
		earningsScore: proxyEarningsScore,
		earnedRecently: func(addr string) bool {
			return globalPerProxyEarnTracker.EarnedSince(addr, paidEarnWindow)
		},
		trackerWarm:     func() bool { return proxyAuditTrackerWarm(start, time.Now()) },
		act:             func() bool { return proxyAuditActEnabled(startupProxyAudit) },
		notActingReason: func() string { return proxyAuditNotActingReason(startupProxyAudit) },
		markParked:      markProxiesParked,
		rand:            rand.Float64,
		failureHistory:  globalProxyFailureHistory,
		cancelMap:       cancelMap,
		cancelMu:        cancelMu,
		log:             tlog,
	})
	g.st.notBefore = start
	return g
}

// runProxyAudit is proxy audit's ticker loop.
func runProxyAudit(ctx context.Context, cancelMap map[string]context.CancelFunc, cancelMu *sync.Mutex, startupProxyAudit bool) {
	g := newLiveProxyAuditor(cancelMap, cancelMu, startupProxyAudit, time.Now())
	currentProxyAuditor.Store(g)
	defer currentProxyAuditor.Store(nil)
	// Say whether proxy audit will act right away: the first tick is minutes
	// off, and until then /metrics would read "not acting" on every restart.
	g.publish(g.env.now(), g.env.act(), proxyAuditResult{})
	ticker := time.NewTicker(proxyAuditTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		g.runOnce()
	}
}

var currentProxyAuditor atomic.Pointer[proxyAuditor]
var proxyAuditOverride atomic.Pointer[bool]

// setProxyAuditOverride sets an in-memory runtime override for whether proxy audit is enabled.
func setProxyAuditOverride(enabled bool) {
	proxyAuditOverride.Store(&enabled)
}

// clearProxyAuditOverride clears any in-memory runtime override.
func clearProxyAuditOverride() {
	proxyAuditOverride.Store(nil)
}

// proxyAuditOverridePath returns ~/.urnetwork/proxy_audit
func proxyAuditOverridePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "proxy_audit"), nil
}

// resolveProxyAuditEnabled reports whether proxy audit is active, checking
// the in-memory override first, then the control-socket state ("proxy_audit"),
// then the override file (~/.urnetwork/proxy_audit), then startupEnabled
// (from URNETWORK_PROXY_AUDIT).
func resolveProxyAuditEnabled(startupEnabled bool) bool {
	if ov := proxyAuditOverride.Load(); ov != nil {
		return *ov
	}
	if v, ok := globalControlState.get("proxy_audit"); ok {
		return strings.EqualFold(v, "on") || v == "1" || strings.EqualFold(v, "true")
	}
	path, err := proxyAuditOverridePath()
	if err == nil {
		if data, err := os.ReadFile(path); err == nil {
			trimmed := strings.TrimSpace(string(data))
			if trimmed != "" {
				return strings.EqualFold(trimmed, "on") || trimmed == "1" || strings.EqualFold(trimmed, "true")
			}
		}
	}
	return startupEnabled
}
