//go:build linux

package urnettools

import "testing"

// A check that started before a fix must not bring the banner back. The poll
// path decides whether lingering applies from the platform, and it applies on
// Linux only, so this test runs there.
func TestLingerStaleCheckIsDropped(t *testing.T) {
	m, _ := lingerModel(t, false)
	calls := &lingerCalls{}
	lingerSeams(t, calls.run, false, true)
	lingerChecks(t, map[string]bool{"alice": true})
	m.enableLingering(&fakeLingerTerminal{})
	// The fix must go through the command seam, never the host's loginctl.
	if len(calls.lines) != 1 || calls.lines[0] != "loginctl enable-linger alice [plain]" {
		t.Fatalf("commands = %v, want the seeded loginctl", calls.lines)
	}
	// A check that started before the fix carries the pre-fix revision and
	// must not put the banner back.
	m.apply(0, topFlowing(t), nil)
	if m.lingerBannerActive() {
		t.Fatal("the banner came back after a successful fix")
	}
	if m.lingerRev == 0 {
		t.Fatal("the fix did not bump the reading revision")
	}
}
