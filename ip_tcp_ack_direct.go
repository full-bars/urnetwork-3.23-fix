package connect

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// The pure-ACK direct path and the monotonic window edge share one kill
// switch: with it off, pure acks always take the ordered queue and the run
// loop applies acks with the historical overwrite rule (the exact behavior
// before the change). URNETWORK_TCP_ACK_DIRECT=off|0|false disables it at the
// library level (read once, cached); a provider may override at startup via
// SetTcpAckDirectEnabled from its tcp_ack_direct control key. Default: on
// (the operator flips it after the canary; see the release notes).
const (
	tcpAckDirectUnset = 0
	tcpAckDirectOn    = 1
	tcpAckDirectOff   = 2
)

var (
	tcpAckDirectOverride atomic.Int32
	tcpAckDirectEnvOnce  sync.Once
	tcpAckDirectEnv      atomic.Int32
)

func tcpAckDirectEnvEnabled() bool {
	tcpAckDirectEnvOnce.Do(func() {
		switch strings.ToLower(strings.TrimSpace(os.Getenv("URNETWORK_TCP_ACK_DIRECT"))) {
		case "off", "0", "false", "no", "disabled":
			tcpAckDirectEnv.Store(tcpAckDirectOff)
		default:
			tcpAckDirectEnv.Store(tcpAckDirectOn)
		}
	})
	return tcpAckDirectEnv.Load() == tcpAckDirectOn
}

// tcpAckDirectEnabled consults the provider override first, then the cached
// environment default (on unless URNETWORK_TCP_ACK_DIRECT disables it).
func tcpAckDirectEnabled() bool {
	switch tcpAckDirectOverride.Load() {
	case tcpAckDirectOn:
		return true
	case tcpAckDirectOff:
		return false
	default:
		return tcpAckDirectEnvEnabled()
	}
}

// SetTcpAckDirectEnabled applies a provider startup decision (its control
// key), overriding the environment default for this process.
func SetTcpAckDirectEnabled(enable bool) {
	if enable {
		tcpAckDirectOverride.Store(tcpAckDirectOn)
	} else {
		tcpAckDirectOverride.Store(tcpAckDirectOff)
	}
}

// Counters for the pure ack path, exposed on /metrics: the direct and queued
// totals for pure acks, and the stale (older-edge or backward-ack) updates the
// monotonic model ignored. The canary reads these to show the path is hit and
// that stale acks actually occur.
var (
	tcpAckDirectAppliedCount atomic.Int64
	tcpAckQueuedCount        atomic.Int64
	tcpAckStaleIgnoredCount  atomic.Int64
	// the production self-check (see tcpAckSelfCheckWithLock): invariant
	// violations and shadow disagreements must stay zero; the trip flag flips
	// the process to the old queued path once, and the tripped gauge reports
	// it on /metrics
	tcpAckInvariantViolations atomic.Int64
	tcpAckShadowDisagreements atomic.Int64
	tcpAckShadowSampleSeen    atomic.Int64
	tcpAckTripped             atomic.Int32
)

// tcpAckShadowSampleRate samples one in this many shared applies for the
// shadow comparison: cheap enough to stay on in production.
const tcpAckShadowSampleRate = 256

func tcpAckShadowSampled() bool {
	return tcpAckShadowSampleSeen.Add(1)%tcpAckShadowSampleRate == 0
}

// tripTcpAckDirect flips the process to the old queued path on an invariant
// violation, once per process, with a single log line.
func tripTcpAckDirect(logger Logger, reason string) {
	// compare-and-swap instead of sync.Once so the mechanism stays testable
	if !tcpAckTripped.CompareAndSwap(0, 1) {
		return
	}
	SetTcpAckDirectEnabled(false)
	if logger != nil {
		logger.V(1).Infof("[tcp] pure ack direct path tripped (%s); acks return to the ordered queue for this process\n", reason)
	}
}

func noteTcpAckDirectApplied() { tcpAckDirectAppliedCount.Add(1) }
func noteTcpAckQueued()        { tcpAckQueuedCount.Add(1) }
func noteTcpAckStaleIgnored()  { tcpAckStaleIgnoredCount.Add(1) }

// writeTcpAckDirectMetrics renders the pure ack counters into the metrics
// exposition (called by the stream metrics render).
func writeTcpAckDirectMetrics(b *strings.Builder) {
	b.WriteString("# HELP urnet_tcp_ack_direct_applied_total Pure acks applied directly, without crossing the sequence queue.\n")
	b.WriteString("# TYPE urnet_tcp_ack_direct_applied_total counter\n")
	fmt.Fprintf(b, "urnet_tcp_ack_direct_applied_total %d\n", tcpAckDirectAppliedCount.Load())
	b.WriteString("# HELP urnet_tcp_ack_queued_total Pure acks that took the ordered sequence queue (direct path off or not yet established).\n")
	b.WriteString("# TYPE urnet_tcp_ack_queued_total counter\n")
	fmt.Fprintf(b, "urnet_tcp_ack_queued_total %d\n", tcpAckQueuedCount.Load())
	b.WriteString("# HELP urnet_tcp_ack_stale_ignored_total Ack or window updates ignored because they moved the greatest advertised edge backwards.\n")
	b.WriteString("# TYPE urnet_tcp_ack_stale_ignored_total counter\n")
	fmt.Fprintf(b, "urnet_tcp_ack_stale_ignored_total %d\n", tcpAckStaleIgnoredCount.Load())
	b.WriteString("# HELP urnet_tcp_ack_direct_tripped Whether the pure ack direct path tripped itself onto the old queued path after an invariant violation (1) or not (0).\n")
	b.WriteString("# TYPE urnet_tcp_ack_direct_tripped gauge\n")
	fmt.Fprintf(b, "urnet_tcp_ack_direct_tripped %d\n", tcpAckTripped.Load())
	b.WriteString("# HELP urnet_tcp_ack_invariant_violations_total Invariant violations seen by the pure ack self-check (must stay zero).\n")
	b.WriteString("# TYPE urnet_tcp_ack_invariant_violations_total counter\n")
	fmt.Fprintf(b, "urnet_tcp_ack_invariant_violations_total %d\n", tcpAckInvariantViolations.Load())
	b.WriteString("# HELP urnet_tcp_ack_shadow_disagreements_total Sampled shadow comparisons where an in-order ack landed differently under the new model than the old overwrite rule would have (must stay zero).\n")
	b.WriteString("# TYPE urnet_tcp_ack_shadow_disagreements_total counter\n")
	fmt.Fprintf(b, "urnet_tcp_ack_shadow_disagreements_total %d\n", tcpAckShadowDisagreements.Load())
}
