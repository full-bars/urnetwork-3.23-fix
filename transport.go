package connect

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"math"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	// "runtime/debug"

	"golang.org/x/exp/maps"

	"github.com/gorilla/websocket"
	quic "github.com/quic-go/quic-go"

	"github.com/urnetwork/connect/protocol"
)

// One throttle per high-volume error class. Each emits at most one line per
// minute and counts suppressions in between (see logThrottle).
var (
	authErrThrottle   = newLogThrottle(time.Minute)
	selectErrThrottle = newLogThrottle(time.Minute)
	writeErrThrottle  = newLogThrottle(time.Minute)
)

// backendFailState is the backend health signal: a count of consecutive
// failures and the time of the most recent one. The two are one value, not two.
// isBackendDegraded needs them from the SAME transition — read separately, a
// stale-streak reset landing between the two loads pairs the old count with the
// new timestamp, and the backend reads as degraded on the strength of a single
// recent failure. Storing an immutable snapshot behind atomic.Pointer makes
// that pairing structural, and keeps the read lock-free: isBackendDegraded runs
// per contract creation and per window resize, so a mutex on the read path
// would put process-wide contention on a hot read to close a race that only
// writers can create.
type backendFailState struct {
	// fails counts backend failures (auth or OOB) since the last success. Any
	// successful connect or OOB result resets it to 0. A real platform outage
	// drives this up fast because every attempt fails with nothing to reset it;
	// isolated transient timeouts never accumulate because an interleaved
	// success clears the count. isBackendDegraded() requires this to cross a
	// threshold so one or two stray failures are not mistaken for an outage.
	fails int64
	// lastNano is the time of the most recent failure, in unix nanos. Updated
	// on every failure, not rate-limited. Used by isBackendDegraded() as the
	// recency guard. Zero means no outstanding failures.
	lastNano int64
}

// backendFail holds the current backend health signal. Never nil after init,
// so readers do not have to nil-check.
//
// This is process-wide rather than per-Client on purpose: "is the control API
// reachable from this host" is a property of the host, not of any one client.
// A host running many proxies gets a stronger signal from sharing the state,
// because a success on any one clears it — so a single misbehaving proxy
// cannot trip the threshold on its own.
//
// The known limit of that framing: a process talking to MULTIPLE platform urls
// (separate network spaces) shares one signal across them, so a dead custom
// endpoint can gate a healthy one. Accepted for now — the fleet runs one
// platform per process — and keying this state by platform url is the upgrade
// path if that changes.
var backendFail atomic.Pointer[backendFailState]

// backendFailMu serializes writers. The pointer store is atomic, but building
// the next state reads the current one first, and that read-modify-write has to
// be exclusive or two concurrent failures both read the same count and one
// increment is lost.
var backendFailMu sync.Mutex

func init() {
	backendFail.Store(&backendFailState{})
}

// activeProxyConnections counts proxy transports that are currently registered
// with the route manager (i.e. authenticated and live on the platform). It is
// incremented after UpdateTransport in runH1/runH3 and decremented when the
// transport is torn down. ActiveProxyConnections() exposes it for the [health]
// heartbeat as proxies=N.
var activeProxyConnections int64

func ActiveProxyConnections() int64 {
	return atomic.LoadInt64(&activeProxyConnections)
}

// backendDegradedFailThreshold is the number of consecutive backend failures
// (with no intervening success) required before the backend is considered
// degraded. Set above the level of normal transient churn.
const backendDegradedFailThreshold = 3

// backendDegradedWindow is how recent the last failure must be for the counter
// to be trusted. Comfortably larger than the 60s reconnect-backoff cap so a real
// outage's retry attempts always read as recent, while a stale count left by an
// old blip on an idle provider does not.
const backendDegradedWindow = 2 * time.Minute

func shouldLogAuthErr() (bool, int64)   { return authErrThrottle.Allow(time.Now()) }
func shouldLogSelectErr() (bool, int64) { return selectErrThrottle.Allow(time.Now()) }

func shouldLogWriteErr() (bool, int64) {
	return writeErrThrottle.Allow(time.Now())
}

// isBackendDegraded returns true when backend failures have accumulated past
// the threshold with no intervening success and the last failure is recent.
// This distinguishes a sustained, broad outage (every attempt failing) from the
// isolated single-connection timeouts that are normal churn on a busy provider.
//
// During an outage where the transport stays connected (the control API down,
// the websocket alive), auth never re-runs and the gated CreateContract is the
// only other success source — so nothing can SUCCEED to clear the state.
// Recovery then rides the recency window instead: after backendDegradedWindow
// without failures this reads false, the sequences that tick before three
// fresh failures land probe the backend, and either one succeeds (clearing the
// state) or the gate re-trips. The steady state of a long OOB-only outage is
// therefore a bounded probe burst every ~backendDegradedWindow, not a latched
// stop — which is also what makes recovery need no timer of its own.
func isBackendDegraded() bool {
	// one load: the count and the timestamp are guaranteed to come from the
	// same transition, so no interleaving writer can pair a stale count with a
	// fresh timestamp under the reader.
	state := backendFail.Load()
	if state.fails < backendDegradedFailThreshold {
		return false
	}
	return time.Now().UnixNano()-state.lastNano < int64(backendDegradedWindow)
}

// IsBackendDegraded is the exported form for use by the provider binary.
func IsBackendDegraded() bool {
	return isBackendDegraded()
}

// noteBackendFailure records a failed backend round-trip (auth or contract OOB).
//
// A streak older than backendDegradedWindow is discarded rather than extended.
// Without that, an idle provider that saw a few failures long ago and simply
// stopped retrying would carry the old count forward: the next single failure
// would push the total past the threshold with a fresh timestamp, and the
// backend would read as degraded on the strength of one recent failure. The
// threshold means "consecutive failures within the window", so a gap that
// invalidates the streak for isBackendDegraded must also reset it here.
func noteBackendFailure() {
	now := time.Now().UnixNano()

	backendFailMu.Lock()
	defer backendFailMu.Unlock()

	state := backendFail.Load()
	fails := int64(1)
	if state.lastNano != 0 && now-state.lastNano < int64(backendDegradedWindow) {
		fails = state.fails + 1
	}
	backendFail.Store(&backendFailState{fails: fails, lastNano: now})
}

