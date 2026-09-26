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

// The pressure sensor's memory headroom was read from the mount root's
// memory.max and memory.current, which exist only in a container. On a systemd
// host the process's limit and usage live on its own cgroup, so headroom came
// back "unknown" exactly where it mattered. Headroom is the smallest
// (limit - current) over the process's cgroup and every ancestor that has a
// limit; memory.high counts as a limit because the kernel throttles at it.
func TestCgroupV2MemoryHeadroom(t *testing.T) {
	const mib = int64(1) << 20
	unit := "/user.slice/user-1000.slice/user@1000.service/app.slice/urnetwork.service"
	self := "0::" + unit + "\n"

	t.Run("container: the mount root is the cgroup", func(t *testing.T) {
		root := t.TempDir()
		writeCgroupFile(t, root, ".", "memory.max", "536870912")
		writeCgroupFile(t, root, ".", "memory.current", "268435456")
		got, ok := cgroupV2MemoryHeadroom(root, "0::/\n")
		if !ok || got != 256*mib {
			t.Fatalf("got %d ok=%v, want 256 MiB", got, ok)
		}
	})

	t.Run("systemd unit: headroom to memory.high", func(t *testing.T) {
		root := t.TempDir()
		writeCgroupFile(t, root, unit, "memory.max", "524288000")     // 500M
		writeCgroupFile(t, root, unit, "memory.high", "471859200")    // 450M
		writeCgroupFile(t, root, unit, "memory.current", "419430400") // 400M
		got, ok := cgroupV2MemoryHeadroom(root, self)
		if !ok || got != 50*mib {
			t.Fatalf("got %d ok=%v, want 50 MiB (450M high - 400M used)", got, ok)
		}
	})

	t.Run("an ancestor with less room is the binding one", func(t *testing.T) {
		root := t.TempDir()
		writeCgroupFile(t, root, unit, "memory.max", "max")
		writeCgroupFile(t, root, unit, "memory.current", "104857600") // 100M
		parent := "/user.slice/user-1000.slice"
		writeCgroupFile(t, root, parent, "memory.max", "314572800")     // 300M
		writeCgroupFile(t, root, parent, "memory.current", "268435456") // 256M
		got, ok := cgroupV2MemoryHeadroom(root, self)
		if !ok || got != 44*mib {
			t.Fatalf("got %d ok=%v, want 44 MiB from the ancestor", got, ok)
		}
	})

	t.Run("usage above the limit is zero headroom, never negative", func(t *testing.T) {
		root := t.TempDir()
		writeCgroupFile(t, root, unit, "memory.max", "104857600")
		writeCgroupFile(t, root, unit, "memory.current", "157286400")
		got, ok := cgroupV2MemoryHeadroom(root, self)
		if !ok || got != 0 {
			t.Fatalf("got %d ok=%v, want 0", got, ok)
		}
	})

	t.Run("no limit, no usage file, garbage: not found", func(t *testing.T) {
		root := t.TempDir()
		writeCgroupFile(t, root, unit, "memory.max", "max")
		writeCgroupFile(t, root, unit, "memory.current", "1000")
		if _, ok := cgroupV2MemoryHeadroom(root, self); ok {
			t.Fatalf("an unlimited cgroup has no headroom figure")
		}
		root2 := t.TempDir()
		writeCgroupFile(t, root2, unit, "memory.max", "104857600") // limit but no memory.current
		if _, ok := cgroupV2MemoryHeadroom(root2, self); ok {
			t.Fatalf("a limit without a usage reading must not invent headroom")
		}
		root3 := t.TempDir()
		writeCgroupFile(t, root3, unit, "memory.max", "nope")
		writeCgroupFile(t, root3, unit, "memory.current", "nope")
		if _, ok := cgroupV2MemoryHeadroom(root3, self); ok {
			t.Fatalf("garbage must not produce a figure")
		}
	})

	t.Run("an inner limited level without usage must not adopt ancestor room", func(t *testing.T) {
		// The kernel enforces the child's 50M limit even though its usage
		// file is unreadable here; the ancestor's 400M of room must not be
		// reported as available to the process.
		root := t.TempDir()
		writeCgroupFile(t, root, unit, "memory.max", "52428800") // 50M, no memory.current
		parent := "/user.slice/user-1000.slice"
		writeCgroupFile(t, root, parent, "memory.max", "524288000")    // 500M
		writeCgroupFile(t, root, parent, "memory.current", "104857600") // 100M used -> 400M room
		if got, ok := cgroupV2MemoryHeadroom(root, self); ok {
			t.Fatalf("got %d MiB of headroom from the ancestor, want the chain indeterminate", got>>20)
		}
	})
}
