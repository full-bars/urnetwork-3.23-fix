package main

// A randomized model test of the IPv6 dual stack gate (proxy_dualstack.go).
//
// The store publishes a sweep as two results: the completed UDP check first,
// its IPv6 measurement a moment later. A poll can land between the two
// publications, so the gate must reach the same verdict whether it saw the
// sweep whole or in two polls, and however many polls fell in between.
//
// modelGate below is the intended rule written from the outside. It is fed one
// publication at a time (never a controller poll), so its verdict cannot depend
// on how often the controller polls. Two replays drive the same sequences
// through it: one calls the gate with the store's publications by hand, and one
// drives the real store and the real proxiedIPv6Controller, so the paths where
// the store hands out no result at all are covered too. The first replay
// asserts the gate and the model agree at every event and that inserting polls
// between publications changes nothing; the second asserts the transport the
// controller starts and stops matches the model's running state.

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// ---- the intended rule, as a reference implementation ---------------------

// The rules the model states, in the order the review of round 5 named them:
//
//   - a start needs two qualifying measurements and a stop two completed checks
//     that disqualify; a poll of the same result counts neither again.
//   - an expired record stops a running transport on the poll that sees it,
//     including the poll of a sweep's udp publication, which lands before the
//     fresh measurement replaces the record.
//   - a udp check contradicts a record only against the basis that record
//     qualified on: a tcp-only record stands on no-relay, so a mismatch takes
//     that basis away and counts one observation, while the same check against
//     a record that stands on its own udp verdict counts nothing extra.
//   - a record stamped before the check it lands beside is not that check's
//     measurement: it adds no stop observation of its own, but a disqualifying
//     one clears the pending start streak.
//   - a record the state file supplied is state, not an observation: the first
//     poll baselines it, and only measurements made since can start or stop.
//   - a check whose measurement has not arrived yet decides nothing; its
//     deferral ends with that measurement or with the next check.

// modelGate reports the same three outcomes as the gate: start, stop, or
// neither. Its state is the evidence it has counted, not a poll history.
type modelGate struct {
	// running mirrors the extra transport.
	running bool
	// baseline is set while the new gate still has to baseline the record the
	// store loaded from disk: its first poll consumes the stamps without
	// judging anything, so the model does the same for its first event.
	baseline bool
	// lastCheck and lastV6 are the stamps already consumed, so a repeated
	// event changes nothing.
	lastCheck time.Time
	lastV6    time.Time
	// standing is the newest measurement and the udp result it was published
	// with. The policy is read from these two.
	standing    proxyIPv6Result
	standingUDP proxyUDPStatus
	// recordUDP is the udp status the standing measurement qualified on. A
	// check contradicts the record only against that basis, so a tcp-only
	// record (no IPv6 UDP verdict) stands on no-relay.
	recordUDP proxyUDPStatus
	// starts counts the qualifying measurements of the current start streak
	// (two start the transport) and startExpiry is when the newest of them
	// stops being fresh.
	starts      int
	startExpiry time.Time
	// stops counts consecutive completed checks that disqualify, and stopOf is
	// the check that supplied the newest one, so one sweep adds at most one.
	stops  int
	stopOf time.Time
	// pending is the check whose measurement has not arrived yet. Such a check
	// decides nothing at once: the gate cannot tell it from a deferred sweep,
	// so the failure streak it would end stays until its measurement or the
	// next check resolves it.
	pending time.Time
}

// fresh reports whether the record's evidence still counts at now.
func fresh(v6 proxyIPv6Result, now time.Time) bool {
	return !v6.At.IsZero() && v6.Interval > 0 && now.Before(v6.At.Add(2*v6.Interval))
}

// qualifies is the policy: does this udp result and this measurement justify
// running the transport now.
func (m *modelGate) qualifies(udp proxyUDPStatus, v6 proxyIPv6Result, now time.Time) bool {
	if v6.Status != proxyIPv6Yes || !fresh(v6, now) {
		return false
	}
	switch udp {
	case proxyUDPOK:
		return v6.UDP == "yes"
	case proxyUDPNoRelay:
		return v6.TCPExit != ""
	}
	return false
}

// recordQualifies reports whether the measurement still stands on the basis it
// qualified with, which is what a later udp check has to contradict to count a
// stop observation.
func (m *modelGate) recordQualifies(basis proxyUDPStatus, v6 proxyIPv6Result, now time.Time) bool {
	if v6.Status != proxyIPv6Yes || !fresh(v6, now) {
		return false
	}
	return v6.UDP == "yes" || (basis == proxyUDPNoRelay && v6.TCPExit != "")
}

// pruneStart drops a pending first measurement whose evidence expired before
// now: two results start the transport only while the first is still fresh.
func (m *modelGate) pruneStart(now time.Time) {
	if m.starts > 0 && !now.Before(m.startExpiry) {
		m.starts = 0
	}
}

// expiryStop stops a running transport whose measurement is no longer
// evidence. An expired record clears the streaks.
func (m *modelGate) expiryStop(now time.Time, v6 proxyIPv6Result) bool {
	if !m.running || v6.At.IsZero() || now.Before(v6.At.Add(2*v6.Interval)) {
		return false
	}
	m.running = false
	m.starts = 0
	m.stops = 0
	return true
}

// countStart adds a new qualifying measurement to the start streak.
func (m *modelGate) countStart(v6 proxyIPv6Result, now time.Time) {
	m.pruneStart(now)
	if m.starts < 2 {
		m.starts++
	}
	m.startExpiry = v6.At.Add(2 * v6.Interval)
}

// judge applies one observation: a completed check together with the
// measurement it published, or with the standing record when the sweep did not
// measure IPv6 again. A qualifying observation ends the failure streak; a
// disqualifying one adds at most one stop observation, for its own check.
func (m *modelGate) judge(checkAt time.Time, udp proxyUDPStatus, v6 proxyIPv6Result, measured bool, now time.Time) {
	if measured {
		// a new measurement replaces the basis the one before it stood on
		m.recordUDP = proxyUDPUnknown
	}
	if m.qualifies(udp, v6, now) {
		m.stops = 0
		if measured {
			m.recordUDP = udp
			m.countStart(v6, now)
		}
		return
	}
	if measured || m.recordQualifies(m.recordUDP, v6, now) {
		if checkAt == m.stopOf {
			// the check that stands already added its one observation, so this
			// record adds none and leaves the streaks alone
			return
		}
		m.stops++
		m.stopOf = checkAt
		m.starts = 0
	}
}

// resolvePending applies the poll that ends a deferred check. A record stamped
// for it (or for a later check) just ends the deferral: the record is judged on
// its own. A new check that brought no record of its own also ends the failure
// streak, because that deferral cannot be a sweep of the new check.
func (m *modelGate) resolvePending(checkNew bool, recordForCheck bool) {
	if m.pending.IsZero() {
		return
	}
	if recordForCheck {
		m.pending = time.Time{}
		return
	}
	if checkNew {
		m.stops = 0
		m.pending = time.Time{}
	}
}

// decide turns the streaks into the one transition a poll can make.
func (m *modelGate) decide(udp proxyUDPStatus, v6 proxyIPv6Result, now time.Time) (start bool, stop bool) {
	if !m.running && m.starts >= 2 && m.qualifies(udp, v6, now) {
		m.running = true
		return true, false
	}
	if m.running && m.stops >= 2 {
		m.running = false
		return false, true
	}
	return false, false
}

