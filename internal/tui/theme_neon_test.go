package tui

import (
	"math"
	"reflect"
	"testing"
)

// neonBackdrop is the near black background the neon theme is designed for.
// The theme does not paint a background of its own, so the contrast checks
// below measure every role against this colour.
const neonBackdrop = "#0A0B0F"

// xtermHex and ansiHex are the palette entries the neon mapping uses, spelled
// out so a test can measure the contrast of the 256 and 16 colour variants.
var (
	xtermHex = map[uint8]string{
		32:  "#0087d7", // cube, IPv6 blue
		33:  "#0087ff", // cube, accent and H1+ blue
		103: "#8787af", // cube, border gray
		119: "#87ff5f", // cube, brand green
		204: "#ff5f87", // cube, brand coral
		229: "#ffffaf", // cube, pale lime
		246: "#949494", // gray ramp, dim text
	}
	ansiHex = map[uint8]string{
		6:  "#008080", // teal, IPv6
		8:  "#808080", // bright black, dim text and border
		9:  "#ff0000", // bright red, bad
		10: "#00ff00", // bright green, ok
		11: "#ffff00", // bright yellow, warn
		13: "#ff00ff", // bright magenta, packets
		14: "#00ffff", // bright cyan, accent and H1+
	}
)

func hexRGB(h string) (uint32, uint32, uint32) {
	if len(h) == 7 && h[0] == '#' {
		h = h[1:]
	}
	var v uint32
	for _, c := range h {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= uint32(c - '0')
		case c >= 'a' && c <= 'f':
			v |= uint32(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v |= uint32(c-'A') + 10
		}
	}
	return (v >> 16) & 0xff, (v >> 8) & 0xff, v & 0xff
}

