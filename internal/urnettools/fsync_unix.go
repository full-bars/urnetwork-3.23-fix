//go:build unix

package urnettools

import (
	"os"
	"syscall"
)

// statFileOwnership returns the numeric uid and gid from a file's inode.
// Only meaningful on Unix; returns (-1, -1) on other platforms.
func statFileOwnership(fi os.FileInfo) (uid, gid int64) {
	if sys := fi.Sys(); sys != nil {
		if s, ok := sys.(*syscall.Stat_t); ok {
			return int64(s.Uid), int64(s.Gid)
		}
	}
	return -1, -1
}

// fsyncFile ensures data durability before rename. Called from copyFile
// before the file descriptor is closed. On Windows syscall.Fsync expects
// a Handle, not an int, so this is a no-op on non-Unix.
func fsyncFile(f *os.File) error {
	return syscall.Fsync(int(f.Fd()))
}
