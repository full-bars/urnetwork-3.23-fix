package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
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

// readMemAvailFrac returns MemAvailable/MemTotal from /proc/meminfo.
func readMemAvailFrac() (float64, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	var total, avail float64
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total, _ = strconv.ParseFloat(f[1], 64)
		case "MemAvailable:":
			avail, _ = strconv.ParseFloat(f[1], 64)
		}
	}
	if total <= 0 || avail <= 0 {
		return 0, fmt.Errorf("meminfo missing MemTotal/MemAvailable")
	}
	return avail / total, nil
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
	// Heap fraction of the max-memory soft limit, when one is configured.
	if limit := debug.SetMemoryLimit(-1); limit > 0 && limit < math.MaxInt64 {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		s.HeapFrac = float64(ms.HeapInuse) / float64(limit)
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

// runPressureMonitor samples sensors every pressureSampleInterval, smooths
// the score, publishes it, and logs on regime changes. When self-heal is
// off it publishes 0 and idles (cheap tick, no sensor reads), so toggling
// on at runtime starts sensing within one interval.
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

	var smoothed float64
	lastRegime := 0
	ticker := time.NewTicker(pressureSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
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
		writePressureStatus(smoothed, comps) // Task 7; no-op stub until then
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

// writePressureStatus is filled in by the observability task.
func writePressureStatus(score float64, comps map[string]float64) {}

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
