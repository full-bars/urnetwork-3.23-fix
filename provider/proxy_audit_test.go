package main

import (
	"fmt"
	"testing"
	"time"
)

// auditCfg is the default policy with the aggregate protections switched off, so
// each test isolates one behavior. Tests for the running floor and the
// correlated-failure breaker switch their own protection back on.
// auditEpoch is the fixed instant every audit test starts from. Tests use
// offsets from it, never the wall clock, so a failure reproduces exactly.
var auditEpoch = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

func auditCfg() proxyAuditConfig {
	c := defaultProxyAuditConfig()
	c.RunningFloorMin = 0
	c.RunningFloorFraction = 0
	c.BreakerFraction = 2 // a fraction can never exceed 1
	c.BreakerBaselineMultiple = 0
	c.DailyParkFraction = 0 // 0 disables the 24h cap
	return c
}

// auditPrimed runs a first tick that gives every proxy one bad sighting, and
// returns the input for the qualifying second tick.
func auditPrimed(cfg proxyAuditConfig, st *proxyAuditState, now time.Time, scores []float64) proxyAuditInput {
	var first, second []proxyAuditProxy
	for i, sc := range scores {
		addr := fmt.Sprintf("10.0.0.%d:1080", i+1)
		first = append(first, auditProxy(addr, sc, now.Add(-7*time.Hour)))
		second = append(second, auditProxy(addr, sc, now.Add(-time.Hour)))
	}
	proxyAuditTick(cfg, st, auditInput(now, first...))
	return auditInput(now.Add(6*time.Hour), second...)
}

// auditProxy builds a proxyAuditProxy that passes every veto by default (running,
// bandwidth known and idle, no earnings) so each test perturbs exactly one
// thing. The grade is a decidable pass at gradedAt with the given score.
func auditProxy(addr string, score float64, gradedAt time.Time) proxyAuditProxy {
	return proxyAuditProxy{
		Addr: addr,
		Entry: ProxyEntry{
			Health:      "up",
			Score:       score,
			Graded:      true,
			LastGraded:  gradedAt,
			LastDecided: gradedAt,
		},
		Running:        true,
		BandwidthKnown: true,
		Idle:           true,
	}
}

func auditInput(now time.Time, ps ...proxyAuditProxy) proxyAuditInput {
	return proxyAuditInput{Now: now, Proxies: ps, Act: true, TrackerWarm: true}
}

func auditParked(res proxyAuditResult, addr string) bool {
	for _, p := range res.Park {
		if p.Addr == addr {
			return true
		}
	}
	return false
}

func TestProxyAudit_SingleBadSightingDoesNotPark(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch

	res := proxyAuditTick(cfg, st, auditInput(now, auditProxy("a:1", 0.2, now.Add(-time.Hour))))
	if auditParked(res, "a:1") {
		t.Fatalf("one bad grade is one sighting; parking needs two")
	}
}

func TestProxyAudit_TwoBadSightingsPark(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch

	proxyAuditTick(cfg, st, auditInput(now, auditProxy("a:1", 0.2, now.Add(-7*time.Hour))))
	res := proxyAuditTick(cfg, st, auditInput(now.Add(6*time.Hour), auditProxy("a:1", 0.3, now.Add(-time.Hour))))
	if !auditParked(res, "a:1") {
		t.Fatalf("two distinct bad grades should park, got %+v", res)
	}
}

// Proxy audit ticks every few minutes but a proxy is only re-graded every
// several hours. Seeing the SAME grade on many ticks must not count as many
// sightings, or one probe pass would convict a proxy.
func TestProxyAudit_SameGradePassCountsOnce(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch
	p := auditProxy("a:1", 0.2, now.Add(-time.Hour))

	var res proxyAuditResult
	for i := 0; i < 5; i++ {
		res = proxyAuditTick(cfg, st, auditInput(now.Add(time.Duration(i)*5*time.Minute), p))
	}
	if auditParked(res, "a:1") {
		t.Fatalf("the same grade pass observed on 5 ticks must count once")
	}
}

func TestProxyAudit_GoodRegradeResetsSightings(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch

	proxyAuditTick(cfg, st, auditInput(now, auditProxy("a:1", 0.2, now.Add(-13*time.Hour))))
	proxyAuditTick(cfg, st, auditInput(now, auditProxy("a:1", 0.9, now.Add(-7*time.Hour)))) // recovered
	res := proxyAuditTick(cfg, st, auditInput(now, auditProxy("a:1", 0.2, now.Add(-time.Hour))))
	if auditParked(res, "a:1") {
		t.Fatalf("a good grade between two bad ones must reset the count")
	}
}

// Tier F is score < 0.6, a display boundary. Park needs the stricter <= 0.4
// on BOTH sightings.
func TestProxyAudit_ScoreBetweenParkThresholdAndTierFDoesNotPark(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch

	proxyAuditTick(cfg, st, auditInput(now, auditProxy("a:1", 0.2, now.Add(-7*time.Hour))))
	res := proxyAuditTick(cfg, st, auditInput(now, auditProxy("a:1", 0.55, now.Add(-time.Hour))))
	if auditParked(res, "a:1") {
		t.Fatalf("0.55 is tier F but not proven junk; must not park")
	}
}

