//go:build !windows

package main

import (
	"os"
	"syscall"
)

// sourceOwnerOf returns the uid and gid that own the file described by info.
func sourceOwnerOf(info os.FileInfo) (*fileOwner, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, false
	}
	return &fileOwner{uid: int(stat.Uid), gid: int(stat.Gid)}, true
}