// noteBackendSuccess clears the recorded backend failure state after a
// successful auth or OOB round-trip.
//
// It takes backendFailMu so the clear cannot land in the middle of a
// concurrent noteBackendFailure's read-modify-write, which would otherwise
// resurrect the count it just cleared.
func noteBackendSuccess() {
	backendFailMu.Lock()
	defer backendFailMu.Unlock()

	backendFail.Store(&backendFailState{})
}

// note that it is possible to have multiple transports for the same client destination
// e.g. platform, p2p, and a bunch of extenders

// extenders are identified and credited with the platform by ip address
// they forward to a special port, 8443, that whitelists their ip without rate limiting
// when an extender gets an http message from a client, it always connects tcp to connect.bringyour.com:8443
// appends the proxy protocol headers, and then forwards the bytes from the client
// https://docs.nginx.com/nginx/admin-guide/load-balancer/using-proxy-protocol/
// rate limit using $proxy_protocol_addr https://www.nginx.com/blog/rate-limiting-nginx/
// add the source ip as the X-Extender header

// the transport attempts to upgrade from http1 to http3
// versus the h1 transport, h3 is:
// - more cpu efficient.
//   The quic stream does not need to mask/unmask each byte before TLS.
// - better throughput on poor networks.
//   quic optimizes congestion control to better handle poor network conditions.
// However, h3 is not available in all locations due to dpi/filtering.
// When available, it takes precedence over the default transport.

// packet translation mode gives options for how udp packets are formed on the wire
// We include options here that are known to help with availability

// When packet translation is set, the upgrade mode must be h3 only

// 1: initial version
// 2: latency and speed test support
const TransportVersion = 2

// turn this on to be extra careful about returning all messages
// note we don't run this because it's most efficient to let the gc handle some infrequent orphaned messages
const DebugCloseSend = false

type TransportControl = byte

const (
	TransportControlSpeedStart TransportControl = 1
	TransportControlSpeedStop  TransportControl = 2
)

type TransportMode string

// in order of increasing preference
const (
	// start all modes in skewed parallel and choose the best one
	TransportModeAuto      TransportMode = "auto"
	TransportModeH3DnsPump TransportMode = "h3dnspump"
	TransportModeH3Dns     TransportMode = "h3dns"
	TransportModeH1        TransportMode = "h1"
	TransportModeH3        TransportMode = "h3"
	TransportModeNone      TransportMode = ""
)

type ClientAuth struct {
	ByJwt string
	// ClientId Id
	InstanceId Id
	AppVersion string
}

func (self *ClientAuth) ClientId() (Id, error) {
	byJwt, err := ParseByJwtUnverified(self.ByJwt)
	if err != nil {
		return Id{}, err
	}
	return byJwt.ClientId, nil
}

// (ctx, network, address)
// type DialContextFunc func(ctx context.Context, network string, address string) (net.Conn, error)

type PlatformTransportSettings struct {
	HttpConnectTimeout   time.Duration
	WsHandshakeTimeout   time.Duration
	QuicConnectTimeout   time.Duration
	QuicHandshakeTimeout time.Duration
	QuicTlsConfig        *tls.Config
	AuthTimeout          time.Duration
	ReconnectTimeout     time.Duration
	PingTimeout          time.Duration
	WriteTimeout         time.Duration
	ReadTimeout          time.Duration
	TransportGenerator   func() (sendTransport Transport, receiveTransport Transport)
	TransportBufferSize  int
	InactiveDrainTimeout time.Duration
	// it smoothes out the h3 transition to not start/stop h1 if h3 connects in this time
	ModeInitialDelay time.Duration

	// MinConnectDelay time.Duration
	// MaxConnectDelay time.Duration

	ProtocolVersion int

	H3Port  int
	DnsPort int

	// FIXME
	DnsTlds        [][]byte
	V2H1Auth       bool
	FramerSettings *FramerSettings

	// Log, when set, is used by the transport.
	// nil resolves to DefaultLogger().
	Log Logger

	PtDnsSlowMultiple int
}

func DefaultPlatformTransportSettings() *PlatformTransportSettings {
	tlsConfig, err := DefaultTlsConfig()
	if err != nil {
		panic(err)
	}
	return &PlatformTransportSettings{
		HttpConnectTimeout:   15 * time.Second,
		WsHandshakeTimeout:   15 * time.Second,
		QuicConnectTimeout:   15 * time.Second,
		QuicHandshakeTimeout: 15 * time.Second,
		QuicTlsConfig:        tlsConfig,
		AuthTimeout:          5 * time.Second,
		ReconnectTimeout:     5 * time.Second,
		PingTimeout:          5 * time.Second,
		WriteTimeout:         10 * time.Second,
		ReadTimeout:          30 * time.Second,
		TransportBufferSize:  16,
		InactiveDrainTimeout: 30 * time.Second,
		ModeInitialDelay:     2 * time.Second,
		// MinConnectDelay:      0,
		// MaxConnectDelay:      1 * time.Second,
		ProtocolVersion: DefaultProtocolVersion,
		H3Port:          443,
		DnsPort:         53,
		// FIXME
		DnsTlds: [][]byte{[]byte("ur.xyz.")},
		// servers are migrated on 2025-06-12. We can remove this and always use true.
		V2H1Auth: true,
		// the platform transport must carry the per-peer encryption handshake,
		// so its framer max is the connect runtime minimum message length
		FramerSettings:    DefaultFramerSettings(int(DefaultClientSettings().MinimumMessageLenLimit())),
		PtDnsSlowMultiple: 4,
	}
}

