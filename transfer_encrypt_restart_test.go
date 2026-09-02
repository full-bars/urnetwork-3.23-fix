package connect

import (
	"errors"
	"testing"
	"time"
)

// TestRestartHandshakeRetriesFailedEpoch verifies the fix for the stuck-epoch
// a FAILED epoch (handshakeErr set, e.g. the handshakeTimeoutWatcher
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

// TestRestartHandshakeRebuildsFailedInitialEpoch pins the cooldown-timestamp
// fix: an epoch STARTED by restartHandshake (the initial epoch, or a normal
// rekey) must not stamp lastHandshakeFailureTime, so if it fails quickly the
// next restartHandshake rebuilds it immediately. Previously the timestamp was
// stamped on every call that reached the rebuild path, so a fast failure of
// the initial epoch was wrongly suppressed by the cooldown — its first retry
// could be delayed up to restartHandshakeCooldown.
func TestRestartHandshakeRebuildsFailedInitialEpoch(t *testing.T) {
	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleClient)
	defer cleanup()

	// Start the initial epoch through restartHandshake itself (not
	// injectTestEpoch), so any timestamp stamping on the start path is
	// exercised.
	sess.restartHandshake()
	first := sess.currentEpoch()
	if first == nil {
		t.Fatal("precondition: restartHandshake must start an initial epoch")
	}

	// Mark that epoch failed, as if the handshake failed quickly (well within
	// restartHandshakeCooldown of the start).
	sess.stateLock.Lock()
	first.handshakeErr = errors.New("fast fail")
	sess.stateLock.Unlock()

	// The next restartHandshake must rebuild immediately: the initial start
	// did not stamp a failure time, so the cooldown cannot suppress the
	// first retry of a failed epoch.
	sess.restartHandshake()
	if sess.currentEpoch() == first {
		t.Fatal("restartHandshake must rebuild a failed initial epoch immediately, not pace its first retry")
	}
	if e := sess.currentEpoch(); e == nil || e.handshakeErr != nil || e.identityFailed {
		t.Fatal("replacement epoch must be a fresh in-flight handshake")
	}
}

// TestHandshakeTimeoutWatcherFreesParkedRunHandshake pins the leak-fix:
// when the watchdog fires at TlsTimeout, it must cancel the epoch's ctx so
// the parked runHandshake worker (blocked in sequenceTlsTransport.Read with
// no deadline) is freed instead of lingering until a rebuild or session
// close. completeHandshake alone only records the failure — the ctx cancel
// is what wakes the blocked Read.
func TestHandshakeTimeoutWatcherFreesParkedRunHandshake(t *testing.T) {
	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleClient)
	defer cleanup()

	// An in-flight epoch with no handshake error, as if the peer is silent
	// and the handshake never completes.
	e := injectTestEpoch(sess, false, nil)
	if e.ctx.Err() != nil {
		t.Fatal("precondition: injected epoch ctx must not be cancelled")
	}

	// Watchdog path: it should record the failure AND cancel the epoch ctx.
	// Use a short TlsTimeout so the test doesn't wait the default 2s.
	sess.settings.TlsTimeout = 50 * time.Millisecond
	sess.handshakeTimeoutWatcher(e)

	if e.handshakeErr == nil {
		t.Fatal("timeout must record a handshake error")
	}
	select {
	case <-e.ctx.Done():
		// freed — the parked runHandshake Read observes ctx.Done() and exits
	default:
		t.Fatal("epoch ctx not cancelled after timeout — runHandshake worker stays parked")
	}
}

// TestHandshakeTimeoutWatcherDoesNotCancelEstablishedEpoch pins the
// race-sibling: if the handshake completes concurrently with the timeout
// firing, completeHandshake no-ops (its done() gate) and the epoch is
// established — cancelling its ctx would tear down a live cipher. The
// watchdog must only cancel when the timeout actually recorded the failure.
func TestHandshakeTimeoutWatcherDoesNotCancelEstablishedEpoch(t *testing.T) {
	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleClient)
	defer cleanup()

	// A completed epoch (handshakeDone closed) standing in for a handshake
	// that finished just as the timeout fired.
	e := injectTestEpoch(sess, true, nil)
	if e.ctx.Err() != nil {
		t.Fatal("precondition: established epoch ctx must start uncancelled")
	}

	sess.settings.TlsTimeout = 50 * time.Millisecond
	sess.handshakeTimeoutWatcher(e)

	// The invariant: a handshake that completed must keep its ctx alive.
	select {
	case <-e.ctx.Done():
		t.Fatal("established epoch ctx was cancelled by the watchdog — live cipher torn down")
	default:
		// correct: completed handshake is left alone
	}
}