// sweep is one completed check and the measurement it published. split says the
// store published the check first and its measurement a moment later, so a poll
// lands on the record that still stands: that poll is where an expired standing
// record stops the transport, before the fresh measurement replaces it. Without
// split both results were in place before the next poll, so no poll ever saw the
// record the measurement replaced.
func (m *modelGate) sweep(now time.Time, udp proxyUDPStatus, checkAt time.Time, v6 proxyIPv6Result, split bool) (start bool, stop bool) {
	m.pruneStart(now)
	switch {
	case m.baseline:
		// the poll of the udp publication baselines the restored record, so
		// only the sweep's own measurement is judged
		m.baseline = false
	case split:
		stop = m.expiryStop(now, m.standing)
		m.resolvePending(true, false)
		switch {
		case m.qualifies(udp, m.standing, now):
			// the check landed before its own measurement: it decides nothing
			// until that measurement arrives, whatever came before it
			m.pending = checkAt
		case checkAt == m.stopOf:
			// one check adds at most one stop observation
		case m.recordQualifies(m.recordUDP, m.standing, now):
			m.stops++
			m.stopOf = checkAt
			m.starts = 0
		}
	default:
		m.resolvePending(true, true)
	}
	// the measurement of this check is here: whatever was deferred is resolved
	m.resolvePending(false, true)
	m.lastCheck, m.lastV6 = checkAt, v6.At
	m.standing, m.standingUDP = v6, udp
	m.judge(checkAt, udp, v6, true, now)
	if m.expiryStop(now, v6) {
		stop = true
	}
	start, again := m.decide(udp, v6, now)
	return start, stop || again
}

// staleRecord is a record published by an earlier check than the one the store
// now holds: an IPv6-only refresh that started before a lazy check completed,
// so its stamp is older than the check it lands beside. It is not that check's
// measurement and adds no stop observation of its own, but a disqualifying one
// still clears the pending start streak: one bad measurement must not leave the
// next good one a start. A qualifying one counts like any new measurement.
func (m *modelGate) staleRecord(now time.Time, udp proxyUDPStatus, checkAt time.Time, v6 proxyIPv6Result) (start bool, stop bool) {
	m.baseline = false
	m.pruneStart(now)
	m.resolvePending(true, false)
	if m.qualifies(udp, v6, now) {
		m.stops = 0
		m.recordUDP = udp
		m.countStart(v6, now)
	} else {
		// the check may still contradict a record that stands on its own udp
		// verdict, which is what a contradiction is counted against
		if m.recordQualifies(m.recordUDP, v6, now) && checkAt != m.stopOf {
			m.stops++
			m.stopOf = checkAt
		}
		m.starts = 0
		m.recordUDP = proxyUDPUnknown
	}
	m.lastCheck, m.lastV6 = checkAt, v6.At
	m.standing, m.standingUDP = v6, udp
	if m.expiryStop(now, v6) {
		stop = true
	}
	start, again := m.decide(udp, v6, now)
	return start, stop || again
}

// udpOnly is a completed check whose sweep did not measure IPv6 again, so it is
// judged with the record that still stands.
func (m *modelGate) udpOnly(now time.Time, udp proxyUDPStatus, checkAt time.Time) (start bool, stop bool) {
	m.pruneStart(now)
	stop = m.expiryStop(now, m.standing)
	m.lastCheck = checkAt
	if m.baseline {
		m.base(checkAt, udp, m.standing, now)
	} else {
		m.resolvePending(true, false)
		m.standingUDP = udp
		switch {
		case m.qualifies(udp, m.standing, now):
			// the check qualifies with the record that stands, but it brought
			// no measurement of its own: the gate defers it, so the failure
			// streak it would end stays until the next check does
			m.pending = checkAt
		case checkAt == m.stopOf:
			// one udp check adds at most one stop observation
		case m.recordQualifies(m.recordUDP, m.standing, now):
			m.stops++
			m.stopOf = checkAt
			m.starts = 0
		}
	}
	start, again := m.decide(udp, m.standing, now)
	return start, stop || again
}

// measurement is a new measurement published on its own, without a new check.
func (m *modelGate) measurement(now time.Time, v6 proxyIPv6Result) (start bool, stop bool) {
	m.pruneStart(now)
	m.lastV6 = v6.At
	if m.baseline {
		m.base(m.lastCheck, m.standingUDP, v6, now)
	} else {
		m.resolvePending(false, !m.pending.IsZero() && !v6.At.Before(m.pending))
		m.standing = v6
		m.judge(m.lastCheck, m.standingUDP, v6, true, now)
	}
	if m.expiryStop(now, v6) {
		stop = true
	}
	start, again := m.decide(m.standingUDP, v6, now)
	return start, stop || again
}

// settle is a poll: nothing new was published, but evidence can expire and a
// streak can decide.
func (m *modelGate) settle(now time.Time) (start bool, stop bool) {
	if m.baseline {
		// a poll with nothing new carries the stamps the gate already had
		m.base(m.lastCheck, m.standingUDP, m.standing, now)
	}
	m.pruneStart(now)
	stop = m.expiryStop(now, m.standing)
	start, again := m.decide(m.standingUDP, m.standing, now)
	return start, stop || again
}

// poll is one poll with whatever the store hands out, for the times the model
// cannot name the publication: a run of publications landed while the switch was
// off, so the poll sees only their last state. A check and a record published
// together are one observation; a record stamped before its check is the stale
// one; either alone is judged against what stands.
func (m *modelGate) poll(now time.Time, udp proxyUDPResult, v6 proxyIPv6Result) (start bool, stop bool) {
	if m.baseline {
		m.base(udp.CheckedAt, udp.Status, v6, now)
		// the baseline poll decides nothing at all
		return false, false
	}
	checkNew := !udp.CheckedAt.IsZero() && !udp.CheckedAt.Equal(m.lastCheck)
	v6New := !v6.At.IsZero() && !v6.At.Equal(m.lastV6)
	switch {
	case checkNew && v6New:
		if v6.At.Before(udp.CheckedAt) {
			return m.staleRecord(now, udp.Status, udp.CheckedAt, v6)
		}
		return m.sweep(now, udp.Status, udp.CheckedAt, v6, false)
	case checkNew:
		return m.udpOnly(now, udp.Status, udp.CheckedAt)
	case v6New:
		return m.measurement(now, v6)
	}
	return m.settle(now)
}

// disable is the kill switch coming off: the transport stops, the streaks are
// dropped, and the consumed stamps stay so a cached result is not counted again.
func (m *modelGate) disable() (stop bool) {
	stop = m.running
	m.running = false
	m.starts = 0
	m.stops = 0
	m.pending = time.Time{}
	return stop
}

// restart begins a new gate for an identity. A record the store loaded from
// disk is state, not an observation, so the new gate baselines it; a record
// this process measured is not marked, so a hot swap counts it as one
// measurement of this process.
func (m *modelGate) restart(now time.Time, checkAt time.Time, udp proxyUDPStatus, v6 proxyIPv6Result, restored bool) {
	*m = modelGate{}
	if v6.At.IsZero() {
		return
	}
	m.standing, m.standingUDP = v6, udp
	m.lastCheck, m.lastV6 = checkAt, v6.At
	if restored {
		// the first poll baselines the record instead of judging it
		m.baseline = true
		return
	}
	m.judge(checkAt, udp, v6, true, now)
}

// base consumes the always-first poll of a gate that started on a restored
// record. It judges nothing: the gate only records the stamps it was handed.
func (m *modelGate) base(checkAt time.Time, udp proxyUDPStatus, v6 proxyIPv6Result, now time.Time) {
	m.baseline = false
	m.lastCheck, m.lastV6 = checkAt, v6.At
	m.standing, m.standingUDP = v6, udp
	if m.qualifies(udp, v6, now) {
		// the basis the restored record stands on, until a measurement of this
		// process replaces it
		m.recordUDP = udp
	}
}

