package urnettools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLogoTerminal makes the terminal look like a real one of the given size
// and returns a restore function.
func fakeLogoTerminal(t *testing.T, width, height int) {
	t.Helper()
	origTTY, origSize := logoStdoutIsTerminal, logoStdoutSize
	t.Cleanup(func() { logoStdoutIsTerminal, logoStdoutSize = origTTY, origSize })
	logoStdoutIsTerminal = func() bool { return true }
	logoStdoutSize = func() (int, int, bool) { return width, height, true }
}

func fakeLogoEnv(t *testing.T, values map[string]string) {
	t.Helper()
	orig := logoGetenv
	t.Cleanup(func() { logoGetenv = orig })
	logoGetenv = func(key string) string { return values[key] }
}

func fakeLogoPrefs(t *testing.T, dir string) {
	t.Helper()
	orig := logoPrefsPath
	t.Cleanup(func() { logoPrefsPath = orig })
	logoPrefsPath = func() string { return filepath.Join(dir, "top.conf") }
}

func resetLogoFlag(t *testing.T) {
	t.Helper()
	orig := logoOffByFlag
	t.Cleanup(func() { logoOffByFlag = orig })
	logoOffByFlag = false
}

func TestLogoAllowedNeedsATerminal(t *testing.T) {
	resetLogoFlag(t)
	origTTY := logoStdoutIsTerminal
	t.Cleanup(func() { logoStdoutIsTerminal = origTTY })
	logoStdoutIsTerminal = func() bool { return false }
	fakeLogoEnv(t, map[string]string{"TERM": "xterm-256color"})
	fakeLogoPrefs(t, t.TempDir())

	if logoAllowed(logoMode{}) {
		t.Fatal("logo must not be drawn when stdout is not a terminal")
	}
}

func TestLogoAllowedHonoursTheSwitch(t *testing.T) {
	resetLogoFlag(t)
	fakeLogoTerminal(t, 120, 40)
	fakeLogoPrefs(t, t.TempDir())
	fakeLogoEnv(t, map[string]string{"TERM": "xterm-256color"})

	if !logoAllowed(logoMode{}) {
		t.Fatal("logo must be on by default on a terminal")
	}
	for _, mode := range []logoMode{{Plain: true}, {JSON: true}, {Brief: true}} {
		if logoAllowed(mode) {
			t.Errorf("logo must be off in mode %+v", mode)
		}
	}
	logoOffByFlag = true
	if logoAllowed(logoMode{}) {
		t.Fatal("--no-logo must hide the logo")
	}
	logoOffByFlag = false

	for _, value := range []string{"1", "true", "on", "yes", "TRUE"} {
		fakeLogoEnv(t, map[string]string{"URNET_NO_LOGO": value})
		if logoAllowed(logoMode{}) {
			t.Errorf("URNET_NO_LOGO=%q must hide the logo", value)
		}
	}
	for _, value := range []string{"", "0", "false", "off"} {
		fakeLogoEnv(t, map[string]string{"TERM": "xterm-256color", "URNET_NO_LOGO": value})
		if !logoAllowed(logoMode{}) {
			t.Errorf("URNET_NO_LOGO=%q must keep the logo", value)
		}
	}
}

