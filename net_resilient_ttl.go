package connect

import (
	"net"
	"syscall"
)

// ttlControl reads and writes the socket-level TTL of a TCP connection without
// duplicating its file descriptor.
//
// The reorder technique alternates the TTL between writes to force retransmits
// and out-of-order arrival, so it needs the raw descriptor. Obtaining it via
// TCPConn.File is the obvious route but has two costs: the returned os.File is
// a dup that must be closed or the descriptor leaks, and os.File.Fd puts the
// socket into blocking mode. Blocking mode detaches the socket from the runtime
// poller, which silently stops SetReadDeadline/SetWriteDeadline from being
// enforced for the remaining life of the connection — a connection that
// fragmented its handshake would thereafter ignore its own write deadlines.
//
// SyscallConn.Control lends the descriptor for the duration of the callback
// only. The poller registration and non-blocking mode are left intact, and
// there is no second descriptor to leak or close.
//
// Control fails once the connection is closed. That is intentional and matches
// how the resilient layer fails: a write error closes the connection, and the
// TTL of a closed socket has no observable effect, so a restore that lands
// after a fail-close is correctly a no-op.
type ttlControl struct {
	raw syscall.RawConn
}

func newTtlControl(tcpConn *net.TCPConn) (*ttlControl, error) {
	raw, err := tcpConn.SyscallConn()
	if err != nil {
		return nil, err
	}
	return &ttlControl{raw: raw}, nil
}

// get returns the native socket TTL, or 0 if it cannot be read. Callers treat
// a non-positive result as "TTL is unusable" and fall back to a single
// unmodified write rather than risk setting a TTL that drops every packet at
// the first hop.
func (self *ttlControl) get() int {
	var ttl int
	if err := self.raw.Control(func(fd uintptr) {
		ttl = GetSocketTtl(SocketHandle(fd))
	}); err != nil {
		return 0
	}
	return ttl
}

// set applies ttl to the socket. Errors are ignored: the only failure mode is a
// closed connection, where the TTL no longer matters.
func (self *ttlControl) set(ttl int) {
	self.raw.Control(func(fd uintptr) {
		SetSocketTtl(SocketHandle(fd), ttl)
	})
}
