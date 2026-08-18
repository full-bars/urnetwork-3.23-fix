package connect

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// epochRoutingSession builds a session whose current epoch is a hand-built
// one with the given epochId set, avoiding the heavyweight NewClient/send-buffer
// machinery. Returns the session and its injected current epoch.
func epochRoutingSession(t *testing.T, role sequenceTlsRole, epochId Id) (*peerEncryptionSession, *tlsHandshakeEpoch) {
	t.Helper()
	// Reuse the real session constructor so self.client / self.client.log are
	// non-nil, but never start the epoch machinery (no background pumps).
	sess, cleanup := newTestEncryptionSession(t, role)
	t.Cleanup(cleanup)
	sess.stateLock.Lock()
	if sess.epoch != nil && sess.epoch.cancel != nil {
		sess.epoch.cancel()
	}
	e := &tlsHandshakeEpoch{
		handshakeDone: make(chan struct{}),
		epochId:       epochId,
	}
	sess.epoch = e
	sess.stateLock.Unlock()
	return sess, e
}

// TestAdoptEpochIdBindsResponder verifies a responder session adopts the
// initiator's generation the first time, and rejects a conflicting one.
func TestAdoptEpochIdBindsResponder(t *testing.T) {
	sess, e := epochRoutingSession(t, sequenceTlsRoleServer, Id{})
	genA := NewId()
	genB := NewId()

	// First adoption binds.
	if !sess.adoptEpochId(e, genA) {
		t.Fatal("expected first adoption to succeed")
	}
	// Same id: ok (idempotent).
	if !sess.adoptEpochId(e, genA) {
		t.Fatal("expected same-id adoption to be accepted")
	}
	// Different id: rejected -> caller must reset onto it.
	if sess.adoptEpochId(e, genB) {
		t.Fatal("expected conflicting id to be rejected (caller resets)")
	}
	if e.epochId != genA {
		t.Fatalf("epoch id changed unexpectedly: got %s want %s", e.epochId, genA)
	}
}

// TestEpochRoutingDropsStaleGeneration verifies deliverHandshake drops bytes
// from a SUPERSEDED (older) generation rather than feeding them into the live
// TLS state, and resets onto a NEWER one.
func TestEpochRoutingDropsStaleGeneration(t *testing.T) {
	older := NewId()
	newer := NewId()

	// Session holding a NEWER generation: a straggler from an older one must
	// be dropped, not fed to TLS state and not reset the live epoch.
	sess, _ := epochRoutingSession(t, sequenceTlsRoleServer, newer)
	before := sess.currentEpoch()
	sess.deliverHandshake([]byte("stale bytes"), older)
	if sess.currentEpoch() != before {
		t.Fatal("stale generation should NOT reset the live epoch")
	}
}

// TestStaleIdentityProofDoesNotTombstone verifies a foreign-epoch identity
// proof is ignored (converged or dropped), never marking the live epoch failed.
func TestStaleIdentityProofDoesNotTombstone(t *testing.T) {
	// Mint the foreign id FIRST so it is genuinely OLDER than the live
	// epoch's id (ULIDs are time-ordered). The stale-proof routing branch
	// must ignore it outright (never evaluate, never fail).
	older := NewId()
	gen := NewId()
	sess, e := epochRoutingSession(t, sequenceTlsRoleServer, gen)
	sess.receivePeerIdentityProofForEpoch(make([]byte, ed25519.SignatureSize), older)
	if e.identityFailed {
		t.Fatal("a foreign-generation proof must not mark the epoch failed")
	}
}

// epochRoutingLiveSession is like epochRoutingSession, but the injected epoch
// carries a real ctx/cancel (via injectTestEpoch) so it is safe to exercise
// code paths that rebuild the epoch (`reset`/`convergeToPeerEpoch`, via
// `buildAndStartEpochWithLock`), which cancel the outgoing epoch's ctx.
// epochRoutingSession's bare epoch has a nil cancel and would panic there.
func epochRoutingLiveSession(t *testing.T, role sequenceTlsRole, epochId Id) (*peerEncryptionSession, *tlsHandshakeEpoch, func()) {
	t.Helper()
	sess, cleanup := newTestEncryptionSession(t, role)
	e := injectTestEpoch(sess, false, nil)
	sess.stateLock.Lock()
	e.epochId = epochId
	sess.stateLock.Unlock()
	return sess, e, cleanup
}

// TestEpochRoutingResetsOntoNewerGeneration verifies the mirror image of
// TestEpochRoutingDropsStaleGeneration: a handshake control naming a NEWER
// generation than the one currently held resets onto a fresh epoch (rather
// than being dropped or fed to the stale TLS state), and the fresh epoch
// adopts the newer generation's id.
func TestEpochRoutingResetsOntoNewerGeneration(t *testing.T) {
	older := NewId()
	newer := NewId()

	sess, e1, cleanup := epochRoutingLiveSession(t, sequenceTlsRoleServer, older)
	defer cleanup()

	sess.deliverHandshake([]byte("fresh handshake bytes"), newer)

	e2 := sess.currentEpoch()
	if e2 == e1 {
		t.Fatal("expected a newer generation to reset onto a fresh epoch")
	}
	select {
	case <-e1.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("expected the superseded epoch's ctx to be cancelled after reset")
	}
	if got := sess.epochIdOf(e2); got != newer {
		t.Fatalf("expected the fresh epoch to adopt the newer generation: got %s want %s", got, newer)
	}
}

