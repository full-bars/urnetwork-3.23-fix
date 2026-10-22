package connect

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"

	// "errors"
	"fmt"
	"io"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	// "golang.org/x/net/proxy"

	"golang.org/x/net/nettest"

	"golang.org/x/exp/maps"

	"github.com/gorilla/websocket"
)

// censorship-resistant strategies for making https connections
// this uses a random walk of best practices to obfuscate and double-encrypt https connections

// note the net_* files are migrated from net.IP/IPNet to netip.Addr/Prefix
// TODO generally the entire package should migrate over to these newer structs

type HttpPostRawFunction func(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error)
type HttpGetRawFunction func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error)

// ---- adaptive parallel block size ----

// probeWindow tracks dial outcomes over a rolling 10-second window so the
// parallel probe batch shrinks on a healthy network and grows when it
// degrades. Each ClientStrategy owns one. A provider runs a strategy per
// proxy, and a process-wide window let one proxy's failing path widen, or
// one healthy path narrow, the batch for every other proxy.
type probeWindow struct {
	mu           sync.Mutex
	windowStart  int64 // UnixNano of current window start
	successCount int64
	failureCount int64
}

// record registers a single dial outcome and resets the window when more
// than 10 seconds have elapsed since it opened.
func (self *probeWindow) record(success bool) {
	now := time.Now().UnixNano()
	self.mu.Lock()
	defer self.mu.Unlock()
	if self.windowStart == 0 || now-self.windowStart > 10*int64(time.Second) {
		self.windowStart = now
		self.successCount = 0
		self.failureCount = 0
	}
	if success {
		self.successCount++
	} else {
		self.failureCount++
	}
}

// blockSize sizes the parallel batch from recent health, never above the
// configured ParallelBlockSize:
//
//	>80 % success → half (healthy, save goroutines)
//	otherwise     → configured
//
// The configured size is a ceiling on concurrent dials, so a degraded
// network probes at the full configured width rather than past it. Returns
// the configured size when the window holds no fresh data.
func (self *probeWindow) blockSize(configured int) int {
	if configured < 1 {
		configured = 1
	}
	self.mu.Lock()
	start := self.windowStart
	s := self.successCount
	f := self.failureCount
	self.mu.Unlock()
	if start == 0 || time.Now().UnixNano()-start > 10*int64(time.Second) {
		return configured
	}
	total := s + f
	if total == 0 {
		return configured
	}
	rate := float64(s) / float64(total)
	if rate > 0.8 {
		return max(1, configured/2)
	}
	return configured
}

func DefaultClientStrategySettings() *ClientStrategySettings {
	return &ClientStrategySettings{
		ExposeServerIps:       true,
		ExposeServerHostNames: true,

		EnableNormal:    true,
		EnableResilient: true,

		ParallelBlockSize: 4,

		ExpandExtenderProfileCount: 8,
		ExtenderNetworks:           []netip.Prefix{},
		ExtenderHostnames:          []string{},
		ReconnectTimeout:           15 * time.Second,
		MaxExtenderCount:           128,
		ExtenderMinimumWeight:      0.1,
		ExtenderDropTimeout:        5 * time.Minute,

		DohSettings: DefaultDohSettings(),

		HelloRetryTimeout: 5 * time.Second,

		MinNextConnectDelay: 100 * time.Millisecond,
		MaxNextConnectDelay: 1000 * time.Millisecond,

		ConnectSettings: *DefaultConnectSettings(),
	}
}

type ClientStrategySettings struct {
	Log Logger

	// expose consistent ips
	// if true, enables ech
	ExposeServerIps bool
	// expose server names
	// if true, enables non-ech
	// TODO set this to default false
	ExposeServerHostNames bool
	// note that extenders and proxy are the only strategies that will be enabled if
	// `ExposeServerIps == false` and `ExposeServerNames == false`

	EnableNormal bool
	// tls frag, retransmit, tls frag + retransmit
	EnableResilient bool

	// for gets and ws connects
	ParallelBlockSize int

	// the number of new profiles to add per expand
	ExpandExtenderProfileCount int
	ExtenderNetworks           []netip.Prefix
	// these are evaluated with DoH to grow the extender ips
	ExtenderHostnames []string
	ReconnectTimeout  time.Duration
	MaxExtenderCount  int
	// DisableManualExtenders makes SetCustomExtenders a no-op. A strategy built
	// for a family-pinned transport sets it: an extender's address family is its
	// own, so it would prove the extender's family, not the provider's.
	DisableManualExtenders bool
	// extender minimum weight
	ExtenderMinimumWeight float32
	// drop dialers that have not had a successful connect in this timeout
	ExtenderDropTimeout time.Duration

	DohSettings *DohSettings

	// InternalDohDomains are network-space domains whose exact host and
	// subdomains resolve through the strategy's direct DoH cache before a
	// control connection is dialed by raw IP (ported from the upstream
	// internal-DoH leaf; nil disables the rule).
	InternalDohDomains []string

	HelloRetryTimeout time.Duration

	MinNextConnectDelay time.Duration
	MaxNextConnectDelay time.Duration

	ConnectSettings
}

// stores statistics on client strategies
type ClientStrategy struct {
	ctx context.Context
	log Logger

	settings *ClientStrategySettings

	// ipFamily is the family pin this strategy's dialers dial for: 4 or 6 for
	// the direct-only strategy a family-pinned platform transport owns, 0 for
	// a family-agnostic one. It is set at construction and never changes; the
	// framed-upgrade rejection cache reads it to keep the pins from aliasing
	// each other (see upgradeRouteScope).
	ipFamily int

	// internalDohResolver exists only when InternalDohDomains are configured
	// (see net_http_internal_doh.go; ported from upstream at 20b29f79).
	internalDohResolver *internalDohResolver
	// dohSettings is the strategy's current DoH settings, replaced live by
	// SetInternalDohSettings (ported from upstream at 20b29f79).
	dohSettings *DohSettings

	mutex sync.Mutex
	// dialers are only updated inside the mutex
	dialers             map[*clientDialer]bool
	resolvedExtenderIps []netip.Addr

	// custom extenders
	// these take precedence over other extenders
	extenderIpSecrets map[netip.Addr]string

	nextConnectTime time.Time
	failureCount    int
	// callers holding the reconnect fast path; guarded by mutex
	reconnectFastPathCount int

	probes probeWindow
}

func (self *ClientStrategy) getBackoffTimeout() time.Duration {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.failureCount == 0 {
		return self.settings.ReconnectTimeout
	}
	multiplier := time.Duration(1) << uint(min(self.failureCount, 10))
	timeout := self.settings.ReconnectTimeout * multiplier
	if timeout > time.Hour {
		timeout = time.Hour
	}
	return timeout
}

func (self *ClientStrategy) ResetHealth() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.failureCount = 0
	for dialer := range self.dialers {
		dialer.ResetHealth()
	}
}

func NewClientStrategyWithDefaults(ctx context.Context) *ClientStrategy {
	return NewClientStrategy(ctx, DefaultClientStrategySettings())
}

// newNormalDialTlsContext builds the tls dial of the normal dialer for a path
// offering nextProtos. One session ticket cache and one handshaker (Chrome's
// hello, or Go's) belong to the path and are shared by every dial it makes.
func newNormalDialTlsContext(
	settings *ClientStrategySettings,
	nextProtos []string,
) DialTlsContextFunction {
	// this dial path's own session ticket cache (see newClientTlsConfig)
	normalTlsConfig := newClientTlsConfig(settings.TlsConfig, nextProtos)
	// the Chrome hello, or Go's (net_tls_hello.go)
	tlsHandshaker := newClientTlsHandshaker(settings.TlsClientHelloFingerprint, normalTlsConfig)
	// the handshake metric label follows the strategy's transport, which
	// the unified dial no longer branches on
	handshakePath := tlsPathNormal
	if settings.ProxySettings != nil {
		handshakePath = tlsPathNormalProxy
	}
	return func(ctx context.Context, network string, addr string) (net.Conn, error) {
		// the handshake half of the dial. It owns the connection's tls
		// lifetime, and closes what it was handed on its own error paths
		// -- dialControlTlsWithFamilyFallback has already read the
		// family off the connection by then, so the close cannot hide it.
		handshake := func(ctx context.Context, conn net.Conn) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}

			tlsConfig := normalTlsConfig
			if tlsConfig.ServerName == "" {
				c := tlsConfig.Clone()
				c.ServerName = host
				tlsConfig = c
			}

			tlsCtx, tlsCancel := context.WithTimeout(ctx, settings.TlsTimeout)
			defer tlsCancel()
			tlsConn, err := tlsHandshaker.handshake(tlsCtx, conn, tlsConfig)
			var tlsState tls.ConnectionState
			if chromeConn, ok := tlsConn.(*chromeTlsConn); ok {
				tlsState = chromeConn.ConnectionState()
			} else if goConn, ok := tlsConn.(*tls.Conn); ok {
				tlsState = goConn.ConnectionState()
			}
			noteTlsHandshake(handshakePath, host, tlsState, err)
			return tlsConn, err
		}
		// one retry over the other address family when the handshake
		// hits its own timeout after the connect succeeded
		// (control_family_dial.go)
		return dialControlTlsWithFamilyFallback(
			ctx, &settings.ConnectSettings, network, addr, settings.DialContext, handshake)
	}
}

