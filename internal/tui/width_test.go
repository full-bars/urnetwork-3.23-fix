package tui

import "testing"

func TestRuneWidth(t *testing.T) {
	cases := []struct {
		r    rune
		want int
	}{
		{'a', 1}, {' ', 1}, {'~', 1},
		{0, 0}, {'\n', 0}, {'\t', 0}, {0x7F, 0}, {0x85, 0},
		{'é', 1}, {0x0301, 0}, {0x200D, 0}, {0xFE0F, 0},
		{'漢', 2}, {'あ', 2}, {'한', 2}, {'Ａ', 2}, {0x1F600, 2},
		{'█', 1}, {'⣿', 1}, {'┌', 1}, {'…', 1}, {'▁', 1},
	}
	for _, c := range cases {
		if got := RuneWidth(c.r); got != c.want {
			t.Errorf("RuneWidth(%U) = %d, want %d", c.r, got, c.want)
		}
	}
}

func TestStringWidth(t *testing.T) {
	if got := StringWidth("a漢é"); got != 4 {
		t.Fatalf("StringWidth = %d, want 4", got)
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		width int
		ascii bool
		want  string
	}{
		{"fits", "proxy", 5, false, "proxy"},
		{"cut with ellipsis", "proxy-alpha", 6, false, "proxy…"},
		{"zero width", "x", 0, false, ""},
		{"width one", "abc", 1, false, "…"},
		{"wide runes never split", "漢字漢字", 4, false, "漢…"},
		{"wide rune would overhang", "a漢字", 3, false, "a…"},
		{"exact wide fit", "漢字", 4, false, "漢字"},
		{"combining stays with base", "ééé", 2, false, "é…"},
		{"ascii long", "proxy-alpha", 8, true, "proxy..."},
		{"ascii narrow", "proxy-alpha", 3, true, "pr."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Truncate(c.in, c.width, c.ascii)
			if got != c.want {
				t.Fatalf("Truncate(%q, %d) = %q, want %q", c.in, c.width, got, c.want)
			}
			if StringWidth(got) > c.width {
				t.Fatalf("result is %d cells, limit %d", StringWidth(got), c.width)
			}
		})
	}
}

func TestTruncateInvalidUTF8(t *testing.T) {
	got := Truncate("ab\xffcd", 3, false)
	if StringWidth(got) > 3 {
		t.Fatalf("too wide: %q", got)
	}
}

func TestTruncateMiddle(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		width int
		ascii bool
		want  string
	}{
		{"fits", "ACTIVE 203.0.113.5", 20, false, "ACTIVE 203.0.113.5"},
		{"ipv6 middle cut", "ACTIVE 2602:f7a3:0:2b00:1234:5678:9abc:def0", 20, false, "ACTIVE 260…9abc:def0"},
		{"odd width", "abcdefghij", 6, false, "abc…ij"},
		{"zero width", "x", 0, false, ""},
		{"width one", "abc", 1, false, "…"},
		{"width two", "abcd", 2, false, "a…"},
		{"width three", "abcdef", 3, false, "a…f"},
		{"wide runes never split", "漢字漢字漢字", 5, false, "漢…字"},
		{"combining stays with base", "éáb́ć", 3, false, "é…ć"},
		{"ascii long", "ACTIVE 2602:f7a3:0:2b00::1", 15, true, "ACTIVE...b00::1"},
		{"ascii narrow", "proxy-alpha", 4, true, "pr.a"},
		{"ascii two", "proxy-alpha", 2, true, "p."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := TruncateMiddle(c.in, c.width, c.ascii)
			if got != c.want {
				t.Fatalf("TruncateMiddle(%q, %d, %v) = %q, want %q", c.in, c.width, c.ascii, got, c.want)
			}
			if StringWidth(got) > c.width {
				t.Fatalf("result is %d cells, limit %d", StringWidth(got), c.width)
			}
		})
	}
}
