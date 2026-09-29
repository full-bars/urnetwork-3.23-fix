package connect

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync/atomic"
	"time"
)

// Smart dialer: make the measured dial cost a signal in transport choice.
//
// The dialer set exists because networks differ. Fragmented and reordered TLS
// survive paths that block or fingerprint a plain handshake, and they cost time
// where no such path exists. This file adds an opt-in preference that reacts to
// the cost measured on the network this process is actually on.
//
// It is deliberately a preference, never a filter:
//
//   - Off by default. With the setting off, ordering and weights are exactly
//     what they were before this file existed.
//   - Only dialers with MEASURED successes are considered, and failures keep
//     their existing authority, so a transport that is blocked here still loses
//     to one that works however slow the working one is.
//   - The reference is the fastest dialer that works here, so a client with a
//     single working transport is never penalised for being the only option.
//   - Nothing is ever excluded: the fallback order for every other dialer is
//     preserved and weights stay at or above each dialer's minimum weight.
//   - Costs are exponential moving averages, so a path that recovers is
//     believed again without an operator doing anything.
var smartDialerEnabled atomic.Bool

const (
	// smartDialerMinSamples is how many connection establishments a transport
	// needs before its measured cost may influence anything. Below this the
	// existing order stands, so one cold or unlucky dial cannot reorder
	// anything. The count is deliberately small: a sample is a whole
	// connection establishment (a reused keep-alive request contributes
	// none), so a healthy client may produce only one sample per connection
	// lifetime, and a higher floor would leave the preference dormant on
	// exactly the healthy networks it exists to simplify.
	smartDialerMinSamples = 2
	// smartDialerLatencyFloor bounds how far a slower working transport's
	// weight can fall relative to a faster one. It is a preference, not a ban.
	smartDialerLatencyFloor = 0.25
)

// SetSmartDialer turns the measured-cost preference on or off and returns the
// previous value. Measurement is always running; this only decides whether
// selection consults it. Safe to call while requests are in flight.
func SetSmartDialer(enabled bool) bool {
	return smartDialerEnabled.Swap(enabled)
}

// SmartDialerEnabled reports whether the measured-cost preference is on.
func SmartDialerEnabled() bool {
	return smartDialerEnabled.Load()
}

// measuredLatency returns the dialer's average connection-establishment cost
// and how many establishments that average is built from.
func (self *clientDialer) measuredLatency() (time.Duration, int) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return time.Duration(self.connectLatencyNanos), self.connectSamples
}

// hasMeasuredLatency reports whether this dialer has both worked here and
// produced enough connection-establishment samples for its cost to mean
// anything. It also requires current health: a dialer whose most recent
// attempt failed does not get to lead the measured order or set the
// fastest-working reference, no matter how fast its older samples were.
// Failures always outrank latency.
func (self *clientDialer) hasMeasuredLatency() bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.connectSamples >= smartDialerMinSamples && 0 < self.successCount && !self.lastSuccessTime.Before(self.lastErrorTime)
}

// weightWithoutLatency is the dialer's score with latency left out: its success
// ratio and its error-streak penalty, floored at the dialer's minimum weight.
// The measured-cost preference builds on this instead of the fixed-baseline
// latency factor, so a slow path is compared against the fastest path that
// works here rather than against an assumed constant.
func (self *clientDialer) weightWithoutLatency() float32 {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	c := self.successCount + self.errorCount
	var w float32
	if 0 < c {
		w = float32(float64(self.successCount) / float64(c))
	} else {
		w = self.minimumWeight
	}

	// Error streak penalty: 0.5^streak, so three consecutive errors leave
	// 0.125x. A working transport that merely started badly recovers as the
	// streak resets on its next success.
	for i := 0; i < self.consecutiveErrors; i++ {
		w *= 0.5
	}

	if w < self.minimumWeight {
		w = self.minimumWeight
	}
	return w
}

