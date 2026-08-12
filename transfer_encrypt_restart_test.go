package connect

import (
	"errors"
	"testing"
	"time"
)

// TestRestartHandshakeRetriesFailedEpoch verifies the fix for the stuck-epoch
// review HIGH: a FAILED epoch (handshakeErr set, e.g. the handshakeTimeoutWatcher
// firing after TlsTimeout against a departed peer) is dead, not in flight, and
// restartHandshake must rebuild it. Previously the guard only checked whether
// an epoch object existed and was not yet established, so a failed epoch was
// treated like an in-flight one and never replaced — a parked Required-mode
// send polled restartHandshake forever with no second attempt.
func TestRestartHandshakeRetriesFailedEpoch(t *testing.T) {
	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleClient)
	defer cleanup()

	failed := injectTestEpoch(sess, true, errors.New("tls handshake timeout after 60s"))
	if sess.currentEpoch() != failed {
		t.Fatal("precondition: injected failed epoch must be current")
	}

	sess.restartHandshake()

	if sess.currentEpoch() == failed {
		t.Fatal("restartHandshake must replace a failed epoch with a fresh one")
	}
	// The replacement must be a genuinely in-flight epoch (no error yet).
	if e := sess.currentEpoch(); e == nil || e.handshakeErr != nil || e.identityFailed {
		t.Fatal("replacement epoch must be a fresh in-flight handshake")
	}
}

// TestRestartHandshakePacesFailedEpochRebuilds verifies the rebuild pacing: a
// fast-failing peer must not spawn a new handshake on every poll tick or sealed
// write. The first rebuild after a failure is immediate; later rebuilds are
// bounded by restartHandshakeCooldown.
func TestRestartHandshakePacesFailedEpochRebuilds(t *testing.T) {
	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleClient)
	defer cleanup()

	// First rebuild: immediate (lastHandshakeFailureTime is zero).
	failed1 := injectTestEpoch(sess, true, errors.New("fast fail"))
	sess.restartHandshake()
	if sess.currentEpoch() == failed1 {
		t.Fatal("first rebuild after a failure must be immediate")
	}

	// Simulate the replacement failing fast: inject another failed epoch and
	// mark the failure time as fresh, then verify the rebuild is paced.
	failed2 := injectTestEpoch(sess, true, errors.New("fast fail again"))
	sess.lastHandshakeFailureTime = time.Now()

	sess.restartHandshake()
	if sess.currentEpoch() != failed2 {
		t.Fatal("rebuild must be paced: a fresh failure within the cooldown must not be rebuilt yet")
	}

	// After the cooldown elapses, the rebuild is allowed.
	sess.lastHandshakeFailureTime = time.Now().Add(-restartHandshakeCooldown - time.Second)
	sess.restartHandshake()
	if sess.currentEpoch() == failed2 {
		t.Fatal("after the cooldown a failed epoch must be rebuilt")
	}
}

// TestRestartHandshakeLeavesInFlightEpochAlone pins the original intent: a
// genuinely in-flight handshake (no error yet) is not replaced by repeated
// restartHandshake calls.
func TestRestartHandshakeLeavesInFlightEpochAlone(t *testing.T) {
	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleClient)
	defer cleanup()

	inFlight := injectTestEpoch(sess, false, nil)
	for i := 0; i < 5; i++ {
		sess.restartHandshake()
	}
	if sess.currentEpoch() != inFlight {
		t.Fatal("an in-flight handshake must not be replaced by restartHandshake")
	}
}
