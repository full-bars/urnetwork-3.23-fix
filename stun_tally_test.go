package connect

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// captureLogger renders and records each Infof so a test can assert on the
// exact [stun] line the aggregate would print.
type captureLogger struct{ lines []string }

func (l *captureLogger) Info(_ ...any) {}
func (l *captureLogger) Infof(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}
func (l *captureLogger) Warningf(format string, args ...any) {}
func (l *captureLogger) Errorf(format string, args ...any)   {}
func (l *captureLogger) V(_ int32) Verbose                   { return nopVerbose{} }

type nopVerbose struct{}

func (nopVerbose) Enabled() bool                 { return true }
func (nopVerbose) Info(_ ...any)                 {}
func (nopVerbose) Infof(format string, _ ...any) {}

func TestIsStunFailLine(t *testing.T) {
	cases := []struct {
		scope, msg string
		want       bool
	}{
		// The four real pion/ice v4.2.7 srflx-gathering failure strings.
		{"ice", "Failed to resolve STUN host: udp4 stun:foo:3478: no such host", true},
		{"ice", "failed to get server reflexive address udp4 stun:stun.l.google.com:19302: read udp: i/o timeout", true},
		{"ice", "failed to create server reflexive candidate: udp4 1.2.3.4 1234: boom", true},
		{"ice", "STUN host stun.foo:3478 is somehow filtered for location tracking reasons", true},
		// Non-failures and wrong scopes must not count.
		{"ice", "Setting new connection state: checking", false},
		{"ice", "Ping STUN from udp4 1.2.3.4:1111 to udp4 5.6.7.8:2222", false},
		{"transfer", "Failed to resolve STUN host: udp4 stun:foo:3478: nope", false},
	}
	for _, c := range cases {
		if got := isStunFailLine(c.scope, c.msg); got != c.want {
			t.Errorf("isStunFailLine(%q,%q)=%v, want %v", c.scope, c.msg, got, c.want)
		}
	}
}

// TestStunTallyAdaptiveInterval verifies that record() emits a single
// aggregate line on the adaptive cadence and picks the right next interval.
func TestStunTallyAdaptiveInterval(t *testing.T) {
	log := &captureLogger{}

	// Low-volume path: a completed 6-minute window with a single result
	// yields rate ~0.17/min (< highRateThreshold) -> relax to 5 minutes.
	low := &stunTallyT{
		lastLogAt: time.Now().Add(-6 * time.Minute),
		nextLogAt: time.Now().Add(-time.Second), // due now
	}
	low.record(false, log)
	if len(log.lines) != 1 {
		t.Fatalf("low volume: want 1 emit, got %d", len(log.lines))
	}
	if !strings.Contains(log.lines[0], "ok=0 fail=1") {
		t.Fatalf("low volume: unexpected line %q", log.lines[0])
	}
	if delta := time.Until(low.nextLogAt); delta < stunLowInterval-time.Second {
		t.Fatalf("low volume: nextLogAt=%v, want ~%v", delta, stunLowInterval)
	}

	// High-volume path: accumulate 100 results over a 6-minute window, then a
	// 101st due-pulse emits rate ~16.8/min (>= highRateThreshold) -> every
	// minute.
	high := &stunTallyT{
		lastLogAt: time.Now().Add(-6 * time.Minute),
		nextLogAt: time.Now().Add(50 * time.Second), // not yet due
	}
	for i := 0; i < 100; i++ {
		high.record(false, log)
	}
	high.nextLogAt = time.Now().Add(-time.Second) // force due
	high.record(false, log)
	if len(log.lines) != 2 {
		t.Fatalf("high volume: want 2 emits total, got %d", len(log.lines))
	}
	if !strings.Contains(log.lines[1], "fail=101") {
		t.Fatalf("high volume: unexpected line %q", log.lines[1])
	}
	if delta := time.Until(high.nextLogAt); delta < stunHighInterval-time.Second {
		t.Fatalf("high volume: nextLogAt=%v, want ~%v", delta, stunHighInterval)
	}
}

// TestStunTallyIdleNeverEmits confirms no line is produced while the window
// has not elapsed (no spam on an idle host).
func TestStunTallyIdleNeverEmits(t *testing.T) {
	log := &captureLogger{}
	tl := &stunTallyT{lastLogAt: time.Now()}
	// Two pulses inside the still-open first window must not emit.
	tl.record(true, log)
	tl.record(true, log)
	if len(log.lines) != 0 {
		t.Fatalf("idle window: want 0 emits, got %d", len(log.lines))
	}
}

