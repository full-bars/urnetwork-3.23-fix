package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Phase 3 of the self-healing supervisor plan (design doc 3.2 and 8 step 4):
// the observed versus actuating pressure split. The actuating score
// (currentPressure / currentPressureNoCPU) keeps today's behavior exactly;
// the observed score (observedPressureForStatus) records what the sensors
// see even while self-heal is off, and only the status writer reads it.

// TestObservedNonZeroActuatingZeroWhenOff drives the real pressure loop with
// self-heal off and a scripted sample: the actuating reads stay exactly 0
// (every consumer's "do not act") while the observed reads keep climbing with
// the sensors, and the status file carries the observed reading.
func TestObservedNonZeroActuatingZeroWhenOff(t *testing.T) {
	resetGlobalControlStateForTest()
	t.Cleanup(resetGlobalControlStateForTest)
	home := withTempHome(t)
	t.Setenv(adaptiveGCDisableEnv, "off")

	origInterval := pressureSampleInterval
	pressureSampleInterval = 5 * time.Millisecond
	t.Cleanup(func() { pressureSampleInterval = origInterval })

	origPressure, origNoCPU := currentPressure(), currentPressureNoCPU()
	origObserved, origObservedNoCPU := observedPressureForStatus(), observedPressureNoCPUForStatus()
	t.Cleanup(func() {
		setPressure(origPressure)
		setPressureNoCPU(origNoCPU)
		setObservedPressure(origObserved)
		setObservedPressureNoCPU(origObservedNoCPU)
		applyPressureMemoryBudget(0)
	})

	origSample := collectPressureSample
	// PSIMem 30 is raw 0.4 (pinned by TestPressureDecide's rise-from-zero row).
	collectPressureSample = func() pressureSample { return pressureSample{PSIMem: 30} }
	t.Cleanup(func() { collectPressureSample = origSample })

	if resolveSelfHealEnabled(false) {
		t.Fatalf("resolveSelfHealEnabled(false) = true, want false")
	}

	setPressure(0.95)
	setPressureNoCPU(0.95)
	// Zero the observed atomics too: a previous test (for example the status
	// golden) may have left them high, which would let the climb-wait below
	// exit before the loop has ticked.
	setObservedPressure(0)
	setObservedPressureNoCPU(0)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPressureMonitor(ctx, false)
	}()
	defer func() {
		cancel()
		<-done
	}()

	// ewma(0, 0.4) is 0.2 on the first tick, 0.35 on the third and 0.375 on
	// the fourth, so a value above 0.35 proves the observed smoothing state
	// carries across ticks (a memoryless filter would stay at 0.2) and stays
	// at or below the raw 0.4.
	deadline := time.Now().Add(10 * time.Second)
	for observedPressureForStatus() <= 0.35 {
		if time.Now().After(deadline) {
			t.Fatalf("observed score never climbed past the first-tick value: observed=%v actuating=%v",
				observedPressureForStatus(), currentPressure())
		}
		time.Sleep(time.Millisecond)
	}

	// Actuating reads stay exactly 0: that is what every consumer sees.
	if got := currentPressure(); got != 0 {
		t.Fatalf("actuating score = %v while self-heal is off, want exactly 0", got)
	}
	if got := currentPressureNoCPU(); got != 0 {
		t.Fatalf("actuating no-CPU score = %v while self-heal is off, want exactly 0", got)
	}
	if got := observedPressureForStatus(); got <= 0.35 || got > 0.4 {
		t.Fatalf("observed score = %v, want in (0.35, 0.4]", got)
	}
	if got := observedPressureNoCPUForStatus(); got <= 0.35 || got > 0.4 {
		t.Fatalf("observed no-CPU score = %v, want in (0.35, 0.4]", got)
	}
	// An actuator fed the actuating value stays calm; fed the observed value
	// it would throttle. The two readings are genuinely different.
	if got := scaledProbeConcurrency(currentPressure()); got != proxyProbeConcurrency {
		t.Fatalf("scaledProbeConcurrency(actuating 0) = %d, want max %d", got, proxyProbeConcurrency)
	}
	if got := scaledProbeConcurrency(observedPressureForStatus()); got >= proxyProbeConcurrency {
		t.Fatalf("scaledProbeConcurrency(observed) = %d, want below max %d", got, proxyProbeConcurrency)
	}

	// The status file carries the observed reading while "score" stays the
	// actuating 0. Retry the read: the loop rewrites the file every tick.
	statusPath := filepath.Join(home, ".urnetwork", "pressure_status")
	var status struct {
		Score    float64 `json:"score"`
		Observed float64 `json:"observed_score"`
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(statusPath)
		if err == nil && json.Unmarshal(b, &status) == nil && status.Observed > 0.35 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pressure_status never carried the observed reading: score=%v observed=%v err=%v",
				status.Score, status.Observed, err)
		}
		time.Sleep(time.Millisecond)
	}
	if status.Score != 0 {
		t.Fatalf("pressure_status score = %v, want the actuating 0", status.Score)
	}
}