type PlatformTransport struct {
	ctx    context.Context
	cancel context.CancelFunc
	log    Logger

	clientStrategy *ClientStrategy
	routeManager   *RouteManager

	platformUrl string
	auth        *ClientAuth

	settings *PlatformTransportSettings

	stateLock sync.Mutex
	// notified when availableModes changes. availableModes is a map, so it
	// cannot be a MonitorValue; the notify is issued inside the same locked
	// scope as the mutation (see setModeAvailable)
	availableModeMonitor *Monitor
	availableModes       map[TransportMode]bool
	targetMode           TransportMode
	// the elected active mode, watched by every transport's mode gate and
	// inactive-drain watchdog. a MonitorValue so the mutation cannot be
	// separated from its notification, and so re-electing the same mode does
	// not wake the election loop's own watchers
	mode *MonitorValue[TransportMode]
}

func NewPlatformTransportWithDefaults(
	ctx context.Context,
	clientStrategy *ClientStrategy,
	routeManager *RouteManager,
	platformUrl string,
	auth *ClientAuth,
) *PlatformTransport {
	return NewPlatformTransport(
		ctx,
		clientStrategy,
		routeManager,
		platformUrl,
		auth,
		DefaultPlatformTransportSettings(),
	)
}

func NewPlatformTransport(
	ctx context.Context,
	clientStrategy *ClientStrategy,
	routeManager *RouteManager,
	platformUrl string,
	auth *ClientAuth,
	settings *PlatformTransportSettings,
) *PlatformTransport {
	return NewPlatformTransportWithTargetMode(
		ctx,
		clientStrategy,
		routeManager,
		platformUrl,
		auth,
		TransportModeAuto,
		settings,
	)
}

func NewPlatformTransportWithTargetMode(
	ctx context.Context,
	clientStrategy *ClientStrategy,
	routeManager *RouteManager,
	platformUrl string,
	auth *ClientAuth,
	targetMode TransportMode,
	settings *PlatformTransportSettings,
) *PlatformTransport {
	cancelCtx, cancel := context.WithCancel(ctx)
	log := loggerOrDefault(settings.Log)
	// propagate so a transport-level logger covers the framer
	if settings.FramerSettings != nil && settings.FramerSettings.Log == nil {
		settings.FramerSettings.Log = log
	}
	transport := &PlatformTransport{
		ctx:    cancelCtx,
		cancel: cancel,
		log:    log,
		// cancel: func() {
		// 	select {
		// 	case <- ctx.Done():
		// 	default:
		// 		debug.PrintStack()
		// 		cancel()
		// 	}
		// },
		clientStrategy:       clientStrategy,
		routeManager:         routeManager,
		platformUrl:          platformUrl,
		auth:                 auth,
		settings:             settings,
		availableModeMonitor: NewMonitor(),
		availableModes:       map[TransportMode]bool{},
		targetMode:           targetMode,
		mode:                 NewMonitorValue[TransportMode](TransportModeNone),
	}
	go HandleError(transport.run, cancel)
	return transport
}

// the auth is used on future connections
func (self *PlatformTransport) SetAuth(auth *ClientAuth) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.auth = auth
}

func (self *PlatformTransport) setModeAvailable(mode TransportMode, available bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.availableModes[mode] == available {
		return
	}
	self.availableModes[mode] = available
	self.availableModeMonitor.NotifyAll()
}

func (self *PlatformTransport) modesAvailable() (map[TransportMode]bool, chan struct{}) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return maps.Clone(self.availableModes), self.availableModeMonitor.NotifyChannel()
}

func (self *PlatformTransport) setActiveMode(mode TransportMode) {
	self.mode.Set(mode)
}

func (self *PlatformTransport) activeMode() (TransportMode, chan struct{}) {
	return self.mode.Get()
}

// transportModePreferences ranks the real transport modes. LOWER IS BETTER (see
// isBetterMode). TransportModeNone is deliberately NOT a key: modePreference
// ranks it, and any unknown mode, worse than every real mode. Leaving it out of
// the table and reading the map directly scored it 0 — better than everything —
// which is why no mode gate ever parked and the election could not distinguish
// "no transport" from "the best transport".
//
// Two tiers, with a tie inside each:
//   - the direct modes (h3, h1) are equally preferred. whichever connects first
//     becomes active and the other does not preempt it; the election is sticky
//     among equals (see run).
//   - the packet translation modes (h3dns, h3dnspump) tunnel over dns to stay
//     reachable where the direct modes are filtered. they are an availability
//     fallback, so they rank below the direct modes and are equally preferred
//     among themselves.
var transportModePreferences = map[TransportMode]int{
	TransportModeH3: 1,
	TransportModeH1: 1,

	TransportModeH3Dns:     2,
	TransportModeH3DnsPump: 2,
}

// modePreferenceNone ranks TransportModeNone — the absence of a transport — and
// any mode missing from the table as worse than every real mode.
const modePreferenceNone = math.MaxInt

func modePreference(mode TransportMode) int {
	if preference, ok := transportModePreferences[mode]; ok {
		return preference
	}
	return modePreferenceNone
}

