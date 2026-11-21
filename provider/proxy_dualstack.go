package main

// IPv6 platform transport of a proxied identity.
//
// The platform decides a provider's address family from its platform
// connections (a connection declares a family and the platform observes the
// family it arrived on). A proxied identity holds one family-agnostic
// connection through its proxy, which the platform counts as IPv4, so clients
// that need IPv6 never reach it, whatever its proxy can do.
//
// For a proxy whose measured result says it carries IPv6, the identity gets one
// extra platform transport pinned to IPv6 and dialed through the same proxy. The
// platform then sees the proxy's IPv6 exit with the IPv6 declaration and
// categorizes the identity as dual stack. The legacy transport is never touched:
// the extra one starts and stops beside it, so an identity without a measured
// IPv6 proxy behaves exactly as before.
//
// The decision reads the cached measurement of the UDP sweep
// (proxy_ipv6_stage.go) and never probes. It needs two consecutive new results
// to start and two to stop, so a proxy that flips between answers does not
// flap between families.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

const (
	proxyDualstackPollInterval = time.Minute
	proxyDualstackStartStreak  = 2
	proxyDualstackStopStreak   = 2
)

// proxyIPv6TransportWanted is the policy: does the measured result of this
// proxy justify declaring IPv6 for its identity?
//
//   - IPv6 works for TCP and for UDP with matching exits, and the IPv4 UDP
//     relay is ok: the proxy carries everything the platform probes
//     (dual stack, dual protocol).
//   - IPv6 works for TCP and the proxy has no UDP relay at all: the platform
//     labels such a provider dual stack with TCP only after its own probes, and
//     the identity refuses UDP flows at once (proxy_udp_egress.go).
//
// A proxy with IPv4 UDP but a failing IPv6 UDP relay is left out: the platform
// would drop its IPv6 label before it dropped UDP, so declaring IPv6 gains
// nothing. An IPv6-only proxy (no IPv4 egress) is not covered by this first
// version, because the stage does not record its IPv4 control.
func proxyIPv6TransportWanted(udp proxyUDPResult, v6 proxyIPv6Result, now time.Time) bool {
	if v6.Status != proxyIPv6Yes || !v6.valid(now) {
		return false
	}
	if !now.Before(v6.At.Add(2 * v6.Interval)) {
		return false
	}
	switch {
	case udp.Status == proxyUDPOK && v6.UDP == "yes":
		return true
	case udp.Status == proxyUDPNoRelay:
		// a refused CONNECT counts as yes, so a result with no TCP exit may
		// not have had a successful IPv6 TCP check at all
		return v6.TCPExit != ""
	}
	return false
}

// proxyIPv6Gate turns the policy into start and stop decisions with
// hysteresis. Only a NEW measurement counts toward a streak, so polling more
// often than the measurements arrive cannot start or stop the transport.
type proxyIPv6Gate struct {
	running bool
	// lastCheckedAt is the completion stamp of the last udp check counted.
	// One observation is one completed check: the IPv6 record a sweep publishes
	// belongs to the check that started it, so a poll that lands between the two
	// publications cannot count the same failure twice, and flow evidence, which
	// moves the evidence time without a check, cannot count a result again.
	lastCheckedAt time.Time
	// lastExpiry is when the ipv6 evidence of the last counted observation stops
	// counting. A pending first result whose evidence expired before the next
	// observation arrived does not contribute to the start streak.
	lastExpiry time.Time
	yes        int
	no         int
}

// observe is called on every poll. It reports whether to start or to stop the
// extra transport. A running transport whose measurement went stale stops at
// once: a proxy that is no longer measured is no longer trusted.
func (self *proxyIPv6Gate) observe(now time.Time, udp proxyUDPResult, v6 proxyIPv6Result) (start bool, stop bool) {
	want := proxyIPv6TransportWanted(udp, v6, now)
	if !udp.CheckedAt.IsZero() && !udp.CheckedAt.Equal(self.lastCheckedAt) {
		if !self.running && self.yes > 0 && !self.lastExpiry.IsZero() && !now.Before(self.lastExpiry) {
			// the pending first result expired before this observation: its
			// evidence does not carry over
			self.yes = 0
		}
		self.lastCheckedAt = udp.CheckedAt
		if !v6.At.IsZero() {
			self.lastExpiry = v6.At.Add(2 * v6.Interval)
		}
		if want {
			self.yes++
			self.no = 0
		} else {
			self.no++
			self.yes = 0
		}
	}
	if self.running && !v6.At.IsZero() && !now.Before(v6.At.Add(2*v6.Interval)) {
		// the measurement behind a running transport expired: no evidence
		self.no = proxyDualstackStopStreak
	}
	if !self.running && self.yes > 0 && !v6.At.IsZero() && !now.Before(v6.At.Add(2*v6.Interval)) {
		// a pending first result that expired before a second one came is no
		// evidence: two results seen before the first expires start it
		self.yes = 0
	}
	if !self.running && self.yes >= proxyDualstackStartStreak && want {
		self.running = true
		return true, false
	}
	if self.running && self.no >= proxyDualstackStopStreak {
		self.running = false
		return false, true
	}
	return false, false
}

