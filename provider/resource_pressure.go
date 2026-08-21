package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

// The pressure system converts raw resource signals into one smoothed score
// in [0,1] that every self-heal actuator consumes. The anchor constants
// below are properties of the metrics themselves (e.g. "a box stalled on
// memory 60% of the time is exhausted") — they are NOT per-server capacity
// tuning, which is exactly what this system exists to eliminate.
const (
	pressureSampleInterval = 30 * time.Second

	// PSI `some avgXX` is "% of wall time at least one task stalled on this
	// resource". 10% = noticeable contention, 60% = severe. Same meaning on
	// any core count — PSI is self-normalizing.
	psiRampLo = 10.0
	psiRampHi = 60.0

	// MemAvailable/MemTotal: plenty of page cache headroom above 25%,
	// reclaim death spiral territory below 5%.
	memAvailRampLo = 0.25 // score 0 at or above this fraction free
	memAvailRampHi = 0.05 // score 1 at or below this fraction free

	// loadavg per core (fallback sensor where PSI is unavailable).
	loadRampLo = 1.0
	loadRampHi = 3.0

	// Self-signals: the provider's own runaway growth. LA1 melted down at
	// ~31k goroutines on 1.6GB.
	goroutineRampLo = 5000
	goroutineRampHi = 25000
	heapRampLo      = 0.60 // fraction of the max-memory soft limit
	heapRampHi      = 0.90

	// Emergency pins: bypass EWMA smoothing entirely.
	emergencyHeapFrac   = 0.90
	emergencyGoroutines = 25000

	// Asymmetric EWMA: react to onset within a sample or two, take minutes
	// of sustained calm to relax. This is the hysteresis.
	ewmaAlphaRise  = 0.5
	ewmaAlphaDecay = 0.1
)

// globalPressure holds the current smoothed score as float64 bits. Zero
// (its natural initial value) means "no pressure" — when the monitor isn't
// running (self-heal off), every consumer sees 0 and behaves exactly like
// the pre-pressure code.
var globalPressure atomic.Uint64

func currentPressure() float64 { return math.Float64frombits(globalPressure.Load()) }
func setPressure(v float64)    { globalPressure.Store(math.Float64bits(v)) }

// pressureSample is one raw reading of every sensor. Zero values mean "no
// data" and normalize to zero pressure (fail-open).
type pressureSample struct {
	PSIMem       float64 // /proc/pressure/memory some avg60 (percent)
	PSICPU       float64 // /proc/pressure/cpu some avg60 (percent)
	MemAvailFrac float64 // MemAvailable/MemTotal; 0 = unknown
	LoadPerCore  float64 // loadavg1 / NumCPU; 0 = unknown
	Goroutines   int
	HeapFrac     float64 // heap in use / max-memory soft limit; 0 = no limit set
	SensorErrs   map[string]error
}

// normalizeRamp maps v onto [0,1] linearly between lo and hi. Works for
// inverted ramps (lo > hi), where smaller v means more pressure.
func normalizeRamp(v, lo, hi float64) float64 {
	if lo == hi {
		return 0
	}
	t := (v - lo) / (hi - lo)
	if t < 0 {
		return 0
	}
	if t > 1 {
		return 1
	}
	return t
}

// computePressure converts one sample into a score plus its per-component
// breakdown. The worst component wins: averaging would dilute a memory
// crisis with a healthy CPU reading.
func computePressure(s pressureSample) (float64, map[string]float64) {
	comps := map[string]float64{
		"psi_mem": normalizeRamp(s.PSIMem, psiRampLo, psiRampHi),
		"psi_cpu": normalizeRamp(s.PSICPU, psiRampLo, psiRampHi),
		"load":    normalizeRamp(s.LoadPerCore, loadRampLo, loadRampHi),
		"goro":    normalizeRamp(float64(s.Goroutines), goroutineRampLo, goroutineRampHi),
	}
	if s.MemAvailFrac > 0 {
		comps["mem"] = normalizeRamp(s.MemAvailFrac, memAvailRampLo, memAvailRampHi)
	}
	if s.HeapFrac > 0 {
		comps["heap"] = normalizeRamp(s.HeapFrac, heapRampLo, heapRampHi)
	}

	// Emergency pin: self-inflicted blowout bypasses smoothing.
	if (s.HeapFrac >= emergencyHeapFrac && s.HeapFrac > 0) || s.Goroutines >= emergencyGoroutines {
		return 1.0, comps
	}

	score := 0.0
	for _, v := range comps {
		if v > score {
			score = v
		}
	}
	return score, comps
}

