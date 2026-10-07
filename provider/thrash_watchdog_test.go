//go:build linux

package main

// Tests for the Phase 2a thrash watchdog: parse, rate math, state machine,
// attribution, cap ladder, messages and status serialization. All waits are
// time-driven with explicit clocks; no wall-clock assertions.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var thrashT0 = time.Unix(1_800_000_000, 0)

// ---------------------------------------------------------------------------
// Parsers: missing keys are unavailable, never 0
// ---------------------------------------------------------------------------

func TestParsePSITotalsMissingFullIsUnavailable(t *testing.T) {
	some, full, someOK, fullOK := parsePSITotals("some avg10=0.00 avg60=0.00 avg300=0.00 total=111\n")
	if !someOK || some != 111 {
		t.Fatalf("some: got (%d, %v), want (111, true)", some, someOK)
	}
	if fullOK || full != 0 {
		t.Fatalf("missing full line must be unavailable, got (%d, %v)", full, fullOK)
	}
	// full present but without a total= field is still unavailable.
	_, _, _, fullOK = parsePSITotals("some avg10=0.00 total=5\nfull avg10=0.00 avg60=0.00\n")
	if fullOK {
		t.Fatalf("full without total= must be unavailable")
	}
	_, full, _, fullOK = parsePSITotals("some avg10=0 total=5\nfull avg10=0 total=42\n")
	if !fullOK || full != 42 {
		t.Fatalf("full total: got (%d, %v), want (42, true)", full, fullOK)
	}
}

func TestParseVMStatSwapMissingCounterIsUnavailable(t *testing.T) {
	// Partial counters: ok=false (values may be partially filled; callers
	// must gate on ok and report the sensor unavailable, never a fake zero).
	if _, _, ok := parseVMStatSwap("pswpin 100\n"); ok {
		t.Fatalf("only pswpin present: want unavailable")
	}
	in, out, ok := parseVMStatSwap("pswpin 100\npswpout 7\n")
	if !ok || in != 100 || out != 7 {
		t.Fatalf("got (%d, %d, %v), want (100, 7, true)", in, out, ok)
	}
}

func TestParseMemoryStatCounter(t *testing.T) {
	content := "anon 100\npswpin 2\npswpout 0\nworkingset_refault_anon 55\n"
	if v, ok := parseMemoryStatCounter(content, "pswpin"); !ok || v != 2 {
		t.Fatalf("pswpin: got (%d, %v)", v, ok)
	}
	if _, ok := parseMemoryStatCounter(content, "pswpout_missing"); ok {
		t.Fatalf("missing key must be unavailable")
	}
	// The LA7 6.x case: swap keys entirely absent from memory.stat.
	if _, ok := parseMemoryStatCounter("anon 100\nfile 50\n", "pswpin"); ok {
		t.Fatalf("no pswpin key: must be unavailable")
	}
}

func TestParseMeminfoSwap(t *testing.T) {
	total, free, ok := parseMeminfoSwap("MemTotal:  2048000 kB\nSwapTotal: 4194304 kB\nSwapFree:  1048576 kB\n")
	if !ok || total != 4096 || free != 1024 {
		t.Fatalf("got (%d, %d, %v), want (4096, 1024, true)", total, free, ok)
	}
	if _, _, ok := parseMeminfoSwap("SwapTotal: 4194304 kB\n"); ok {
		t.Fatalf("missing SwapFree must be unavailable")
	}
}

// ---------------------------------------------------------------------------
// Rate math
// ---------------------------------------------------------------------------

func TestThrashCounterDeltaSkipsFirstAndReset(t *testing.T) {
	var d pressureCounterDelta
	if r, ok := d.rate(100, thrashT0); ok || r != 0 {
		t.Fatalf("first sample must be unavailable, got (%v, %v)", r, ok)
	}
	if r, ok := d.rate(1100, thrashT0.Add(10*time.Second)); !ok || r != 100 {
		t.Fatalf("want (100, true), got (%v, %v)", r, ok)
	}
	// Counter moved backwards (reboot / cgroup recreation): unavailable.
	if r, ok := d.rate(5, thrashT0.Add(20*time.Second)); ok || r != 0 {
		t.Fatalf("reset must be unavailable, got (%v, %v)", r, ok)
	}
	if r, ok := d.rate(505, thrashT0.Add(30*time.Second)); !ok || r != 50 {
		t.Fatalf("after reset, want (50, true), got (%v, %v)", r, ok)
	}
}

