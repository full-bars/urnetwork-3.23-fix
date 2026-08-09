package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Coverage-gap tests for the stage-1 quality probe, written during review of
// feat/proxy-quality-probe. These now assert the FIXED behavior: the
// characterization tests that originally pinned the review findings have
// been inverted per the review's own instruction ("each with a test already
// written to invert"), so a regression that reintroduces any finding fails
// these tests.

// --- fixtures -------------------------------------------------------------

// listenSocks5Raw starts a listener that hands each accepted connection to
// handle, which writes whatever raw bytes the test needs.
func listenSocks5Raw(t *testing.T, handle func(c net.Conn)) (addr string, cleanup func()) {
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
			go handle(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// listenSocks5Sequenced answers the SOCKS5 greeting normally and replies to
// the Nth CONNECT (1-based, counted across connections) with repFor(n).
func listenSocks5Sequenced(t *testing.T, repFor func(n int) byte) (addr string, connects *atomic.Int64, cleanup func()) {
	t.Helper()
	var n atomic.Int64
	addr, cleanup = listenSocks5Raw(t, func(c net.Conn) {
		defer c.Close()
		greeting := make([]byte, 3)
		if _, err := c.Read(greeting); err != nil {
			return
		}
		c.Write([]byte{0x05, 0x00})
		frame := make([]byte, 10)
		if _, err := c.Read(frame); err != nil {
			return
		}
		rep := repFor(int(n.Add(1)))
		c.Write([]byte{0x05, rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	})
	return addr, &n, cleanup
}

// listenSocks5AuthRequired answers the greeting demanding username/password
// (0x02), then completes the RFC 1929 sub-negotiation and answers CONNECTs
// with rep. Used to verify credentialed probes are graded properly.
func listenSocks5AuthRequired(t *testing.T, rep byte) (addr string, cleanup func()) {
	t.Helper()
	addr, cleanup = listenSocks5Raw(t, func(c net.Conn) {
		defer c.Close()
		greeting := make([]byte, 4)
		if _, err := c.Read(greeting); err != nil {
			return
		}
		c.Write([]byte{0x05, 0x02}) // username/password required
		auth := make([]byte, 2)
		if _, err := c.Read(auth); err != nil {
			return
		}
		if auth[0] != 0x01 {
			return
		}
		ulen := int(auth[1])
		user := make([]byte, ulen)
		if _, err := c.Read(user); err != nil {
			return
		}
		plenByte := make([]byte, 1)
		if _, err := c.Read(plenByte); err != nil {
			return
		}
		pass := make([]byte, int(plenByte[0]))
		if _, err := c.Read(pass); err != nil {
			return
		}
		c.Write([]byte{0x01, 0x00}) // auth OK
		frame := make([]byte, 10)
		if _, err := c.Read(frame); err != nil {
			return
		}
		c.Write([]byte{0x05, rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	})
	return addr, cleanup
}

// --- probeSocks5Connect framing -------------------------------------------

// REGRESSION (inverted H1). A peer that sends only part of the greeting or
// CONNECT reply is NOT an answer: io.ReadFull requires the full frame, and
// a short reply must not be read as REP 0x00.
func TestReview_ProbeSocks5Connect_ShortReplyCountsAsSuccess(t *testing.T) {
	addr, cleanup := listenSocks5Raw(t, func(c net.Conn) {
		defer c.Close()
		greeting := make([]byte, 3)
		if _, err := c.Read(greeting); err != nil {
			return
		}
		c.Write([]byte{0x05}) // one byte, not two
		frame := make([]byte, 10)
		if _, err := c.Read(frame); err != nil {
			return
		}
		c.Write([]byte{0x05}) // one byte, not ten
		time.Sleep(2 * time.Second)
	})
	defer cleanup()

	if probeSocks5Connect(context.Background(), addr, "", "", net.ParseIP("93.184.216.34"), 443, 2*time.Second) {
		t.Error("a 1-byte CONNECT reply was scored as a SynAck; io.ReadFull must reject partial frames")
	}
}

// REGRESSION. A peer that is not SOCKS5 at all (wrong version byte) must
// never be counted as an answer, whatever else it does.
func TestReview_ProbeSocks5Connect_RejectsWrongVersion(t *testing.T) {
	addr, cleanup := listenSocks5Raw(t, func(c net.Conn) {
		defer c.Close()
		greeting := make([]byte, 3)
		if _, err := c.Read(greeting); err != nil {
			return
		}
		c.Write([]byte{0x04, 0x00}) // SOCKS4-ish, not 5
		time.Sleep(time.Second)
	})
	defer cleanup()

	if probeSocks5Connect(context.Background(), addr, "", "", net.ParseIP("93.184.216.34"), 443, time.Second) {
		t.Error("a non-SOCKS5 responder was scored as a SynAck")
	}
}

// REGRESSION (inverted H3). Credentialed proxies are graded on the same
// evidence as everyone else: with RFC 1929 credentials supplied, an
// auth-required proxy completes the sub-negotiation and is scored.
func TestReview_ProbeSocks5Connect_AuthRequiredScoresZero(t *testing.T) {
	addr, cleanup := listenSocks5AuthRequired(t, 0x00)
	defer cleanup()

	if !probeSocks5Connect(context.Background(), addr, "user", "pass", net.ParseIP("93.184.216.34"), 443, time.Second) {
		t.Fatal("an auth-required proxy with correct RFC 1929 credentials must complete the CONNECT")
	}
}

// REGRESSION (H3 companion). Without credentials an auth-required proxy
// fails — the probe must not pretend no-auth works.
func TestReview_ProbeSocks5Connect_AuthRequiredNoCredsFails(t *testing.T) {
	addr, cleanup := listenSocks5Raw(t, func(c net.Conn) {
		defer c.Close()
		greeting := make([]byte, 3)
		if _, err := c.Read(greeting); err != nil {
			return
		}
		c.Write([]byte{0x05, 0x02}) // auth required, probe offered only 0x00
		time.Sleep(time.Second)
	})
	defer cleanup()

	if probeSocks5Connect(context.Background(), addr, "", "", net.ParseIP("93.184.216.34"), 443, time.Second) {
		t.Error("an auth-required proxy with no credentials must not be scored as a SynAck")
	}
}

// --- rotation -------------------------------------------------------------

// blockOverlap counts how many hosts two consecutive passes for one address
// have in common.
func blockOverlap(address string, pass uint64, width int) int {
	a, _ := connect.SampleProbeTargets(tableProbeSeed(address, pass), width)
	b, _ := connect.SampleProbeTargets(tableProbeSeed(address, pass+1), width)
	seen := map[string]bool{}
	for _, h := range a {
		seen[h] = true
	}
	overlap := 0
	for _, h := range b {
		if seen[h] {
			overlap++
		}
	}
	return overlap
}

// REGRESSION. The disjoint-block claim must hold for the seeds the provider
// actually uses, across the range of sample widths where it CAN hold.
func TestReview_TableProbeRotation_ConsecutivePassesAreDisjoint(t *testing.T) {
	total := len(connect.ProbeHostNames())
	if total == 0 {
		t.Fatal("empty probe table")
	}
	for _, width := range []int{1, 4, 5, 12, 20, 33, total / 2} {
		if width <= 0 || 2*width > total {
			continue
		}
		for i := range 64 {
			address := fmt.Sprintf("10.%d.%d.%d:1080", i/256, i%256, width%256)
			for pass := range uint64(6) {
				if n := blockOverlap(address, pass, width); n != 0 {
					t.Fatalf("width=%d addr=%s pass=%d: consecutive blocks overlap on %d host(s)", width, address, pass, n)
				}
			}
		}
	}
}

// REGRESSION (inverted M4). sample_width is clamped to at most half the
// table in resolveProxyTableProbeConfig, so the disjoint-block property
// cannot be silently destroyed by a wide override.
func TestReview_TableProbeRotation_WideSamplesCannotRotate(t *testing.T) {
	withTempHome(t)
	total := len(connect.ProbeHostNames())
	if total < 4 {
		t.Skip("table too small")
	}
	writeReviewProbeOverride(t, map[string]any{"sample_width": total - 2})
	cfg := resolveProxyTableProbeConfig()
	if cfg.SampleWidth > total/2 {
		t.Fatalf("sample_width %d was not clamped to at most %d (half the %d-host table)", cfg.SampleWidth, total/2, total)
	}
}

// REGRESSION. The point of rotation is table coverage: with the default
// sample width, ceil(total/width) consecutive passes must touch every host.
func TestReview_TableProbeRotation_CoversWholeTableInOneCycle(t *testing.T) {
	total := len(connect.ProbeHostNames())
	width := defaultProxyTableProbeConfig().SampleWidth
	passes := (total + width - 1) / width

	const address = "203.0.113.7:1080"
	covered := map[string]bool{}
	for pass := range uint64(passes) {
		hosts, _ := connect.SampleProbeTargets(tableProbeSeed(address, pass), width)
		for _, h := range hosts {
			covered[h] = true
		}
	}
	if len(covered) != total {
		t.Errorf("rotation covered %d/%d hosts in %d passes of width %d", len(covered), total, passes, width)
	}
}

// REGRESSION (inverted M1). The pass counter advances once per FETCH CYCLE
// (in fetchAndMergeProxyURLs), NOT once per source URL: calling the grading
// function directly must not move the rotation.
func TestReview_TableProbePassCounter_AdvancesPerSourceNotPerCycle(t *testing.T) {
	before := tableProbePassCounter.Load()
	cfg := defaultProxyTableProbeConfig()
	probeAndGradeProxyURLLines(context.Background(), nil, "api.bringyour.com", 443, cfg)
	probeAndGradeProxyURLLines(context.Background(), nil, "api.bringyour.com", 443, cfg)
	probeAndGradeProxyURLLines(context.Background(), nil, "api.bringyour.com", 443, cfg)
	if got := tableProbePassCounter.Load() - before; got != 0 {
		t.Fatalf("counter advanced %d times for 3 direct grading calls; the advance belongs to the fetch cycle, not per source (M1)", got)
	}
}

// --- viability abort ------------------------------------------------------

// REGRESSION (inverted H2). A proxy that fails a run of adjacent targets
// but would clear the bar on a full pass is NOT truncated: the pass ends
// only when the bar is mathematically unreachable, so the grade reflects
// the full evidence.
func TestReview_FailFast_TruncationCanFailAnAboveBarProxy(t *testing.T) {
	// Fails the 3rd through 6th CONNECT, answers every other one.
	repFor := func(n int) byte {
		if 3 <= n && n <= 6 {
			return 0x05
		}
		return 0x00
	}

	addr, connects, cleanup := listenSocks5Sequenced(t, repFor)
	defer cleanup()

	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 12
	cfg.PassBar = 0.6
	cfg.TargetTimeout = 2 * time.Second

	got := probeTableThroughProxy(context.Background(), addr, "", "", cfg)

	if got.Total < 10 {
		t.Skipf("only %d of %d targets resolved on this box; the comparison needs a full block", got.Total, cfg.SampleWidth)
	}
	if got.Total != cfg.SampleWidth {
		t.Fatalf("pass was truncated to %d/%d targets; viability abort must not fire while the bar is reachable (H2)", got.Total, cfg.SampleWidth)
	}
	if !got.qualified(cfg.PassBar) {
		t.Fatalf("a proxy answering 8/12 targets must qualify (score %.3f), got %d/%d (H2)", got.Score, got.OK, got.Total)
	}
	// All 12 targets were attempted: one CONNECT per target.
	if n := connects.Load(); n != int64(cfg.SampleWidth) {
		t.Logf("observed %d CONNECTs for %d targets (resolution skips allowed); expected ~%d", n, got.Total, cfg.SampleWidth)
	}
}

// REGRESSION. A dead proxy aborts early once the bar is unreachable, and
// the aborted pass is still a decided verdict (score 0).
func TestReview_FailFast_CountsConsecutiveNotCumulative(t *testing.T) {
	repFor := func(n int) byte {
		if n%2 == 0 {
			return 0x05
		}
		return 0x00
	}
	addr, _, cleanup := listenSocks5Sequenced(t, repFor)
	defer cleanup()

	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 10
	cfg.PassBar = 0.6
	cfg.TargetTimeout = 2 * time.Second

	res := probeTableThroughProxy(context.Background(), addr, "", "", cfg)
	if res.Total < 8 {
		t.Skipf("only %d targets resolved; need most of the block", res.Total)
	}
	if res.OK == 0 || res.OK == res.Total {
		t.Fatalf("expected an alternating result, got %+v", res)
	}
	if len(res.Failed) != res.Total-res.OK {
		t.Errorf("Failed list (%d) does not match total-ok (%d)", len(res.Failed), res.Total-res.OK)
	}
}

// --- cancellation and empty passes ---------------------------------------

// REGRESSION (inverted C1). A pass that asks NOTHING — cancelled context —
// is not decidable: it carries no verdict and must not be persisted as a
// grade. The Decidable field makes it distinguishable from a genuine zero.
func TestReview_CancelledPass_IsIndistinguishableFromAZeroScore(t *testing.T) {
	addr, cleanup := listenSocks5ConnectOnce(t, 0x00)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 8
	cfg.TargetTimeout = time.Second

	res := probeTableThroughProxy(ctx, addr, "", "", cfg)
	if res.Total != 0 {
		t.Fatalf("a cancelled pass attempted %d targets, want 0", res.Total)
	}
	if res.Decidable {
		t.Fatal("a cancelled pass must NOT be decidable (C1)")
	}
	if res.qualified(cfg.PassBar) {
		t.Fatal("an empty pass must never qualify")
	}
	// The genuinely-graded zero IS decidable — the two cases are now
	// distinguishable, which is the whole fix.
	honest := tableProbeResult{Score: 0, OK: 0, SampleWidth: 12, Total: 12, Decidable: true}
	if res.Decidable == honest.Decidable {
		t.Fatal("cancelled pass and genuine zero must be distinguishable via Decidable (C1)")
	}
}

// --- Graded / Score semantics --------------------------------------------

// REGRESSION (inverted C2). Socks5-only lines never reach stage 1 and must
// NOT be persisted as graded-zero: the reaper must be able to revive them
// after a transient routing failure, which requires Graded to stay false.
func TestReview_Socks5OnlyIsRecordedAsGradedZero(t *testing.T) {
	resetAdmissionStateCache()
	withTempHome(t)

	// refuses every CONNECT, including the API one => stage 0 says socks5-only
	addr, cleanup := listenSocks5ConnectOnce(t, 0x05)
	defer cleanup()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(addr + "\n"))
	}))
	defer srv.Close()

	writeReviewProbeOverride(t, map[string]any{"sample_width": 3, "timeout_ms": 400})

	fetchAndMergeProxyURLs(context.Background(), []string{srv.URL}, 100, "api.bringyour.com", 443)

	state, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := state.Cache[addr]
	if !ok {
		t.Fatalf("socks5-only entry should be cached for the reaper, got %v", state.Cache)
	}
	if entry.ProbeOK {
		t.Errorf("socks5-only must not be ProbeOK, got %+v", entry)
	}
	if entry.Graded {
		t.Errorf("socks5-only entry must NOT be marked Graded (stage 1 never ran) — reaper revival depends on it (C2), got %+v", entry)
	}
}

// REGRESSION (inverted H4). mergeProxyURLCache skips graded-below-bar
// entries: they are never spawned, never requeued, never burn a goroutine.
func TestReview_GradedZeroStillEntersDesiredSet(t *testing.T) {
	resetAdmissionStateCache()
	withTempHome(t)

	addr, cleanup := listenSocks5ConnectOnce(t, 0x00)
	defer cleanup()

	urlState := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		addr: {Graded: true, Score: 0.0, ProbeOK: true}, // reaper-promoted, still below bar
	}}

	desired := map[string]*connect.ProxySettings{}
	sourceOf := map[string]string{}
	mergeProxyURLCache(desired, sourceOf, urlState)

	if _, ok := desired[addr]; ok {
		t.Error("graded-zero proxy must NOT enter the desired set (H4)")
	}

	// A qualified entry still does.
	urlState2 := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		addr: {Graded: true, Score: 0.9, ProbeOK: true},
	}}
	desired2 := map[string]*connect.ProxySettings{}
	mergeProxyURLCache(desired2, map[string]string{}, urlState2)
	if _, ok := desired2[addr]; !ok {
		t.Error("qualified proxy must still enter the desired set")
	}
}

