package urnettools

import "github.com/urnetwork/connect/internal/tui"

// drawProxyHeatmap selects the heatmap renderer for the Proxies panel.
func (m *topModel) drawProxyHeatmap(b *tui.Buffer) {
	m.drawProxyGrid(b)
}