// A reachable-but-undecidable pass (Pending) is "could not evaluate from this
// box". It must neither confirm a sighting nor erase an earlier one.
func TestProxyAudit_UndecidablePassNeitherCountsNorClears(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch

	proxyAuditTick(cfg, st, auditInput(now, auditProxy("a:1", 0.2, now.Add(-13*time.Hour))))

	// What the grader writes for a reachable-but-undecidable pass: the attempt
	// clock (LastGraded) moves and Pending is set, the old grade and the
	// decided clock (LastDecided) stay.
	pending := auditProxy("a:1", 0.2, now.Add(-13*time.Hour))
	pending.Entry.LastGraded = now.Add(-7 * time.Hour)
	pending.Entry.Pending = true
	res := proxyAuditTick(cfg, st, auditInput(now, pending))
	if auditParked(res, "a:1") {
		t.Fatalf("an undecidable pass must not count as a second sighting")
	}

	res = proxyAuditTick(cfg, st, auditInput(now, auditProxy("a:1", 0.2, now.Add(-time.Hour))))
	if !auditParked(res, "a:1") {
		t.Fatalf("the earlier bad sighting should survive an undecidable pass, got %+v", res)
	}
}

// Each veto blocks parking even with two bad sightings. Idle and earnings are
// vetoes only: they never cause a park.
func TestProxyAudit_VetoesBlockParking(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(p *proxyAuditProxy, in *proxyAuditInput)
	}{
		{"not running", func(p *proxyAuditProxy, in *proxyAuditInput) { p.Running = false }},
		{"bandwidth unknown", func(p *proxyAuditProxy, in *proxyAuditInput) { p.BandwidthKnown = false }},
		{"not idle", func(p *proxyAuditProxy, in *proxyAuditInput) { p.Idle = false }},
		{"earned recently", func(p *proxyAuditProxy, in *proxyAuditInput) { p.EarnedRecently = true }},
		{"retained earnings score", func(p *proxyAuditProxy, in *proxyAuditInput) { p.EarningsScore = earningsMinRetainedScore }},
		{"tracker cold", func(p *proxyAuditProxy, in *proxyAuditInput) { in.TrackerWarm = false }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newProxyAuditState()
			cfg := auditCfg()
			now := auditEpoch

			first := auditProxy("a:1", 0.2, now.Add(-7*time.Hour))
			proxyAuditTick(cfg, st, auditInput(now, first))

			second := auditProxy("a:1", 0.2, now.Add(-time.Hour))
			in := auditInput(now.Add(6*time.Hour), second)
			tc.mutate(&in.Proxies[0], &in)
			res := proxyAuditTick(cfg, st, in)
			if auditParked(res, "a:1") {
				t.Fatalf("veto %q should block parking", tc.name)
			}
		})
	}
}

// A well-graded idle proxy is never a park candidate: idle is not a reason.
func TestProxyAudit_GoodGradeIdleProxyIsNeverParked(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch

	var res proxyAuditResult
	for i := 0; i < 4; i++ {
		res = proxyAuditTick(cfg, st, auditInput(now.Add(time.Duration(i)*6*time.Hour),
			auditProxy("a:1", 0.95, now.Add(time.Duration(i)*6*time.Hour-time.Hour))))
	}
	if auditParked(res, "a:1") {
		t.Fatalf("an idle A-graded proxy must never be parked")
	}
}

func TestProxyAudit_PerTickCapParksWorstFirst(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch
	in := auditPrimed(cfg, st, now, []float64{0.30, 0.10, 0.35, 0.05, 0.25, 0.20})

	res := proxyAuditTick(cfg, st, in)
	if len(res.Park) != cfg.MaxParksPerTick {
		t.Fatalf("expected %d parks (per-tick cap), got %d", cfg.MaxParksPerTick, len(res.Park))
	}
	want := []string{"10.0.0.4:1080", "10.0.0.2:1080", "10.0.0.6:1080"} // scores 0.05, 0.10, 0.20
	for i, w := range want {
		if res.Park[i].Addr != w {
			t.Fatalf("park[%d] = %s, want %s (worst score first)", i, res.Park[i].Addr, w)
		}
	}
}

// Never park a small fleet down to nothing: the floor is max(10, 50%) of the
// running paid proxies, so 12 running can shed at most 2.
func TestProxyAudit_RunningFloorLimitsParks(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	cfg.MaxParksPerTick = 100
	cfg.RunningFloorMin = 10
	cfg.RunningFloorFraction = 0.5
	now := auditEpoch
	scores := make([]float64, 12)
	for i := range scores {
		scores[i] = 0.1
	}
	in := auditPrimed(cfg, st, now, scores)

	res := proxyAuditTick(cfg, st, in)
	if len(res.Park) != 2 {
		t.Fatalf("12 running with a floor of 10 allows exactly 2 parks, got %d", len(res.Park))
	}
}

func TestProxyAudit_SmallFleetAtOrBelowFloorNeverParks(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	cfg.RunningFloorMin = 10
	cfg.RunningFloorFraction = 0.5
	now := auditEpoch
	in := auditPrimed(cfg, st, now, []float64{0.1, 0.1, 0.1})

	if res := proxyAuditTick(cfg, st, in); len(res.Park) != 0 {
		t.Fatalf("3 running is below the floor of 10; nothing may park, got %d", len(res.Park))
	}
}

