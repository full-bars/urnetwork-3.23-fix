package main

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// A sweep stamps its IPv6 record when the probe starts, so a lazy udp check
// that completes while the probe runs always lands with a later stamp. The
// record is therefore never the check's own measurement, and it must count
// once all the same: a proxy that has lost IPv6 must not keep its extra
// transport because every negative arrives behind a newer check.
func TestProxyIPv6GateCountsAMeasurementOlderThanItsCheck(t *testing.T) {
	start := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	udpOK := func(at time.Time) proxyUDPResult {
		return proxyUDPResult{Status: proxyUDPOK, CheckedAt: at}
	}
	for _, split := range []bool{false, true} {
		name := "both results in place before the poll"
		if split {
			name = "check polled before its sweep's record"
		}
		t.Run(name, func(t *testing.T) {
			gate := &proxyIPv6Gate{}
			first := v6Result(proxyIPv6Yes, "yes", start, time.Hour)
			if started, _ := gate.observe(start, udpOK(start), first); started {
				t.Fatal("started on the first measurement")
			}
			at := start.Add(first.Interval)
			standing := v6Result(proxyIPv6Yes, "yes", at, time.Hour)
			if started, _ := gate.observe(at, udpOK(at), standing); !started {
				t.Fatal("did not start on the second measurement")
			}

			for round := 1; round <= proxyDualstackStopStreak; round++ {
				// the probe of this sweep starts at probeAt, and a lazy udp
				// check completes one second later, so the disqualifying record
				// is stamped before the check it lands beside
				probeAt := standing.At.Add(standing.Interval)
				checkAt := probeAt.Add(time.Second)
				negative := standing.measured(probeAt, proxyIPv6No, "unknown", "", "")
				if split {
					// the poll lands on the check alone: it defers only when
					// the record that still stands qualifies
					qualifies := proxyIPv6TransportWanted(udpOK(checkAt), standing, checkAt)
					if started, stopped := gate.observe(checkAt, udpOK(checkAt), standing); started || stopped {
						t.Fatalf("round %d: the deferred check decided on its own: start %t stop %t", round, started, stopped)
					}
					if gate.pending != qualifies {
						t.Fatalf("round %d: pending %t, want %t for a standing record that qualifies %t", round, gate.pending, qualifies, qualifies)
					}
				}
				started, stopped := gate.observe(checkAt.Add(time.Minute), udpOK(checkAt), negative)
				if started {
					t.Fatalf("round %d: a disqualifying measurement started the transport", round)
				}
				if gate.no != round || gate.yes != 0 {
					t.Fatalf("round %d: stop streak %d, start streak %d, want %d and 0", round, gate.no, gate.yes, round)
				}
				wantStop := round >= proxyDualstackStopStreak
				if stopped != wantStop || gate.running == wantStop {
					t.Fatalf("round %d: stop %t running %t, want stop %t", round, stopped, gate.running, wantStop)
				}
				// a poll that sees the same two results again adds nothing
				if _, again := gate.observe(checkAt.Add(2*time.Minute), udpOK(checkAt), negative); again {
					t.Fatalf("round %d: a repeated poll stopped the transport", round)
				}
				if gate.no != round {
					t.Fatalf("round %d: a repeated poll moved the stop streak to %d", round, gate.no)
				}
				standing = negative
			}
		})
	}
}

// ipv6TransportFixture starts the extra transport on two qualifying
// measurements and returns the record that is standing.
func ipv6TransportFixture(t *testing.T) (*proxiedIPv6Controller, *proxyUDPCheckStore, *connect.ProxySettings, string, proxyIPv6Result) {
	t.Helper()
	controller, _, _, store, settings := controllerFixture(t)
	store.probe = func(context.Context, *connect.ProxySettings) proxyUDPResult {
		return proxyUDPResult{Status: proxyUDPOK}
	}
	start := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	key := settings.Key()
	store.entries[key] = &proxyUDPCheckEntry{}
	var record proxyIPv6Result
	for i := 0; i < 2; i++ {
		now := start.Add(time.Duration(i) * 12 * time.Hour)
		record = record.measured(now, proxyIPv6Yes, "yes", "2001:db8::1", "2001:db8::1")
		store.entries[key].ipv6 = record
		store.Check(context.Background(), settings, now)
		controller.step(now)
	}
	if !controller.transport.hasExtra() {
		t.Fatal("control setup did not start the extra")
	}
	t.Cleanup(func() {
		controller.releasePin()
		controller.transport.Close()
	})
	return controller, store, settings, key, record
}

