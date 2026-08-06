package connect

import (
	"context"
	"net"
	// "net/http"

	// "os"
	// "strings"
	"fmt"
	"io"
	"time"
	// "strconv"
	// "slices"

	"crypto/tls"
	// "crypto/ecdsa"
	// "crypto/ed25519"
	// "crypto/elliptic"
	// "crypto/rand"
	// "crypto/rsa"
	// "crypto/x509"
	// "crypto/x509/pkix"
	// "encoding/pem"
	// "encoding/json"
	// "flag"
	// "log"
	// "math/big"

	// "crypto/md5"
	"encoding/binary"
	// "encoding/hex"
	// "syscall"

	mathrand "math/rand"

	"golang.org/x/crypto/cryptobyte"
	// "golang.org/x/net/idna"

	// "google.golang.org/protobuf/proto"

	"src.agwa.name/tlshacks"
)

// see https://upb-syssec.github.io/blog/2023/record-fragmentation/

// set this as the `DialTLSContext` or equivalent
// returns a tls connection
func NewResilientDialTlsContext(
	connectSettings *ConnectSettings,
	fragment bool,
	reorder bool,
) DialTlsContextFunction {
	return func(
		ctx context.Context,
		network string,
		addr string,
	) (net.Conn, error) {
		switch network {
		case "tcp", "tcp4", "tcp6":
		default:
			panic(fmt.Errorf("Resilient connections only support tcp network."))
		}

		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			panic(err)
		}

		// fmt.Printf("Extender client 1\n")

		conn, err := connectSettings.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}

		rconn := NewResilientTlsConn(conn, fragment, reorder)

		// copy and extend
		tlsConfig := connectSettings.TlsConfig.Clone()
		tlsConfig.ServerName = host
		tlsConn := tls.Client(rconn, tlsConfig)

		func() {
			tlsCtx, tlsCancel := context.WithTimeout(ctx, connectSettings.TlsTimeout)
			defer tlsCancel()
			err = tlsConn.HandshakeContext(tlsCtx)
		}()
		if err != nil {
			tlsConn.Close()
			return nil, err
		}
		// once the stream is established, no longer need the resilient features
		if err := rconn.Off(); err != nil {
			tlsConn.Close()
			return nil, err
		}

		return tlsConn, nil
	}
}

// adapts techniques to overcome adversarial networks
// the network uses this to the connect to the platform and extenders
// inspiraton for techniques taken from the Jigsaw project Outline SDK

type ResilientTlsConn struct {
	conn     net.Conn
	fragment bool
	reorder  bool
	buffer   []byte
	enabled  bool
}

// must be created before the tls connection starts
func NewResilientTlsConn(conn net.Conn, fragment bool, reorder bool) *ResilientTlsConn {
	return &ResilientTlsConn{
		conn:     conn,
		fragment: fragment,
		reorder:  reorder,
		buffer:   []byte{},
		enabled:  true,
	}
}

// Off permanently disables the resilient fragment/reorder layer. It cannot be
// re-enabled: once the TLS stream has been written past, the record boundaries
// needed to realign the fragmentation are no longer known. It drains
// any partially-buffered record first — an earlier Write already returned
// len(b), nil for those bytes, so stranding them would silently lose data
// the caller believes was sent. A partial or failed drain leaves the wire
// state indeterminate: the connection is failed closed (closed, so the
// caller must not hand it back as established) and the drain error is
// returned — io.ErrShortWrite when the drain came up short with a nil
// error. Returns nil on a successful drain or when the buffer is empty.
// Off is not safe for concurrent use with Write.
func (self *ResilientTlsConn) Off() error {
	if 0 < len(self.buffer) {
		n, err := self.conn.Write(self.buffer)
		if err != nil || n < len(self.buffer) {
			if err == nil {
				err = io.ErrShortWrite
			}
			self.failConnection()
			return err
		}
		self.buffer = nil
	}
	// can't turn back on after off because we don't know where to align the tls header
	self.enabled = false
	return nil
}

