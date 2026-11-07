package connect

import (
	"bytes"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"time"
)

// Activation pacing: a working extender stops posting while its signed record is
// valid and renews before the record expires, and what the operator's budget said
// survives a restart.
//
// Until this, a success scheduled the next post a fixed ActivateTimeout away,
// whatever the record's expiry said, and every state lived in memory: a restarted
// process posted every family at once, which spent the operator's activation budget
// (six an hour per account) on hosts that were already activated, and a refusal
// that named a retry_after was forgotten with the process.
//
// What is kept, per schedule key (the ip version, or 0 for the plain api url):
//
//	State                 how the last attempt ended
//	RecordExpireTime      when the signed record the operator issued expires
//	LastAttemptAt         when the last post that reached the operator was made
//	NextAllowedAttemptAt  no post before this
//
// A success sets the next attempt to the renewal point of the verified record (renewalTime). A
// renewal is a post like any other: it takes the same hold on a refusal or a failure,
// and the record stays standing until it expires. A budget refusal and a failed
// renewal are a hold (Hold): neither the network change nor the address trigger posts
// through it. Persisted the moment it is set (SavePacing), and read at start, when no
// family posts before its NextAllowedAttemptAt.

// How the last attempt of a family ended.
const (
	// the family has not posted
	ExtenderPacingUnactivated = "unactivated"
	// the record is valid and the last post worked
	ExtenderPacingActivated = "activated"
	// the record is valid and the last post, a renewal, was refused
	ExtenderPacingRenewalRefused = "renewal_refused"
	// the record is valid and the last post, a renewal, failed
	ExtenderPacingRenewalFailed = "renewal_failed"
	// no valid record, and the last post was refused
	ExtenderPacingRefused = "refused"
	// no valid record, and the last post failed
	ExtenderPacingFailed = "failed"
)

// The pacing of one family, which is what is stored.
type ExtenderActivationPacing struct {
	State                string    `json:"state"`
	RecordExpireTime     time.Time `json:"record_expire_time,omitempty"`
	LastAttemptAt        time.Time `json:"last_attempt_at,omitempty"`
	NextAllowedAttemptAt time.Time `json:"next_allowed_attempt_at,omitempty"`
	// the address the record was issued for. A restored family without it reads
	// the first caller address check as an address that moved and posts.
	Ip netip.Addr `json:"ip,omitempty"`
	// whether NextAllowedAttemptAt holds against the triggers too (a network
	// change, an address change), because it is the operator's budget or a failed
	// renewal. Without it only the schedule moves it.
	Hold bool `json:"hold,omitempty"`
	// the carriers and dns ports the post that made the record offered, so a carrier
	// that failed to bind and is missing from the record is still known after a
	// restart (CarriersChanged). Nil for a family that holds no record.
	PostedCarriers []string `json:"posted_carriers,omitempty"`
	PostedDnsPorts []int    `json:"posted_dns_ports,omitempty"`
}

// The record valid at `now`: the operator's record expires at its expiry, not after.
func (self ExtenderActivationPacing) RecordValid(now time.Time) bool {
	return !self.RecordExpireTime.IsZero() && now.Before(self.RecordExpireTime)
}

// Whether an attempt is allowed at `now`. The boundary is allowed: the instant
// NextAllowedAttemptAt is reached the family may post, as the schedule is due at its
// time.
func (self ExtenderActivationPacing) AttemptAllowed(now time.Time) bool {
	return !now.Before(self.NextAllowedAttemptAt)
}

// What the store holds: the pacing of every family, of one identity. A record of
// another key says nothing of this one.
type ExtenderActivationPacingRecord struct {
	PublicKey []byte                           `json:"public_key"`
	Families  map[int]ExtenderActivationPacing `json:"families"`
}

// Where the pacing survives a restart. LoadPacing runs once, off the constructor, and
// is waited for no longer than PacingLoadTimeout; SavePacing runs on the loop goroutine
// and must not block for long. A load that fails starts as a new process does; a save
// that fails is logged and the pacing stands in memory. A store that has been released
// answers both with an error.
type ExtenderActivationPacingStore interface {
	LoadPacing() (*ExtenderActivationPacingRecord, error)
	SavePacing(record *ExtenderActivationPacingRecord) error
}

// The pacing of every family, a copy. Safe to call from any goroutine.
func (self *ExtenderActivator) Pacing() map[int]ExtenderActivationPacing {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return maps.Clone(self.pacing)
}

// How long the first read of the store may take before the activator starts without
// it.
const extenderPacingLoadTimeout = 2 * time.Second

// The furthest ahead a stored attempt time is trusted at load. A saved time beyond it
// came from a clock that was ahead, and would keep the family silent for as long.
const extenderPacingMaxHorizon = 24 * time.Hour

// Closed once the store has been read (or has failed, or timed out): from then on the
// loop may post. It is closed at once with no store.
func (self *ExtenderActivator) Ready() <-chan struct{} {
	return self.pacingReady
}

