package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ledgerTrim used to write an EMPTY ledger when the newest line alone was
// bigger than the keep size: the backward loop broke on the first iteration,
// start stayed len(b), and b[len(b):] is nothing. The whole audit trail of a
// self-managing agent vanished, silently, because ledgerRecord discards the
// error. Keeping the oversized newest line is over the bound for one entry,
// which beats losing all of them.
func TestLedgerTrimKeepsAnOversizedNewestLine(t *testing.T) {
	withTempHome(t)
	path := filepath.Join(t.TempDir(), "autopilot.jsonl")
	small := strings.Repeat("x", 100)
	huge := strings.Repeat("y", 2000) // bigger than keepBytes on its own
	if err := os.WriteFile(path, []byte(small+"\n"+small+"\n"+huge+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ledgerTrim(path, 200); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		t.Fatal("the ledger was wiped; the newest line must be kept even if oversized")
	}
	if !strings.Contains(string(b), "yyy") {
		t.Fatalf("the newest (oversized) entry must survive, got %d bytes", len(b))
	}
	// The point the first version of this test missed: the older lines MUST
	// still be trimmed away. Keeping them all (start = 0) leaves the file over
	// the bound forever and makes every later append re-run a useless trim.
	if strings.Contains(string(b), "xxx") {
		t.Fatalf("older lines must still be trimmed; got %d bytes, still contains the old entries", len(b))
	}
	if len(b) > 2000+16 {
		t.Fatalf("result is %d bytes: trimming did not reduce the file to the oversized newest line", len(b))
	}
}

// The ordinary case must still behave: keep the newest whole lines that fit, and
// drop the rest. This is the branch the existing size-bound test exercises; pin
// it next to the oversized case so the fix cannot have changed it.
func TestLedgerTrimStillKeepsTheNewestWholeLinesThatFit(t *testing.T) {
	withTempHome(t)
	path := filepath.Join(t.TempDir(), "autopilot.jsonl")
	line := strings.Repeat("z", 90)
	if err := os.WriteFile(path, []byte(line+"\n"+line+"\n"+line+"\n"+line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ledgerTrim(path, 400); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Count(strings.TrimRight(string(b), "\n"), "\n") + 1
	if lines > 4 || lines < 3 {
		t.Fatalf("keepBytes=400 should retain 4 lines of 91 bytes, got %d lines (%d bytes)", lines, len(b))
	}
	if !strings.HasSuffix(strings.TrimRight(string(b), "\n"), line) {
		t.Fatal("the newest lines must be the ones kept")
	}
}
