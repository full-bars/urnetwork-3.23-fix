package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests for the read-only direct-path grade (proxy_direct_grade.go).

// directSampleHosts returns the hosts a direct pass at the current pass counter
// will dial, split into literal-IP entries and DNS names.
//
// The split matters for testing: resolveProbeTarget parses a literal IP BEFORE
// consulting the DNS cache, so a test can point the NAMES at loopback but can
// never redirect a literal entry — those always dial the real address.
func directSampleHosts(cfg proxyTableProbeConfig) (literals, names []string) {
	// mirror the pass the grader is about to run: it uses its OWN counter, and
	// takes the pre-increment value, so Load() is exactly the pass it will use
	for _, h := range sampleProbeHosts(directProbePassCounter.Load(), cfg.SampleWidth, cfg.UseSpreadOrder) {
		if net.ParseIP(h) != nil {
			literals = append(literals, h)
		} else {
			names = append(names, h)
		}
	}
	return literals, names
}

// seedDirectDNS points the given names at ip and clears them on cleanup.
func seedDirectDNS(t *testing.T, names []string, ip string) {
	t.Helper()
	probeDNSCache.Lock()
	defer probeDNSCache.Unlock()
	for _, h := range names {
		probeDNSCache.m[h] = probeDNSCachedIP{ip: net.ParseIP(ip), at: time.Now()}
		delete(probeDNSCache.fail, h)
	}
	t.Cleanup(func() {
		probeDNSCache.Lock()
		defer probeDNSCache.Unlock()
		for _, h := range names {
			delete(probeDNSCache.m, h)
			delete(probeDNSCache.fail, h)
		}
	})
}

// directTestConfig installs an offline dialer and returns the config the pass
// will use. EVERY destination is dialed to a loopback listener, so the test never
// touches the network — including the table's literal-IP rows, which bypass the
// DNS cache and would otherwise dial the real address.
func directTestConfig(t *testing.T) proxyTableProbeConfig {
	t.Helper()
	cert, err := selfSignedForTest()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// Complete the handshake, THEN hold the connection open until the
				// client closes. Closing before the handshake finishes would make
				// every probe look unreachable.
				if tc, ok := c.(*tls.Conn); ok {
					if err := tc.Handshake(); err != nil {
						return
					}
				}
				_, _ = io.Copy(io.Discard, c)
			}(c)
		}
	}()

	saved := directDial
	directDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", ln.Addr().String())
	}
	t.Cleanup(func() { directDial = saved })

	// The pass now requires a VERIFIED TLS handshake, so the test seam must
	// supply a TLS endpoint and a config that accepts its self-signed cert. The
	// verification behaviour itself is exercised by
	// TestProbeTableDirect_InterceptedIsItsOwnOutcome.
	savedTLS := directTLSConfig
	directTLSConfig = func(serverName string) *tls.Config {
		return &tls.Config{ServerName: serverName, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
	}
	t.Cleanup(func() { directTLSConfig = savedTLS })

	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 6
	cfg.TargetTimeout = 300 * time.Millisecond
	return cfg
}

// selfSignedForTest mints a throwaway certificate so the direct pass has a TLS
// endpoint to handshake with.
func selfSignedForTest() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "direct-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// TestProbeTableDirect_InterceptedIsItsOwnOutcome pins the distinction the whole
// TLS change exists for: a connection that completes but whose certificate does
// NOT verify must be reported as intercepted, never counted as reachable. Under
// a plain TCP probe this box would have scored a perfect A.
//
// Falsifiable: make the probe count a completed TCP connect as reachable and this
// pass scores 1.0 instead of reporting interception.
func TestProbeTableDirect_InterceptedIsItsOwnOutcome(t *testing.T) {
	withTempHome(t)
	cfg := directTestConfig(t) // installs the TLS listener and a permissive seam
	// ...then REPLACE the seam with one that verifies against the system roots,
	// which the throwaway certificate cannot satisfy. directTLSConfig is a
	// package-level seam, so it MUST be restored or every later test inherits
	// strict verification.
	prev := directTLSConfig
	t.Cleanup(func() { directTLSConfig = prev })
	directTLSConfig = func(serverName string) *tls.Config {
		return &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
	}
	_, names := directSampleHosts(cfg)
	seedDirectDNS(t, names, "127.0.0.1")

	res := probeTableDirect(context.Background(), cfg)
	if res.OK != 0 {
		t.Errorf("a destination whose certificate does not verify must never count as reachable: OK=%d", res.OK)
	}
	if res.Intercepted == 0 {
		t.Errorf("expected the intercepted outcome to be recorded, got %+v", res)
	}
	if res.Intercepted != res.Total {
		t.Errorf("every attempted destination should be intercepted here: intercepted=%d total=%d",
			res.Intercepted, res.Total)
	}
}

