package urnettools

import (
	"fmt"
	"math"
	"net"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/internal/tui"
)

// The proxy grid: one colored dot per proxy, drawn into the Proxies panel.
//
// The provider answers the compact "proxy_cells" command with 4 packed bytes
// per proxy (index, state, throughput bucket, clients). This file decodes
// that payload and draws it as a heatmap. Sparse grids use one cell per proxy.
// Dense grids use two dot-rows per text cell.

// proxyCell is one decoded grid cell from the provider's proxy_cells reply.
type proxyCell struct {
	index   int
	state   uint8 // connect.ProxyCellUp .. ProxyCellParked
	bucket  uint8 // 0..7 throughput bucket
	clients uint8 // capped at 255
}

// proxyCellStateName maps a state code to its legend label.
func proxyCellStateName(state uint8) string {
	switch state {
	case connect.ProxyCellUp:
		return "up"
	case connect.ProxyCellDegraded:
		return "dropped"
	case connect.ProxyCellConnecting:
		return "connecting"
	case connect.ProxyCellDead:
		return "dead"
	case connect.ProxyCellParked:
		return "parked"
	}
	return "unknown"
}

// proxyCellSeverity ranks states for the worst-state rule: dead is worst,
// parked is best (it is resting on purpose, not broken).
func proxyCellSeverity(state uint8) int {
	switch state {
	case connect.ProxyCellDead:
		return 4
	case connect.ProxyCellDegraded:
		return 3
	case connect.ProxyCellConnecting:
		return 2
	case connect.ProxyCellParked:
		return 1
	}
	return 0 // up
}

// decodeProxyCells unpacks the provider's 4-bytes-per-proxy wire format.
// Extra trailing bytes are ignored.
func decodeProxyCells(b []byte) []proxyCell {
	n := len(b) / 4
	out := make([]proxyCell, 0, n)
	for i := 0; i+4 <= len(b); i += 4 {
		out = append(out, proxyCell{
			index:   int(b[i]) | int(b[i+1])<<8,
			state:   b[i+2] >> 5,
			bucket:  b[i+2] & 0x07,
			clients: b[i+3],
		})
	}
	return out
}

// fetchUDPScanEvents asks the provider for recent finished UDP checks for the scan wave.
func fetchUDPScanEvents(p Provider) ([]ProxyUDPScanEvent, error) {
	if p.StateDir == "" {
		return nil, errSnapshotUnavailable
	}
	sockPath := filepath.Join(p.StateDir, "provider.sock")
	resp, err := sendSocketRequestTimeout(sockPath, controlRequest{Cmd: "udp_scan_events"}, topLightTimeout)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errSnapshotUnavailable, err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("%w: %s", errSnapshotUnavailable, resp.Error)
	}
	return resp.UDPScanEvents, nil
}

// fetchProxyCells asks the provider for the compact grid payload. A provider
// that predates the command (or has no proxies) leaves the field empty; the
// caller treats that as "no grid" and falls back to the bars.
func fetchProxyCells(p Provider) ([]proxyCell, []ProxyUDPScanEvent, error) {
	if p.StateDir == "" {
		return nil, nil, errSnapshotUnavailable
	}
	sockPath := filepath.Join(p.StateDir, "provider.sock")
	resp, err := sendSocketRequestTimeout(sockPath, controlRequest{Cmd: "proxy_cells"}, topLightTimeout)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", errSnapshotUnavailable, err)
	}
	if !resp.OK {
		if len(resp.Error) >= 15 && resp.Error[:15] == "unknown command" {
			return nil, nil, errTrafficUnsupported
		}
		return nil, nil, fmt.Errorf("%w: %s", errSnapshotUnavailable, resp.Error)
	}
	return decodeProxyCells(resp.ProxyCells), resp.UDPScanEvents, nil
}

const defaultProxyAfterglowSeconds = 5

type proxyGlowSample struct {
	bucket float64
	at     time.Time
}

