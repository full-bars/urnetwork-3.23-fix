package connect

import (
	"context"
	"testing"
	"time"
)

func TestIpv6EvaluationFailuresAndAdmission(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &multiClientWindow{ctx: ctx, settings: DefaultMultiClientSettings(), clients: map[Id]*multiClientChannel{}}
	w.noteIpv6EvaluationFailed(IpFamilyDualstack, true) // no shortfall
	w.setIpv6Shortfall(1)
	w.noteIpv6EvaluationFailed(IpFamilyV4Only, true)
	w.noteIpv6EvaluationFailed(IpFamilyDualstack, false)
	if w.ipv6FillFailures != 0 {
		t.Fatalf("excluded failure counted: %d", w.ipv6FillFailures)
	}
	w.noteIpv6EvaluationFailed(IpFamilyDualstack, true)
	if w.ipv6FillFailures != 1 {
		t.Fatalf("failure count = %d", w.ipv6FillFailures)
	}
	w.noteIpv6CandidateAdmitted()
	if w.ipv6FillFailures != 0 {
		t.Fatal("admission did not reset failures")
	}
	w.noteIpv6EvaluationFailed(IpFamilyV6Only, true)
	w.noteIpv6EvaluationFailed(IpFamilyDualstack, true)
	if !w.ipv6Starved(time.Now()) || w.ipv6FillWanted(time.Now()) != 0 {
		t.Fatal("repeated failures did not starve IPv6 fill")
	}
	cancel()
	w.ipv6StarvedUntil = time.Time{}
	w.noteIpv6EvaluationFailed(IpFamilyDualstack, true)
	if w.ipv6FillFailures != 0 {
		t.Fatal("cancelled window counted a failure")
	}
}