// TestProbeTableDirect_ScoresReachableSample: every DNS name in the sample
// reaches the box's own egress, so the pass is decidable and every failure is a
// literal-IP entry the test could not redirect.
func TestProbeTableDirect_ScoresReachableSample(t *testing.T) {
	withTempHome(t)
	cfg := directTestConfig(t)
	literals, names := directSampleHosts(cfg)
	seedDirectDNS(t, names, "127.0.0.1")

	res := probeTableDirect(context.Background(), cfg)
	if !res.Decidable {
		t.Fatalf("expected a decidable direct pass, got %+v", res)
	}
	if res.Total != cfg.SampleWidth {
		t.Errorf("Total = %d, want the full %d-wide sample", res.Total, cfg.SampleWidth)
	}
	if res.OK < len(names) {
		t.Errorf("every seeded name should have been reachable: ok=%d names=%d failed=%v",
			res.OK, len(names), res.Failed)
	}
	// Every name was seeded to loopback and every dial is redirected there, so
	// nothing should have failed at all — the literals are redirected too now.
	if len(res.Failed) != 0 {
		t.Errorf("expected no failures with the dialer redirected to the test endpoint, got %v", res.Failed)
	}
	_ = literals
}

// TestProbeTableDirect_UnresolvedHostsAreExcluded: a name the box's own resolver
// cannot answer is excluded from the denominator (it is the box's problem, not
// the route's). Only literal-IP entries remain attemptable, and a sample that
// thin is not decidable.
func TestProbeTableDirect_UnresolvedHostsAreExcluded(t *testing.T) {
	withTempHome(t)
	cfg := directTestConfig(t)
	literals, names := directSampleHosts(cfg)

	probeDNSCache.Lock()
	for _, h := range names {
		delete(probeDNSCache.m, h)
		probeDNSCache.fail[h] = time.Now()
	}
	probeDNSCache.Unlock()
	t.Cleanup(func() {
		probeDNSCache.Lock()
		defer probeDNSCache.Unlock()
		for _, h := range names {
			delete(probeDNSCache.fail, h)
		}
	})

	res := probeTableDirect(context.Background(), cfg)
	if res.Total != len(literals) {
		t.Errorf("only literal-IP entries are attemptable here: Total=%d, want %d",
			res.Total, len(literals))
	}
	// the same quorum rule the proxy probe uses, so a gutted sample never reports
	quorum := (cfg.SampleWidth + 1) / 2
	if want := len(literals) >= quorum; res.Decidable != want {
		t.Errorf("Decidable = %v, want %v (resolvable=%d, quorum=%d)",
			res.Decidable, want, len(literals), quorum)
	}
}

