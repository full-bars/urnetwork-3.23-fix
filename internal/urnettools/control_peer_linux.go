//go:build linux

package urnettools

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// socketPeerPID returns SO_PEERCRED's listener credentials captured at listen
// time, not necessarily the process that later accepts or writes. A listener
// inherited by a forked child still reports the original listener's PID. The
// HotSwap candidate binds its own control socket after the parent releases it
// (provider/main.go); it does not inherit that listener.
func socketPeerPID(conn net.Conn) (int, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("not a unix socket connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	if cred.Pid <= 0 {
		return 0, fmt.Errorf("peer pid not reported")
	}
	return int(cred.Pid), nil
}
