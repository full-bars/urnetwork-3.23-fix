package urnettools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHeatmapStyleSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "top.conf")
	for _, style := range heatmapStyles {
		if err := saveTopSettings(path, topSettings{ProxyHeatmapStyle: style}); err != nil {
			t.Fatal(err)
		}
		loaded := loadTopSettings(path)
		if loaded.ProxyHeatmapStyle != style {
			t.Fatalf("got %v, want %v", loaded.ProxyHeatmapStyle, style)
		}
		var model topModel
		model.applySettings(loaded)
		if got := model.settings().ProxyHeatmapStyle; got != style {
			t.Fatalf("model got %v, want %v", got, style)
		}
	}
	if err := os.WriteFile(path, []byte("proxy.heatmap_style = invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadTopSettings(path).ProxyHeatmapStyle; got != heatmapGlow {
		t.Fatalf("unknown style = %v, want Glow", got)
	}
}
