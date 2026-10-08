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
// pressureSampleInterval is how often the pressure loop takes a full sample. A
// variable so tests can drive the real loop in milliseconds.
var pressureSampleInterval = 30 * time.Second

const (
	// PSI `some avgXX` is "% of wall time at least one task stalled on this
	// resource". 10% = noticeable contention, 60% = severe. Same meaning on
	// any core count — PSI is self-normalizing.
	psiRampLo = 10.0
	psiRampHi = 60.0

	// CPU PSI on a SINGLE-CORE box sits in a busy-but-healthy band (~30-50)
	// during ordinary dialing, so the shared ramp scored normal operation as
	// sustained pressure and helped pin the pool (LA7: 500 -> 284 -> 138 and
	// no regrowth). The quieter 1-core ramp; multi-core keeps the shared PSI
	// ramp, which self-normalizes across cores there.
	cpu1CoreRampLo = 40.0
	cpu1CoreRampHi = 90.0

	// MemAvailable/MemTotal: plenty of page cache headroom above 25%,
	// reclaim death spiral territory below 5%.
	memAvailRampLo = 0.25 // score 0 at or above this fraction free
	memAvailRampHi = 0.05 // score 1 at or below this fraction free

	// loadavg per core (fallback sensor where PSI is unavailable).
	loadRampLo = 1.0
	loadRampHi = 3.0

	// Disk/IO PSI (same ramp as mem/cpu: some avg60 percent).
	ioRampLo = 10.0
	ioRampHi = 60.0

	// Self-signals: the provider's own runaway growth. LA1 melted down at
	// ~31k goroutines on 1.6GB.
	//
	// With running proxies the goroutine sensor is PER PROXY, not absolute: a
	// healthy proxy costs 20-27 goroutines, so a fixed ceiling pinned any healthy
	// pool above ~1,000 proxies at 1.00. The process-wide overhead (control
	// socket, monitors, metrics, GC workers) is subtracted first so a small pool
	// is not charged for it. The absolute ramp below still applies when nothing
	// is running (direct-only node), where there is no proxy count to divide by,
	// AND below goroutineMinPoolForPerProxy, where dividing by a small pool is
	// not meaningful: several self-heal consumers of this pressure score (the
	// AIMD URL pool controller, the reaper, probe concurrency) SHRINK the pool
	// under pressure, which shrinks the denominator and raises per-proxy
	// further -- a possible positive-feedback loop on a small URL-sourced pool.
	goroutineFixedOverhead      = 1000
	goroutineMinPoolForPerProxy = 50
	goroutinePerProxyRampLo     = 60.0
	goroutinePerProxyRampHi     = 150.0
	emergencyGoroutinesPerProxy = 200.0
	goroutineRampLo             = 5000
	goroutineRampHi             = 25000
	heapRampLo                  = 0.60 // fraction of the max-memory soft limit
	heapRampHi                  = 0.90

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

// globalPressureNoCPU mirrors globalPressure with the psi_cpu component
// excluded, for the pool GROW gate (scoring fix: CPU stalls may shrink the
// pool but must not block regrowth on busy-but-healthy small boxes).
var globalPressureNoCPU atomic.Uint64

func currentPressureNoCPU() float64 { return math.Float64frombits(globalPressureNoCPU.Load()) }
func setPressureNoCPU(v float64)    { globalPressureNoCPU.Store(math.Float64bits(v)) }

// lastRunningProxyCount mirrors the pressure monitor's last running-proxy
// sample as an atomic: the thrash watchdog's escalation path needs the count
// but must not take proxyHealthMu (rule 1 — never block on a lock a wedge can
// hold).
var lastRunningProxyCount atomic.Int64

// scoreExcludingCPU is the worst component with psi_cpu removed. The main
// score's emergency pins are heap/goroutine conditions, and those saturate
// their own components (the ramps top out exactly where the pins fire), so
// this loop alone honours emergencies; a saturated psi_cpu alone no longer
// blocks growth (scoring fix: CPU stalls may shrink the pool, not pin it).
func scoreExcludingCPU(comps map[string]float64) float64 {
	best := 0.0
	for k, v := range comps {
		if k == "psi_cpu" {
			continue
		}
		if v > best {
			best = v
		}
	}
	return best
}

// pressureSample is one raw reading of every sensor. Zero values mean "no
// data" and normalize to zero pressure (fail-open).
type pressureSample struct {
	PSIMem       float64 // /proc/pressure/memory some avg60 (percent)
	PSICPU       float64 // /proc/pressure/cpu some avg60 (percent)
	PSIIO        float64 // /proc/pressure/io some avg60 (percent); 0 = unavailable
	MemAvailFrac float64 // MemAvailable/MemTotal; 0 = unknown
	LoadPerCore  float64 // loadavg1 / NumCPU; 0 = unknown
	Goroutines   int
	// RunningProxies is how many proxies this provider runs; the goroutine
	// sensor judges goroutines per proxy when it is > 0.
	RunningProxies int
	HeapFrac       float64 // heap in use / max-memory soft limit; 0 = no limit set
	FDFrac         float64 // open FDs / RLIMIT_NOFILE; 0 = unavailable
	// Cores is the effective CPU count (GOMAXPROCS/cgroup-quota aware) the
	// CPU-PSI ramp is scaled by. 0 = unknown (multi-core ramp, as before).
	Cores      float64
	SensorErrs map[string]error
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

// goroutinesPerProxy is the per-proxy goroutine cost after removing the fixed
// process overhead; ok is false when there are no running proxies to divide by,
// or too few for the division to be meaningful (see goroutineMinPoolForPerProxy).
func goroutinesPerProxy(s pressureSample) (perProxy float64, ok bool) {
	if s.RunningProxies < goroutineMinPoolForPerProxy {
		return 0, false
	}
	return float64(max(0, s.Goroutines-goroutineFixedOverhead)) / float64(s.RunningProxies), true
}

// goroutineComponent scores runaway goroutine growth in [0,1].
func goroutineComponent(s pressureSample) float64 {
	if per, ok := goroutinesPerProxy(s); ok {
		return normalizeRamp(per, goroutinePerProxyRampLo, goroutinePerProxyRampHi)
	}
	return normalizeRamp(float64(s.Goroutines), goroutineRampLo, goroutineRampHi)
}

// goroutineEmergency reports a self-inflicted goroutine blowout that bypasses
// smoothing.
func goroutineEmergency(s pressureSample) bool {
	if per, ok := goroutinesPerProxy(s); ok {
		return per >= emergencyGoroutinesPerProxy
	}
	return s.Goroutines >= emergencyGoroutines
}

// computePressure converts one sample into a score plus its per-component
// breakdown. The worst component wins: averaging would dilute a memory
// crisis with a healthy CPU reading.
func computePressure(s pressureSample) (float64, map[string]float64) {
	comps := map[string]float64{
		"psi_mem": normalizeRamp(s.PSIMem, psiRampLo, psiRampHi),
		"psi_cpu": cpuPressureComponent(s.PSICPU, s.Cores),
		"psi_io":  normalizeRamp(s.PSIIO, ioRampLo, ioRampHi),
		"load":    normalizeRamp(s.LoadPerCore, loadRampLo, loadRampHi),
		"goro":    goroutineComponent(s),
	}
	if s.FDFrac > 0 {
		// FDFrac is the fraction of RLIMIT_NOFILE currently USED. Map it so
		// 50% used = score 0, 90% used = score 1 — approaching FD exhaustion
		// (a proxy's load-bearing resource) raises pressure (finding #5).
		comps["fd"] = normalizeRamp(s.FDFrac, 0.5, 0.9)
	}
	if s.MemAvailFrac > 0 {
		comps["mem"] = normalizeRamp(s.MemAvailFrac, memAvailRampLo, memAvailRampHi)
	}
	if s.HeapFrac > 0 {
		comps["heap"] = normalizeRamp(s.HeapFrac, heapRampLo, heapRampHi)
	}

	// Emergency pin: self-inflicted blowout bypasses smoothing.
	if (s.HeapFrac >= emergencyHeapFrac && s.HeapFrac > 0) || goroutineEmergency(s) {
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

// runningProxyCountForPressure is the pool size the goroutine sensor divides
// by: the health registry count minus the native direct transport, which is a
// single fixed goroutine, not a pool member. The direct entry is registered at
// index 0 by default, so WITHOUT this the sensor read 1 running proxy on a
// direct-only node, divided by 1, and judged a ~1,000-goroutine background
// (which is roughly the process's fixed overhead) as an emergency blowout.
// With it excluded, a direct-only node has RunningProxies == 0 and falls back
// to the absolute ramp, as the sensor documents.
func runningProxyCountForPressure() int {
	n := connect.ProxyHealthCount()
	if connect.ProxyKeyByIndex(0) == directProxyKey {
		n--
	}
	return n
}

// publishSelfHealOffPressure is what the pressure loop does on a tick while
// self-heal is off: every consumer reads a zero score, which they all treat as
// "do not act", and the connection memory budget returns to full so connections
// opened now do not keep the reduced buffers of an earlier pressure episode.
func publishSelfHealOffPressure() {
	setPressure(0)
	setPressureNoCPU(0)
	applyPressureMemoryBudget(0)
}

// collectPressureSample reads every sensor, recording errors per-sensor so
// one missing source (PSI on old kernels, everything on Windows/macOS)
// never blanks the others. Self-signals always work.
func collectPressureSample() pressureSample {
	s := pressureSample{SensorErrs: map[string]error{}, Goroutines: runtime.NumGoroutine(), RunningProxies: runningProxyCountForPressure(), Cores: effectiveCores()}

	if v, err := readPSI("memory"); err == nil {
		s.PSIMem = v
	} else if v, herr := readHostMemPressure(); herr == nil {
		// No PSI (macOS, Windows, old kernels): use the OS memory-pressure
		// level, translated onto the same percent scale.
		s.PSIMem = v
	} else {
		s.SensorErrs["psi_mem"] = err
	}
	if v, err := readPSI("cpu"); err == nil {
		s.PSICPU = v
	} else {
		s.SensorErrs["psi_cpu"] = err
	}
	// I/O pressure (disk-bound host). Same PSI source family as mem/cpu, so
	// it fails silently on the same old-kernel / non-Linux cases.
	if v, err := readPSI("io"); err == nil {
		s.PSIIO = v
	} else {
		s.SensorErrs["psi_io"] = err
	}
	if v := readFDFrac(); v >= 0 {
		s.FDFrac = v
	} else {
		s.SensorErrs["fd"] = fmt.Errorf("cannot read FD usage")
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
	// Heap fraction of the memory budget: live heap plus goroutine stacks over
	// the GOMEMLIMIT soft limit (effective RAM when no finite limit is set).
	// The same numerator feeds the 10s subtick, so the two governor paths cannot
	// disagree about one reading. It is deliberately NOT Sys-HeapReleased: that
	// tracks the heap goal, which moves with the GOGC value the governor itself
	// writes, so a box merely sitting at its soft limit read as an emergency
	// and the governor chased its own tail.
	s.HeapFrac = heapFracFn()
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
// One controller owns debug.SetGCPercent for the whole process (a "one owner"
// rule). It is fed by both memory signals and tightens to
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

// gcFreeOSMemoryMinInterval bounds how often entering the critical level may
// force a full GC and scavenge.
const gcFreeOSMemoryMinInterval = 5 * time.Minute

// Seams for tests.
var (
	gcFreeOSMemory = debug.FreeOSMemory
	gcNow          = time.Now
)

// gcTightening is true while the governor is actively tightening (level>0).
// Pool GROWTH is frozen while true; shrinking is still allowed.
var gcTightening atomic.Bool

// gcGovernorState tracks the single GC writer's hysteresis.
type gcGovernorState struct {
	baselineGOGC         int
	baselinePinned       bool // URNETWORK_BASELINE_GOGC set it; never re-adopt over it
	currentGOGC          int
	level                int // 0 normal,1 tighten,2 hard,3 critical
	consecutiveCalmCount int
	lastTightenAction    string
	gcStateName          string
	lastHeapFrac         float64
	lastHostAvailMiB     int64
	lastFreeOS           time.Time // last forced FreeOSMemory, for rate limiting
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

// readStackBytes returns the bytes of memory the runtime holds for goroutine
// stacks (/memory/classes/heap/stacks:bytes), or 0 if unavailable.
func readStackBytes() uint64 {
	samples := []metrics.Sample{{Name: "/memory/classes/heap/stacks:bytes"}}
	metrics.Read(samples)
	if samples[0].Value.Kind() == metrics.KindUint64 {
		return samples[0].Value.Uint64()
	}
	return 0
}

// heapFracFrom is the one definition of the governor's heap fraction: live
// heap plus stacks over the memory budget. 0 when either the live reading or
// the limit is unavailable.
func heapFracFrom(live, stacks uint64, limit int64) float64 {
	if live == 0 || limit <= 0 {
		return 0
	}
	return float64(live+stacks) / float64(limit)
}

// heapFracFn is the one heap numerator seam: the 10s subtick, the 30s sweep
// sample and the self-heal-off tick all read it, so they cannot disagree about
// one reading. A variable so a test can prove the sample goes through it.
var heapFracFn = liveHeapFrac

// liveHeapFrac is the heap fraction of the memory budget used by BOTH the 10s
// subtick and the 30s sweep sample.
func liveHeapFrac() float64 {
	limit := debug.SetMemoryLimit(-1)
	if limit <= 0 || limit >= math.MaxInt64 {
		if ram := detectEffectiveRAMLimitBytes(); ram > 0 {
			limit = ram
		}
	}
	return heapFracFrom(readLiveHeapBytes(), readStackBytes(), limit)
}

// hostAvailMiB returns the tighter of host and cgroup available memory, or -1
// if unavailable (the consolidated controller treats -1 as "no host signal").
func hostAvailMiB() int64 {
	// Prefer host availability, but never blank the signal when only a cgroup
	// headroom is available (e.g. a container that cannot read /proc/meminfo).
	h := readMemAvailableMiB()
	c := readCgroupAvailableMiB()
	switch {
	case h >= 0 && c >= 0:
		if c < h {
			return c
		}
		return h
	case h >= 0:
		// Host memory is readable but no cgroup headroom exists (bare metal,
		// a VM, or a cgroup whose accounting we cannot read). Fall through to
		// the host signal rather than blanking it -- this returned h before
		// the switch refactor.
		return h
	case c >= 0:
		return c
	default:
		return -1
	}
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
		// Return memory to the OS at the critical state, but not on every
		// re-entry: flapping 2<->3 would otherwise force a full GC each time.
		if now := gcNow(); state.lastFreeOS.IsZero() || now.Sub(state.lastFreeOS) >= gcFreeOSMemoryMinInterval {
			state.lastFreeOS = now
			gcFreeOSMemory()
		}
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
	// Record the sampled heap + host state up front so writePressureStatus keeps
	// reporting the true pressure sample even when adaptive GC is disabled (the
	// early return below must not blank heap_frac/host in the status file).
	state.lastHeapFrac = heapFrac
	state.lastHostAvailMiB = hostAvail

	if !gcAdaptiveEnabled() {
		return
	}

	// The baseline is captured once when the monitor arms, but GOGC has other
	// writers: the auto tier writes its value once per process at the first proxy
	// launch (which can come after the monitor armed, on a box whose first proxy
	// is delayed), and an operator can set it live. A baseline captured before
	// such a write is stale, and the next tighten-then-release would put it back
	// over the real value (on the smallest boxes that is the tier's 50 replaced
	// by 100, and nothing restores it). While the governor is not tightening it
	// owns no GOGC, so a value it did not write is the new baseline. An explicit
	// URNETWORK_BASELINE_GOGC is never overridden.
	if state.level == 0 && !state.baselinePinned {
		if cur, ok := readGOGCPercent(); ok && cur > 0 && cur != state.currentGOGC {
			state.baselineGOGC = cur
			state.currentGOGC = cur
		}
	}

	// Heap level from the raw fraction. Note (inference from the arithmetic, not
	// measured): with a finite GOMEMLIMIT and a live-heap numerator, levels 1 to 3
	// already put the heap goal at or above the limit, so the limit paces GC and
	// the GOGC write adds little. The heap layer's real effects are gcTightening
	// (which freezes pool growth) and the level-3 FreeOSMemory; the host-RAM
	// levels below still tighten GOGC for real.
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

// logGCGovernorChange logs a governor GOGC change. Every writer path uses it,
// so an operator can always tell why GOGC moved (the 10s subtick and the
// self-heal-off tick used to change it silently).
func logGCGovernorChange(prevGOGC int, state *gcGovernorState) {
	if state.currentGOGC == prevGOGC {
		return
	}
	if state.currentGOGC < prevGOGC {
		// Tightened (a lower GOGC collects harder).
		what := "tightened"
		switch state.gcStateName {
		case "hard":
			what = "tightened hard"
		case "critical":
			what = "tightened as far as it goes (and handed free memory back to the OS)"
		}
		tlog("[proxy][pressure] Memory governor: %s garbage collection to keep the heap in check (heap at %.0f%% of its soft limit; collecting at %d%% growth, was %d%%) — it relaxes on its own once things calm down. (gcGovernor %s heap=%.2f go=%d)\n",
			what, state.lastHeapFrac*100, state.currentGOGC, prevGOGC,
			state.lastTightenAction, state.lastHeapFrac, state.currentGOGC)
		return
	}
	what := "eased to a gentler setting"
	if state.gcStateName == "normal" {
		what = "back to normal"
	}
	tlog("[proxy][pressure] Memory governor: memory has calmed down, so garbage collection is %s (heap at %.0f%% of its soft limit; collecting at %d%% growth, was %d%%). (gcGovernor %s heap=%.2f go=%d)\n",
		what, state.lastHeapFrac*100, state.currentGOGC, prevGOGC,
		state.lastTightenAction, state.lastHeapFrac, state.currentGOGC)
}

// cpuPressureComponent is the ONE CPU-PSI ramp, shared by computePressure and
// cpuVetoSignal (they used to duplicate it). cores selects the 1-core ramp;
// note the deliberate veto-cutoff behavior change on single-core boxes:
// psi_cpu values that used to read 0.6+ now read far lower, so the GC veto no
// longer fires on ordinary single-core busyness.
func cpuPressureComponent(raw, cores float64) float64 {
	if raw <= 0 {
		return 0
	}
	if cores > 0 && cores <= 1.5 {
		return normalizeRamp(raw, cpu1CoreRampLo, cpu1CoreRampHi)
	}
	return normalizeRamp(raw, psiRampLo, psiRampHi)
}

// effectiveCores is the CPU count the pressure math should believe: NumCPU,
// capped by a cgroup cpu.max quota and by an explicit GOMAXPROCS, because
// both genuinely limit this process on an otherwise-big box.
func effectiveCores() float64 {
	cores := float64(runtime.NumCPU())
	if q, ok := cgroupCPUQuotaCores(); ok && q > 0 && q < cores {
		cores = q
	}
	if g := float64(runtime.GOMAXPROCS(0)); g > 0 && g < cores {
		cores = g
	}
	return cores
}

// cgroupCPUQuotaCores reads the effective cgroup v2 CPU quota in cores: the
// TIGHTEST cpu.max among this process's own cgroup and every ancestor up to
// /sys/fs/cgroup, because each level constrains its subtree (a systemd unit
// lives in a slice; the quota may sit there, not at the unit's own cgroup or
// the mount root where the old single-file read looked). ok=false when
// nothing in the chain is quota'd.
func cgroupCPUQuotaCores() (float64, bool) {
	best := 0.0
	check := func(dir string) {
		b, err := os.ReadFile(filepath.Join(dir, "cpu.max"))
		if err != nil {
			return
		}
		f := strings.Fields(string(b))
		if len(f) != 2 || f[0] == "max" {
			return
		}
		quota, err1 := strconv.ParseFloat(f[0], 64)
		period, err2 := strconv.ParseFloat(f[1], 64)
		if err1 != nil || err2 != nil || period <= 0 || quota <= 0 {
			return
		}
		if cores := quota / period; best == 0 || cores < best {
			best = cores
		}
	}
	dir := cgroupV2SelfDir()
	if dir == "" {
		dir = "/sys/fs/cgroup"
	}
	for strings.HasPrefix(dir, "/sys/fs/cgroup") {
		check(dir)
		if dir == "/sys/fs/cgroup" {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if best > 0 {
		return best, true
	}
	return 0, false
}

// cpuVetoSignal is the normalized CPU PSI the governor's CPU veto reads, the
// same quantity the sweep passes (comps["psi_cpu"]). 0 when PSI is
// unavailable, which leaves the veto inert, as before.
func cpuVetoSignal() float64 {
	v, err := readPSI("cpu")
	if err != nil {
		return 0
	}
	return cpuPressureComponent(v, effectiveCores())
}

// gcSubtickStep is the fast heap-only path: tighten on a raw live-heap spike.
// It is host-blind (hostAvail -1) and never releases, but honours the same
// CPU veto as the sweep.
func gcSubtickStep(heapFrac, psiCPU float64, state *gcGovernorState) {
	prev := state.currentGOGC
	gcGovernor(heapFrac, -1, psiCPU, false, state)
	logGCGovernorChange(prev, state)
}

// gcSelfHealOffTick runs on the full-sweep cadence when self-heal is off. The
// governor is a memory-safety actuator independent of self-heal, so this tick
// must still give it the host+heap view and the right to RELEASE; skipping it
// left a tightened GOGC in place for the rest of the process's life.
func gcSelfHealOffTick(heapFrac float64, hostAvail int64, psiCPU float64, state *gcGovernorState) {
	prev := state.currentGOGC
	gcGovernor(heapFrac, hostAvail, psiCPU, true, state)
	logGCGovernorChange(prev, state)
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
	for _, name := range []string{"psi_mem", "psi_cpu", "psi_io", "mem", "load", "fd"} {
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
				gcState.baselinePinned = true
				debug.SetGCPercent(n)
			} else {
				tlog("[proxy][pressure] warn: ignoring invalid URNETWORK_BASELINE_GOGC=%q\n", v)
			}
		}
		tlog("[proxy][pressure] gcGovernor armed (baseline GOGC=%d)\n", gcState.baselineGOGC)
	}
	gcState.currentGOGC = gcState.baselineGOGC
	// However this loop ends (ctx, or a panic the supervisor restarts it after),
	// leave the score, memory budget and GOGC neutral. Without it a dead monitor
	// froze its last reading in force, and a restart would adopt the tightened
	// GOGC as its baseline.
	defer func() {
		resetPressureActuators(&gcState, debug.SetGCPercent)
		clearPressureStatus()
	}()

	var headroom headroomTracker
	headroomLow := headroomLowThresholdMiB(detectEffectiveRAMLimitBytes() >> 20)
	var smoothed float64
	var smoothedNoCPU float64
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
			// release (that needs the full sweep's host+heap view). It uses the
			// same heap numerator and the same CPU veto as the sweep, so it cannot
			// re-tighten within 10s of a release the sweep granted during a
			// CPU-bound episode. Passing -1 for hostAvail makes the host layer
			// inert, and canRelease=false keeps it from touching the calm counter.
			if gcAdaptiveEnabled() {
				gcSubtickStep(heapFracFn(), cpuVetoSignal(), &gcState)
			}
			continue
		case <-fullTicker.C:
			// Track the peak running count and heartbeat for the OOM-aware start
			// cap. It runs whether or not self-heal is on: it is bookkeeping, not
			// an actuator.
			// runningProxyCountForPressure excludes the native direct
			// transport (a single fixed goroutine, not a pool member), so
			// neither the OOM peak nor the headroom log's proxy count is
			// off by one on a direct-only or direct+proxies node.
			proxyCount := runningProxyCountForPressure()
			oomCapUpdatePeak(proxyCount, time.Now())
			lastRunningProxyCount.Store(int64(proxyCount))
			// Real free memory, independent of the pressure score and of
			// self-heal: log when the box gets short and when it recovers.
			avail := hostAvailMiB() // one reading, used for both the decision and the line
			if line := headroomLogLine(headroom.Observe(avail, headroomLow), avail, headroomLow, proxyCount, runtime.NumGoroutine()); line != "" {
				importantLogf("%s\n", line)
			}
		}

		if !resolveSelfHealEnabled(selfHealEnabled) {
			smoothed = 0
			smoothedNoCPU = 0
			publishSelfHealOffPressure()
			// The GC governor is independent of self-heal: keep giving it the
			// full host+heap view so a tightened level can release.
			if gcAdaptiveEnabled() {
				gcSelfHealOffTick(heapFracFn(), hostAvailMiB(), cpuVetoSignal(), &gcState)
			}
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

		// CPU-excluded companion score for the grow gate. The main score's
		// emergency pins are heap/goroutine conditions, and those saturate
		// their own components to 1.0, so the loop alone carries emergencies;
		// a saturated psi_cpu alone no longer blocks growth (scoring fix).
		rawNoCPU := scoreExcludingCPU(comps)
		if rawNoCPU >= 1.0 {
			smoothedNoCPU = 1.0
		} else {
			smoothedNoCPU = ewmaUpdate(smoothedNoCPU, rawNoCPU)
		}
		setPressureNoCPU(smoothedNoCPU)

		// Consolidated GC governor: merge heap + host-RAM, tighter wins.
		prevGOGC := gcState.currentGOGC
		gcGovernor(sample.HeapFrac, hostAvailMiB(), comps["psi_cpu"], true, &gcState)
		logGCGovernorChange(prevGOGC, &gcState)

		// MemoryBudget actuator (#6): scale the per-connection memory-dominant
		// settings (queue caps, receive windows, socket buffers) down proportionally
		// to live pressure, so NEW connections/sequences opened while under pressure
		// get smaller buffers before we have to shed proxies/pool. Settings sample
		// the budget at construction time, so this affects future objects only —
		// exactly the "shrink before you have to kill" semantic.
		applyPressureMemoryBudget(smoothed)

		writePressureStatus(smoothed, comps, &gcState)
		if r := pressureRegime(smoothed); r != lastRegime {
			// Plain words first, machine counters in the trailing paren
			// (operator-log readability contract); never an empty "()".
			if compsTxt := formatComponents(comps); compsTxt != "" {
				tlog("🧯 [proxy][pressure] %s (%s)\n", pressureSummaryOf(smoothed, comps), compsTxt)
			} else {
				tlog("🧯 [proxy][pressure] %s\n", pressureSummaryOf(smoothed, comps))
			}
			lastRegime = r
		}
	}
}

// memoryBudgetActuateThreshold is the smoothed-pressure score above which we
// begin shrinking the per-connection memory budget. Below it we keep the
// process at its full (unscaled) budget so normal operation isn't affected.
const memoryBudgetActuateThreshold = 0.5

// applyPressureMemoryBudget wires the never-called SetMemoryBudget actuator
// (#6) to live pressure. When smoothed pressure crosses the threshold, it sets
// a process memory budget below the referenceMemoryBudgetByteCount so future
// Default*Settings constructions (connection queues, receive windows, socket
// buffers) scale DOWN, shrinking buffers for newly-opened objects before we
// have to shed proxies or pool. On calm it restores the full budget (0 =
// unscaled). Only affects objects constructed after the call.
func applyPressureMemoryBudget(smoothed float64) {
	if smoothed < memoryBudgetActuateThreshold {
		if connect.MemoryBudget() != 0 {
			connect.SetMemoryBudget(0) // back to unscaled defaults
		}
		return
	}
	// Scale the budget down linearly from reference (64MiB) toward a floor as
	// pressure rises 0.5 -> 1.0. A smaller budget makes MemoryScaledByteCount
	// shrink queue/window caps. Floor at 24MiB keeps a working minimum.
	const ref = 64 << 20 // referenceMemoryBudgetByteCount
	const floor = 24 << 20
	frac := (smoothed - memoryBudgetActuateThreshold) / (1 - memoryBudgetActuateThreshold)
	budget := connect.ByteCount(ref - int64(frac*float64(ref-floor)))
	if budget < floor {
		budget = floor
	}
	connect.SetMemoryBudget(budget)
}

func formatComponents(comps map[string]float64) string {
	parts := make([]string, 0, len(comps))
	for _, k := range []string{"psi_mem", "psi_cpu", "psi_io", "mem", "load", "goro", "heap", "fd"} {
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
	// Thrash fields + a plain-language summary: nothing here needs decoding.
	var psiFull any
	if v, ok := thrashPSIFullForStatus(); ok {
		psiFull = v
	}
	var swapIO any
	if v, ok := thrashSwapIOForStatus(); ok {
		swapIO = v
	}
	payload, err := json.Marshal(map[string]any{
		"score":        score,
		"components":   comps,
		"target_pool":  target,
		"gc_state":     gcStateNameOf(gcState),
		"heap_frac":    gcState.lastHeapFrac,
		"thrash_state": thrashStateName(),
		"psi_mem_full": psiFull,
		"swap_io_rate": swapIO,
		"summary":      pressureSummaryOf(score, comps),
		"updated":      time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return
	}
	path := filepath.Join(home, ".urnetwork", "pressure_status")
	_ = os.MkdirAll(filepath.Dir(path), 0700)
	_ = os.WriteFile(path, payload, 0600)
}

// clearPressureStatus removes the persisted score when the monitor exits, so
// a reader does not see the dead monitor's last (possibly emergency) reading
// during the restart backoff. Best-effort, like writePressureStatus.
func clearPressureStatus() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	_ = os.Remove(filepath.Join(home, ".urnetwork", "pressure_status"))
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

// applyShedBackoff keeps a shed proxy down for at least shedBackoff from now.
// It only ever lengthens: a shed picks addresses from its own snapshot, so the
// address may already be held by something longer (proxy audit's park,
// a URL give-up), and overwriting that with a short window would relaunch it
// early. Pool shed and trim shed both come through here.
func applyShedBackoff(addr string, now time.Time) {
	globalProxyFailureHistory.ExtendBackoffUntil(addr, now.Add(shedBackoff))
}

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

// aimdCeilingFrac and aimdCeilingDecay shape the regrowth ceiling. After a
// pressure cut the pool may not regrow past aimdCeilingFrac of the level
// where pressure appeared; the ceiling then relaxes back to that level over
// aimdCeilingDecay and is released, so a box whose capacity has changed can
// rediscover it. Without the memory the loop sawtooths: +aimdIncrement every
// poolControlInterval until it hits the same wall, then shed again.
const (
	aimdCeilingFrac  = 0.9
	aimdCeilingDecay = 24 * time.Hour
)

// aimdCeilingState remembers where pressure last forced a cut. In memory
// only: a restart forgets it and the pool rediscovers its level.
type aimdCeilingState struct {
	level int       // starting ceiling: aimdCeilingFrac x hit, never below aimdFloor
	hit   int       // pool target when pressure forced the cut
	at    time.Time // when the cut happened
}

// record notes a cut made at pool size hit. While a ceiling is still active the
// highest hit wins: a sustained episode cuts again every sample at 0.7x, and
// remembering the last, lowest cut would pin the pool near the floor for a day
// after what is often an external, transient host event. A ceiling that has
// been released starts fresh.
func (c *aimdCeilingState) record(hit int, now time.Time) {
	if c.effective(now) > 0 && c.hit > hit {
		hit = c.hit
	}
	level := int(float64(hit) * aimdCeilingFrac)
	if level < aimdFloor {
		level = aimdFloor
	}
	if hit < level {
		hit = level
	}
	c.level, c.hit, c.at = level, hit, now
}

// effective returns the current regrowth ceiling, or 0 for none.
func (c *aimdCeilingState) effective(now time.Time) int {
	if c.at.IsZero() {
		return 0
	}
	age := now.Sub(c.at)
	if age < 0 {
		age = 0
	}
	if age >= aimdCeilingDecay {
		return 0
	}
	return c.level + int(float64(c.hit-c.level)*float64(age)/float64(aimdCeilingDecay))
}

// ceilingHit is the pool size to remember for a cut: the smaller of the target
// and the pool that actually exists, because a target the cache never reached
// would record a ceiling that constrains nothing. pool <= 0 means unknown.
func ceilingHit(target, pool int) int {
	if pool > 0 && pool < target {
		return pool
	}
	return target
}

// combineCeilings returns the tightest positive of the operator-configured
// ceiling and the learned one; 0 means no ceiling.
func combineCeilings(configured, learned int) int {
	switch {
	case configured <= 0:
		return learned
	case learned <= 0:
		return configured
	case learned < configured:
		return learned
	}
	return configured
}

// capURLTarget clamps the URL pool target to what the running-proxy cap
// leaves after paid proxies, which the cap also counts and which are never
// shed. It never returns below 1: a zero target reads as "first run".
func capURLTarget(next, cap, paid int) int {
	if cap <= 0 {
		return next
	}
	room := cap - paid
	if room < 1 {
		room = 1
	}
	if next > room {
		return room
	}
	return next
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

// aimdMoveMessage explains one pool-target move in a plain sentence,
// attributing the cause honestly: AIMD pressure steps, cache tracking and cap
// or ceiling changes all share this path, and a wrong attribution sends an
// operator chasing a cause that never existed.
func aimdMoveMessage(target, next, cacheSize int, pressure float64) string {
	switch {
	case next > target && pressure < aimdGrowBelow:
		return fmt.Sprintf("Pool size target raised %d -> %d: pressure is low (%.2f), growing toward the allowed maximum. (pressure=%.2f cache=%d)\n", target, next, pressure, pressure, cacheSize)
	case next > target:
		return fmt.Sprintf("Pool size target raised %d -> %d: a cap or ceiling changed. (pressure=%.2f cache=%d)\n", target, next, pressure, cacheSize)
	case pressure > aimdShrinkAbove:
		return fmt.Sprintf("Pool size target lowered %d -> %d: pressure has been high (%.2f), shrinking to fit. (pressure=%.2f cache=%d)\n", target, next, pressure, pressure, cacheSize)
	case pressure < aimdGrowBelow && next == cacheSize+aimdIncrement:
		return fmt.Sprintf("Pool size target lowered %d -> %d: only %d proxies are cached, so the target follows the live pool. (pressure=%.2f cache=%d)\n", target, next, cacheSize, pressure, cacheSize)
	default:
		return fmt.Sprintf("Pool size target lowered %d -> %d: a cap or ceiling lowered it. (pressure=%.2f cache=%d)\n", target, next, pressure, cacheSize)
	}
}

// selectURLProxiesToShed ranks URL-sourced proxies for removal under
// sustained pressure: dead first, then degraded tiers, then healthy ones by
// ascending persisted earnings, then ascending lifetime traffic. Lifetime
// traffic alone is empty right after a restart, which made every proxy look
// idle; the earnings score survives restarts. Shedding an earning proxy is
// the last resort, and the caller logs each healthy shed individually.
func selectURLProxiesToShed(state *ProxyState, traffic map[string]uint64, earnings map[string]float64, n int) []string {
	rank := healthRank // shared with the operator proxy-trim shed ranking
	type cand struct {
		addr string
		r    int
		earn float64
		tx   uint64
	}
	var cands []cand
	for addr, e := range state.Proxies {
		if e.Source != "url" {
			continue
		}
		cands = append(cands, cand{addr, rank(e.Health), earnings[addr], traffic[addr]})
	}
	slices.SortFunc(cands, func(a, b cand) int {
		if a.r != b.r {
			return a.r - b.r
		}
		if a.earn != b.earn {
			if a.earn < b.earn {
				return -1
			}
			return 1
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
	var ceilingMemory aimdCeilingState
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
		// Grow/hold decisions must not be blocked by CPU stalls alone
		// (scoring fix: a 1-core box's CPU PSI otherwise pins the pool below
		// its ceiling forever). Outside the shrink regime, read the score with
		// psi_cpu excluded; the shrink decision below keeps the full score.
		if pressure <= aimdShrinkAbove {
			if noCPU := currentPressureNoCPU(); noCPU < pressure {
				pressure = noCPU
			}
		}
		// While the GC governor is tightening or the thrash watchdog holds the
		// freeze rung, freeze pool GROWTH only: if the current pressure would
		// trigger a grow (low pressure), hold the target flat by suppressing
		// the grow branch. Shrinking is still allowed so the pool can keep
		// shedding load under pressure. This prevents the pool growing into a
		// memory squeeze the system is deliberately backing away from (the
		// observer-effect the spec warned about).
		if (gcTightening.Load() || thrashFreeze.Load()) && pressure < aimdGrowBelow {
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
		next := aimdStep(target, cacheSize, effectivePressure,
			combineCeilings(resolveProxyURLMax(configuredMax), ceilingMemory.effective(time.Now())))
		// An operator trim cap overrides the AIMD operating point: never grow
		// the URL pool target above what the running-proxy cap leaves after paid
		// proxies (they fight otherwise, burning fetch/probe work on proxies that
		// can never launch). The cap counts every running proxy, paid included.
		if tc, _ := effectiveTrimCap(); tc > 0 {
			next = capURLTarget(next, tc, paidProxyCount())
		}
		if next != urlState.TargetPoolSize {
			urlState.TargetPoolSize = next
			if err := writeProxyURLState(urlState); err != nil {
				tlog("⚠️ [proxy][pressure] warn: could not persist target: %v\n", err)
			}
		}
		release()
		if next != target {
			tlog("🧯 [proxy][pressure] %s", aimdMoveMessage(target, next, cacheSize, pressure))
		}

		if pressure > aimdShrinkAbove {
			// Remember where pressure hit so regrowth stops short of it.
			ceilingMemory.record(ceilingHit(target, cacheSize), time.Now())
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

	// Per-proxy traffic for last-resort ranking, keyed by proxy identity to
	// match state.Proxies.
	traffic := runningProxyEarnings()
	now := time.Now()
	earnings := make(map[string]float64, len(state.Proxies))
	for addr, e := range state.Proxies {
		if e.Source == "url" {
			earnings[addr] = proxyEarningsScore(addr, now)
		}
	}

	shed := selectURLProxiesToShed(state, traffic, earnings, excess)
	for _, addr := range shed {
		if state.Proxies[addr].Health == "up" {
			tlog("🧯 [proxy][pressure] shedding HEALTHY proxy %s (last resort, pool over target)\n", proxyKeyDisplay(addr))
		}
		applyShedBackoff(addr, time.Now())
	}
	if err := removeDeadProxies(state, map[string][]string{"url": shed}); err != nil {
		tlog("⚠️ [proxy][pressure] warn: shed failed: %v\n", err)
		return
	}
	tlog("🧯 [proxy][pressure] shed %d url proxies to reach target %d\n", len(shed), target)
}

// paidRunningCount is how many of the running proxies are paid or file
// sourced: in proxy.state, not URL-sourced, and not the direct transport. The
// trim/OOM cap counts RUNNING proxies, so this must too. proxy.state also holds
// every desired entry, dead or backed-off or held by trim, and counting those
// drove cap minus paid to nothing and pinned the URL pool at 1. The pool
// controller never sheds these, though the operator trim can.
func paidRunningCount(state *ProxyState, running []string) int {
	n := 0
	for _, key := range running {
		if key == directProxyKey {
			continue
		}
		if e, ok := state.Proxies[key]; ok && e.Source != "url" {
			n++
		}
	}
	return n
}

// paidProxyCount reads proxy.state and the running set for paidRunningCount.
// Returns 0 when the state cannot be read, which leaves the cap clamp at its
// pre-existing behaviour.
func paidProxyCount() int {
	state, err := readProxyState()
	if err != nil {
		return 0
	}
	return paidRunningCount(state, runningProxyAddresses())
}
