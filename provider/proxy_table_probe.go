package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
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
//     mathematically unreachable (OK + remaining < needed), so an aborted
//     pass can never look worse than the evidence supports — the failure
//     run that triggered the abort is the same one that would have failed
//     the full pass.
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
}

// defaultProxyTableProbeConfig returns the stock configuration. SampleWidth
// 12, 4s per target, tiered 0.9/0.6.
func defaultProxyTableProbeConfig() proxyTableProbeConfig {
	return proxyTableProbeConfig{
		Enabled:       true,
		SampleWidth:   12,
		TargetTimeout: 4 * time.Second,
		PassBar:       0.6,
		PreferredBar:  0.9,
	}
}

// proxyProbeOverridePath returns ~/.urnetwork/proxy_probe.json, a runtime
// override file an operator can write (or delete) without restarting. The
// JSON shape matches proxyTableProbeConfig:
//
//	{"enabled": true, "sample_width": 12, "timeout_ms": 4000,
//	 "pass_bar": 0.6, "preferred_bar": 0.9}
//
// Missing or malformed keys fall back to defaults.
func proxyProbeOverridePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "proxy_probe.json"), nil
}

// resolveProxyTableProbeConfig re-reads the override file on every call and
// returns the effective configuration. Absent/unparseable file or fields
// fall back to defaults; an explicitly-set field wins over the default.
func resolveProxyTableProbeConfig() proxyTableProbeConfig {
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
		Enabled      *bool    `json:"enabled"`
		SampleWidth  *int     `json:"sample_width"`
		TimeoutMS    *int     `json:"timeout_ms"`
		PassBar      *float64 `json:"pass_bar"`
		PreferredBar *float64 `json:"preferred_bar"`
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
	// Clamp sample width so the disjoint-block rotation property holds
	// (two blocks of n out of a table of total are disjoint only when
	// 2n <= total). Upstream's default width is the whole table; a wide
	// override silently destroys the property, so clamp and say so —
	// ONCE, not once per auth attempt (finding NEW-6: this resolver runs
	// on the auth hot path).
	if maxWidth := len(connect.ProbeHostNames()) / 2; cfg.SampleWidth > maxWidth {
		cfg.SampleWidth = maxWidth
		clampWarnings.Do(func() {
			tlog("[proxy][url] stage-1: sample_width clamped to %d (half the %d-host table; disjoint rotation requires 2*width <= table)\n",
				maxWidth, len(connect.ProbeHostNames()))
		})
	}
	// An inverted bar pair would let the log label ("preferred") disagree
	// with the gate decision. Clamp PreferredBar up to PassBar.
	if cfg.PreferredBar < cfg.PassBar {
		cfg.PreferredBar = cfg.PassBar
		clampWarnings.Do(func() {
			tlog("[proxy][url] stage-1: preferred_bar clamped up to pass_bar %.2f (inverted pair)\n", cfg.PassBar)
		})
	}
	return cfg
}

// clampWarnings dedupes the config-clamp log lines so they print once per
// process instead of once per auth attempt (finding NEW-6).
var clampWarnings sync.Once

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
	// Score is OK/SampleWidth at or above which the proxy is qualified.
	// The denominator is the INTENDED sample, not the attempted subset:
	// a pass aborted by fail-fast or cancellation therefore can never look
	// worse than the evidence supports (unattempted targets are unknown,
	// not failures, but they are also not counted as successes).
	Score float64
	// OK is how many sampled targets answered with a SynAck.
	OK int
	// SampleWidth is the intended sample size (the denominator of Score).
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

// probeDNSCache memoizes target resolution for the lifetime of the box.
// The table is ~127 hosts, so one lookup per host per process covers every
// proxy and every cycle, and stage 0 already caches the API address the
// same way ("so each probe doesn't trigger a fresh DNS lookup"). Failures
// are memoized too (with a short TTL) so a resolver degradation does not
// re-issue the same failing lookup per proxy per pass (finding NEW-8).
var probeDNSCache = struct {
	sync.Mutex
	m    map[string]net.IP
	fail map[string]time.Time
}{m: map[string]net.IP{}, fail: map[string]time.Time{}}

