package connect

import (
	"os"
	"testing"
)

// The detector tests exercise the application-standard admits and the
// privileged-port signature check, which production leaves off by default. Opt
// in for the whole test binary; tests that check the production default clear
// the variables themselves.
func init() {
	os.Setenv("URNETWORK_DPI_ADMITS", "on")
	os.Setenv("URNETWORK_DPI_PRIVILEGED_BT", "1")
}

func TestDpiEnvOptIn(t *testing.T) {
	cases := []struct {
		admits, privileged     string
		wantAdmits, wantPrivBt bool
	}{
		{"", "", false, false},
		{"off", "0", false, false},
		{"on", "", true, false},
		{"ON", "1", true, true},
		{"", "1", false, true},
		{"true", "true", false, false},
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