// proxyDualstackEnabledIn reports the proxy_dualstack setting, an operator kill
// switch that is on by default.
func proxyDualstackEnabledIn(state *controlState) bool {
	if state == nil {
		return true
	}
	value, found := state.get("proxy_dualstack")
	return !found || strings.EqualFold(value, "on")
}

func proxyDualstackEnabled() bool {
	return proxyDualstackEnabledIn(globalControlState)
}

var (
	proxyIPv6TransportsRunning atomic.Int64
	proxyIPv6TransportStarts   atomic.Int64
	proxyIPv6TransportStops    atomic.Int64
)

// proxiedPlatformTransport is the identity's platform transport: the legacy
// transport plus at most one extra IPv6 transport. It forwards the credential
// to both, so a renewed token reaches the extra one too, and closes both.
type proxiedPlatformTransport struct {
	providerPlatformTransport
	// authMu orders credential forwarding: a SetAuth and the publication of the
	// extra transport never interleave, so an older credential cannot reach the
	// extra transport after a newer one
	authMu sync.Mutex
	mu     sync.Mutex
	auth   *connect.ClientAuth
	extra  providerPlatformTransport
	closed bool
}

func newProxiedPlatformTransport(base providerPlatformTransport, auth *connect.ClientAuth) *proxiedPlatformTransport {
	return &proxiedPlatformTransport{providerPlatformTransport: base, auth: auth}
}

func (self *proxiedPlatformTransport) SetAuth(auth *connect.ClientAuth) {
	self.authMu.Lock()
	defer self.authMu.Unlock()
	self.mu.Lock()
	self.auth = auth
	extra := self.extra
	self.mu.Unlock()
	self.providerPlatformTransport.SetAuth(auth)
	if extra != nil {
		extra.SetAuth(auth)
	}
}

func (self *proxiedPlatformTransport) Kick() {
	self.mu.Lock()
	extra := self.extra
	self.mu.Unlock()
	self.providerPlatformTransport.Kick()
	if extra != nil {
		extra.Kick()
	}
}

func (self *proxiedPlatformTransport) Close() {
	self.mu.Lock()
	self.closed = true
	self.mu.Unlock()
	self.stopExtra()
	self.providerPlatformTransport.Close()
}

// startExtra builds and keeps the extra transport with the latest credential.
// It reports false when one already runs, the wrapper is closed, or the builder
// declines. A credential renewed while the transport was being built is applied
// before startExtra returns, and a Close that ran meanwhile closes what was built.
func (self *proxiedPlatformTransport) startExtra(build func(auth *connect.ClientAuth) providerPlatformTransport) bool {
	self.mu.Lock()
	if self.extra != nil || self.closed {
		self.mu.Unlock()
		return false
	}
	auth := self.auth
	self.mu.Unlock()
	extra := build(auth)
	if extra == nil {
		return false
	}
	self.authMu.Lock()
	self.mu.Lock()
	if self.extra != nil || self.closed {
		self.mu.Unlock()
		self.authMu.Unlock()
		extra.Close()
		return false
	}
	self.extra = extra
	// counted under mu, so a stopExtra that follows cannot decrement first
	proxyIPv6TransportsRunning.Add(1)
	proxyIPv6TransportStarts.Add(1)
	latest := self.auth
	self.mu.Unlock()
	if latest != auth {
		extra.SetAuth(latest)
	}
	self.authMu.Unlock()
	return true
}

// stopExtra closes the extra transport and leaves the legacy one running.
func (self *proxiedPlatformTransport) stopExtra() bool {
	self.mu.Lock()
	extra := self.extra
	self.extra = nil
	if extra != nil {
		proxyIPv6TransportsRunning.Add(-1)
		proxyIPv6TransportStops.Add(1)
	}
	self.mu.Unlock()
	if extra == nil {
		return false
	}
	extra.Close()
	return true
}

