package main

import (
	"testing"

	"github.com/urnetwork/connect"
)

func TestParseH3Mode(t *testing.T) {
	for value, want := range map[string]h3Setting{
		"": {Mode: h3ModeDirect}, "direct": {Mode: h3ModeDirect}, "DIRECT": {Mode: h3ModeDirect},
		"off": {Mode: h3ModeOff}, "0": {Mode: h3ModeOff},
		"on": {Mode: h3ModeOn}, " On ": {Mode: h3ModeOn},
		"all": {Mode: h3ModeAll}, "16": {Mode: h3ModeTop, Cap: 16}, " 64 ": {Mode: h3ModeTop, Cap: 64},
		"auto": {Mode: h3ModeAuto}, "AUTO": {Mode: h3ModeAuto}, " Auto ": {Mode: h3ModeAuto},
	} {
		got, err := parseH3Mode(value)
		if err != nil || got != want {
			t.Fatalf("parseH3Mode(%q) = %v, %v; want %v", value, got, err, want)
		}
	}
	for _, bad := range []string{"maybe", "true", "-1", "+4", "00", "04", "1.5", "all proxies"} {
		if _, err := parseH3Mode(bad); err == nil {
			t.Fatalf("parseH3Mode(%q) must be rejected", bad)
		}
	}
	for _, value := range []string{"off", "direct", "on", "all", "auto", "16"} {
		got, _ := parseH3Mode(value)
		if got.String() != value {
			t.Fatalf("%q does not round trip, got %q", value, got.String())
		}
	}
}

// The default: the direct identity runs every upstream carrier as an automatic
// fallback behind H1, and no identity is the beside-H1 auxiliary.
func TestDirectIdentityDefaultsToAutomaticFallbacks(t *testing.T) {
	t.Setenv("URNETWORK_H3", "")
	settings := platformTransportSettingsFor(nil, true)
	if settings.EnableH3 {
		t.Fatal("the beside-H1 auxiliary must stay opt-in")
	}
	want := connect.DefaultTransportModePreferences()
	if len(settings.ModePreferences) != len(want) {
		t.Fatalf("direct modes = %v, want upstream's %v", settings.ModePreferences, want)
	}
	for mode, priority := range want {
		if settings.ModePreferences[mode] != priority {
			t.Fatalf("mode %s priority %d, want %d", mode, settings.ModePreferences[mode], priority)
		}
	}
	if !(settings.ModePreferences[connect.TransportModeH1] < settings.ModePreferences[connect.TransportModeH3]) {
		t.Fatal("H1 must be the primary: H3 is a fallback unless the auxiliary is on")
	}
}

func TestH3ModePreferencesPerMode(t *testing.T) {
	for _, mode := range []h3Mode{h3ModeOff, h3ModeDirect, h3ModeOn, h3ModeTop, h3ModeAll, h3ModeAuto} {
		got := h3ModePreferences(h3Setting{Mode: mode, Cap: 4}, false, false)
		if len(got) != 1 || got[connect.TransportModeH1] != 1 {
			t.Fatalf("mode %v: an ineligible proxy must be H1 only, got %v", mode, got)
		}
	}
	for _, mode := range []h3Mode{h3ModeOff} {
		if got := h3ModePreferences(h3Setting{Mode: mode}, true, true); len(got) != 1 || got[connect.TransportModeH1] != 1 {
			t.Fatalf("off: every identity must be H1 only, got %v", got)
		}
	}
	// the fallbacks, for the direct identity in every mode but off, and for an
	// eligible proxy
	want := connect.DefaultTransportModePreferences()
	for _, c := range []struct {
		name          string
		setting       h3Setting
		direct, proxy bool
	}{
		{"direct", h3Setting{Mode: h3ModeDirect}, true, false},
		{"top direct", h3Setting{Mode: h3ModeTop, Cap: 2}, true, false},
		{"top proxy", h3Setting{Mode: h3ModeTop, Cap: 2}, false, true},
		{"all proxy", h3Setting{Mode: h3ModeAll}, false, true},
		{"auto direct", h3Setting{Mode: h3ModeAuto}, true, false},
		{"auto proxy", h3Setting{Mode: h3ModeAuto}, false, true},
	} {
		got := h3ModePreferences(c.setting, c.direct, c.proxy)
		for mode, priority := range want {
			if got[mode] != priority {
				t.Fatalf("%s: mode %s priority %d, want %d", c.name, mode, got[mode], priority)
			}
		}
	}
	on := h3ModePreferences(h3Setting{Mode: h3ModeOn}, true, false)
	if on[connect.TransportModeH3] != on[connect.TransportModeH1] {
		t.Fatalf("on: H3 must share H1's tier (auxiliary), got %v", on)
	}
	for _, mode := range []connect.TransportMode{connect.TransportModeH3Dns, connect.TransportModeH3DnsPump} {
		if on[mode] <= on[connect.TransportModeH1] {
			t.Fatalf("on: %s must stay a fallback, got %v", mode, on)
		}
	}
	// a proxy is never the auxiliary
	if got := h3ModePreferences(h3Setting{Mode: h3ModeAll}, false, true); got[connect.TransportModeH3] <= got[connect.TransportModeH1] {
		t.Fatalf("a proxy must not run H3 beside H1, got %v", got)
	}
}