// failConnection marks the connection unusable after an indeterminate write
// (a partial or failed record send: the peer has part of the bytes and the
// wire state is unknowable). The buffered record is dropped so it is never
// re-sent, the resilient layer is disabled, and the underlying connection is
// closed so later writes fail instead of appending to a corrupt stream.
func (self *ResilientTlsConn) failConnection() {
	self.buffer = nil
	self.enabled = false
	self.conn.Close()
}

// writeRecord writes record whole to w. On any short or failed write the
// connection is failed closed: a partial record on the wire cannot be
// coherently retried — the buffer still holds the full record, so a retry
// would re-send the bytes already on the wire — and the layer is disabled
// and the connection closed so later writes fail instead of appending to
// the corrupt stream. A short write with a nil error is converted to
// io.ErrShortWrite so the caller never mistakes a closed connection for
// success. The buffer is not advanced here; callers advance it past the
// record only after this returns nil.
func (self *ResilientTlsConn) writeRecord(w io.Writer, record []byte) error {
	n, err := w.Write(record)
	if err == nil && n == len(record) {
		return nil
	}
	if err == nil {
		err = io.ErrShortWrite
	}
	self.failConnection()
	return err
}

// Write sends b over the underlying connection. While enabled, TLS records
// are intercepted before the write: handshake records (content type 22) that
// carry a server name are fragmented only when fragment is set, and sent
// with alternating socket TTLs only when reorder is set and the underlying
// connection is a *net.TCPConn; every other record is flushed as-is. On
// success Write returns len(b), nil even when part of b remains buffered
// awaiting a complete record; a failure flushing a buffered record drops
// the buffer, disables the layer, and closes the connection (see
// writeRecord), so a later retry cannot append to a corrupt stream. When
// disabled, Write forwards directly to the underlying connection.
func (self *ResilientTlsConn) Write(b []byte) (int, error) {
	if self.enabled {
		self.buffer = append(self.buffer, b...)
		for 5 <= len(self.buffer) {
			tlsHeader := parseTlsHeader(self.buffer[0:5])
			if 5+int(tlsHeader.contentLength) <= len(self.buffer) {
				if tlsHeader.contentType == 22 {
					// handshake
					handshakeBytes := self.buffer[5 : 5+tlsHeader.contentLength]
					clientHello, meta := UnmarshalClientHello(handshakeBytes)
					if clientHello != nil && clientHello.Info.ServerName != nil {
						// send the server name one character at a time
						// for each fragment, alternate the ttl of the connection to force retransmits and out-of-order arrival

						// initialSplitLen := mathrand.Intn((meta.ServerNameValueEnd+meta.ServerNameValueStart)/2-meta.ServerNameValueStart)
						split := meta.ServerNameValueStart + mathrand.Intn((meta.ServerNameValueEnd+meta.ServerNameValueStart)/2-meta.ServerNameValueStart)
						step := 1 + mathrand.Intn(meta.ServerNameValueEnd-split)
						blockSize := 64

						// a fragment write failed after earlier fragments of this
						// record were already sent: the peer has part of the
						// record while the buffer still holds all of it. The
						// record cannot be coherently retried — resending from
						// the start would duplicate the fragments already on
						// the wire. Fail the connection: drop the buffered
						// record, disable the layer, and close the connection
						// so a later retry cannot append to the corrupt
						// stream. (Handled by writeRecord below.)

						if tcpConn, ok := self.conn.(*net.TCPConn); ok {

							if self.fragment && self.reorder {
								tcpConn.SetNoDelay(true)

								ttlCtl, err := newTtlControl(tcpConn)
								if err != nil {
									return 0, err
								}

								nativeTtl := ttlCtl.get()
								if nativeTtl <= 0 {
									// syscall failed or returned a value we can't safely restore
									// (setting back to 0 would drop all packets at the first hop)
									record := tlsHeader.reconstruct(handshakeBytes)
									if err := self.writeRecord(tcpConn, record); err != nil {
										return 0, err
									}
									self.buffer = self.buffer[5+tlsHeader.contentLength:]
									continue
								}
								// restore the TTL on every exit after this point.
								// a fragment-write failure closes the connection,
								// so the restore is a no-op there; it matters on
								// the paths that hand the connection back usable
								defer ttlCtl.set(nativeTtl)

								// fmt.Printf("native ttl=%d, server name start=%d, end=%d\n", nativeTtl, meta.ServerNameValueStart, meta.ServerNameValueEnd)

								ttlCtl.set(0)
								record := tlsHeader.reconstruct(handshakeBytes[0:split])
								if err := self.writeRecord(tcpConn, record); err != nil {
									return 0, err
								}
								// fmt.Printf("frag ttl=0\n")

								for i := split; i < meta.ServerNameValueEnd; i += step {
									var ttl int
									if 0 == mathrand.Intn(2) {
										ttl = 0
									} else {
										ttl = nativeTtl
									}
									ttlCtl.set(ttl)
									record := tlsHeader.reconstruct(handshakeBytes[i:min(i+step, meta.ServerNameValueEnd)])
									if err := self.writeRecord(tcpConn, record); err != nil {
										return 0, err
									}
									// fmt.Printf("frag ttl=%d\n", ttl)
								}

								ttlCtl.set(nativeTtl)

								tailRecord := tlsHeader.reconstruct(handshakeBytes[meta.ServerNameValueEnd:])
								if err := self.writeRecord(tcpConn, tailRecord); err != nil {
									return 0, err
								}
								// fmt.Printf("frag ttl=%d\n", nativeTtl)
							} else if self.fragment {

								record := tlsHeader.reconstruct(handshakeBytes[0:split])
								if err := self.writeRecord(tcpConn, record); err != nil {
									return 0, err
								}

								for i := split; i < meta.ServerNameValueEnd; i += step {
									record := tlsHeader.reconstruct(handshakeBytes[i:min(i+step, meta.ServerNameValueEnd)])
									if err := self.writeRecord(tcpConn, record); err != nil {
										return 0, err
									}
								}

								record = tlsHeader.reconstruct(handshakeBytes[meta.ServerNameValueEnd:])
								if err := self.writeRecord(tcpConn, record); err != nil {
									return 0, err
								}

							} else if self.reorder {

								tlsBytes := tlsHeader.reconstruct(handshakeBytes)

								tcpConn.SetNoDelay(true)

								ttlCtl, err := newTtlControl(tcpConn)
								if err != nil {
									return 0, err
								}

								nativeTtl := ttlCtl.get()
								if nativeTtl <= 0 {
									// syscall failed; fall back to a single write
									if err := self.writeRecord(tcpConn, tlsBytes); err != nil {
										return 0, err
									}
									self.buffer = self.buffer[5+tlsHeader.contentLength:]
									continue
								}
								// restore the TTL on every exit after this point.
								// a block-write failure closes the connection, so
								// the restore is a no-op there; it matters on the
								// paths that hand the connection back usable
								defer ttlCtl.set(nativeTtl)

								for i := 0; i*blockSize < len(tlsBytes); i += 1 {
									var ttl int
									if 0 == i%2 {
										ttl = 0
									} else {
										ttl = nativeTtl
									}
									ttlCtl.set(ttl)
									b := tlsBytes[i*blockSize : min((i+1)*blockSize, len(tlsBytes))]
									if err := self.writeRecord(tcpConn, b); err != nil {
										return 0, err
									}
								}

								ttlCtl.set(nativeTtl)

							} else {
								record := tlsHeader.reconstruct(handshakeBytes)
								if err := self.writeRecord(tcpConn, record); err != nil {
									return 0, err
								}
							}

						} else {

							if self.fragment {
								record := tlsHeader.reconstruct(handshakeBytes[0:split])
								if err := self.writeRecord(self.conn, record); err != nil {
									return 0, err
								}

								for i := split; i < meta.ServerNameValueEnd; i += step {
									record := tlsHeader.reconstruct(handshakeBytes[i:min(i+step, meta.ServerNameValueEnd)])
									if err := self.writeRecord(self.conn, record); err != nil {
										return 0, err
									}
								}

								record = tlsHeader.reconstruct(handshakeBytes[meta.ServerNameValueEnd:])
								if err := self.writeRecord(self.conn, record); err != nil {
									return 0, err
								}
							} else {
								record := tlsHeader.reconstruct(handshakeBytes)
								if err := self.writeRecord(self.conn, record); err != nil {
									return 0, err
								}
							}

						}

					} else {
						// flush the raw record; a short or failed write leaves a
						// partial record on the wire, so writeRecord fails closed
						if err := self.writeRecord(self.conn, self.buffer[0:5+tlsHeader.contentLength]); err != nil {
							return 0, err
						}
					}
				} else {
					// flush the raw record; a short or failed write leaves a
					// partial record on the wire, so writeRecord fails closed
					if err := self.writeRecord(self.conn, self.buffer[0:5+tlsHeader.contentLength]); err != nil {
						return 0, err
					}
				}

				self.buffer = self.buffer[5+tlsHeader.contentLength:]
			} else {
				break
			}
		}
		return len(b), nil
	} else {
		return self.conn.Write(b)
	}
}

