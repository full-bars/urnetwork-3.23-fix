//go:build unix

package urnettools

import (
	"os"
	"path/filepath"
	"syscall"
)

// chownConfigFdToDirOwner hands a root-created config to the owner of its
// directory, so a user's own next non-sudo save can overwrite it. Under sudo
// with HOME preserved the config would otherwise stay root-owned in the
// invoking user's config directory. The file is chowned by its open
// descriptor (os.File.Chown works on the fd, not a path), so a concurrent
// swap of the path to a symlink cannot redirect the ownership change to an
// unrelated root-owned file. No-op when not running as root or when the
// directory is owned by root. Best-effort: a failure to chown is not a reason
// to fail a settings save.
func chownConfigFdToDirOwner(file *os.File, dir string) {
	if os.Geteuid() != 0 {
		return
	}
	fi, err := file.Stat()
	if err != nil {
		return
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	// Already owned by the user that will read it back.
	if st.Uid != 0 {
		return
	}
	dfi, err := os.Stat(dir)
	if err != nil {
		return
	}
	ds, ok := dfi.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	_ = file.Chown(int(ds.Uid), int(ds.Gid))
}

// mkdirAllForOwner is os.MkdirAll for a per-user config directory. When running
// as root it hands every directory it creates to the owner of the nearest
// directory that already existed, usually the invoking user's home.
func mkdirAllForOwner(dir string, perm os.FileMode) error {
	return mkdirAllHandOver(dir, perm, os.Geteuid() == 0, os.Lchown)
}

// mkdirAllHandOver creates dir and its missing parents. Under sudo with HOME
// preserved, the first save creates ~/.config/urnet-tools as root, so
// chownConfigFdToDirOwner then sees a root-owned directory, does nothing, and
// the invoking user's next non-sudo save fails in os.CreateTemp. With root true
// it chowns each newly created directory, outermost first, to the owner of the
// nearest ancestor that already existed (nothing to do when that is root).
// Lchown, not Chown, so a path swapped for a symlink is not followed. A chown
// failure is not a reason to fail a settings save.
func mkdirAllHandOver(dir string, perm os.FileMode, root bool, chown func(path string, uid, gid int) error) error {
	if !root {
		return os.MkdirAll(dir, perm)
	}
	var missing []string
	ancestor := filepath.Clean(dir)
	for {
		if fi, err := os.Stat(ancestor); err == nil && fi.IsDir() {
			break
		}
		missing = append(missing, ancestor)
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			break
		}
		ancestor = parent
	}
	if err := os.MkdirAll(dir, perm); err != nil {
		return err
	}
	fi, err := os.Stat(ancestor)
	if err != nil {
		return nil
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid == 0 {
		return nil
	}
	for i := len(missing) - 1; i >= 0; i-- {
		_ = chown(missing[i], int(st.Uid), int(st.Gid))
	}
	return nil
}