func (self *proxiedPlatformTransport) hasExtra() bool {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.extra != nil
}

// proxiedIPv6Controller watches one proxied identity's cached measurement and
// starts or stops its extra IPv6 transport.
type proxiedIPv6Controller struct {
	store     *proxyUDPCheckStore
	key       string
	transport *proxiedPlatformTransport
	build     func(auth *connect.ClientAuth) providerPlatformTransport
	enabled   func() bool
	gate      proxyIPv6Gate
}

// step runs one poll at time now.
func (self *proxiedIPv6Controller) step(now time.Time) {
	if self.enabled != nil && !self.enabled() {
		if self.transport.stopExtra() {
			tlog("[ipv6] proxy identity: IPv6 transport stopped (proxy_dualstack is off)\n")
		}
		// clear the streak, keep which results were already counted: a cached
		// result must not be counted again when the switch comes back on
		self.gate = proxyIPv6Gate{lastCheckedAt: self.gate.lastCheckedAt, lastExpiry: self.gate.lastExpiry}
		return
	}
	udp, v6, state := self.store.measurement(self.key, now)
	if state == measurementInFlight {
		// a UDP check is running: its zero result would read as a bad
		// observation, so this poll changes nothing. An IPv6 record that has
		// expired meanwhile still stops a running transport
		if self.gate.running && !v6.At.IsZero() && !now.Before(v6.At.Add(2*v6.Interval)) {
			if self.transport.stopExtra() {
				tlog("[ipv6] proxy identity: IPv6 transport stopped (the IPv6 result expired)\n")
			}
			self.gate = proxyIPv6Gate{lastCheckedAt: self.gate.lastCheckedAt, lastExpiry: self.gate.lastExpiry}
		}
		return
	}
	if state == measurementMissing {
		// no measurement is no evidence: a running transport is treated like one
		// whose result went stale and stops, and the streaks start over
		if self.transport.stopExtra() {
			tlog("[ipv6] proxy identity: IPv6 transport stopped (the proxy has no measurement)\n")
		}
		self.gate = proxyIPv6Gate{}
		return
	}
	if state == measurementExpired {
		// the udp evidence behind a running transport expired: the proxy is no
		// longer measured, so it stops on this poll instead of waiting for two
		// more checks that may never come
		if self.transport.stopExtra() {
			tlog("[ipv6] proxy identity: IPv6 transport stopped (the UDP evidence expired)\n")
		}
		self.gate = proxyIPv6Gate{}
		return
	}
	if self.gate.running && v6.At.IsZero() {
		// the identity's record was pruned and recreated without an ipv6
		// measurement: no measurement is no evidence, the same as having no
		// entry at all, so the transport stops on this poll
		if self.transport.stopExtra() {
			tlog("[ipv6] proxy identity: IPv6 transport stopped (the record has no IPv6 measurement)\n")
		}
		self.gate = proxyIPv6Gate{}
		return
	}
	start, stop := self.gate.observe(now, udp, v6)
	if start {
		if self.transport.startExtra(self.build) {
			tlog("[ipv6] proxy identity: IPv6 transport started (the proxy was measured to carry IPv6)\n")
		} else {
			// the builder declined, one runs already, or the wrapper is closed:
			// forget the start and its streak, so a poll of the same result
			// does not retry and the next two new measurements do
			self.gate.running = false
			self.gate.yes = 0
		}
	}
	if stop && self.transport.stopExtra() {
		tlog("[ipv6] proxy identity: IPv6 transport stopped (the proxy no longer qualifies)\n")
	}
}

// run polls until ctx ends and closes the extra transport on the way out.
func (self *proxiedIPv6Controller) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer self.transport.stopExtra()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			self.step(time.Now())
		}
	}
}

// measurementState says whether a cached measurement can be read now.
type measurementState int

const (
	// measurementMissing: the store has no entry for the identity
	measurementMissing measurementState = iota
	// measurementInFlight: a UDP check is running, so the entry holds no
	// completed UDP result yet. The IPv6 result alone says nothing about the
	// policy, so a reader waits for the check to finish
	measurementInFlight
	measurementReady
	// measurementExpired: the identity has a completed check, but its result is
	// past the TTL that makes it evidence. It is distinct from measurementMissing,
	// which means no check has completed yet: a running transport stops in both
	// cases, and neither counts toward a streak.
	measurementExpired
)

