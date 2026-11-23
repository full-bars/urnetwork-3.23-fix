package urnettools

import (
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/urnetwork/connect/internal/brand"
	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

// The model behind `urnet-tools top`. It is deliberately free of the terminal,
// the clock and the network: it is fed snapshots, key events and the time, and
// it draws into a tui.Buffer. That is what lets the golden-frame tests drive it
// directly, and lets the run loop (top_run.go) stay a thin plumbing layer.

const (
	// topDefaultInterval is the poll period.
	topDefaultInterval = time.Second
	// topMinInterval is the fastest poll, like btop's 100ms. What is polled at
	// that rate is the light traffic counters; the full snapshot is fetched at
	// most once every topSnapshotEvery however fast the rate is set.
	topMinInterval = 100 * time.Millisecond
	// topSnapshotEvery is the slowest-changing half of the picture (proxies,
	// state, resources, per-second history). The provider caches its snapshot
	// for a second and building one is the costly part, so asking more often
	// gains nothing.
	topSnapshotEvery = time.Second
	// topRetryDelay is how long a silent provider is left alone between
	// attempts, and the number the DISCONNECTED countdown counts down from.
	topRetryDelay = 3 * time.Second
	// topMaxEvents bounds the session event list.
	topMaxEvents = 64

	// A provider on a starved box answers slowly; it is not gone. A provider that
	// has answered stays "connected but slow" through misses until it has been
	// silent for topLossGrace.
	topLossGrace = 20 * time.Second
	// topSlowReply is how long a reply may take before it counts as the provider
	// struggling: the polls after it are spaced by how long it took.
	topSlowReply = 1500 * time.Millisecond
	// topMinBackoff and topMaxBackoff bound the extra spacing added to the heavy
	// poll while the provider struggles. It starts at the minimum, doubles per
	// miss, and stops at the maximum.
	topMinBackoff = 2 * time.Second
	topMaxBackoff = 10 * time.Second
	// topLightTimeout is the read deadline of the cheap commands (traffic,
	// internals). If they cannot be answered this fast the reading is skipped
	// rather than left holding a socket and a goroutine on a struggling box.
	topLightTimeout = 1500 * time.Millisecond

	// topMinWidth and topMinHeight are the smallest terminal the full layout is
	// designed for (the same size the widget layer's example screen uses).
	// Below it the compact single-panel view is drawn instead.
	topMinWidth  = 72
	topMinHeight = 20
)

var topMin = tui.MinSize{W: topMinWidth, H: topMinHeight}

// topIntervalSteps are the refresh rates + and - move between.
var topIntervalSteps = []time.Duration{
	100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond, time.Second,
	2 * time.Second, 5 * time.Second, 10 * time.Second,
}

// topSource fetches one live snapshot for a provider. The real one asks the
// control socket; tests supply a fake.
type topSource interface {
	Fetch(p Provider) (*NodeSnapshot, error)
}

// topTrafficSource is the optional second half of a source: the light live
// counters. A source that lacks it (or a provider that predates the command)
// leaves top on the snapshot's own once-a-second rates.
type topTrafficSource interface {
	FetchTraffic(p Provider) (*LiveTraffic, error)
}

// topInternalsSource is the optional third half of a source: the runtime views
// of the Internals panel. A source that lacks it, or a provider that predates
// the commands, leaves the panel out.
type topInternalsSource interface {
	FetchInternals(p Provider) (*NodeInternals, error)
	FetchGoroutines(p Provider) (*GoroutineGroups, error)
}

// topSourceFunc adapts a function to topSource.
type topSourceFunc func(Provider) (*NodeSnapshot, error)

func (f topSourceFunc) Fetch(p Provider) (*NodeSnapshot, error) { return f(p) }

// socketTopSource reads the snapshot over the control socket, through the same
// seam `status` uses.
type socketTopSource struct{}

func (socketTopSource) Fetch(p Provider) (*NodeSnapshot, error) {
	snap, _, err := fetchSnapshotFn(p)
	return snap, err
}

func (socketTopSource) FetchTraffic(p Provider) (*LiveTraffic, error) {
	return fetchLiveTraffic(p)
}

func (socketTopSource) FetchInternals(p Provider) (*NodeInternals, error) {
	return fetchInternals(p)
}

func (socketTopSource) FetchGoroutines(p Provider) (*GoroutineGroups, error) {
	return fetchGoroutines(p)
}

type topConn int

const (
	// topConnecting is the state before the first answer of a provider.
	topConnecting topConn = iota
	topConnected
	topDisconnected
)

type topLevel int

const (
	topInfo topLevel = iota
	topGood
	topWarn
	topBad
)

// topEvent is one line of the Events panel.
type topEvent struct {
	At    time.Time
	Text  string
	Level topLevel
}

type topModel struct {
	theme     tui.Theme
	providers []Provider
	cur       int
	interval  time.Duration
	retry     time.Duration // pause between attempts on a silent provider
	now       func() time.Time

	conn      topConn
	snap      *NodeSnapshot // last good snapshot of the current provider
	downSince time.Time
	nextRetry time.Time
	lastErr   string
	events    []topEvent

	// Events view state: zoom, scroll and filter. The keys that drive them
	// are owned by the key table lane; the state and the pure functions live
	// here so the view is a function of the model alone.
	eventZoom   bool
	eventScroll int
	eventHeight int
	eventFilter eventFilter

	// copyOnExit writes the screen to the state dir when the user quits with
	// --copy. copyText holds what was written so the caller can print it once
	// the terminal is back; copyPath names the file.
	copyOnQuit bool
	copyPlain  bool
	copyText   string
	copyPath   string
	copyErr    error

	// overview is the all-providers screen. providerSnaps and providerExt
	// hold the last reading of every provider, so the rows come from data top
	// already fetched; overviewSel is the row the arrow keys move.
	overview      bool
	overviewSel   int
	providerSnaps []*NodeSnapshot
	providerExt   []ExtenderReading
	// overviewLast gates the once per snapshot cadence fetch of the providers
	// the live screen is not showing.
	overviewLast time.Time

	// extender is the last reading the control socket gave for the current
	// provider, fetched off the UI loop (see wantExtender)
	extender     ExtenderReading
	carriers     topCarrierHistory
	extenderBusy bool
	lastExtender time.Time

	// gen names the current provider selection. A fetch started for an earlier
	// selection can finish after Tab and must not be applied to the new one.
	gen       int
	fetching  bool
	lastFetch time.Time

	// The provider's health as seen by the polls. lastAnswer is the last reply of
	// any kind; failStreak counts snapshot requests missed in a row; backoff is the
	// extra spacing added to the heavy poll (zero when healthy); slowReported says
	// the Events panel already carries this episode, so it is not repeated.
	lastAnswer   time.Time
	lastDone     time.Time // when the last snapshot request finished, answered or not
	failStreak   int
	backoff      time.Duration
	slowReported bool

	// live is the sliding-window rate built from the light traffic polls.
	// trafficOK goes false once a provider says it does not know the command,
	// and trafficBusy marks a traffic poll in flight.
	live              topLive
	trafficHistory    topTrafficHistory
	transportsHistory topTransportHistory
	trafficOK         bool
	trafficBusy       bool
	lastTraffic       time.Time

	// winLevel selects the fixed graph window, winCustom overrides it when set
	// by the mouse wheel, and winRing holds the readings it draws, one per
	// traffic poll, trimmed to the longest window. peaks keeps the highest
	// rates since start, and showPeaks says the Now panel had the rows for them
	// this frame.
	winLevel  int
	winCustom time.Duration
	winRing   topGraphRing
	peaks     topPeaks
	showPeaks bool

	// The steady axis of each graph: one pair for the provider's own history,
	// one per graph window preset, and one for a custom wheel-zoomed window.
	scaleB, scaleT                   topScale
	winScaleB                        [len(topGraphWindows)]topScale
	winScaleT                        [len(topGraphWindows)]topScale
	winCustomScaleB, winCustomScaleT topScale

	// windowHours selects the history the graphs draw: 0 keeps the provider's
	// ten minute ring, 1 shows the downsampled hour, 24 the day. windowB and
	// windowT hold the client-side downsampled history, binned from the same
	// snapshots, so a provider never has to keep a longer ring.
	windowHours  int
	windowB      topWindow
	windowT      topWindow
	windowScaleB topScale
	windowScaleT topScale

	// rt is the runtime (Internals) panel's state.
	rt topRuntime

	// proxyCells is the decoded proxy_cells grid payload and when it was
	// fetched. The Proxies panel draws it as a heatmap when tall enough.
	proxyCells            []proxyCell
	proxyCellsAt          time.Time
	proxyGlow             map[int]proxyGlowSample
	proxyAfterglowSeconds int
	proxyHeatmapStyle     heatmapStyle
	proxyTimeline         []proxyTimelineSample
	proxyTimelineGroups   int
	// remote shows the mirrored transfer chart in the graph band (key r), and
	// remoteScales holds the easing state of each of its scales by group.
	remote       bool
	remoteScales map[string]*remoteScale
	// directionScales is the same easing state for the Billable and Total
	// direction pages, keyed per page and direction so the two pages never
	// share a height and neither eases against the other's peak.
	directionScales map[string]*remoteScale
	// proxyGridW and proxyGridH are the text-cell size of the drawn grid, used
	// for hit testing. Set by drawProxyGrid each frame.
	proxyGridW, proxyGridH int
	// proxyDetail is the full per-proxy records from proxy_cell_detail, keyed
	// by index, for the hover/click popup. Fetched on demand, not polled.
	proxyDetail              map[int]ProxyCellDetail
	proxyDetailAt            time.Time
	proxyPopup               bool
	proxyPopupFetch          bool
	proxyPopupX, proxyPopupY int
	proxyPopupRect           tui.Rect
	proxyPopupIndices        []int

	// graph is the graph glyph style. themeLocked is set when the environment
	// forced a theme (NO_COLOR, a dumb terminal), which the menu does not
	// override. settingsPath is where the menu saves; empty means nowhere.
	graph        tui.GraphSymbols
	themeLocked  bool
	savedTheme   string
	settingsPath string
	// logoSetting is the ui.logo value read from the settings file. top does
	// not change it, but a save rewrites the whole file, so it is written back.
	logoSetting *bool
	menu        bool
	menuSel     int
	menuNote    string

	// mouse is on in Mouse Mode, where the terminal reports clicks and hover to
	// top, and off in Keyboard Mode, where the terminal keeps them so its own
	// text selection works. K switches between the two (M is an alias) and
	// --no-mouse starts in Keyboard Mode.
	mouse bool
	// mouseSupported is false for a terminal that cannot report the mouse, so
	// K can say why it changed nothing instead of doing it silently.
	mouseSupported bool

	// notice is a one-line message shown in place of the footer hints for a
	// few seconds, and noticeUntil is when it gives the row back.
	notice      string
	noticeUntil time.Time

	// clip sends the screen to the clipboard for the c key. It is nil when top
	// has no terminal to copy from, and c is not offered then.
	clip *topClipboard

	// regions is the hit map for the last frame, rebuilt by every render.
	regions []topRegion
	// hover is the name of the region under the cursor, for hover highlighting.
	hover string

	// layout is the operator's arrangement of the screen; zero values are the
	// defaults. It is persisted with the menu settings.
	layout topLayout
	// drag is the divider being dragged, if any; lastPress sees double clicks.
	drag      *topDrag
	lastPress topPress
	// The geometry of the last full frame, used to translate pointer cells
	// into divider rows while dragging.
	contentRect                 tui.Rect
	nowRows, resRows, extRows   int
	showExtender, withInternals bool
	// page is the selected page's index in topPages. It belongs to the session,
	// not to a provider, so switching provider keeps the page.
	page int
	// stripHidden is set by the p key: the page strip is drawn by default and
	// this hides it for the rest of the session. It is not saved anywhere.
	stripHidden bool
	// compact records that the last frame was the single-panel view, where the
	// page keys do nothing.
	compact bool
	// gorScroll is the wheel offset of the goroutine list.
	gorScroll int
	gorHeight int

	// logo says the logo may be drawn in this run, and logoCaps is what the
	// terminal can show. splash is the logo shown once at start, and about and
	// aboutRect are the About window and where it was drawn (top_logo.go).
	logo      bool
	logoCaps  brand.Capabilities
	splash    topSplash
	about     bool
	aboutRect tui.Rect

	help  bool
	quit  bool
	panel string
}

func newTopModel(providers []Provider, cur int, interval time.Duration, th tui.Theme, now func() time.Time) *topModel {
	if now == nil {
		now = time.Now
	}
	if interval <= 0 {
		interval = topDefaultInterval
	}
	if cur < 0 || cur >= len(providers) {
		cur = 0
	}
	m := &topModel{theme: th, providers: providers, cur: cur, interval: interval, retry: topRetryDelay, now: now, trafficOK: true, mouse: true, mouseSupported: true, winLevel: topGraphWindowDefault}
	m.providerSnaps = make([]*NodeSnapshot, len(providers))
	m.providerExt = make([]ExtenderReading, len(providers))
	m.rt.reset()
	m.carriers.reset()
	return m
}

func (m *topModel) provider() Provider {
	if len(m.providers) == 0 {
		return Provider{}
	}
	return m.providers[m.cur]
}

// wantFetch reports whether a snapshot should be requested now and, if so,
// marks one in flight. A connected provider is polled every interval, though
// never faster than topSnapshotEvery; a silent one is retried on the
// countdown, not every tick.
func (m *topModel) wantFetch(now time.Time) (gen int, p Provider, ok bool) {
	if m.fetching || len(m.providers) == 0 {
		return 0, Provider{}, false
	}
	switch {
	case m.lastFetch.IsZero():
	case m.conn == topDisconnected:
		if now.Before(m.nextRetry) {
			return 0, Provider{}, false
		}
	default:
		// A healthy provider is polled on a steady cadence measured from the start
		// of the last request. While it struggles the spacing is measured from
		// when that request FINISHED: from its start, a 5 second miss would already
		// have used up a 2 second backoff and the next request would go out at once.
		since := now.Sub(m.lastFetch)
		if m.backoff > 0 && m.lastDone.After(m.lastFetch) {
			since = now.Sub(m.lastDone)
		}
		if since < max(m.interval, topSnapshotEvery)+m.backoff {
			return 0, Provider{}, false
		}
	}
	m.fetching = true
	m.lastFetch = now
	return m.gen, m.provider(), true
}

// wantTraffic reports whether the light traffic counters should be read now
// and, if so, marks one in flight. They are read every interval once the
// provider is connected, and never for a provider that has said it does not
// know the command.
func (m *topModel) wantTraffic(now time.Time) (gen int, p Provider, ok bool) {
	if m.trafficBusy || !m.trafficOK || m.conn != topConnected || len(m.providers) == 0 {
		return 0, Provider{}, false
	}
	if m.failStreak >= 2 {
		return 0, Provider{}, false // two misses in a row: add no load at all
	}
	if !m.lastTraffic.IsZero() && now.Sub(m.lastTraffic) < m.interval {
		return 0, Provider{}, false
	}
	m.trafficBusy = true
	m.lastTraffic = now
	return m.gen, m.provider(), true
}

// applyTraffic folds one traffic reading into the live rate. A missed reading
// only skips a sample: losing the provider is the snapshot path's call, so a
// slow counter read never flips the screen to DISCONNECTED. A provider that does
// not know the command is left on the snapshot's own rates.
func (m *topModel) applyTraffic(gen int, lt *LiveTraffic, err error) {
	if gen != m.gen {
		return
	}
	m.trafficBusy = false
	// A reading that started before a disconnect is not current: `lost`
	// resets the live window without bumping the generation, so without this
	// check a stale reading could become the first sample in the reset window
	// and distort post-reconnect rates.
	if m.conn != topConnected {
		return
	}
	switch {
	case errors.Is(err, errTrafficUnsupported):
		m.trafficOK = false
		m.live.reset()
		m.transportsHistory.reset()
	case err != nil || lt == nil:
	default:
		m.markAnswered(m.now())
		last := int64(0)
		if n := len(m.live.samples); n > 0 {
			last = m.live.samples[n-1].at
			// A long gap has no samples for its missing intervals. Start a new
			// rate window so its average does not fill the gap with traffic.
			if time.Duration(lt.AtUnixNano-last) > 2*m.interval {
				m.live.reset()
				m.transportsHistory.reset()
			}
		}
		m.live.add(lt.AtUnixNano, lt.BillableBytes, lt.TotalBytes)
		// The billable direction and packet rates of this reply, for page 2.
		dirs := m.trafficHistory.observeDirs(lt, 2*max(m.interval, time.Second))
		// The peak is the reading's own rate, per second however fast the poll
		// is. A counter that went backwards restarts the live window, so the
		// pair is only read when it moves forward.
		if n := len(m.live.samples); n >= 2 {
			a, b := m.live.samples[n-2], m.live.samples[n-1]
			if span := time.Duration(b.at - a.at); span > 0 && b.billable >= a.billable && b.total >= a.total {
				secs := span.Seconds()
				m.peaks.observeLive(m.now(), float64(b.billable-a.billable)/secs, float64(b.total-a.total)/secs)
			}
		}
		// The transports page differences two of these readings into a window
		// share, so they are kept next to the live window that accepted them.
		m.transportsHistory.push(lt.AtUnixNano, lt.Transports)
		// One zoom column per reading the window accepted; a stalled clock adds
		// none, and a restarted window has no span yet, so `recent` says not ok.
		if n := len(m.live.samples); n > 0 && m.live.samples[n-1].at != last {
			if b, t, ok := m.live.recent(topZoomSpan); ok {
				if len(m.trafficHistory.billable) == 0 && m.snap != nil {
					history := m.snap.Rate.HistoryBps
					for i, value := range history {
						// The seeded seconds are dated back from this reading.
						m.trafficHistory.arrived = m.now().Add(-time.Duration(len(history)-1-i) * time.Second)
						total := math.NaN()
						if m.snap.Traffic != nil && i < len(m.snap.Traffic.TotalHistoryBps) {
							total = m.snap.Traffic.TotalHistoryBps[i]
						}
						at := lt.AtUnixNano - int64(len(history)-i)*int64(time.Second)
						m.trafficHistory.push(at, value, total)
						m.trafficHistory.setDirs(snapshotDirRates(m.snap, i, len(history)))
						// The graphs draw the window from the same history, so
						// a provider that has been up for a while fills the
						// longer levels at once instead of from the first poll.
						m.winRing.push(at-int64(time.Second), time.Second, value, total)
					}
				}
				m.trafficHistory.arrived = m.now()
				if m.interval <= time.Second {
					m.trafficHistory.pushColumn(lt.AtUnixNano, time.Duration(lt.AtUnixNano-last), b, t)
				} else {
					m.trafficHistory.pushSlot(lt.AtUnixNano, b, t)
				}
				m.trafficHistory.setDirs(dirs)
				m.winRing.push(lt.AtUnixNano, time.Duration(lt.AtUnixNano-last), b, t)
			}
		}
	}
}

// apply folds the outcome of a fetch into the model. Results for a provider
// that is no longer selected are dropped.
func (m *topModel) apply(gen int, snap *NodeSnapshot, err error) {
	if gen != m.gen {
		return
	}
	m.fetching = false
	now := m.now()
	m.lastDone = now
	if err != nil || snap == nil {
		if isTimeoutErr(err) && m.snap != nil && m.conn == topConnected && now.Sub(m.lastAnswer) < topLossGrace {
			m.missed(now, err)
			return
		}
		if m.cur < len(m.providerSnaps) {
			m.providerSnaps[m.cur] = nil
		}
		m.lost(now, err)
		return
	}
	// How long the request took, when it is known: a result applied with no
	// recorded start has no latency to judge.
	var took time.Duration
	if !m.lastFetch.IsZero() && now.After(m.lastFetch) {
		took = now.Sub(m.lastFetch)
	}
	m.answered(now, took)
	prev := m.snap
	if m.conn == topDisconnected && prev != nil {
		m.addEvent(now, fmt.Sprintf("reconnected after %s", tui.Duration(now.Sub(m.downSince))), topGood)
	}
	if prev == nil {
		// The first snapshot of a selection also carries the last restart, so
		// the Events panel is not empty on a provider that has been up for days.
		if txt := restartEventText(snap); txt != "" {
			m.addEvent(startedAt(snap, now), txt, topInfo)
		}
	} else {
		m.noteChange(now, prev, snap)
	}
	m.conn = topConnected
	m.snap = snap
	// The provider computes its own one and five minute averages, so the peaks
	// come from the same figures the rows above them show.
	{
		total1m, total5m := 0.0, 0.0
		if tr := snap.Traffic; tr != nil {
			total1m, total5m = tr.TotalAvg1mBps, tr.TotalAvg5mBps
		}
		m.peaks.observeAverages(now, snap.Rate.Avg1mBps, snap.Rate.Avg5mBps, total1m, total5m)
	}
	if m.cur < len(m.providerSnaps) {
		// The overview rows read the same snapshot the live screen does, so
		// the per provider history accumulates as top polls.
		m.providerSnaps[m.cur] = snap
	}
	m.lastErr = ""
	m.windowB.push(snap.Rate.NowBps, now)
	if snap.Traffic != nil {
		m.windowT.push(snap.Traffic.TotalNowBps, now)
	}
}

// lost records that the provider stopped answering (or never did).
func (m *topModel) lost(now time.Time, err error) {
	reason := "no answer"
	if err != nil {
		reason = strings.TrimPrefix(err.Error(), errSnapshotUnavailable.Error()+": ")
	}
	if m.conn != topDisconnected {
		m.downSince = now
		what := "lost the provider"
		if m.snap == nil {
			what = "provider not answering"
		}
		m.addEvent(now, what+": "+reason, topBad)
	}
	m.conn = topDisconnected
	m.lastErr = reason
	m.nextRetry = now.Add(m.retry)
	m.live.reset() // a rate from before the outage would read as current
	m.transportsHistory.reset()
	m.failStreak, m.backoff, m.slowReported = 0, 0, false
	// Clear only the Internals DATA, not its state. ok/gOK say whether the
	// provider knows the command (its refusal survives the outage apparent,
	// so we do not re-ask a provider that cannot answer); showing is the
	// user's view choice; busy/gBusy guard an in-flight fetch whose result
	// carries the same generation. A reading from before the outage would
	// otherwise draw at full brightness while the panels around it dim.
	m.rt.cur, m.rt.recent, m.rt.gor, m.rt.gorAt, m.rt.groups = nil, nil, nil, 0, nil
}

// isTimeoutErr reports a request that got no answer in time, which on a starved
// box means slow. A refused connection or a missing socket is the opposite: the
// provider is not there (an update or a restart looks exactly like that), and
// that is still "lost" at once. The cause's type survives the wrapping in
// fetchSnapshot; the text check covers a source that flattened it.
func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, os.ErrDeadlineExceeded) || strings.Contains(err.Error(), "i/o timeout")
}

