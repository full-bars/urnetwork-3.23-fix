package main

import (
	"math"
	"sort"
	"time"
)

// Proxy audit: acts on the A-F proxy grade by PARKING a proven-junk paid or
// file proxy (cancel it and back off its relaunch) and restoring it when it
// regrades non-F or its backoff lapses.
//
// This file is the pure decision core: no I/O, no clocks, no globals. The
// caller snapshots the world into a proxyAuditInput, gets a proxyAuditResult, and
// decides whether to act on it or only log it (self-heal off = observe).
//
// Policy in one paragraph: the GRADE alone decides who is a candidate. Idle
// and earnings are vetoes only. They stop proxy audit from cancelling a proxy
// that is carrying sessions or earning, but they are never a reason to park
// and never lower a grade. A well-graded idle proxy is just unassigned by the
// platform and is left alone.

// proxyAuditConfig holds the tunable thresholds. defaultProxyAuditConfig is the
// shipped policy; tests override fields to isolate one behavior.
type proxyAuditConfig struct {
	// ParkScoreMax is the highest table-probe score that still counts as a
	// "bad" sighting. Deliberately stricter than the tier-F display boundary
	// (< 0.6): a proxy at 0.58 is poor, not proven junk.
	ParkScoreMax float64
	// SightingsRequired is how many distinct bad grade passes, with no good
	// decidable pass in between, a proxy needs before it is a candidate.
	SightingsRequired int

	// MaxParksPerTick caps how many proxies one tick may park.
	MaxParksPerTick int
	// RunningFloorMin and RunningFloorFraction set the floor below which the
	// audit never parks: max(RunningFloorMin, ceil(running*fraction)) paid
	// proxies stay running. A box at or under the floor never parks anything.
	RunningFloorMin      int
	RunningFloorFraction float64
	// MinDecidableFraction is the share of paid proxies that must carry a
	// decidable grade for the pass to be trusted. Below it the pass is "thin"
	// (the box could not evaluate most proxies) and nothing is parked.
	MinDecidableFraction float64

	// Correlated-failure breaker. When at least BreakerMinFresh proxies got a
	// fresh decidable grade this tick and the bad share exceeds BreakerFraction,
	// or exceeds BreakerBaselineMultiple times the trailing baseline (0 disables
	// the baseline test), the pass is distrusted: no parks, and its bad grades
	// do not count as sightings. Mass failure is more likely a box-level
	// problem than many independent proxy failures.
	BreakerFraction         float64
	BreakerBaselineMultiple float64
	BreakerMinFresh         int
	// BreakerMinBad and BreakerBaselineFloor keep the baseline test honest on a
	// clean fleet. The trailing baseline can be zero, and "more than twice zero"
	// is true for any bad grade at all, so the baseline test only fires when at
	// least BreakerMinBad grades are bad AND the bad share exceeds the multiple
	// of max(baseline, BreakerBaselineFloor). One bad grade among a handful is
	// noise, not a box-level failure.
	BreakerMinBad        int
	BreakerBaselineFloor float64

	// ParkLadder is the backoff per consecutive park of the same proxy; the
	// last rung repeats. The first rung stays well under the 24h client-JWT
	// lifetime so a relaunch after the first park still reuses a live token.
	// JitterFraction spreads each backoff by +/- that share so a batch parked
	// together does not return as one wave.
	ParkLadder     []time.Duration
	JitterFraction float64
	// MinDwell is how long a park must stand before a good regrade can restore
	// it: it stops flapping and gives the old goroutine time to finish winding
	// down before the same address is relaunched.
	MinDwell time.Duration
	// RestoreScoreMin is the regrade score (not tier F) that releases a park.
	// Park is at ParkScoreMax (0.4) and restore at 0.6: the gap is hysteresis.
	RestoreScoreMin float64
	// DailyParkFraction caps parks per rolling 24h as a share of the paid set
	// (at least 1). Zero disables the cap.
	DailyParkFraction float64
	// MaxEvidenceAge is how old the latest bad grade may be and still justify a
	// park: twice the calm paid stale window, so a proxy is regraded at least
	// once in it. Older evidence (a proxy that stayed busy, or held back by the
	// caps) has to be re-earned. Zero disables the check.
	MaxEvidenceAge time.Duration
}

