package urnettools

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var markerLineRe = regexp.MustCompile(`^(update|hotswap|manual) \d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z\n$`)

func TestRestartMarkerLineFormat(t *testing.T) {
	at := time.Date(2026, 9, 19, 16, 4, 31, 0, time.FixedZone("x", 3600))
	if got := string(restartMarkerLine("update", at)); got != "update 2026-09-19T15:04:31Z\n" {
		t.Fatalf("got %q; want reason then RFC3339 UTC", got)
	}
}

func TestWriteRestartMarkerWritesOneLine(t *testing.T) {
	dir := t.TempDir()
	for _, reason := range []string{"update", "hotswap", "manual"} {
		if err := writeRestartMarkerIn("", dir, reason, time.Now()); err != nil {
			t.Fatalf("%s: %v", reason, err)
		}
		b, err := os.ReadFile(filepath.Join(dir, ".restart-reason"))
		if err != nil {
			t.Fatal(err)
		}
		if !markerLineRe.Match(b) || !strings.HasPrefix(string(b), reason+" ") {
			t.Fatalf("%s: marker = %q", reason, b)
		}
	}
	// A later write replaces an earlier marker instead of appending.
	b, _ := os.ReadFile(filepath.Join(dir, ".restart-reason"))
	if strings.Count(string(b), "\n") != 1 {
		t.Fatalf("marker must stay one line, got %q", b)
	}
}

func TestWriteRestartMarkerRejectsBadInput(t *testing.T) {
	if err := writeRestartMarkerIn("", t.TempDir(), "crash", time.Now()); err == nil {
		t.Fatal("unknown reason accepted")
	}
	if err := writeRestartMarkerIn("", "", "update", time.Now()); err == nil {
		t.Fatal("empty state dir accepted")
	}
	if err := writeRestartMarkerIn("", filepath.Join(t.TempDir(), "missing"), "update", time.Now()); err == nil {
		t.Fatal("missing state dir accepted (the writer must not create one)")
	}
}

// A failed marker write must never abort a restart: recordRestartReason has
// no error result and swallows a bad state dir.
func TestRecordRestartReasonNeverFails(t *testing.T) {
	recordRestartReason(Provider{}, "manual")
	recordRestartReason(Provider{StateDir: filepath.Join(t.TempDir(), "missing")}, "manual")
	dir := t.TempDir()
	recordRestartReason(Provider{StateDir: dir}, "manual")
	if b, err := os.ReadFile(filepath.Join(dir, ".restart-reason")); err != nil || !markerLineRe.Match(b) {
		t.Fatalf("marker = %q, %v", b, err)
	}
}