// markAnswered records proof of life: any reply, however small.
func (m *topModel) markAnswered(now time.Time) { m.lastAnswer = now }

// isSlow reports a provider that is connected but struggling: it has missed its
// last poll, or its last reply took long enough that polls are being spaced out.
func (m *topModel) isSlow() bool {
	return m.conn == topConnected && (m.failStreak > 0 || m.backoff > 0)
}

// missed folds in a snapshot request that got no answer from a provider that has
// answered recently. The last good data stays on screen. The heavy poll backs
// off (doubling per miss, capped), and one Events line marks the episode once it
// is more than a blip.
func (m *topModel) missed(now time.Time, err error) {
	m.failStreak++
	switch {
	case m.backoff == 0:
		m.backoff = topMinBackoff
	default:
		m.backoff = min(m.backoff*2, topMaxBackoff)
	}
	if err != nil {
		m.lastErr = strings.TrimPrefix(err.Error(), errSnapshotUnavailable.Error()+": ")
	}
	if m.failStreak >= 2 && !m.slowReported {
		m.slowReported = true
		m.addEvent(now, fmt.Sprintf("provider slow: no answer for %s", tui.Duration(now.Sub(m.lastAnswer))), topWarn)
	}
}

// answered folds in a snapshot that arrived after took. A prompt one ends any
// slow episode; one that took seconds keeps the provider marked slow, with the
// polls spaced by how long it took, so the next request does not land on a
// provider still busy with this one.
func (m *topModel) answered(now time.Time, took time.Duration) {
	m.lastAnswer = now
	m.failStreak = 0
	if took >= topSlowReply {
		m.backoff = min(max(took, topMinBackoff), topMaxBackoff)
		return
	}
	m.backoff = 0
	if m.slowReported {
		m.slowReported = false
		m.addEvent(now, "provider responsive again", topGood)
	}
}

