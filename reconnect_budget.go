package connect

import (
	"context"
	"fmt"
	"math"
	mathrand "math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The reconnect attempt budget.
//
// While the backend is failing, every disconnected proxy retries on its own
// backoff and nothing adds the attempts up, so N proxies produce N/60 full
// connect attempts per second. The budget is the missing sum. It is a single
// process-wide rate that platform-transport connects must be admitted under,
// and it only exists while the governor says attempts are failing:
//
//   - healthy backend: the rate sits at the ceiling with an empty queue, and
//     admit is a lock and two compares. Nothing waits, so startup, hot restarts
//     and ordinary reconnects run at full speed.
//   - failing backend: the rate is cut by budgetDecreaseFactor every control
//     interval down to a floor, so a few scouts keep probing and the rest of the
//     proxies wait without holding a socket.
//   - recovery: as soon as recent attempts connect, the rate doubles every
//     interval back to the ceiling, and the queue drains first-in first-out, so
//     thousands of parked proxies return in about a minute without a herd.
//
// The direct path is exempt, and connected transports never pass through here:
// only the start of a connect does.

const (
	// budgetFloorRate is the attempts per second let through while the backend
	// is failing: the scouts.
	budgetFloorRate = 2.0
	// budgetCeilingRate is where the budget opens again. It matches the ceiling
	// the proxy auth limiter already runs at, so recovery is no harsher on the
	// backend than ordinary startup.
	budgetCeilingRate = 200.0

	budgetDecreaseFactor = 4.0
	budgetIncreaseFactor = 2.0

	budgetControlInterval = 5 * time.Second
	budgetTick            = 50 * time.Millisecond

	// budgetRecentWindow is the span the controller judges. Short, so recovery
	// is seen in seconds.
	budgetRecentWindow = 10 * time.Second
	// fewer attempts than this in the window say nothing, in either direction
	budgetMinDecreaseAttempts = 10
	budgetMinIncreaseAttempts = 3
	// share of good attempts at which the budget starts opening
	budgetOpenRatio = 0.5

	// budgetFailedBytesCeilingPerSec bounds what failed connect attempts may burn
	// per second (about 2 Mbit/s). The classifier can be wrong about why attempts
	// fail; the bytes they spend are not an opinion. Failed attempts in ordinary
	// operation cost a few tens of KB/s, and dead proxies refuse in a few hundred
	// bytes, so this only trips on a real storm. Bytes of attempts that connected
	// are not counted, so a healthy startup is never throttled by it.
	budgetFailedBytesCeilingPerSec = 256 << 10

	// the byte breaker only acts when at least this many attempts failed in the
	// window and they are at least this share of all attempts
	budgetMinByteBreakerFailures = 20
	budgetByteBreakerShare       = 0.5
	// the rate is cut for failing attempts only when the platform visibly failed
	// at least this many in the recent window
	budgetMinHardFailures = 2

	// memory pressure holds reconnects down as a host problem, separately from the
	// backend verdict's rate: it must take this many consecutive pressured
	// samples (one every control tick), and never goes below a startup-safe rate
	budgetMemoryConsecutive = 3
	budgetMemoryFloorRate   = 20.0

	// outageMarkerMaxAge is how recent the previous run's outage marker must be
	// for a restart to start throttled.
	outageMarkerMaxAge = 10 * time.Minute
	// outageMarkerWriteEvery bounds how often a throttled budget touches the marker.
	outageMarkerWriteEvery = 30 * time.Second

	// budgetProbeHoldoff is how long after a cut for reason=bytes or
	// reason=verdict the "probe harder" reopen may not double the rate. Without
	// it, an outage whose attempts never reach the platform (silent timeouts,
	// TLS or SOCKS failures) reads as "no evidence" every interval, so a bytes cut
	// doubled straight back open within seconds and the budget oscillated around
	// the byte ceiling.
	budgetProbeHoldoff = 5 * time.Minute
	// inside the holdoff the no-evidence branch still probes, but only by
	// budgetProbeStep every budgetProbeRampEvery control intervals (+2/s per 30 s)
	budgetProbeRampEvery = 6
	budgetProbeStep      = budgetFloorRate
)

// A waiter that finds the dispatcher has not ticked for budgetStallGrace is let
// through: a dead dispatcher must never park proxies forever. Variables so tests
// can shorten them.
var (
	budgetStallCheckIntervalNs atomic.Int64
	budgetStallGraceNs         atomic.Int64
)

func init() {
	budgetStallCheckIntervalNs.Store(int64(5 * time.Second))
	budgetStallGraceNs.Store(int64(5 * time.Second))
}

// setBudgetStallForTest shortens the stall detector and returns a restore func.
// Atomic because waiter goroutines read them while a test restores them.
func setBudgetStallForTest(interval, grace time.Duration) func() {
	prevI, prevG := budgetStallCheckIntervalNs.Swap(int64(interval)), budgetStallGraceNs.Swap(int64(grace))
	return func() {
		budgetStallCheckIntervalNs.Store(prevI)
		budgetStallGraceNs.Store(prevG)
	}
}

type governMode int32

const (
	// governOff: the governor and budget do nothing. An emergency override only.
	governOff governMode = iota
	// governShadow: observe and report only, never delay a connect. An override
	// for debugging and rollback, not a default.
	governShadow
	// governEnforce: the budget gates connects and a failing backend gets one
	// dial round per attempt. This is the default: the protection is on for
	// every provider unless an operator turns it down.
	governEnforce
)

var governModeValue atomic.Int32

func init() { governModeValue.Store(int32(parseGovernMode(os.Getenv("URNETWORK_GOVERN")))) }

// parseGovernMode reads URNETWORK_GOVERN. Unset, empty and unrecognised values
// all mean enforce: a typo must never silently disable the protection. Only an
// explicit off or shadow turns it down.
func parseGovernMode(raw string) governMode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "off", "0", "false", "no":
		return governOff
	case "shadow":
		return governShadow
	}
	return governEnforce
}