// If most proxies could not be evaluated from this box (DNS gutted, probe
// rate-limited) the grades are not evidence, so proxy audit acts on none.
func TestProxyAudit_ThinPassBlocksParking(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch

	bad := auditProxy("a:1", 0.1, now.Add(-7*time.Hour))
	pend := func(addr string, at time.Time) proxyAuditProxy {
		p := auditProxy(addr, 0, at)
		p.Entry.Graded = false
		p.Entry.Pending = true
		p.Entry.LastDecided = time.Time{} // never reached a verdict
		return p
	}
	proxyAuditTick(cfg, st, auditInput(now, bad, pend("b:1", now.Add(-7*time.Hour)), pend("c:1", now.Add(-7*time.Hour)), pend("d:1", now.Add(-7*time.Hour))))

	bad2 := auditProxy("a:1", 0.1, now.Add(-time.Hour))
	res := proxyAuditTick(cfg, st, auditInput(now.Add(6*time.Hour), bad2, pend("b:1", now.Add(-time.Hour)), pend("c:1", now.Add(-time.Hour)), pend("d:1", now.Add(-time.Hour))))
	if len(res.Park) != 0 || !res.Thin {
		t.Fatalf("1 decidable of 4 is a thin pass: expected no parks and Thin=true, got %+v", res)
	}
}

func auditBreakerCfg() proxyAuditConfig {
	c := auditCfg()
	c.BreakerFraction = 0.4
	c.BreakerBaselineMultiple = 2
	c.BreakerMinFresh = 5
	return c
}

// A pass in which most proxies suddenly grade bad is more likely a box-level
// problem (conntrack pressure from the probe fan-out, an upstream :443
// throttle) than ten independent proxy failures.
func TestProxyAudit_BreakerDistrustsMassFailure(t *testing.T) {
	st := newProxyAuditState()
	now := auditEpoch
	scores := make([]float64, 10)
	for i := range scores {
		scores[i] = 0.1
	}
	in := auditPrimed(auditCfg(), st, now, scores) // first tick with the breaker off

	res := proxyAuditTick(auditBreakerCfg(), st, in)
	if !res.Distrusted || len(res.Park) != 0 {
		t.Fatalf("10 of 10 freshly bad must trip the breaker, got %+v", res)
	}
}

// Distrusting a pass means its bad grades are not evidence: they must not
// count toward the two sightings either.
func TestProxyAudit_DistrustedPassDoesNotCountAsSighting(t *testing.T) {
	st := newProxyAuditState()
	now := auditEpoch
	scores := make([]float64, 10)
	for i := range scores {
		scores[i] = 0.1
	}
	in := auditPrimed(auditCfg(), st, now, scores)
	proxyAuditTick(auditBreakerCfg(), st, in) // distrusted

	// Same grades observed again, breaker off. If the distrusted pass had
	// counted, the count would already be 2 and these would park.
	res := proxyAuditTick(auditCfg(), st, auditInput(in.Now.Add(5*time.Minute), in.Proxies...))
	if len(res.Park) != 0 {
		t.Fatalf("a distrusted pass must not count as a sighting, got %d parks", len(res.Park))
	}
}

func TestProxyAudit_BreakerIgnoresSmallSamples(t *testing.T) {
	st := newProxyAuditState()
	now := auditEpoch
	in := auditPrimed(auditCfg(), st, now, []float64{0.1, 0.1})

	res := proxyAuditTick(auditBreakerCfg(), st, in)
	if res.Distrusted || len(res.Park) != 2 {
		t.Fatalf("2 fresh grades are below BreakerMinFresh; expected no distrust and 2 parks, got %+v", res)
	}
}

// 30% bad is under the absolute 40% line but triple a fleet whose normal rate
// is 10%: a jump over the trailing baseline is also suspicious.
func TestProxyAudit_BreakerTripsOnJumpOverBaseline(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditBreakerCfg()
	now := auditEpoch

	mk := func(tick int, bad int) []proxyAuditProxy {
		var ps []proxyAuditProxy
		for i := 0; i < 10; i++ {
			score := 0.95
			if i < bad {
				score = 0.1
			}
			at := now.Add(time.Duration(tick)*7*time.Hour - time.Hour)
			ps = append(ps, auditProxy(fmt.Sprintf("10.0.0.%d:1080", i+1), score, at))
		}
		return ps
	}
	for tick := 0; tick < 3; tick++ { // quiet ticks: 1 of 10 bad
		proxyAuditTick(cfg, st, auditInput(now.Add(time.Duration(tick)*7*time.Hour), mk(tick, 1)...))
	}
	res := proxyAuditTick(cfg, st, auditInput(now.Add(3*7*time.Hour), mk(3, 3)...))
	if !res.Distrusted {
		t.Fatalf("30%% bad against a 10%% baseline should trip the breaker, got %+v", res)
	}
}

// --- park bookkeeping: ladder, dwell, restore, caps, release ----------------

func TestProxyAudit_CommitParkWalksBackoffLadder(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch

	want := []time.Duration{6 * time.Hour, 12 * time.Hour, 24 * time.Hour, 48 * time.Hour, 168 * time.Hour, 168 * time.Hour}
	for i, w := range want {
		got := st.commitPark(cfg, "a:1", now, 0.5).Sub(now) // rnd 0.5 = no jitter
		if got != w {
			t.Fatalf("park #%d backoff = %s, want %s", i+1, got, w)
		}
		st.expireParks(now.Add(200 * time.Hour)) // let it lapse so the next park is a re-park
	}
}