// Reads the store off the constructor, which must not wait on a disk, and applies it
// once. A store that has not answered within PacingLoadTimeout is started without and
// its late answer is dropped: a post may have gone by then, so it can no longer be
// the first word on the family.
func (self *ExtenderActivator) startPacingLoad() {
	store := self.settings.PacingStore
	if store == nil {
		close(self.pacingReady)
		return
	}
	type loaded struct {
		record *ExtenderActivationPacingRecord
		err    error
	}
	// buffered so an answer nobody waits for any more does not leak its goroutine
	results := make(chan loaded, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				results <- loaded{err: fmt.Errorf("the store panicked: %v", r)}
			}
		}()
		record, err := store.LoadPacing()
		results <- loaded{record: record, err: err}
	}()
	timeout := self.settings.PacingLoadTimeout
	go func() {
		defer close(self.pacingReady)
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case result := <-results:
			self.applyPacing(result.record, result.err)
		case <-timer.C:
			self.log.Infof("🌉 [extender]activate pacing load timed out after %s, starting fresh\n", timeout)
		case <-self.ctx.Done():
		}
	}()
}

// Applies what the store held, once, before the first pass.
//
// A record of another key is dropped. A stored time too far ahead is clamped (F3). A
// stored record is judged against the directory (verifyPacing): one the directory does
// not hold is not an activation, and the family posts as a new one does.
//
// A family whose record stands is shown as activated and, because the record says
// nothing of what this process serves, posts once soon, spread over ReplacedPostJitter,
// and then paces from the renewal of that post's record. Only a hold, the operator's
// budget or a failed renewal, keeps it from posting then. A role replaced under a
// running process (PreviousState) already carries its own state and posts as
// continueFrom set it, so only a hold stands against that.
func (self *ExtenderActivator) applyPacing(record *ExtenderActivationPacingRecord, err error) {
	if err != nil {
		self.log.Infof("🌉 [extender]activate pacing load err = %s\n", activationLogError(err.Error()))
		return
	}
	if record == nil || !bytes.Equal(record.PublicKey, self.settings.PublicKey) {
		return
	}
	now := self.settings.Now()
	replaced := self.settings.PreviousState != nil
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for key, pacing := range record.Families {
		schedule, ok := self.schedules[key]
		if !ok {
			continue
		}
		if !replaced {
			pacing = clampPacing(pacing, now)
			var verdict string
			pacing, verdict = self.verifyPacing(pacing, now)
			if verdict != "" {
				self.log.Infof("🌉 [extender]activate pacing v%d cleared: %s\n", key, verdict)
			}
		}
		self.pacing[key] = pacing
		valid := pacing.RecordValid(now)
		holding := pacing.Hold && pacing.NextAllowedAttemptAt.After(now)
		switch {
		case !holding && valid && !replaced:
			postTime := now.Add(uniformJitter(self.settings.ReplacedPostJitter, self.settings.postJitterForTest))
			// the renewal is sooner than the spread says: it is the post
			if pacing.NextAllowedAttemptAt.After(now) && pacing.NextAllowedAttemptAt.Before(postTime) {
				postTime = pacing.NextAllowedAttemptAt
			}
			schedule.nextActivateTime = postTime
		case (pacing.Hold || !replaced) && schedule.nextActivateTime.Before(pacing.NextAllowedAttemptAt):
			schedule.nextActivateTime = pacing.NextAllowedAttemptAt
		}
		if valid && !replaced {
			schedule.postedCarriers = slices.Clone(pacing.PostedCarriers)
			schedule.postedDnsPorts = slices.Clone(pacing.PostedDnsPorts)
		}
		if family, ok := self.families[key]; ok && !family.Activated && valid {
			family.Activated = true
			family.ExpireTime = pacing.RecordExpireTime
			family.LastActivationTime = pacing.LastAttemptAt
			family.Ip = pacing.Ip
		}
	}
	self.changedWithLock()
}

// A stored pacing with its times brought inside what this clock can have written: an
// attempt in the future is now, and a next attempt more than extenderPacingMaxHorizon
// ahead is that far.
func clampPacing(pacing ExtenderActivationPacing, now time.Time) ExtenderActivationPacing {
	if pacing.LastAttemptAt.After(now) {
		pacing.LastAttemptAt = now
	}
	if limit := now.Add(extenderPacingMaxHorizon); pacing.NextAllowedAttemptAt.After(limit) {
		pacing.NextAllowedAttemptAt = limit
	}
	return pacing
}

