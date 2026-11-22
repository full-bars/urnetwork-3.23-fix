package connect

import (
	"strings"
	"testing"
)

// The backward credential scan is bounded per '@' and never walks a line twice,
// so its cost is linear in the length of the line. This counts the bytes the
// scan reads instead of timing it: a wall clock check flaked under -race and on
// a loaded host, and said nothing about the shape of the work.
func TestRedactProxyCredentialsScanStaysLinear(t *testing.T) {
	scanned := func(line string) (string, int) {
		stats := &proxyRedactStats{}
		out := redactProxyCredentials(line, stats)
		return out, stats.scanned
	}

	// an at-sign whose token is not an address costs no scan at all, however
	// many of them the line carries
	atSigns := strings.Repeat("u@x", 4096) + " end"
	if out, count := scanned(atSigns); out != atSigns || count != 0 {
		t.Fatalf("a line of non-address at-signs was walked: count=%d out==in:%t", count, out == atSigns)
	}

	const hostPort = "gw.example.net:1080"
	// one credential costs a few constant passes over its run and nothing more:
	// the scheme walk, the scheme run and the plain walk
	const passesPerCredential = 3
	// under the bound the whole run is read and the whole run goes
	for _, n := range []int{64, proxyCredentialScanLimit - 1, proxyCredentialScanLimit} {
		line := strings.Repeat("p", n) + "@" + hostPort
		out, count := scanned(line)
		if count > passesPerCredential*n {
			t.Fatalf("%d byte credential: scanned %d bytes", n, count)
		}
		if out != hostPort {
			t.Fatalf("%d byte credential: output %q", n, out)
		}
	}

	// just over the bound the scan stops at the bound, so the run in front of it
	// stays: one byte of the credential and the address are printed
	justOver := strings.Repeat("p", proxyCredentialScanLimit+1) + "@" + hostPort
	out, count := scanned(justOver)
	if count > passesPerCredential*proxyCredentialScanLimit {
		t.Fatalf("a credential over the bound scanned %d bytes, bound is %d", count, proxyCredentialScanLimit)
	}
	if want := strings.Repeat("p", 1) + hostPort; out != want {
		t.Fatalf("a credential just over the bound printed %q, want %q", out, want)
	}

	// and the cost grows with the line, not with the number of at-signs on it
	line := func(repeats int) string {
		return strings.Repeat("a:p@198.51.100.20:80 x@y ", repeats)
	}
	_, smallCount := scanned(line(1000))
	_, largeCount := scanned(line(16000))
	if smallCount == 0 {
		t.Fatal("the scaling input scanned nothing")
	}
	if limit := 16*smallCount + proxyCredentialScanLimit; largeCount > limit {
		t.Fatalf("16000 repeats scanned %d bytes against %d at 1000 (limit %d)", largeCount, smallCount, limit)
	}
}
