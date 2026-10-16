package connect

import (
	"context"
	"sync"
	"testing"
	"time"
)

// seqHedgeRecorder is a fake dial evaluator for the sequential-dial hedge: it
// counts how many dialers were started, signals the first and second starts,
// and blocks every dial until the test releases it (no sleeps anywhere).
type seqHedgeRecorder struct {
	mu           sync.Mutex
	started      int
	first        chan struct{}
	second       chan struct{}
	release      chan struct{}
	succeedFirst bool
}

func newSeqHedgeRecorder(succeedFirst bool) *seqHedgeRecorder {
	return &seqHedgeRecorder{
		first:        make(chan struct{}),
		second:       make(chan struct{}),
		release:      make(chan struct{}),
		succeedFirst: succeedFirst,
	}
}

func (self *seqHedgeRecorder) count() int {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.started
}

func (self *seqHedgeRecorder) eval(ctx context.Context, dialer *clientDialer) *evalResult {
	self.mu.Lock()
	self.started += 1
	n := self.started
	self.mu.Unlock()
	if n == 1 {
		close(self.first)
	}
	if n == 2 {
		close(self.second)
	}
	if n == 1 && self.succeedFirst {
		return &evalResult{dialer: dialer}
	}
	select {
	case <-self.release:
		return nil
	case <-ctx.Done():
		return nil
	}
}

// withManualHedgeTimer swaps the hedge timer seam for one whose timers park
// for an hour and are handed to the test to fire when the assertions are ready.
func withManualHedgeTimer(t *testing.T) chan *time.Timer {
	t.Helper()
	timers := make(chan *time.Timer, 8)
	old := sequentialDialHedgeTimer
	sequentialDialHedgeTimer = func(d time.Duration) *time.Timer {
		timer := time.NewTimer(time.Hour)
		timers <- timer
		return timer
	}
	t.Cleanup(func() {
		sequentialDialHedgeTimer = old
	})
	return timers
}

func newSeqHedgeStrategy(t *testing.T, ctx context.Context, hedge time.Duration) *ClientStrategy {
	t.Helper()
	settings := DefaultClientStrategySettings()
	settings.SequentialDialHedge = hedge
	strategy := NewClientStrategy(ctx, settings)
	if len(strategy.dialers) < 2 {
		t.Fatalf("the strategy built %d dialers; the hedge test needs at least two", len(strategy.dialers))
	}
	return strategy
}

// With the hedge on, the first-choice dialer runs alone and the rest only start
// after the hedge fires while it is still trying.
func TestSequentialDialHedgeEscalatesAfterTheHedge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := newSeqHedgeStrategy(t, ctx, time.Hour)
	timers := withManualHedgeTimer(t)
	h := newSeqHedgeRecorder(false)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = strategy.parallelEvalErr(ctx, h.eval, false)
	}()

	select {
	case <-h.first:
	case <-time.After(5 * time.Second):
		t.Fatal("no dialer was started")
	}
	if got := h.count(); got != 1 {
		t.Fatalf("%d dialers raced before the hedge: the first choice must run alone", got)
	}
	var timer *time.Timer
	select {
	case timer = <-timers:
	case <-time.After(5 * time.Second):
		t.Fatal("the hedge timer was never created")
	}
	if got := h.count(); got != 1 {
		t.Fatalf("%d dialers raced while the hedge was armed", got)
	}
	timer.Reset(0)
	select {
	case <-h.second:
	case <-time.After(5 * time.Second):
		t.Fatal("no escalation after the hedge fired")
	}
	close(h.release)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the eval call never returned")
	}
}

// With the hedge off, the first-choice dialer runs to completion alone and the
// rest only start when it fails (the historical behavior, no timer is used).
func TestSequentialDialHedgeOffEscalatesOnFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := newSeqHedgeStrategy(t, ctx, 0)
	timers := withManualHedgeTimer(t)
	h := newSeqHedgeRecorder(false)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = strategy.parallelEvalErr(ctx, h.eval, false)
	}()

	select {
	case <-h.first:
	case <-time.After(5 * time.Second):
		t.Fatal("no dialer was started")
	}
	select {
	case <-timers:
		t.Fatal("the hedge timer was created with the hedge off")
	default:
	}
	if got := h.count(); got != 1 {
		t.Fatalf("%d dialers raced with the hedge off", got)
	}
	close(h.release)
	select {
	case <-h.second:
	case <-time.After(5 * time.Second):
		t.Fatal("no escalation after the first choice failed")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the eval call never returned")
	}
}

// When the first choice succeeds before the hedge, nothing else starts.
func TestSequentialDialFirstChoiceSuccessStopsEscalation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := newSeqHedgeStrategy(t, ctx, time.Hour)
	withManualHedgeTimer(t)
	h := newSeqHedgeRecorder(true)

	done := make(chan *evalResult, 1)
	go func() {
		result, _ := strategy.parallelEvalErr(ctx, h.eval, false)
		done <- result
	}()

	select {
	case result := <-done:
		if result == nil {
			t.Fatal("the successful first choice was not returned")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the eval call never returned")
	}
	if got := h.count(); got != 1 {
		t.Fatalf("%d dialers ran although the first choice succeeded", got)
	}
}