// TestRunDirectGradeOnce_WritesItsOwnFileAndLeavesProxyStateAlone is the
// structural safety test for the whole feature: the direct grade is persisted to
// its own file and the direct path never creates or touches proxy.state, so no
// existing consumer (trim, audit, admission, the paid grader) can act on it.
func TestRunDirectGradeOnce_WritesItsOwnFileAndLeavesProxyStateAlone(t *testing.T) {
	home := withTempHome(t)
	t.Setenv("DISABLE_DIRECT_IP", "") // isDirectEnabled() must be true
	directTestConfig(t)               // installs the loopback port + cleanup

	// runDirectGradeOnce resolves its OWN config, so drive it through the
	// override file and mirror the result here — a local cfg would not be what
	// the run actually uses.
	writeReviewProbeOverride(t, map[string]any{"sample_width": 6, "timeout_ms": 300})
	cfg := resolveProxyTableProbeConfig()
	_, names := directSampleHosts(cfg)
	seedDirectDNS(t, names, "127.0.0.1")

	runDirectGradeOnce(context.Background())

	statePath := filepath.Join(home, ".urnetwork", "proxy.state")
	if _, err := os.Stat(statePath); err == nil {
		t.Fatalf("the direct grade must never create proxy.state (%s) — that is the file "+
			"trim/audit/admission read", statePath)
	}

	g, ok := readDirectGrade()
	if !ok || !g.Graded {
		t.Fatalf("expected a persisted direct grade, got ok=%v %+v", ok, g)
	}
	if g.Total != cfg.SampleWidth {
		t.Errorf("persisted Total = %d, want %d", g.Total, cfg.SampleWidth)
	}
	if g.OK < len(names) {
		t.Errorf("persisted OK = %d, want at least the %d reachable names", g.OK, len(names))
	}
	if want := float64(g.OK) / float64(g.Total); g.Score != want {
		t.Errorf("persisted score = %v, want %v", g.Score, want)
	}
	if g.LastGraded.IsZero() {
		t.Errorf("persisted grade must carry a timestamp")
	}
}

// TestRunDirectGradeOnce_SkipsWhenProbeDisabled: the stage-1 kill switch is
// global — if the probes themselves are the problem, the direct pass is a full
// skip too, and no file is written.
func TestRunDirectGradeOnce_SkipsWhenProbeDisabled(t *testing.T) {
	home := withTempHome(t)
	t.Setenv("DISABLE_DIRECT_IP", "")
	directTestConfig(t)

	gradePath := filepath.Join(home, ".urnetwork", "direct_grade.json")
	writeReviewProbeOverride(t, map[string]any{"enabled": false})
	if _, err := os.Stat(gradePath); err == nil {
		t.Fatalf("precondition: no grade file should exist yet")
	}
	runDirectGradeOnce(context.Background())
	if _, err := os.Stat(gradePath); err == nil {
		t.Fatalf("the direct pass must be a full skip when the table probe is disabled")
	}
}

// TestDirectGradeLine_OffAndUngraded pins the two display states that must not be
// confused with a real grade.
func TestDirectGradeLine_OffAndUngraded(t *testing.T) {
	withTempHome(t)

	// direct transport disabled by env -> "off", not "ungraded"
	t.Setenv("DISABLE_DIRECT_IP", "1")
	if got := directGradeLine(); got != "direct: off" {
		t.Errorf("disabled direct line = %q, want the off state", got)
	}

	// enabled but never graded -> "(ungraded)"
	t.Setenv("DISABLE_DIRECT_IP", "")
	if got := directGradeLine(); got != "direct: (ungraded)" {
		t.Errorf("ungraded direct line = %q, want the ungraded state", got)
	}
}

// TestProbeTableDirect_RotatesHostBlocks pins the direct pass's OWN rotation.
// It must not ride tableProbePassCounter: that counter is advanced by the URL
// fetcher and the paid sweep, so on a node running only the direct transport it
// never moves and the grade would dial the same block forever.
func TestProbeTableDirect_RotatesHostBlocks(t *testing.T) {
	withTempHome(t)
	cfg := directTestConfig(t)
	const w = 6
	cfg.SampleWidth = w

	start := directProbePassCounter.Load()
	b1 := sampleProbeHosts(start, w, cfg.UseSpreadOrder)
	b2 := sampleProbeHosts(start+1, w, cfg.UseSpreadOrder)
	if len(b1) == 0 || len(b2) == 0 {
		t.Fatal("empty block")
	}
	seen := map[string]bool{}
	for _, h := range b1 {
		seen[h] = true
	}
	overlap := 0
	for _, h := range b2 {
		if seen[h] {
			overlap++
		}
	}
	if overlap != 0 {
		t.Errorf("consecutive direct passes overlap by %d hosts; the counter must advance", overlap)
	}

	// and the grader must actually advance its own counter, once per pass
	seedDirectDNS(t, b1, "127.0.0.1")
	seedDirectDNS(t, b2, "127.0.0.1")
	before := directProbePassCounter.Load()
	probeTableDirect(context.Background(), cfg)
	probeTableDirect(context.Background(), cfg)
	if got := directProbePassCounter.Load() - before; got != 2 {
		t.Errorf("direct pass counter advanced by %d, want 2", got)
	}
}

