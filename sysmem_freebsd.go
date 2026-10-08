//go:build freebsd

package connect

import "golang.org/x/sys/unix"

// HostMemoryTotalBytes is the host's physical RAM from sysctl hw.physmem.
// ok is false when it cannot be read.
func HostMemoryTotalBytes() (int64, bool) {
	v, err := unix.SysctlUint64("hw.physmem")
	if err != nil || v == 0 {
		return 0, false
	}
	return int64(v), true
}

// HostMemoryAvailableBytes approximates Linux MemAvailable from the VM page
// statistics: free, inactive and cache pages are all reclaimable without
// paging out anonymous memory. laundry is the count of pages that have been
// freed but not yet reclaimed by the pager; it is included because the kernel
// has already dropped the reference to them.
//
// A statistic the kernel does not report is skipped rather than treated as
// zero, so a counter that appears in a later FreeBSD release degrades to
// "one fewer input" instead of silently under-reporting available memory.
// ok is false only when none could be read at all.
func HostMemoryAvailableBytes() (int64, bool) {
	// vm.stats.vm.v_*_count are signed kernel counters, so a value with the
	// sign bit set is a runaway or a reading this code does not understand,
	// not a page count. Including it would subtract real memory.
	counters := []string{
		"vm.stats.vm.v_free_count",
		"vm.stats.vm.v_inactive_count",
		"vm.stats.vm.v_cache_count",
		// Note the name: the kernel calls it "laundry", not "launder". The
		// wrong spelling reads as ENOENT and silently drops a counter that
		// exists, which is how it survived until a FreeBSD CI run.
		"vm.stats.vm.v_laundry_count",
	}

	var pages int64
	read := false
	for _, name := range counters {
		v, err := unix.SysctlUint32(name)
		if err != nil || int64(int32(v)) < 0 {
			continue
		}
		pages += int64(v)
		read = true
	}

	if !read {
		return 0, false
	}
	return pages * int64(unix.Getpagesize()), true
}
