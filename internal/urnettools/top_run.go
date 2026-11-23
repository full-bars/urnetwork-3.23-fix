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

// topMinFrame is the shortest time between two redraws, 20 frames a second.
const topMinFrame = 50 * time.Millisecond

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
	// the extender reading
	isExtender    bool
	extender      ExtenderReading
	isProxyDetail bool
	proxyDetail   map[int]ProxyCellDetail
	isProxyCells  bool
	proxyCells    []proxyCell
	isOverview    bool
	provider      int
}

// topFetchOverview reads one provider and recovers source panics.
func topFetchOverview(src topSource, p Provider, gen, i int) (res topResult) {
	res = topResult{gen: gen, isOverview: true, provider: i}
	defer func() {
		if r := recover(); r != nil {
			res.snap, res.err = nil, fmt.Errorf("overview fetch panicked: %v", r)
		}
	}()
	res.snap, res.err = src.Fetch(p)
	if extSrc, ok := src.(topExtenderSource); ok {
		func() {
			defer func() {
				if recover() != nil {
					res.extender = ExtenderReading{Source: SourceUnknown}
				}
			}()
			res.extender = extSrc.FetchExtender(p)
		}()
	}
	return res
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
	var detailBusy bool
	fetchDetail := func(gen int, p Provider) {
		detailBusy = true
		m.proxyDetailAt = m.now()
		go func() {
			res := topResult{gen: gen, isProxyDetail: true}
			defer func() {
				if r := recover(); r != nil {
					res.err = fmt.Errorf("proxy detail fetch panicked: %v", r)
				}
				select {
				case results <- res:
				case <-ctx.Done():
				}
			}()
			res.proxyDetail, res.err = fetchProxyCellDetail(p)
		}()
	}
	var proxyBusy bool
	fetchCells := func(gen int, p Provider) {
		proxyBusy = true
		go func() {
			res := topResult{gen: gen, isProxyCells: true}
			defer func() {
				if r := recover(); r != nil {
					res.err = fmt.Errorf("proxy cells fetch panicked: %v", r)
				}
				select {
				case results <- res:
				case <-ctx.Done():
				}
			}()
			res.proxyCells, res.err = fetchProxyCells(p)
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
	fetchInternalsFn := func(gen int, p Provider, now time.Time) {
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
			if res.err == nil && res.internals != nil {
				res.internals.SigpipeRisk = sigpipeRestartRiskCached(p, now)
			}
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

	// The overview fetches one snapshot and extender reading of every provider
	// the live screen is not showing, at the snapshot cadence.
	overviewBusy := make(map[int]int)
	fetchOverview := func(gen, i int, p Provider) {
		if previous, ok := overviewBusy[i]; ok && previous == gen {
			return
		}
		overviewBusy[i] = gen
		go func() {
			res := topFetchOverview(src, p, gen, i)
			select {
			case results <- res:
			case <-ctx.Done():
			}
		}()
	}

	// The extender box is optional the same way.
	extSrc, hasExtender := src.(topExtenderSource)
	fetchExtender := func(gen int, p Provider) {
		go func() {
			res := topResult{gen: gen, isExtender: true}
			defer func() {
				if r := recover(); r != nil {
					res.extender = ExtenderReading{Source: SourceUnknown}
				}
				select {
				case results <- res:
				case <-ctx.Done():
				}
			}()
			res.extender = extSrc.FetchExtender(p)
		}()
	}

	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	buf := tui.New(0, 0)
	defer func() {
		if m.copyOnQuit && buf.Width() > 0 && buf.Height() > 0 {
			m.copyOnExit(buf)
		}
	}()
	scr.SetMouse(m.mouse)
	lastMouse := m.mouse
	var lastDraw time.Time
	dirty := true
	for {
		if m.mouse != lastMouse {
			scr.SetMouse(m.mouse)
			lastMouse = m.mouse
		}
		if m.proxyPopup && m.proxyPopupFetch && !detailBusy && m.cur >= 0 && m.cur < len(m.providers) {
			m.proxyPopupFetch = false
			if m.proxyDetailAt.IsZero() || m.now().Sub(m.proxyDetailAt) >= 5*time.Second {
				if _, ok := src.(socketTopSource); ok {
					fetchDetail(m.gen, m.providers[m.cur])
				}
			}
		}
		if gen, p, ok := m.wantFetch(m.now()); ok {
			fetch(gen, p)
			if _, ok := src.(socketTopSource); ok && !proxyBusy {
				fetchCells(gen, p)
			}
		}
		if hasTraffic {
			if gen, p, ok := m.wantTraffic(m.now()); ok {
				fetchTraffic(gen, p)
			}
		}
		if hasExtender {
			if gen, p, ok := m.wantExtender(m.now()); ok {
				fetchExtender(gen, p)
			}
		}
		if gen, ok := m.wantOverview(m.now()); ok {
			for i, p := range m.providers {
				if i == m.cur {
					continue // the main poll keeps this one fresh
				}
				fetchOverview(gen, i, p)
			}
		}
		if hasRuntime {
			if gen, p, ok := m.wantInternals(m.now()); ok {
				fetchInternalsFn(gen, p, m.now())
			}
			if gen, p, ok := m.wantGroups(m.now()); ok {
				fetchGroupsFn(gen, p)
			}
		}
		// Draw only when something changed, and at most once per topMinFrame.
		// Every poll result, tick and mouse move wakes the loop, which at a
		// fast refresh interval is over a hundred wakeups a second. Drawing on
		// each one costs a core's worth of CPU and sends the whole changed
		// screen down the terminal link every time.
		var frameWait <-chan time.Time
		if dirty {
			if since := time.Since(lastDraw); lastDraw.IsZero() || since >= topMinFrame {
				w, h := scr.Size()
				buf.Resize(w, h)
				buf.Fill(buf.Rect(), ' ', tui.Style{})
				m.render(buf)
				scr.Draw(buf)
				lastDraw, dirty = time.Now(), false
			} else {
				frameWait = time.After(topMinFrame - since)
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-frameWait:
		case ev, ok := <-scr.Events():
			if !ok {
				return nil
			}
			// A mouse move that leaves the hover as it was changes nothing on
			// screen, so it does not ask for a redraw.
			move := ev.Kind == tcellui.EventMouse && ev.Action == tcellui.MouseMove
			before := ""
			if move {
				before = m.hoverSignature()
			}
			// A copy reads the frame the loop last drew, so the buffer stays
			// owned by this goroutine and nothing races the renderer.
			switch m.handle(ev) {
			case topQuit:
				return nil
			case topCopy:
				m.copyFrame(buf)
			}
			if !move || m.hoverSignature() != before {
				dirty = true
			}
		case r := <-results:
			dirty = true
			switch {
			case r.isProxyDetail:
				detailBusy = false
				m.applyProxyCellDetail(r.gen, r.proxyDetail, r.err)
			case r.isProxyCells:
				proxyBusy = false
				m.applyProxyCells(r.gen, r.proxyCells, r.err)
			case r.isExtender:
				m.applyExtender(r.gen, r.extender)
			case r.isOverview:
				if overviewBusy[r.provider] == r.gen {
					delete(overviewBusy, r.provider)
				}
				m.applyOverview(r.gen, r.provider, r.snap, r.err, r.extender)
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
			dirty = true
		}
	}
}