func currentGovernMode() governMode { return governMode(governModeValue.Load()) }

// setGovernModeForTest swaps the mode and returns a restore func.
func setGovernModeForTest(mode governMode) func() {
	prev := governModeValue.Swap(int32(mode))
	return func() { governModeValue.Store(prev) }
}

// scoutRandomEvery makes every Nth released token go to a random waiter and the
// rest to the waiter with the best record. Best-first brings the proxies that
// earned before the outage back soonest and probes with the likeliest to work;
// the random slot means a stale favourite, or a proxy with no history yet, can
// never hold everyone else out.
const scoutRandomEvery = 3

type admitWaiter struct {
	ch       chan struct{}
	score    uint64
	canceled atomic.Bool
}

type reconnectBudget struct {
	gov *connectGovernor
	now func() time.Time

	// lastOutage is the unix nanos of the last control tick that found the
	// budget throttled
	lastOutage atomic.Int64

	// markerPath, when set, is a file that exists while the budget is throttled so
	// a restart in the middle of an outage starts throttled instead of stampeding
	markerPath      string
	lastMarkerWrite time.Time
	// ioProbe, when set, is called at the start of every marker file operation.
	// For tests that check no lock is held across file I/O.
	ioProbe func()

	// pick chooses the random slot's waiter; replaceable so tests are exact
	pick func(n int) int

	// memoryPressure reports whether the host or the Go heap is critically short
	// of memory; replaceable so tests are exact
	memoryPressure func() bool

	// emit writes one log line; replaceable so tests can read what an operator would
	emit func(string)

	mu   sync.Mutex
	rate float64
	// memRate is the cap memory pressure puts on the dispatch rate; it is kept
	// apart from rate so that pressure never reads as a backend outage
	memRate      float64
	memPressured int
	// throttledSince is when the backend rate first fell below the ceiling, zero while open
	throttledSince time.Time
	// memHeld is true while memory pressure is holding the dispatch rate down
	memHeld  bool
	carry    float64
	lastTick time.Time
	// side effects of control decisions, run off the dispatcher goroutine
	side        chan func()
	sideOnce    sync.Once
	sideDropped atomic.Int64
	// markerResync is set when a side effect was dropped: the next decision
	// re-applies the marker file state (write while throttled, remove otherwise)
	// without waiting for the periodic refresh. Guarded by mu.
	markerResync bool
	// emitAt, when set, is used instead of emit for the budget's own decisions so
	// a line keeps the time the decision was made even if its worker ran late.
	emitAt     func(line string, at time.Time)
	queue      []*admitWaiter
	dispatched int

	// lastCut is when the rate was last cut for reason=bytes or reason=verdict,
	// and probeIntervals counts no-evidence control intervals since the last
	// bounded probe step (see budgetProbeHoldoff)
	lastCut        time.Time
	probeIntervals int

	// attemptCap holds connects down when almost every attempt fails in classes
	// the verdict gives no weight (see reconnect_attempt_cap.go)
	attemptCap attemptCapState
}

