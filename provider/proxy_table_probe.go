package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"

	"golang.org/x/time/rate"
)

// Stage-1 quality probe: a table probe against a sampled block of the
// backend's destination table, run on stage-0 survivors (proxies that
// passed the SOCKS5 + API CONNECT check) before they are admitted to the
// auth queue. The design follows connect/ip_remote_multi_client_probe.go:
//
//   - POSITIVE evidence only: a SynAck through the proxy proves its own
//     upstream dial succeeded; silence never convicts (anti-bot egress drops
//     are a policy, not a verdict).
//   - Resolution happens OUTSIDE the probed channel: the box's own DNS
//     resolves hostnames, so a proxy with broken DNS does not fail a TCP
//     probe that was never about DNS.
//   - Deterministic disjoint-block rotation (see sampleProbeTargets), so a
//     proxy re-probed over a session walks the whole table instead of
//     re-testing the same few sites.
//   - Fail-fast by viability: the pass aborts only when the bar is already
//     mathematically unreachable on the denominator the score will actually
//     use (attempted + still-untried), so an aborted pass can never look
//     worse than the evidence supports and hosts the box's resolver cannot
//     answer — which leave the score denominator — can never abort a pass
//     that could still qualify (review #8).
//
// The bar is tiered: score >= PreferredBar is "preferred", >= PassBar is
// "qualified". Only qualified (or better) proxies enter the auth queue.

// proxyTableProbeConfig holds the tunable knobs for the stage-1 probe.
type proxyTableProbeConfig struct {
	// Enabled turns stage-1 gating on or off at runtime (kill switch).
	// When false, URL-source proxies are admitted on stage 0 alone, exactly
	// as before this feature shipped.
	Enabled bool
	// SampleWidth is how many health hosts one pass dials (upstream
	// ProbeSampleHostCount). Clamped to at most half the table so the
	// disjoint-block rotation property holds.
	SampleWidth int
	// TargetTimeout bounds each individual CONNECT attempt.
	TargetTimeout time.Duration
	// PassBar is the qualification bar (free-tier admission).
	PassBar float64
	// PreferredBar is the preferred tier, validated >= PassBar.
	PreferredBar float64
	// MaxSampleWidth is the upper bound the ADAPTIVE probe may grow to for
	// a borderline proxy (one whose provisional score sits within
	// BorderlineBand of PassBar after the base SampleWidth, where a wider
	// sample decides quality with more confidence). Clearly-good and
	// clearly-dead proxies stop at (or before) SampleWidth, spending paid
	// probe bandwidth only in the uncertain middle. Default 30. Clamped to
	// <= ProbeHostCount/2 like SampleWidth. Effective when >= SampleWidth.
	MaxSampleWidth int
	// BorderlineBand is the half-width (around PassBar) below which a
	// provisional base-sample score is treated as "too close to call" and
	// the probe grows the base toward MaxSampleWidth. A score more than
	// BorderlineBand away from PassBar is a decisive verdict and stops at
	// the base width. Default 0.15.
	BorderlineBand float64
	// MaxPaidProbesPerTick caps how many paid/file proxies ONE grading pass
	// (one 5-minute tick) may probe. This is the throughput lever that turns
	// a 4000-proxy full sweep from ~22h into ~100 minutes: the collector
	// keeps the OLDEST-STALE-FIRST entries up to this budget and defers the
	// rest to a later tick, so a pass is bounded regardless of how stale the
	// fleet is. 0 disables the cap (probe everything eligible this pass).
	// Default 200.
	MaxPaidProbesPerTick int
}

// defaultProxyTableProbeConfig returns the stock configuration. SampleWidth
// 12, 4s per target, tiered 0.9/0.6.
func defaultProxyTableProbeConfig() proxyTableProbeConfig {
	return proxyTableProbeConfig{
		Enabled:              true,
		SampleWidth:          12,
		TargetTimeout:        4 * time.Second,
		PassBar:              0.6,
		PreferredBar:         0.9,
		MaxSampleWidth:       30,
		BorderlineBand:       0.15,
		MaxPaidProbesPerTick: 200,
	}
}

// probeWidth returns the width of the host pool sampled for a pass: the
// wider of SampleWidth and MaxSampleWidth, so the adaptive path always has
// hosts to grow into. Equal to SampleWidth when MaxSampleWidth is at/below
// it (adaptive overflow disabled / no room to grow).
func (c proxyTableProbeConfig) probeWidth() int {
	if c.MaxSampleWidth > c.SampleWidth {
		return c.MaxSampleWidth
	}
	return c.SampleWidth
}

// proxyProbeOverridePath returns ~/.urnetwork/proxy_probe.json, a runtime
// override file an operator can write (or delete) without restarting. The
// JSON shape matches proxyTableProbeConfig:
//
//	{"enabled": true, "sample_width": 12, "timeout_ms": 4000,
//	 "pass_bar": 0.6, "preferred_bar": 0.9, "max_sample_width": 30,
//	 "borderline_band": 0.15, "max_paid_probes_per_tick": 200}
//
// Missing or malformed keys fall back to defaults.
func proxyProbeOverridePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "proxy_probe.json"), nil
}

