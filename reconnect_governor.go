package connect

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The reconnect governor, in SHADOW mode.
//
// During a backend episode where the API answers but slowly, thousands of
// proxies each retry the platform transport on their own backoff and nothing
// adds the attempts up. The governor is the process-wide view that does: it
// classifies every connect attempt, attributes real dials and bytes to it, and
// decides over a sliding window whether the backend is degraded.
//
// Shadow mode means it only OBSERVES. It reports what a shared attempt budget
// would have held back (`would_hold`) and logs a `[govern]` line, so the
// thresholds and the true cost of a failed attempt can be calibrated on the
// fleet before anything is allowed to delay a connect. Nothing here gates,
// delays or cancels an attempt.

// govWindow is how far back the verdict looks.
const govWindow = 60 * time.Second

// govMinAttempts is the fewest platform-class attempts in the window that can
// produce a degraded verdict, so a handful of failures is never an outage.
const govMinAttempts = 20

// govMinHardFailures is how many attempts in the window must have been visibly
// failed BY THE PLATFORM (a timeout after it answered, a rejected upgrade) for a
// degraded verdict. Sessions that die young with no such failure beside them are
// as likely to be proxies resetting connections as a struggling backend, and the
// verdict throttles every reconnect on the host.
const govMinHardFailures = 3

// govDegradedRatio: below this share of good platform attempts the window is
// degraded. A connect that dies young is not good.
const govDegradedRatio = 0.2

// govShadowBudgetPerSecond is the attempt rate the future budget would allow
// while degraded. Shadow mode only uses it to report what would be held.
const govShadowBudgetPerSecond = 2

// platformReachedBytes separates a timeout after hearing the platform from a
// timeout in silence. A TLS server hello with its certificate chain is several
// KB, so more than one packet back means the TLS peer answered. It is a
// heuristic, validated by the shadow data itself.
const platformReachedBytes = 1500

// govHeartbeatTicks is how many quiet one-minute ticks pass between healthy
// `[govern]` heartbeat lines.
const govHeartbeatTicks = 5

type connectClass uint8

const (
	// connectOK: the platform transport connected and authenticated
	connectOK connectClass = iota
	// connectSocksFail: the proxy path failed before the platform answered. A
	// property of the proxy, not the backend; excluded from the verdict.
	connectSocksFail
	connectTlsFail
	connectUpgradeFail
	// connectTimeoutPlatform: gave up after the platform answered
	connectTimeoutPlatform
	// connectTimeoutNoReply: gave up without hearing the platform
	connectTimeoutNoReply
	connectOtherFail
	// connectCanceled: local teardown, not an attempt outcome
	connectCanceled
	// connectRefused: the platform enforcing a rule (an explicit 401/403, a
	// client-limit close), not the backend failing; carries no weight either way
	connectRefused

	connectClassCount
)

func (self connectClass) String() string {
	switch self {
	case connectOK:
		return "ok"
	case connectSocksFail:
		return "socks"
	case connectTlsFail:
		return "tls"
	case connectUpgradeFail:
		return "upgrade"
	case connectTimeoutPlatform:
		return "timeout_platform"
	case connectTimeoutNoReply:
		return "timeout_silent"
	case connectOtherFail:
		return "other"
	case connectCanceled:
		return "canceled"
	case connectRefused:
		return "refused"
	}
	return fmt.Sprintf("class(%d)", uint8(self))
}

// attemptStats counts what ONE connect attempt cost: how many proxy dials it
// opened (the dial fan-out) and how many bytes crossed those connections,
// handshakes of attempts that fail included. It travels in the context, so the
// dial path needs no signature change.
type attemptStats struct {
	dials atomic.Int64
	rx    atomic.Int64
	tx    atomic.Int64
	// done is set once the attempt has finished. Bytes on its connection after
	// that are the session, not the handshake.
	done atomic.Bool
	// targets records which address each dialled hostname resolved to, so a
	// failed attempt can be charged to that address (see net_target_health.go)
	targets attemptTargets
}

type attemptStatsKey struct{}

func withAttemptStats(ctx context.Context, stats *attemptStats) context.Context {
	return context.WithValue(ctx, attemptStatsKey{}, stats)
}