func TestLogoAllowedHonoursTheSetting(t *testing.T) {
	resetLogoFlag(t)
	fakeLogoTerminal(t, 120, 40)
	fakeLogoEnv(t, map[string]string{"TERM": "xterm-256color"})
	dir := t.TempDir()
	fakeLogoPrefs(t, dir)
	path := filepath.Join(dir, "top.conf")

	if err := os.WriteFile(path, []byte("ui.logo = off\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if logoAllowed(logoMode{}) {
		t.Fatal("ui.logo = off must hide the logo")
	}
	if err := os.WriteFile(path, []byte("ui.logo = on\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !logoAllowed(logoMode{}) {
		t.Fatal("ui.logo = on must keep the logo")
	}
}

func TestLogoSettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "top.conf")
	off := false
	if err := saveTopSettings(path, topSettings{Theme: "nord", Logo: &off}); err != nil {
		t.Fatal(err)
	}
	got := loadTopSettings(path)
	if got.logoOn() {
		t.Fatal("ui.logo = off was not saved")
	}
	if got.Theme != "nord" {
		t.Fatalf("theme was not preserved: %q", got.Theme)
	}
	on := true
	if err := saveTopSettings(path, topSettings{Logo: &on}); err != nil {
		t.Fatal(err)
	}
	if !loadTopSettings(path).logoOn() {
		t.Fatal("ui.logo = on was not saved")
	}
	// A file without the key keeps the default, which is on.
	if err := os.WriteFile(path, []byte("theme = nord\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !loadTopSettings(path).logoOn() {
		t.Fatal("a missing ui.logo must default to on")
	}
}

func TestLogoArtworkWidthRules(t *testing.T) {
	resetLogoFlag(t)
	fakeLogoEnv(t, map[string]string{"TERM": "xterm-256color"})
	fakeLogoPrefs(t, t.TempDir())
	for _, tc := range []struct {
		width  int
		height int
		icon8  bool
		icon12 bool
		word   string
	}{
		{39, 40, false, false, ""},
		{40, 40, true, false, ""},
		{59, 40, true, false, ""},
		{60, 40, true, true, ""},
		{99, 40, true, true, ""},
		{100, 40, true, true, "wordmark-80x6"},
		{120, 40, true, true, "wordmark-100x7"},
		{120, 5, true, false, ""},
	} {
		fakeLogoTerminal(t, tc.width, tc.height)
		if art, cols := logoIcon8(); (art != "") != tc.icon8 {
			t.Errorf("width %d height %d: 8x4 icon present = %v, want %v", tc.width, tc.height, art != "", tc.icon8)
		} else if art != "" && cols != 8 {
			t.Errorf("8x4 icon reports %d columns", cols)
		}
		if art, _ := logoIcon12(); (art != "") != tc.icon12 {
			t.Errorf("width %d height %d: 12x6 icon present = %v, want %v", tc.width, tc.height, art != "", tc.icon12)
		}
		art, _ := logoWordmark()
		name := ""
		if art != "" {
			name = wordmarkName(art)
		}
		if name != tc.word {
			t.Errorf("width %d: wordmark %q, want %q", tc.width, name, tc.word)
		}
	}
}

// wordmarkName identifies which wordmark a rendering came from by its width.
func wordmarkName(art string) string {
	lines := strings.Split(art, "\n")
	if len(lines) < 2 {
		return "not-block-art"
	}
	if n := len([]rune(lines[1])); n >= 100 {
		return "wordmark-100x7"
	}
	return "wordmark-80x6"
}

func TestVersionLogo(t *testing.T) {
	resetLogoFlag(t)
	fakeLogoEnv(t, map[string]string{"TERM": "xterm-256color"})
	fakeLogoPrefs(t, t.TempDir())
	origVersion := ToolVersion
	t.Cleanup(func() { ToolVersion = origVersion })
	ToolVersion = "v9.9.9"

	// A pipe gets the plain version line and nothing else.
	origTTY := logoStdoutIsTerminal
	t.Cleanup(func() { logoStdoutIsTerminal = origTTY })
	logoStdoutIsTerminal = func() bool { return false }
	if got := captureStdout(t, printToolVersion); got != "v9.9.9\n" {
		t.Fatalf("piped version output %q", got)
	}

	fakeLogoTerminal(t, 100, 30)
	got := captureStdout(t, printToolVersion)
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("logo version output has %d lines:\n%s", len(lines), got)
	}
	if !strings.Contains(got, "v9.9.9") {
		t.Fatalf("version text missing: %q", got)
	}
	if !strings.Contains(lines[0], "▄") && !strings.Contains(lines[0], "█") {
		t.Fatalf("first line carries no icon: %q", lines[0])
	}

	logoOffByFlag = true
	if got := captureStdout(t, printToolVersion); got != "v9.9.9\n" {
		t.Fatalf("--no-logo version output %q", got)
	}

	// A terminal without block characters keeps the plain version line.
	logoOffByFlag = false
	fakeLogoEnv(t, map[string]string{"TERM": "dumb"})
	if got := captureStdout(t, printToolVersion); got != "v9.9.9\n" {
		t.Fatalf("dumb terminal version output %q", got)
	}
}

