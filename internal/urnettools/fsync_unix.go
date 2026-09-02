//go:build unix

package urnettools

import (
	"fmt"
	"os"
	"path/filepath"
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

// writeFileAtomic writes data to path atomically using an unpredictable
// temp name (os.CreateTemp) + fsync + rename. Prevents:
// - Concurrent writers clobbering each other's fixed-name .tmp file (M7)
// - Half-written files visible to live readers on crash
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
	if err := fsyncFile(tmpFile); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return err
	}
	// Apply perm through the held fd (not os.Chmod by path) so a symlink
	// planted at tmpPath cannot redirect the metadata change (same class
	// as installBinary's CRITICAL). Then close.
	if err := tmpFile.Chmod(perm); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	// Sync the parent directory so the rename survives a crash before the
	// directory entry is committed (fsync after rename).
	if dir, err := os.Open(dir); err == nil {
		dir.Sync()
		dir.Close()
	}
	return nil
}
