package brand

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

var update = flag.Bool("update", false, "rewrite the golden renderings")

var assetNames = []string{"icon-flat-24x12", "icon-flat-12x6", "icon-flat-8x4", "wordmark-80x6", "wordmark-100x7"}

var depths = []struct {
	name  string
	depth Depth
}{
	{"truecolor", TrueColor},
	{"256", Color256},
	{"16", Color16},
	{"mono", Mono},
}

// TestGolden compares every asset and colour depth against a committed rendering.
func TestGolden(t *testing.T) {
	for _, name := range assetNames {
		a := Assets[name]
		if a == nil {
			t.Fatalf("missing %s", name)
		}
		for _, d := range depths {
			got := Render(a, Capabilities{Depth: d.depth, UTF8: true})
			path := filepath.Join("testdata", name+"."+d.name+".golden")
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				continue
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s: %v (run go test -run TestGolden -update)", path, err)
			}
			if got != string(want) {
				t.Errorf("%s %s: rendering changed\n got: %q\nwant: %q", name, d.name, got, string(want))
			}
		}
	}
}

// TestRenderedWidth checks that a coloured rendering still occupies exactly the
// cell width the asset declares, since colour sequences add no cells.
func TestRenderedWidth(t *testing.T) {
	for _, name := range assetNames {
		a := Assets[name]
		for _, d := range depths {
			rendered := Render(a, Capabilities{Depth: d.depth, UTF8: true})
			lines := strings.Split(rendered, "\n")
			if len(lines) != a.Rows {
				t.Fatalf("%s %s: %d lines, want %d", name, d.name, len(lines), a.Rows)
			}
			for i, line := range lines {
				if w := visibleWidth(line); w != a.Cols {
					t.Errorf("%s %s line %d: %d cells, want %d", name, d.name, i, w, a.Cols)
				}
				if n := utf8.RuneCountInString(line); n < visibleWidth(line) {
					t.Errorf("%s %s line %d: %d runes below %d cells", name, d.name, i, n, a.Cols)
				}
			}
		}
	}
}

// TestNoColorWidth checks the block shapes survive without colour.
func TestNoColorWidth(t *testing.T) {
	for _, name := range assetNames {
		a := Assets[name]
		rendered := Render(a, Capabilities{Depth: TrueColor, UTF8: true, NoColor: true})
		if strings.Contains(rendered, "\x1b") {
			t.Errorf("%s: NO_COLOR rendering carries escape sequences", name)
		}
		lines := strings.Split(rendered, "\n")
		if len(lines) != a.Rows {
			t.Fatalf("%s: %d lines, want %d", name, len(lines), a.Rows)
		}
		for i, line := range lines {
			if w := visibleWidth(line); w != a.Cols {
				t.Errorf("%s line %d: %d cells, want %d", name, i, w, a.Cols)
			}
		}
	}
}

func TestAssetsAndCapabilities(t *testing.T) {
	for _, name := range assetNames {
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
			}
			if (depth == Color256 || depth == TrueColor) && !strings.HasPrefix(name, "wordmark") {
				if !strings.Contains(s, "\x1b") {
					t.Errorf("%s depth %d: expected color", name, depth)
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