func (self *PlatformTransport) run() {
	defer self.cancel()

	// TODO udp protocols need proxy protocol support in the load balancer
	// see https://github.com/nginx/nginx/issues/1061
	switch self.targetMode {
	case TransportModeAuto:
		go HandleError(func() {
			self.runH1(0)
		}, self.cancel)
		// go HandleError(func() {
		// 	self.runH3(TransportModeH3, 0, 1)
		// }, self.cancel)
		// go HandleError(func() {
		// 	self.runH3(TransportModeH3Dns, self.settings.ModeInitialDelay, self.settings.PtDnsSlowMultiple)
		// }, self.cancel)
		// go HandleError(func() {
		// 	self.runH3(TransportModeH3DnsPump, self.settings.ModeInitialDelay*2, self.settings.PtDnsSlowMultiple)
		// }, self.cancel)
	case TransportModeH3:
		go HandleError(func() {
			self.runH3(TransportModeH3, 0, 1)
		}, self.cancel)
	case TransportModeH1:
		go HandleError(func() {
			self.runH1(0)
		}, self.cancel)
	case TransportModeH3Dns:
		go HandleError(func() {
			self.runH3(TransportModeH3Dns, 0, self.settings.PtDnsSlowMultiple)
		}, self.cancel)
	case TransportModeH3DnsPump:
		go HandleError(func() {
			self.runH3(TransportModeH3DnsPump, 0, self.settings.PtDnsSlowMultiple)
		}, self.cancel)
	}

	for {
		available, notify := self.modesAvailable()

		// descending preference. the comparator must be consistent: the previous
		// one returned 1 for both (a, b) and (b, a) when the preferences tied
		// (h3 and h1 do), and `maps.Keys` is randomly ordered, so the election
		// picked an arbitrary winner among tied modes on every pass — flipping
		// the active mode and thrashing the gates. break ties on the mode name
		orderedModes := maps.Keys(transportModePreferences)
		slices.SortFunc(orderedModes, func(a TransportMode, b TransportMode) int {
			preferenceA := modePreference(a)
			preferenceB := modePreference(b)
			if preferenceA < preferenceB {
				return -1
			} else if preferenceB < preferenceA {
				return 1
			}
			return strings.Compare(string(a), string(b))
		})
		bestMode := TransportModeNone
		for _, mode := range orderedModes {
			if available[mode] {
				bestMode = mode
				break
			}
		}

		// equally preferred modes do not preempt each other: whichever connected
		// first stays active (h3 and h1 tie, as do h3dns and h3dnspump). the
		// active mode changes only when something strictly better becomes
		// available, or when it is no longer available itself — in which case it
		// falls back to the best that remains, and to TransportModeNone when
		// nothing does.
		activeMode := self.mode.Value()
		if !available[activeMode] || isBetterMode(bestMode, activeMode) {
			activeMode = bestMode
		}
		self.setActiveMode(activeMode)

		select {
		case <-notify:
		case <-self.ctx.Done():
			return
		}
	}
}

// isBetterMode reports whether mode is strictly preferred over other. Lower
// preference values are better; TransportModeNone is worse than everything.
func isBetterMode(mode TransportMode, other TransportMode) bool {
	return modePreference(mode) < modePreference(other)
}

// standDown reports whether a transport running mode should stand down because a
// strictly better mode is currently active, along with the channel that closes
// when the active mode changes. A transport runs when it is the active mode, or
// when nothing better than it is active — including at startup, where the active
// mode is TransportModeNone (worse than every real mode) precisely so that the
// first transport is admitted and can make itself available.
func (self *PlatformTransport) standDown(mode TransportMode) (bool, chan struct{}) {
	activeMode, notify := self.activeMode()
	return isBetterMode(activeMode, mode), notify
}

// proxyIndex returns this transport's proxy list index when running behind a
// proxy, or ok=false in non-proxy (direct) mode.
func (self *PlatformTransport) proxyIndex() (int, bool) {
	if self.clientStrategy == nil || self.clientStrategy.settings == nil {
		return 0, false
	}
	ps := self.clientStrategy.settings.ProxySettings
	if ps == nil {
		// native [direct] proxy — always index 0
		return 0, true
	}
	return ps.Index, true
}

