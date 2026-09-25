//go:build unix

package urnettools

import (
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// chownConfigToDirOwner hands a root-created config file to the owner of its
// directory, so a user's own next non-sudo save can overwrite it. Under sudo
// with HOME preserved the config would otherwise stay root-owned in the
// invoking user's config directory. No-op when not running as root or when
// the directory is owned by root. Best-effort: a failure to chown is not a
// reason to fail a settings save.
func chownConfigToDirOwner(path string) {
	if os.Geteuid() != 0 {
		return
	}
	fi, err := os.Stat(path)
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
	dfi, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return
	}
	ds, ok := dfi.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	_ = unix.Chown(path, int(ds.Uid), int(ds.Gid))
}