// TestEpochRoutingLegacyControlSkipsRouting verifies a control with no
// epoch id at all (a pre-epoch/legacy peer) never engages the routing logic,
// even when the session already has a bound generation — preserving the
// pre-epoch behavior for that control, as documented on the proto field.
func TestEpochRoutingLegacyControlSkipsRouting(t *testing.T) {
	gen := NewId()
	sess, e := epochRoutingSession(t, sequenceTlsRoleServer, gen)

	sess.deliverHandshake([]byte("legacy control bytes"), Id{})

	if sess.currentEpoch() != e {
		t.Fatal("a legacy (epoch-id-less) control must not reset the current epoch")
	}
}

// TestConvergeToPeerEpochOnNewerIdentityProof verifies an identity proof
// naming a NEWER generation than the one currently held converges the
// session onto a fresh epoch (via convergeToPeerEpoch) rather than being fed
// to — or failing verification against — the superseded epoch's exporter.
func TestConvergeToPeerEpochOnNewerIdentityProof(t *testing.T) {
	older := NewId()
	newer := NewId()

	sess, e1, cleanup := epochRoutingLiveSession(t, sequenceTlsRoleServer, older)
	defer cleanup()

	// The payload's shape is irrelevant here: a generation mismatch is
	// diagnosed, and resolved, before the payload is ever inspected.
	sess.receivePeerIdentityProofForEpoch([]byte("irrelevant"), newer)

	e2 := sess.currentEpoch()
	if e2 == e1 {
		t.Fatal("expected a newer-generation identity proof to converge onto a fresh epoch")
	}
	if e1.identityFailed {
		t.Fatal("the superseded epoch must not be marked failed by a foreign-generation proof")
	}
}

// TestIdentityProofOlderGenerationIgnored verifies an identity proof naming
// an OLDER (superseded) generation than the one currently held is ignored
// outright: no rebuild, and — critically — the payload is never evaluated,
// so even a malformed one cannot tombstone the live epoch.
func TestIdentityProofOlderGenerationIgnored(t *testing.T) {
	older := NewId()
	newer := NewId()

	sess, e := epochRoutingSession(t, sequenceTlsRoleServer, newer)
	before := sess.currentEpoch()

	// A malformed-length payload would normally be recorded as a failure;
	// a stale generation must never reach that check.
	sess.receivePeerIdentityProofForEpoch([]byte("short"), older)

	if sess.currentEpoch() != before {
		t.Fatal("an older generation's proof must not trigger a rebuild")
	}
	if e.identityFailed {
		t.Fatal("an older generation's proof must never be evaluated")
	}
}

// TestAdoptEpochIdNoopCases covers adoptEpochId's two trivial-success paths:
// a nil epoch (nothing to bind) and a zero epoch id (a legacy control),
// neither of which should touch an existing binding.
func TestAdoptEpochIdNoopCases(t *testing.T) {
	gen := NewId()
	sess, e := epochRoutingSession(t, sequenceTlsRoleServer, gen)

	if !sess.adoptEpochId(nil, NewId()) {
		t.Fatal("expected adoptEpochId(nil, ...) to be a trivial success")
	}
	if !sess.adoptEpochId(e, Id{}) {
		t.Fatal("expected a zero epoch id to be accepted as a no-op")
	}
	if e.epochId != gen {
		t.Fatalf("adoptEpochId with a zero id must not change the existing binding: got %s want %s", e.epochId, gen)
	}
}

// TestEpochIdOfNilEpoch verifies epochIdOf's nil-epoch guard.
func TestEpochIdOfNilEpoch(t *testing.T) {
	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleServer)
	defer cleanup()
	if got := sess.epochIdOf(nil); got != (Id{}) {
		t.Fatalf("expected epochIdOf(nil) to return the zero id, got %s", got)
	}
}

// TestBuildAndStartEpochMintsIdForClientRoleOnly verifies
// buildAndStartEpochWithLock's role split: the TLS-client role mints a
// non-zero wire identity for the generation at construction, while the
// TLS-server role leaves it zero until it adopts one from the peer.
func TestBuildAndStartEpochMintsIdForClientRoleOnly(t *testing.T) {
	clientSess, clientCleanup := newTestEncryptionSession(t, sequenceTlsRoleClient)
	defer clientCleanup()
	clientSess.startEpoch()
	ce := clientSess.currentEpoch()
	if ce == nil {
		t.Fatal("expected startEpoch to build a client-role epoch")
	}
	if clientSess.epochIdOf(ce) == (Id{}) {
		t.Fatal("expected the TLS-client role to mint a non-zero epoch id at construction")
	}

	serverSess, serverCleanup := newTestEncryptionSession(t, sequenceTlsRoleServer)
	defer serverCleanup()
	serverSess.startEpoch()
	se := serverSess.currentEpoch()
	if se == nil {
		t.Fatal("expected startEpoch to build a server-role epoch")
	}
	if serverSess.epochIdOf(se) != (Id{}) {
		t.Fatal("expected the TLS-server role to leave the epoch id unset until it adopts one")
	}
}

