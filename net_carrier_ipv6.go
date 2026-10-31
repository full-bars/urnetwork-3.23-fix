package connect

// net_carrier_ipv6.go — per-carrier IPv6 family memory for proxied dials.
//
// A SOCKS5 exit that cannot reach IPv6 only reveals it by making the caller wait
// out the dial timeout. `ipv6Available()` cannot help: it probes THIS host's
// route, and the dial that fails is made from the exit's. So the verdict is kept
// per carrier, keyed by ProxySettings.Key() (the same identity proxy health and
// earnings use), and is learned from the dials themselves.
//
// After carrierIpv6FailureThreshold consecutive explicit-v6 dial failures through
// a carrier, v6 is suppressed through THAT carrier for carrierIpv6Cooldown. A
// suppressed dial fails at once with errCarrierIpv6Suppressed, which is a
// definitive failure, so the dial race (net_dial_race.go) moves to the v4
// address without waiting. When the cooldown ends exactly one probe dial is let
// through: success clears the verdict, failure re-arms it.
//
// Scope, stated plainly: only a dial that names the family can be judged. In the
// default proxy_target_family=v4 mode the proxy is handed a v4 literal, and in
// `proxy` mode it is handed a hostname, so the family the exit picks is never
// visible here. The verdict therefore engages for explicit tcp6/udp6 networks and
// v6 literals, which is what the dial race and any forced-v6 caller produce.
// Nothing here is global: one carrier's verdict never reads or writes another's.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

const (
	// carrierIpv6FailureThreshold is the consecutive v6 failures through one
	// carrier that mark it. Three: one failure is routine (a single unreachable
	// target), two can be bad luck on a lossy exit, and each failure can cost a
	// full dial timeout, so waiting for more than three burns minutes.
	carrierIpv6FailureThreshold = 3
	// carrierIpv6Cooldown is how long a marked carrier is not offered v6. Fifteen
	// minutes: long enough that a v6-less exit costs one probe per quarter hour
	// instead of one per dial, short enough that an exit that gains v6 is
	// re-offered it within a session.
	carrierIpv6Cooldown = 15 * time.Minute
)

var errCarrierIpv6Suppressed = errors.New("ipv6 suppressed for this proxy carrier (recent v6 dials through it failed)")

type carrierIpv6Entry struct {
	// consecutive v6 failures since the last v6 success
	failures int
	// zero while not marked
	suppressedUntil time.Time
	// a probe dial is in flight after the cooldown
	probing bool
}

type carrierIpv6Table struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]*carrierIpv6Entry
	// called on a transition (mark, re-arm, clear) with a one-line description
	logf func(format string, args ...any)
}

func newCarrierIpv6Table() *carrierIpv6Table {
	return &carrierIpv6Table{
		now:     time.Now,
		entries: map[string]*carrierIpv6Entry{},
	}
}

// the process-wide table the proxied dial path consults
var carrierIpv6 = newCarrierIpv6Table()

func (self *carrierIpv6Table) log(format string, args ...any) {
	if self.logf != nil {
		self.logf(format, args...)
		return
	}
	DefaultLogger().Infof(format, args...)
}

func carrierLabel(key string) string {
	address, _ := SplitProxyKey(key)
	return address
}

// allow reports whether a v6 dial may start through the carrier. When it returns
// true with probe set, the caller holds the single probe slot and MUST report an
// outcome through done.
func (self *carrierIpv6Table) allow(key string) (ok bool, probe bool) {
	self.mu.Lock()
	defer self.mu.Unlock()
	entry := self.entries[key]
	if entry == nil || entry.suppressedUntil.IsZero() {
		return true, false
	}
	if self.now().Before(entry.suppressedUntil) || entry.probing {
		return false, false
	}
	entry.probing = true
	return true, true
}