func (m *topModel) proxyGlowBucket(c proxyCell, now time.Time) uint8 {
	seconds := m.proxyAfterglowSeconds
	if seconds <= 0 {
		seconds = defaultProxyAfterglowSeconds
	}
	level := float64(c.bucket)
	if previous, ok := m.proxyGlow[c.index]; ok {
		age := now.Sub(previous.at).Seconds()
		if age >= 0 {
			level = max(level, previous.bucket*max(0, 1-age/float64(seconds)))
		}
	}
	return uint8(level + 0.5)
}

// applyProxyCells folds a fetched grid into the model. A nil payload clears
// the grid (provider predates the command) so the panel falls back to bars.
func (m *topModel) applyProxyCells(gen int, cells []proxyCell, err error) {
	if gen != m.gen {
		return
	}
	if err != nil || cells == nil {
		m.proxyCells = nil
		m.proxyGlow = nil
		return
	}
	now := m.now()
	next := make(map[int]proxyGlowSample, len(cells))
	for _, c := range cells {
		if c.state != connect.ProxyCellUp {
			continue
		}
		previous, ok := m.proxyGlow[c.index]
		if ok && m.proxyGlowBucket(c, now) > c.bucket {
			next[c.index] = previous
		} else if c.bucket > 0 {
			next[c.index] = proxyGlowSample{bucket: float64(c.bucket), at: now}
		}
	}
	m.proxyGlow = next
	m.proxyCells = cells
	m.proxyCellsAt = now
}

// proxyGridLayout maps text cells to proxy positions. Sparse grids give each
// proxy a text cell. Dense grids retain the two dot-rows per text cell.
// When hasDirect is true, cell (0, 0) is reserved as a full cell for the
// direct slot (index 0), and proxies start after it without being grouped with it.
type proxyGridLayout struct {
	width, height, cols, rows int
	sparse                    bool
	hasDirect                 bool
}

func newProxyGridLayout(n, w, h int, direct ...bool) proxyGridLayout {
	l := proxyGridLayout{width: w, height: h}
	if len(direct) > 0 {
		l.hasDirect = direct[0]
	}
	if n <= 0 || w <= 0 || h <= 0 {
		return l
	}
	if n < w*h {
		l.sparse = true
		l.cols = min(w, max(1, int(math.Ceil(math.Sqrt(float64(n)*float64(w)/float64(h))))))
		l.rows = (n + l.cols - 1) / l.cols
		if l.rows > h {
			l.rows = h
			l.cols = (n + h - 1) / h
		}
	} else {
		l.cols, l.rows = w, 2*h
	}
	return l
}

func (l proxyGridLayout) proxyCapacity() int {
	if !l.hasDirect {
		return l.cols * l.rows
	}
	if l.sparse {
		return l.cols*l.rows - 1
	}
	if l.width <= 0 || l.height <= 0 {
		return 0
	}
	cap := 2 * (l.width*l.height - 1)
	if cap < 1 {
		return 1
	}
	return cap
}

func gridCoordinate(i, count, size int, direct ...bool) int {
	if count <= 1 {
		if len(direct) > 0 && direct[0] {
			return 0
		}
		return (size - 1) / 2
	}
	return i * (size - 1) / (count - 1)
}

// indices returns the two dense dot positions or the single sparse position.
func (l proxyGridLayout) indices(x, y int) (int, int) {
	if x < 0 || y < 0 || x >= l.width || y >= l.height || l.cols == 0 {
		return -1, -1
	}
	if l.sparse {
		if l.hasDirect && x == 0 && y == 0 {
			return 0, -1
		}
		col, row := -1, -1
		for i := 0; i < l.cols; i++ {
			if gridCoordinate(i, l.cols, l.width, l.hasDirect) == x {
				col = i
				break
			}
		}
		for i := 0; i < l.rows; i++ {
			if gridCoordinate(i, l.rows, l.height, l.hasDirect) == y {
				row = i
				break
			}
		}
		if col < 0 || row < 0 {
			return -1, -1
		}
		return row*l.cols + col, -1
	}

	if !l.hasDirect {
		return x*l.rows + 2*y, x*l.rows + 2*y + 1
	}

	// Dense layout with direct slot:
	// Cell (0, 0) is reserved for the full cell direct star.
	if x == 0 && y == 0 {
		return 0, -1
	}
	if x == 0 {
		upper := 1 + 2*(y-1)
		return upper, upper + 1
	}
	col0Proxies := 2 * (l.height - 1)
	start := 1 + col0Proxies + (x-1)*l.rows
	upper := start + 2*y
	return upper, upper + 1
}