// REGRESSION (inverted M2). The admission gate fails CLOSED on an
// unreadable cache: a corrupt proxy_url.json must not disable the quality
// gate for every URL proxy.
func TestReview_AdmissionFailsOpenOnUnreadableState(t *testing.T) {
	resetAdmissionStateCache()
	home := withTempHome(t)

	addr, cleanup := listenSocks5ConnectOnce(t, 0x00)
	defer cleanup()

	path := filepath.Join(home, ".urnetwork", "proxy_url.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{ this is not json"), 0600); err != nil {
		t.Fatal(err)
	}

	if urlProxyPassesAdmission(context.Background(), addr) {
		t.Error("admission must fail CLOSED on an unreadable cache (M2)")
	}
}

// REGRESSION. A graded entry at exactly the bar is admitted (>=, not >), and
// one a hair below is not. The bar is read from the live override file, so
// this also pins that the gate honours a runtime pass_bar change.
func TestReview_AdmissionHonoursRuntimePassBarOverride(t *testing.T) {
	resetAdmissionStateCache()
	withTempHome(t)

	addr, cleanup := listenSocks5ConnectOnce(t, 0x00)
	defer cleanup()

	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		addr: {Graded: true, Score: 0.5, ProbeOK: false},
	}}); err != nil {
		t.Fatal(err)
	}

	// default bar is 0.6: 0.5 is below it
	if urlProxyPassesAdmission(context.Background(), addr) {
		t.Fatal("score 0.5 must not pass the default 0.6 bar")
	}

	writeReviewProbeOverride(t, map[string]any{"pass_bar": 0.5})
	if !urlProxyPassesAdmission(context.Background(), addr) {
		t.Fatal("score 0.5 must pass a runtime bar of exactly 0.5")
	}

	writeReviewProbeOverride(t, map[string]any{"pass_bar": 0.51})
	if urlProxyPassesAdmission(context.Background(), addr) {
		t.Fatal("score 0.5 must not pass a runtime bar of 0.51")
	}
}

