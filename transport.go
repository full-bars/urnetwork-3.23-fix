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
	TransportControlClose      TransportControl = 3
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
	InstanceId    Id
	AppVersion    string
	ProvideIntent bool
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

	// FlapStableAfter is how long a platform session must live before the next
	// reconnect is treated as ordinary. Sessions that die sooner extend the wait
	// before the next connect (see flapGuard). Zero uses the default, negative
	// disables the guard.
	FlapStableAfter      time.Duration
	TransportGenerator   func() (sendTransport Transport, receiveTransport Transport)
	TransportBufferSize  int
	InactiveDrainTimeout time.Duration
	// it smoothes out the h3 transition to not start/stop h1 if h3 connects in this time
	ModeInitialDelay time.Duration

	// PlatformTransportBudget, H1BudgetByteCount, H3BudgetByteCount, and
	// ModePreferences are the settings-only surface this generation adopts for
	// PlatformTransportAutoEligibility (platform_transport_status.go) and the
	// per-owner settings constructor (DefaultPlatformTransportSettingsWithMemoryTarget).
	// The pinned v2026 engine also admits the transport against these; in this
	// generation the transport swap that wires the reservations is a later
	// slice, so nothing below the status view reads them yet.
	PlatformTransportBudget *PlatformTransportBudget
	// Non-positive carrier byte counts resolve to the memory-scaled defaults,
	// like the pinned engine (H1MemoryReservationByteCount / defaultH3BudgetByteCount).
	H1BudgetByteCount ByteCount
	H3BudgetByteCount ByteCount
	// ModePreferences configures which Auto modes the settings view considers
	// enabled, and their priorities. Lower values are preferred. Nil selects
	// DefaultTransportModePreferences.
	ModePreferences map[TransportMode]int

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

	// EnableH3 makes this identity eligible for an H3 (QUIC) transport beside
	// H1 in Auto mode. It is OFF by default and is meant for an identity that
	// reaches the platform from the host's own address (the direct identity):
	// runH3 opens a host UDP socket, so a proxied identity must never enable
	// it without a socket that goes through its proxy. Eligible is not
	// running: H3 only dials while the runtime gate is on (SetH3Enabled), so
	// an eligible identity with the gate off opens no socket. While H3 runs in
	// Auto mode it is an auxiliary transport: H1 stays the authoritative
	// health signal, so an H3 connect failure (UDP filtered, no route) is
	// "mode unavailable" and is NOT a backend failure or a proxy auth failure.
	// See h3Auxiliary.
	EnableH3 bool

	// sendRouteObserverForTest, when set, is called with the send route of each
	// H3 connection once it is registered, so a test can push messages into it.
	sendRouteObserverForTest func(send chan []byte)

	ClientLimitBackoff     *ClientLimitBackoff
	clientLimitHoldForTest func()
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
		// Port note (v2026 engine -> 3.23 generation): the Auto policy and the
		// carrier budget defaults are the pinned engine's. Only the status view
		// reads them so far; the transport swap wires admission later. Without
		// them a default settings value would report every Auto mode eligible
		// for lack of a budget rather than because one fits.
		ModePreferences:         DefaultTransportModePreferences(),
		PlatformTransportBudget: DefaultPlatformTransportBudget(),
		H1BudgetByteCount:       MemoryScaledByteCount(kib(512), kib(256)),
		H3BudgetByteCount:       defaultH3BudgetByteCount(),
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
	// auth is written at runtime by the client-JWT renewal watcher (SetAuth)
	// and read by connection dial paths; atomic so readers never observe a
	// torn/stale pointer without paying for the state lock on hot paths.
	auth atomic.Pointer[ClientAuth]

	settings *PlatformTransportSettings

	// modePreferences is the Auto policy this transport was constructed with,
	// used by the status view (platform_transport_status.go) exactly as the
	// pinned engine uses it. Nil (the default for a zero-value transport) means
	// no DNS-family mode is configured.
	modePreferences map[TransportMode]int

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

	clientLimitBackoff *ClientLimitBackoff
	// clientLimitCloseCount counts closes the platform made for the client limit,
	// so a session that ended that way is not taken for the backend failing
	clientLimitCloseCount atomic.Int64
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
		settings:             settings,
		availableModeMonitor: NewMonitor(),
		availableModes:       map[TransportMode]bool{},
		targetMode:           targetMode,
		mode:                 NewMonitorValue[TransportMode](TransportModeNone),
	}
	transport.clientLimitBackoff = settings.ClientLimitBackoff
	if transport.clientLimitBackoff == nil {
		transport.clientLimitBackoff = NewClientLimitBackoff()
	}
	transport.auth.Store(auth)
	go HandleError(transport.run, cancel)
	return transport
}

// the auth is used on future connections
func (self *PlatformTransport) SetAuth(auth *ClientAuth) {
	self.auth.Store(auth)
}

