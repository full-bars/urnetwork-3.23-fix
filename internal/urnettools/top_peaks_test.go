package urnettools

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// The Now panel keeps the highest rates top has seen since it started, for
// billable and for total traffic: the live reading, the one minute average and
// the five minute average. The averages count only once their window is full,
// so a partial window early after start cannot inflate them.

func TestTopPeaksHoldTheHighestLiveRate(t *testing.T) {
	var p topPeaks
	at := topBase
	for _, v := range []float64{1e6, 5e6, 2e6, 0, 3e6} {
		at = at.Add(time.Second)
		p.observeLive(at, v, v*2)
	}
	if p.billable.live != 5e6 || p.total.live != 10e6 {
		t.Fatalf("peaks are %v and %v, want the highest 5e6 and 1e7", p.billable.live, p.total.live)
	}
}

func TestTopPeaksKeepBillableAndTotalApart(t *testing.T) {
	var p topPeaks
	at := topBase
	p.observeLive(at.Add(time.Second), 9e6, 1e6)
	if p.billable.live != 9e6 || p.total.live != 1e6 {
		t.Fatalf("billable %v total %v", p.billable.live, p.total.live)
	}
	p.observeLive(at.Add(2*time.Second), 1e6, 9e6)
	if p.billable.live != 9e6 || p.total.live != 9e6 {
		t.Fatalf("after the second reading: billable %v total %v", p.billable.live, p.total.live)
	}
}

func TestTopPeaksWaitForTheirAveragesToFill(t *testing.T) {
	var p topPeaks
	start := topBase
	p.observeLive(start, 1e6, 1e6)
	// Half a minute in: the one minute average is still partial.
	for i := 1; i <= 30; i++ {
		p.observeAverages(start.Add(time.Duration(i)*time.Second), 9e6, 8e6, 7e6, 6e6)
	}
	if p.billable.has1 || p.total.has1 || p.billable.has5 || p.total.has5 {
		t.Fatal("an average before its window is full must stay a dash")
	}
	// A full minute: the one minute peak counts, the five minute one does not.
	p.observeAverages(start.Add(time.Minute), 9e6, 8e6, 7e6, 6e6)
	if !p.billable.has1 || p.billable.min1 != 9e6 || !p.total.has1 || p.total.min1 != 7e6 {
		t.Fatalf("one minute peaks %v and %v", p.billable.min1, p.total.min1)
	}
	if p.billable.has5 || p.total.has5 {
		t.Fatal("the five minute average is not full yet")
	}
	// Five minutes: the five minute peak counts, and neither peak comes down
	// when the provider's own average drops.
	p.observeAverages(start.Add(5*time.Minute), 1e6, 4e6, 1e6, 3e6)
	if !p.billable.has5 || p.billable.min5 != 4e6 || p.total.min5 != 3e6 {
		t.Fatalf("five minute peaks %v and %v", p.billable.min5, p.total.min5)
	}
	if p.billable.min1 != 9e6 || p.total.min1 != 7e6 {
		t.Fatalf("the one minute peaks dropped to %v and %v", p.billable.min1, p.total.min1)
	}
}

func TestTopPeaksResetWithTheProcess(t *testing.T) {
	var p topPeaks
	p.observeLive(topBase, 5e6, 5e6)
	p.reset()
	if p.billable.live != 0 || p.billable.has1 || p.since != (time.Time{}) {
		t.Fatalf("after a reset: %+v", p.billable)
	}
}

