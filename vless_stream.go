package connect

// vless_stream.go — the VLESS request and response framing (protocol
// version 0) over an established transport.
//
// Request, sent once before the first inner byte:
//
//	version (0) | user id (16) | addons length (1) | addons |
//	command (1 = tcp) | port (2, big endian) | address type (1) | address
//
// The address type is 1 for ipv4 (4 bytes), 2 for a domain (a length byte and
// the name) and 3 for ipv6 (16 bytes). The addons are the protobuf message
// `{ string flow = 1; }`, empty without a flow.
//
// Response, read once before the first inner byte is returned:
//
//	version (0) | addons length (1) | addons

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
)

const (
	vlessVersion    = byte(0)
	vlessCommandTcp = byte(1)

	vlessAddressTypeIpv4   = byte(1)
	vlessAddressTypeDomain = byte(2)
	vlessAddressTypeIpv6   = byte(3)
)

// The request header for one tcp stream to host:port.
func vlessRequestHeader(id [16]byte, flow string, host string, port uint16) ([]byte, error) {
	var addons []byte
	if flow != VlessFlowNone {
		if 127 < len(flow) {
			return nil, errors.New("vless flow is too long")
		}
		// field 1, wire type 2 (length delimited)
		addons = append(addons, 0x0a, byte(len(flow)))
		addons = append(addons, flow...)
	}

	header := make([]byte, 0, 1+16+1+len(addons)+1+2+1+1+len(host))
	header = append(header, vlessVersion)
	header = append(header, id[:]...)
	header = append(header, byte(len(addons)))
	header = append(header, addons...)
	header = append(header, vlessCommandTcp)
	header = append(header, byte(port>>8), byte(port))
	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap()
		if addr.Zone() != "" {
			return nil, fmt.Errorf("vless cannot carry a zoned address: %s", host)
		}
		if addr.Is4() {
			ip := addr.As4()
			header = append(header, vlessAddressTypeIpv4)
			header = append(header, ip[:]...)
		} else {
			ip := addr.As16()
			header = append(header, vlessAddressTypeIpv6)
			header = append(header, ip[:]...)
		}
	} else {
		if host == "" || 255 < len(host) {
			return nil, fmt.Errorf("vless destination host must be 1 to 255 bytes: %q", host)
		}
		header = append(header, vlessAddressTypeDomain, byte(len(host)))
		header = append(header, host...)
	}
	return header, nil
}

// Reads and checks the response header. The addons are read and dropped:
// a server sends none to a client that asked for none, and the vision flow
// carries everything it needs in the padding.
func readVlessResponseHeader(r io.Reader) error {
	var head [2]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return err
	}
	if head[0] != vlessVersion {
		return fmt.Errorf("vless response version %d", head[0])
	}
	if addonsLength := int(head[1]); 0 < addonsLength {
		if _, err := io.CopyN(io.Discard, r, int64(addonsLength)); err != nil {
			return err
		}
	}
	return nil
}

// One VLESS stream without a flow. The request header goes out with the first
// write -- the inner tls client hello, on every strategy dial -- so the
// server gets both in one packet. A read before any write sends the header
// alone first, since the server answers nothing before it.
//
// Read and Write may be called concurrently with each other, as on any
// net.Conn; concurrent writes are serialized.
type vlessConn struct {
	net.Conn

	writeLock     sync.Mutex
	requestHeader []byte
	requestSent   bool

	// the response header is parsed once. The lock serializes concurrent
	// readers, and the buffer keeps bytes a timed out read consumed, so a
	// later Read can resume after the caller refreshes the deadline
	responseLock sync.Mutex
	responseDone bool
	responseBuf  []byte
	responseErr  error
}

func newVlessConn(conn net.Conn, requestHeader []byte) *vlessConn {
	return &vlessConn{
		Conn:          conn,
		requestHeader: requestHeader,
	}
}

func (self *vlessConn) Write(b []byte) (int, error) {
	self.writeLock.Lock()
	defer self.writeLock.Unlock()
	if !self.requestSent {
		self.requestSent = true
		packet := make([]byte, 0, len(self.requestHeader)+len(b))
		packet = append(packet, self.requestHeader...)
		packet = append(packet, b...)
		self.requestHeader = nil
		if _, err := self.Conn.Write(packet); err != nil {
			return 0, err
		}
		return len(b), nil
	}
	return self.Conn.Write(b)
}

func (self *vlessConn) sendRequestHeader() error {
	self.writeLock.Lock()
	defer self.writeLock.Unlock()
	if self.requestSent {
		return nil
	}
	self.requestSent = true
	requestHeader := self.requestHeader
	self.requestHeader = nil
	_, err := self.Conn.Write(requestHeader)
	return err
}

func (self *vlessConn) Read(b []byte) (int, error) {
	if err := self.readResponseHeader(); err != nil {
		return 0, err
	}
	self.responseLock.Lock()
	if 0 < len(self.responseBuf) {
		n := copy(b, self.responseBuf)
		self.responseBuf = self.responseBuf[n:]
		self.responseLock.Unlock()
		return n, nil
	}
	self.responseLock.Unlock()
	return self.Conn.Read(b)
}

// Consumes the response header, resuming where a previous call stopped. A read
// error leaves the bytes read so far in responseBuf, so a timeout does not
// poison the stream: the caller can refresh the deadline and call Read again.
func (self *vlessConn) readResponseHeader() error {
	self.responseLock.Lock()
	defer self.responseLock.Unlock()
	if self.responseDone {
		return self.responseErr
	}
	if err := self.sendRequestHeader(); err != nil {
		return err
	}
	for {
		if 2 <= len(self.responseBuf) {
			if self.responseBuf[0] != vlessVersion {
				self.responseDone = true
				self.responseErr = fmt.Errorf("vless response version %d", self.responseBuf[0])
				return self.responseErr
			}
			headerLength := 2 + int(self.responseBuf[1])
			if headerLength <= len(self.responseBuf) {
				self.responseBuf = append(self.responseBuf[:0], self.responseBuf[headerLength:]...)
				self.responseDone = true
				self.responseErr = nil
				return nil
			}
		}
		var chunk [512]byte
		n, err := self.Conn.Read(chunk[:])
		if 0 < n {
			self.responseBuf = append(self.responseBuf, chunk[:n]...)
		}
		if err != nil {
			return err
		}
	}
}
