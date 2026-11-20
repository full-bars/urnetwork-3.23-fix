package urnettools

import "github.com/urnetwork/connect/internal/tui"

// drawProxyHeatmap selects the heatmap renderer for the Proxies panel.
func (m *topModel) drawProxyHeatmap(b *tui.Buffer) {
	switch m.proxyHeatmapStyle {
	case heatmapAurora:
		m.drawProxyAurora(b)
		return
	case heatmapSonar:
		m.drawProxySonar(b)
		return
	case heatmapLocality:
		m.drawProxyLocality(b)
		return
	case heatmapBlocks:
		m.drawProxyBlocks(b)
		return
	case heatmapTimeline:
		m.drawProxyTimeline(b)
		return
	}
	m.drawProxyGrid(b)
}