func newReconnectBudget(gov *connectGovernor, now func() time.Time) *reconnectBudget {
	b := &reconnectBudget{gov: gov, now: now, rate: budgetCeilingRate, memRate: budgetCeilingRate, lastTick: now(), pick: mathrand.Intn, memoryPressure: readMemoryPressure, emit: emitGovernLine, emitAt: emitGovernLineAt}
	b.attemptCap.proxyCount = proxiedCount
	return b
}

func (self *reconnectBudget) currentRate() float64 {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.rate
}

// effectiveRate is the rate the dispatcher actually releases at: the backend
// verdict's rate, held down further if memory is short.
func (self *reconnectBudget) effectiveRate() float64 {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.effectiveRateLocked()
}

func (self *reconnectBudget) effectiveRateLocked() float64 {
	rate := min(self.rate, self.memRate)
	if 0 < self.attemptCap.rate {
		rate = min(rate, self.attemptCap.rate)
	}
	return rate
}

func (self *reconnectBudget) setRate(rate float64) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.rate = min(max(rate, budgetFloorRate), budgetCeilingRate)
}

// setPick replaces the random slot's chooser and returns a restore func. For
// tests that need the dispatcher to be deterministic.
func (self *reconnectBudget) setPick(fn func(n int) int) func() {
	self.mu.Lock()
	defer self.mu.Unlock()
	prev := self.pick
	self.pick = fn
	return func() {
		self.mu.Lock()
		defer self.mu.Unlock()
		self.pick = prev
	}
}

func (self *reconnectBudget) queued() int {
	self.mu.Lock()
	defer self.mu.Unlock()
	return len(self.queue)
}

// throttled reports whether the budget is holding anything back: a reduced rate,
// or waiters still draining after the rate recovered.
func (self *reconnectBudget) throttled() bool {
	self.mu.Lock()
	defer self.mu.Unlock()
	// The backend verdict's rate only. A queue held back by memory pressure is a
	// host problem and must not read as a backend outage (it would freeze the
	// reaper, ignore real renewal refusals and write the restart marker).
	return self.rate < budgetCeilingRate
}

// admit blocks until a connect may start. It returns false only when ctx ends
// first. The direct path never waits.
func (self *reconnectBudget) admit(ctx context.Context, direct bool) bool {
	return self.admitParked(ctx, direct, nil, nil)
}

