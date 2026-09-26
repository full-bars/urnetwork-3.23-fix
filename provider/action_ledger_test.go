package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var ledgerT0 = time.Date(2026, 9, 26, 1, 0, 0, 0, time.UTC)

func ledgerFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "autopilot.jsonl")
}

// Every automatic decision leaves a machine-readable line an operator can audit
// later without grepping a rotating RAM log.
func TestLedgerAppendAndReadBack(t *testing.T) {
	path := ledgerFile(t)
	entries := []ledgerEntry{
		{Actor: "oomcap", Action: "reduce", From: 0, To: 3301, Mode: "on", Reason: "OOM kill since the last start (peak running 4127)"},
		{Actor: "trim", Action: "applied", From: 4127, To: 2000, Mode: "operator", Reason: "shed 2127"},
	}
	for i, e := range entries {
		if err := ledgerAppend(path, e, ledgerT0.Add(time.Duration(i)*time.Minute), 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ledgerTail(path, 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("tail: %v %v", got, err)
	}
	if got[0].Actor != "oomcap" || got[0].To != 3301 || got[0].Time != "2026-09-26T01:00:00Z" {
		t.Fatalf("first entry: %+v", got[0])
	}
	if got[1].Actor != "trim" || got[1].Time != "2026-09-26T01:01:00Z" {
		t.Fatalf("second entry: %+v", got[1])
	}
	// Newest N only.
	last, _ := ledgerTail(path, 1)
	if len(last) != 1 || last[0].Actor != "trim" {
		t.Fatalf("tail(1): %+v", last)
	}
}

func TestLedgerIsSizeBoundedAndKeepsTheNewest(t *testing.T) {
	path := ledgerFile(t)
	const max = 4096
	for i := 0; i < 200; i++ {
		e := ledgerEntry{Actor: "trim", Action: "applied", To: i, Reason: strings.Repeat("x", 40)}
		if err := ledgerAppend(path, e, ledgerT0.Add(time.Duration(i)*time.Second), max); err != nil {
			t.Fatal(err)
		}
	}
	st, err := os.Stat(path)
	if err != nil || st.Size() > max {
		t.Fatalf("ledger grew past its bound: size %d err %v (max %d)", st.Size(), err, max)
	}
	got, _ := ledgerTail(path, 1000)
	if len(got) == 0 || got[len(got)-1].To != 199 {
		t.Fatalf("the newest entry must survive rotation, got last=%+v", got)
	}
	// Only whole lines remain and they stay in order.
	for i := 1; i < len(got); i++ {
		if got[i].To != got[i-1].To+1 {
			t.Fatalf("rotation left a gap or reorder at %d: %d then %d", i, got[i-1].To, got[i].To)
		}
	}
}

func TestLedgerToleratesAMissingFileAndACorruptLine(t *testing.T) {
	path := ledgerFile(t)
	if got, err := ledgerTail(path, 5); err != nil || len(got) != 0 {
		t.Fatalf("missing file: %v %v", got, err)
	}
	if err := ledgerAppend(path, ledgerEntry{Actor: "a", Action: "x"}, ledgerT0, 1<<20); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString("{not json\n")
	_ = f.Close()
	if err := ledgerAppend(path, ledgerEntry{Actor: "b", Action: "y"}, ledgerT0.Add(time.Second), 1<<20); err != nil {
		t.Fatal(err)
	}
	got, err := ledgerTail(path, 10)
	if err != nil || len(got) != 2 || got[0].Actor != "a" || got[1].Actor != "b" {
		t.Fatalf("a corrupt line must be skipped, got %+v err %v", got, err)
	}
}

// The OOM decision writes the ledger (a decision is only worth acting on if it
// can be audited), in shadow and on mode alike.
func TestOOMCapDecisionsAreLedgered(t *testing.T) {
	withTempHome(t)
	t.Setenv("URNETWORK_OOM_CAP", "shadow")
	oomCapDecide(4127, "boot-A", 0, oomT0)
	oomCapRecordStart(4127, "boot-A", 0, oomT0)
	oomCapDecide(4127, "boot-A", 1, oomT0.Add(time.Hour))

	dir, _ := oomCapDir()
	got, err := ledgerTail(filepath.Join(dir, "autopilot.jsonl"), 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("want exactly one ledger entry, got %+v err %v", got, err)
	}
	e := got[0]
	if e.Actor != "oomcap" || e.Action != "reduce" || e.From != 0 || e.To != 3301 || e.Mode != "shadow" ||
		!strings.Contains(e.Reason, "peak running 4127") {
		t.Fatalf("ledger entry: %+v", e)
	}
}

// A trim command's result is ledgered too, so operator actions and automatic
// ones sit in one timeline.
func TestReloadLedgersTheTrimResult(t *testing.T) {
	r, _, _ := trimFixture(t)
	if err := writeTrimTarget(1); err != nil {
		t.Fatal(err)
	}
	r.reload()
	dir, _ := oomCapDir()
	got, _ := ledgerTail(filepath.Join(dir, "autopilot.jsonl"), 10)
	if len(got) != 1 || got[0].Actor != "trim" || got[0].Action != "applied" || got[0].To != 1 ||
		!strings.Contains(got[0].Reason, "shed 2") {
		t.Fatalf("trim ledger: %+v", got)
	}
	// An unchanged cap on a later reload writes nothing more.
	r.reload()
	again, _ := ledgerTail(filepath.Join(dir, "autopilot.jsonl"), 10)
	if len(again) != 1 {
		t.Fatalf("an unchanged cap must not re-ledger, got %d entries", len(again))
	}
}

// The timeline is readable through the control socket, so `urnet-tools` or a
// dashboard needs no file access: newest N, chronological, limit bounded.
func TestControlLedgerReturnsTheTimeline(t *testing.T) {
	withTempHome(t)
	dir, _ := oomCapDir()
	path := filepath.Join(dir, ledgerFileName)
	for i := 0; i < 5; i++ {
		if err := ledgerAppend(path, ledgerEntry{Actor: "trim", Action: "applied", To: i}, ledgerT0.Add(time.Duration(i)*time.Second), 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	state := newControlState()

	resp := handleControlRequest(state, controlRequest{Cmd: "ledger", Limit: 3})
	if !resp.OK {
		t.Fatalf("ledger failed: %s", resp.Error)
	}
	var got []ledgerEntry
	if err := json.Unmarshal([]byte(resp.Value), &got); err != nil {
		t.Fatalf("value is not the JSON entry list: %v\n%s", err, resp.Value)
	}
	if len(got) != 3 || got[0].To != 2 || got[2].To != 4 {
		t.Fatalf("want the newest 3 in order (2,3,4), got %+v", got)
	}

	// Default limit, and an absurd limit is bounded rather than rejected.
	if r := handleControlRequest(state, controlRequest{Cmd: "ledger"}); !r.OK {
		t.Fatalf("default limit failed: %s", r.Error)
	}
	if r := handleControlRequest(state, controlRequest{Cmd: "ledger", Limit: 1 << 30}); !r.OK {
		t.Fatalf("huge limit must be clamped, got error %s", r.Error)
	}

	// No ledger yet: an empty list, not an error.
	withTempHome(t)
	r := handleControlRequest(newControlState(), controlRequest{Cmd: "ledger"})
	if !r.OK || strings.TrimSpace(r.Value) != "[]" {
		t.Fatalf("empty ledger must answer [], got ok=%v %q", r.OK, r.Value)
	}
}