// noteChange records what differs between two consecutive good snapshots that
// an operator would want to see: a restart, a version change, a state change.
func (m *topModel) noteChange(now time.Time, prev, cur *NodeSnapshot) {
	restarted := (prev.StartedAt != "" && cur.StartedAt != "" && prev.StartedAt != cur.StartedAt) ||
		cur.UptimeSeconds+5 < prev.UptimeSeconds
	if restarted {
		txt := restartEventText(cur)
		if txt == "" {
			txt = "restart"
		}
		m.addEvent(now, txt, topWarn)
	} else if prev.Version != cur.Version {
		m.addEvent(now, fmt.Sprintf("version %s to %s", orDash(prev.Version), orDash(cur.Version)), topWarn)
	}
	if prev.State != cur.State {
		lvl := topInfo
		switch cur.State {
		case "flowing":
			lvl = topGood
		case "idle":
			lvl = topWarn
		case "degraded":
			lvl = topBad
		}
		m.addEvent(now, fmt.Sprintf("state %s to %s", orDash(prev.State), orDash(cur.State)), lvl)
	}
	if cur.RestartPending && !prev.RestartPending {
		m.addEvent(now, "restart pending: config changes are waiting", topWarn)
	}
}

func (m *topModel) addEvent(at time.Time, text string, lvl topLevel) {
	m.events = append(m.events, topEvent{At: at, Text: text, Level: lvl})
	if len(m.events) > topMaxEvents {
		m.events = m.events[len(m.events)-topMaxEvents:]
	}
}

