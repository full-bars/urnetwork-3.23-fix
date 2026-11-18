//go:build linux

package urnettools

import (
	"reflect"
	"testing"
)

func stubOpenRCForStartTest(t *testing.T) {
	t.Helper()
	rig := newOpenRCTestRig(t, true, true)
	openrcProbeFn = func() bool { return false }
	t.Cleanup(func() {
		if rig.ran() != 0 {
			t.Errorf("unexpected OpenRC commands: %v", rig.argv)
		}
	})
}

func TestStartOpenRCRouteWhenPresent(t *testing.T) {
	rig := newOpenRCTestRig(t, true, true)
	stubDiscovery(t, nil, nil)
	if err := cmdStart(nil, false, false); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"rc-service", "urnetwork", "start"}}
	if !reflect.DeepEqual(rig.argv, want) {
		t.Fatalf("OpenRC commands = %v, want %v", rig.argv, want)
	}
}