// getAuth returns the current auth credential. It is safe for concurrent use
// with SetAuth (the client-JWT renewal watcher rotates the bearer at runtime).
func (self *PlatformTransport) getAuth() *ClientAuth {
	return self.auth.Load()
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
		if self.settings.EnableH3 {
			go HandleError(func() {
				self.runH3(TransportModeH3, 0, 1)
			}, self.cancel)
		}
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

func (self *PlatformTransport) runH1(initialTimeout time.Duration) {
	// connect and update route manager for this transport
	defer self.cancel()

	clientId, _ := self.getAuth().ClientId()

	if 0 < initialTimeout {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(initialTimeout):
		}
	}

	var authErrBackoff time.Duration
	flaps := newFlapGuard(self.settings.FlapStableAfter, self.settings.ReconnectTimeout)
	startGovernorLoop()

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

		if !self.waitClientLimitHold() {
			return
		}
		auth := self.getAuth()
		clientLimitResetGeneration := self.clientLimitResetGeneration()

		reconnect := NewReconnect(self.settings.ReconnectTimeout)
		// what this one attempt costs: dials opened and bytes moved
		attemptStats := &attemptStats{}

		connect := func() (*websocket.Conn, error) {
			header := http.Header{}
			if self.settings.V2H1Auth {
				header.Add("Authorization", fmt.Sprintf("Bearer %s", auth.ByJwt))
				header.Add("X-UR-AppVersion", auth.AppVersion)
				header.Add("X-UR-InstanceId", auth.InstanceId.String())
				header.Add("X-UR-TransportVersion", fmt.Sprintf("%d", TransportVersion))
				applyProvideIntentHeader(header, *auth)
			}

			ws, _, err := self.clientStrategy.WsDialContext(withAttemptStats(self.ctx, attemptStats), self.platformUrl, header)
			if err != nil {
				return nil, err
			}

			// G-H2 fix: set a per-websocket read limit immediately after the
			// dial succeeds, BEFORE any read (including the auth echo). Without
			// it, Gorilla WebSocket has no positive limit until SetReadLimit is
			// called, so the authentication echo could cause an oversized
			// allocation from a misbehaving peer. 1MB is well above normal
			// transfer frame sizes (IP packets up to ~64KB).
			ws.SetReadLimit(1024 * 1024)

			success := false
			defer func() {
				if !success {
					ws.Close()
				}
			}()

			if !self.settings.V2H1Auth {
				authBytes, err := EncodeFrame(&protocol.Auth{
					ByJwt:         auth.ByJwt,
					AppVersion:    auth.AppVersion,
					InstanceId:    auth.InstanceId.Bytes(),
					ProvideIntent: auth.ProvideIntent,
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

		// while the backend is failing, a disconnected proxy waits its turn
		// under the shared attempt budget; the direct path never waits
		{
			proxyIndex, proxied := self.proxyIndex()
			if !admitConnect(self.ctx, !proxied || proxyIndex == 0, proxyIndex) {
				return
			}
		}

		var ws *websocket.Conn
		var err error
		if self.log.V(2).Enabled() {
			ws, err = TraceWithReturnError(fmt.Sprintf("[t]connect %s", clientId), connect)
		} else {
			ws, err = connect()
		}
		connectGov.recordAttempt(classifyConnectError(err, attemptStats), attemptStats)
		if err != nil {
			if isClientLimitCloseError(err) {
				self.noteClientLimitClose(TransportModeH1, auth.ProvideIntent, clientLimitResetGeneration)
				continue
			}
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
				// every backing-off transport hears the pulse at the same instant;
				// spread the retries so the hour does not begin with a herd
				if !waitPulseJitter(self.ctx) {
					return
				}
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

			go HandleError(func() {
				self.runClientLimitWatch(handleCtx, func() {
					handleCancel()
					ws.Close()
				})
			}, handleCancel)

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
					if self.drainsWhenInactive(mode, TransportModeH1) {
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
						recordTransportCloseCause(self.proxySourceLabel(), "h1", "write", err)
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
							transportPingFrames.tx.Add(1)
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
								self.log.Infof("[ts]send message must be >16 bytes (%d)\n", len(message))
								MessagePoolReturn(message)
							} else {
								byteCount := len(message)
								if write(message) != nil {
									return
								}
								h1ModeStats.addTx(byteCount)
								if self.settings.EnableH3 {
									h1DirectStats.addTx(byteCount)
								}
							}
						case <-WakeupAfter(self.settings.PingTimeout, self.settings.PingTimeout):
							ws.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
							if err := ws.WriteMessage(websocket.BinaryMessage, make([]byte, 0)); err != nil {
								// note that for websocket a dealine timeout cannot be recovered
								return
							}
							transportPingFrames.tx.Add(1)
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
						recordTransportCloseCause(self.proxySourceLabel(), "h1", "read", err)
						self.log.V(2).Infof("[tr]%s<- error = %s\n", clientId, err)
						if isClientLimitCloseError(err) {
							self.noteClientLimitClose(TransportModeH1, auth.ProvideIntent, clientLimitResetGeneration)
						}
						return
					}

					switch messageType {
					case websocket.BinaryMessage:

						message, err := MessagePoolReadAll(r)
						if err != nil {
							recordTransportCloseCause(self.proxySourceLabel(), "h1", "read", err)
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
									recordSpeedTestStart()
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
								case TransportControlClose:
									reason, _ := transportCloseReason(message)
									MessagePoolReturn(message)
									if reason == TransportCloseReasonClientLimitExceeded {
										self.noteClientLimitClose(TransportModeH1, auth.ProvideIntent, clientLimitResetGeneration)
									}
									return
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
							// Always echo in full: a test that never comes back is retried by
							// the server, many times over (see transport_speed_echo.go).
							recordSpeedEchoBytes(len(message))
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
							h1ModeStats.addRx(len(message))
							if self.settings.EnableH3 {
								h1DirectStats.addRx(len(message))
							}
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
		sessionStart := time.Now()
		limitClosesBefore := self.clientLimitCloseCount.Load()
		if self.log.V(2).Enabled() {
			Trace(fmt.Sprintf("[t]connect run %s", clientId), c)
		} else {
			c()
		}

		// a session that dies young is not a success: the connect that began it
		// already reset the failed-connect backoff and the shared failure
		// streak, so without this wait a slow backend that accepts the websocket
		// and then drops it makes every proxy reconnect again immediately
		lived := time.Since(sessionStart)
		if flapWait := sessionEndWait(self.ctx, flaps, lived, self.clientLimitCloseCount.Load() != limitClosesBefore, clientId.String(), self.log, true, self.proxySourceLabel()); 0 < flapWait {
			select {
			case <-self.ctx.Done():
				return
			case <-time.After(flapWait):
			}
		} else {
			select {
			case <-self.ctx.Done():
				return
			case <-reconnect.After():
			}
		}
	}
}

func (self *PlatformTransport) runH3(ptMode TransportMode, initialTimeout time.Duration, slowMultiple int) {
	// connect and update route manager for this transport
	defer self.cancel()

	if slowMultiple < 1 {
		panic(fmt.Errorf("Bad slow multiple: %d", slowMultiple))
	}

	clientId, _ := self.getAuth().ClientId()

	// H3 and the DNS packet-translation modes all run here: count each under the
	// mode it was started for
	counters := h3CountersFor(ptMode)

	if 0 < initialTimeout {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(initialTimeout):
		}
	}

	var authErrBackoff time.Duration
	h3UnavailableLogged := false
	// the same flap guard the H1 loop has: a platform that accepts the connection and
	// then drops it must not make this transport redial the moment it ends
	flaps := newFlapGuard(self.settings.FlapStableAfter, self.settings.ReconnectTimeout)

	for {
		// an eligible identity with the runtime gate off opens no socket and
		// dials nothing: wait here until it is switched on
		if !self.waitH3Enabled() {
			return
		}

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

		if !self.waitClientLimitHold() {
			return
		}
		auth := self.getAuth()
		clientLimitResetGeneration := self.clientLimitResetGeneration()

		reconnect := NewReconnect(self.settings.ReconnectTimeout)

		connect := func() (*h3ConnStream, error) {
			counters.attempts.Add(1)
			// DATAGRAM is only offered by the gated auxiliary H3 on the plain H3
			// path, never by the sole H3 mode or the DNS translation modes
			offerDatagrams := ptMode == TransportModeH3 && self.h3Gated() && H3DatagramsEnabled()
			if offerDatagrams {
				h3DatagramOffered.Add(1)
			}
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
				EnableDatagrams:         offerDatagrams,
			}
			applyH3MemoryBounds(quicConfig)
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
				packetConn = self.countH3Socket(udpConn)
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

			// Encode the auth frame per dial attempt, not once for the whole
			// reconnect loop: SetAuth rotates the JWT in place, and a
			// transport that presents the startup token forever (the old
			// behavior) would keep the stale credential after renewal.
			// Mirrors runH1, which rebuilds the frame inside its connect
			// closure. The echo verification below compares against the same
			// per-attempt bytes, so the snapshot is consistent.
			authMessage := &protocol.Auth{
				ByJwt:         auth.ByJwt,
				AppVersion:    auth.AppVersion,
				InstanceId:    auth.InstanceId.Bytes(),
				ProvideIntent: auth.ProvideIntent,
			}
			SetH3DatagramAuthOffer(authMessage, offerDatagrams)
			authBytes, err := EncodeFrame(authMessage, self.settings.ProtocolVersion)
			if err != nil {
				return nil, err
			}
			defer MessagePoolReturn(authBytes)

			stream.SetWriteDeadline(time.Now().Add(time.Duration(slowMultiple) * self.settings.AuthTimeout))
			if err := framer.Write(stream, authBytes); err != nil {
				return nil, err
			}
			stream.SetReadDeadline(time.Now().Add(time.Duration(slowMultiple) * self.settings.AuthTimeout))
			useDatagrams := false
			if message, err := framer.Read(stream); err != nil {
				return nil, err
			} else {
				defer MessagePoolReturn(message)
				if !offerDatagrams {
					// verify the auth echo, byte for byte as before
					if !bytes.Equal(authBytes, message) {
						return nil, fmt.Errorf("Auth response error: bad bytes.")
					}
				} else {
					// A server that accepts DATAGRAM answers with a re-encoded
					// Auth that carries the accepted version, so the bytes differ
					// from the request. A server that does not echoes the request
					// unchanged, which validates as "not accepted": the plain
					// stream, the intended fallback.
					responseMessage, err := DecodeFrame(message)
					if err != nil {
						return nil, err
					}
					authResponse, ok := responseMessage.(*protocol.Auth)
					if !ok {
						return nil, fmt.Errorf("Auth response error: got %T.", responseMessage)
					}
					connectionState := conn.ConnectionState()
					useDatagrams, err = ValidateH3DatagramAuthResponse(
						authMessage,
						authResponse,
						true,
						connectionState.SupportsDatagrams.Local,
						connectionState.SupportsDatagrams.Remote,
					)
					if err != nil {
						return nil, err
					}
					if useDatagrams {
						h3DatagramAccepted.Add(1)
					}
				}
			}

			success = true
			return &h3ConnStream{
				conn:             conn,
				stream:           stream,
				quicTransport:    quicTransport,
				packetConn:       packetConn,
				offeredDatagrams: offerDatagrams,
				useDatagrams:     useDatagrams,
			}, nil
		}

		var connStream *h3ConnStream
		var err error
		if self.log.V(2).Enabled() {
			connStream, err = TraceWithReturnError(fmt.Sprintf("[t]connect %s", clientId), connect)
		} else {
			connStream, err = connect()
		}
		// the governor reads the platform's health from every transport's connects; an
		// auxiliary H3 beside H1 is never a health signal. (H3 is UDP, so it never
		// goes through a proxy and the attempt budget would exempt it as direct.)
		if !self.h3Gated() {
			connectGov.recordAttempt(classifyConnectError(err, nil), nil)
		}
		if err != nil {
			if isClientLimitCloseError(err) {
				self.noteClientLimitClose(ptMode, auth.ProvideIntent, clientLimitResetGeneration)
				continue
			}
			// a canceled dial is local teardown — this transport or its owner
			// shutting down mid-connect — not a backend signal, and not a
			// fault of the proxy being dialed. See runH1 for why the burst
			// from a closing multi-client window would otherwise gate the
			// next session.
			if self.ctx.Err() == nil {
				counters.connectFailures.Add(1)
				// an auxiliary H3 (opt-in, beside H1) is not a health signal
				if !self.h3Gated() {
					noteBackendFailure()
					if idx, ok := self.proxyIndex(); ok {
						RecordProxyAuthFailure(idx, err)
					}
				}
			}
			if self.h3Gated() {
				// an auxiliary H3 that cannot connect is "unavailable", not an
				// auth error: say so once, then only at verbose levels
				if !h3UnavailableLogged {
					h3UnavailableLogged = true
					self.log.Infof("[t]h3 unavailable, staying on h1 (retrying quietly): %s\n", err)
				} else if self.log.V(2).Enabled() {
					self.log.Infof("[t]h3 unavailable: %s\n", err)
				}
			} else if ok, suppressed := shouldLogAuthErr(); ok {
				if suppressed > 0 {
					self.log.Infof("[t]auth error %s = %s (%d suppressed)\n", clientId, err, suppressed)
				} else {
					self.log.Infof("[t]auth error %s = %s\n", clientId, err)
				}
			}
			if self.h3Gated() {
				authErrBackoff = nextH3Backoff(authErrBackoff, self.settings.ReconnectTimeout, h3AuxiliaryMaxBackoff)
			} else if authErrBackoff == 0 {
				authErrBackoff = self.settings.ReconnectTimeout
			} else {
				authErrBackoff = min(authErrBackoff*2, 60*time.Second)
			}
			var pulse <-chan struct{}
			var gateChanged <-chan struct{}
			wait := authErrBackoff
			if self.h3Gated() {
				// jittered, and cut short if the gate changes: a switch-off
				// must park the transport now, not after the sleep
				wait = jitteredH3Wait(authErrBackoff)
				var on bool
				on, gateChanged = h3GateWatch()
				if !on {
					// switched off while the dial was in flight: park now, there is
					// no later change to wake a sleep that started after it
					authErrBackoff = 0
					continue
				}
			} else {
				pulse = Pulse()
			}
			select {
			case <-self.ctx.Done():
				return
			case <-time.After(wait):
				continue
			case <-gateChanged:
				// an operator toggle starts over: a transport woken by it must not
				// carry the multi-minute backoff into its next failure
				authErrBackoff = 0
				continue
			case <-pulse:
				if !waitPulseJitter(self.ctx) {
					return
				}
				authErrBackoff = 0
				self.clientStrategy.ResetHealth()
				continue
			}
		}
		authErrBackoff = 0
		if self.h3Gated() && !H3Enabled() {
			// switched off while the dial was in flight
			connStream.close()
			continue
		}
		self.noteAuthSuccess()
		counters.connects.Add(1)

		conn := connStream.conn
		stream := connStream.stream

		c := func() {
			// one close can be seen by the stream reader, the datagram reader and the
			// cleanup below; each sighting would re-roll the hold jitter, so only the
			// first starts the hold
			var clientLimitNoted atomic.Bool
			noteClientLimitOnce := func() {
				if clientLimitNoted.CompareAndSwap(false, true) {
					self.noteClientLimitClose(ptMode, auth.ProvideIntent, clientLimitResetGeneration)
				}
			}
			// release the socket and the quic transport after the connection,
			// otherwise every reconnect leaks one udp fd, its read goroutine
			// and the transport state
			defer func() {
				if isClientLimitCloseError(context.Cause(conn.Context())) {
					noteClientLimitOnce()
				}
				connStream.close()
			}()

			self.setModeAvailable(ptMode, true)
			defer self.setModeAvailable(ptMode, false)

			handleCtx, handleCancel := context.WithCancel(self.ctx)
			defer handleCancel()

			go HandleError(func() {
				self.runClientLimitWatch(handleCtx, func() {
					handleCancel()
					conn.CloseWithError(0, "client limit")
				})
			}, handleCancel)

			framer := NewFramer(self.settings.FramerSettings)

			var datagramReassembler *H3DatagramReassembler
			if connStream.useDatagrams {
				var datagramErr error
				datagramSettings, datagramBudget := h3DatagramLimits()
				datagramReassembler, datagramErr = NewH3DatagramReassembler(
					datagramSettings,
					datagramBudget,
					h3DatagramStats,
				)
				if datagramErr != nil {
					self.log.Infof("[t]h3 datagram receiver init error = %s\n", datagramErr)
					return
				}
				defer datagramReassembler.Close()
				self.log.Infof("[t]h3 datagram accepted by the server, receiving datagrams on %s\n", clientId)
			}

			// send side of a negotiated DATAGRAM connection
			var datagramFragmenter *H3DatagramFragmenter
			var maxDatagramByteCount atomic.Int64
			datagramGuard := newH3DatagramSendGuard()
			// when the stream and the datagram lane last received something, which
			// is how the guard tells a dead datagram lane from a dead connection
			var lastStreamReceiveNano atomic.Int64
			lastStreamReceiveNano.Store(time.Now().UnixNano())
			if connStream.useDatagrams {
				datagramSettings, _ := h3DatagramLimits()
				var fragmenterErr error
				datagramFragmenter, fragmenterErr = NewH3DatagramFragmenter(datagramSettings, h3DatagramStats)
				if fragmenterErr != nil {
					self.log.Infof("[t]h3 datagram sender init error = %s\n", fragmenterErr)
					return
				}
				maxDatagramByteCount.Store(int64(initialH3DatagramPathByteCount(
					datagramSettings.TargetDatagramByteCount,
					conn.SendDatagram,
				)))
			}

			var readCounter atomic.Uint64
			var readPayloadCounter atomic.Uint64
			var writeCounter atomic.Uint64
			var writePayloadCounter atomic.Uint64
			var writePayloadBytes atomic.Uint64
			var readPayloadBytes atomic.Uint64
			// set when this connection is closed because a gate (h3 or h3_datagram)
			// changed, which is an operator action and not a drop
			var gateClosed atomic.Bool

			counters.up.Add(1)
			// per-connection frame and byte counts, so the log says what this transport carried
			connectTime := time.Now()
			defer func() {
				counters.up.Add(-1)
				// a connection that ends while its transport is still wanted is a
				// drop; closing it because the gate was turned off is not
				if self.ctx.Err() == nil && !gateClosed.Load() && (!self.h3Gated() || H3Enabled()) {
					counters.drops.Add(1)
				}
				self.log.Infof("[t]h3 closed %s after %s (frames out=%d in=%d, payload bytes out=%d in=%d)\n",
					clientId, time.Since(connectTime).Round(time.Second), writeCounter.Load(), readCounter.Load(),
					writePayloadBytes.Load(), readPayloadBytes.Load())
			}()

			send := make(chan []byte, self.settings.TransportBufferSize)
			receive := make(chan []byte, self.settings.TransportBufferSize)

			// The stream writer reads `streamInput`. Without DATAGRAM that is `send`
			// itself. With it, a dispatcher sits between them and decides the lane
			// per message, and the stream lane goes through a bounded queue: a
			// datagram send that blocks on quic-go's own queue must not hold up the
			// frames that go on the stream, and the queue is bounded by count and by
			// the bytes it retains so splitting the lanes cannot double what a
			// connection holds.
			streamInput := (<-chan []byte)(send)
			var streamSend chan []byte
			var streamSendBudget *H3HybridStreamSendBudget
			if connStream.useDatagrams {
				streamQueueMessageCount := min(
					H3HybridStreamQueueMessageCount,
					max(1, self.settings.TransportBufferSize),
				)
				streamSend = make(chan []byte, streamQueueMessageCount)
				streamSendBudget = NewH3HybridStreamSendBudget(
					streamQueueMessageCount,
					H3HybridStreamQueueByteCount,
					h3DatagramStats,
				)
				streamInput = streamSend
			}
			releaseStreamMessage := func(message []byte) {
				if streamSendBudget != nil {
					streamSendBudget.Release(H3HybridStreamRetainedByteCount(message))
				}
				MessagePoolReturn(message)
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

			self.routeManager.UpdateTransport(sendTransport, []Route{send})
			self.routeManager.UpdateTransport(receiveTransport, []Route{receive})
			if observer := self.settings.sendRouteObserverForTest; observer != nil {
				observer(send)
			}

			atomic.AddInt64(&activeProxyConnections, 1)
			// an auxiliary H3 is not the identity health signal: its connects and
			// drops must not mark it up, down or dropped while H1 says if it is healthy
			if idx, ok := self.proxyIndex(); ok && !self.h3Gated() {
				markProxyUp(idx)
			}

			defer func() {
				atomic.AddInt64(&activeProxyConnections, -1)
				if idx, ok := self.proxyIndex(); ok && !self.h3Gated() {
					markProxyDown(idx)
					RecordProxyTransportDrop(idx, nil)
				}
				self.routeManager.RemoveTransport(sendTransport)
				self.routeManager.RemoveTransport(receiveTransport)

				// note `send` is not closed. This channel is left open.
				// it used to be closed after a delay, but it is not needed to close it.
			}()

			if self.h3Gated() {
				go HandleError(func() {
					defer handleCancel()

					for {
						on, notify := h3GateWatch()
						if !on {
							self.log.Infof("[t]h3 switched off, closing %s\n", clientId)
							gateClosed.Store(true)
							return
						}
						// the DATAGRAM offer is made at dial time, so a change
						// closes the connection and lets it reconnect with it
						datagramsOn, datagramNotify := h3DatagramGate.watch()
						if datagramsOn != connStream.offeredDatagrams {
							self.log.Infof("[t]h3 datagram offer changed (now %v), reconnecting %s\n", datagramsOn, clientId)
							gateClosed.Store(true)
							return
						}
						select {
						case <-handleCtx.Done():
							return
						case <-notify:
						case <-datagramNotify:
						}
					}
				}, handleCancel)
			}

			go HandleError(func() {
				defer handleCancel()

				for {
					mode, notify := self.activeMode()
					if self.drainsWhenInactive(mode, ptMode) {
						startReadCount := readPayloadCounter.Load()
						startWriteCount := writePayloadCounter.Load()
						select {
						case <-handleCtx.Done():
							return
						case <-time.After(time.Duration(slowMultiple) * self.settings.InactiveDrainTimeout):
							// no activity after cool down, shut down this transport
							if readPayloadCounter.Load() == startReadCount && writePayloadCounter.Load() == startWriteCount {
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
				// Cancel FIRST, then drain: the dispatcher closes streamSend only
				// when it sees handleCtx done, so a drain registered ahead of the
				// cancel waits forever on a channel nothing will close, and the
				// connection never exits.
				defer func() {
					handleCancel()
					if streamSend != nil {
						for message := range streamSend {
							releaseStreamMessage(message)
						}
					}
				}()

				for {
					select {
					case <-handleCtx.Done():
						return
					case message, ok := <-streamInput:
						if !ok {
							return
						}
						// if !MessagePoolCheckShared(message) {
						// 	panic("[t]shared should be set")
						// }
						stream.SetWriteDeadline(time.Now().Add(time.Duration(slowMultiple) * self.settings.WriteTimeout))
						messageByteCount := len(message)
						err := framer.Write(stream, message)
						releaseStreamMessage(message)
						if err == nil && connStream.useDatagrams {
							h3DatagramStats.RecordStreamSent(messageByteCount)
						}
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
						writeCounter.Add(1)
						if 0 < messageByteCount {
							writePayloadCounter.Add(1)
							writePayloadBytes.Add(uint64(messageByteCount))
							counters.addTx(messageByteCount)
						}
						self.log.V(2).Infof("[ts]%s->\n", clientId)
					case <-WakeupAfter(self.settings.PingTimeout, self.settings.PingTimeout):
						stream.SetWriteDeadline(time.Now().Add(time.Duration(slowMultiple) * self.settings.WriteTimeout))
						if err := framer.Write(stream, make([]byte, 0)); err != nil {
							// note that for websocket a dealine timeout cannot be recovered
							return
						}
						writeCounter.Add(1)
					}
				}
			}, handleCancel)

			if connStream.useDatagrams {
				go HandleError(func() {
					defer handleCancel()
					defer close(streamSend)

					datagramSettings, _ := h3DatagramLimits()
					// Datagrams leave on their own goroutine: quic-go's SendDatagram
					// blocks while its flight is full and takes no context, so calling
					// it from here would let a peer that stops draining datagrams hold
					// up the stream frames queued behind it.
					datagramSender := newH3DatagramSender(handleCtx, conn.SendDatagram, func(err error) {
						// The lane sends asynchronously, so the errors quic-go returned
						// synchronously now arrive here. A usable too-large report lowers
						// the live path limit (never raises it) so the next frame takes
						// the stream instead of being discarded, and is not counted;
						// reportH3DatagramSendError counts everything else -- every
						// other error and any too-large report that could not lower the
						// limit -- so the send-error metric keeps meaning something.
						reportH3DatagramSendError(h3DatagramStats, &maxDatagramByteCount, err)
					})
					logSendError := func(err error) {
						if ok, suppressed := shouldLogWriteErr(); ok {
							if suppressed > 0 {
								self.log.Infof("[ts]%s-> error = %s (%d suppressed)\n", clientId, err, suppressed)
							} else {
								self.log.Infof("[ts]%s-> error = %s\n", clientId, err)
							}
						}
					}
					// offerStream moves one message to the bounded stream queue,
					// compacting an oversized backing buffer first and returning the
					// message to the pool on every failure path
					offerStream := func(message []byte) bool {
						retainedByteCount := H3HybridStreamRetainedByteCount(message)
						if streamSendBudget.MaxByteCount() < retainedByteCount &&
							len(message) <= streamSendBudget.MaxByteCount()-MessagePoolMetaByteCount {
							compactMessage := MessagePoolCopy(message)
							MessagePoolReturn(message)
							message = compactMessage
							retainedByteCount = H3HybridStreamRetainedByteCount(message)
						}
						if !streamSendBudget.Acquire(handleCtx, retainedByteCount) {
							MessagePoolReturn(message)
							if handleCtx.Err() == nil {
								logSendError(fmt.Errorf(
									"H3 hybrid stream message retained bytes %d exceed queue limit %d",
									retainedByteCount, streamSendBudget.MaxByteCount()))
							}
							return false
						}
						select {
						case <-handleCtx.Done():
							streamSendBudget.Release(retainedByteCount)
							MessagePoolReturn(message)
							return false
						case streamSend <- message:
							return true
						}
					}

					// Datagrams are only used while H1 is up. H1 is the authoritative
					// path and Transfer recovers anything a lossy lane drops by
					// resending, so with H1 up a dead datagram lane costs a resend.
					// With H1 down H3 is the only path and a dead lane would stall
					// every small frame, so the stream carries everything.
					var h1Up atomic.Bool
					refreshH1Up := func() {
						modes, _ := self.modesAvailable()
						h1Up.Store(modes[TransportModeH1])
					}
					refreshH1Up()
					refresh := time.NewTicker(time.Second)
					defer refresh.Stop()

					for {
						select {
						case <-handleCtx.Done():
							return
						case <-refresh.C:
							refreshH1Up()
						case message, ok := <-send:
							if !ok {
								return
							}
							useDatagram := H3DatagramSendEnabled() && h1Up.Load() &&
								datagramSettings.UseDatagramForPath(len(message), int(maxDatagramByteCount.Load()))
							if useDatagram {
								now := time.Now()
								streamAlive := now.Sub(time.Unix(0, lastStreamReceiveNano.Load())) < datagramGuard.window
								allowed, trippedNow := datagramGuard.allow(now, streamAlive)
								if trippedNow {
									h3DatagramBlackholes.Add(1)
									self.log.Infof("[t]h3 datagram send switched off for this connection: %d datagrams sent and none received for %s while the stream is alive, sending on the stream again (%s)\n",
										datagramGuard.sinceReceive.Load(), datagramGuard.window, clientId)
								}
								useDatagram = allowed
							}
							if useDatagram {
								messageByteCount := len(message)
								currentMax := int(maxDatagramByteCount.Load())
								useStream, nextMax, err := datagramFragmenter.SendHybrid(handleCtx, message, currentMax,
									func(_ context.Context, datagram []byte) error {
										// The lane takes the datagram or reports the flight
										// full; either way the dispatcher never waits on
										// quic-go's blocking send, and a full lane falls back
										// to the reliable stream.
										if !datagramSender.trySend(datagram) {
											return errQuicDatagramFlightFull
										}
										return nil
									})
								if nextMax != currentMax {
									maxDatagramByteCount.Store(int64(nextMax))
								}
								if err != nil {
									// A datagram-path failure is not a stream failure: put the
									// message on the reliable lane rather than tearing the whole
									// connection down. Only a stream write error owns that.
									// offerStream owns the message on every path, including its
									// failures, so it must not be returned here as well.
									logSendError(err)
									if !offerStream(message) {
										return
									}
									continue
								}
								if !useStream {
									datagramGuard.noteSent(time.Now())
									MessagePoolReturn(message)
									writeCounter.Add(1)
									writePayloadCounter.Add(1)
									writePayloadBytes.Add(uint64(messageByteCount))
									counters.addTx(messageByteCount)
									self.log.V(2).Infof("[ts]%s->datagram\n", clientId)
									continue
								}
							}
							if !offerStream(message) {
								return
							}
						}
					}
				}, handleCancel)
			}

			// `receive` has one closer, after EVERY reader has finished: a second
			// reader (DATAGRAM) sending on a channel the stream reader already
			// closed would panic the provider.
			//
			// Every reader's slot is reserved HERE, before the closer starts
			// waiting. The datagram reader is spawned after the stream reader, so
			// an Add made at its spawn site can race a Wait that already observed
			// zero — a WaitGroup misuse, and then a send on the closed channel.
			var receiveReaders sync.WaitGroup
			readerCount := 1
			if connStream.useDatagrams {
				readerCount++
			}
			receiveReaders.Add(readerCount)
			go func() {
				receiveReaders.Wait()
				close(receive)
			}()

			go HandleError(func() {
				defer func() {
					handleCancel()
					receiveReaders.Done()
				}()

				if connStream.useDatagrams {
					// DATAGRAM activity is invisible to a stream read deadline, so
					// a deadline here would tear down a connection whose stream is
					// legitimately idle. QUIC's own idle timeout still detects a
					// dead peer, and closing the connection unblocks the read.
					stream.SetReadDeadline(time.Time{})
				}

				for {
					select {
					case <-handleCtx.Done():
						return
					default:
					}

					if !connStream.useDatagrams {
						stream.SetReadDeadline(time.Now().Add(time.Duration(slowMultiple) * self.settings.ReadTimeout))
					}
					message, err := framer.Read(stream)
					if err != nil {
						recordTransportCloseCause(self.proxySourceLabel(), "h3", "read", err)
						self.log.Infof("[tr]%s<- error = %s\n", clientId, err)
						if isClientLimitCloseError(err) {
							noteClientLimitOnce()
						}
						return
					}
					readCounter.Add(1)
					lastStreamReceiveNano.Store(time.Now().UnixNano())

					if 0 == len(message) {
						// ping
						transportPingFrames.rx.Add(1)
						self.log.V(2).Infof("[tr]ping %s<-\n", clientId)
						MessagePoolReturn(message)
						continue
					}

					// count payload reads only: pings are keepalive, not use
					readPayloadCounter.Add(1)

					select {
					case <-handleCtx.Done():
						MessagePoolReturn(message)
						return
					case receive <- message:
						readPayloadBytes.Add(uint64(len(message)))
						counters.addRx(len(message))
						self.log.V(2).Infof("[tr]%s<-\n", clientId)
					case <-time.After(time.Duration(slowMultiple) * self.settings.ReadTimeout):
						self.log.Infof("[tr]drop %s<-\n", clientId)
						MessagePoolReturn(message)
					}
				}
			}, handleCancel)

			if connStream.useDatagrams {
				// Expiry is otherwise driven only by arriving datagrams, so an
				// incomplete message on a quiet but open connection would hold
				// its allocations until teardown. A timer releases them.
				go HandleError(func() {
					ticker := time.NewTicker(h3DatagramExpireInterval)
					defer ticker.Stop()
					for {
						select {
						case <-handleCtx.Done():
							return
						case <-ticker.C:
							datagramReassembler.Expire(time.Now())
						}
					}
				})
				go HandleError(func() {
					defer func() {
						handleCancel()
						receiveReaders.Done()
					}()

					for {
						datagram, err := conn.ReceiveDatagram(handleCtx)
						if err != nil {
							// the context ended or the connection closed
							if isClientLimitCloseError(err) {
								noteClientLimitOnce()
							}
							return
						}
						message := datagramReassembler.Accept(datagram, time.Now())
						if message == nil {
							continue
						}
						datagramGuard.noteReceived()
						readCounter.Add(1)
						select {
						case receive <- message:
							// counted only once the route accepted it: a dropped
							// datagram must not show up as payload received
							readPayloadCounter.Add(1)
							readPayloadBytes.Add(uint64(len(message)))
							counters.addRx(len(message))
							self.log.V(2).Infof("[tr]%s<-datagram\n", clientId)
						default:
							// the route is full: drop now instead of holding the
							// pump. The lane is unreliable and Transfer resends.
							h3DatagramDrops.Add(1)
							MessagePoolReturn(message)
						}
					}
				}, handleCancel)
			}

			select {
			case <-handleCtx.Done():
			}
		}
		reconnect = NewReconnect(self.settings.ReconnectTimeout)
		sessionStart := time.Now()
		limitClosesBefore := self.clientLimitCloseCount.Load()
		if self.log.V(2).Enabled() {
			Trace(fmt.Sprintf("[t]connect run %s", clientId), c)
		} else {
			c()
		}

		lived := time.Since(sessionStart)
		if flapWait := sessionEndWait(self.ctx, flaps, lived, self.clientLimitCloseCount.Load() != limitClosesBefore, clientId.String(), self.log, !self.h3Gated(), self.proxySourceLabel()); 0 < flapWait {
			select {
			case <-self.ctx.Done():
				return
			case <-time.After(flapWait):
			}
		} else {
			select {
			case <-self.ctx.Done():
				return
			case <-reconnect.After():
			}
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
