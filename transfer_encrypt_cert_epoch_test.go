package connect

// Sequence TLS certificate verification belongs to one handshake epoch: the
// epoch whose cipher seals the frame. A background rekey must have its own
// peer certificate checked against the contract commitment before its cipher
// seals, and a promotion that races a send must never let one epoch's check
// stand in for another epoch's. Every epoch here completes a real TLS 1.3
// handshake with the production sequence configs, so verification reads the
// certificate the handshake actually observed.
//
// These tests drive `SendSequence.writeMaybeWrappedBytes`, the single writer
// both acknowledgement modes go through, against a bare sequence whose only
// route writer captures the framed bytes. That keeps the race windows
// deterministic: the epoch promotion happens inside a test barrier that fires
// exactly between the sealing snapshot and the use of it.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/tls"
	"encoding/pem"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Returns a fresh synthetic sequence certificate and its PEM block, the form a
// contract commits to.
func newCertEpochCertificate(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	certificate, err := generateSequenceTlsCertificate()
	if err != nil {
		t.Fatal(err)
	}
	certificatePem := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]})
	return certificate, certificatePem
}

// Completes a TLS handshake over an in-memory pipe in which the peer presents
// peerCertificate, and returns the local client end. Its ConnectionState
// carries that certificate as the peer leaf, as a completed epoch's does.
func newCertEpochTlsConn(t *testing.T, peerCertificate tls.Certificate) *tls.Conn {
	t.Helper()
	localCertificate, _ := newCertEpochCertificate(t)
	clientConfig := DefaultSequenceClientTlsConfig()
	clientConfig.Certificates = []tls.Certificate{localCertificate}
	serverConfig, err := DefaultSequenceServerTlsConfig()
	if err != nil {
		t.Fatal(err)
	}
	serverConfig.Certificates = []tls.Certificate{peerCertificate}
	localConn, peerConn := net.Pipe()
	// closing the pipe rather than the TLS ends skips a close_notify that
	// nothing reads
	t.Cleanup(func() {
		localConn.Close()
		peerConn.Close()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	client := tls.Client(localConn, clientConfig)
	server := tls.Server(peerConn, serverConfig)
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.HandshakeContext(ctx)
	}()
	if err := client.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	return client
}

// Builds a handshake epoch whose completed TLS connection observed
// peerCertificate. Each epoch has its own AEAD key, so a sealed frame names
// the epoch that sealed it.
func newCertEpoch(t *testing.T, peerCertificate tls.Certificate, keyByte byte) *tlsHandshakeEpoch {
	t.Helper()
	block, err := aes.NewCipher(bytes.Repeat([]byte{keyByte}, 32))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	return &tlsHandshakeEpoch{
		ctx:              ctx,
		cancel:           cancel,
		epochId:          NewId(),
		tlsConn:          newCertEpochTlsConn(t, peerCertificate),
		handshakeDone:    make(chan struct{}),
		derivedTlsCipher: &sequenceCipher{aead: aead},
	}
}

// Installs the trusted set and the epochs. established seals; current is the
// newest epoch, an in-flight rekey when it differs from established.
func setCertEpochSessionForTest(
	session *peerEncryptionSession,
	trustedPems [][]byte,
	established *tlsHandshakeEpoch,
	current *tlsHandshakeEpoch,
) {
	session.stateLock.Lock()
	defer session.stateLock.Unlock()
	session.trustedPeerCertPems = map[string]bool{}
	for _, trustedPem := range trustedPems {
		session.trustedPeerCertPems[string(trustedPem)] = true
	}
	session.establishedEpoch = established
	session.epoch = current
}

// Completes a rekey as a verified identity proof does: e becomes the newest
// and the established epoch, and the epoch it replaces stays for decrypt only.
func promoteCertEpochForTest(session *peerEncryptionSession, e *tlsHandshakeEpoch) {
	session.stateLock.Lock()
	defer session.stateLock.Unlock()
	session.epoch = e
	e.peerIdentityVerified = true
	session.markEstablishedWithLock(e)
}

// Reads an epoch's own verification under the session lock.
func certEpochVerified(session *peerEncryptionSession, e *tlsHandshakeEpoch) bool {
	session.stateLock.Lock()
	defer session.stateLock.Unlock()
	return e.certVerified
}

// Takes the frame a write committed to the route, if any, and returns the
// epoch whose cipher sealed it, or nil when nothing was written. Fails on
// plaintext, on a frame no known epoch opens, and on a frame sealed by an
// epoch whose own certificate was never verified (reported without stopping,
// so the test can show what that seal leads to).
func takeCertEpochWire(
	t *testing.T,
	session *peerEncryptionSession,
	route chan []byte,
	epochs ...*tlsHandshakeEpoch,
) *tlsHandshakeEpoch {
	t.Helper()
	var message []byte
	select {
	case message = <-route:
	default:
		return nil
	}
	// message is the capture writer's private copy and the opened plaintext is
	// a fresh slice: neither came from the message pool
	var outer protocol.TransferFrame
	if err := ProtoUnmarshal(message, &outer); err != nil {
		t.Fatal(err)
	}
	if len(outer.EncryptedTransferFrame) == 0 {
		t.Fatal("send with an established cipher wrote plaintext")
	}
	for _, e := range epochs {
		if _, err := e.derivedTlsCipher.Open(outer.EncryptedTransferFrame); err != nil {
			continue
		}
		if !certEpochVerified(session, e) {
			t.Errorf("frame sealed by epoch %s whose own peer certificate was never verified", e.epochId)
		}
		return e
	}
	t.Fatal("frame sealed by a cipher of no known epoch")
	return nil
}

// certEpochCaptureWriter is the sequence's only route writer: it copies each
// framed write and takes ownership of the pooled buffer, as MultiRouteWriter
// promises.
type certEpochCaptureWriter struct {
	route chan []byte
}

func (self *certEpochCaptureWriter) Write(
	ctx context.Context,
	transferFrameBytes []byte,
	timeout time.Duration,
) error {
	select {
	case self.route <- append([]byte(nil), transferFrameBytes...):
	default:
	}
	MessagePoolReturn(transferFrameBytes)
	return nil
}

func (self *certEpochCaptureWriter) WriteDetailed(
	ctx context.Context,
	transferFrameBytes []byte,
	timeout time.Duration,
) (bool, error) {
	err := self.Write(ctx, transferFrameBytes, timeout)
	return err == nil, err
}

func (self *certEpochCaptureWriter) GetActiveRoutes() []Route   { return nil }
func (self *certEpochCaptureWriter) GetInactiveRoutes() []Route { return nil }

// Builds a sequence with a session whose epochs the test controls, and a
// capture writer as its only route. The session is never registered with a
// manager, so the test owns its lifetime.
func newCertEpochWriteFixture(t *testing.T) (*SendSequence, *peerEncryptionSession, chan []byte) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	clientSettings := DefaultClientSettings()
	clientSettings.EncryptionSettings.Mode = EncryptionModeRequired
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
	sendBuffer := NewSendBuffer(ctx, client, clientSettings.SendBufferSettings)
	sequence := NewSendSequence(
		ctx,
		client,
		sendBuffer,
		DestinationId(NewId()),
		MultiHopId{},
		false,
		false,
		sequenceTlsRoleClient,
		false,
		clientSettings.SendBufferSettings,
	)
	t.Cleanup(sequence.cancel)

	route := make(chan []byte, 8)
	sequence.contractMultiRouteWriter = &certEpochCaptureWriter{route: route}
	sequence.contractMultiRouteWriterDestination = sequence.destination

	session := &peerEncryptionSession{
		ctx:      ctx,
		cancel:   cancel,
		client:   client,
		peerId:   sequence.destination.DestinationId,
		role:     sequenceTlsRoleClient,
		settings: clientSettings.EncryptionSettings,
	}
	sequence.session = session
	return sequence, session, route
}