// ewmaUpdate applies asymmetric exponential smoothing.
func ewmaUpdate(prev, raw float64) float64 {
	alpha := ewmaAlphaDecay
	if raw > prev {
		alpha = ewmaAlphaRise
	}
	return prev + alpha*(raw-prev)
}

// parsePSISome extracts avg10 and avg60 from the "some" line of a
// /proc/pressure file.
func parsePSISome(content string) (avg10, avg60 float64, err error) {
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		for _, field := range strings.Fields(line)[1:] {
			k, v, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			switch k {
			case "avg10":
				avg10, err = strconv.ParseFloat(v, 64)
			case "avg60":
				avg60, err = strconv.ParseFloat(v, 64)
			}
			if err != nil {
				return 0, 0, err
			}
		}
		return avg10, avg60, nil
	}
	return 0, 0, fmt.Errorf("no 'some' line in PSI content")
}

func readPSI(resource string) (avg60 float64, err error) {
	b, err := os.ReadFile("/proc/pressure/" + resource)
	if err != nil {
		return 0, err
	}
	_, avg60, err = parsePSISome(string(b))
	return avg60, err
}

// readMemAvailFrac returns the available memory fraction the provider can see,
// taking the tighter of host MemAvailable and cgroup headroom against the
// effective RAM. Host-only MemAvailable misleads inside Docker, where
// /proc/meminfo reflects host RAM rather than the container limit.
func readMemAvailFrac() (float64, error) {
	ram := detectEffectiveRAMLimitBytes()
	host := readMemAvailableMiB()
	if host < 0 && ram <= 0 {
		return 0, fmt.Errorf("cannot read host memory")
	}
	var availMiB float64
	cgroup := readCgroupAvailableMiB()
	// Use >= for host so a legitimately read MemAvailable==0 (memory fully
	// exhausted, right before OOM) still counts as max pressure rather than
	// indistinguishable-from-no-data. readMemAvailableMiB returns -1 on error,
	// so 0 is real data.
	switch {
	case host >= 0 && cgroup >= 0:
		availMiB = min(float64(host), float64(cgroup))
	case host >= 0:
		availMiB = float64(host)
	case cgroup >= 0:
		availMiB = float64(cgroup)
	default:
		return 0, fmt.Errorf("cannot read host or cgroup memory availability")
	}
	if ram <= 0 {
		return 0, fmt.Errorf("no RAM baseline for fraction")
	}
	return max(0, availMiB) * 1024 * 1024 / float64(ram), nil
}

// collectPressureSample reads every sensor, recording errors per-sensor so
// one missing source (PSI on old kernels, everything on Windows/macOS)
// never blanks the others. Self-signals always work.
func collectPressureSample() pressureSample {
	s := pressureSample{SensorErrs: map[string]error{}, Goroutines: runtime.NumGoroutine()}

	if v, err := readPSI("memory"); err == nil {
		s.PSIMem = v
	} else {
		s.SensorErrs["psi_mem"] = err
	}
	if v, err := readPSI("cpu"); err == nil {
		s.PSICPU = v
	} else {
		s.SensorErrs["psi_cpu"] = err
	}
	if v, err := readMemAvailFrac(); err == nil {
		s.MemAvailFrac = v
	} else {
		s.SensorErrs["mem"] = err
	}
	if l1, _, err := getSystemLoad(); err == nil && runtime.NumCPU() > 0 {
		s.LoadPerCore = l1 / float64(runtime.NumCPU())
	} else if err != nil {
		s.SensorErrs["load"] = err
	}
	// Heap fraction of the memory budget. The numerator is the amount of
	// runtime-managed memory the limit actually counts (Sys minus released),
	// which includes goroutine stacks and runtime overhead, not just live
	// heap. The denominator is the current GOMEMLIMIT soft limit if one is set;
	// otherwise it falls back to the effective RAM so the heap sensor and the
	// emergency pin still work even in a process with no finite limit.
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	used := float64(ms.Sys - ms.HeapReleased)
	limit := debug.SetMemoryLimit(-1)
	if limit <= 0 || limit >= math.MaxInt64 {
		if ram := detectEffectiveRAMLimitBytes(); ram > 0 {
			limit = ram
		}
	}
	if limit > 0 {
		s.HeapFrac = used / float64(limit)
	}
	return s
}

