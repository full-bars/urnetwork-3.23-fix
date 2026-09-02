//go:build !unix

package urnettools

import (
	"fmt"
	"os"
	"path/filepath"
)

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

// writeFileAtomic writes data to path atomically using os.CreateTemp + rename.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmpFile, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return err
	}
	tmpFile.Close()
	return os.Rename(tmpPath, path)
}
