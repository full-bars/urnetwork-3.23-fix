package connect

import (
	"strings"
	"testing"
	"time"
)

// The backward credential scan is bounded per '@', so a line with a long run of
// at-signs whose tokens never name the proxy costs a linear pass, not a
// quadratic one. The bound is relative: the same code at sixteen times the
// length must not come out much slower than sixteen times the smaller run.
func TestRedactProxyEndpointLongPrefixScanStaysLinear(t *testing.T) {
	settings := proxyUserinfoSettings("gw.example.net:1080")
	fastest := func(repeats int) time.Duration {
		line := strings.Repeat("u@x", repeats) + " end"
		best := time.Duration(0)
		for range 3 {
			started := time.Now()
			got := RedactProxyEndpoint(line, settings)
			elapsed := time.Since(started)
			if got != line+" proxy[3]" {
				t.Fatalf("%d repeats changed the line: %q", repeats, got)
			}
			if best == 0 || elapsed < best {
				best = elapsed
			}
		}
		return best
	}
	small := fastest(1000)
	large := fastest(16000)
	t.Logf("1000 repeats %s, 16000 repeats %s", small, large)
	// a floor keeps a very fast small run from setting an impossible ratio
	if limit := 10*small + 10*time.Millisecond; large > limit {
		t.Fatalf("16000 repeats took %s against %s at 1000: the scan grows faster than the line (limit %s)", large, small, limit)
	}
}