// Jitter spreads a batch parked in the same grading window so it does not
// come back as one synchronized wave.
func TestProxyAudit_CommitParkAppliesJitter(t *testing.T) {
	cfg := auditCfg()
	now := auditEpoch

	low := newProxyAuditState().commitPark(cfg, "a:1", now, 0).Sub(now)
	high := newProxyAuditState().commitPark(cfg, "a:1", now, 1).Sub(now)
	if low != 4*time.Hour+30*time.Minute || high != 7*time.Hour+30*time.Minute {
		t.Fatalf("first park jitter range = [%s, %s], want [4h30m, 7h30m] (6h +/-25%%)", low, high)
	}
}

// Parking spends the evidence. Without this, a proxy relaunched after its
// backoff still carries two bad sightings from the OLD grades and is parked
// again on the very next tick without any new proof.
func TestProxyAudit_CommitParkConsumesEvidence(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch
	in := auditPrimed(cfg, st, now, []float64{0.1})
	if res := proxyAuditTick(cfg, st, in); !auditParked(res, "10.0.0.1:1080") {
		t.Fatalf("setup: expected a park proposal")
	}
	until := st.commitPark(cfg, "10.0.0.1:1080", in.Now, 0.5)

	after := auditInput(until.Add(time.Minute), in.Proxies...) // relaunched, same old grades
	if res := proxyAuditTick(cfg, st, after); auditParked(res, "10.0.0.1:1080") {
		t.Fatalf("a relaunched proxy must earn two NEW bad grades before it is parked again")
	}
}

func TestProxyAudit_RestoresOnGoodRegradeAfterDwell(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch
	st.commitPark(cfg, "a:1", now, 0.5)

	regraded := auditProxy("a:1", 0.8, now.Add(90*time.Minute))
	regraded.Running = false // parked
	res := proxyAuditTick(cfg, st, auditInput(now.Add(2*time.Hour), regraded))
	if len(res.Release) != 1 || res.Release[0] != "a:1" {
		t.Fatalf("a non-F regrade after the dwell should release the park, got %+v", res)
	}
	if got := st.commitPark(cfg, "a:1", now, 0.5).Sub(now); got != 6*time.Hour {
		t.Fatalf("a good regrade must reset the ladder; next park = %s, want 6h", got)
	}
}

func TestProxyAudit_NoRestoreBeforeMinDwell(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch
	st.commitPark(cfg, "a:1", now, 0.5)

	regraded := auditProxy("a:1", 0.9, now.Add(20*time.Minute))
	regraded.Running = false
	if res := proxyAuditTick(cfg, st, auditInput(now.Add(30*time.Minute), regraded)); len(res.Release) != 0 {
		t.Fatalf("inside the 1h dwell nothing may be released (kills flapping and the stale-unregister race), got %+v", res)
	}
	if res := proxyAuditTick(cfg, st, auditInput(now.Add(70*time.Minute), regraded)); len(res.Release) != 1 {
		t.Fatalf("once the dwell has passed the good grade should release, got %+v", res)
	}
}

// Hysteresis: park at <= 0.4, restore only at >= 0.6 (not tier F). A 0.5 in
// between changes nothing.
func TestProxyAudit_NoRestoreOnMiddlingRegrade(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch
	st.commitPark(cfg, "a:1", now, 0.5)

	mid := auditProxy("a:1", 0.5, now.Add(2*time.Hour))
	mid.Running = false
	if res := proxyAuditTick(cfg, st, auditInput(now.Add(3*time.Hour), mid)); len(res.Release) != 0 {
		t.Fatalf("0.5 is between park and restore thresholds; must stay parked, got %+v", res)
	}
	ok := auditProxy("a:1", 0.65, now.Add(4*time.Hour))
	ok.Running = false
	if res := proxyAuditTick(cfg, st, auditInput(now.Add(5*time.Hour), ok)); len(res.Release) != 1 {
		t.Fatalf("0.65 is not tier F and should restore, got %+v", res)
	}
}

// When the backoff simply lapses the reload reconciler relaunches the proxy on
// its own, so the record is dropped without a release. The ladder level stays:
// a proxy that comes back and is parked again climbs to the next rung.
func TestProxyAudit_ExpiredParkIsDroppedButKeepsLadder(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch
	until := st.commitPark(cfg, "a:1", now, 0.5)

	st.expireParks(until.Add(time.Second))
	if st.isParked("a:1") {
		t.Fatalf("a lapsed park should no longer be tracked as parked")
	}
	if got := st.commitPark(cfg, "a:1", until, 0.5).Sub(until); got != 12*time.Hour {
		t.Fatalf("re-park after expiry should climb to the 12h rung, got %s", got)
	}
}

func TestProxyAudit_DailyParkCap(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	cfg.DailyParkFraction = 0.15
	now := auditEpoch
	scores := make([]float64, 20)
	for i := range scores {
		scores[i] = 0.1
	}
	in := auditPrimed(cfg, st, now, scores)

	res := proxyAuditTick(cfg, st, in)
	if len(res.Park) != 3 {
		t.Fatalf("20 proxies at 15%% allow 3 parks per 24h, got %d", len(res.Park))
	}
	for _, p := range res.Park {
		st.commitPark(cfg, p.Addr, in.Now, 0.5)
	}

	if res := proxyAuditTick(cfg, st, auditInput(in.Now.Add(time.Hour), in.Proxies...)); len(res.Park) != 0 {
		t.Fatalf("the 24h budget is spent; expected no parks, got %d", len(res.Park))
	}
	// Evidence must be current to act on, so regrade everything (still bad)
	// just before the budget refills.
	later := in.Now.Add(25 * time.Hour)
	var regraded []proxyAuditProxy
	for _, p := range in.Proxies {
		q := p
		q.Entry.LastGraded = later.Add(-time.Hour)
		q.Entry.LastDecided = later.Add(-time.Hour)
		regraded = append(regraded, q)
	}
	if res := proxyAuditTick(cfg, st, auditInput(later, regraded...)); len(res.Park) != 3 {
		t.Fatalf("after 24h the budget refills; expected 3 parks, got %d", len(res.Park))
	}
}

