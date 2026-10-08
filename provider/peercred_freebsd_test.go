//go:build freebsd

package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// verifyPeerCredentials off Linux is a no-op, so nothing on Linux exercises the
// FreeBSD path. This drives it over a real AF_LOCAL socketpair-backed listener,
// which is the only way to prove LOCAL_PEERCRED returns what the code expects.
func TestVerifyPeerCredentialsAcceptsSameUID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// Two separate channels: `release` is closed exactly once by the test to
	// let the client exit, while a failed dial must not close it — the test's
	// deferred close would then panic on an already-closed channel and hide the
	// real failure. A failed dial instead closes the listener, which unblocks
	// Accept so the test cannot hang.
	release := make(chan struct{})
	go func() {
		c, err := net.Dial("unix", path)
		if err != nil {
			ln.Close() // unblock Accept rather than leaving it hanging
			return
		}
		defer c.Close()
		<-release
	}()
	defer close(release)

	conn, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer conn.Close()

	uc, ok := conn.(*net.UnixConn)
	if !ok {
		t.Fatalf("accepted conn is %T, not *net.UnixConn", conn)
	}
	// The provider and this test process share a uid, so the peer MUST be
	// accepted. A failure here means LOCAL_PEERCRED is being read wrongly and
	// every control-socket command would be refused in production.
	if err := verifyPeerCredentials(uc); err != nil {
		t.Fatalf("same-uid peer rejected: %v", err)
	}
}

// A credential that cannot be read must be REFUSED, not allowed. There is no
// easy way to make the kernel withhold LOCAL_PEERCRED here, so this pins the
// decision function on the input that matters: a peer uid that is neither the
// provider's nor root.
func TestVerifyPeerCredentialsRefusesForeignUID(t *testing.T) {
	// Pick a uid that is neither the current one nor 0.
	foreign := uint32(65534) // nobody
	if foreign == uint32(os.Getuid()) || foreign == 0 {
		t.Skip("test uid collides with the foreign uid under test")
	}
	if peerAllowed(foreign, uint32(os.Getuid())) {
		t.Fatalf("uid %d must not be accepted when the provider runs as %d",
			foreign, os.Getuid())
	}
	if !peerAllowed(0, uint32(os.Getuid())) {
		t.Error("root must be accepted: it can already manage the provider by other means")
	}
}