func attemptStatsFrom(ctx context.Context) *attemptStats {
	stats, _ := ctx.Value(attemptStatsKey{}).(*attemptStats)
	return stats
}

// classifyConnectError buckets a connect error. stats may be nil.
func classifyConnectError(err error, stats *attemptStats) connectClass {
	if err == nil {
		return connectOK
	}
	if errors.Is(err, context.Canceled) {
		return connectCanceled
	}
	// a dial that gave up carries the real dial error; classify that, not the
	// timeout it ended as
	// the platform enforcing a rule says nothing about whether the backend works;
	// this is structural on purpose (see explicitRejection)
	if explicitRejection(err) || isClientLimitCloseError(err) {
		return connectRefused
	}
	var dialErr *dialTimeoutError
	if errors.As(err, &dialErr) && dialErr.last != nil {
		// a dial that got past the proxy is the evidence about the platform, even
		// if a later round's proxy path then failed
		if dialErr.platform != nil {
			return classifyConnectError(dialErr.platform, stats)
		}
		return classifyConnectError(dialErr.last, stats)
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "bad handshake"), strings.Contains(msg, "websocket:"):
		return connectUpgradeFail
	case strings.Contains(msg, "tls:"), strings.Contains(msg, "x509"), strings.Contains(msg, "certificate"):
		return connectTlsFail
	case isProxyLevelError(err):
		// A SOCKS handshake that failed, even by timing out, is the proxy's
		// failure. This must precede the timeout case: the attempt's byte count
		// sums every dial it made, so earlier dials can carry it past the
		// "platform answered" threshold, and a healthy backend then reads as
		// degraded just because the proxy list is poor.
		return connectSocksFail
	case strings.Contains(msg, "timeout"), isTimeoutError(err):
		if stats != nil && platformReachedBytes <= stats.rx.Load() {
			return connectTimeoutPlatform
		}
		return connectTimeoutNoReply
	case strings.Contains(msg, "socks"), strings.Contains(msg, "dial tcp"),
		strings.Contains(msg, "connection refused"), strings.Contains(msg, "no route"),
		strings.Contains(msg, "proxy"), strings.Contains(msg, "unreachable"),
		strings.Contains(msg, "no such host"):
		return connectSocksFail
	}
	return connectOtherFail
}

type govBucket struct {
	sec         int64
	attempts    int64
	ok          int64
	early       int64
	earlyProven int64
	classes     [connectClassCount]int64
	dials       int64
	bytes       int64
	failedBytes int64
}

// govSnapshot is the governor's view of the last govWindow.
type govSnapshot struct {
	Attempts int64
	OK       int64
	Early    int64
	// EarlyProven counts early deaths of proxies that had held a stable session
	EarlyProven      int64
	Classes          [connectClassCount]int64
	Dials            int64
	Bytes            int64
	FailedBytes      int64
	PlatformAttempts int64
	PlatformGood     int64
	// HardFailures counts attempts the platform visibly failed: timeouts after it
	// answered and rejected upgrades.
	HardFailures int64
	// Threshold is the good ratio below which this host's window reads as degraded
	Threshold float64
	// Baseline is the share of platform attempts this host usually sees go well, 0 until learned
	Baseline float64
	// StartupGrace is true while the governor is inside its startup grace and its verdict is not trusted
	StartupGrace bool
	Ratio        float64
	Degraded     bool
	WouldHold    int64
}

type connectGovernor struct {
	// baseline is the share of platform attempts this host usually sees go well,
	// learned from windows the verdict called healthy; 0 until learned
	baseline float64
	// baselinePath, when set, is where the baseline is kept across restarts
	baselinePath  string
	savedBaseline float64
	// started is when the governor was created, for the startup grace
	started time.Time
	// seedWindows counts, and seedMin keeps the lowest of, the first healthy
	// windows: one start burst must not seed the baseline
	seedWindows int
	seedMin     float64
	mu          sync.Mutex
	now         func() time.Time
	ring        [int(govWindow / time.Second)]govBucket

	// unproven tracks early deaths of not-yet-stable proxies (governor_unproven.go)
	unproven unprovenEarlyState
}

func newConnectGovernor(now func() time.Time) *connectGovernor {
	return &connectGovernor{now: now, started: now()}
}

