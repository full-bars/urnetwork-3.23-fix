package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The kill switch is a kill switch: turning it off has to FORGET the standing
// automatic cap, not merely stop enforcing it. Nothing ages the cap while the
// mode is off (the reduce and relax logic never runs), so a cap set days ago
// survives untouched and binds again the moment the switch goes back on,
// shedding proxies on the very start the operator flipped it on to let the pool
// grow.
func TestUnreadableProxyTrimStillAppliesTheAutomaticCap(t *testing.T) {
	withTempHome(t)
	dir, _ := oomCapDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A DIRECTORY at the proxy_trim path: os.ReadFile returns EISDIR, which is
	// NOT IsNotExist, so readTrimTarget reports a real error.
	trimPath, err := proxyTrimPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(trimPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := oomWriteJSON(filepath.Join(dir, "oom_cap.json"), oomCapState{Cap: 500}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("URNETWORK_OOM_CAP", "on")

	cap, source, err := effectiveTrimCapSource()
	if err != nil {
		t.Fatalf("an unreadable operator file must not fail the whole lookup: %v", err)
	}
	if cap != 500 || source != trimCapOOM {
		t.Fatalf("want the automatic cap 500 to still apply, got cap=%d source=%q", cap, source)
	}
}

// The same file, readable or not, must never be blamed for a decision the
// automatic logic made, and the operator must be told their cap is being
// ignored rather than discovering it when the pool stops shrinking.
func TestUnreadableProxyTrimWarnsOnce(t *testing.T) {
	withTempHome(t)
	dir, _ := oomCapDir()
	_ = os.MkdirAll(dir, 0o700)
	trimPath, _ := proxyTrimPath()
	if err := os.MkdirAll(trimPath, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("URNETWORK_OOM_CAP", "on")
	trimUnreadableReset()
	t.Cleanup(trimUnreadableReset)
	drainDeferredCrit() // start from an empty queue

	// First sighting warns on the ramlog and queues the events.log line.
	out := captureTlog(t, func() { effectiveTrimCapSource() })
	if !strings.Contains(out, "cannot read proxy_trim") {
		t.Fatalf("an unreadable proxy_trim must warn, got %q", out)
	}
	if strings.Contains(out, "\n\n") {
		t.Fatalf("a warning must not be followed by a blank line, got %q", out)
	}
	first := drainDeferredCrit()
	if len(first) != 1 || !strings.Contains(first[0], "cannot read proxy_trim") {
		t.Fatalf("the warning must queue exactly one events.log line, got %q", first)
	}

	// A repeat of the same error says nothing at all, not even a blank line.
	repeat := captureTlog(t, func() { effectiveTrimCapSource() })
	if strings.TrimSpace(repeat) != "" {
		t.Fatalf("the same error must warn once, not on every reload, got %q", repeat)
	}
	if got := drainDeferredCrit(); len(got) != 0 {
		t.Fatalf("a repeat must queue nothing, got %q", got)
	}
}

// readTrimTarget keeps its own contract for the CLI paths that call it
// directly: it still reports the real error, so a command that writes the file
// can tell the operator what went wrong.
func TestReadTrimTargetStillReportsUnreadableFile(t *testing.T) {
	withTempHome(t)
	trimPath, _ := proxyTrimPath()
	if err := os.MkdirAll(trimPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readTrimTarget(); err == nil {
		t.Fatal("readTrimTarget must still report an unreadable file to its direct callers")
	}
}