// admitParked is admit that ranks the caller by score (evaluated only if it has
// to wait, so the healthy path never pays for it) and reports to onPark when it
// starts and stops waiting, so the proxy can be shown as parked rather than
// broken. Both may be nil.
func (self *reconnectBudget) admitParked(ctx context.Context, direct bool, score func() uint64, onPark func(parked bool)) bool {
	if direct {
		return true
	}
	self.mu.Lock()
	if self.effectiveRateLocked() >= budgetCeilingRate && len(self.queue) == 0 {
		self.mu.Unlock()
		return true
	}
	self.mu.Unlock()

	// Scored only now that we know we must wait, so the healthy path never pays
	// for it, and BEFORE joining the queue: the dispatcher reads the score under
	// the lock, so it must never be written after the waiter is visible. The
	// scorer takes its own locks, so it cannot run under ours.
	var waiterScore uint64
	if score != nil {
		waiterScore = score()
	}

	self.mu.Lock()
	if self.effectiveRateLocked() >= budgetCeilingRate && len(self.queue) == 0 {
		// the budget reopened while we were scoring
		self.mu.Unlock()
		return true
	}
	w := &admitWaiter{ch: make(chan struct{}), score: waiterScore}
	self.queue = append(self.queue, w)
	self.mu.Unlock()

	if onPark != nil {
		onPark(true)
		defer onPark(false)
	}

	stall := time.NewTicker(time.Duration(budgetStallCheckIntervalNs.Load()))
	defer stall.Stop()
	for {
		select {
		case <-w.ch:
			return true
		case <-ctx.Done():
			w.canceled.Store(true)
			return false
		case <-stall.C:
			if self.stalled() {
				// the dispatcher is not running: fail open, and drop the
				// queue entry so it does not take a later token
				w.canceled.Store(true)
				return true
			}
		}
	}
}

// stalled reports whether the dispatcher has missed its ticks.
func (self *reconnectBudget) stalled() bool {
	self.mu.Lock()
	defer self.mu.Unlock()
	return time.Duration(budgetStallGraceNs.Load()) < self.now().Sub(self.lastTick)
}

// tick releases the waiters the elapsed time has earned, oldest first. A
// canceled waiter earns nothing and is dropped without using a token.
func (self *reconnectBudget) tick(now time.Time) {
	self.mu.Lock()
	defer self.mu.Unlock()

	dt := now.Sub(self.lastTick)
	self.lastTick = now
	if dt <= 0 {
		return
	}
	// a long gap (the loop was not running, the host slept) must not mint a
	// burst of tokens
	dt = min(dt, time.Second)

	self.carry += self.effectiveRateLocked() * dt.Seconds()
	// the epsilon absorbs float drift: twenty ticks of 0.1 token sum to
	// 1.9999999999999998, which must still release two
	n := int(math.Floor(self.carry + 1e-9))
	self.carry -= float64(n)

	// canceled waiters earn nothing and are dropped without using a token
	live := self.queue[:0]
	for _, w := range self.queue {
		if !w.canceled.Load() {
			live = append(live, w)
		}
	}
	clear(self.queue[len(live):])
	self.queue = live

	released := 0
	for released < n && 0 < len(self.queue) {
		i := self.nextLocked()
		w := self.queue[i]
		self.queue = append(self.queue[:i], self.queue[i+1:]...)
		close(w.ch)
		released += 1
		self.dispatched += 1
	}
	if len(self.queue) == 0 {
		// unspent time is not banked: an idle budget must not release a burst
		self.carry = 0
	}
}

// nextLocked picks the queue index to release next: the best score, earliest
// arrival on ties (so equal scores stay first-in first-out), except that every
// scoutRandomEvery-th token goes to a random waiter. Caller holds mu and the
// queue is not empty.
func (self *reconnectBudget) nextLocked() int {
	if self.dispatched%scoutRandomEvery == scoutRandomEvery-1 {
		return min(max(self.pick(len(self.queue)), 0), len(self.queue)-1)
	}
	best := 0
	for i, w := range self.queue {
		if self.queue[best].score < w.score {
			best = i
		}
	}
	return best
}

// control adjusts the rate from what recent attempts did. Cut hard on evidence
// of failure, open fast on evidence of success, hold in between.
func (self *reconnectBudget) control() {
	self.controlWith(runSideEffectNow)
}

// runSideEffectNow runs a side effect on the calling goroutine.
func runSideEffectNow(f func()) bool {
	f()
	return true
}

