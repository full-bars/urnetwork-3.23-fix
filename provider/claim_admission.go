// One process-local relayer owns nonce reads through durable preparation,
// broadcast and finality. Waiting members continue discovering epochs; no
// goroutine mutates another member's queue. Separate processes need separate
// relayer keys or an external nonce owner; this admission is not a chain lock.
package main

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"time"
)

const (
	claimOperationTimeout = 5 * time.Minute
	claimRecentBurst      = 8
)

type claimAdmissionTicket struct {
	owner  string
	recent bool
	epoch  int64
}

type claimAdmissionWaiter struct {
	ready    chan struct{}
	notified bool
}

// Methods are concurrent-safe. At most one ticket per class per member keeps
// repeated polls bounded and preserves historical position during new arrivals.
type claimAdmission struct {
	stateLock   sync.Mutex
	owner       string
	waiting     []claimAdmissionTicket
	waiterKVs   map[string]*claimAdmissionWaiter
	recentBurst int
	latestEpoch int64
	nonceFloor  uint64
	chainId     *big.Int
	nonceError  error
}

func (self *claimAdmission) nextWithLock() int {
	recent, historical := -1, -1
	for index, ticket := range self.waiting {
		if ticket.recent && recent < 0 {
			recent = index
		}
		if !ticket.recent && historical < 0 {
			historical = index
		}
	}
	if historical >= 0 && (recent < 0 || self.recentBurst >= claimRecentBurst) {
		return historical
	}
	return recent
}

// A retained level notification cannot be lost if release precedes subscribe.
// Rearm displaced selections so an old ready signal cannot busy-spin its owner.
func (self *claimAdmission) signalWithLock() {
	selected := ""
	if self.owner == "" {
		if index := self.nextWithLock(); index >= 0 {
			selected = self.waiting[index].owner
		}
	}
	for owner, waiter := range self.waiterKVs {
		if owner == selected && !waiter.notified {
			close(waiter.ready)
			waiter.notified = true
		} else if owner != selected && waiter.notified {
			waiter.ready, waiter.notified = make(chan struct{}), false
		}
	}
}

// A shared high-water prevents a lagging member's old queue from claiming
// current priority. A new observation also reclassifies tickets already waiting.
func (self *claimAdmission) observeEpoch(epoch int64) int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if epoch > self.latestEpoch {
		self.latestEpoch = epoch
		retained := self.waiting[:0]
		for _, ticket := range self.waiting {
			ticket.recent = ticket.epoch >= epoch-1
			duplicate := false
			for index := range retained {
				if retained[index].owner == ticket.owner && retained[index].recent == ticket.recent {
					retained[index].epoch = max(retained[index].epoch, ticket.epoch)
					duplicate = true
					break
				}
			}
			if !duplicate {
				retained = append(retained, ticket)
			}
		}
		self.waiting = retained
		self.signalWithLock()
	}
	return self.latestEpoch
}

// A miss changes no durable claim state. Each offered class retains FIFO
// position until served or withdrawn; fresh work cannot replace an old ticket.
func (self *claimAdmission) acquire(ctx context.Context, owner string, candidates []claimPollCandidate) (int, bool, error) {
	if self == nil || ctx == nil || owner == "" {
		return -1, false, errors.New("claim admission is unavailable")
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if err := ctx.Err(); err != nil {
		return -1, false, err
	}
	if self.nonceError != nil {
		return -1, false, self.nonceError
	}
	if self.owner == owner {
		return -1, false, errors.New("claim admission cannot be entered twice by one owner")
	}
	recentIndex, historicalIndex := -1, -1
	for index, candidate := range candidates {
		candidates[index].recent = candidate.entry.Epoch >= self.latestEpoch-1
		if candidates[index].recent {
			if recentIndex < 0 {
				recentIndex = index
			}
		} else if historicalIndex < 0 {
			historicalIndex = index
		}
	}
	recent, historical := recentIndex >= 0, historicalIndex >= 0
	foundRecent, foundHistorical := false, false
	retained := self.waiting[:0]
	for _, ticket := range self.waiting {
		if ticket.owner == owner {
			if ticket.recent && !recent || !ticket.recent && !historical {
				continue
			}
			if ticket.recent {
				foundRecent = true
				ticket.epoch = candidates[recentIndex].entry.Epoch
			} else {
				foundHistorical = true
				ticket.epoch = candidates[historicalIndex].entry.Epoch
			}
		}
		retained = append(retained, ticket)
	}
	self.waiting = retained
	if recent && !foundRecent {
		self.waiting = append(self.waiting, claimAdmissionTicket{owner: owner, recent: true, epoch: candidates[recentIndex].entry.Epoch})
	}
	if historical && !foundHistorical {
		self.waiting = append(self.waiting, claimAdmissionTicket{owner: owner, epoch: candidates[historicalIndex].entry.Epoch})
	}
	if self.waiterKVs == nil {
		self.waiterKVs = map[string]*claimAdmissionWaiter{}
	}
	if !recent && !historical {
		delete(self.waiterKVs, owner)
	} else if self.waiterKVs[owner] == nil {
		self.waiterKVs[owner] = &claimAdmissionWaiter{ready: make(chan struct{})}
	}
	index := self.nextWithLock()
	if self.owner != "" || index < 0 || self.waiting[index].owner != owner {
		self.signalWithLock()
		return -1, false, nil
	}
	selected := self.waiting[index]
	self.waiting = append(self.waiting[:index], self.waiting[index+1:]...)
	self.owner = owner
	if selected.recent {
		self.recentBurst = min(self.recentBurst+1, claimRecentBurst)
	} else {
		self.recentBurst = 0
	}
	self.signalWithLock()
	if selected.recent {
		return recentIndex, true, nil
	}
	return historicalIndex, true, nil
}

// Only the admitted callback releases ownership, including its timeout path.
func (self *claimAdmission) release(owner string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.owner == "" || self.owner != owner {
		panic("claim admission released by a different owner")
	}
	self.owner = ""
	self.signalWithLock()
}

func (self *claimAdmission) ready(owner string) <-chan struct{} {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if waiter := self.waiterKVs[owner]; waiter != nil {
		return waiter.ready
	}
	return nil
}

// Cancellation removes only this member's waiting work. It never unlocks an
// in-flight transaction or pretends its signed outcome is known.
func (self *claimAdmission) forget(owner string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	retained := self.waiting[:0]
	for _, ticket := range self.waiting {
		if ticket.owner != owner {
			retained = append(retained, ticket)
		}
	}
	self.waiting = retained
	delete(self.waiterKVs, owner)
	self.signalWithLock()
}

// The five-minute budget starts after admission. Each selected operation owns
// reconciliation, exact nonce/sign/broadcast checkpoints and finality together.
func beginClaimOperation(ctx context.Context, admission *claimAdmission, owner string, candidates []claimPollCandidate) (int, context.Context, func(), error) {
	index, admitted, err := admission.acquire(ctx, owner, candidates)
	if err != nil || !admitted {
		return -1, nil, nil, err
	}
	operationCtx, cancel := context.WithTimeout(ctx, claimOperationTimeout)
	return index, operationCtx, func() { cancel(); admission.release(owner) }, nil
}
