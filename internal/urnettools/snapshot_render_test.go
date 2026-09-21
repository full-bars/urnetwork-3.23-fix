package urnettools

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if !sameGolden(got, want) {
		t.Fatalf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// sameGolden compares rendered output with a golden file. CRLF pairs in the
// file are treated as LF: a Windows checkout with core.autocrlf rewrites the
// file, and that is not a difference in what the program renders.
func sameGolden(got string, want []byte) bool {
	return got == strings.ReplaceAll(string(want), "\r\n", "\n")
}

func TestRenderLiveBlockFlowing(t *testing.T) {
	s := loadSnapshotFixture(t, "node_snapshot_v1.json")
	assertGolden(t, "live_flowing", renderLiveBlock(s, liveOpts{Color: true}))
}

func TestRenderLiveBlockIdleWithHint(t *testing.T) {
	s := loadSnapshotFixture(t, "node_snapshot_v1_idle_minimal.json")
	assertGolden(t, "live_idle_hint", renderLiveBlock(s, liveOpts{Color: true}))
}

func TestRenderLiveBlockMinimalOptionals(t *testing.T) {
	s := loadSnapshotFixture(t, "node_snapshot_v1.json")
	s.PreviousVersion = nil
	s.Resources.MemLimitBytes, s.Resources.RSSBytes = nil, nil
	s.Resources.OpenFDs, s.Resources.FDLimit = nil, nil
	out := renderLiveBlock(s, liveOpts{})
	for _, bad := range []string{"limit", "RSS", "fds", " to "} {
		if strings.Contains(out, bad) {
			t.Fatalf("omitted field %q leaked into output:\n%s", bad, out)
		}
	}
	assertGolden(t, "live_minimal_optionals", out)
}

func TestRenderLiveBlockNarrow80(t *testing.T) {
	s := loadSnapshotFixture(t, "node_snapshot_v1.json")
	for len(s.Rate.HistoryBps) < 60 {
		s.Rate.HistoryBps = append(s.Rate.HistoryBps, s.Rate.HistoryBps...)
	}
	s.Rate.HistoryBps = s.Rate.HistoryBps[:60]
	out := renderLiveBlock(s, liveOpts{Width: 80})
	for _, ln := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if n := len([]rune(ln)); n > 80 {
			t.Fatalf("line is %d columns, want <= 80: %q", n, ln)
		}
	}
	assertGolden(t, "live_narrow_80", out)
}

func TestRenderLiveBlockNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("TERM", "xterm-256color")
	s := loadSnapshotFixture(t, "node_snapshot_v1.json")
	o := liveOptsFromEnv(true, 100)
	if o.Color || o.ASCII {
		t.Fatalf("NO_COLOR opts = %+v; want no color, unicode sparkline", o)
	}
	out := renderLiveBlock(s, o)
	if strings.Contains(out, "\x1b") {
		t.Fatalf("NO_COLOR output contains an escape: %q", out)
	}
	assertGolden(t, "live_no_color", out)
}

func TestRenderLiveBlockDumbTerminal(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	s := loadSnapshotFixture(t, "node_snapshot_v1.json")
	o := liveOptsFromEnv(true, 100)
	if o.Color || !o.ASCII {
		t.Fatalf("TERM=dumb opts = %+v; want plain, ascii", o)
	}
	out := renderLiveBlock(s, o)
	for _, r := range out {
		if r > 127 {
			t.Fatalf("non-ascii rune %q on a dumb terminal:\n%s", r, out)
		}
	}
	assertGolden(t, "live_dumb", out)
}

func TestLiveOptsFromEnvColorNeedsTTY(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm")
	if liveOptsFromEnv(false, 80).Color {
		t.Fatal("color enabled on a non-terminal")
	}
	if !liveOptsFromEnv(true, 80).Color {
		t.Fatal("color disabled on a terminal with no NO_COLOR")
	}
}

