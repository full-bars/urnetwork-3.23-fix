package connect

// Visibility for the dual-stack platform transports.
//
// A transport marked FamilyEvents (the native identity: each pinned transport,
// the standby, or the single legacy transport) keeps per-family counters and
// writes one log line per connect, failure and session end. The counters are
// keyed by the family the transport is pinned to ("ipv4", "ipv6") or "any" for a
// family-agnostic transport (the standby, or the legacy single transport), so a
// node on the kill switch reads the same series under "any" and the two builds
// compare directly. Proxied identities are never counted here: there are
// hundreds of them and they have their own series.
//
// Every label set is closed (family, carrier mode, close cause), so the series
// count is bounded.

import (
	"fmt"
	"net"
	"sync/atomic"
	"time"
)

const (
	familyStatsIpv4 = iota
	familyStatsIpv6
	familyStatsAny
	familyStatsCount
)

var familyStatsNames = [familyStatsCount]string{"ipv4", "ipv6", "any"}

// the carrier modes the counters distinguish
var familyStatsModeNames = [2]string{"h1", "h3"}

// session lifetime buckets, seconds; the last bucket is +Inf
var familyLifetimeBounds = [5]int64{10, 60, 300, 1800, 7200}

type familyModeStats struct {
	attempts atomic.Uint64
	connects atomic.Uint64
	failures atomic.Uint64
	drops    [12]atomic.Uint64 // by close cause (transportCloseCauseNames)
}

type familyStats struct {
	modes [2]familyModeStats
	// sessions up right now
	up atomic.Int64
	// time spent with at least one session up, completed spans only (the span in
	// flight is added at export from liveSince)
	connectedNanos atomic.Int64
	// when the family last went from no session to one, zero while none is up
	liveSince       atomic.Int64
	lastConnectUnix atomic.Int64
	lastDropUnix    atomic.Int64
	// a pinned transport that connected over the other family
	mismatches atomic.Uint64
	// completed session lifetimes
	lifetimeBuckets  [len(familyLifetimeBounds) + 1]atomic.Uint64
	lifetimeSumNanos atomic.Int64
	lifetimeCount    atomic.Uint64
}

var familyStatsAll [familyStatsCount]familyStats

// group level: the pinned transports as a set, and the standby
var familyGroupStats struct {
	standbyActivations  atomic.Uint64
	standbyActiveNanos  atomic.Int64
	standbyActiveSince  atomic.Int64
	pinnedDownEvents    atomic.Uint64
	pinnedDownNanos     atomic.Int64
	pinnedDownSince     atomic.Int64
	pinnedLongestNanos  atomic.Int64
	pinnedRecoveryCount atomic.Uint64
}

func familyStatsIndex(ipFamily int) int {
	switch ipFamily {
	case 4:
		return familyStatsIpv4
	case 6:
		return familyStatsIpv6
	}
	return familyStatsAny
}

func familyModeIndex(mode TransportMode) int {
	if mode == TransportModeH1 {
		return 0
	}
	return 1
}

// FamilyModeSnapshot is one family and carrier's counters.
type FamilyModeSnapshot struct {
	Attempts uint64
	Connects uint64
	Failures uint64
	// Drops is by close cause, in TransportCloseCauseNames order
	Drops [12]uint64
}

// FamilyTransportSnapshot is one family's counters.
type FamilyTransportSnapshot struct {
	Family           string
	Modes            [2]FamilyModeSnapshot
	Up               int64
	ConnectedSeconds float64
	LastConnectUnix  int64
	LastDropUnix     int64
	Mismatches       uint64
	// cumulative histogram counts for LifetimeBounds, then +Inf
	LifetimeBuckets    [len(familyLifetimeBounds) + 1]uint64
	LifetimeSumSeconds float64
	LifetimeCount      uint64
}

// FamilyGroupSnapshot is the group-level counters.
type FamilyGroupSnapshot struct {
	StandbyActivations   uint64
	StandbyActiveSeconds float64
	// PinnedDown is true while no pinned transport is connected
	PinnedDown          bool
	PinnedDownEvents    uint64
	PinnedDownSeconds   float64
	PinnedLongestSecond float64
	PinnedRecoveries    uint64
}