func (l proxyGridLayout) cellFor(index int) (x, y int, upper bool) {
	if index < 0 || l.cols == 0 {
		return -1, -1, false
	}
	if l.sparse {
		if index >= l.cols*l.rows {
			return -1, -1, false
		}
		if l.hasDirect && index == 0 {
			return 0, 0, true
		}
		return gridCoordinate(index%l.cols, l.cols, l.width, l.hasDirect), gridCoordinate(index/l.cols, l.rows, l.height, l.hasDirect), true
	}

	if !l.hasDirect {
		if index >= l.cols*l.rows {
			return -1, -1, false
		}
		return index / l.rows, (index % l.rows) / 2, index%2 == 0
	}

	// Dense layout with direct slot
	if index == 0 {
		return 0, 0, true
	}
	col0Proxies := 2 * (l.height - 1)
	if index-1 < col0Proxies {
		return 0, 1 + (index-1)/2, (index-1)%2 == 0
	}
	rem := (index - 1) - col0Proxies
	if rem < 0 || l.rows == 0 {
		return -1, -1, false
	}
	x = 1 + rem/l.rows
	colRem := rem % l.rows
	y = colRem / 2
	upper = colRem%2 == 0
	if x >= l.width || y >= l.height {
		return -1, -1, false
	}
	return x, y, upper
}

func (m *topModel) directCellStar(c proxyCell, now time.Time) (rune, tui.Style) {
	glyph := '★'
	if m.theme.ASCII {
		glyph = '*'
	}
	st := m.proxyCellStyle(c.state, m.proxyGlowBucket(c, now))
	if m.theme.ASCII {
		st.Attrs |= tui.AttrBold
	}
	return glyph, st
}

func (m *topModel) drawLegendDirect(b *tui.Buffer, x, y int, c proxyCell) int {
	star := "★"
	if m.theme.ASCII {
		star = "*"
	}
	starStyle := m.proxyCellStyle(c.state, 4)
	if m.theme.ASCII {
		starStyle.Attrs |= tui.AttrBold
	}
	dirText := star + " direct " + proxyCellStateName(c.state)
	wDir := tui.StringWidth(dirText)
	m.record("legend:direct", b.Absolute(tui.Rect{X: x, Y: y, W: wDir, H: 1}))
	b.Put(star, x, y, starStyle)
	x += tui.StringWidth(star)
	return b.Put(" direct "+proxyCellStateName(c.state), x, y, m.theme.Dim)
}