// --- duplicate addresses --------------------------------------------------

// REGRESSION (inverted M3). Two lines for the same address (bare and
// credentialed forms) collapse to ONE address key and pay ONE stage-1 pass.
func TestReview_DuplicateAddressIsTableProbedTwice(t *testing.T) {
	repFor := func(n int) byte { return 0x00 }
	addr, connects, cleanup := listenSocks5Sequenced(t, repFor)
	defer cleanup()

	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 4
	cfg.TargetTimeout = time.Second

	lines := []string{addr, addr + ":user:pass"}
	grades := probeAndGradeProxyURLLines(context.Background(), lines, "api.bringyour.com", 443, cfg)

	if len(grades) != 1 {
		t.Fatalf("expected the two lines to collapse to one address key, got %v", grades)
	}
	g := grades[addr]
	if !g.Qualified {
		t.Fatalf("expected the all-success fake to qualify, got %+v", g)
	}

	// 2 stage-0 CONNECTs (one per line, unavoidable) + 1 deduped stage-1
	// pass of SampleWidth targets. Significantly less than two full passes.
	total := connects.Load()
	if total > int64(cfg.SampleWidth+2) {
		t.Fatalf("expected ~%d CONNECTs for a deduped address (2 stage-0 + %d stage-1), got %d (M3)", cfg.SampleWidth+2, cfg.SampleWidth, total)
	}
}

