//go:build unix

package urnettools

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// writeStateFile writes data to a file inside stateDir with O_NOFOLLOW to
// prevent symlink-following attacks. If a symlink exists at the target path,
// the write is refused rather than silently overwriting the symlink target.
// This prevents a privileged-provider-user from planting
// node_name -> /etc/shadow and having root overwrite shadow via urnet-tools set.
func writeStateFile(stateDir, name string, data []byte, perm os.FileMode) error {
	path := filepath.Join(stateDir, name)

	// Reject if path is already a symlink (attack indicator)
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to write %s: path is a symlink (possible symlink attack)", path)
	}

	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_NOFOLLOW, uint32(perm))
	if err != nil {
		return fmt.Errorf("write %s: %v", path, err)
	}
	defer unix.Close(fd)

	_, err = unix.Write(fd, data)
	return err
}

// chownStateFile changes ownership of a file without following symlinks.
// Uses Lchown instead of Chown so a symlink at the target path is not followed.
func chownStateFile(path string, uid, gid int) error {
	return unix.Lchown(path, uid, gid)
}

// chownStateDir changes ownership of the state directory without following
// symlinks. Uses Lchown instead of os.Chown.
func chownStateDir(path string, uid, gid int) error {
	return unix.Lchown(path, uid, gid)
}