// probeConfigTTL bounds how long resolveProxyTableProbeConfig reuses a
// parsed override snapshot. The resolver runs on the auth hot path (twice
// per retry iteration) and on every cache merge; a few seconds of staleness
// is invisible to an operator editing proxy_probe.json, and the TTL stops a
// filesystem read per call (review #14, extending the admissionStateTTL
// pattern).
const probeConfigTTL = 5 * time.Second

var probeConfigCache struct {
	sync.Mutex
	cfg proxyTableProbeConfig
	at  time.Time
}

// resolveProxyTableProbeConfig returns the effective probe configuration,
// reusing the previous parse within probeConfigTTL so the auth hot path does
// not issue one filesystem read per call (review #14).
func resolveProxyTableProbeConfig() proxyTableProbeConfig {
	probeConfigCache.Lock()
	defer probeConfigCache.Unlock()
	if !probeConfigCache.at.IsZero() && time.Since(probeConfigCache.at) < probeConfigTTL {
		return probeConfigCache.cfg
	}
	cfg := loadProxyTableProbeConfig()
	probeConfigCache.cfg = cfg
	probeConfigCache.at = time.Now()
	return cfg
}

// loadProxyTableProbeConfig re-reads the override file and returns the
// effective configuration. Absent/unparseable file or fields fall back to
// defaults; an explicitly-set field wins over the default.
func loadProxyTableProbeConfig() proxyTableProbeConfig {
	cfg := defaultProxyTableProbeConfig()
	path, err := proxyProbeOverridePath()
	if err != nil {
		return cfg
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	var over struct {
		Enabled        *bool    `json:"enabled"`
		SampleWidth    *int     `json:"sample_width"`
		TimeoutMS      *int     `json:"timeout_ms"`
		PassBar        *float64 `json:"pass_bar"`
		PreferredBar   *float64 `json:"preferred_bar"`
		MaxSampleWidth *int     `json:"max_sample_width"`
		BorderlineBand *float64 `json:"borderline_band"`
		MaxPaidPerTick *int     `json:"max_paid_probes_per_tick"`
	}
	if err := json.Unmarshal(b, &over); err != nil {
		return cfg
	}
	if over.Enabled != nil {
		cfg.Enabled = *over.Enabled
	}
	if over.SampleWidth != nil && *over.SampleWidth > 0 {
		cfg.SampleWidth = *over.SampleWidth
	}
	if over.TimeoutMS != nil && *over.TimeoutMS > 0 {
		cfg.TargetTimeout = time.Duration(*over.TimeoutMS) * time.Millisecond
	}
	if over.PassBar != nil && *over.PassBar > 0 && *over.PassBar <= 1.0 {
		cfg.PassBar = *over.PassBar
	}
	if over.PreferredBar != nil && *over.PreferredBar > 0 && *over.PreferredBar <= 1.0 {
		cfg.PreferredBar = *over.PreferredBar
	}
	if over.MaxSampleWidth != nil && *over.MaxSampleWidth > 0 {
		cfg.MaxSampleWidth = *over.MaxSampleWidth
	}
	if over.BorderlineBand != nil && *over.BorderlineBand >= 0 && *over.BorderlineBand <= 1.0 {
		cfg.BorderlineBand = *over.BorderlineBand
	}
	if over.MaxPaidPerTick != nil && *over.MaxPaidPerTick >= 0 {
		cfg.MaxPaidProbesPerTick = *over.MaxPaidPerTick
	}
	// Clamp the borderline band so the uncertain-margin stays within [0,1]
	// around PassBar. A band wider than the distance to the nearer edge would
	// push the low end of [PassBar-band, PassBar+band] below 0 (or the high
	// end above 1), making EVERY real score "borderline" and defeating the
	// "grow only in the uncertain middle" intent (review MEDIUM). The band
	// caps at the smaller of PassBar and 1-PassBar.
	if cfg.PassBar > 0 && cfg.BorderlineBand > 0 {
		if cap := min(cfg.PassBar, 1-cfg.PassBar); cfg.BorderlineBand > cap {
			cfg.BorderlineBand = cap
		}
	}
	// Clamp sample width so the disjoint-block rotation property holds
	// (two blocks of n out of a table of total are disjoint only when
	// 2n <= total). Upstream's default width is the whole table; a wide
	// override silently destroys the property, so clamp and say so —
	// ONCE, not once per auth attempt (finding NEW-6: this resolver runs
	// on the auth hot path).
	if maxWidth := connect.ProbeHostCount() / 2; cfg.SampleWidth > maxWidth {
		cfg.SampleWidth = maxWidth
		widthClampWarning.Do(func() {
			tlog("[proxy][url] stage-1: sample_width clamped to %d (half the %d-host table; disjoint rotation requires 2*width <= table)\n",
				maxWidth, connect.ProbeHostCount())
		})
	}
	// Clamp the adaptive ceiling too (same disjoint-block constraint). An
	// operator may set it below SampleWidth, which simply disables adaptive
	// overflow (the probe uses SampleWidth alone); no warning needed there.
	if cfg.MaxSampleWidth > connect.ProbeHostCount()/2 {
		cfg.MaxSampleWidth = connect.ProbeHostCount() / 2
	}
	// An inverted bar pair would let the log label ("preferred") disagree
	// with the gate decision. Clamp PreferredBar up to PassBar.
	if cfg.PreferredBar < cfg.PassBar {
		cfg.PreferredBar = cfg.PassBar
		barClampWarning.Do(func() {
			tlog("[proxy][url] stage-1: preferred_bar clamped up to pass_bar %.2f (inverted pair)\n", cfg.PassBar)
		})
	}
	return cfg
}

// widthClampWarning and barClampWarning dedupe the config-clamp log lines so
// each prints once per process instead of once per auth attempt (finding
// NEW-6). Separate guards per clamp: a shared Once would let the first clamp
// that fires silence the other for the whole process lifetime (review #7).
var (
	widthClampWarning sync.Once
	barClampWarning   sync.Once
)

// tableProbePassCounter increments once per fetch cycle so consecutive
// cycles rotate the sampled block (disjoint rotation across passes).
var tableProbePassCounter atomic.Uint64

// tableProbeSeed derives the deterministic rotation seed for a proxy
// address and pass. Same (address, pass) always yields the same seed.
// Addition (not XOR or hashing of the pair) is deliberate: sampleProbeTargets
// computes block start = (seed*n) % tableSize, so consecutive seeds return
// DISJOINT blocks — the upstream rotation guarantee that a proxy re-probed
// over a session walks the whole table instead of re-testing the same few
// sites. A differing address shifts the base, spreading simultaneous probes
// across the table; a differing pass moves the block forward by one step.
func tableProbeSeed(address string, pass uint64) uint64 {
	h := fnv.New64a()
	h.Write([]byte(address))
	return h.Sum64() + pass
}

// tableProbeResult is the outcome of one stage-1 pass against one proxy.
type tableProbeResult struct {
	// Score is OK/Total — the share of ATTEMPTED targets that answered
	// (upstream's Answered/Sent semantics). The denominator is the
	// attempted subset, not the intended sample: hosts the box's resolver
	// could not answer are excluded from both the pass and the score, so a
	// DNS failure on this box can never convict a proxy (findings H2,
	// NEW-1, review #8).
	Score float64
	// OK is how many sampled targets answered with a SynAck.
	OK int
	// SampleWidth is the intended sample size (not the Score denominator —
	// see Score).
	SampleWidth int
	// Total is how many targets were actually attempted (unresolvable
	// hosts are excluded — resolution failure is the box's problem, not
	// the proxy's, and must not convict it).
	Total int
	// Decidable is true when the pass produced a genuine verdict: at least
	// one target was attempted and the context was not cancelled. A pass
	// that asked nothing (cancelled context, resolver outage) is NOT
	// decidable and must not be persisted as a grade — absence of evidence
	// is not evidence of absence (finding C1).
	Decidable bool
	// Failed lists the target hostnames that did not answer.
	Failed []string
}

// qualified reports whether the pass clears the given bar. A pass that
// asked nothing never qualifies anyone.
func (r tableProbeResult) qualified(bar float64) bool {
	if !r.Decidable || r.SampleWidth == 0 {
		return false
	}
	return r.Score >= bar
}

// probeDNSCache memoizes target resolution. The table is ~127 hosts, so one
// lookup per host per TTL covers every proxy and every cycle, and stage 0
// already caches the API address the same way ("so each probe doesn't
// trigger a fresh DNS lookup"). Failures are memoized with a short TTL so a
// resolver degradation does not re-issue the same failing lookup per proxy
// per pass (finding NEW-8); successes are memoized with a longer TTL so a
// health host that changes address is re-resolved within hours instead of
// the box dialing a stale IP through every proxy for the whole process
// lifetime (review #15).
var probeDNSCache = struct {
	sync.Mutex
	m    map[string]probeDNSCachedIP
	fail map[string]time.Time
}{m: map[string]probeDNSCachedIP{}, fail: map[string]time.Time{}}

// probeDNSCachedIP is a successful resolution memoized with its lookup time
// so it can expire (probeDNSSuccessTTL).
type probeDNSCachedIP struct {
	ip net.IP
	at time.Time
}

// probeDNSFailTTL is how long a failed resolution is remembered before the
// box retries it. Long enough to absorb a whole fetch cycle of probes,
// short enough that a recovered resolver is noticed.
const probeDNSFailTTL = 30 * time.Second

// probeDNSSuccessTTL is how long a successful resolution is remembered
// before the box re-resolves the host. Long enough that a fetch cycle only
// pays one lookup per host, short enough that an address change is noticed
// within hours.
const probeDNSSuccessTTL = 2 * time.Hour

// resolveProbeTarget resolves host, returning the box's own DNS answer or
// nil when it cannot answer. Literal IPs pass straight through — they are
// the resolver-down fallback. Resolution is deliberately OUTSIDE the
// probed channel: a proxy with broken DNS must not fail a TCP probe that
// was never about DNS.
func resolveProbeTarget(ctx context.Context, host string) net.IP {
	if ip := net.ParseIP(host); ip != nil {
		return ip
	}
	probeDNSCache.Lock()
	if e, ok := probeDNSCache.m[host]; ok {
		if time.Since(e.at) < probeDNSSuccessTTL {
			probeDNSCache.Unlock()
			return e.ip
		}
		// Stale address: drop it so the lookup below re-resolves.
		delete(probeDNSCache.m, host)
	}
	if t, ok := probeDNSCache.fail[host]; ok && time.Since(t) < probeDNSFailTTL {
		probeDNSCache.Unlock()
		return nil
	}
	probeDNSCache.Unlock()

	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	if err != nil || len(addrs) == 0 {
		probeDNSCache.Lock()
		probeDNSCache.fail[host] = time.Now()
		probeDNSCache.Unlock()
		return nil
	}
	ip := addrs[0].AsSlice()
	probeDNSCache.Lock()
	probeDNSCache.m[host] = probeDNSCachedIP{ip: ip, at: time.Now()}
	delete(probeDNSCache.fail, host)
	probeDNSCache.Unlock()
	return ip
}

// probeTableThroughProxy runs one stage-1 pass against one proxy: sample a
// block of health targets, resolve each via the box's own DNS (outside the
// probed channel), dial :443 through the proxy via SOCKS5 CONNECT, count
// SynAcks, abort early only when the bar is mathematically unreachable.
//
// Credentials (user/password) are passed through to probeSocks5Connect so
// credentialed URL entries — usually the paid, higher-quality ones — are
// graded on the same evidence as everyone else instead of being convicted
// on a handshake they were never offered (finding H3).
func probeTableThroughProxy(ctx context.Context, address, user, password string, cfg proxyTableProbeConfig) tableProbeResult {
	pass := tableProbePassCounter.Load()
	hosts, _ := connect.SampleProbeTargets(tableProbeSeed(address, pass), cfg.SampleWidth)

	// res.SampleWidth is the number of targets the pass INTENDED to dial
	// (base block len(hosts), plus any adaptive growth). Unresolvable hosts
	// stay inside SampleWidth (they are part of the intended sample) but are
	// excluded from Total and from the score denominator, matching upstream's
	// Answered/Sent semantics: a DNS failure on the box is the box's problem,
	// not the proxy's, and must not convict it.
	res := tableProbeResult{SampleWidth: len(hosts), Failed: []string{}}
	baseUnresolved := 0
	extraUnresolved := 0

	// --- Base block (unchanged semantics) ---------------------------------
	for _, host := range hosts {
		if ctx.Err() != nil {
			break
		}
		ip := resolveProbeTarget(ctx, host)
		if ip == nil {
			baseUnresolved++
			continue
		}
		res.Total++
		if probeSocks5Connect(ctx, address, user, password, ip, 443, cfg.TargetTimeout) {
			res.OK++
		} else {
			res.Failed = append(res.Failed, host)
		}

		// Viability abort — the pass ends only when the verdict is already
		// decided: even if every remaining target succeeds, the bar is
		// unreachable. Viability is measured against the denominator the
		// score will actually use (attempted + still-untried), so hosts the
		// box's resolver cannot answer — which leave the score denominator —
		// can never abort a pass that could still qualify on its resolvable
		// targets (review #8). This preserves the no-convict guarantee
		// (finding H2): an aborted pass is never a biased sample, because
		// the only way to abort is to be unable to qualify. A good proxy that
		// fails a run of adjacent anti-bot targets walks the whole block and
		// is scored on its full pass.
		remaining := len(hosts) - res.Total - baseUnresolved
		best := float64(res.OK+remaining) / float64(res.Total+remaining)
		if best < cfg.PassBar {
			break
		}
	}

	// ADAPTIVE GROWTH (borderline-only). The base pass already yields a
	// decisive verdict for clearly-good and clearly-dead proxies. A proxy
	// whose base score lands within BorderlineBand of PassBar is genuinely
	// uncertain — the small sample cannot tell a mediocre-but-usable proxy
	// from a failing one. For exactly those we grow the sample up to
	// MaxSampleWidth using GROWTH BLOCKS DRAWN AT THE BASE WIDTH, so they are
	// provably disjoint from the base block AND from each other: sampleProbe
	// targets tiles the table by `(seed*width) % total`, so consecutive seeds
	// at the SAME width return consecutive, non-overlapping strides. Requesting
	// the growth block at a DIFFERENT width (`extra`) would break that tiling
	// and silently overlap the base ~20% of rows (Sonnet review MEDIUM B).
	// Successive same-width blocks are drained in order until `extra` distinct
	// hosts are gathered; the expanded pass therefore walks genuinely new
	// targets. Clearly-good and clearly-dead proxies never grow: paid probe
	// bandwidth is spent only in the uncertain middle. (Design 2026-08-23.)
	if cfg.MaxSampleWidth > cfg.SampleWidth && growthNeeded(res, cfg) {
		extra := cfg.MaxSampleWidth - res.SampleWidth
		extraHosts := disjointGrowthHosts(address, pass, cfg.SampleWidth, extra)
		grew := 0
		for _, host := range extraHosts {
			if ctx.Err() != nil {
				break
			}
			ip := resolveProbeTarget(ctx, host)
			if ip == nil {
				// Intended but not resolvable from this box; still counted so
				// the decidable quorum matches the base convention.
				res.SampleWidth++
				extraUnresolved++
				grew++
				continue
			}
			res.Total++
			res.SampleWidth++
			grew++
			if probeSocks5Connect(ctx, address, user, password, ip, 443, cfg.TargetTimeout) {
				res.OK++
			} else {
				res.Failed = append(res.Failed, host)
			}
			// Re-check viability at the grown denominator so an expansion can
			// never drift into a biased sample: if even a perfect finish of
			// the remaining grown block cannot clear the bar, stop spending.
			remainingExtra := extra - grew
			if remainingExtra < 0 {
				remainingExtra = 0
			}
			best := float64(res.OK+remainingExtra) / float64(res.Total+remainingExtra)
			if best < cfg.PassBar {
				break
			}
		}
	}

	// Decidable = the box's resolver let us ask a QUORUM of the intended
	// sample AND the context survived the pass. A pass interrupted by
	// cancellation carries no verdict (finding C1), and a pass whose sample
	// was gutted by the box's own DNS (fewer than half the intended targets
	// resolvable) is too thin to grade — a proxy that answered the only 2
	// resolvable hosts must not get a confident 1.0 (finding NEW-1). Both
	// cases leave the prior grade intact. The quorum is measured against
	// RESOLVABLE hosts, so a pass that ran to completion on a healthy
	// resolver is decidable even though the abort skipped the tail.
	resolvable := res.SampleWidth - baseUnresolved - extraUnresolved
	res.Decidable = resolvable >= (res.SampleWidth+1)/2 && res.Total > 0 && ctx.Err() == nil
	// Score is OK / ATTEMPTED (matching upstream's Answered/Sent): hosts the
	// box's own resolver could not answer are excluded from the denominator
	// exactly as they are excluded from the pass — a DNS failure on this box
	// must not convict a proxy that was never asked the question. The
	// viability abort already guarantees an aborted pass could not qualify,
	// so the smaller denominator never lets a truncated pass look better
	// than the evidence.
	if res.Total > 0 {
		res.Score = float64(res.OK) / float64(res.Total)
	}
	return res
}

// growthNeeded reports whether a base sample is borderline enough to warrant
// adaptive growth: the base score sits within BorderlineBand of PassBar, so
// the small sample cannot tell a mediocre-but-usable proxy from a failing
// one. Clearly-good scores (well above the top of the band) and clearly-dead
// scores (well below the bottom) return false — they are never grown, keeping
// probe bandwidth on the uncertain middle. A pass with no attempted sample
// (resolver outage) is not grown either.
//
// NOTE on viability-aborted bases: growth may still trigger for a base that
// hit the viability abort, as long as its score falls within the band. The
// abort means the base block's own remaining hosts could not recover it — but
// the GROWTH block is a fresh, wider host universe, so re-checking there can
// legitimately recover a proxy the marginal base block happened to under-serve.
// This is the adaptive intent (spend where the sample is genuinely uncertain),
// not a contradiction: an abort only marks the base block exhausted, and a
// score inside the band is precisely the uncertain case growth exists for.
func growthNeeded(res tableProbeResult, cfg proxyTableProbeConfig) bool {
	if res.Total <= 0 {
		return false
	}
	score := float64(res.OK) / float64(res.Total)
	lo := cfg.PassBar - cfg.BorderlineBand
	hi := cfg.PassBar + cfg.BorderlineBand
	return score >= lo && score <= hi
}

// disjointGrowthHosts returns up to `extra` DISTINCT hosts to add to a base
// probe block, drawn from consecutive SAME-WIDTH rotation strides so they are
// provably disjoint from the base block and from one another.
//
// sampleProbeTargets(seed, width) tiles the host table into disjoint
// `width`-wide strides: block start = (seed*width) % total. Consecutive seeds
// at the SAME width therefore land on consecutive non-overlapping strides.
// Walking seed+1, seed+2, ... at the base width collects genuinely new hosts
// (draining the table in order), stopping at `count`. This is what makes the
// adaptive growth block overlap-free — a call at a DIFFERENT width would
// break the stride tiling and collide with the base ~20% of the time (Sonnet
// review MEDIUM B).
func disjointGrowthHosts(address string, pass uint64, baseWidth, count int) []string {
	seen := map[string]bool{}
	var out []string
	// Walk the table in consecutive same-width strides (seed, seed+1, ...)
	// so each block is disjoint from the base and from every other growth
	// block, collecting distinct hosts until `count` are gathered. The seen
	// guard stays as a defensive net: the table is small (ProbeHostCount()
	// ~127), so enough consecutive steps can wrap around the boundary back
	// into already-visited hosts.
	for step := 1; len(out) < count; step++ {
		hosts, _ := connect.SampleProbeTargets(tableProbeSeed(address, pass)+uint64(step), baseWidth)
		if len(hosts) == 0 {
			break // table exhausted (guard; should not happen)
		}
		for _, h := range hosts {
			if seen[h] {
				continue
			}
			seen[h] = true
			out = append(out, h)
			if len(out) >= count {
				break
			}
		}
		if len(seen) >= connect.ProbeHostCount() {
			break // walked the whole table; no new hosts to give
		}
	}
	return out
}

// probeSocks5Connect dials the proxy, completes the SOCKS5 greeting (with
// RFC 1929 username/password sub-negotiation when the proxy requires it and
// credentials were supplied), and issues a CONNECT to ip:port, reporting
// whether the proxy answered with REP 0x00 (the SynAck-equivalent). One
// connection per target — a SOCKS5 CONNECT tunnel cannot be reused for a
// second destination.
//
// Both reads use io.ReadFull: a peer that sends a partial reply is not an
// answer (finding H1). The greeting method byte is inspected — a proxy that
// answers "no acceptable method" (0xFF) fails the greeting rather than
// proceeding into a CONNECT it will reject.
// maxProbeDialsPerSec caps how many per-target SOCKS5 CONNECT dials the whole
// provider may issue per second (across all concurrent table probes). 50 is a
// deliberately modest ceiling: high enough that a single proxy's adaptive
// probe (<= ~30 targets every few minutes) is unaffected, and low enough that
// even a full fleet sweep never thrashes target egress IPs or the box. This is
// the global dial rate limit from the probe-redesign: the per-tick budget caps
// HOW MANY proxies one pass probes; this caps how FAST their dials go out.
const maxProbeDialsPerSec = 50

// maxProbeDialBurst is the token-bucket burst for the global dial limiter: a
// small short-run allowance so a probe pass does not stall on its first few
// targets while the bucket refills.
const maxProbeDialBurst = 10

// globalProbeDialLimiter is the process-wide token bucket in front of every
// per-target CONNECT dial in probeSocks5Connect. One limiter covers all
// concurrent table probes (URL stage-1 and paid/file grading alike), so a
// large fleet sweep simply paces itself instead of bursting target-site
// connections in a scanning-like pattern from the proxy egress IPs. Design
// 2026-08-23.
var globalProbeDialLimiter = rate.NewLimiter(rate.Limit(maxProbeDialsPerSec), maxProbeDialBurst)

func probeSocks5Connect(ctx context.Context, address, user, password string, ip net.IP, port uint16, timeout time.Duration) bool {
	// Global dial rate limit, applied here (one call per sampled health
	// target, through the proxy). Wait blocks long enough to hold the
	// long-term ceiling flat; it is cancellable via ctx so a shutdown or a
	// timed-out caller is not held hostage by the bucket.
	if err := globalProbeDialLimiter.Wait(ctx); err != nil {
		return false
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", address)
	if err != nil {
		return false
	}
	defer conn.Close()

	if deadline, ok := dialCtx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}

	// Greeting: offer no-auth, plus username/password when we have BOTH
	// creds. socks5Greet validates the server's method selection (0x00, or
	// 0x02 with complete credentials) and runs the RFC 1929 sub-negotiation;
	// a server that picks a method we never offered is not an answer
	// (review #3).
	if !socks5Greet(conn, user, password) {
		return false
	}

	connectFrame := socks5ConnectV4(ip, port)
	if _, err := conn.Write(connectFrame); err != nil {
		return false
	}
	// The CONNECT reply is parsed by ATYP (IPv4/domain/IPv6), not by a
	// fixed length, so a short domain reply or an IPv6 BND.ADDR is handled
	// correctly; only a fully-consumed reply with REP 0x00 counts (review
	// #9/10).
	return readSocks5ConnectReply(conn)
}

// proxyURLGrade is the admission decision for one URL-source line after the
// full staged probe (stage 0 SOCKS5+API, stage 1 table).
type proxyURLGrade struct {
	// Qualified is true when the proxy cleared the stage-1 bar and may
	// enter the auth queue.
	Qualified bool
	// Socks5Only is true when stage 0 succeeded but the proxy failed the
	// API CONNECT — cached with ProbeOK=false for the reaper, never
	// admitted. Stage 1 never ran for these lines, so Decidable is false
	// and the merge loop must NOT persist a grade for them (finding C2).
	Socks5Only bool
	// Decidable mirrors tableProbeResult.Decidable: true only when a real
	// stage-1 verdict exists. False for socks5-only lines and for passes
	// that could not ask anything (cancelled/DNS-down).
	Decidable bool
	// Score and Failed record the stage-1 table probe for grading.
	Score  float64
	Failed []string
}

// probeAndGradeProxyURLLines runs the full staged probe over the lines:
// stage 0 (SOCKS5 + API CONNECT, via probeAndFilterProxyURLLines), then
// stage 1 (table probe) on survivors. Returns the grade per address. Lines
// that fail to parse or die at stage 0 get no grade entry (dropped).
//
// The caller advances tableProbePassCounter once per FETCH CYCLE (not once
// per source URL), so the rotation moves one block per cycle regardless of
// how many sources are configured (finding M1).
func probeAndGradeProxyURLLines(ctx context.Context, lines []string, apiHost string, apiPort uint16, cfg proxyTableProbeConfig) map[string]proxyURLGrade {
	apiOK, socks5Only := probeAndFilterProxyURLLines(ctx, lines, apiHost, apiPort)

	// Kill switch (Enabled=false): stage-1 grading is OFF. Every stage-0
	// survivor is treated as qualified (pre-feature behavior — URL proxies
	// admitted on the SOCKS5+API check alone) and no grades are recorded,
	// so the auth-time gate has nothing to enforce. This must be a full
	// skip of the table probe, not just a gate bypass: otherwise the
	// fetch path would still burn dial resources and write Score/Graded
	// for a feature that is supposedly disabled.
	if !cfg.Enabled {
		grades := make(map[string]proxyURLGrade, len(apiOK)+len(socks5Only))
		for _, line := range apiOK {
			address, _, _, ok := parseProxyURLLine(line)
			if !ok {
				continue
			}
			grades[address] = proxyURLGrade{Qualified: true}
		}
		for _, line := range socks5Only {
			address, _, _, ok := parseProxyURLLine(line)
			if !ok {
				continue
			}
			grades[address] = proxyURLGrade{Socks5Only: true}
		}
		return grades
	}

	// Stage 1: table probe the stage-0 survivors in parallel. Each pass is
	// bounded by the same pressure-scaled pool; a pass itself is sequential
	// per proxy (one connection at a time through that proxy) with
	// fail-fast. Survivors are de-duplicated by ADDRESS so a duplicate line
	// (bare and credentialed forms of the same endpoint) pays one pass, not
	// two (finding M3).
	survivorSet := make(map[string]bool, len(apiOK))
	survivorCreds := make(map[string]struct{ user, password string }, len(apiOK))
	for _, line := range apiOK {
		address, user, password, ok := parseProxyURLLine(line)
		if !ok {
			continue
		}
		survivorSet[address] = true
		creds := survivorCreds[address]
		if user != "" || password != "" {
			creds = struct{ user, password string }{user, password}
		}
		survivorCreds[address] = creds
	}
	survivors := make([]string, 0, len(survivorSet))
	for address := range survivorSet {
		survivors = append(survivors, address)
	}

	sem := make(chan struct{}, scaledProbeConcurrency(currentPressure()))
	stage1Results := make([]tableProbeResult, len(survivors))
	var wg sync.WaitGroup
	for i, address := range survivors {
		creds := survivorCreds[address]
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, address string, user, password string) {
			defer wg.Done()
			defer func() { <-sem }()
			stage1Results[i] = probeTableThroughProxy(ctx, address, user, password, cfg)
		}(i, address, creds.user, creds.password)
	}
	wg.Wait()

	grades := make(map[string]proxyURLGrade, len(lines))
	for i, tr := range stage1Results {
		g := proxyURLGrade{Score: tr.Score, Failed: tr.Failed, Decidable: tr.Decidable}
		if tr.qualified(cfg.PassBar) {
			g.Qualified = true
		}
		grades[survivors[i]] = g
	}
	for _, line := range socks5Only {
		address, _, _, ok := parseProxyURLLine(line)
		if !ok {
			continue
		}
		// Spoke SOCKS5 but failed the API CONNECT: cached ProbeOK=false for
		// the reaper, never admitted. Never graded (no stage-1 verdict).
		grades[address] = proxyURLGrade{Socks5Only: true}
	}
	return grades
}

