//go:build linux

package connect

import (
	"os"
	"testing"
	"time"
)

// countOpenFds counts the process's open file descriptors via /proc/self/fd,
// which only exists on Linux.
func countOpenFds(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	return len(entries)
}

// TestResilientTlsConnFragmentDoesNotLeakFileDescriptors guards the fragment
// path against descriptor growth on failure. It originally caught a leak where
// every failed fragment write stranded one fd from tcpConn.File(); the path now
// reads the TTL through SyscallConn and never dups at all, so this stays as a
// regression guard against reintroducing a File-based descriptor.
func TestResilientTlsConnFragmentDoesNotLeakFileDescriptors(t *testing.T) {
	record := buildClientHelloRecord(t)

	before := countOpenFds(t)
	for i := 0; i < 20; i++ {
		client, server := newTcpPair(t)
		client.SetWriteDeadline(time.Now().Add(-time.Second))
		rconn := NewResilientTlsConn(client, true, true)
		if _, err := rconn.Write(record); err == nil {
			t.Fatalf("iteration %d: expected write error, got nil", i)
		}
		client.Close()
		server.Close()
	}
	after := countOpenFds(t)
	if after > before+5 {
		t.Fatalf("file descriptors grew from %d to %d over 20 failed fragment writes", before, after)
	}
}

// TestResilientTlsConnReorderOnlyDoesNotLeakFileDescriptors is the same
// regression guard for the reorder-only path.
func TestResilientTlsConnReorderOnlyDoesNotLeakFileDescriptors(t *testing.T) {
	record := buildClientHelloRecord(t)

	before := countOpenFds(t)
	for i := 0; i < 20; i++ {
		client, server := newTcpPair(t)
		client.SetWriteDeadline(time.Now().Add(-time.Second))
		rconn := NewResilientTlsConn(client, false, true)
		if _, err := rconn.Write(record); err == nil {
			t.Fatalf("iteration %d: expected write error, got nil", i)
		}
		client.Close()
		server.Close()
	}
	after := countOpenFds(t)
	if after > before+5 {
		t.Fatalf("file descriptors grew from %d to %d over 20 failed reorder writes", before, after)
	}
}
