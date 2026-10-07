package main

import "testing"

// TestDirectOnlyH3Defaults pins which proxy-resolution states auto-enable the
// H3 transport family. Only a SETTLED direct-only node qualifies: a node still
// loading proxies, one whose pool resolved empty, and one with direct switched
// off (nothing served at all) must all keep the off default.
func TestDirectOnlyH3Defaults(t *testing.T) {
	cases := []struct {
		name       string
		resolution int32
		want       bool
	}{
		{"direct-only, settled", proxyResolutionZeroValid, true},
		{"direct off and no source", proxyResolutionNoSource, false},
		{"proxies resolved", proxyResolutionOK, false},
		{"source yielded nothing", proxyResolutionEmpty, false},
		{"source failed", proxyResolutionFailed, false},
		{"still pending", proxyResolutionPending, false},
	}
	for _, tc := range cases {
		if got := directOnlyH3Defaults(tc.resolution); got != tc.want {
			t.Errorf("directOnlyH3Defaults(%s) = %v, want %v",
				tc.name, got, tc.want)
		}
	}
}

// TestDefaultH3Setting checks the startup fallback: URNETWORK_H3 still asks for
// H3 on a pooled node, while a settled direct-only node gets it for free.
func TestDefaultH3Setting(t *testing.T) {
	t.Setenv("URNETWORK_H3", "")

	if !defaultH3Setting(nil, proxyResolutionZeroValid) {
		t.Error("a settled direct-only node must default H3 on with no env set")
	}
	if defaultH3Setting(nil, proxyResolutionOK) {
		t.Error("a pooled node must keep the H3 off default with no env set")
	}

	t.Setenv("URNETWORK_H3", "on")
	if !defaultH3Setting(nil, proxyResolutionOK) {
		t.Error("URNETWORK_H3=on must still enable H3 on a pooled node")
	}
}