// inFlight is a poll while a UDP check runs: the store hands out no result, so
// nothing is judged. A record with no measurement is no evidence at all and
// resets the gate; a running transport whose record expired stops at once.
func (m *modelGate) inFlight(now time.Time, v6 proxyIPv6Result) (stop bool) {
	if v6.At.IsZero() {
		stop = m.running
		*m = modelGate{}
		return stop
	}
	if m.running && !fresh(v6, now) {
		m.running = false
		m.starts = 0
		m.stops = 0
		// the consumed record goes with the stop; the check stamp stays
		m.lastV6 = time.Time{}
		m.recordUDP = proxyUDPUnknown
		m.pending = time.Time{}
		m.stopOf = time.Time{}
		return true
	}
	return false
}

// missing is a poll with no store entry at all: no measurement is no evidence,
// so a running transport stops and the streaks start over.
func (m *modelGate) missing() (stop bool) {
	stop = m.running
	*m = modelGate{}
	return stop
}

// noRecord is a poll whose entry carries no IPv6 measurement, the shape a pruned
// and recreated entry has: the same as having no entry at all.
func (m *modelGate) noRecord() (stop bool) {
	stop = m.running
	*m = modelGate{}
	return stop
}

// expiredEvidence is a poll whose UDP result is past its TTL: the transport
// stops at once, the streaks are cleared, and the consumed stamps stay so the
// next check cannot count the record again.
func (m *modelGate) expiredEvidence() (stop bool) {
	stop = m.running
	m.running = false
	m.starts = 0
	m.stops = 0
	m.pending = time.Time{}
	return stop
}

// ---- the events the store and the controller produce -----------------------

// sweepShape says how the store published a sweep's two results.
type sweepShape int

const (
	// sweepSplit publishes the completed udp check first and its measurement a
	// moment later, so a poll lands between the two.
	sweepSplit sweepShape = iota
	// sweepAtomic publishes both before the next poll.
	sweepAtomic
	// sweepStale publishes a measurement stamped before its check: an IPv6-only
	// refresh that started before a lazy check completed.
	sweepStale
)

func (self sweepShape) String() string {
	switch self {
	case sweepSplit:
		return "split"
	case sweepAtomic:
		return "atomic"
	case sweepStale:
		return "stale"
	}
	return "?"
}

type gateEventKind int

const (
	// eventSweep publishes a completed udp check and an IPv6 measurement.
	eventSweep gateEventKind = iota
	// eventUDPCheck completes a udp check whose sweep does not measure IPv6.
	eventUDPCheck
	// eventMeasurement publishes an IPv6 measurement without a check.
	eventMeasurement
	// eventPoll polls with nothing new published.
	eventPoll
	// eventAdvance moves the clock across an expiry.
	eventAdvance
	// eventRestart starts a new process on the restored store.
	eventRestart
	eventDisable
	eventEnable
	// eventInFlight polls while a udp check and its measurement are both running.
	eventInFlight
	// eventOverlap is a lazy check that lands while an IPv6-only refresh runs:
	// the check completes first and the refresh publishes its older record.
	eventOverlap
	// eventExpire moves the udp evidence past its TTL.
	eventExpire
	// eventMissing drops the store entry.
	eventMissing
	// eventEvict evicts the entry and recreates it with a fresh measurement.
	eventEvict
)

func (self gateEventKind) String() string {
	switch self {
	case eventSweep:
		return "sweep"
	case eventUDPCheck:
		return "udp-check"
	case eventMeasurement:
		return "measurement"
	case eventPoll:
		return "poll"
	case eventAdvance:
		return "advance"
	case eventRestart:
		return "restart"
	case eventDisable:
		return "disable"
	case eventEnable:
		return "enable"
	case eventInFlight:
		return "in-flight"
	case eventOverlap:
		return "overlap"
	case eventExpire:
		return "expire"
	case eventMissing:
		return "missing"
	case eventEvict:
		return "evict"
	}
	return "?"
}

type gateEvent struct {
	kind gateEventKind
	// udp is the status of the check the event publishes
	udp proxyUDPStatus
	// v6 is the measurement the event publishes, with its stamp set by the
	// replay
	v6 proxyIPv6Result
	// shape says how a sweep's two results were published
	shape sweepShape
	// older is how far a stale record's stamp sits behind its check
	older time.Duration
	// advance is how far the clock moves before the event
	advance time.Duration
	// restored marks a restart on a store it loaded its record from disk into
	restored bool
}

func (self gateEvent) String() string {
	switch self.kind {
	case eventSweep:
		if self.shape == sweepStale {
			return fmt.Sprintf("sweep(%s, udp=%s, v6=%s/%s, older=%s)", self.shape, self.udp, self.v6.Status, self.v6.UDP, self.older)
		}
		return fmt.Sprintf("sweep(%s, udp=%s, v6=%s/%s, interval=%s)", self.shape, self.udp, self.v6.Status, self.v6.UDP, self.v6.Interval)
	case eventUDPCheck:
		return fmt.Sprintf("udp-check(udp=%s)", self.udp)
	case eventMeasurement:
		return fmt.Sprintf("measurement(v6=%s/%s, interval=%s)", self.v6.Status, self.v6.UDP, self.v6.Interval)
	case eventAdvance:
		return fmt.Sprintf("advance(%s)", self.advance)
	}
	return self.kind.String()
}

// isPublication reports whether the event wrote the store.
func (self gateEvent) isPublication() bool {
	switch self.kind {
	case eventSweep, eventUDPCheck, eventMeasurement:
		return true
	}
	return false
}

func randomMeasurement(r *rand.Rand) proxyIPv6Result {
	status := []proxyIPv6Status{proxyIPv6Yes, proxyIPv6No, proxyIPv6Unknown}[r.Intn(3)]
	udp := []string{"yes", "unknown", "no", "mismatch"}[r.Intn(4)]
	interval := []time.Duration{time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour}[r.Intn(4)]
	// the stamp is the moment the sweep published it, set by the replay
	return v6Result(status, udp, time.Time{}, interval)
}

var (
	gateEventUDPStatuses = []proxyUDPStatus{proxyUDPOK, proxyUDPExitMismatch, proxyUDPNoRelay}
	gateEventAdvances    = []time.Duration{
		1 * time.Minute,
		5 * time.Minute,
		30 * time.Minute,
		3 * time.Hour,
		13 * time.Hour,
		25 * time.Hour,
		49 * time.Hour,
	}
)

// randomPublication is one store publication for the gate replay.
func randomPublication(r *rand.Rand) gateEvent {
	switch r.Intn(10) {
	case 0, 1, 2, 3, 4, 5:
		event := gateEvent{
			kind:    eventSweep,
			shape:   []sweepShape{sweepSplit, sweepSplit, sweepAtomic, sweepAtomic, sweepStale}[r.Intn(5)],
			udp:     gateEventUDPStatuses[r.Intn(len(gateEventUDPStatuses))],
			v6:      randomMeasurement(r),
			advance: gateEventAdvances[r.Intn(len(gateEventAdvances))],
		}
		if event.shape == sweepStale {
			event.older = []time.Duration{5 * time.Minute, event.v6.Interval / 2, 2 * event.v6.Interval}[r.Intn(3)]
		}
		return event
	case 6, 7, 8:
		return gateEvent{kind: eventUDPCheck, udp: gateEventUDPStatuses[r.Intn(len(gateEventUDPStatuses))], advance: gateEventAdvances[r.Intn(3)]}
	}
	return gateEvent{kind: eventMeasurement, v6: randomMeasurement(r), advance: gateEventAdvances[r.Intn(3)]}
}