// drawProxyGrid draws the proxy heatmap into b: a grid of colored cells plus a
// legend row of state counts. It reads only the model, so a frame is a pure
// function of (model, size).
func (m *topModel) drawProxyGrid(b *tui.Buffer) {
	th := m.theme
	cells := m.proxyCells
	if len(cells) == 0 {
		b.Put("no proxies", 0, 0, th.Dim)
		return
	}
	w, h := b.Width(), b.Height()
	gridH := h - 1 // one row for the legend
	if gridH < 1 {
		gridH = 1
	}
	// record the drawn grid size for hit testing
	m.proxyGridW, m.proxyGridH = w, gridH
	hasDirect := false
	var directCell proxyCell
	for _, c := range cells {
		if c.index == 0 {
			hasDirect = true
			directCell = c
			break
		}
	}
	layout := newProxyGridLayout(len(cells), w, gridH, hasDirect)
	capacity := layout.proxyCapacity()
	group := 1
	if hasDirect {
		numProxies := len(cells) - 1
		if capacity > 0 && numProxies > capacity {
			group = (numProxies + capacity - 1) / capacity
		}
	} else if capacity > 0 && len(cells) > capacity {
		group = (len(cells) + capacity - 1) / capacity
	}
	if group < 1 {
		group = 1
	}

	ascii := th.ASCII
	now := m.now()
	for y := 0; y < gridH; y++ {
		for x := 0; x < w; x++ {
			if hasDirect && x == 0 && y == 0 {
				glyph, st := m.directCellStar(directCell, now)
				b.Set(0, 0, tui.Cell{Rune: glyph, Style: st})
				continue
			}
			if layout.sparse {
				idx, _ := layout.indices(x, y)
				if idx < 0 || idx >= len(cells) {
					continue
				}
				c := cells[idx]
				if c.index == 0 {
					glyph, st := m.directCellStar(c, now)
					b.Set(x, y, tui.Cell{Rune: glyph, Style: st})
					continue
				}
				bucket := m.proxyGlowBucket(c, now)
				st := m.proxyCellStyle(c.state, bucket)
				if ascii && (th.Name != "mono" || layout.sparse) {
					glyph := proxyCellASCII(c.state)
					if c.state == connect.ProxyCellUp {
						glyph = []rune{'.', ':', '+', '*'}[min(int(bucket)/2, 3)]
					}
					b.Set(x, y, tui.Cell{Rune: glyph, Style: st})
					continue
				}
				b.Set(x, y, tui.Cell{Rune: '∙', Style: st})
				continue
			}

			upperIdx, lowerIdx := layout.indices(x, y)
			var worst uint8 = connect.ProxyCellUp
			var worstBucket uint8
			upperState, lowerState := uint8(connect.ProxyCellUp), uint8(connect.ProxyCellUp)
			var upperBucket, lowerBucket uint8
			hasUpper, hasLower := false, false
			for k := 0; k < group; k++ {
				if ui := upperIdx + k*capacity; upperIdx >= 0 && ui < len(cells) {
					hasUpper = true
					if proxyCellSeverity(cells[ui].state) > proxyCellSeverity(upperState) {
						upperState = cells[ui].state
					}
					upperBucket = max(upperBucket, m.proxyGlowBucket(cells[ui], now))
					if proxyCellSeverity(cells[ui].state) > proxyCellSeverity(worst) {
						worst = cells[ui].state
					}
					if bucket := m.proxyGlowBucket(cells[ui], now); bucket > worstBucket {
						worstBucket = bucket
					}
				}
				if li := lowerIdx + k*capacity; lowerIdx >= 0 && li < len(cells) {
					hasLower = true
					if proxyCellSeverity(cells[li].state) > proxyCellSeverity(lowerState) {
						lowerState = cells[li].state
					}
					lowerBucket = max(lowerBucket, m.proxyGlowBucket(cells[li], now))
					if proxyCellSeverity(cells[li].state) > proxyCellSeverity(worst) {
						worst = cells[li].state
					}
					if bucket := m.proxyGlowBucket(cells[li], now); bucket > worstBucket {
						worstBucket = bucket
					}
				}
			}
			if !hasUpper && !hasLower {
				continue
			}
			st := m.proxyCellStyle(worst, worstBucket)
			if ascii && (th.Name != "mono" || layout.sparse) {
				glyph := proxyCellASCII(worst)
				if worst == connect.ProxyCellUp {
					glyph = []rune{'.', ':', '+', '*'}[min(int(worstBucket)/2, 3)]
				}
				b.Set(x, y, tui.Cell{Rune: glyph, Style: st})
				continue
			}
			upperStyle := m.proxyCellStyle(upperState, upperBucket)
			lowerStyle := m.proxyCellStyle(lowerState, lowerBucket)
			glyph := '▀'
			if !hasUpper {
				glyph = '▄'
				upperStyle = lowerStyle
			}
			if hasUpper && hasLower {
				if th.Name == "colorblind" || th.Name == "mono" || upperStyle.FG.Kind == tui.ColorDefault || lowerStyle.FG.Kind == tui.ColorDefault {
					glyph = '█'
					upperStyle = st
				} else {
					upperStyle.BG = lowerStyle.FG
				}
			}
			b.Set(x, y, tui.Cell{Rune: glyph, Style: upperStyle})
		}
	}

	// Legend row: counts per state, zero states dimmed.
	counts := [5]int{}
	for _, c := range cells {
		if int(c.state) < len(counts) {
			if c.index != 0 {
				counts[c.state]++
			}
		}
	}
	names := []string{"up", "dropped", "connecting", "dead", "parked"}
	legend := fmt.Sprintf("%d up  %d dropped  %d connecting  %d dead  %d parked", counts[0], counts[1], counts[2], counts[3], counts[4])
	totalWidth := tui.StringWidth(legend)
	if hasDirect {
		star := "★"
		if th.ASCII {
			star = "*"
		}
		totalWidth += tui.StringWidth(star+" direct "+proxyCellStateName(directCell.state)) + 2
	}
	if totalWidth > w {
		names = []string{"up", "drop", "conn", "dead", "park"}
	}
	x := 0
	for _, c := range cells {
		if c.index == 0 {
			x = m.drawLegendDirect(b, x, gridH, c)
			break
		}
	}
	for i, name := range names {
		st := m.proxyCellStyle(uint8(i), 4)
		if counts[i] == 0 {
			st = th.Dim
		}
		if x > 0 {
			x = b.Put("  ", x, gridH, tui.Style{})
		}
		label := fmt.Sprintf("%d %s", counts[i], name)
		wLabel := tui.StringWidth(label)
		m.record("legend:"+proxyCellStateName(uint8(i)), b.Absolute(tui.Rect{X: x, Y: gridH, W: wLabel, H: 1}))
		x = b.Put(label, x, gridH, st)
	}
}