// FamilyTransportStatsSnapshot is everything the dual-stack series export.
type FamilyTransportStatsSnapshot struct {
	Families       [familyStatsCount]FamilyTransportSnapshot
	Group          FamilyGroupSnapshot
	LifetimeBounds [len(familyLifetimeBounds)]int64
	ModeNames      [2]string
	CauseNames     []string
}

// FamilyTransportStatsNow returns the cumulative dual-stack counters.
func FamilyTransportStatsNow() FamilyTransportStatsSnapshot {
	now := time.Now().UnixNano()
	out := FamilyTransportStatsSnapshot{
		LifetimeBounds: familyLifetimeBounds,
		ModeNames:      familyStatsModeNames,
		CauseNames:     transportCloseCauseNames,
	}
	for i := range familyStatsAll {
		fs := &familyStatsAll[i]
		snap := &out.Families[i]
		snap.Family = familyStatsNames[i]
		for m := range fs.modes {
			snap.Modes[m].Attempts = fs.modes[m].attempts.Load()
			snap.Modes[m].Connects = fs.modes[m].connects.Load()
			snap.Modes[m].Failures = fs.modes[m].failures.Load()
			for c := range fs.modes[m].drops {
				snap.Modes[m].Drops[c] = fs.modes[m].drops[c].Load()
			}
		}
		snap.Up = fs.up.Load()
		connected := fs.connectedNanos.Load()
		if live := fs.liveSince.Load(); 0 < live && 0 < snap.Up {
			connected += now - live
		}
		snap.ConnectedSeconds = float64(connected) / 1e9
		snap.LastConnectUnix = fs.lastConnectUnix.Load()
		snap.LastDropUnix = fs.lastDropUnix.Load()
		snap.Mismatches = fs.mismatches.Load()
		var cumulative uint64
		for b := range fs.lifetimeBuckets {
			cumulative += fs.lifetimeBuckets[b].Load()
			snap.LifetimeBuckets[b] = cumulative
		}
		snap.LifetimeSumSeconds = float64(fs.lifetimeSumNanos.Load()) / 1e9
		snap.LifetimeCount = fs.lifetimeCount.Load()
	}
	g := &familyGroupStats
	out.Group.StandbyActivations = g.standbyActivations.Load()
	standby := g.standbyActiveNanos.Load()
	if since := g.standbyActiveSince.Load(); 0 < since {
		standby += now - since
	}
	out.Group.StandbyActiveSeconds = float64(standby) / 1e9
	down := g.pinnedDownNanos.Load()
	if since := g.pinnedDownSince.Load(); 0 < since {
		out.Group.PinnedDown = true
		down += now - since
	}
	out.Group.PinnedDownEvents = g.pinnedDownEvents.Load()
	out.Group.PinnedDownSeconds = float64(down) / 1e9
	out.Group.PinnedLongestSecond = float64(g.pinnedLongestNanos.Load()) / 1e9
	out.Group.PinnedRecoveries = g.pinnedRecoveryCount.Load()
	return out
}

var familyEventThrottles [familyStatsCount]*logThrottle

func init() {
	for i := range familyEventThrottles {
		familyEventThrottles[i] = newLogThrottle(time.Minute)
	}
}

// familyLabel is the short name used in log lines: v4, v6 or any.
func familyLabel(index int) string {
	switch index {
	case familyStatsIpv4:
		return "v4"
	case familyStatsIpv6:
		return "v6"
	}
	return "any"
}

func addrString(addr net.Addr) string {
	if addr == nil {
		return "?"
	}
	return addr.String()
}

// addrFamily is 4 or 6 for an address with a literal ip, else 0.
func addrFamily(addr net.Addr) int {
	if addr == nil {
		return 0
	}
	var ip net.IP
	switch a := addr.(type) {
	case *net.TCPAddr:
		ip = a.IP
	case *net.UDPAddr:
		ip = a.IP
	case *net.IPAddr:
		ip = a.IP
	default:
		return ipLiteralFamily(addr.String())
	}
	if ip == nil {
		return 0
	}
	if ip.To4() != nil {
		return 4
	}
	return 6
}

// familySession is the state of one connected session, handed back to
// familyNoteSessionEnded.
type familySession struct {
	started time.Time
	remote  string
	local   string
	mode    TransportMode
	// zero value (not tracked) when the transport is not marked FamilyEvents
	tracked bool
}

