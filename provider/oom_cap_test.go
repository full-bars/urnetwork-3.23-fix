package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var oomT0 = time.Unix(1_800_000_000, 0)

func TestOOMKilledSinceMarker(t *testing.T) {
	m := &oomMarker{BootID: "boot-A", OOMKills: 2}
	cases := []struct {
		name  string
		m     *oomMarker
		boot  string
		kills int64
		want  bool
	}{
		{"counter rose on the same boot", m, "boot-A", 3, true},
		{"counter unchanged", m, "boot-A", 2, false},
		{"a reboot resets the counter and is never an OOM", m, "boot-B", 9, false},
		{"no previous marker", nil, "boot-A", 3, false},
		{"unknown boot id", m, "", 3, false},
		{"unreadable counter", m, "boot-A", -1, false},
	}
	for _, c := range cases {
		if got := oomKilledSinceMarker(c.m, c.boot, c.kills); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestOOMCapReducesToEightyPercentOfWhatDied(t *testing.T) {
	st, d := oomCapOnOOM(oomCapState{}, 4127, 4127, oomT0)
	if d.Action != "reduce" || d.To != 3301 || st.Cap != 3301 { // int(4127*0.8)
		t.Fatalf("decision %+v state %+v, want reduce to 3301", d, st)
	}
	// A second OOM at the reduced size steps down again from the standing cap.
	st, d = oomCapOnOOM(st, 3301, 4127, oomT0.Add(time.Hour))
	if d.Action != "reduce" || d.To != 2640 {
		t.Fatalf("second OOM: %+v, want reduce to 2640", d)
	}
}

func TestOOMCapNeverGoesBelowTheFloor(t *testing.T) {
	// floor = max(50, desired/4) = 1000 for a 4000-proxy list
	st, d := oomCapOnOOM(oomCapState{}, 1100, 4000, oomT0)
	if d.To != 1000 || st.Cap != 1000 {
		t.Fatalf("got %+v, want the 1000 floor", d)
	}
	_, d = oomCapOnOOM(st, 1000, 4000, oomT0.Add(time.Hour))
	if d.Action != "none" {
		t.Fatalf("at the floor a further OOM must not reduce, got %+v", d)
	}
	// Tiny list: the absolute floor of 50 applies.
	_, d = oomCapOnOOM(oomCapState{}, 60, 100, oomT0)
	if d.To != 50 {
		t.Fatalf("absolute floor: %+v, want 50", d)
	}
}

func TestOOMCapFreezesAfterThreeReductionsInADay(t *testing.T) {
	st := oomCapState{}
	var d oomCapDecision
	proxies := 4000
	for i := 0; i < 3; i++ {
		st, d = oomCapOnOOM(st, proxies, 4000, oomT0.Add(time.Duration(i)*time.Hour))
		if d.Action != "reduce" {
			t.Fatalf("reduction %d: %+v", i, d)
		}
		proxies = st.Cap
	}
	st, d = oomCapOnOOM(st, proxies, 4000, oomT0.Add(4*time.Hour))
	if d.Action != "frozen" || st.Cap != d.From {
		t.Fatalf("fourth OOM inside the window must freeze, got %+v", d)
	}
	// Outside the 24h window the budget is available again.
	_, d = oomCapOnOOM(st, proxies, 4000, oomT0.Add(30*time.Hour))
	if d.Action == "frozen" {
		t.Fatalf("after the window the budget must recover, got %+v", d)
	}
}

func TestOOMCapRelaxesSlowlyAndClears(t *testing.T) {
	st := oomCapState{Cap: 2000, SinceUnix: oomT0.Unix()}
	if _, d := oomCapOnCleanStart(st, 4000, oomT0.Add(23*time.Hour)); d.Action != "none" {
		t.Fatalf("before a clean day nothing changes, got %+v", d)
	}
	st, d := oomCapOnCleanStart(st, 4000, oomT0.Add(25*time.Hour))
	if d.Action != "relax" || d.To != 2200 {
		t.Fatalf("after a clean day: %+v, want relax to 2200", d)
	}
	// Relaxing again needs another clean day from the change.
	if _, d := oomCapOnCleanStart(st, 4000, oomT0.Add(26*time.Hour)); d.Action != "none" {
		t.Fatalf("relax must wait a full window from the last change, got %+v", d)
	}
	st = oomCapState{Cap: 3800, SinceUnix: oomT0.Unix()}
	st, d = oomCapOnCleanStart(st, 4000, oomT0.Add(25*time.Hour))
	if d.Action != "clear" || st.Cap != 0 {
		t.Fatalf("reaching the desired size clears the cap, got %+v state %+v", d, st)
	}
	if _, d := oomCapOnCleanStart(oomCapState{}, 4000, oomT0); d.Action != "none" {
		t.Fatalf("no cap: nothing to relax, got %+v", d)
	}
}

// End to end through the persisted marker, on a temp home: start 1 records the
// marker; start 2 on the same boot with a higher oom_kill counter would reduce;
// start 3 (no further OOM) does nothing; a reboot is never blamed.
func TestOOMCapStartupShadowFlow(t *testing.T) {
	withTempHome(t)

	if lines := oomCapStartup(4127, 4127, "boot-A", 0, oomT0); lines != nil {
		t.Fatalf("first ever start must be silent, got %q", lines)
	}
	lines := oomCapStartup(4127, 4127, "boot-A", 1, oomT0.Add(time.Hour))
	if len(lines) != 1 || !strings.Contains(lines[0], "[oomcap] shadow: OOM kill since the last start") ||
		!strings.Contains(lines[0], "would reduce the automatic start cap 0 -> 3301") || !strings.Contains(lines[0], "not enforced") {
		t.Fatalf("second start: %q", lines)
	}
	if lines := oomCapStartup(4127, 4127, "boot-A", 1, oomT0.Add(2*time.Hour)); lines != nil {
		t.Fatalf("no new OOM: must be silent, got %q", lines)
	}
	if lines := oomCapStartup(4127, 4127, "boot-B", 0, oomT0.Add(3*time.Hour)); lines != nil {
		t.Fatalf("a reboot must not be blamed on an OOM, got %q", lines)
	}
}

func TestOOMCapMode(t *testing.T) {
	for env, want := range map[string]oomCapModeKind{
		"": oomCapShadow, "shadow": oomCapShadow, "SHADOW": oomCapShadow, "garbage": oomCapShadow,
		"off": oomCapOff, "0": oomCapOff, "false": oomCapOff, "on": oomCapOn, "1": oomCapOn, "true": oomCapOn,
	} {
		t.Setenv("URNETWORK_OOM_CAP", env)
		if got := oomCapMode(); got != want {
			t.Errorf("URNETWORK_OOM_CAP=%q: got %v, want %v", env, got, want)
		}
	}
}

// The operator's trim file is authoritative and is never written by the auto
// logic; the effective cap is the tightest positive of the two, and the auto cap
// only counts when the mode is "on".
func TestEffectiveTrimCap(t *testing.T) {
	cases := []struct {
		name     string
		mode     string
		operator int
		auto     int
		want     int
	}{
		{"nothing set", "on", 0, 0, 0},
		{"operator only", "on", 2000, 0, 2000},
		{"auto only, enforced", "on", 0, 1500, 1500},
		{"both, the tighter wins (auto)", "on", 2000, 1500, 1500},
		{"both, the tighter wins (operator)", "on", 1000, 1500, 1000},
		{"auto ignored in shadow", "shadow", 0, 1500, 0},
		{"auto ignored when off", "off", 2000, 1500, 2000},
		{"default mode is shadow", "", 0, 1500, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withTempHome(t)
			t.Setenv("URNETWORK_OOM_CAP", c.mode)
			if c.operator > 0 {
				if err := writeTrimTarget(c.operator); err != nil {
					t.Fatal(err)
				}
			}
			if c.auto > 0 {
				dir, _ := oomCapDir()
				_ = os.MkdirAll(dir, 0o700)
				if err := oomWriteJSON(filepath.Join(dir, "oom_cap.json"), oomCapState{Cap: c.auto}); err != nil {
					t.Fatal(err)
				}
			}
			got, err := effectiveTrimCap()
			if err != nil || got != c.want {
				t.Fatalf("effectiveTrimCap = %d, %v; want %d", got, err, c.want)
			}
		})
	}
}

// In "on" mode the decision is APPLIED and says so; the persisted cap is what
// effectiveTrimCap then enforces for this very start.
func TestOOMCapDecideAppliesInOnMode(t *testing.T) {
	withTempHome(t)
	t.Setenv("URNETWORK_OOM_CAP", "on")
	oomCapDecide(4127, "boot-A", 0, oomT0)
	oomCapRecordStart(4127, "boot-A", 0, oomT0)
	lines := oomCapDecide(4127, "boot-A", 1, oomT0.Add(time.Hour))
	if len(lines) != 1 || !strings.Contains(lines[0], "[oomcap] applied: OOM kill since the last start") ||
		!strings.Contains(lines[0], "automatic start cap 0 -> 3301") || strings.Contains(lines[0], "not enforced") {
		t.Fatalf("on mode line: %q", lines)
	}
	if got, _ := effectiveTrimCap(); got != 3301 {
		t.Fatalf("the reduced cap must be enforced immediately, got %d", got)
	}
}

// The reload path enforces the automatic cap through the same trim machinery.
func TestReload_EnforcesTheAutomaticCapInOnMode(t *testing.T) {
	r, _, cancelled := trimFixture(t)
	t.Setenv("URNETWORK_OOM_CAP", "on")
	dir, _ := oomCapDir()
	_ = os.MkdirAll(dir, 0o700)
	if err := oomWriteJSON(filepath.Join(dir, "oom_cap.json"), oomCapState{Cap: 1}); err != nil {
		t.Fatal(err)
	}
	r.reload()
	if got := cancelled.Load(); got != 2 {
		t.Fatalf("auto cap 1 of 3 must shed 2, cancelled %d", got)
	}
}
