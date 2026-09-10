//go:build !linux

package main

import "net"

// verifyPeerCredentials is a no-op on non-Linux platforms where
// SO_PEERCRED is not available. The socket file permissions (0600)
// provide the equivalent protection on macOS/Windows.
func verifyPeerCredentials(_ *net.UnixConn) error {
	return nil
}