// generateSequence builds one random sequence for the gate replay. It includes
// publications while the switch is off: the controller does not read the store
// then, so they land and wait for the poll that follows the switch coming back.
func generateSequence(r *rand.Rand) []gateEvent {
	count := 10 + r.Intn(18)
	events := make([]gateEvent, 0, count)
	disabled := false
	for len(events) < count {
		roll := r.Intn(100)
		var event gateEvent
		switch {
		case disabled && roll < 45:
			event = gateEvent{kind: eventPoll, advance: gateEventAdvances[r.Intn(len(gateEventAdvances))] / 8}
		case disabled && roll < 70:
			event = randomPublication(r)
		case disabled && roll < 85:
			event = gateEvent{kind: eventAdvance, advance: gateEventAdvances[r.Intn(3)]}
		case disabled:
			event = gateEvent{kind: eventEnable, advance: time.Minute}
			disabled = false
		case roll < 50:
			event = randomPublication(r)
		case roll < 60:
			event = gateEvent{kind: eventUDPCheck, udp: gateEventUDPStatuses[r.Intn(len(gateEventUDPStatuses))], advance: gateEventAdvances[r.Intn(3)]}
		case roll < 68:
			event = gateEvent{kind: eventMeasurement, v6: randomMeasurement(r), advance: gateEventAdvances[r.Intn(3)]}
		case roll < 84:
			event = gateEvent{kind: eventPoll, advance: gateEventAdvances[r.Intn(len(gateEventAdvances))]}
		case roll < 94:
			event = gateEvent{kind: eventAdvance, advance: gateEventAdvances[r.Intn(len(gateEventAdvances))]}
		case roll < 97:
			event = gateEvent{kind: eventRestart, advance: time.Minute}
		default:
			event = gateEvent{kind: eventDisable, advance: time.Minute}
			disabled = true
		}
		events = append(events, event)
	}
	return events
}

// generateControllerSequence builds one random sequence for the store and
// controller replay: the same publications, plus the states where the store
// hands out no completed result at all.
func generateControllerSequence(r *rand.Rand) []gateEvent {
	count := 12 + r.Intn(14)
	events := make([]gateEvent, 0, count)
	disabled := false
	for len(events) < count {
		roll := r.Intn(100)
		var event gateEvent
		switch {
		case disabled && roll < 25:
			event = gateEvent{kind: eventPoll, advance: gateEventAdvances[r.Intn(len(gateEventAdvances))] / 8}
		case disabled && roll < 75:
			event = randomPublication(r)
		case disabled:
			event = gateEvent{kind: eventEnable, advance: time.Minute}
			disabled = false
		case roll < 45:
			event = randomPublication(r)
		case roll < 53:
			event = gateEvent{kind: eventPoll, advance: gateEventAdvances[r.Intn(len(gateEventAdvances))]}
		case roll < 63:
			event = gateEvent{kind: eventAdvance, advance: gateEventAdvances[r.Intn(len(gateEventAdvances))]}
		case roll < 70:
			event = gateEvent{kind: eventInFlight, advance: gateEventAdvances[r.Intn(3)]}
		case roll < 77:
			event = gateEvent{kind: eventOverlap, udp: gateEventUDPStatuses[r.Intn(len(gateEventUDPStatuses))], v6: randomMeasurement(r), older: []time.Duration{5 * time.Minute, 30 * time.Minute, 2 * time.Hour}[r.Intn(3)], advance: gateEventAdvances[r.Intn(3)]}
		case roll < 81:
			event = gateEvent{kind: eventExpire, advance: gateEventAdvances[r.Intn(3)]}
		case roll < 85:
			event = gateEvent{kind: eventMissing, advance: gateEventAdvances[r.Intn(3)]}
		case roll < 90:
			event = gateEvent{kind: eventEvict, udp: proxyUDPOK, v6: randomMeasurement(r), advance: gateEventAdvances[r.Intn(len(gateEventAdvances))]}
		case roll < 95:
			event = gateEvent{kind: eventRestart, advance: gateEventAdvances[r.Intn(3)]}
		case roll < 98:
			event = gateEvent{kind: eventRestart, restored: true, advance: gateEventAdvances[r.Intn(3)]}
		default:
			event = gateEvent{kind: eventDisable, advance: time.Minute}
			disabled = true
		}
		events = append(events, event)
	}
	return events
}

// ---- replaying a sequence through the real gate and the model -------------

// gateRun is one replay: the real gate behind the controller's switch, the
// model, the cached measurement the store hands out, and the clock.
type gateRun struct {
	gate  proxyIPv6Gate
	model *modelGate
	now   time.Time
	udp   proxyUDPResult
	v6    proxyIPv6Result
	// restored is the mark the store puts on a record it loaded from disk and
	// no measurement of this process has replaced
	restored bool
	disabled bool
}

// observe polls with the record as the store hands it out: marked while it is
// still the one the file supplied.
func (r *gateRun) observe() (start bool, stop bool) {
	v6 := r.v6
	v6.restored = r.restored && !v6.At.IsZero()
	return r.gate.observe(r.now, r.udp, v6)
}

// poll is the controller's step. A disabled switch stops the transport and
// clears the streaks while keeping the consumed stamps, so a cached result is
// not counted again when the switch comes back on.
func (r *gateRun) poll() (start bool, stop bool) {
	if r.disabled {
		stop = r.gate.running
		r.gate = proxyIPv6Gate{
			lastCheckedAt:    r.gate.lastCheckedAt,
			lastExpiry:       r.gate.lastExpiry,
			lastV6At:         r.gate.lastV6At,
			lastCountedCheck: r.gate.lastCountedCheck,
			recordUDP:        r.gate.recordUDP,
		}
		return false, stop
	}
	return r.observe()
}

// land applies a publication nobody read: the switch is off, so the controller
// does not look at the store and the gate sees nothing until it comes back on.
func (r *gateRun) land(event gateEvent) {
	switch event.kind {
	case eventSweep:
		r.udp = proxyUDPResult{Status: event.udp, CheckedAt: r.now}
		v6 := event.v6
		v6.At = r.now
		if event.shape == sweepStale {
			v6.At = r.now.Add(-event.older)
		}
		r.v6 = v6
		r.restored = false
	case eventUDPCheck:
		r.udp = proxyUDPResult{Status: event.udp, CheckedAt: r.now}
	case eventMeasurement:
		v6 := event.v6
		v6.At = r.now
		r.v6 = v6
		r.restored = false
	}
}

func (r *gateRun) restart(restored bool) {
	r.gate = proxyIPv6Gate{}
	r.restored = restored
	r.model.restart(r.now, r.udp.CheckedAt, r.udp.Status, r.v6, restored)
}