// Read forwards to the underlying connection; the resilient layer only
// transforms writes, never reads.
func (self *ResilientTlsConn) Read(b []byte) (int, error) {
	return self.conn.Read(b)
}

// Close closes the underlying connection, completing the net.Conn
// implementation that tls.Client runs over.
func (self *ResilientTlsConn) Close() error {
	return self.conn.Close()
}

// LocalAddr returns the underlying connection's local address.
func (self *ResilientTlsConn) LocalAddr() net.Addr {
	return self.conn.LocalAddr()
}

// RemoteAddr returns the underlying connection's remote address.
func (self *ResilientTlsConn) RemoteAddr() net.Addr {
	return self.conn.RemoteAddr()
}

// SetDeadline forwards the deadline to the underlying connection.
func (self *ResilientTlsConn) SetDeadline(t time.Time) error {
	return self.conn.SetDeadline(t)
}

// SetReadDeadline forwards the read deadline to the underlying connection.
func (self *ResilientTlsConn) SetReadDeadline(t time.Time) error {
	return self.conn.SetReadDeadline(t)
}

// SetWriteDeadline forwards the write deadline to the underlying connection.
func (self *ResilientTlsConn) SetWriteDeadline(t time.Time) error {
	return self.conn.SetWriteDeadline(t)
}