// ConsecutiveDialFailures is how many dials in a row have failed on this
// transport since its last success or reset (zero while connected).
func (self *PlatformTransport) ConsecutiveDialFailures() int {
	backoff := self.failBackoff
	if self.pinned() {
		backoff = self.pinnedBackoff
	}
	if backoff == nil {
		return 0
	}
	backoff.stateLock.Lock()
	defer backoff.stateLock.Unlock()
	return backoff.failures
}

func (self *PlatformTransport) familyTracked() bool {
	return self.settings != nil && self.settings.FamilyEvents
}

// familyNoteAttempt counts one connect attempt in mode.
func (self *PlatformTransport) familyNoteAttempt(mode TransportMode) {
	if !self.familyTracked() {
		return
	}
	familyStatsAll[familyStatsIndex(self.ipFamily)].modes[familyModeIndex(mode)].attempts.Add(1)
}

// familyNoteConnectFailed counts a failed attempt and writes one throttled line.
func (self *PlatformTransport) familyNoteConnectFailed(mode TransportMode, err error, dial time.Duration) {
	if !self.familyTracked() || err == nil {
		return
	}
	index := familyStatsIndex(self.ipFamily)
	familyStatsAll[index].modes[familyModeIndex(mode)].failures.Add(1)
	if ok, suppressed := familyEventThrottles[index].Allow(time.Now()); ok {
		self.log.Infof("[ipv6] %s %s connect failed class=%s dial=%s consecutive=%d suppressed=%d err=%q\n",
			familyLabel(index), mode, classifyConnectError(err, nil), dial.Round(time.Millisecond),
			self.ConsecutiveDialFailures(), suppressed, err.Error())
	}
}

// familyNoteConnected records a session that came up, checks that a pinned
// transport really connected over its family, and writes one line.
func (self *PlatformTransport) familyNoteConnected(mode TransportMode, carrier string, local net.Addr, remote net.Addr, dial time.Duration) familySession {
	if !self.familyTracked() {
		return familySession{}
	}
	index := familyStatsIndex(self.ipFamily)
	fs := &familyStatsAll[index]
	fs.modes[familyModeIndex(mode)].connects.Add(1)
	now := time.Now()
	if fs.up.Add(1) == 1 {
		fs.liveSince.Store(now.UnixNano())
	}
	fs.lastConnectUnix.Store(now.Unix())
	observed := addrFamily(remote)
	mismatch := self.ipFamily != 0 && observed != 0 && observed != self.ipFamily
	if mismatch {
		fs.mismatches.Add(1)
		self.log.Infof("[ipv6] %s %s FAMILY MISMATCH: pinned ipv%d but connected to %s\n",
			familyLabel(index), mode, self.ipFamily, addrString(remote))
	}
	self.log.Infof("[ipv6] %s %s connected carrier=%s remote=%s local=%s remote_family=ipv%d dial=%s after_failures=%d\n",
		familyLabel(index), mode, carrier, addrString(remote), addrString(local), observed,
		dial.Round(time.Millisecond), self.ConsecutiveDialFailures())
	return familySession{
		started: now,
		remote:  addrString(remote),
		local:   addrString(local),
		mode:    mode,
		tracked: true,
	}
}

