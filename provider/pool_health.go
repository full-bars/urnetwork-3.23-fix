package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// heartbeatHostLabel names the node on the build line. It reuses the node-name
// resolution the hub reporter already uses (control state, then the
// ~/.urnetwork/node_name override), falling back to HOST_HOSTNAME and then the
// kernel hostname. Unlike providerDescription it never resolves the public IP,
// because this runs on every heartbeat tick.
func heartbeatHostLabel() string {
	hostname, _ := os.Hostname()
	return hostLabelFrom(resolveNodeName(""), os.Getenv("HOST_HOSTNAME"), hostname)
}

// hostLabelFrom holds the precedence so it can be tested without touching the
// control state, the override file, or the host's own name.
func hostLabelFrom(nodeName, hostEnv, hostname string) string {
	for _, candidate := range []string{nodeName, hostEnv, hostname} {
		if v := strings.TrimSpace(candidate); v != "" {
			return v
		}
	}
	return ""
}

// Verdicts reported on the [health][pool] line.
const (
	poolVerdictWarming = "warming"
	poolVerdictOK      = "ok"
	poolVerdictWatch   = "watch"
	poolVerdictLeak    = "leak"
)

// poolFloorWindowDuration is how far back the idle floor looks. One hour is
// long enough that any real traffic pattern drains the pool to its true
// baseline at least once inside it.
const poolFloorWindowDuration = time.Hour

// poolWatchRise and poolLeakRise are how long the floor must climb without
// interruption before the line says something. 15 minutes of monotonic rise is
// worth a look; 30 minutes is not a traffic pattern.
const (
	poolWatchRise = 15 * time.Minute
	poolLeakRise  = 30 * time.Minute
)

// poolHealthWindow turns the raw message-pool counters into a verdict an
// operator can act on without knowing what a message pool is.
//
// The number it watches is the idle floor: the lowest in-use count seen over
// the last hour. Buffers legitimately in flight are taken and given back
// constantly, so a healthy pool touches a low number at least once an hour and
// the floor stays flat. A buffer taken and never given back raises that
// minimum permanently.
//
// This is why the floor is the signal and the return percentage is not. A leak
// proportional to throughput keeps returned/taken pinned near 100% forever,
// because the denominator grows exactly as fast as the leak: 1 buffer lost per
// 10,000 taken reads 99.99% while shedding ~1440 buffers a day. The floor
// moves anyway.
//
// The tradeoff is detection latency. Pre-leak samples have to age out of the
// window before the floor can move at all, so a leak beginning at boot is
// called ~90 minutes in: one hour to clear the window, then 30 minutes of
// monotonic rise. That is deliberate. A faster signal would fire on ordinary
// load steps, and a leak verdict nobody trusts is worth nothing.
type poolHealthWindow struct {
	// window is how many ticks cover poolFloorWindowDuration, and so the
	// length of both sample rings.
	window int
	// watchRise and leakRise are poolWatchRise and poolLeakRise expressed in
	// ticks, so the thresholds hold whatever the heartbeat interval is.
	watchRise int
	leakRise  int

	inUse  []uint64 // in-use samples, oldest first
	floors []uint64 // floor per tick, oldest first

	floorRise   int // consecutive ticks the floor increased
	createdRise int // consecutive ticks the created total increased
	prevFloor   uint64
	prevCreated uint64
	seeded      bool
}

// poolHealthReading is one tick's assessment.
type poolHealthReading struct {
	Verdict string
	// Floor is the lowest in-use count over the window.
	Floor uint64
	// FloorAtWindowStart is the floor as it read a full window ago, so the
	// line can say what the number grew from.
	FloorAtWindowStart uint64
	// RisingFor is how long the floor has been climbing without interruption.
	RisingFor time.Duration
	// CreatedRising is true when the pool has kept allocating new buffers for
	// as long as a leak-grade floor rise would take. On a healthy pool
	// allocation plateaus once the working set is covered.
	CreatedRising bool
	// Full is true once the window holds a complete hour of samples. Until
	// then there is no floor to trust and the verdict is warming.
	Full bool
	// WarmingRemaining is how long until the window fills, computed from the
	// actual sample count rather than uptime.  This stays accurate when pool
	// activity starts late and the window has been sitting idle.
	WarmingRemaining time.Duration
}

// newPoolHealthWindow sizes the rings and thresholds from the heartbeat
// interval, so a provider running URNETWORK_HEALTH_INTERVAL=10m still reasons
// over one hour and 30 minutes of rise rather than over 12 and 6 ticks.
func newPoolHealthWindow(interval time.Duration) *poolHealthWindow {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticks := func(d time.Duration, minimum int) int {
		n := int((d + interval - 1) / interval) // ceil
		if n < minimum {
			return minimum
		}
		return n
	}
	return &poolHealthWindow{
		window:    ticks(poolFloorWindowDuration, 2),
		watchRise: ticks(poolWatchRise, 1),
		leakRise:  ticks(poolLeakRise, 1),
	}
}