// pressureRegime buckets the score for change-only logging.
func pressureRegime(score float64) int {
	switch {
	case score >= 0.75:
		return 3
	case score >= 0.5:
		return 2
	case score >= 0.25:
		return 1
	default:
		return 0
	}
}

// ---------------------------------------------------------------------------
// Adaptive GC governor (consolidated single writer)
// ---------------------------------------------------------------------------
// One controller owns debug.SetGCPercent for the whole process (the "one owner"
// rule from the design review). It is fed by both memory signals and tightens to
// the tighter of the two: the process heap fraction (this process vs its own
// limit) and host available RAM in MiB (the former eco monitor's signal, kept in
// absolute MiB so small memory-fragile boxes keep the exact protection the eco
// monitor gave them).
//
// The separate runEcoMemoryMonitor loop is retired; its host-RAM safety is folded
// in here. Because this is the only writer, every mode is eligible to run it
// (baseline no profile, auto Tier 1-4, turbo, eco), and the "two writers per box"
// constraint that forced the old mode split disappears.
//
// Off switches (rollback): an operator-set GOGC env means we never touch the
// knob; the URNETWORK_ADAPTIVE_GC kill switch (0/false/off/no) disables the
// whole governor. It is on by default. The governor only ever tightens (lowers
// GOGC); it never raises GOGC above baseline, and release happens only after
// four consecutive calm samples, one level at a time.
const (
	adaptiveGCDisableEnv = "URNETWORK_ADAPTIVE_GC"
	gcSubtickInterval    = 10 * time.Second

	// Host available-MiB thresholds kept from the retired eco monitor so
	// small boxes lose nothing (absolute MiB, not a fraction).
	ecoCriticalMiB int64 = 150
	ecoPressureMiB int64 = 300
)

// gcTightening is true while the governor is actively tightening (level>0).
// Pool GROWTH is frozen while true; shrinking is still allowed.
var gcTightening atomic.Bool

// gcGovernorState tracks the single GC writer's hysteresis.
type gcGovernorState struct {
	baselineGOGC         int
	currentGOGC          int
	level                int // 0 normal,1 tighten,2 hard,3 critical
	consecutiveCalmCount int
	lastTightenAction    string
	gcStateName          string
	lastHeapFrac         float64
	lastHostAvailMiB     int64
}