// controlWith is control with the caller's choice of where the decision's side
// effects (the [govern] log lines and the outage marker file) run. The dispatcher
// goroutine uses enqueueSide: a log write or a marker write that blocks on a
// stalled disk must not stop it ticking, because a stale tick makes every parked
// waiter look stalled and releases them all, and the budget is cut at exactly the
// moment the backend is failing.
func (self *reconnectBudget) controlWith(run func(func()) bool) {
	recent := self.gov.snapshotWindow(budgetRecentWindow)
	// the marker's file operation runs after the lock is released
	if after := self.adjust(recent); after != nil {
		if !run(after) {
			// the effect was dropped (the worker's queue is full). Log lines may be
			// lost, but the marker file must end up true to the budget, so the next
			// decision re-applies whatever the marker should be.
			self.mu.Lock()
			self.markerResync = true
			self.mu.Unlock()
		}
	}
	self.controlCapWith(recent, run)
}

// budgetSideQueueLen bounds the side effects waiting for the worker. Decisions are
// a few per control interval, so it only fills when the disk has been stuck for a
// long time.
const budgetSideQueueLen = 256

// enqueueSide hands a side effect to the single worker without ever blocking. A
// full queue drops it and counts: the marker write is repeated every
// outageMarkerWriteEvery while throttled, and a dropped log line is better than a
// stalled dispatcher.
func (self *reconnectBudget) enqueueSide(f func()) bool {
	self.sideOnce.Do(func() {
		self.side = make(chan func(), budgetSideQueueLen)
		go func() {
			for f := range self.side {
				f()
			}
		}()
	})
	select {
	case self.side <- f:
		return true
	default:
		self.sideDropped.Add(1)
		return false
	}
}