// observe records one tick and returns the resulting reading. inUse is
// taken-minus-returned across every pool bucket; created is the all-time
// allocation total.
func (w *poolHealthWindow) observe(inUse uint64, created uint64, interval time.Duration) poolHealthReading {
	w.inUse = appendCapped(w.inUse, inUse, w.window)
	floor := minUint64(w.inUse)
	w.floors = appendCapped(w.floors, floor, w.window)

	if w.seeded {
		if floor > w.prevFloor {
			w.floorRise++
		} else {
			w.floorRise = 0
		}
		if created > w.prevCreated {
			w.createdRise++
		} else {
			w.createdRise = 0
		}
	}
	w.prevFloor = floor
	w.prevCreated = created
	w.seeded = true

	r := poolHealthReading{
		Floor:              floor,
		FloorAtWindowStart: w.floors[0],
		RisingFor:          time.Duration(w.floorRise) * interval,
		CreatedRising:      w.createdRise >= w.leakRise,
		Full:               len(w.inUse) >= w.window,
	}
	if !r.Full {
		remaining := time.Duration(w.window-len(w.inUse)) * interval
		if remaining < time.Minute {
			remaining = time.Minute
		}
		r.WarmingRemaining = remaining
	}

	switch {
	case !r.Full:
		r.Verdict = poolVerdictWarming
	case w.floorRise >= w.leakRise:
		r.Verdict = poolVerdictLeak
	case w.floorRise >= w.watchRise, r.CreatedRising:
		r.Verdict = poolVerdictWatch
	default:
		r.Verdict = poolVerdictOK
	}
	return r
}

// poolHealthLine renders the [health][pool] body: a verdict, then plain
// English, then the counts. Healthy ticks stay on one short line; a tick with
// something to report spends the words to say what the number means and what
// happens if it keeps moving.
func poolHealthLine(r poolHealthReading, inUse, created, taken, returned uint64) string {
	returnedPct := 0.0
	if taken > 0 {
		returnedPct = 100 * float64(returned) / float64(taken)
	}
	counts := fmt.Sprintf("%d allocated since start (%.2f%% returned)", created, returnedPct)

	switch r.Verdict {
	case poolVerdictWarming:
		remaining := int(r.WarmingRemaining.Minutes())
		if remaining < 1 {
			remaining = 1
		}
		return fmt.Sprintf(
			"%s — %d buffers in use, %s. Stuck-buffer check needs 1h of uptime (%dm to go).",
			r.Verdict, inUse, counts, remaining,
		)

	case poolVerdictLeak:
		return fmt.Sprintf(
			"%s — %d buffers taken and never given back, climbing for %s. Memory will keep growing until restart. This is a bug worth reporting with this line. %d in use now, %s.",
			r.Verdict, r.Floor, roundMinutes(r.RisingFor), inUse, counts,
		)

	case poolVerdictWatch:
		if r.CreatedRising && r.Floor <= r.FloorAtWindowStart {
			return fmt.Sprintf(
				"%s — the pool keeps allocating new buffers an hour after startup, which usually means buffers are not coming back. %d in use now, %d stuck, %s.",
				r.Verdict, inUse, r.Floor, counts,
			)
		}
		return fmt.Sprintf(
			"%s — %d buffers were taken and never given back, up from %d an hour ago. If this keeps climbing, memory use grows until the provider restarts. %d in use now, %s.",
			r.Verdict, r.Floor, r.FloorAtWindowStart, inUse, counts,
		)

	default:
		stuck := "none stuck"
		if r.Floor > 0 {
			stuck = fmt.Sprintf("%d stuck but holding steady", r.Floor)
		}
		return fmt.Sprintf("%s — %d buffers in use, %s, %s", r.Verdict, inUse, stuck, counts)
	}
}

// roundMinutes renders a rise duration the way the line reads it: "30+
// minutes", never "30m0s".
func roundMinutes(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	if m <= 0 {
		m = 1
	}
	return fmt.Sprintf("%d+ minutes", m)
}

func appendCapped(s []uint64, v uint64, limit int) []uint64 {
	s = append(s, v)
	if len(s) > limit {
		s = s[len(s)-limit:]
	}
	return s
}

func minUint64(s []uint64) uint64 {
	if len(s) == 0 {
		return 0
	}
	m := s[0]
	for _, v := range s[1:] {
		if v < m {
			m = v
		}
	}
	return m
}