// admissionStateTTL bounds how long the auth gate will reuse a parsed
// proxy_url.json snapshot. The cache file is written by fetch cycles (every
// ~15 minutes) and the reaper (every 5 minutes), so a few seconds of
// staleness is invisible; a TTL stops the gate from re-parsing the whole
// cache on every auth attempt inside the retry loop (finding M6).
const admissionStateTTL = 5 * time.Second

var admissionStateCache struct {
	sync.Mutex
	state *ProxyURLState
	at    time.Time
}

// resetAdmissionStateCache clears the TTL cache. Test-only: the auth gate
// otherwise reuses a snapshot for admissionStateTTL, which makes tests that
// write proxy_url.json then assert on the gate read stale state.
func resetAdmissionStateCache() {
	admissionStateCache.Lock()
	defer admissionStateCache.Unlock()
	admissionStateCache.state = nil
	admissionStateCache.at = time.Time{}
}

// errProxyURLBelowBar is the sentinel for a URL-source proxy rejected by its
// recorded stage-1 score. main.go's auth loop distinguishes it from a
// reachability failure so a quality rejection is never counted as an auth
// failure or give-up and can never trigger eviction (review #2).
var errProxyURLBelowBar = errors.New("proxy below stage-1 bar")

// cachedProxyURLState returns a parsed snapshot of proxy_url.json, reusing
// the previous parse within admissionStateTTL. On a read error with a
// previously-cached snapshot, the STALE snapshot is returned (with a
// warning) rather than failing closed: a transient filesystem hiccup must
// not brick the entire URL pool when a valid — if slightly old — state
// exists. Only when no state has ever been cached does the caller fail
// closed (finding M2).
//
// The returned *ProxyURLState and its Cache map are SHARED across every
// concurrent caller (all auth goroutines, fetch, reaper) and must be
// treated as READ-ONLY. The TTL cache hands out the same pointer until it
// expires; mutating the state or map from any caller is an unsynchronized
// write against the other readers (review #17).
func cachedProxyURLState() (*ProxyURLState, error) {
	admissionStateCache.Lock()
	defer admissionStateCache.Unlock()
	if admissionStateCache.state != nil && time.Since(admissionStateCache.at) < admissionStateTTL {
		return admissionStateCache.state, nil
	}
	state, err := readProxyURLState()
	if err != nil {
		if admissionStateCache.state != nil {
			tlog("[proxy][url] warning: could not re-read proxy_url.json (%v); using %v-old cached state\n",
				err, time.Since(admissionStateCache.at).Round(time.Second))
			return admissionStateCache.state, nil
		}
		return nil, err
	}
	admissionStateCache.state = state
	admissionStateCache.at = time.Now()
	return state, nil
}

