//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// verifyPeerCredentials checks that the connecting process has the same
// UID as the provider. This prevents any other user on the system from
// sending commands to the control socket (defense-in-depth alongside
// the 0600 file permission on the socket).
func verifyPeerCredentials(conn *net.UnixConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("peer cred: %w", err)
	}

	var ucred *syscall.Ucred
	var credErr error
	err = raw.Control(func(fd uintptr) {
		ucred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil {
		return fmt.Errorf("peer cred raw: %w", err)
	}
	if credErr != nil {
		return fmt.Errorf("peer cred: %w", credErr)
	}

	providerUID := uint32(os.Getuid())
	if ucred.Uid != providerUID {
		return fmt.Errorf("peer cred: UID %d != provider UID %d", ucred.Uid, providerUID)
	}
	return nil
}