// govStartupGrace is how long after the governor starts (a cold start, a hot swap
// or a restart) its verdict is not trusted. A start is a stampede: thousands of
// proxies dial at once and for a while most attempts fail on the host's own
// congestion, which is not the backend failing, and throttling it would slow the
// start the owner requires to stay fast. A restart that finds a fresh outage
// marker starts throttled on purpose and is not affected. Starting inside a real
// outage costs one unprotected minute of attempts, a one-time hundred megabytes,
// against a sustained leak the protection exists for.
const govStartupGrace = 90 * time.Second

// bucket returns the current second's bucket, zeroing it when it still holds an
// older lap of the ring. Caller holds mu.
func (self *connectGovernor) bucket() *govBucket {
	sec := self.now().Unix()
	b := &self.ring[int(sec%int64(len(self.ring)))]
	if b.sec != sec {
		*b = govBucket{sec: sec}
	}
	return b
}

// recordAttempt records one finished connect attempt. Local teardown is not an
// outcome and is dropped.
func (self *connectGovernor) recordAttempt(class connectClass, stats *attemptStats) {
	if stats != nil {
		stats.done.Store(true)
		recordConnectAttemptBytes(class, stats.rx.Load(), stats.tx.Load())
	}
	if class == connectCanceled {
		return
	}
	stats.recordTargetOutcome(class)
	self.mu.Lock()
	defer self.mu.Unlock()
	b := self.bucket()
	b.attempts += 1
	b.classes[class] += 1
	if class == connectOK {
		b.ok += 1
	}
	if stats != nil {
		bytes := stats.rx.Load() + stats.tx.Load()
		b.dials += stats.dials.Load()
		b.bytes += bytes
		if class != connectOK {
			b.failedBytes += bytes
		}
	}
}

// recordCanceledBytes charges the bytes a canceled attempt already burned to the
// window without counting it as an attempt: a cancel is local teardown, not an
// outcome, but the packets are on the wire all the same. The H3 gate cancels a
// lower-priority carrier whenever a better one is waiting, and each of those
// cancels arrives after most of a PTO ladder, so leaving them out hid about two
// thirds of a silent edge's QUIC egress. They add only to the failed-byte total
// the byte breaker reads; the breaker still needs most attempts to be failing.
func (self *connectGovernor) recordCanceledBytes(stats *attemptStats) {
	if stats == nil {
		return
	}
	bytes := stats.rx.Load() + stats.tx.Load()
	if bytes <= 0 {
		return
	}
	self.mu.Lock()
	defer self.mu.Unlock()
	b := self.bucket()
	b.dials += stats.dials.Load()
	b.bytes += bytes
	b.failedBytes += bytes
}

// recordEarlyDeath records that a connect which counted as OK lost its session
// young. Such a connect is a failure in all but name: it resets the backoff and
// the shared failure streak, and it is what the incident was made of.
func (self *connectGovernor) recordEarlyDeath() {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.bucket().early += 1
}

// recordProvenEarlyDeath is recordEarlyDeath for a proxy that has held a stable
// session before in this process. A proxy that never lived long may simply be a
// poor one, so its early death is ambiguous. One that did and now dies young is
// strong evidence that the platform is dropping connections it used to keep, and
// that is the incident's own shape (the platform accepts the websocket, then
// drops it), so it counts as a hard failure.
func (self *connectGovernor) recordProvenEarlyDeath() {
	self.mu.Lock()
	defer self.mu.Unlock()
	b := self.bucket()
	b.early += 1
	b.earlyProven += 1
}

// govBaselineFraction: a window is degraded when its good ratio falls below this
// fraction of the host's learned baseline (and below the plain threshold).
const govBaselineFraction = 0.4

// govMinThreshold: the degraded threshold never falls below this, however low
// the baseline, so a mislearned baseline cannot make the verdict blind.
const govMinThreshold = 0.08

// govBaselineAlphaUp and govBaselineAlphaDown are the weights of a healthy window
// that is better or worse than the baseline. Down is far slower: a backend that
// degrades over a quarter of an hour must not drag the baseline with it.
// govBaselineSeedWindows is how many qualifying windows seed the baseline.
const govBaselineSeedWindows = 3