func (self *PlatformTransport) runH1(initialTimeout time.Duration) {
	// connect and update route manager for this transport
	defer self.cancel()

	clientId, _ := self.auth.ClientId()

	if 0 < initialTimeout {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(initialTimeout):
		}
	}

	var authErrBackoff time.Duration

	for {
		// stand down while a strictly better mode is active
		func() {
			for {
				standDown, notify := self.standDown(TransportModeH1)
				if !standDown {
					return
				}
				select {
				case <-self.ctx.Done():
					return
				case <-notify:
				}
			}
		}()

		reconnect := NewReconnect(self.settings.ReconnectTimeout)
		connect := func() (*websocket.Conn, error) {
			header := http.Header{}
			if self.settings.V2H1Auth {
				header.Add("Authorization", fmt.Sprintf("Bearer %s", self.auth.ByJwt))
				header.Add("X-UR-AppVersion", self.auth.AppVersion)
				header.Add("X-UR-InstanceId", self.auth.InstanceId.String())
				header.Add("X-UR-TransportVersion", fmt.Sprintf("%d", TransportVersion))
			}

			ws, _, err := self.clientStrategy.WsDialContext(self.ctx, self.platformUrl, header)
			if err != nil {
				return nil, err
			}

			success := false
			defer func() {
				if !success {
					ws.Close()
				}
			}()

			if !self.settings.V2H1Auth {
				authBytes, err := EncodeFrame(&protocol.Auth{
					ByJwt:      self.auth.ByJwt,
					AppVersion: self.auth.AppVersion,
					InstanceId: self.auth.InstanceId.Bytes(),
				}, self.settings.ProtocolVersion)
				if err != nil {
					return nil, err
				}
				defer MessagePoolReturn(authBytes)

				ws.SetWriteDeadline(time.Now().Add(self.settings.AuthTimeout))
				if err := ws.WriteMessage(websocket.BinaryMessage, authBytes); err != nil {
					return nil, err
				}
				ws.SetReadDeadline(time.Now().Add(self.settings.AuthTimeout))
				if messageType, message, err := ws.ReadMessage(); err != nil {
					return nil, err
				} else {
					// verify the auth echo
					switch messageType {
					case websocket.BinaryMessage:
						if !bytes.Equal(authBytes, message) {
							return nil, fmt.Errorf("Auth response error: bad bytes.")
						}
					default:
						return nil, fmt.Errorf("Auth response error.")
					}
				}
			}

			success = true
			return ws, nil
		}

		if connectDelay := self.clientStrategy.NextConnectTime().Sub(time.Now()); 0 < connectDelay {
			select {
			case <-self.ctx.Done():
				return
			case <-time.After(connectDelay):
			}
		}

		var ws *websocket.Conn
		var err error
		if self.log.V(2).Enabled() {
			ws, err = TraceWithReturnError(fmt.Sprintf("[t]connect %s", clientId), connect)
		} else {
			ws, err = connect()
		}
		if err != nil {
			// a canceled dial is local teardown — this transport or its owner
			// shutting down mid-connect — not a backend signal, and not a
			// fault of the proxy being dialed. Without this carve-out, closing
			// a multi-client window cancels many transports at once and the
			// burst of canceled dials trips the degraded threshold with fresh
			// timestamps, so the NEXT session starts gated. The contract OOB
			// path makes the same carve-out on client.Done.
			if self.ctx.Err() == nil {
				noteBackendFailure()
				if idx, ok := self.proxyIndex(); ok {
					RecordProxyAuthFailure(idx, err)
				}
			}
			if ok, suppressed := shouldLogAuthErr(); ok {
				if suppressed > 0 {
					self.log.Infof("[t]auth error %s = %s (%d suppressed)\n", clientId, err, suppressed)
				} else {
					self.log.Infof("[t]auth error %s = %s\n", clientId, err)
				}
			}
			if authErrBackoff == 0 {
				authErrBackoff = self.settings.ReconnectTimeout
			} else {
				authErrBackoff = min(authErrBackoff*2, 60*time.Second)
			}
			select {
			case <-self.ctx.Done():
				return
			case <-time.After(authErrBackoff):
				continue
			case <-Pulse():
				authErrBackoff = 0
				self.clientStrategy.ResetHealth()
				continue
			}
		}
		authErrBackoff = 0
		noteBackendSuccess()

		c := func() {
			defer ws.Close()

			self.setModeAvailable(TransportModeH1, true)
			defer self.setModeAvailable(TransportModeH1, false)

			handleCtx, handleCancel := context.WithCancel(self.ctx)
			defer handleCancel()

			var readCounter atomic.Uint64
			var writeCounter atomic.Uint64

			send := make(chan []byte, self.settings.TransportBufferSize)
			receive := make(chan []byte, self.settings.TransportBufferSize)
			controlSend := make(chan []byte, self.settings.TransportBufferSize)

			drain := func(c chan []byte) {
				for {
					select {
					case message, ok := <-c:
						if !ok {
							return
						}
						MessagePoolReturn(message)
					default:
						return
					}
				}
			}

			var exportedSend chan []byte
			// note: this should be false in production
			//       it seems better to potentially leak messages than to
			//       have an extra inefficiency on the packet path
			if DebugCloseSend {
				// use zero buffer here so that the transport can stop accepting and not drop messages
				exportedSend = make(chan []byte)
				go HandleError(func() {
					defer func() {
						handleCancel()
						close(send)
						drain(send)
					}()
					for {
						select {
						case <-handleCtx.Done():
							return
						case message, ok := <-exportedSend:
							if !ok {
								return
							}
							select {
							case <-handleCtx.Done():
								MessagePoolReturn(message)
								return
							case send <- message:
							}
						}
					}
				}, func() {
					handleCancel()
					close(send)
					drain(send)
				})
			} else {
				exportedSend = send
			}

			// the platform can route any destination,
			// since every client has a platform transport
			var sendTransport Transport
			var receiveTransport Transport
			if self.settings.TransportGenerator != nil {
				sendTransport, receiveTransport = self.settings.TransportGenerator()
			} else {
				sendTransport = NewSendGatewayTransport()
				receiveTransport = NewReceiveGatewayTransport()
			}

			self.routeManager.UpdateTransport(sendTransport, []Route{exportedSend})
			self.routeManager.UpdateTransport(receiveTransport, []Route{receive})

			atomic.AddInt64(&activeProxyConnections, 1)
			if idx, ok := self.proxyIndex(); ok {
				markProxyUp(idx)
			}

			defer func() {
				atomic.AddInt64(&activeProxyConnections, -1)
				if idx, ok := self.proxyIndex(); ok {
					markProxyDown(idx)
					RecordProxyTransportDrop(idx, nil)
				}
				self.routeManager.RemoveTransport(sendTransport)
				self.routeManager.RemoveTransport(receiveTransport)
			}()

			go HandleError(func() {
				defer handleCancel()

				for {
					mode, notify := self.activeMode()
					if mode != TransportModeH1 {
						startReadCount := readCounter.Load()
						startWriteCount := writeCounter.Load()
						select {
						case <-handleCtx.Done():
							return
						case <-time.After(self.settings.InactiveDrainTimeout):
							// no activity after cool down, shut down this transport
							if readCounter.Load() == startReadCount && writeCounter.Load() == startWriteCount {
								handleCancel()
							}
						case <-notify:
						}
					} else {
						select {
						case <-handleCtx.Done():
							return
						case <-notify:
						}
					}
				}
			}, handleCancel)

			go HandleError(func() {
				defer handleCancel()

				speedTest := false

				write := func(message []byte) error {
					ws.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
					err := ws.WriteMessage(websocket.BinaryMessage, message)
					MessagePoolReturn(message)
					if err != nil {
						// note that for websocket a dealine timeout cannot be recovered
						if ok, suppressed := shouldLogWriteErr(); ok {
							if suppressed > 0 {
								self.log.Infof("[ts]%s-> error = %s (%d suppressed)\n", clientId, err, suppressed)
							} else {
								self.log.Infof("[ts]%s-> error = %s\n", clientId, err)
							}
						}
						return err
					}
					self.log.V(2).Infof("[ts]%s->\n", clientId)

					writeCounter.Add(1)
					return nil
				}

				for {
					if speedTest {
						select {
						case <-handleCtx.Done():
							return
						case <-WakeupAfter(self.settings.PingTimeout, self.settings.PingTimeout):
							ws.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
							if err := ws.WriteMessage(websocket.BinaryMessage, make([]byte, 0)); err != nil {
								// note that for websocket a dealine timeout cannot be recovered
								return
							}
						case message, ok := <-controlSend:
							if !ok {
								return
							}
							if len(message) == 5 {
								switch message[0] {
								case TransportControlSpeedStop:
									speedTest = false
								}
							}
							if write(message) != nil {
								return
							}
						}
					} else {
						select {
						case <-handleCtx.Done():
							return
						case message, ok := <-send:
							if !ok {
								return
							}
							// if !MessagePoolCheckShared(message) {
							// 	panic("[t]shared should be set")
							// }

							if len(message) <= 16 {
								self.log.Infof("[ts]send message must be >16 bytes (%s)\n", len(message))
								MessagePoolReturn(message)
							} else if write(message) != nil {
								return
							}
						case <-WakeupAfter(self.settings.PingTimeout, self.settings.PingTimeout):
							ws.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
							if err := ws.WriteMessage(websocket.BinaryMessage, make([]byte, 0)); err != nil {
								// note that for websocket a dealine timeout cannot be recovered
								return
							}
						case message, ok := <-controlSend:
							if !ok {
								return
							}
							if len(message) == 5 {
								switch message[0] {
								case TransportControlSpeedStart:
									speedTest = true
								}
							}
							if write(message) != nil {
								return
							}
						}
					}
				}
			}, handleCancel)

			go HandleError(func() {
				defer func() {
					handleCancel()
					close(receive)
					close(controlSend)

					drain(receive)
					drain(controlSend)
				}()

				speedTest := false

				for {
					select {
					case <-handleCtx.Done():
						return
					default:
					}

					ws.SetReadDeadline(time.Now().Add(self.settings.ReadTimeout))
					messageType, r, err := ws.NextReader()
					if err != nil {
						self.log.V(2).Infof("[tr]%s<- error = %s\n", clientId, err)
						return
					}

					switch messageType {
					case websocket.BinaryMessage:

						message, err := MessagePoolReadAll(r)
						if err != nil {
							self.log.V(2).Infof("[tr]%s<- error = %s\n", clientId, err)
							return
						}

						readCounter.Add(1)

						if len(message) <= 16 {
							if len(message) == 0 {
								// ping
								self.log.V(2).Infof("[tr]ping %s<-\n", clientId)
								MessagePoolReturn(message)
							} else if len(message) == 5 {
								switch message[0] {
								case TransportControlSpeedStart:
									speedTest = true
									// echo
									select {
									case <-self.ctx.Done():
										MessagePoolReturn(message)
									case controlSend <- message:
									}
								case TransportControlSpeedStop:
									speedTest = false
									// echo
									select {
									case <-self.ctx.Done():
										MessagePoolReturn(message)
									case controlSend <- message:
									}
								default:
									MessagePoolReturn(message)
								}
							} else if len(message) == 16 {
								// latency test echo
								select {
								case <-self.ctx.Done():
									MessagePoolReturn(message)
								case controlSend <- message:
								}
							} else {
								MessagePoolReturn(message)
							}
							continue
						}
						if speedTest {
							// speed test echo
							select {
							case <-self.ctx.Done():
								MessagePoolReturn(message)
							case controlSend <- message:
							}
							continue
						}

						select {
						case <-handleCtx.Done():
							MessagePoolReturn(message)
							return
						case receive <- message:
							self.log.V(2).Infof("[tr]%s<-\n", clientId)
						case <-time.After(self.settings.ReadTimeout):
							self.log.Infof("[tr]drop %s<-\n", clientId)
							MessagePoolReturn(message)
						}
					default:
						self.log.V(2).Infof("[tr]other=%s %s<-\n", messageType, clientId)
					}

					// messageType, message, err := ws.ReadMessage()
					// if err != nil {
					// 	self.log.Infof("[tr]%s<- error = %s\n", clientId, err)
					// 	return
					// }

				}
			}, func() {
				handleCancel()
				close(receive)
				close(controlSend)

				drain(receive)
				drain(controlSend)
			})

			select {
			case <-handleCtx.Done():
			}
		}

		reconnect = NewReconnect(self.settings.ReconnectTimeout)
		if self.log.V(2).Enabled() {
			Trace(fmt.Sprintf("[t]connect run %s", clientId), c)
		} else {
			c()
		}

		select {
		case <-self.ctx.Done():
			return
		case <-reconnect.After():
		}
	}
}

