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
	old := importantLogHook
	var lines []string
	importantLogHook = func(line string) { lines = append(lines, line) }
	t.Cleanup(func() { importantLogHook = old })

	effectiveTrimCapSource()
	first := strings.Join(lines, "\n")
	if !strings.Contains(first, "cannot read proxy_trim") {
		t.Fatalf("an unreadable proxy_trim must warn, got %q", first)
	}
	lines = nil
	effectiveTrimCapSource() // the same error a second time
	if got := strings.Join(lines, "\n"); strings.Contains(got, "cannot read proxy_trim") {
		t.Fatalf("the same error must warn once, not on every reload, got %q", got)
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
