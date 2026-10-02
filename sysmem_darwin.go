//go:build darwin

package connect

import (
	"os"

	"golang.org/x/sys/unix"
)

// HostMemoryTotalBytes is the host's physical RAM from sysctl hw.memsize.
// ok is false when it cannot be read.
func HostMemoryTotalBytes() (int64, bool) {
	v, err := unix.SysctlUint64("hw.memsize")
	if err != nil || v == 0 {
		return 0, false
	}
	return int64(v), true
}

// HostMemoryAvailableBytes approximates Linux MemAvailable from the VM page
// counters: free, speculative, purgeable and file-backed (external) pages are
// all reclaimable without paging out anonymous memory. A counter the kernel
// does not report is skipped; ok is false only when none could be read.
func HostMemoryAvailableBytes() (int64, bool) {
	pages := uint64(0)
	read := false
	for _, name := range []string{
		"vm.page_free_count",
		"vm.page_speculative_count",
		"vm.page_purgeable_count",
		"vm.page_pageable_external_count",
	} {
		if v, err := unix.SysctlUint32(name); err == nil {
			pages += uint64(v)
			read = true
		}
	}
	if !read {
		return 0, false
	}
	return int64(pages) * int64(os.Getpagesize()), true
}