// TestPressureStatusObservedJSONGolden pins the pressure_status JSON shape
// byte for byte (minus the volatile "updated" stamp) for both shapes: the
// self-heal-off shape (score 0, live observed) and the self-heal-on shape
// (observed equals score). The separator inside the summary sentence is the
// program's own (see pressureSummaryOf), reproduced here so the pin is exact.
func TestPressureStatusObservedJSONGolden(t *testing.T) {
	home := withTempHome(t)

	origPressure, origObserved := currentPressure(), observedPressureForStatus()
	t.Cleanup(func() {
		setPressure(origPressure)
		setObservedPressure(origObserved)
	})

	prevSnap := globalThrashSnap.Load()
	globalThrashSnap.Store(nil) // deterministic thrash fields: null / empty
	t.Cleanup(func() { globalThrashSnap.Store(prevSnap) })

	cases := []struct {
		name     string
		score    float64
		observed float64
		want     string
	}{
		{
			name:     "self-heal-off-shape",
			score:    0,
			observed: 0.72,
			want: `{
  "components": {
    "psi_mem": 0.72
  },
  "gc_state": "normal",
  "heap_frac": 0,
  "observed_score": 0.72,
  "psi_mem_full": null,
  "score": 0,
  "summary": "memory pressure 0.72 — tasks are stalling on memory",
  "swap_io_rate": null,
  "target_pool": 0,
  "thrash_state": ""
}`,
		},
		{
			name:     "self-heal-on-shape",
			score:    0.5,
			observed: 0.5,
			want: `{
  "components": {
    "psi_mem": 0.5
  },
  "gc_state": "normal",
  "heap_frac": 0,
  "observed_score": 0.5,
  "psi_mem_full": null,
  "score": 0.5,
  "summary": "memory pressure 0.50 — tasks are stalling on memory",
  "swap_io_rate": null,
  "target_pool": 0,
  "thrash_state": ""
}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setPressure(tc.score)
			setObservedPressure(tc.observed)

			var gc gcGovernorState
			writePressureStatus(tc.score, map[string]float64{"psi_mem": tc.observed}, &gc)

			b, err := os.ReadFile(filepath.Join(home, ".urnetwork", "pressure_status"))
			if err != nil {
				t.Fatalf("read pressure_status: %v", err)
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatalf("unmarshal pressure_status: %v", err)
			}
			if _, ok := m["updated"].(string); !ok {
				t.Fatalf("updated stamp missing or not a string: %v", m["updated"])
			}
			delete(m, "updated")
			got, err := json.MarshalIndent(m, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("pressure_status JSON drifted from the pinned shape:\n--- got ---\n%s\n--- want ---\n%s", got, tc.want)
			}
		})
	}
}

// TestConsumerCallSitesReadActuatingPressure is the reflection-free breaker
// for the observed/actuating split: it parses the consumer sources and pins
// which accessor each file uses. Actuators must keep reading the actuating
// value (currentPressure / currentPressureNoCPU); the observed accessors are
// for status only, and their only production reader is writePressureStatus.
//
// The metrics and telemetry surfaces (metrics_provider.go,
// bandwidth_reporter.go, node_snapshot.go, proxy_grade_summary.go) are pinned
// to the ACTUATING value on purpose for this phase: moving them to the
// observed reading is a deliberate, visible change tracked separately.
func TestConsumerCallSitesReadActuatingPressure(t *testing.T) {
	// Consumer files the design doc names (section 3.2), with the exact
	// number of call sites of each accessor. A change here must be a
	// deliberate edit, never an accident.
	pins := []struct {
		file              string
		wantPressure      int
		wantPressureNoCPU int
		wantObserved      int
		wantObservedNoCPU int
	}{
		{"proxy_url_source.go", 5, 0, 0, 0},
		{"proxy_probe.go", 1, 0, 0, 0},
		{"proxy_table_probe.go", 1, 0, 0, 0},
		{"proxy_grade_paid.go", 2, 0, 0, 0},
		// Telemetry surfaces and actuator-adjacent readers found in review:
		// all read the actuating value and none may move to observed.
		{"bandwidth_reporter.go", 1, 0, 0, 0},
		{"metrics_provider.go", 1, 0, 0, 0},
		{"node_snapshot.go", 1, 0, 0, 0},
		{"proxy_grade_summary.go", 1, 0, 0, 0},
		// resource_pressure.go holds the accessor definitions and the status
		// writer, so its whole-file pin allows exactly one observed read
		// (writePressureStatus); runPoolController is pinned separately below.
		{"resource_pressure.go", 1, 1, 1, 0},
	}
	for _, pin := range pins {
		got := accessorCallSites(t, pin.file)
		if got["currentPressure"] != pin.wantPressure ||
			got["currentPressureNoCPU"] != pin.wantPressureNoCPU ||
			got["observedPressureForStatus"] != pin.wantObserved ||
			got["observedPressureNoCPUForStatus"] != pin.wantObservedNoCPU {
			t.Errorf("%s: pressure accessor call sites changed: got %v, want currentPressure=%d currentPressureNoCPU=%d observedPressureForStatus=%d observedPressureNoCPUForStatus=%d",
				pin.file, got, pin.wantPressure, pin.wantPressureNoCPU, pin.wantObserved, pin.wantObservedNoCPU)
		}
	}

	// No non-test provider file other than resource_pressure.go may read an
	// observed accessor at all: an actuator reading observed would act while
	// self-heal is off.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "resource_pressure.go" {
			continue
		}
		got := accessorCallSites(t, f)
		if got["observedPressureForStatus"] != 0 || got["observedPressureNoCPUForStatus"] != 0 {
			t.Errorf("%s reads an observed-pressure accessor (%v); actuators must read currentPressure()/currentPressureNoCPU()", f, got)
		}
	}

	// The AIMD pool controller is an actuator: it reads the actuating score
	// and its CPU-excluded companion, and never the observed ones.
	poolBody := funcBody(t, "resource_pressure.go", "func runPoolController(")
	if n := strings.Count(poolBody, "currentPressure()"); n != 1 {
		t.Errorf("runPoolController: currentPressure() call sites = %d, want 1", n)
	}
	if n := strings.Count(poolBody, "currentPressureNoCPU()"); n != 1 {
		t.Errorf("runPoolController: currentPressureNoCPU() call sites = %d, want 1", n)
	}
	if strings.Contains(poolBody, "observedPressureForStatus(") || strings.Contains(poolBody, "observedPressureNoCPUForStatus(") {
		t.Errorf("runPoolController reads an observed-pressure accessor; the pool controller is an actuator")
	}

	// The status writer is the single production reader of the observed
	// reading ("observed for status only", design doc 3.2).
	statusBody := funcBody(t, "resource_pressure.go", "func writePressureStatus(")
	if n := strings.Count(statusBody, "observedPressureForStatus()"); n != 1 {
		t.Errorf("writePressureStatus: observedPressureForStatus() call sites = %d, want exactly 1", n)
	}
	if strings.Contains(statusBody, "observedPressureNoCPUForStatus(") {
		t.Errorf("writePressureStatus must not read the CPU-excluded observed accessor")
	}
}

// accessorCallSites counts, per accessor, every IDENTIFIER reference in one
// source file (calls, function values, wrappers): parsing with go/parser and
// inspecting the AST, so comments and strings cannot spoof the count and a
// bare reference like `pressure: currentPressure,` cannot slip through. Only
// the declaration's own name is skipped.
func accessorCallSites(t *testing.T, path string) map[string]int {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	names := map[string]bool{
		"currentPressure":                true,
		"currentPressureNoCPU":           true,
		"observedPressureForStatus":      true,
		"observedPressureNoCPUForStatus": true,
	}
	declPos := map[token.Pos]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if fd, ok := n.(*ast.FuncDecl); ok && fd.Name != nil {
			declPos[fd.Name.Pos()] = true
		}
		return true
	})
	counts := map[string]int{}
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && names[id.Name] && !declPos[id.Pos()] {
			counts[id.Name]++
		}
		return true
	})
	return counts
}

// funcBody returns the source text of one top-level function, from its
// declaration line to the closing brace at column 0. Textual, no reflection.
func funcBody(t *testing.T, path, decl string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(b), "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, decl) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s: declaration %q not found", path, decl)
	}
	for i := start + 1; i < len(lines); i++ {
		if lines[i] == "}" {
			return strings.Join(lines[start:i+1], "\n")
		}
	}
	t.Fatalf("%s: closing brace for %q not found", path, decl)
	return ""
}

// TestObservedSplitToggleOffToOnRestartsActuatingFromZero pins the runtime
// toggle neutrality: while off, observed carries its own history; the first
// tick after self-heal turns on must restart the ACTUATING value from zero
// (ewma(0, raw) = 0.2 for raw 0.4), never inherit the observed carry.
func TestObservedSplitToggleOffToOnRestartsActuatingFromZero(t *testing.T) {
	resetGlobalControlStateForTest()
	t.Cleanup(resetGlobalControlStateForTest)
	withTempHome(t)
	t.Setenv(adaptiveGCDisableEnv, "off")
	// The runtime self-heal switch is the control state key proxy_self_heal
	// (resolved every tick); the URNETWORK_SELF_HEAL env is a startup-only
	// input and cannot toggle a running loop.

	origInterval := pressureSampleInterval
	pressureSampleInterval = 200 * time.Millisecond
	t.Cleanup(func() { pressureSampleInterval = origInterval })

	origSample := collectPressureSample
	collectPressureSample = func() pressureSample { return pressureSample{PSIMem: 30} }
	t.Cleanup(func() { collectPressureSample = origSample })

	origPressure, origNoCPU := currentPressure(), currentPressureNoCPU()
	origObserved, origObservedNoCPU := observedPressureForStatus(), observedPressureNoCPUForStatus()
	t.Cleanup(func() {
		setPressure(origPressure)
		setPressureNoCPU(origNoCPU)
		setObservedPressure(origObserved)
		setObservedPressureNoCPU(origObservedNoCPU)
		applyPressureMemoryBudget(0)
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPressureMonitor(ctx, false)
	}()
	defer func() {
		cancel()
		<-done
	}()

	// Off phase: observed climbs past 0.35 while the actuating reads stay 0.
	deadline := time.Now().Add(10 * time.Second)
	for observedPressureForStatus() <= 0.35 {
		if time.Now().After(deadline) {
			t.Fatalf("observed never climbed while off: %v", observedPressureForStatus())
		}
		time.Sleep(time.Millisecond)
	}
	if got := currentPressure(); got != 0 {
		t.Fatalf("actuating while off = %v, want 0", got)
	}

	// Toggle on: the first on-tick publishes ewma(0, 0.4) = 0.2 and it holds
	// for a whole 200ms interval, so the 1ms poll cannot miss it.
	if err := globalControlState.set("proxy_self_heal", "on"); err != nil {
		t.Fatalf("set proxy_self_heal on: %v", err)
	}
	var first float64
	onDeadline := time.Now().Add(10 * time.Second)
	for {
		if v := currentPressure(); v > 0 {
			first = v
			break
		}
		if time.Now().After(onDeadline) {
			t.Fatal("actuating never rose after the toggle")
		}
		time.Sleep(time.Millisecond)
	}
	if first < 0.15 || first > 0.25 {
		t.Fatalf("first actuating value after the toggle = %v, want the zero-based ewma 0.2 (the observed carry must not leak into actuating)", first)
	}

	// On path: observed equals actuating (design doc 3.2). Poll for equality
	// because two separate atomic loads can straddle a tick boundary.
	eqDeadline := time.Now().Add(10 * time.Second)
	for {
		a, o := currentPressure(), observedPressureForStatus()
		if a == o && a > 0.25 {
			break
		}
		if time.Now().After(eqDeadline) {
			t.Fatalf("on path: observed and actuating never equalized (actuating=%v observed=%v)", currentPressure(), observedPressureForStatus())
		}
		time.Sleep(time.Millisecond)
	}

	// Toggle back off: actuating returns to exactly 0 on the next tick while
	// observed must continue from its history, not drop to 0.
	if err := globalControlState.set("proxy_self_heal", "off"); err != nil {
		t.Fatalf("set proxy_self_heal off: %v", err)
	}
	offDeadline := time.Now().Add(10 * time.Second)
	for currentPressure() != 0 {
		if time.Now().After(offDeadline) {
			t.Fatalf("actuating never returned to 0 after toggling off: %v", currentPressure())
		}
		time.Sleep(time.Millisecond)
	}
	if got := observedPressureForStatus(); got == 0 {
		t.Fatal("observed dropped to 0 after toggling off; it must continue from its history")
	}
}

// TestObservedPressureNoCPUSplitUnderCPULoad: with self-heal off and a
// CPU-only sample, the observed score rises with CPU pressure while the
// CPU-excluded observed companion stays at zero.
func TestObservedPressureNoCPUSplitUnderCPULoad(t *testing.T) {
	resetGlobalControlStateForTest()
	t.Cleanup(resetGlobalControlStateForTest)
	withTempHome(t)
	t.Setenv(adaptiveGCDisableEnv, "off")
	setObservedPressure(0)
	setObservedPressureNoCPU(0)

	origInterval := pressureSampleInterval
	pressureSampleInterval = 5 * time.Millisecond
	t.Cleanup(func() { pressureSampleInterval = origInterval })

	origSample := collectPressureSample
	// PSICPU 55 with 8 cores is raw 0.9 (pinned by TestPressureDecide's
	// cpu-only row); the CPU-excluded companion's raw is 0.
	collectPressureSample = func() pressureSample { return pressureSample{PSICPU: 55, Cores: 8} }
	t.Cleanup(func() { collectPressureSample = origSample })

	origObserved, origObservedNoCPU := observedPressureForStatus(), observedPressureNoCPUForStatus()
	t.Cleanup(func() {
		setObservedPressure(origObserved)
		setObservedPressureNoCPU(origObservedNoCPU)
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPressureMonitor(ctx, false)
	}()
	defer func() {
		cancel()
		<-done
	}()

	deadline := time.Now().Add(10 * time.Second)
	for observedPressureForStatus() <= 0.35 {
		if time.Now().After(deadline) {
			t.Fatalf("observed never rose under CPU-only load: %v", observedPressureForStatus())
		}
		time.Sleep(time.Millisecond)
	}
	if got := observedPressureNoCPUForStatus(); got != 0 {
		t.Fatalf("CPU-excluded observed = %v under CPU-only load, want 0", got)
	}
	if got := currentPressure(); got != 0 {
		t.Fatalf("actuating = %v while off, want 0", got)
	}
}
