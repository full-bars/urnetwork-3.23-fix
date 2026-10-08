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
