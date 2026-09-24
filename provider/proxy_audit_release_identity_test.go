package main

import (
	"os"
	"reflect"
	"testing"
	"time"
)

func TestResolveAuditReleaseKeys(t *testing.T) {
	u1, u2 := identityKey("gw.example:1080", "u1"), identityKey("gw.example:1080", "u2")
	other := identityKey("other.example:1080", "u1")
	known := []string{u2, other, u1, "10.0.0.5:1080"}

	// An operator can only type an address: it names every identity there.
	if got, want := resolveAuditReleaseKeys("gw.example:1080", known), []string{u1, u2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("bare address must resolve to every identity at it: got %q want %q", got, want)
	}
	// An exact identity key names only itself.
	if got, want := resolveAuditReleaseKeys(u1, known), []string{u1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("exact key must resolve to itself only: got %q want %q", got, want)
	}
	// An unauthenticated proxy's key IS its address.
	if got, want := resolveAuditReleaseKeys("10.0.0.5:1080", known), []string{"10.0.0.5:1080"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("bare unauthenticated key: got %q want %q", got, want)
	}
	// Nothing known: keep the input so a stray backoff/failure count is still cleared.
	if got, want := resolveAuditReleaseKeys("9.9.9.9:1", known), []string{"9.9.9.9:1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unknown address must fall through: got %q want %q", got, want)
	}
}

// Parks and backoffs are stored under identity keys, which an operator cannot
// type. Releasing by address through the real control handler must give back
// every account parked at that gateway, and clear their backoffs.
func TestControlSocketAuditRelease_ByAddressReleasesIdentityKeyedParks(t *testing.T) {
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", t.TempDir())
	t.Cleanup(func() { os.Setenv("HOME", oldHome) })

	u1, u2 := identityKey("gw.example:1080", "u1"), identityKey("gw.example:1080", "u2")
	survivor := identityKey("other.example:1080", "u1")
	h := newAuditHarness(u1, u2, survivor)
	savedHist := globalProxyFailureHistory
	globalProxyFailureHistory = h.hist
	currentProxyAuditor.Store(h.g)
	t.Cleanup(func() {
		currentProxyAuditor.Store(nil)
		globalProxyFailureHistory = savedHist
	})

	cfg := defaultProxyAuditConfig()
	for _, k := range []string{u1, u2, survivor} {
		until := h.g.st.commitPark(cfg, k, h.now, 0.5)
		h.hist.SetBackoffUntil(k, until)
	}

	state := newControlState()
	resp := handleControlRequest(state, controlRequest{Cmd: "audit", Action: "release", Address: "gw.example:1080"})
	if !resp.OK {
		t.Fatalf("release by address rejected: %v", resp.Error)
	}
	for _, k := range []string{u1, u2} {
		if h.g.st.isParked(k) {
			t.Errorf("%q still parked after release by its address", k)
		}
		if !h.hist.Eligible(k, h.now) {
			t.Errorf("%q backoff not cleared after release by its address", k)
		}
	}
	if !h.g.st.isParked(survivor) {
		t.Errorf("a proxy at a different address must stay parked")
	}
	if h.hist.Eligible(survivor, h.now.Add(time.Second)) {
		t.Errorf("a proxy at a different address must keep its backoff")
	}
}
