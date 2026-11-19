//go:build linux

package nicstat

import (
	"os"
	"testing"
)

func TestLinuxLiveCounters(t *testing.T) {
	if _, err := os.Stat("/proc/net/dev"); os.IsNotExist(err) {
		t.Skip("procfs not mounted")
	} else if err != nil {
		t.Fatal(err)
	}
	counters, err := Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range counters {
		if c.Name == "lo" && !c.Loopback {
			t.Error("lo was not marked loopback")
		}
	}
}
