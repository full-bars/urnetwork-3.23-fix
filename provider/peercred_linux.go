//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// verifyPeerCredentials checks that the connecting process has the same
// UID as the provider, or is root (uid 0). Root can always manage any
// provider (it already has filesystem access to the state dir and the
// ability to signal the process). This prevents unprivileged users on
// the system from sending commands to the control socket while allowing
// root (systemd, cron, fleet scripts) to manage the provider.
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
	if !peerAllowed(ucred.Uid, providerUID) {
		return fmt.Errorf("peer cred: UID %d is neither the provider UID %d nor root", ucred.Uid, providerUID)
	}
	return nil
}