func (self *PlatformTransport) runH3(ptMode TransportMode, initialTimeout time.Duration, slowMultiple int) {
	// connect and update route manager for this transport
	defer self.cancel()

	if slowMultiple < 1 {
		panic(fmt.Errorf("Bad slow multiple: %d", slowMultiple))
	}

	clientId, _ := self.auth.ClientId()

	authBytes, err := EncodeFrame(&protocol.Auth{
		ByJwt:      self.auth.ByJwt,
		AppVersion: self.auth.AppVersion,
		InstanceId: self.auth.InstanceId.Bytes(),
	}, self.settings.ProtocolVersion)
	if err != nil {
		return
	}
	defer MessagePoolReturn(authBytes)

	if 0 < initialTimeout {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(initialTimeout):
		}
	}

	var authErrBackoff time.Duration

	for {
		// stand down while a strictly better mode is active
		func() {
			for {
				standDown, notify := self.standDown(ptMode)
				if !standDown {
					return
				}
				select {
				case <-self.ctx.Done():
					return
				case <-notify:
				}
			}
		}()

		reconnect := NewReconnect(self.settings.ReconnectTimeout)

		type ConnStream struct {
			conn   *quic.Conn
			stream *quic.Stream
		}

		connect := func() (*ConnStream, error) {
			// quicConfig := &quic.Config{
			// 	HandshakeIdleTimeout: self.settings.QuicConnectTimeout + self.settings.QuicHandshakeTimeout,
			// }

			success := false

			quicConfig := &quic.Config{
				HandshakeIdleTimeout:    time.Duration(slowMultiple) * (self.settings.QuicConnectTimeout + self.settings.QuicHandshakeTimeout),
				MaxIdleTimeout:          self.settings.PingTimeout * 4,
				KeepAlivePeriod:         0,
				Allow0RTT:               true,
				DisablePathMTUDiscovery: true,
				InitialPacketSize:       1400,
			}
			var tlsConfig *tls.Config
			if self.settings.QuicTlsConfig != nil {
				// copy
				tlsConfig = self.settings.QuicTlsConfig.Clone()
			} else {
				tlsConfig = &tls.Config{}
			}

			var packetConn net.PacketConn

			udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
			if err != nil {
				return nil, err
			}
			defer func() {
				if !success {
					udpConn.Close()
				}
			}()

			// udpAddr, err := net.ResolveUDPAddr("udp", addr)
			// if err != nil {
			// 	return nil, err
			// }

			serverName, err := connectHost(self.platformUrl)
			if err != nil {
				return nil, err
			}
			var udpAddr *net.UDPAddr
			switch ptMode {
			case TransportModeH3Dns:
				tld := self.settings.DnsTlds[mathrand.Intn(len(self.settings.DnsTlds))]
				udpAddr, err = net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", serverName, self.settings.DnsPort))
				ptSettings := DefaultPacketTranslationSettings()
				ptSettings.DnsTlds = [][]byte{tld}
				packetConn, err = NewPacketTranslation(self.ctx, PacketTranslationModeDns, udpConn, ptSettings)
				if err != nil {
					return nil, err
				}
			case TransportModeH3DnsPump:
				tld := self.settings.DnsTlds[mathrand.Intn(len(self.settings.DnsTlds))]
				pumpServerName, err := pumpHost(self.platformUrl, tld)
				if err != nil {
					return nil, err
				}
				udpAddr, err = net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", pumpServerName, self.settings.DnsPort))
				ptSettings := DefaultPacketTranslationSettings()
				ptSettings.DnsTlds = [][]byte{tld}
				packetConn, err = NewPacketTranslation(self.ctx, PacketTranslationModeDnsPump, udpConn, ptSettings)
				if err != nil {
					return nil, err
				}
			default:
				udpAddr, err = net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", serverName, self.settings.H3Port))
				if err != nil {
					return nil, err
				}
				packetConn = udpConn
			}

			defer func() {
				if !success {
					packetConn.Close()
				}
			}()

			self.log.Infof("[c]h3 connect to %v (%s)\n", udpAddr, serverName)

			tlsConfig.ServerName = serverName
			quicTransport := &quic.Transport{
				Conn: packetConn,
				// createdConn: true,
				// isSingleUse: true,
			}
			conn, err := quicTransport.DialEarly(self.ctx, udpAddr, tlsConfig, quicConfig)

			// conn, err := quic.Dial(self.ctx, packetConn, packetConn.ConnectedAddr(), self.settings.QuicTlsConfig, quicConfig)
			if err != nil {
				self.log.Infof("[c]h3 connect err = %s\n", err)
				return nil, err
			}
			defer func() {
				if !success {
					conn.CloseWithError(0, "")
				}
			}()

			stream, err := conn.OpenStreamSync(self.ctx)
			if err != nil {
				self.log.Infof("[c]h3 open stream err = %s\n", err)
				return nil, err
			}

			framer := NewFramer(self.settings.FramerSettings)

			stream.SetWriteDeadline(time.Now().Add(time.Duration(slowMultiple) * self.settings.AuthTimeout))
			if err := framer.Write(stream, authBytes); err != nil {
				return nil, err
			}
			stream.SetReadDeadline(time.Now().Add(time.Duration(slowMultiple) * self.settings.AuthTimeout))
			if message, err := framer.Read(stream); err != nil {
				return nil, err
			} else {
				// verify the auth echo
				if !bytes.Equal(authBytes, message) {
					return nil, fmt.Errorf("Auth response error: bad bytes.")
				}
			}

			success = true
			return &ConnStream{
				conn:   conn,
				stream: stream,
			}, nil
		}

		var connStream *ConnStream
		var err error
		if self.log.V(2).Enabled() {
			connStream, err = TraceWithReturnError(fmt.Sprintf("[t]connect %s", clientId), connect)
		} else {
			connStream, err = connect()
		}
		if err != nil {
			// a canceled dial is local teardown — this transport or its owner
			// shutting down mid-connect — not a backend signal, and not a
			// fault of the proxy being dialed. See runH1 for why the burst
			// from a closing multi-client window would otherwise gate the
			// next session.
			if self.ctx.Err() == nil {
				noteBackendFailure()
				if idx, ok := self.proxyIndex(); ok {
					RecordProxyAuthFailure(idx, err)
				}
			}
			if ok, suppressed := shouldLogAuthErr(); ok {
				if suppressed > 0 {
					self.log.Infof("[t]auth error %s = %s (%d suppressed)\n", clientId, err, suppressed)
				} else {
					self.log.Infof("[t]auth error %s = %s\n", clientId, err)
				}
			}
			if authErrBackoff == 0 {
				authErrBackoff = self.settings.ReconnectTimeout
			} else {
				authErrBackoff = min(authErrBackoff*2, 60*time.Second)
			}
			select {
			case <-self.ctx.Done():
				return
			case <-time.After(authErrBackoff):
				continue
			case <-Pulse():
				authErrBackoff = 0
				self.clientStrategy.ResetHealth()
				continue
			}
		}
		authErrBackoff = 0
		noteBackendSuccess()

		conn := connStream.conn
		stream := connStream.stream

		c := func() {
			defer conn.CloseWithError(0, "")

			self.setModeAvailable(ptMode, true)
			defer self.setModeAvailable(ptMode, false)

			handleCtx, handleCancel := context.WithCancel(self.ctx)
			defer handleCancel()

			framer := NewFramer(self.settings.FramerSettings)

			var readCounter atomic.Uint64
			var writeCounter atomic.Uint64

			send := make(chan []byte, self.settings.TransportBufferSize)
			receive := make(chan []byte, self.settings.TransportBufferSize)

			// the platform can route any destination,
			// since every client has a platform transport
			var sendTransport Transport
			var receiveTransport Transport
			if self.settings.TransportGenerator != nil {
				sendTransport, receiveTransport = self.settings.TransportGenerator()
			} else {
				sendTransport = NewSendGatewayTransport()
				receiveTransport = NewReceiveGatewayTransport()
			}

			self.routeManager.UpdateTransport(sendTransport, []Route{send})
			self.routeManager.UpdateTransport(receiveTransport, []Route{receive})

			atomic.AddInt64(&activeProxyConnections, 1)
			if idx, ok := self.proxyIndex(); ok {
				markProxyUp(idx)
			}

			defer func() {
				atomic.AddInt64(&activeProxyConnections, -1)
				if idx, ok := self.proxyIndex(); ok {
					markProxyDown(idx)
					RecordProxyTransportDrop(idx, nil)
				}
				self.routeManager.RemoveTransport(sendTransport)
				self.routeManager.RemoveTransport(receiveTransport)

				// note `send` is not closed. This channel is left open.
				// it used to be closed after a delay, but it is not needed to close it.
			}()

			go HandleError(func() {
				defer handleCancel()

				for {
					mode, notify := self.activeMode()
					if mode != ptMode {
						startReadCount := readCounter.Load()
						startWriteCount := writeCounter.Load()
						select {
						case <-handleCtx.Done():
							return
						case <-time.After(time.Duration(slowMultiple) * self.settings.InactiveDrainTimeout):
							// no activity after cool down, shut down this transport
							if readCounter.Load() == startReadCount && writeCounter.Load() == startWriteCount {
								handleCancel()
							}
						case <-notify:
						}
					} else {
						select {
						case <-handleCtx.Done():
							return
						case <-notify:
						}
					}
				}
			}, handleCancel)

			go HandleError(func() {
				defer handleCancel()

				for {
					select {
					case <-handleCtx.Done():
						return
					case message, ok := <-send:
						if !ok {
							return
						}
						// if !MessagePoolCheckShared(message) {
						// 	panic("[t]shared should be set")
						// }
						stream.SetWriteDeadline(time.Now().Add(time.Duration(slowMultiple) * self.settings.WriteTimeout))
						err := framer.Write(stream, message)
						MessagePoolReturn(message)
						if err != nil {
							// note that for websocket a dealine timeout cannot be recovered
							if ok, suppressed := shouldLogWriteErr(); ok {
								if suppressed > 0 {
									self.log.Infof("[ts]%s-> error = %s (%d suppressed)\n", clientId, err, suppressed)
								} else {
									self.log.Infof("[ts]%s-> error = %s\n", clientId, err)
								}
							}
							return
						}
						self.log.V(2).Infof("[ts]%s->\n", clientId)
					case <-WakeupAfter(self.settings.PingTimeout, self.settings.PingTimeout):
						stream.SetWriteDeadline(time.Now().Add(time.Duration(slowMultiple) * self.settings.WriteTimeout))
						if err := framer.Write(stream, make([]byte, 0)); err != nil {
							// note that for websocket a dealine timeout cannot be recovered
							return
						}
					}
				}
			}, handleCancel)

			go HandleError(func() {
				defer func() {
					handleCancel()
					close(receive)
				}()

				for {
					select {
					case <-handleCtx.Done():
						return
					default:
					}

					stream.SetReadDeadline(time.Now().Add(time.Duration(slowMultiple) * self.settings.ReadTimeout))
					message, err := framer.Read(stream)
					if err != nil {
						self.log.Infof("[tr]%s<- error = %s\n", clientId, err)
						return
					}

					if 0 == len(message) {
						// ping
						self.log.V(2).Infof("[tr]ping %s<-\n", clientId)
						MessagePoolReturn(message)
						continue
					}

					select {
					case <-handleCtx.Done():
						MessagePoolReturn(message)
						return
					case receive <- message:
						self.log.V(2).Infof("[tr]%s<-\n", clientId)
					case <-time.After(time.Duration(slowMultiple) * self.settings.ReadTimeout):
						self.log.Infof("[tr]drop %s<-\n", clientId)
						MessagePoolReturn(message)
					}
				}
			}, func() {
				handleCancel()
				close(receive)
			})

			select {
			case <-handleCtx.Done():
			}
		}
		reconnect = NewReconnect(self.settings.ReconnectTimeout)
		if self.log.V(2).Enabled() {
			Trace(fmt.Sprintf("[t]connect run %s", clientId), c)
		} else {
			c()
		}

		select {
		case <-self.ctx.Done():
			return
		case <-reconnect.After():
		}
	}
}

func (self *PlatformTransport) Close() {
	self.cancel()
}

func connectHost(platformUrl string) (string, error) {
	u, err := url.Parse(platformUrl)
	if err != nil {
		return "", err
	}
	return u.Hostname(), nil
}

// this host should resolve in dns to the root zone ips for the tld
func pumpHost(platformUrl string, tld []byte) (string, error) {
	u, err := url.Parse(platformUrl)
	if err != nil {
		return "", err
	}
	host := u.Hostname()
	if net.ParseIP(host) != nil {
		return host, nil
	}
	// tld replace . with -
	// zone-<tld>.<base>
	baseHost := strings.SplitN(host, ".", 2)[1]
	pumpHost := fmt.Sprintf("zone-%s.%s", strings.ReplaceAll(string(tld), ".", "-"), baseHost)
	return pumpHost, nil
}