// One send through the shared writer. Returns the error the send path raised,
// which is nil when the frame was written.
func certEpochWrite(sequence *SendSequence, label string) error {
	_, err := sequence.writeMaybeWrappedBytes([]byte(label), sequence.destination, false, false)
	return err
}

// A rekey that completes after the established epoch's certificate matched,
// but before that match is recorded, must not inherit it. Marking the newly
// established epoch at that point would let its cipher seal without its own
// certificate ever being compared to the commitment.
func TestCertVerificationMarksOnlyCheckedEpoch(t *testing.T) {
	sequence, session, route := newCertEpochWriteFixture(t)
	trusted, trustedPem := newCertEpochCertificate(t)
	untrusted, _ := newCertEpochCertificate(t)
	e1 := newCertEpoch(t, trusted, 1)
	e2 := newCertEpoch(t, untrusted, 2)
	setCertEpochSessionForTest(session, [][]byte{trustedPem}, e1, e2)
	marks := 0
	session.beforeCertVerifiedMarkForTest = func() {
		marks += 1
		if marks == 1 {
			promoteCertEpochForTest(session, e2)
		}
	}

	if err := certEpochWrite(sequence, "checked epoch"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if marks != 1 {
		t.Fatalf("send recorded %d certificate matches, want 1", marks)
	}
	if sealer := takeCertEpochWire(t, session, route, e1, e2); sealer != e1 {
		t.Fatal("first send was not sealed by the epoch it verified")
	}
	if !certEpochVerified(session, e1) || certEpochVerified(session, e2) {
		t.Errorf(
			"certificate match recorded on the wrong epoch: checked=%t promoted=%t",
			certEpochVerified(session, e1), certEpochVerified(session, e2),
		)
	}

	// the promoted epoch presents an uncommitted certificate: refused, unsent
	if err := certEpochWrite(sequence, "promoted epoch"); err == nil {
		t.Error("promoted epoch sealed without its own certificate checked")
	}
	if sealer := takeCertEpochWire(t, session, route, e1, e2); sealer != nil {
		t.Error("refused send reached the wire")
	}
}

// A background rekey keeps the verified established cipher sealing. Once the
// rekey is promoted, the next send checks the new epoch's own certificate: a
// rotated certificate the peer committed to passes and seals with the new
// cipher, and an uncommitted one fails the send with nothing written.
func TestRekeyPromotionReverifiesPeerCertificate(t *testing.T) {
	for _, rekeyCommitted := range []bool{true, false} {
		name := "uncommitted"
		if rekeyCommitted {
			name = "committed"
		}
		t.Run(name, func(t *testing.T) {
			sequence, session, route := newCertEpochWriteFixture(t)
			trusted, trustedPem := newCertEpochCertificate(t)
			rotated, rotatedPem := newCertEpochCertificate(t)
			uncommitted, _ := newCertEpochCertificate(t)
			rekeyCertificate := uncommitted
			if rekeyCommitted {
				rekeyCertificate = rotated
			}
			e1 := newCertEpoch(t, trusted, 1)
			e2 := newCertEpoch(t, rekeyCertificate, 2)
			setCertEpochSessionForTest(session, [][]byte{trustedPem, rotatedPem}, e1, e1)

			if err := certEpochWrite(sequence, "rekey"); err != nil {
				t.Fatalf("first send: %v", err)
			}
			if sealer := takeCertEpochWire(t, session, route, e1, e2); sealer != e1 {
				t.Fatal("first send was not sealed by the established epoch")
			}

			// the rekey starts; the established epoch keeps sealing through it
			session.stateLock.Lock()
			session.epoch = e2
			session.stateLock.Unlock()
			if err := certEpochWrite(sequence, "during rekey"); err != nil {
				t.Fatalf("send during the rekey: %v", err)
			}
			if sealer := takeCertEpochWire(t, session, route, e1, e2); sealer != e1 {
				t.Fatal("send during the rekey was not sealed by the established epoch")
			}
			if certEpochVerified(session, e2) {
				t.Error("in-flight rekey epoch inherited the established verification")
			}

			promoteCertEpochForTest(session, e2)
			err := certEpochWrite(sequence, "after promotion")
			sealer := takeCertEpochWire(t, session, route, e1, e2)
			if rekeyCommitted {
				if err != nil || sealer != e2 || !certEpochVerified(session, e2) {
					t.Fatalf("rotated committed certificate: err=%v sealedByRekey=%t", err, sealer == e2)
				}
			} else {
				if err == nil || sealer != nil || certEpochVerified(session, e2) {
					t.Fatalf(
						"uncommitted rekey certificate: err=%v written=%t verified=%t",
						err, sealer != nil, certEpochVerified(session, e2),
					)
				}
			}
		})
	}
}

// A promotion between taking the sealing epoch and verifying must not pair the
// newer epoch's verification with the older epoch's cipher. Here the
// established epoch's peer passed its identity proof but presents a
// certificate the contract never committed to; its cipher must never seal.
func TestSealingCipherBelongsToVerifiedEpoch(t *testing.T) {
	sequence, session, route := newCertEpochWriteFixture(t)
	trusted, trustedPem := newCertEpochCertificate(t)
	uncommitted, _ := newCertEpochCertificate(t)
	e1 := newCertEpoch(t, uncommitted, 1)
	e2 := newCertEpoch(t, trusted, 2)
	setCertEpochSessionForTest(session, [][]byte{trustedPem}, e1, e2)
	taken := 0
	session.afterSealingEpochForTest = func() {
		taken += 1
		if taken == 1 {
			promoteCertEpochForTest(session, e2)
		}
	}

	// the first write takes e1, then races the promotion: e1's own certificate
	// is uncommitted, so nothing may be sealed under its cipher
	if err := certEpochWrite(sequence, "sealing epoch"); err == nil {
		t.Error("sealed with an epoch whose own certificate was never verified")
	}
	if taken != 1 {
		t.Fatalf("send took the sealing epoch %d times, want 1", taken)
	}
	if sealer := takeCertEpochWire(t, session, route, e1, e2); sealer != nil {
		t.Error("refused send reached the wire")
	}
	if certEpochVerified(session, e1) || certEpochVerified(session, e2) {
		t.Fatalf(
			"certificate match recorded on the wrong epoch: checked=%t promoted=%t",
			certEpochVerified(session, e1), certEpochVerified(session, e2),
		)
	}

	// the promoted epoch's certificate matches, so it may seal
	if err := certEpochWrite(sequence, "after promotion"); err != nil {
		t.Fatal(err)
	}
	if sealer := takeCertEpochWire(t, session, route, e1, e2); sealer != e2 {
		t.Fatal("send after promotion was not sealed by the promoted epoch")
	}
	if certEpochVerified(session, e1) {
		t.Fatal("uncommitted certificate was recorded as verified")
	}
}

// The certificate check is cached on the epoch, not re-run on every send: an
// established epoch keeps sealing through a background rekey without repeating
// the comparison, and the in-flight epoch stays unverified until it is
// promoted and checked itself.
func TestEstablishedEpochKeepsSealingThroughRekey(t *testing.T) {
	sequence, session, route := newCertEpochWriteFixture(t)
	trusted, trustedPem := newCertEpochCertificate(t)
	rekeyCertificate, _ := newCertEpochCertificate(t)
	e1 := newCertEpoch(t, trusted, 1)
	e2 := newCertEpoch(t, rekeyCertificate, 2)
	setCertEpochSessionForTest(session, [][]byte{trustedPem}, e1, e1)
	marks := 0
	session.beforeCertVerifiedMarkForTest = func() {
		marks += 1
	}

	if err := certEpochWrite(sequence, "first"); err != nil {
		t.Fatal(err)
	}
	if sealer := takeCertEpochWire(t, session, route, e1, e2); sealer != e1 {
		t.Fatal("first send was not sealed by the established epoch")
	}

	// a rekey starts in the background; the established epoch keeps sealing
	session.stateLock.Lock()
	session.epoch = e2
	session.stateLock.Unlock()
	for i := 0; i < 3; i += 1 {
		if err := certEpochWrite(sequence, "during rekey"); err != nil {
			t.Fatalf("send %d during the rekey: %v", i, err)
		}
		if sealer := takeCertEpochWire(t, session, route, e1, e2); sealer != e1 {
			t.Fatalf("send %d during the rekey was not sealed by the established epoch", i)
		}
	}
	if marks != 1 {
		t.Fatalf("certificate matches recorded %d times, want 1 (checked once per epoch)", marks)
	}
	if certEpochVerified(session, e2) {
		t.Fatal("in-flight rekey epoch inherited the established verification")
	}
}