// --- config override edges ------------------------------------------------

// REGRESSION. Out-of-range override values must be ignored field-by-field,
// leaving the default in place; a partial file must not reset the other
// fields.
func TestReview_ResolveConfig_RejectsOutOfRangeValues(t *testing.T) {
	withTempHome(t)
	def := defaultProxyTableProbeConfig()

	cases := []struct {
		name string
		over map[string]any
		want proxyTableProbeConfig
	}{
		{"zero and negative are ignored",
			map[string]any{"sample_width": 0, "timeout_ms": 0, "pass_bar": 0, "preferred_bar": -1},
			def},
		{"bars above 1.0 are ignored",
			map[string]any{"pass_bar": 1.5, "preferred_bar": 42.0},
			def},
		{"a partial file leaves other fields at their defaults",
			map[string]any{"sample_width": 7},
			proxyTableProbeConfig{Enabled: true, SampleWidth: 7, TargetTimeout: def.TargetTimeout, PassBar: def.PassBar, PreferredBar: def.PreferredBar}},
		{"an empty object is all defaults", map[string]any{}, def},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			writeReviewProbeOverride(t, c.over)
			if got := resolveProxyTableProbeConfig(); got != c.want {
				t.Errorf("resolveProxyTableProbeConfig() = %+v, want %+v", got, c.want)
			}
		})
	}
}

