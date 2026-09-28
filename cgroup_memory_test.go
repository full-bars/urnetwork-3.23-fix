package connect

import (
	"os"
	"path/filepath"
	"testing"
)

// writeCgroupFile lays out one cgroup v2 control file under a fake mount.
func writeCgroupFile(t *testing.T, root, rel, name, value string) {
	t.Helper()
	dir := filepath.Join(root, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The ceiling that matters is the one the kernel enforces on THIS process:
// the tightest memory.max or memory.high on its own cgroup or any ancestor.
// Reading only the mount root's memory.max (the old behavior) works inside a
// container, whose root IS its cgroup, but on a systemd host the unit sets its
// limits on its own directory, so MemoryMax=/MemoryHigh= were never seen.
func TestCgroupV2MemoryCeiling(t *testing.T) {
	const mib = int64(1) << 20
	unit := "/user.slice/user-1000.slice/user@1000.service/app.slice/urnetwork.service"
	self := "0::" + unit + "\n"

	t.Run("container: the mount root is the cgroup", func(t *testing.T) {
		root := t.TempDir()
		writeCgroupFile(t, root, ".", "memory.max", "536870912")
		got, ok := cgroupV2MemoryCeiling(root, "0::/\n")
		if !ok || got != 512*mib {
			t.Fatalf("got %d ok=%v, want 512 MiB", got, ok)
		}
	})

	t.Run("systemd unit: memory.high is tighter than memory.max", func(t *testing.T) {
		root := t.TempDir()
		writeCgroupFile(t, root, unit, "memory.max", "524288000")  // 500M
		writeCgroupFile(t, root, unit, "memory.high", "471859200") // 450M
		got, ok := cgroupV2MemoryCeiling(root, self)
		if !ok || got != 450*mib {
			t.Fatalf("got %d ok=%v, want 450 MiB (memory.high throttles first)", got, ok)
		}
	})

	t.Run("an ancestor slice can be the tightest limit", func(t *testing.T) {
		root := t.TempDir()
		writeCgroupFile(t, root, unit, "memory.max", "max")
		writeCgroupFile(t, root, "/user.slice/user-1000.slice", "memory.max", "314572800") // 300M
		got, ok := cgroupV2MemoryCeiling(root, self)
		if !ok || got != 300*mib {
			t.Fatalf("got %d ok=%v, want 300 MiB from the ancestor", got, ok)
		}
	})

	t.Run("no limit anywhere is reported as not found", func(t *testing.T) {
		root := t.TempDir()
		writeCgroupFile(t, root, unit, "memory.max", "max")
		writeCgroupFile(t, root, unit, "memory.high", "max")
		if got, ok := cgroupV2MemoryCeiling(root, self); ok {
			t.Fatalf("got %d, want not found", got)
		}
	})

	t.Run("garbage and a missing tree degrade to not found", func(t *testing.T) {
		root := t.TempDir()
		writeCgroupFile(t, root, unit, "memory.max", "not-a-number")
		if _, ok := cgroupV2MemoryCeiling(root, self); ok {
			t.Fatalf("garbage memory.max must not produce a ceiling")
		}
		if _, ok := cgroupV2MemoryCeiling(t.TempDir(), self); ok {
			t.Fatalf("missing tree must not produce a ceiling")
		}
		if _, ok := cgroupV2MemoryCeiling(root, "no-v2-line\n1:name=x:/y\n"); ok {
			t.Fatalf("a /proc/self/cgroup with no v2 line must not produce a ceiling")
		}
	})
}