// extender udp 53 to platform extender
func NewClientStrategy(ctx context.Context, settings *ClientStrategySettings) *ClientStrategy {
	// wire the internal DoH resolver when InternalDohDomains are configured
	// (ported from upstream net_http.go at 20b29f79; inert when unset)
	settings, internalDohResolver := clientStrategySettingsWithInternalDoh(settings)

	// create dialers to match settings
	dialers := map[*clientDialer]bool{}
	resolvedExtenderIps := []netip.Addr{}

	if settings.EnableNormal {
		// TODO ECH support
		if settings.ExposeServerHostNames && settings.ExposeServerIps {
			// Every dial takes the explicit path below -- proxy or not --
			// instead of a raw tls.Dialer. The dial runs through
			// ConnectSettings.DialContext, which applies the address-family
			// policy and routes through ProxySettings when one is configured;
			// the tls.Dialer shortcut bypassed both, so a strategy raced a
			// forced dialer against an unforced one, and the fallback below
			// never saw the tls handshake it exists to classify.
			//
			// The cost is that ConnectTimeout and TlsTimeout are now separate
			// budgets rather than one deadline shared across connect and
			// handshake. That is the intended behavior, and it applies to every
			// dial, not only forced ones.

			dialer := &clientDialer{
				log:           loggerOrDefault(settings.Log),
				description:   "normal",
				minimumWeight: 0.5,
				priority:      25,
				// websocket upgrades require http/1.1, while api requests can
				// negotiate h2, so each path keeps its own tls dialer
				dialTlsContext:     newNormalDialTlsContext(settings, clientWebSocketNextProtos),
				httpDialTlsContext: newNormalDialTlsContext(settings, clientHttpNextProtos),
				settings:           settings,
			}
			dialers[dialer] = true
		}
	}
	if settings.EnableResilient {
		// TODO ECH support
		if settings.ExposeServerHostNames && settings.ExposeServerIps {
			// fragment+reorder
			dialer1 := &clientDialer{
				log:                loggerOrDefault(settings.Log),
				description:        "fragment+reorder",
				minimumWeight:      0.25,
				priority:           50,
				dialTlsContext:     newResilientDialTlsContext(&settings.ConnectSettings, true, true, false, clientWebSocketNextProtos),
				httpDialTlsContext: newResilientDialTlsContext(&settings.ConnectSettings, true, true, false, clientHttpNextProtos),
				settings:           settings,
			}
			// fragment
			// this is the highest priority because it has no performance impact and additional security benefits
			dialer2 := &clientDialer{
				log:                loggerOrDefault(settings.Log),
				description:        "fragment",
				minimumWeight:      0.25,
				priority:           0,
				dialTlsContext:     newResilientDialTlsContext(&settings.ConnectSettings, true, false, false, clientWebSocketNextProtos),
				httpDialTlsContext: newResilientDialTlsContext(&settings.ConnectSettings, true, false, false, clientHttpNextProtos),
				settings:           settings,
			}
			// reorder
			dialer3 := &clientDialer{
				log:                loggerOrDefault(settings.Log),
				description:        "reorder",
				minimumWeight:      0.25,
				priority:           50,
				dialTlsContext:     newResilientDialTlsContext(&settings.ConnectSettings, false, true, false, clientWebSocketNextProtos),
				httpDialTlsContext: newResilientDialTlsContext(&settings.ConnectSettings, false, true, false, clientHttpNextProtos),
				settings:           settings,
			}

			dialers[dialer1] = true
			dialers[dialer2] = true
			dialers[dialer3] = true

			// fragment+segment: the combined mode cuts at the TLS record
			// layer (which survives a proxy) and at the TCP segment layer
			// (which only works on a direct connection); behind a proxy it
			// would equal "fragment", so it is not registered there, which
			// also saves a dial per reconnect round.
			if settings.ProxySettings == nil {
				dialer4 := &clientDialer{
					log:                loggerOrDefault(settings.Log),
					description:        "fragment+segment",
					minimumWeight:      0.25,
					priority:           50,
					dialTlsContext:     newResilientDialTlsContext(&settings.ConnectSettings, true, false, true, clientWebSocketNextProtos),
					httpDialTlsContext: newResilientDialTlsContext(&settings.ConnectSettings, true, false, true, clientHttpNextProtos),
					settings:           settings,
				}
				dialers[dialer4] = true
			}
		}
	}
	// FIXME
	/*
		if settings.EnablePt {
			// these route the api via the connect server
			// the connect server runs the api listening for pt connections to the api host

			ptDialer1 := &clientDialer{
				log: loggerOrDefault(settings.Log),
				description:    "dns",
				minimumWeight:  0.25,
				priority:       100,
				dialTlsContext: NewPtDialTlsContext(
					&settings.ConnectSettings,
					PacketTranslationModeDns,
					&settings.PacketTranslationSettings,
				),
				settings:       settings,
			}
			ptDialer2 := &clientDialer{
				log: loggerOrDefault(settings.Log),
				description:    "dnspump",
				minimumWeight:  0.25,
				priority:       125,
				dialTlsContext: NewPtDialTlsContext(
					&settings.ConnectSettings,
					PacketTranslationModeDnsPump,
					&settings.PacketTranslationSettings,
				),
				settings:       settings,
			}

			dialers[ptDialer1] = true
			dialers[ptDialer2] = true
		}
	*/

	return &ClientStrategy{
		ctx:                 ctx,
		log:                 loggerOrDefault(settings.Log),
		settings:            settings,
		internalDohResolver: internalDohResolver,
		dohSettings:         settings.DohSettings,
		dialers:             dialers,
		resolvedExtenderIps: resolvedExtenderIps,
		extenderIpSecrets:   map[netip.Addr]string{},
	}

}

func (self *ClientStrategy) SetCustomExtenders(extenderIpSecrets map[netip.Addr]string) {
	if self.settings.DisableManualExtenders {
		return
	}
	self.mutex.Lock()
	defer self.mutex.Unlock()

	self.extenderIpSecrets = maps.Clone(extenderIpSecrets)
	for dialer, _ := range self.dialers {
		if dialer.IsExtender() {
			dialer.Close()
			delete(self.dialers, dialer)
		}
	}
}

func (self *ClientStrategy) CustomExtenders() map[netip.Addr]string {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	return maps.Clone(self.extenderIpSecrets)
}

// nextConnectMaxLead caps how far the shared next-connect timestamp may run
// ahead of wall clock. A dialer whose pacing wait was cancelled used to keep its
// step, so rapid connect/disconnect cycles compounded the lead without bound.
// The cancel release below is the primary fix; this clamp is the backstop that
// bounds the damage of any reservation that still leaks (from upstream at
// 20b29f79).
const nextConnectMaxLead = 10 * time.Second

// new connections should use next connect time to avoid flooding the network
// at once.
//
// The returned release gives this caller's reservation back to the staircase.
// It exists for one situation: the caller's pacing wait was cancelled before it
// ever dialed, so the step it reserved paces nobody. A caller that goes on to
// dial must NOT call release. The release is idempotent and never nil.
func (self *ClientStrategy) NextConnectTime() (time.Time, func()) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	now := time.Now()
	connectDelayRange := self.settings.MaxNextConnectDelay - self.settings.MinNextConnectDelay
	connectDelay := self.settings.MinNextConnectDelay
	if 0 < connectDelayRange {
		connectDelay += time.Duration(mathrand.Int63n(int64(connectDelayRange)))
	}
	nextConnectTime := self.nextConnectTime.Add(connectDelay)
	if nextConnectTime.Before(now) {
		nextConnectTime = now
	}
	if maxLead := now.Add(nextConnectMaxLead); maxLead.Before(nextConnectTime) {
		nextConnectTime = maxLead
	}
	self.nextConnectTime = nextConnectTime

	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			self.mutex.Lock()
			defer self.mutex.Unlock()
			self.nextConnectTime = self.nextConnectTime.Add(-connectDelay)
		})
	}
	return nextConnectTime, release
}