// Turning self-heal off must give back every proxy proxy audit took, or a
// park could outlive the switch by up to a week.
func TestProxyAudit_ReleaseAllReturnsOwnParks(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch
	st.commitPark(cfg, "a:1", now, 0.5)
	st.commitPark(cfg, "b:1", now, 0.5)

	got := st.releaseAll()
	if len(got) != 2 || st.isParked("a:1") || st.isParked("b:1") {
		t.Fatalf("releaseAll should return both parks and clear them, got %v", got)
	}
	if len(st.releaseAll()) != 0 {
		t.Fatalf("a second releaseAll has nothing left to release")
	}
}

// An address that left the desired set (operator removed or re-pasted it) must
// not stay parked or keep stale evidence.
func TestProxyAudit_PrunesAddressesLeavingTheDesiredSet(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch
	proxyAuditTick(cfg, st, auditInput(now, auditProxy("gone:1", 0.1, now.Add(-time.Hour)), auditProxy("kept:1", 0.1, now.Add(-time.Hour))))
	st.commitPark(cfg, "gone:1", now, 0.5)

	res := proxyAuditTick(cfg, st, auditInput(now.Add(time.Hour), auditProxy("kept:1", 0.1, now.Add(-time.Hour))))
	if len(res.Release) != 1 || res.Release[0] != "gone:1" {
		t.Fatalf("a parked address that left the desired set should be released, got %+v", res)
	}
	if _, ok := st.sightings["gone:1"]; ok || st.isParked("gone:1") {
		t.Fatalf("state for a removed address should be dropped")
	}
}

// proxy.state persists the last grade across restarts, so the first tick after
// a restart sees a possibly days-old F. That pass predates proxy audit's
// memory and is not evidence: it must take two passes graded after the
// audit started, or a restart would shorten the proof it was designed to
// demand.
func TestProxyAudit_GradePassesBeforeStartAreNotEvidence(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch
	st.notBefore = now

	proxyAuditTick(cfg, st, auditInput(now, auditProxy("a:1", 0.1, now.Add(-2*time.Hour)))) // persisted pre-start grade

	res := proxyAuditTick(cfg, st, auditInput(now.Add(6*time.Hour), auditProxy("a:1", 0.1, now.Add(time.Hour))))
	if auditParked(res, "a:1") {
		t.Fatalf("a pre-start grade plus one fresh grade is only one sighting")
	}
	res = proxyAuditTick(cfg, st, auditInput(now.Add(12*time.Hour), auditProxy("a:1", 0.1, now.Add(7*time.Hour))))
	if !auditParked(res, "a:1") {
		t.Fatalf("two grades after start should park, got %+v", res)
	}
}

// --- passes with no verdict -------------------------------------------------

// auditNoVerdict is what the grader writes when a pass completes but reaches no
// verdict (stage-0 liveness failed, or every host was unresolved or denied):
// the attempt clock LastGraded advances and Pending is cleared, while the old
// Score and Graded stay. Only LastDecided says the grade is not new.
func auditNoVerdict(p proxyAuditProxy, at time.Time) proxyAuditProxy {
	p.Entry.LastGraded = at
	p.Entry.Pending = false
	return p
}

// One real bad grade plus one pass that reached no verdict is ONE sighting.
// The stale score must not be counted a second time.
func TestProxyAudit_NoVerdictPassIsNotASecondSighting(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch

	first := auditProxy("a:1", 0.1, now.Add(-13*time.Hour))
	proxyAuditTick(cfg, st, auditInput(now, first))

	res := proxyAuditTick(cfg, st, auditInput(now, auditNoVerdict(first, now.Add(-7*time.Hour))))
	if auditParked(res, "a:1") {
		t.Fatalf("a pass with no verdict must not count as a second bad sighting")
	}

	res = proxyAuditTick(cfg, st, auditInput(now, auditProxy("a:1", 0.1, now.Add(-time.Hour))))
	if !auditParked(res, "a:1") {
		t.Fatalf("the first sighting must survive the no-verdict pass, so a real second one parks, got %+v", res)
	}
}

// A no-verdict pass with an old GOOD score must not read as a fresh good pass
// and reset the ladder.
func TestProxyAudit_NoVerdictPassDoesNotResetLadder(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch

	good := auditProxy("a:1", 0.9, now.Add(-13*time.Hour))
	proxyAuditTick(cfg, st, auditInput(now, good))
	st.levels["a:1"] = 2 // as if it had been parked twice since

	proxyAuditTick(cfg, st, auditInput(now, auditNoVerdict(good, now.Add(-7*time.Hour))))
	if st.levels["a:1"] != 2 {
		t.Fatalf("a no-verdict pass must not reset the backoff ladder, level = %d", st.levels["a:1"])
	}
}

