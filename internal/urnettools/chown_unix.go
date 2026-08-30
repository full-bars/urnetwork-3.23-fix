//go:build unix

package urnettools

import (
	"os"
	"syscall"
)

// chownLikeStateOwner chowns path to the owner of stateDir when the caller is a
// different user (cross-user session load: the provider's uid must be able to
// read what the tool staged under a root run). No-op when ownership already
// matches.
func chownLikeStateOwner(stateDir, path string) error {
	fi, err := os.Stat(stateDir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	pfi, err := os.Stat(path)
	if err != nil {
		return err
	}
	pst, ok := pfi.Sys().(*syscall.Stat_t)
	if !ok || (pst.Uid == st.Uid && pst.Gid == st.Gid) {
		return nil
	}
	return os.Chown(path, int(st.Uid), int(st.Gid))
}
