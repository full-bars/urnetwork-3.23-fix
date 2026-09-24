package urnettools

import (
	"math"
	"sync"
	"time"
)

// Demo mode (`urnet-tools top --demo`, deliberately not in the help text) draws
// the screen from synthetic snapshots so it can be looked at, or captured in
// tmux, on a box with no provider. Every value is a pure function of the clock
// it is given, so the same instant always gives the same picture and tests can
// assert on it. It never discovers providers and never touches real state.

const (
	demoHistoryLen = 600
	demoMiB        = 1 << 20
)

// demoAnchor is the fixed instant demo uptime counts from, so uptime advances
// with the clock without any state.
var demoAnchor = time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)

// demoProvider is the one provider a demo session shows.
func demoProvider() Provider {
	return Provider{Unit: "urnetwork-demo.service", User: "demo", Network: "demo", Running: true}
}

// stripDemoFlag removes --demo from args and reports whether it was there. It
// runs before the real flag parsers so they never see it.
func stripDemoFlag(args []string) (demo bool, rest []string) {
	for _, a := range args {
		if a == "--demo" {
			demo = true
			continue
		}
		rest = append(rest, a)
	}
	return demo, rest
}

// demoRateAt is the billable rate in bytes per second at instant t: two
// overlapping waves, and a short quiet spell every 15 minutes so the idle state
// and its hint get drawn too.
func demoRateAt(t time.Time) float64 {
	s := float64(t.Unix())
	if int64(s)%900 >= 780 && int64(s)%900 < 840 {
		return 0
	}
	r := 22*demoMiB + 14*demoMiB*math.Sin(2*math.Pi*s/180) + 5*demoMiB*math.Sin(2*math.Pi*s/37)
	if r < 0 {
		r = 0
	}
	return math.Round(r)
}

// demoRateAtF is demoRateAt at a fractional instant, so the live counters move
// smoothly between whole seconds. It agrees with demoRateAt on whole seconds.
func demoRateAtF(t time.Time) float64 {
	s := float64(t.UnixNano()) / 1e9
	if m := int64(s) % 900; m >= 780 && m < 840 {
		return 0
	}
	return 22*demoMiB + 14*demoMiB*math.Sin(2*math.Pi*s/180) + 5*demoMiB*math.Sin(2*math.Pi*s/37)
}

// demoTotalFactor is how much more total traffic there is than billable in the
// demo: everything moved is billable plus 60%.
const demoTotalFactor = 1.6

// demoLive integrates the demo rate into cumulative counters for the light
// traffic poll, the way a real provider's byte counters grow.
type demoLive struct {
	mu       sync.Mutex
	last     time.Time
	billable float64
}

// demoTopSource serves synthetic snapshots computed from now(), and live
// counters that grow with it.
type demoTopSource struct {
	now  func() time.Time
	live *demoLive
}

func newDemoTopSource(now func() time.Time) topSource {
	return demoTopSource{now: now, live: &demoLive{}}
}

// FetchTraffic advances the counters to now in 100ms steps and reports them.
func (d demoTopSource) FetchTraffic(Provider) (*LiveTraffic, error) {
	now := d.now()
	d.live.mu.Lock()
	defer d.live.mu.Unlock()
	if d.live.last.IsZero() {
		d.live.last = now
	}
	for t := d.live.last; t.Before(now); {
		step := min(100*time.Millisecond, now.Sub(t))
		d.live.billable += demoRateAtF(t.Add(step/2)) * step.Seconds()
		t = t.Add(step)
	}
	d.live.last = now
	return &LiveTraffic{
		AtUnixNano:    now.UnixNano(),
		BillableBytes: uint64(d.live.billable),
		TotalBytes:    uint64(d.live.billable * demoTotalFactor),
	}, nil
}

func (d demoTopSource) Fetch(Provider) (*NodeSnapshot, error) {
	now := d.now().UTC().Truncate(time.Second)

	hist := make([]float64, demoHistoryLen)
	var sum1, sum5 float64
	for i := range hist {
		hist[i] = demoRateAt(now.Add(-time.Duration(demoHistoryLen-1-i) * time.Second))
		if i >= demoHistoryLen-60 {
			sum1 += hist[i]
		}
		if i >= demoHistoryLen-300 {
			sum5 += hist[i]
		}
	}
	nowBps := hist[demoHistoryLen-1]

	state := "flowing"
	var hint *string
	if nowBps == 0 {
		state = "idle"
		h := "no traffic assigned by the platform right now (demo)"
		hint = &h
	}
	s := float64(now.Unix())
	prev := "v3.23.0-fix.32.0"
	memLimit := uint64(4 * 1024 * demoMiB)
	rss := uint64(2300 * demoMiB)
	fds, fdLimit := 310+int(20*math.Sin(s/50)), 65536

	totalHist := make([]float64, len(hist))
	for i, v := range hist {
		totalHist[i] = math.Round(v * demoTotalFactor)
	}
	uptime := now.Sub(demoAnchor).Seconds()
	lifetime := uint64(281 * 1024 * 1024 * 1024 * 1024)
	traffic := &SnapshotTraffic{
		BillableBytes:         uint64(22 * demoMiB * uptime),
		TotalBytes:            uint64(22 * demoMiB * uptime * demoTotalFactor),
		LifetimeBillableBytes: &lifetime,
		TotalNowBps:           math.Round(nowBps * demoTotalFactor),
		TotalAvg1mBps:         math.Round(sum1 / 60 * demoTotalFactor),
		TotalAvg5mBps:         math.Round(sum5 / 300 * demoTotalFactor),
		TotalHistoryBps:       totalHist,
	}

	return &NodeSnapshot{
		V:               1,
		Version:         "v3.23.0-fix.demo",
		PreviousVersion: &prev,
		StartedAt:       demoAnchor.Format(time.RFC3339),
		Now:             now.Format(time.RFC3339),
		UptimeSeconds:   now.Sub(demoAnchor).Seconds(),
		State:           state,
		Rate: SnapshotRate{
			NowBps:                 nowBps,
			Avg1mBps:               math.Round(sum1 / 60),
			Avg5mBps:               math.Round(sum5 / 300),
			HistoryIntervalSeconds: 1,
			HistoryBps:             hist,
		},
		Clients:  150 + int(60*math.Sin(s/90)),
		Sessions: SnapshotSessions{PQE: 90 + int(20*math.Sin(s/70)), Classical: 60},
		Proxies:  SnapshotProxies{Up: 58, Degraded: 2},
		Pressure: math.Round((0.21+0.1*math.Sin(s/120))*100) / 100,
		Restart:  SnapshotRestart{Reason: "update", CleanShutdown: true},
		Resources: SnapshotResources{
			HeapInuseBytes: uint64(1800*demoMiB) + uint64(120*demoMiB*(1+math.Sin(s/60))/2),
			MemLimitBytes:  &memLimit,
			RSSBytes:       &rss,
			Goroutines:     1200 + int(40*math.Sin(s/45)),
			OpenFDs:        &fds,
			FDLimit:        &fdLimit,
		},
		IdleHint: hint,
		Traffic:  traffic,
	}, nil
}