// A DNS or limiter outage on the box leaves every old score in place while
// every fresh pass reaches no verdict. That is not evidence about proxies, so
// the pass is thin even though the stale entries still look graded.
func TestProxyAudit_FreshPassesMostlyWithoutVerdictAreThin(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch

	mk := func(i int, score float64, at time.Time) proxyAuditProxy {
		return auditProxy(fmt.Sprintf("10.0.0.%d:1080", i), score, at)
	}
	var first []proxyAuditProxy
	for i := 0; i < 100; i++ {
		score := 0.95
		if i < 10 {
			score = 0.1 // ten proxies with one bad sighting
		}
		first = append(first, mk(i, score, now.Add(-13*time.Hour)))
	}
	proxyAuditTick(cfg, st, auditInput(now, first...))

	// Those ten are re-probed during the outage: attempt clock moves, no verdict.
	second := make([]proxyAuditProxy, len(first))
	copy(second, first)
	for i := 0; i < 10; i++ {
		second[i] = auditNoVerdict(first[i], now.Add(-time.Hour))
	}
	res := proxyAuditTick(cfg, st, auditInput(now.Add(time.Hour), second...))
	if !res.Thin || len(res.Park) != 0 {
		t.Fatalf("10 fresh attempts with no verdict is a thin pass, got %+v", res)
	}
}

// --- breaker baseline on a clean fleet --------------------------------------

// auditFreshTicks feeds n proxies a fresh decidable grade on each of the ticks,
// with bad[tick] of them scoring bad, and returns the last result. Every tick
// is a new grade pass for every proxy, like a busy grading window.
func auditFreshTicks(cfg proxyAuditConfig, st *proxyAuditState, now time.Time, n int, bad []int) proxyAuditResult {
	var res proxyAuditResult
	for tick, nb := range bad {
		var ps []proxyAuditProxy
		for i := 0; i < n; i++ {
			score := 0.95
			if i < nb {
				score = 0.1
			}
			at := now.Add(time.Duration(tick)*7*time.Hour - time.Hour)
			ps = append(ps, auditProxy(fmt.Sprintf("10.0.0.%d:1080", i+1), score, at))
		}
		res = proxyAuditTick(cfg, st, auditInput(now.Add(time.Duration(tick)*7*time.Hour), ps...))
	}
	return res
}

// On a clean fleet the trailing baseline is zero, so "more than twice the
// baseline" is true for ANY bad grade. One bad grade among a handful of fresh
// ones is ordinary noise, not a box-level failure, and must not be discarded.
func TestProxyAudit_BreakerIgnoresOneBadGradeOnACleanFleet(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditBreakerCfg()
	now := auditEpoch

	res := auditFreshTicks(cfg, st, now, 6, []int{0, 0, 1})
	if res.Distrusted {
		t.Fatalf("1 bad of 6 fresh grades on a clean fleet must not trip the breaker, got %+v", res)
	}
}

// The guard must not blind the jump test: several bad grades at once over a
// near-zero baseline is exactly the box-level signal it exists for.
func TestProxyAudit_BreakerStillTripsOnAJumpOverANearZeroBaseline(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditBreakerCfg()
	now := auditEpoch

	res := auditFreshTicks(cfg, st, now, 10, []int{0, 0, 0, 3})
	if !res.Distrusted {
		t.Fatalf("3 bad of 10 over a zero baseline is a jump and should trip the breaker, got %+v", res)
	}
}

// --- evidence must belong to this proxy instance and be recent --------------

// Two bad grades taken while the proxy was down (vetoed) sit at bad=2. Days
// later the vendor fixes it, it comes up idle, and without an age limit it
// would be parked within one tick on grades that are days old.
func TestProxyAudit_StaleEvidenceIsNotEnough(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch

	down := func(score float64, at time.Time) proxyAuditProxy {
		p := auditProxy("a:1", score, at)
		p.Running = false
		return p
	}
	proxyAuditTick(cfg, st, auditInput(now, down(0.1, now.Add(-7*time.Hour))))
	proxyAuditTick(cfg, st, auditInput(now.Add(6*time.Hour), down(0.1, now.Add(-time.Hour)))) // bad=2, vetoed

	// Three days later it is up and idle, with no new grade yet.
	up := auditProxy("a:1", 0.1, now.Add(-time.Hour))
	res := proxyAuditTick(cfg, st, auditInput(now.Add(72*time.Hour), up))
	if auditParked(res, "a:1") {
		t.Fatalf("grades three days old are not evidence to park on")
	}

	// It must earn two fresh bad grades from here.
	res = proxyAuditTick(cfg, st, auditInput(now.Add(73*time.Hour), auditProxy("a:1", 0.1, now.Add(72*time.Hour))))
	if auditParked(res, "a:1") {
		t.Fatalf("one fresh bad grade after stale evidence is only one sighting")
	}
	res = proxyAuditTick(cfg, st, auditInput(now.Add(80*time.Hour), auditProxy("a:1", 0.1, now.Add(79*time.Hour))))
	if !auditParked(res, "a:1") {
		t.Fatalf("two fresh bad grades should park, got %+v", res)
	}
}

// A proxy that goes not-running and comes back is a new instance. Grades
// gathered about the old one must be re-earned, or a relaunch (a fixed
// transport, a credential rotation that restarted it) inherits a verdict it
// never had a chance to change.
func TestProxyAudit_RelaunchClearsEvidence(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch

	proxyAuditTick(cfg, st, auditInput(now, auditProxy("a:1", 0.1, now.Add(-7*time.Hour))))
	down := auditProxy("a:1", 0.1, now.Add(-time.Hour))
	down.Running = false
	proxyAuditTick(cfg, st, auditInput(now.Add(6*time.Hour), down)) // bad=2 while down

	back := auditProxy("a:1", 0.1, now.Add(-time.Hour)) // same grades, running again
	res := proxyAuditTick(cfg, st, auditInput(now.Add(7*time.Hour), back))
	if auditParked(res, "a:1") {
		t.Fatalf("a relaunched proxy must not be parked on grades about its previous run")
	}
}