// runSweepBehindALazyCheck publishes one IPv6 sweep whose probe finishes after
// a lazy udp check completed, then polls. The sweeps up to it keep the udp
// evidence fresh the way the scheduler does.
func runSweepBehindALazyCheck(t *testing.T, controller *proxiedIPv6Controller, store *proxyUDPCheckStore, settings *connect.ProxySettings, key string, record proxyIPv6Result) proxyIPv6Result {
	t.Helper()
	due := record.At.Add(record.Interval)
	for next := record.At.Add(time.Hour); next.Before(due.Add(-time.Hour)); next = next.Add(time.Hour) {
		store.Check(context.Background(), settings, next)
		controller.step(next)
	}
	store.Check(context.Background(), settings, due.Add(-proxyUDPLazyTTL+time.Millisecond))
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	store.ipv6Probe = func(_ context.Context, _ *connect.ProxySettings, previous proxyIPv6Result, now time.Time, _ proxyIPv6Dials) (proxyIPv6Result, bool) {
		close(entered)
		<-release
		return previous.measured(now, proxyIPv6No, "unknown", "", ""), true
	}
	go func() {
		defer close(finished)
		store.Check(withIPv6Sweep(context.Background()), settings, due)
	}()
	<-entered
	// the lazy check completes while the probe runs
	store.Check(context.Background(), settings, due.Add(time.Second))
	// a poll between the two publications must not decide anything
	controller.step(due.Add(2 * time.Second))
	close(release)
	<-finished
	poll := due.Add(time.Minute)
	udp, v6, state := store.measurement(key, poll)
	if state != measurementReady || udp.Status != proxyUDPOK || v6.Status != proxyIPv6No || !v6.At.Before(udp.CheckedAt) {
		t.Fatalf("unexpected observations at %v: udp=%+v v6=%+v state=%v", poll, udp, v6, state)
	}
	controller.step(poll)
	return v6
}

func TestProxiedIPv6ControllerStopsOnNegativesBehindLazyChecks(t *testing.T) {
	controller, store, settings, key, record := ipv6TransportFixture(t)
	for round := 1; round <= proxyDualstackStopStreak; round++ {
		record = runSweepBehindALazyCheck(t, controller, store, settings, key, record)
		if controller.gate.no != round {
			t.Fatalf("round %d: stop streak %d, want %d", round, controller.gate.no, round)
		}
		wantRunning := round < proxyDualstackStopStreak
		if controller.transport.hasExtra() != wantRunning {
			t.Fatalf("round %d: extra running %t, want %t", round, controller.transport.hasExtra(), wantRunning)
		}
	}
}

// Positive control: a pair of failures published with their own checks stops
// the extra as well, so the counting rule above has not replaced that path.
func TestProxiedIPv6ControllerStopsOnSameStampNegatives(t *testing.T) {
	controller, store, settings, key, record := ipv6TransportFixture(t)
	for i := 0; i < 2; i++ {
		now := record.At.Add(record.Interval)
		store.Check(context.Background(), settings, now)
		record = record.measured(now, proxyIPv6No, "unknown", "", "")
		store.entries[key].ipv6 = record
		controller.step(now)
	}
	if controller.transport.hasExtra() {
		t.Fatal("a pair of same-stamp failures should stop the extra")
	}
	if controller.gate.no != proxyDualstackStopStreak {
		t.Fatalf("stop streak %d after a pair of same-stamp failures", controller.gate.no)
	}
}