// restartEventText is "restart: update (v1 to v2)" from the snapshot's own
// restart fields; the version pair appears only when the version changed.
func restartEventText(s *NodeSnapshot) string {
	if s.Restart.Reason == "" {
		return ""
	}
	txt := "restart: " + s.Restart.Reason
	if s.PreviousVersion != nil && *s.PreviousVersion != "" && *s.PreviousVersion != s.Version {
		txt += fmt.Sprintf(" (%s to %s)", *s.PreviousVersion, s.Version)
	}
	return txt
}

// startedAt is when the provider process started, or fallback when the
// snapshot does not say.
func startedAt(s *NodeSnapshot, fallback time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339, s.StartedAt); err == nil {
		return t.In(fallback.Location())
	}
	return fallback
}

// topEffect is what the run loop must do after an input event.
type topEffect int

const (
	topNone topEffect = iota
	topQuit
	// topRefetch means the selection changed and the new provider should be
	// asked at once.
	topRefetch
	// topCopy means the loop should send the frame it last drew to the
	// clipboard. The loop owns the buffer, so the key only asks for it.
	topCopy
)

// handle applies one input event.
func (m *topModel) handle(ev tcellui.Event) topEffect {
	// Whether this event may switch pages is decided before the event closes
	// anything: the key or click that dismisses a popup must not also jump to
	// another page.
	pageKeys := m.pageKeysActive()
	if eff, done := m.splashHandle(ev); done {
		return eff
	}
	if eff, done := m.aboutHandle(ev); done {
		return eff
	}
	if ev.Kind == tcellui.EventKey || ev.Kind == tcellui.EventRune {
		m.proxyPopup = false
	}
	if (m.menu || m.help || m.panel != "") && ev.Kind == tcellui.EventMouse && ev.Action == tcellui.MouseRelease {
		if m.drag != nil {
			m.saveLayout()
		}
		m.drag = nil
		m.lastPress = topPress{}
	}
	if m.menu && m.menuHandle(ev) {
		return topNone
	}
	if m.overview && ev.Kind == tcellui.EventKey {
		switch ev.Key {
		case tcellui.KeyEsc:
			m.overview = false
			return topNone
		case tcellui.KeyUp:
			m.overviewSel = max(0, m.overviewSel-1)
			return topNone
		case tcellui.KeyDown:
			m.overviewSel = min(len(m.providers)-1, m.overviewSel+1)
			return topNone
		case tcellui.KeyEnter:
			m.overview = false
			return m.selectProviderIndex(m.overviewSel)
		}
	}
	if m.overview && ev.Kind == tcellui.EventRune && (ev.Rune == '?' || ev.Rune == 'm') {
		return topNone
	}
	switch ev.Kind {
	case tcellui.EventQuit:
		m.quit = true
		return topQuit
	case tcellui.EventMouse:
		return m.mouseHandle(ev)
	case tcellui.EventKey:
		switch ev.Key {
		case tcellui.KeyCtrlC:
			m.quit = true
			return topQuit
		case tcellui.KeyEsc:
			if m.panel != "" {
				if m.panel == "goroutines" {
					m.rt.showing = false
				}
				m.panel = ""
				return topNone
			}
			// Esc backs out of the help overlay first; it quits only from the
			// main screen.
			if m.help {
				m.help = false
				return topNone
			}
			m.quit = true
			return topQuit
		case tcellui.KeyTab:
			return m.selectProvider(1)
		case tcellui.KeyBacktab:
			return m.selectProvider(-1)
		case tcellui.KeyPageDown:
			if pageKeys {
				m.stepPage(1)
			}
		case tcellui.KeyPageUp:
			if pageKeys {
				m.stepPage(-1)
			}
		}
	case tcellui.EventRune:
		switch ev.Rune {
		case 'q', 'Q':
			m.quit = true
			return topQuit
		case '?':
			m.help = !m.help
		case 'A':
			// About takes every mouse event, so a divider drag in progress would
			// never see its release. End it here the way a release does.
			if m.drag != nil {
				m.saveLayout()
			}
			m.drag, m.lastPress = nil, topPress{}
			m.about, m.help = true, false
		case 'm':
			m.menu, m.help, m.menuNote = true, false, ""
		case 'K':
			m.toggleMouseMode()
		case 'M':
			// The old key, kept as a hidden alias of K so nobody loses the
			// toggle. It is not in the footer, the help or the docs.
			m.toggleMouseMode()
		case 'c':
			// The loop holds the frame, so the key only asks for the copy.
			if m.clip == nil {
				m.setNotice(topNoticeNoClipboard)
				return topNone
			}
			return topCopy
		case 't':
			switch m.windowHours {
			case 0:
				m.windowHours = 1
			case 1:
				m.windowHours = 24
			default:
				m.windowHours = 0
			}
		case 'o':
			if len(m.providers) > 1 {
				m.overview, m.overviewSel = true, m.cur
			}
		case 'a':
			m.panel = "alerts"
		case 'i':
			m.panel = "gauges"
		case 'v':
			m.panel = "carriers"
		// Like btop: + lengthens the update interval (slower), - shortens it.
		case '+', '=':
			m.stepInterval(1)
		case '-', '_':
			m.stepInterval(-1)
		case 'w':
			m.stepGraphWindow(1)
			m.saveSettings()
		case 'W':
			m.stepGraphWindow(-1)
			m.saveSettings()
		case 'r', 'R':
			// The Remote chart takes the bottom of the graph band. Switching it
			// on starts its scales from the current window, not from whatever
			// they eased to the last time it was shown.
			m.remote = !m.remote
			if m.remote {
				m.remoteScales = nil
			}
		case 'u', 'U':
			tui.SetDecimalUnits(!tui.DecimalUnits())
		case 'g', 'G':
			// Only where the provider can answer; the list is fetched on the
			// next loop turn.
			if m.rt.gOK && m.rt.ok {
				switch {
				case m.panel == "goroutines":
					m.panel, m.rt.showing = "", false
				case m.withInternals:
					// The Internals panel is on screen: the list replaces its rows.
					m.rt.showing = !m.rt.showing
				default:
					// No room for the Internals panel, so the list opens as an
					// overlay like the alerts and the gauges. Without this the
					// key flipped a hidden switch and nothing showed.
					m.panel, m.rt.showing = "goroutines", true
				}
				m.rt.gLast = time.Time{}
			}
		case 'p', 'P':
			// The page strip belongs to the page keys: a popup, the overview,
			// the help or the menu keeps the key for itself, and the compact
			// view has no strip to toggle.
			if pageKeys {
				m.togglePageStrip()
			}
		case ']':
			if pageKeys {
				m.stepPage(1)
			}
		case '[':
			if pageKeys {
				m.stepPage(-1)
			}
		default:
			// 1 to 9 jump straight to a page, numbered as the strip numbers
			// them.
			if ev.Rune >= '1' && ev.Rune <= '9' && pageKeys {
				m.selectPage(int(ev.Rune - '1'))
			}
		}
	}
	return topNone
}

