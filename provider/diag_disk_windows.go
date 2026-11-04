//go:build windows

package main

import "golang.org/x/sys/windows"

// diskFreeBytes is the space available to the calling user on the volume
// holding dir.
func diskFreeBytes(dir string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, err
	}
	return avail, nil
}
