package urnettools

import (
	"fmt"
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
	// topDefaultInterval is the poll period. The provider caches its snapshot
	// for about a second, so polling faster than that costs nothing extra but
	// shows nothing new either; the floor exists to stop --interval 1ms from
	// hammering the control socket.
	topDefaultInterval = time.Second
	topMinInterval     = 250 * time.Millisecond
	// topRetryDelay is how long a silent provider is left alone between
	// attempts, and the number the DISCONNECTED countdown counts down from.
	topRetryDelay = 3 * time.Second
	// topMaxEvents bounds the session event list.
	topMaxEvents = 64

	// topMinWidth and topMinHeight are the smallest terminal the full layout is
	// designed for (the same size the widget layer's example screen uses).
	// Below it the compact single-panel view is drawn instead.
	topMinWidth  = 72
	topMinHeight = 20
)

var topMin = tui.MinSize{W: topMinWidth, H: topMinHeight}

// topIntervalSteps are the refresh rates + and - move between.
var topIntervalSteps = []time.Duration{
	250 * time.Millisecond, 500 * time.Millisecond, time.Second,
	2 * time.Second, 5 * time.Second, 10 * time.Second,
}

// topSource fetches one live snapshot for a provider. The real one asks the
// control socket; tests supply a fake.
type topSource interface {
	Fetch(p Provider) (*NodeSnapshot, error)
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
	return &topModel{theme: th, providers: providers, cur: cur, interval: interval, retry: topRetryDelay, now: now}
}

func (m *topModel) provider() Provider {
	if len(m.providers) == 0 {
		return Provider{}
	}
	return m.providers[m.cur]
}

// wantFetch reports whether a snapshot should be requested now and, if so,
// marks one in flight. A connected provider is polled every interval; a silent
// one is retried on the countdown, not every tick.
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
		if now.Sub(m.lastFetch) < m.interval {
			return 0, Provider{}, false
		}
	}
	m.fetching = true
	m.lastFetch = now
	return m.gen, m.provider(), true
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
		m.lost(now, err)
		return
	}
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