// govBaselineMaxStep caps a window's ratio at this multiple of the current
// baseline when it is folded in.
const govBaselineMaxStep = 1.25

const (
	govBaselineAlphaUp   = 0.1
	govBaselineAlphaDown = 0.01
)

// learnBaseline folds the current window into the baseline if it was a healthy
// one. Call once a minute. The first window only seeds it if it is itself above
// the plain threshold: a host that starts in the middle of an outage must not
// learn that a collapsed ratio is normal.
func (self *connectGovernor) learnBaseline() {
	s := self.snapshot()
	if s.Degraded || s.PlatformAttempts < govMinAttempts {
		return
	}
	path, baseline, save := func() (string, float64, bool) {
		self.mu.Lock()
		defer self.mu.Unlock()
		switch {
		case self.baseline == 0:
			// Seeded from several windows, taking the lowest: one start burst of
			// clean connects must not leave the baseline far above the host's
			// normal, which would also raise the bar the budget needs to reopen.
			if s.StartupGrace || s.Ratio < govDegradedRatio {
				break
			}
			if self.seedWindows == 0 || s.Ratio < self.seedMin {
				self.seedMin = s.Ratio
			}
			self.seedWindows += 1
			if govBaselineSeedWindows <= self.seedWindows {
				self.baseline = self.seedMin
			}
		case self.baseline <= s.Ratio:
			// A single window may not pull the baseline far: a fleet-wide reconnect
			// burst (hundreds of clean connects in a minute after a platform-side
			// drop) says nothing about what this host's proxies usually do, and would
			// otherwise teach it that 90% good is normal.
			ratio := min(s.Ratio, govBaselineMaxStep*self.baseline)
			self.baseline = (1-govBaselineAlphaUp)*self.baseline + govBaselineAlphaUp*ratio
		case 0.5*self.baseline <= s.Ratio:
			self.baseline = (1-govBaselineAlphaDown)*self.baseline + govBaselineAlphaDown*s.Ratio
		}
		// written when it has moved enough to matter, never on every minute
		if self.baselinePath == "" || self.baseline == 0 || math.Abs(self.baseline-self.savedBaseline) < govBaselineSaveDelta {
			return "", 0, false
		}
		self.savedBaseline = self.baseline
		return self.baselinePath, self.baseline, true
	}()
	// the file write happens outside the lock
	if save {
		writeBaselineFile(path, baseline)
	}
}

// govBaselineSaveDelta is how far the baseline must move before it is written
// again, so a steady host does not touch the disk every minute.
const govBaselineSaveDelta = 0.02

// govBaselineMaxAge is how old a kept baseline may be and still be believed.
const govBaselineMaxAge = 7 * 24 * time.Hour

func writeBaselineFile(path string, baseline float64) {
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatFloat(baseline, 'f', 4, 64)+"\n"), 0o600); err == nil {
		_ = os.Rename(tmp, path)
	}
}

// saveBaseline writes the baseline now (used by tests and at shutdown paths).
func (self *connectGovernor) saveBaseline() {
	self.mu.Lock()
	path, baseline := self.baselinePath, self.baseline
	self.mu.Unlock()
	if path != "" && 0 < baseline {
		writeBaselineFile(path, baseline)
	}
}

