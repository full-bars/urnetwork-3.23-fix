package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
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
// are directly comparable. "my own route reaches 11/12 of the table, my proxies
// reach 5/12" is the control that separates a bad proxy from a bad box.
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
	Score       float64   `json:"score"`
	OK          int       `json:"ok"`
	Total       int       `json:"total"`
	SampleWidth int       `json:"sample_width"`
	Graded      bool      `json:"graded"`
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
func probeDirectTarget(ctx context.Context, ip net.IP, port uint16, timeout time.Duration) (answered, attempted bool) {
	waitCtx, cancelWait := context.WithTimeout(ctx, timeout)
	err := globalProbeDialLimiter.Wait(waitCtx)
	cancelWait()
	if err != nil || ctx.Err() != nil {
		return false, false
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := directDial(dialCtx, "tcp", net.JoinHostPort(ip.String(), fmt.Sprint(port)))
	if err != nil {
		return false, true
	}
	_ = conn.Close()
	return true, true
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
		width = 12
	}
	// advance FIRST so consecutive passes walk disjoint blocks; the proxy probe
	// seeds on fnv(address)+pass, so the two sample different hosts by design —
	// the scores are comparable as estimates of the same fraction, not paired.
	pass := directProbePassCounter.Add(1) - 1
	hosts := sampleProbeHosts(pass, width, cfg.UseSpreadOrder)

	res := tableProbeResult{SampleWidth: len(hosts), Failed: []string{}}
	unresolved := 0
	for _, host := range hosts {
		if ctx.Err() != nil {
			break
		}
		ip := resolveProbeTarget(ctx, host)
		if ip == nil {
			unresolved++
			continue
		}
		answered, attempted := probeDirectTarget(ctx, ip, 443, cfg.TargetTimeout)
		if !attempted {
			unresolved++
			continue
		}
		res.Total++
		if answered {
			res.OK++
		} else {
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
	return res
}

// runDirectGradeOnce performs one direct-path pass and persists it. It is a full
// skip when the table probe is killed (the probes themselves are then the
// problem) or when the direct transport is not running (nothing to measure).
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
		LastGraded:  time.Now(),
		Failed:      capFailedList(res.Failed),
	}
	if err := writeDirectGrade(g); err != nil {
		tlog("[proxy][grade] warning: could not write direct grade: %v\n", err)
		return
	}
	importantLogf("[proxy][grade] direct: %s (score %.2f, %d/%d)\n",
		proxyGradeTier(res.Score), res.Score, res.OK, res.Total)
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

// directGradeLine renders the read-only direct-path grade for the summary line.
// It is display-only: nothing in the provider consumes the value.
func directGradeLine() string {
	if !isDirectEnabled() {
		return "[proxy][grade] direct: off"
	}
	g, ok := readDirectGrade()
	if !ok || !g.Graded {
		return "[proxy][grade] direct: (ungraded)"
	}
	// A grade older than a few sweep intervals is not a current reading: if the
	// transport was toggled off and back on, or passes keep failing to decide,
	// say so rather than presenting a historical score as today's.
	if time.Since(g.LastGraded) > 3*proxyReaperInterval {
		return "[proxy][grade] direct: (stale)"
	}
	return fmt.Sprintf("[proxy][grade] direct: %s (score %.2f, %d/%d, %s ago)",
		proxyGradeTier(g.Score), g.Score, g.OK, g.Total,
		time.Since(g.LastGraded).Round(time.Minute))
}