// gcAdaptiveEnabled reports whether the governor may act at all this run:
// default ON (memory-safety actuator), unless the operator set GOGC (never
// touch their knob) or flipped the explicit kill switch.
func gcAdaptiveEnabled() bool {
	if os.Getenv("GOGC") != "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(adaptiveGCDisableEnv))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// readLiveHeapBytes returns the runtime's live-heap estimate (non-STW) via
// /gc/heap/live:bytes. Returns 0 if unavailable.
func readLiveHeapBytes() uint64 {
	samples := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	metrics.Read(samples)
	if samples[0].Value.Kind() == metrics.KindUint64 {
		return samples[0].Value.Uint64()
	}
	return 0
}

// readGOGCPercent returns the current GOGC percentage via the non-mutating
// /gc/gogc:percent runtime/metrics sample, and ok=false if unavailable.
func readGOGCPercent() (int, bool) {
	samples := []metrics.Sample{{Name: "/gc/gogc:percent"}}
	metrics.Read(samples)
	if samples[0].Value.Kind() == metrics.KindUint64 {
		return int(samples[0].Value.Uint64()), true
	}
	return 0, false
}

// liveHeapFrac is the raw live-heap fraction of the memory budget, for the
// fast 10s subtick. Same numerator/denominator convention as the sensor.
func liveHeapFrac() float64 {
	live := readLiveHeapBytes()
	if live == 0 {
		return 0
	}
	limit := debug.SetMemoryLimit(-1)
	if limit <= 0 || limit >= math.MaxInt64 {
		if ram := detectEffectiveRAMLimitBytes(); ram > 0 {
			limit = ram
		}
	}
	if limit <= 0 {
		return 0
	}
	return float64(live) / float64(limit)
}

// hostAvailMiB returns the tighter of host and cgroup available memory, or -1
// if unavailable (the consolidated controller treats -1 as "no host signal").
func hostAvailMiB() int64 {
	h := readMemAvailableMiB()
	if h < 0 {
		return -1
	}
	if c := readCgroupAvailableMiB(); c >= 0 && c < h {
		return c
	}
	return h
}

// applyGCLevel writes the GOGC for the current level (min with baseline so we
// never raise GOGC above the operator baseline), logs, and updates gcTightening.
func applyGCLevel(state *gcGovernorState) {
	gogc := state.baselineGOGC
	name := "normal"
	switch state.level {
	case 1:
		gogc = min(state.baselineGOGC, 50)
		name = "tighten"
	case 2:
		gogc = min(state.baselineGOGC, 25)
		name = "hard"
	case 3:
		gogc = min(state.baselineGOGC, 10)
		name = "critical"
		// Return memory to the OS at the critical state.
		debug.FreeOSMemory()
	}
	state.gcStateName = name
	if gogc != state.currentGOGC {
		state.lastTightenAction = fmt.Sprintf("%s_gogc%d_heap%.2f", name, gogc, state.lastHeapFrac)
		state.currentGOGC = gogc
		debug.SetGCPercent(gogc)
	}
	gcTightening.Store(state.level > 0)
}

// gcGovernor runs the consolidated single-writer hysteresis on one sample.
// heapFrac is raw; hostAvail is available MiB (-1 = no host signal / subtick);
// psiCPU is the normalized CPU PSI for the veto.
func gcGovernor(heapFrac float64, hostAvail int64, psiCPU float64, canRelease bool, state *gcGovernorState) {
	if !gcAdaptiveEnabled() {
		return
	}

	// Heap level from the raw fraction.
	heapLevel := 0
	switch {
	case heapFrac >= 0.92:
		heapLevel = 3
	case heapFrac >= 0.80:
		heapLevel = 2
	case heapFrac >= 0.70:
		heapLevel = 1
	}
	// CPU veto: a CPU-bound episode must not drive heap tightening; memory wins
	// priority only above 0.85.
	if psiCPU > 0.8 && heapFrac < 0.85 {
		heapLevel = 0
	}

	// Host-RAM level (former eco signal). No CPU veto here: low host RAM is real
	// regardless of CPU load and must still tighten.
	hostLevel := 0
	if hostAvail >= 0 {
		switch {
		case hostAvail <= ecoCriticalMiB:
			hostLevel = 3
		case hostAvail <= ecoPressureMiB:
			hostLevel = 2
		}
	}

	state.lastHeapFrac = heapFrac
	state.lastHostAvailMiB = hostAvail

	// Tighter of the two wins.
	target := heapLevel
	if hostLevel > target {
		target = hostLevel
	}

	if target > state.level {
		// Tighten immediately on the raw reading.
		state.level = target
		state.consecutiveCalmCount = 0
		applyGCLevel(state)
	} else if state.level > 0 && target < state.level {
		// Calmer than the current level: progress toward release. Only the full
		// 30s sweep (canRelease) may count calm samples and actually release;
		// the subtick is host-blind and must NOT reset an in-progress release
		// streak just because it happens to observe a calm heap on its own
		// (live-heap) numerator, nor may it increment it (it lacks the host
		// signal that justified the tighten). So when canRelease is false we
		// leave the streak untouched entirely.
		if canRelease {
			state.consecutiveCalmCount++
			if state.consecutiveCalmCount >= 4 {
				state.level--
				state.consecutiveCalmCount = 0
				applyGCLevel(state)
			}
		}
	} else {
		state.consecutiveCalmCount = 0
	}
}

// runPressureMonitor samples sensors every pressureSampleInterval, smooths
// the score, publishes it, and logs on regime changes. When self-heal is
// off it publishes 0 and idles (cheap tick, no sensor reads), so toggling
// on at runtime starts sensing within one interval.
//
// It also owns the single GC writer (consolidated adaptive GC): a 10s heap
// subtick reacts to heap spikes faster than the 30s sweep, and the full
// sweep merges both heap + host-RAM signals. Both tickers run in this one
// goroutine so the shared gcGovernorState has no concurrent access.
func runPressureMonitor(ctx context.Context, selfHealEnabled bool) {
	// Log the active sensor set once at startup.
	first := collectPressureSample()
	active := []string{"goro"}
	for _, name := range []string{"psi_mem", "psi_cpu", "mem", "load"} {
		if _, bad := first.SensorErrs[name]; !bad {
			active = append(active, name)
		}
	}
	tlog("[proxy][pressure] monitor started, sensors: %s (self-heal %v)\n",
		strings.Join(active, ","), resolveSelfHealEnabled(selfHealEnabled))

	// Initialize the GC governor state: capture baseline once after all
	// static tuning. Only apply the URNETWORK_BASELINE_GOGC override when the
	// governor is actually enabled for this run; when the operator set GOGC or
	// flipped the kill switch, we never write the GC knob (the "operator GOGC /
	// kill switch always wins" invariant).
	var gcState gcGovernorState
	// Read the effective GOGC baseline WITHOUT the SetGCPercent(-1) "query"
	// idiom: that call actually sets GOGC to -1 and disables the garbage
	// collector as a side effect. /gc/gogc:percent is a pure, non-mutating read.
	gcState.baselineGOGC = 100 // Go's default
	if gogc, ok := readGOGCPercent(); ok {
		gcState.baselineGOGC = gogc
	}
	if gcAdaptiveEnabled() {
		if v := os.Getenv("URNETWORK_BASELINE_GOGC"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				gcState.baselineGOGC = n
				debug.SetGCPercent(n)
			} else {
				tlog("[proxy][pressure] warn: ignoring invalid URNETWORK_BASELINE_GOGC=%q\n", v)
			}
		}
		tlog("[proxy][pressure] gcGovernor armed (baseline GOGC=%d)\n", gcState.baselineGOGC)
	}
	gcState.currentGOGC = gcState.baselineGOGC

	var smoothed float64
	lastRegime := 0
	fullTicker := time.NewTicker(pressureSampleInterval)
	defer fullTicker.Stop()
	subTicker := time.NewTicker(gcSubtickInterval)
	defer subTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-subTicker.C:
			// Fast heap-only path: tighten on a raw live-heap spike without
			// waiting for the 30s sweep. This subtick is intentionally heap-only
			// and host-blind, so it can only tighten — it never participates in
			// release (that needs the full sweep's host+heap view), and the CPU
			// veto (also host-sweep-derived) does not apply here. Passing -1 for
			// hostAvail makes the host layer inert, and canRelease=false keeps it
			// from touching the calm counter.
			if gcAdaptiveEnabled() {
				gcGovernor(liveHeapFrac(), -1, 0, false, &gcState)
			}
			continue
		case <-fullTicker.C:
		}

		if !resolveSelfHealEnabled(selfHealEnabled) {
			smoothed = 0
			setPressure(0)
			continue
		}
		sample := collectPressureSample()
		raw, comps := computePressure(sample)
		if raw >= 1.0 {
			smoothed = 1.0 // emergency pin bypasses the slow rise
		} else {
			smoothed = ewmaUpdate(smoothed, raw)
		}
		setPressure(smoothed)

		// Consolidated GC governor: merge heap + host-RAM, tighter wins.
		prevGOGC := gcState.currentGOGC
		gcGovernor(sample.HeapFrac, hostAvailMiB(), comps["psi_cpu"], true, &gcState)
		if gcState.currentGOGC != prevGOGC {
			tlog("[proxy][pressure] gcGovernor %s (heap=%.2f go=%d)\n",
				gcState.lastTightenAction, gcState.lastHeapFrac, gcState.currentGOGC)
		}

		writePressureStatus(smoothed, comps, &gcState)
		if r := pressureRegime(smoothed); r != lastRegime {
			tlog("[proxy][pressure] %.2f (%s)\n", smoothed, formatComponents(comps))
			lastRegime = r
		}
	}
}

