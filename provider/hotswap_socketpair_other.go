//go:build !windows && !linux

package main

import "syscall"

// hotSwapSocketpair creates the parent/candidate socketpair with both
// descriptors already close-on-exec.
//
// darwin and the BSDs have no SOCK_CLOEXEC flag for socketpair(2), so
// CLOEXEC has to be set in a second step. Holding syscall.ForkLock for
// read across both steps is the same protocol the standard library uses
// (os/exec, net): it blocks forks for the window between creating the
// descriptors and marking them, which is exactly the leak SOCK_CLOEXEC
// closes atomically on Linux.
//
// CLOEXEC on the child half is not a problem for the handoff: os/exec
// dups the ExtraFiles descriptor into the child and clears CLOEXEC on
// the dup, which is why the Linux SOCK_CLOEXEC path works the same way.
func hotSwapSocketpair() ([2]int, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return [2]int{}, err
	}
	syscall.CloseOnExec(fds[0])
	syscall.CloseOnExec(fds[1])
	return [2]int{fds[0], fds[1]}, nil
}