// TestDirectGradeLine_StaleAfterSeveralIntervals: a grade older than a few sweep
// intervals must not be presented as a current reading (e.g. after the transport
// was toggled off and back on, or when passes keep failing to decide).
func TestDirectGradeLine_StaleAfterSeveralIntervals(t *testing.T) {
	withTempHome(t)
	t.Setenv("DISABLE_DIRECT_IP", "")

	base := directGrade{Score: 1.0, OK: 12, Total: 12, SampleWidth: 12, Graded: true}
	base.LastGraded = time.Now().Add(-4 * proxyReaperInterval)
	if err := writeDirectGrade(base); err != nil {
		t.Fatal(err)
	}
	if got := directGradeLine(); got != "direct: (stale)" {
		t.Errorf("stale direct line = %q, want the stale state", got)
	}

	base.LastGraded = time.Now()
	if err := writeDirectGrade(base); err != nil {
		t.Fatal(err)
	}
	if got := directGradeLine(); !strings.Contains(got, "A (score 1.00") {
		t.Errorf("fresh direct line = %q, want a real grade", got)
	}
}

// TestIsImportantLogLine_DirectGrade: the direct line must reach the /dev/shm
// important buffer in every state. No existing marker can match it — they all
// carry the "[proxy][grade] " prefix while this line continues with "direct: "
// — so the explicit marker is load-bearing.
func TestIsImportantLogLine_DirectGrade(t *testing.T) {
	for _, line := range []string{
		"[proxy][grade] direct: A (score 1.00, 12/12)",
		"[proxy][grade] direct: F (score 0.00, 0/12)",
		"[proxy][grade] direct: (ungraded)",
		"[proxy][grade] direct: (stale)",
		"[proxy][grade] direct: off",
	} {
		if !isImportantLogLine(line) {
			t.Errorf("direct grade line must reach the important buffer: %q", line)
		}
	}
}

// TestReadDirectGrade_ToleratesCorruptionAndZeroTimestamps: a corrupt file is
// "no evidence", never an error the caller must handle, and a grade with a zero
// timestamp must degrade to the stale state rather than rendering a bogus "ago".
func TestReadDirectGrade_ToleratesCorruptionAndZeroTimestamps(t *testing.T) {
	home := withTempHome(t)
	t.Setenv("DISABLE_DIRECT_IP", "")
	p := filepath.Join(home, ".urnetwork", "direct_grade.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}

	// corrupt bytes -> no evidence, and the summary reports ungraded
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readDirectGrade(); ok {
		t.Errorf("a corrupt file must read as no evidence")
	}
	if got := directGradeLine(); got != "direct: (ungraded)" {
		t.Errorf("corrupt grade line = %q, want the ungraded state", got)
	}

	// Graded=true with a zero timestamp -> stale, not a negative "ago"
	if err := writeDirectGrade(directGrade{Graded: true}); err != nil {
		t.Fatal(err)
	}
	g, ok := readDirectGrade()
	if !ok || !g.Graded {
		t.Fatalf("expected the grade to round-trip, got ok=%v %+v", ok, g)
	}
	if got := directGradeLine(); got != "direct: (stale)" {
		t.Errorf("zero-timestamp grade line = %q, want the stale state", got)
	}
}

// TestDirectGrade_RoundTrip: the persisted grade survives a read unchanged.
func TestDirectGrade_RoundTrip(t *testing.T) {
	withTempHome(t)
	want := directGrade{
		Score: 0.83, OK: 5, Total: 6, SampleWidth: 6, Graded: true,
		LastGraded: time.Now().Truncate(time.Second), Failed: []string{"a.example"},
	}
	if err := writeDirectGrade(want); err != nil {
		t.Fatal(err)
	}
	got, ok := readDirectGrade()
	if !ok {
		t.Fatal("expected a readable direct grade")
	}
	if got.Score != want.Score || got.OK != want.OK || got.Total != want.Total ||
		got.SampleWidth != want.SampleWidth || !got.Graded ||
		!got.LastGraded.Equal(want.LastGraded) {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}
