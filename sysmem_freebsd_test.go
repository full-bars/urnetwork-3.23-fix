//go:build freebsd

package connect

import (
	"testing"

	"golang.org/x/sys/unix"
)

// The sysctl names below are a contract with the kernel, not with the headers
// this binary was compiled against. A misspelling reads as ENOENT, which the
// readers above treat as "counter not present" and skip — so a typo silently
// removes a counter instead of failing. Each name is asserted to exist here.
func TestFreeBSDHostMemorySysctlsExist(t *testing.T) {
	for _, name := range []string{
		"hw.physmem",
		"vm.stats.vm.v_free_count",
		"vm.stats.vm.v_inactive_count",
		"vm.stats.vm.v_cache_count",
		// The kernel spells it "laundry". "launder" does not exist and the
		// reader skips unknown counters silently, so this assert is the only
		// thing that catches that class of typo.
		"vm.stats.vm.v_laundry_count",
	} {
		if _, err := unix.SysctlUint32(name); err != nil {
			if _, err2 := unix.SysctlUint64(name); err2 != nil {
				t.Errorf("sysctl %s is not readable: %v / %v", name, err, err2)
			}
		}
	}
}

// The reading must be plausible against the host's real RAM. Without this, a
// reader that returned (0, true) — or a garbage count — would satisfy the
// ok=true contract while telling the governor the box has no memory.
func TestFreeBSDHostMemoryAvailableIsPlausible(t *testing.T) {
	total, ok := HostMemoryTotalBytes()
	if !ok || total <= 0 {
		t.Fatalf("HostMemoryTotalBytes unreadable: %d %v", total, ok)
	}
	avail, aok := HostMemoryAvailableBytes()
	if !aok {
		t.Skip("no available-memory counter readable on this kernel")
	}
	if avail <= 0 {
		t.Errorf("available memory is %d on a host with %d bytes of RAM", avail, total)
	}
	if avail > total {
		t.Errorf("available %d exceeds total %d", avail, total)
	}
	// A VM can report almost no free memory, so only flag a reading that is
	// effectively zero on a host with more than a gigabyte.
	if total > 1<<30 && avail < total/1000 {
		t.Errorf("available %d is implausibly low against total %d", avail, total)
	}
}