func formatComponents(comps map[string]float64) string {
	parts := make([]string, 0, len(comps))
	for _, k := range []string{"psi_mem", "psi_cpu", "mem", "load", "goro", "heap"} {
		if v, ok := comps[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%.2f", k, v))
		}
	}
	return strings.Join(parts, " ")
}

// writePressureStatus persists the current score for `urnet-tools self-heal
// status` and debugging, plus the adaptive-GC governor state. Best-effort;
// failures are silent (status is advisory, the atomic is the source of truth).
func writePressureStatus(score float64, comps map[string]float64, gcState *gcGovernorState) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	var target int
	release, err := acquireProxyLockWithRetry()
	if err == nil {
		if state, err := readProxyURLState(); err == nil {
			target = state.TargetPoolSize
		}
		release()
	}
	payload, err := json.Marshal(map[string]any{
		"score":       score,
		"components":  comps,
		"target_pool": target,
		"gc_state":    gcStateNameOf(gcState),
		"heap_frac":   gcState.lastHeapFrac,
		"updated":     time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return
	}
	path := filepath.Join(home, ".urnetwork", "pressure_status")
	_ = os.MkdirAll(filepath.Dir(path), 0700)
	_ = os.WriteFile(path, payload, 0600)
}

// gcStateNameOf returns the governor's human-readable state, defaulting to
// "normal" before any tightening (zero-value level).
func gcStateNameOf(state *gcGovernorState) string {
	if state == nil || state.gcStateName == "" {
		return "normal"
	}
	return state.gcStateName
}