// proxyCellStyle picks the style for a grid cell: state color, with up cells
// brightened by their throughput bucket.
func (m *topModel) proxyCellStyle(state, bucket uint8) tui.Style {
	th := m.theme
	// The default colored theme gives every state a hue of its own, and the
	// throughput ramp of an up proxy stays in green to aqua, so no state is
	// ever mistaken for a bright up dot: dropped is orange, connecting is
	// violet, dead is bright red, parked is dim. Mono, ASCII and colorblind
	// themes keep their own marks below.
	colored := th.OK.FG.Kind != tui.ColorDefault && th.Name != "colorblind" && !th.ASCII
	switch state {
	case connect.ProxyCellDead:
		if colored {
			return tui.Style{FG: tui.Xterm(196), Attrs: tui.AttrBold}
		}
		return th.Bad
	case connect.ProxyCellDegraded:
		if colored {
			return tui.Style{FG: tui.Xterm(208)}
		}
		return th.Warn
	case connect.ProxyCellConnecting:
		if th.ASCII {
			return tui.Style{}
		}
		if colored {
			return tui.Style{FG: tui.Xterm(141)}
		}
		return tui.Style{FG: tui.Ansi(4)} // blue
	case connect.ProxyCellParked:
		return th.Dim
	}
	// up: brightness by throughput bucket.
	if th.OK.FG.Kind == tui.ColorDefault {
		return th.OK // mono theme carries no color
	}
	if th.Name == "colorblind" {
		st := th.OK
		if bucket >= 4 {
			st.Attrs |= tui.AttrBold
		} else if bucket == 0 {
			st.Attrs |= tui.AttrDim
		}
		return st
	}
	ramp := []tui.Color{
		tui.Xterm(22),
		tui.Xterm(28), tui.Xterm(34), tui.Xterm(40), tui.Xterm(46),
		tui.Xterm(48), tui.Xterm(49), tui.Xterm(121),
	}
	if int(bucket) >= len(ramp) {
		bucket = uint8(len(ramp) - 1)
	}
	return tui.Style{FG: ramp[bucket]}
}

// proxyCellASCII is the ASCII-theme glyph for a state.
func proxyCellASCII(state uint8) rune {
	switch state {
	case connect.ProxyCellDegraded:
		return 'o'
	case connect.ProxyCellConnecting:
		return '-'
	case connect.ProxyCellDead:
		return 'x'
	case connect.ProxyCellParked:
		return ' '
	}
	return '.'
}