func TestThrashRatesDelayedTicker(t *testing.T) {
	// A swapped-out ticker can slip 90s+ between samples: the fraction must be
	// delta_total_us / delta_wall_us, not per-sample-count and not avg60.
	var tr thrashTracker
	rd := thrashRead{
		psiSomeTotal: 100_000_000, psiFullTotal: 50_000_000,
		psiSomeOK: true, psiFullOK: true, psiUnit: true,
		swapIn: 0, swapOut: 0, swapOK: true, swapUnit: true,
		refault: 0, refaultOK: true,
	}
	tr.rates(rd, thrashT0) // baseline

	rd.psiSomeTotal = 100_000_000 + 90_000_000 // 90s stalled over 90s wall
	rd.psiFullTotal = 50_000_000 + 45_000_000  // 45s of 90s
	rd.swapIn = 9000                           // 100 pages/s
	rd.refault = 900                           // 10 pages/s
	r := tr.rates(rd, thrashT0.Add(90*time.Second))

	if got := r.fullFrac; got < 0.49 || got > 0.51 {
		t.Fatalf("fullFrac over 90s slip: want ~0.5, got %v", got)
	}
	if got := r.someFrac; got < 0.99 || got > 1.01 {
		t.Fatalf("someFrac: want ~1.0, got %v", got)
	}
	if got := r.swapInPS; got < 99 || got > 101 {
		t.Fatalf("swapInPS: want ~100, got %v", got)
	}
	if got := r.refaultPS; got < 9 || got > 11 {
		t.Fatalf("refaultPS: want ~10, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// State machine
// ---------------------------------------------------------------------------

func mildRates(fullFrac float64, swapOK bool, swapPS float64) thrashRates {
	return thrashRates{fullFrac: fullFrac, fullOK: true, swapOK: swapOK, swapInPS: swapPS}
}

func TestThrashMachineMildCorroboratedTakesThreeMinutes(t *testing.T) {
	m := &thrashMachine{state: thrashCalm, stateSince: thrashT0}
	rt := mildRates(0.15, true, 300) // corroborated by 300 pages/s
	st := m.step(thrashT0, rt, thrashRead{})
	if !st.changed || st.cur != thrashUnderPressure {
		t.Fatalf("t0: want under-pressure, got %v (changed=%v)", st.cur, st.changed)
	}
	st = m.step(thrashT0.Add(2*time.Minute), rt, thrashRead{})
	if st.cur != thrashUnderPressure {
		t.Fatalf("t+2m: want still under-pressure, got %v", st.cur)
	}
	st = m.step(thrashT0.Add(3*time.Minute+time.Second), rt, thrashRead{})
	if st.cur != thrashThrashing {
		t.Fatalf("t+3m: want thrashing, got %v", st.cur)
	}
}

func TestThrashMachineSevereAloneEscalatesAtNinetySeconds(t *testing.T) {
	// The 51%-pool redegrade case: NO corroborators readable at all, PSI full
	// severe. Must escalate anyway — never AND with an unavailable signal.
	m := &thrashMachine{state: thrashCalm, stateSince: thrashT0}
	rt := thrashRates{fullFrac: 0.30, fullOK: true}
	if st := m.step(thrashT0, rt, thrashRead{}); st.cur != thrashUnderPressure {
		t.Fatalf("t0: want under-pressure, got %v", st.cur)
	}
	if st := m.step(thrashT0.Add(80*time.Second), rt, thrashRead{}); st.cur != thrashUnderPressure {
		t.Fatalf("t+80s: want under-pressure, got %v", st.cur)
	}
	if st := m.step(thrashT0.Add(91*time.Second), rt, thrashRead{}); st.cur != thrashThrashing {
		t.Fatalf("t+91s: want thrashing, got %v", st.cur)
	}
}

func TestThrashMachineMildUncorroboratedNeedsStricterBar(t *testing.T) {
	// 15% with nothing readable: below the stricter 20% PSI-only bar -> calm.
	m1 := &thrashMachine{state: thrashCalm, stateSince: thrashT0}
	if st := m1.step(thrashT0, thrashRates{fullFrac: 0.15, fullOK: true}, thrashRead{}); st.cur != thrashCalm {
		t.Fatalf("15%% uncorroborated: want calm, got %v", st.cur)
	}
	// 22% with nothing readable: above the stricter bar -> counts.
	m2 := &thrashMachine{state: thrashCalm, stateSince: thrashT0}
	if st := m2.step(thrashT0, thrashRates{fullFrac: 0.22, fullOK: true}, thrashRead{}); st.cur != thrashUnderPressure {
		t.Fatalf("22%% uncorroborated: want under-pressure, got %v", st.cur)
	}
	// Corroborators readable but quiet -> 15% does NOT count (peace, not thrash).
	m3 := &thrashMachine{state: thrashCalm, stateSince: thrashT0}
	if st := m3.step(thrashT0, mildRates(0.15, true, 10), thrashRead{}); st.cur != thrashCalm {
		t.Fatalf("15%% corroborated-quiet: want calm, got %v", st.cur)
	}
}

func TestThrashMachineRelaxesOneStepPerCalmWindow(t *testing.T) {
	m := &thrashMachine{state: thrashCalm, stateSince: thrashT0}
	severe := thrashRates{fullFrac: 0.30, fullOK: true}
	m.step(thrashT0, severe, thrashRead{})
	thrashAt := thrashT0.Add(91 * time.Second)
	if st := m.step(thrashAt, severe, thrashRead{}); st.cur != thrashThrashing {
		t.Fatalf("want thrashing, got %v", st.cur)
	}
	calm := thrashRates{fullFrac: 0, fullOK: true}
	// Calm starts here.
	a := thrashAt.Add(time.Minute)
	if st := m.step(a, calm, thrashRead{}); st.cur != thrashThrashing {
		t.Fatalf("calm just started: want still thrashing, got %v", st.cur)
	}
	b := a.Add(5*time.Minute + time.Second)
	if st := m.step(b, calm, thrashRead{}); st.cur != thrashUnderPressure {
		t.Fatalf("first window: want under-pressure, got %v", st.cur)
	}
	if st := m.step(b.Add(time.Minute), calm, thrashRead{}); st.cur != thrashUnderPressure {
		t.Fatalf("between windows: want still under-pressure, got %v", st.cur)
	}
	// The window counts from the step that started the calm streak (b+1m).
	c := b.Add(6*time.Minute + 2*time.Second)
	if st := m.step(c, calm, thrashRead{}); st.cur != thrashCalm {
		t.Fatalf("second window: want calm, got %v", st.cur)
	}
}

func TestThrashMachineCriticalAfterFifteenMinutes(t *testing.T) {
	m := &thrashMachine{state: thrashCalm, stateSince: thrashT0}
	severe := thrashRates{fullFrac: 0.30, fullOK: true}
	m.step(thrashT0, severe, thrashRead{})
	thrashAt := thrashT0.Add(91 * time.Second)
	if st := m.step(thrashAt, severe, thrashRead{}); st.cur != thrashThrashing {
		t.Fatalf("want thrashing, got %v", st.cur)
	}
	// condDur counts from the first severe tick (thrashT0): promotion needs
	// the CONTINUOUS condition to have held for the full window.
	if st := m.step(thrashT0.Add(14*time.Minute+time.Second), severe, thrashRead{}); st.cur != thrashThrashing {
		t.Fatalf("t0+14m: want thrashing, got %v", st.cur)
	}
	if st := m.step(thrashT0.Add(15*time.Minute+time.Second), severe, thrashRead{}); st.cur != thrashCritical {
		t.Fatalf("t0+15m: want critical, got %v", st.cur)
	}
	if m.thrashSince.IsZero() || !m.thrashSince.Equal(thrashAt) {
		t.Fatalf("thrashSince must mark the thrashing entry (survives critical): got %v, want %v", m.thrashSince, thrashAt)
	}
}

// ---------------------------------------------------------------------------
// Attribution
// ---------------------------------------------------------------------------

func TestThrashAttribution(t *testing.T) {
	ours := thrashRead{unitSwapOK: true, unitSwapMiB: 3100, hostSwapOK: true, hostSwapTotalMiB: 3300, hostSwapUsedMiB: 3300}
	if attr, share, ok := thrashAttribution(ours); attr != "unit" || !ok || share < 0.9 {
		t.Fatalf("unit-dominant: got (%s, %v, %v)", attr, share, ok)
	}
	other := thrashRead{unitSwapOK: true, unitSwapMiB: 100, hostSwapOK: true, hostSwapTotalMiB: 3300, hostSwapUsedMiB: 3000}
	if attr, _, ok := thrashAttribution(other); attr != "other" || !ok {
		t.Fatalf("other-dominant: want other, got (%s, %v)", attr, ok)
	}
	unitOnly := thrashRead{unitSwapOK: true, unitSwapMiB: 500}
	if attr, _, shareOK := thrashAttribution(unitOnly); attr != "unit" || shareOK {
		t.Fatalf("unit-only: want (unit, share unknown), got (%s, %v)", attr, shareOK)
	}
	psiOnly := thrashRead{psiUnit: true, psiFullOK: true}
	if attr, _, _ := thrashAttribution(psiOnly); attr != "unit" {
		t.Fatalf("per-unit PSI: want unit, got %s", attr)
	}
	if attr, _, _ := thrashAttribution(thrashRead{}); attr != "unknown" {
		t.Fatalf("nothing readable: want unknown, got %s", attr)
	}
}

// ---------------------------------------------------------------------------
// Thrash cap: ladder, sizing, persistence, integration
// ---------------------------------------------------------------------------

func TestThrashCapEscalationLadder(t *testing.T) {
	withTempHome(t)
	base := thrashT0

	allowed, code, reason, n := thrashCapEscalationAllowed(thrashCapState{}, base)
	if !allowed || code != "" || n != 0 {
		t.Fatalf("first escalation must be allowed, got (%v, %q, %q, %d)", allowed, code, reason, n)
	}
	if err := recordThrashEscalation(300, base); err != nil {
		t.Fatalf("record: %v", err)
	}
	// 10 minutes later: inside the 30m re-arm window.
	if allowed, code, _, n := thrashCapEscalationAllowed(readThrashCapState(), base.Add(10*time.Minute)); allowed || code != "rearm" || n != 1 {
		t.Fatalf("within backoff must be denied with code rearm, got (allowed=%v, code=%q, n=%d)", allowed, code, n)
	}
	// Past 30m: allowed again (count now 1 -> next backoff 2h).
	second := base.Add(31 * time.Minute)
	if allowed, _, _, _ := thrashCapEscalationAllowed(readThrashCapState(), second); !allowed {
		t.Fatalf("past the first backoff must be allowed")
	}
	recordThrashEscalation(300, second)
	// 1h after the second: inside the 2h window.
	if allowed, code, _, _ := thrashCapEscalationAllowed(readThrashCapState(), second.Add(time.Hour)); allowed || code != "rearm" {
		t.Fatalf("within 2h backoff must be denied with code rearm")
	}
	third := second.Add(2*time.Hour + time.Minute)
	if allowed, _, _, _ := thrashCapEscalationAllowed(readThrashCapState(), third); !allowed {
		t.Fatalf("past the 2h backoff must be allowed")
	}
	recordThrashEscalation(300, third)
	// Three in 24h: cap reached.
	if allowed, code, _, n := thrashCapEscalationAllowed(readThrashCapState(), third.Add(7*time.Hour)); allowed || code != "cap-reached" || n != 3 {
		t.Fatalf("3/24h must deny with code cap-reached, got (allowed=%v, code=%q, n=%d)", allowed, code, n)
	}
	// Far enough out that the oldest fall out of the window: allowed again.
	later := base.Add(25 * time.Hour)
	if allowed, _, _, n := thrashCapEscalationAllowed(readThrashCapState(), later); !allowed {
		t.Fatalf("aged-out window must allow again (n=%d)", n)
	}
}

func TestThrashCapForNextStart(t *testing.T) {
	cases := []struct{ running, want int }{{500, 300}, {80, 48}, {1, 1}, {0, 0}}
	for _, c := range cases {
		if got := thrashCapForNextStart(c.running); got != c.want {
			t.Errorf("running=%d: got %d, want %d", c.running, got, c.want)
		}
	}
}

func TestActiveThrashCapExpiry(t *testing.T) {
	withTempHome(t)
	if err := recordThrashEscalation(300, thrashT0); err != nil {
		t.Fatalf("record: %v", err)
	}
	if cap, ok := activeThrashCap(thrashT0.Add(time.Hour)); !ok || cap != 300 {
		t.Fatalf("within hold: want (300, true), got (%d, %v)", cap, ok)
	}
	if _, ok := activeThrashCap(thrashT0.Add(25 * time.Hour)); ok {
		t.Fatalf("expired cap must be inactive")
	}
}

func TestEffectiveTrimCapIncludesThrashCap(t *testing.T) {
	home := withTempHome(t)
	t.Setenv("URNETWORK_OOM_CAP", "off")
	trimUnreadableReset()

	if err := recordThrashEscalation(300, time.Now()); err != nil {
		t.Fatalf("record: %v", err)
	}
	cap, src, err := effectiveTrimCapSource()
	if err != nil || cap != 300 || src != trimCapThrash {
		t.Fatalf("thrash cap alone: got (%d, %q, %v)", cap, src, err)
	}
	// A tighter operator cap wins and is credited as the operator's.
	dir := filepath.Join(home, ".urnetwork")
	if err := os.WriteFile(filepath.Join(dir, "proxy_trim"), []byte("200\n"), 0600); err != nil {
		t.Fatalf("write proxy_trim: %v", err)
	}
	cap, src, err = effectiveTrimCapSource()
	if err != nil || cap != 200 || src != trimCapOperator {
		t.Fatalf("tighter operator cap: got (%d, %q, %v)", cap, src, err)
	}
	// A looser operator cap loses to the thrash cap.
	if err := os.WriteFile(filepath.Join(dir, "proxy_trim"), []byte("400\n"), 0600); err != nil {
		t.Fatalf("write proxy_trim: %v", err)
	}
	if cap, src, _ = effectiveTrimCapSource(); cap != 300 || src != trimCapThrash {
		t.Fatalf("looser operator cap: got (%d, %q)", cap, src)
	}
}

// ---------------------------------------------------------------------------
// Scoring fixes
// ---------------------------------------------------------------------------

func TestScoreExcludingCPU(t *testing.T) {
	comps := map[string]float64{"psi_cpu": 0.9, "mem": 0.3, "heap": 0.2}
	if got := scoreExcludingCPU(comps); got != 0.3 {
		t.Fatalf("want 0.3, got %v", got)
	}
	// A saturated psi_cpu alone must NOT carry (scoring fix), even at 1.0.
	if got := scoreExcludingCPU(map[string]float64{"psi_cpu": 1.0, "mem": 0.1}); got != 0.1 {
		t.Fatalf("cpu-saturated: want 0.1, got %v", got)
	}
	// Emergency conditions saturate their own components, so the loop carries them.
	if got := scoreExcludingCPU(map[string]float64{"psi_cpu": 0.4, "heap": 1.0}); got != 1.0 {
		t.Fatalf("heap emergency: want 1.0, got %v", got)
	}
}

func TestCpuPressureComponent(t *testing.T) {
	// Multi-core keeps the shared ramp: (35-10)/50 = 0.5.
	if got := cpuPressureComponent(35, 4); got != 0.5 {
		t.Fatalf("multi-core: want 0.5, got %v", got)
	}
	// Single core uses the quiet ramp: (50-40)/50 = 0.2 (was 0.8 before).
	if got := cpuPressureComponent(50, 1); got < 0.19 || got > 0.21 {
		t.Fatalf("1-core: want ~0.2, got %v", got)
	}
	// Unknown cores behave like before (multi-core ramp).
	if got := cpuPressureComponent(35, 0); got != 0.5 {
		t.Fatalf("unknown cores: want 0.5, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// Messages and serialization: human-readable, null-not-0
// ---------------------------------------------------------------------------

func TestThrashMessagesAreHumanReadable(t *testing.T) {
	rt := thrashRates{fullFrac: 0.51, fullOK: true, swapInPS: 3200, swapOutPS: 3200, swapOK: true}
	rd := thrashRead{unitSwapOK: true, unitSwapMiB: 3100, hostSwapOK: true, hostSwapTotalMiB: 3300, hostSwapUsedMiB: 3300, heapOK: true, heapFrac: 3.1, heapUsedMiB: 2100, heapLimitMiB: 680, ramAvailMiB: 82, ramAvailOK: true}

	msg := thrashOnsetMsg(rt, rd, 0.94, true)
	for _, want := range []string{"thrashing swap", "memory is currently stalled ~51% of the time", "25 MB/s", "3.0 GB", "94% of all swap in use", "Heap is 3.1x"} {
		if !strings.Contains(msg, want) {
			t.Errorf("onset msg missing %q: %s", want, msg)
		}
	}
	other := thrashOtherMsg(rd, 0.04)
	if !strings.Contains(other, "another process") || !strings.Contains(other, "Not restarting") {
		t.Errorf("other-process msg: %s", other)
	}
	action := thrashActionMsg(1, 3, 500, 300)
	for _, want := range []string{"will not recover on its own", "restart 1 of max 3 per day", "cap reduced 500 -> 300"} {
		if !strings.Contains(action, want) {
			t.Errorf("action msg missing %q: %s", want, action)
		}
	}
	cleared := thrashClearedMsg("Thrash cleared by the restart", rd, rt)
	for _, want := range []string{"RAM free", "swap in use", "memory stalls 51%"} {
		if !strings.Contains(cleared, want) {
			t.Errorf("cleared msg missing %q: %s", want, cleared)
		}
	}
	warn := thrashEarlyWarnMsg(rd)
	if !strings.Contains(warn, "heap is 3.1x its size limit") || !strings.Contains(warn, "will start thrashing") {
		t.Errorf("early-warn msg: %s", warn)
	}
}

func TestThrashSnapshotNullsForUnavailable(t *testing.T) {
	m := &thrashMachine{state: thrashCalm, stateSince: thrashT0}
	snap := buildThrashSnapshot(m, thrashRead{}, thrashRates{}, "unknown", 0, false, 0, "", thrashT0)
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"psi_mem_full":null`, `"swap_io_pps":null`, `"unit_swap_mib":null`, `"unit_swap_share":null`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("snapshot missing %s: %s", want, string(b))
		}
	}
	if strings.Contains(string(b), `"psi_mem_full":0`) {
		t.Errorf("psi_mem_full must never serialize as 0 when unavailable: %s", string(b))
	}
}

func TestThrashSensorLine(t *testing.T) {
	rd := thrashRead{
		psiSomeOK: true, psiFullOK: true, psiUnit: true,
		swapOK: true, swapUnit: true, refaultOK: true, pgscanOK: true,
		unitSwapOK: true, hostSwapOK: true, ramAvailOK: true, ramTotalOK: true, heapOK: true,
	}
	active, missing := thrashSensorLine(rd)
	if len(missing) != 0 {
		t.Fatalf("all-readable: unexpected missing %v", missing)
	}
	if !thrashHas(active, "psi-mem(unit)") || !thrashHas(active, "swap-activity(unit)") {
		t.Fatalf("active: %v", active)
	}
	active, missing = thrashSensorLine(thrashRead{})
	if len(active) != 0 || len(missing) == 0 {
		t.Fatalf("nothing-readable: want no active, some missing; got %v / %v", active, missing)
	}
}

func thrashHas(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Review-round regressions (source flip, gates, loop, status, messages)
// ---------------------------------------------------------------------------

func TestThrashTrackerSourceFlipReBaselines(t *testing.T) {
	var tr thrashTracker
	rd := thrashRead{
		psiSomeTotal: 100, psiFullTotal: 100, psiSomeOK: true, psiFullOK: true,
		psiSomeUnit: true, psiUnit: true,
		swapIn: 50, swapOut: 50, swapOK: true, swapUnit: true,
	}
	tr.rates(rd, thrashT0)
	rd.psiSomeTotal, rd.psiFullTotal = 100+90_000_000, 100+45_000_000
	rd.swapIn = 50 + 9000
	r := tr.rates(rd, thrashT0.Add(90*time.Second))
	if r.fullFrac < 0.49 || r.fullFrac > 0.51 || r.swapInPS < 99 {
		t.Fatalf("same-source rates: full=%v swapIn=%v", r.fullFrac, r.swapInPS)
	}
	// Flip to host counters with much larger cumulative values: the tracker
	// must re-baseline instead of fabricating a huge rate (the false-severe
	// bug: only backward moves are clamped, forward jumps are not).
	rd.psiSomeUnit, rd.psiUnit, rd.swapUnit = false, false, false
	rd.psiSomeTotal, rd.psiFullTotal = 9_000_000_000, 9_000_000_000
	rd.swapIn, rd.swapOut = 900_000_000, 900_000_000
	r = tr.rates(rd, thrashT0.Add(180*time.Second))
	if r.fullOK || r.swapOK {
		t.Fatalf("source flip must report unavailable, got (fullOK=%v swapOK=%v)", r.fullOK, r.swapOK)
	}
	// The next same-source tick yields real rates again.
	rd.psiFullTotal += 45_000_000
	rd.swapIn += 9000
	r = tr.rates(rd, thrashT0.Add(270*time.Second))
	if r.fullFrac < 0.49 || r.fullFrac > 0.51 {
		t.Fatalf("post-flip rates: full=%v", r.fullFrac)
	}
}

func TestThrashTrackerClampsFraction(t *testing.T) {
	var tr thrashTracker
	rd := thrashRead{psiFullTotal: 1, psiFullOK: true, psiUnit: true}
	tr.rates(rd, thrashT0)
	rd.psiFullTotal = 1 + 999_000_000 // absurd delta
	r := tr.rates(rd, thrashT0.Add(time.Second))
	if r.fullFrac != 1.0 {
		t.Fatalf("fraction must clamp to 1.0, got %v", r.fullFrac)
	}
}

func TestThrashAttributionExactHalfIsOurs(t *testing.T) {
	rd := thrashRead{unitSwapOK: true, unitSwapMiB: 1650, hostSwapOK: true, hostSwapUsedMiB: 3300, hostSwapTotalMiB: 3300}
	if attr, share, ok := thrashAttribution(rd); attr != "unit" || !ok || share != 0.5 {
		t.Fatalf("exactly 50%% must be ours: got (%s, %v, %v)", attr, share, ok)
	}
}

func TestThrashMachineSevereBoundaryInclusive(t *testing.T) {
	m := &thrashMachine{state: thrashCalm, stateSince: thrashT0}
	rt := thrashRates{fullFrac: 0.25, fullOK: true} // exactly the severe floor
	m.step(thrashT0, rt, thrashRead{})
	if st := m.step(thrashT0.Add(91*time.Second), rt, thrashRead{}); st.cur != thrashThrashing {
		t.Fatalf("exactly 25%% must be severe: got %v", st.cur)
	}
}

func TestThrashEscalateGates(t *testing.T) {
	home := withTempHome(t)
	t.Cleanup(ResetHotSwapStateForTest)
	now := thrashT0
	rt := thrashRates{fullFrac: 0.5, fullOK: true, swapOK: true, swapInPS: 500, swapOutPS: 500}
	rdUnit := thrashRead{unitSwapOK: true, unitSwapMiB: 3000, hostSwapOK: true, hostSwapUsedMiB: 3300, hostSwapTotalMiB: 3300}

	// self-heal off
	if got := thrashEscalate(now, rt, rdUnit, false, "unit", 0.9, true); got.Action != "alert" || got.Code != "self-heal-off" {
		t.Fatalf("self-heal off: got (%s, %s)", got.Action, got.Code)
	}
	// hot-swap draining: the real drain flag, not the echoed env var
	isHotSwapDraining.Store(true)
	got := thrashEscalate(now, rt, rdUnit, true, "unit", 0.9, true)
	isHotSwapDraining.Store(false)
	if got.Code != "hotswap" {
		t.Fatalf("draining: got %s", got.Code)
	}
	// other-process attribution
	rdOther := thrashRead{unitSwapOK: true, unitSwapMiB: 100, hostSwapOK: true, hostSwapUsedMiB: 3000, hostSwapTotalMiB: 3300}
	if got := thrashEscalate(now, rt, rdOther, true, "other", 0.03, true); got.Code != "attributed-other" {
		t.Fatalf("other: got %s", got.Code)
	}
	// unknown attribution
	if got := thrashEscalate(now, rt, thrashRead{}, true, "unknown", 0, false); got.Code != "attributed-unknown" {
		t.Fatalf("unknown: got %s", got.Code)
	}
	// no supervisor
	t.Setenv("INVOCATION_ID", "")
	t.Setenv("NOTIFY_SOCKET", "")
	if got := thrashEscalate(now, rt, rdUnit, true, "unit", 0.9, true); got.Code != "no-supervisor" {
		t.Fatalf("no supervisor: got %s", got.Code)
	}
	// inside the re-arm window
	t.Setenv("INVOCATION_ID", "test-supervisor")
	if err := recordThrashEscalation(300, now.Add(-time.Minute)); err != nil {
		t.Fatalf("record: %v", err)
	}
	if got := thrashEscalate(now, rt, rdUnit, true, "unit", 0.9, true); got.Code != "rearm" {
		t.Fatalf("rearm: got %s", got.Code)
	}
	// past the window: the restart is taken
	got = thrashEscalate(now.Add(31*time.Minute), rt, rdUnit, true, "unit", 0.9, true)
	if got.Action != "restart" || got.Restarts != 2 {
		t.Fatalf("restart: got (%s, %d)", got.Action, got.Restarts)
	}
	// The computed cap is 0 (nothing running), so the standing 300 from the
	// earlier escalation is preserved rather than erased.
	if cap, ok := activeThrashCap(now.Add(31 * time.Minute)); !ok || cap != 300 {
		t.Fatalf("standing cap must be preserved when the new cap is 0: got (%d, %v)", cap, ok)
	}
	b, err := os.ReadFile(filepath.Join(home, ".urnetwork", "autopilot.jsonl"))
	if err != nil || !strings.Contains(string(b), `"actor":"thrash"`) {
		t.Fatalf("ledger must carry the cycle: err=%v content=%s", err, string(b))
	}
}

// shrinkThrashDurations shrinks the watchdog timing vars for a loop-level
// test and returns the restore func.
func shrinkThrashDurations() func() {
	origInterval, origSustain, origSevere := thrashSampleInterval, thrashSustain, thrashSevereSustain
	origRetry, origRecovery := thrashRetryInterval, thrashRecoveryCheckAfter
	thrashSampleInterval = 5 * time.Millisecond
	thrashSustain = time.Millisecond
	thrashSevereSustain = time.Millisecond
	thrashRetryInterval = time.Millisecond
	thrashRecoveryCheckAfter = time.Hour // keep the recovery line out of the way
	return func() {
		thrashSampleInterval, thrashSustain, thrashSevereSustain = origInterval, origSustain, origSevere
		thrashRetryInterval, thrashRecoveryCheckAfter = origRetry, origRecovery
	}
}

func TestRunThrashWatchdogEscalatesAndExits(t *testing.T) {
	home := withTempHome(t)
	t.Setenv("INVOCATION_ID", "test-supervisor")
	// resolveSelfHealEnabled reads control state first, then the file, then
	// the startup default. Pin both layers: in-memory override (unpersisted)
	// plus the file (the file layer wins unless control state overrides).
	if err := globalControlState.set("proxy_self_heal", "on"); err == nil {
		t.Cleanup(func() { _ = globalControlState.clear("proxy_self_heal") })
	}
	if err := os.MkdirAll(filepath.Join(home, ".urnetwork"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, ".urnetwork", "proxy_self_heal"), []byte("on\n"), 0o600); err != nil {
		t.Fatalf("write override: %v", err)
	}
	ResetHotSwapStateForTest()
	t.Cleanup(ResetHotSwapStateForTest)
	restore := shrinkThrashDurations()
	defer restore()

	calls := 0
	var psiFull, swapIn uint64
	prevRead := thrashReadFn
	thrashReadFn = func() thrashRead {
		calls++
		if calls == 1 {
			// Startup sensor line: calm, all sources readable.
			return thrashRead{psiSomeOK: true, psiFullOK: true, psiSomeUnit: true, psiUnit: true, swapOK: true, swapUnit: true, ramAvailOK: true, ramAvailMiB: 500}
		}
		psiFull += 50_000_000
		swapIn += 100_000
		return thrashRead{
			psiSomeTotal: psiFull, psiFullTotal: psiFull,
			psiSomeOK: true, psiFullOK: true, psiSomeUnit: true, psiUnit: true,
			swapIn: swapIn, swapOut: swapIn, swapOK: true, swapUnit: true,
			unitSwapMiB: 3000, unitSwapOK: true,
			hostSwapUsedMiB: 3300, hostSwapTotalMiB: 3300, hostSwapOK: true,
			heapFrac: 3.1, heapUsedMiB: 2100, heapLimitMiB: 680, heapOK: true,
			ramAvailMiB: 82, ramAvailOK: true,
		}
	}
	defer func() { thrashReadFn = prevRead }()

	exitCode := -1
	prevExit := thrashExitFn
	thrashExitFn = func(code int) { exitCode = code }
	defer func() { thrashExitFn = prevExit }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		runThrashWatchdog(ctx, true)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("watchdog never escalated")
	}
	if exitCode != thrashExitCode {
		t.Fatalf("exit code: got %d, want %d", exitCode, thrashExitCode)
	}
	// The escalation must be persisted: the restarts ring is the anti-loop
	// accounting (the cap itself is 0 here because no proxies are running in
	// the test).
	if st := readThrashCapState(); len(st.Restarts) == 0 {
		t.Fatalf("thrash cap state must record the restart")
	}
	b, err := os.ReadFile(filepath.Join(home, ".urnetwork", "autopilot.jsonl"))
	if err != nil || !strings.Contains(string(b), `"actor":"thrash"`) {
		t.Fatalf("ledger must carry the restart: err=%v content=%s", err, string(b))
	}
}

func TestPressureStatusThrashFields(t *testing.T) {
	home := withTempHome(t)

	f := 0.51
	snap := &thrashSnapshot{State: "thrashing", SinceUnix: thrashT0.Unix(), PSIFull: &f}
	snap.Summary = "memory thrash: test summary"
	globalThrashSnap.Store(snap)
	defer globalThrashSnap.Store(nil)

	var gc gcGovernorState
	writePressureStatus(0.9, map[string]float64{"heap": 0.9}, &gc)
	b, err := os.ReadFile(filepath.Join(home, ".urnetwork", "pressure_status"))
	if err != nil {
		t.Fatalf("read pressure_status: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["thrash_state"] != "thrashing" {
		t.Fatalf("thrash_state: got %v", m["thrash_state"])
	}
	if v, ok := m["psi_mem_full"].(float64); !ok || v != f {
		t.Fatalf("psi_mem_full: got %v (want %v, a number)", m["psi_mem_full"], f)
	}
	if v, ok := m["swap_io_rate"]; !ok || v != nil {
		t.Fatalf("swap_io_rate must be null when the snapshot has none: got %v", v)
	}
	if sum, _ := m["summary"].(string); !strings.Contains(sum, "memory thrash: test summary") {
		t.Fatalf("summary must reuse the thrash sentence: %q", sum)
	}

	// Now with values present: numbers, not nulls.
	swap := 5123.0
	snap2 := &thrashSnapshot{State: "under-pressure", PSIFull: &f, SwapIOPS: &swap}
	globalThrashSnap.Store(snap2)
	writePressureStatus(0.4, map[string]float64{"heap": 0.4}, &gc)
	b, err = os.ReadFile(filepath.Join(home, ".urnetwork", "pressure_status"))
	if err != nil {
		t.Fatalf("read pressure_status (2): %v", err)
	}
	if !strings.Contains(string(b), `"swap_io_rate":5123`) {
		t.Fatalf("swap_io_rate must serialize as a number: %s", string(b))
	}
}

func TestThrashMessagesUnavailableNoFakeZeros(t *testing.T) {
	rt := thrashRates{} // everything unavailable
	rd := thrashRead{heapOK: true, heapFrac: 3.1, heapUsedMiB: 2100, heapLimitMiB: 680, ramAvailOK: false}
	msg := thrashOnsetMsg(rt, rd, 0, false)
	if !strings.Contains(msg, "unreadable") {
		t.Fatalf("onset must say swap activity is unreadable: %s", msg)
	}
	for _, bad := range []string{"0 MB/s", "0.0%"} {
		if strings.Contains(msg, bad) {
			t.Fatalf("onset fabricates a zero (%q): %s", bad, msg)
		}
	}
	cleared := thrashClearedMsg("Thrash cleared", rd, rt)
	if !strings.Contains(cleared, "unknown") || strings.Contains(cleared, "0 MB RAM") {
		t.Fatalf("cleared must not print fake zeros: %s", cleared)
	}
	detail := thrashDetailLine(rt, rd, "unknown")
	if strings.Contains(detail, "0.00") || strings.Contains(detail, "ram 0 MB") {
		t.Fatalf("detail must not print fake zeros: %s", detail)
	}
	warn := thrashEarlyWarnMsg(rd)
	if !strings.Contains(warn, "unknown") || strings.Contains(warn, "0 MB RAM") {
		t.Fatalf("early warn must not print fake zeros: %s", warn)
	}
}

func TestThrashMachineCalmRecoveryIsNotCritical(t *testing.T) {
	m := &thrashMachine{state: thrashCalm, stateSince: thrashT0}
	severe := thrashRates{fullFrac: 0.30, fullOK: true}
	m.step(thrashT0, severe, thrashRead{})
	thrashAt := thrashT0.Add(91 * time.Second)
	m.step(thrashAt, severe, thrashRead{})
	// The thrash ends at minute 14…
	calm := thrashRates{fullFrac: 0, fullOK: true}
	if st := m.step(thrashAt.Add(14*time.Minute), calm, thrashRead{}); st.cur != thrashThrashing {
		t.Fatalf("relax window: want thrashing, got %v", st.cur)
	}
	// …so the 15-minute mark must NOT promote to critical (it is recovering).
	if st := m.step(thrashAt.Add(15*time.Minute+time.Second), calm, thrashRead{}); st.cur != thrashThrashing {
		t.Fatalf("calm recovery must not promote to critical: got %v", st.cur)
	}
}

func TestPressureSummaryOf(t *testing.T) {
	f := 0.51
	globalThrashSnap.Store(&thrashSnapshot{State: "thrashing", PSIFull: &f, Summary: "memory thrash: x"})
	if got := pressureSummaryOf(0.9, map[string]float64{"heap": 0.9}); !strings.Contains(got, "memory pressure 0.90 — memory thrash: x") {
		t.Fatalf("thrashing summary must reuse the thrash sentence: %q", got)
	}
	globalThrashSnap.Store(nil)
	if got := pressureSummaryOf(0.2, map[string]float64{"heap": 0.2}); !strings.HasPrefix(got, "system calm") {
		t.Fatalf("low pressure must read calm: %q", got)
	}
	if got := pressureSummaryOf(0.8, map[string]float64{"heap": 0.8}); !strings.Contains(got, "close to its soft limit") {
		t.Fatalf("heap-driven pressure must name the driver: %q", got)
	}
}

// ---------------------------------------------------------------------------
// Sonnet-round regressions (neutral ticks, escalation cond gate, cap rules)
// ---------------------------------------------------------------------------

func TestThrashMachineNeutralTickHoldsClocks(t *testing.T) {
	m := &thrashMachine{state: thrashCalm, stateSince: thrashT0}
	severe := thrashRates{fullFrac: 0.30, fullOK: true}
	m.step(thrashT0, severe, thrashRead{})
	thrashAt := thrashT0.Add(91 * time.Second)
	m.step(thrashAt, severe, thrashRead{})
	// Unavailable ticks (baseline / source flip) are NEUTRAL: hold state and
	// clocks, never relax and never reset the sustain clocks.
	for i := 0; i < 5; i++ {
		st := m.step(thrashAt.Add(time.Duration(i+1)*time.Minute), thrashRates{}, thrashRead{})
		if st.changed || st.cur != thrashThrashing || st.cond {
			t.Fatalf("neutral tick %d must hold: changed=%v cur=%v cond=%v", i, st.changed, st.cur, st.cond)
		}
	}
	// A READABLE calm continues into the relax ladder from its own window.
	readableCalm := thrashRates{fullFrac: 0, fullOK: true}
	a := thrashAt.Add(10 * time.Minute)
	m.step(a, readableCalm, thrashRead{})
	if st := m.step(a.Add(5*time.Minute+time.Second), readableCalm, thrashRead{}); st.cur != thrashUnderPressure {
		t.Fatalf("readable calm must relax after its window, got %v", st.cur)
	}
}

func TestThrashStepCondFalseDuringRelax(t *testing.T) {
	// B1 regression: the machine sits in thrashing through its relax window,
	// but cond must read false so the watchdog cannot escalate on a
	// recovered box.
	m := &thrashMachine{state: thrashCalm, stateSince: thrashT0}
	severe := thrashRates{fullFrac: 0.30, fullOK: true}
	m.step(thrashT0, severe, thrashRead{})
	thrashAt := thrashT0.Add(91 * time.Second)
	m.step(thrashAt, severe, thrashRead{})
	st := m.step(thrashAt.Add(time.Minute), thrashRates{fullFrac: 0, fullOK: true}, thrashRead{})
	if st.cur != thrashThrashing || st.cond {
		t.Fatalf("relax tick must be thrashing with cond=false, got (cur=%v cond=%v)", st.cur, st.cond)
	}
}

func TestThrashStepCondFlapDoesNotPromoteToCritical(t *testing.T) {
	// 14 minutes of thrash, then calm, then ONE resumed tick: the critical
	// clock must restart with the condition, not count across the calm gap.
	m := &thrashMachine{state: thrashCalm, stateSince: thrashT0}
	severe := thrashRates{fullFrac: 0.30, fullOK: true}
	calm := thrashRates{fullFrac: 0, fullOK: true}
	m.step(thrashT0, severe, thrashRead{})
	thrashAt := thrashT0.Add(91 * time.Second)
	m.step(thrashAt, severe, thrashRead{})
	m.step(thrashAt.Add(14*time.Minute), calm, thrashRead{}) // relax begins
	m.step(thrashAt.Add(15*time.Minute), calm, thrashRead{})
	m.step(thrashAt.Add(16*time.Minute), calm, thrashRead{})
	st := m.step(thrashAt.Add(18*time.Minute), severe, thrashRead{})
	if st.cur != thrashThrashing {
		t.Fatalf("a single resumed tick must not promote to critical, got %v", st.cur)
	}
	if !st.cond {
		t.Fatalf("resumed condition must read cond=true")
	}
}

func TestThrashCapPreserveRules(t *testing.T) {
	withTempHome(t)
	base := thrashT0
	if err := recordThrashEscalation(300, base); err != nil {
		t.Fatal(err)
	}
	// cap 0 keeps the unexpired standing cap.
	if err := recordThrashEscalation(0, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if st := readThrashCapState(); st.Cap != 300 {
		t.Fatalf("unexpired standing cap must be kept, got %d", st.Cap)
	}
	// A looser new cap loses to the tighter standing one.
	if err := recordThrashEscalation(2000, base.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if st := readThrashCapState(); st.Cap != 300 {
		t.Fatalf("tighter standing cap must win, got %d", st.Cap)
	}
	// A tighter new cap wins.
	if err := recordThrashEscalation(100, base.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if st := readThrashCapState(); st.Cap != 100 {
		t.Fatalf("tighter new cap must win, got %d", st.Cap)
	}
	// An EXPIRED standing cap is not resurrected by a cap-0 escalation.
	if err := recordThrashEscalation(0, base.Add(28*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if st := readThrashCapState(); st.Cap != 0 {
		t.Fatalf("expired cap must not be preserved, got %d", st.Cap)
	}
}

func TestThrashRestartsFutureDatedClamped(t *testing.T) {
	now := thrashT0
	st := thrashCapState{Restarts: []int64{now.Add(time.Hour).Unix()}}
	got := thrashRestartsWithin(st, now)
	if len(got) != 1 || got[0] != now.Unix() {
		t.Fatalf("future entry must be clamped to now, got %v", got)
	}
}

func TestThrashEscalatePersistFailureAlert(t *testing.T) {
	home := withTempHome(t)
	t.Setenv("INVOCATION_ID", "test-supervisor")
	// Block the state dir: a FILE where ~/.urnetwork belongs makes MkdirAll
	// fail, so the anti-loop record cannot persist and the restart must NOT
	// proceed (an unthrottled restart loop otherwise).
	if err := os.WriteFile(filepath.Join(home, ".urnetwork"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	rt := thrashRates{fullFrac: 0.5, fullOK: true}
	rdUnit := thrashRead{unitSwapOK: true, unitSwapMiB: 3000, hostSwapOK: true, hostSwapUsedMiB: 3300, hostSwapTotalMiB: 3300}
	got := thrashEscalate(thrashT0, rt, rdUnit, true, "unit", 0.9, true)
	if got.Action != "alert" || got.Code != "persist-failed" {
		t.Fatalf("persist failure must alert, not restart: got (%s, %s)", got.Action, got.Code)
	}
}

func TestThrashRecoveryReportKeyedToRestartRecency(t *testing.T) {
	run := func(restartAgo time.Duration) []string {
		home := withTempHome(t)
		dir := filepath.Join(home, ".urnetwork")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		if err := oomWriteJSON(filepath.Join(dir, "thrash_cap.json"), thrashCapState{
			Cap:         300,
			SetUnix:     now.Add(restartAgo).Unix(),
			ExpiresUnix: now.Add(20 * time.Hour).Unix(),
			Restarts:    []int64{now.Add(restartAgo).Unix()},
		}); err != nil {
			t.Fatal(err)
		}
		restore := shrinkThrashDurations()
		defer restore()
		thrashRecoveryCheckAfter = 5 * time.Millisecond

		prevHook := importantLogHook
		var lines []string
		importantLogHook = func(line string) { lines = append(lines, line) }
		defer func() { importantLogHook = prevHook }()

		prevRead := thrashReadFn
		thrashReadFn = func() thrashRead {
			return thrashRead{psiSomeOK: true, psiFullOK: true, psiSomeUnit: true, psiUnit: true, ramAvailOK: true, ramAvailMiB: 500}
		}
		defer func() { thrashReadFn = prevRead }()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		done := make(chan struct{})
		go func() { runThrashWatchdog(ctx, false); close(done) }()
		time.Sleep(300 * time.Millisecond)
		cancel()
		<-done
		return lines
	}

	found := false
	for _, l := range run(-time.Minute) {
		if strings.Contains(l, "Thrash cleared by the restart") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a restart moments before process start must report its outcome")
	}
	for _, l := range run(-10 * time.Hour) {
		if strings.Contains(l, "Thrash cleared by the restart") {
			t.Fatalf("a stale restarts-ring entry must NOT claim a restart outcome: %q", l)
		}
	}
}