// selectProvider moves the selection by delta, wrapping, and resets the view:
// history and events belong to one provider.
func (m *topModel) selectProvider(delta int) topEffect {
	n := len(m.providers)
	if n < 2 {
		return topNone
	}
	return m.selectProviderIndex(((m.cur+delta)%n + n) % n)
}

// selectProviderIndex jumps straight to the provider at index i, the pick the
// overview screen returns. The reset is the same one the Tab key performs.
func (m *topModel) selectProviderIndex(i int) topEffect {
	if i < 0 || i >= len(m.providers) || i == m.cur {
		return topNone
	}
	m.cur = i
	m.gen++
	m.conn, m.snap, m.events = topConnecting, nil, nil
	m.downSince, m.nextRetry, m.lastErr = time.Time{}, time.Time{}, ""
	m.fetching, m.lastFetch = false, time.Time{}
	m.live.reset()
	m.trafficHistory = topTrafficHistory{}
	m.winRing.reset()
	m.peaks.reset()
	for i := range m.winScaleB {
		m.winScaleB[i].reset()
		m.winScaleT[i].reset()
	}
	m.transportsHistory.reset()
	m.trafficOK, m.trafficBusy, m.lastTraffic = true, false, time.Time{}
	m.proxyCells, m.proxyCellsAt = nil, time.Time{}
	m.proxyGlow = nil
	m.proxyDetail = nil
	m.proxyDetailAt = time.Time{}
	m.proxyPopup = false
	m.extender = ExtenderReading{}
	m.carriers.reset()
	m.extenderBusy = false
	m.lastExtender = time.Time{}
	m.lastAnswer, m.lastDone, m.failStreak, m.backoff, m.slowReported = time.Time{}, time.Time{}, 0, 0, false
	m.winRing.reset()
	for i := range m.winScaleB {
		m.winScaleB[i].reset()
		m.winScaleT[i].reset()
	}
	m.winCustomScaleB.reset()
	m.winCustomScaleT.reset()
	m.scaleB.reset()
	m.scaleT.reset()
	m.windowB.reset()
	m.windowT.reset()
	m.windowScaleB.reset()
	m.windowScaleT.reset()
	m.remoteScales = nil
	m.directionScales = nil
	showing := m.rt.showing
	m.rt.reset()
	m.rt.showing = showing // the view choice is the user's, not the provider's
	m.eventScroll, m.gorScroll = 0, 0
	return topRefetch
}