// proxyAuditBaselineAlpha is the smoothing factor of the trailing bad-share
// baseline (exponentially weighted; higher reacts faster).
const proxyAuditBaselineAlpha = 0.3

func defaultProxyAuditConfig() proxyAuditConfig {
	return proxyAuditConfig{
		ParkScoreMax:      0.4,
		SightingsRequired: 2,

		MaxParksPerTick:      3,
		RunningFloorMin:      10,
		RunningFloorFraction: 0.5,
		MinDecidableFraction: 0.5,

		BreakerFraction:         0.4,
		BreakerBaselineMultiple: 2,
		BreakerMinFresh:         5,
		BreakerMinBad:           3,
		BreakerBaselineFloor:    0.1,

		ParkLadder:        []time.Duration{6 * time.Hour, 12 * time.Hour, 24 * time.Hour, 48 * time.Hour, 168 * time.Hour},
		JitterFraction:    0.25,
		MinDwell:          time.Hour,
		RestoreScoreMin:   0.6,
		DailyParkFraction: 0.15,
		MaxEvidenceAge:    2 * paidStaleCalm,
	}
}

// proxyAuditProxy is one paid/file proxy as seen this tick.
type proxyAuditProxy struct {
	Addr  string
	Entry ProxyEntry // grade fields read from proxy.state

	// Running is true when the LIVE registry says the proxy is up. It must not
	// be derived from proxy.state Health, which goes stale after a park.
	Running bool
	// BandwidthKnown is false when the bandwidth registry has no entry. Unknown
	// is NOT idle: a proxy we cannot measure might be carrying sessions.
	BandwidthKnown bool
	// Idle is meaningful only when BandwidthKnown.
	Idle bool
	// EarningsScore is the persisted decayed earnings score; EarnedRecently is
	// the in-memory recent-delta check. Either one vetoes.
	EarningsScore  float64
	EarnedRecently bool
	// Cred is an opaque fingerprint of the credentials the proxy is graded and
	// launched with (never the credentials themselves). Grades measured under
	// other credentials are not evidence about this ones.
	Cred string
}

type proxyAuditInput struct {
	Now     time.Time
	Proxies []proxyAuditProxy
	// Act is true when the caller will execute parks (self-heal on and hot
	// restart on). The decision logic is identical either way; the caller uses
	// it to choose between parking and logging would-park.
	Act bool
	// TrackerWarm is true once the earn tracker and earnings store have been
	// running long enough that "no earnings" means something after a restart.
	TrackerWarm bool
}

type proxyAuditPark struct {
	Addr string
}

type proxyAuditResult struct {
	// Park lists proxies that pass every gate this tick, worst first, already
	// trimmed to the per-tick cap and the running floor.
	Park []proxyAuditPark
	// Thin is true when too few proxies carried a decidable grade to trust the
	// pass; Distrusted is true when the correlated-failure breaker tripped.
	Thin       bool
	Distrusted bool
	// Release lists parked addresses the caller should hand back by clearing
	// their backoff (globalProxyFailureHistory.Reset): restored on a good
	// regrade, or gone from the desired set. A park that merely expires needs
	// no release; the reload reconciler relaunches it on its own.
	Release []string
}

