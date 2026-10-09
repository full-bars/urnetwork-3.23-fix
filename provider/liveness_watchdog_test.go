package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSdWatchdogInterval(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	if _, ok := sdWatchdogInterval(env(nil)); ok {
		t.Fatal("no WATCHDOG_USEC: the watchdog must stay off")
	}
	if _, ok := sdWatchdogInterval(env(map[string]string{"WATCHDOG_USEC": "0"})); ok {
		t.Fatal("WATCHDOG_USEC=0 means disabled")
	}
	if _, ok := sdWatchdogInterval(env(map[string]string{"WATCHDOG_USEC": "garbage"})); ok {
		t.Fatal("an unparseable WATCHDOG_USEC must stay off")
	}
	got, ok := sdWatchdogInterval(env(map[string]string{"WATCHDOG_USEC": "1200000000"}))
	if !ok || got != 400*time.Second {
		t.Fatalf("1200s timeout must ping every 400s, got %v ok=%v", got, ok)
	}
	if got, ok := sdWatchdogInterval(env(map[string]string{"WATCHDOG_USEC": "900000"})); !ok || got < time.Second {
		t.Fatalf("the interval must have a one second floor, got %v ok=%v", got, ok)
	}
}

// Startup is judged against the startup grace until the progress loop has
// ticked once (the loop starts after the proxy list is loaded, which takes
// minutes on a big list); after that the strict budget applies.
func TestLivenessProgressStartupGraceThenStrictBudget(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Unix(1_000_000, 0).UnixNano())
	l := newLivenessProgress(func() time.Time { return time.Unix(0, clock.Load()) })

	clock.Add(int64(25 * time.Minute))
	if !l.fresh(10 * time.Minute) {
		t.Fatal("25 minutes of startup is inside the startup grace")
	}
	clock.Add(int64(10 * time.Minute))
	if l.fresh(10 * time.Minute) {
		t.Fatal("35 minutes without the loop ever ticking is a stalled start")
	}

	l.tick()
	if !l.fresh(10 * time.Minute) {
		t.Fatal("a tick restores freshness")
	}
	clock.Add(int64(9 * time.Minute))
	if !l.fresh(10 * time.Minute) {
		t.Fatal("9 minutes after a tick is inside the budget")
	}
	clock.Add(int64(2 * time.Minute))
	if l.fresh(10 * time.Minute) {
		t.Fatal("after the first tick the strict 10 minute budget applies, not the startup grace")
	}
}

type watchdogHarness struct {
	t      *testing.T
	clock  atomic.Int64
	l      *livenessProgress
	tick   chan time.Time
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	pings  int
	lines  []string
	stalls int
	undos  int
	gates  []bool // the episodeStart argument of every gate call
}

func (h *watchdogHarness) now() time.Time { return time.Unix(0, h.clock.Load()) }

func startWatchdogHarness(t *testing.T, gate func(episodeStart bool) (bool, string), undoFn func()) *watchdogHarness {
	h := &watchdogHarness{t: t, tick: make(chan time.Time)}
	h.clock.Store(time.Unix(1_000_000, 0).UnixNano())
	h.l = newLivenessProgress(h.now)
	h.l.tick()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan struct{})
	go func() {
		defer close(h.done)
		runSdWatchdogLoop(ctx, h.tick, h.l, 10*time.Minute,
			func() error { h.mu.Lock(); h.pings++; h.mu.Unlock(); return nil },
			func(f string, a ...any) {
				h.mu.Lock()
				h.lines = append(h.lines, strings.TrimSpace(fmt.Sprintf(f, a...)))
				h.mu.Unlock()
			},
			func(start bool) (bool, string) {
				h.mu.Lock()
				h.gates = append(h.gates, start)
				h.mu.Unlock()
				return gate(start)
			},
			func() func() {
				h.mu.Lock()
				h.stalls++
				h.mu.Unlock()
				return func() {
					if undoFn != nil {
						undoFn()
					}
					h.mu.Lock()
					h.undos++
					h.mu.Unlock()
				}
			})
	}()
	return h
}

// step delivers one tick and waits for the loop to finish handling it.
func (h *watchdogHarness) step() {
	h.t.Helper()
	for i := 0; i < 2; i++ {
		select {
		case h.tick <- h.now():
		case <-time.After(2 * time.Second):
			h.t.Fatal("the watchdog loop did not take the tick")
		}
	}
}

func (h *watchdogHarness) stop() { h.cancel(); <-h.done }

