//go:build linux

package main

import "syscall"

// hotSwapSocketpair creates the parent/candidate socketpair with both
// descriptors already close-on-exec.
//
// Linux can set SOCK_CLOEXEC in the socketpair(2) call itself, which is
// atomic: there is no window in which a concurrent fork/exec on another
// goroutine could inherit a descriptor that has not had CLOEXEC applied
// yet. Every other Unix has to emulate that with a lock (see the
// !linux variant), so this stays a separate file rather than a runtime
// branch.
func hotSwapSocketpair() ([2]int, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return [2]int{}, err
	}
	return [2]int{fds[0], fds[1]}, nil
}
