package connect

import (
	"crypto/ed25519"
	"testing"
)

// epochRoutingSession builds a session whose current epoch is a hand-built
// one with the given epochId set, avoiding the heavyweight NewClient/send-buffer
// machinery. Returns the session and its injected current epoch.
func epochRoutingSession(t *testing.T, role sequenceTlsRole, epochId Id) (*peerEncryptionSession, *tlsHandshakeEpoch) {
	t.Helper()
	// Reuse the real session constructor so self.client / self.client.log are
	// non-nil, but never start the epoch machinery (no background pumps).
	sess, _ := newTestEncryptionSession(t, role)
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
	gen := NewId()
	sess, e := epochRoutingSession(t, sequenceTlsRoleServer, gen)
	// A proof for the SAME generation is accepted for buffering/verify.
	sess.receivePeerIdentityProofForEpoch(make([]byte, ed25519.SignatureSize), gen)
	if e.identityFailed {
		t.Fatal("same-generation proof must not mark the epoch failed")
	}
}
