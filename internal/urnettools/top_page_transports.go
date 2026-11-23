package urnettools

import (
	"fmt"
	"math/bits"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// Page for `urnet-tools top`: the share of the window's traffic each transport
// carried, as one segmented bar, a legend with whole percents, a footer of the
// enabled transports that carried nothing, and the live connection counts of
// the H1 carriers.
//
// The shares come from two cumulative readings of the provider's transport
// block: the newest one and the one a window earlier. H1 and H1+ are separate
// carriers on the wire and stay separate segments here, which is why h1_plus
// is its own kind and not folded into h1_websocket.

// transportPalette is the colour set of the bar and the legend markers. It is
// one small struct on purpose: a theme can hand back different styles without
// the drawing code below changing. The series colours are the app's: green for
// H3, light blue for H1, the URnetwork blue for the separate H1+ carrier, pink
// for WhoDis and yellow for its pump.
type transportPalette struct {
	h3, h1, h1Plus, dns, dnsPump tui.Style
}

// transportsPalette picks the styles for this theme. ASCII and mono draw the
// bar in plain cells, and the colourblind theme uses the theme's own roles so
// the segments stay tellable apart.
func transportsPalette(th tui.Theme) transportPalette {
	if th.ASCII {
		return transportPalette{}
	}
	if th.Name == "colorblind" {
		return transportPalette{h3: th.OK, h1: th.Accent, h1Plus: th.Warn, dns: th.Bad, dnsPump: th.Dim}
	}
	return transportPalette{
		h3:      tui.Style{FG: tui.Xterm(120)},
		h1:      tui.Style{FG: tui.Xterm(117)},
		h1Plus:  tui.Style{FG: tui.RGB(0x00, 0x99, 0xFF)},
		dns:     tui.Style{FG: tui.Xterm(213)},
		dnsPump: tui.Style{FG: tui.Xterm(221)},
	}
}

// style is the segment and marker colour of one kind.
func (p transportPalette) style(kind string) tui.Style {
	switch kind {
	case "h3":
		return p.h3
	case "h1_websocket":
		return p.h1
	case "h1_plus":
		return p.h1Plus
	case "dns":
		return p.dns
	case "dns_pump":
		return p.dnsPump
	}
	return tui.Style{}
}

// transportLabel is the short name of a kind, the one the app shows. H1 and
// H1+ are named apart because they are two carriers, not one.
func transportLabel(kind string) string {
	switch kind {
	case "h3":
		return "H3"
	case "h1_websocket":
		return "H1"
	case "h1_plus":
		return "H1+"
	case "dns":
		return "whodis"
	case "dns_pump":
		return "whodis pump"
	}
	return kind
}

// transportKindEnabled reports whether the provider runs the kind at all. A
// kind it does not run is left out of the legend and the table; a kind it runs
// but that carried nothing in the window is listed as unused.
func transportKindEnabled(kind string, live *LiveTransports) bool {
	if live == nil {
		return false
	}
	switch kind {
	case "h3":
		return live.H3 != nil
	case "h1_websocket":
		return live.H1WebSocket != nil
	case "h1_plus":
		return live.H1Plus != nil
	case "dns":
		return live.DNS != nil
	case "dns_pump":
		return live.DNSPump != nil
	}
	return false
}

// transportsData is what the page draws: the window shares, the live block for
// the gauges, the span the shares cover, and whether there are two readings to
// difference yet.
func (m *topModel) transportsData() (shares []TransportWindowShare, live *LiveTransports, span time.Duration, ready, ok bool) {
	newest, ok := m.transportsHistory.newest()
	if !ok {
		return nil, nil, 0, false, false
	}
	live = newest.transports
	prev, cur, ok := m.transportsHistory.window()
	if !ok {
		return nil, live, 0, false, true
	}
	shares = transportWindowShares(prev.transports, cur.transports)
	return shares, live, time.Duration(cur.at - prev.at), true, true
}

// transportsPageAvailable offers the page once a traffic reading has carried
// the transport block. A provider that predates it leaves LiveTraffic.Transports
// nil, so no reading is kept and the page stays out of the strip and off the
// number keys.
func (m *topModel) transportsPageAvailable(tui.Rect) bool {
	_, ok := m.transportsHistory.newest()
	return ok
}

// drawTransportsPage paints the Transports page inside the content rectangle.
func (m *topModel) drawTransportsPage(b *tui.Buffer, content tui.Rect) {
	th := m.theme
	box := tui.DrawBox(b.Sub(content), "Transports", th.Frame, th.Border, th.Accent, th.ASCII)
	m.record("box:transports", content)

	shares, live, span, ready, ok := m.transportsData()
	if !ok {
		msg := "waiting for the provider"
		if m.conn == topDisconnected {
			msg = "no data: " + m.lastErr
		}
		box.Put(tui.Truncate(msg, box.Width(), th.ASCII), 0, 0, th.Dim)
		return
	}
	if !ready {
		box.Put(tui.Truncate("collecting the first window", box.Width(), th.ASCII), 0, 0, th.Dim)
		return
	}

	pal := transportsPalette(th)
	width := box.Width()

	// The window caption, then the bar on a row of its own: the bar takes the
	// full content width, so its parts are the shares in cells and nothing
	// else competes for the row.
	box.Put(tui.Truncate("last "+shortSpan(span), width, th.ASCII), 0, 0, th.Dim)
	if box.Height() > 1 {
		drawTransportBar(box, 1, shares, pal, th.ASCII)
	}
	y := 3

	// Legend of the kinds that carried traffic, then the unused footer.
	y = drawTransportLegend(box, y, shares, live, pal, th)
	if y < box.Height() {
		y++
	}
	y = drawTransportUnused(box, y, shares, live, th)

	// Table of the same window, one row per enabled kind, below the legend.
	if y < box.Height() {
		y++
	}
	drawTransportTable(box, y, shares, live, span, th)
}

// shortSpan is a span in the compact form the caption uses: seconds up to two
// minutes, whole minutes above that, so a short first window is not called a
// minute of traffic.
func shortSpan(span time.Duration) string {
	if span <= 0 {
		span = topTransportWindow
	}
	seconds := int(span / time.Second)
	if seconds < 120 {
		return fmt.Sprintf("%ds", seconds)
	}
	return fmt.Sprintf("%dm", seconds/60)
}

// drawTransportBar paints the segmented bar on one row: each used kind as a
// run of block cells, in the palette colour of that kind, so the runs tile the
// whole width. The first and last cells are half blocks, which rounds the ends
// without leaving a gap. ASCII draws the runs in '#' and an idle window in '-'.
func drawTransportBar(b *tui.Buffer, y int, shares []TransportWindowShare, pal transportPalette, ascii bool) {
	width := b.Width()
	if width <= 0 || y < 0 || y >= b.Height() {
		return
	}
	full, left, right, empty := '█', '▌', '▐', '░'
	if ascii {
		full, left, right, empty = '#', '#', '#', '-'
	}
	cells := transportBarCells(shares, width)
	tiled := 0
	for _, n := range cells {
		tiled += n
	}
	if tiled == 0 {
		// An idle window has no segment to draw: the empty cells say so, and
		// the legend and footer name the kinds that sat out.
		b.Fill(tui.Rect{X: 0, Y: y, W: width, H: 1}, empty, tui.Style{})
		return
	}
	x := 0
	for i, share := range shares {
		n := cells[i]
		if n <= 0 {
			continue
		}
		st := pal.style(share.Kind)
		for j := 0; j < n && x < width; j++ {
			ch := full
			switch {
			case x == 0:
				ch = left
			case x == width-1:
				ch = right
			}
			b.Put(string(ch), x, y, st)
			x++
		}
	}
}

// transportBarCells splits width cells over the used kinds in proportion to the
// bytes they carried, by largest remainder, so the widths add up to exactly the
// bar width. Every used kind gets at least one cell when the bar has room,
// because a kind that carried only a fraction of a percent still ran.
func transportBarCells(shares []TransportWindowShare, width int) []int {
	used := make([]int, 0, len(shares))
	var total uint64
	for i, s := range shares {
		if s.Bytes == 0 {
			continue
		}
		used = append(used, i)
		total += s.Bytes
	}
	cells := make([]int, len(shares))
	if len(used) == 0 || total == 0 || width <= 0 {
		return cells
	}
	remainders := make([]uint64, len(used))
	assigned := 0
	for k, i := range used {
		hi, lo := bits.Mul64(shares[i].Bytes, uint64(width))
		whole, rem := bits.Div64(hi, lo, total)
		if int(whole) > width {
			whole = uint64(width)
		}
		cells[i] = int(whole)
		remainders[k] = rem
		assigned += int(whole)
	}
	// Largest remainder first; the earlier kind in transportKindOrder keeps a
	// tie, which is the order the shares arrive in.
	for ; assigned < width; assigned++ {
		best := -1
		for k := range used {
			if best == -1 || remainders[k] > remainders[best] {
				best = k
			}
		}
		if best == -1 {
			break
		}
		cells[used[best]]++
		remainders[best] = 0
	}
	// A bar at least one cell per used kind wide never shows a zero-width
	// segment: take the cell from the widest neighbour.
	if width >= len(used) {
		for _, i := range used {
			if cells[i] > 0 {
				continue
			}
			wide := used[0]
			for _, j := range used {
				if cells[j] > cells[wide] {
					wide = j
				}
			}
			if cells[wide] <= 1 {
				break
			}
			cells[wide]--
			cells[i] = 1
		}
	}
	return cells
}

// drawTransportLegend paints the legend and returns the row below it. Each
// entry is a filled marker in the kind's colour, its name and its whole
// percent, packed across the width and wrapped onto further rows while the box
// has height for them. An idle window says so in place of the entries.
func drawTransportLegend(b *tui.Buffer, y int, shares []TransportWindowShare, live *LiveTransports, pal transportPalette, th tui.Theme) int {
	width := b.Width()
	if y >= b.Height() {
		return y
	}
	type item struct {
		kind   string
		text   string
		marker string
	}
	marker := "●"
	if th.ASCII {
		marker = "*"
	}
	var items []item
	for _, s := range shares {
		if !s.Used || s.Bytes == 0 {
			continue
		}
		items = append(items, item{kind: s.Kind, text: fmt.Sprintf("%s %d%%", transportLabel(s.Kind), s.Percent), marker: marker})
	}
	if len(items) == 0 {
		b.Put(tui.Truncate("idle: no traffic in the window", width, th.ASCII), 0, y, th.Dim)
		return y + 1
	}
	x := 0
	for _, it := range items {
		entry := it.marker + " " + it.text
		w := tui.StringWidth(entry)
		if x > 0 && x+w > width {
			y++
			x = 0
			if y >= b.Height() {
				return y
			}
		}
		if x > 0 {
			x = b.Put("   ", x, y, th.Dim)
		}
		x = b.Put(it.marker, x, y, pal.style(it.kind))
		x = b.Put(" "+it.text, x, y, th.Accent)
	}
	return y + 1
}

// drawTransportUnused paints the footer of enabled kinds that carried nothing
// in the window, with a hollow marker so it cannot be read as a share. It
// returns the row below the footer. Rows that do not fit are dropped rather
// than cut in half.
func drawTransportUnused(b *tui.Buffer, y int, shares []TransportWindowShare, live *LiveTransports, th tui.Theme) int {
	if y >= b.Height() {
		return y
	}
	hollow := "○"
	if th.ASCII {
		hollow = "o"
	}
	var unused []string
	for _, s := range shares {
		if s.Used && s.Bytes > 0 {
			continue
		}
		if !transportKindEnabled(s.Kind, live) {
			continue
		}
		unused = append(unused, hollow+" "+transportLabel(s.Kind))
	}
	if len(unused) == 0 {
		return y
	}
	width := b.Width()
	x := b.Put("unused ", 0, y, th.Dim)
	first := true
	for _, entry := range unused {
		w := tui.StringWidth(entry)
		if !first {
			w += 3
		}
		if !first && x+w > width {
			y++
			x = 0
			first = true
			if y >= b.Height() {
				return y
			}
		}
		if !first {
			x = b.Put("   ", x, y, th.Dim)
		}
		x = b.Put(entry, x, y, th.Dim)
		first = false
	}
	return y + 1
}

// drawTransportTable paints the per-kind table: kind, window share, window rate
// and the live connection gauge. Below it is the H1 line, which says how many
// of the live H1 connections run on the H1+ carrier.
func drawTransportTable(b *tui.Buffer, y int, shares []TransportWindowShare, live *LiveTransports, span time.Duration, th tui.Theme) {
	if y >= b.Height() {
		return
	}
	rows := make([]tui.Row, 0, len(shares))
	for _, s := range shares {
		if !transportKindEnabled(s.Kind, live) {
			continue
		}
		share := "unused"
		if s.Used && s.Bytes > 0 {
			share = fmt.Sprintf("%d%%", s.Percent)
		}
		rows = append(rows, tui.Row{Cells: []string{
			transportLabel(s.Kind),
			share,
			transportWindowRate(s, span),
			transportConns(s.Kind, live),
		}, Style: th.Dim})
	}
	table := tui.Table{
		Columns: []tui.Column{
			{Title: "transport", Width: 12},
			{Title: "share", Width: 7, Align: tui.AlignRight},
			{Title: "rate", Width: 0, Align: tui.AlignRight},
			{Title: "conns", Width: 6, Align: tui.AlignRight},
		},
		Rows:        rows,
		Header:      true,
		HeaderStyle: th.Accent,
	}
	drawn := tui.DrawTable(b.Sub(tui.Rect{Y: y, W: b.Width(), H: b.Height() - y}), table, th.ASCII)
	if line := transportH1Line(live); line != "" && y+1+drawn < b.Height() {
		b.Put(tui.Truncate(line, b.Width(), th.ASCII), 0, y+1+drawn, th.Dim)
	}
}

// transportWindowRate is the kind's payload over the window as a rate, both
// directions counted, in the units the other pages use. A zero span has no
// rate to show.
func transportWindowRate(share TransportWindowShare, span time.Duration) string {
	if share.Bytes == 0 || span <= 0 {
		return tui.Rate(0)
	}
	return tui.Rate(float64(share.Bytes) / span.Seconds())
}

// transportConns is the kind's live connection gauge. The provider reports a
// gauge only for the H1 carriers; the others carry no connection count.
func transportConns(kind string, live *LiveTransports) string {
	if live == nil {
		return ""
	}
	var gauge *LiveTransport
	switch kind {
	case "h1_websocket":
		gauge = live.H1WebSocket
	case "h1_plus":
		gauge = live.H1Plus
	}
	if gauge == nil {
		return ""
	}
	return fmt.Sprintf("%d", gauge.Up)
}

// transportH1Line says how the live H1 connections split over the two carriers,
// because a total that mixes them hides which carrier is actually carrying
// traffic. It is empty when the provider reports neither gauge.
func transportH1Line(live *LiveTransports) string {
	if live == nil || live.H1WebSocket == nil {
		return ""
	}
	h1 := live.H1WebSocket.Up
	var plus int64
	if live.H1Plus != nil {
		plus = live.H1Plus.Up
	}
	total := h1 + plus
	if total <= 0 {
		return "H1 connections: none up"
	}
	return fmt.Sprintf("H1 connections: %d, of them %d on the H1+ carrier (%.0f%%)", total, plus, 100*float64(plus)/float64(total))
}
