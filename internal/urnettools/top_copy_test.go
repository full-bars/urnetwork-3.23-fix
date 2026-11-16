package urnettools

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// copyGoldenBuffer is a fixed screen: a box, a styled row with trailing
// spaces, a second row and an empty row. The copy must carry the text and the
// box, not the styles or the trailing blanks.
func copyGoldenBuffer() *tui.Buffer {
	b := tui.New(40, 6)
	in := tui.DrawBox(b, "Top", tui.BorderRounded, tui.Style{FG: tui.Ansi(6)}, tui.Style{FG: tui.Ansi(6)}.With(tui.AttrBold), false)
	in.Put("hello  world   ", 0, 0, tui.Style{FG: tui.Ansi(1)})
	in.Put("x", 1, 2, tui.Style{})
	return b
}

func TestCopyTextGolden(t *testing.T) {
	got := copyText(copyGoldenBuffer(), false)
	assertGolden(t, "top_copy_40x6", got)
}

func TestCopyTextPlainGolden(t *testing.T) {
	got := copyText(copyGoldenBuffer(), true)
	assertGolden(t, "top_copy_40x6_plain", got)
}

func TestCopyTextDropsStylesAndTrailingSpaces(t *testing.T) {
	got := copyText(copyGoldenBuffer(), false)
	if got == "" {
		t.Fatal("empty copy")
	}
	for _, line := range splitLines(got) {
		if len(line) > 0 && line[len(line)-1] == ' ' {
			t.Fatalf("trailing space in %q", line)
		}
	}
	// The box characters survive the non-plain copy.
	if !containsRune(got, '╭') || !containsRune(got, '│') {
		t.Fatalf("box characters were lost:\n%s", got)
	}
	plain := copyText(copyGoldenBuffer(), true)
	if containsRune(plain, '╭') || containsRune(plain, '│') || containsRune(plain, '─') {
		t.Fatalf("plain copy kept box characters:\n%s", plain)
	}
	for _, r := range []rune{'+', '-', '|'} {
		if !containsRune(plain, r) {
			t.Fatalf("plain copy lost %q:\n%s", r, plain)
		}
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}

func TestWriteTopCopy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	now := time.Date(2026, 9, 19, 16, 4, 31, 0, time.UTC)
	path, err := writeTopCopy("screen\n", now)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "urnet-tools", "top-20260919-160431.txt"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "screen\n" {
		t.Fatalf("content = %q", b)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Join(dir, "urnet-tools"))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, want 0700", di.Mode().Perm())
	}
}

func TestTopStateDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	if got := topStateDir(); got != filepath.Join(dir, "urnet-tools") {
		t.Fatalf("topStateDir = %q", got)
	}
	t.Setenv("XDG_STATE_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := topStateDir(); got != filepath.Join(home, ".local", "state", "urnet-tools") {
		t.Fatalf("topStateDir = %q", got)
	}
}