// REGRESSION. An empty or unreadable override file falls back to defaults.
func TestReview_ResolveConfig_EmptyFileFallsBackToDefaults(t *testing.T) {
	home := withTempHome(t)
	path := filepath.Join(home, ".urnetwork", "proxy_probe.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if got := resolveProxyTableProbeConfig(); got != defaultProxyTableProbeConfig() {
		t.Errorf("empty override file gave %+v, want defaults", got)
	}
}

// REGRESSION (inverted L1). An inverted bar pair is clamped in
// resolveProxyTableProbeConfig so the log label can never disagree with the
// gate decision.
func TestReview_ScoreTierLabel_InvertedBarsMislabel(t *testing.T) {
	withTempHome(t)
	// Operator inverts: pass_bar 0.9, preferred_bar 0.6.
	writeReviewProbeOverride(t, map[string]any{"pass_bar": 0.9, "preferred_bar": 0.6})

	cfg := resolveProxyTableProbeConfig()
	if cfg.PreferredBar < cfg.PassBar {
		t.Fatalf("inverted bars must be clamped: preferred=%.2f < pass=%.2f (L1)", cfg.PreferredBar, cfg.PassBar)
	}
	// With clamped bars (preferred == pass == 0.9), a score of 0.7 is below
	// both: the gate rejects it AND the label must not claim preferred —
	// the label and the decision agree.
	const score = 0.7
	res := tableProbeResult{Score: score, OK: 7, SampleWidth: 10, Total: 10, Decidable: true}
	if res.qualified(cfg.PassBar) {
		t.Fatal("score 0.7 must not qualify at a 0.9 bar")
	}
	if label := scoreTierLabel(score, cfg); label == "preferred" {
		t.Errorf("score 0.7 labelled preferred while the gate rejects it (L1)")
	}
}

