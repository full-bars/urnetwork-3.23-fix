package urnettools

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui/tcellui"
)

// stubTopCmd swaps the terminal and screen seams and isolates HOME, returning
// counters for how often discovery ran and a screen was opened.
func stubTopCmd(t *testing.T, isTTY bool, providers []Provider, openErr error) (discovered, opened *int) {
	t.Helper()
	stubStatus(t, providers, nil, false)
	origTTY, origOpen := topStdoutIsTerminal, topOpenScreen
	t.Cleanup(func() { topStdoutIsTerminal, topOpenScreen = origTTY, origOpen })
	discovered, opened = new(int), new(int)
	origDisc := discoverStatusFn
	discoverStatusFn = func() []Provider { *discovered++; return origDisc() }
	topStdoutIsTerminal = func() bool { return isTTY }
	topOpenScreen = func() (tcellui.Screen, error) {
		*opened++
		return nil, openErr
	}
	return discovered, opened
}

// `urtop` from cron, a pipe or a redirect has no terminal to draw on. It must
// say so and point at the scriptable command, and it must do so before it
// discovers providers or touches the terminal.
func TestCmdTopRefusesWithoutATerminalBeforeDoingAnythingElse(t *testing.T) {
	discovered, opened := stubTopCmd(t, false, []Provider{{Unit: "a", Running: true}}, nil)

	err := cmdTop(nil)
	if err == nil {
		t.Fatal("top without a terminal must fail")
	}
	for _, want := range []string{"interactive terminal", "urnet-tools status", "--json"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should mention %q", err, want)
		}
	}
	if *discovered != 0 || *opened != 0 {
		t.Fatalf("nothing else may run first: discovery=%d screenOpens=%d", *discovered, *opened)
	}
}

// A bad flag is the operator's mistake and is reported as such, even without a
// terminal, since it would fail the same way on one.
func TestCmdTopRejectsABadIntervalBeforeTheTerminalCheck(t *testing.T) {
	stubTopCmd(t, false, nil, nil)
	err := cmdTop([]string{"--interval", "10ms"})
	if err == nil || !strings.Contains(err.Error(), "at least") {
		t.Fatalf("a too-small --interval should be rejected, got %v", err)
	}
}

// If the terminal cannot be opened (a dumb terminal, a broken TERM), the error
// names the alternative rather than dumping a tcell error alone.
func TestCmdTopReportsAScreenThatCannotOpen(t *testing.T) {
	_, opened := stubTopCmd(t, true, []Provider{{Unit: "a", Running: true}}, errors.New("terminal too dumb"))

	err := cmdTop(nil)
	if err == nil {
		t.Fatal("an unopenable screen must fail")
	}
	if *opened != 1 {
		t.Fatalf("the screen should be tried once, got %d", *opened)
	}
	for _, want := range []string{"cannot open the terminal", "terminal too dumb", "urnet-tools status"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should mention %q", err, want)
		}
	}
}

// --demo feeds synthetic snapshots so the screen can be looked at (and
// captured in tmux) without a provider. It must not discover providers: it is
// for a box that has none, and it must never read or write real state.
func TestCmdTopDemoSkipsDiscoveryAndNeedsNoProvider(t *testing.T) {
	discovered, opened := stubTopCmd(t, true, nil, errors.New("stop before drawing"))

	err := cmdTop([]string{"--demo"})
	if err == nil || !strings.Contains(err.Error(), "stop before drawing") {
		t.Fatalf("--demo with no providers should reach the screen, got %v", err)
	}
	if *discovered != 0 {
		t.Fatalf("--demo must not discover providers, discovery ran %d times", *discovered)
	}
	if *opened != 1 {
		t.Fatalf("the screen should be opened once, got %d", *opened)
	}
}

func TestStripDemoFlag(t *testing.T) {
	cases := []struct {
		in       []string
		demo     bool
		wantRest []string
	}{
		{nil, false, nil},
		{[]string{"--demo"}, true, nil},
		{[]string{"--interval", "2s", "--demo", "--unit", "x"}, true, []string{"--interval", "2s", "--unit", "x"}},
		{[]string{"--unit", "x"}, false, []string{"--unit", "x"}},
	}
	for _, c := range cases {
		demo, rest := stripDemoFlag(c.in)
		if demo != c.demo || strings.Join(rest, " ") != strings.Join(c.wantRest, " ") {
			t.Errorf("stripDemoFlag(%v) = %v, %v; want %v, %v", c.in, demo, rest, c.demo, c.wantRest)
		}
	}
}

