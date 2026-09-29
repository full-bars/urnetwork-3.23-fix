package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The kill switch is a kill switch: turning it off has to FORGET the standing
// automatic cap, not merely stop enforcing it. Nothing ages the cap while the
// mode is off (the reduce and relax logic never runs), so a cap set days ago
// survives untouched and binds again the moment the switch goes back on,
// shedding proxies on the very start the operator flipped it on to let the pool
// grow.
func TestOOMCapOffForgetsTheStandingCap(t *testing.T) {
	withTempHome(t)
	dir, _ := oomCapDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "oom_cap.json")
	if err := oomWriteJSON(statePath, oomCapState{Cap: 800, Reductions: []int64{1000, 2000}}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("URNETWORK_OOM_CAP", "off")
	// oomCapDecide RETURNS the lines; provide() is what writes them.
	lines := oomCapDecide(2000, "boot", 0, time.Now())
	if len(lines) != 1 || !strings.Contains(lines[0], "cleared the automatic start cap 800 -> none") {
		t.Fatalf("off must say it forgot the cap, got %q", lines)
	}

	var st oomCapState
	if !oomReadJSON(statePath, &st) {
		t.Fatal("oom_cap.json must still be readable")
	}
	if st.Cap != 0 {
		t.Fatalf("off must clear the cap, state is %+v", st)
	}
	if len(st.Reductions) != 2 {
		t.Fatalf("off must keep the reduction history, it is what stops a flapping box cutting again immediately; got %+v", st.Reductions)
	}
	got, _ := ledgerTail(filepath.Join(dir, ledgerFileName), 10)
	if len(got) != 1 || got[0].Actor != "oomcap" || got[0].Action != "clear" ||
		got[0].From != 800 || got[0].To != 0 || got[0].Mode != "off" {
		t.Fatalf("want one oomcap clear entry 800 -> 0 in mode off, got %+v", got)
	}
}

// And the cap really is gone: turning the switch back on must not resume a cap
// that was cleared days ago, and no automatic cap may bind.
func TestOOMCapOnAfterOffAppliesNoStaleCap(t *testing.T) {
	withTempHome(t)
	dir, _ := oomCapDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := oomWriteJSON(filepath.Join(dir, "oom_cap.json"), oomCapState{Cap: 800}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("URNETWORK_OOM_CAP", "off")
	oomCapDecide(2000, "boot", 0, time.Now())

	t.Setenv("URNETWORK_OOM_CAP", "on")
	cap, source, err := effectiveTrimCapSource()
	if err != nil {
		t.Fatalf("effectiveTrimCapSource: %v", err)
	}
	if cap != 0 || source != "" {
		t.Fatalf("after off cleared the cap, on must not resume it: got cap=%d source=%q", cap, source)
	}
}

// Turning the kill switch off when no cap is standing must say nothing and
// write nothing: the reset is not a decision, it is the absence of one.
func TestOOMCapOffWithNoStandingCapIsSilent(t *testing.T) {
	withTempHome(t)
	t.Setenv("URNETWORK_OOM_CAP", "off")
	if lines := oomCapDecide(2000, "boot", 0, time.Now()); lines != nil {
		t.Fatalf("off with no cap must be silent, got %q", lines)
	}
	dir, _ := oomCapDir()
	if got, _ := ledgerTail(filepath.Join(dir, ledgerFileName), 10); len(got) != 0 {
		t.Fatalf("off with no cap must not write the ledger, got %+v", got)
	}
}

// A live `set oom-cap on` has to say what it will cost, not just that the value
// landed. A mode switch that silently starts shedding a third of the pool reads
// as a mystery a day later.
func TestOOMCapLiveApplyOnNamesTheCapAndTheShed(t *testing.T) {
	withTempHome(t)
	dir, _ := oomCapDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := oomWriteJSON(filepath.Join(dir, "oom_cap.json"), oomCapState{Cap: 500}); err != nil {
		t.Fatal(err)
	}
	// Two proxies running, so the shed count (running minus cap, floored at 0)
	// is 0 rather than a negative number: a cap above the running pool sheds
	// nothing.
	t.Setenv("URNETWORK_OOM_CAP", "on")
	old := importantLogHook
	var lines []string
	importantLogHook = func(line string) { lines = append(lines, line) }
	t.Cleanup(func() { importantLogHook = old })

	applyLiveSideEffect("oom_cap", "on")

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "enforcing cap 500") {
		t.Fatalf("live apply on must name the cap in force, got %q", joined)
	}
	if !strings.Contains(joined, "sheds about 0 of") {
		t.Fatalf("live apply on must report the shed count, got %q", joined)
	}
}

// A live `set oom-cap off` has to clear the standing cap immediately, not wait
// for the next start: the operator's kill switch is a kill switch, and a cap
// that is still on disk is one restart away from shedding the pool again.
func TestOOMCapLiveApplyOffClearsTheStandingCap(t *testing.T) {
	withTempHome(t)
	dir, _ := oomCapDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "oom_cap.json")
	if err := oomWriteJSON(statePath, oomCapState{Cap: 800}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("URNETWORK_OOM_CAP", "off")
	old := importantLogHook
	var lines []string
	importantLogHook = func(line string) { lines = append(lines, line) }
	t.Cleanup(func() { importantLogHook = old })

	applyLiveSideEffect("oom_cap", "off")

	var st oomCapState
	if !oomReadJSON(statePath, &st) || st.Cap != 0 {
		t.Fatalf("live apply off must clear the cap immediately, state is %+v", st)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "cleared the automatic start cap 800 -> none") {
		t.Fatalf("live apply off must say the cap was forgotten, got %q", lines)
	}
	// And with the cap gone, no automatic cap binds at all.
	if cap, source, err := effectiveTrimCapSource(); err != nil || cap != 0 || source != "" {
		t.Fatalf("after a live off, no automatic cap may bind: cap=%d source=%q err=%v", cap, source, err)
	}
}

// An unreadable proxy_trim (permissions, a directory where the file belongs, a
// broken disk) used to make effectiveTrimCapSource return an error, and both
// callers skip capping entirely on an error, so one unreadable file silently
// disabled the OOM protection on exactly the boxes short of memory. The
// automatic cap is independent of the operator's file and must still apply.