const (
	// reconnectFastPathLimit caps how many callers may hold the reconnect fast
	// path (NextReconnectTime) at once; callers past the cap fall back to the
	// serialized NextConnectTime staircase.
	reconnectFastPathLimit = 4
	// reconnectFastPathMaxDelay is the independent per-caller jitter for a
	// fast-path reconnect: uniform in [0, 250ms).
	reconnectFastPathMaxDelay = 250 * time.Millisecond
)

// NextReconnectTime is the scoped fast path of NextConnectTime for a caller
// whose transport was connected and just lost its connection: a small
// independent jitter that neither reads nor advances the shared timestamp, capped
// at reconnectFastPathLimit concurrent holders. The returned release frees the
// slot and MUST be called once the dial attempt completes; it is idempotent and
// never nil. Retries after a failed reconnect go back through NextConnectTime.
func (self *ClientStrategy) NextReconnectTime() (time.Time, func()) {
	acquired := false
	func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		if self.reconnectFastPathCount < reconnectFastPathLimit {
			self.reconnectFastPathCount += 1
			acquired = true
		}
	}()
	if !acquired {
		next, _ := self.NextConnectTime()
		return next, func() {}
	}

	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			self.mutex.Lock()
			defer self.mutex.Unlock()
			self.reconnectFastPathCount -= 1
		})
	}
	jitter := time.Duration(mathrand.Int63n(int64(reconnectFastPathMaxDelay)))
	return time.Now().Add(jitter), release
}

func (self *ClientStrategy) dialerWeights() map[*clientDialer]float32 {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	weights := map[*clientDialer]float32{}

	if len(self.extenderIpSecrets) == 0 {
		if SmartDialerEnabled() {
			// Measured-cost preference: score each dialer without the
			// fixed-baseline latency factor, then scale by its measured cost
			// relative to the fastest dialer that works on this network.
			for dialer, _ := range self.dialers {
				weights[dialer] = dialer.weightWithoutLatency()
			}
			applySmartDialerWeights(weights)
		} else {
			for dialer, _ := range self.dialers {
				w := dialer.Weight()
				weights[dialer] = w
			}
		}
	} else {
		for dialer, _ := range self.dialers {
			if dialer.IsExtender() {
				weights[dialer] = 1.0
			}
		}
	}

	return weights
}

type httpResult struct {
	response *http.Response

	// status string
	// statusCode int
	// header http.Header
	// trailer http.Header
	bodyBytes []byte
}

type evalResult struct {
	dialer *clientDialer
	wsConn *websocket.Conn
	err    error
	// materialize, when set, is run for the selected HTTP response (ported
	// from upstream net_http.go at 20b29f79; nil is inert on the fork).
	materialize func() error
	// h1Conn is the framed H1 message connection an H1 dial produced; dialerInfo
	// describes the dialer that won it (stage 3, from upstream at 20b29f79).
	h1Conn     H1MessageConn
	dialerInfo *DialerInfo
	// terminal ends the logical dial with this result's error instead of
	// retrying the remaining dialers (an authorization denial).
	terminal bool

	httpResult
}

func newEvalResultFromHttpResponse(response *http.Response, err error) *evalResult {
	if err == nil {
		defer response.Body.Close()
		bodyBytes, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
		return &evalResult{
			err: err,
			httpResult: httpResult{
				response: response,
				// status: response.Status,
				// statusCode: response.StatusCode,
				// header: response.Header.Clone(),
				// trailer: response.Trailer.Clone(),
				bodyBytes: bodyBytes,
			},
		}
	} else {
		return &evalResult{
			err: err,
		}
	}
}

func (self *evalResult) Close() {
	// fork: an H1 dial (websocket or H1+) stores its connection in h1Conn; a
	// losing parallel dial must close it, or its authenticated platform session
	// stays open with no reader. (Upstream's Close does the same.)
	if self.h1Conn != nil {
		self.h1Conn.Close()
	}
	if self.wsConn != nil {
		self.wsConn.Close()
		// if wsConn is set, the response does not need to be closed
		// https://pkg.go.dev/github.com/gorilla/websocket#Dialer.DialContext
	}
	// else if self.response != nil {
	// 	self.response.Body.Close()
	// }
}

func (self *ClientStrategy) parallelEval(ctx context.Context, eval func(ctx context.Context, dialer *clientDialer) *evalResult) *evalResult {
	result, _ := self.parallelEvalErr(ctx, eval, false)
	return result
}