func TestDemoSourceProducesValidSnapshotsThatMove(t *testing.T) {
	t0 := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	now := t0
	src := newDemoTopSource(func() time.Time { return now })

	first, err := src.Fetch(Provider{})
	if err != nil || first == nil {
		t.Fatalf("demo fetch: %v %v", first, err)
	}
	if got := len(first.Rate.HistoryBps); got != 600 {
		t.Fatalf("history should be a full 10 minute ring of 600, got %d", got)
	}
	for i, v := range first.Rate.HistoryBps {
		if v < 0 {
			t.Fatalf("history[%d] = %v is negative", i, v)
		}
	}
	switch first.State {
	case "starting", "degraded", "idle", "flowing":
	default:
		t.Fatalf("state %q is not one the real snapshot uses", first.State)
	}
	if first.Proxies.Up+first.Proxies.Degraded+first.Proxies.Connecting+first.Proxies.Dead == 0 {
		t.Fatal("demo needs proxies to draw the pool panel")
	}

	// The direction block and the light reply both carry the split, and the
	// lanes add back to the sums, so the direction pages have real data.
	d := first.Traffic.Directions
	if d == nil {
		t.Fatal("demo snapshot has no direction block")
	}
	if len(d.BillableRx.HistoryBps) != 600 || len(d.BillableTx.HistoryBps) != 600 ||
		len(d.TotalRx.HistoryBps) != 600 || len(d.TotalTx.HistoryBps) != 600 {
		t.Fatal("demo direction history is not a full ring")
	}
	if got := d.BillableRx.NowBps + d.BillableTx.NowBps; got != first.Rate.NowBps {
		t.Fatalf("billable split sums to %v, rate is %v", got, first.Rate.NowBps)
	}
	if got := d.TotalRx.NowBps + d.TotalTx.NowBps; got != first.Traffic.TotalNowBps {
		t.Fatalf("total split sums to %v, total rate is %v", got, first.Traffic.TotalNowBps)
	}
	// The family block splits the same lanes again, so the IPv4 / IPv6 page
	// draws two families that add back to each direction.
	if d.Families == nil {
		t.Fatal("demo snapshot has no IP family block")
	}
	for _, c := range []struct {
		name  string
		split SnapshotFamilySplit
		lane  SnapshotTrafficDirection
	}{
		{"billable rx", d.Families.BillableRx, d.BillableRx},
		{"billable tx", d.Families.BillableTx, d.BillableTx},
		{"total rx", d.Families.TotalRx, d.TotalRx},
		{"total tx", d.Families.TotalTx, d.TotalTx},
	} {
		if c.split.V4.NowBps+c.split.V6.NowBps != c.lane.NowBps {
			t.Fatalf("%s: families sum to %v, lane is %v", c.name, c.split.V4.NowBps+c.split.V6.NowBps, c.lane.NowBps)
		}
		if c.split.V6.NowBps <= 0 || c.split.V4.NowBps <= 0 {
			t.Fatalf("%s: the demo split is all one family: v4 %v v6 %v", c.name, c.split.V4.NowBps, c.split.V6.NowBps)
		}
	}
	tr, ok := src.(interface {
		FetchTraffic(Provider) (*LiveTraffic, error)
	})
	if !ok {
		t.Fatal("the demo source does not serve the light traffic reply")
	}
	lt, _ := tr.FetchTraffic(Provider{})
	if lt.BillableRxBytes == nil || lt.BillableTxBytes == nil || lt.TotalRxBytes == nil || lt.TotalTxBytes == nil {
		t.Fatalf("demo live traffic has no direction counters: %+v", lt)
	}
	if *lt.BillableRxBytes+*lt.BillableTxBytes != lt.BillableBytes ||
		*lt.TotalRxBytes+*lt.TotalTxBytes != lt.TotalBytes {
		t.Fatalf("demo live split does not add up: %+v", lt)
	}

	now = t0.Add(37 * time.Second)
	later, _ := src.Fetch(Provider{})
	if later.UptimeSeconds <= first.UptimeSeconds {
		t.Fatal("uptime must advance with the clock")
	}
	if later.Rate.NowBps == first.Rate.NowBps && later.Rate.HistoryBps[599] == first.Rate.HistoryBps[599] {
		t.Fatal("the demo rate must move over time or the graph shows nothing")
	}
	// Deterministic: the same instant gives the same picture (goldens and
	// screen captures depend on it).
	again, _ := src.Fetch(Provider{})
	if again.Rate.NowBps != later.Rate.NowBps {
		t.Fatal("demo output must be a pure function of the clock")
	}
}
