package main

// A randomized model test of the IPv6 dual stack gate (proxy_dualstack.go).
//
// The store publishes a sweep as two results: the completed UDP check first,
// its IPv6 measurement a moment later. A poll can land between the two
// publications, so the gate must reach the same verdict whether it saw the
// sweep whole or in two polls, and however many polls fell in between.
//
// modelGate below is the intended rule written from the outside. It is fed one
// completed event at a time and never sees a poll, so its verdict cannot depend
// on one. The test replays random event sequences through the real gate and the
// model, asserts they agree at every event, then replays every sequence again
// with extra polls inserted between every pair of publications.

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// ---- the intended rule, as a reference implementation ---------------------

// modelGate reports the same three outcomes as the gate: start, stop, or
// neither. Its state is the evidence it has counted, not a poll history.
type modelGate struct {
	// since is when this process started: a record measured before it came
	// from disk, so it is baselined and never counted.
	since time.Time
	// running mirrors the extra transport.
	running bool
	// lastCheck and lastV6 are the stamps already consumed, so a repeated
	// event changes nothing.
	lastCheck time.Time
	lastV6    time.Time
	// standing is the newest measurement and the udp result it was published
	// with. The policy is read from these two.
	standing    proxyIPv6Result
	standingUDP proxyUDPStatus
	// starts counts the qualifying measurements of the current start streak
	// (two start the transport) and startExpiry is when the newest of them
	// stops being fresh.
	starts      int
	startExpiry time.Time
	// stops counts consecutive completed checks that disqualify, and stopOf is
	// the check that supplied the newest one, so one sweep adds at most one.
	stops  int
	stopOf time.Time
}