// probeDNSFailTTL is how long a failed resolution is remembered before the
// box retries it. Long enough to absorb a whole fetch cycle of probes,
// short enough that a recovered resolver is noticed.
const probeDNSFailTTL = 30 * time.Second

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
	if ip, ok := probeDNSCache.m[host]; ok {
		probeDNSCache.Unlock()
		return ip
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
	probeDNSCache.m[host] = ip
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

	res := tableProbeResult{SampleWidth: len(hosts), Failed: []string{}}
	// The minimum successes needed to clear the bar on the FULL intended
	// sample. When even a perfect finish cannot reach it, the pass is over.
	needed := int(math.Ceil(cfg.PassBar * float64(len(hosts))))
	unresolved := 0

	for _, host := range hosts {
		if ctx.Err() != nil {
			break
		}
		ip := resolveProbeTarget(ctx, host)
		if ip == nil {
			// Not the proxy's fault; excluded from the pass.
			unresolved++
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
		// unreachable. This is the no-convict guarantee (finding H2): an
		// aborted pass is never a biased sample, because the only way to
		// abort is to be unable to qualify. A good proxy that fails a run
		// of adjacent anti-bot targets walks the whole block and is scored
		// on its full pass.
		if res.OK+(len(hosts)-res.Total-unresolved) < needed {
			break
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
	resolvable := len(hosts) - unresolved
	res.Decidable = resolvable >= (len(hosts)+1)/2 && res.Total > 0 && ctx.Err() == nil
	// Score is OK / ATTEMPTED (matching upstream's Answered/Sent): hosts
	// the box's own resolver could not answer are excluded from the
	// denominator exactly as they are excluded from the pass — a DNS
	// failure on this box must not convict a proxy that was never asked
	// the question. The viability abort already guarantees an aborted pass
	// could not qualify, so the smaller denominator never lets a truncated
	// pass look better than the evidence.
	if res.Total > 0 {
		res.Score = float64(res.OK) / float64(res.Total)
	}
	return res
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
func probeSocks5Connect(ctx context.Context, address, user, password string, ip net.IP, port uint16, timeout time.Duration) bool {
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

	// Greeting: offer no-auth, plus username/password when we have creds.
	// RFC 1929 bounds each field at 255 bytes; longer credentials would
	// truncate silently on the wire and make a working proxy look dead, so
	// reject them outright.
	if len(user) > 255 || len(password) > 255 {
		return false
	}
	greeting := []byte{0x05, 0x01, 0x00}
	if user != "" || password != "" {
		greeting = []byte{0x05, 0x02, 0x00, 0x02}
	}
	if _, err := conn.Write(greeting); err != nil {
		return false
	}
	greetingResp := make([]byte, 2)
	if _, err := io.ReadFull(conn, greetingResp); err != nil {
		return false
	}
	if greetingResp[0] != 0x05 {
		return false
	}
	if greetingResp[1] == 0xFF {
		// "no acceptable method"
		return false
	}
	if greetingResp[1] == 0x02 {
		// RFC 1929 sub-negotiation.
		auth := []byte{0x01, byte(len(user))}
		auth = append(auth, []byte(user)...)
		auth = append(auth, byte(len(password)))
		auth = append(auth, []byte(password)...)
		if _, err := conn.Write(auth); err != nil {
			return false
		}
		authResp := make([]byte, 2)
		if _, err := io.ReadFull(conn, authResp); err != nil {
			return false
		}
		if authResp[0] != 0x01 || authResp[1] != 0x00 {
			return false
		}
	}

	connectFrame := socks5ConnectV4(ip, port)
	if _, err := conn.Write(connectFrame); err != nil {
		return false
	}
	connectResp := make([]byte, 10)
	if _, err := io.ReadFull(conn, connectResp); err != nil {
		return false
	}
	return len(connectResp) >= 2 && connectResp[0] == 0x05 && connectResp[1] == 0x00
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

// cachedProxyURLState returns a parsed snapshot of proxy_url.json, reusing
// the previous parse within admissionStateTTL. On a read error with a
// previously-cached snapshot, the STALE snapshot is returned (with a
// warning) rather than failing closed: a transient filesystem hiccup must
// not brick the entire URL pool when a valid — if slightly old — state
// exists. Only when no state has ever been cached does the caller fail
// closed (finding M2).
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

// urlProxyPassesAdmission is the auth-time gate for URL-sourced proxies: a
// cheap live SOCKS5 check (existing behavior) AND the recorded stage-1
// score, when one exists. A proxy whose last recorded score is below the
// bar never spends an auth-rate-limiter slot, even if it happens to be
// reachable right now. Ungraded entries (pre-upgrade caches, or addresses
// added outside the URL pipeline) are gated only by the live probe.
//
// The kill switch (enabled=false) restores pre-stage-1 behavior entirely:
// the live probe is the only gate, exactly as before this feature shipped.
//
// On an unreadable cache the gate FAILS CLOSED with a loud log rather than
// admitting everything: a safety gate that quietly does nothing is worse
// than an absent one (finding M2).
func urlProxyPassesAdmission(ctx context.Context, address string) bool {
	if !probeProxySocks5(ctx, address, proxyProbeTimeout) {
		return false
	}
	cfg := resolveProxyTableProbeConfig()
	if !cfg.Enabled {
		// Kill switch off: stage-1 gating is disabled, live probe only.
		return true
	}
	state, err := cachedProxyURLState()
	if err != nil {
		tlog("[proxy][url] warning: could not read proxy_url.json for admission gate (%v); DENYING %s\n", err, address)
		return false
	}
	entry, ok := state.Cache[address]
	if !ok || !entry.Graded {
		// No recorded stage-1 grade — nothing to enforce. The live probe is
		// the only gate. (An entry scored 0.0 has Graded=true and IS
		// enforced — a zero score is a verdict, not an absence of one.)
		return true
	}
	return entry.Score >= cfg.PassBar
}

// scoreTierLabel returns a human label for a stage-1 score. The bars are
// validated (PreferredBar >= PassBar) in resolveProxyTableProbeConfig, so
// the label can never disagree with the gate.
func scoreTierLabel(score float64, cfg proxyTableProbeConfig) string {
	if score >= cfg.PreferredBar {
		return "preferred"
	}
	if score >= cfg.PassBar {
		return "qualified"
	}
	return "below-bar"
}

// describeProxyTableProbeConfig is for logs: a one-line dump of the
// effective stage-1 configuration.
func describeProxyTableProbeConfig(cfg proxyTableProbeConfig) string {
	return fmt.Sprintf("enabled=%v sample_width=%d timeout=%v pass_bar=%.2f preferred_bar=%.2f",
		cfg.Enabled, cfg.SampleWidth, cfg.TargetTimeout, cfg.PassBar, cfg.PreferredBar)
}
