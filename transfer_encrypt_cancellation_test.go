package connect

// A coalesced "handshake failed: context canceled" line cannot say who
// cancelled the epoch. These tests pin the attribution: each local lifetime
// transition stamps its own cause, an inherited cancellation is reported as
// the parent lifetime without exposing the inherited message, and the counter
// records each current-epoch cancellation once, whether or not the shared
// failure line is throttled.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Builds a session through the production constructor so the cancellation
// wiring under test is the real one.
func newCancellationSession(t *testing.T, companion bool) *peerEncryptionSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	clientSettings := DefaultClientSettings()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
	manager := NewEncryptionSessionManager(ctx, client, client.ClientKeyManager(), clientSettings.EncryptionSettings)
	return newPeerEncryptionSession(
		ctx,
		manager,
		client,
		NewId(),
		sequenceTlsRoleClient,
		clientSettings.EncryptionSettings,
		companion,
	)
}

// Reads the current count for one cell.
func tlsCancellationCount(owner *tlsCancellationOwner, role sequenceTlsRole, companion bool) uint64 {
	for _, count := range GetTlsHandshakeCancellationCounts() {
		if count.Source == owner.name && count.Role == role.String() && count.Companion == companion {
			return count.Count
		}
	}
	return 0
}

// Each local transition stamps its own cause, and the cause stays matchable as
// a plain cancellation for existing callers.
func TestTlsCancellationOwnerNamesLocalTransitions(t *testing.T) {
	session := newCancellationSession(t, false)
	session.closeFor(tlsControlEnqueueRejected)
	if cause := context.Cause(session.ctx); cause != error(tlsControlEnqueueRejected) {
		t.Fatalf("control enqueue rejection cause = %v", cause)
	}
	if !errors.Is(context.Cause(session.ctx), context.Canceled) {
		t.Fatal("owner cause is not matchable as a cancellation")
	}

	retired := newCancellationSession(t, false)
	retired.close()
	if cause := context.Cause(retired.ctx); cause != error(tlsSessionRetired) {
		t.Fatalf("session retirement cause = %v", cause)
	}

	parentCtx, cancelParent := context.WithCancel(context.Background())
	t.Cleanup(cancelParent)
	epochCtx, cancelEpoch := epochHandshakeContext(parentCtx)
	cancelEpoch()
	if cause := context.Cause(epochCtx); cause != error(tlsEpochRetired) {
		t.Fatalf("epoch retirement cause = %v", cause)
	}

	// a live context has no owner
	if owner := tlsHandshakeCancellationOwner(context.Background()); owner != tlsCancellationUnknown {
		t.Fatalf("live context owner = %s", owner.name)
	}

	// an inherited cancellation names the parent lifetime and keeps its
	// message out of the reported source
	inherited, cancelInherited := context.WithCancel(context.Background())
	cancelInherited()
	inheritedCtx, cancelChild := epochHandshakeContext(inherited)
	defer cancelChild()
	if owner := tlsHandshakeCancellationOwner(inheritedCtx); owner != tlsParentLifetime {
		t.Fatalf("inherited cancellation owner = %s", owner.name)
	}
}

// A session close that wakes the handshake is attributed to the session
// retirement, once per completed handshake.
func TestCoalescedHandshakeFailureAttributesOwner(t *testing.T) {
	session := newCancellationSession(t, false)
	epoch := &tlsHandshakeEpoch{
		ctx:           session.ctx,
		cancel:        func() {},
		handshakeDone: make(chan struct{}),
		epochId:       NewId(),
	}
	session.stateLock.Lock()
	session.epoch = epoch
	session.stateLock.Unlock()

	before := tlsCancellationCount(tlsSessionRetired, sequenceTlsRoleClient, false)
	session.close()
	session.completeHandshake(epoch, context.Canceled)
	after := tlsCancellationCount(tlsSessionRetired, sequenceTlsRoleClient, false)
	if after != before+1 {
		t.Fatalf("session_retired count %d -> %d, want one record", before, after)
	}

	// a second completion for the same epoch is not a second transition
	session.completeHandshake(epoch, context.Canceled)
	if again := tlsCancellationCount(tlsSessionRetired, sequenceTlsRoleClient, false); again != after {
		t.Fatalf("repeated completion recorded again: %d -> %d", after, again)
	}
}

// A cancelled epoch is attributed to whichever transition owns the cause, so a
// superseded handshake is not mistaken for a retired session.
func TestSupersededEpochAttributesEpochRetirement(t *testing.T) {
	session := newCancellationSession(t, true)
	epochCtx, cancelEpoch := epochHandshakeContext(session.ctx)
	epoch := &tlsHandshakeEpoch{
		ctx:           epochCtx,
		cancel:        cancelEpoch,
		handshakeDone: make(chan struct{}),
		epochId:       NewId(),
	}
	session.stateLock.Lock()
	session.epoch = epoch
	session.stateLock.Unlock()

	before := tlsCancellationCount(tlsEpochRetired, sequenceTlsRoleClient, true)
	retiredBefore := tlsCancellationCount(tlsSessionRetired, sequenceTlsRoleClient, true)
	cancelEpoch()
	session.completeHandshake(epoch, context.Canceled)
	if got := tlsCancellationCount(tlsEpochRetired, sequenceTlsRoleClient, true); got != before+1 {
		t.Fatalf("epoch_retired count %d -> %d, want one record", before, got)
	}
	if got := tlsCancellationCount(tlsSessionRetired, sequenceTlsRoleClient, true); got != retiredBefore {
		t.Fatalf("superseded epoch counted as session retirement: %d -> %d", retiredBefore, got)
	}
}

// The failure line is throttled per reason class across every peer. A
// cancellation whose line that throttle suppresses is still counted, so a wave
// of cancellations is sized by its handshakes and not by the log cadence.
func TestTlsCancellationCountsThrottledFailure(t *testing.T) {
	previous, hadPrevious := handshakeErrThrottle.Load("context_canceled")
	t.Cleanup(func() {
		if hadPrevious {
			handshakeErrThrottle.Store("context_canceled", previous)
		} else {
			handshakeErrThrottle.Delete("context_canceled")
		}
	})
	// an hour-long window that has just emitted: every further line is suppressed
	throttle := newLogThrottle(time.Hour)
	if ok, _ := throttle.Allow(time.Now()); !ok {
		t.Fatal("fresh throttle refused its first line")
	}
	handshakeErrThrottle.Store("context_canceled", throttle)

	before := tlsCancellationCount(tlsControlEnqueueRejected, sequenceTlsRoleClient, false)
	for i := 0; i < 3; i += 1 {
		session := newCancellationSession(t, false)
		epoch := &tlsHandshakeEpoch{
			ctx:           session.ctx,
			cancel:        func() {},
			handshakeDone: make(chan struct{}),
			epochId:       NewId(),
		}
		session.stateLock.Lock()
		session.epoch = epoch
		session.stateLock.Unlock()
		session.closeFor(tlsControlEnqueueRejected)
		session.completeHandshake(epoch, context.Canceled)
	}
	if got := tlsCancellationCount(tlsControlEnqueueRejected, sequenceTlsRoleClient, false); got != before+3 {
		t.Fatalf("control_enqueue_rejected count %d -> %d, want three records", before, got)
	}
	if ok, _ := throttle.Allow(time.Now()); ok {
		t.Fatal("test throttle let a failure line through")
	}
}
