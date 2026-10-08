package main

import (
	connect "github.com/urnetwork/connect"
	"testing"
)

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

func TestH3DefaultsFollowResolutionTransitions(t *testing.T) {
	oldState, oldStatus := globalControlState, currentProxyResolution()
	oldH3, oldOffer, oldSend := connect.H3Enabled(), connect.H3DatagramsEnabled(), connect.H3DatagramSendEnabled()
	t.Cleanup(func() {
		globalControlState = oldState
		proxyResolutionStatus.Store(oldStatus)
		connect.SetH3Enabled(oldH3)
		connect.SetH3DatagramsEnabled(oldOffer)
		connect.SetH3DatagramSendEnabled(oldSend)
	})
	for _, tc := range []struct {
		name, env, override  string
		wantH3, wantDatagram bool
	}{
		{"unset", "", "", false, false},
		{"environment fallback", "on", "", true, false},
		{"explicit on", "", "on", true, true},
		{"explicit off beats environment", "on", "off", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("URNETWORK_H3", tc.env)
			globalControlState = newControlState()
			for _, key := range []string{"h3", "h3_datagram", "h3_datagram_send"} {
				if tc.override != "" {
					if err := globalControlState.set(key, tc.override); err != nil {
						t.Fatal(err)
					}
				}
				if err := applyLiveSideEffect(key, onOff(tc.override == "on")); err != nil {
					t.Fatal(err)
				}
			}
			proxyResolutionStatus.Store(proxyResolutionPending)
			setProxyResolutionStatus(proxyResolutionZeroValid, "")
			wantDirect := tc.override != "off"
			if connect.H3Enabled() != wantDirect || connect.H3DatagramsEnabled() != wantDirect || connect.H3DatagramSendEnabled() != wantDirect {
				t.Fatal("entering direct-only resolution did not respect defaults and overrides")
			}
			setProxyResolutionOK()
			if connect.H3Enabled() != tc.wantH3 || connect.H3DatagramsEnabled() != tc.wantDatagram || connect.H3DatagramSendEnabled() != tc.wantDatagram {
				t.Fatalf("leaving direct-only: h3=%v offer=%v send=%v", connect.H3Enabled(), connect.H3DatagramsEnabled(), connect.H3DatagramSendEnabled())
			}
		})
	}
}
