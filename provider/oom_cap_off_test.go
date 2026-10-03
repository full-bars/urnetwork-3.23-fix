package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// `urnet-tools set oom-cap off` used to be turned into a CLEAR by the CLI (a
// literal "off" is the clear value for every key outside a small whitelist, and
// oom_cap was not in it). The provider's clear handler drops the override and
// re-applies liveDefaults["oom_cap"] = "shadow", so the feature was never
// actually off and the code that forgets a standing cap on "off" never ran.
// With the key whitelisted the value "off" is sent instead, and the kill switch
// does what it says.
//
// This pins the PROVIDER half: that a persisted "off" really does turn the mode
// off and forget the cap. The CLI half is pinned by the urnet-tools test.
func TestOOMCapOffValueDisablesAndForgetsTheCap(t *testing.T) {
	withTempHome(t)
	dir, _ := oomCapDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := oomWriteJSON(filepath.Join(dir, "oom_cap.json"), oomCapState{Cap: 900}); err != nil {
		t.Fatal(err)
	}

	// No env var: the persisted control value is the only source.
	t.Setenv("URNETWORK_OOM_CAP", "")
	setPersistedControlValue(t, "oom_cap", "off")

	if got := oomCapModeName(oomCapMode()); got != "off" {
		t.Fatalf("a persisted off must actually be off, got %q", got)
	}
	cap, source, err := effectiveTrimCapSource()
	if err != nil {
		t.Fatal(err)
	}
	if cap != 0 || source != "" {
		t.Fatalf("while off no automatic cap may bind, got cap=%d source=%q", cap, source)
	}

	// And the live-apply path forgets the standing cap immediately, not on the
	// next start.
	if err := oomWriteJSON(filepath.Join(dir, "oom_cap.json"), oomCapState{Cap: 900}); err != nil {
		t.Fatal(err)
	}
	for _, line := range oomCapResetOnOff() {
		if !strings.Contains(line, "cleared the automatic start cap 900 -> none") {
			t.Fatalf("off must report clearing the cap, got %q", line)
		}
	}
	var st oomCapState
	if !oomReadJSON(filepath.Join(dir, "oom_cap.json"), &st) || st.Cap != 0 {
		t.Fatalf("off must clear the standing cap, state is %+v", st)
	}
}

// The heartbeat is what proves the process is still up, and oomKilledSinceMarker
// ages the marker against oomMarkerMaxAge. Gating the heartbeat on the mode
// meant a box left off for a weekend came back with a frozen marker and the
// FIRST real OOM kill after re-enabling was refused as stale: no reduction, no
// ledger entry, no log line. Refusing a kill is the one failure this mechanism
// must never make.
func TestMarkerHeartbeatKeepsRunningWhileTheSwitchIsOff(t *testing.T) {
	withTempHome(t)
	dir, _ := oomCapDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("URNETWORK_OOM_CAP", "")

	now := time.Now()
	// A marker written well beyond oomMarkerMaxAge ago, with a stale heartbeat.
	old := now.Add(-100 * time.Hour)
	// The marker is this process's own, recorded at its start 100h ago (a
	// process only heartbeats a marker it recorded itself).
	oomCapRecordStart(100, "boot-A", 3, old)

	// With the switch OFF, the heartbeat must still be refreshed. The peak is
	// not updated (the feature is off), but liveness must be recorded.
	t.Setenv("URNETWORK_OOM_CAP", "off")
	oomCapUpdatePeak(999, now)

	var m oomMarker
	if !oomReadJSON(filepath.Join(dir, "run.marker"), &m) {
		t.Fatal("run.marker must still exist")
	}
	if time.Unix(m.LastSeenUnix, 0).Before(now.Add(-time.Minute)) {
		t.Fatalf("the heartbeat must refresh while off, LastSeenUnix is still %v old",
			now.Sub(time.Unix(m.LastSeenUnix, 0)))
	}
	if m.Proxies == 999 {
		t.Fatal("the peak must NOT be updated while off; the feature is disabled")
	}
}

// And the consequence: a genuine OOM kill after a long off period is claimed,
// not refused as stale.
func TestOOMKillIsStillClaimedAfterALongOffPeriod(t *testing.T) {
	withTempHome(t)
	dir, _ := oomCapDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("URNETWORK_OOM_CAP", "off")

	now := time.Now()
	old := now.Add(-100 * time.Hour)
	// This process wrote the marker at its own start, 100h ago (a process only
	// heartbeats a marker it recorded itself).
	oomCapRecordStart(100, "boot-A", 3, old)
	// The process has been up and heartbeating through the whole off period.
	oomCapUpdatePeak(100, now)

	var m oomMarker
	oomReadJSON(filepath.Join(dir, "run.marker"), &m)
	// Same boot, counter higher: a real kill. It must be claimed.
	if !oomKilledSinceMarker(&m, "boot-A", 4, now) {
		t.Fatal("a real OOM kill after a long off period must be claimed, not refused as stale")
	}
	// A reboot is still never blamed.
	if oomKilledSinceMarker(&m, "boot-B", 99, now) {
		t.Fatal("a different boot must never be blamed")
	}
}