// adjust applies one control decision under the lock and returns the marker file
// operation to run once it is released, or nil.
func (self *reconnectBudget) adjust(recent govSnapshot) func() {
	// read before the lock: it touches the runtime and /proc
	pressured := self.memoryPressure != nil && self.memoryPressure()
	self.mu.Lock()
	defer self.mu.Unlock()

	var events []string
	rateBefore := self.rate

	// Every reconnect attempt costs memory, and a box that is out of it dies and
	// restarts into the same storm. Held down regardless of the backend's health,
	// but as a host problem: it never touches rate, lastOutage or the marker.
	if pressured {
		self.memPressured += 1
	} else {
		self.memPressured = 0
	}
	switch {
	case budgetMemoryConsecutive <= self.memPressured:
		self.memRate = max(budgetMemoryFloorRate, self.memRate/budgetDecreaseFactor)
		if !self.memHeld {
			self.memHeld = true
			events = append(events, "[govern] memory pressure: holding reconnects to at most "+rateString(self.memRate)+" until memory frees up (this is the host, not the backend)")
		}
	case !pressured:
		self.memRate = min(budgetCeilingRate, self.memRate*budgetIncreaseFactor)
		if self.memHeld && self.memRate >= budgetCeilingRate {
			self.memHeld = false
			events = append(events, "[govern] memory pressure over: reconnects back to full speed")
		}
	}

	failedBytesPerSec := float64(recent.FailedBytes) / budgetRecentWindow.Seconds()
	reason := ""
	failedAttempts := recent.Attempts - recent.OK
	mostlyFailing := budgetMinByteBreakerFailures <= failedAttempts && budgetByteBreakerShare*float64(recent.Attempts) <= float64(failedAttempts)
	// inside the startup grace nothing is cut: a start is a stampede whose failures
	// are the host's own congestion, not the backend (see govStartupGrace)
	grace := recent.StartupGrace && self.rate >= budgetCeilingRate
	switch {
	case grace:
	case failedBytesPerSec > budgetFailedBytesCeilingPerSec && mostlyFailing:
		reason = "bytes"
		// whatever the verdict says, failed attempts are burning the pipe. It needs
		// most attempts to be failing: a healthy startup in which a minority of
		// proxies fail after the TLS handshake spends real bytes on them and is not
		// a flood.
		self.rate = max(budgetFloorRate, self.rate/budgetDecreaseFactor)
		self.noteCutLocked()
	case budgetMinDecreaseAttempts <= recent.PlatformAttempts && recent.Ratio < recent.Threshold &&
		budgetMinHardFailures <= recent.HardFailures:
		reason = "verdict"
		self.rate = max(budgetFloorRate, self.rate/budgetDecreaseFactor)
		self.noteCutLocked()
	case self.rate < budgetCeilingRate &&
		(budgetMinIncreaseAttempts <= recent.PlatformAttempts && openRatio(recent) <= recent.Ratio):
		self.rate = min(budgetCeilingRate, self.rate*budgetIncreaseFactor)
	case self.rate < budgetCeilingRate && recent.PlatformAttempts < budgetMinIncreaseAttempts:
		// Too few attempts reached the platform to say anything. Either nothing
		// is trying, or the scouts the budget released landed on dead proxies.
		// A reduced rate must not be held on no evidence, so probe harder:
		// an uninformative attempt is cheap, it never reached the platform.
		// Right after a bytes or verdict cut, though, "no evidence" is what an
		// outage the classifier cannot see looks like, so the probe only creeps.
		self.probeLocked()
	}

	if self.rate < budgetCeilingRate {
		self.lastOutage.Store(self.now().UnixNano())
		if self.throttledSince.IsZero() {
			self.throttledSince = self.now()
		}
	}
	if self.rate < rateBefore && reason != "" {
		events = append(events, fmt.Sprintf(
			"[govern] budget cut from %s to %s reason=%s good=%.0f%% threshold=%.0f%% hard_failures=%d platform_attempts=%d failed_KB/s=%.0f",
			rateString(rateBefore), rateString(self.rate), reason, 100*recent.Ratio, 100*recent.Threshold,
			recent.HardFailures, recent.PlatformAttempts, failedBytesPerSec/1024))
	}
	if rateBefore < budgetCeilingRate && budgetCeilingRate <= self.rate && !self.throttledSince.IsZero() {
		events = append(events, fmt.Sprintf("[govern] budget reopened to %s after being throttled for %s",
			rateString(self.rate), self.now().Sub(self.throttledSince).Round(time.Second)))
		self.throttledSince = time.Time{}
	}
	markerOp := self.planOutageMarkerLocked()
	if len(events) == 0 {
		return markerOp
	}
	emit, at := self.emit, self.now()
	return func() {
		if emit != nil {
			for _, line := range events {
				self.emitDecision(emit, line, at)
			}
		}
		if markerOp != nil {
			markerOp()
		}
	}
}

// noteCutLocked records a cut for reason=bytes or reason=verdict. Caller holds mu.
func (self *reconnectBudget) noteCutLocked() {
	self.lastCut = self.now()
	self.probeIntervals = 0
}

// probeLocked is the no-evidence reopen: double the rate, unless the budget was
// cut on evidence within budgetProbeHoldoff, in which case add budgetProbeStep
// once every budgetProbeRampEvery intervals. Caller holds mu.
func (self *reconnectBudget) probeLocked() {
	if !self.lastCut.IsZero() && self.now().Sub(self.lastCut) < budgetProbeHoldoff {
		self.probeIntervals += 1
		if budgetProbeRampEvery <= self.probeIntervals {
			self.probeIntervals = 0
			self.rate = min(budgetCeilingRate, self.rate+budgetProbeStep)
		}
		return
	}
	self.probeIntervals = 0
	self.rate = min(budgetCeilingRate, self.rate*budgetIncreaseFactor)
}

func rateString(rate float64) string { return fmt.Sprintf("%.1f/s", rate) }

// governEventSink, when set, also receives every [govern] event line so the
// embedding program can keep a durable record of enforcement (the ramlogs are
// RAM and roll over within the hour). It must not block, and it is called from
// emit sites that may hold the budget lock, so it must not call back into the
// governor.
var governEventSink atomic.Pointer[func(GovernEvent)]

// SetGovernEventSink installs the durable sink for [govern] event lines; nil
// removes it.
func SetGovernEventSink(sink func(GovernEvent)) {
	if sink == nil {
		governEventSink.Store(nil)
		return
	}
	governEventSink.Store(&sink)
}