// ProxyCellDetail is the full per-proxy record from the provider's
// "proxy_cell_detail" command, used for the hover/click popup.
type ProxyCellTraffic struct {
	Rx uint64 `json:"rx"`
	Tx uint64 `json:"tx"`
}

type ProxyCellFamilyStats struct {
	ConnectsOK      uint64 `json:"connects_ok"`
	ConnectsFailed  uint64 `json:"connects_failed"`
	RxBytes         uint64 `json:"rx_bytes"`
	TxBytes         uint64 `json:"tx_bytes"`
	LastSuccessUnix int64  `json:"last_success_unix,omitempty"`
}

type ProxyCellDetail struct {
	LifetimeBillable *ProxyCellTraffic     `json:"lifetime_billable,omitempty"`
	LifetimeTotal    *ProxyCellTraffic     `json:"lifetime_total,omitempty"`
	Index            int                   `json:"index"`
	Addr             string                `json:"addr"`
	State            uint8                 `json:"state"`
	Clients          int64                 `json:"clients"`
	IntervalBytes    uint64                `json:"interval_bytes"`
	AgeSeconds       int64                 `json:"age_seconds"`
	UDP              string                `json:"udp,omitempty"`
	IPv4             *ProxyCellFamilyStats `json:"ipv4,omitempty"`
	IPv6             *ProxyCellFamilyStats `json:"ipv6,omitempty"`
}

// fetchProxyCellDetail asks the provider for the full per-proxy records. It is
// called only when the popup opens, never on the grid's poll cadence.
func fetchProxyCellDetail(p Provider) (map[int]ProxyCellDetail, error) {
	if p.StateDir == "" {
		return nil, errSnapshotUnavailable
	}
	sockPath := filepath.Join(p.StateDir, "provider.sock")
	resp, err := sendSocketRequestTimeout(sockPath, controlRequest{Cmd: "proxy_cell_detail"}, topLightTimeout)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errSnapshotUnavailable, err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("%w: %s", errSnapshotUnavailable, resp.Error)
	}
	out := make(map[int]ProxyCellDetail, len(resp.ProxyCellDetail))
	for _, d := range resp.ProxyCellDetail {
		out[d.Index] = d
	}
	return out, nil
}

// applyProxyCellDetail folds a fetched detail record set into the model.
func (m *topModel) applyProxyCellDetail(gen int, detail map[int]ProxyCellDetail, err error) {
	if gen != m.gen {
		return
	}
	if err != nil || detail == nil {
		return
	}
	m.proxyDetail = detail
}

// proxyCellAt maps a buffer position (text cell) to the proxy index that owns
// it. Hit testing is by text cell: a cell holding two dot-rows returns the
// upper proxy's index (the first of its group when grouped). ok is false when
// the position holds no proxy. Pure: reads only the model.
func (m *topModel) proxyCellFirstAt(x, y int) (int, bool) {
	cells := m.proxyCells
	if len(cells) == 0 || m.proxyGridW <= 0 || m.proxyGridH <= 0 {
		return 0, false
	}
	if x < 0 || x >= m.proxyGridW || y < 0 || y >= m.proxyGridH {
		return 0, false
	}
	hasDirect := false
	for _, c := range cells {
		if c.index == 0 {
			hasDirect = true
			break
		}
	}
	layout := newProxyGridLayout(len(cells), m.proxyGridW, m.proxyGridH, hasDirect)
	upperIdx, _ := layout.indices(x, y)
	if upperIdx < 0 || upperIdx >= len(cells) {
		return 0, false
	}
	return cells[upperIdx].index, true
}