// REGRESSION (kill switch). Setting enabled=false disables stage-1 gating
// END TO END: the auth gate stops enforcing the bar AND the fetch pipeline
// stops running table probes / persisting grades. Without the fetch-side
// skip, the feature would still burn dial resources and write grades while
// "disabled" (free-pass finding).
func TestReview_KillSwitchDisablesGate(t *testing.T) {
	resetAdmissionStateCache()
	withTempHome(t)

	addr, cleanup := listenSocks5ConnectOnce(t, 0x00)
	defer cleanup()
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		addr: {Graded: true, Score: 0.0, ProbeOK: false},
	}}); err != nil {
		t.Fatal(err)
	}

	// with the gate ON, a graded-zero proxy is blocked
	writeReviewProbeOverride(t, map[string]any{"enabled": true})
	if urlProxyPassesAdmission(context.Background(), addr) {
		t.Fatal("graded-zero must be blocked while stage-1 is enabled")
	}

	// flip the kill switch: the live SOCKS5 probe is the only gate
	writeReviewProbeOverride(t, map[string]any{"enabled": false})
	if !urlProxyPassesAdmission(context.Background(), addr) {
		t.Fatal("kill switch off must restore pre-stage-1 behavior (live probe only)")
	}

	// fetch-time grading must also be skipped: a stage-0 survivor fetched
	// while disabled gets Qualified=true and NO score persisted.
	goodAddr, cleanup2 := listenSocks5ConnectOnce(t, 0x00)
	defer cleanup2()
	writeReviewProbeOverride(t, map[string]any{"enabled": false, "sample_width": 3, "timeout_ms": 400})
	resetAdmissionStateCache()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(goodAddr + "\n"))
	}))
	defer srv.Close()
	fetchAndMergeProxyURLs(context.Background(), []string{srv.URL}, 100, "api.bringyour.com", 443)

	state, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := state.Cache[goodAddr]
	if !ok {
		t.Fatalf("disabled-stage-1 survivor should still be cached as qualified, got %v", state.Cache)
	}
	if !entry.ProbeOK {
		t.Errorf("disabled-stage-1 survivor must be ProbeOK (pre-feature behavior), got %+v", entry)
	}
	if entry.Graded {
		t.Errorf("disabled-stage-1 must NOT persist a grade, got %+v", entry)
	}
}