// restoreBaseline reads the baseline a previous run kept, so a restart on a host
// with a poor proxy list does not start by judging it against the blunt plain
// threshold. A stale, unreadable or nonsensical file is ignored.
func (self *connectGovernor) restoreBaseline(path string, now time.Time) {
	self.mu.Lock()
	self.baselinePath = path
	self.mu.Unlock()
	info, err := os.Stat(path)
	if err != nil || govBaselineMaxAge <= now.Sub(info.ModTime()) {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
	if err != nil || v < govMinThreshold || 1 < v {
		return
	}
	self.mu.Lock()
	defer self.mu.Unlock()
	if self.baseline == 0 {
		self.baseline = v
		self.savedBaseline = v
	}
}

func (self *connectGovernor) snapshot() govSnapshot {
	return self.snapshotWindow(govWindow)
}

// snapshotWindow is the governor's view of the last `window` (at most govWindow,
// the ring's length). The verdict uses the full window; the attempt budget uses
// a short one so it reacts to recovery in seconds, not after a minute of
// failures has aged out.
func (self *connectGovernor) snapshotWindow(window time.Duration) govSnapshot {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.observeUnprovenLocked()

	var s govSnapshot
	seconds := min(int64(window/time.Second), int64(len(self.ring)))
	cutoff := self.now().Unix() - seconds
	for i := range self.ring {
		b := &self.ring[i]
		if b.sec <= cutoff {
			continue
		}
		s.Attempts += b.attempts
		s.OK += b.ok
		s.Early += b.early
		s.EarlyProven += b.earlyProven
		for c := range b.classes {
			s.Classes[c] += b.classes[c]
		}
		s.Dials += b.dials
		s.Bytes += b.bytes
		s.FailedBytes += b.failedBytes
	}

	// Only evidence that the PLATFORM answered counts against it: a timeout
	// after it replied, a rejected upgrade, and a connect whose session died
	// young. A proxy that hangs, refuses, or tampers with TLS fails the same way
	// whatever the backend does, so socks, silent-timeout, tls and unclassified
	// failures are ambiguous and carry no weight either way. Without that, a
	// poor proxy list on a healthy backend would read as an outage and the
	// governor would throttle the live proxies.
	s.PlatformAttempts = s.OK + s.Classes[connectTimeoutPlatform] + s.Classes[connectUpgradeFail]
	s.PlatformGood = max(0, s.OK-s.Early)
	// a connect whose session died young is as hard a failure as any: it is the
	// incident's own shape (the platform accepts the websocket, then drops it)
	s.HardFailures = s.Classes[connectTimeoutPlatform] + s.Classes[connectUpgradeFail] + s.EarlyProven
	if self.unprovenEarlyHardLocked() {
		s.HardFailures += max(0, s.Early-s.EarlyProven)
	}
	if 0 < s.PlatformAttempts {
		s.Ratio = float64(s.PlatformGood) / float64(s.PlatformAttempts)
	}
	// What counts as too low is relative to what this host normally sees: a poor
	// proxy list on a healthy backend sits at a low ratio all day, and an outage
	// is a collapse from that, not the level itself.
	threshold := govDegradedRatio
	if 0 < self.baseline {
		threshold = max(govMinThreshold, min(threshold, govBaselineFraction*self.baseline))
	}
	s.Threshold = threshold
	s.Baseline = self.baseline
	s.StartupGrace = self.now().Sub(self.started) < govStartupGrace
	s.Degraded = !s.StartupGrace && govMinAttempts <= s.PlatformAttempts && s.Ratio < threshold && govMinHardFailures <= s.HardFailures
	if s.Degraded {
		s.WouldHold = max(0, s.Attempts-govShadowBudgetPerSecond*int64(govWindow/time.Second))
	}
	return s
}

// govReporter turns the per-minute snapshots into `[govern]` lines: every
// transition and every degraded tick, plus a slow heartbeat while healthy.
type govReporter struct {
	degraded bool
	quiet    int

	// mode, rate and waiting describe the budget in the line. All optional: a
	// zero reporter reads as shadow mode with no budget fields.
	mode    func() governMode
	rate    func() float64
	waiting func() int
	// capRate is the per-host attempt cap, 0 while off; optional
	capRate func() float64
}

func (self *govReporter) currentMode() governMode {
	if self.mode == nil {
		return governShadow
	}
	return self.mode()
}

// degradedLabel says what the governor is actually doing about the verdict.
func (self *govReporter) degradedLabel() string {
	switch self.currentMode() {
	case governEnforce:
		return "DEGRADED (enforcing: connects run under a shared attempt budget)"
	case governOff:
		return "DEGRADED (governor off: not acting)"
	}
	return "DEGRADED (shadow: observing only)"
}

// budgetFields is the tail of every line: mode, and the budget when it is known.
func (self *govReporter) budgetFields() string {
	out := " mode=" + govModeName(self.currentMode())
	if self.rate != nil {
		out += fmt.Sprintf(" rate=%.1f/s", self.rate())
	}
	if self.waiting != nil {
		out += fmt.Sprintf(" waiting=%d", self.waiting())
	}
	if self.capRate != nil {
		if c := self.capRate(); 0 < c {
			out += fmt.Sprintf(" cap=%.1f/s", c)
		}
	}
	return out + governFeedFields()
}

func govModeName(mode governMode) string {
	switch mode {
	case governOff:
		return "off"
	case governEnforce:
		return "enforce"
	}
	return "shadow"
}

func (self *govReporter) next(s govSnapshot) (string, bool) {
	if s.Degraded {
		self.degraded = true
		self.quiet = 0
		return govLine(self.degradedLabel(), s) + self.budgetFields(), true
	}
	if self.degraded {
		self.degraded = false
		self.quiet = 0
		return govLine("recovered", s) + self.budgetFields(), true
	}
	if s.Attempts == 0 {
		return "", false
	}
	self.quiet += 1
	if govHeartbeatTicks <= self.quiet {
		self.quiet = 0
		return govLine("ok", s) + self.budgetFields(), true
	}
	return "", false
}

func govLine(state string, s govSnapshot) string {
	perAttemptDials, perAttemptKB := 0.0, 0.0
	if 0 < s.Attempts {
		perAttemptDials = float64(s.Dials) / float64(s.Attempts)
		perAttemptKB = float64(s.Bytes) / float64(s.Attempts) / 1024
	}
	line := fmt.Sprintf(
		"[govern] %s window=%ds attempts=%d (%.1f/s) connected=%d died_young=%d good=%.0f%% baseline=%.0f%% threshold=%.0f%% socks=%d tls=%d upgrade=%d timeout_platform=%d timeout_silent=%d other=%d refused=%d dials/attempt=%.1f KB/attempt=%.1f failed_KB=%d would_hold=%d",
		state, int(govWindow/time.Second), s.Attempts, float64(s.Attempts)/govWindow.Seconds(),
		s.OK, s.Early, 100*s.Ratio, 100*s.Baseline, 100*s.Threshold,
		s.Classes[connectSocksFail], s.Classes[connectTlsFail], s.Classes[connectUpgradeFail],
		s.Classes[connectTimeoutPlatform], s.Classes[connectTimeoutNoReply], s.Classes[connectOtherFail], s.Classes[connectRefused],
		perAttemptDials, perAttemptKB, s.FailedBytes/1024, s.WouldHold,
	)
	if s.StartupGrace {
		line += " startup_grace=yes"
	}
	return line
}

// connectGov is the process-wide governor. "Is the platform reachable from this
// host" is a property of the host, not of one proxy, so every transport feeds
// the same one.
var connectGov = newConnectGovernor(time.Now)

var govLoopOnce sync.Once

// startGovernorLoop starts the once-a-minute reporter on first use.
func startGovernorLoop() {
	govLoopOnce.Do(func() {
		go func() {
			reporter := &govReporter{
				mode:    currentGovernMode,
				rate:    connectBudget.currentRate,
				waiting: connectBudget.queued,
				capRate: connectBudget.currentCap,
			}
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for range ticker.C {
				connectGov.learnBaseline()
				if line, ok := reporter.next(connectGov.snapshot()); ok {
					DefaultLogger().Infof("%s\n", line)
				}
			}
		}()
	})
}

// GovernorStatus is the exported view of the governor for the health heartbeat.
type GovernorStatus struct {
	// Mode is "off", "shadow" or "enforce"
	Mode string
	// Verdict is "ok" or "degraded"
	Verdict  string
	Degraded bool
	// BudgetRate is the attempts per second connects are admitted under
	BudgetRate float64
	// Waiting is how many proxies are waiting their turn under the budget
	Waiting      int
	Attempts     int64
	Connected    int64
	DiedYoung    int64
	WouldHold    int64
	FlapBackoffs int64
}

func GovernorStatusNow() GovernorStatus {
	s := connectGov.snapshot()
	verdict := "ok"
	if s.Degraded {
		verdict = "degraded"
	}
	return GovernorStatus{
		Mode:         govModeName(currentGovernMode()),
		Verdict:      verdict,
		Degraded:     s.Degraded,
		BudgetRate:   connectBudget.currentRate(),
		Waiting:      connectBudget.queued(),
		Attempts:     s.Attempts,
		Connected:    s.OK,
		DiedYoung:    s.Early,
		WouldHold:    s.WouldHold,
		FlapBackoffs: TransportFlapBackoffs(),
	}
}