// parallelEvalErr is parallelEval that also says why it returned no result: a
// *dialTimeoutError carrying the real dial errors, or a wrapped context error
// for local teardown.
//
// singleRound lets the call give up after one pass over the dialers while the
// backend is failing (see singleRoundWhenDegraded). Otherwise it keeps retrying
// every dialer, round after round, until the request deadline: one failed
// attempt then costs many dials, which is what multiplies a backend outage into
// a flood. Only the platform transport dial opts in; API requests keep their
// retries.
func (self *ClientStrategy) parallelEvalErr(ctx context.Context, eval func(ctx context.Context, dialer *clientDialer) *evalResult, singleRound bool) (*evalResult, error) {
	// in this order:
	// 1. try all dialers that previously worked sequentially
	// 2. try dialers that previously failed in parallel blocks
	// 3. expand the extenders and try new extenders in parallel blocks

	handleCtx, handleCancel := context.WithTimeout(ctx, self.settings.RequestTimeout)
	defer handleCancel()

	start := time.Now()
	rec := &evalErrorRecorder{}
	// noteFailed records a dial error unless the call is already over: a dial
	// cancelled by the deadline is the deadline's doing, not a cause
	noteFailed := func(result *evalResult) {
		if result == nil || result.err == nil || handleCtx.Err() != nil {
			return
		}
		// a dial's own deadline derives from the request deadline and can fire a
		// moment before the context reports done; an error landing that close to
		// the deadline is the deadline's doing, not a cause
		if deadline, ok := handleCtx.Deadline(); ok && time.Until(deadline) < dialDeadlineNoise {
			return
		}
		rec.note(result.err)
	}
	// finish is the error for a call that ends without a result
	finish := func() error {
		if err := self.ctx.Err(); err != nil {
			return fmt.Errorf("dial canceled: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("dial canceled: %w", err)
		}
		return rec.timeoutError(time.Since(start))
	}
	// merge handleCtx with self.ctx
	go HandleError(func() {
		defer handleCancel()
		select {
		case <-handleCtx.Done():
			return
		case <-self.ctx.Done():
			return
		}
	})

	out := make(chan *evalResult)

	run := func(dialer *clientDialer) {
		success := false
		defer func() {
			if !success {
				select {
				case out <- nil:
				case <-handleCtx.Done():
				}
			}
		}()
		result := eval(handleCtx, dialer)
		noteFailed(result)
		if result == nil {
			return
		}

		result.dialer = dialer
		select {
		case out <- result:
			success = true
		case <-handleCtx.Done():
			result.Close()
		}
	}

	// keep trying as long as there is time left
	for {
		select {
		case <-handleCtx.Done():
			return nil, finish()
		default:
		}

		rec.round()
		reconnect := NewReconnect(self.getBackoffTimeout())

		self.collapseExtenderDialers()

		// the number of runs with pending out
		p := 0

		// CRITICAL #1 fix: compute blockSize once per iteration to prevent
		// the value from changing between the two call sites, which could
		// make blockSize-p go negative and cause a slice bounds panic.
		blockSize := self.probes.blockSize(self.settings.ParallelBlockSize)

		dialerWeights := self.dialerWeights()

		if 0 < len(dialerWeights) {
			serialDialers := []*clientDialer{}
			parallelDialers := []*clientDialer{}

			dialers := maps.Keys(dialerWeights)
			WeightedShuffle(dialers, dialerWeights)

			// always try the top options first
			serialDialers = append(serialDialers, dialers[0])

			for _, dialer := range dialers[1:] {
				if dialer.IsLastSuccess() {
					serialDialers = append(serialDialers, dialer)
				} else {
					parallelDialers = append(parallelDialers, dialer)
				}
			}

			// WeightedShuffle(serialDialers, dialerWeights)
			sortDialersByHealth(serialDialers)
			for _, dialer := range serialDialers {
				select {
				case <-handleCtx.Done():
					return nil, finish()
				default:
				}

				result := eval(handleCtx, dialer)
				noteFailed(result)
				if result != nil {
					if result.err == nil || result.terminal {
						self.log.V(2).Infof("[net][p]select: %s\n", dialer.String())
						return result, nil
					}
					self.log.V(2).Infof("[net][p]select: %s = %s\n", dialer.String(), result.err)
					result.Close()
				}
			}

			// note parallel dialers is in the original weighted order
			// WeightedShuffle(parallelDialers, dialerWeights)
			n := min(len(parallelDialers), blockSize)
			p += n
			for _, dialer := range parallelDialers[0:n] {
				go HandleError(func() {
					run(dialer)
				})
			}
			for _, dialer := range parallelDialers[n:] {
				select {
				case <-handleCtx.Done():
					return nil, finish()
				case result := <-out:
					if result != nil {
						if result.err == nil || result.terminal {
							self.log.V(2).Infof("[net][p]select: %s\n", result.dialer.String())
							return result, nil
						}
						self.log.V(2).Infof("[net][p]select: %s = %s\n", result.dialer.String(), result.err)
						result.Close()
					}
					go HandleError(func() {
						run(dialer)
					})
				}
			}
		}

		if expandedDialers, _ := self.expandExtenderDialers(); 0 < len(expandedDialers) {
			n := min(len(expandedDialers), max(0, blockSize-p))
			p += n
			for _, dialer := range expandedDialers[0:n] {
				go HandleError(func() {
					run(dialer)
				})
			}
			for _, dialer := range expandedDialers[n:] {
				select {
				case <-handleCtx.Done():
					return nil, finish()
				case result := <-out:
					if result != nil {
						if result.err == nil || result.terminal {
							self.log.V(2).Infof("[net][p]select: %s\n", result.dialer.String())
							return result, nil
						}
						self.log.V(2).Infof("[net][p]select: %s = %s\n", result.dialer.String(), result.err)
						result.Close()
					}
					go HandleError(func() {
						run(dialer)
					})
				}
			}
		}

		for range p {
			select {
			case <-handleCtx.Done():
				return nil, finish()
			case result := <-out:
				if result != nil {
					if result.err == nil || result.terminal {
						return result, nil
					}
					result.Close()
				}
			}
		}

		// the rate limit is important when when the connect timeout is small
		// e.g. local closes due to disconnected network
		self.mutex.Lock()
		self.failureCount += 1
		self.mutex.Unlock()
		if singleRound && singleRoundCheck() {
			return nil, finish()
		}
		select {
		case <-handleCtx.Done():
			return nil, finish()
		case <-reconnect.After():
		case <-Pulse():
			self.ResetHealth()
		}
	}

}

func (self *ClientStrategy) serialEval(ctx context.Context, eval func(ctx context.Context, dialer *clientDialer) *evalResult, helloEval func(ctx context.Context, dialer *clientDialer) *evalResult) *evalResult {
	handleCtx, handleCancel := context.WithTimeout(ctx, self.settings.RequestTimeout)
	defer handleCancel()
	// merge handleCtx with self.ctx
	go HandleError(func() {
		defer handleCancel()
		select {
		case <-handleCtx.Done():
			return
		case <-self.ctx.Done():
			return
		}
	}, handleCancel)

	// keep trying as long as there is time left
	for {
		select {
		case <-handleCtx.Done():
			return nil
		default:
		}

		self.collapseExtenderDialers()

		dialerWeights := self.dialerWeights()

		serialDialers := []*clientDialer{}

		for dialer, _ := range dialerWeights {
			if dialer.IsLastSuccess() {
				serialDialers = append(serialDialers, dialer)
			}
		}

		sortDialersByHealth(serialDialers)
		for _, dialer := range serialDialers {
			select {
			case <-handleCtx.Done():
				return nil
			default:
			}

			startDial := time.Now()
			result := eval(handleCtx, dialer)
			dialDur := time.Since(startDial)
			if result != nil {
				if result.err == nil {
					self.mutex.Lock()
					self.failureCount = 0
					self.mutex.Unlock()
					self.log.Infof("🌐 [net][s]select: %s dur=%dms\n", dialer.String(), dialDur.Milliseconds())
					return result
				}
				if ok, suppressed := shouldLogSelectErr(); ok {
					if suppressed > 0 {
						self.log.Infof("🌐 [net][s]select: %s = %s dur=%dms (%d suppressed)\n", dialer.String(), result.err, dialDur.Milliseconds(), suppressed)
					} else {
						self.log.Infof("🌐 [net][s]select: %s = %s dur=%dms\n", dialer.String(), result.err, dialDur.Milliseconds())
					}
				}
				result.Close()
			}
		}

		// it's more efficient to iterate with a parallel hello
		// keep retrying hello until at least one dialer is success
		for {
			helloStartTime := time.Now()
			result := self.parallelEval(handleCtx, helloEval)
			if result != nil {
				result.Close()
			}
			helloEndTime := time.Now()

			// check if any dialer succeeded
			successCount := 0
			for dialer, _ := range self.dialerWeights() {
				if dialer.IsLastSuccess() {
					successCount += 1
				}
			}
			if 0 < successCount {
				break
			}

			timeout := self.settings.HelloRetryTimeout - helloEndTime.Sub(helloStartTime)
			if 0 < timeout {
				select {
				case <-handleCtx.Done():
					return nil
				case <-time.After(timeout):
				}
			} else {
				select {
				case <-handleCtx.Done():
					return nil
				default:
				}
			}
		}
		// if result.err != nil {
		// 	return &evalResult{
		// 		err: result.err,
		// 	}
		// }
	}

}

func (self *ClientStrategy) HttpParallel(request *http.Request) (*httpResult, error) {
	eval := func(handleCtx context.Context, dialer *clientDialer) *evalResult {
		httpClient := dialer.HttpClient()
		startDial := time.Now()
		timing := &dialTiming{}
		response, err := httpClient.Do(timing.trace(handleCtx, request, startDial))
		if log := self.log.V(2); log.Enabled() {
			if err != nil {
				self.log.Infof("[net]http parallel %s %s = %s\n", request.Method, request.URL, err)
			} else {
				self.log.Infof("[net]http parallel %s %s = %s\n", request.Method, request.URL, response.Status)
			}
		}

		// Only a genuine connection establishment is a dial-cost sample for
		// the smart dialer: a reused keep-alive connection performed no dial,
		// and folding its (fast) request time in would hide the connect-time
		// cost this measurement exists to compare. The legacy full-duration
		// EMA still folds every success (byte-identical smart_dialer-off
		// behavior), so the connect-only sample goes to observeConnect.
		freshConnect, establishTime := timing.sample()
		if dialer.Update(handleCtx, err, time.Since(startDial)) {
			self.probes.record(err == nil)
		}
		if err == nil && freshConnect {
			dialer.observeConnect(establishTime)
		}

		return newEvalResultFromHttpResponse(response, err)
	}

	result, evalErr := self.parallelEvalErr(request.Context(), eval, false)
	if result == nil {
		return nil, evalErr
	}
	return &result.httpResult, result.err
}

func (self *ClientStrategy) HttpSerial(request *http.Request, helloRequest *http.Request) (*httpResult, error) {
	// in this order:
	// 1. try all dialers that previously worked sequentially
	// 2. retest and expand dialers using get of the hello request.
	//    This is a basic ping to the server, which is run in parallel.
	// 3. continue from 1 until timeout

	eval := func(handleCtx context.Context, dialer *clientDialer) *evalResult {
		httpClient := dialer.HttpClient()
		startDial := time.Now()
		timing := &dialTiming{}
		response, err := httpClient.Do(timing.trace(handleCtx, request, startDial))
		if log := self.log.V(2); log.Enabled() {
			if err != nil {
				self.log.Infof("[net]http serial %s %s = %s\n", request.Method, request.URL, err)
			} else {
				self.log.Infof("[net]http serial %s %s = %s\n", request.Method, request.URL, response.Status)
			}
		}

		freshConnect, establishTime := timing.sample()
		if dialer.Update(handleCtx, err, time.Since(startDial)) {
			self.probes.record(err == nil)
		}
		if err == nil && freshConnect {
			dialer.observeConnect(establishTime)
		}

		return newEvalResultFromHttpResponse(response, err)
	}
	helloEval := func(handleCtx context.Context, dialer *clientDialer) *evalResult {
		httpClient := dialer.HttpClient()
		startDial := time.Now()
		timing := &dialTiming{}
		response, err := httpClient.Do(timing.trace(handleCtx, helloRequest, startDial))
		if log := self.log.V(2); log.Enabled() {
			if err != nil {
				self.log.Infof("[net]http serial hello %s %s = %s\n", helloRequest.Method, helloRequest.URL, err)
			} else {
				self.log.Infof("[net]http serial hello %s %s = %s\n", helloRequest.Method, helloRequest.URL, response.Status)
			}
		}

		freshConnect, establishTime := timing.sample()
		if dialer.Update(handleCtx, err, time.Since(startDial)) {
			self.probes.record(err == nil)
		}
		if err == nil && freshConnect {
			dialer.observeConnect(establishTime)
		}

		return newEvalResultFromHttpResponse(response, err)
	}

	result := self.serialEval(request.Context(), eval, helloEval)
	if result == nil {
		return nil, fmt.Errorf("Timeout.")
	}
	return &result.httpResult, result.err
}

func (self *ClientStrategy) WsDialContext(ctx context.Context, url string, requestHeader http.Header) (*websocket.Conn, *http.Response, error) {
	eval := func(handleCtx context.Context, dialer *clientDialer) *evalResult {
		wsDialer := dialer.WsDialer(self.settings)
		startDial := time.Now()
		timing := &dialTiming{}
		wsConn, response, err := wsDialer.DialContext(timing.traceCtx(handleCtx, startDial), url, requestHeader)
		if log := self.log.V(2); log.Enabled() {
			if err != nil {
				self.log.Infof("[net]ws dial %s = %s\n", url, err)
			} else {
				self.log.Infof("[net]ws dial %s = %s\n", url, response.Status)
			}
		}

		// A WebSocket dial always establishes a fresh connection: gorilla
		// has no connection pool, and it reports the establishment through
		// GotConn right after the TCP+TLS dial (before the upgrade request),
		// so the sample uses the same connect-only basis as the HTTP paths.
		// If the trace did not fire, fall back to the full dial duration so
		// the successful dial still counts.
		freshConnect, establishTime := timing.sample()
		if !freshConnect {
			establishTime = time.Since(startDial)
		}
		if dialer.Update(handleCtx, err, time.Since(startDial)) {
			self.probes.record(err == nil)
		}
		if err == nil {
			dialer.observeConnect(establishTime)
		}

		// keep the platform's answer: "bad handshake" alone cannot tell a refused
		// token from an overloaded backend. The dialer's own health bookkeeping
		// above saw the original error and is unaffected.
		resultErr := err
		if err != nil && response != nil {
			resultErr = &wsStatusError{status: response.StatusCode, err: err}
		}

		return &evalResult{
			wsConn: wsConn,
			err:    resultErr,
			httpResult: httpResult{
				// status: response.Status,
				// statusCode: response.StatusCode,
				// header: response.Header.Clone(),
				response: response,
			},
		}
	}

	result, evalErr := self.parallelEvalErr(ctx, eval, true)
	if result == nil {
		return nil, nil, evalErr
	}
	return result.wsConn, result.response, result.err
}

func (self *ClientStrategy) collapseExtenderDialers() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	for dialer, _ := range self.dialers {
		if dialer.IsExtender() && dialer.IsLastSuccess() {
			if self.settings.ExtenderDropTimeout <= time.Now().Sub(dialer.lastErrorTime) {
				dialer.Close()
				delete(self.dialers, dialer)
			}
		}
	}
}