// proxyAuditSighting tracks the bad-grade evidence for one address.
type proxyAuditSighting struct {
	lastGraded  time.Time // LastGraded (attempt clock) last seen
	lastDecided time.Time // LastDecided (verdict clock) last seen
	bad         int       // consecutive decidable bad passes

	// wasDown is set when the proxy was last seen not running; coming back up
	// is a new instance and the old evidence does not carry over. cred is the
	// credential fingerprint the evidence was gathered under.
	wasDown bool
	cred    string

	// evidenceFrom is when the current evidence began: the moment a relaunch or
	// credential change was observed. A grade stamped earlier was measured
	// against the previous instance or credentials and does not count, even if
	// this is the first tick to see it.
	evidenceFrom time.Time
	// evidenceAt is when the latest bad grade counted toward bad was taken, and
	// prevEvidenceAt the one before it. They are separate from lastDecided (the
	// identity of the last grade seen) so a distrusted pass, which rolls bad
	// back, can roll the age back too instead of refreshing old evidence.
	evidenceAt     time.Time
	prevEvidenceAt time.Time
}

// proxyAuditState is the in-memory memory of proxy audit. It is deliberately
// not persisted: after a restart parks lapse and evidence is rebuilt from the
// next two grade passes.
type proxyAuditState struct {
	sightings map[string]*proxyAuditSighting

	// baseline is the trailing share of freshly graded proxies that graded
	// bad, smoothed across ticks that had enough fresh grades to measure.
	baseline    float64
	baselineSet bool

	// parks are the proxies THIS audit is holding out, so it only ever
	// releases what it took. levels is the ladder rung per address and
	// outlives a park (a re-park climbs); a good regrade resets it. parkTimes
	// feeds the rolling 24h cap.
	parks     map[string]*proxyAuditParkRecord
	levels    map[string]int
	parkTimes []time.Time
	// ownBackoff is the backoff end proxy audit set per address, so a release
	// clears exactly that and not a longer one someone else put there. It is
	// dropped when the park lapses or is released.
	ownBackoff map[string]time.Time

	// notBefore is when proxy audit's memory began. Grade passes stamped
	// earlier (proxy.state persists the last grade across restarts) are
	// recorded but are not evidence, so a restart cannot shorten the proof.
	notBefore time.Time
}

type proxyAuditParkRecord struct {
	parkedAt time.Time
	until    time.Time
	// cred is the credential fingerprint the proxy was parked under. The
	// address-keyed backoff behind a park belongs to those credentials; when the
	// address is re-added with new ones (LA7, 2026-09-18) the park is released.
	cred string
}

func newProxyAuditState() *proxyAuditState {
	return &proxyAuditState{
		sightings:  map[string]*proxyAuditSighting{},
		parks:      map[string]*proxyAuditParkRecord{},
		levels:     map[string]int{},
		ownBackoff: map[string]time.Time{},
	}
}

// takeOwnBackoff returns and forgets the backoff end proxy audit set for addr,
// for the caller to release with proxyFailureHistory.ReleaseBackoff.
func (st *proxyAuditState) takeOwnBackoff(addr string) (time.Time, bool) {
	until, ok := st.ownBackoff[addr]
	delete(st.ownBackoff, addr)
	return until, ok
}