// replay drives one sequence. It returns one verdict per event and side. A
// sweep is published in two polls on the real side when its shape is split,
// with `between` extra polls inserted between its two publications.
func replay(events []gateEvent, between map[int]int, start time.Time, coverage *modelCoverage) (real []verdict, model []verdict, err error) {
	r := &gateRun{gate: proxyIPv6Gate{}, model: &modelGate{}, now: start}
	real = make([]verdict, len(events))
	model = make([]verdict, len(events))
	for i, event := range events {
		r.now = r.now.Add(event.advance + time.Second)
		if r.disabled && event.kind != eventEnable && event.kind != eventDisable {
			// the controller does not read the store while the switch is off,
			// so neither the gate nor the model judges anything
			r.land(event)
			real[i] = verdict{}
			model[i] = verdict{}
			if event.isPublication() {
				coverage.offPublications++
			}
			if real[i] != model[i] {
				return real, model, fmt.Errorf("event %d (%s): disabled gate said %+v, model said %+v", i, event, real[i], model[i])
			}
			continue
		}
		switch event.kind {
		case eventDisable:
			r.disabled = true
			// the controller stops the transport and resets the gate on this poll
			real[i] = verdictOf(r.poll())
			model[i] = verdict{false, r.model.disable()}
		case eventEnable:
			r.disabled = false
			real[i] = verdictOf(r.poll())
			model[i] = verdictOf(r.model.poll(r.now, r.udp, r.v6))
		case eventRestart:
			r.restart(event.restored)
			real[i] = verdictOf(r.poll())
			model[i] = verdictOf(r.model.settle(r.now))
		case eventPoll, eventAdvance:
			real[i] = verdictOf(r.poll())
			model[i] = verdictOf(r.model.settle(r.now))
		case eventUDPCheck:
			r.udp = proxyUDPResult{Status: event.udp, CheckedAt: r.now}
			real[i] = verdictOf(r.poll())
			model[i] = verdictOf(r.model.udpOnly(r.now, event.udp, r.now))
		case eventMeasurement:
			event.v6.At = r.now
			r.v6 = event.v6
			r.restored = false
			real[i] = verdictOf(r.poll())
			model[i] = verdictOf(r.model.measurement(r.now, event.v6))
		case eventSweep:
			checkAt := r.now
			// the store publishes the completed udp check first, with the
			// previous measurement still in place
			r.udp = proxyUDPResult{Status: event.udp, CheckedAt: checkAt}
			switch event.shape {
			case sweepStale:
				// the refresh started before a lazy check completed, so its
				// record is stamped behind the check that is in place now
				v6 := event.v6
				v6.At = checkAt.Add(-event.older)
				r.v6 = v6
				r.restored = false
				real[i] = verdictOf(r.poll())
				model[i] = verdictOf(r.model.staleRecord(r.now, event.udp, checkAt, v6))
				coverage.stale++
			case sweepAtomic:
				v6 := event.v6
				v6.At = checkAt
				r.v6 = v6
				r.restored = false
				real[i] = verdictOf(r.poll())
				model[i] = verdictOf(r.model.sweep(r.now, event.udp, checkAt, v6, false))
				coverage.atomic++
			default:
				first := verdictOf(r.poll())
				for k := 0; k < between[i]; k++ {
					if extra := verdictOf(r.poll()); extra.Start || extra.Stop {
						return real, model, fmt.Errorf("event %d (%s): an inserted poll changed the gate: %+v", i, event, extra)
					}
				}
				event.v6.At = checkAt
				r.v6 = event.v6
				// the sweep measured the record: this process owns it now
				r.restored = false
				second := verdictOf(r.poll())
				if first.Start && second.Start || first.Stop && second.Stop {
					return real, model, fmt.Errorf("event %d (%s): one sweep made the same transition twice: %+v then %+v", i, event, first, second)
				}
				if (first.Start || second.Start) && (first.Stop || second.Stop) {
					return real, model, fmt.Errorf("event %d (%s): one sweep both started and stopped the transport: %+v then %+v", i, event, first, second)
				}
				real[i] = verdict{first.Start || second.Start, first.Stop || second.Stop}
				model[i] = verdictOf(r.model.sweep(r.now, event.udp, checkAt, event.v6, true))
				coverage.split++
			}
		default:
			return real, model, fmt.Errorf("event %d (%s) is not a gate event", i, event)
		}
		if real[i] != model[i] {
			return real, model, fmt.Errorf("event %d (%s): the gate said %+v, the model said %+v", i, event, real[i], model[i])
		}
	}
	return real, model, nil
}

// ---- replaying a sequence through the real store and the controller -------

// controllerRun drives one identity's store entry and its controller. Store
// states are written the way the sweep, the TTL and the prune leave them, and
// every event is one or more real controller steps.
type controllerRun struct {
	t          *testing.T
	name       string
	store      *proxyUDPCheckStore
	settings   *connect.ProxySettings
	transport  *proxiedPlatformTransport
	controller *proxiedIPv6Controller
	model      *modelGate
	now        time.Time
	builds     int
	disabled   bool
	starts     int
	stops      int
}

func newControllerRun(t *testing.T, name string) *controllerRun {
	t.Helper()
	h := newIPv6CheckHarness(t)
	run := &controllerRun{t: t, name: name, store: h.store, settings: ipv6TestSettings(), model: &modelGate{}}
	run.disabled = false
	run.swap()
	t.Cleanup(func() { run.transport.stopExtra() })
	return run
}

// swap replaces the controller and its transport the way a reload rebuilds the
// identity: the store survives, the gate and the wrapper do not.
func (self *controllerRun) swap() {
	if self.transport != nil {
		self.transport.Close()
	}
	self.transport = newProxiedPlatformTransport(&fakePlatformTransport{}, &connect.ClientAuth{ByJwt: "jwt"})
	transport := self.transport
	self.controller = &proxiedIPv6Controller{
		store:     self.store,
		key:       self.settings.Key(),
		transport: transport,
		build: func(*connect.ClientAuth) providerPlatformTransport {
			self.builds++
			return &fakePlatformTransport{}
		},
		enabled: func() bool { return !self.disabled },
	}
}

// publishCheck writes the udp result of a completed check and keeps the record.
func (self *controllerRun) publishCheck(status proxyUDPStatus, at time.Time) {
	self.store.mu.Lock()
	defer self.store.mu.Unlock()
	key := self.settings.Key()
	entry := self.store.entries[key]
	if entry == nil || entry.done != nil {
		entry = &proxyUDPCheckEntry{}
		self.store.entries[key] = entry
	}
	entry.result = proxyUDPResult{Status: status}
	entry.realStatus = status
	entry.at = at
	entry.completedAt = at
}

// publishRecord writes an IPv6 measurement with its own stamp.
func (self *controllerRun) publishRecord(v6 proxyIPv6Result, restored bool) {
	self.store.mu.Lock()
	defer self.store.mu.Unlock()
	key := self.settings.Key()
	entry := self.store.entries[key]
	if entry == nil {
		entry = &proxyUDPCheckEntry{}
		self.store.entries[key] = entry
	}
	entry.ipv6 = v6
	entry.ipv6Restored = restored
	entry.ipv6Running = false
}

// installCheck puts a running udp check on the entry and keeps the record.
func (self *controllerRun) installCheck() bool {
	self.store.mu.Lock()
	defer self.store.mu.Unlock()
	entry := self.store.entries[self.settings.Key()]
	if entry == nil {
		return false
	}
	entry.done = make(chan struct{})
	entry.ipv6Running = true
	return true
}

// completeCheck publishes the result of the running check and clears its flag.
func (self *controllerRun) completeCheck(status proxyUDPStatus, at time.Time) {
	self.store.mu.Lock()
	entry := self.store.entries[self.settings.Key()]
	if entry != nil && entry.done != nil {
		close(entry.done)
		entry.done = nil
	}
	self.store.mu.Unlock()
	self.publishCheck(status, at)
}

// dropCheck ends a running check that published nothing: the entry keeps the
// result it had, the way a cancelled check restores it.
func (self *controllerRun) dropCheck() {
	self.store.mu.Lock()
	defer self.store.mu.Unlock()
	if entry := self.store.entries[self.settings.Key()]; entry != nil {
		entry.done = nil
		entry.ipv6Running = false
	}
}