// cachedProxyURLScore returns the recorded stage-1 score for an address
// from the TTL-cached state, and whether the entry is graded at all. Used
// by the auth path to name the real reason a proxy is being rejected.
func cachedProxyURLScore(address string) (float64, bool) {
	state, err := cachedProxyURLState()
	if err != nil {
		return 0, false
	}
	entry, ok := state.Cache[address]
	if !ok || !entry.Graded {
		return 0, false
	}
	return entry.Score, true
}

// urlProxyPassesAdmission is the auth-time gate for URL-sourced proxies: the
// recorded stage-1 score, when one exists, AND a cheap live SOCKS5 check. A
// proxy whose last recorded score is below the bar is rejected WITHOUT
// spending a dial or up to proxyProbeTimeout per auth attempt (review #16);
// entries at or above the bar, and ungraded entries (pre-upgrade caches, or
// addresses added outside the URL pipeline), are gated by the live probe.
//
// The kill switch (enabled=false) restores pre-stage-1 behavior entirely:
// the live probe is the only gate, exactly as before this feature shipped.
//
// On an unreadable cache the gate FAILS CLOSED with a loud log rather than
// admitting everything: a safety gate that quietly does nothing is worse
// than an absent one (finding M2).
func urlProxyPassesAdmission(ctx context.Context, address string) bool {
	cfg := resolveProxyTableProbeConfig()
	var user, password string
	if cfg.Enabled {
		state, err := cachedProxyURLState()
		if err != nil {
			tlog("[proxy][url] warning: could not read proxy_url.json for admission gate (%v); DENYING %s\n", err, address)
			return false
		}
		entry, ok := state.Cache[address]
		if ok && entry.Graded {
			// A recorded verdict below the bar is final until the next
			// re-grade: reject before dialing. (An entry scored 0.0 has
			// Graded=true and IS enforced — a zero score is a verdict, not
			// an absence of one.)
			if entry.Score < cfg.PassBar {
				return false
			}
		}
		if ok {
			user, password = entry.User, entry.Password
		}
		// Ungraded entries (no recorded verdict) fall through to the live
		// probe — nothing to enforce.
	} else {
		// Kill switch off: pre-stage-1 behavior. Best-effort credential
		// lookup so credentialed entries are live-probed on the same terms
		// stage 1 grades them (finding H3); a cache read failure simply
		// means no credentials, exactly as before the feature shipped.
		if state, err := cachedProxyURLState(); err == nil {
			if entry, ok := state.Cache[address]; ok {
				user, password = entry.User, entry.Password
			}
		}
	}
	// The live SOCKS5 check is the final gate. Credentials are passed
	// through so a credentialed proxy (which stage 1 graded WITH creds,
	// finding H3) is not convicted by a credential-less handshake — under
	// socks5Greet a server selecting 0x02 without creds fails immediately
	// (Opus review finding 4).
	if user == "" && password == "" {
		return probeProxySocks5(ctx, address, proxyProbeTimeout)
	}
	return probeProxy(ctx, address, user, password, "", 0) != probeDead
}

// describeProxyTableProbeConfig is for logs: a one-line dump of the
// effective stage-1 configuration.
func describeProxyTableProbeConfig(cfg proxyTableProbeConfig) string {
	return fmt.Sprintf("enabled=%v sample_width=%d max_sample_width=%d borderline_band=%.2f max_paid_probes_per_tick=%d timeout=%v pass_bar=%.2f preferred_bar=%.2f",
		cfg.Enabled, cfg.SampleWidth, cfg.MaxSampleWidth, cfg.BorderlineBand, cfg.MaxPaidProbesPerTick, cfg.TargetTimeout, cfg.PassBar, cfg.PreferredBar)
}
