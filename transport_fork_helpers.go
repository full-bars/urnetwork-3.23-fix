package connect

import (
	"context"
	mathrand "math/rand"
	"net"
	"sync/atomic"
	"time"

	quic "github.com/quic-go/quic-go"
)

// Fork-only helpers that used to live in transport.go: per-proxy identity
// lookup, the H3 gate and auxiliary-mode rules and backoff, and the session-end
// wait. They moved verbatim so transport.go can be replaced by the upstream file
// with this logic kept in one place.

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

// admitH3Dial gates the start of an H3 carrier dial under the shared attempt
// budget, the way the H1 loop gates its own: a proxied identity waits its turn
// while the backend is failing, the direct path never waits, and a family-pinned
// transport paces itself and neither waits on nor feeds the budget. It returns
// false only when ctx ends first. QUIC left out of the budget is what let every
// proxy of a node set to h3 all redial a silent edge on its own timer.
func (self *PlatformTransport) admitH3Dial(ctx context.Context) bool {
	if self.pinned() {
		return true
	}
	proxyIndex, proxied := self.proxyIndex()
	return admitConnectQuiet(ctx, !proxied || proxyIndex == 0)
}

// h3Gated reports whether this transport's H3 is an opt-in auxiliary that the
// runtime gate (SetH3Enabled) switches: Auto mode on an eligible identity.
func (self *PlatformTransport) h3Gated() bool {
	return self.targetMode == TransportModeAuto && self.settings.EnableH3
}

// h3GatedFor reports whether the carrier running mode is the gated auxiliary.
// The gate governs only the plain H3 carrier: the dns carriers beside it are
// fallbacks in their own right and run whatever the gate says.
func (self *PlatformTransport) h3GatedFor(mode TransportMode) bool {
	return mode == TransportModeH3 && self.h3Gated()
}

// h3GroupGated reports whether every mode of an H3 carrier group is gated, in
// which case the group holds no budget reservation until the gate is on.
func (self *PlatformTransport) h3GroupGated(modes []TransportMode) bool {
	for _, mode := range modes {
		if !self.h3GatedFor(mode) {
			return false
		}
	}
	return 0 < len(modes)
}

// h3Auxiliary reports whether this transport runs H3 only as an opt-in
// auxiliary beside H1: eligible (Auto mode with EnableH3) and switched on. With
// the gate off H3 does not run at all, so H1 behaves exactly as it does on a
// build without H3. The explicit H3 target mode is the sole transport and keeps
// the full failure accounting.
func (self *PlatformTransport) h3Auxiliary() bool {
	return self.h3Gated() && H3Enabled()
}

// noteAuthSuccess clears the shared backend failure state after a successful
// auth. An auxiliary H3 authenticating must not clear failures H1 recorded: H1
// is the health signal, so only it may reset the state.
func (self *PlatformTransport) noteAuthSuccess() {
	if self.h3Gated() {
		return
	}
	noteBackendSuccess()
}

// drainsWhenInactive reports whether a transport running ptMode should drain
// after InactiveDrainTimeout of no traffic because activeMode is another mode.
// Equally preferred modes tie and only one is elected active, so without an
// exemption the other idles out and reconnects in a cycle, and when H3 wins the
// election it is H1, the health signal, that gets torn down. With an auxiliary
// H3 both transports stay connected and both carry traffic.
func (self *PlatformTransport) drainsWhenInactive(activeMode TransportMode, ptMode TransportMode) bool {
	return activeMode != ptMode && !self.h3Auxiliary()
}

// h3AuxiliaryMaxBackoff is how long an auxiliary H3 waits between attempts once
// it keeps failing. A network that filters UDP stays filtered, and each attempt
// costs a handshake timeout, so a failing H3 backs off far beyond the 60 second
// ceiling the sole transport uses.
const h3AuxiliaryMaxBackoff = 10 * time.Minute

// h3BackoffFloor is the shortest wait an auxiliary H3 takes between failed
// attempts, so an unset reconnect timeout cannot make it a tight dial loop.
const h3BackoffFloor = time.Second

// h3BackoffJitter is the share of the wait that is spread randomly either way.
const h3BackoffJitter = 0.2

// nextH3Backoff doubles the wait between failed attempts up to ceiling.
func nextH3Backoff(current time.Duration, base time.Duration, ceiling time.Duration) time.Duration {
	base = min(max(base, h3BackoffFloor), ceiling)
	if current < base {
		return base
	}
	return min(current*2, ceiling)
}

// jitteredH3Wait spreads d by +-h3BackoffJitter. Auxiliary H3 dials fail together
// when a network filters UDP or after a fleet-wide start, and plain doubling
// keeps them retrying in lockstep waves; the spread desynchronizes them.
func jitteredH3Wait(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d + time.Duration((mathrand.Float64()*2-1)*h3BackoffJitter*float64(d))
}

// h3ConnStream is an established H3 connection with the transport and socket it
// runs on. quic-go only closes a transport and its socket after the connection
// for a single use transport, so the owner must close them when the connection ends.
type h3ConnStream struct {
	conn          *quic.Conn
	stream        *quic.Stream
	quicTransport *quic.Transport
	packetConn    net.PacketConn
	// offeredDatagrams is whether this connection offered QUIC DATAGRAM, and
	// useDatagrams whether the server accepted. A connection that offered and
	// was not accepted runs on the plain stream.
	offeredDatagrams bool
	useDatagrams     bool
}

