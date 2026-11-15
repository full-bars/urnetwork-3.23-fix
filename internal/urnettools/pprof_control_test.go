package urnettools

import "testing"

func TestValidPprofControlValue(t *testing.T) {
	for _, value := range []string{"on", "off", "ON", "127.0.0.1:6060", "[::1]:7000", "127.0.0.1:0"} {
		if !validPprofControlValue(value) {
			t.Fatalf("%q should be valid", value)
		}
	}
	for _, value := range []string{"", "maybe", "0.0.0.0:6060", "192.168.1.5:6060", "localhost:6060", "127.0.0.1", "127.0.0.1:70000"} {
		if validPprofControlValue(value) {
			t.Fatalf("%q should be invalid", value)
		}
	}
}

// pprof defaults to on, so `set pprof off` has to be stored as off. Turning
// it into a clear would put the default back, which is on.
func TestPprofOffIsNotAClear(t *testing.T) {
	if treatsOffAsClear("pprof") {
		t.Fatal("set pprof off must persist off, not clear the key")
	}
}