func (self *ClientStrategy) expandExtenderDialers() (expandedDialers []*clientDialer, expandedExtenderIps []netip.Addr) {

	// - distribute new ips evenly over new profiles
	// - distribute existing ids as weighted where needed
	// - `extenderIpSecrets` overrides new ips

	self.mutex.Lock()
	defer self.mutex.Unlock()

	if self.settings.ExpandExtenderProfileCount <= 0 {
		return []*clientDialer{}, []netip.Addr{}
	}

	visitedExtenderProfiles := map[ExtenderProfile]bool{}
	visitedExtenderIps := map[netip.Addr]bool{}

	for dialer, _ := range self.dialers {
		if dialer.IsExtender() {
			visitedExtenderProfiles[dialer.extenderConfig.Profile] = true
			visitedExtenderIps[dialer.extenderConfig.Ip] = true
		}
	}

	if self.settings.MaxExtenderCount <= len(visitedExtenderProfiles) {
		// at maximum extenders
		return []*clientDialer{}, []netip.Addr{}
	}

	extenderProfiles := EnumerateExtenderProfiles(
		min(self.settings.ExpandExtenderProfileCount, self.settings.MaxExtenderCount-len(visitedExtenderProfiles)),
		visitedExtenderProfiles,
	)

	extenderConfigs := []*ExtenderConfig{}
	if len(self.extenderIpSecrets) == 0 {

		// filter resolved ips by visited
		unusedExtenderIps := []netip.Addr{}
		for _, ip := range self.resolvedExtenderIps {
			if !visitedExtenderIps[ip] {
				unusedExtenderIps = append(unusedExtenderIps, ip)
			}
		}

		deviceIpv4 := nettest.SupportsIPv4()
		deviceIpv6 := nettest.SupportsIPv6()

		// expand the ips to have one new ip per profile
		if len(unusedExtenderIps) < len(extenderProfiles) {
			// iterate these for ips not used
			for _, network := range self.settings.ExtenderNetworks {
				if network.Addr().Is4() && deviceIpv4 || network.Addr().Is6() && deviceIpv6 {
					for ip := network.Addr(); network.Contains(ip); ip = ip.Next() {
						if !visitedExtenderIps[ip] {
							visitedExtenderIps[ip] = true
							expandedExtenderIps = append(expandedExtenderIps, ip)
						}
					}
				}
			}

			mathrand.Shuffle(len(expandedExtenderIps), func(i int, j int) {
				expandedExtenderIps[i], expandedExtenderIps[j] = expandedExtenderIps[j], expandedExtenderIps[i]
			})

			if len(extenderProfiles) <= len(expandedExtenderIps) {
				expandedExtenderIps = expandedExtenderIps[0:len(extenderProfiles)]
			}

			// if not enough ips, use DoH to load ips for the extender hostnames
			if len(expandedExtenderIps) < len(extenderProfiles) && 0 < len(self.settings.ExtenderHostnames) {

				// the network can be both ipv4 and ipv6
				if deviceIpv4 {
					ips := DohQuery(self.ctx, 4, "A", self.settings.DohSettings, self.settings.ExtenderHostnames...)
					for ip, _ := range ips {
						if !visitedExtenderIps[ip] {
							visitedExtenderIps[ip] = true
							expandedExtenderIps = append(expandedExtenderIps, ip)
						}
					}
				}
				if deviceIpv6 {
					ips := DohQuery(self.ctx, 6, "AAAA", self.settings.DohSettings, self.settings.ExtenderHostnames...)
					for ip, _ := range ips {
						if !visitedExtenderIps[ip] {
							visitedExtenderIps[ip] = true
							expandedExtenderIps = append(expandedExtenderIps, ip)
						}
					}
				}
			}

			unusedExtenderIps = append(unusedExtenderIps, expandedExtenderIps...)
		}

		// unused ips first
		mathrand.Shuffle(len(unusedExtenderIps), func(i int, j int) {
			unusedExtenderIps[i], unusedExtenderIps[j] = unusedExtenderIps[j], unusedExtenderIps[i]
		})
		n := min(len(extenderProfiles), len(unusedExtenderIps))
		for i := range n {
			extenderConfig := &ExtenderConfig{
				Profile: extenderProfiles[i],
				Ip:      unusedExtenderIps[i],
			}
			extenderConfigs = append(extenderConfigs, extenderConfig)
		}

		// existing ips distributed as weighted
		if n < len(extenderProfiles) {
			weights := map[netip.Addr]float32{}

			netWeight := float32(0)
			for dialer, _ := range self.dialers {
				if dialer.IsExtender() {
					w := dialer.Weight()
					weights[dialer.extenderConfig.Ip] = w
					netWeight += w
				}
			}
			for _, ip := range unusedExtenderIps {
				w := self.settings.ExtenderMinimumWeight
				weights[ip] = w
				netWeight += w
			}

			if 0 < len(weights) {
				ips := maps.Keys(weights)
				mathrand.Shuffle(len(ips), func(i int, j int) {
					ips[i], ips[j] = ips[j], ips[i]
				})

				for _, extenderProfile := range extenderProfiles[n:] {
					v := mathrand.Float32() * netWeight
					i := 0
					for i < len(ips)-1 {
						v -= weights[ips[i]]
						if v <= 0 {
							break
						}
						i += 1
					}
					extenderConfig := &ExtenderConfig{
						Profile: extenderProfile,
						Ip:      ips[i],
					}
					extenderConfigs = append(extenderConfigs, extenderConfig)
				}
			}
		}
	} else {
		ips := maps.Keys(self.extenderIpSecrets)
		for _, extenderProfile := range extenderProfiles {
			ip := ips[mathrand.Intn(len(ips))]
			extenderConfig := &ExtenderConfig{
				Profile: extenderProfile,
				Ip:      ip,
				Secret:  self.extenderIpSecrets[ip],
			}
			extenderConfigs = append(extenderConfigs, extenderConfig)
		}
	}

	for _, extenderConfig := range extenderConfigs {
		dialTlsContext := NewExtenderDialTlsContext(
			&self.settings.ConnectSettings,
			extenderConfig,
		)

		dialer := &clientDialer{
			minimumWeight:  self.settings.ExtenderMinimumWeight,
			priority:       100,
			dialTlsContext: dialTlsContext,
			extenderConfig: extenderConfig,
			settings:       self.settings,
		}
		expandedDialers = append(expandedDialers, dialer)
	}

	for _, dialer := range expandedDialers {
		self.dialers[dialer] = true
	}
	self.resolvedExtenderIps = append(self.resolvedExtenderIps, expandedExtenderIps...)

	return
}

