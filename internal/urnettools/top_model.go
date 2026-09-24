package urnettools

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

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
	failStreak   int
	backoff      time.Duration
	slowReported bool

	// live is the sliding-window rate built from the light traffic polls.
	// trafficOK goes false once a provider says it does not know the command,
	// and trafficBusy marks a traffic poll in flight.
	live        topLive
	trafficOK   bool
	trafficBusy bool
	lastTraffic time.Time

	help bool
	quit bool
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
	return &topModel{theme: th, providers: providers, cur: cur, interval: interval, retry: topRetryDelay, now: now, trafficOK: true}
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
		if now.Sub(m.lastFetch) < max(m.interval, topSnapshotEvery)+m.backoff {
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
	case err != nil || lt == nil:
	default:
		m.markAnswered(m.now())
		m.live.add(lt.AtUnixNano, lt.BillableBytes, lt.TotalBytes)
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
	if err != nil || snap == nil {
		if isTimeoutErr(err) && m.snap != nil && m.conn == topConnected && now.Sub(m.lastAnswer) < topLossGrace {
			m.missed(now, err)
			return
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
	m.lastErr = ""
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
	m.failStreak, m.backoff, m.slowReported = 0, 0, false
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
)

// handle applies one input event.
func (m *topModel) handle(ev tcellui.Event) topEffect {
	switch ev.Kind {
	case tcellui.EventQuit:
		m.quit = true
		return topQuit
	case tcellui.EventKey:
		switch ev.Key {
		case tcellui.KeyCtrlC:
			m.quit = true
			return topQuit
		case tcellui.KeyEsc:
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
		}
	case tcellui.EventRune:
		switch ev.Rune {
		case 'q', 'Q':
			m.quit = true
			return topQuit
		case '?':
			m.help = !m.help
		case '+', '=':
			m.stepInterval(-1)
		case '-', '_':
			m.stepInterval(1)
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
	m.cur = ((m.cur+delta)%n + n) % n
	m.gen++
	m.conn, m.snap, m.events = topConnecting, nil, nil
	m.downSince, m.nextRetry, m.lastErr = time.Time{}, time.Time{}, ""
	m.fetching, m.lastFetch = false, time.Time{}
	m.live.reset()
	m.trafficOK, m.trafficBusy, m.lastTraffic = true, false, time.Time{}
	m.lastAnswer, m.failStreak, m.backoff, m.slowReported = time.Time{}, 0, 0, false
	return topRefetch
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
}

// series builds what the graphs draw from the last snapshot and the live rate.
func (m *topModel) series() topSeries {
	var out topSeries
	if m.snap == nil {
		return out
	}
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
