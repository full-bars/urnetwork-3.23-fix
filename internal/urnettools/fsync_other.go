//go:build !unix

package urnettools

import "os"

// statFileOwnership returns the numeric uid and gid from a file's inode.
// No-op on non-Unix platforms.
func statFileOwnership(fi os.FileInfo) (uid, gid int64) {
	return -1, -1
}

// fsyncFile ensures data durability before rename. No-op on non-Unix:
// Windows syscall.Fsync expects a Handle, not an int.
func fsyncFile(f *os.File) error {
	return nil
}