package main

import (
	"encoding/json"
	"math"
	"runtime"
	"runtime/debug"
	"testing"
)

func TestCollectResources(t *testing.T) {
	r := collectResources()
	if r.HeapInuseBytes == 0 {
		t.Error("HeapInuseBytes = 0, want > 0")
	}
	if r.Goroutines < 1 {
		t.Errorf("Goroutines = %d, want >= 1", r.Goroutines)
	}
	if runtime.GOOS == "linux" {
		if r.RSSBytes == 0 {
			t.Error("RSSBytes = 0 on linux")
		}
		if r.OpenFDs == 0 {
			t.Error("OpenFDs = 0 on linux")
		}
		if r.FDLimit != 0 && r.OpenFDs > r.FDLimit {
			t.Errorf("OpenFDs %d above FDLimit %d", r.OpenFDs, r.FDLimit)
		}
	} else if r.RSSBytes != 0 || r.OpenFDs != 0 || r.FDLimit != 0 {
		t.Errorf("non-linux reported linux-only fields: %+v", r)
	}
}

func TestSnapshotResourcesOmitsUnknown(t *testing.T) {
	b, err := json.Marshal(SnapshotResources{HeapInuseBytes: 1, Goroutines: 2})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"mem_limit_bytes", "rss_bytes", "open_fds", "fd_limit"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s present, want omitted when unknown", k)
		}
	}
}

func TestCollectResourcesMemLimit(t *testing.T) {
	prev := debug.SetMemoryLimit(1 << 30)
	defer debug.SetMemoryLimit(prev)
	if got := collectResources().MemLimitBytes; got != 1<<30 {
		t.Errorf("MemLimitBytes = %d, want %d", got, 1<<30)
	}

	debug.SetMemoryLimit(math.MaxInt64)
	if got := collectResources().MemLimitBytes; got != 0 {
		t.Errorf("MemLimitBytes = %d with no limit set, want 0 (omitted)", got)
	}
}