// expireUDP moves the last completed check past its TTL.
func (self *controllerRun) expireUDP() {
	self.store.mu.Lock()
	defer self.store.mu.Unlock()
	entry := self.store.entries[self.settings.Key()]
	if entry == nil {
		return
	}
	entry.at = self.now.Add(-proxyUDPTTL(entry.result.Status) - time.Minute)
	entry.completedAt = entry.at
	entry.realStatus = entry.result.Status
}

// evict runs the store's own prune until the entry is gone, then checks it is.
func (self *controllerRun) evict() {
	self.store.mu.Lock()
	self.store.lastPrune = time.Time{}
	if entry := self.store.entries[self.settings.Key()]; entry != nil {
		entry.at = self.now.Add(-3 * proxyUDPOKTTL)
		entry.result = proxyUDPResult{Status: proxyUDPOK}
		entry.realStatus = proxyUDPOK
		entry.ipv6 = v6Result(proxyIPv6Yes, "yes", entry.at, time.Hour)
	}
	self.store.mu.Unlock()
	self.store.prune(self.now)
	if _, _, state := self.store.measurement(self.settings.Key(), self.now); state != measurementMissing {
		self.t.Fatalf("%s: the store did not evict the entry", self.name)
	}
}

// drop removes the entry the way a prune at the size limit does.
func (self *controllerRun) drop() {
	self.store.mu.Lock()
	delete(self.store.entries, self.settings.Key())
	self.store.mu.Unlock()
}

// markRestored puts the store's mark on the record: the file supplied it and no
// measurement of this process has replaced it.
func (self *controllerRun) markRestored() {
	self.store.mu.Lock()
	defer self.store.mu.Unlock()
	if entry := self.store.entries[self.settings.Key()]; entry != nil {
		entry.ipv6Restored = true
	}
}

// record is the store's IPv6 record as the controller reads it.
func (self *controllerRun) record() proxyIPv6Result {
	self.store.mu.Lock()
	defer self.store.mu.Unlock()
	if entry := self.store.entries[self.settings.Key()]; entry != nil {
		return entry.ipv6
	}
	return proxyIPv6Result{}
}

// pump runs one controller step and reports what the transport did.
func (self *controllerRun) pump() verdict {
	wasRunning := self.transport.hasExtra()
	self.controller.step(self.now)
	nowRunning := self.transport.hasExtra()
	return verdict{nowRunning && !wasRunning, wasRunning && !nowRunning}
}

// agree checks one event's verdict and the transport state it left behind.
func (self *controllerRun) agree(event gateEvent, got verdict, want verdict) {
	self.t.Helper()
	if got.Start {
		self.starts++
	}
	if got.Stop {
		self.stops++
	}
	if got != want {
		self.t.Fatalf("%s: event (%s): the controller said %+v, the model said %+v (gate yes=%d no=%d check=%s v6=%s basis=%s; model starts=%d stops=%d pending=%s check=%s v6=%s basis=%s)",
			self.name, event, got, want,
			self.controller.gate.yes, self.controller.gate.no,
			self.controller.gate.lastCheckedAt.Format("01-02T15:04"), self.controller.gate.lastV6At.Format("01-02T15:04"), self.controller.gate.recordUDP,
			self.model.starts, self.model.stops, self.model.pending.Format("01-02T15:04"),
			self.model.lastCheck.Format("01-02T15:04"), self.model.lastV6.Format("01-02T15:04"), self.model.recordUDP)
	}
	if self.transport.hasExtra() != self.model.running {
		self.t.Fatalf("%s: event (%s): the transport runs=%v, the model says running=%v", self.name, event, self.transport.hasExtra(), self.model.running)
	}
}

// step runs one controller step and the model poll of the same store state.
func (self *controllerRun) step(event gateEvent) verdict {
	got := self.pump()
	self.agree(event, got, self.storeModel())
	return got
}

// storeModel is what the model expects the controller to do with the store
// state as it is now.
func (self *controllerRun) storeModel() verdict {
	udp, v6, state := self.store.measurement(self.settings.Key(), self.now)
	switch state {
	case measurementMissing:
		return verdict{Stop: self.model.missing()}
	case measurementInFlight:
		return verdict{Stop: self.model.inFlight(self.now, v6)}
	case measurementExpired:
		return verdict{Stop: self.model.expiredEvidence()}
	}
	if v6.At.IsZero() {
		return verdict{Stop: self.model.noRecord()}
	}
	return verdictOf(self.model.poll(self.now, udp, v6))
}

// landWhileOff applies a publication nobody read: the switch is off, so the
// controller never looks at the store, and the model sees the result only when
// the switch comes back on.
func (self *controllerRun) landWhileOff(event gateEvent) {
	switch event.kind {
	case eventSweep:
		v6 := event.v6
		v6.At = self.now
		if event.shape == sweepStale {
			v6.At = self.now.Add(-event.older)
		}
		self.publishCheck(event.udp, self.now)
		self.publishRecord(v6, false)
	case eventUDPCheck:
		self.publishCheck(event.udp, self.now)
	case eventMeasurement:
		v6 := event.v6
		v6.At = self.now
		self.publishRecord(v6, false)
	}
}

func replayController(t *testing.T, name string, events []gateEvent, start time.Time, coverage *modelCoverage) (starts int, stops int) {
	r := newControllerRun(t, name)
	r.now = start
	for i, event := range events {
		r.now = r.now.Add(event.advance + time.Second)
		if r.disabled && event.kind != eventEnable && event.kind != eventDisable {
			r.landWhileOff(event)
			coverage.offPublications++
			continue
		}
		switch event.kind {
		case eventDisable:
			r.disabled = true
			r.agree(event, r.pump(), verdict{Stop: r.model.disable()})
		case eventEnable:
			r.disabled = false
			r.step(event)
		case eventPoll, eventAdvance:
			r.step(event)
		case eventUDPCheck:
			r.publishCheck(event.udp, r.now)
			r.step(event)
		case eventMeasurement:
			v6 := event.v6
			v6.At = r.now
			r.publishRecord(v6, false)
			r.step(event)
		case eventInFlight:
			if !r.installCheck() {
				break
			}
			r.step(event)
			// the check ends and the entry keeps what it had
			r.dropCheck()
			coverage.inFlight++
		case eventOverlap:
			if !r.installCheck() {
				break
			}
			// both run: the poll sees the record that stands
			first := r.pump()
			r.agree(event, first, r.storeModel())
			// the lazy check completes, then the refresh publishes its older
			// record: the record is not that check's measurement
			r.completeCheck(event.udp, r.now)
			v6 := event.v6
			v6.At = r.now.Add(-event.older - time.Second)
			r.publishRecord(v6, false)
			second := r.pump()
			r.agree(event, second, r.storeModel())
			if first.Start && second.Start || first.Stop && second.Stop {
				t.Fatalf("%s: event %d (%s): the overlap made the same transition twice: %+v then %+v", name, i, event, first, second)
			}
			if (first.Start || second.Start) && (first.Stop || second.Stop) {
				t.Fatalf("%s: event %d (%s): the overlap both started and stopped the transport", name, i, event)
			}
			coverage.overlap++
		case eventExpire:
			r.expireUDP()
			r.step(event)
			coverage.expire++
		case eventMissing:
			r.drop()
			r.step(event)
			coverage.missing++
		case eventEvict:
			r.evict()
			first := r.pump()
			r.agree(event, first, r.storeModel())
			// a fresh measurement recreates the entry
			v6 := event.v6
			v6.At = r.now
			r.publishCheck(event.udp, r.now)
			r.publishRecord(v6, false)
			second := r.pump()
			r.agree(event, second, r.storeModel())
			coverage.evict++
		case eventRestart:
			if r.transport.hasExtra() != r.model.running {
				t.Fatalf("%s: event %d (%s): the transport and the model disagree before the swap", name, i, event)
			}
			teardown := verdict{Stop: r.model.running}
			if event.restored {
				r.markRestored()
			}
			// the new controller reads the store the way any controller does:
			// a completed measurement is this process's first observation, and
			// any other state is left to the poll below
			udp, v6, state := r.store.measurement(r.settings.Key(), r.now)
			r.swap()
			if state == measurementReady {
				r.model.restart(r.now, udp.CheckedAt, udp.Status, v6, event.restored)
			} else {
				r.model.restart(r.now, time.Time{}, proxyUDPUnknown, proxyIPv6Result{}, false)
			}
			after := r.pump()
			after.Stop = after.Stop || teardown.Stop
			r.agree(event, after, merged(teardown, r.storeModel()))
			if event.restored {
				coverage.restored++
			} else {
				coverage.hotSwap++
			}
		case eventSweep:
			checkAt := r.now
			r.publishCheck(event.udp, checkAt)
			switch event.shape {
			case sweepStale:
				v6 := event.v6
				v6.At = checkAt.Add(-event.older)
				r.publishRecord(v6, false)
				r.step(event)
				coverage.stale++
			case sweepAtomic:
				v6 := event.v6
				v6.At = checkAt
				r.publishRecord(v6, false)
				r.step(event)
				coverage.atomic++
			default:
				first := r.pump()
				v6 := event.v6
				v6.At = checkAt
				r.publishRecord(v6, false)
				second := r.pump()
				if first.Start && second.Start || first.Stop && second.Stop {
					t.Fatalf("%s: event %d (%s): one sweep made the same transition twice: %+v then %+v", name, i, event, first, second)
				}
				if (first.Start || second.Start) && (first.Stop || second.Stop) {
					t.Fatalf("%s: event %d (%s): one sweep both started and stopped the transport", name, i, event)
				}
				r.agree(event, merged(first, second), verdictOf(r.model.sweep(r.now, event.udp, checkAt, v6, true)))
				coverage.split++
			}
		default:
			t.Fatalf("%s: event %d (%s) is not a controller event", name, i, event)
		}
	}
	return r.starts, r.stops
}