func (h *watchdogHarness) counts() (pings, stalls, undos int, lines []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pings, h.stalls, h.undos, append([]string(nil), h.lines...)
}

func always() func(bool) (bool, string) { return func(bool) (bool, string) { return true, "" } }

func TestRunSdWatchdogWithholdsThePingWhileStalledAndUndoesWhenItResolves(t *testing.T) {
	h := startWatchdogHarness(t, always(), nil)
	defer h.stop()

	h.step()
	healthy, _, _, _ := h.counts()
	if healthy < 1 {
		t.Fatal("a healthy process must be pinged")
	}

	h.clock.Add(int64(11 * time.Minute))
	h.step()
	h.step()
	p, stalls, undos, lines := h.counts()
	if p != healthy {
		t.Fatalf("a stalled process must NOT be pinged: pings went %d -> %d", healthy, p)
	}
	if stalls != 1 || undos != 0 || len(lines) != 1 {
		t.Fatalf("one episode: one onStall, no undo yet, one log line; got stalls=%d undos=%d lines=%v", stalls, undos, lines)
	}

	h.l.tick() // progress resumes before systemd acted
	h.step()
	waitUntil(t, "the undo to run", func() bool { _, _, u, _ := h.counts(); return u == 1 })
	if p, _, _, _ := h.counts(); p <= healthy {
		t.Fatal("pinging must resume")
	}
}

// the ping must never wait for the undo (the lock behind it can block)
func TestRunSdWatchdogPingsBeforeAndDoesNotWaitForTheUndo(t *testing.T) {
	release := make(chan struct{})
	h := startWatchdogHarness(t, always(), func() { <-release })
	defer func() { close(release); h.stop() }()

	h.clock.Add(int64(11 * time.Minute))
	h.step()
	h.l.tick()
	before, _, _, _ := h.counts()
	h.step() // the undo is blocked; the loop must still run and ping
	h.step()
	after, _, _, _ := h.counts()
	if after <= before {
		t.Fatalf("a blocked undo stopped the pings: %d -> %d", before, after)
	}
}

// self-heal off or a spent budget: keep feeding, record nothing, say why once
func TestRunSdWatchdogKeepsFeedingWhenTheGateSaysNo(t *testing.T) {
	h := startWatchdogHarness(t, func(bool) (bool, string) { return false, "self-heal is off" }, nil)
	defer h.stop()
	h.clock.Add(int64(11 * time.Minute))
	for i := 0; i < 3; i++ {
		h.step()
	}
	p, stalls, _, lines := h.counts()
	if p < 5 || stalls != 0 {
		t.Fatalf("the watchdog must keep being fed and nothing recorded, pings=%d stalls=%d", p, stalls)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "self-heal is off") {
		t.Fatalf("expected exactly one line naming the reason, got %v", lines)
	}
}

// self-heal turned on in the middle of a stall must start acting, loudly
func TestRunSdWatchdogActsWhenSelfHealComesOnMidStall(t *testing.T) {
	var on atomic.Bool
	h := startWatchdogHarness(t, func(bool) (bool, string) {
		if on.Load() {
			return true, ""
		}
		return false, "self-heal is off"
	}, nil)
	defer h.stop()
	h.clock.Add(int64(11 * time.Minute))
	h.step()
	_, stalls, _, _ := h.counts()
	if stalls != 0 {
		t.Fatal("not acting yet")
	}
	on.Store(true)
	h.step()
	h.step()
	_, stalls, _, lines := h.counts()
	if stalls != 1 {
		t.Fatalf("turning self-heal on mid-stall must start the episode, onStall ran %d times", stalls)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "withholding") {
		t.Fatalf("the withholding must be logged, got %v", lines)
	}
}

// the budget includes the entry the stall itself writes, so only the first stale
// tick of an episode may ask for it
func TestRunSdWatchdogAsksForTheBudgetOnlyAtTheStartOfAnEpisode(t *testing.T) {
	h := startWatchdogHarness(t, always(), nil)
	defer h.stop()
	h.clock.Add(int64(11 * time.Minute))
	for i := 0; i < 3; i++ {
		h.step()
	}
	h.mu.Lock()
	gates := append([]bool(nil), h.gates...)
	h.mu.Unlock()
	if len(gates) < 3 || !gates[0] {
		t.Fatalf("the first stale tick is the episode start, got %v", gates)
	}
	for _, start := range gates[1:] {
		if start {
			t.Fatalf("later ticks of the same episode must not ask for the budget: %v", gates)
		}
	}
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