// A rotated credential is a different proxy as far as grading goes: the old
// grades measured the old credentials.
func TestProxyAudit_CredentialChangeClearsEvidence(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch

	with := func(cred string, score float64, at time.Time) proxyAuditProxy {
		p := auditProxy("a:1", score, at)
		p.Cred = cred
		return p
	}
	proxyAuditTick(cfg, st, auditInput(now, with("old", 0.1, now.Add(-13*time.Hour))))
	proxyAuditTick(cfg, st, auditInput(now, with("old", 0.1, now.Add(-7*time.Hour))))

	res := proxyAuditTick(cfg, st, auditInput(now.Add(time.Hour), with("new", 0.1, now.Add(-7*time.Hour))))
	if auditParked(res, "a:1") {
		t.Fatalf("grades taken with the old credentials must not park the rotated proxy")
	}
}

// A proxy that stays busy is vetoed, and its two bad sightings just sit
// there. When it finally goes idle days later, grades that old are history,
// not a reason to cancel it before it has been regraded.
func TestProxyAudit_EvidenceThatAgedWhileVetoedIsNotEnough(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch

	busy := func(at time.Time) proxyAuditProxy {
		p := auditProxy("a:1", 0.1, at)
		p.Idle = false
		return p
	}
	proxyAuditTick(cfg, st, auditInput(now, busy(now.Add(-7*time.Hour))))
	proxyAuditTick(cfg, st, auditInput(now.Add(6*time.Hour), busy(now.Add(-time.Hour)))) // bad=2, vetoed

	idle := auditProxy("a:1", 0.1, now.Add(-time.Hour)) // same old grade, now idle
	res := proxyAuditTick(cfg, st, auditInput(now.Add(72*time.Hour), idle))
	if auditParked(res, "a:1") {
		t.Fatalf("evidence three days old must not park a proxy that has not been regraded")
	}

	// A fresh bad grade on top of the earlier ones is current evidence again.
	res = proxyAuditTick(cfg, st, auditInput(now.Add(79*time.Hour), auditProxy("a:1", 0.1, now.Add(78*time.Hour))))
	if !auditParked(res, "a:1") {
		t.Fatalf("a fresh bad grade after consecutive bad ones should park, got %+v", res)
	}
}

// LA7 (2026-09-18): the same host:port is pasted again with new credentials.
// Address is the proxy's identity, so a parked address whose credentials
// changed is a different proxy for grading purposes: the park (and the
// address-keyed backoff behind it) belonged to the old credentials, and the
// ladder must start over.
func TestProxyAudit_CredentialRotationReleasesAPark(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch

	with := func(cred string, at time.Time) proxyAuditProxy {
		p := auditProxy("a:1", 0.1, at)
		p.Cred = cred
		return p
	}
	proxyAuditTick(cfg, st, auditInput(now, with("old", now.Add(-7*time.Hour))))
	res := proxyAuditTick(cfg, st, auditInput(now.Add(6*time.Hour), with("old", now.Add(-time.Hour))))
	if !auditParked(res, "a:1") {
		t.Fatalf("setup: expected a park, got %+v", res)
	}
	st.commitPark(cfg, "a:1", now.Add(6*time.Hour), 0.5)

	// Two hours later the operator re-pastes the address with new credentials.
	res = proxyAuditTick(cfg, st, auditInput(now.Add(8*time.Hour), with("new", now.Add(-time.Hour))))
	if len(res.Release) != 1 || res.Release[0] != "a:1" {
		t.Fatalf("a rotated credential must release the park, got %+v", res.Release)
	}
	if st.isParked("a:1") {
		t.Fatalf("the park record must be dropped on rotation")
	}
	// The next park of the new credentials starts at the bottom rung.
	if got := st.commitPark(cfg, "a:1", now.Add(9*time.Hour), 0.5).Sub(now.Add(9 * time.Hour)); got != cfg.ParkLadder[0] {
		t.Fatalf("the ladder must reset on rotation, got %s want %s", got, cfg.ParkLadder[0])
	}
}

// Rotation must not release a proxy that merely has an empty fingerprint on
// both sides (no credentials at all), and a park with unchanged credentials
// stands.
func TestProxyAudit_UnchangedCredentialsKeepThePark(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch
	for _, cred := range []string{"", "same"} {
		st = newProxyAuditState()
		p := auditProxy("a:1", 0.1, now.Add(-time.Hour))
		p.Cred = cred
		proxyAuditTick(cfg, st, auditInput(now, p))
		st.commitPark(cfg, "a:1", now, 0.5)
		res := proxyAuditTick(cfg, st, auditInput(now.Add(2*time.Hour), p))
		if len(res.Release) != 0 || !st.isParked("a:1") {
			t.Fatalf("cred %q: unchanged credentials must keep the park, got release=%v", cred, res.Release)
		}
	}
}