// TestStunTallyFirstPulseSeedsWindow: a tally built long before its first pulse
// must not measure the whole idle period on its first emit. Seeding lastLogAt at
// construction did exactly that — a busy host reported ~0 events/min and then sat
// on the slow cadence for the rest of the run.
func TestStunTallyFirstPulseSeedsWindow(t *testing.T) {
	log := &captureLogger{}
	tl := &stunTallyT{lastLogAt: time.Now().Add(-2 * time.Hour)}
	tl.record(true, log)
	if time.Since(tl.lastLogAt) > time.Minute {
		t.Errorf("first pulse must re-seed lastLogAt to now, got %s ago", time.Since(tl.lastLogAt))
	}
	if tl.nextLogAt.IsZero() {
		t.Error("first pulse must arm nextLogAt")
	}
	if len(log.lines) != 0 {
		t.Errorf("first pulse must not emit, got %q", log.lines)
	}
}

// TestStunTallyDiscardsStaleWindow: counts left over from long ago are not this
// window's events — reporting them would resurrect a stale burst as fresh.
func TestStunTallyDiscardsStaleWindow(t *testing.T) {
	log := &captureLogger{}
	tl := &stunTallyT{
		lastLogAt: time.Now().Add(-2 * time.Hour),
		nextLogAt: time.Now().Add(-time.Minute),
		ok:        500,
		fail:      500,
	}
	tl.record(true, log)
	if len(log.lines) != 0 {
		t.Fatalf("stale window must not emit, got %q", log.lines)
	}
	if tl.ok != 0 || tl.fail != 0 {
		t.Errorf("stale counts must be dropped, got ok=%d fail=%d", tl.ok, tl.fail)
	}
}

// blockingLogger blocks inside Infof until released, so a test can prove the
// tally lock is not held across the log call.
type blockingLogger struct {
	entered chan struct{}
	release chan struct{}
}

func (l *blockingLogger) Info(...any)             {}
func (l *blockingLogger) Infof(string, ...any)    { close(l.entered); <-l.release }
func (l *blockingLogger) Warningf(string, ...any) {}
func (l *blockingLogger) Errorf(string, ...any)   {}
func (l *blockingLogger) V(int32) Verbose         { return nopVerbose{} }

// TestStunTallyDoesNotHoldLockDuringLog: a concurrent recorder must not block
// behind a log sink, i.e. the emit path unlocks before rendering and writing.
func TestStunTallyDoesNotHoldLockDuringLog(t *testing.T) {
	tl := &stunTallyT{
		lastLogAt: time.Now().Add(-time.Minute),
		nextLogAt: time.Now().Add(-time.Second),
		ok:        1,
	}
	log := &blockingLogger{entered: make(chan struct{}), release: make(chan struct{})}
	go tl.record(true, log)
	select {
	case <-log.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("emit never reached the logger")
	}

	// While the sink is blocked inside Infof, the lock must be free.
	done := make(chan struct{})
	go func() { tl.record(true, &captureLogger{}); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("tally lock held across the log call: concurrent record blocked")
	}
	close(log.release)
}

// quietVerbose / quietLogger model production levels: V(anything) is disabled.
type quietVerbose struct{}

func (quietVerbose) Enabled() bool        { return false }
func (quietVerbose) Info(...any)          {}
func (quietVerbose) Infof(string, ...any) {}

type quietLogger struct{}

func (quietLogger) Info(...any)             {}
func (quietLogger) Infof(string, ...any)    {}
func (quietLogger) Warningf(string, ...any) {}
func (quietLogger) Errorf(string, ...any)   {}
func (quietLogger) V(int32) Verbose         { return quietVerbose{} }

// TestPionDebugfCountsFailuresAtProductionLogLevel: STUN gather failures must
// reach the aggregate with verbose logging OFF — the production case. Gating the
// count behind V(2) meant DNS and srflx failures never counted at all.
func TestPionDebugfCountsFailuresAtProductionLogLevel(t *testing.T) {
	pl := &pionLeveledLogger{log: quietLogger{}, scope: "ice"}

	stunTally.mu.Lock()
	before := stunTally.fail
	stunTally.mu.Unlock()

	pl.Debugf("Failed to resolve STUN host: udp4 %s: no such host", "stun:foo:3478")

	stunTally.mu.Lock()
	after := stunTally.fail
	stunTally.mu.Unlock()
	if after != before+1 {
		t.Errorf("STUN gather failure at level 0 must count: fail %d -> %d", before, after)
	}
	StopStunProbe()
}

// TestStopStunProbeIsIdempotent: stopping the loop twice must not panic.
func TestStopStunProbeIsIdempotent(t *testing.T) {
	StopStunProbe()
	StopStunProbe()
}
