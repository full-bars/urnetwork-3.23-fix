package urnettools

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/internal/tui"
)

// The proxy grid: one colored dot per proxy, drawn into the Proxies panel.
//
// The provider answers the compact "proxy_cells" command with 4 packed bytes
// per proxy (index, state, throughput bucket, clients). This file decodes
// that payload and draws it as a heatmap: each text cell holds two dot-rows
// (a half-block), so a 4400-proxy pool fits in about 70x32 cells. Proxies
// are laid out column-major by index, so a contiguous run of dead proxies
// reads as a red column at a glance.

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

// fetchProxyCells asks the provider for the compact grid payload. A provider
// that predates the command (or has no proxies) leaves the field empty; the
// caller treats that as "no grid" and falls back to the bars.
func fetchProxyCells(p Provider) ([]proxyCell, error) {
	if p.StateDir == "" {
		return nil, errSnapshotUnavailable
	}
	sockPath := filepath.Join(p.StateDir, "provider.sock")
	resp, err := sendSocketRequestTimeout(sockPath, controlRequest{Cmd: "proxy_cells"}, topLightTimeout)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errSnapshotUnavailable, err)
	}
	if !resp.OK {
		if len(resp.Error) >= 15 && resp.Error[:15] == "unknown command" {
			return nil, errTrafficUnsupported
		}
		return nil, fmt.Errorf("%w: %s", errSnapshotUnavailable, resp.Error)
	}
	return decodeProxyCells(resp.ProxyCells), nil
}

// applyProxyCells folds a fetched grid into the model. A nil payload clears
// the grid (provider predates the command) so the panel falls back to bars.
func (m *topModel) applyProxyCells(gen int, cells []proxyCell, err error) {
	if gen != m.gen {
		return
	}
	if err != nil || cells == nil {
		m.proxyCells = nil
		return
	}
	m.proxyCells = cells
	m.proxyCellsAt = m.now()
}

// proxyGridLayout says where proxy index i lands in the grid. The grid is
// column-major: consecutive indices stack down a column, so a contiguous dead
// range draws as a red column. Each text cell holds two dot-rows (upper and
// lower half of a half-block).
type proxyGridLayout struct {
	cols    int // text columns
	dotRows int // dot-rows = 2 * text rows
}

// cellFor maps a proxy index to its text cell and which half of it.
func (l proxyGridLayout) cellFor(index int) (x, y int, upper bool) {
	if index < 0 || index >= l.cols*l.dotRows {
		return 0, 0, false
	}
	col := index / l.dotRows
	dotRow := index % l.dotRows
	return col, dotRow / 2, dotRow%2 == 0
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
	layout := proxyGridLayout{cols: w, dotRows: 2 * gridH}
	capacity := layout.cols * layout.dotRows
	// Grouping: when there are more proxies than dot-positions, several
	// proxies share one dot-position and the cell shows the worst state.
	group := (len(cells) + capacity - 1) / capacity
	if group < 1 {
		group = 1
	}

	ascii := th.ASCII
	for y := 0; y < gridH; y++ {
		for x := 0; x < w; x++ {
			upperIdx := x*layout.dotRows + 2*y
			lowerIdx := upperIdx + 1
			var worst uint8 = connect.ProxyCellUp
			var worstBucket uint8
			hasUpper, hasLower := false, false
			for k := 0; k < group; k++ {
				if ui := upperIdx + k*capacity; ui < len(cells) {
					hasUpper = true
					if proxyCellSeverity(cells[ui].state) > proxyCellSeverity(worst) {
						worst = cells[ui].state
					}
					if cells[ui].bucket > worstBucket {
						worstBucket = cells[ui].bucket
					}
				}
				if li := lowerIdx + k*capacity; li < len(cells) {
					hasLower = true
					if proxyCellSeverity(cells[li].state) > proxyCellSeverity(worst) {
						worst = cells[li].state
					}
					if cells[li].bucket > worstBucket {
						worstBucket = cells[li].bucket
					}
				}
			}
			if !hasUpper && !hasLower {
				continue
			}
			st := m.proxyCellStyle(worst, worstBucket)
			if ascii {
				b.Set(x, y, tui.Cell{Rune: proxyCellASCII(worst), Style: st})
				continue
			}
			switch {
			case hasUpper && hasLower:
				b.Set(x, y, tui.Cell{Rune: '█', Style: st})
			case hasUpper:
				b.Set(x, y, tui.Cell{Rune: '▀', Style: st})
			default:
				b.Set(x, y, tui.Cell{Rune: '▄', Style: st})
			}
		}
	}

	// Legend row: counts per state, zero states dimmed.
	counts := [5]int{}
	for _, c := range cells {
		if int(c.state) < len(counts) {
			counts[c.state]++
		}
	}
	x := 0
	for i, name := range []string{"up", "dropped", "connecting", "dead", "parked"} {
		st := th.OK
		switch i {
		case 1:
			st = th.Warn
		case 2:
			st = tui.Style{}
		case 3:
			st = th.Bad
		case 4:
			st = th.Dim
		}
		if counts[i] == 0 {
			st = th.Dim
		}
		if x > 0 {
			x = b.Put("  ", x, gridH, tui.Style{})
		}
		label := fmt.Sprintf("%d %s", counts[i], name)
		x = b.Put(label, x, gridH, st)
	}
}

