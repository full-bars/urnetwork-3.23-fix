//go:build freebsd

package main

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// verifyPeerCredentials checks that the connecting process has the same UID as
// the provider, or is root, using the socket's LOCAL_PEERCRED option.
//
// Without this the control socket is protected by file permissions alone. That
// is weaker on FreeBSD than it is on Linux for two reasons: the socket is a
// filesystem path any local user can attempt to connect to, and FreeBSD's
// LOCAL_PEERCRED is available precisely so a server can ask the kernel who is on
// the other end rather than trusting the connection attempt.
func verifyPeerCredentials(conn *net.UnixConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("peer cred: %w", err)
	}

	var xcred *unix.Xucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		xcred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return fmt.Errorf("peer cred raw: %w", err)
	}
	if credErr != nil {
		return fmt.Errorf("peer cred: %w", credErr)
	}
	if xcred == nil {
		// No credential came back. Treating that as "allowed" would hand the
		// socket to anyone; treat it as the refusal it is.
		return fmt.Errorf("peer cred: kernel returned no credentials")
	}

	providerUID := uint32(os.Getuid())
	if !peerAllowed(xcred.Uid, providerUID) {
		return fmt.Errorf("peer cred: UID %d is neither the provider UID %d nor root", xcred.Uid, providerUID)
	}
	return nil
}