func TestHelpParagraphKeepsText(t *testing.T) {
	resetLogoFlag(t)
	fakeLogoEnv(t, map[string]string{"TERM": "xterm-256color"})
	fakeLogoPrefs(t, t.TempDir())
	const text = "Show provider service status."

	origTTY := logoStdoutIsTerminal
	t.Cleanup(func() { logoStdoutIsTerminal = origTTY })
	logoStdoutIsTerminal = func() bool { return false }
	if got := logoParagraph(text); got != text {
		t.Fatalf("without a terminal the paragraph changed: %q", got)
	}

	fakeLogoTerminal(t, 100, 30)
	got := logoParagraph(text)
	if !strings.Contains(got, text) {
		t.Fatalf("paragraph text missing from %q", got)
	}
	if len(strings.Split(got, "\n")) != 4 {
		t.Fatalf("paragraph block has %d lines:\n%s", len(strings.Split(got, "\n")), got)
	}

	logoOffByFlag = true
	if got := logoParagraph(text); got != text {
		t.Fatalf("--no-logo paragraph changed: %q", got)
	}
}

func TestRootTitleLogo(t *testing.T) {
	resetLogoFlag(t)
	fakeLogoEnv(t, map[string]string{"TERM": "xterm-256color"})
	fakeLogoPrefs(t, t.TempDir())
	const title = "urnet-tools — provider-aware URnetwork manager"

	fakeLogoTerminal(t, 100, 30)
	got := logoTitle(title)
	lines := strings.Split(got, "\n")
	if len(lines) != 6 {
		t.Fatalf("title block has %d lines:\n%s", len(lines), got)
	}
	if !strings.Contains(got, title) {
		t.Fatalf("title missing: %q", got)
	}

	logoOffByFlag = true
	if got := logoTitle(title); got != title {
		t.Fatalf("--no-logo title changed: %q", got)
	}
}

func TestStatusLogo(t *testing.T) {
	resetLogoFlag(t)
	fakeLogoEnv(t, map[string]string{"TERM": "xterm-256color"})
	fakeLogoPrefs(t, t.TempDir())
	fakeLogoTerminal(t, 120, 40)

	got := captureStdout(t, func() { printStatusLogo(logoMode{}) })
	if !strings.Contains(got, "▗") || !strings.Contains(got, "▟") {
		t.Fatalf("status logo missing block glyphs:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Fatalf("status logo must leave a blank line after it: %q", got)
	}

	// --plain keeps the plain output byte for byte.
	if got := captureStdout(t, func() { printStatusLogo(logoMode{Plain: true}) }); got != "" {
		t.Fatalf("--plain status printed %q", got)
	}

	logoOffByFlag = true
	if got := captureStdout(t, func() { printStatusLogo(logoMode{}) }); got != "" {
		t.Fatalf("--no-logo status printed %q", got)
	}

	// Too narrow for the wordmark: nothing is printed.
	logoOffByFlag = false
	fakeLogoTerminal(t, 99, 40)
	if got := captureStdout(t, func() { printStatusLogo(logoMode{}) }); got != "" {
		t.Fatalf("narrow status printed %q", got)
	}

	// A terminal without block characters keeps the plain output.
	fakeLogoTerminal(t, 120, 40)
	fakeLogoEnv(t, map[string]string{"TERM": "dumb"})
	if got := captureStdout(t, func() { printStatusLogo(logoMode{}) }); got != "" {
		t.Fatalf("dumb terminal status printed %q", got)
	}
}

func TestStripLogoFlag(t *testing.T) {
	resetLogoFlag(t)
	got := stripLogoFlag([]string{"status", "--no-logo", "--plain"})
	if len(got) != 2 || got[0] != "status" || got[1] != "--plain" {
		t.Fatalf("stripLogoFlag left %q", got)
	}
	if !logoOffByFlag {
		t.Fatal("stripLogoFlag did not record --no-logo")
	}
}