// fetchStretchMax is the ceiling on how far pressure can stretch the URL
// fetch interval (8× base at full pressure).
const (
	fetchStretchStart = 0.3
	fetchStretchFull  = 0.9
	fetchStretchMax   = 8.0
)

// fetchStretch maps pressure to a fetch-interval multiplier: 1× while calm,
// growing linearly to fetchStretchMax at fetchStretchFull. Replaces the old
// binary skip-at-threshold gate — a box at moderate pressure now slows down
// proportionally instead of getting zero or total protection.
func fetchStretch(pressure float64) float64 {
	t := normalizeRamp(pressure, fetchStretchStart, fetchStretchFull)
	return 1.0 + t*(fetchStretchMax-1.0)
}

// scaledProbeConcurrency shrinks the probe worker pool as pressure rises.
// Probe bursts are the provider's main self-generated load spike; the floor
// of 1 keeps the reaper/fetch pipelines draining even at full pressure.
func scaledProbeConcurrency(pressure float64) int {
	n := int(math.Round(float64(proxyProbeConcurrency) * (1 - pressure)))
	if n < 1 {
		return 1
	}
	return n
}

// getSystemLoad reads /proc/loadavg and returns the 1-minute and 5-minute
// load averages. Returns an error on non-Linux systems or parse failure;
// callers should fail-open (skip gating) when this happens.
func getSystemLoad() (load1, load5 float64, err error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, err
	}
	parts := strings.Fields(string(data))
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("unexpected /proc/loadavg format: %q", string(data))
	}
	load1, err = strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0, 0, err
	}
	load5, err = strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return 0, 0, err
	}
	return load1, load5, nil
}

// The cleanup/reaper/pool-controller constants below implement the
// inversion: under the old design, load-shedding actuators (cleanup,
// reaper) ran LESS often under pressure, on the theory that pressure means
// "back off everything." But cleanup and the reaper are the actuators that
// SHED load — gating them under pressure is exactly backwards. They now run
// MORE often as pressure rises. The AIMD pool controller is the same idea
// applied continuously: discover the sustainable proxy_url pool size per box
// instead of relying on a single operator-set ceiling.
const (
	// AIMD pool controller: additive increase / multiplicative decrease,
	// TCP-congestion style. The configured proxy_url_max is only a ceiling;
	// the operating point is discovered per box.
	aimdIncrement    = 25
	aimdDecreaseMult = 0.7
	aimdFloor        = 50
	aimdGrowBelow    = 0.3  // pressure below this → grow
	aimdShrinkAbove  = 0.75 // pressure above this (2 consecutive samples) → shrink
	shedBackoff      = time.Hour

	cleanupScaleStart = 0.3
	cleanupScaleFull  = 0.8
	cleanupScaleMin   = 1.0 / 6.0 // 6h base → 1h at full pressure

	reaperStaleCalm = 3 * time.Hour // matches the pre-pressure fixed value
	reaperStaleHot  = 1 * time.Hour

	// Paid/file proxies get a WIDER stale window than URL-sourced proxies:
	// the operator pays for the bandwidth their probes consume, and paid
	// proxies are stable by construction (they are in the desired file set,
	// never evicted for free newcomers, and their client IDs are preserved
	// across restarts so backend reputation accrues). URL-sourced proxies
	// are free to probe aggressively; paid ones should be re-probed only
	// when genuinely suspect. 6h calm / 3h hot is 3x wider than the URL
	// window's 3h/1h — with the earn-skip in runPaidProxyGradeOnce, a paid
	// proxy that is actively relaying traffic is never probed at all.
	paidStaleCalm = 6 * time.Hour
	paidStaleHot  = 3 * time.Hour
)