// orderDialersByMeasuredCost returns the attempt order when the measured-cost
// preference is on: dialers whose cost has been measured here come first,
// fastest first, and every other dialer keeps the order it was given. Nothing
// is dropped, so a network where the preferred transport is blocked simply
// fails over to the next one, and the blocked transport's error count removes
// it from the front by itself.
func orderDialersByMeasuredCost(dialers []*clientDialer) []*clientDialer {
	ordered := slices.Clone(dialers)

	measured := make([]*clientDialer, 0, len(ordered))
	rest := make([]*clientDialer, 0, len(ordered))
	for _, dialer := range ordered {
		if dialer.hasMeasuredLatency() {
			measured = append(measured, dialer)
		} else {
			rest = append(rest, dialer)
		}
	}
	slices.SortStableFunc(measured, func(a *clientDialer, b *clientDialer) int {
		aAvg, _ := a.measuredLatency()
		bAvg, _ := b.measuredLatency()
		return cmp.Compare(aAvg, bAvg)
	})
	return append(measured, rest...)
}

// applySmartDialerWeights scales parallel-eval weights by measured cost. Only
// dialers with measured successes are scaled, the scale is relative to the
// fastest dialer that works here, and no weight drops below its dialer's
// minimum, which keeps every transport reachable.
func applySmartDialerWeights(weights map[*clientDialer]float32) {
	var fastest time.Duration
	haveFastest := false
	for dialer := range weights {
		if !dialer.hasMeasuredLatency() {
			continue
		}
		if avg, _ := dialer.measuredLatency(); !haveFastest || avg < fastest {
			fastest = avg
			haveFastest = true
		}
	}
	if !haveFastest || fastest <= 0 {
		return
	}

	for dialer, weight := range weights {
		if !dialer.hasMeasuredLatency() {
			continue
		}
		avg, _ := dialer.measuredLatency()
		if avg <= 0 {
			continue
		}
		factor := float32(float64(fastest) / float64(avg))
		if factor < smartDialerLatencyFloor {
			factor = smartDialerLatencyFloor
		}
		if 1 < factor {
			factor = 1
		}
		scaled := weight * factor
		dialer.mutex.Lock()
		floor := dialer.minimumWeight
		dialer.mutex.Unlock()
		if scaled < floor {
			scaled = floor
		}
		weights[dialer] = scaled
	}
}

// smartDialerProbeRefreshAge is how old a dialer's connect measurement may get
// before a probe takes it again, so a path that became slow or fast is noticed
// without a restart.
const smartDialerProbeRefreshAge = 30 * time.Minute

// dialProbe makes one connect-only attempt through a dialer and reports
// whether it established a fresh connection and how long that took.
type dialProbe func(ctx context.Context, dialer *clientDialer) (fresh bool, establish time.Duration, err error)

// needsConnectProbe reports whether the smart dialer lacks a usable
// connect-cost measurement for this dialer: too few samples, or a measurement
// old enough that the network may have changed under it.
func (self *clientDialer) needsConnectProbe(now time.Time) (bool, int) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.connectSamples < smartDialerMinSamples {
		return true, smartDialerMinSamples - self.connectSamples
	}
	return now.Sub(self.connectObservedAt) > smartDialerProbeRefreshAge, 1
}

