//go:build windows

package urnettools

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeStateFile writes data to a file inside stateDir, refusing to follow a
// symlink or other reparse point at the target. Windows has no O_NOFOLLOW, so
// the target is Lstat'ed first and, after opening, the open handle is checked
// to be the same file (os.SameFile) so a swap between the check and the open
// is detected before anything is truncated or written.
func writeStateFile(stateDir, name string, data []byte, perm os.FileMode) error {
	path := filepath.Join(stateDir, name)
	before, err := os.Lstat(path)
	existed := err == nil
	if existed {
		if !before.Mode().IsRegular() {
			return fmt.Errorf("refusing to write %s: not a regular file (symlink or reparse point)", path)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("write %s: %v", path, err)
	}
	flags := os.O_WRONLY | os.O_CREATE
	if !existed {
		// The target did not exist at the Lstat above. O_EXCL makes the
		// create fail if anything (a symlink or reparse point included)
		// appeared in the meantime, instead of writing through it; the
		// SameFile check below only covers a target that existed.
		flags |= os.O_EXCL
	}
	f, err := os.OpenFile(path, flags, perm)
	if err != nil {
		return fmt.Errorf("write %s: %v", path, err)
	}
	after, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("stat %s: %v", path, err)
	}
	if !after.Mode().IsRegular() || (existed && !os.SameFile(before, after)) {
		f.Close()
		return fmt.Errorf("refusing to write %s: target changed while opening (possible symlink swap)", path)
	}
	if err := f.Truncate(0); err != nil {
		f.Close()
		return fmt.Errorf("truncate %s: %v", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %v", path, err)
	}
	return nil
}

// openNonblockFlag has no Windows equivalent; FIFOs do not exist there.
const openNonblockFlag = 0

// writeStateFileOwned is writeStateFile; there is no Unix ownership to hand over.
func writeStateFileOwned(stateDir, name string, data []byte, perm os.FileMode, ownerDir string) error {
	return writeStateFile(stateDir, name, data, perm)
}

// chownStateFile is a no-op on Windows (no Unix ownership model).
func chownStateFile(path string, uid, gid int) error {
	return nil
}

// chownStateDir is a no-op on Windows (no Unix ownership model).
func chownStateDir(path string, uid, gid int) error {
	return nil
}

// openStateFileNoFollow opens a state file without following symlinks.
// Windows has no O_NOFOLLOW, so the path is Lstat'ed, opened, and the open
// handle is verified to be the same regular file (os.SameFile) so a swap
// between the check and the open is rejected. A missing file returns the
// unwrapped *PathError so os.IsNotExist works on the result, as it does on
// unix.
func openStateFileNoFollow(stateDir, name string) (*os.File, error) {
	p := filepath.Join(stateDir, name)
	before, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing to read %s: not a regular file (symlink, reparse point or directory)", p)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		f.Close()
		return nil, fmt.Errorf("refusing to read %s: target changed while opening", p)
	}
	return f, nil
}