// cleanupIntervalScale shrinks the cleanup interval as pressure rises —
// cleanup sheds load, so overload is when it should run MORE often, not
// less (this inverts the original gate-everything design).
func cleanupIntervalScale(pressure float64) float64 {
	t := normalizeRamp(pressure, cleanupScaleStart, cleanupScaleFull)
	return 1.0 - t*(1.0-cleanupScaleMin)
}

// reaperStaleThreshold shrinks the once-good re-probe window under pressure:
// when the box is drowning, finding dead weight faster matters more.
func reaperStaleThreshold(pressure float64) time.Duration {
	t := normalizeRamp(pressure, cleanupScaleStart, cleanupScaleFull)
	return reaperStaleCalm - time.Duration(t*float64(reaperStaleCalm-reaperStaleHot))
}

// paidStaleThreshold is the same shape for the PAID/file-proxy window, which
// is deliberately wider: the operator pays for paid-probe bandwidth, and paid
// proxies are stable by construction, so they should be re-probed far less
// often than URL-sourced ones. Combined with the earn-skip in
// runPaidProxyGradeOnce (a proxy with live billable traffic is skipped
// entirely), this drives paid probe spend toward zero in steady state.
func paidStaleThreshold(pressure float64) time.Duration {
	t := normalizeRamp(pressure, cleanupScaleStart, cleanupScaleFull)
	return paidStaleCalm - time.Duration(t*float64(paidStaleCalm-paidStaleHot))
}

// aimdStep computes the next target pool size. cacheSize anchors growth so
// the target never runs far ahead of what actually exists.
func aimdStep(target, cacheSize int, pressure float64, ceiling int) int {
	switch {
	case pressure > aimdShrinkAbove:
		next := int(float64(target) * aimdDecreaseMult)
		if next < aimdFloor {
			next = aimdFloor
		}
		return next
	case pressure < aimdGrowBelow:
		base := target
		if cacheSize+aimdIncrement < base {
			base = cacheSize + aimdIncrement // track reality when cache lags target
		} else {
			base = target + aimdIncrement
		}
		if ceiling > 0 && base > ceiling {
			base = ceiling
		}
		return base
	default:
		return target
	}
}

// selectURLProxiesToShed ranks URL-sourced proxies for removal under
// sustained pressure: dead first, then degraded tiers, then healthy ones by
// ascending traffic — shedding an earning proxy is the last resort, and the
// caller logs each healthy shed individually.
func selectURLProxiesToShed(state *ProxyState, traffic map[string]uint64, n int) []string {
	rank := func(health string) int {
		switch health {
		case "dead":
			return 0
		case "inactive":
			return 1
		case "long_offline":
			return 2
		case "offline":
			return 3
		case "recently_offline":
			return 4
		default: // "up" and anything unknown sheds last
			return 5
		}
	}
	type cand struct {
		addr string
		r    int
		tx   uint64
	}
	var cands []cand
	for addr, e := range state.Proxies {
		if e.Source != "url" {
			continue
		}
		cands = append(cands, cand{addr, rank(e.Health), traffic[addr]})
	}
	slices.SortFunc(cands, func(a, b cand) int {
		if a.r != b.r {
			return a.r - b.r
		}
		if a.tx != b.tx {
			if a.tx < b.tx {
				return -1
			}
			return 1
		}
		return strings.Compare(a.addr, b.addr)
	})
	if n > len(cands) {
		n = len(cands)
	}
	out := make([]string, n)
	for i := range out {
		out[i] = cands[i].addr
	}
	return out
}

// poolControlInterval is how often the AIMD pool controller samples
// pressure and steps the target.
const poolControlInterval = 5 * time.Minute

