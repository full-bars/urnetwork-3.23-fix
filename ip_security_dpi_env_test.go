package connect

import (
	"os"
	"testing"
)

// The detector tests exercise the privileged-port signature check, which
// production leaves off by default. Opt in for the whole test binary; tests that
// check the production default clear the variable themselves.
func init() {
	os.Setenv("URNETWORK_DPI_PRIVILEGED_BT", "1")
}

func TestDpiEnvOptIn(t *testing.T) {
	cases := []struct {
		admits, privileged     string
		wantAdmits, wantPrivBt bool
	}{
		{"", "", true, false},
		{"off", "0", false, false},
		{"on", "", true, false},
		{"OFF", "1", false, true},
		{"", "1", true, true},
		{"false", "true", true, false},
	}
	t.Cleanup(resetDpiAdmitsEnvForTest)
	for _, c := range cases {
		t.Setenv("URNETWORK_DPI_ADMITS", c.admits)
		t.Setenv("URNETWORK_DPI_PRIVILEGED_BT", c.privileged)
		resetDpiAdmitsEnvForTest()
		if got := DpiAdmitsEnabled(); got != c.wantAdmits {
			t.Fatalf("ADMITS=%q: got %t, want %t", c.admits, got, c.wantAdmits)
		}
		if got := DpiPrivilegedBtEnabled(); got != c.wantPrivBt {
			t.Fatalf("PRIVILEGED_BT=%q: got %t, want %t", c.privileged, got, c.wantPrivBt)
		}
	}
}

// The production default with nothing set: admits on, privileged check off.
func TestDpiProductionDefaults(t *testing.T) {
	t.Setenv("URNETWORK_DPI_ADMITS", "")
	t.Setenv("URNETWORK_DPI_PRIVILEGED_BT", "")
	resetDpiAdmitsEnvForTest()
	t.Cleanup(resetDpiAdmitsEnvForTest)
	if !DpiAdmitsEnabled() {
		t.Fatal("application-standard admits must default on")
	}
	if DpiPrivilegedBtEnabled() {
		t.Fatal("privileged-port BitTorrent check must default off")
	}
}
