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
	first, err := Read()
	if err != nil {
		t.Fatal(err)
	}
	second, err := Read()
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]Counters)
	for _, c := range second {
		byName[c.Name] = c
	}
	for _, c := range first {
		d, ok := byName[c.Name]
		if !ok {
			continue
		} // Interfaces may disappear between snapshots.
		if d.RxBytes < c.RxBytes || d.TxBytes < c.TxBytes {
			t.Errorf("counter decreased on %s", c.Name)
		}
	}
	lo, ok := byName["lo"]
	if ok && !lo.Loopback {
		t.Error("lo was not marked loopback")
	}
}
