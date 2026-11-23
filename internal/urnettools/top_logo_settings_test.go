package urnettools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
)

// A save from top writes the whole settings file. It keeps ui.logo, so an
// operator who turned the logo off does not get it back by changing the graph
// window or the theme in top.
func TestTopSaveKeepsTheLogoSetting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "top.conf")
	if err := os.WriteFile(path, []byte("ui.logo = off\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), &fakeClock{t: topBase})
	m.settingsPath = path
	m.applySettings(loadTopSettings(path))
	m.stepGraphWindow(1)
	m.saveSettings()
	if loadTopSettings(path).logoOn() {
		data, _ := os.ReadFile(path)
		t.Fatalf("a save from top dropped ui.logo = off:\n%s", data)
	}
}