// proxyCellStyle picks the style for a grid cell: state color, with up cells
// brightened by their throughput bucket.
func (m *topModel) proxyCellStyle(state, bucket uint8) tui.Style {
	th := m.theme
	switch state {
	case connect.ProxyCellDead:
		return th.Bad
	case connect.ProxyCellDegraded:
		return th.Warn
	case connect.ProxyCellConnecting:
		return tui.Style{FG: tui.Ansi(4)} // blue
	case connect.ProxyCellParked:
		return th.Dim
	}
	// up: brightness by throughput bucket.
	if th.OK.FG.Kind == tui.ColorDefault {
		return th.OK // mono theme carries no color
	}
	ramp := []tui.Color{
		th.OK.FG,
		tui.Xterm(28), tui.Xterm(34), tui.Xterm(40), tui.Xterm(46),
		tui.Xterm(83), tui.Xterm(118), tui.Xterm(154),
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
type ProxyCellDetail struct {
	Index         int    `json:"index"`
	Addr          string `json:"addr"`
	State         uint8  `json:"state"`
	Clients       int64  `json:"clients"`
	IntervalBytes uint64 `json:"interval_bytes"`
	AgeSeconds    int64  `json:"age_seconds"`
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
func (m *topModel) proxyCellAt(x, y int) (int, bool) {
	cells := m.proxyCells
	if len(cells) == 0 || m.proxyGridW <= 0 || m.proxyGridH <= 0 {
		return 0, false
	}
	if x < 0 || x >= m.proxyGridW || y < 0 || y >= m.proxyGridH {
		return 0, false
	}
	layout := proxyGridLayout{cols: m.proxyGridW, dotRows: 2 * m.proxyGridH}
	capacity := layout.cols * layout.dotRows
	group := (len(cells) + capacity - 1) / capacity
	if group < 1 {
		group = 1
	}
	upperIdx := x*layout.dotRows + 2*y
	if upperIdx >= len(cells) {
		return 0, false
	}
	return cells[upperIdx].index, true
}

// proxyCellInfo builds the detail lines for one proxy index: address, state,
// clients, bytes in the interval, age. Reads the model's detail map; a proxy
// the detail fetch has not seen reads "?" for the fields only detail carries.
// Pure: reads only the model.
func (m *topModel) proxyCellInfo(index int) []string {
	cells := m.proxyCells
	if index < 0 || index >= len(cells) {
		return nil
	}
	c := cells[index]
	st := proxyCellStateName(c.state)
	addr, intervalBytes, age := "?", uint64(0), "?"
	clients := int64(c.clients)
	if d, ok := m.proxyDetail[c.index]; ok {
		addr = d.Addr
		intervalBytes = d.IntervalBytes
		clients = d.Clients
		age = tui.Duration(time.Duration(d.AgeSeconds) * time.Second)
	}
	return []string{
		fmt.Sprintf("proxy[%d] %s", c.index, addr),
		fmt.Sprintf("state %s", st),
		fmt.Sprintf("clients %d", clients),
		fmt.Sprintf("bytes in interval %d", intervalBytes),
		fmt.Sprintf("age %s", age),
	}
}
