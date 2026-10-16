// One queue writer publishes sanitized immutable snapshots only after its
// existing durable acknowledgement. HTTP readers never access writer state.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/urfoundation/sn/protocol"
)

// Concurrent-safe publication has its own lifecycle. It owns no signing key,
// filesystem descriptor, repair authority or callback into the queue writer.
type claimProgressOwner struct {
	stateLock sync.Mutex
	closed    bool
	value     protocol.ClaimProgress
}

func newClaimProgressOwner(member string, pool *protocol.ClaimProgressPool) *claimProgressOwner {
	var instance [16]byte
	if _, err := rand.Read(instance[:]); err != nil {
		return nil
	}
	var declared *protocol.ClaimProgressPool
	if pool != nil && pool.Validate() == nil {
		copy := *pool
		declared = &copy
	}
	return &claimProgressOwner{value: protocol.ClaimProgress{Schema: protocol.ClaimProgressSchema, Member: member, Status: "unknown", InstanceId: hex.EncodeToString(instance[:]), StartedAt: time.Now().UTC().Format(time.RFC3339Nano), DeclaredPool: declared}}
}

func claimProgressStatus(value string) string {
	switch value {
	case "pending", "retry", "submitting", "uncertain", "finalized", "no-claim":
		return value
	}
	return "unknown"
}

func claimProgressPending(entry *ClaimQueueEntry) bool {
	return entry.Status != "finalized" && entry.Status != "no-claim"
}

// Summary storage is constant; detail holds at most the public entry bound.
type claimProgressCensus struct {
	entries                                                                       []protocol.ClaimProgressEntry
	total, unresolved, finalized, noClaim, omittedUnresolved, omittedObservations uint64
	oldest                                                                        *int64
}

// Oldest unresolved liabilities precede recent terminal epochs. The complete
// count and oldest epoch remain visible when bounded detail omits later work.
func claimProgressEntries(queue *ClaimQueue, pool *protocol.ClaimProgressPool, published time.Time) (claimProgressCensus, bool) {
	census := claimProgressCensus{}
	selected := make([]*ClaimQueueEntry, 0, protocol.MaxClaimProgressEntries)
	for _, entry := range queue.Entries {
		if entry == nil || entry.Epoch < 0 {
			return census, false
		}
		census.total++
		switch entry.Status {
		case "finalized":
			census.finalized++
		case "no-claim":
			census.noClaim++
		default:
			census.unresolved++
			if census.oldest == nil || entry.Epoch < *census.oldest {
				epoch := entry.Epoch
				census.oldest = &epoch
			}
		}
		position := sort.Search(len(selected), func(i int) bool {
			left, right := claimProgressPending(entry), claimProgressPending(selected[i])
			if left != right {
				return left
			}
			if left {
				return entry.Epoch < selected[i].Epoch
			}
			return entry.Epoch > selected[i].Epoch
		})
		if position >= protocol.MaxClaimProgressEntries {
			continue
		}
		selected = slices.Insert(selected, position, entry)
		if len(selected) > protocol.MaxClaimProgressEntries {
			selected = selected[:protocol.MaxClaimProgressEntries]
		}
	}
	census.entries = make([]protocol.ClaimProgressEntry, 0, len(selected))
	census.omittedUnresolved = census.unresolved
	for _, entry := range selected {
		if claimProgressPending(entry) {
			census.omittedUnresolved--
		}
		projected := protocol.ClaimProgressEntry{Epoch: entry.Epoch, QueueStatus: claimProgressStatus(entry.Status), ObservationStatus: "unknown", DomainStatus: "unknown"}
		observation := entry.PublicObservation
		validObservation := false
		if observation != nil && observation.Epoch == entry.Epoch && observation.Validate() == nil {
			observed, _ := time.Parse(time.RFC3339Nano, observation.ObservedAt)
			validObservation = !observed.After(published)
		}
		if validObservation {
			copy := *observation
			if observation.LeafClaimed != nil {
				value := *observation.LeafClaimed
				copy.LeafClaimed = &value
			}
			projected.Observation = &copy
			projected.ObservationStatus = "retained"
			if pool != nil && copy.EvidenceKind != "api-no-claim" {
				projected.DomainStatus = "identity"
				if *pool == copy.Pool {
					projected.DomainStatus = "match"
				}
			}
		} else if observation != nil {
			census.omittedObservations++
		}
		census.entries = append(census.entries, projected)
	}
	return census, true
}

