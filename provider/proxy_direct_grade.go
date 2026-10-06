package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// Direct-path (native local-IP transport) health grading.
//
// The provider can run a DIRECT transport that reaches the internet from the
// box's own egress IP, alongside its proxies (directProxyKey, toggled by
// `provider direct on|off`). Nothing grades it today: proxy_trim explicitly
// skips directProxyKey, proxy audit only walks proxy_url.json, and the paid
// grader collects only from proxy.state and the proxy file. So an operator
// cannot tell whether a failing proxy is bad or the box's own route is bad.
//
// This adds a READ-ONLY grade for the direct path, sampled from the SAME
// destination table with the SAME sampler as the proxy probe, so the two scores
// are comparable as ESTIMATES of the same fraction (not paired — the proxy probe
// seeds on fnv(address)+pass and this one on its own counter, so they sample
// different hosts). "my own route reaches 11/12 of the table, my proxies reach
// 5/12" is the control that separates a bad proxy from a bad box.
//
// What it measures, precisely: EGRESS IPv4:443 reachability from this box. It is
// gated on the operator's CONFIG (the direct toggle file / env), i.e. desired
// state, not on whether the transport goroutine actually came up. And the
// transport's own product is INBOUND reachability of the box IP (NAT/CGNAT/
// firewall), which no outbound probe can see — so read the line as "this box's
// egress can reach the internet", not "the direct transport is healthy".
//
// SAFETY — structural, not a guard to remember: the result is written to its
// OWN file (direct_grade.json) and NEVER into proxy.state, so no existing
// consumer can act on it. Trim, audit and admission all read proxy.state or the
// proxy file; none of them read this. There is no eviction path to disable.
//
// The dial is deliberately NOT the proxy probe's. A proxy's health is "did the
// tunnel's SOCKS5 CONNECT reach the destination"; the direct path has no tunnel,
// so its health is "did a TCP connection from this box's own IP reach the
// destination". That is also why this pass is a simple fixed-width sample with
// no adaptive growth and no confirmation floor — it is a visibility metric, not
// a verdict that anything acts on, so spending extra dials to harden it would be
// pointless.

// directProbeOutcome classifies one direct-path dial. The three decidable
// outcomes are deliberately DISTINCT: a connection that completes but whose TLS
// handshake fails verification is neither a healthy route nor a dead one — it is
// the signature of a transparent interceptor, which is exactly what an
// unauthenticated TCP ACK cannot tell apart from a working route. The proxy probe
// does not need this distinction, because its CONNECT reply comes from the proxy
// over a tunnel the box does not control, so a local middlebox cannot forge it.
type directProbeOutcome int

const (
	directUnattempted directProbeOutcome = iota // box-side: limiter denial or caller deadline
	directUnreachable                           // the connection never completed
	directIntercepted                           // TCP completed, TLS verification failed
	directReachable                             // TCP completed and the TLS chain verified
)

// directTLSConfig builds the client config the direct pass verifies with. A var
// so a test can inject a CA, mirroring proxyProbeTLSClientConfig on the proxy side.
var directTLSConfig = func(serverName string) *tls.Config {
	return &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
}

// directProbeDialLimiter paces the direct pass on its OWN bucket. Sharing
// globalProbeDialLimiter coupled the two: a direct burst could deny a proxy pass
// a token, and a denial shrinks `remaining` in the proxy viability-abort
// arithmetic (best = (OK+remaining)/(Total+remaining), which DECREASES as
// remaining shrinks), so a proxy pass could be sealed at F one dial early by the
// direct pass's traffic. A dedicated bucket makes the isolation structural rather
// than merely low-probability.
var directProbeDialLimiter = rate.NewLimiter(rate.Limit(directProbeDialsPerSec), directProbeDialBurst)

const (
	directProbeDialsPerSec = 10
	// The burst must cover the widest sample an operator can set: the loader
	// clamps sample_width to 63, and a bucket smaller than the pass would make
	// the tail of a wide pass wait past the per-target timeout, gutting the
	// sample and silently degrading the line to (stale). The pass runs once per
	// sweep, so a burst this size costs nothing.
	directProbeDialBurst = 64
)