// pruneParkTimes drops parks older than 24h from the rolling budget.
func (st *proxyAuditState) pruneParkTimes(now time.Time) {
	cutoff := now.Add(-24 * time.Hour)
	kept := st.parkTimes[:0]
	for _, t := range st.parkTimes {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	st.parkTimes = kept
}

// keepTime is the upkeep that needs no view of the paid list, and so is safe
// while the list is unreadable: lapsed parks stop counting as parked (reload
// relaunches them regardless) and the 24h budget keeps aging. It deliberately
// does not release by presence: with no list every address looks gone, and that
// would release every park.
func (st *proxyAuditState) keepTime(now time.Time) {
	st.expireParks(now)
	st.pruneParkTimes(now)
}

// parkedAddrs lists the addresses proxy audit is holding out, sorted.
func (st *proxyAuditState) parkedAddrs() []string {
	out := make([]string, 0, len(st.parks))
	for addr := range st.parks {
		out = append(out, addr)
	}
	sort.Strings(out)
	return out
}

func (st *proxyAuditState) isParked(addr string) bool {
	_, ok := st.parks[addr]
	return ok
}

// release removes addr from parked proxies if present and returns whether it was parked.
func (st *proxyAuditState) release(addr string) bool {
	if _, ok := st.parks[addr]; ok {
		delete(st.parks, addr)
		return true
	}
	return false
}

// commitPark records that the caller parked addr and returns when the backoff
// ends. rnd is a sample in [0,1) for the jitter (0.5 = none). Parking spends
// the evidence: the proxy needs two NEW bad grades before it can be parked
// again, otherwise a relaunch would re-park it on the old grades.
func (st *proxyAuditState) commitPark(cfg proxyAuditConfig, addr string, now time.Time, rnd float64) time.Time {
	level := st.levels[addr]
	rung := level
	if rung >= len(cfg.ParkLadder) {
		rung = len(cfg.ParkLadder) - 1
	}
	base := cfg.ParkLadder[rung]
	jitter := 1 + (2*rnd-1)*cfg.JitterFraction
	until := now.Add(time.Duration(float64(base) * jitter))

	st.levels[addr] = level + 1
	rec := &proxyAuditParkRecord{parkedAt: now, until: until}
	if s := st.sightings[addr]; s != nil {
		rec.cred = s.cred
	}
	st.parks[addr] = rec
	st.ownBackoff[addr] = until
	st.parkTimes = append(st.parkTimes, now)
	if s := st.sightings[addr]; s != nil {
		s.bad = 0
	}
	return until
}

// expireParks drops park records whose backoff has ended. The reload
// reconciler relaunches those proxies by itself; the ladder level is kept.
func (st *proxyAuditState) expireParks(now time.Time) {
	for addr, rec := range st.parks {
		if !now.Before(rec.until) {
			delete(st.parks, addr)
			delete(st.ownBackoff, addr) // lapsed on its own: nothing to release
		}
	}
}

// releaseAll returns and forgets every park, for when the operator turns
// self-heal off: proxy audit must give back what it took.
func (st *proxyAuditState) releaseAll() []string {
	var out []string
	for addr := range st.parks {
		out = append(out, addr)
		delete(st.parks, addr)
	}
	sort.Strings(out)
	return out
}

// observe folds this tick's grade for p into the sighting record.
//
// A grade is identified by LastDecided, the clock the grader advances ONLY when
// a pass reached a verdict. LastGraded advances on every completed pass, so a
// pass that reached no verdict (stage-0 liveness failed, or every host was
// unresolved or denied) moves it while Score and Graded stay at their old
// values. Keying on LastGraded would count that stale score a second time.
// Seeing the same grade on many ticks also counts once.
//
// fresh reports a NEW decidable pass this tick; wasBad that it counted as a bad
// sighting (so a distrusted pass can be rolled back); undecided that a new
// attempt reached no verdict (so a tick full of them can be called thin).
func (st *proxyAuditState) observe(cfg proxyAuditConfig, p proxyAuditProxy, now time.Time) (s *proxyAuditSighting, fresh, wasBad, undecided bool) {
	s = st.sightings[p.Addr]
	if s == nil {
		s = &proxyAuditSighting{cred: p.Cred}
		st.sightings[p.Addr] = s
	}
	// Evidence belongs to one instance of the proxy under one set of
	// credentials. A relaunch (down, then up) or a rotated credential starts it
	// over; the grade already on record stays recorded so it is not counted as
	// new, but it no longer counts toward a park.
	if !p.Running {
		s.wasDown = true
	} else if s.wasDown {
		s.wasDown = false
		s.bad = 0
		s.evidenceFrom = now
	}
	if p.Cred != s.cred {
		s.cred = p.Cred
		s.bad = 0
		s.evidenceFrom = now
	}
	attempted := p.Entry.LastGraded.After(s.lastGraded)
	decided := p.Entry.LastDecided.After(s.lastDecided)
	if attempted {
		s.lastGraded = p.Entry.LastGraded
	}
	if decided {
		s.lastDecided = p.Entry.LastDecided
	}

	switch {
	case decided:
		if p.Entry.LastDecided.Before(st.notBefore) {
			return s, false, false, false // graded before proxy audit existed
		}
		if !p.Entry.Graded {
			return s, false, false, false
		}
		if p.Entry.LastDecided.Before(s.evidenceFrom) {
			return s, false, false, false // measured against the previous instance or credentials
		}
		fresh = true
		if p.Entry.Score <= cfg.ParkScoreMax {
			s.bad++
			s.prevEvidenceAt, s.evidenceAt = s.evidenceAt, p.Entry.LastDecided
			wasBad = true
		} else {
			s.bad = 0
			if p.Entry.Score >= cfg.RestoreScoreMin {
				delete(st.levels, p.Addr) // healthy again: start the ladder over
			}
		}
	case attempted && !p.Entry.LastGraded.Before(st.notBefore):
		// A new attempt with no verdict neither confirms nor clears earlier
		// evidence: "could not evaluate from this box".
		undecided = true
	}
	return s, fresh, wasBad, undecided
}

// vetoed reports whether something says this proxy must not be cancelled now.
func proxyAuditVetoed(p proxyAuditProxy, in proxyAuditInput) bool {
	if !p.Running || !p.BandwidthKnown || !p.Idle {
		return true
	}
	if !in.TrackerWarm {
		return true
	}
	if p.EarnedRecently || p.EarningsScore >= earningsMinRetainedScore {
		return true
	}
	return false
}

// proxyAuditTick evaluates one audit tick.
func proxyAuditTick(cfg proxyAuditConfig, st *proxyAuditState, in proxyAuditInput) proxyAuditResult {
	var res proxyAuditResult

	// Fold every proxy's grade into the evidence first, remembering which
	// bad sightings this tick added so a distrusted pass can undo them.
	type scored struct {
		p proxyAuditProxy
		s *proxyAuditSighting
	}
	all := make([]scored, 0, len(in.Proxies))
	var fresh, freshBad, freshUndecided int
	var freshBadSightings []*proxyAuditSighting
	decidable, running := 0, 0
	for _, p := range in.Proxies {
		s, isFresh, wasBad, isUndecided := st.observe(cfg, p, in.Now)
		all = append(all, scored{p, s})
		if isFresh {
			fresh++
		}
		if isUndecided {
			freshUndecided++
		}
		if wasBad {
			freshBad++
			freshBadSightings = append(freshBadSightings, s)
		}
		if proxyAuditLatestPassDecided(p.Entry) {
			decidable++
		}
		if p.Running {
			running++
		}
	}

	res.Release = st.maintainParks(cfg, in)

	// A tick where most fresh attempts reached no verdict says the box could not
	// evaluate proxies (DNS or limiter outage), not that the proxies are bad. The
	// stale scores still look graded, so this is judged on the fresh attempts.
	if attempts := fresh + freshUndecided; attempts >= cfg.BreakerMinFresh &&
		float64(fresh)/float64(attempts) < cfg.MinDecidableFraction {
		res.Thin = true
		return res
	}

	if fresh >= cfg.BreakerMinFresh && fresh > 0 {
		frac := float64(freshBad) / float64(fresh)
		distrust := frac > cfg.BreakerFraction ||
			(cfg.BreakerBaselineMultiple > 0 && st.baselineSet && freshBad >= cfg.BreakerMinBad &&
				frac > cfg.BreakerBaselineMultiple*math.Max(st.baseline, cfg.BreakerBaselineFloor))
		if distrust {
			for _, s := range freshBadSightings {
				s.bad-- // this pass is not evidence, nor is its timestamp
				s.evidenceAt = s.prevEvidenceAt
			}
			res.Distrusted = true
			return res
		}
		if st.baselineSet {
			st.baseline += proxyAuditBaselineAlpha * (frac - st.baseline)
		} else {
			st.baseline, st.baselineSet = frac, true
		}
	}

	if n := len(in.Proxies); n > 0 && float64(decidable)/float64(n) < cfg.MinDecidableFraction {
		res.Thin = true
		return res
	}

	var candidates []scored
	for _, c := range all {
		if st.isParked(c.p.Addr) || c.s.bad < cfg.SightingsRequired || proxyAuditVetoed(c.p, in) {
			continue
		}
		if cfg.MaxEvidenceAge > 0 && in.Now.Sub(c.s.evidenceAt) > cfg.MaxEvidenceAge {
			continue // the bad grades are too old to act on
		}
		candidates = append(candidates, c)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].p.Entry.Score != candidates[j].p.Entry.Score {
			return candidates[i].p.Entry.Score < candidates[j].p.Entry.Score
		}
		return candidates[i].p.Addr < candidates[j].p.Addr
	})

	minRunning := int(math.Ceil(float64(running) * cfg.RunningFloorFraction))
	if minRunning < cfg.RunningFloorMin {
		minRunning = cfg.RunningFloorMin
	}
	limit := running - minRunning
	if limit > cfg.MaxParksPerTick {
		limit = cfg.MaxParksPerTick
	}
	if cfg.DailyParkFraction > 0 {
		cap24 := int(math.Ceil(float64(len(in.Proxies))*cfg.DailyParkFraction - 1e-9))
		if cap24 < 1 {
			cap24 = 1
		}
		if allowed := cap24 - len(st.parkTimes); allowed < limit {
			limit = allowed
		}
	}
	for i := 0; i < len(candidates) && i < limit; i++ {
		res.Park = append(res.Park, proxyAuditPark{Addr: candidates[i].p.Addr})
	}
	return res
}

