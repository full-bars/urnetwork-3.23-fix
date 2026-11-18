package urnettools

import "github.com/urnetwork/connect/internal/tui"

// drawDetails shows the selected diagnostics without changing the panel layout.
func (m *topModel) drawDetails(b *tui.Buffer) {
	w, h := min(b.Width(), 76), min(b.Height(), 18)
	if w < 4 || h < 4 {
		return
	}
	r := tui.Rect{X: (b.Width() - w) / 2, Y: (b.Height() - h) / 2, W: w, H: h}
	title := map[string]string{"alerts": "Alerts", "gauges": "Resource gauges", "carriers": "Extender carriers"}[m.panel]
	in := tui.DrawBox(b.Sub(r), title, m.theme.Frame, m.theme.Border, m.theme.Accent, m.theme.ASCII)
	in.Fill(in.Rect(), ' ', tui.Style{})
	if in.Height() < 2 {
		return
	}
	content := in.Sub(tui.Rect{W: in.Width(), H: in.Height() - 1})
	switch m.panel {
	case "alerts":
		if m.alertRows() == 0 {
			content.Put("No active alerts", 0, 0, m.theme.Dim)
		} else {
			m.drawAlertBanner(content)
		}
	case "gauges":
		m.drawGauges(content)
	case "carriers":
		m.drawCarriers(content)
	}
	in.Put("Esc closes", 0, in.Height()-1, m.theme.Dim)
}