// proxyCellInfo builds the detail lines for one proxy index: address, state,
// clients, traffic and attachment duration. Reads the model's detail map; a proxy
// the detail fetch has not seen reads "?" for the fields only detail carries.
// Pure: reads only the model.
func (m *topModel) proxyCellInfo(index int) []string {
	var c proxyCell
	found := false
	for _, cell := range m.proxyCells {
		if cell.index == index {
			c, found = cell, true
			break
		}
	}
	if !found {
		return nil
	}
	st := proxyCellStateName(c.state)
	addr, intervalBytes, age, udp := "?", "?", "?", "?"
	billable, total := "?", "?"
	clients := int64(c.clients)
	if d, ok := m.proxyDetail[c.index]; ok {
		addr = proxyDisplayAddress(d.Addr)
		intervalBytes = fmt.Sprint(d.IntervalBytes)
		clients = d.Clients
		if d.Clients == 0 {
			age = "idle"
		} else {
			age = tui.Duration(time.Duration(d.AgeSeconds) * time.Second)
		}
		udp = proxyUDPDisplay(d.UDP)
		if d.LifetimeBillable != nil {
			billable = fmt.Sprintf("in %s out %s", tui.Bytes(float64(d.LifetimeBillable.Rx)), tui.Bytes(float64(d.LifetimeBillable.Tx)))
		}
		if d.LifetimeTotal != nil {
			total = fmt.Sprintf("in %s out %s", tui.Bytes(float64(d.LifetimeTotal.Rx)), tui.Bytes(float64(d.LifetimeTotal.Tx)))
		}
	}
	if index == 0 {
		addr = "direct (this host)"
	}
	lines := []string{
		fmt.Sprintf("proxy[%d] %s", c.index, addr),
		fmt.Sprintf("state %s", st),
		fmt.Sprintf("udp %s", udp),
		fmt.Sprintf("clients %d", clients),
		fmt.Sprintf("traffic, last second %s", intervalBytes),
		fmt.Sprintf("lifetime billable %s", billable),
		fmt.Sprintf("lifetime total %s", total),
		fmt.Sprintf("clients attached for %s", age),
	}
	if index == 0 {
		now := time.Now()
		if m != nil && m.now != nil {
			now = m.now()
		}
		var d ProxyCellDetail
		if m != nil && m.proxyDetail != nil {
			d = m.proxyDetail[0]
		}
		lines = append(lines,
			formatProxyFamilyLine("ipv4", d.IPv4, now),
			formatProxyFamilyLine("ipv6", d.IPv6, now),
		)
	}
	return lines
}

func formatProxyFamilyLine(name string, f *ProxyCellFamilyStats, now time.Time) string {
	if f == nil {
		return fmt.Sprintf("%s ok 0 fail 0  in 0 B out 0 B  last never", name)
	}
	last := "never"
	if f.LastSuccessUnix > 0 {
		lastTime := time.Unix(f.LastSuccessUnix, 0)
		if !now.Before(lastTime) {
			last = tui.Duration(now.Sub(lastTime)) + " ago"
		} else {
			last = "0s ago"
		}
	}
	inStr := tui.Bytes(float64(f.RxBytes))
	outStr := tui.Bytes(float64(f.TxBytes))
	return fmt.Sprintf("%s ok %d fail %d  in %s out %s  last %s", name, f.ConnectsOK, f.ConnectsFailed, inStr, outStr, last)
}

// proxyUDPDisplay names a provider UDP check state the way the UDP row does.
// An empty value is a provider that predates the field.
func proxyUDPDisplay(state string) string {
	switch state {
	case "ok":
		return "ok"
	case "no_relay":
		return "no UDP"
	case "mismatch":
		return "mismatch"
	case "unknown":
		return "unknown"
	case "unchecked":
		return "unchecked"
	case "direct":
		return "native (no relay)"
	}
	return "?"
}

