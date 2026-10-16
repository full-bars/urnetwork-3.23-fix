package connect

import (
	"crypto/tls"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// Operator accounting for platform streams and their P2P transports. Every
// counter moves when a stream is requested, created or ends, or when a
// transport starts, stops or fails to connect. None is touched per packet.
//
// The questions these answer without a log window or a goroutine profile:
// how many streams of each kind are live, how long they live (a sequence that
// outlives its transport shows up as a long lifetime), why they ended, how
// often the platform asks for a stream that already exists or that was just
// reaped, and how often transports churn.

type streamKind int

const (
	// streamKindSingle: the provider is one end of the stream.
	streamKindSingle streamKind = iota
	// streamKindTwoEnded: the provider relays between a source and a destination.
	streamKindTwoEnded
	streamKindCount
)

var streamKindNames = [streamKindCount]string{"single", "two_ended"}

func streamKindOf(sourceId *Id, destinationId *Id) streamKind {
	if sourceId != nil && destinationId != nil {
		return streamKindTwoEnded
	}
	return streamKindSingle
}

// why a stream sequence ended
const (
	streamEndTransportEnded = iota // its transport gave up on its own
	streamEndCancelled             // close, reset, replacement, parent context
	streamEndReasonCount
)

var streamEndReasonNames = [streamEndReasonCount]string{"transport_ended", "cancelled"}

// lifetime histogram upper bounds in seconds; one more bucket holds the rest
var streamLifetimeBoundsSeconds = [...]float64{1, 10, 60, 300, 1800, 3600, 21600, 86400}

const streamLifetimeBucketCount = len(streamLifetimeBoundsSeconds) + 1

// what the platform's StreamOpen resolved to in the buffer
const (
	streamOpenCreated  = iota // a new sequence was started
	streamOpenExisting        // the sequence already existed and was reused
	streamOpenReplaced        // an older sequence for the same stream id was cancelled
	streamOpenResultCount
)

var streamOpenResultNames = [streamOpenResultCount]string{"created", "existing", "replaced"}

type streamMetricsState struct {
	opened [streamKindCount]atomic.Int64
	closed [streamKindCount][streamEndReasonCount]atomic.Int64

	lifetimeBuckets [streamKindCount][streamLifetimeBucketCount]atomic.Int64
	lifetimeCount   [streamKindCount]atomic.Int64
	lifetimeSumNs   [streamKindCount]atomic.Int64

	openRequests [streamOpenResultCount]atomic.Int64
	closeFrames  atomic.Int64
	resetFrames  atomic.Int64
	openFrames   atomic.Int64

	p2pStarted      atomic.Int64
	p2pStopped      atomic.Int64
	p2pConnFailures atomic.Int64

	// platform and API TLS handshakes by outcome, from the dial paths that carry
	// a session cache (resumed handshakes skip certificate verification)
	// identities whose dial backoff entered the long tier (see
	// PlatformTransportSettings.ReconnectLongAfter), and how many sit in it now
	dialBackoffLongEntered atomic.Int64
	dialBackoffLongActive  atomic.Int64

	// TLS handshakes by dial path, target host class and result
	tls [tlsPathCount][tlsHostCount][tlsResultCount]atomic.Int64
}

// The dial path a TLS handshake ran on (the strategy's dialers), the class of
// host it dialed (a fixed set, so the series stay bounded) and its result.
type tlsPath int

const (
	tlsPathNormal tlsPath = iota
	tlsPathNormalProxy
	tlsPathResilientFragmentReorder
	tlsPathResilientFragment
	tlsPathResilientReorder
	tlsPathResilientSegment
	tlsPathCount
)

var tlsPathNames = [tlsPathCount]string{"normal", "normal_proxy", "resilient_fragment_reorder", "resilient_fragment", "resilient_reorder", "resilient_segment"}

const (
	tlsHostApi = iota
	tlsHostConnect
	tlsHostOther
	tlsHostCount
)

var tlsHostNames = [tlsHostCount]string{"api", "connect", "other"}

const (
	tlsResultFull = iota
	tlsResultResumed
	tlsResultFailed
	tlsResultCount
)

var tlsResultNames = [tlsResultCount]string{"full", "resumed", "failed"}

func tlsHostClass(host string) int {
	host = strings.ToLower(host)
	switch {
	case strings.HasPrefix(host, "api."):
		return tlsHostApi
	case strings.HasPrefix(host, "connect."):
		return tlsHostConnect
	default:
		return tlsHostOther
	}
}

func resilientTlsPath(fragment bool, reorder bool, segment bool) tlsPath {
	switch {
	case segment:
		return tlsPathResilientSegment
	case fragment && reorder:
		return tlsPathResilientFragmentReorder
	case fragment:
		return tlsPathResilientFragment
	default:
		return tlsPathResilientReorder
	}
}

// noteTlsHandshake counts one finished client handshake. Called once per dial.
// host is the dialed host name (without the port).
func noteTlsHandshake(path tlsPath, host string, state tls.ConnectionState, err error) {
	result := tlsResultFull
	switch {
	case err != nil:
		result = tlsResultFailed
	case state.DidResume:
		result = tlsResultResumed
	}
	streamMetrics.tls[path][tlsHostClass(host)][result].Add(1)
}

// tlsHandshakeTotal sums one result over every path and host.
func (self *streamMetricsState) tlsHandshakeTotal(result int) int64 {
	var total int64
	for p := 0; p < int(tlsPathCount); p += 1 {
		for h := 0; h < tlsHostCount; h += 1 {
			total += self.tls[p][h][result].Load()
		}
	}
	return total
}

var streamMetrics streamMetricsState

func (self *streamMetricsState) observeLifetime(kind streamKind, d time.Duration) {
	seconds := d.Seconds()
	i := 0
	for i < len(streamLifetimeBoundsSeconds) && seconds > streamLifetimeBoundsSeconds[i] {
		i += 1
	}
	self.lifetimeBuckets[kind][i].Add(1)
	self.lifetimeCount[kind].Add(1)
	self.lifetimeSumNs[kind].Add(int64(d))
}

// liveByKind is opened minus closed. The two are read separately, so a scrape
// can be off by one transiently; it never drifts.
func (self *streamMetricsState) liveByKind(kind streamKind) int64 {
	live := self.opened[kind].Load()
	for r := 0; r < streamEndReasonCount; r += 1 {
		live -= self.closed[kind][r].Load()
	}
	return live
}

func (self *streamMetricsState) writePrometheus(b *strings.Builder) {
	fmt.Fprintf(b, "# HELP urnet_stream_sequences_by_kind Live stream sequences by kind (single: provider is one end; two_ended: provider relays).\n")
	fmt.Fprintf(b, "# TYPE urnet_stream_sequences_by_kind gauge\n")
	for k := streamKind(0); k < streamKindCount; k += 1 {
		fmt.Fprintf(b, "urnet_stream_sequences_by_kind{kind=%q} %d\n", streamKindNames[k], self.liveByKind(k))
	}

	fmt.Fprintf(b, "# HELP urnet_stream_sequences_opened_total Stream sequences started, by kind.\n")
	fmt.Fprintf(b, "# TYPE urnet_stream_sequences_opened_total counter\n")
	for k := streamKind(0); k < streamKindCount; k += 1 {
		fmt.Fprintf(b, "urnet_stream_sequences_opened_total{kind=%q} %d\n", streamKindNames[k], self.opened[k].Load())
	}

	fmt.Fprintf(b, "# HELP urnet_stream_sequences_closed_total Stream sequences ended, by kind and reason (transport_ended: its transport gave up; cancelled: close, reset, replacement or shutdown).\n")
	fmt.Fprintf(b, "# TYPE urnet_stream_sequences_closed_total counter\n")
	for k := streamKind(0); k < streamKindCount; k += 1 {
		for r := 0; r < streamEndReasonCount; r += 1 {
			fmt.Fprintf(b, "urnet_stream_sequences_closed_total{kind=%q,reason=%q} %d\n", streamKindNames[k], streamEndReasonNames[r], self.closed[k][r].Load())
		}
	}

	fmt.Fprintf(b, "# HELP urnet_stream_sequence_lifetime_seconds How long a stream sequence lived, observed when it ended.\n")
	fmt.Fprintf(b, "# TYPE urnet_stream_sequence_lifetime_seconds histogram\n")
	for k := streamKind(0); k < streamKindCount; k += 1 {
		var cumulative int64
		for i := 0; i < streamLifetimeBucketCount; i += 1 {
			cumulative += self.lifetimeBuckets[k][i].Load()
			if i < len(streamLifetimeBoundsSeconds) {
				fmt.Fprintf(b, "urnet_stream_sequence_lifetime_seconds_bucket{kind=%q,le=\"%g\"} %d\n", streamKindNames[k], streamLifetimeBoundsSeconds[i], cumulative)
			} else {
				fmt.Fprintf(b, "urnet_stream_sequence_lifetime_seconds_bucket{kind=%q,le=\"+Inf\"} %d\n", streamKindNames[k], cumulative)
			}
		}
		fmt.Fprintf(b, "urnet_stream_sequence_lifetime_seconds_sum{kind=%q} %g\n", streamKindNames[k], float64(self.lifetimeSumNs[k].Load())/1e9)
		fmt.Fprintf(b, "urnet_stream_sequence_lifetime_seconds_count{kind=%q} %d\n", streamKindNames[k], self.lifetimeCount[k].Load())
	}

	fmt.Fprintf(b, "# HELP urnet_stream_open_requests_total Platform stream open requests by what the buffer did with them.\n")
	fmt.Fprintf(b, "# TYPE urnet_stream_open_requests_total counter\n")
	for r := 0; r < streamOpenResultCount; r += 1 {
		fmt.Fprintf(b, "urnet_stream_open_requests_total{result=%q} %d\n", streamOpenResultNames[r], self.openRequests[r].Load())
	}

	fmt.Fprintf(b, "# HELP urnet_stream_control_frames_total Stream control frames received from the platform.\n")
	fmt.Fprintf(b, "# TYPE urnet_stream_control_frames_total counter\n")
	fmt.Fprintf(b, "urnet_stream_control_frames_total{type=\"open\"} %d\n", self.openFrames.Load())
	fmt.Fprintf(b, "urnet_stream_control_frames_total{type=\"close\"} %d\n", self.closeFrames.Load())
	fmt.Fprintf(b, "urnet_stream_control_frames_total{type=\"reset\"} %d\n", self.resetFrames.Load())

	fmt.Fprintf(b, "# HELP urnet_p2p_transports_started_total P2P transports started.\n")
	fmt.Fprintf(b, "# TYPE urnet_p2p_transports_started_total counter\n")
	fmt.Fprintf(b, "urnet_p2p_transports_started_total %d\n", self.p2pStarted.Load())
	fmt.Fprintf(b, "# HELP urnet_p2p_transports_stopped_total P2P transports stopped.\n")
	fmt.Fprintf(b, "# TYPE urnet_p2p_transports_stopped_total counter\n")
	fmt.Fprintf(b, "urnet_p2p_transports_stopped_total %d\n", self.p2pStopped.Load())
	fmt.Fprintf(b, "# HELP urnet_p2p_connect_failures_total P2P connection attempts that returned an error (a connection that came up and then failed its readiness exchange is not counted).\n")
	fmt.Fprintf(b, "# TYPE urnet_p2p_connect_failures_total counter\n")
	fmt.Fprintf(b, "urnet_p2p_connect_failures_total %d\n", self.p2pConnFailures.Load())

	fmt.Fprintf(b, "# HELP urnet_tls_handshakes_total Client TLS handshakes on the platform and API dial paths, by dial path, target host class and result (resumed ones skip certificate verification).\n")
	fmt.Fprintf(b, "# TYPE urnet_tls_handshakes_total counter\n")
	for p := 0; p < int(tlsPathCount); p += 1 {
		for h := 0; h < tlsHostCount; h += 1 {
			for r := 0; r < tlsResultCount; r += 1 {
				fmt.Fprintf(b, "urnet_tls_handshakes_total{path=%q,host=%q,result=%q} %d\n", tlsPathNames[p], tlsHostNames[h], tlsResultNames[r], self.tls[p][h][r].Load())
			}
		}
	}
	fmt.Fprintf(b, "# HELP urnet_dial_backoff_long_tier_active Identities currently waiting in the long dial backoff tier because their own path keeps failing.\n")
	fmt.Fprintf(b, "# TYPE urnet_dial_backoff_long_tier_active gauge\n")
	fmt.Fprintf(b, "urnet_dial_backoff_long_tier_active %d\n", self.dialBackoffLongActive.Load())
	fmt.Fprintf(b, "# HELP urnet_dial_backoff_long_tier_entered_total Times an identity's dial backoff entered the long tier.\n")
	fmt.Fprintf(b, "# TYPE urnet_dial_backoff_long_tier_entered_total counter\n")
	fmt.Fprintf(b, "urnet_dial_backoff_long_tier_entered_total %d\n", self.dialBackoffLongEntered.Load())

	writeTcpAckDirectMetrics(b)
}
