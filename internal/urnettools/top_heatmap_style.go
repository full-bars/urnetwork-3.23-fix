package urnettools

import "strings"

type heatmapStyle uint8

const (
	heatmapGlow heatmapStyle = iota
	heatmapAurora
	heatmapSonar
	heatmapLocality
	heatmapBlocks
	heatmapTimeline
)

var heatmapStyles = [...]heatmapStyle{
	heatmapGlow, heatmapAurora, heatmapSonar, heatmapLocality, heatmapBlocks, heatmapTimeline,
}

var heatmapStyleNames = [...]string{"Glow", "Aurora", "Sonar", "Locality", "Blocks", "Timeline"}

func (s heatmapStyle) String() string {
	if int(s) >= len(heatmapStyleNames) {
		return heatmapStyleNames[heatmapGlow]
	}
	return heatmapStyleNames[s]
}

func parseHeatmapStyle(name string) heatmapStyle {
	for _, style := range heatmapStyles {
		if strings.EqualFold(strings.TrimSpace(name), style.String()) {
			return style
		}
	}
	return heatmapGlow
}
