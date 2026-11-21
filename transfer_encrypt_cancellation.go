// First-cancellation attribution for peer TLS establishment.
package connect

import (
	"context"
	"sync/atomic"
)

// A private context cause names one fixed owner. Counters span the process and
// keep only the wire role and companion flag, never peers or epochs. A
// coalesced "handshake failed: context canceled" line cannot say who cancelled
// the epoch; these counters can.
type tlsCancellationOwner struct {
	name   string
	counts [2][2]atomic.Uint64
}

// epochHandshakeContext is the context one handshake epoch's workers run on.
// Cancelling it retires that epoch, which is what a superseded or failed
// handshake needs to report.
func epochHandshakeContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancelCause := context.WithCancelCause(parent)
	return ctx, func() { cancelCause(tlsEpochRetired) }
}

var (
	tlsParentLifetime         = &tlsCancellationOwner{name: "parent_lifetime"}
	tlsSessionRetired         = &tlsCancellationOwner{name: "session_retired"}
	tlsControlEnqueueRejected = &tlsCancellationOwner{name: "control_enqueue_rejected"}
	tlsEpochRetired           = &tlsCancellationOwner{name: "epoch_retired"}
	tlsCancellationUnknown    = &tlsCancellationOwner{name: "unknown"}

	tlsCancellationOwners = [...]*tlsCancellationOwner{
		tlsParentLifetime,
		tlsSessionRetired,
		tlsControlEnqueueRejected,
		tlsEpochRetired,
		tlsCancellationUnknown,
	}
)

// Keeps the cancellation text stable for callers that print the cause.
func (self *tlsCancellationOwner) Error() string { return context.Canceled.Error() }

// Keeps `errors.Is(cause, context.Canceled)` true for callers that match the
// cause chain.
func (self *tlsCancellationOwner) Unwrap() error { return context.Canceled }

// Local cancellations carry one of the fixed owners above. An inherited cause
// may hold private application data, so it is reported as the parent lifetime
// without exposing it. A live context has no known owner.
func tlsHandshakeCancellationOwner(ctx context.Context) *tlsCancellationOwner {
	if ctx == nil || ctx.Err() != context.Canceled {
		return tlsCancellationUnknown
	}
	if owner, ok := context.Cause(ctx).(*tlsCancellationOwner); ok {
		return owner
	}
	return tlsParentLifetime
}

// Counts once per current-epoch handshake that failed as cancelled, whether or
// not the shared failure line was throttled. An unknown role is a synthetic
// epoch with no wire peer, so it is not counted.
func (self *tlsCancellationOwner) record(role sequenceTlsRole, companion bool) {
	if role != sequenceTlsRoleClient && role != sequenceTlsRoleServer {
		return
	}
	companionIndex := 0
	if companion {
		companionIndex = 1
	}
	self.counts[role][companionIndex].Add(1)
}

// A cumulative process count of context-canceled completions of a current
// handshake epoch. Each field has a fixed domain.
type TlsHandshakeCancellationCount struct {
	Source    string
	Role      string
	Companion bool
	Count     uint64
}

// Snapshots every cell, including observed zero counters. Each cell is atomic,
// so a cancellation during a scrape may appear on the following one.
func GetTlsHandshakeCancellationCounts() []TlsHandshakeCancellationCount {
	counts := make([]TlsHandshakeCancellationCount, 0, len(tlsCancellationOwners)*4)
	for _, owner := range tlsCancellationOwners {
		for role := range owner.counts {
			for companion := range owner.counts[role] {
				counts = append(counts, TlsHandshakeCancellationCount{
					Source:    owner.name,
					Role:      sequenceTlsRole(role).String(),
					Companion: companion != 0,
					Count:     owner.counts[role][companion].Load(),
				})
			}
		}
	}
	return counts
}
