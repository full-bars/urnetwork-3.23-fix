package urnettools

import (
	"os"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
)

// TestNeonRenderProof writes the dashboard and two graph pages in the neon
// theme as ANSI text, so a screenshot is drawn by the same code the screen
// runs. It only runs when URNET_NEON_PROOF names a directory, because it
// writes outside the repository.
func TestNeonRenderProof(t *testing.T) {
	dir := os.Getenv("URNET_NEON_PROOF")
	if dir == "" {
		t.Skip("set URNET_NEON_PROOF to a directory to write the neon render proof")
	}
	cases := []struct {
		name string
		page int
	}{
		{"dashboard", pageByID("overview")},
		{"remote", pageByID("remote")},
		{"billable", pageByID("billable")},
	}
	for _, c := range cases {
		clock := &fakeClock{t: topBase}
		m := newTestModel(tui.NeonTheme(), topProviders(1), clock)
		m.apply(0, directionFixture(t), nil)
		m.page = c.page
		b := tui.New(120, 40)
		renderFresh(m, b)
		path := dir + "/neon_" + c.name + ".ansi"
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(remoteANSI(b)), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dir+"/neon_"+c.name+".txt", []byte(b.String()+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", path)
	}
}