// The peaks are fed by the same readings the rows above them show.
func TestTopPeaksFollowTheTrafficPolls(t *testing.T) {
	m, clock := liveModel(t, 500*time.Millisecond)
	var at int64 = 1_000_000_000_000
	var bytes uint64
	// A burst, then quiet traffic: the peak keeps the burst.
	feedTrafficAt(t, m, clock, 4, 500*time.Millisecond, 8_000_000, &at, &bytes)
	feedTrafficAt(t, m, clock, 4, 500*time.Millisecond, 1_000, &at, &bytes)
	if m.peaks.billable.live < 7.9e6 || m.peaks.billable.live > 8.1e6 {
		t.Fatalf("live peak %v, want the 8 MB/s burst", m.peaks.billable.live)
	}
	if m.peaks.total.live < 15.9e6 || m.peaks.total.live > 16.1e6 {
		t.Fatalf("total live peak %v, want twice the burst", m.peaks.total.live)
	}
}

func TestTopPeaksShowInTheNowPanel(t *testing.T) {
	m, clock := liveModel(t, 500*time.Millisecond)
	var at int64 = 1_000_000_000_000
	var bytes uint64
	feedTrafficAt(t, m, clock, 8, 500*time.Millisecond, 2_000_000, &at, &bytes)
	txt, _ := showOnSim(t, m, 120, 40)
	if !strings.Contains(txt, "peak/s") || !strings.Contains(txt, "bill") || !strings.Contains(txt, "total") {
		t.Fatalf("the peaks block is missing:\n%s", txt)
	}
	// The live peak is about 2 MB/s, and the averages are dashes until their
	// windows are full.
	for _, ln := range strings.Split(txt, "\n") {
		if strings.Contains(ln, "peak/s") {
			if !strings.Contains(ln, "live") || !strings.Contains(ln, "1m") || !strings.Contains(ln, "5m") {
				t.Fatalf("peak header lacks its columns: %q", ln)
			}
		}
		if strings.Contains(ln, "bill") && strings.Contains(ln, "1.9M") {
			if !strings.Contains(ln, "-") {
				t.Fatalf("the averages are not dashes yet: %q", ln)
			}
		}
	}
}

func TestTopPeaksUseTheChosenUnits(t *testing.T) {
	defer tui.SetDecimalUnits(tui.DecimalUnits())
	m, clock := liveModel(t, 500*time.Millisecond)
	var at int64 = 1_000_000_000_000
	var bytes uint64
	feedTrafficAt(t, m, clock, 8, 500*time.Millisecond, 1_000_000, &at, &bytes)
	tui.SetDecimalUnits(false)
	bin, _ := showOnSim(t, m, 120, 40)
	tui.SetDecimalUnits(true)
	dec, _ := showOnSim(t, m, 120, 40)
	if bin == dec {
		t.Fatal("the unit toggle does not reach the peaks")
	}
}

func TestTopPeaksStepAsideOnAShortPanel(t *testing.T) {
	m, clock := liveModel(t, 500*time.Millisecond)
	var at int64 = 1_000_000_000_000
	var bytes uint64
	feedTrafficAt(t, m, clock, 8, 500*time.Millisecond, 2_000_000, &at, &bytes)
	txt, _ := showOnSim(t, m, 100, 24)
	if strings.Contains(txt, "peak/s") {
		t.Fatalf("the peaks block took rows a short panel needs:\n%s", txt)
	}
}

// The peaks block takes three rows from the side column, never from the graphs
// or the panels below it.
func TestTopPeaksDoNotStarveTheDashboard(t *testing.T) {
	for _, h := range []int{30, 34, 40} {
		m, clock := liveModel(t, 500*time.Millisecond)
		var at int64 = 1_000_000_000_000
		var bytes uint64
		feedTrafficAt(t, m, clock, 8, 500*time.Millisecond, 2_000_000, &at, &bytes)
		txt, _ := showOnSim(t, m, 120, h)
		for _, want := range []string{"Billable,", "Proxies", "Events", "Now"} {
			if !strings.Contains(txt, want) {
				t.Fatalf("at 120x%d the %q box is missing:\n%s", h, want, txt)
			}
		}
		if h >= 40 && !strings.Contains(txt, "peak/s") {
			t.Fatalf("at 120x%d the peaks block should fit:\n%s", h, txt)
		}
	}
}
