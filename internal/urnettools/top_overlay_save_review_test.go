package urnettools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect/internal/tui/tcellui"
)

func TestTopOverlayReleaseSavesActiveDrag(t *testing.T) {
	for _, overlay := range []string{"menu", "help", "gauges"} {
		t.Run(overlay, func(t *testing.T) {
			m, _ := mouseModel(t, 120, 40)
			m.settingsPath = filepath.Join(t.TempDir(), "settings.json")
			d := regionRect(t, m, "left:proxies")
			m.handle(mouseEv(tcellui.MousePress, tcellui.MouseLeft, d.X+5, d.Y))
			m.handle(mouseEv(tcellui.MouseDrag, tcellui.MouseLeft, d.X+5, d.Y-5))
			if m.drag == nil || len(m.layout.Dividers) == 0 {
				t.Fatal("drag did not adjust layout")
			}
			switch overlay {
			case "menu":
				m.menu = true
			case "help":
				m.help = true
			default:
				m.panel = overlay
			}
			m.handle(mouseEv(tcellui.MouseRelease, tcellui.MouseLeft, d.X+5, d.Y-5))
			saved, err := os.ReadFile(m.settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			if m.drag != nil {
				t.Fatal("drag remains active")
			}
			m.layout.Dividers = nil
			m.handle(mouseEv(tcellui.MouseRelease, tcellui.MouseLeft, d.X+5, d.Y-5))
			again, err := os.ReadFile(m.settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(saved) != string(again) {
				t.Fatal("release without drag changed settings")
			}
		})
	}
}
