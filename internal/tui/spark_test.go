package tui

import (
	"testing"
	"unicode/utf8"
)

func TestSparkWidthAndScaling(t *testing.T) {
	cases := []struct {
		name  string
		in    []uint64
		width int
		ascii bool
		want  string
	}{
		{"empty is blank", nil, 4, false, "    "},
		{"zero width", []uint64{1, 2}, 0, false, ""},
		{"all zero is a flat baseline", []uint64{0, 0, 0}, 3, false, "▁▁▁"},
		{"ramp uses the full range", []uint64{0, 1, 2, 3, 4, 5, 6, 7}, 8, false, "▁▂▃▄▅▆▇█"},
		{"fewer values right-align", []uint64{0, 7}, 4, false, "  ▁█"},
		{"more values average into buckets", []uint64{0, 0, 8, 8}, 2, false, "▁█"},
		{"ascii fallback", []uint64{0, 7}, 2, true, "_#"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Spark(c.in, c.width, c.ascii)
			if got != c.want {
				t.Fatalf("Spark(%v, %d) = %q, want %q", c.in, c.width, got, c.want)
			}
			if c.width > 0 && utf8.RuneCountInString(got) != c.width {
				t.Fatalf("width %d, got %d runes", c.width, utf8.RuneCountInString(got))
			}
		})
	}
}