// non-extender dialers are never dropped
type clientDialer struct {
	log Logger

	description   string
	minimumWeight float32
	// 0 is max
	priority int

	dialTlsContext DialTlsContextFunction
	// httpDialTlsContext serves the api client, which can negotiate h2.
	// dialTlsContext serves the websocket, which requires http/1.1; a single
	// h2-enabled tls dialer would hand gorilla/websocket a connection it
	// cannot speak. When nil, dialTlsContext serves both.
	httpDialTlsContext DialTlsContextFunction

	extenderConfig *ExtenderConfig

	mutex           sync.Mutex
	successCount    uint64
	errorCount      uint64
	lastSuccessTime time.Time
	lastErrorTime   time.Time

	// Composite scoring fields
	consecutiveErrors int   // error streak (resets on success)
	avgLatencyNanos   int64 // EMA of response latency in nanoseconds; 0 = no samples
	latencySamples    int   // successful dials the EMA is built from
	// connectLatencyNanos/connectSamples hold the connect-only EMA the smart
	// dialer orders by. They are folded ONLY for genuine connection
	// establishments (see observeConnect), so the legacy avgLatencyNanos above
	// keeps its byte-identical semantics for the smart_dialer-off path.
	connectLatencyNanos int64 // EMA of connection-establishment cost; 0 = no samples
	connectSamples      int   // genuine connection establishments the EMA is built from
	connectObservedAt   time.Time

	httpClient      *http.Client
	websocketDialer *websocket.Dialer

	settings *ClientStrategySettings
}

// CRITICAL #2 fix: dialerStats is a thread-safe snapshot of dialer performance
// fields. Use Stats() instead of reading successCount/errorCount/lastSuccessTime
// directly to avoid data races with concurrent Update() calls.
type dialerStats struct {
	successCount    uint64
	errorCount      uint64
	lastSuccessTime time.Time
}

// successRate is successCount over all counted outcomes, 0 with no samples.
func (self dialerStats) successRate() float32 {
	total := self.successCount + self.errorCount
	if total == 0 {
		return 0
	}
	return float32(self.successCount) / float32(total)
}

// sortDialersByHealth orders serial dialers by priority (lower first), then
// success rate (higher first), then most recent success. Each dialer's
// health is read once, under its mutex, before sorting: Update runs
// concurrently, so reading inside the comparator raced and could change the
// inputs partway through the sort. parallelEval, serialEval and the tests
// share this one ordering.
func sortDialersByHealth(dialers []*clientDialer) {
	type ranked struct {
		dialer *clientDialer
		stats  dialerStats
	}
	rs := make([]ranked, len(dialers))
	for i, d := range dialers {
		rs[i] = ranked{dialer: d, stats: d.Stats()}
	}
	slices.SortStableFunc(rs, func(a ranked, b ranked) int {
		if a.dialer.priority != b.dialer.priority {
			return a.dialer.priority - b.dialer.priority
		}
		if aRate, bRate := a.stats.successRate(), b.stats.successRate(); aRate != bRate {
			if aRate > bRate {
				return -1
			}
			return 1
		}
		return b.stats.lastSuccessTime.Compare(a.stats.lastSuccessTime)
	})
	for i, r := range rs {
		dialers[i] = r.dialer
	}

	// Measured-cost preference (opt-in): a transport whose cost has been
	// measured on this network sorts ahead of the static health order, fastest
	// first. Everything else keeps the order computed above, so no transport is
	// skipped and a network where the preferred one is blocked simply falls
	// over to the next.
	if SmartDialerEnabled() {
		ordered := orderDialersByMeasuredCost(dialers)
		copy(dialers, ordered)
	}
}

// Stats returns a thread-safe snapshot of the dialer's performance counters.
// Call this instead of reading fields directly in sort comparators.
func (self *clientDialer) Stats() dialerStats {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return dialerStats{
		successCount:    self.successCount,
		errorCount:      self.errorCount,
		lastSuccessTime: self.lastSuccessTime,
	}
}

func (self *clientDialer) HttpClient() *http.Client {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if self.httpClient == nil {
		httpDialTlsContext := self.httpDialTlsContext
		if httpDialTlsContext == nil {
			httpDialTlsContext = self.dialTlsContext
		}
		transport := &http.Transport{
			DialTLSContext:        httpDialTlsContext,
			IdleConnTimeout:       self.settings.ConnectSettings.IdleConnTimeout,
			TLSHandshakeTimeout:   self.settings.ConnectSettings.TlsTimeout,
			ResponseHeaderTimeout: self.settings.ConnectTimeout,
			ExpectContinueTimeout: self.settings.ConnectTimeout,
			DisableKeepAlives:     false,
			// A custom DialTLSContext disables net/http's automatic HTTP/2
			// attempt unless this is set, and the api path now offers h2
			// (net_tls.go clientHttpNextProtos). Without it the transport
			// would speak http/1.1 into an h2-negotiated connection.
			ForceAttemptHTTP2: true,
		}
		self.httpClient = &http.Client{
			Transport: transport,
			Timeout:   self.settings.RequestTimeout,
		}
	}
	return self.httpClient
}

func (self *clientDialer) WsDialer(settings *ClientStrategySettings) *websocket.Dialer {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if self.websocketDialer == nil {
		// pool, size := MessagePool(2048)
		self.websocketDialer = &websocket.Dialer{
			NetDialTLSContext: self.dialTlsContext,
			HandshakeTimeout:  settings.HandshakeTimeout,
			// ReadBufferSize: size,
			// WriteBufferSize: size,
			// WriteBufferPool: pool,
			EnableCompression: false,
		}
	}
	return self.websocketDialer
}