// TestDeliverEncryptedControlParsesEpochId exercises DeliverEncryptedControl's
// new epoch_id parsing directly (rather than calling
// receivePeerIdentityProofForEpoch/deliverHandshake with an already-parsed
// Id), confirming a same-generation proof delivered over the wire behaves
// like the direct-call case.
func TestDeliverEncryptedControlParsesEpochId(t *testing.T) {
	gen := NewId()
	sess, e := epochRoutingSession(t, sequenceTlsRoleServer, gen)

	ec := &protocol.EncryptedControl{
		ControlType: protocol.EncryptedControlType_EncryptedControlIdentityProof,
		Payload:     make([]byte, ed25519.SignatureSize),
		EpochId:     gen.Bytes(),
	}
	sess.DeliverEncryptedControl(ec)

	if e.identityFailed {
		t.Fatal("a same-generation proof delivered via DeliverEncryptedControl must not fail the epoch")
	}
}

// TestDeliverEncryptedControlMalformedEpochIdFallsBackToLegacy verifies an
// epoch_id that fails to parse (wrong length) is treated as unset — the
// legacy, pre-epoch behavior — rather than propagating a parse error or
// panicking.
func TestDeliverEncryptedControlMalformedEpochIdFallsBackToLegacy(t *testing.T) {
	gen := NewId()
	sess, _ := epochRoutingSession(t, sequenceTlsRoleServer, gen)
	before := sess.currentEpoch()

	ec := &protocol.EncryptedControl{
		ControlType: protocol.EncryptedControlType_EncryptedControlHandshake,
		Payload:     []byte("not a client hello"),
		EpochId:     []byte{1, 2, 3}, // wrong length: IdFromBytes fails, treated as unset
	}
	sess.DeliverEncryptedControl(ec)

	if sess.currentEpoch() != before {
		t.Fatal("a malformed epoch id must fall back to legacy (no-routing) behavior, not reset")
	}
}

// TestDeliverEncryptedControlResetsOntoNewerGeneration is the end-to-end,
// wire-format counterpart of TestEpochRoutingResetsOntoNewerGeneration: a
// handshake control arriving through the public DeliverEncryptedControl
// entry point, naming a newer generation, resets onto a fresh epoch that
// adopts it.
func TestDeliverEncryptedControlResetsOntoNewerGeneration(t *testing.T) {
	older := NewId()
	newer := NewId()

	sess, e1, cleanup := epochRoutingLiveSession(t, sequenceTlsRoleServer, older)
	defer cleanup()

	ec := &protocol.EncryptedControl{
		ControlType: protocol.EncryptedControlType_EncryptedControlHandshake,
		Payload:     []byte("fresh handshake bytes"),
		EpochId:     newer.Bytes(),
	}
	sess.DeliverEncryptedControl(ec)

	e2 := sess.currentEpoch()
	if e2 == e1 {
		t.Fatal("expected a newer generation delivered over the wire to reset onto a fresh epoch")
	}
	if got := sess.epochIdOf(e2); got != newer {
		t.Fatalf("expected the fresh epoch to adopt the newer generation: got %s want %s", got, newer)
	}
}

// TestDeliverEncryptedControlMalformedEpochIdRejected verifies a control with
// a nonempty epoch_id that fails to decode (or decodes to zero) is rejected
// outright, never downgraded to the legacy path where it could feed foreign
// bytes into the live TLS state.
func TestDeliverEncryptedControlMalformedEpochIdRejected(t *testing.T) {
	sess, e := epochRoutingSession(t, sequenceTlsRoleServer, NewId())
	// Give the session a real epoch with a transport so we can observe whether
	// the payload is delivered. DeliverEncryptedControl with a nonempty
	// undecodable epoch_id must drop the WHOLE control: the payload must
	// never reach the transport inbox.
	transport := newSequenceTlsTransport(context.Background())
	e.transport = transport
	sess.epoch = e

	sess.DeliverEncryptedControl(&protocol.EncryptedControl{
		ControlType: protocol.EncryptedControlType_EncryptedControlHandshake,
		Payload:     []byte("foreign bytes"),
		EpochId:     []byte{0xff, 0xfe}, // not a valid ulid
	})

	transport.inboxLock.Lock()
	inboxLen := len(transport.inboxBuf)
	transport.inboxLock.Unlock()
	if inboxLen != 0 {
		t.Fatalf("malformed epoch_id control's payload reached the transport (inbox %d bytes); must be dropped", inboxLen)
	}
	if sess.currentEpoch() != e {
		t.Fatal("malformed epoch_id control must not reset the epoch")
	}
}
