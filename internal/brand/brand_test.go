package brand

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAssetsAndCapabilities(t *testing.T) {
	for _, name := range []string{"icon-flat-24x12", "icon-flat-12x6", "icon-flat-8x4", "wordmark-80x6", "wordmark-100x7"} {
		a := Assets[name]
		if a == nil {
			t.Fatalf("missing %s", name)
		}
		for _, depth := range []Depth{Mono, Color16, Color256, TrueColor} {
			s := Render(a, Capabilities{Depth: depth, UTF8: true})
			if len(strings.Split(s, "\n")) != a.Rows {
				t.Errorf("%s depth %d: wrong height", name, depth)
			}
			if depth == Mono || depth == Color16 || strings.HasPrefix(name, "wordmark") {
				if strings.Contains(s, "\x1b") {
					t.Errorf("%s depth %d: unexpected color", name, depth)
				}
				for _, line := range strings.Split(s, "\n") {
					if utf8.RuneCountInString(line) > a.Cols {
						t.Errorf("%s: over width", name)
					}
				}
			}
		}
		if got := Render(a, Capabilities{}); got != "URnetwork" {
			t.Errorf("ASCII %s: %q", name, got)
		}
		if got := Render(a, Capabilities{Depth: TrueColor, UTF8: true, NoColor: true}); strings.Contains(got, "\x1b") {
			t.Errorf("NO_COLOR %s", name)
		}
	}
}

func TestChoose(t *testing.T) {
	for _, tc := range []struct {
		width int
		want  string
	}{{39, ""}, {40, "icon-flat-8x4"}, {60, "icon-flat-12x6"}, {100, "wordmark-80x6"}, {120, "wordmark-100x7"}} {
		a := Choose(tc.width, 30)
		got := ""
		if a != nil {
			got = a.Name
		}
		if got != tc.want {
			t.Errorf("width %d: %q, want %q", tc.width, got, tc.want)
		}
	}
	if Choose(120, 3) != nil {
		t.Fatal("insufficient height")
	}
}

func TestBeside(t *testing.T) {
	if got := Beside("\x1b[31mab\x1b[0m", 2, "hello", false); got != "\x1b[31mab\x1b[0m  hello" {
		t.Fatalf("colored layout: %q", got)
	}
	got := Beside("ab\ncd\nef", 2, "hello", true)
	if got != "ab\ncd  hello\nef" {
		t.Fatalf("got %q", got)
	}
}