// measurement returns the cached UDP result and IPv6 result of an identity.
// A UDP result past its TTL is returned as unknown, so a result restored from
// disk cannot qualify a transport after its own evidence expired.
func (self *proxyUDPCheckStore) measurement(key string, now time.Time) (proxyUDPResult, proxyIPv6Result, measurementState) {
	self.mu.Lock()
	defer self.mu.Unlock()
	entry := self.cachedEntryLocked(key)
	if entry == nil {
		return proxyUDPResult{}, proxyIPv6Result{}, measurementMissing
	}
	if entry.done != nil {
		return proxyUDPResult{}, entry.ipv6, measurementInFlight
	}
	if !entry.at.IsZero() && now.Sub(entry.at) >= proxyUDPTTL(entry.result.Status) {
		return proxyUDPResult{Status: proxyUDPUnknown}, entry.ipv6, measurementExpired
	}
	result := entry.result
	result.CheckedAt = entry.completedAt
	return result, entry.ipv6, measurementReady
}

// newProxiedIPv6Builder returns the builder of the extra transport, or nil when
// the platform has no IPv6 service name for this connect url. The transport is
// pinned to IPv6 and dialed through the identity's proxy (see
// connect.NewProxiedFamilyClientStrategy). It owns its budget, runs H1 only and
// shares the identity's client limit backoff, so a client limit hold of the
// identity holds it too.
// newProxiedIPv6Strategy builds the strategy of the extra transport. It is a
// variable so a test can hold the strategy the builder hands to the transport.
var newProxiedIPv6Strategy = connect.NewProxiedFamilyClientStrategy

func newProxiedIPv6Builder(
	ctx context.Context,
	clientStrategySettings *connect.ClientStrategySettings,
	routeManager *connect.RouteManager,
	connectUrl string,
	platformSettings *connect.PlatformTransportSettings,
) func(auth *connect.ClientAuth) providerPlatformTransport {
	urlV6 := familyServiceUrl(connectUrl, 6)
	if urlV6 == "" || !familyTransportHostAllowed(connectUrl) {
		return nil
	}
	return func(auth *connect.ClientAuth) providerPlatformTransport {
		settings := *platformSettings
		settings.IpFamily = 6
		settings.EnableH3 = false
		settings.H3PacketConnFactory = nil
		settings.ModePreferences = map[connect.TransportMode]int{connect.TransportModeH1: 1}
		settings.FamilyEvents = false
		settings.AltUrl = ""
		settings.PlatformTransportBudgetPriority = connect.PlatformTransportBudgetPriorityBackground
		target := connect.MemoryBudget()
		if target <= 0 {
			target = familyBudgetReferenceByteCount
		}
		settings.PlatformTransportBudget = connect.NewPlatformTransportBudgetForMemoryTarget(target)
		// this strategy is built for this transport alone, so the transport
		// releases it when the extra transport stops
		settings.OwnClientStrategy = true
		strategy := newProxiedIPv6Strategy(ctx, clientStrategySettings, 6)
		return connect.NewPlatformTransportWithTargetMode(
			ctx, strategy, routeManager, urlV6, auth, connect.TransportModeAuto, &settings,
		)
	}
}

// writeProxyIPv6TransportMetrics exports how many proxied identities hold the
// extra IPv6 platform transport right now and how often one started or stopped.
// Counts only: no proxy address or identity appears in a label.
func writeProxyIPv6TransportMetrics(b *strings.Builder) {
	fmt.Fprintf(b, "# HELP urnet_proxy_ipv6_transports Proxied identities that hold an extra IPv6 platform transport.\n")
	fmt.Fprintf(b, "# TYPE urnet_proxy_ipv6_transports gauge\n")
	fmt.Fprintf(b, "urnet_proxy_ipv6_transports %d\n", proxyIPv6TransportsRunning.Load())
	fmt.Fprintf(b, "# HELP urnet_proxy_ipv6_transport_changes_total Starts and stops of the extra IPv6 platform transport of proxied identities.\n")
	fmt.Fprintf(b, "# TYPE urnet_proxy_ipv6_transport_changes_total counter\n")
	fmt.Fprintf(b, "urnet_proxy_ipv6_transport_changes_total{change=\"start\"} %d\n", proxyIPv6TransportStarts.Load())
	fmt.Fprintf(b, "urnet_proxy_ipv6_transport_changes_total{change=\"stop\"} %d\n", proxyIPv6TransportStops.Load())
}
