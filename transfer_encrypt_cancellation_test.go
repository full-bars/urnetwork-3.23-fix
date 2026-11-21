package connect

// A coalesced "handshake failed: context canceled" line cannot say who
// cancelled the epoch. These tests pin the attribution: each local lifetime
// transition stamps its own cause, an inherited cancellation is reported as
// the parent lifetime without exposing the inherited message, and the counter
// records on the same edge as the failure line.

import (
	"context"
	"errors"
	"testing"
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

// The counter records on the same edge as the failure line: a session close
// that wakes the handshake is attributed to the session retirement, once per
// completed handshake.
func TestCoalescedHandshakeFailureAttributesOwner(t *testing.T) {
	// the failure line is throttled per reason class; let this test emit
	handshakeErrThrottle.Store("context_canceled", newLogThrottle(0))

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
	handshakeErrThrottle.Store("context_canceled", newLogThrottle(0))

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
	cancelEpoch()
	session.completeHandshake(epoch, context.Canceled)
	if got := tlsCancellationCount(tlsEpochRetired, sequenceTlsRoleClient, true); got != before+1 {
		t.Fatalf("epoch_retired count %d -> %d, want one record", before, got)
	}
	if got := tlsCancellationCount(tlsSessionRetired, sequenceTlsRoleClient, true); got != 0 {
		t.Fatalf("superseded epoch counted as session retirement: %d", got)
	}
}
