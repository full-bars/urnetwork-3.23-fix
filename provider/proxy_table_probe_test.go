package main

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// listenSocks5ConnectOnce starts a TCP listener that answers the SOCKS5
// greeting and every CONNECT with the given REP code (0x00 = success,
// 0x05 = connection refused). Used to exercise the stage-1 table probe's
// scoring against a deterministic fake proxy.
func listenSocks5ConnectOnce(t *testing.T, rep byte) (addr string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 3)
				if _, err := c.Read(buf); err != nil {
					return
				}
				c.Write([]byte{0x05, 0x00}) // greeting: no auth
				// read the CONNECT frame (10 bytes for ATYP=1)
				connectFrame := make([]byte, 10)
				if _, err := c.Read(connectFrame); err != nil {
					return
				}
				// VER REP RSV ATYP BND.ADDR(4) BND.PORT(2)
				resp := []byte{0x05, rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
				c.Write(resp)
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// TestProbeTableThroughProxy_AllSuccess: a proxy that answers every CONNECT
// with REP 0x00 must score 1.0 with ok == sample width and no failed
// targets.
func TestProbeTableThroughProxy_AllSuccess(t *testing.T) {
	addr, cleanup := listenSocks5ConnectOnce(t, 0x00)
	defer cleanup()

	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 4 // small block for the test
	cfg.TargetTimeout = time.Second

	res := probeTableThroughProxy(context.Background(), addr, "", "", "", 0, cfg)
	if res.Total != res.SampleWidth {
		t.Skipf("only %d of %d sampled targets resolved on this box (DNS); the full-success assertion needs every host to resolve", res.Total, res.SampleWidth)
	}
	if res.SampleWidth == 0 {
		t.Fatalf("expected a non-zero sample, got sample_width=0")
	}
	if res.OK != res.SampleWidth {
		t.Fatalf("expected ok==sample_width (%d==%d), got %+v", res.OK, res.SampleWidth, res)
	}
	if res.Score != 1.0 {
		t.Fatalf("expected score 1.0, got %v", res.Score)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("expected no failed targets, got %v", res.Failed)
	}
	if !res.Decidable {
		t.Fatal("a completed pass with attempts must be decidable")
	}
}

// TestProbeTableThroughProxy_AllFail: a proxy that answers every CONNECT
// with REP 0x05 must score 0.0 with all attempted targets failed.
func TestProbeTableThroughProxy_AllFail(t *testing.T) {
	addr, cleanup := listenSocks5ConnectOnce(t, 0x05)
	defer cleanup()

	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 4
	cfg.TargetTimeout = time.Second

	res := probeTableThroughProxy(context.Background(), addr, "", "", "", 0, cfg)
	if res.Total == 0 {
		t.Fatalf("expected a non-zero attempted sample, got total=0")
	}
	if res.OK != 0 {
		t.Fatalf("expected 0 successes, got %d", res.OK)
	}
	if res.Score != 0.0 {
		t.Fatalf("expected score 0.0, got %v", res.Score)
	}
	if len(res.Failed) != res.Total {
		t.Fatalf("expected all %d attempted targets failed, got %d failed: %v", res.Total, len(res.Failed), res.Failed)
	}
	if !res.Decidable {
		t.Fatal("a completed pass with attempts must be decidable even when everything failed")
	}
}

// TestProbeTableThroughProxy_ViabilityAbort: a dead proxy must abort early —
// once even a perfect finish cannot clear the bar, the pass is over. The
// aborted pass is still DECIDABLE (the verdict is already determined), and
// its score is computed against the full intended sample so the truncation
// cannot bias it downward.
func TestProbeTableThroughProxy_ViabilityAbort(t *testing.T) {
	addr := closedPortAddr(t)

	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 20
	cfg.PassBar = 0.6
	cfg.TargetTimeout = 100 * time.Millisecond

	// Seed ALL 20 sampled hosts with synthetic resolutions so the test runs
	// deterministically offline (no real DNS) and the abort fires exactly as
	// the arithmetic below predicts — the earlier skip-guard (Total != 20)
	// silently masked a real decidable-regression on partial-DNS boxes.
	pass := tableProbePassCounter.Load()
	hosts, _ := connect.SampleProbeTargets(tableProbeSeed(addr, pass), 20)
	probeDNSCache.Lock()
	for _, h := range hosts {
		probeDNSCache.m[h] = probeDNSCachedIP{ip: net.ParseIP("93.184.216.34"), at: time.Now()}
		delete(probeDNSCache.fail, h)
	}
	probeDNSCache.Unlock()
	t.Cleanup(func() {
		probeDNSCache.Lock()
		defer probeDNSCache.Unlock()
		for _, h := range hosts {
			delete(probeDNSCache.m, h)
		}
	})

	res := probeTableThroughProxy(context.Background(), addr, "", "", "", 0, cfg)
	if res.OK != 0 {
		t.Fatalf("expected 0 successes, got %d", res.OK)
	}
	// With every host resolvable the viability threshold is remaining/20 < 0.6
	// (best possible score on the attemptable denominator): with OK=0 the pass
	// aborts once remaining < 12, i.e. after 9 attempts (remaining 11 < 12 is
	// the first time the strict inequality holds).
	if res.Total != 9 {
		t.Fatalf("expected viability abort after 9 attempts (needed 12 of 20), got total=%d (walked: %+v)", res.Total, res)
	}
	if !res.Decidable {
		t.Fatalf("a viability-aborted pass IS decidable: the bar is unreachable, the verdict is determined (got SampleWidth=%d Total=%d Decidable=%v)", res.SampleWidth, res.Total, res.Decidable)
	}
	if res.Score != 0.0 {
		t.Fatalf("expected score 0.0, got %v", res.Score)
	}
}

// TestProbeTableThroughProxy_AdjacentFailuresNotBias: a proxy that fails a
// run of adjacent targets but answers the rest walks the FULL block — the
// viability abort only fires when the bar is unreachable, so a good proxy
// is never truncated at its failure run (finding H2).
func TestProbeTableThroughProxy_AdjacentFailuresNotBias(t *testing.T) {
	// Fails targets 3-6, answers everything else.
	repFor := func(n int) byte {
		if 3 <= n && n <= 6 {
			return 0x05
		}
		return 0x00
	}
	addr, _, cleanup := listenSocks5Sequenced(t, repFor)
	defer cleanup()

	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 12
	cfg.PassBar = 0.6
	cfg.TargetTimeout = time.Second

	res := probeTableThroughProxy(context.Background(), addr, "", "", "", 0, cfg)
	if res.Total < 10 {
		t.Skipf("only %d targets resolved on this box; need most of the block", res.Total)
	}
	if res.Total != res.SampleWidth {
		t.Fatalf("expected the full %d-target block to be walked, got total=%d (truncated at a failure run)", res.SampleWidth, res.Total)
	}
	if !res.qualified(cfg.PassBar) {
		t.Fatalf("a proxy failing only 4 adjacent targets must still qualify (8/12 = 0.667), got score=%v total=%d", res.Score, res.Total)
	}
}

// TestTableProbeSeed_Deterministic: the same (address, pass) must yield the
// same seed; different passes must yield different seeds so consecutive
// probes walk disjoint blocks of the table.
func TestTableProbeSeed_Deterministic(t *testing.T) {
	s1 := tableProbeSeed("1.2.3.4:1080", 0)
	s2 := tableProbeSeed("1.2.3.4:1080", 0)
	if s1 != s2 {
		t.Fatalf("seed must be deterministic: %d vs %d", s1, s2)
	}
	s3 := tableProbeSeed("1.2.3.4:1080", 1)
	if s1 == s3 {
		t.Fatalf("different passes must produce different seeds: %d == %d", s1, s3)
	}
}

// TestTableProbeResult_Qualified: the tiered bar from the config decides
// admission — score >= qualified bar passes, below it fails.
func TestTableProbeResult_Qualified(t *testing.T) {
	cfg := defaultProxyTableProbeConfig()
	cfg.PassBar = 0.6

	cases := []struct {
		score float64
		want  bool
	}{
		{1.0, true},
		{0.9, true},
		{0.6, true},
		{0.59, false},
		{0.0, false},
	}
	for _, c := range cases {
		res := tableProbeResult{Score: c.score, OK: int(c.score * 100), SampleWidth: 100, Total: 100, Decidable: true}
		if got := res.qualified(cfg.PassBar); got != c.want {
			t.Errorf("score %v: qualified=%v, want %v", c.score, got, c.want)
		}
	}
}

// TestTableProbeResult_Qualified_NoQuestionsAsked: a pass that asked nothing
// never qualifies anyone (zero denominator).
func TestTableProbeResult_Qualified_NoQuestionsAsked(t *testing.T) {
	res := tableProbeResult{Score: 1.0, OK: 0, Total: 0}
	if res.qualified(0.6) {
		t.Error("a pass with zero attempts must not qualify")
	}
}

// TestURLProxyPassesAdmission_UngradedPasses: entries without a recorded
// score (old cache, or proxies added outside the URL pipeline) must not be
// blocked by the stage-1 gate.
func TestURLProxyPassesAdmission_UngradedPasses(t *testing.T) {
	resetAdmissionStateCache()
	withTempHome(t)
	// no proxy_url.json at all -> nothing to look up -> the live greeting
	// probe is the only gate, and this address is a dead port.
	if urlProxyPassesAdmission(context.Background(), closedPortAddr(t)) {
		t.Error("expected dead proxy to fail admission even with no score on file")
	}
}

// TestURLProxyPassesAdmission_GradedZeroBlocked: a proxy that was graded
// and scored 0.0 (every table target failed) must be blocked by the
// stage-1 gate even though its score field is numerically zero — zero is a
// verdict, not "ungraded". This is the honeypot case: it passes the API
// CONNECT but can dial nothing else.
func TestURLProxyPassesAdmission_GradedZeroBlocked(t *testing.T) {
	resetAdmissionStateCache()
	withTempHome(t)
	// The live SOCKS5 probe would pass for a reachable proxy; the recorded
	// grade must still block it. Use a live fake so the probe is not the
	// reason for the failure.
	addr, cleanup := listenSocks5ConnectOnce(t, 0x00)
	defer cleanup()
	state := &ProxyURLState{
		Cache: map[string]ProxyURLEntry{
			addr: {Graded: true, Score: 0.0, ProbeOK: false},
		},
	}
	if err := writeProxyURLState(state); err != nil {
		t.Fatal(err)
	}
	if urlProxyPassesAdmission(context.Background(), addr) {
		t.Error("graded-zero proxy must be blocked by the stage-1 gate")
	}
}

// TestURLProxyPassesAdmission_GradedQualifiedPasses: a graded proxy at or
// above the bar is admitted (the live probe is the remaining gate).
func TestURLProxyPassesAdmission_GradedQualifiedPasses(t *testing.T) {
	resetAdmissionStateCache()
	withTempHome(t)
	addr, cleanup := listenSocks5ConnectOnce(t, 0x00)
	defer cleanup()
	if err := writeProxyURLState(&ProxyURLState{
		Cache: map[string]ProxyURLEntry{
			addr: {Graded: true, Score: 1.0, ProbeOK: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !urlProxyPassesAdmission(context.Background(), addr) {
		t.Error("qualified proxy must pass the stage-1 gate")
	}
}