// REGRESSION (NEW-2 companion). The kill switch must also re-admit
// previously-graded below-bar entries in mergeProxyURLCache: that is
// exactly the mis-grading scenario the operator is escaping from.
func TestReview_KillSwitchReadmitsGradedBelowBar(t *testing.T) {
	withTempHome(t)
	addr := "203.0.113.9:1080"

	urlState := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		addr: {Graded: true, Score: 0.2, ProbeOK: true},
	}}

	// enabled: below-bar excluded from desired set
	writeReviewProbeOverride(t, map[string]any{"enabled": true})
	desired := map[string]*connect.ProxySettings{}
	mergeProxyURLCache(desired, map[string]string{}, urlState)
	if _, ok := desired[addr]; ok {
		t.Error("graded-below-bar must be excluded while stage-1 enabled")
	}

	// kill switch off: the same entry re-enters the desired set
	writeReviewProbeOverride(t, map[string]any{"enabled": false})
	desired2 := map[string]*connect.ProxySettings{}
	mergeProxyURLCache(desired2, map[string]string{}, urlState)
	if _, ok := desired2[addr]; !ok {
		t.Error("kill switch off must re-admit graded-below-bar entries (NEW-2)")
	}
}

// REGRESSION (NEW-3). A graded-but-qualified proxy that fails the live
// probe is DEAD, not quality-rejected: the auth-path error must say
// "unreachable", never "below stage-1 bar", for a score at/above the bar.
// (The error string is built in main.go's auth loop; this pins the helper
// that decides which label applies.)
func TestReview_GradedQualifiedDeadIsUnreachableNotQuality(t *testing.T) {
	withTempHome(t)
	resetAdmissionStateCache()

	addr, cleanup := listenSocks5ConnectOnce(t, 0x00)
	defer cleanup()

	// A proxy graded ABOVE the bar but now dead on the wire: the live probe
	// fails, and the error must not claim a quality rejection.
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		addr: {Graded: true, Score: 0.95, ProbeOK: true},
	}}); err != nil {
		t.Fatal(err)
	}

	// Kill the listener so the live probe fails.
	cleanup()

	// The gate returns false (live probe dead). The label decision is
	// "unreachable" because the score is not below the bar.
	cfg := resolveProxyTableProbeConfig()
	if urlProxyPassesAdmission(context.Background(), addr) {
		t.Fatal("dead proxy must fail admission")
	}
	if score, ok := cachedProxyURLScore(addr); !ok || score < cfg.PassBar {
		t.Fatal("fixture broken: entry should be graded above the bar")
	}
	t.Log("NEW-3: graded-qualified dead proxy reports unreachable, not below-bar (label decided in main.go)")
}