// probeDialers gives the dialers a few connect-only attempts each when any of
// them lacks a current connect-cost measurement, so the measured-cost
// preference has something to compare.
//
// Why it exists: the preference only ranks dialers it has measured, but the
// highest-priority transport (fragment) is tried first and, wherever it
// works, keeps winning, so the alternatives (normal above all) are never
// dialed and never measured. On exactly the networks that need no DPI
// circumvention the preference then has no faster option to pick.
//
// When any dialer needs a probe, EVERY dialer is probed in the same round,
// the current leader included. Samples taken at different times against
// different hosts (a real dial during the startup storm against a probe
// afterwards) are not comparable, so the comparison has to be made on one
// basis.
//
// A successful probe is a normal success for its dialer and a fresh
// connection becomes a sample. A failed probe is recorded like a real dial
// failure only for a dialer that has never succeeded (a blocked transport
// still cannot lead the order); for a dialer with real successes it is left
// out of the health record, because one failed GET to the api host must not
// demote the transport that is carrying live traffic. A probe never carries a
// live request, does nothing while the smart dialer is off (and stops as soon
// as it is turned off), and is skipped while custom extenders are configured,
// because then only the extenders are used. It returns how many dialers it
// attempted.
func (self *ClientStrategy) probeDialers(ctx context.Context, probe dialProbe) int {
	if !SmartDialerEnabled() {
		return 0
	}

	self.mutex.Lock()
	if 0 < len(self.extenderIpSecrets) {
		self.mutex.Unlock()
		return 0
	}
	dialers := make([]*clientDialer, 0, len(self.dialers))
	for dialer := range self.dialers {
		dialers = append(dialers, dialer)
	}
	self.mutex.Unlock()
	// a stable order keeps the probe sequence predictable in logs and tests
	slices.SortFunc(dialers, func(a *clientDialer, b *clientDialer) int {
		return cmp.Or(cmp.Compare(a.priority, b.priority), cmp.Compare(a.description, b.description))
	})

	now := time.Now()
	anyNeeded := false
	for _, dialer := range dialers {
		if needed, _ := dialer.needsConnectProbe(now); needed {
			anyNeeded = true
			break
		}
	}
	if !anyNeeded {
		return 0
	}

	probed := 0
	for _, dialer := range dialers {
		if ctx.Err() != nil || !SmartDialerEnabled() {
			break
		}
		_, attempts := dialer.needsConnectProbe(now)

		attempted := false
		for i := 0; i < attempts && ctx.Err() == nil && SmartDialerEnabled(); i += 1 {
			start := time.Now()
			fresh, establish, err := probe(ctx, dialer)
			if ctx.Err() != nil {
				// canceled mid-probe: an aborted attempt is not evidence
				break
			}
			attempted = true
			if err != nil {
				if dialer.Stats().successCount == 0 {
					dialer.Update(ctx, err, time.Since(start))
				}
				// do not hammer a transport that cannot connect here
				break
			}
			dialer.Update(ctx, nil, time.Since(start))
			if fresh {
				dialer.observeConnect(establish)
			}
		}
		if attempted {
			probed += 1
		}
	}
	return probed
}

// ProbeDialers measures the connect cost of every transport that lacks a
// current measurement by making connect-only requests to probeUrl (use the
// api url). Call it at startup and periodically while the smart dialer is on.
// It returns how many transports it attempted; zero when the smart dialer is
// off or every transport is already measured.
func (self *ClientStrategy) ProbeDialers(ctx context.Context, probeUrl string) int {
	return self.probeDialers(ctx, func(ctx context.Context, dialer *clientDialer) (bool, time.Duration, error) {
		return connectProbe(ctx, dialer, probeUrl)
	})
}

// connectProbe does one GET through the dialer on a connection of its own.
// The dialer's shared client keeps connections alive, and a request served by
// a kept-alive connection dials nothing, so it would report reuse and produce
// no sample; a private transport with keep-alives off always dials.
func connectProbe(ctx context.Context, dialer *clientDialer, probeUrl string) (bool, time.Duration, error) {
	base, ok := dialer.HttpClient().Transport.(*http.Transport)
	if !ok {
		return false, 0, fmt.Errorf("dialer %s has no http transport to probe", dialer.description)
	}
	transport := base.Clone()
	transport.DisableKeepAlives = true
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   dialer.settings.RequestTimeout,
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, probeUrl, nil)
	if err != nil {
		return false, 0, err
	}
	timing := &dialTiming{}
	start := time.Now()
	response, err := client.Do(timing.trace(ctx, request, start))
	if err != nil {
		return false, 0, err
	}
	io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	response.Body.Close()

	fresh, establish := timing.sample()
	return fresh, establish, nil
}
