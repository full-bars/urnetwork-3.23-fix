package urnettools

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

// topTick is how often the loop wakes to poll and to redraw the clock, the live
// rate and the DISCONNECTED countdown. It is the poll floor, so any interval
// that is a multiple of it is met exactly.
const topTick = topMinInterval

// topResult is one finished fetch, tagged with the selection it was for. A
// traffic result carries the light live counters instead of a snapshot.
type topResult struct {
	gen       int
	snap      *NodeSnapshot
	err       error
	isTraffic bool
	traffic   *LiveTraffic
	// The runtime views of the Internals panel.
	isInternals bool
	internals   *NodeInternals
	isGroups    bool
	groups      *GoroutineGroups
}

// runTop drives the model on a screen until the user quits, ctx is cancelled
// (SIGINT and SIGTERM cancel it), or the screen goes away. The terminal is
// given back on every way out, including a panic: a terminal left in raw mode
// on the alternate screen is the classic failure of a full-screen tool, so the
// restore does not depend on the happy path.
func runTop(ctx context.Context, scr tcellui.Screen, m *topModel, src topSource, tick time.Duration) (err error) {
	defer func() {
		r := recover()
		scr.Close()
		if r != nil {
			err = fmt.Errorf("urnet-tools top crashed: %v\n%s", r, debug.Stack())
		}
	}()

	results := make(chan topResult, 8)
	fetch := func(gen int, p Provider) {
		go func() {
			var res topResult
			res.gen = gen
			// A panic in a fetch goroutine would kill the process with the
			// terminal still in raw mode, so it is turned into an error the
			// model shows as a lost provider.
			defer func() {
				if r := recover(); r != nil {
					res.snap, res.err = nil, fmt.Errorf("snapshot fetch panicked: %v", r)
				}
				select {
				case results <- res:
				case <-ctx.Done():
				}
			}()
			res.snap, res.err = src.Fetch(p)
		}()
	}

	// The light counters are optional: a source without them (or a provider
	// that predates the command) leaves top on the snapshot's own rates.
	traffic, hasTraffic := src.(topTrafficSource)
	fetchTraffic := func(gen int, p Provider) {
		go func() {
			res := topResult{gen: gen, isTraffic: true}
			defer func() {
				if r := recover(); r != nil {
					res.traffic, res.err = nil, fmt.Errorf("traffic fetch panicked: %v", r)
				}
				select {
				case results <- res:
				case <-ctx.Done():
				}
			}()
			res.traffic, res.err = traffic.FetchTraffic(p)
		}()
	}

	// The runtime views are optional the same way.
	rtSrc, hasRuntime := src.(topInternalsSource)
	fetchInternalsFn := func(gen int, p Provider) {
		go func() {
			res := topResult{gen: gen, isInternals: true}
			defer func() {
				if r := recover(); r != nil {
					res.internals, res.err = nil, fmt.Errorf("internals fetch panicked: %v", r)
				}
				select {
				case results <- res:
				case <-ctx.Done():
				}
			}()
			res.internals, res.err = rtSrc.FetchInternals(p)
		}()
	}
	fetchGroupsFn := func(gen int, p Provider) {
		go func() {
			res := topResult{gen: gen, isGroups: true}
			defer func() {
				if r := recover(); r != nil {
					res.groups, res.err = nil, fmt.Errorf("goroutines fetch panicked: %v", r)
				}
				select {
				case results <- res:
				case <-ctx.Done():
				}
			}()
			res.groups, res.err = rtSrc.FetchGoroutines(p)
		}()
	}

	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	buf := tui.New(0, 0)
	for {
		if gen, p, ok := m.wantFetch(m.now()); ok {
			fetch(gen, p)
		}
		if hasTraffic {
			if gen, p, ok := m.wantTraffic(m.now()); ok {
				fetchTraffic(gen, p)
			}
		}
		if hasRuntime {
			if gen, p, ok := m.wantInternals(m.now()); ok {
				fetchInternalsFn(gen, p)
			}
			if gen, p, ok := m.wantGroups(m.now()); ok {
				fetchGroupsFn(gen, p)
			}
		}
		w, h := scr.Size()
		buf.Resize(w, h)
		buf.Fill(buf.Rect(), ' ', tui.Style{})
		m.render(buf)
		scr.Draw(buf)

		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-scr.Events():
			if !ok {
				return nil
			}
			if m.handle(ev) == topQuit {
				return nil
			}
		case r := <-results:
			switch {
			case r.isTraffic:
				m.applyTraffic(r.gen, r.traffic, r.err)
			case r.isInternals:
				m.applyInternals(r.gen, r.internals, r.err)
			case r.isGroups:
				m.applyGroups(r.gen, r.groups, r.err)
			default:
				m.apply(r.gen, r.snap, r.err)
			}
		case <-ticker.C:
		}
	}
}
