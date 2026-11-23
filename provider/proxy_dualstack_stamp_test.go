package main

// The dual stack gate (proxy_dualstack.go) defers a stop decision to the
// measurement the sweep publishes with its own UDP check: proxyIPv6RecordForCheck
// pairs the two by comparing udp.CheckedAt with v6.At, and round 3 finding 2
// leans on a sweep stamping both with the SAME instant. Nothing else pins that.
//
// These tests exist for that reason. The deferral is only sound while one sweep
// writes one instant to both results: a record stamped before its check reads as
// the previous sweep's, so the deferred decision would wait for evidence that
// never arrives or, worse, count the standing record as its own measurement. A
// later change to stamping (a real clock in the stage, a stamp taken after the
// publication) would silently break the gate, and these tests fail instead.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// setIPv6Interval makes a stored measurement due on the test's synthetic clock
// instead of waiting out the interval its own result derived.
func setIPv6Interval(store *proxyUDPCheckStore, settings *connect.ProxySettings, interval time.Duration) {
	store.mu.Lock()
	if entry := store.entries[settings.Key()]; entry != nil {
		entry.ipv6.Interval = interval
	}
	store.mu.Unlock()
}

// measureYes and measureNo are the stage results these tests publish.
func measureYes(previous proxyIPv6Result, at time.Time) proxyIPv6Result {
	return previous.measured(at, proxyIPv6Yes, "yes", "2001:db8::1", "2001:db8::1")
}

func measureNo(previous proxyIPv6Result, at time.Time) proxyIPv6Result {
	return previous.measured(at, proxyIPv6No, "unknown", "", "")
}

// One sweep stamps its UDP check and its IPv6 measurement with one instant,
// which is the assumption the gate's deferral (finding 2) rests on.
func TestUDPSweepStampsItsCheckAndMeasurementWithTheSameInstant(t *testing.T) {
	h := newIPv6CheckHarness(t)
	settings := ipv6TestSettings()
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)

	h.store.ipv6Probe = func(_ context.Context, _ *connect.ProxySettings, previous proxyIPv6Result, at time.Time, _ proxyIPv6Dials) (proxyIPv6Result, bool) {
		return measureNo(previous, at), true
	}
	h.store.Check(withIPv6Sweep(context.Background()), settings, now)

	udp, v6, state := h.store.measurement(settings.Key(), now)
	if state != measurementReady {
		t.Fatalf("measurement state = %v, want ready", state)
	}
	if udp.CheckedAt.IsZero() || v6.At.IsZero() {
		t.Fatalf("the sweep left a stamp unset: udp.CheckedAt=%v v6.At=%v", udp.CheckedAt, v6.At)
	}
	if !udp.CheckedAt.Equal(v6.At) {
		t.Fatalf("one sweep stamped its check at %v and its measurement at %v: the gate cannot pair them", udp.CheckedAt, v6.At)
	}
	if !proxyIPv6RecordForCheck(udp, v6) {
		t.Fatal("the record a sweep just published does not read as its own check's")
	}
}