// Judges a stored record against the directory. The directory holds the record the
// platform serves, so a stored activation it does not hold (missing, revoked, expired,
// another expiry, or without the address the pacing was kept for) is no activation:
// the family is cleared to unactivated and posts as a new one does. The second result
// says why, "" when nothing was cleared. A hold stays, since it is the operator's word
// on when to come back. With no directory the answer is not known, and the record
// stands: the post every restart makes re-verifies it.
func (self *ExtenderActivator) verifyPacing(pacing ExtenderActivationPacing, now time.Time) (ExtenderActivationPacing, string) {
	if !pacing.RecordValid(now) || self.settings.Directory == nil {
		return pacing, ""
	}
	reason := ""
	expire, ips, ok := self.settings.Directory.ActiveRecordOf(self.settings.PublicKey)
	listsAddress := !pacing.Ip.IsValid() || len(ips) == 0 || slices.Contains(ips, pacing.Ip)
	switch {
	case !ok:
		reason = "the directory holds no valid record of this key"
	case !listsAddress:
		reason = "the directory's record does not list the stored address"
	case expire.UnixMilli() == pacing.RecordExpireTime.UnixMilli():
		// the record this family was kept for
	case expire.After(pacing.RecordExpireTime):
		// The directory keeps one record per key, the newest. A host with both
		// families posts twice, so the family that posted first sees a LATER record
		// than the one it stored: the platform's newer signed record still lists
		// this address and outlives the stored one, so the activation stands. Adopt
		// its expiry, so the next check against the directory agrees.
		pacing.RecordExpireTime = expire
	default:
		// the directory's record ends before the one stored: the stored one is not
		// what the platform holds
		reason = "the directory's record expires at another time"
	}
	if reason == "" {
		return pacing, ""
	}
	pacing.State = ExtenderPacingUnactivated
	pacing.RecordExpireTime = time.Time{}
	pacing.Ip = netip.Addr{}
	pacing.PostedCarriers = nil
	pacing.PostedDnsPorts = nil
	if !pacing.Hold {
		pacing.NextAllowedAttemptAt = time.Time{}
	}
	return pacing, reason
}

// Puts a hold back on the schedules the triggers have made due. It runs ahead of the
// pass, after every trigger.
func (self *ExtenderActivator) applyPacingHold(now time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for key, pacing := range self.pacing {
		schedule, ok := self.schedules[key]
		if !ok || !pacing.Hold || pacing.AttemptAllowed(now) {
			continue
		}
		if schedule.nextActivateTime.Before(pacing.NextAllowedAttemptAt) {
			schedule.nextActivateTime = pacing.NextAllowedAttemptAt
		}
	}
}

// Records where one family's attempt left it, in memory (savePacing writes it). `outcome` is how
// the attempt ended; `next` is the schedule's next attempt as the outcome just set it, and
// `budgetHeld` says the operator named when it has room (retry_after_seconds).
func (self *ExtenderActivator) recordPacing(
	key int,
	now time.Time,
	outcome extenderActivateOutcome,
	next time.Time,
	budgetHeld bool,
	postedCarriers []string,
	postedDnsPorts []int,
) {
	self.stateLock.Lock()
	previous := self.pacing[key]
	pacing := ExtenderActivationPacing{
		State:                ExtenderPacingUnactivated,
		LastAttemptAt:        now,
		NextAllowedAttemptAt: next,
	}
	if outcome == extenderActivateActivated {
		pacing.State = ExtenderPacingActivated
		pacing.RecordExpireTime = self.recordExpiryWithLock(key)
		if family, ok := self.families[key]; ok {
			pacing.Ip = family.Ip
		}
		pacing.PostedCarriers = slices.Clone(postedCarriers)
		pacing.PostedDnsPorts = slices.Clone(postedDnsPorts)
	} else {
		// a failed renewal does not clear the record: it stands until it expires
		if previous.RecordValid(now) {
			pacing.RecordExpireTime = previous.RecordExpireTime
			pacing.Ip = previous.Ip
			pacing.PostedCarriers = previous.PostedCarriers
			pacing.PostedDnsPorts = previous.PostedDnsPorts
		}
		valid := pacing.RecordValid(now)
		switch {
		case outcome == extenderActivateRefused && valid:
			pacing.State = ExtenderPacingRenewalRefused
		case outcome == extenderActivateRefused:
			pacing.State = ExtenderPacingRefused
		case valid:
			pacing.State = ExtenderPacingRenewalFailed
		default:
			pacing.State = ExtenderPacingFailed
		}
		pacing.Hold = budgetHeld || (valid && outcome != extenderActivateActivated)
	}
	// no change notice of its own: the outcome that led here already moved the
	// counter, and Status reads the pacing live under the same lock
	self.pacing[key] = pacing
	self.stateLock.Unlock()
}

// The expiry of the record a family just activated with, zero when the answer named
// none. The one schedule of the plain api url answers for whichever family the
// operator named, the latest expiry of those standing.
func (self *ExtenderActivator) recordExpiryWithLock(key int) time.Time {
	if !self.fallback {
		if family, ok := self.families[key]; ok {
			return family.ExpireTime
		}
		return time.Time{}
	}
	expire := time.Time{}
	for _, family := range self.families {
		if family.Activated && expire.Before(family.ExpireTime) {
			expire = family.ExpireTime
		}
	}
	return expire
}

// Writes the pacing, now. A pass that moved none writes nothing.
func (self *ExtenderActivator) savePacing() {
	store := self.settings.PacingStore
	if store == nil {
		return
	}
	self.stateLock.Lock()
	record := &ExtenderActivationPacingRecord{
		PublicKey: slices.Clone(self.settings.PublicKey),
		Families:  maps.Clone(self.pacing),
	}
	self.stateLock.Unlock()
	if err := store.SavePacing(record); err != nil {
		self.log.Infof("🌉 [extender]activate pacing save err = %s\n", activationLogError(err.Error()))
	}
}
