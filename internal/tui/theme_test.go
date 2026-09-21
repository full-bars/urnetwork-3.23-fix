package tui

import "testing"

func TestThemeFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"plain color terminal", map[string]string{"TERM": "xterm-256color"}, "default"},
		{"NO_COLOR set", map[string]string{"TERM": "xterm", "NO_COLOR": "1"}, "mono"},
		{"NO_COLOR empty is ignored", map[string]string{"TERM": "xterm", "NO_COLOR": ""}, "default"},
		{"dumb", map[string]string{"TERM": "dumb"}, "mono"},
		{"TERM unset", map[string]string{}, "mono"},
	}
	for _, c := range cases {
		if got := ThemeFromEnv(env(c.env)).Name; got != c.want {
			t.Errorf("%s: theme %q, want %q", c.name, got, c.want)
		}
	}
}

func TestMonoThemeHasNoStyling(t *testing.T) {
	m := MonoTheme()
	for name, st := range map[string]Style{"ok": m.OK, "warn": m.Warn, "bad": m.Bad, "dim": m.Dim,
		"accent": m.Accent, "graph": m.Graph, "border": m.Border} {
		if st != (Style{}) {
			t.Errorf("mono %s style = %+v, want zero", name, st)
		}
	}
	if !m.ASCII || m.Frame != BorderASCII {
		t.Fatal("mono must use ascii glyphs and borders")
	}
	for _, r := range []rune{m.Frame.H, m.Frame.V, m.Frame.TL, m.Frame.TR, m.Frame.BL, m.Frame.BR} {
		if r > 0x7E {
			t.Errorf("mono border rune %U is not ascii", r)
		}
	}
}

func TestDefaultThemeRolesDistinct(t *testing.T) {
	d := DefaultTheme()
	if d.OK == d.Warn || d.Warn == d.Bad || d.OK == d.Bad {
		t.Fatal("ok/warn/bad must be distinguishable")
	}
	if d.ASCII {
		t.Fatal("default theme is not ascii")
	}
	for _, c := range []Color{d.OK.FG, d.Warn.FG, d.Bad.FG, d.Dim.FG, d.Accent.FG, d.Graph.FG, d.Border.FG} {
		if c.Kind != Color16 {
			t.Fatalf("default theme should use base colors, got %+v", c)
		}
	}
}

func TestLevel(t *testing.T) {
	th := DefaultTheme()
	if th.Level(0.1, 0.7, 0.9) != th.OK || th.Level(0.7, 0.7, 0.9) != th.Warn ||
		th.Level(0.95, 0.7, 0.9) != th.Bad || th.Level(0.9, 0.7, 0.9) != th.Bad {
		t.Fatal("Level thresholds")
	}
}