// wantOverview reports whether the overview screen should ask every other
// provider for one snapshot. It fires at the snapshot cadence while the screen
// is open, which is the same load as the one provider on screen.
func (m *topModel) wantOverview(now time.Time) (gen int, ok bool) {
	if !m.overview || len(m.providers) == 0 {
		return 0, false
	}
	if !m.overviewLast.IsZero() && now.Sub(m.overviewLast) < max(m.interval, topSnapshotEvery) {
		return 0, false
	}
	m.overviewLast = now
	return m.gen, true
}

// applyOverview stores one reading the overview fetch brought in. A result for
// an earlier selection is dropped, like every other fetch.
func (m *topModel) applyOverview(gen, idx int, snap *NodeSnapshot, err error, ext ExtenderReading) {
	if gen != m.gen || idx < 0 || idx >= len(m.providers) {
		return
	}
	if err != nil {
		snap = nil
	}
	m.providerSnaps[idx] = snap
	if ext.Source != "" {
		m.providerExt[idx] = ext
	}
}

// stepInterval moves the poll period one step faster (dir < 0) or slower
// (dir > 0) along topIntervalSteps, starting from wherever a custom
// --interval sits between them.
func (m *topModel) stepInterval(dir int) {
	steps := topIntervalSteps
	if dir < 0 {
		for i := len(steps) - 1; i >= 0; i-- {
			if steps[i] < m.interval {
				m.interval = steps[i]
				return
			}
		}
		return
	}
	for _, s := range steps {
		if s > m.interval {
			m.interval = s
			return
		}
	}
}

