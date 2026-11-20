package tui

import (
	"fmt"
	"strings"
	"testing"
)

func TestGraphPeakLabelsPlacement(t *testing.T) {
	format := func(v float64) string { return fmt.Sprintf("%.0f/s", v) }
	cols := make([]float64, 40)
	cols[10], cols[16], cols[30] = 5, 5, 5
	b := New(40, 10)
	drawPeakLabels(b, cols, 1, 10, format, Style{})
	if got := strings.Count(b.String(), "5/s"); got != 3 {
		t.Fatalf("got %d labels, want 3: %s", got, b.String())
	}

	// Overlap: col 13 and col 10 are too close
	b = New(40, 10)
	cols[13], cols[16] = 5, 0
	drawPeakLabels(b, cols, 1, 10, format, Style{})
	if got := strings.Count(b.String(), "5/s"); got != 2 {
		t.Fatalf("got %d labels, want 2 after overlap: %s", got, b.String())
	}

	// Short plot: height < 3
	b = New(4, 2)
	drawPeakLabels(b, cols, 1, 10, format, Style{})
	if strings.Contains(b.String(), "5") {
		t.Fatal("label placed on a short plot")
	}
}

func TestGraphPeakAndValleyLabels(t *testing.T) {
	format := func(v float64) string { return fmt.Sprintf("%.0f", v) }
	cols := make([]float64, 30)
	// Peak at 8, valley at 12, peak at 20
	cols[7] = 2
	cols[8] = 8 // peak
	cols[9] = 2
	cols[11] = 4
	cols[12] = 1 // valley
	cols[13] = 4
	cols[19] = 3
	cols[20] = 7 // peak
	cols[21] = 3

	b := New(30, 8)
	drawPeakLabels(b, cols, 1, 10, format, Style{})
	out := b.String()
	if !strings.Contains(out, "8") {
		t.Fatalf("peak 8 missing in %s", out)
	}
	if !strings.Contains(out, "7") {
		t.Fatalf("peak 7 missing in %s", out)
	}
	if !strings.Contains(out, "1") {
		t.Fatalf("valley 1 missing in %s", out)
	}
}

func TestGraphPeakLabelsScrollingStability(t *testing.T) {
	format := func(v float64) string { return fmt.Sprintf("%.0f", v) }
	// Simulate scrolling window across 10 frames
	base := make([]float64, 60)
	base[20] = 9 // major peak
	base[25] = 2
	base[35] = 7 // secondary peak

	for shift := 0; shift < 10; shift++ {
		cols := base[shift : shift+30]
		b := New(30, 8)
		drawPeakLabels(b, cols, 1, 10, format, Style{})
		out := b.String()
		if !strings.Contains(out, "9") {
			t.Fatalf("frame %d: major peak 9 was dropped in %s", shift, out)
		}
	}
}

func TestGraphPeakLabelsUnitFormatting(t *testing.T) {
	cols := make([]float64, 20)
	cols[10] = 259 * 1024 * 1024 // 259 MiB/s or MB/s
	cols[9], cols[11] = 1000, 1000

	b := New(30, 8)
	SetDecimalUnits(false)
	drawPeakLabels(b, cols, 1, 300*1024*1024, RateShort, Style{})
	if !strings.Contains(b.String(), "259 MiB/s") {
		t.Fatalf("binary units want 259 MiB/s, got %s", b.String())
	}

	SetDecimalUnits(true)
	defer SetDecimalUnits(false)
	b = New(30, 8)
	drawPeakLabels(b, cols, 1, 300*1024*1024, RateShort, Style{})
	if !strings.Contains(b.String(), "MB/s") {
		t.Fatalf("decimal units want MB/s, got %s", b.String())
	}
}