// close ends the connection, then the transport, then the socket
func (self *h3ConnStream) close() {
	self.conn.CloseWithError(0, "")
	self.quicTransport.Close()
	self.packetConn.Close()
}

// sessionEndWait applies the end-of-session rules shared by the H1 and H3 loops
// and returns the extra wait before the next connect, 0 for none.
//
// A session that dies young is evidence about the platform, but only if it was not
// cut short by our own teardown (a reload, the reaper, shutdown) and was not the
// platform enforcing the client limit. countsAsHealth is false for a transport its
// owner disabled (the group standing the standby down is our own teardown, not
// evidence about the platform) and for an auxiliary
// transport beside H1, which is never a health signal; it is still held back so it
// cannot redial in a tight loop.
func sessionEndWait(ctx context.Context, flaps *flapGuard, lived time.Duration, clientLimitClosed bool, clientId string, log Logger, countsAsHealth bool, proxySource string) time.Duration {
	if countsAsHealth {
		// only the main transport: an auxiliary H3 beside H1 would double count
		recordSessionEnd(ctx, lived, clientLimitClosed, proxySource)
	}
	if countsAsHealth && flaps.isYoung(lived) && ctx.Err() == nil && !clientLimitClosed {
		if flaps.everStable {
			connectGov.recordProvenEarlyDeath()
		} else {
			connectGov.recordEarlyDeath()
		}
	}
	flapWait := flaps.sessionEnded(lived)
	if 0 < flapWait && ctx.Err() == nil {
		if ok, suppressed := flapErrThrottle.Allow(time.Now()); ok {
			log.Infof("[t]flap %s session lived %s, holding reconnect %s (%d suppressed)\n", clientId, lived.Round(time.Millisecond), flapWait.Round(time.Millisecond), suppressed)
		}
	}
	return flapWait
}

// forkModePreferences is the Auto mode policy of a transport. A caller that sets
// ModePreferences explicitly gets exactly that (normalized). Otherwise the fork
// policy applies: H1 always, plus H3 on an identity that opted in with EnableH3,
// the two equally preferred so the election is sticky among equals and H3 stays
// an auxiliary beside H1 (see h3Auxiliary). The dns carriers are never started by
// default; upstream's default policy enables them.
func forkModePreferences(settings *PlatformTransportSettings) map[TransportMode]int {
	if settings.ModePreferences != nil {
		return normalizeTransportModePreferences(settings.ModePreferences)
	}
	preferences := map[TransportMode]int{TransportModeH1: 1}
	if settings.EnableH3 {
		preferences[TransportModeH3] = 1
	}
	return preferences
}

// h3DatagramOffer reports whether a dial in ptMode offers QUIC DATAGRAM. The
// explicit EnableH3Datagrams setting always wins. Otherwise only the gated
// auxiliary H3 on the plain H3 path offers it, and only while the operator
// datagram gate is on. Like upstream, the setting and both gates default on;
// an operator turns them off with SetH3DatagramsEnabled.
func (self *PlatformTransport) h3DatagramOffer(ptMode TransportMode) bool {
	if self.settings.EnableH3Datagrams {
		return true
	}
	return ptMode == TransportModeH3 && self.h3Gated() && H3DatagramsEnabled()
}

// h3UseDatagramForPath is the per-message lane decision. The gated auxiliary H3
// honors the operator send gate: with it off, a connection that negotiated
// DATAGRAM is receive-only and the stream carries everything.
func (self *PlatformTransport) h3UseDatagramForPath(messageByteCount int, maxDatagramByteCount int) bool {
	if self.h3Gated() && !H3DatagramSendEnabled() {
		return false
	}
	return self.h3DatagramSettings.UseDatagramForPath(messageByteCount, maxDatagramByteCount)
}

// isModeAvailable reports whether the transport has a live connection in mode.
func (self *PlatformTransport) isModeAvailable(mode TransportMode) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.availableModes[mode]
}

// h1MaxMessageSeen is the largest complete message any H1 transport has read,
// exported as urnet_h1_max_message_bytes so the read limit rests on evidence.
var h1MaxMessageSeen atomic.Int64

func noteH1MessageSize(size int) {
	for {
		seen := h1MaxMessageSeen.Load()
		if int64(size) <= seen || h1MaxMessageSeen.CompareAndSwap(seen, int64(size)) {
			return
		}
	}
}

// H1MaxMessageSeen returns the largest complete message read on any H1 transport.
func H1MaxMessageSeen() int64 { return h1MaxMessageSeen.Load() }

// noteFramedWriteError passes a framed batch write's result through, recording its
// error in the close-cause metric and the session-ended text as the websocket
// write path does.
func noteFramedWriteError(transport *PlatformTransport, endCause *familyEndCause, open bool, err error) (bool, error) {
	if err != nil {
		recordTransportCloseCause(transport.proxySourceLabel(), "h1", "write", err)
		endCause.note(err)
	}
	return open, err
}
