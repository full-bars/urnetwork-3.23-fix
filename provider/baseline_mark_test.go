package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// `urnet-tools baseline mark <label>` goes through the control socket so the
// PROVIDER stays the only writer to the file. A second writer would be a second
// process appending to the same JSONL during a hot swap, which is exactly the
// inter-process-lock case the append already takes a lock for, and exactly the
// kind of thing that turns an audit trail into a puzzle.

func TestBaselineMarkCommandGoesThroughTheSocketAndWritesTheMark(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)

	resp := handleControlRequest(newControlState(), controlRequest{Cmd: "mark", Value: "pre-upgrade check"})
	if !resp.OK {
		t.Fatalf("mark failed: %s", resp.Error)
	}
	rows, err := baselineTail(filepath.Join(dir, ".urnetwork", baselineFileName), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("wrote %d rows, want 1", len(rows))
	}
	if rows[0].Kind != "mark" {
		t.Errorf("kind = %q, want mark", rows[0].Kind)
	}
	if rows[0].Label != "pre-upgrade check" {
		t.Errorf("label = %q, want %q", rows[0].Label, "pre-upgrade check")
	}
	// The socket must report the timestamp it recorded, so the CLI can print it
	// and the operator can correlate it with their own notes.
	if resp.Value == "" {
		t.Error("the response carries no timestamp; `baseline mark` is supposed to print it")
	}
	if _, err := time.Parse(time.RFC3339, resp.Value); err != nil {
		t.Errorf("response value %q is not an RFC3339 timestamp: %v", resp.Value, err)
	}
}

func TestBaselineMarkRejectsAnEmptyLabel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	// A mark with no label is a line a reader cannot act on and compare cannot
	// use as a boundary, so it is refused rather than written.
	for _, bad := range []string{"", "   "} {
		resp := handleControlRequest(newControlState(), controlRequest{Cmd: "mark", Value: bad})
		if resp.OK {
			t.Errorf("mark accepted the label %q; an unlabelled mark is not a usable boundary", bad)
		}
	}
	rows, _ := baselineTail(filepath.Join(dir, ".urnetwork", baselineFileName), 10)
	if len(rows) != 0 {
		t.Errorf("a rejected mark still wrote %d row(s)", len(rows))
	}
}

// A mark is an annotation, not an observation. If it carried proxy counts or
// memory figures, a reader would treat them as measurements taken at that
// moment, which they are not.
func TestBaselineMarkCarriesNoMeasurements(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	resp := handleControlRequest(newControlState(), controlRequest{Cmd: "mark", Value: "checkpoint"})
	if !resp.OK {
		t.Fatal(resp.Error)
	}
	rows, err := baselineTail(filepath.Join(dir, ".urnetwork", baselineFileName), 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	m := rows[0]
	if m.Proxies.Up != 0 || m.Desired != nil || m.TrimCap != nil || m.Host != nil || m.OOM != nil {
		t.Errorf("the mark carries measurements that a reader would take as readings: %+v", m)
	}
	if m.Version != "" {
		t.Errorf("the mark carries a version %q; it is an annotation, not a sample", m.Version)
	}
}

// Marking while the recorder is off must still work: the operator turning it
// off is not the same as losing the ability to annotate an upgrade.
func TestBaselineMarkWorksWhileTheRecorderIsOff(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	prev := baselineEnabled.Load()
	baselineEnabled.Store(false)
	t.Cleanup(func() { baselineEnabled.Store(prev) })

	resp := handleControlRequest(newControlState(), controlRequest{Cmd: "mark", Value: "with recorder off"})
	if !resp.OK {
		t.Fatalf("mark failed with the recorder off: %s", resp.Error)
	}
	rows, _ := baselineTail(filepath.Join(dir, ".urnetwork", baselineFileName), 10)
	if len(rows) != 1 {
		t.Errorf("wrote %d rows with the recorder off, want 1", len(rows))
	}
}

// The single-writer rule: the CLI must not open the file itself. This asserts
// it, because the alternative is two processes appending to one JSONL and the
// file's own comment says the append takes an inter-process lock precisely
// because a hot swap has two writers.
func TestBaselineCLIHasNoDirectFileWrite(t *testing.T) {
	// internal/urnettools may READ the file for show and compare. It must never
	// write it: `baseline mark` goes through the socket.
	b, err := os.ReadFile(filepath.Join("..", "internal", "urnettools", "baseline.go"))
	if err != nil {
		t.Skipf("internal/urnettools/baseline.go not present yet: %v", err)
	}
	src := string(b)
	for _, forbidden := range []string{
		"baselineAppend",
		"ledgerAppend",
		"os.OpenFile",
		"WriteFile",
		"O_APPEND",
	} {
		if strings.Contains(src, forbidden) {
			t.Errorf("the CLI references %s; the provider must be the only writer to %s, "+
				"so `baseline mark` has to go through the control socket", forbidden, baselineFileName)
		}
	}
}