// proxyCellIndices returns all proxies represented by a text cell.
func (m *topModel) proxyCellAt(x, y int) []int {
	if m.proxyGridW <= 0 || m.proxyGridH <= 0 || len(m.proxyCells) == 0 {
		return nil
	}
	hasDirect := false
	for _, c := range m.proxyCells {
		if c.index == 0 {
			hasDirect = true
			break
		}
	}
	gridX := x
	gridY := y
	if m.panel == "proxies" {
		if m.proxyWinCellWidth == 2 {
			gridX = x / 2
		}
		gridY = y + m.proxyWinScroll
	}
	if hasDirect && gridX == 0 && gridY == 0 {
		return []int{0}
	}
	l := newProxyGridLayout(len(m.proxyCells), m.proxyGridW, m.proxyGridH, hasDirect)
	upper, lower := l.indices(gridX, gridY)
	capacity := l.proxyCapacity()
	if capacity == 0 {
		return nil
	}
	var positions []int
	for _, pos := range []int{upper, lower} {
		if pos < 0 {
			continue
		}
		for i := pos; i < len(m.proxyCells); i += capacity {
			positions = append(positions, i)
		}
	}
	// In grouped cells the most severe entries are shown first.
	if len(positions) > 2 {
		sort.SliceStable(positions, func(i, j int) bool {
			return proxyCellSeverity(m.proxyCells[positions[i]].state) > proxyCellSeverity(m.proxyCells[positions[j]].state)
		})
	}
	out := make([]int, 0, len(positions))
	for _, i := range positions {
		out = append(out, m.proxyCells[i].index)
	}
	return out
}

func (m *topModel) proxyPopupAt(x, y int) bool {
	r := m.proxyPopupRect
	indices := m.proxyCellAt(x-r.X, y-r.Y)
	if len(indices) == 0 {
		m.proxyPopup = false
		return false
	}
	if !m.proxyPopup || x != m.proxyPopupX || y != m.proxyPopupY {
		m.proxyPopupFetch = true
	}
	m.proxyPopup, m.proxyPopupX, m.proxyPopupY, m.proxyPopupIndices = true, x, y, indices
	return true
}

func (m *topModel) drawProxyPopup(b *tui.Buffer) {
	r := m.proxyPopupRect.Intersect(b.Rect())
	if r.W < 3 || r.H < 3 {
		return
	}
	lines := []string{}
	if len(m.proxyPopupIndices) > 2 {
		lines = append(lines, fmt.Sprintf("%d proxies. Worst first", len(m.proxyPopupIndices)))
	}
	for _, index := range m.proxyPopupIndices {
		info := m.proxyCellInfo(index)
		if len(info) < 2 {
			continue
		}
		lines = append(lines, info[0]+" "+info[1])
		if len(m.proxyPopupIndices) == 1 {
			if len(info) >= 8 {
				lines = append(lines, info[4:7]...)
				lines = append(lines, info[2]+"  "+info[3]+"  "+info[7])
			} else {
				lines = append(lines, info[2:]...)
			}
			if len(info) > 8 {
				lines = append(lines, info[8:]...)
			}
		} else if len(info) > 2 {
			lines[len(lines)-1] += "  " + info[2]
		}
		if len(m.proxyPopupIndices) > 1 && len(lines) >= 5 {
			break
		}
	}
	if len(lines) == 0 {
		return
	}
	w := 0
	for _, line := range lines {
		w = max(w, tui.StringWidth(line))
	}
	w, h := min(r.W, w+2), min(r.H, len(lines)+2)
	x := min(max(m.proxyPopupX+1, r.X), r.X+r.W-w)
	y := min(max(m.proxyPopupY+1, r.Y), r.Y+r.H-h)
	panel := b.Sub(tui.Rect{X: x, Y: y, W: w, H: h})
	panel.Fill(panel.Rect(), ' ', tui.Style{})
	in := tui.DrawBox(panel, "Proxy", m.theme.Frame, m.theme.Border, m.theme.Accent, m.theme.ASCII)
	for i, line := range lines {
		in.Put(tui.Truncate(line, in.Width(), m.theme.ASCII), 0, i, tui.Style{})
	}
}

// proxyDisplayAddress also protects the screen when an older provider sends a URL.
func proxyDisplayAddress(raw string) string {
	if !strings.Contains(raw, "://") {
		raw = "//" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || !connect.ProxyHostAllowed(u.Hostname()) || !connect.ProxyPortAllowed(u.Port()) {
		return "?"
	}
	return net.JoinHostPort(u.Hostname(), u.Port())
}