// familyNoteSessionEnded closes the books on a session: lifetime, connected time,
// the drop by cause, and one line.
func (self *PlatformTransport) familyNoteSessionEnded(session familySession, cause string, detail string) {
	if !session.tracked {
		return
	}
	index := familyStatsIndex(self.ipFamily)
	fs := &familyStatsAll[index]
	now := time.Now()
	lived := now.Sub(session.started)
	// connected time is the time with at least one session up, so an H1 and an H3
	// session overlapping on one family count once
	if n := fs.up.Add(-1); n <= 0 {
		// an unpaired end can only come from a bug upstream of here, but a
		// negative counter would suppress every connected-time close after it
		if n < 0 {
			fs.up.Add(-n)
		}
		if since := fs.liveSince.Swap(0); 0 < since {
			fs.connectedNanos.Add(now.UnixNano() - since)
		}
	}
	fs.lifetimeSumNanos.Add(int64(lived))
	fs.lifetimeCount.Add(1)
	seconds := int64(lived / time.Second)
	bucket := len(familyLifetimeBounds)
	for b, bound := range familyLifetimeBounds {
		if seconds < bound {
			bucket = b
			break
		}
	}
	fs.lifetimeBuckets[bucket].Add(1)
	causeIndex := len(transportCloseCauseNames) - 1
	for c, name := range transportCloseCauseNames {
		if name == cause {
			causeIndex = c
			break
		}
	}
	// a session we ended ourselves (shutdown, the operator gate) is not a drop
	local := self.ctx.Err() != nil || cause == "closed_local"
	if !local {
		fs.modes[familyModeIndex(session.mode)].drops[causeIndex].Add(1)
		fs.lastDropUnix.Store(now.Unix())
	}
	self.log.Infof("[ipv6] %s %s session ended lived=%s cause=%s drop=%t remote=%s err=%q\n",
		familyLabel(index), session.mode, lived.Round(time.Millisecond), cause, !local, session.remote, detail)
}

// familyEndCause remembers the first error that ended a session, as a close
// cause name.
type familyEndCause struct {
	value  atomic.Pointer[string]
	detail atomic.Pointer[string]
}

func (self *familyEndCause) note(err error) {
	if err == nil {
		return
	}
	name := transportCloseCauseNames[classifyTransportCloseCause(err)]
	if self.value.CompareAndSwap(nil, &name) {
		// the raw error of the first failure, so a cause of "other" can be read
		text := err.Error()
		if 200 < len(text) {
			text = text[:200]
		}
		self.detail.Store(&text)
	}
}

// getDetail is the raw error text behind the cause, or "".
func (self *familyEndCause) getDetail() string {
	if p := self.detail.Load(); p != nil {
		return *p
	}
	return ""
}

func (self *familyEndCause) get() string {
	if p := self.value.Load(); p != nil {
		return *p
	}
	return "other"
}

// familyGroupPinnedChanged is called by the group when the set of pinned
// transports goes from some connected to none connected, or back.
func familyGroupPinnedChanged(log Logger, pinnedConnected bool, status string) {
	g := &familyGroupStats
	now := time.Now()
	if !pinnedConnected {
		if g.pinnedDownSince.CompareAndSwap(0, now.UnixNano()) {
			g.pinnedDownEvents.Add(1)
			log.Infof("[ipv6] pinned transports: none connected (%s)\n", status)
		}
		return
	}
	if since := g.pinnedDownSince.Swap(0); 0 < since {
		down := now.UnixNano() - since
		g.pinnedDownNanos.Add(down)
		g.pinnedRecoveryCount.Add(1)
		for {
			longest := g.pinnedLongestNanos.Load()
			if down <= longest || g.pinnedLongestNanos.CompareAndSwap(longest, down) {
				break
			}
		}
		log.Infof("[ipv6] pinned transports: connected again after %s (%s)\n",
			time.Duration(down).Round(time.Second), status)
	}
}

// familyGroupStandbyChanged is called when the standby starts or stops dialing.
func familyGroupStandbyChanged(log Logger, active bool, reason string) {
	g := &familyGroupStats
	now := time.Now()
	if active {
		if g.standbyActiveSince.CompareAndSwap(0, now.UnixNano()) {
			g.standbyActivations.Add(1)
			log.Infof("[ipv6] standby transport active: %s\n", reason)
		}
		return
	}
	if since := g.standbyActiveSince.Swap(0); 0 < since {
		active := now.UnixNano() - since
		g.standbyActiveNanos.Add(active)
		log.Infof("[ipv6] standby transport stood down after %s: %s\n",
			time.Duration(active).Round(time.Second), reason)
	}
}

// familyStatusString is the tuple the group status lines use.
func familyStatusString(v4 PlatformTransportState, v6 PlatformTransportState, standby PlatformTransportState) string {
	return fmt.Sprintf("v4=%s v6=%s standby=%s", v4, v6, standby)
}

// familyGroupPinnedReset forgets an open pinned-down stretch without counting it
// as a recovery, for when the operator fixes the families (a disabled pin is not
// a failure).
func familyGroupPinnedReset() {
	familyGroupStats.pinnedDownSince.Store(0)
}