// Re-review #4: a grade stamped BEFORE a rotation (or relaunch) but first seen
// AFTER it was measured against the old credentials or instance. The reset
// must not let it count as the first sighting of the new one.
func TestProxyAudit_AGradeStampedBeforeARotationIsNotEvidenceAboutTheNewCredentials(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch

	with := func(cred string, at time.Time) proxyAuditProxy {
		p := auditProxy("a:1", 0.1, at)
		p.Cred = cred
		return p
	}
	proxyAuditTick(cfg, st, auditInput(now, with("old", now.Add(-time.Hour))))
	// A new bad grade landed at +30m under the old credentials; the operator
	// rotated at +50m; proxy audit's tick at +1h sees both at once.
	proxyAuditTick(cfg, st, auditInput(now.Add(time.Hour), with("new", now.Add(30*time.Minute))))

	// Only genuinely new grades count from here: one is not enough...
	res := proxyAuditTick(cfg, st, auditInput(now.Add(8*time.Hour), with("new", now.Add(7*time.Hour))))
	if auditParked(res, "a:1") {
		t.Fatalf("one grade taken under the new credentials is one sighting, not two")
	}
	// ...two are.
	res = proxyAuditTick(cfg, st, auditInput(now.Add(15*time.Hour), with("new", now.Add(14*time.Hour))))
	if !auditParked(res, "a:1") {
		t.Fatalf("two grades under the new credentials should park, got %+v", res)
	}
}

// Re-review #5: a distrusted pass is thrown away, and that has to include the
// age of the evidence. Otherwise a discarded pass refreshes two bad grades that
// were 25 hours old and makes the proxy a candidate on them.
func TestProxyAudit_ADistrustedPassDoesNotRefreshTheAgeOfOldEvidence(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	cfg.BreakerFraction = 0.4 // auditCfg switches the breaker off
	now := auditEpoch

	target := func(idle bool, decidedAt time.Time) proxyAuditProxy {
		p := auditProxy("t:1", 0.1, decidedAt)
		p.Idle = idle
		return p
	}
	// Two bad grades, both old, while the proxy is busy (vetoed).
	proxyAuditTick(cfg, st, auditInput(now, target(false, now.Add(-31*time.Hour))))
	proxyAuditTick(cfg, st, auditInput(now.Add(time.Hour), target(false, now.Add(-25*time.Hour))))

	// A day later a mass failure: every proxy gets a fresh bad grade, so the
	// breaker distrusts the pass and rolls it back.
	t2 := now.Add(30 * time.Hour)
	ps := []proxyAuditProxy{target(false, t2.Add(-time.Hour))}
	for i := 0; i < 5; i++ {
		ps = append(ps, auditProxy(fmt.Sprintf("o:%d", i), 0.1, t2.Add(-time.Hour)))
	}
	if res := proxyAuditTick(cfg, st, auditInput(t2, ps...)); !res.Distrusted {
		t.Fatalf("setup: the mass failure should trip the breaker, got %+v", res)
	}

	// Next tick nothing new arrives and the target is idle. Its evidence is
	// still the 25 hour old grades, so it is not a candidate.
	ps[0] = target(true, t2.Add(-time.Hour))
	res := proxyAuditTick(cfg, st, auditInput(t2.Add(5*time.Minute), ps...))
	if auditParked(res, "t:1") {
		t.Fatalf("a discarded pass must not refresh the age of 25 hour old evidence")
	}
}

// Pin the boundary: evidence exactly MaxEvidenceAge old still counts, one
// second older does not.
func TestProxyAudit_EvidenceAgeBoundaryIsInclusive(t *testing.T) {
	cfg := auditCfg()
	now := auditEpoch
	for _, tc := range []struct {
		name string
		age  time.Duration
		want bool
	}{
		{"exactly the limit", cfg.MaxEvidenceAge, true},
		{"one second past", cfg.MaxEvidenceAge + time.Second, false},
	} {
		st := newProxyAuditState()
		proxyAuditTick(cfg, st, auditInput(now.Add(-2*time.Hour), auditProxy("a:1", 0.1, now.Add(-tc.age-8*time.Hour))))
		proxyAuditTick(cfg, st, auditInput(now, auditProxy("a:1", 0.1, now.Add(-tc.age))))
		res := proxyAuditTick(cfg, st, auditInput(now, auditProxy("a:1", 0.1, now.Add(-tc.age))))
		if got := auditParked(res, "a:1"); got != tc.want {
			t.Errorf("%s: parked=%v want %v", tc.name, got, tc.want)
		}
	}
}

// Pin (do not "fix") the flap behaviour from re-review #3: a proxy seen not
// running at any tick starts its evidence over, so a proxy that flaps between
// grade passes is not parkable. Parking targets proxies that stay up but grade
// as junk; flappers are the degraded and dead paths' business.
func TestProxyAudit_AProxyThatFlapsBetweenGradesIsNotParkable(t *testing.T) {
	st := newProxyAuditState()
	cfg := auditCfg()
	now := auditEpoch

	proxyAuditTick(cfg, st, auditInput(now, auditProxy("a:1", 0.1, now.Add(-7*time.Hour))))
	blip := auditProxy("a:1", 0.1, now.Add(-7*time.Hour))
	blip.Running = false // a tick that happens to land while it reconnects
	proxyAuditTick(cfg, st, auditInput(now.Add(3*time.Hour), blip))
	res := proxyAuditTick(cfg, st, auditInput(now.Add(6*time.Hour), auditProxy("a:1", 0.1, now.Add(-time.Hour))))
	if auditParked(res, "a:1") {
		t.Fatalf("a proxy seen down in between must start over, got a park")
	}
}