// done records the outcome of a v6 dial that allow admitted. A neutral outcome
// (the caller cancelled, so the carrier proved nothing) only releases the probe.
func (self *carrierIpv6Table) done(key string, probe bool, err error, neutral bool) {
	self.mu.Lock()
	defer self.mu.Unlock()
	entry := self.entries[key]
	if neutral {
		if probe && entry != nil {
			entry.probing = false
		}
		return
	}
	if err == nil {
		if entry != nil {
			delete(self.entries, key)
			if !entry.suppressedUntil.IsZero() {
				self.log("[net]ipv6 carrier %s: v6 reachable again, verdict cleared\n", carrierLabel(key))
			}
		}
		return
	}
	if entry == nil {
		entry = &carrierIpv6Entry{}
		self.entries[key] = entry
	}
	entry.failures += 1
	entry.probing = false
	if entry.failures < carrierIpv6FailureThreshold {
		return
	}
	wasMarked := !entry.suppressedUntil.IsZero()
	entry.suppressedUntil = self.now().Add(carrierIpv6Cooldown)
	if probe || wasMarked {
		self.log("[net]ipv6 carrier %s: v6 probe failed, suppressed for %s\n", carrierLabel(key), carrierIpv6Cooldown)
	} else {
		self.log("[net]ipv6 carrier %s: %d consecutive v6 dial failures, v6 suppressed for %s (v4 only)\n",
			carrierLabel(key), entry.failures, carrierIpv6Cooldown)
	}
}

// noteV4Failure drops a not-yet-marked v6 streak. A carrier that is failing v4
// as well is a dead proxy, which is not evidence about its v6.
func (self *carrierIpv6Table) noteV4Failure(key string) {
	self.mu.Lock()
	defer self.mu.Unlock()
	if entry := self.entries[key]; entry != nil && entry.suppressedUntil.IsZero() {
		delete(self.entries, key)
	}
}

// CarrierIpv6Status is one marked carrier, for status output.
type CarrierIpv6Status struct {
	Carrier           string
	ConsecutiveFailed int
	SuppressedUntil   time.Time
	Probing           bool
}

// Snapshot lists the carriers currently carrying a v6 verdict, sorted.
func (self *carrierIpv6Table) Snapshot() []CarrierIpv6Status {
	self.mu.Lock()
	defer self.mu.Unlock()
	out := []CarrierIpv6Status{}
	for key, entry := range self.entries {
		if entry.suppressedUntil.IsZero() {
			continue
		}
		out = append(out, CarrierIpv6Status{
			Carrier:           carrierLabel(key),
			ConsecutiveFailed: entry.failures,
			SuppressedUntil:   entry.suppressedUntil,
			Probing:           entry.probing,
		})
	}
	sort.Slice(out, func(i int, j int) bool { return out[i].Carrier < out[j].Carrier })
	return out
}

// CarrierIpv6Snapshot is the process-wide table's marked carriers.
func CarrierIpv6Snapshot() []CarrierIpv6Status {
	return carrierIpv6.Snapshot()
}

func (self CarrierIpv6Status) String() string {
	return fmt.Sprintf("%s v6 suppressed until %s (failures=%d probing=%t)",
		self.Carrier, self.SuppressedUntil.Format(time.RFC3339), self.ConsecutiveFailed, self.Probing)
}

// dialNamesFamily is 6 or 4 when the dial itself fixes the family (a tcp6/udp6
// or tcp4/udp4 network, else an ip literal address), and 0 when it does not.
func dialNamesFamily(network string, addr string) int {
	switch network {
	case "tcp6", "udp6":
		return 6
	case "tcp4", "udp4":
		return 4
	}
	return ipLiteralFamily(addr)
}

// carrierIpv6Neutral is a dial error that says nothing about the carrier: the
// attempt was cancelled (a race loser, a teardown). A deadline that expired is
// NOT neutral: a v6 dial that hangs until the timeout is the signal.
func carrierIpv6Neutral(err error) bool {
	return errors.Is(err, context.Canceled)
}