// maintainParks keeps proxy audit's own records honest each tick: it prunes
// state for addresses that left the desired set (releasing any park on them),
// restores parks whose proxy has regraded healthy after the dwell, and drops
// parks whose backoff lapsed. It returns the addresses to release.
func (st *proxyAuditState) maintainParks(cfg proxyAuditConfig, in proxyAuditInput) []string {
	var release []string
	present := make(map[string]bool, len(in.Proxies))
	for _, p := range in.Proxies {
		present[p.Addr] = true
	}
	for addr := range st.sightings {
		if !present[addr] {
			delete(st.sightings, addr)
		}
	}
	for addr := range st.levels {
		if !present[addr] {
			delete(st.levels, addr)
		}
	}
	for addr := range st.parks {
		if !present[addr] {
			release = append(release, addr)
			delete(st.parks, addr)
		}
	}
	// A pruned address is fresh evidence-wise. The 24h budget is pruned by age
	// only: a park that was released early or restored still counts, which is
	// conservative.
	st.pruneParkTimes(in.Now)

	for _, p := range in.Proxies {
		rec := st.parks[p.Addr]
		if rec == nil {
			continue
		}
		if rec.cred != p.Cred {
			// Rotated credentials are a different proxy: give the address back
			// and start its ladder over.
			release = append(release, p.Addr)
			delete(st.parks, p.Addr)
			delete(st.levels, p.Addr)
			continue
		}
		e := p.Entry
		if e.Graded && !e.Pending && e.Score >= cfg.RestoreScoreMin &&
			e.LastGraded.After(rec.parkedAt) && in.Now.Sub(rec.parkedAt) >= cfg.MinDwell {
			release = append(release, p.Addr)
			delete(st.parks, p.Addr)
		}
	}
	st.expireParks(in.Now)
	sort.Strings(release)
	return release
}

// proxyAuditLatestPassDecided reports whether the entry's most recent pass
// reached a verdict. Entries graded before LastDecided existed (zero) count as
// decided, so an upgrade does not read as a fleet-wide thin pass.
func proxyAuditLatestPassDecided(e ProxyEntry) bool {
	if !e.Graded || e.Pending {
		return false
	}
	return e.LastDecided.IsZero() || !e.LastGraded.After(e.LastDecided)
}