func (self *clientDialer) Weight() float32 {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	c := self.successCount + self.errorCount
	var baseScore float32
	if 0 < c {
		baseScore = float32(float64(self.successCount) / float64(c))
	} else {
		baseScore = self.minimumWeight
	}

	// Latency factor: penalise slow dialers.  Baseline is 500 ms; anything
	// faster gets a boost up to 1.2×, anything slower is penalised down to
	// 0.3×.  When no samples exist the factor is 1.0 (neutral).
	const baselineLatencyNanos int64 = 500_000_000 // 500 ms
	latencyFactor := float32(1.0)
	if self.avgLatencyNanos > 0 {
		ratio := float32(self.avgLatencyNanos) / float32(baselineLatencyNanos)
		if ratio < 1.0 {
			// faster than baseline → boost (max 1.2)
			latencyFactor = 1.0 + 0.2*(1.0-ratio)
			if latencyFactor > 1.2 {
				latencyFactor = 1.2
			}
		} else {
			// slower than baseline → penalise (min 0.3)
			latencyFactor = 1.0 / (1.0 + 0.5*(ratio-1.0))
			if latencyFactor < 0.3 {
				latencyFactor = 0.3
			}
		}
	}

	// Error streak penalty: 0.5^streak.  3 consecutive errors → 0.125×
	streakPenalty := float32(1.0)
	for i := 0; i < self.consecutiveErrors; i++ {
		streakPenalty *= 0.5
	}

	w := baseScore * latencyFactor * streakPenalty
	if w < self.minimumWeight {
		w = self.minimumWeight
	}
	return w
}

// dialTiming observes one HTTP attempt's connection lifecycle. It reports
// whether the attempt actually dialed (obtained a fresh connection) and, if
// so, how long establishing that connection took. A request served from a
// reused keep-alive connection performs no dial, so its (fast) round trip
// must not enter the dial-cost average: folding it in would drift the
// average toward request-only cost and stop reflecting the front-loaded
// work a DPI-circumvention transport pays (fragmented or reordered TLS is
// set up at connect, not on every request an open connection carries).
type dialTiming struct {
	mutex         sync.Mutex
	fresh         bool
	establishTime time.Duration
}

// trace attaches an httptrace to the request that records whether the
// connection is fresh and how long its establishment took, measured from
// start. Callbacks may fire on transport goroutines, so state is guarded.
func (self *dialTiming) trace(ctx context.Context, request *http.Request, start time.Time) *http.Request {
	return request.WithContext(self.traceCtx(ctx, start))
}

// traceCtx attaches an httptrace to a context that records whether the
// connection established is fresh and how long its establishment took,
// measured from start. Used by dialers that take a context directly (the
// WebSocket path); gorilla's DialContext wires the context through to the
// transport, which fires ClientTrace callbacks on it.
func (self *dialTiming) traceCtx(ctx context.Context, start time.Time) context.Context {
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			if info.Reused {
				return
			}
			self.mutex.Lock()
			self.fresh = true
			self.establishTime = time.Since(start)
			self.mutex.Unlock()
		},
	}
	return httptrace.WithClientTrace(ctx, trace)
}

// sample returns whether a fresh connection was established and, if so, how
// long that establishment took.
func (self *dialTiming) sample() (bool, time.Duration) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.fresh, self.establishTime
}

// Update records a dial outcome and reports whether it counted. An error
// after the handle context was canceled is a race loser, not a failure, and
// is not counted.
//
// The EMA of response latency folds every success (byte-identical legacy
// behavior; this is the value the smart_dialer-off Weight() path reads). The
// smart dialer's connect-only measurement has its own fold, observeConnect,
// called only for genuine connection establishments.
func (self *clientDialer) Update(handleCtx context.Context, err error, duration time.Duration) bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if err == nil {
		self.successCount += 1
		self.lastSuccessTime = time.Now()
		self.consecutiveErrors = 0
		// Update exponential moving average of latency (α = 0.3)
		const alpha = 0.3
		nanos := duration.Nanoseconds()
		if self.avgLatencyNanos == 0 {
			self.avgLatencyNanos = nanos
		} else {
			self.avgLatencyNanos = int64(alpha*float64(nanos) + (1-alpha)*float64(self.avgLatencyNanos))
		}
		self.latencySamples += 1
		return true
	} else {
		select {
		case <-handleCtx.Done():
			// ignore any error is the context is canceled
		default:
			self.errorCount += 1
			self.lastErrorTime = time.Now()
			self.consecutiveErrors++
			if self.consecutiveErrors > 20 {
				self.consecutiveErrors = 20
			}
			return true
		}
	}
	return false
}

// observeConnect folds one connection-establishment cost into the average the
// smart dialer orders by (α = 0.3). Call it only for a genuine dial: a request
// served from a reused keep-alive connection performed no dial, and folding
// its fast round trip in would drift the average toward request-only cost and
// stop reflecting the front-loaded work a DPI-circumvention transport pays
// (fragmented or reordered TLS is set up at connect, not on every request an
// open connection carries). This is a separate EMA from Update's
// avgLatencyNanos so the legacy smart_dialer-off path keeps its byte-identical
// full-duration folding.
func (self *clientDialer) observeConnect(duration time.Duration) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	const latencyAlpha = 0.3
	nanos := duration.Nanoseconds()
	if self.connectSamples == 0 {
		self.connectLatencyNanos = nanos
	} else {
		self.connectLatencyNanos = int64(latencyAlpha*float64(nanos) + (1-latencyAlpha)*float64(self.connectLatencyNanos))
	}
	self.connectSamples += 1
	self.connectObservedAt = time.Now()
}

func (self *clientDialer) IsExtender() bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	return self.extenderConfig != nil
}

func (self *clientDialer) IsLastSuccess() bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	return !self.lastSuccessTime.Before(self.lastErrorTime)
}

func (self *clientDialer) ResetHealth() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.successCount = 0
	self.errorCount = 0
	self.lastSuccessTime = time.Time{}
	self.lastErrorTime = time.Time{}
	self.consecutiveErrors = 0
	self.avgLatencyNanos = 0
	self.latencySamples = 0
	self.connectLatencyNanos = 0
	self.connectSamples = 0
	self.connectObservedAt = time.Time{}
}

func (self *clientDialer) String() string {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	latencyStr := ""
	if self.avgLatencyNanos > 0 {
		latencyStr = fmt.Sprintf(" avg=%dms", time.Duration(self.avgLatencyNanos).Milliseconds())
	}

	if self.extenderConfig != nil {
		return fmt.Sprintf("extender (%v) success=%d error=%d%s", self.extenderConfig, self.successCount, self.errorCount, latencyStr)
	} else if self.settings != nil && self.settings.ProxySettings != nil {
		var clients int64
		var maxAge time.Duration
		bw := RegisteredProxyBandwidth(self.settings.ProxySettings.Index)
		if bw != nil {
			clients = bw.Clients.Load()
			maxAge = bw.MaxAge()
		}
		ageStr := ""
		if clients > 0 {
			ageStr = fmt.Sprintf(" age=%s", maxAge.Round(time.Second).String())
		}
		return fmt.Sprintf("proxy[%d] (%s) [%s] success=%d error=%d%s clients=%d%s", self.settings.ProxySettings.Index, self.settings.ProxySettings.Address, self.description, self.successCount, self.errorCount, latencyStr, clients, ageStr)
	} else {
		return fmt.Sprintf("[direct] %s success=%d error=%d%s", self.description, self.successCount, self.errorCount, latencyStr)
	}
}

// routeIdentity is the stable identity of the route this dialer takes, in the
// shape of String() without the live counters: the proxy index and address, or
// just the description for a direct dial. The strategy's dialers are cached
// and reused, and a rebuild (a proxy reload, a recreated strategy) issues new
// pointers for the same route, so the framed-upgrade rejection cache keys on
// this identity instead of the pointer (see upgradeRouteScope).
func (self *clientDialer) routeIdentity() string {
	if self.extenderConfig != nil {
		return fmt.Sprintf("extender|%v", self.extenderConfig)
	}
	if self.settings != nil && self.settings.ProxySettings != nil {
		return fmt.Sprintf("proxy|%d|%s|%s", self.settings.ProxySettings.Index, self.settings.ProxySettings.Address, self.description)
	}
	return fmt.Sprintf("direct|%s", self.description)
}

func (self *clientDialer) Close() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if self.httpClient != nil {
		self.httpClient.CloseIdleConnections()
		self.httpClient = nil
	}
}

type ApiCallback[R any] interface {
	Result(result R, err error)
}

// for internal use
type simpleApiCallback[R any] struct {
	callback func(result R, err error)
}

func NewApiCallback[R any](callback func(result R, err error)) ApiCallback[R] {
	return &simpleApiCallback[R]{
		callback: callback,
	}
}

func NewNoopApiCallback[R any]() ApiCallback[R] {
	return &simpleApiCallback[R]{
		callback: func(result R, err error) {},
	}
}