func TestResolveH3Mode(t *testing.T) {
	t.Setenv("URNETWORK_H3", "")
	if got := resolveH3Mode(nil, proxyResolutionNoSource); got.Mode != h3ModeDirect {
		t.Fatalf("unset on a pooled node = %v, want direct", got)
	}
	if got := resolveH3Mode(nil, proxyResolutionZeroValid); got.Mode != h3ModeOn {
		t.Fatalf("unset on a settled direct-only node = %v, want on (the existing default)", got)
	}
	t.Setenv("URNETWORK_H3", "on")
	if got := resolveH3Mode(nil, proxyResolutionNoSource); got.Mode != h3ModeOn {
		t.Fatalf("URNETWORK_H3=on = %v, want on", got)
	}
	state := newControlState()
	for _, c := range []struct {
		value string
		want  h3Mode
	}{{"off", h3ModeOff}, {"direct", h3ModeDirect}, {"on", h3ModeOn}, {"all", h3ModeAll}, {"auto", h3ModeAuto}, {"8", h3ModeTop}} {
		if err := state.set("h3", c.value); err != nil {
			t.Fatal(err)
		}
		if got := resolveH3Mode(state, proxyResolutionZeroValid); got.Mode != c.want {
			t.Fatalf("explicit %q = %v, want %v", c.value, got, c.want)
		}
	}
}

// A change of setting relaunches the direct identity when its policy changed;
// the startup replay does not.
func TestApplyH3ModeRelaunchesOnlyOnAChange(t *testing.T) {
	restoreH3Gate(t)
	h3AppliedSetting.Store(nil)
	t.Cleanup(func() {
		h3AppliedSetting.Store(nil)
		// the count steps at the end store a ranking; leaving it behind makes
		// the next h3 ranking test see a stale one under shuffled order
		h3RankingValue.Store(nil)
	})
	direct := h3Setting{Mode: h3ModeDirect}
	if _, relaunch := applyH3Mode(direct); relaunch {
		t.Fatal("the first application is the startup replay and must not relaunch")
	}
	if _, relaunch := applyH3Mode(direct); relaunch {
		t.Fatal("an unchanged mode must not relaunch")
	}
	if previous, relaunch := applyH3Mode(h3Setting{Mode: h3ModeOn}); !relaunch || previous != direct {
		t.Fatalf("direct -> on: previous=%v relaunch=%v", previous, relaunch)
	}
	if !connect.H3Enabled() {
		t.Fatal("on must open the auxiliary gate")
	}
	if _, relaunch := applyH3Mode(h3Setting{Mode: h3ModeOff}); !relaunch || connect.H3Enabled() {
		t.Fatalf("on -> off: relaunch=%v gate=%v", relaunch, connect.H3Enabled())
	}
	// widening to proxies leaves the direct identity's policy alone
	applyH3Mode(direct)
	if _, relaunch := applyH3Mode(h3Setting{Mode: h3ModeAll}); relaunch {
		t.Fatal("direct -> all must not relaunch the direct identity")
	}
	if _, relaunch := applyH3Mode(h3Setting{Mode: h3ModeTop, Cap: 4}); relaunch {
		t.Fatal("all -> 4 must not relaunch the direct identity")
	}
}