type tlsHeader struct {
	contentType   byte
	tlsVersion    uint16
	contentLength uint16
}

func parseTlsHeader(b []byte) *tlsHeader {
	return &tlsHeader{
		contentType:   b[0],
		tlsVersion:    binary.BigEndian.Uint16(b[1:3]),
		contentLength: binary.BigEndian.Uint16(b[3:5]),
	}
}

// reconstruct frames content as a standalone TLS record: it allocates a
// 5+len(content) slice carrying this record's content type and version with
// the length field recomputed from len(content). Used to re-frame the
// client-hello fragments that Write emits.
func (self *tlsHeader) reconstruct(content []byte) []byte {
	b := make([]byte, 5+len(content))
	b[0] = self.contentType
	binary.BigEndian.PutUint16(b[1:3], self.tlsVersion)
	binary.BigEndian.PutUint16(b[3:5], uint16(len(content)))
	copy(b[5:5+len(content)], content)
	return b
}

// https://github.com/AGWA/tlshacks/blob/main/client_hello.go

type clientHelloMeta struct {
	ServerNameValueStart int
	ServerNameValueEnd   int
}

func UnmarshalClientHello(handshakeBytes []byte) (*tlshacks.ClientHelloInfo, *clientHelloMeta) {
	info := &tlshacks.ClientHelloInfo{Raw: handshakeBytes}
	meta := &clientHelloMeta{}
	handshakeMessage := cryptobyte.String(handshakeBytes)

	handshakeMessageLength := len(handshakeMessage)

	var messageType uint8
	if !handshakeMessage.ReadUint8(&messageType) || messageType != 1 {
		// fmt.Printf("hello 1\n")
		return nil, nil
	}

	handshakeStart := handshakeMessageLength - len(handshakeMessage)

	var clientHello cryptobyte.String
	if !handshakeMessage.ReadUint24LengthPrefixed(&clientHello) || !handshakeMessage.Empty() {
		// fmt.Printf("hello 2\n")
		return nil, nil
	}

	clientHelloLength := len(clientHello)

	if !clientHello.ReadUint16((*uint16)(&info.Version)) {
		// fmt.Printf("hello 3\n")
		return nil, nil
	}

	if !clientHello.ReadBytes(&info.Random, 32) {
		// fmt.Printf("hello 4\n")
		return nil, nil
	}

	if !clientHello.ReadUint8LengthPrefixed((*cryptobyte.String)(&info.SessionID)) {
		// fmt.Printf("hello 5\n")
		return nil, nil
	}

	var cipherSuites cryptobyte.String
	if !clientHello.ReadUint16LengthPrefixed(&cipherSuites) {
		// fmt.Printf("hello 6\n")
		return nil, nil
	}
	info.CipherSuites = []tlshacks.CipherSuite{}
	for !cipherSuites.Empty() {
		var suite uint16
		if !cipherSuites.ReadUint16(&suite) {
			// fmt.Printf("hello 7\n")
			return nil, nil
		}
		info.CipherSuites = append(info.CipherSuites, tlshacks.MakeCipherSuite(suite))
	}

	var compressionMethods cryptobyte.String
	if !clientHello.ReadUint8LengthPrefixed(&compressionMethods) {
		// fmt.Printf("hello 8\n")
		return nil, nil
	}
	info.CompressionMethods = []tlshacks.CompressionMethod{}
	for !compressionMethods.Empty() {
		var method uint8
		if !compressionMethods.ReadUint8(&method) {
			// fmt.Printf("hello 9\n")
			return nil, nil
		}
		info.CompressionMethods = append(info.CompressionMethods, tlshacks.CompressionMethod(method))
	}

	info.Extensions = []tlshacks.Extension{}

	if clientHello.Empty() {
		// fmt.Printf("hello 10\n")
		return info, meta
	}

	clientHelloStart := clientHelloLength - len(clientHello)

	var extensions cryptobyte.String
	if !clientHello.ReadUint16LengthPrefixed(&extensions) {
		// fmt.Printf("hello 11\n")
		return nil, nil
	}
	extensionsLength := len(extensions)

	extensionParsers := map[uint16]func([]byte) tlshacks.ExtensionData{
		0:  tlshacks.ParseServerNameData,
		10: tlshacks.ParseSupportedGroupsData,
		11: tlshacks.ParseECPointFormatsData,
		16: tlshacks.ParseALPNData,
		18: tlshacks.ParseEmptyExtensionData,
		22: tlshacks.ParseEmptyExtensionData,
		23: tlshacks.ParseEmptyExtensionData,
		49: tlshacks.ParseEmptyExtensionData,
	}

	for !extensions.Empty() {
		var extType uint16
		var extData cryptobyte.String

		start := extensionsLength - len(extensions)
		if !extensions.ReadUint16(&extType) || !extensions.ReadUint16LengthPrefixed(&extData) {
			// fmt.Printf("hello 12\n")
			return nil, nil
		}
		end := extensionsLength - len(extensions)

		parseData := extensionParsers[extType]
		if parseData == nil {
			parseData = tlshacks.ParseUnknownExtensionData
		}
		data := parseData(extData)

		info.Extensions = append(info.Extensions, tlshacks.Extension{
			Type:    extType,
			Name:    tlshacks.Extensions[extType].Name,
			Grease:  tlshacks.Extensions[extType].Grease,
			Private: tlshacks.Extensions[extType].Private,
			Data:    data,
		})

		switch extType {
		case 0:
			info.Info.ServerName = &data.(*tlshacks.ServerNameData).HostName
			meta.ServerNameValueStart = handshakeStart + clientHelloStart + start
			meta.ServerNameValueEnd = handshakeStart + clientHelloStart + end
		case 16:
			info.Info.Protocols = data.(*tlshacks.ALPNData).Protocols
		case 18:
			info.Info.SCTs = true
		}

	}

	if !clientHello.Empty() {
		// fmt.Printf("hello 13\n")
		return nil, nil
	}

	info.Info.JA3String = tlshacks.JA3String(info)
	info.Info.JA3Fingerprint = tlshacks.JA3Fingerprint(info.Info.JA3String)

	// fmt.Printf("hello 14\n")
	return info, meta
}
