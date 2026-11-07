package main

import "syscall"

func diskFreeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return diagAvailableBytes(st.F_bavail, uint64(st.F_bsize)), nil
}