// A failed save never calls this method. It accepts only the exact queue
// representation just acknowledged by the original descriptor/head owner.
func (self *claimProgressOwner) acknowledge(queue *ClaimQueue, digest [32]byte, omitted uint64) {
	if self == nil {
		return
	}
	var pool *protocol.ClaimProgressPool
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.value.DeclaredPool != nil {
			copy := *self.value.DeclaredPool
			pool = &copy
		}
	}()
	now := time.Now().UTC()
	census, valid := claimProgressEntries(queue, pool, now)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return
	}
	start, _ := time.Parse(time.RFC3339Nano, self.value.StartedAt)
	previous, _ := time.Parse(time.RFC3339Nano, self.value.PublishedAt)
	if !valid || self.value.Sequence == math.MaxUint64 || now.Before(start) || now.Before(previous) {
		self.value.Status = "unavailable"
		return
	}
	self.value.Sequence++
	self.value.Status = "active"
	self.value.PublishedAt = now.Format(time.RFC3339Nano)
	self.value.QueueSha256 = hex.EncodeToString(digest[:])
	self.value.Entries = census.entries
	self.value.OmittedEntries = census.total - uint64(len(census.entries))
	self.value.OmittedObservations = omitted + census.omittedObservations
	self.value.TotalEntries = census.total
	self.value.UnresolvedEntries = census.unresolved
	self.value.FinalizedEntries = census.finalized
	self.value.NoClaimEntries = census.noClaim
	self.value.OmittedUnresolvedEntries = census.omittedUnresolved
	self.value.OldestUnresolvedEpoch = census.oldest
}

func (self *claimProgressOwner) unavailable() {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.closed {
		self.value.Status = "unavailable"
	}
}
func (self *claimProgressOwner) close() {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.closed = true
	self.value.Status = "closed"
}

// The encoded bytes are owned by the caller and never share mutable entries.
func (self *claimProgressOwner) snapshot(ctx context.Context) ([]byte, bool) {
	if self == nil || ctx == nil || ctx.Err() != nil {
		return nil, false
	}
	self.stateLock.Lock()
	value := self.value
	self.stateLock.Unlock()
	// Entries and observations are immutable once published; replacement under
	// the lock never mutates their backing array or nested observation values.
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > protocol.MaxClaimProgressBytes || value.Validate() != nil || ctx.Err() != nil {
		return nil, false
	}
	return raw, value.Status == "active"
}

func (self *ClaimSwarm) serveClaimProgress(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet || request.Context().Err() != nil || len(request.URL.RawQuery) > 256 {
		http.Error(writer, "invalid claim observation request", http.StatusBadRequest)
		return
	}
	query := request.URL.Query()
	values, ok := query["id"]
	if !ok || len(query) != 1 || len(values) != 1 || len(values[0]) == 0 || len(values[0]) > 128 {
		http.Error(writer, "invalid claim member", http.StatusBadRequest)
		return
	}
	id := values[0]
	self.stateLock.Lock()
	owner, known := self.progress[id]
	self.stateLock.Unlock()
	if !known {
		http.NotFound(writer, request)
		return
	}
	raw, active := owner.snapshot(request.Context())
	if raw == nil {
		raw, _ = json.Marshal(protocol.ClaimProgress{Schema: protocol.ClaimProgressSchema, Member: id, Status: "unknown"})
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	if !active {
		writer.WriteHeader(http.StatusServiceUnavailable)
	}
	_, _ = writer.Write(raw)
}