// emitGovernLine writes a [govern] line to the log (the provider's important log
// picks up every line that starts with it) and hands it to the event sink.
func emitGovernLine(line string) { emitGovernLineAt(line, time.Now()) }

// emitGovernLineAt is emitGovernLine for a decision made at the given time, which
// the event keeps even if this call runs late (a worker behind a stalled disk).
func emitGovernLineAt(line string, at time.Time) {
	DefaultLogger().Infof("%s\n", line)
	if sink := governEventSink.Load(); sink != nil {
		kind, mode := ClassifyGovernLine(line)
		(*sink)(GovernEvent{Kind: kind, Mode: mode, Line: line, At: at})
	}
}

// emitDecision emits one of the budget's lines with the time of its decision.
func (self *reconnectBudget) emitDecision(emit func(string), line string, at time.Time) {
	if self.emitAt != nil {
		self.emitAt(line, at)
		return
	}
	emit(line)
}

// writeOutageMarker leaves the marker file. Errors are ignored: the marker is a
// courtesy to the next run, never a reason to fail.
func (self *reconnectBudget) writeOutageMarker(path string) {
	if self.ioProbe != nil {
		self.ioProbe()
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, []byte(self.now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

func (self *reconnectBudget) removeOutageMarker(path string) {
	if self.ioProbe != nil {
		self.ioProbe()
	}
	_ = os.Remove(path)
}

// planOutageMarkerLocked keeps the marker true to the budget: present (and fresh)
// while throttled, gone once it has reopened. It only DECIDES under the lock and
// returns the file operation to run after the lock is released, so a slow disk
// never stalls anything waiting on the budget. Caller holds mu.
func (self *reconnectBudget) planOutageMarkerLocked() func() {
	if self.markerPath == "" {
		return nil
	}
	path := self.markerPath
	// a dropped side effect asks for the marker to be re-applied now
	resync := self.markerResync
	self.markerResync = false
	if self.rate < budgetCeilingRate {
		if resync || outageMarkerWriteEvery <= self.now().Sub(self.lastMarkerWrite) {
			self.lastMarkerWrite = self.now()
			return func() { self.writeOutageMarker(path) }
		}
		return nil
	}
	if !self.lastMarkerWrite.IsZero() || resync {
		self.lastMarkerWrite = time.Time{}
		return func() { self.removeOutageMarker(path) }
	}
	return nil
}

// restoreOutageMarker reads the previous run's marker. A recent one means the
// backend was failing when that run ended, so this one starts at the floor and
// earns its way up on evidence; an absent or old one starts open, so a healthy
// restart or hot swap pays nothing.
func (self *reconnectBudget) restoreOutageMarker(path string, now time.Time) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.markerPath = path
	info, err := os.Stat(path)
	if err != nil || outageMarkerMaxAge <= now.Sub(info.ModTime()) {
		return
	}
	self.rate = budgetFloorRate
	self.lastOutage.Store(info.ModTime().UnixNano())
	self.lastMarkerWrite = now
	self.throttledSince = now
	if self.emit != nil {
		self.emit(fmt.Sprintf("[govern] restart marker found (the last run ended %s into an outage): starting at %s and ramping on evidence",
			now.Sub(info.ModTime()).Round(time.Second), rateString(budgetFloorRate)))
	}
}

// outageRecent reports whether the budget is throttled now or was within d. A
// proxy that has been down since an outage may only be called dead after a
// quiet period, not on the day the backend comes back.
func (self *reconnectBudget) outageRecent(d time.Duration) bool {
	if self.throttled() {
		return true
	}
	last := self.lastOutage.Load()
	return last != 0 && self.now().Sub(time.Unix(0, last)) < d
}

func (self *reconnectBudget) run(ctx context.Context) {
	tick := time.NewTicker(budgetTick)
	defer tick.Stop()
	control := time.NewTicker(budgetControlInterval)
	defer control.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			self.tick(now)
		case <-control.C:
			self.controlWith(self.enqueueSide)
		}
	}
}