// qualifies is the policy: does this udp result and this measurement justify
// running the transport now.
func (m *modelGate) qualifies(udp proxyUDPStatus, v6 proxyIPv6Result, now time.Time) bool {
	if v6.Status != proxyIPv6Yes || v6.At.IsZero() || v6.Interval <= 0 {
		return false
	}
	if !now.Before(v6.At.Add(2 * v6.Interval)) {
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

// recordQualifies reports whether the measurement still stands on its own,
// which is what a udp check has to contradict to count a stop observation.
func (m *modelGate) recordQualifies(udp proxyUDPStatus, v6 proxyIPv6Result, now time.Time) bool {
	if v6.Status != proxyIPv6Yes || v6.At.IsZero() || v6.Interval <= 0 {
		return false
	}
	if !now.Before(v6.At.Add(2 * v6.Interval)) {
		return false
	}
	return v6.UDP == "yes" || (udp == proxyUDPNoRelay && v6.TCPExit != "")
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
func (m *modelGate) expiryStop(now time.Time) bool {
	if !m.running {
		return false
	}
	if m.standing.At.IsZero() || now.Before(m.standing.At.Add(2*m.standing.Interval)) {
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
	if m.qualifies(udp, v6, now) {
		m.stops = 0
		if measured {
			m.countStart(v6, now)
		}
		return
	}
	if measured || m.recordQualifies(udp, v6, now) {
		if checkAt != m.stopOf {
			m.stops++
			m.stopOf = checkAt
		}
		m.starts = 0
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

// settle is a poll: nothing new was published, but evidence can expire and a
// streak can decide.
func (m *modelGate) settle(now time.Time) (start bool, stop bool) {
	m.pruneStart(now)
	stop = m.expiryStop(now)
	start, again := m.decide(m.standingUDP, m.standing, now)
	return start, stop || again
}

// sweep is one completed check and the measurement it published.
func (m *modelGate) sweep(now time.Time, udp proxyUDPStatus, checkAt time.Time, v6 proxyIPv6Result) (start bool, stop bool) {
	m.pruneStart(now)
	stop = m.expiryStop(now)
	m.lastCheck, m.lastV6 = checkAt, v6.At
	m.standing, m.standingUDP = v6, udp
	m.judge(checkAt, udp, v6, true, now)
	start, again := m.decide(udp, v6, now)
	return start, stop || again
}

// udpOnly is a completed check whose sweep did not measure IPv6 again, so it is
// judged with the record that still stands.
func (m *modelGate) udpOnly(now time.Time, udp proxyUDPStatus, checkAt time.Time) (start bool, stop bool) {
	m.pruneStart(now)
	stop = m.expiryStop(now)
	m.lastCheck = checkAt
	m.standingUDP = udp
	m.judge(checkAt, udp, m.standing, false, now)
	start, again := m.decide(udp, m.standing, now)
	return start, stop || again
}

// measurement is a new measurement published on its own, without a check.
func (m *modelGate) measurement(now time.Time, v6 proxyIPv6Result) (start bool, stop bool) {
	m.pruneStart(now)
	stop = m.expiryStop(now)
	m.lastV6 = v6.At
	m.standing = v6
	m.judge(m.lastCheck, m.standingUDP, v6, true, now)
	start, again := m.decide(m.standingUDP, v6, now)
	return start, stop || again
}

// disable is the kill switch coming off: the transport stops, the streaks are
// dropped, and the consumed stamps stay so a cached result is not counted again.
func (m *modelGate) disable() (stop bool) {
	stop = m.running
	m.running = false
	m.starts = 0
	m.stops = 0
	return stop
}

// restart begins a new process with the measurement the store restored from
// disk. That record predates the new gate, so it is state, not an observation.
func (m *modelGate) restart(now time.Time, checkAt time.Time, udp proxyUDPStatus, v6 proxyIPv6Result) {
	*m = modelGate{since: now}
	if !v6.At.IsZero() {
		m.lastCheck, m.lastV6 = checkAt, v6.At
		m.standing, m.standingUDP = v6, udp
	}
}

// ---- the events the store and the controller produce -----------------------

type gateEventKind int

const (
	// eventSweep publishes a completed udp check and then its measurement.
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
	// advance is how far the clock moves before the event
	advance time.Duration
}

func (self gateEvent) String() string {
	switch self.kind {
	case eventSweep:
		return fmt.Sprintf("sweep(udp=%s, v6=%s/%s, interval=%s)", self.udp, self.v6.Status, self.v6.UDP, self.v6.Interval)
	case eventUDPCheck:
		return fmt.Sprintf("udp-check(udp=%s)", self.udp)
	case eventMeasurement:
		return fmt.Sprintf("measurement(v6=%s/%s, interval=%s)", self.v6.Status, self.v6.UDP, self.v6.Interval)
	case eventAdvance:
		return fmt.Sprintf("advance(%s)", self.advance)
	}
	return self.kind.String()
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

// generateSequence builds one random sequence. A publication while the switch
// is off is left out on purpose: the controller does not look at the store
// then, and the switch rule is pinned by its own controller test.
func generateSequence(r *rand.Rand) []gateEvent {
	count := 8 + r.Intn(16)
	events := make([]gateEvent, 0, count)
	disabled := false
	for len(events) < count {
		roll := r.Intn(100)
		var event gateEvent
		switch {
		case disabled && roll < 60:
			event = gateEvent{kind: eventPoll, advance: gateEventAdvances[r.Intn(len(gateEventAdvances))] / 8}
		case disabled:
			event = gateEvent{kind: eventEnable, advance: time.Minute}
			disabled = false
		case roll < 55:
			event = gateEvent{
				kind:    eventSweep,
				udp:     gateEventUDPStatuses[r.Intn(len(gateEventUDPStatuses))],
				v6:      randomMeasurement(r),
				advance: gateEventAdvances[r.Intn(len(gateEventAdvances))],
			}
		case roll < 63:
			event = gateEvent{kind: eventUDPCheck, udp: gateEventUDPStatuses[r.Intn(len(gateEventUDPStatuses))], advance: gateEventAdvances[r.Intn(3)]}
		case roll < 70:
			event = gateEvent{kind: eventMeasurement, v6: randomMeasurement(r), advance: gateEventAdvances[r.Intn(3)]}
		case roll < 85:
			event = gateEvent{kind: eventPoll, advance: gateEventAdvances[r.Intn(len(gateEventAdvances))]}
		case roll < 95:
			event = gateEvent{kind: eventAdvance, advance: gateEventAdvances[r.Intn(len(gateEventAdvances))]}
		case roll < 98:
			event = gateEvent{kind: eventRestart, advance: time.Minute}
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
	gate     proxyIPv6Gate
	model    *modelGate
	now      time.Time
	udp      proxyUDPResult
	v6       proxyIPv6Result
	disabled bool
}

// poll is the controller's step. A disabled switch stops the transport and
// clears the streaks while keeping the consumed stamps, so a cached result is
// not counted again when the switch comes back on.
func (r *gateRun) poll() (start bool, stop bool) {
	if r.disabled {
		stop = r.gate.running
		r.gate = proxyIPv6Gate{
			since:            r.gate.since,
			lastCheckedAt:    r.gate.lastCheckedAt,
			lastExpiry:       r.gate.lastExpiry,
			lastV6At:         r.gate.lastV6At,
			lastCountedCheck: r.gate.lastCountedCheck,
		}
		return false, stop
	}
	return r.gate.observe(r.now, r.udp, r.v6)
}

func (r *gateRun) restart() {
	r.gate = proxyIPv6Gate{since: r.now}
	r.model.restart(r.now, r.udp.CheckedAt, r.udp.Status, r.v6)
}

// replay drives one sequence. It returns one verdict per event and side. A
// sweep is published in two polls on the real side, with `between` extra polls
// inserted between its two publications.
func replay(events []gateEvent, between map[int]int, start time.Time) (real []verdict, model []verdict, err error) {
	r := &gateRun{gate: proxyIPv6Gate{since: start}, model: &modelGate{since: start}, now: start}
	real = make([]verdict, len(events))
	model = make([]verdict, len(events))
	for i, event := range events {
		r.now = r.now.Add(event.advance + time.Second)
		if r.disabled && event.kind != eventEnable && event.kind != eventDisable {
			// the controller does not read the store while the switch is off
			start, stop := r.poll()
			real[i] = verdict{start, stop}
			model[i] = verdictOf(r.model.settle(r.now))
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
			model[i] = verdictOf(r.model.settle(r.now))
		case eventRestart:
			r.restart()
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
			real[i] = verdictOf(r.poll())
			model[i] = verdictOf(r.model.measurement(r.now, event.v6))
		case eventSweep:
			checkAt := r.now
			// the store publishes the completed udp check first, with the
			// previous measurement still in place
			r.udp = proxyUDPResult{Status: event.udp, CheckedAt: checkAt}
			first := verdictOf(r.poll())
			for k := 0; k < between[i]; k++ {
				if extra := verdictOf(r.poll()); extra.Start || extra.Stop {
					return real, model, fmt.Errorf("event %d (%s): an inserted poll changed the gate: %+v", i, event, extra)
				}
			}
			event.v6.At = checkAt
			r.v6 = event.v6
			second := verdictOf(r.poll())
			if first.Start && second.Start || first.Stop && second.Stop {
				return real, model, fmt.Errorf("event %d (%s): one sweep made the same transition twice: %+v then %+v", i, event, first, second)
			}
			if (first.Start || second.Start) && (first.Stop || second.Stop) {
				return real, model, fmt.Errorf("event %d (%s): one sweep both started and stopped the transport: %+v then %+v", i, event, first, second)
			}
			real[i] = verdict{first.Start || second.Start, first.Stop || second.Stop}
			model[i] = verdictOf(r.model.sweep(r.now, event.udp, checkAt, event.v6))
		}
		if real[i] != model[i] {
			return real, model, fmt.Errorf("event %d (%s): the gate said %+v, the model said %+v", i, event, real[i], model[i])
		}
	}
	return real, model, nil
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
			name: "a start after a restart",
			events: withAdvance(
				qualifying,
				gateEvent{kind: eventRestart},
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
	}
}

type gateSequence struct {
	name   string
	events []gateEvent
}

// modelTestSequences is the number of random sequences the test replays.
const modelTestSequences = 2000

// modelTestSeed is the fixed base seed: every run replays the same sequences.
func modelTestSeed(index int) int64 { return int64(index)*7919 + 13 }

func TestProxyIPv6GateMatchesTheModelOnRandomSequences(t *testing.T) {
	start := time.Date(2026, 11, 21, 6, 0, 0, 0, time.UTC)
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

		real, _, err := replay(events, nil, start)
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
			if event.kind != eventSweep {
				continue
			}
			replayed, _, err := replay(events, map[int]int{i: 1}, start)
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
			if event.kind == eventSweep {
				all[i] = 2
			}
		}
		if len(all) > 0 {
			replayed, _, err := replay(events, all, start)
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
	t.Logf("replayed %d sequences: %d starts, %d stops", len(sequences), starts, stops)
}

func describeSequence(events []gateEvent) string {
	out := ""
	for i, event := range events {
		out += fmt.Sprintf("  %2d %s\n", i, event)
	}
	return out
}