// directDial is the dialer the direct pass uses, as a var so a test can inject a
// deterministic offline dialer. A port override alone cannot keep a test off the
// network: the table's literal-IP rows are parsed before the DNS cache, so they
// would still dial the real address.
var directDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// directGrade is the persisted, read-only result of one direct-path pass.
type directGrade struct {
	Score       float64 `json:"score"`
	OK          int     `json:"ok"`
	Total       int     `json:"total"`
	SampleWidth int     `json:"sample_width"`
	Graded      bool    `json:"graded"`
	// Intercepted is how many sampled destinations completed a connection but
	// failed TLS verification. Reported, never folded into the score silently.
	Intercepted int       `json:"intercepted,omitempty"`
	LastGraded  time.Time `json:"last_graded"`
	Failed      []string  `json:"failed,omitempty"`
}

// directGradePath returns the direct-grade file path.
func directGradePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "direct_grade.json"), nil
}

// readDirectGrade returns the last direct grade, or ok=false when none has been
// written (never graded, unreadable, or corrupt — all "no evidence", never an
// error the caller must handle).
func readDirectGrade() (directGrade, bool) {
	p, err := directGradePath()
	if err != nil {
		return directGrade{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return directGrade{}, false
	}
	var g directGrade
	if json.Unmarshal(b, &g) != nil {
		return directGrade{}, false
	}
	return g, true
}

// writeDirectGrade persists the grade as PLAIN JSON via atomicWriteFile, so it
// round-trips through readDirectGrade with a plain Unmarshal. (atomicWriteJSON
// wraps its payload in a checksum envelope, which a plain reader would decode
// into a zero value — the writer and reader must agree on the shape.)
func writeDirectGrade(g directGrade) error {
	p, err := directGradePath()
	if err != nil {
		return err
	}
	b, err := json.Marshal(g)
	if err != nil {
		return err
	}
	return atomicWriteFile(p, b, 0600)
}

// probeDirectTarget dials ip:port from the box's own egress and reports whether
// the destination accepted the connection. attempted=false means the BOX could
// not send (limiter denial or caller deadline), which is not evidence about the
// route — the same positive-evidence-only rule the proxy probe uses.
func probeDirectTarget(ctx context.Context, ip net.IP, host string, port uint16, timeout time.Duration) (directProbeOutcome, bool) {
	waitCtx, cancelWait := context.WithTimeout(ctx, timeout)
	err := directProbeDialLimiter.Wait(waitCtx)
	cancelWait()
	if err != nil || ctx.Err() != nil {
		return directUnattempted, false
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := directDial(dialCtx, "tcp", net.JoinHostPort(ip.String(), fmt.Sprint(port)))
	if err != nil {
		return directUnreachable, true
	}
	defer conn.Close()

	// A VERIFIED TLS handshake with SNI is the evidence, not a bare TCP ACK: any
	// transparent interceptor completes a TCP handshake for every destination, so
	// a TCP-only pass would report a perfect score for a box with no egress.
	// A certificate-verification failure is its own outcome. A genuinely
	// misconfigured certificate lands there too, which is why the count is
	// reported rather than folded silently into the score.
	tlsConn := tls.Client(conn, directTLSConfig(host))
	if deadline, ok := dialCtx.Deadline(); ok {
		_ = tlsConn.SetDeadline(deadline)
	}
	if err := tlsConn.HandshakeContext(dialCtx); err != nil {
		var certErr *tls.CertificateVerificationError
		if errors.As(err, &certErr) {
			return directIntercepted, true
		}
		return directUnreachable, true
	}
	return directReachable, true
}

// directProbePassCounter advances once per direct pass so the grade walks the
// table like the proxy probe does. It must NOT share tableProbePassCounter: that
// one is advanced by the URL fetcher and the paid sweep, so on a node running
// only the direct transport it never moves and the direct pass would dial the
// same block forever.
var directProbePassCounter atomic.Uint64

// probeTableDirect runs one direct-path pass over a fixed-width sample of the
// health table, mirroring the proxy probe's scoring and decidability rules so
// the two scores are comparable.
func probeTableDirect(ctx context.Context, cfg proxyTableProbeConfig) tableProbeResult {
	width := cfg.SampleWidth
	if width <= 0 {
		// only reachable for a hand-built config; resolved configs always carry a width
		width = defaultProxyTableProbeConfig().SampleWidth
	}
	// advance FIRST so consecutive passes walk disjoint blocks; the proxy probe
	// seeds on fnv(address)+pass, so the two sample different hosts by design —
	// the scores are comparable as estimates of the same fraction, not paired.
	pass := directProbePassCounter.Add(1) - 1
	hosts := sampleProbeHosts(pass, width, cfg.UseSpreadOrder)

	res := tableProbeResult{SampleWidth: len(hosts), Failed: []string{}}
	unresolved := 0
	intercepted := 0
	for _, host := range hosts {
		if ctx.Err() != nil {
			break
		}
		ip := resolveProbeTarget(ctx, host)
		if ip == nil {
			unresolved++
			continue
		}
		outcome, attempted := probeDirectTarget(ctx, ip, host, 443, cfg.TargetTimeout)
		if !attempted {
			unresolved++
			continue
		}
		res.Total++
		switch outcome {
		case directReachable:
			res.OK++
		case directIntercepted:
			intercepted++
			res.Failed = append(res.Failed, host+" (intercepted)")
		default:
			res.Failed = append(res.Failed, host)
		}
	}
	// Same decidability rule as the proxy probe: a sample gutted by the box's
	// own resolver is too thin to report, and a cancelled pass carries no
	// verdict at all.
	resolvable := res.SampleWidth - unresolved
	res.Decidable = ctx.Err() == nil && res.SampleWidth > 0 && res.Total > 0 &&
		resolvable >= (res.SampleWidth+1)/2
	if res.Total > 0 {
		res.Score = float64(res.OK) / float64(res.Total)
	}
	res.Intercepted = intercepted
	return res
}

// runDirectGradeOnce performs one direct-path pass and persists it. It is a full
// skip when the table probe is killed (the probes themselves are then the
// problem) or when the direct transport is DISABLED IN CONFIG — the gate is
// desired state, not whether the transport goroutine actually came up.
func runDirectGradeOnce(ctx context.Context) {
	cfg := resolveProxyTableProbeConfig()
	if !cfg.Enabled {
		return
	}
	if !isDirectEnabled() {
		return
	}
	res := probeTableDirect(ctx, cfg)
	if !res.Decidable {
		// Absence of evidence is not evidence of absence: keep the previous
		// grade rather than writing a misleading one.
		return
	}
	g := directGrade{
		Score:       res.Score,
		OK:          res.OK,
		Total:       res.Total,
		SampleWidth: res.SampleWidth,
		Graded:      true,
		Intercepted: res.Intercepted,
		LastGraded:  time.Now(),
		Failed:      capFailedList(res.Failed),
	}
	// Emit only on a tier CHANGE: the summary carries the steady state, and an
	// unconditional line here would write ~288 near-identical important lines a
	// day on a healthy box (the paid grader keys its lines the same way).
	prev, hadPrev := readDirectGrade()
	if err := writeDirectGrade(g); err != nil {
		tlog("[proxy][grade] warning: could not write direct grade: %v\n", err)
		return
	}
	if !hadPrev || !prev.Graded || proxyGradeTier(prev.Score) != proxyGradeTier(res.Score) {
		importantLogf("[proxy][grade] direct: %s (score %.2f, %d/%d)\n",
			proxyGradeTier(res.Score), res.Score, res.OK, res.Total)
	}
}

// runDirectGrader drives the direct-path grade on the reaper ticker cadence, so
// it rides the same sweep window as the proxy grades.
func runDirectGrader(ctx context.Context) {
	ticker := time.NewTicker(proxyReaperInterval)
	defer ticker.Stop()
	// one immediate pass so a fresh process is not reported ungraded for a
	// whole tick (a pass that cannot decide writes nothing, so this is safe)
	runDirectGradeOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		runDirectGradeOnce(ctx)
	}
}

// directGradeLine renders the read-only direct-path grade for the summary. It
// returns the text WITHOUT the log prefix, which the caller applies like every
// sibling summary line. Display-only: nothing in the provider consumes the value.
func directGradeLine() string {
	if !isDirectEnabled() {
		return "direct: off"
	}
	g, ok := readDirectGrade()
	if !ok || !g.Graded {
		return "direct: (ungraded)"
	}
	// A grade older than a few sweep intervals is not a current reading: if the
	// transport was toggled off and back on, or passes keep failing to decide,
	// say so rather than presenting a historical score as today's.
	if time.Since(g.LastGraded) > 3*proxyReaperInterval {
		return "direct: (stale)"
	}
	ago := time.Since(g.LastGraded).Round(time.Minute)
	if ago < 0 {
		ago = 0 // a backward clock step must not render a negative age
	}
	if g.Intercepted > 0 {
		return fmt.Sprintf("direct: %s (score %.2f, %d/%d, %d intercepted, %s ago)",
			proxyGradeTier(g.Score), g.Score, g.OK, g.Total, g.Intercepted, ago)
	}
	return fmt.Sprintf("direct: %s (score %.2f, %d/%d, %s ago)",
		proxyGradeTier(g.Score), g.Score, g.OK, g.Total, ago)
}
