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

// TestGolden compares every asset and color depth against a committed rendering.
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

// TestRenderedWidth checks that a colored rendering still occupies exactly the
// cell width the asset declares, since color sequences add no cells.
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
				if w := visibleWidth(line); w > a.Cols {
					t.Errorf("%s %s line %d: %d cells, more than %d", name, d.name, i, w, a.Cols)
				}
				if n := utf8.RuneCountInString(line); n < visibleWidth(line) {
					t.Errorf("%s %s line %d: %d runes below %d cells", name, d.name, i, n, a.Cols)
				}
			}
			if w := widestRow(lines); w != a.Cols {
				t.Errorf("%s %s: widest row %d cells, want %d", name, d.name, w, a.Cols)
			}
		}
	}
}

// TestNoColorWidth checks the block shapes survive without color.
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
			if w := visibleWidth(line); w > a.Cols {
				t.Errorf("%s line %d: %d cells, more than %d", name, i, w, a.Cols)
			}
		}
		if w := widestRow(lines); w != a.Cols {
			t.Errorf("%s: widest row %d cells, want %d", name, w, a.Cols)
		}
	}
}

// widestRow reports the cell width of the widest row in a rendering.
func widestRow(lines []string) int {
	widest := 0
	for _, line := range lines {
		if w := visibleWidth(line); w > widest {
			widest = w
		}
	}
	return widest
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

// gridText joins the glyphs of a grid the way Render lays out rows.
func gridText(rows [][]Cell) string {
	lines := make([]string, len(rows))
	for i, row := range rows {
		var line strings.Builder
		for _, c := range row {
			line.WriteRune(c.Glyph)
		}
		lines[i] = strings.TrimRight(line.String(), " ")
	}
	return strings.Join(lines, "\n")
}

// TestGridMatchesRender checks that the cells a full screen view paints carry
// the glyphs of the mono rendering, keep the asset's size, and carry color
// only where the depth allows it.
func TestGridMatchesRender(t *testing.T) {
	mono := Capabilities{Depth: Mono, NoColor: true, UTF8: true}
	for _, name := range assetNames {
		a := Assets[name]
		if got, want := gridText(Grid(a, mono)), Render(a, mono); got != want {
			t.Errorf("%s: mono grid glyphs differ from the rendering\n got: %q\nwant: %q", name, got, want)
		}
		for _, caps := range []Capabilities{{Depth: TrueColor, UTF8: true}, {Depth: Color16, UTF8: true}, mono} {
			grid := Grid(a, caps)
			if len(grid) != a.Rows {
				t.Fatalf("%s: grid has %d rows, want %d", name, len(grid), a.Rows)
			}
			colored := false
			for _, row := range grid {
				if len(row) != a.Cols {
					t.Fatalf("%s: grid row has %d cells, want %d", name, len(row), a.Cols)
				}
				for _, c := range row {
					colored = colored || c.FG != nil || c.BG != nil
				}
			}
			if caps.Depth <= Color16 && colored {
				t.Errorf("%s: a depth %d grid carries color", name, caps.Depth)
			}
			if caps.Depth == TrueColor && a.Kind == "halfblock" && !colored {
				t.Errorf("%s: a truecolor grid carries no color", name)
			}
		}
	}
	if Grid(Assets["icon-flat-12x6"], Capabilities{Depth: TrueColor}) != nil {
		t.Error("a terminal without UTF-8 must get no grid")
	}
}