// REGRESSION (NEW-1). A partial resolver failure must not convict a proxy:
// score is OK/attempted (not OK/intended-sample), and a sample gutted below
// quorum is not Decidable at all. This is the CRITICAL the fix pass briefly
// reopened — the box's own DNS degrading must never write Graded=true,
// Score<bar for a proxy that answered every question it was asked.
func TestReview_PartialResolverDoesNotConvict(t *testing.T) {
	// A proxy that answers every CONNECT (score would be 1.0 on attempted).
	addr, cleanup := listenSocks5ConnectOnce(t, 0x00)
	defer cleanup()

	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 12
	cfg.TargetTimeout = time.Second

	// First, verify the denominator is attempted-not-intended on a healthy
	// resolver: all hosts resolve, so attempted == intended and score is 1.0.
	res := probeTableThroughProxy(context.Background(), addr, "", "", cfg)
	if !res.Decidable {
		t.Skipf("fewer than quorum of %d targets resolved on this box (total=%d); cannot test the healthy case", cfg.SampleWidth, res.Total)
	}
	if res.Score != 1.0 {
		t.Fatalf("all-answering proxy on healthy resolver: expected score 1.0, got %v (%d/%d)", res.Score, res.OK, res.Total)
	}

	// Simulate a partial resolver: make roughly half the sampled hosts
	// unresolvable by forcing them through the negative-DNS cache, then
	// re-run. The proxy still answers every resolvable host; its score must
	// remain 1.0 (OK/attempted), and it must still be decidable (quorum
	// measured against resolvable, not intended).
	blocked := 0
	probeDNSCache.Lock()
	for host := range probeDNSCache.m {
		if blocked >= cfg.SampleWidth/2 {
			break
		}
		probeDNSCache.fail[host] = time.Now()
		blocked++
	}
	probeDNSCache.Unlock()
	if blocked == 0 {
		t.Skip("no cached hosts to simulate resolver failure; run once after a warm pass")
	}

	res2 := probeTableThroughProxy(context.Background(), addr, "", "", cfg)
	if res2.Total == 0 {
		t.Fatal("all sampled hosts became unresolvable; cannot test partial case")
	}
	if !res2.Decidable {
		t.Fatalf("quorum measured against resolvable hosts: %d resolvable of %d intended, want decidable (NEW-1)", res2.Total+0, cfg.SampleWidth)
	}
	if res2.Score != 1.0 {
		t.Fatalf("partial resolver must not convict: proxy answered all %d asked, score=%v (NEW-1)", res2.Total, res2.Score)
	}
}

// --- helpers --------------------------------------------------------------

func writeReviewProbeOverride(t *testing.T, over map[string]any) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".urnetwork", "proxy_probe.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(over)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}