// relativeLuminance is the WCAG 2.1 definition for a truecolor hex value.
func relativeLuminance(h string) float64 {
	r, g, b := hexRGB(h)
	lin := func(c uint32) float64 {
		f := float64(c) / 255
		if f <= 0.04045 {
			return f / 12.92
		}
		return math.Pow((f+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

// contrastRatio is (Lmax+0.05)/(Lmin+0.05) for two hex colours (>= 1).
func contrastRatio(a, b string) float64 {
	la, lb := relativeLuminance(a), relativeLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// neonRoles returns every foreground role of the theme by name, so the checks
// below run over the whole palette rather than a hand written list that a new
// role can slip past.
func neonRoles(th Theme) map[string]Color {
	return map[string]Color{
		"ok":          th.OK.FG,
		"warn":        th.Warn.FG,
		"bad":         th.Bad.FG,
		"dim":         th.Dim.FG,
		"accent":      th.Accent.FG,
		"graph":       th.Graph.FG,
		"border":      th.Border.FG,
		"seriesBytes": th.SeriesBytesFG,
		"seriesPkts":  th.SeriesPacketsFG,
		"seriesV6":    th.SeriesV6FG,
		"seriesH1":    th.SeriesH1FG,
	}
}

// textRoles must clear the WCAG AA text threshold; graphRoles only have to be
// distinguishable, which is the 3:1 non-text threshold.
var (
	textRoles  = []string{"ok", "warn", "bad", "dim", "accent"}
	graphRoles = []string{"graph", "border", "seriesBytes", "seriesPkts", "seriesV6", "seriesH1"}
)

// TestNeonRolesComplete fails when a role is added to the theme without a
// neon value: every Style field the default theme sets must be set in neon
// too, and every role the theme adds must be set as well.
func TestNeonRolesComplete(t *testing.T) {
	p := neonPalette
	for _, r := range []neonRole{
		p.ok, p.warn, p.bad, p.dim, p.accent, p.graph, p.border,
		p.bytes, p.packets, p.v6, p.h1,
	} {
		if r.hex == "" || r.hex[0] != '#' {
			t.Errorf("palette role %+v has no truecolor value", r)
		}
	}
	base := reflect.ValueOf(DefaultTheme())
	th := reflect.ValueOf(NeonTheme())
	typ := base.Type()
	for i := 0; i < typ.NumField(); i++ {
		switch typ.Field(i).Name {
		case "Name", "Frame", "ASCII":
			continue // not colours
		}
		if base.Field(i).IsZero() {
			continue // the default theme does not use this role
		}
		if th.Field(i).IsZero() {
			t.Errorf("neon leaves %s unset while the default theme sets it", typ.Field(i).Name)
		}
	}
	for name, c := range neonRoles(NeonTheme()) {
		if c == (Color{}) {
			t.Errorf("neon role %s is zero", name)
		}
	}
	if th := NeonTheme(); th.IconOK == 0 || th.IconWarn == 0 || th.IconBad == 0 {
		t.Error("neon icons must be set, so state is never read by colour alone")
	}
}

// TestNeonContrast measures every role against the dark background at all
// three depths and fails below 4.5 for text roles and 3 for graph roles.
func TestNeonContrast(t *testing.T) {
	hexOf := func(t *testing.T, c Color) string {
		t.Helper()
		r, g, b := c.Components()
		return "#" + hexByte(r) + hexByte(g) + hexByte(b)
	}
	xtermOf := func(t *testing.T, c Color) string {
		t.Helper()
		if c.Kind != Color256 {
			t.Fatalf("colour is not xterm 256: %+v", c)
		}
		h, ok := xtermHex[uint8(c.Value)]
		if !ok {
			t.Fatalf("xterm entry %d has no measured hex", c.Value)
		}
		return h
	}
	ansiOf := func(t *testing.T, c Color) string {
		t.Helper()
		if c.Kind != Color16 {
			t.Fatalf("colour is not ANSI 16: %+v", c)
		}
		h, ok := ansiHex[uint8(c.Value)]
		if !ok {
			t.Fatalf("ANSI entry %d has no measured hex", c.Value)
		}
		return h
	}
	variants := []struct {
		name  string
		theme Theme
		hexOf func(t *testing.T, c Color) string
	}{
		{"truecolor", NeonThemeDepth(DepthTrue), hexOf},
		{"256", NeonThemeDepth(Depth256), xtermOf},
		{"16", NeonThemeDepth(Depth16), ansiOf},
	}
	for _, v := range variants {
		roles := neonRoles(v.theme)
		for _, group := range []struct {
			roles []string
			min   float64
		}{{textRoles, 4.5}, {graphRoles, 3}} {
			for _, name := range group.roles {
				ratio := contrastRatio(v.hexOf(t, roles[name]), neonBackdrop)
				if ratio < group.min {
					t.Errorf("%s %s: contrast %.2f is below %.1f", v.name, name, ratio, group.min)
				}
				t.Logf("%-9s %-12s %.2f:1", v.name, name, ratio)
			}
		}
	}
}

func hexByte(v uint8) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[v>>4], digits[v&0xf]})
}

// TestNeonTheme256Mapping pins the xterm 256 index chosen for every role, so a
// change to the approximation is a reviewed change and not a silent drift.
func TestNeonTheme256Mapping(t *testing.T) {
	want := map[string]uint8{
		"ok": 119, "warn": 229, "bad": 204, "dim": 246, "accent": 33,
		"graph": 119, "border": 103, "seriesBytes": 119, "seriesPkts": 204,
		"seriesV6": 32, "seriesH1": 33,
	}
	for name, c := range neonRoles(NeonThemeDepth(Depth256)) {
		if c.Kind != Color256 {
			t.Errorf("%s is not an xterm 256 colour: %+v", name, c)
			continue
		}
		if uint8(c.Value) != want[name] {
			t.Errorf("%s maps to xterm %d, want %d", name, c.Value, want[name])
		}
	}
}

// TestNeonTheme16Mapping pins the base ANSI colour chosen for every role.
func TestNeonTheme16Mapping(t *testing.T) {
	want := map[string]uint8{
		"ok": 10, "warn": 11, "bad": 9, "dim": 8, "accent": 14,
		"graph": 10, "border": 8, "seriesBytes": 10, "seriesPkts": 13,
		"seriesV6": 6, "seriesH1": 14,
	}
	for name, c := range neonRoles(NeonThemeDepth(Depth16)) {
		if c.Kind != Color16 {
			t.Errorf("%s is not an ANSI 16 colour: %+v", name, c)
			continue
		}
		if uint8(c.Value) != want[name] {
			t.Errorf("%s maps to ANSI %d, want %d", name, c.Value, want[name])
		}
	}
}

// TestNeonSeriesAreDistinct keeps the graph curves apart from each other, so
// bytes, packets, IPv6 and H1+ can be told apart in one chart.
func TestNeonSeriesAreDistinct(t *testing.T) {
	for _, depth := range []Depth{DepthTrue, Depth256, Depth16} {
		roles := neonRoles(NeonThemeDepth(depth))
		seen := map[uint32]string{}
		for _, name := range []string{"seriesBytes", "seriesPkts", "seriesV6", "seriesH1"} {
			c := roles[name]
			if prev, ok := seen[c.Value]; ok {
				t.Errorf("depth %d: %s and %s share a colour", depth, prev, name)
			}
			seen[c.Value] = name
		}
	}
}

// TestNeonThemeRegistered checks the theme is reachable by name and in the
// order the m menu cycles, with the mono theme still last.
func TestNeonThemeRegistered(t *testing.T) {
	names := ThemeNames()
	idx := -1
	for i, n := range names {
		if n == "neon" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("ThemeNames() has no neon: %v", names)
	}
	if names[len(names)-1] != "mono" {
		t.Errorf("mono must stay last in the menu order, got %q", names[len(names)-1])
	}
	th, ok := ThemeByName("neon")
	if !ok || th.Name != "neon" {
		t.Fatalf("ThemeByName(\"neon\") = %q, %v", th.Name, ok)
	}
	if _, ok := ThemeByName("nope"); ok {
		t.Error("ThemeByName accepted an unknown name")
	}
}

// TestDepthFromEnv covers the depth the theme is built at.
func TestDepthFromEnv(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want Depth
	}{
		{map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor"}, DepthTrue},
		{map[string]string{"TERM": "xterm-256color", "COLORTERM": "24bit"}, DepthTrue},
		{map[string]string{"TERM": "xterm-direct"}, DepthTrue},
		{map[string]string{"TERM": "xterm-256color"}, Depth256},
		{map[string]string{"TERM": "screen-256color"}, Depth256},
		{map[string]string{"TERM": "xterm"}, Depth256},
		{map[string]string{"TERM": ""}, Depth16},
	}
	for _, c := range cases {
		got := DepthFromEnv(func(k string) string { return c.env[k] })
		if got != c.want {
			t.Errorf("%v: depth = %v, want %v", c.env, got, c.want)
		}
	}
	// The environment built theme keeps the neon name and its own colour kind,
	// so the m menu shows the right entry after a cycle.
	th := NeonThemeFromEnv(func(k string) string { return map[string]string{"TERM": "xterm-256color"}[k] })
	if th.Name != "neon" || th.OK.FG.Kind != Color256 {
		t.Errorf("NeonThemeFromEnv: name %q, ok kind %v", th.Name, th.OK.FG.Kind)
	}
}