func (self *simpleApiCallback[R]) Result(result R, err error) {
	self.callback(result, err)
}

type ApiCallbackResult[R any] struct {
	Result R
	Error  error
}

func NewBlockingApiCallback[R any](ctx context.Context) (ApiCallback[R], chan ApiCallbackResult[R]) {
	c := make(chan ApiCallbackResult[R])
	apiCallback := NewApiCallback[R](func(result R, err error) {
		r := ApiCallbackResult[R]{
			Result: result,
			Error:  err,
		}
		select {
		case <-ctx.Done():
		case c <- r:
		}
	})
	return apiCallback, c
}

// HttpStatusError is a non-200 HTTP response from an API call.
//
// Error returns exactly the text this error had before it carried a type, so
// every log line and every caller that prints it is unchanged. The status
// code makes a confirmed rejection distinguishable from a transport error or
// a 5xx without matching on text.
type HttpStatusError struct {
	StatusCode int
	Status     string
	Body       []byte
}

func (self *HttpStatusError) Error() string {
	body := strings.TrimSpace(string(self.Body))
	if strings.HasPrefix(body, "<") {
		return fmt.Sprintf("%s: <html error page, %d bytes>", self.Status, len(self.Body))
	}
	return fmt.Sprintf("%s: %s", self.Status, body)
}

func (self *HttpStatusError) HTTPStatusCode() int {
	return self.StatusCode
}

// httpErrorFromResponse builds an error from a non-200 response. Some
// front-end proxies (nginx, Cloudflare) return an HTML error page as the
// body instead of plain text or JSON; embedding that verbatim produces
// multi-line, unreadable log spam at every call site that prints the error.
// Collapse an HTML body down to a short descriptor instead.
func httpErrorFromResponse(status string, bodyBytes []byte) error {
	statusCode := 0
	if fields := strings.Fields(status); 0 < len(fields) {
		if parsed, err := strconv.Atoi(fields[0]); err == nil {
			statusCode = parsed
		}
	}
	return &HttpStatusError{StatusCode: statusCode, Status: status, Body: bodyBytes}
}

func HttpPostWithStrategyRaw(
	ctx context.Context,
	clientStrategy *ClientStrategy,
	requestUrl string,
	requestBodyBytes []byte,
	byJwt string,
) ([]byte, error) {
	request, err := http.NewRequestWithContext(
		ctx,
		"POST",
		requestUrl,
		bytes.NewReader(requestBodyBytes),
	)
	if err != nil {
		return nil, err
	}

	request.Header.Add("Content-Type", "text/json")

	if byJwt != "" {
		auth := fmt.Sprintf("Bearer %s", byJwt)
		request.Header.Add("Authorization", auth)
	}

	helloRequest, err := HelloRequestFromUrl(ctx, requestUrl, byJwt)
	if err != nil {
		return nil, err
	}

	r, err := clientStrategy.HttpSerial(request, helloRequest)
	if err != nil {
		return nil, err
	}

	if http.StatusOK != r.response.StatusCode {
		// the response body is the error message
		err = httpErrorFromResponse(r.response.Status, r.bodyBytes)
		return nil, err
	}

	return r.bodyBytes, nil
}

func HttpPostWithStrategy[R any](
	ctx context.Context,
	clientStrategy *ClientStrategy,
	requestUrl string,
	args any,
	byJwt string,
	result R,
	callback ApiCallback[R],
) (R, error) {
	return HttpPostWithRawFunction(
		ctx,
		func(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error) {
			return HttpPostWithStrategyRaw(ctx, clientStrategy, requestUrl, requestBodyBytes, byJwt)
		},
		requestUrl,
		args,
		byJwt,
		result,
		callback,
	)
}

func HttpPostWithRawFunction[R any](
	ctx context.Context,
	httpPostRaw HttpPostRawFunction,
	requestUrl string,
	args any,
	byJwt string,
	result R,
	callback ApiCallback[R],
) (R, error) {
	var requestBodyBytes []byte
	if args == nil {
		requestBodyBytes = make([]byte, 0)
	} else {
		var err error
		requestBodyBytes, err = json.Marshal(args)
		if err != nil {
			var empty R
			callback.Result(empty, err)
			return empty, err
		}
	}

	bodyBytes, err := httpPostRaw(ctx, requestUrl, requestBodyBytes, byJwt)
	if err != nil {
		var empty R
		callback.Result(empty, err)
		return empty, err
	}

	err = json.Unmarshal(bodyBytes, &result)
	if err != nil {
		var empty R
		callback.Result(empty, err)
		return empty, err
	}

	callback.Result(result, nil)
	return result, nil
}

func HelloRequestFromUrl(ctx context.Context, requestUrl string, byJwt string) (*http.Request, error) {
	u, err := url.Parse(requestUrl)
	if err != nil {
		return nil, err
	}
	helloUrl := fmt.Sprintf("%s://%s/hello", u.Scheme, u.Host)

	req, err := http.NewRequestWithContext(ctx, "GET", helloUrl, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Add("Content-Type", "text/json")

	if byJwt != "" {
		auth := fmt.Sprintf("Bearer %s", byJwt)
		req.Header.Add("Authorization", auth)
	}

	return req, nil
}

func HttpGetWithStrategyRaw(
	ctx context.Context,
	clientStrategy *ClientStrategy,
	requestUrl string,
	byJwt string,
) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, "GET", requestUrl, nil)
	if err != nil {
		return nil, err
	}

	request.Header.Add("Content-Type", "text/json")

	if byJwt != "" {
		auth := fmt.Sprintf("Bearer %s", byJwt)
		request.Header.Add("Authorization", auth)
	}

	r, err := clientStrategy.HttpParallel(request)
	if err != nil {
		return nil, err
	}

	if http.StatusOK != r.response.StatusCode {
		// the response body is the error message
		err = httpErrorFromResponse(r.response.Status, r.bodyBytes)
		return nil, err
	}

	return r.bodyBytes, nil
}

func HttpGetWithStrategy[R any](
	ctx context.Context,
	clientStrategy *ClientStrategy,
	requestUrl string,
	byJwt string,
	result R,
	callback ApiCallback[R],
) (R, error) {
	return HttpGetWithRawFunction[R](
		ctx,
		func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
			return HttpGetWithStrategyRaw(ctx, clientStrategy, requestUrl, byJwt)
		},
		requestUrl,
		byJwt,
		result,
		callback,
	)
}

func HttpGetWithRawFunction[R any](
	ctx context.Context,
	httpGetRaw HttpGetRawFunction,
	requestUrl string,
	byJwt string,
	result R,
	callback ApiCallback[R],
) (R, error) {
	bodyBytes, err := httpGetRaw(ctx, requestUrl, byJwt)
	if err != nil {
		var empty R
		callback.Result(empty, err)
		return empty, err
	}

	err = json.Unmarshal(bodyBytes, &result)
	if err != nil {
		var empty R
		callback.Result(empty, err)
		return empty, err
	}

	callback.Result(result, nil)
	return result, nil
}

/**
 * Streaming POST
 */
func HttpPostStreamWithStrategyRaw(
	ctx context.Context,
	requestUrl string,
	body io.Reader,
	byJwt string,
) ([]byte, error) {

	req, err := http.NewRequestWithContext(ctx, "POST", requestUrl, body)
	if err != nil {
		return nil, err
	}

	if byJwt != "" {
		req.Header.Set("Authorization", "Bearer "+byJwt)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	defer res.Body.Close()
	bodyBytes, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, err
	}

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", res.Status, strings.TrimSpace(string(bodyBytes)))
	}

	return bodyBytes, nil
}

type HttpPostStreamRawFunction func(
	ctx context.Context,
	requestUrl string,
	body io.Reader,
	byJwt string,
) ([]byte, error)

func HttpPostWithStreamFunction[R any](
	ctx context.Context,
	httpPostRaw HttpPostStreamRawFunction,
	requestUrl string,
	body io.Reader,
	byJwt string,
	result R,
	callback ApiCallback[R],
) (R, error) {
	bodyBytes, err := httpPostRaw(ctx, requestUrl, body, byJwt)
	if err != nil {
		var empty R
		callback.Result(empty, err)
		return empty, err
	}
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		var empty R
		callback.Result(empty, err)
		return empty, err
	}
	callback.Result(result, nil)
	return result, nil
}
