package urnettools

import "testing"

func TestHeatmapStyleNames(t *testing.T) {
	want := []string{"Glow", "Aurora", "Sonar", "Locality", "Blocks", "Timeline"}
	if len(heatmapStyles) != len(want) {
		t.Fatalf("got %d styles, want %d", len(heatmapStyles), len(want))
	}
	for i, name := range want {
		if got := heatmapStyles[i].String(); got != name {
			t.Errorf("style %d = %q, want %q", i, got, name)
		}
		if got := parseHeatmapStyle(name); got != heatmapStyles[i] {
			t.Errorf("parse %q = %v", name, got)
		}
	}
	if got := parseHeatmapStyle("unknown"); got != heatmapGlow {
		t.Errorf("unknown style = %v, want Glow", got)
	}
	if got := heatmapStyle(255).String(); got != "Glow" {
		t.Errorf("invalid style = %q, want Glow", got)
	}
}