// connectBudget is the process-wide budget.
var connectBudget = newReconnectBudget(connectGov, time.Now)

var budgetLoopOnce sync.Once

// admitConnect gates the start of a platform-transport connect. In any mode but
// enforce it admits immediately.
func admitConnect(ctx context.Context, direct bool, proxyIndex int) bool {
	if currentGovernMode() != governEnforce {
		return true
	}
	budgetLoopOnce.Do(func() { go connectBudget.run(context.Background()) })
	return connectBudget.admitParked(
		ctx,
		direct,
		func() uint64 { return proxyScoutScore(proxyIndex) },
		func(parked bool) { markProxyParked(proxyIndex, parked) },
	)
}

// OutageRecent reports whether a backend outage is under way or ended within d,
// as seen by the reconnect attempt budget. The provider's reapers use it to hold
// off: a proxy that has been down since the outage is not dead until a long
// quiet period says so. Always false unless the governor is enforcing.
func OutageRecent(d time.Duration) bool {
	if currentGovernMode() != governEnforce {
		return false
	}
	return connectBudget.outageRecent(d)
}

// singleRoundCheck is replaceable so tests can force either answer.
var singleRoundCheck = singleRoundWhenDegraded

// singleRoundWhenDegraded reports whether a failed dial should give up after
// one pass over the dialers instead of retrying them until the request deadline.
// Retrying is what makes one failed attempt cost many dials; while the backend
// is failing the next round fails too.
func singleRoundWhenDegraded() bool {
	return currentGovernMode() == governEnforce && connectGov.snapshot().Degraded
}

// backendOutageGrace is how long after the budget last found itself throttled the
// backend still counts as degraded for the purposes of destructive decisions.
const backendOutageGrace = 10 * time.Minute

// backendDegradedFrom is the pure form of BackendDegraded: the verdict is
// failing now, or the budget was throttled within the grace period.
func backendDegradedFrom(gov *connectGovernor, budget *reconnectBudget) bool {
	return gov.snapshot().Degraded || budget.outageRecent(backendOutageGrace)
}

// backendDegradedNow is replaceable so tests are not at the mercy of the
// process-wide governor other tests feed.
var backendDegradedNow = func() bool {
	if currentGovernMode() == governOff {
		return false
	}
	return backendDegradedFrom(connectGov, connectBudget)
}

// BackendDegraded reports whether the platform backend is failing right now or
// was within the last few minutes. Anything destructive that an outage could
// trigger by mistake, such as evicting a saved client identity, must check it
// first. Unlike OutageRecent it also works in shadow mode, because it only
// reads what the governor observed.
func BackendDegraded() bool { return backendDegradedNow() }

// InitGovernState tells the governor where to keep its learned state and reads
// the previous run's. Call once at startup. It does nothing when the governor is
// off. Shadow restores and keeps the baseline too, so its verdict shows what
// enforce would decide; the outage marker changes behaviour, so only enforce
// restores that.
func InitGovernState(markerPath string) {
	mode := currentGovernMode()
	if mode == governOff {
		return
	}
	if mode == governEnforce {
		connectBudget.restoreOutageMarker(markerPath, time.Now())
	}
	// the learned baseline lives beside the marker
	connectGov.restoreBaseline(filepath.Join(filepath.Dir(markerPath), "govern_baseline"), time.Now())
}

// openRatio is the share of good platform attempts at which a throttled budget
// opens. With nothing learned it is the fixed budgetOpenRatio. Otherwise it is
// relative to what this host normally sees, never below the degraded threshold:
// a host whose normal is 30% good would otherwise sit at the floor for ever after
// one outage, because the open bar would be more than it ever achieves.
func openRatio(recent govSnapshot) float64 {
	if recent.Baseline <= 0 {
		return budgetOpenRatio
	}
	return max(recent.Threshold, min(budgetOpenRatio, 0.8*recent.Baseline))
}