// runPoolController runs the AIMD loop: every poolControlInterval it reads
// pressure, steps the target, persists it, and — on a shrink — sheds the
// worst URL-sourced proxies down to the new target via the same
// removeDeadProxies path the cleanup job uses (cache removal, NO blacklist,
// so shed addresses re-enter through a normal fetch+probe once the box
// recovers and the target grows back).
func runPoolController(ctx context.Context, configuredMax int, selfHealEnabled bool) {
	var highSamples int
	ticker := time.NewTicker(poolControlInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !resolveSelfHealEnabled(selfHealEnabled) {
			highSamples = 0
			continue
		}
		pressure := currentPressure()
		// While the GC governor is tightening, freeze pool GROWTH only: if the
		// current pressure would trigger a grow (low pressure), hold the target
		// flat by suppressing the grow branch. Shrinking is still allowed so the
		// pool can keep shedding load under pressure. This prevents the pool
		// growing into a memory squeeze the governor is deliberately backing
		// away from (the observer-effect the spec warned about).
		if gcTightening.Load() && pressure < aimdGrowBelow {
			pressure = aimdGrowBelow
		}
		if pressure > aimdShrinkAbove {
			highSamples++
		} else {
			highSamples = 0
		}
		// Shrink requires 2 consecutive high samples (10 min sustained);
		// growth and hold act immediately.
		effectivePressure := pressure
		if pressure > aimdShrinkAbove && highSamples < 2 {
			continue
		}

		release, err := acquireProxyLock()
		if err != nil {
			continue
		}
		urlState, err := readProxyURLState()
		if err != nil {
			release()
			continue
		}
		cacheSize := len(urlState.Cache)
		target := urlState.TargetPoolSize
		if target <= 0 {
			// First run: start from where we are (bounded by the ceiling).
			target = cacheSize
			if ceiling := resolveProxyURLMax(configuredMax); ceiling > 0 && target > ceiling {
				target = ceiling
			}
			if target < aimdFloor {
				target = aimdFloor
			}
		}
		next := aimdStep(target, cacheSize, effectivePressure, resolveProxyURLMax(configuredMax))
		if next != urlState.TargetPoolSize {
			urlState.TargetPoolSize = next
			if err := writeProxyURLState(urlState); err != nil {
				tlog("[proxy][pressure] warn: could not persist target: %v\n", err)
			}
		}
		release()
		if next != target {
			tlog("[proxy][pressure] pool target %d -> %d (pressure=%.2f cache=%d)\n", target, next, pressure, cacheSize)
		}

		if pressure > aimdShrinkAbove {
			shedPoolToTarget(next)
			highSamples = 0 // one cut per sustained-high episode; re-arm
		}
	}
}

// shedPoolToTarget removes the worst URL proxies until the live url-sourced
// count is at most target. Healthy sheds are logged individually — removing
// an earning proxy is deliberate and visible, never silent.
func shedPoolToTarget(target int) {
	state, err := readProxyState()
	if err != nil {
		return
	}
	urlCount := 0
	for _, e := range state.Proxies {
		if e.Source == "url" {
			urlCount++
		}
	}
	excess := urlCount - target
	if excess <= 0 {
		return
	}

	// Per-proxy traffic for last-resort ranking, keyed by address.
	traffic := map[string]uint64{}
	_, _, _, bandwidth, _ := connect.ProxyHealthSnapshot()
	for key, bw := range bandwidth {
		_, ip := parseProxyString(key)
		traffic[ip] += bw.TotalRx.Load() + bw.TotalTx.Load()
	}

	shed := selectURLProxiesToShed(state, traffic, excess)
	for _, addr := range shed {
		if state.Proxies[addr].Health == "up" {
			tlog("[proxy][pressure] shedding HEALTHY proxy %s (last resort, pool over target)\n", addr)
		}
		globalProxyFailureHistory.SetBackoffUntil(addr, time.Now().Add(shedBackoff))
	}
	if err := removeDeadProxies(state, map[string][]string{"url": shed}); err != nil {
		tlog("[proxy][pressure] warn: shed failed: %v\n", err)
		return
	}
	tlog("[proxy][pressure] shed %d url proxies to reach target %d\n", len(shed), target)
}
