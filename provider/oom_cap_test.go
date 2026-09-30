package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var oomT0 = time.Unix(1_800_000_000, 0)

func TestOOMKilledSinceMarker(t *testing.T) {
	m := &oomMarker{BootID: "boot-A", OOMKills: 2}
	// A marker from a start more than oomMarkerMaxAge ago is stale: the global
	// oom_kill counter may have been bumped by ANY workload since, so it must
	// not be blamed on this provider.
	stale := &oomMarker{BootID: "boot-A", OOMKills: 2, StartedUnix: oomT0.Add(-(oomMarkerMaxAge + time.Hour)).Unix()}
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
		{"a stale marker is never blamed even with a higher counter", stale, "boot-A", 9, false},
	}
	for _, c := range cases {
		if got := oomKilledSinceMarker(c.m, c.boot, c.kills, oomT0.Add(time.Hour)); got != c.want {
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
	// The at-floor OOM still restarts the clean-day clock: the box just died,
	// so a start more than 24h after the original reduction must not relax.
	st, _ = oomCapOnOOM(oomCapState{Cap: 1000, SinceUnix: oomT0.Unix()}, 500, 4000, oomT0.Add(time.Hour))
	if st.SinceUnix != oomT0.Add(time.Hour).Unix() {
		t.Fatalf("an at-floor OOM must reset SinceUnix, got %d (want %d)", st.SinceUnix, oomT0.Add(time.Hour).Unix())
	}
	// Tiny list: the absolute floor of 50 applies.
	_, d = oomCapOnOOM(oomCapState{}, 60, 100, oomT0)
	if d.To != 50 {
		t.Fatalf("absolute floor: %+v, want 50", d)
	}
}

// On a small desired pool the floor (max(oomCapMinFloor, desired/4)) can sit
// AT OR ABOVE what actually died: desired=63 gives floor=50, and a 20-proxy
// death would otherwise get "reduce 0 -> 50", a cap larger than the pool that
// just got OOM-killed. That must be reported as no-op ("none"), not a reduce
// that grows the cap right after a kill.
func TestOOMCapNoOpWhenTheFloorIsNotTighterThanWhatDied(t *testing.T) {
	st, d := oomCapOnOOM(oomCapState{}, 20, 63, oomT0)
	if d.Action != "none" || st.Cap != 0 {
		t.Fatalf("floor (50) >= proxiesAtDeath (20): got %+v state %+v, want a no-op", d, st)
	}
	// The no-op OOM still restarts the clean-day clock.
	if st.SinceUnix != oomT0.Unix() {
		t.Fatalf("a no-op OOM must still reset SinceUnix, got %d want %d", st.SinceUnix, oomT0.Unix())
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
	// The frozen OOM just killed the box: it must restart the clean-day
	// clock, so the cap cannot relax 24h after the LAST REDUCTION while only
	// hours after this kill.
	if st.SinceUnix != oomT0.Add(4*time.Hour).Unix() {
		t.Fatalf("a frozen OOM must reset SinceUnix, got %d (want %d)", st.SinceUnix, oomT0.Add(4*time.Hour).Unix())
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
		!strings.Contains(lines[0], "would reduce the automatic start cap none -> 3301") || !strings.Contains(lines[0], "not enforced") {
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
		!strings.Contains(lines[0], "automatic start cap none -> 3301") || strings.Contains(lines[0], "not enforced") {
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

// The cap after an OOM is 80% of what was RUNNING at death, so the marker must
// carry the peak running count, not just what the start launched: a box that
// launches 2,000 but later admits 3,800 died at 3,800.
func TestOOMMarkerWithPeak(t *testing.T) {
	m := oomMarker{BootID: "b", Proxies: 2000, StartedUnix: 5}
	if got, changed := oomMarkerWithPeak(m, 1500); changed || got.Proxies != 2000 {
		t.Fatalf("a lower reading must not shrink the peak: %+v changed=%v", got, changed)
	}
	if got, changed := oomMarkerWithPeak(m, 2000); changed || got.Proxies != 2000 {
		t.Fatalf("an equal reading is not a change: %+v changed=%v", got, changed)
	}
	got, changed := oomMarkerWithPeak(m, 3800)
	if !changed || got.Proxies != 3800 || got.BootID != "b" || got.StartedUnix != 5 {
		t.Fatalf("a higher reading raises only the peak: %+v changed=%v", got, changed)
	}
}

// An OOM after 72h of uptime must still be blamed: the heartbeat, not the
// frozen StartedUnix, is what oomKilledSinceMarker ages. Before the fix this
// was silently dropped: a provider running 96h that got OOM-killed came back
// to a "clean start" because the marker looked more than 72h old.
func TestOOMCapBlamesAnOOMAfter72hOfUptime(t *testing.T) {
	withTempHome(t)
	t.Setenv("URNETWORK_OOM_CAP", "on")

	oomCapStartup(1000, 1000, "boot-A", 5, oomT0)
	// The provider stays up and the pressure loop keeps the heartbeat fresh,
	// well past the 72h mark that would make a heartbeat-less marker stale.
	oomCapUpdatePeak(1000, oomT0.Add(95*time.Hour))

	lines := oomCapStartup(1000, 800, "boot-A", 6, oomT0.Add(96*time.Hour))
	if len(lines) != 1 || !strings.Contains(lines[0], "OOM kill since the last start") {
		t.Fatalf("an OOM after 96h of heartbeat-refreshed uptime must be blamed, got %q", lines)
	}
}

// A marker with no heartbeat (written before this field existed, or a process
// that died before its first hourly refresh) still falls back to StartedUnix,
// so the pre-existing staleness behavior for old markers is unchanged.
func TestOOMCapMarkerWithoutHeartbeatFallsBackToStartedUnix(t *testing.T) {
	fresh := &oomMarker{BootID: "boot-A", OOMKills: 2, StartedUnix: oomT0.Unix()}
	if !oomKilledSinceMarker(fresh, "boot-A", 3, oomT0.Add(71*time.Hour)) {
		t.Fatalf("within 71h of StartedUnix with no heartbeat must still be blamed")
	}
	stale := &oomMarker{BootID: "boot-A", OOMKills: 2, StartedUnix: oomT0.Unix()}
	if oomKilledSinceMarker(stale, "boot-A", 3, oomT0.Add(80*time.Hour)) {
		t.Fatalf("80h past StartedUnix with no heartbeat must still be stale")
	}
}

func TestOOMCapUpdatePeakPersists(t *testing.T) {
	withTempHome(t)
	oomCapRecordStart(2000, "boot-A", 0, oomT0)
	oomCapUpdatePeak(3800, oomT0)
	oomCapUpdatePeak(1000, oomT0) // lower: ignored
	lines := oomCapDecide(4127, "boot-A", 1, oomT0.Add(time.Hour))
	// 80% of the 3800 peak, not of the 2000 launched.
	if len(lines) != 1 || !strings.Contains(lines[0], "cap none -> 3040") || !strings.Contains(lines[0], "(peak running 3800)") {
		t.Fatalf("decision must use the peak running count, got %q", lines)
	}
}

// The headline [oomcap] line must read as a sentence for every action, in both
// tenses, and a frozen decision must not read as a move between equal numbers.
func TestOOMCapActionPhrase(t *testing.T) {
	cases := []struct {
		d       oomCapDecision
		applied bool
		want    string
	}{
		{oomCapDecision{Action: "reduce", From: 0, To: 3301}, false, "would reduce the automatic start cap none -> 3301"},
		{oomCapDecision{Action: "reduce", From: 0, To: 3301}, true, "reduced the automatic start cap none -> 3301"},
		{oomCapDecision{Action: "relax", From: 3301, To: 3631}, false, "would relax the automatic start cap 3301 -> 3631"},
		{oomCapDecision{Action: "clear", From: 4300, To: 0}, true, "cleared the automatic start cap 4300 -> none"},
		{oomCapDecision{Action: "frozen", From: 100, To: 100}, false, "would hold the automatic start cap at 100 (reduction limit reached)"},
		{oomCapDecision{Action: "frozen", From: 100, To: 100}, true, "held the automatic start cap at 100 (reduction limit reached)"},
	}
	for _, c := range cases {
		if got := oomCapActionPhrase(c.d, c.applied); got != c.want {
			t.Errorf("oomCapActionPhrase(%+v, applied=%v) = %q, want %q", c.d, c.applied, got, c.want)
		}
	}
}

// A HotSwap parent and candidate both write oom_cap.json and run.marker at
// start. Concurrent writers must not fail on the shared temp file or leave a
// partial file behind.
func TestOOMWriteJSONConcurrentWritersLeaveWholeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oom_cap.json")
	const writers, each = 8, 40
	errs := make(chan error, writers*each)
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := oomWriteJSON(path, oomCapState{Cap: w*1000 + i, SinceUnix: 1}); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent oomWriteJSON failed: %v", err)
	}
	var st oomCapState
	if !oomReadJSON(path, &st) || st.SinceUnix != 1 {
		t.Fatalf("state file torn or unreadable after concurrent writes: %+v", st)
	}
}

// A cap or OOM attribution that sits in a torn, hand-edited or unreadable file
// silently vanishes. A missing file is normal (first start) and stays silent;
// a file that exists but cannot be read is reported.
func TestOOMCapDecideWarnsWhenStateFilesAreUnreadable(t *testing.T) {
	withTempHome(t)
	dir, err := oomCapDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	if lines := oomCapDecide(4127, "boot-A", 0, oomT0); lines != nil {
		t.Fatalf("missing state files are normal and must be silent, got %q", lines)
	}

	if err := os.WriteFile(filepath.Join(dir, "oom_cap.json"), []byte(`{"cap": 33`), 0o600); err != nil {
		t.Fatal(err)
	}
	lines := oomCapDecide(4127, "boot-A", 0, oomT0.Add(time.Hour))
	if len(lines) != 1 || !strings.Contains(lines[0], "oom_cap.json exists but could not be read") {
		t.Fatalf("a torn oom_cap.json must be reported once, got %q", lines)
	}
	// The decision rewrites the state file, so the next start is clean.
	if lines := oomCapDecide(4127, "boot-A", 0, oomT0.Add(2*time.Hour)); lines != nil {
		t.Fatalf("after the rewrite the warning must not repeat, got %q", lines)
	}

	if err := os.WriteFile(filepath.Join(dir, "run.marker"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	lines = oomCapDecide(4127, "boot-A", 0, oomT0.Add(3*time.Hour))
	if len(lines) != 1 || !strings.Contains(lines[0], "run.marker exists but could not be read") {
		t.Fatalf("an unreadable run.marker must be reported, got %q", lines)
	}
}

func writeMemoryEvents(t *testing.T, root, rel string, oomKill int) {
	t.Helper()
	dir := filepath.Join(root, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "low 0\nhigh 0\nmax 0\noom 0\noom_kill " + strconv.Itoa(oomKill) + "\noom_group_kill 0\n"
	if err := os.WriteFile(filepath.Join(dir, "memory.events"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseCgroupOOMKills(t *testing.T) {
	if n, ok := parseCgroupOOMKills("low 0\nhigh 0\nmax 0\noom 2\noom_kill 3\noom_group_kill 1\n"); !ok || n != 3 {
		t.Fatalf("got %d ok=%v, want 3 (oom_kill, not oom or oom_group_kill)", n, ok)
	}
	for _, bad := range []string{"", "oom 1\n", "oom_kill x\n", "oom_kill -1\n"} {
		if _, ok := parseCgroupOOMKills(bad); ok {
			t.Fatalf("%q must not parse", bad)
		}
	}
}

// The service's own cgroup is recreated on restart, so its counter resets. The
// counter to compare is the parent's, which persists and is hierarchical.
func TestReadCgroupOOMKillsUsesTheParentNotTheRecreatedOwnCgroup(t *testing.T) {
	root := t.TempDir()
	unit := "/user.slice/user-1000.slice/user@1000.service/app.slice/urnetwork.service"
	self := "0::" + unit + "\n"
	writeMemoryEvents(t, root, unit, 0)
	writeMemoryEvents(t, root, "/user.slice/user-1000.slice/user@1000.service/app.slice", 3)
	writeMemoryEvents(t, root, "/user.slice", 40)

	n, scope, ok := readCgroupOOMKills(root, self)
	if !ok || n != 3 || scope != "cgroup:/user.slice/user-1000.slice/user@1000.service/app.slice" {
		t.Fatalf("got %d %q ok=%v, want the app.slice counter 3", n, scope, ok)
	}
}

func TestReadCgroupOOMKillsWalksUpPastAnUnreadableParent(t *testing.T) {
	root := t.TempDir()
	unit := "/system.slice/urnetwork.service"
	writeMemoryEvents(t, root, "/system.slice", 5)
	n, scope, ok := readCgroupOOMKills(root, "0::"+unit+"\n")
	if !ok || n != 5 || scope != "cgroup:/system.slice" {
		t.Fatalf("got %d %q ok=%v", n, scope, ok)
	}
}

func TestReadCgroupOOMKillsInAContainerUsesItsOwnRoot(t *testing.T) {
	root := t.TempDir()
	writeMemoryEvents(t, root, ".", 2)
	n, scope, ok := readCgroupOOMKills(root, "0::/\n")
	if !ok || n != 2 || scope != "cgroup:/" {
		t.Fatalf("got %d %q ok=%v", n, scope, ok)
	}
}

func TestReadCgroupOOMKillsReportsNotOKWithoutAnyFile(t *testing.T) {
	for name, self := range map[string]string{
		"no file on the path": "0::/system.slice/urnetwork.service\n",
		"cgroup v1 only":      "1:name=systemd:/x\n",
		"empty":               "",
	} {
		if _, _, ok := readCgroupOOMKills(t.TempDir(), self); ok {
			t.Fatalf("%s: must report not ok", name)
		}
	}
}

// The finding: the host-wide counter blamed this provider for another
// workload's OOM kill. With a cgroup counter, only kills inside this subtree
// count; the host counter is ignored.
func TestOOMKillEpochIgnoresAnotherWorkloadsKillOnTheHost(t *testing.T) {
	root := t.TempDir()
	unit := "/user.slice/user-1000.slice/app.slice/urnetwork.service"
	self := "0::" + unit + "\n"
	parent := "/user.slice/user-1000.slice/app.slice"
	writeMemoryEvents(t, root, parent, 1)

	epochA, killsA := oomKillEpoch("boot-A", root, self, 10) // marker taken here
	marker := &oomMarker{BootID: epochA, OOMKills: killsA, StartedUnix: oomT0.Unix(), LastSeenUnix: oomT0.Unix()}

	// Some other container on the host is OOM-killed: only the host counter moves.
	epochB, killsB := oomKillEpoch("boot-A", root, self, 11)
	if oomKilledSinceMarker(marker, epochB, killsB, oomT0.Add(time.Hour)) {
		t.Fatal("another workload's OOM kill on the host must not be blamed on this provider")
	}

	// A kill inside this subtree does count.
	writeMemoryEvents(t, root, parent, 2)
	epochC, killsC := oomKillEpoch("boot-A", root, self, 11)
	if !oomKilledSinceMarker(marker, epochC, killsC, oomT0.Add(time.Hour)) {
		t.Fatal("an OOM kill inside this provider's own cgroup subtree must be attributed")
	}
}

// Without a readable cgroup file the host-wide counter and the plain boot id are
// used, exactly as before, and a marker from a different scope is not compared.
func TestOOMKillEpochFallsBackToHostAndNeverComparesAcrossScopes(t *testing.T) {
	epoch, kills := oomKillEpoch("boot-A", t.TempDir(), "0::/nowhere\n", 7)
	if epoch != "boot-A" || kills != 7 {
		t.Fatalf("fallback = %q,%d, want the plain boot id and the host counter", epoch, kills)
	}
	if e, k := oomKillEpoch("", t.TempDir(), "0::/x\n", 7); e != "" || k != 7 {
		t.Fatalf("an unknown boot id stays unknown, got %q,%d", e, k)
	}

	// A marker written under the old (host) epoch must not be compared with a
	// cgroup-scoped reading after an upgrade: the counters are different things.
	root := t.TempDir()
	writeMemoryEvents(t, root, "/system.slice", 50)
	hostMarker := &oomMarker{BootID: "boot-A", OOMKills: 3, StartedUnix: oomT0.Unix(), LastSeenUnix: oomT0.Unix()}
	e, k := oomKillEpoch("boot-A", root, "0::/system.slice/urnetwork.service\n", 3)
	if oomKilledSinceMarker(hostMarker, e, k, oomT0.Add(time.Hour)) {
		t.Fatal("a host-scoped marker must not be compared with a cgroup-scoped reading")
	}
}

// A kill that happened while the kill switch was off must not be blamed after
// the switch goes back on. With the switch off the start used to write no
// marker while the heartbeat kept the OLD one fresh, so its OOMKills baseline
// and peak came from the last start that ran with the switch on, and the next
// clean start read the counter as having risen and reduced the cap for it.
func TestOOMCapDoesNotBlameAKillFromWhileTheSwitchWasOff(t *testing.T) {
	withTempHome(t)
	t.Setenv("URNETWORK_OOM_CAP", "on")
	oomCapStartup(1000, 1000, "boot-A", 3, oomT0)

	// the operator turns the switch off, a kill happens, the provider restarts
	t.Setenv("URNETWORK_OOM_CAP", "off")
	oomCapStartup(1000, 1000, "boot-A", 4, oomT0.Add(time.Hour))
	oomCapUpdatePeak(1000, oomT0.Add(2*time.Hour)) // the heartbeat runs while off

	// the switch goes back on and the next start is clean (counter unchanged)
	t.Setenv("URNETWORK_OOM_CAP", "on")
	lines := oomCapStartup(1000, 1000, "boot-A", 4, oomT0.Add(3*time.Hour))
	for _, l := range lines {
		if strings.Contains(l, "OOM kill since the last start") {
			t.Fatalf("a kill from the switch-off period was blamed after re-enabling: %q", lines)
		}
	}
	if got, _ := effectiveTrimCap(); got != 0 {
		t.Fatalf("no cap should stand after a clean start, got %d", got)
	}
}

// Turning the switch off must not stop a kill AFTER re-enabling from being
// blamed: the marker written while off carries the real baseline, so a fresh
// rise in the counter still reduces the cap.
func TestOOMCapStillBlamesAKillAfterTheSwitchIsBackOn(t *testing.T) {
	withTempHome(t)
	t.Setenv("URNETWORK_OOM_CAP", "off")
	oomCapStartup(1000, 1000, "boot-A", 4, oomT0)
	t.Setenv("URNETWORK_OOM_CAP", "on")
	oomCapStartup(1000, 1000, "boot-A", 4, oomT0.Add(time.Hour))
	lines := oomCapStartup(1000, 800, "boot-A", 5, oomT0.Add(2*time.Hour))
	if len(lines) != 1 || !strings.Contains(lines[0], "OOM kill since the last start") {
		t.Fatalf("a kill after re-enabling must be blamed, got %q", lines)
	}
}
