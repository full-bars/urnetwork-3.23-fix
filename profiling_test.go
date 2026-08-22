package connect

import (
	"testing"
)

func TestEnableProfilingRejectsNonLoopback(t *testing.T) {
	// A public (non-loopback) bind trigger is ambiguous without a real NIC,
	// so only assert the validation path with an obviously public-looking addr.
	err := EnableProfiling("8.8.8.8:6060")
	if err == nil {
		t.Fatalf("expected non-loopback bind to be rejected")
	}
}

func TestEnableProfilingLoopbackStarts(t *testing.T) {
	addr := "127.0.0.1:0"
	err := EnableProfiling(addr)
	if err != nil {
		t.Fatalf("expected loopback bind to succeed: %v", err)
	}
	// addr:0 means we can't know the port; just confirm no error returned.
}