// merged is the one verdict of an event made of several polls.
func merged(verdicts ...verdict) verdict {
	out := verdict{}
	for _, one := range verdicts {
		out.Start = out.Start || one.Start
		out.Stop = out.Stop || one.Stop
	}
	return out
}

// verdictOf converts the gate's two results to one verdict.
func verdictOf(start bool, stop bool) verdict { return verdict{start, stop} }

type verdict struct {
	Start bool
	Stop  bool
}

func (self verdict) String() string {
	switch {
	case self.Start && self.Stop:
		return "start+stop"
	case self.Start:
		return "start"
	case self.Stop:
		return "stop"
	}
	return "neither"
}

// ---- the test -------------------------------------------------------------

// modelCoverage counts the shapes the replay actually reached, so a generator
// that stops producing one of them fails the test instead of passing quietly.
type modelCoverage struct {
	split, atomic, stale, offPublications int
	inFlight, overlap, expire, missing    int
	evict, hotSwap, restored              int
}

func (self *modelCoverage) check(t *testing.T) {
	t.Helper()
	for _, want := range []struct {
		name string
		got  int
	}{
		{"a sweep split by a poll", self.split},
		{"a sweep published whole", self.atomic},
		{"a record stamped before its check", self.stale},
		{"a publication while the switch is off", self.offPublications},
		{"a poll while a check runs", self.inFlight},
		{"two checks in flight", self.overlap},
		{"expired udp evidence", self.expire},
		{"a missing entry", self.missing},
		{"an evicted and recreated entry", self.evict},
		{"a controller replaced mid-run", self.hotSwap},
		{"a restart on a restored record", self.restored},
	} {
		if want.got == 0 {
			t.Fatalf("the sequences never produced %s", want.name)
		}
	}
}

// directedSequences are hand written sequences for the shapes a random
// generator reaches only rarely, so the whole class stays covered.
func directedSequences() []gateSequence {
	qualifying := gateEvent{kind: eventSweep, udp: proxyUDPOK, v6: v6Result(proxyIPv6Yes, "yes", time.Time{}, 24*time.Hour)}
	badCheck := gateEvent{kind: eventUDPCheck, udp: proxyUDPExitMismatch}
	okCheck := gateEvent{kind: eventUDPCheck, udp: proxyUDPOK}
	measurement := gateEvent{kind: eventMeasurement, v6: v6Result(proxyIPv6Yes, "yes", time.Time{}, 24*time.Hour)}
	withAdvance := func(events ...gateEvent) []gateEvent {
		for i := range events {
			events[i].advance = time.Minute
		}
		return events
	}
	return []gateSequence{
		{
			// a udp-only qualifying check has no measurement of its own: it
			// ends the failure streak once a newer check shows it was udp only
			name: "a udp-only qualifying check ends the failure streak",
			events: withAdvance(
				qualifying, qualifying,
				badCheck,
				okCheck,
				badCheck,
				badCheck,
			),
		},
		{
			// a record restored from disk is a baseline, not a measurement
			name: "a start after a restart on a record loaded from disk",
			events: withAdvance(
				qualifying,
				gateEvent{kind: eventRestart, restored: true},
				measurement,
				qualifying,
			),
		},
		{
			// a disable keeps the consumed stamps: the cached record is not
			// counted again when the switch comes back on
			name: "a disable keeps the counted stamps",
			events: withAdvance(
				qualifying,
				gateEvent{kind: eventDisable},
				gateEvent{kind: eventEnable},
				gateEvent{kind: eventPoll},
				measurement,
				measurement,
			),
		},
		{
			// an expired record stops at once and clears both streaks
			name: "an expired record stops and clears the streaks",
			events: []gateEvent{
				{kind: eventSweep, udp: proxyUDPOK, v6: v6Result(proxyIPv6Yes, "yes", time.Time{}, time.Hour), advance: time.Minute},
				{kind: eventSweep, udp: proxyUDPOK, v6: v6Result(proxyIPv6Yes, "yes", time.Time{}, time.Hour), advance: time.Minute},
				{kind: eventAdvance, advance: 3 * time.Hour},
				{kind: eventMeasurement, v6: v6Result(proxyIPv6Yes, "yes", time.Time{}, time.Hour), advance: time.Minute},
				{kind: eventMeasurement, v6: v6Result(proxyIPv6Yes, "yes", time.Time{}, time.Hour), advance: time.Minute},
			},
		},
		{
			// the poll between the two publications of a sweep sees the record
			// that still stands, so an expired one stops the transport before
			// the fresh measurement replaces it
			name: "an expired standing record stops before its replacement",
			events: []gateEvent{
				{kind: eventSweep, udp: proxyUDPOK, v6: v6Result(proxyIPv6Yes, "yes", time.Time{}, time.Hour), advance: time.Minute},
				{kind: eventSweep, udp: proxyUDPOK, v6: v6Result(proxyIPv6Yes, "yes", time.Time{}, time.Hour), advance: time.Minute},
				{kind: eventSweep, udp: proxyUDPOK, v6: v6Result(proxyIPv6Yes, "yes", time.Time{}, time.Hour), advance: 3 * time.Hour},
			},
		},
		{
			// a tcp-only record stands on the no-relay status of its check, so
			// a check that takes that basis away counts one observation
			name: "a contradiction against a no-relay basis counts once",
			events: withAdvance(
				gateEvent{kind: eventSweep, udp: proxyUDPNoRelay, v6: v6Result(proxyIPv6Yes, "unknown", time.Time{}, 12*time.Hour)},
				gateEvent{kind: eventSweep, udp: proxyUDPNoRelay, v6: v6Result(proxyIPv6Yes, "unknown", time.Time{}, 12*time.Hour)},
				badCheck,
				badCheck,
			),
		},
		{
			// a disqualifying record stamped before its check adds no stop
			// observation of its own but breaks the pending start streak
			name: "an older negative record clears the start streak without a stop",
			events: withAdvance(
				gateEvent{kind: eventSweep, udp: proxyUDPOK, v6: v6Result(proxyIPv6Yes, "yes", time.Time{}, 12*time.Hour)},
				gateEvent{kind: eventSweep, shape: sweepStale, older: 5 * time.Minute, udp: proxyUDPExitMismatch, v6: v6Result(proxyIPv6Yes, "unknown", time.Time{}, 12*time.Hour)},
				gateEvent{kind: eventSweep, udp: proxyUDPOK, v6: v6Result(proxyIPv6Yes, "yes", time.Time{}, 12*time.Hour)},
			),
		},
	}
}

