package connect

import (
	"runtime"
	"testing"
)

func TestHostMemoryReaders(t *testing.T) {
	total, tok := HostMemoryTotalBytes()
	avail, aok := HostMemoryAvailableBytes()
	switch runtime.GOOS {
	case "linux", "darwin", "windows", "freebsd":
	default:
		if tok || aok {
			t.Fatalf("expected unavailable on %s, got total=%v avail=%v", runtime.GOOS, tok, aok)
		}
		return
	}
	if !tok || total <= 0 {
		t.Fatalf("total unreadable: %d %v", total, tok)
	}
	if !aok || avail < 0 {
		t.Fatalf("available unreadable: %d %v", avail, aok)
	}
	if avail > total {
		t.Fatalf("available %d exceeds total %d", avail, total)
	}
}

func TestEffectiveRAMLimitUsesHostTotalWhenNoCgroup(t *testing.T) {
	v, _ := EffectiveRAMLimit()
	if v <= 0 {
		t.Fatalf("non-positive limit %d", v)
	}
}

// TestEffectiveRAMLimitKnownDistinguishesFallbackFromRealReading pins the
// source-based classification the memory-headroom threshold depends on. The
// unreadable-host fallback is 850 MiB, which is also a plausible real host, so
// the byte count alone cannot identify it.
func TestEffectiveRAMLimitKnownDistinguishesFallbackFromRealReading(t *testing.T) {
	const fallbackBytes = 850 * 1024 * 1024

	if ramLimitKnown(fallbackBytes, "fallback default, host memory unreadable") {
		t.Fatalf("the %d-byte unreadable placeholder reported as a real reading", fallbackBytes)
	}
	// A real 850 MiB host differs from the placeholder only in its source, and
	// must not be mistaken for it.
	for _, source := range []string{
		"host MemTotal, no cgroup limit found",
		"cgroup v2 memory.max at /",
		"cgroup v1 memory.limit_in_bytes",
	} {
		if !ramLimitKnown(fallbackBytes, source) {
			t.Fatalf("a real 850 MiB reading from %q reported as unknown", source)
		}
	}
	// Sub-1-MiB values truncate to 0 MiB in the headroom arithmetic and cannot
	// be a host total or a cgroup limit, so they are corrupt reads too.
	for _, v := range []int64{1, 512, 1<<20 - 1} {
		if ramLimitKnown(v, "host MemTotal, no cgroup limit found") {
			t.Fatalf("implausible %d-byte reading reported as known", v)
		}
	}
	if !ramLimitKnown(1<<20, "host MemTotal, no cgroup limit found") {
		t.Fatal("a 1 MiB reading must be known")
	}
	if v, _ := EffectiveRAMLimitKnown(); v <= 0 {
		t.Fatalf("EffectiveRAMLimitKnown returned non-positive %d", v)
	}
}