// history returns the throughput samples for the graph, oldest first.
// topRates is the current billable and total throughput. totalOK is false when
// there is no total figure at all (a provider that predates the traffic block).
type topRates struct {
	billable float64
	total    float64
	totalOK  bool
}

// rates is the live rate when top has one, otherwise the snapshot's own.
func (m *topModel) rates() topRates {
	var r topRates
	if m.snap == nil {
		return r
	}
	r.billable = m.snap.Rate.NowBps
	if m.snap.Traffic != nil {
		r.total, r.totalOK = m.snap.Traffic.TotalNowBps, true
	}
	if m.conn == topConnected {
		if b, t, ok := m.live.rates(); ok {
			r.billable, r.total, r.totalOK = b, t, true
		}
	}
	return r
}

// topSeries is what the two graphs draw: the provider's per-second history, the
// time base to bucket it against, and the live rate as a separate tail.
type topSeries struct {
	billable []float64
	// total is nil when the provider has no total-traffic history.
	total []float64
	// anchor is the absolute index of the newest history sample. It is the
	// provider's own sample count when it sends one: that identifies each sample
	// exactly, however the poll and the provider's tick line up. From a provider
	// that predates it, the snapshot's clock second stands in, and zero means no
	// usable time base at all.
	anchor int64
	// The live rate, drawn as its own newest column rather than as a sample in
	// the series. Appended to the series it would sit at a guessed index and be
	// replaced by the provider's real sample a second later, changing a column
	// that was already complete.
	tailBillable, tailTotal float64
	live                    bool
	// capacity is the most columns the graph holds, and scaleB and scaleT the
	// steady axis of each graph. window says the series is one fixed graph
	// window with level its index in topGraphWindows. at, spans and covered
	// describe the readings behind the window so drawGraph can place them on a
	// time axis of the plot width.
	capacity       int
	scaleB, scaleT *topScale
	window         bool
	columns        bool
	level          int
	winSpan        time.Duration
	covered        time.Duration
	full           bool
	at             []int64
	spans          []time.Duration
}

