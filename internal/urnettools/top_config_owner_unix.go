//go:build unix

package urnettools

import (
	"os"
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