func TestRenderLiveBlockStatesAndFlags(t *testing.T) {
	s := loadSnapshotFixture(t, "node_snapshot_v1.json")
	cases := []struct{ state, code string }{
		{"flowing", "\x1b[32m"}, {"idle", "\x1b[33m"}, {"degraded", "\x1b[31m"},
		{"starting", "\x1b[2m"}, {"stopped", "\x1b[2m"},
	}
	for _, c := range cases {
		s.State = c.state
		out := renderLiveBlock(s, liveOpts{Color: true})
		if !strings.Contains(out, c.code+strings.ToUpper(c.state)+"\x1b[0m") {
			t.Errorf("state %s not painted %q:\n%s", c.state, c.code, out)
		}
	}
	s.State = "flowing"
	if strings.Contains(renderLiveBlock(s, liveOpts{}), "RESTART PENDING") {
		t.Error("RESTART PENDING shown when not pending")
	}
	s.RestartPending = true
	first := strings.SplitN(renderLiveBlock(s, liveOpts{}), "\n", 2)[0]
	if !strings.Contains(first, "FLOWING") || !strings.HasSuffix(first, "RESTART PENDING") {
		t.Errorf("header = %q; want state then RESTART PENDING flag", first)
	}
}

func TestRenderLiveBlockIdleHintOnlyWhenIdle(t *testing.T) {
	s := loadSnapshotFixture(t, "node_snapshot_v1_idle_minimal.json")
	if !strings.Contains(renderLiveBlock(s, liveOpts{}), "why idle") {
		t.Fatal("idle node lost its hint")
	}
	s.State = "degraded" // hint present but state is not idle
	if strings.Contains(renderLiveBlock(s, liveOpts{}), "why idle") {
		t.Fatal("hint shown for a non-idle state")
	}
}

func TestFormatUptime(t *testing.T) {
	for _, c := range []struct {
		in   float64
		want string
	}{{273871, "3d 4h"}, {3600*5 + 720, "5h 12m"}, {450, "7m 30s"}, {42.9, "42s"}, {-3, "0s"}} {
		if got := formatUptime(c.in); got != c.want {
			t.Errorf("formatUptime(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	for _, c := range []struct {
		in   float64
		want string
	}{{0, "0 B"}, {1536, "1.5 KiB"}, {1932735283, "1.8 GiB"}, {4294967296, "4.0 GiB"}, {5 << 20, "5.0 MiB"}} {
		if got := formatBytes(c.in); got != c.want {
			t.Errorf("formatBytes(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLastRestartText(t *testing.T) {
	s := loadSnapshotFixture(t, "node_snapshot_v1.json")
	if got := lastRestartText(s); got != "last restart: update (v3.23.0-fix.32.0 to v3.23.0-fix.32.1)" {
		t.Fatalf("got %q", got)
	}
	same := s.Version
	s.PreviousVersion = &same
	if got := lastRestartText(s); got != "last restart: update" {
		t.Fatalf("unchanged version: got %q", got)
	}
	s.Restart.Reason = ""
	if got := lastRestartText(s); got != "" {
		t.Fatalf("empty reason: got %q", got)
	}
}

func TestRenderProviderTable(t *testing.T) {
	flow := loadSnapshotFixture(t, "node_snapshot_v1.json")
	idle := loadSnapshotFixture(t, "node_snapshot_v1_idle_minimal.json")
	out := renderProviderTable([]providerRow{
		{Name: "urnetwork-a.service", Running: true, Snap: flow},
		{Name: "urnetwork-b.service", Running: true, Snap: idle},
		{Name: "urnetwork-c.service", Running: true, Version: "v3.22.0"},
		{Name: "urnetwork-d.service"},
	}, liveOpts{})
	assertGolden(t, "live_provider_table", out)
}

// Golden files are compared byte for byte, and a Windows checkout with
// core.autocrlf (the default on GitHub's Windows runners) rewrites them with
// CRLF while the program renders LF. The comparison must not care.
func TestSameGoldenIgnoresLineEndingConversion(t *testing.T) {
	const lf = "line one\nline two\n"
	if !sameGolden(lf, []byte(lf)) {
		t.Fatal("identical text must match")
	}
	if !sameGolden(lf, []byte("line one\r\nline two\r\n")) {
		t.Fatal("a golden file checked out with CRLF must still match LF output")
	}
	if sameGolden(lf, []byte("line one\nline TWO\n")) {
		t.Fatal("a real difference must still fail")
	}
	if sameGolden(lf, []byte("line one\nline two")) {
		t.Fatal("a missing trailing newline is a real difference")
	}
	// A lone CR inside a line is content, not a line ending.
	if sameGolden("a\rb\n", []byte("a\nb\n")) {
		t.Fatal("only CRLF pairs are normalized")
	}
}