// series builds what the graphs draw from the last snapshot and the live rate.
func (m *topModel) series() topSeries {
	var out topSeries
	if m.snap == nil {
		return out
	}
	if len(m.winRing.at) > 0 {
		level := m.winLevel
		if level < 0 || level >= len(topGraphWindows) {
			level = topGraphWindowDefault
		}
		span := m.graphWindow()
		out.window, out.level, out.winSpan = true, level, span
		if m.winCustom > 0 {
			out.scaleB, out.scaleT = &m.winCustomScaleB, &m.winCustomScaleT
		} else {
			out.scaleB, out.scaleT = &m.winScaleB[level], &m.winScaleT[level]
		}
		out.at, out.spans = m.winRing.at, m.winRing.spans
		out.anchor = m.winRing.at[len(m.winRing.at)-1]
		out.covered, out.full = m.winRing.cover(span)
		out.billable = m.winRing.billable
		if m.snap.Traffic != nil {
			out.total = m.winRing.total
		}
		return out
	}
	if m.windowHours > 0 {
		out.scaleB, out.scaleT = &m.windowScaleB, &m.windowScaleT
		now := m.now()
		switch m.windowHours {
		case 1:
			out.anchor = now.Unix() / 60
		case 24:
			out.anchor = now.Unix() / 900
		}
		out.billable = m.windowB.windowFor(m.windowHours, now)
		out.capacity = len(out.billable)
		if m.snap.Traffic != nil {
			out.total = m.windowT.windowFor(m.windowHours, now)
		}
		if m.conn == topConnected {
			out.tailBillable, out.tailTotal, out.live = m.live.rates()
		}
		return out
	}
	out.capacity = topGraphCapacity
	out.scaleB, out.scaleT = &m.scaleB, &m.scaleT
	switch {
	case m.snap.Rate.HistorySeq > 0:
		out.anchor = int64(m.snap.Rate.HistorySeq)
	default:
		if t, err := time.Parse(time.RFC3339, m.snap.Now); err == nil {
			out.anchor = t.Unix()
		}
	}
	out.billable = m.snap.Rate.HistoryBps
	if m.snap.Traffic != nil {
		out.total = m.snap.Traffic.TotalHistoryBps
	}
	if m.conn == topConnected {
		out.tailBillable, out.tailTotal, out.live = m.live.rates()
	}
	return out
}

func (m *topModel) history() []float64 {
	if m.snap == nil {
		return nil
	}
	return m.snap.Rate.HistoryBps
}

// windowFor returns the series for a window: h is 1 or 24, and the zero value
// of windowHours keeps the provider's own ten minute ring.
func (m *topModel) windowFor(h int) []float64 {
	if m.snap == nil {
		return nil
	}
	switch h {
	case 1:
		return m.windowB.windowFor(1, m.now())
	case 24:
		return m.windowB.windowFor(24, m.now())
	}
	return m.history()
}

// sortedEvents returns the events newest first. Events with the same time keep
// the order they happened in, latest on top, which is why the copy is built
// back to front before the stable sort.
func (m *topModel) sortedEvents() []topEvent {
	out := make([]topEvent, 0, len(m.events))
	for i := len(m.events) - 1; i >= 0; i-- {
		out = append(out, m.events[i])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

// verdict is the state word in the header, with the role it is drawn in.
func (m *topModel) verdict() (string, topLevel) {
	switch m.conn {
	case topConnecting:
		return "CONNECTING", topInfo
	case topDisconnected:
		// The countdown runs on the model's clock, not the wall clock, so a
		// frame is a pure function of the model.
		left := max(m.nextRetry.Sub(m.now()), 0)
		return fmt.Sprintf("DISCONNECTED retry in %ds", int((left+time.Second-1)/time.Second)), topBad
	}
	state := strings.ToUpper(m.snap.State)
	if state == "" {
		state = "UNKNOWN"
	}
	switch m.snap.State {
	case "flowing":
		return state, topGood
	case "idle":
		return state, topWarn
	case "degraded":
		return state, topBad
	}
	return state, topInfo
}

func (m *topModel) style(l topLevel) tui.Style {
	switch l {
	case topGood:
		return m.theme.OK
	case topWarn:
		return m.theme.Warn
	case topBad:
		return m.theme.Bad
	}
	return m.theme.Dim
}

// nodeLabel names the provider in the header: its network when known.
func topNodeLabel(p Provider) string {
	if p.Network != "" {
		return p.Network
	}
	return providerLabel(p)
}