// directedControllerSequences cover the store states the random controller
// generator reaches only by luck.
func directedControllerSequences() []gateSequence {
	qualifying := func(shape sweepShape) gateEvent {
		return gateEvent{kind: eventSweep, shape: shape, udp: proxyUDPOK, v6: v6Result(proxyIPv6Yes, "yes", time.Time{}, 12*time.Hour), advance: 13 * time.Hour}
	}
	return []gateSequence{
		{
			// two checks in flight for one identity: the lazy check completes
			// first and the refresh publishes a record stamped before it
			name: "a lazy check completes beside a running refresh",
			events: []gateEvent{
				qualifying(sweepAtomic),
				{kind: eventOverlap, udp: proxyUDPOK, v6: v6Result(proxyIPv6Yes, "yes", time.Time{}, 12*time.Hour), older: 30 * time.Minute, advance: 13 * time.Hour},
				qualifying(sweepAtomic),
			},
		},
		{
			// the entry is evicted and recreated without a measurement: the
			// transport stops and the next measurement is the first of a streak
			name: "an evicted entry is recreated and counted from the start",
			events: []gateEvent{
				qualifying(sweepAtomic),
				qualifying(sweepAtomic),
				{kind: eventEvict, udp: proxyUDPOK, v6: v6Result(proxyIPv6Yes, "yes", time.Time{}, 12*time.Hour), advance: 3 * time.Hour},
				qualifying(sweepAtomic),
				qualifying(sweepAtomic),
			},
		},
		{
			// a controller replaced mid-run counts a record this process
			// measured, and baselines one the file supplied
			name: "a hot swap keeps measuring, a restored record is a baseline",
			events: []gateEvent{
				qualifying(sweepAtomic),
				{kind: eventRestart, advance: time.Minute},
				qualifying(sweepAtomic),
				{kind: eventRestart, restored: true, advance: time.Minute},
				qualifying(sweepAtomic),
				qualifying(sweepAtomic),
			},
		},
	}
}

type gateSequence struct {
	name   string
	events []gateEvent
}

// modelTestSequences is the number of random gate sequences the test replays.
const modelTestSequences = 2000

// modelTestControllerSequences is the number of random store and controller
// sequences the test replays.
const modelTestControllerSequences = 1000

// modelTestSeed is the fixed base seed: every run replays the same sequences.
func modelTestSeed(index int) int64 { return int64(index)*7919 + 13 }

func TestProxyIPv6GateMatchesTheModelOnRandomSequences(t *testing.T) {
	start := time.Date(2026, 11, 21, 6, 0, 0, 0, time.UTC)
	coverage := &modelCoverage{}

	sequences := make([]gateSequence, 0, modelTestSequences+8)
	for index := 0; index < modelTestSequences; index++ {
		seed := modelTestSeed(index)
		sequences = append(sequences, gateSequence{
			name:   fmt.Sprintf("seed %d", seed),
			events: generateSequence(rand.New(rand.NewSource(seed))),
		})
	}
	sequences = append(sequences, directedSequences()...)

	starts, stops := 0, 0
	for _, one := range sequences {
		events := one.events

		real, _, err := replay(events, nil, start, coverage)
		if err != nil {
			t.Fatalf("%s: %v\nsequence:\n%s", one.name, err, describeSequence(events))
		}
		for _, verdict := range real {
			if verdict.Start {
				starts++
			}
			if verdict.Stop {
				stops++
			}
		}

		// the same sequence with one poll between the publications of each
		// sweep in turn, and then with two polls at every such position
		for i, event := range events {
			if event.kind != eventSweep || event.shape != sweepSplit {
				continue
			}
			replayed, _, err := replay(events, map[int]int{i: 1}, start, &modelCoverage{})
			if err != nil {
				t.Fatalf("%s, one poll inserted at event %d: %v\nsequence:\n%s", one.name, i, err, describeSequence(events))
			}
			for j := range real {
				if replayed[j] != real[j] {
					t.Fatalf("%s: inserting a poll at event %d changed event %d from %+v to %+v\nsequence:\n%s", one.name, i, j, real[j], replayed[j], describeSequence(events))
				}
			}
		}
		all := map[int]int{}
		for i, event := range events {
			if event.kind == eventSweep && event.shape == sweepSplit {
				all[i] = 2
			}
		}
		if len(all) > 0 {
			replayed, _, err := replay(events, all, start, &modelCoverage{})
			if err != nil {
				t.Fatalf("%s, polls inserted at every publication: %v\nsequence:\n%s", one.name, err, describeSequence(events))
			}
			for j := range real {
				if replayed[j] != real[j] {
					t.Fatalf("%s: polls at every publication changed event %d from %+v to %+v\nsequence:\n%s", one.name, j, real[j], replayed[j], describeSequence(events))
				}
			}
		}
	}
	// a random test that never moves the transport proves nothing
	if starts < 100 || stops < 100 {
		t.Fatalf("the sequences exercised %d starts and %d stops, which is too few to trust", starts, stops)
	}

	controllerSequences := make([]gateSequence, 0, modelTestControllerSequences+8)
	for index := 0; index < modelTestControllerSequences; index++ {
		seed := modelTestSeed(index) + 1_000_003
		controllerSequences = append(controllerSequences, gateSequence{
			name:   fmt.Sprintf("controller seed %d", seed),
			events: generateControllerSequence(rand.New(rand.NewSource(seed))),
		})
	}
	controllerSequences = append(controllerSequences, directedControllerSequences()...)
	controllerStarts, controllerStops := 0, 0
	for _, one := range controllerSequences {
		oneStarts, oneStops := replayController(t, one.name, one.events, start, coverage)
		controllerStarts += oneStarts
		controllerStops += oneStops
	}
	if controllerStarts < 50 || controllerStops < 50 {
		t.Fatalf("the store and controller sequences exercised %d starts and %d stops, which is too few to trust", controllerStarts, controllerStops)
	}

	coverage.check(t)
	t.Logf("replayed %d gate sequences (%d starts, %d stops) and %d store/controller sequences (%d starts, %d stops)",
		len(sequences), starts, stops, len(controllerSequences), controllerStarts, controllerStops)
}

func describeSequence(events []gateEvent) string {
	out := ""
	for i, event := range events {
		out += fmt.Sprintf("  %2d %s\n", i, event)
	}
	return out
}
