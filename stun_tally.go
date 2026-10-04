package connect

import (
	"strings"
	"sync"
	"time"
)

// stunTally aggregates STUN server transaction results process-wide into one
// low-noise [stun] line, so the operator gets a single success+failure signal
// instead of pion's failure-only per-event logs. In particular it provides the
// SUCCESS signal that pion never logs at all: a successful STUN transaction is
// silent inside pion/ice.
//
// Pulse sources (both in the fork's own code — nothing in pion is edited):
//   - ok:   a server-reflexive (srflx) ICE candidate reaching the
//     OnICECandidate callback (see peerConn.addIceCandidates). A srflx
//     candidate only exists because the STUN server returned an
//     XOR-MAPPED-ADDRESS, so one srflx candidate == one successful STUN
//     transaction. Success is unobservable from the log stream, so it is
//     counted at the candidate site instead.
//   - fail: a STUN srflx-gathering error surfaced by pion/ice and routed
//     through pionLeveledLogger under the "ice" scope (matched by
//     isStunFailLine against gather.go's error strings).
//
// The aggregate is log-only: it observes results and never alters STUN
// behavior, timing, or NAT/ICE logic.
const (
	// stunHighInterval / stunLowInterval are the adaptive emit cadence.
	stunHighInterval = time.Minute
	stunLowInterval  = 5 * time.Minute

	// stunHighRateThreshold is the event rate (STUN result pulses/min) above
	// which the aggregate is considered "high volume" and logs every minute.
	// Below it the interval relaxes to 5 minutes.
	stunHighRateThreshold = 10.0
)

// stunTally is the process-global aggregate. It emits through the Logger of
// whichever call site recorded the triggering pulse, so the line follows the
// same logger (and level) as the pion errors it summarizes.
var stunTally = &stunTallyT{lastLogAt: time.Now()}

type stunTallyT struct {
	mu        sync.Mutex
	ok        int
	fail      int
	lastLogAt time.Time
	nextLogAt time.Time
}

// record counts one STUN result pulse as ok or fail, and when the adaptive
// interval has elapsed emits a single aggregate line then reschedules.
//
// Adaptive policy: at each emit, the event rate over the just-finished window
// is rate = (ok+fail) / elapsedMinutes. If rate >= stunHighRateThreshold
// (~10 events/min) the next line is scheduled 1 minute out; otherwise 5
// minutes out. Counters reset at each emit. With no STUN activity nothing is
// emitted, so an idle host stays silent.
func (t *stunTallyT) record(ok bool, log Logger) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if ok {
		t.ok++
	} else {
		t.fail++
	}

	now := time.Now()
	if t.nextLogAt.IsZero() {
		// First activity: prime the interval but do not emit yet.
		t.nextLogAt = now.Add(stunHighInterval)
		return
	}
	if now.Before(t.nextLogAt) {
		return
	}

	elapsedMin := now.Sub(t.lastLogAt).Minutes()
	if elapsedMin <= 0 {
		elapsedMin = 1
	}
	eventsPerMin := float64(t.ok+t.fail) / elapsedMin

	okN, failN := t.ok, t.fail
	t.ok, t.fail = 0, 0
	t.lastLogAt = now
	if eventsPerMin >= stunHighRateThreshold {
		t.nextLogAt = now.Add(stunHighInterval)
	} else {
		t.nextLogAt = now.Add(stunLowInterval)
	}

	log.Infof("📡 [stun] ok=%d fail=%d\n", okN, failN)
}

// stunFailMarkers match the pion/ice v4.2.7 srflx-gathering failure strings
// (gather.go) that surface through pionLeveledLogger. Each is worth one fail
// pulse. A successful STUN gather emits no log in pion, so ok pulses are never
// matched here — they come from the srflx candidate site instead.
var stunFailMarkers = []string{
	"Failed to resolve STUN host",                 // gather.go:835 (DNS)
	"failed to get server reflexive address",      // gather.go:874 (the STUN transaction)
	"failed to create server reflexive candidate", // gather.go:893
	"filtered for location tracking reasons",      // gather.go:841
}

// isStunFailLine reports whether a rendered pion log line describes a STUN
// srflx-gathering failure. Only the "ice" scope runs the STUN server gathers,
// so other scopes are skipped cheaply.
func isStunFailLine(scope, msg string) bool {
	if scope != "ice" {
		return false
	}
	for _, m := range stunFailMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}