// The IPv6-only path: a refresh of a due measurement runs without a UDP check,
// so its stamp is later than the check it followed and the two differ. The gate
// must still pair the record with that check, start only on a second
// measurement, and go on to stop after two disqualifying checks: no decision on
// one result, no double count, no stuck state.
func TestIPv6OnlyRefreshStampsLaterAndTheGateStillDecides(t *testing.T) {
	h := newIPv6CheckHarness(t)
	settings := ipv6TestSettings()
	ctx := withIPv6Sweep(context.Background())
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)

	yes := true
	h.store.ipv6Probe = func(_ context.Context, _ *connect.ProxySettings, previous proxyIPv6Result, at time.Time, _ proxyIPv6Dials) (proxyIPv6Result, bool) {
		if yes {
			return measureYes(previous, at), true
		}
		return measureNo(previous, at), true
	}

	// the sweep that measures both
	h.store.Check(ctx, settings, now)
	check0, v6At0, state := h.store.measurement(settings.Key(), now)
	if state != measurementReady {
		t.Fatalf("measurement state = %v, want ready", state)
	}

	// the measurement alone comes due while the UDP result is still fresh, so
	// only the IPv6 stage runs
	setIPv6Interval(h.store, settings, 10*time.Minute)
	refreshed := now.Add(11 * time.Minute)
	h.store.Check(ctx, settings, refreshed)
	if got := h.udpRuns.Load(); got != 1 {
		t.Fatalf("the IPv6-only refresh probed UDP %d times, want 1", got)
	}
	check, v6, state := h.store.measurement(settings.Key(), refreshed)
	if state != measurementReady {
		t.Fatalf("measurement state = %v, want ready", state)
	}
	if !check.CheckedAt.Equal(check0.CheckedAt) {
		t.Fatalf("the refresh moved the check stamp: %v -> %v", check0.CheckedAt, check.CheckedAt)
	}
	if !v6.At.After(check.CheckedAt) {
		t.Fatalf("the refresh stamped its record at %v, not after the check at %v", v6.At, check.CheckedAt)
	}
	// the record still pairs with the check it followed: that is what the gate
	// reads, and why a refresh cannot pass for a check
	if !proxyIPv6RecordForCheck(check, v6) {
		t.Fatal("a refreshed record no longer pairs with the check it followed")
	}

	gate := &proxyIPv6Gate{}
	if start, stop := gate.observe(now, check0, v6At0); start || stop {
		t.Fatalf("one measurement decided start=%v stop=%v", start, stop)
	}
	start, stop := gate.observe(refreshed, check, v6)
	if !start || stop {
		t.Fatalf("the second qualifying measurement decided start=%v stop=%v", start, stop)
	}
	if start, stop := gate.observe(refreshed, check, v6); start || stop {
		t.Fatalf("a repeated poll of the same measurement decided start=%v stop=%v", start, stop)
	}

	// two disqualifying checks stop the transport, one observation each
	yes = false
	setIPv6Interval(h.store, settings, 10*time.Minute)
	first := now.Add(42 * time.Minute)
	h.store.Check(ctx, settings, first)
	check, v6, state = h.store.measurement(settings.Key(), first)
	if state != measurementReady {
		t.Fatalf("measurement state = %v, want ready", state)
	}
	if start, stop := gate.observe(first, check, v6); start || stop {
		t.Fatalf("one disqualifying check decided start=%v stop=%v", start, stop)
	}
	if start, stop := gate.observe(first, check, v6); start || stop {
		t.Fatalf("a repeated poll of one check decided start=%v stop=%v", start, stop)
	}

	setIPv6Interval(h.store, settings, 10*time.Minute)
	second := now.Add(84 * time.Minute)
	h.store.Check(ctx, settings, second)
	check, v6, state = h.store.measurement(settings.Key(), second)
	if state != measurementReady {
		t.Fatalf("measurement state = %v, want ready", state)
	}
	if start, stop := gate.observe(second, check, v6); start || !stop {
		t.Fatalf("two disqualifying checks decided start=%v stop=%v", start, stop)
	}
	if start, stop := gate.observe(second, check, v6); start || stop {
		t.Fatalf("a repeated poll after the stop decided start=%v stop=%v", start, stop)
	}
}

// A sweep publishes its UDP result before its own measurement, so a poll can
// land between the two. The gate then sees a new check beside the previous
// sweep's record and must defer, because that record says nothing about this
// check. It resolves on the measurement carrying the check's own instant, which
// is the comparison this file pins.
func TestGateDefersToTheMeasurementOfItsOwnCheck(t *testing.T) {
	h := newIPv6CheckHarness(t)
	settings := ipv6TestSettings()
	ctx := withIPv6Sweep(context.Background())
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)

	h.store.ipv6Probe = func(_ context.Context, _ *connect.ProxySettings, previous proxyIPv6Result, at time.Time, _ proxyIPv6Dials) (proxyIPv6Result, bool) {
		return measureYes(previous, at), true
	}
	h.store.Check(ctx, settings, now)
	check0, v6At0, state := h.store.measurement(settings.Key(), now)
	if state != measurementReady {
		t.Fatalf("measurement state = %v, want ready", state)
	}

	gate := &proxyIPv6Gate{}
	if start, stop := gate.observe(now, check0, v6At0); start || stop {
		t.Fatalf("one sweep decided start=%v stop=%v", start, stop)
	}

	// the next check is due while the standing record still qualifies
	setIPv6Interval(h.store, settings, 30*time.Minute)
	later := now.Add(31 * time.Minute)
	var midCheck proxyUDPResult
	var midV6 proxyIPv6Result
	var midStart, midStop bool
	h.store.ipv6Probe = func(_ context.Context, _ *connect.ProxySettings, previous proxyIPv6Result, at time.Time, _ proxyIPv6Dials) (proxyIPv6Result, bool) {
		// the poll lands between the two publications of this sweep
		midCheck, midV6, _ = h.store.measurement(settings.Key(), at)
		midStart, midStop = gate.observe(at, midCheck, midV6)
		return measureYes(previous, at), true
	}
	h.store.Check(ctx, settings, later)

	if !midCheck.CheckedAt.Equal(later) || !midV6.At.Equal(now) {
		t.Fatalf("the poll between the publications read udp.CheckedAt=%v v6.At=%v", midCheck.CheckedAt, midV6.At)
	}
	if midStart || midStop || !gate.pending {
		t.Fatalf("the poll between the publications decided start=%v stop=%v pending=%v", midStart, midStop, gate.pending)
	}

	// the measurement of that same sweep arrives and the deferral resolves
	check, v6, state := h.store.measurement(settings.Key(), later)
	if state != measurementReady {
		t.Fatalf("measurement state = %v, want ready", state)
	}
	start, stop := gate.observe(later, check, v6)
	if gate.pending {
		t.Fatal("the deferral did not resolve on the sweep's own measurement")
	}
	if !start || stop {
		t.Fatalf("the deferred measurement decided start=%v stop=%v", start, stop)
	}
	if start, stop := gate.observe(later, check, v6); start || stop {
		t.Fatalf("a repeated poll of the resolved measurement decided start=%v stop=%v", start, stop)
	}
}
